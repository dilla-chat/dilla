//! Bot v2 task 20 fix round 1 (Jonas, 2026-11-07): a member pins the device list of an account the
//! instance signed as a Bot, trusting it on first sight as the Bot trusts a user's first push, so it
//! can check the Bot's command key without sharing a group with the Bot.
#![cfg(not(target_arch = "wasm32"))]
mod client_support;
use client_support::*;
use dilla_core::bot::command::CommandKeyPublication;
use dilla_core::bot::device_list_push::{DeviceListPush, DeviceListRow};
use dilla_core::cbor::decode_strict;
use dilla_core::identity::{
    DeviceEntry, DeviceList, DeviceListUnsigned, SskSigner, Tier, UmkSigner,
};
use dilla_core::ids::{CommunityId, DeviceId, UserId};
use openmls::prelude::SignatureScheme;
use openmls_basic_credential::SignatureKeyPair;

const BOT: [u8; 16] = [0xb0; 16];
const HUMAN: [u8; 16] = [0xc0; 16];
const BOT_DEVICE: [u8; 16] = [0xb1; 16];
const SECOND_DEVICE: [u8; 16] = [0xb2; 16];
const COMMUNITY: [u8; 16] = [0x22; 16];

fn attestation_for(user: &[u8; 16], kind: u8) -> Vec<u8> {
    use ed25519_dalek::Signer;
    let key = ed25519_dalek::SigningKey::from_bytes(&[0x41; 32]);
    assert_eq!(key.verifying_key().to_bytes(), INSTANCE_SIGNER_PUB);
    let message = dilla_core::identity::account_kind_message(&INSTANCE, user, kind);
    dilla_core::identity::AccountKindAttestation {
        key_id: [0x53; 16],
        kind,
        signature: key.sign(&message).to_bytes(),
    }
    .encode()
}

fn dsk(seed: u8) -> (SignatureKeyPair, [u8; 32]) {
    let public = SskSigner::from_bytes(&[seed; 32]).public();
    (
        SignatureKeyPair::from_raw(SignatureScheme::ED25519, vec![seed; 32], public.to_vec()),
        public,
    )
}

fn entry(device: [u8; 16], dsk_pub: [u8; 32]) -> DeviceEntry {
    DeviceEntry {
        device_id: DeviceId::from_bytes(device),
        dsk_pub,
        tier: Tier::Native,
        added_at: 100,
        revoked_at: None,
    }
}

/// A user's keys: the UMK and the SSK it certifies.
struct Keys {
    umk: UmkSigner,
    ssk: SskSigner,
}

impl Keys {
    fn new(seed: u8) -> Self {
        Self {
            umk: UmkSigner::from_bytes(&[seed; 32]),
            ssk: SskSigner::from_bytes(&[seed ^ 0x5a; 32]),
        }
    }

    fn list(
        &self,
        user: [u8; 16],
        version: u64,
        prev: Option<&DeviceList>,
        entries: Vec<DeviceEntry>,
    ) -> DeviceList {
        let unsigned = DeviceListUnsigned {
            v: 1,
            user_id: UserId::from_bytes(user),
            version,
            prev_hash: prev.map_or([0; 32], DeviceList::hash),
            entries,
        };
        let sig_ssk = self.ssk.sign_device_list(&unsigned);
        DeviceList { unsigned, sig_ssk }
    }

    fn push(&self, lists: &[&DeviceList]) -> Vec<u8> {
        let ssk_pub = self.ssk.public();
        DeviceListPush {
            umk_pub: self.umk.public(),
            ssk_pub,
            sig_umk_ssk: self.umk.sign_ssk(&ssk_pub),
            history: lists
                .iter()
                .map(|l| DeviceListRow {
                    version: l.unsigned.version,
                    blob: l.encode(),
                    ssk_signature: l.sig_ssk,
                    prev_hash: l.unsigned.prev_hash,
                })
                .collect(),
        }
        .encode()
    }
}

fn publication(device: [u8; 16], signer: &SignatureKeyPair) -> Vec<u8> {
    CommandKeyPublication::sign(
        CommunityId::from_bytes(COMMUNITY),
        [0x0a; 16],
        [0x0c; 32],
        NOW + 3600,
        DeviceId::from_bytes(device),
        signer,
    )
    .expect("signed")
    .encode()
}

fn state(answer: &[u8]) -> (u64, u64) {
    decode_strict(answer, |d| {
        d.array(2)?;
        Ok((d.uint()?, d.uint()?))
    })
    .expect("[state, version]")
}

#[test]
fn a_human_account_is_refused_and_nothing_is_pinned() {
    let mut alice = ready_core(0x0a, "alice");
    alice
        .core
        .account_kind_ingest(&HUMAN, &attestation_for(&HUMAN, 0))
        .expect("attested human");
    let keys = Keys::new(0x31);
    let v1 = keys.list(HUMAN, 1, None, vec![entry(BOT_DEVICE, dsk(0x61).1)]);
    assert_eq!(
        code(
            alice
                .core
                .bot_device_list_pin(&HUMAN, &keys.push(&[&v1]), NOW)
        ),
        "E_CREDENTIAL"
    );
    assert!(
        alice
            .core
            .trusted_device_list(&HUMAN)
            .expect("read")
            .is_none(),
        "nothing cached for a human"
    );
}

#[test]
fn a_user_without_an_attestation_is_refused() {
    let mut alice = ready_core(0x0a, "alice");
    let keys = Keys::new(0x32);
    let v1 = keys.list(BOT, 1, None, vec![entry(BOT_DEVICE, dsk(0x61).1)]);
    assert_eq!(
        code(
            alice
                .core
                .bot_device_list_pin(&BOT, &keys.push(&[&v1]), NOW)
        ),
        "E_DEVICE_LIST_STALE"
    );
    assert!(
        alice
            .core
            .trusted_device_list(&BOT)
            .expect("read")
            .is_none()
    );
}

#[test]
fn a_bot_is_pinned_on_first_sight_and_its_command_key_then_verifies() {
    let mut alice = ready_core(0x0a, "alice");
    alice
        .core
        .account_kind_ingest(&BOT, &attestation_for(&BOT, 1))
        .expect("attested Bot");
    let (signer, signer_pub) = dsk(0x61);
    let publication = publication(BOT_DEVICE, &signer);
    // Before the pin there is no list to check the key against.
    assert_eq!(
        code(alice.core.bot_command_key_verify(&publication, &BOT, NOW)),
        "E_DEVICE_LIST_STALE"
    );
    let keys = Keys::new(0x33);
    let v1 = keys.list(BOT, 1, None, vec![entry(BOT_DEVICE, signer_pub)]);
    assert_eq!(
        state(
            &alice
                .core
                .bot_device_list_pin(&BOT, &keys.push(&[&v1]), NOW)
                .expect("pinned")
        ),
        (0, 1)
    );
    alice
        .core
        .bot_command_key_verify(&publication, &BOT, NOW)
        .expect("verifies after the pin");
}

#[test]
fn a_second_push_under_another_umk_is_held_and_verify_refuses() {
    let mut alice = ready_core(0x0a, "alice");
    alice
        .core
        .account_kind_ingest(&BOT, &attestation_for(&BOT, 1))
        .expect("attested Bot");
    let (signer, signer_pub) = dsk(0x61);
    let keys = Keys::new(0x34);
    let v1 = keys.list(BOT, 1, None, vec![entry(BOT_DEVICE, signer_pub)]);
    alice
        .core
        .bot_device_list_pin(&BOT, &keys.push(&[&v1]), NOW)
        .expect("pinned");
    let (other_signer, other_pub) = dsk(0x62);
    let other = Keys::new(0x44);
    let theirs = other.list(BOT, 1, None, vec![entry(SECOND_DEVICE, other_pub)]);
    assert_eq!(
        state(
            &alice
                .core
                .bot_device_list_pin(&BOT, &other.push(&[&theirs]), NOW)
                .expect("held")
        ),
        (1, 1)
    );
    // The changed key is held: not put in place, and nothing verifies until it is approved.
    assert!(
        alice
            .core
            .bot_command_key_verify(&publication(SECOND_DEVICE, &other_signer), &BOT, NOW)
            .is_err()
    );
    assert!(
        alice
            .core
            .bot_command_key_verify(&publication(BOT_DEVICE, &signer), &BOT, NOW)
            .is_err()
    );
}

#[test]
fn a_push_under_the_pinned_umk_extends_the_list() {
    let mut alice = ready_core(0x0a, "alice");
    alice
        .core
        .account_kind_ingest(&BOT, &attestation_for(&BOT, 1))
        .expect("attested Bot");
    let (_, first_pub) = dsk(0x61);
    let keys = Keys::new(0x35);
    let v1 = keys.list(BOT, 1, None, vec![entry(BOT_DEVICE, first_pub)]);
    alice
        .core
        .bot_device_list_pin(&BOT, &keys.push(&[&v1]), NOW)
        .expect("pinned");
    let (second, second_pub) = dsk(0x63);
    let v2 = keys.list(
        BOT,
        2,
        Some(&v1),
        vec![
            entry(BOT_DEVICE, first_pub),
            entry(SECOND_DEVICE, second_pub),
        ],
    );
    assert_eq!(
        state(
            &alice
                .core
                .bot_device_list_pin(&BOT, &keys.push(&[&v1, &v2]), NOW)
                .expect("extended")
        ),
        (0, 2)
    );
    alice
        .core
        .bot_command_key_verify(&publication(SECOND_DEVICE, &second), &BOT, NOW)
        .expect("the new device's key verifies");
}
