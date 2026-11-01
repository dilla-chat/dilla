//! Durable native Bot upload intents for MLS group operations.

use super::error::{E_CORE_INPUT, E_CORE_LIMIT, E_CORE_STATE};
use super::{ClientCore, ClientError, bot_identity, schema};
use crate::cbor::Encoder;
use crate::identity::sha256;
use crate::mls::{GroupKind, StorageError, UnitScope};
use rusqlite::{OptionalExtension, params};

// Reserve enough for the public intent-result tuple plus its native [body] wrapper within the
// 32 MiB logical result transport. This is an encoded MLS/public result limit, not a frame cap.
const MAX_INTENT_RESULT: usize = 32 * 1024 * 1024 - 64;
type RetryRow = (Vec<u8>, Vec<u8>, i64, Vec<u8>, Option<Vec<u8>>);
type ResolutionRow = (Vec<u8>, Vec<u8>, i64, i64, Option<Vec<u8>>);
type IntentResultRow = (i64, Vec<u8>, i64, Option<Vec<u8>>);

#[derive(Clone, Copy)]
pub(super) struct IntentWrite {
    pub operation_id: [u8; 16],
    pub installation_id: [u8; 16],
    pub kind: u64,
    pub input_hash: [u8; 32],
}

#[derive(Clone, Copy)]
pub(super) struct Binding {
    pub community_id: [u8; 16],
    pub installation_id: [u8; 16],
}

#[derive(Clone, Copy)]
pub(super) struct Resolution {
    pub operation_id: [u8; 16],
    pub installation_id: [u8; 16],
    pub group_id: [u8; 16],
    pub kind: u64,
    pub state: u64,
    pub hash: [u8; 32],
}

fn conflict() -> ClientError {
    ClientError::new(
        E_CORE_STATE,
        "bot upload intent conflicts with an existing operation",
    )
}

pub(super) fn binding(core: &ClientCore) -> Result<Binding, ClientError> {
    let own = core.own()?;
    if own.kind != 1 || own.tier != 0 {
        return Err(ClientError::new(
            E_CORE_STATE,
            "native Bot identity required",
        ));
    }
    let raw = core.read(|c| schema::meta_get(c, schema::BOT_BINDING))?;
    let Some(raw) = raw else {
        return Err(ClientError::new(
            E_CORE_STATE,
            "native Bot binding required",
        ));
    };
    let b = bot_identity::decode_binding(&raw)?;
    if !bot_identity::nonzero_ids(&b) || b.instance_id != own.instance_id {
        return Err(ClientError::new(
            E_CORE_STATE,
            "native Bot binding mismatch",
        ));
    }
    Ok(Binding {
        community_id: b.community_id,
        installation_id: b.installation_id,
    })
}

pub(super) fn group_kind(value: u64) -> Result<GroupKind, ClientError> {
    match value {
        0 => Ok(GroupKind::Text),
        1 => Ok(GroupKind::Call),
        _ => Err(ClientError::new(
            E_CORE_INPUT,
            "bot group kind must be text or call",
        )),
    }
}

fn validate_group_scope(
    core: &ClientCore,
    group_id: &[u8; 16],
    binding: Binding,
) -> Result<(), ClientError> {
    let row = core
        .read(|c| super::groups::group_row(c, group_id))?
        .ok_or_else(|| ClientError::new(E_CORE_STATE, "bot commit group is absent"))?;
    if !matches!(row.kind, 0 | 1)
        || row.community_id.as_deref() != Some(binding.community_id.as_slice())
    {
        return Err(ClientError::new(
            E_CORE_STATE,
            "bot upload intent group is outside its installation scope",
        ));
    }
    Ok(())
}

pub(super) fn operation(operation_id: &[u8; 16]) -> Result<(), ClientError> {
    if operation_id == &[0; 16] {
        Err(ClientError::new(
            E_CORE_INPUT,
            "operation id must be nonzero",
        ))
    } else {
        Ok(())
    }
}

pub(super) fn write(
    operation_id: &[u8; 16],
    installation_id: [u8; 16],
    kind: u64,
    args: impl FnOnce(&mut Encoder),
) -> Result<IntentWrite, ClientError> {
    operation(operation_id)?;
    let mut e = Encoder::new();
    e.array(2).uint(kind);
    args(&mut e);
    Ok(IntentWrite {
        operation_id: *operation_id,
        installation_id,
        kind,
        input_hash: sha256(&e.into_vec()),
    })
}

pub(super) fn retry(
    core: &ClientCore,
    intent: IntentWrite,
    group_id: &[u8; 16],
) -> Result<Option<Vec<u8>>, ClientError> {
    let found: Option<RetryRow> = core.read(|c| {
        c.query_row(
            "SELECT installation_id,group_id,kind,input_hash,result FROM app_bot_intents WHERE operation_id=?1",
            [intent.operation_id.as_slice()],
            |r| Ok((r.get(0)?,r.get(1)?,r.get(2)?,r.get(3)?,r.get(4)?)),
        ).optional().map_err(Into::into)
    })?;
    let Some((installation, group, kind, hash, result)) = found else {
        return Ok(None);
    };
    if installation.as_slice() != intent.installation_id
        || group.as_slice() != group_id
        || kind != intent.kind as i64
        || hash.as_slice() != intent.input_hash
    {
        return Err(conflict());
    }
    result.map(Some).ok_or_else(conflict)
}

/// Only one unresolved upload may own a group's staged MLS state. The partial unique index is
/// the atomic backstop; this check makes ordinary conflicts fail before doing MLS work.
pub(super) fn ensure_no_pending_for_group(
    core: &ClientCore,
    group_id: &[u8; 16],
) -> Result<(), ClientError> {
    let pending: bool = core.read(|c| {
        c.query_row(
            "SELECT EXISTS(SELECT 1 FROM app_bot_intents WHERE group_id=?1 AND state=0)",
            [group_id.as_slice()],
            |r| r.get(0),
        )
        .map_err(Into::into)
    })?;
    if pending { Err(conflict()) } else { Ok(()) }
}

pub(super) fn persist(
    c: &rusqlite::Connection,
    intent: IntentWrite,
    group_id: &[u8; 16],
    result: &[u8],
) -> Result<(), StorageError> {
    c.execute(
        "INSERT INTO app_bot_intents(operation_id,installation_id,group_id,kind,input_hash,result) VALUES(?1,?2,?3,?4,?5,?6)",
        params![intent.operation_id.as_slice(), intent.installation_id.as_slice(), group_id.as_slice(), intent.kind as i64, intent.input_hash.as_slice(), result],
    )?;
    Ok(())
}

pub(super) fn check_result_size(bytes: &[u8]) -> Result<(), ClientError> {
    if bytes.len() > MAX_INTENT_RESULT {
        Err(ClientError::new(
            E_CORE_LIMIT,
            "bot upload intent exceeds staged transport limit",
        ))
    } else {
        Ok(())
    }
}

pub(super) fn make_resolution(
    operation_id: &[u8; 16],
    b: Binding,
    group_id: &[u8; 16],
    kind: u64,
    state: u64,
    args: impl FnOnce(&mut Encoder),
) -> Result<Resolution, ClientError> {
    operation(operation_id)?;
    if !matches!(kind, 2 | 5 | 9) || !matches!(state, 1 | 2) {
        return Err(ClientError::new(
            E_CORE_INPUT,
            "invalid bot intent resolution",
        ));
    }
    let mut e = Encoder::new();
    e.array(2).uint(state);
    args(&mut e);
    Ok(Resolution {
        operation_id: *operation_id,
        installation_id: b.installation_id,
        group_id: *group_id,
        kind,
        state,
        hash: sha256(&e.into_vec()),
    })
}

pub(super) fn already_resolved(core: &ClientCore, r: Resolution) -> Result<bool, ClientError> {
    let found: Option<ResolutionRow> = core.read(|c| {
        c.query_row(
            "SELECT installation_id,group_id,kind,state,resolution_hash FROM app_bot_intents WHERE operation_id=?1",
            [r.operation_id.as_slice()],
            |row| Ok((row.get(0)?,row.get(1)?,row.get(2)?,row.get(3)?,row.get(4)?)),
        ).optional().map_err(Into::into)
    })?;
    let Some((installation, group, kind, state, hash)) = found else {
        return Err(conflict());
    };
    if installation.as_slice() != r.installation_id
        || group.as_slice() != r.group_id
        || kind != r.kind as i64
    {
        return Err(conflict());
    }
    if state == 0 {
        return Ok(false);
    }
    if state == r.state as i64 && hash.as_deref() == Some(r.hash.as_slice()) {
        Ok(true)
    } else {
        Err(conflict())
    }
}

pub(super) fn resolve(u: &UnitScope<'_>, r: Resolution) -> Result<(), ClientError> {
    u.with_conn(|c| {
        let row: Option<ResolutionRow> = c
            .query_row(
                "SELECT installation_id,group_id,kind,state,resolution_hash FROM app_bot_intents WHERE operation_id=?1",
                [r.operation_id.as_slice()],
                |row| Ok((row.get(0)?,row.get(1)?,row.get(2)?,row.get(3)?,row.get(4)?)),
            )
            .optional()?;
        let Some((installation, group, kind, state, hash)) = row else {
            return Err(StorageError::Codec("bot intent missing during resolution".into()));
        };
        if installation.as_slice() != r.installation_id || group.as_slice() != r.group_id || kind != r.kind as i64 {
            return Err(StorageError::Codec("bot intent binding mismatch during resolution".into()));
        }
        if state != 0 {
            if state == r.state as i64 && hash.as_deref() == Some(r.hash.as_slice()) { return Ok(()); }
            return Err(StorageError::Codec("bot intent resolution conflicts".into()));
        }
        c.execute(
            "UPDATE app_bot_intents SET state=?2,result=NULL,resolution_hash=?3 WHERE operation_id=?1 AND state=0",
            params![r.operation_id.as_slice(), r.state as i64, r.hash.as_slice()],
        )?;
        Ok(())
    })?;
    Ok(())
}

/// Resolve only abandoned external-join uploads; this never changes MLS storage.
/// The discard hash is identical to bot_group_discard's empty-argument resolution.
pub(super) fn discard_pending_external_joins(
    c: &rusqlite::Connection,
    group_id: &[u8; 16],
) -> Result<(), StorageError> {
    let mut e = Encoder::new();
    e.array(2).uint(2).array(0);
    let hash = sha256(&e.into_vec());
    c.execute(
        "UPDATE app_bot_intents SET state=2,result=NULL,resolution_hash=?2 WHERE group_id=?1 AND kind=5 AND state=0",
        params![group_id.as_slice(), hash.as_slice()],
    )?;
    Ok(())
}

/// Discard every pending Bot upload whose staged MLS state is being forgotten.
/// Create, join and commit abort all use the same empty-argument discard hash.
pub(super) fn discard_pending_for_forgotten_group(
    c: &rusqlite::Connection,
    group_id: &[u8; 16],
) -> Result<(), StorageError> {
    let mut e = Encoder::new();
    e.array(2).uint(2).array(0);
    let hash = sha256(&e.into_vec());
    c.execute(
        "UPDATE app_bot_intents SET state=2,result=NULL,resolution_hash=?2 WHERE group_id=?1 AND kind IN (2,5,9) AND state=0",
        params![group_id.as_slice(), hash.as_slice()],
    )?;
    Ok(())
}

fn intent_kind(
    core: &ClientCore,
    operation_id: &[u8; 16],
    group_id: &[u8; 16],
    b: Binding,
) -> Result<u64, ClientError> {
    let found: Option<(Vec<u8>, Vec<u8>, i64, i64)> =
        core.read(|c| {
            c.query_row(
            "SELECT installation_id,group_id,kind,state FROM app_bot_intents WHERE operation_id=?1",
            [operation_id.as_slice()],
            |r| Ok((r.get(0)?,r.get(1)?,r.get(2)?,r.get(3)?)),
        ).optional().map_err(Into::into)
        })?;
    let Some((installation, group, kind, _state)) = found else {
        return Err(conflict());
    };
    if installation.as_slice() != b.installation_id || group.as_slice() != group_id {
        return Err(conflict());
    }
    Ok(kind as u64)
}

impl ClientCore {
    /// Return the authoritative durable upload intent for this installation, if present.
    pub fn bot_intent_result(&self, operation_id: &[u8; 16]) -> Result<Vec<u8>, ClientError> {
        operation(operation_id)?;
        let b = binding(self)?;
        let found: Option<IntentResultRow> = self.read(|c| {
            c.query_row(
                "SELECT kind,group_id,state,result FROM app_bot_intents WHERE operation_id=?1 AND installation_id=?2",
                params![operation_id.as_slice(), b.installation_id.as_slice()],
                |r| Ok((r.get(0)?,r.get(1)?,r.get(2)?,r.get(3)?)),
            ).optional().map_err(Into::into)
        })?;
        let mut e = Encoder::new();
        if let Some((kind, group, state, result)) = found {
            let group: [u8; 16] = group
                .try_into()
                .map_err(|_| ClientError::new(E_CORE_STATE, "malformed bot intent group id"))?;
            e.array(4)
                .uint(kind as u64)
                .bytes(&group)
                .uint(state as u64)
                .opt_bytes(result.as_deref());
        } else {
            e.array(1).null();
        }
        Ok(e.into_vec())
    }

    /// Create a text or call group for the installation's bound community and durably retain its
    /// exact public upload bytes before the MLS transaction commits.
    pub fn bot_group_create(
        &mut self,
        operation_id: &[u8; 16],
        group_id: &[u8; 16],
        channel_id: &[u8; 16],
        kind: u64,
        policy: u64,
        external_sender: &[u8; 32],
    ) -> Result<Vec<u8>, ClientError> {
        let b = binding(self)?;
        let group_kind = group_kind(kind)?;
        let intent = write(operation_id, b.installation_id, 2, |e| {
            e.array(6)
                .bytes(group_id)
                .bytes(&b.community_id)
                .bytes(channel_id)
                .uint(kind)
                .uint(policy)
                .bytes(external_sender);
        })?;
        if let Some(result) = retry(self, intent, group_id)? {
            return Ok(result);
        }
        ensure_no_pending_for_group(self, group_id)?;
        self.group_create_kind_with_intent(
            group_id,
            Some(&b.community_id),
            channel_id,
            policy,
            external_sender,
            group_kind,
            Some(intent),
        )
    }

    /// Join a group in the installation's community and retain the exact external-commit upload.
    #[allow(clippy::too_many_arguments)]
    pub fn bot_group_external_join(
        &mut self,
        operation_id: &[u8; 16],
        group_id: &[u8; 16],
        channel_id: &[u8; 16],
        kind: u64,
        policy: u64,
        group_info: &[u8],
        ratchet_tree: &[u8],
    ) -> Result<Vec<u8>, ClientError> {
        let b = binding(self)?;
        let group_kind = group_kind(kind)?;
        let intent = write(operation_id, b.installation_id, 5, |e| {
            e.array(7)
                .bytes(group_id)
                .bytes(&b.community_id)
                .bytes(channel_id)
                .uint(kind)
                .uint(policy)
                .bytes(group_info)
                .bytes(ratchet_tree);
        })?;
        // Repair pre-fix orphan records only after the row has been forgotten or removed.
        // ACTIVE, NEEDS_RESYNC, REGISTERING and especially live JOINING rows are untouched.
        self.write(|_, u| {
            u.with_conn(|c| {
                let state: Option<i64> = c
                    .query_row(
                        "SELECT state FROM app_groups WHERE group_id=?1",
                        [group_id.as_slice()],
                        |r| r.get(0),
                    )
                    .optional()?;
                if state.is_none() || state == Some(super::groups::STATE_GONE) {
                    discard_pending_external_joins(c, group_id)?;
                }
                Ok(())
            })?;
            Ok(())
        })?;
        if let Some(result) = retry(self, intent, group_id)? {
            return Ok(result);
        }
        ensure_no_pending_for_group(self, group_id)?;
        self.group_join_kind_with_intent(
            group_id,
            Some(&b.community_id),
            channel_id,
            policy,
            group_info,
            ratchet_tree,
            group_kind,
            Some(intent),
        )
    }

    /// Build and durably retain one pending commit upload for the bound installation.
    pub fn bot_commit_build(
        &mut self,
        operation_id: &[u8; 16],
        group_id: &[u8; 16],
        proposals_body: &[u8],
    ) -> Result<Vec<u8>, ClientError> {
        let b = binding(self)?;
        let intent = write(operation_id, b.installation_id, 9, |e| {
            e.array(2).bytes(group_id).bytes(proposals_body);
        })?;
        if let Some(result) = retry(self, intent, group_id)? {
            return Ok(result);
        }
        ensure_no_pending_for_group(self, group_id)?;
        validate_group_scope(self, group_id, b)?;
        self.commit_build_inner_with_intent(group_id, proposals_body, false, Some(intent))
    }

    pub fn bot_group_registered(
        &mut self,
        operation_id: &[u8; 16],
        group_id: &[u8; 16],
        next_sequence: u64,
    ) -> Result<(), ClientError> {
        let r = make_resolution(operation_id, binding(self)?, group_id, 2, 1, |e| {
            e.array(1).uint(next_sequence);
        })?;
        self.group_registered_with_intent(group_id, next_sequence, Some(r))
    }

    pub fn bot_group_discard(
        &mut self,
        operation_id: &[u8; 16],
        group_id: &[u8; 16],
    ) -> Result<(), ClientError> {
        let b = binding(self)?;
        let kind = intent_kind(self, operation_id, group_id, b)?;
        if !matches!(kind, 2 | 5) {
            return Err(conflict());
        }
        let r = make_resolution(operation_id, b, group_id, kind, 2, |e| {
            e.array(0);
        })?;
        self.group_discard_with_intent(group_id, Some(r))
    }

    pub fn bot_group_joined(
        &mut self,
        operation_id: &[u8; 16],
        group_id: &[u8; 16],
        sequence: u64,
    ) -> Result<(), ClientError> {
        let r = make_resolution(operation_id, binding(self)?, group_id, 5, 1, |e| {
            e.array(1).uint(sequence);
        })?;
        self.group_joined_with_intent(group_id, sequence, Some(r))
    }

    pub fn bot_commit_confirm(
        &mut self,
        operation_id: &[u8; 16],
        group_id: &[u8; 16],
    ) -> Result<(), ClientError> {
        let r = make_resolution(operation_id, binding(self)?, group_id, 9, 1, |e| {
            e.array(0);
        })?;
        self.commit_confirm_with_intent(group_id, Some(r))
            .map(|_| ())
    }

    pub fn bot_commit_abort(
        &mut self,
        operation_id: &[u8; 16],
        group_id: &[u8; 16],
    ) -> Result<(), ClientError> {
        let r = make_resolution(operation_id, binding(self)?, group_id, 9, 2, |e| {
            e.array(0);
        })?;
        self.commit_abort_with_intent(group_id, Some(r))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn staged_result_limit_reserves_native_wrapper_headroom() {
        assert!(check_result_size(&vec![0; MAX_INTENT_RESULT]).is_ok());
        assert_eq!(
            check_result_size(&vec![0; MAX_INTENT_RESULT + 1])
                .expect_err("wrapper headroom is enforced")
                .code,
            E_CORE_LIMIT
        );
        assert_eq!(MAX_INTENT_RESULT + 64, 32 * 1024 * 1024);
    }
}
