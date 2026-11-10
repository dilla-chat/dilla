//! Durable server-authoritative moderation removals.

use super::error::{E_CORE_INPUT, E_CORE_NOT_FOUND};
use super::fold::{self, FoldScope};
use super::{ClientCore, ClientError};
use crate::bot::RemovalProof;
use crate::identity::{CredentialIdentity, DeviceList};
use crate::ids::UserId;
use crate::mls::StorageError;
use ed25519_dalek::{Signature, VerifyingKey};
use rusqlite::{OptionalExtension, params};

type StoredTarget = (Option<i64>, Option<Vec<u8>>, Option<Vec<u8>>);
type CachedActorList = (i64, Vec<u8>, Option<Vec<u8>>);

pub(super) struct TrustedActor {
    pub list: DeviceList,
    pub credential: CredentialIdentity,
}

pub(super) fn group_community(
    core: &ClientCore,
    group_id: &[u8; 16],
) -> Result<[u8; 16], ClientError> {
    let row: Option<(i64, i64, Option<Vec<u8>>, i64)> = core.read(|c| {
        c.query_row(
            "SELECT kind,state,community_id,pending_commit FROM app_groups WHERE group_id=?1",
            [group_id.as_slice()],
            |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?, r.get(3)?)),
        )
        .optional()
        .map_err(Into::into)
    })?;
    let Some((kind, state, community, pending)) = row else {
        return Err(ClientError::new(E_CORE_NOT_FOUND, ""));
    };
    if kind != 0 {
        return Err(malformed("moderation removals require a text group"));
    }
    if state != 2 || pending != 0 {
        return Err(ClientError::new(
            super::error::E_CORE_STATE,
            "group is not active and settled",
        ));
    }
    community
        .as_deref()
        .and_then(|b| <[u8; 16]>::try_from(b).ok())
        .filter(|v| *v != [0; 16])
        .ok_or_else(|| malformed("group is not bound to a community"))
}

/// One signed entry from the authenticated server removal feed. `accepted_at` is the server's
/// recorded acceptance time; the caller must obtain this entry from the bound server feed.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct RemovalFeedEntry {
    pub revision: u64,
    pub proof: Vec<u8>,
    pub signature: [u8; 64],
    pub accepted_at: u64,
}

fn malformed(why: &str) -> ClientError {
    ClientError::new(E_CORE_INPUT, why)
}

pub(super) fn trusted_actor(core: &ClientCore, user: &UserId) -> Result<TrustedActor, ClientError> {
    let candidates: Vec<CachedActorList> = core.read(|c| {
        let mut stmt = c.prepare(
            "SELECT version,list,credential FROM app_device_lists WHERE user_id=?1 \
             UNION ALL SELECT version,list,credential FROM app_removal_actor_lists WHERE user_id=?1",
        )?;
        let rows = stmt.query_map([user.as_bytes().as_slice()], |r| {
            Ok((r.get(0)?, r.get(1)?, r.get(2)?))
        })?;
        rows.collect::<Result<_, _>>().map_err(Into::into)
    })?;
    let pin: Option<[u8; 32]> = core.read(|c| {
        c.query_row(
            "SELECT umk_pub FROM app_trust_pins WHERE user_id=?1",
            [user.as_bytes().as_slice()],
            |r| r.get(0),
        )
        .optional()
        .map_err(Into::into)
    })?;
    let pin = pin.ok_or_else(|| malformed("moderator has no pinned user key"))?;
    let mut trusted: Option<TrustedActor> = None;
    for (version, bytes, credential_bytes) in candidates {
        let Some(credential_bytes) = credential_bytes else {
            continue;
        };
        let Ok(list) = DeviceList::decode(&bytes) else {
            continue;
        };
        let Ok(credential) = CredentialIdentity::decode(&credential_bytes) else {
            continue;
        };
        let Ok(version_u64) = u64::try_from(version) else {
            continue;
        };
        let Some(dsk) = list
            .lookup(&credential.device_id)
            .map(|entry| entry.dsk_pub)
        else {
            continue;
        };
        if list.unsigned.user_id != *user
            || list.unsigned.version != version_u64
            || credential.user_id != *user
            || credential.umk_pub != pin
        {
            continue;
        }
        // The device-list signature is checked with the SSK from the UMK-anchored credential.
        if list.verify(&credential.ssk_pub).is_err() || credential.verify_signatures(&dsk).is_err()
        {
            continue;
        }
        if let Some(prior) = trusted.as_ref() {
            if list.unsigned.version == prior.list.unsigned.version
                && list.encode() != prior.list.encode()
            {
                return Err(malformed("conflicting trusted moderator device lists"));
            }
            if list.unsigned.version <= prior.list.unsigned.version {
                continue;
            }
        }
        trusted = Some(TrustedActor { list, credential });
    }
    trusted.ok_or_else(|| malformed("moderator has no UMK-anchored signed device list"))
}

pub(super) fn verify_entry_with_list(
    group_id: &[u8; 16],
    community: &[u8; 16],
    entry: &RemovalFeedEntry,
    list: &DeviceList,
) -> Result<RemovalProof, ClientError> {
    let proof =
        RemovalProof::decode(&entry.proof).map_err(|_| malformed("removal proof is malformed"))?;
    if proof.group_id.as_bytes() != group_id
        || proof.community_id.as_bytes() != community
        || proof.actor_user != list.unsigned.user_id
        || entry.accepted_at == 0
        || entry.accepted_at > i64::MAX as u64
        || entry.accepted_at >= proof.expires
        || proof.expires - entry.accepted_at > 30
    {
        return Err(malformed(
            "removal proof context or acceptance time is invalid",
        ));
    }
    let device = list
        .lookup(&proof.actor_device)
        .ok_or_else(|| malformed("moderator device is not listed"))?;
    if device.added_at > entry.accepted_at
        || device.revoked_at.is_some_and(|t| entry.accepted_at >= t)
    {
        return Err(malformed("moderator device was not valid at acceptance"));
    }
    let key = VerifyingKey::from_bytes(&device.dsk_pub)
        .map_err(|_| malformed("moderator signing key is invalid"))?;
    let signature = Signature::from_bytes(&entry.signature);
    key.verify_strict(
        &proof
            .signature_preimage()
            .map_err(|_| malformed("removal proof is invalid"))?,
        &signature,
    )
    .map_err(|_| malformed("removal proof signature is invalid"))?;
    Ok(proof)
}

pub(super) fn ingest(
    core: &mut ClientCore,
    group_id: &[u8; 16],
    entries: &[RemovalFeedEntry],
) -> Result<u64, ClientError> {
    core.own()?;
    if group_id == &[0; 16] {
        return Err(malformed("group id must be nonzero"));
    }
    let community = group_community(core, group_id)?;
    // Verify every entry before the transaction mutates its cursor or tombstones.
    for entry in entries {
        let proof = RemovalProof::decode(&entry.proof)
            .map_err(|_| malformed("removal proof is malformed"))?;
        let trusted = trusted_actor(core, &proof.actor_user)?;
        verify_entry_with_list(group_id, &community, entry, &trusted.list)?;
    }
    core.write(|_, u| {
        Ok(u.with_conn(|c| {
            let mut cursor = c.query_row(
                "SELECT revision FROM app_removal_cursor WHERE group_id=?1",
                [group_id.as_slice()],
                |r| r.get::<_, i64>(0),
            ).optional()?.unwrap_or(0);
            for entry in entries {
                let revision = entry.revision;
                let seq = RemovalProof::decode(&entry.proof).map_err(|_| StorageError::Codec("removal proof is malformed".into()))?.target_seq;
                if revision == 0 || seq == 0 || revision > i64::MAX as u64 || seq > i64::MAX as u64 {
                    return Err(StorageError::Codec("removal revision and target sequence must be positive signed integers".into()));
                }
                let rev = revision as i64;
                let target = seq as i64;
            let proof = RemovalProof::decode(&entry.proof)
                .map_err(|_| StorageError::Codec("removal proof is malformed".into()))?;
                if rev <= cursor {
                    let prior: Option<(i64, Vec<u8>, Vec<u8>, i64)> = c.query_row(
                        "SELECT seq,proof,signature,accepted_at FROM app_removal_entries WHERE group_id=?1 AND revision=?2",
                        params![group_id.as_slice(), rev], |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?, r.get(3)?)),
                    ).optional()?;
                    if prior.as_ref().is_none_or(|(seq, proof, sig, accepted)| {
                        *seq != target || proof.as_slice() != entry.proof || sig.as_slice() != entry.signature || *accepted != entry.accepted_at as i64
                    }) {
                        return Err(StorageError::Codec("removal feed replay does not match stored revision".into()));
                    }
                    mark_matching_manual_evidence(c, group_id, &proof)?;
                    continue;
                }
                if rev != cursor + 1 {
                    return Err(StorageError::Codec("removal feed revision gap".into()));
                }
                c.execute("INSERT INTO app_removal_entries(group_id,revision,seq,proof,signature,accepted_at) VALUES(?1,?2,?3,?4,?5,?6)",
                    params![group_id.as_slice(), rev, target, entry.proof, entry.signature.as_slice(), entry.accepted_at as i64])?;
                mark_matching_manual_evidence(c, group_id, &proof)?;
                let seq_revision: Option<i64> = c.query_row(
                    "SELECT revision FROM app_removal_tombstones WHERE group_id=?1 AND seq=?2",
                    params![group_id.as_slice(), target], |r| r.get(0),
                ).optional()?;
                if let Some(existing) = seq_revision {
                    // A later feed revision may repeat a target, but the original tombstone
                    // remains authoritative and the revision still advances.
                    let _ = existing;
                } else {
                    c.execute("INSERT INTO app_removal_tombstones(group_id,seq,revision) VALUES(?1,?2,?3)", params![group_id.as_slice(), target, rev])?;
                    let stored: Option<StoredTarget> = c.query_row(
                        "SELECT type,msg_id,reply_to FROM app_messages WHERE group_id=?1 AND seq=?2",
                        params![group_id.as_slice(), target],
                        |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?)),
                    ).optional()?;
                    if let Some((Some(0), Some(msg_id), _)) = stored.as_ref() {
                        c.execute("UPDATE app_messages SET body='',envelope=NULL,status=2,reason='' WHERE group_id=?1 AND reply_to=?2 AND type=1",
                            params![group_id.as_slice(), msg_id])?;
                    }
                    c.execute("UPDATE app_messages SET status=2,body='',edit_body=NULL,edit_seq=0,envelope=NULL,reason='' WHERE group_id=?1 AND seq=?2",
                        params![group_id.as_slice(), target])?;
                    c.execute("DELETE FROM app_pending_history WHERE group_id=?1 AND seq=?2", params![group_id.as_slice(), target])?;
                    super::bot_events::suppress_source(c, group_id, target)?;
                    if let Some((ty, msg, reply)) = stored
                        && let (Some(target_id), Some(ty)) = (msg.as_deref().and_then(|v| <[u8;16]>::try_from(v).ok()), ty)
                        && let Some(fold_target) = fold::target_of(Some(ty), Some(target_id), reply.as_deref().and_then(|v| <[u8;16]>::try_from(v).ok()))
                    {
                        fold::refold(c, group_id, &fold_target, &FoldScope::Full, None, None)?;
                    }
                    super::search::index_fold(c, group_id, seq)?;
                }
                cursor = rev;
                c.execute("INSERT INTO app_removal_cursor(group_id,revision) VALUES(?1,?2) ON CONFLICT(group_id) DO UPDATE SET revision=excluded.revision",
                    params![group_id.as_slice(), cursor])?;
            }
            Ok(cursor as u64)
        })?)
    }).map_err(|e| {
        if e.code == super::error::E_CORE_STORAGE {
            let msg = e.detail.as_str();
            if msg.contains("revision gap") || msg.contains("replay") || msg.contains("positive signed") {
                return malformed(msg);
            }
        }
        e
    })
}

/// Only a proof that has passed `verify_entry_with_list` above can move the durable outbox to
/// accepted. All local request context is compared so a colliding ID cannot release another
/// action's evidence.
fn mark_matching_manual_evidence(
    c: &rusqlite::Connection,
    group_id: &[u8; 16],
    proof: &RemovalProof,
) -> Result<(), StorageError> {
    c.execute(
        "UPDATE app_manual_removal_evidence SET state=2 WHERE proof_request_id=?1 AND actor_user_id=?2 AND actor_device_id=?3 AND community_id=?4 AND group_id=?5 AND target_sequence=?6 AND state<2",
        params![proof.request_id.as_bytes().as_slice(), proof.actor_user.as_bytes().as_slice(), proof.actor_device.as_bytes().as_slice(), proof.community_id.as_bytes().as_slice(), group_id.as_slice(), proof.target_seq as i64],
    )?;
    Ok(())
}

pub(super) fn is_tombstoned(
    c: &rusqlite::Connection,
    group_id: &[u8; 16],
    seq: u64,
) -> Result<bool, StorageError> {
    Ok(c.query_row(
        "SELECT 1 FROM app_removal_tombstones WHERE group_id=?1 AND seq=?2",
        params![group_id.as_slice(), seq as i64],
        |_| Ok(1_i64),
    )
    .optional()?
    .is_some())
}
