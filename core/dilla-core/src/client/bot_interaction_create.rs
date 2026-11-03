//! Transaction-local creation of managed Bot interaction groups.

use super::error::{E_CORE_INPUT, E_CORE_STATE};
use super::{ClientCore, ClientError, bot_identity, bot_intents, wire};
use crate::bot::INTERACTION_CLOCK_TOLERANCE_SECS;
use crate::bot::interaction_admission::Expected;
use crate::cbor::Encoder;
use crate::identity::CredentialIdentity;
use crate::mls::{DillaGroup, DillaInteractionBinding, GroupKind, StorageError};
use openmls::prelude::{BasicCredential, CredentialWithKey, MlsMessageBodyIn, MlsMessageIn};
use openmls_traits::OpenMlsProvider;
use rusqlite::params;
use tls_codec::Deserialize as _;

fn rejected(reason: &'static str) -> ClientError {
    // This fixed reason is private to the native helper boundary; the helper still returns its
    // generic status. It lets opt-in test diagnostics distinguish validation paths safely.
    ClientError::new(
        E_CORE_INPUT,
        format!("interaction create rejected: {reason}"),
    )
}

pub(super) fn snapshot_is_fresh(snapshot_now: u64, not_after: u64, local_now: u64) -> bool {
    snapshot_now <= local_now.saturating_add(INTERACTION_CLOCK_TOLERANCE_SECS)
        && not_after > local_now.saturating_sub(INTERACTION_CLOCK_TOLERANCE_SECS)
}

#[cfg(test)]
#[expect(
    clippy::items_after_test_module,
    reason = "The test module stays beside its focused freshness helper."
)]
mod freshness_tests {
    use super::snapshot_is_fresh;

    #[test]
    fn freshness_allows_only_five_seconds_of_shared_host_clock_skew() {
        assert!(snapshot_is_fresh(105, 106, 100));
        assert!(!snapshot_is_fresh(106, 200, 100));
        assert!(snapshot_is_fresh(90, 96, 100));
        assert!(!snapshot_is_fresh(90, 95, 100));
        assert!(!snapshot_is_fresh(90, 94, 100));
    }
}

impl ClientCore {
    /// Create a managed kind-3 group from a signed, locally trusted interaction snapshot and a
    /// real participant KeyPackage. The candidate is created as Bot-only epoch zero, then the
    /// participant is Added and merged. Its authenticated v2 context and exact roster are checked
    /// before app group or upload-intent rows are written. The outer storage unit also contains
    /// every nested OpenMLS provider transaction, so a rejected candidate leaves no MLS state.
    /// The caller must serialize operations for this Bot identity: snapshot trust pins are read
    /// before the write unit, and a second `ClientCore` sharing this store must not mutate them
    /// concurrently between verification and candidate creation.
    pub fn bot_interaction_group_create(
        &mut self,
        operation_id: &[u8; 16],
        group_id: &[u8; 16],
        snapshot_wire: &[u8],
        expected: &Expected,
        participant_key_package: &[u8],
    ) -> Result<Vec<u8>, ClientError> {
        let own = self.own()?;
        let scope = bot_intents::binding(self)?;
        if own.kind != 1
            || own.tier != 0
            || expected.group.as_bytes() != group_id
            || expected.bot_user.as_bytes() != &own.user_id
            || expected.bot_device.as_bytes() != &own.device_id
            || expected.instance.as_bytes() != &own.instance_id
            || expected.community.as_bytes() != &scope.community_id
            || expected.installation.as_bytes() != &scope.installation_id
        {
            return Err(ClientError::new(
                E_CORE_STATE,
                "native Bot binding mismatch",
            ));
        }

        // The durable exact-input check precedes freshness checks so a completed request remains
        // replayable after its signed snapshot expires. A conflicting reuse still fails the hash.
        let intent = bot_intents::write(operation_id, scope.installation_id, 2, |e| {
            // Bind the complete Expected tuple as well as the signed proof and real KeyPackage.
            e.array(14)
                .bytes(group_id)
                .bytes(snapshot_wire)
                .bytes(expected.instance.as_bytes())
                .bytes(expected.installation.as_bytes())
                .bytes(expected.community.as_bytes())
                .bytes(expected.bot_user.as_bytes())
                .bytes(expected.bot_device.as_bytes())
                .bytes(expected.participant_user.as_bytes())
                .bytes(expected.participant_device.as_bytes())
                .bytes(expected.group.as_bytes())
                .uint(expected.minimum_eligibility_revision)
                .uint(expected.server_now)
                .uint(expected.policy_version)
                .bytes(participant_key_package);
        })?;
        if let Some(result) = bot_intents::retry(self, intent, group_id)? {
            return Ok(result);
        }
        bot_intents::ensure_no_pending_for_group(self, group_id)?;

        // This is a read-only verification against the persisted instance key and both signed
        // device-list pins. New creates must also be fresh against the shared host clock; caller
        // supplied server_now is bound to the snapshot but is not itself a clock authority.
        let snapshot = self.bot_verify_interaction_snapshot(snapshot_wire, expected)?;
        let local_now = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map_err(|_| rejected("clock_unavailable"))?
            .as_secs();
        // Preserve expired-proof rejection before the read-only per-device duplicate check.
        // Creation still re-samples under its write transaction immediately before MLS mutation.
        if !super::bot_interaction_create::snapshot_is_fresh(
            snapshot.now,
            snapshot.not_after,
            local_now,
        ) {
            return Err(rejected("snapshot_expired"));
        }
        if self
            .read(|c| super::groups::group_row(c, group_id))?
            .is_some()
        {
            return Err(ClientError::new(E_CORE_STATE, "group already exists"));
        }
        let existing = self.bot_interaction_group_descriptors(
            expected.instance.as_bytes(),
            expected.community.as_bytes(),
            expected.installation.as_bytes(),
        )?;
        if existing.iter().any(|descriptor| {
            descriptor.participant_user == *expected.participant_user.as_bytes()
                && descriptor.participant_device == *expected.participant_device.as_bytes()
        }) {
            return Err(ClientError::new(
                E_CORE_STATE,
                "participant device already has an interaction group",
            ));
        }

        let (group, result) = self.write(|ctx, unit| {
            let signer = ctx
                .signer
                .ok_or_else(|| ClientError::new(super::error::E_CORE_NO_IDENTITY, ""))?;
            let bot_credential = CredentialIdentity::decode(&own.credential)
                .map_err(|_| ClientError::new(E_CORE_STATE, "Bot credential is malformed"))?;
            if bot_credential.user_id.as_bytes() != &own.user_id
                || bot_credential.device_id.as_bytes() != &own.device_id
            {
                return Err(ClientError::new(E_CORE_STATE, "Bot credential binding mismatch"));
            }
            let credential = CredentialWithKey {
                credential: BasicCredential::new(own.credential.clone()).into(),
                signature_key: signer.public().into(),
            };
            let binding = DillaInteractionBinding {
                instance_id: crate::ids::InstanceId::from_bytes(*expected.instance.as_bytes()),
                bot_user_id: *expected.bot_user.as_bytes(),
                installation_id: *expected.installation.as_bytes(),
                community_id: crate::ids::CommunityId::from_bytes(*expected.community.as_bytes()),
                participant_user_id: *expected.participant_user.as_bytes(),
                participant_device_id: crate::ids::DeviceId::from_bytes(*expected.participant_device.as_bytes()),
                policy_version: expected.policy_version,
                e2ee_version: 1,
                media_version: 0,
            };

            let input = MlsMessageIn::tls_deserialize_exact(participant_key_package)
                .map_err(|_| rejected("key_package_decode"))?;
            let MlsMessageBodyIn::KeyPackage(input) = input.extract() else {
                return Err(rejected("key_package_kind"));
            };
            let key_package = crate::public_group::validate_key_package(ctx.provider.crypto(), input)
                .map_err(|_| rejected("key_package_validation"))?;

            // Re-sample after acquiring the outer write transaction, immediately before the
            // first provider mutation, so lock contention cannot admit an expired snapshot.
            let local_now = std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .map_err(|_| rejected("clock_unavailable"))?
                .as_secs();
            if !snapshot_is_fresh(snapshot.now, snapshot.not_after, local_now) {
                return Err(rejected("snapshot_expired"));
            }

            let mut candidate = DillaGroup::create_interaction(
                ctx.provider,
                signer,
                credential,
                openmls::prelude::GroupId::from_slice(group_id),
                binding.clone(),
            )?;
            let added = candidate.add_members(ctx.provider, signer, &[key_package])?;
            let welcome = added
                .welcomes
                .iter()
                .find(|(device, _)| device.as_bytes() == expected.participant_device.as_bytes())
                .map(|(_, message)| wire::tls(message))
                .transpose()?
                .ok_or_else(|| rejected("welcome_missing"))?;
            candidate.merge_pending_commit(ctx.provider)?;

            bot_identity::verify_interaction_group_snapshot(
                &candidate,
                &snapshot,
                expected,
                &own.user_id,
                &own.device_id,
            )?;
            if candidate.kind() != GroupKind::Interaction || candidate.epoch() != 1 {
                return Err(rejected("candidate_shape"));
            }

            let group_info = wire::tls(&candidate.export_group_info(ctx.provider, signer)?)?;
            let ratchet_tree = wire::tls(&candidate.export_ratchet_tree())?;
            let mut encoded = Encoder::new();
            encoded
                .array(5)
                .bytes(group_id)
                .bytes(&binding.encode())
                .bytes(&group_info)
                .bytes(&ratchet_tree)
                .bytes(&welcome);
            let result = encoded.into_vec();
            bot_intents::check_result_size(&result)?;

            unit.with_conn(|c| {
                c.execute(
                    "INSERT INTO app_groups(group_id,kind,community_id,target_id,state,epoch,interaction_installation_id,interaction_device_id) VALUES(?1,3,?2,?3,0,1,?4,?5)",
                    params![group_id.as_slice(), expected.community.as_bytes().as_slice(), expected.participant_user.as_bytes().as_slice(), expected.installation.as_bytes().as_slice(), expected.participant_device.as_bytes().as_slice()],
                )?;
                bot_intents::persist(c, intent, group_id, &result)?;
                Ok::<_, StorageError>(())
            })?;
            Ok((candidate, result))
        })?;
        self.keep_group(*group_id, group);
        Ok(result)
    }
}
