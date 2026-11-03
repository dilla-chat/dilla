//! Verification of the server-signed, short-lived human Welcome context for a current
//! kind-3 Bot interaction group. A valid proof is evidence only: callers must compare it with
//! the actual authenticated MLS binding and roster before applying a Welcome.

use crate::{
    cbor::{Decoder, decode_strict},
    identity::DeviceList,
    ids::{CommunityId, DeviceId, InstanceId, MsgId, UserId},
    mls::DillaInteractionBinding,
};
use ed25519_dalek::{Signature, VerifyingKey};

use super::{
    CodecError, INTERACTION_CLOCK_TOLERANCE_SECS,
    interaction_admission::AuthenticatedRosterLeaf,
    interaction_snapshot::{SnapshotList, validate_list},
};

const DOMAIN: &[u8] = b"dilla bot current interaction context v1\0";
const MAX_CONTEXT: usize = 160 * 1024;
const MAX_LIST: usize = 64 * 1024;
const MAX_I64: u64 = i64::MAX as u64;

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Expected {
    pub instance: InstanceId,
    pub group: MsgId,
    pub participant_user: UserId,
    pub participant_device: DeviceId,
    /// Current caller clock in the server timestamp domain.
    pub now: u64,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Registration {
    pub installation: MsgId,
    pub operation: MsgId,
    pub bot_user: MsgId,
    pub bot_device: MsgId,
    pub community: MsgId,
    pub group: MsgId,
    pub body_sha256: [u8; 32],
    pub generation: u64,
    pub accepted_epoch: u64,
    pub next_seq: u64,
    pub accepted_at: u64,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct RosterEntry {
    pub leaf_index: u64,
    pub user: MsgId,
    pub device: MsgId,
    pub signature_key: [u8; 32],
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct CurrentContext {
    pub instance: MsgId,
    pub instance_key_id: MsgId,
    pub instance_signer: [u8; 32],
    pub issued_at: u64,
    pub not_after: u64,
    pub registration: Registration,
    pub binding: DillaInteractionBinding,
    pub current_epoch: u64,
    pub tree_hash: [u8; 32],
    pub roster: [RosterEntry; 2],
    pub eligibility_revision: u64,
    pub policy_version: u64,
    pub e2ee_version: u64,
    pub media_version: u64,
    pub bot: SnapshotList,
    pub participant: SnapshotList,
}

fn msg(d: &mut Decoder<'_>) -> Result<MsgId, crate::cbor::CborError> {
    Ok(MsgId::from_bytes(d.bytes_exact::<16>()?))
}

fn list(d: &mut Decoder<'_>) -> Result<SnapshotList, crate::cbor::CborError> {
    d.array(9)?;
    Ok(SnapshotList {
        user: msg(d)?,
        umk: d.bytes_exact()?,
        ssk: d.bytes_exact()?,
        binding_signature: d.bytes_exact()?,
        version: d.uint()?,
        blob: d.bytes()?.to_vec(),
        signature: d.bytes_exact()?,
        prev_hash: d.bytes_exact()?,
        hash: d.bytes_exact()?,
    })
}

fn decode_payload(payload: &[u8]) -> Result<CurrentContext, CodecError> {
    decode_strict(payload, |d| {
        d.array(18)?;
        let version = d.uint()?;
        let instance = msg(d)?;
        let instance_key_id = msg(d)?;
        let instance_signer = d.bytes_exact()?;
        let issued_at = d.uint()?;
        let not_after = d.uint()?;

        d.array(11)?;
        let registration = Registration {
            installation: msg(d)?,
            operation: msg(d)?,
            bot_user: msg(d)?,
            bot_device: msg(d)?,
            community: msg(d)?,
            group: msg(d)?,
            body_sha256: d.bytes_exact()?,
            generation: d.uint()?,
            accepted_epoch: d.uint()?,
            next_seq: d.uint()?,
            accepted_at: d.uint()?,
        };

        d.array(9)?;
        let binding = DillaInteractionBinding {
            instance_id: InstanceId::from_bytes(d.bytes_exact::<16>()?),
            installation_id: d.bytes_exact()?,
            community_id: CommunityId::from_bytes(d.bytes_exact::<16>()?),
            bot_user_id: d.bytes_exact()?,
            participant_user_id: d.bytes_exact()?,
            participant_device_id: DeviceId::from_bytes(d.bytes_exact::<16>()?),
            policy_version: d.uint()?,
            e2ee_version: d.uint()?,
            media_version: d.uint()?,
        };
        let current_epoch = d.uint()?;
        let leaf_count = d.uint()?;
        let tree_hash = d.bytes_exact()?;
        d.array(2)?;
        let roster = [
            {
                d.array(4)?;
                RosterEntry {
                    leaf_index: d.uint()?,
                    user: msg(d)?,
                    device: msg(d)?,
                    signature_key: d.bytes_exact()?,
                }
            },
            {
                d.array(4)?;
                RosterEntry {
                    leaf_index: d.uint()?,
                    user: msg(d)?,
                    device: msg(d)?,
                    signature_key: d.bytes_exact()?,
                }
            },
        ];
        let eligibility_revision = d.uint()?;
        let policy_version = d.uint()?;
        let e2ee_version = d.uint()?;
        let media_version = d.uint()?;
        let bot = list(d)?;
        let participant = list(d)?;
        if version != 1 || leaf_count != 2 {
            return Err(crate::cbor::CborError::TypeMismatch {
                expected: "current interaction context v1 with two leaves",
                offset: d.position(),
            });
        }
        Ok(CurrentContext {
            instance,
            instance_key_id,
            instance_signer,
            issued_at,
            not_after,
            registration,
            binding,
            current_epoch,
            tree_hash,
            roster,
            eligibility_revision,
            policy_version,
            e2ee_version,
            media_version,
            bot,
            participant,
        })
    })
    .map_err(Into::into)
}

fn is_nonzero(id: &MsgId) -> bool {
    id.as_bytes().iter().any(|byte| *byte != 0)
}

/// Whether a proof issued at `issued_at` (instance clock) and valid until `not_after` is current
/// at the client clock `now`. `issued_at` may lie up to `INTERACTION_CLOCK_TOLERANCE_SECS` ahead
/// of `now`, the same skew the Bot side allows; expiry is exact.
pub(crate) fn is_current(issued_at: u64, not_after: u64, now: u64) -> bool {
    issued_at <= now.saturating_add(INTERACTION_CLOCK_TOLERANCE_SECS) && not_after > now
}

/// Verify canonical `[payload_bstr, signature_bstr]` using an instance key already pinned by
/// trusted local configuration. The wire's included signer key is only a consistency check.
pub fn verify(
    wire: &[u8],
    pinned_instance_key: &[u8; 32],
    expected: &Expected,
) -> Result<CurrentContext, CodecError> {
    if wire.is_empty() || wire.len() > MAX_CONTEXT + 128 {
        return Err(CodecError::Limit);
    }
    let (payload, signature) = decode_strict(wire, |d| {
        d.array(2)?;
        Ok((d.bytes()?.to_vec(), d.bytes_exact::<64>()?))
    })?;
    if payload.is_empty() || payload.len() > MAX_CONTEXT {
        return Err(CodecError::Limit);
    }
    let key = VerifyingKey::from_bytes(pinned_instance_key).map_err(|_| CodecError::Value)?;
    let mut preimage = Vec::with_capacity(DOMAIN.len() + payload.len());
    preimage.extend_from_slice(DOMAIN);
    preimage.extend_from_slice(&payload);
    key.verify_strict(&preimage, &Signature::from_bytes(&signature))
        .map_err(|_| CodecError::Context)?;

    let context = decode_payload(&payload)?;
    let r = &context.registration;
    let b = &context.binding;
    let ids = [
        context.instance,
        context.instance_key_id,
        r.installation,
        r.operation,
        r.bot_user,
        r.bot_device,
        r.community,
        r.group,
        MsgId::from_bytes(*b.instance_id.as_bytes()),
        MsgId::from_bytes(b.installation_id),
        MsgId::from_bytes(*b.community_id.as_bytes()),
        MsgId::from_bytes(b.bot_user_id),
        MsgId::from_bytes(b.participant_user_id),
        MsgId::from_bytes(*b.participant_device_id.as_bytes()),
        context.roster[0].user,
        context.roster[0].device,
        context.roster[1].user,
        context.roster[1].device,
    ];
    // The instance's timestamps may lie up to the shared skew ahead of this client's clock.
    let latest_instance_time = expected
        .now
        .saturating_add(INTERACTION_CLOCK_TOLERANCE_SECS);
    if ids.iter().any(|id| !is_nonzero(id))
        || context.instance_signer != *pinned_instance_key
        || context.not_after > MAX_I64
        || expected.now > MAX_I64
        || context.issued_at > MAX_I64
        || r.generation > MAX_I64
        || r.accepted_epoch > MAX_I64
        || r.next_seq == 0
        || r.next_seq > MAX_I64
        || r.accepted_at > latest_instance_time
        || context.current_epoch > MAX_I64
        || r.accepted_epoch > context.current_epoch
        || context.eligibility_revision == 0
        || context.eligibility_revision > MAX_I64
        || context.policy_version > MAX_I64
        || context.e2ee_version != 1
        || context.media_version != 0
        || b.e2ee_version != 1
        || b.media_version != 0
        || context.roster[0].leaf_index == context.roster[1].leaf_index
        || context.roster[0].leaf_index > u32::MAX as u64
        || context.roster[1].leaf_index > u32::MAX as u64
        || context.roster[0].signature_key == context.roster[1].signature_key
    {
        return Err(CodecError::Value);
    }
    if !is_current(context.issued_at, context.not_after, expected.now) {
        return Err(CodecError::Expired);
    }
    if context.not_after - context.issued_at > 30 {
        return Err(CodecError::Value);
    }
    if context.instance != MsgId::from_bytes(*expected.instance.as_bytes())
        || r.group != expected.group
        || b.instance_id != expected.instance
        || b.installation_id != *r.installation.as_bytes()
        || b.community_id.as_bytes() != r.community.as_bytes()
        || b.bot_user_id != *r.bot_user.as_bytes()
        || b.participant_user_id != *expected.participant_user.as_bytes()
        || b.participant_device_id != expected.participant_device
        || b.policy_version != context.policy_version
        || b.e2ee_version != context.e2ee_version
        || b.media_version != context.media_version
        || r.bot_user == MsgId::from_bytes(*expected.participant_user.as_bytes())
        || r.bot_device == MsgId::from_bytes(*expected.participant_device.as_bytes())
        || r.accepted_at > context.issued_at
    {
        return Err(CodecError::Context);
    }
    let bot_roster = context
        .roster
        .iter()
        .find(|leaf| leaf.user == r.bot_user && leaf.device == r.bot_device);
    let participant_roster = context.roster.iter().find(|leaf| {
        leaf.user.as_bytes() == expected.participant_user.as_bytes()
            && leaf.device.as_bytes() == expected.participant_device.as_bytes()
    });
    if bot_roster.is_none() || participant_roster.is_none() {
        return Err(CodecError::Context);
    }
    for item in [&context.bot, &context.participant] {
        if item.blob.len() > MAX_LIST {
            return Err(CodecError::Limit);
        }
        validate_list(item)?;
    }
    if context.bot.user != r.bot_user
        || context.participant.user.as_bytes() != expected.participant_user.as_bytes()
    {
        return Err(CodecError::Context);
    }
    let bot_devices = DeviceList::decode(&context.bot.blob).map_err(|_| CodecError::Format)?;
    let participant_devices =
        DeviceList::decode(&context.participant.blob).map_err(|_| CodecError::Format)?;
    let bot_leaf = bot_roster.expect("checked above");
    let participant_leaf = participant_roster.expect("checked above");
    let bot_device = DeviceId::from_bytes(*bot_leaf.device.as_bytes());
    let participant_device = DeviceId::from_bytes(*participant_leaf.device.as_bytes());
    let bot_entry = bot_devices.lookup(&bot_device).ok_or(CodecError::Context)?;
    let participant_entry = participant_devices
        .lookup(&participant_device)
        .ok_or(CodecError::Context)?;
    bot_devices
        .check_leaf(&bot_device, &bot_leaf.signature_key, bot_entry.tier)
        .map_err(|_| CodecError::Context)?;
    participant_devices
        .check_leaf(
            &participant_device,
            &participant_leaf.signature_key,
            participant_entry.tier,
        )
        .map_err(|_| CodecError::Context)?;
    Ok(context)
}

impl CurrentContext {
    /// Bind the signed snapshot to the actual OpenMLS-authenticated state. The caller must only
    /// pass leaves derived from authenticated MLS credentials and verify each credential chain.
    #[allow(dead_code)] // The human Welcome processor will consume this when that path is wired.
    pub(crate) fn verify_authenticated_group(
        &self,
        actual_binding: &DillaInteractionBinding,
        actual_epoch: u64,
        actual_tree_hash: &[u8; 32],
        actual_leaves: &[(u64, AuthenticatedRosterLeaf)],
    ) -> Result<(), CodecError> {
        if self.binding != *actual_binding
            || self.current_epoch != actual_epoch
            || self.tree_hash != *actual_tree_hash
            || actual_leaves.len() != 2
        {
            return Err(CodecError::Context);
        }
        for signed in &self.roster {
            let user = UserId::from_bytes(*signed.user.as_bytes());
            let device = DeviceId::from_bytes(*signed.device.as_bytes());
            let Some((leaf_index, actual)) = actual_leaves
                .iter()
                .find(|(_, leaf)| leaf.user == user && leaf.device == device)
            else {
                return Err(CodecError::Context);
            };
            if *leaf_index != signed.leaf_index || actual.dsk_pub != signed.signature_key {
                return Err(CodecError::Context);
            }
            let list = if signed.user == self.registration.bot_user {
                DeviceList::decode(&self.bot.blob).map_err(|_| CodecError::Format)?
            } else {
                DeviceList::decode(&self.participant.blob).map_err(|_| CodecError::Format)?
            };
            let listed = list.lookup(&device).ok_or(CodecError::Context)?;
            if listed.tier != actual.tier {
                return Err(CodecError::Context);
            }
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        cbor::Encoder,
        identity::{DeviceEntry, DeviceListUnsigned, SskSigner, Tier, UmkSigner},
    };
    use ed25519_dalek::{Signer, SigningKey};
    use sha2::{Digest, Sha256};

    fn mid(byte: u8) -> MsgId {
        MsgId::from_bytes([byte; 16])
    }

    fn expected() -> Expected {
        Expected {
            instance: InstanceId::from_bytes([2; 16]),
            group: mid(9),
            participant_user: UserId::from_bytes([7; 16]),
            participant_device: DeviceId::from_bytes([8; 16]),
            now: 100,
        }
    }

    fn signed_list(user: MsgId, device: MsgId, seed: u8) -> SnapshotList {
        let umk = UmkSigner::from_bytes(&[seed; 32]);
        let ssk = SskSigner::from_bytes(&[seed.wrapping_add(20); 32]);
        let unsigned = DeviceListUnsigned {
            v: 1,
            user_id: UserId::from_bytes(user.0),
            version: 1,
            prev_hash: [0; 32],
            entries: vec![DeviceEntry {
                device_id: DeviceId::from_bytes(device.0),
                dsk_pub: [seed.wrapping_add(1); 32],
                tier: Tier::Native,
                added_at: 1,
                revoked_at: None,
            }],
        };
        let value = DeviceList {
            sig_ssk: ssk.sign_device_list(&unsigned),
            unsigned,
        };
        let blob = value.encode();
        SnapshotList {
            user,
            umk: umk.public(),
            ssk: ssk.public(),
            binding_signature: umk.sign_ssk(&ssk.public()),
            version: 1,
            signature: value.sig_ssk,
            prev_hash: value.unsigned.prev_hash,
            hash: Sha256::digest(&blob).into(),
            blob,
        }
    }

    fn make_wire(
        expected: &Expected,
        key: &SigningKey,
        issued_at: u64,
        not_after: u64,
        wrong_binding_community: bool,
        wrong_roster_user: bool,
    ) -> (
        Vec<u8>,
        DillaInteractionBinding,
        Vec<(u64, AuthenticatedRosterLeaf)>,
    ) {
        let bot = signed_list(mid(5), mid(6), 40);
        let participant = signed_list(mid(7), mid(8), 50);
        let binding = DillaInteractionBinding {
            instance_id: expected.instance,
            bot_user_id: mid(5).0,
            installation_id: mid(3).0,
            community_id: crate::ids::CommunityId::from_bytes(if wrong_binding_community {
                [44; 16]
            } else {
                [4; 16]
            }),
            participant_user_id: mid(7).0,
            participant_device_id: DeviceId::from_bytes(mid(8).0),
            policy_version: 3,
            e2ee_version: 1,
            media_version: 0,
        };
        let bot_roster_user = if wrong_roster_user { mid(77) } else { mid(7) };
        let mut payload = Encoder::new();
        payload
            .array(18)
            .uint(1)
            .bytes(expected.instance.as_bytes())
            .bytes(mid(11).as_bytes())
            .bytes(&key.verifying_key().to_bytes())
            .uint(issued_at)
            .uint(not_after);
        payload
            .array(11)
            .bytes(mid(3).as_bytes())
            .bytes(mid(12).as_bytes())
            .bytes(mid(5).as_bytes())
            .bytes(mid(6).as_bytes())
            .bytes(mid(4).as_bytes())
            .bytes(expected.group.as_bytes())
            .bytes(&[13; 32])
            .uint(2)
            .uint(1)
            .uint(1)
            .uint(80);
        payload
            .array(9)
            .bytes(binding.instance_id.as_bytes())
            .bytes(&binding.installation_id)
            .bytes(binding.community_id.as_bytes())
            .bytes(&binding.bot_user_id)
            .bytes(&binding.participant_user_id)
            .bytes(binding.participant_device_id.as_bytes())
            .uint(binding.policy_version)
            .uint(binding.e2ee_version)
            .uint(binding.media_version)
            .uint(1)
            .uint(2)
            .bytes(&[14; 32])
            .array(2)
            .array(4)
            .uint(0)
            .bytes(mid(5).as_bytes())
            .bytes(mid(6).as_bytes())
            .bytes(&[41; 32])
            .array(4)
            .uint(1)
            .bytes(bot_roster_user.as_bytes())
            .bytes(mid(8).as_bytes())
            .bytes(&[51; 32])
            .uint(1)
            .uint(3)
            .uint(1)
            .uint(0);
        for item in [&bot, &participant] {
            payload
                .array(9)
                .bytes(item.user.as_bytes())
                .bytes(&item.umk)
                .bytes(&item.ssk)
                .bytes(&item.binding_signature)
                .uint(item.version)
                .bytes(&item.blob)
                .bytes(&item.signature)
                .bytes(&item.prev_hash)
                .bytes(&item.hash);
        }
        let payload = payload.into_vec();
        let signature = key.sign(&[DOMAIN, payload.as_slice()].concat()).to_bytes();
        let mut wire = Encoder::new();
        wire.array(2).bytes(&payload).bytes(&signature);
        let leaves = vec![
            (
                0,
                AuthenticatedRosterLeaf {
                    user: UserId::from_bytes(mid(5).0),
                    device: DeviceId::from_bytes(mid(6).0),
                    dsk_pub: [41; 32],
                    tier: Tier::Native,
                },
            ),
            (
                1,
                AuthenticatedRosterLeaf {
                    user: UserId::from_bytes(mid(7).0),
                    device: DeviceId::from_bytes(mid(8).0),
                    dsk_pub: [51; 32],
                    tier: Tier::Native,
                },
            ),
        ];
        (wire.into_vec(), binding, leaves)
    }

    #[test]
    fn verifies_current_signed_context_and_authenticated_roster() {
        let expected = expected();
        let key = SigningKey::from_bytes(&[91; 32]);
        let (wire, binding, leaves) = make_wire(&expected, &key, 90, 120, false, false);
        let context = verify(&wire, &key.verifying_key().to_bytes(), &expected).unwrap();
        assert_eq!(context.registration.group, expected.group);
        context
            .verify_authenticated_group(&binding, 1, &[14; 32], &leaves)
            .unwrap();
    }

    #[test]
    fn rejects_expiry_wrong_pin_local_identity_and_signed_tuple_changes() {
        let expected = expected();
        let key = SigningKey::from_bytes(&[91; 32]);
        let (wire, _, _) = make_wire(&expected, &key, 90, 100, false, false);
        assert_eq!(
            verify(&wire, &key.verifying_key().to_bytes(), &expected),
            Err(CodecError::Expired)
        );
        let (wire, _, _) = make_wire(&expected, &key, 90, 120, false, false);
        assert!(verify(&wire, &[92; 32], &expected).is_err());
        let mut trailing = wire.clone();
        trailing.push(0);
        assert_eq!(
            verify(&trailing, &key.verifying_key().to_bytes(), &expected),
            Err(CodecError::Format)
        );
        let (payload, _) = decode_strict(&wire, |d| {
            d.array(2)?;
            Ok((d.bytes()?.to_vec(), d.bytes_exact::<64>()?))
        })
        .unwrap();
        let mut noncanonical_payload = payload;
        noncanonical_payload.push(0);
        let signature = key
            .sign(&[DOMAIN, noncanonical_payload.as_slice()].concat())
            .to_bytes();
        let mut malformed = Encoder::new();
        malformed
            .array(2)
            .bytes(&noncanonical_payload)
            .bytes(&signature);
        assert_eq!(
            verify(
                &malformed.into_vec(),
                &key.verifying_key().to_bytes(),
                &expected
            ),
            Err(CodecError::Format)
        );
        let mut wrong_device = expected.clone();
        wrong_device.participant_device = DeviceId::from_bytes([18; 16]);
        assert_eq!(
            verify(&wire, &key.verifying_key().to_bytes(), &wrong_device),
            Err(CodecError::Context)
        );
        let (bad_binding, _, _) = make_wire(&expected, &key, 90, 120, true, false);
        assert_eq!(
            verify(&bad_binding, &key.verifying_key().to_bytes(), &expected),
            Err(CodecError::Context)
        );
        let (bad_roster, _, _) = make_wire(&expected, &key, 90, 120, false, true);
        assert_eq!(
            verify(&bad_roster, &key.verifying_key().to_bytes(), &expected),
            Err(CodecError::Context)
        );
    }

    #[test]
    fn freshness_allows_an_instance_clock_up_to_five_seconds_ahead_and_keeps_expiry_exact() {
        // now = 100: issued_at may be at most 105; not_after must be strictly after 100.
        assert!(is_current(100, 130, 100));
        assert!(is_current(101, 131, 100));
        assert!(is_current(104, 134, 100));
        assert!(is_current(105, 135, 100));
        assert!(!is_current(106, 136, 100));
        assert!(is_current(70, 101, 100));
        assert!(!is_current(70, 100, 100));
        assert!(!is_current(64, 94, 100));
        assert!(!is_current(u64::MAX, u64::MAX, 100));

        let expected = expected();
        let key = SigningKey::from_bytes(&[91; 32]);
        let pin = key.verifying_key().to_bytes();
        for issued_at in [101, 104, 105] {
            let (wire, _, _) = make_wire(&expected, &key, issued_at, issued_at + 30, false, false);
            assert!(
                verify(&wire, &pin, &expected).is_ok(),
                "issued_at = now + {} is accepted",
                issued_at - expected.now
            );
        }
        let (beyond, _, _) = make_wire(&expected, &key, 106, 136, false, false);
        assert_eq!(verify(&beyond, &pin, &expected), Err(CodecError::Expired));
        let (expired, _, _) = make_wire(&expected, &key, 70, 100, false, false);
        assert_eq!(verify(&expired, &pin, &expected), Err(CodecError::Expired));
    }

    #[test]
    fn rejects_future_or_overlong_context_and_actual_mls_mismatch() {
        let expected = expected();
        let key = SigningKey::from_bytes(&[91; 32]);
        let (future, _, _) = make_wire(&expected, &key, 106, 120, false, false);
        assert_eq!(
            verify(&future, &key.verifying_key().to_bytes(), &expected),
            Err(CodecError::Expired)
        );
        let (long, _, _) = make_wire(&expected, &key, 69, 100, false, false);
        assert!(verify(&long, &key.verifying_key().to_bytes(), &expected).is_err());
        let (wire, binding, leaves) = make_wire(&expected, &key, 90, 120, false, false);
        let context = verify(&wire, &key.verifying_key().to_bytes(), &expected).unwrap();
        let mut changed = binding.clone();
        changed.policy_version += 1;
        assert_eq!(
            context.verify_authenticated_group(&changed, 1, &[14; 32], &leaves),
            Err(CodecError::Context)
        );
        let mut changed_leaves = leaves.clone();
        changed_leaves[1].1.dsk_pub[0] ^= 1;
        assert_eq!(
            context.verify_authenticated_group(&binding, 1, &[14; 32], &changed_leaves),
            Err(CodecError::Context)
        );
        assert_eq!(
            context.verify_authenticated_group(&binding, 2, &[14; 32], &leaves),
            Err(CodecError::Context)
        );
        let mut wrong_index = leaves;
        wrong_index[1].0 = 2;
        assert_eq!(
            context.verify_authenticated_group(&binding, 1, &[14; 32], &wrong_index),
            Err(CodecError::Context)
        );
    }

    #[test]
    fn verifies_shared_go_wire_vector_and_rejects_mutations() {
        let corpus: serde_json::Value =
            serde_json::from_str(include_str!("../../../../protocol/vectors/bot.json"))
                .expect("bot vectors parse");
        let vector = &corpus["current_human_interaction_context"];
        let bytes = |field: &str| hex::decode(vector[field].as_str().unwrap()).unwrap();
        let seed: [u8; 32] = bytes("seed").try_into().unwrap();
        let public_key: [u8; 32] = bytes("public_key").try_into().unwrap();
        let payload = bytes("payload_hex");
        let signature: [u8; 64] = bytes("signature_hex").try_into().unwrap();
        let signing_key = SigningKey::from_bytes(&seed);
        assert_eq!(signing_key.verifying_key().to_bytes(), public_key);
        assert_eq!(
            signing_key
                .sign(&[DOMAIN, payload.as_slice()].concat())
                .to_bytes(),
            signature
        );
        let wire = bytes("wire_hex");
        let (wire_payload, wire_signature) = decode_strict(&wire, |d| {
            d.array(2)?;
            Ok((d.bytes()?.to_vec(), d.bytes_exact::<64>()?))
        })
        .unwrap();
        assert_eq!(wire_payload, payload);
        assert_eq!(wire_signature, signature);

        let expected = expected();
        let expected_vector = &vector["expected"];
        assert_eq!(
            hex::encode(expected.instance.as_bytes()),
            expected_vector["instance"]
        );
        assert_eq!(
            hex::encode(expected.group.as_bytes()),
            expected_vector["group"]
        );
        assert_eq!(
            hex::encode(expected.participant_user.as_bytes()),
            expected_vector["participant_user"]
        );
        assert_eq!(
            hex::encode(expected.participant_device.as_bytes()),
            expected_vector["participant_device"]
        );
        assert_eq!(expected.now, expected_vector["now"].as_u64().unwrap());
        let context =
            verify(&wire, &public_key, &expected).expect("shared current context verifies");
        assert_eq!(context.registration.group, expected.group);
        assert_eq!(context.registration.accepted_at, 90);
        assert_eq!(context.eligibility_revision, 12);
        assert_ne!(
            context.roster[0].signature_key,
            context.roster[1].signature_key
        );

        let rejects = vector["rejects"].as_array().unwrap();
        assert_eq!(rejects.len(), 2);
        let flipped_signature = hex::decode(rejects[0]["wire_hex"].as_str().unwrap()).unwrap();
        assert_eq!(
            verify(&flipped_signature, &public_key, &expected),
            Err(CodecError::Context)
        );
        let malformed_payload = hex::decode(rejects[1]["wire_hex"].as_str().unwrap()).unwrap();
        assert_eq!(
            verify(&malformed_payload, &public_key, &expected),
            Err(CodecError::Format)
        );
    }
}
