//! Local, authenticated snapshots used to repair a human device's restored group state.

use super::error::{E_CORE_INPUT, E_CORE_STATE};
use super::{ClientCore, ClientError, wire};
use crate::cbor::Encoder;
use crate::identity::{CredentialIdentity, Kind};
use crate::mls::{DillaGroup, GroupKind};
use openmls::prelude::BasicCredential;
use openmls_basic_credential::SignatureKeyPair;
use rusqlite::OptionalExtension;

const STATE_ACTIVE: i64 = 2;
const STATE_NEEDS_RESYNC: i64 = 3;
type StoredGroupRow = (i64, Option<Vec<u8>>, Vec<u8>, i64, i64, i64, i64);

fn refused() -> ClientError {
    ClientError::new(
        E_CORE_STATE,
        "group cannot be exported as a local heal snapshot",
    )
}

impl ClientCore {
    /// Export the local MLS view of a human-owned group for same-snapshot restore healing.
    /// Cryptographic material is always derived from OpenMLS state; the sole argument is the
    /// opaque group id. The wire value is `[GroupInfo, empty handshake tail, ratchet tree]`.
    pub fn group_heal_snapshot(&self, group_id: &[u8; 16]) -> Result<Vec<u8>, ClientError> {
        if group_id.iter().all(|byte| *byte == 0) {
            return Err(refused());
        }
        let own = self.own()?;
        if own.kind != Kind::User.as_u8() {
            return Err(refused());
        }

        let row: Option<StoredGroupRow> = self.read(|c| {
            c.query_row(
                "SELECT kind,community_id,target_id,state,epoch,max_epoch,pending_commit \
                 FROM app_groups WHERE group_id=?1",
                [group_id.as_slice()],
                |r| {
                    Ok((
                        r.get(0)?,
                        r.get(1)?,
                        r.get(2)?,
                        r.get(3)?,
                        r.get(4)?,
                        r.get(5)?,
                        r.get(6)?,
                    ))
                },
            )
            .optional()
            .map_err(Into::into)
        })?;
        let Some((kind, community, target, state, epoch, max_epoch, pending)) = row else {
            return Err(refused());
        };
        if !matches!(kind, 0 | 1 | 3)
            || !matches!(state, STATE_ACTIVE | STATE_NEEDS_RESYNC)
            || pending != 0
        {
            return Err(refused());
        }

        let group = DillaGroup::load(
            &self.provider,
            &openmls::prelude::GroupId::from_slice(group_id),
        )
        .map_err(|_| refused())?
        .ok_or_else(refused)?;
        if group.group_id().as_slice() != group_id
            || group.kind().as_u8() as i64 != kind
            || u64::try_from(epoch).ok() != Some(group.epoch())
            || u64::try_from(max_epoch)
                .ok()
                .is_none_or(|max| max > group.epoch())
        {
            return Err(refused());
        }

        match group.kind() {
            GroupKind::Text | GroupKind::Call => {
                let binding = group.binding().map_err(|_| refused())?;
                if binding.instance_id.as_bytes() != &own.instance_id
                    || binding
                        .community_id
                        .as_ref()
                        .map(|id| id.as_bytes().as_slice())
                        != community.as_deref()
                    || binding.target_id.as_slice() != target.as_slice()
                    || binding.kind != group.kind()
                {
                    return Err(refused());
                }
            }
            GroupKind::Pairing => return Err(refused()),
        }

        // Verify the current own leaf against the persisted device identity, including the DSK
        // and credential signatures. A cached MLS object is never accepted as caller evidence.
        let own_leaf = group.own_leaf_index().u32();
        let member = group.member_at(own_leaf).ok_or_else(refused)?;
        let basic = BasicCredential::try_from(member.credential).map_err(|_| refused())?;
        let credential = CredentialIdentity::decode(basic.identity()).map_err(|_| refused())?;
        let dsk: [u8; 32] = member
            .signature_key
            .as_slice()
            .try_into()
            .map_err(|_| refused())?;
        credential.verify_signatures(&dsk).map_err(|_| refused())?;
        if credential.user_id.as_bytes() != &own.user_id
            || credential.device_id.as_bytes() != &own.device_id
            || credential.kind != Kind::User
            || dsk != own.dsk_pub
        {
            return Err(refused());
        }

        let signer: &SignatureKeyPair = self.signer()?;
        let group_info = wire::tls(&group.export_group_info(&self.provider, signer)?)?;
        let ratchet_tree = wire::tls(&group.export_ratchet_tree())?;
        let mut out = Encoder::new();
        out.array(3)
            .bytes(&group_info)
            .array(0)
            .bytes(&ratchet_tree);
        if out.as_slice().len() > crate::bot::HELPER_PAYLOAD_MAX {
            return Err(ClientError::new(
                E_CORE_INPUT,
                "heal snapshot exceeds payload limit",
            ));
        }
        Ok(out.into_vec())
    }
}

#[cfg(all(test, not(target_arch = "wasm32")))]
mod tests {
    use super::*;
    use crate::mls::ConnHandle;
    use ed25519_dalek::SigningKey;
    use std::sync::{Arc, Mutex};

    const INSTANCE: [u8; 16] = [0x11; 16];
    const USER: [u8; 16] = [0x22; 16];
    const GROUP: [u8; 16] = [0x33; 16];
    const COMMUNITY: [u8; 16] = [0x44; 16];
    const CHANNEL: [u8; 16] = [0x55; 16];

    fn conn() -> ConnHandle {
        Arc::new(Mutex::new(rusqlite::Connection::open_in_memory().unwrap()))
    }

    fn ready() -> (ConnHandle, ClientCore) {
        let conn = conn();
        let mut core = ClientCore::open(conn.clone()).unwrap();
        core.signup_begin(&INSTANCE).unwrap();
        let instance_signer = SigningKey::from_bytes(&[0x66; 32])
            .verifying_key()
            .to_bytes();
        core.signup_complete(&USER, "heal-human", 1_800_000_000, &instance_signer)
            .unwrap();
        core.group_create(&GROUP, Some(&COMMUNITY), &CHANNEL, 1, &instance_signer)
            .unwrap();
        core.group_registered(&GROUP, 1).unwrap();
        (conn, core)
    }

    #[test]
    fn valid_channel_snapshot_is_three_part_cbor_and_survives_restart() {
        let (conn, core) = ready();
        let snapshot = core.group_heal_snapshot(&GROUP).expect("local snapshot");
        let fields = crate::cbor::decode_strict(&snapshot, |d| {
            d.array(3)?;
            let group_info = d.bytes()?.to_vec();
            d.array(0)?;
            let ratchet_tree = d.bytes()?.to_vec();
            Ok((group_info, ratchet_tree))
        })
        .expect("GroupInfo, empty tail array, and ratchet tree");
        assert!(!fields.0.is_empty());
        assert!(!fields.1.is_empty());

        conn.lock()
            .unwrap()
            .execute(
                "UPDATE app_groups SET state=3 WHERE group_id=?1",
                [GROUP.as_slice()],
            )
            .unwrap();
        assert_eq!(core.group_heal_snapshot(&GROUP).unwrap(), snapshot);

        drop(core);
        let restarted = ClientCore::open(conn).expect("restart");
        assert_eq!(restarted.group_heal_snapshot(&GROUP).unwrap(), snapshot);
    }

    #[test]
    fn refuses_unknown_stale_and_pending_group_rows() {
        let (conn, core) = ready();
        let wrong = [0x77; 16];
        assert_eq!(
            core.group_heal_snapshot(&wrong).unwrap_err().code,
            E_CORE_STATE
        );

        conn.lock()
            .unwrap()
            .execute(
                "UPDATE app_groups SET epoch=epoch+1 WHERE group_id=?1",
                [GROUP.as_slice()],
            )
            .unwrap();
        assert_eq!(
            core.group_heal_snapshot(&GROUP).unwrap_err().code,
            E_CORE_STATE
        );
        conn.lock()
            .unwrap()
            .execute(
                "UPDATE app_groups SET epoch=0,pending_commit=1 WHERE group_id=?1",
                [GROUP.as_slice()],
            )
            .unwrap();
        assert_eq!(
            core.group_heal_snapshot(&GROUP).unwrap_err().code,
            E_CORE_STATE
        );
    }

    #[test]
    fn bot_identity_cannot_export_a_human_heal_snapshot() {
        let mut core = ClientCore::open(conn()).unwrap();
        core.bot_bootstrap_begin(&INSTANCE, &COMMUNITY, &[0x77; 16], &[0x88; 32])
            .unwrap();
        core.bot_bootstrap_complete(&USER, "heal-bot", 1_800_000_000)
            .unwrap();
        assert_eq!(
            core.group_heal_snapshot(&GROUP).unwrap_err().code,
            E_CORE_STATE
        );
    }
}
