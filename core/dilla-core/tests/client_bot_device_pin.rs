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

// ---- Fix round 2 ----

/// N2: a pinned push that shows a newer signed head without reaching it is recorded; nothing verifies until a push
/// reaches the head.
#[test]
fn a_push_behind_its_signed_head_is_recorded_and_verify_refuses_until_it_is_reached() {
    let mut alice = ready_core(0x0a, "alice");
    alice
        .core
        .account_kind_ingest(&BOT, &attestation_for(&BOT, 1))
        .expect("attested Bot");
    let (signer, signer_pub) = dsk(0x61);
    let keys = Keys::new(0x36);
    let v1 = keys.list(BOT, 1, None, vec![entry(BOT_DEVICE, signer_pub)]);
    alice
        .core
        .bot_device_list_pin(&BOT, &keys.push(&[&v1]), NOW)
        .expect("pinned");
    let first = publication(BOT_DEVICE, &signer);
    alice
        .core
        .bot_command_key_verify(&first, &BOT, NOW)
        .expect("verifies at v1");
    // v3 revokes the device; a push of v3 alone does not chain to the head it shows.
    let v2 = keys.list(BOT, 2, Some(&v1), vec![entry(BOT_DEVICE, signer_pub)]);
    let mut revoked = entry(BOT_DEVICE, signer_pub);
    revoked.revoked_at = Some(200);
    let v3 = keys.list(BOT, 3, Some(&v2), vec![revoked]);
    assert_eq!(
        state(
            &alice
                .core
                .bot_device_list_pin(&BOT, &keys.push(&[&v3]), NOW)
                .expect("behind")
        ),
        (2, 1)
    );
    assert_eq!(
        code(alice.core.bot_command_key_verify(&first, &BOT, NOW)),
        "E_DEVICE_LIST_STALE",
        "behind: refused"
    );
    // A push that reaches the head clears the mark; the revocation it carries now refuses the device itself.
    assert_eq!(
        state(
            &alice
                .core
                .bot_device_list_pin(&BOT, &keys.push(&[&v1, &v2, &v3]), NOW)
                .expect("reached")
        ),
        (0, 3)
    );
    assert_eq!(
        code(alice.core.bot_command_key_verify(&first, &BOT, NOW)),
        "E_DEVICE_REVOKED"
    );
    // A newer list under a fresh device verifies again.
    let (fresh, fresh_pub) = dsk(0x64);
    let v4 = keys.list(BOT, 4, Some(&v3), vec![entry(SECOND_DEVICE, fresh_pub)]);
    assert_eq!(
        state(
            &alice
                .core
                .bot_device_list_pin(&BOT, &keys.push(&[&v3, &v4]), NOW)
                .expect("extended")
        ),
        (0, 4)
    );
    alice
        .core
        .bot_command_key_verify(&publication(SECOND_DEVICE, &fresh), &BOT, NOW)
        .expect("verifies at the head");
}

/// N3: a stored kind is final: a human re-attested as a Bot is refused, and the pin then refuses that user.
#[test]
fn a_stored_human_re_attested_as_a_bot_is_refused_and_cannot_be_pinned() {
    let mut alice = ready_core(0x0a, "alice");
    alice
        .core
        .account_kind_ingest(&HUMAN, &attestation_for(&HUMAN, 0))
        .expect("attested human");
    assert_eq!(
        code(
            alice
                .core
                .account_kind_ingest(&HUMAN, &attestation_for(&HUMAN, 1))
        ),
        "E_ACCOUNT_KIND_CHANGED"
    );
    // The same kind again is still fine.
    alice
        .core
        .account_kind_ingest(&HUMAN, &attestation_for(&HUMAN, 0))
        .expect("same kind");
    let keys = Keys::new(0x37);
    let v1 = keys.list(HUMAN, 1, None, vec![entry(BOT_DEVICE, dsk(0x61).1)]);
    assert_eq!(
        code(
            alice
                .core
                .bot_device_list_pin(&HUMAN, &keys.push(&[&v1]), NOW)
        ),
        "E_CREDENTIAL"
    );
}

/// N5: the push path and the group-leaf path share one pin: a Bot pinned by push and then seen in a group under another
/// UMK is held, the pin is not replaced, and its key does not verify.
#[test]
fn a_bot_pinned_by_push_then_seen_in_a_group_under_another_umk_is_held() {
    let instance = Instance::generate();
    let mut relay = Relay::new(GROUP);
    let mut alice = ready_core(0xa1, "alice");
    alice.create_and_register(&mut relay, &instance);
    let mut bob = RawPeer::new(0xb2, 0xb3);
    bob.join_external(&mut relay);
    alice.sync(&relay);
    alice
        .core
        .account_kind_ingest(&bob.user, &attestation_for(&bob.user, 1))
        .expect("attested Bot");
    // First sight by push, under a UMK that is not the one in bob's group leaf.
    let (signer, signer_pub) = dsk(0x61);
    let pushed = Keys::new(0x38);
    let v1 = pushed.list(bob.user, 1, None, vec![entry(bob.device, signer_pub)]);
    alice
        .core
        .bot_device_list_pin(&bob.user, &pushed.push(&[&v1]), NOW)
        .expect("pinned by push");
    let by_push = CommandKeyPublication::sign(
        CommunityId::from_bytes(COMMUNITY),
        [0x0a; 16],
        [0x0c; 32],
        NOW + 3600,
        DeviceId::from_bytes(bob.device),
        &signer,
    )
    .expect("signed")
    .encode();
    alice
        .core
        .bot_command_key_verify(&by_push, &bob.user, NOW)
        .expect("verifies after the push");
    // Then bob's group leaf, under his real UMK (RawPeer seeds: UMK 0xb2, SSK 0xf2): the history the leaf path reads starts
    // at the cached (pushed) v1 and goes on under the leaf's key, as a rotated account's does.
    let real = Keys {
        umk: UmkSigner::from_bytes(&[0xb2; 32]),
        ssk: SskSigner::from_bytes(&[0xf2; 32]),
    };
    let bob_dsk: [u8; 32] = bob.signer.public().try_into().expect("dsk");
    let v2 = real.list(bob.user, 2, Some(&v1), vec![entry(bob.device, bob_dsk)]);
    let mut history = dilla_core::cbor::Encoder::new();
    history.array(2);
    for l in [&v1, &v2] {
        history
            .array(4)
            .uint(l.unsigned.version)
            .bytes(&l.encode())
            .bytes(&l.sig_ssk)
            .bytes(&l.unsigned.prev_hash);
    }
    // The leaf verdict itself may refuse (another key); either way the pin is what matters.
    let _ = alice.core.device_list_ingest(
        &relay.group_id,
        relay.leaf_of(bob.device),
        &bob.user,
        &history.into_vec(),
        &v2.encode(),
        NOW,
    );
    let pin = pin_of(&alice, &bob.user).expect("a pin");
    assert_eq!(pin.0, pushed.umk.public(), "the pushed UMK stays pinned");
    assert!(
        pin.1.is_some_and(|changed| changed != pushed.umk.public()),
        "the leaf's UMK is the held change"
    );
    assert!(
        alice
            .core
            .bot_command_key_verify(&by_push, &bob.user, NOW)
            .is_err(),
        "held: nothing verifies"
    );
}

/// A user's pin from `trust_state`: (pinned UMK, pending changed UMK).
fn pin_of(alice: &Core, user: &[u8; 16]) -> Option<([u8; 32], Option<[u8; 32]>)> {
    let state = alice.core.trust_state().expect("trust state");
    decode_strict(&state, |d| {
        let n = d.array_len()?;
        let mut found = None;
        for _ in 0..n {
            d.array(6)?;
            let candidate = d.bytes_exact::<16>()?;
            let umk = d.bytes_exact::<32>()?;
            d.uint()?;
            d.uint()?;
            let changed = d.opt_bytes_exact::<32>()?;
            d.skip()?;
            if candidate == *user {
                found = Some((umk, changed));
            }
        }
        Ok(found)
    })
    .expect("trust state shape")
}

// ---- Task 20b ----

const OTHER_BOT: [u8; 16] = [0xb9; 16];

fn pinned_user(answer: &[u8]) -> (u64, [u8; 16]) {
    decode_strict(answer, |d| {
        d.array(2)?;
        Ok((d.uint()?, d.bytes_exact::<16>()?))
    })
    .expect("[state, pinned]")
}

/// The per-community Bot-user pin lives in the store: first sight pins, the same user again is fine, another user is
/// `changed` and never replaces the pin, and all of it survives a reopen (an in-memory pin re-pinned on every reload).
#[test]
fn the_community_bot_user_pin_survives_a_reopen_and_is_never_replaced() {
    let mut alice = ready_core(0x0a, "alice");
    assert_eq!(
        alice.core.bot_user_pinned(&COMMUNITY).expect("read"),
        None,
        "nothing pinned yet"
    );
    assert_eq!(
        pinned_user(
            &alice
                .core
                .bot_user_pin(&COMMUNITY, &BOT)
                .expect("first sight")
        ),
        (0, BOT)
    );
    assert_eq!(
        pinned_user(&alice.core.bot_user_pin(&COMMUNITY, &BOT).expect("same")),
        (1, BOT)
    );
    assert_eq!(
        pinned_user(
            &alice
                .core
                .bot_user_pin(&COMMUNITY, &OTHER_BOT)
                .expect("another")
        ),
        (2, BOT),
        "another Bot user is changed, and the pin stays"
    );
    let mut alice = alice.reopen();
    assert_eq!(
        alice.core.bot_user_pinned(&COMMUNITY).expect("read"),
        Some(BOT),
        "the pin survives a reopen"
    );
    assert_eq!(
        pinned_user(
            &alice
                .core
                .bot_user_pin(&COMMUNITY, &OTHER_BOT)
                .expect("another")
        ),
        (2, BOT),
        "a different Bot user after a reload is changed, not a first sight"
    );
    // Another community has its own pin.
    assert_eq!(
        pinned_user(
            &alice
                .core
                .bot_user_pin(&[0x23; 16], &OTHER_BOT)
                .expect("first sight")
        ),
        (0, OTHER_BOT)
    );
}

/// An account the instance attested as a human is never pinned as a community's Bot.
#[test]
fn an_attested_human_is_not_pinned_as_the_community_bot() {
    let mut alice = ready_core(0x0a, "alice");
    alice
        .core
        .account_kind_ingest(&HUMAN, &attestation_for(&HUMAN, 0))
        .expect("attested human");
    assert_eq!(
        code(alice.core.bot_user_pin(&COMMUNITY, &HUMAN)),
        "E_CREDENTIAL"
    );
    assert_eq!(alice.core.bot_user_pinned(&COMMUNITY).expect("read"), None);
}

/// Re-review M-A: the behind mark is cleared by any ingest that brings the cache to the marked head, here a group-leaf
/// ingest, with no further `bot_device_list_pin`.
#[test]
fn a_group_leaf_ingest_that_reaches_the_head_clears_the_behind_mark() {
    let instance = Instance::generate();
    let mut relay = Relay::new(GROUP);
    let mut alice = ready_core(0xa1, "alice");
    alice.create_and_register(&mut relay, &instance);
    let mut bob = RawPeer::new(0xb2, 0xb3);
    bob.join_external(&mut relay);
    alice.sync(&relay);
    alice
        .core
        .account_kind_ingest(&bob.user, &attestation_for(&bob.user, 1))
        .expect("attested Bot");
    // bob's real keys (RawPeer seeds: UMK 0xb2, SSK 0xf2), so the leaf path extends the pushed cache.
    let real = Keys {
        umk: UmkSigner::from_bytes(&[0xb2; 32]),
        ssk: SskSigner::from_bytes(&[0xf2; 32]),
    };
    let bob_dsk: [u8; 32] = bob.signer.public().try_into().expect("dsk");
    let v1 = real.list(bob.user, 1, None, vec![entry(bob.device, bob_dsk)]);
    let v2 = real.list(bob.user, 2, Some(&v1), vec![entry(bob.device, bob_dsk)]);
    let v3 = real.list(bob.user, 3, Some(&v2), vec![entry(bob.device, bob_dsk)]);
    alice
        .core
        .bot_device_list_pin(&bob.user, &real.push(&[&v1]), NOW)
        .expect("pinned");
    assert_eq!(
        state(
            &alice
                .core
                .bot_device_list_pin(&bob.user, &real.push(&[&v3]), NOW)
                .expect("behind")
        ),
        (2, 1)
    );
    let key = CommandKeyPublication::sign(
        CommunityId::from_bytes(COMMUNITY),
        [0x0a; 16],
        [0x0c; 32],
        NOW + 3600,
        DeviceId::from_bytes(bob.device),
        &bob.signer,
    )
    .expect("signed")
    .encode();
    assert_eq!(
        code(alice.core.bot_command_key_verify(&key, &bob.user, NOW)),
        "E_DEVICE_LIST_STALE",
        "behind: refused"
    );
    // The group-leaf path reads the history from the cached v1 to the head v3.
    let mut history = dilla_core::cbor::Encoder::new();
    history.array(3);
    for l in [&v1, &v2, &v3] {
        history
            .array(4)
            .uint(l.unsigned.version)
            .bytes(&l.encode())
            .bytes(&l.sig_ssk)
            .bytes(&l.unsigned.prev_hash);
    }
    alice
        .core
        .device_list_ingest(
            &relay.group_id,
            relay.leaf_of(bob.device),
            &bob.user,
            &history.into_vec(),
            &v3.encode(),
            NOW,
        )
        .expect("leaf ingest");
    alice
        .core
        .bot_command_key_verify(&key, &bob.user, NOW)
        .expect("the leaf ingest reached the head: the mark is gone");
}
