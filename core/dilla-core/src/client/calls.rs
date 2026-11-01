//! Call groups in the browser core (L-CORE-50): create and join a call group, read each epoch's
//! media key only through the call gate, leave, rotate and forget.
//!
//! The gate is the core's answer to "may this device's audio reach every leaf of this tree": a
//! stored failed or changed-key verdict of a leaf (the send block), a leaf that does not validate
//! against its cached signed list now (pending included: ruling RV-1), and for a DM's call group
//! the send block of the DM's text group (decision 4). It runs in the core, on the core's own live
//! tree, so neither the worker nor the instance can skip it, and before any key byte is derived.

use super::error::{E_BINDING, E_DEVICE_LIST_STALE, E_UMK_CHANGED};
use super::error::{E_CORE_INPUT, E_CORE_NO_IDENTITY, E_CORE_NOT_FOUND, E_CORE_STATE};
use super::groups::{
    GroupRow, STATE_ACTIVE, STATE_GONE, STATE_JOINING, group_row, holder, row_kind,
};
use super::{ClientCore, ClientError, Own, trust, wire};
use crate::cbor::Encoder;
use crate::identity::{CredentialIdentity, DISPLAY_GROUP, group_digits, sas};
use crate::mls::{DillaGroup, DillaProvider, GroupKind, StorageError, UnitScope};
use openmls::prelude::{BasicCredential, GroupId};
use rusqlite::params;

/// What gate step 3 reads: the DM text group (its id and its MLS state when it loads), none, or
/// the refusal of a call group whose community disagrees with the text group held for its target.
type TextGroup<'a> = Result<Option<([u8; 16], Option<&'a DillaGroup>)>, &'static str>;

/// The DM text group of a call group (review M3). Whether a call belongs to a DM is read from the
/// text group this device holds for the call's target, not from the call group's own binding,
/// which its creator chose: a call group whose community differs from that text group's is
/// refused (`E_BINDING`), so a DM's call bound to a made-up community cannot pass as a voice
/// channel and skip decision 4. `Ok(Some(id))` for a DM (both bindings without a community);
/// `Ok(None)` when no text group is held for the target, or for a voice channel whose text group
/// agrees.
fn dm_text_id(
    c: &rusqlite::Connection,
    group: &DillaGroup,
    id: &[u8; 16],
) -> Result<Result<Option<[u8; 16]>, &'static str>, StorageError> {
    let Ok(binding) = group.binding() else {
        return Ok(Err(E_BINDING));
    };
    let Some(text_id) = holder(c, &binding.target_id, 0, id)? else {
        return Ok(Ok(None));
    };
    let text_community = group_row(c, &text_id)?.and_then(|r| r.community_id);
    let call_community = binding.community_id.as_ref().map(|c| c.as_bytes().to_vec());
    if text_community != call_community {
        return Ok(Err(E_BINDING));
    }
    Ok(Ok(call_community.is_none().then_some(text_id)))
}

/// The text group's MLS state for step 3, loaded only when the cache does not hold it; a state
/// that does not load answers from its stored verdicts (fail closed).
fn load_text(
    provider: &DillaProvider,
    dm: &Result<Option<[u8; 16]>, &'static str>,
) -> Option<DillaGroup> {
    match dm {
        Ok(Some(t)) => DillaGroup::load(provider, &GroupId::from_slice(t)).unwrap_or(None),
        _ => None,
    }
}

/// The call gate, in the ledger's order; the answer is the refusal code. With `stale_ok` (the join
/// before its commit is posted) a pending leaf alone passes step 2: `call_join_check` settles it.
fn gate_verdict(
    c: &rusqlite::Connection,
    group: &DillaGroup,
    id: &[u8; 16],
    own: &Own,
    text: TextGroup<'_>,
    stale_ok: bool,
) -> Result<Result<(), &'static str>, StorageError> {
    // 1. The send block: a stored failed or changed-key verdict of a leaf of this tree.
    if let Some(code) = trust::send_block(c, group, id)? {
        return Ok(Err(code));
    }
    // 2. Every other leaf validates against its cached signed list now; pending refuses.
    match trust::call_leaves_verdict(c, group, id, &own.user_id, &own.device_id)? {
        Ok(()) => {}
        Err(E_DEVICE_LIST_STALE) if stale_ok => {}
        Err(code) => return Ok(Err(code)),
    }
    // 3. A DM's call group refuses while the DM's text group is send-blocked (decision 4), and a
    // call group whose community disagrees with the text group held for its target is refused.
    let text = match text {
        Ok(text) => text,
        Err(code) => return Ok(Err(code)),
    };
    if let Some((text_id, text_group)) = text {
        let block = match text_group {
            Some(g) => trust::send_block(c, g, &text_id)?,
            None => trust::stored_block(c, &text_id)?,
        };
        if let Some(code) = block {
            return Ok(Err(code));
        }
    }
    Ok(Ok(()))
}

/// The call gate of a join, inside the join's unit right after the audit of the joined tree
/// (`group_join_kind`): an `Err` rolls the whole unit back, so no row, verdict or MLS state of the
/// refused join survives and no external commit leaves the core.
pub(super) fn join_gate(
    provider: &DillaProvider,
    u: &UnitScope<'_>,
    group: &DillaGroup,
    id: &[u8; 16],
    own: &Own,
) -> Result<(), ClientError> {
    let dm = u.with_conn(|c| dm_text_id(c, group, id))?;
    let loaded = load_text(provider, &dm);
    let text = dm.map(|t| t.map(|t| (t, loaded.as_ref())));
    u.with_conn(|c| {
        Ok(match gate_verdict(c, group, id, own, text, true)? {
            Ok(()) => Ok(()),
            Err(code) => Err(refusal(c, group, own, code)?),
        })
    })?
}

fn refused(code: &'static str) -> ClientError {
    ClientError::new(code, "")
}

/// The gate's refusal. An `E_UMK_CHANGED` names the contact behind it when the tree shows one
/// (web-3 task 10, concern 3): its detail is the user id (hex) of the first leaf by user id, this
/// device's own user aside, whose pin holds a changed key — the contact the person must compare
/// safety numbers with, whether or not they ever entered the call's room. Empty when no leaf of
/// this tree is such a contact (a DM's call refused for its text group, decision 4).
fn refusal(
    c: &rusqlite::Connection,
    group: &DillaGroup,
    own: &Own,
    code: &'static str,
) -> Result<ClientError, StorageError> {
    if code != E_UMK_CHANGED {
        return Ok(refused(code));
    }
    let mut users: Vec<[u8; 16]> = group
        .members()
        .into_iter()
        .filter_map(|m| {
            BasicCredential::try_from(m.credential)
                .ok()
                .and_then(|basic| CredentialIdentity::decode(basic.identity()).ok())
                .map(|identity| *identity.user_id.as_bytes())
        })
        .filter(|u| *u != own.user_id)
        .collect();
    users.sort_unstable();
    users.dedup();
    let mut stmt = c.prepare_cached(
        "SELECT 1 FROM app_trust_pins WHERE user_id=?1 AND changed_umk IS NOT NULL",
    )?;
    for user in users {
        if stmt.exists([user.as_slice()])? {
            let hex: String = user.iter().map(|b| format!("{b:02x}")).collect();
            return Ok(ClientError::new(code, hex));
        }
    }
    Ok(refused(code))
}

fn not_call() -> ClientError {
    ClientError::new(E_CORE_INPUT, "not a call group")
}

fn state(n: i64) -> ClientError {
    ClientError::new(E_CORE_STATE, format!("group state {n}"))
}

impl ClientCore {
    /// `group_create` for a call group: kind call, media version 1, the row written with kind 1.
    pub fn call_group_create(
        &mut self,
        id: &[u8; 16],
        community: Option<&[u8; 16]>,
        channel: &[u8; 16],
        policy: u64,
        external_sender_pub: &[u8; 32],
    ) -> Result<Vec<u8>, ClientError> {
        self.group_create_kind(
            id,
            community,
            channel,
            policy,
            external_sender_pub,
            GroupKind::Call,
        )
    }

    /// `group_join_external` for a call group, gated in the join's unit (`join_gate`): a pending
    /// leaf is tolerated (the group is staged, the commit returned unposted for `call_join_check`),
    /// every other refusal rolls the join back.
    pub fn call_group_join_external(
        &mut self,
        id: &[u8; 16],
        community: Option<&[u8; 16]>,
        channel: &[u8; 16],
        policy: u64,
        info_body: &[u8],
        tree_body: &[u8],
    ) -> Result<Vec<u8>, ClientError> {
        self.group_join_kind(
            id,
            community,
            channel,
            policy,
            info_body,
            tree_body,
            GroupKind::Call,
        )
    }

    /// The row of a call group: `E_CORE_NOT_FOUND` for an unknown id, `E_CORE_INPUT` for a group
    /// that is not a call group.
    fn call_row(&self, id: &[u8; 16]) -> Result<GroupRow, ClientError> {
        let row = self
            .read(|c| group_row(c, id))?
            .ok_or_else(|| ClientError::new(E_CORE_NOT_FOUND, ""))?;
        if row.kind != row_kind(GroupKind::Call) {
            return Err(not_call());
        }
        Ok(row)
    }

    /// The call group's MLS state: the cache, else the store, checked against its row.
    fn call_group<'a>(
        &'a self,
        id: &[u8; 16],
        row: &GroupRow,
        slot: &'a mut Option<DillaGroup>,
    ) -> Result<&'a DillaGroup, ClientError> {
        if let Some(group) = self.groups.get(id) {
            return Ok(group);
        }
        let group = DillaGroup::load(&self.provider, &GroupId::from_slice(id))?
            .ok_or_else(|| ClientError::new(E_CORE_NOT_FOUND, ""))?;
        if !super::matches_row(&group, id, row, &self.own()?) {
            return Err(super::mismatched());
        }
        Ok(slot.insert(group))
    }

    /// The call gate over the core's own live tree (`gate_verdict`, nothing tolerated).
    fn call_gate(&self, group: &DillaGroup, id: &[u8; 16]) -> Result<(), ClientError> {
        let own = self.own()?;
        let dm = self.read(|c| dm_text_id(c, group, id))?;
        let cached = matches!(dm, Ok(Some(t)) if self.groups.contains_key(&t));
        let loaded = if cached {
            None
        } else {
            load_text(&self.provider, &dm)
        };
        let text = dm.map(|t| t.map(|t| (t, self.groups.get(&t).or(loaded.as_ref()))));
        self.read(|c| {
            Ok(match gate_verdict(c, group, id, &own, text, false)? {
                Ok(()) => Ok(()),
                Err(code) => Err(refusal(c, group, &own, code)?),
            })
        })?
    }

    /// CBOR `[epoch, base_key(16), own_leaf, [[leaf, device_id, user_id]], code]` of the active call
    /// group's current epoch, where `code` is the SAS of its `epoch_authenticator` as 6 groups of
    /// 5 digits. Refused by the call gate before any key byte is derived.
    pub fn call_media_epoch(&self, id: &[u8; 16]) -> Result<Vec<u8>, ClientError> {
        let row = self.call_row(id)?;
        if row.state != STATE_ACTIVE {
            return Err(state(row.state));
        }
        let mut slot = None;
        let group = self.call_group(id, &row, &mut slot)?;
        self.call_gate(group, id)?;
        let media = group.media_epoch(&self.provider)?;
        let code = group_digits(&sas(&group.epoch_authenticator()?), DISPLAY_GROUP);
        // Sized up front so the buffer holding the key never reallocates and leaves a copy behind.
        let mut e = Encoder::with_capacity(96 + 48 * media.roster.len());
        e.array(5)
            .uint(media.epoch)
            .bytes(&media.base_key[..])
            .uint(u64::from(media.own_leaf));
        e.array(media.roster.len());
        for entry in &media.roster {
            e.array(3)
                .uint(u64::from(entry.leaf_index))
                .bytes(entry.device_id.as_bytes())
                .bytes(entry.user_id.as_bytes());
        }
        e.text(&code);
        Ok(e.into_vec())
    }

    /// The full call gate on a staged call group (joined locally, its external commit not yet
    /// posted): the worker posts the commit only when this passes (ruling RV-1). `E_CORE_STATE`
    /// for a call group in any other state.
    pub fn call_join_check(&self, id: &[u8; 16]) -> Result<(), ClientError> {
        let row = self.call_row(id)?;
        if row.state != STATE_JOINING {
            return Err(state(row.state));
        }
        let mut slot = None;
        let group = self.call_group(id, &row, &mut slot)?;
        self.call_gate(group, id)
    }

    /// CBOR `[epoch, proposal]`: a `Remove` of this device's own leaf, for
    /// `POST /v1/groups/{id}/proposal`. Another member commits it.
    pub fn call_leave_proposal(&mut self, id: &[u8; 16]) -> Result<Vec<u8>, ClientError> {
        self.own()?;
        let row = self.call_row(id)?;
        if row.state != STATE_ACTIVE {
            return Err(state(row.state));
        }
        self.signer()?;
        self.retry_reload(id, |this| {
            let mut group = this
                .take_group(id)?
                .ok_or_else(|| ClientError::new(E_CORE_NOT_FOUND, ""))?;
            let result = this.write(|ctx, _| {
                let signer = ctx
                    .signer
                    .ok_or_else(|| ClientError::new(E_CORE_NO_IDENTITY, ""))?;
                let epoch = group.epoch();
                let proposal = wire::tls(&group.leave(ctx.provider, signer)?)?;
                let mut e = Encoder::new();
                e.array(2).uint(epoch).bytes(&proposal);
                Ok(e.into_vec())
            });
            if result.is_ok() {
                this.keep_group(*id, group);
            }
            result
        })
    }

    /// An empty commit with an UpdatePath (a fresh leaf secret), in `commit_build`'s body shape;
    /// confirmed with `commit_confirm` or dropped with `commit_abort`.
    ///
    /// **It cancels a pending leave.** This device's own `Remove` of its own leaf
    /// (`call_leave_proposal`) is taken out of its queue first, because OpenMLS refuses to commit a
    /// self-removal. Once the rotation is committed the posted proposal is stale and this device
    /// stays a member, so a worker that has posted a leave must not rotate (it forgets instead).
    pub fn call_rotate(&mut self, id: &[u8; 16]) -> Result<Vec<u8>, ClientError> {
        self.call_row(id)?;
        // An empty `[proposals]` body: the rotation commits only what is already queued.
        self.commit_build_inner(id, &[0x80], true)
    }

    /// Deletes the call group's MLS state, its proposals, handshake tail and every per-group trust
    /// row (stored verdicts, audit rows, held evidence), and sets its row to gone (state 4); its
    /// channel's next call group can then be created or joined. A gone row stays gone.
    pub fn call_group_forget(&mut self, id: &[u8; 16]) -> Result<(), ClientError> {
        self.own()?;
        let row = self.call_row(id)?;
        if row.state == STATE_GONE {
            return self.write(|_, u| {
                u.with_conn(|c| super::bot_intents::discard_pending_for_forgotten_group(c, id))?;
                Ok(())
            });
        }
        let mut group = self.take_group_unchecked(id)?;
        self.write(|ctx, u| {
            if let Some(g) = group.as_mut() {
                g.delete(ctx.provider)?;
            }
            u.with_conn(|c| {
                super::bot_intents::discard_pending_for_forgotten_group(c, id)?;
                c.execute(
                    "DELETE FROM app_proposals WHERE group_id=?1",
                    [id.as_slice()],
                )?;
                for table in [
                    "app_handshake_tail",
                    "app_quarantine",
                    "app_trust_audit",
                    "app_pending_history",
                ] {
                    c.execute(
                        &format!("DELETE FROM {table} WHERE group_id=?1"),
                        [id.as_slice()],
                    )?;
                }
                c.execute(
                    "UPDATE app_groups SET state=?2,resync=0,was_gone=0,epoch=0,pending_commit=0 \
                     WHERE group_id=?1",
                    params![id.as_slice(), STATE_GONE],
                )?;
                Ok(())
            })?;
            Ok(())
        })
    }
}
