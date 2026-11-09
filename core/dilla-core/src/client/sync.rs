//! Apply ordered delivery rows with MLS changes and the cursor in one unit per row.

use super::error::{
    E_CORE_INPUT, E_CORE_MLS, E_CORE_NOT_FOUND, E_CORE_RELOAD, E_CORE_STATE, E_CORE_STORAGE,
};
use super::fold;
use super::groups::{
    STATE_ACTIVE, STATE_GONE, STATE_NEEDS_RESYNC, checked, group_row, set_mls, set_pending,
};
use super::messages::{
    REASON_CLASS_MISMATCH, REASON_OWN_UNKNOWN, REASON_PRUNED, REASON_SENDER_MISMATCH,
    STATUS_CANNOT_DECRYPT, STATUS_DELETED, STATUS_OK, StoredMessage, TYPE_CLASS_MISMATCH,
    broadcast_ok, insert_message, mentionable_of, mentions_me_gated, own_roles_of, refold_insert,
};
use super::{ClientCore, ClientError, Own, wire};
use crate::cbor::Encoder;
use crate::envelope::Envelope;
use crate::ids::MsgId;
use crate::mls::{DillaGroup, DillaProcessed, StorageError};
use openmls::prelude::{GroupId, MlsMessageOut};
use rusqlite::{OptionalExtension, params};
use std::collections::BTreeMap;

enum Row {
    Handshake(wire::HandshakeRow),
    Message(wire::MessageRow),
}
/// web3.7 (R5): a class the host stored that disagrees with the decrypted type: class 0 is a
/// post (type 0), class 1 every other type. A legacy row (no class) never disagrees.
fn class_lies(class: Option<u64>, ty: crate::envelope::EnvelopeType) -> bool {
    class.is_some_and(|c| c != if ty.as_u8() == 0 { 0 } else { 1 })
}
fn absent() -> ClientError {
    ClientError::new(E_CORE_NOT_FOUND, "")
}
fn empty_message<'a>(
    id: &'a [u8; 16],
    row: &'a wire::MessageRow,
    status: i64,
    reason: &'a str,
) -> StoredMessage<'a> {
    StoredMessage {
        group_id: id,
        seq: row.seq,
        epoch: row.epoch,
        recv_ts: row.recv_ts,
        status,
        reason,
        sender_user: None,
        sender_device: &row.uploader_device,
        sender_leaf: None,
        sender_sig_key: None,
        sender_kind: None,
        sender_tier: None,
        msg_id: None,
        ty: None,
        reply_to: None,
        thread_id: None,
        body: "",
        envelope: None,
        franking_tag: &row.franking_tag,
        mention: false,
    }
}
fn own_message<'a>(
    id: &'a [u8; 16],
    row: &'a wire::MessageRow,
    own: &'a Own,
    env: &'a Envelope,
    bytes: &'a [u8],
    leaf: u32,
) -> StoredMessage<'a> {
    StoredMessage {
        group_id: id,
        seq: row.seq,
        epoch: row.epoch,
        recv_ts: row.recv_ts,
        status: STATUS_OK,
        reason: "",
        sender_user: Some(&own.user_id),
        sender_device: &own.device_id,
        sender_leaf: Some(leaf),
        sender_sig_key: None,
        sender_kind: Some(own.kind),
        sender_tier: Some(own.tier),
        msg_id: Some(env.msg_id.as_bytes()),
        ty: Some(env.kind.as_u8()),
        reply_to: env.reply_to.as_ref().map(MsgId::as_bytes),
        thread_id: env.thread_id.as_ref().map(MsgId::as_bytes),
        body: &env.body,
        envelope: Some(bytes),
        franking_tag: &row.franking_tag,
        mention: false,
    }
}
/// Raises `app_groups.max_epoch` (the F6 floor of a rejoin) to `epoch`, a newly held epoch.
fn raise_floor(c: &rusqlite::Connection, id: &[u8; 16], epoch: i64) -> Result<(), StorageError> {
    c.execute(
        "UPDATE app_groups SET max_epoch=MAX(max_epoch,?2) WHERE group_id=?1",
        params![id.as_slice(), epoch],
    )?;
    Ok(())
}
/// Whether a message row is already stored at `(id, seq)`.
fn message_stored(c: &rusqlite::Connection, id: &[u8; 16], seq: u64) -> Result<bool, StorageError> {
    Ok(c.query_row(
        "SELECT 1 FROM app_messages WHERE group_id=?1 AND seq=?2",
        params![id.as_slice(), seq as i64],
        |r| r.get::<_, i64>(0),
    )
    .optional()?
    .is_some())
}

type StoredKind = (
    Option<i64>,
    Option<[u8; 16]>,
    Option<[u8; 16]>,
    Option<[u8; 16]>,
    String,
);
type PendingHistoryRow = (
    u64,
    Vec<u8>,
    u32,
    Vec<u8>,
    u64,
    u64,
    [u8; 16],
    [u8; 32],
    [u8; 16],
    [u8; 16],
);
fn stored_kind(
    c: &rusqlite::Connection,
    id: &[u8; 16],
    seq: u64,
) -> Result<Option<StoredKind>, StorageError> {
    c.query_row("SELECT type, msg_id, reply_to, sender_user, body FROM app_messages WHERE group_id = ?1 AND seq = ?2",
        params![id.as_slice(), seq as i64],
        |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?, r.get(3)?, r.get(4)?)))
        .optional().map_err(Into::into)
}

fn refold_deleted(
    c: &rusqlite::Connection,
    id: &[u8; 16],
    stored: Option<StoredKind>,
) -> Result<(), StorageError> {
    if let Some((ty, msg_id, reply_to, _, _)) = stored
        && let Some(target) = fold::target_of(ty, msg_id, reply_to)
    {
        fold::refold(c, id, &target, &fold::FoldScope::Full, None, None)?;
        if let Some(row) = fold::resolve(c, id, &target)? {
            super::search::index_fold(c, id, row.seq)?;
        }
    }
    Ok(())
}
/// An outbox row: its `msg_id`, the stored envelope bytes and the envelope they decode to.
type OutboxEntry = ([u8; 16], Vec<u8>, Envelope);
/// The group's unresolved outbox row (in any state: in flight, failed after a lost answer, or
/// queued again after an echo wait) whose envelope commitment `C` is `served`: the served row is
/// that row's upload, which the server stored. `C` is keyed by the envelope's random `k_f`, so it
/// names one envelope; adopting it is what keeps a stored upload from being sent again.
fn unresolved_with(
    c: &rusqlite::Connection,
    id: &[u8; 16],
    served: &[u8; 32],
) -> Result<Option<OutboxEntry>, StorageError> {
    let mut stmt = c.prepare(
        "SELECT msg_id,envelope FROM app_outbox WHERE group_id=?1 ORDER BY created,msg_id",
    )?;
    let rows = stmt
        .query_map([id.as_slice()], |r| {
            Ok((r.get::<_, [u8; 16]>(0)?, r.get::<_, Vec<u8>>(1)?))
        })?
        .collect::<Result<Vec<_>, _>>()?;
    for (msg_id, bytes) in rows {
        let env = Envelope::decode(&bytes)
            .map_err(|_| StorageError::Sqlite("outbox envelope does not decode".into()))?;
        let commitment = env
            .commitment()
            .map_err(|_| StorageError::Sqlite("outbox envelope has no commitment".into()))?;
        if &commitment == served {
            return Ok(Some((msg_id, bytes, env)));
        }
    }
    Ok(None)
}
fn apply_message(
    c: &rusqlite::Connection,
    id: &[u8; 16],
    row: &wire::MessageRow,
    own: &Own,
    group: &DillaGroup,
) -> Result<(bool, bool), StorageError> {
    if row.deleted {
        let stored = stored_kind(c, id, row.seq)?;
        let changed = c.execute(
            "UPDATE app_messages SET status=2,body='',envelope=NULL,reason='' \
             WHERE group_id=?1 AND seq=?2",
            params![id.as_slice(), row.seq as i64],
        )?;
        if changed > 0 {
            super::bot_events::suppress_source(c, id, row.seq as i64)?;
            c.execute(
                "DELETE FROM app_pending_history WHERE group_id=?1 AND seq=?2",
                params![id.as_slice(), row.seq as i64],
            )?;
            refold_deleted(c, id, stored)?;
            super::search::index_fold(c, id, row.seq)?;
            return Ok((true, false));
        }
        // The deleted upload of an unresolved outbox row (its commitment survives the deletion): the
        // server stored it, so the outbox row is done and never resent.
        if row.uploader_device == own.device_id
            && let Some(served) = row.commitment.as_ref()
            && let Some((msg_id, _, env)) = unresolved_with(c, id, served)?
        {
            let marker = StoredMessage {
                status: STATUS_DELETED,
                sender_user: Some(&own.user_id),
                sender_device: &own.device_id,
                sender_leaf: Some(group.own_leaf_index().u32()),
                sender_kind: Some(own.kind),
                sender_tier: Some(own.tier),
                msg_id: Some(env.msg_id.as_bytes()),
                ty: Some(env.kind.as_u8()),
                reply_to: env.reply_to.as_ref().map(MsgId::as_bytes),
                thread_id: env.thread_id.as_ref().map(MsgId::as_bytes),
                ..empty_message(id, row, STATUS_DELETED, "")
            };
            insert_message(c, &marker)?;
            if let Some(target) = fold::target_of(
                Some(i64::from(env.kind.as_u8())),
                Some(*env.msg_id.as_bytes()),
                env.reply_to.as_ref().map(|r| *r.as_bytes()),
            ) {
                fold::refold(
                    c,
                    id,
                    &target,
                    &fold::scope_of(
                        Some(i64::from(env.kind.as_u8())),
                        Some(own.user_id),
                        &env.body,
                    ),
                    None,
                    None,
                )?;
            }
            c.execute(
                "DELETE FROM app_outbox WHERE msg_id=?1",
                [msg_id.as_slice()],
            )?;
            return Ok((true, true));
        }
        insert_message(c, &empty_message(id, row, STATUS_DELETED, ""))?;
        return Ok((true, false));
    }
    // MLS application still happened before this storage step. A moderation tombstone only
    // suppresses presentation and retained plaintext; it is never treated as an MLS envelope.
    if super::removals::is_tombstoned(c, id, row.seq)? {
        insert_message(c, &empty_message(id, row, STATUS_DELETED, ""))?;
        super::search::index_fold(c, id, row.seq)?;
        return Ok((true, false));
    }
    if row.blob.is_none() {
        insert_message(
            c,
            &empty_message(id, row, STATUS_CANNOT_DECRYPT, REASON_PRUNED),
        )?;
        return Ok((true, false));
    }
    if row.uploader_device == own.device_id {
        if message_stored(c, id, row.seq)? {
            return Ok((false, false));
        }
        // Adopted only as the upload of the outbox row (in flight, failed or queued again) whose
        // envelope commitment the row carries (the served field and the ciphertext's
        // authenticated data, which must agree when both are there). Anything else is an own
        // upload this device cannot place.
        let carried = row.blob.as_deref().and_then(wire::blob_commitment);
        let served = match (row.commitment, carried) {
            (Some(field), Some(blob)) if field != blob => None,
            (field, blob) => field.or(blob),
        };
        let adopt = match served {
            Some(served) => unresolved_with(c, id, &served)?,
            None => None,
        };
        if let Some((msg_id, bytes, env)) = adopt {
            insert_message(
                c,
                &own_message(id, row, own, &env, &bytes, group.own_leaf_index().u32()),
            )?;
            refold_insert(
                c,
                id,
                row.seq,
                &env,
                &own.user_id,
                &own.device_id,
                Some(own),
            )?;
            c.execute(
                "DELETE FROM app_outbox WHERE msg_id=?1",
                [msg_id.as_slice()],
            )?;
            return Ok((true, true));
        }
        insert_message(
            c,
            &empty_message(id, row, STATUS_CANNOT_DECRYPT, REASON_OWN_UNKNOWN),
        )?;
        return Ok((true, false));
    }
    Err(StorageError::Sqlite(
        "message branch was not selected".into(),
    ))
}

impl ClientCore {
    /// Drain retained application evidence in pages of at most 256 rows after signed history arrives
    /// or a key is verified. Evidence is retained for a pending leaf and for a leaf whose verdict
    /// failed (unlisted, changed key, wrong key); a row becomes readable only after MLS and the
    /// current leaf verdict both pass in one unit.
    /// The held reasons below are the ones a later validation can end; `E_DEVICE_REVOKED` is not
    /// among them: a revoked device's rows are final (`trust::apply_verdict`).
    pub fn revalidate_pending(
        &mut self,
        id: &[u8; 16],
        leaf_index: u32,
        limit: u32,
    ) -> Result<u32, ClientError> {
        if !(1..=256).contains(&limit) {
            return Err(ClientError::new(
                E_CORE_INPUT,
                "pending limit must be 1..=256",
            ));
        }
        let own = self.own()?;
        let (pending_user, pending_device) = self.current_leaf_pair(id, leaf_index)?;
        // Bot v2 Task 16: a Bot judges the leaf against its pushed list first (a no-op for a person).
        self.rejudge_pushed_leaf(id, leaf_index)?;
        let mut cursor = -1_i64;
        let mut total_recovered = 0_u32;
        loop {
            let pending: Vec<PendingHistoryRow> = self.read(|c| {
                let mut stmt = c.prepare("SELECT p.seq,p.envelope,p.sender_leaf,p.sender_credential,m.epoch,m.recv_ts,m.sender_device,m.franking_tag,p.user_id,p.device_id \
                    FROM app_pending_history p INDEXED BY app_pending_history_by_leaf_seq JOIN app_messages m ON m.group_id=p.group_id AND m.seq=p.seq \
                    WHERE p.group_id=?1 AND p.user_id=?2 AND p.device_id=?3 AND p.sender_leaf=?4 AND p.seq>?5 AND m.status=1 AND m.reason IN ('E_DEVICE_LIST_STALE','E_UMK_CHANGED','E_CREDENTIAL','E_DEVICE_UNLISTED','E_TIER_MISMATCH') ORDER BY p.seq LIMIT ?6")?;
                stmt.query_map(params![id.as_slice(),pending_user.as_slice(),pending_device.as_slice(), i64::from(leaf_index), cursor, i64::from(limit)], |r| {
                    Ok((r.get::<_, i64>(0)? as u64, r.get(1)?, r.get(2)?, r.get(3)?,
                        r.get::<_, i64>(4)? as u64, r.get::<_, i64>(5)? as u64, r.get(6)?, r.get(7)?,r.get(8)?,r.get(9)?))
                })?.collect::<Result<_, _>>().map_err(Into::into)
            })?;
            if pending.is_empty() {
                return Ok(total_recovered);
            }
            // Advance by the last examined row, including evidence that remains neutral.
            // Such a row must not be retried indefinitely ahead of later valid rows.
            cursor = pending.last().expect("nonempty page").0 as i64;
            let page_len = pending.len();
            let recovered = self.retry_reload(id, |this| {
                let group = this.take_group(id)?.ok_or_else(absent)?;
                let result = this.write(|_, u| {
                    let mut recovered = 0u32;
                    for (seq, envelope_bytes, leaf, credential, epoch, recv_ts, uploader, franking, user, device) in &pending {
                        let envelope = match Envelope::decode(envelope_bytes) { Ok(v) => v, Err(_) => continue };
                        let valid = u.with_conn(|c| Ok(super::trust::validate_pending_sender(c, &group, id, *leaf, credential)))?;
                        if valid.is_err() { continue; }
                        let Some(member) = group.member_at(*leaf) else { continue };
                        let basic = match openmls::prelude::BasicCredential::try_from(member.credential) { Ok(v) => v, Err(_) => continue };
                        let sender = match crate::identity::CredentialIdentity::decode(basic.identity()) { Ok(v) => v, Err(_) => continue };
                        if sender.device_id.as_bytes() != uploader || sender.user_id.as_bytes() != user || sender.device_id.as_bytes() != device { continue; }
                        u.with_conn(|c| {
                            let still_pending: bool = c.query_row(
                                "SELECT 1 FROM app_messages WHERE group_id=?1 AND seq=?2 AND status=1 AND reason IN ('E_DEVICE_LIST_STALE','E_UMK_CHANGED','E_CREDENTIAL','E_DEVICE_UNLISTED','E_TIER_MISMATCH')",
                                params![id.as_slice(), *seq as i64], |_| Ok(1_i64)).optional()?.is_some();
                            if !still_pending { return Ok(()); }
                            let roles = own_roles_of(c, id)?;
                            let mentionable = mentionable_of(c, id)?;
                            let broadcast = broadcast_ok(c, id, sender.user_id.as_bytes())?;
                            c.execute("DELETE FROM app_messages WHERE group_id=?1 AND seq=?2", params![id.as_slice(), *seq as i64])?;
                            insert_message(c, &StoredMessage {
                                group_id: id, seq: *seq, epoch: *epoch, recv_ts: *recv_ts,
                                status: STATUS_OK, reason: "", sender_user: Some(sender.user_id.as_bytes()),
                                sender_device: sender.device_id.as_bytes(), sender_leaf: Some(*leaf),
                                sender_sig_key: Some(member.signature_key.as_slice()),
                                sender_kind: Some(sender.kind.as_u8()), sender_tier: Some(sender.tier.as_u8()),
                                msg_id: Some(envelope.msg_id.as_bytes()), ty: Some(envelope.kind.as_u8()),
                                reply_to: envelope.reply_to.as_ref().map(MsgId::as_bytes),
                                thread_id: envelope.thread_id.as_ref().map(MsgId::as_bytes), body: &envelope.body,
                                envelope: Some(envelope_bytes), franking_tag: franking,
                                mention: envelope.kind.as_u8() == 0 && sender.user_id.as_bytes() != &own.user_id
                                    && mentions_me_gated(&envelope.body, &own.user_id, &roles, &mentionable, broadcast),
                            })?;
                            super::bot_events::record_accepted(
                                c, &group, id, *seq, &own,
                                &sender, *leaf, &member.signature_key, *epoch, *recv_ts,
                                envelope.msg_id.as_bytes(), envelope.kind.as_u8(), &envelope.body,
                                envelope.reply_to.as_ref().map(|v| v.as_bytes()),
                                envelope.thread_id.as_ref().map(|v| v.as_bytes()),
                            )?;
                            refold_insert(c, id, *seq, &envelope, sender.user_id.as_bytes(),
                                sender.device_id.as_bytes(), Some(&own))?;
                            c.execute("DELETE FROM app_pending_history WHERE group_id=?1 AND seq=?2", params![id.as_slice(), *seq as i64])?;
                            Ok(())
                        })?;
                        recovered += 1;
                    }
                    if recovered > 0 { u.with_conn(super::trust::bump_generation)?; }
                    Ok(recovered)
                });
                this.keep_group(*id, group);
                result
            })?;
            total_recovered += recovered;
            if page_len < limit as usize {
                return Ok(total_recovered);
            }
        }
    }

    pub fn commit_build(
        &mut self,
        id: &[u8; 16],
        proposals_body: &[u8],
    ) -> Result<Vec<u8>, ClientError> {
        self.commit_build_inner(id, proposals_body, false)
    }

    /// `commit_build`'s body. With `rotate` (`call_rotate`), this device's own pending `Remove` of
    /// its own leaf is taken out of its queue first: a rotation is an UpdatePath commit, and a
    /// device never commits its own removal (OpenMLS refuses it); another member commits the
    /// posted proposal.
    pub(super) fn commit_build_inner(
        &mut self,
        id: &[u8; 16],
        proposals_body: &[u8],
        rotate: bool,
    ) -> Result<Vec<u8>, ClientError> {
        self.commit_build_inner_with_intent(id, proposals_body, rotate, None)
    }

    pub(super) fn commit_build_inner_with_intent(
        &mut self,
        id: &[u8; 16],
        proposals_body: &[u8],
        rotate: bool,
        intent: Option<super::bot_intents::IntentWrite>,
    ) -> Result<Vec<u8>, ClientError> {
        self.own()?;
        let proposals = wire::decode_proposals_body(proposals_body)?;
        let row = self.read(|c| group_row(c, id))?.ok_or_else(absent)?;
        if row.state != STATE_ACTIVE {
            return Err(ClientError::new(
                E_CORE_STATE,
                format!("group state {}", row.state),
            ));
        }
        self.signer()?;
        self.retry_reload(id, |this| {
            let mut group = this.take_group(id)?.ok_or_else(absent)?;
            if group.has_pending_commit() {
                this.keep_group(*id, group);
                return Err(ClientError::new(E_CORE_STATE, "a commit is pending"));
            }
            let result = this.write(|ctx, u| {
                for item in &proposals {
                    if item.void { continue; }
                    let _ = (&item.reference, item.kind, item.target_leaf);
                    let Ok(message) = wire::protocol_message(&item.blob) else { continue; };
                    if message.epoch().as_u64() != group.epoch() { continue; }
                    match group.process_message(ctx.provider, message) {
                        Ok(DillaProcessed::Proposal(proposal)) => {
                            if let openmls::prelude::Proposal::Add(add) = proposal.proposal() {
                                // Controller ruling 3: a leaf that fails validation is left out of
                                // this commit (never committed, never a refusal that stalls this
                                // device's commits). A leaf whose signed history is not yet known
                                // still refuses, so the worker fetches it and retries.
                                match u.with_conn(|c| Ok(super::trust::validate_proposed_add(c,add.key_package())))? {
                                    Ok(()) => {}
                                    Err(e) if e.code == super::error::E_DEVICE_LIST_STALE => return Err(e),
                                    Err(_) => continue,
                                }
                            }
                            let reference = proposal.proposal_reference_ref().as_slice().to_vec();
                            let exists = u.with_conn(|c| Ok(c.query_row("SELECT 1 FROM app_proposals WHERE group_id=?1 AND ref=?2", params![id.as_slice(), &reference], |r| r.get::<_, i64>(0)).optional()?.is_some()))?;
                            if !exists {
                                group.store_pending_proposal(ctx.provider, *proposal)?;
                                u.with_conn(|c| { c.execute("INSERT INTO app_proposals (group_id,ref,epoch) VALUES(?1,?2,?3)", params![id.as_slice(), reference, group.epoch() as i64])?; Ok(()) })?;
                            }
                        }
                        Ok(_) => {}
                        Err(e) => {
                            let e: ClientError = e.into();
                            if e.code == E_CORE_STORAGE || e.code == E_CORE_RELOAD { return Err(e); }
                            group = DillaGroup::load(ctx.provider, &GroupId::from_slice(id))?.ok_or_else(absent)?;
                        }
                    }
                }
                if rotate {
                    group.withdraw_leave(ctx.provider)?;
                }
                // Ruling 3 for the whole queue: `self_update` commits every queued proposal,
                // including an Add that `group_apply` queued from a handshake row (it is never
                // validated there) or that validated when an earlier commit was built. A queued
                // Add whose leaf fails is taken out; one whose signed history is unknown refuses.
                group.drop_queued_adds(ctx.provider, |kp| {
                    match u.with_conn(|c| Ok(super::trust::validate_proposed_add(c, kp)))? {
                        Ok(()) => Ok(true),
                        Err(e) if e.code == super::error::E_DEVICE_LIST_STALE => Err(e),
                        Err(_) => Ok(false),
                    }
                })?;
                let epoch = group.epoch();
                let bundle = group.self_update(ctx.provider, ctx.signer.ok_or_else(|| ClientError::new(super::error::E_CORE_NO_IDENTITY, ""))?)?;
                u.with_conn(|c| set_pending(c, id, true))?;
                let info = bundle.group_info.ok_or_else(|| ClientError::new(E_CORE_MLS, "the staged commit carries no GroupInfo"))?;
                let mut out = Encoder::new();
                out.array(5).uint(epoch).bytes(&wire::tls(&bundle.commit)?).bytes(&wire::tls(&MlsMessageOut::from(info))?);
                out.array(bundle.welcomes.len());
                for (device, welcome) in &bundle.welcomes { out.array(2).bytes(device.as_bytes()).bytes(&wire::tls(welcome)?); }
                out.null();
                let result = out.into_vec();
                if let Some(intent) = intent {
                    super::bot_intents::check_result_size(&result)?;
                    u.with_conn(|c| super::bot_intents::persist(c, intent, id, &result))?;
                }
                Ok(result)
            });
            if result.is_ok() { this.keep_group(*id, group); }
            result
        })
    }

    pub fn commit_confirm(&mut self, id: &[u8; 16]) -> Result<Vec<u8>, ClientError> {
        self.commit_confirm_with_intent(id, None)
    }

    pub(super) fn commit_confirm_with_intent(
        &mut self,
        id: &[u8; 16],
        intent: Option<super::bot_intents::Resolution>,
    ) -> Result<Vec<u8>, ClientError> {
        if let Some(intent) = intent
            && super::bot_intents::already_resolved(self, intent)?
        {
            return Ok(Vec::new());
        }
        self.own()?;
        let row = self.read(|c| group_row(c, id))?.ok_or_else(absent)?;
        if row.state != STATE_ACTIVE {
            return Err(ClientError::new(
                E_CORE_STATE,
                format!("group state {}", row.state),
            ));
        }
        self.retry_reload(id, |this| {
            let mut group = this.take_group(id)?.ok_or_else(absent)?;
            let result = this.write(|ctx, u| {
                let had_pending_commit = group.has_pending_commit();
                if had_pending_commit {
                    group.merge_pending_commit(ctx.provider)?;
                }
                let held = checked("epoch", group.epoch())?;
                u.with_conn(|c| {
                    // A Bot result may be confirmed after group_apply already consumed that
                    // commit and advanced the stored group. In that case this finalizer owns no
                    // pending MLS state, so it must not erase proposals staged afterward or
                    // rewrite an epoch that has since advanced.
                    if had_pending_commit || intent.is_none() {
                        c.execute(
                            "DELETE FROM app_proposals WHERE group_id=?1",
                            [id.as_slice()],
                        )?;
                        raise_floor(c, id, held)?;
                        set_mls(c, id, held, false)?;
                    }
                    Ok(())
                })?;
                if let Some(intent) = intent {
                    super::bot_intents::resolve(u, intent)?;
                }
                Ok(wire::encode_apply_result(&wire::ApplyResult {
                    state: 2,
                    epoch: group.epoch(),
                    next_seq: row.next_seq as u64,
                    new_seqs: vec![],
                    proposals_pending: 0,
                    flags: 1,
                }))
            });
            if result.is_ok() {
                this.keep_group(*id, group);
            }
            result
        })
    }

    pub fn commit_abort(&mut self, id: &[u8; 16]) -> Result<(), ClientError> {
        self.commit_abort_with_intent(id, None)
    }

    pub(super) fn commit_abort_with_intent(
        &mut self,
        id: &[u8; 16],
        intent: Option<super::bot_intents::Resolution>,
    ) -> Result<(), ClientError> {
        if let Some(intent) = intent
            && super::bot_intents::already_resolved(self, intent)?
        {
            return Ok(());
        }
        self.own()?;
        self.read(|c| group_row(c, id))?.ok_or_else(absent)?;
        self.retry_reload(id, |this| {
            let Some(mut group) = this.take_group(id)? else {
                if let Some(intent) = intent {
                    return this.write(|_, u| super::bot_intents::resolve(u, intent));
                }
                return Ok(());
            };
            if !group.has_pending_commit() {
                this.keep_group(*id, group);
                if let Some(intent) = intent {
                    return this.write(|_, u| super::bot_intents::resolve(u, intent));
                }
                return Ok(());
            }
            let result = this.write(|ctx, u| {
                group.clear_pending_commit(ctx.provider)?;
                u.with_conn(|c| set_pending(c, id, false))?;
                if let Some(intent) = intent {
                    super::bot_intents::resolve(u, intent)?;
                }
                Ok(())
            });
            if result.is_ok() {
                this.keep_group(*id, group);
            }
            result
        })
    }

    pub fn cursor_body(&self, id: &[u8; 16]) -> Result<Vec<u8>, ClientError> {
        self.own()?;
        let (state, next_seq, acked_seq, epoch): (i64, i64, i64, i64) = self
            .read(|c| {
                c.query_row(
                    "SELECT state,next_seq,acked_seq,epoch FROM app_groups WHERE group_id=?1",
                    [id.as_slice()],
                    |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?, r.get(3)?)),
                )
                .optional()
                .map_err(Into::into)
            })?
            .ok_or_else(absent)?;
        if state != STATE_ACTIVE && state != STATE_NEEDS_RESYNC {
            return Err(ClientError::new(
                E_CORE_STATE,
                format!("group state {state}"),
            ));
        }
        let mut out = Encoder::new();
        if next_seq - 1 <= acked_seq {
            out.null();
        } else {
            out.array(2).uint((next_seq - 1) as u64).uint(epoch as u64);
        }
        Ok(out.into_vec())
    }

    pub fn cursor_acked(
        &mut self,
        id: &[u8; 16],
        last_seq: u64,
        last_epoch: u64,
    ) -> Result<(), ClientError> {
        self.own()?;
        let row = self.read(|c| group_row(c, id))?.ok_or_else(absent)?;
        if last_seq > (row.next_seq - 1) as u64 {
            return Err(ClientError::new(
                E_CORE_INPUT,
                "last_seq beyond the applied head",
            ));
        }
        let epoch = checked("last_epoch", last_epoch)?;
        self.write(|_, u| { u.with_conn(|c| { c.execute("UPDATE app_groups SET acked_seq=?2,acked_epoch=?3 WHERE group_id=?1 AND acked_seq<?2", params![id.as_slice(), last_seq as i64, epoch])?; Ok(()) })?; Ok(()) })
    }

    pub fn message_deleted(&mut self, id: &[u8; 16], seq: u64) -> Result<Vec<u8>, ClientError> {
        self.own()?;
        let seq_i = checked("seq", seq)?;
        let row = self.read(|c| group_row(c, id))?.ok_or_else(absent)?;
        if row.state == STATE_GONE {
            return Err(ClientError::new(E_CORE_STATE, "state 4"));
        }
        let (changed, proposals): (usize, i64) = self.write(|_, u| Ok(u.with_conn(|c| {
            let stored = stored_kind(c, id, seq)?;
            let changed = c.execute("UPDATE app_messages SET status=2,body='',envelope=NULL,reason='' WHERE group_id=?1 AND seq=?2 AND status<>2", params![id.as_slice(), seq_i])?;
            if changed > 0 {
                super::bot_events::suppress_source(c, id, seq_i)?;
            }
            c.execute("DELETE FROM app_pending_history WHERE group_id=?1 AND seq=?2", params![id.as_slice(), seq_i])?;
            if changed == 1 { refold_deleted(c, id, stored)?; }
            if changed == 1 { super::search::index_fold(c,id,seq)?; }
            let proposals = c.query_row("SELECT COUNT(*) FROM app_proposals WHERE group_id=?1", [id.as_slice()], |r| r.get(0))?;
            Ok((changed, proposals))
        })?))?;
        let loaded = if self.groups.contains_key(id) {
            None
        } else {
            DillaGroup::load(&self.provider, &GroupId::from_slice(id))?
        };
        let epoch = self
            .groups
            .get(id)
            .or(loaded.as_ref())
            .map_or(0, DillaGroup::epoch);
        Ok(wire::encode_apply_result(&wire::ApplyResult {
            state: row.state as u8,
            epoch,
            next_seq: row.next_seq as u64,
            new_seqs: if changed == 1 { vec![seq] } else { vec![] },
            proposals_pending: proposals as u64,
            flags: 0,
        }))
    }

    pub fn group_apply(
        &mut self,
        id: &[u8; 16],
        handshakes: &[u8],
        messages: &[u8],
        through: u64,
    ) -> Result<Vec<u8>, ClientError> {
        self.group_apply_scoped(id, handshakes, messages, through)
    }

    fn group_apply_scoped(
        &mut self,
        id: &[u8; 16],
        handshakes: &[u8],
        messages: &[u8],
        through: u64,
    ) -> Result<Vec<u8>, ClientError> {
        let own = self.own()?;
        let handshakes = wire::decode_handshake_rows(handshakes)?;
        let messages = wire::decode_message_rows(messages)?;
        let mut rows = BTreeMap::new();
        for h in handshakes {
            let seq = h.seq;
            if rows.insert(seq, Row::Handshake(h)).is_some() {
                return Err(ClientError::new(
                    E_CORE_INPUT,
                    format!("seq {seq} appears twice"),
                ));
            }
        }
        for m in messages {
            checked("messages: epoch", m.epoch)?;
            checked("messages: recv_ts", m.recv_ts)?;
            let seq = m.seq;
            if rows.insert(seq, Row::Message(m)).is_some() {
                return Err(ClientError::new(
                    E_CORE_INPUT,
                    format!("seq {seq} appears twice"),
                ));
            }
        }
        if through >= i64::MAX as u64 {
            return Err(ClientError::new(E_CORE_INPUT, "through out of range"));
        }
        let r = self.read(|c| group_row(c, id))?.ok_or_else(absent)?;
        if r.state != STATE_ACTIVE {
            return Err(ClientError::new(
                E_CORE_STATE,
                format!("group state {}", r.state),
            ));
        }
        // L-CORE-52: a call group's application messages are skipped, never decrypted or folded.
        let call = r.kind == super::groups::row_kind(crate::mls::GroupKind::Call);
        let mut next = r.next_seq as u64;
        let mut new_seqs = Vec::new();
        let mut flags = 0u8;
        let mut stopped = false;
        for (seq, row) in rows {
            if seq < next || seq > through {
                continue;
            }
            checked("seq", seq)?;
            let (changed, adopted, epoch_changed, stop) = self.retry_reload(id, |this| {
                let mut group = this.take_group(id)?.ok_or_else(absent)?;
                let mut mls_failed = false;
                let result = this.write(|ctx, u| {
                    let mut changed = false;
                    let mut adopted = false;
                    let mut epoch_changed = false;
                    let mut stop = false;
                    match &row {
                        Row::Handshake(h) => {
                            let hs_epoch = checked("handshakes: epoch", h.epoch)?;
                            u.with_conn(|c| {
                                c.execute("INSERT OR REPLACE INTO app_handshake_tail (group_id,seq,epoch,kind,sender,blob) VALUES(?1,?2,?3,?4,?5,?6)",
                                    params![id.as_slice(), seq as i64, hs_epoch, h.kind as i64, h.sender.map(i64::from), &h.blob])?;
                                c.execute("DELETE FROM app_handshake_tail WHERE group_id=?1 AND seq NOT IN (SELECT seq FROM app_handshake_tail WHERE group_id=?1 ORDER BY seq DESC LIMIT ?2)",
                                    params![id.as_slice(), super::schema::HANDSHAKE_TAIL as i64])?;
                                Ok(())
                            })?;
                            let attempt = (|| -> Result<bool, ClientError> {
                                let message = wire::protocol_message(&h.blob)?;
                                if message.epoch().as_u64() < group.epoch() { return Ok(false); }
                                match group.process_message(ctx.provider, message)? {
                                    DillaProcessed::StagedCommit(commit, author) => {
                                        if commit.self_removed() {
                                            // The epoch this device held last is already the
                                            // floor (F6): it was written when it was reached.
                                            group.delete(ctx.provider)?;
                                            u.with_conn(|c| {
                                                c.execute("DELETE FROM app_proposals WHERE group_id=?1", [id.as_slice()])?;
                                                c.execute(
                                                    "UPDATE app_groups SET state=4,next_seq=?2,epoch=0,pending_commit=0 WHERE group_id=?1",
                                                    params![id.as_slice(), seq as i64 + 1],
                                                )?;
                                                Ok(())
                                            })?;
                                            epoch_changed = true;
                                            return Ok(true);
                                        }
                                        // The clear is persisted with its column at once: a merge
                                        // that then fails is caught below and commits the unit
                                        // (pre-flight ruling (c)).
                                        if group.has_pending_commit() {
                                            group.clear_pending_commit(ctx.provider)?;
                                            u.with_conn(|c| set_pending(c, id, false))?;
                                        }
                                        let old_devices = super::trust::member_devices(&group);
                                        let old_leaves = super::trust::member_fingerprints(&group);
                                        group.merge_staged_commit(ctx.provider, *commit)?;
                                        let current_devices = super::trust::member_devices(&group);
                                        u.with_conn(|c| {
                                            super::trust::audit_tree(c,&group,id,&own.user_id,&own.device_id,Some((&author.0,&author.1)),Some(&old_leaves))?;
                                            super::trust::clear_removed_blocks(c,id,&old_devices,&current_devices)?;
                                            Ok(())
                                        })?;
                                        let held = checked("epoch", group.epoch())?;
                                        u.with_conn(|c| { c.execute("DELETE FROM app_proposals WHERE group_id=?1", [id.as_slice()])?; raise_floor(c, id, held)?; set_mls(c, id, held, false) })?;
                                        epoch_changed = true;
                                    }
                                    DillaProcessed::OwnPendingCommit => {
                                        let old_leaves = super::trust::member_fingerprints(&group);
                                        group.merge_pending_commit(ctx.provider)?;
                                        u.with_conn(|c| super::trust::audit_tree(c,&group,id,&own.user_id,&own.device_id,Some((&own.user_id,&own.device_id)),Some(&old_leaves)))?;
                                        let held = checked("epoch", group.epoch())?;
                                        u.with_conn(|c| { c.execute("DELETE FROM app_proposals WHERE group_id=?1", [id.as_slice()])?; raise_floor(c, id, held)?; set_mls(c, id, held, false) })?;
                                        epoch_changed = true;
                                    }
                                    DillaProcessed::Proposal(proposal) => {
                                        let reference = proposal.proposal_reference_ref().as_slice().to_vec();
                                        let exists: bool = u.with_conn(|c| Ok(c.query_row("SELECT 1 FROM app_proposals WHERE group_id=?1 AND ref=?2", params![id.as_slice(), &reference], |r| r.get::<_, i64>(0)).optional()?.is_some()))?;
                                        if !exists {
                                            group.store_pending_proposal(ctx.provider, *proposal)?;
                                            u.with_conn(|c| { c.execute("INSERT INTO app_proposals (group_id,ref,epoch) VALUES(?1,?2,?3)", params![id.as_slice(), reference, group.epoch() as i64])?; Ok(()) })?;
                                        }
                                    }
                                    _ => {}
                                }
                                Ok(false)
                            })();
                            match attempt {
                                Ok(removed) => { stop = removed; }
                                Err(e) if e.code == E_CORE_STORAGE || e.code == E_CORE_RELOAD => return Err(e),
                                Err(_) if h.kind == 1 || h.kind == 2 => {
                                    u.with_conn(|c| { c.execute("UPDATE app_groups SET state=3 WHERE group_id=?1", [id.as_slice()])?; Ok(()) })?;
                                    stop = true;
                                    mls_failed = true;
                                }
                                Err(_) => { mls_failed = true; }
                            }
                        }
                        Row::Message(m) => {
                            // Do not borrow the connection across an OpenMLS call: `with_conn`
                            // releases it before process_message takes the storage handle.
                            // A row already stored at this seq (an own row confirmed ahead of
                            // next_seq, or one the server serves twice) is skipped before any
                            // MLS processing; rule 2 alone updates a stored row.
                            let occupied = call
                                || !m.deleted && u.with_conn(|c| message_stored(c, id, m.seq))?;
                            if occupied {
                                // Skipped: not listed; next_seq moves past it below.
                            } else if m.deleted
                                || m.blob.is_none()
                                || m.uploader_device == own.device_id
                            {
                                (changed, adopted) =
                                    u.with_conn(|c| apply_message(c, id, m, &own, &group))?;
                            } else {
                                let parsed =
                                    wire::protocol_message(m.blob.as_deref().unwrap_or_default());
                                let processed = parsed.and_then(|message| {
                                    group
                                        .process_message(ctx.provider, message)
                                        .map_err(Into::into)
                                });
                                let (status, reason, application, pending_application) = match processed {
                                    // Neither shown nor folded, as a sender mismatch; checked before the
                                    // sender's trust, so a lying row never waits as pending history.
                                    Ok(DillaProcessed::Application(r))
                                        if r.sender.device_id.as_bytes() == &m.uploader_device
                                            && class_lies(m.class, r.envelope.kind) =>
                                    {
                                        (STATUS_CANNOT_DECRYPT, REASON_CLASS_MISMATCH, None, None)
                                    }
                                    Ok(DillaProcessed::Application(r))
                                        if r.sender.device_id.as_bytes() == &m.uploader_device =>
                                    {
                                        let verdict = u.with_conn(|c| Ok(super::trust::validate_cached_sender(c,&group,id,&r)))?;
                                        match verdict {
                                            Ok(()) => (STATUS_OK, "", Some(r), None),
                                            Err(e) => {
                                                if e.code != super::error::E_DEVICE_LIST_STALE {
                                                    u.with_conn(|c| super::trust::audit_sender(c,&group,id,&own.user_id,&own.device_id,r.sender_leaf))?;
                                                }
                                                // The MLS-authenticated evidence waits, unreadable: it is
                                                // released only if this exact leaf later validates (a list
                                                // naming it, or exact-key verification of its key). A
                                                // revoked device's message is final and keeps nothing.
                                                let kept = (e.code != super::error::E_DEVICE_REVOKED).then_some(r);
                                                (STATUS_CANNOT_DECRYPT, e.code, None, kept)
                                            },
                                        }
                                    }
                                    Ok(DillaProcessed::Application(_)) => {
                                        (STATUS_CANNOT_DECRYPT, REASON_SENDER_MISMATCH, None, None)
                                    }
                                    Ok(_) => (STATUS_CANNOT_DECRYPT, E_CORE_MLS, None, None),
                                    Err(err) => {
                                        if err.code == E_CORE_STORAGE || err.code == E_CORE_RELOAD {
                                            return Err(err);
                                        }
                                        mls_failed = true;
                                        (STATUS_CANNOT_DECRYPT, err.code, None, None)
                                    }
                                };
                                let bytes = application
                                    .as_ref()
                                    .map(|r| r.envelope.encode())
                                    .transpose()?;
                                u.with_conn(|c| {
                                    if super::removals::is_tombstoned(c, id, m.seq)? {
                                        if let Some(r) = application.as_ref() {
                                            let sender_sig_key = group.member_at(r.sender_leaf).map(|member| member.signature_key);
                                            let marker = StoredMessage {
                                                group_id: id, seq: m.seq, epoch: r.epoch, recv_ts: m.recv_ts,
                                                status: STATUS_DELETED, reason: "", sender_user: Some(r.sender.user_id.as_bytes()),
                                                sender_device: r.sender.device_id.as_bytes(), sender_leaf: Some(r.sender_leaf),
                                                sender_sig_key: sender_sig_key.as_deref(),
                                                sender_kind: Some(r.sender.kind.as_u8()), sender_tier: Some(r.sender.tier.as_u8()),
                                                msg_id: Some(r.envelope.msg_id.as_bytes()), ty: Some(r.envelope.kind.as_u8()),
                                                reply_to: r.envelope.reply_to.as_ref().map(MsgId::as_bytes),
                                                thread_id: r.envelope.thread_id.as_ref().map(MsgId::as_bytes),
                                                body: "", envelope: None, franking_tag: &m.franking_tag, mention: false,
                                            };
                                            insert_message(c, &marker)?;
                                            if r.envelope.kind.as_u8() == 0 {
                                                c.execute("UPDATE app_messages SET body='',envelope=NULL,status=2,reason='' WHERE group_id=?1 AND reply_to=?2 AND type=1",
                                                    params![id.as_slice(), r.envelope.msg_id.as_bytes().as_slice()])?;
                                            }
                                        } else {
                                            insert_message(c, &empty_message(id, m, STATUS_DELETED, ""))?;
                                        }
                                        super::search::index_fold(c, id, m.seq)?;
                                        return Ok(());
                                    }
                                    if let Some(r) = application.as_ref() {
                                        let roles = own_roles_of(c, id)?;
                                        let mentionable = mentionable_of(c, id)?;
                                        let broadcast = broadcast_ok(c, id, r.sender.user_id.as_bytes())?;
                                        let sender_member = group.member_at(r.sender_leaf);
                                        let msg = StoredMessage {
                                            group_id: id,
                                            seq: m.seq,
                                            epoch: r.epoch,
                                            recv_ts: m.recv_ts,
                                            status,
                                            reason,
                                            sender_user: Some(r.sender.user_id.as_bytes()),
                                            sender_device: r.sender.device_id.as_bytes(),
                                            sender_leaf: Some(r.sender_leaf),
                                            sender_sig_key: sender_member.as_ref().map(|member| member.signature_key.as_slice()),
                                            sender_kind: Some(r.sender.kind.as_u8()),
                                            sender_tier: Some(r.sender.tier.as_u8()),
                                            msg_id: Some(r.envelope.msg_id.as_bytes()),
                                            ty: Some(r.envelope.kind.as_u8()),
                                            reply_to: r.envelope.reply_to.as_ref().map(MsgId::as_bytes),
                                            thread_id: r.envelope.thread_id.as_ref().map(MsgId::as_bytes),
                                            body: &r.envelope.body,
                                            envelope: bytes.as_deref(),
                                            franking_tag: &m.franking_tag,
                                            mention: r.envelope.kind.as_u8() == 0
                                                && r.sender.user_id.as_bytes() != &own.user_id
                                                && mentions_me_gated(&r.envelope.body, &own.user_id, &roles, &mentionable, broadcast),
                                        };
                                        insert_message(c, &msg)?;
                                        if status == STATUS_OK
                                            && let Some(member) = sender_member.as_ref()
                                        {
                                            super::bot_events::record_accepted(
                                                c, &group, id, m.seq, &own,
                                                &r.sender, r.sender_leaf, &member.signature_key,
                                                r.epoch, m.recv_ts, r.envelope.msg_id.as_bytes(),
                                                r.envelope.kind.as_u8(), &r.envelope.body,
                                                r.envelope.reply_to.as_ref().map(|v| v.as_bytes()),
                                                r.envelope.thread_id.as_ref().map(|v| v.as_bytes()),
                                            )?;
                                        }
                                        refold_insert(c, id, m.seq, &r.envelope, r.sender.user_id.as_bytes(), r.sender.device_id.as_bytes(), Some(&own))?;
                                    } else {
                                        let mut empty = empty_message(id, m, status, reason);
                                        if reason == REASON_CLASS_MISMATCH {
                                            empty.ty = Some(TYPE_CLASS_MISMATCH as u8);
                                        }
                                        insert_message(c, &empty)?;
                                        if let Some(r) = pending_application.as_ref() {
                                            c.execute("INSERT INTO app_pending_history(group_id,user_id,device_id,seq,ciphertext,envelope,sender_leaf,sender_credential) VALUES(?1,?2,?3,?4,?5,?6,?7,?8)",
                                                params![id.as_slice(),r.sender.user_id.as_bytes(),r.sender.device_id.as_bytes(),m.seq as i64,m.blob.as_deref().unwrap_or_default(),
                                                    r.envelope.encode().map_err(|e| StorageError::Codec(e.to_string()))?,
                                                    i64::from(r.sender_leaf),r.sender.encode()])?;
                                        }
                                    }
                                    Ok(())
                                })?;
                                changed = true;
                            }
                        }
                    }
                    if !stop || matches!(&row, Row::Handshake(_)) && !mls_failed {
                        u.with_conn(|c| { c.execute("UPDATE app_groups SET next_seq=?2 WHERE group_id=?1", params![id.as_slice(), seq as i64 + 1])?; Ok(()) })?;
                    }
                    Ok((changed, adopted, epoch_changed, stop))
                });
                if result.is_ok() && !mls_failed && !matches!(&result, Ok((_,_,_,true))) {
                    this.keep_group(*id, group);
                }
                result
            })?;
            if !stop || !matches!(&row, Row::Handshake(_)) {
                next = seq + 1;
            }
            if changed {
                new_seqs.push(seq);
            }
            if adopted {
                flags |= 2;
            }
            if epoch_changed {
                flags |= 1;
            }
            if stop {
                stopped = true;
                break;
            }
        }
        if !stopped && through + 1 > next {
            self.write(|_, u| {
                u.with_conn(|c| {
                    c.execute(
                        "UPDATE app_groups SET next_seq=?2 WHERE group_id=?1 AND next_seq<?2",
                        params![id.as_slice(), through as i64 + 1],
                    )?;
                    Ok(())
                })?;
                Ok(())
            })?;
        }
        let row = self.read(|c| group_row(c, id))?.ok_or_else(absent)?;
        let group = self.groups.get(id);
        let loaded = if group.is_none() {
            DillaGroup::load(&self.provider, &openmls::prelude::GroupId::from_slice(id))?
        } else {
            None
        };
        let epoch = group.or(loaded.as_ref()).map_or(0, DillaGroup::epoch);
        let proposals: u64 = self.read(|c| {
            c.query_row(
                "SELECT COUNT(*) FROM app_proposals WHERE group_id=?1",
                [id.as_slice()],
                |r| r.get::<_, i64>(0),
            )
            .map(|v| v as u64)
            .map_err(Into::into)
        })?;
        Ok(wire::encode_apply_result(&wire::ApplyResult {
            state: row.state as u8,
            epoch,
            next_seq: row.next_seq as u64,
            new_seqs,
            proposals_pending: proposals,
            flags,
        }))
    }
}
