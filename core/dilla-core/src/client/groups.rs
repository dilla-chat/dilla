//! Local group lifecycle and Welcome acceptance.

use super::error::{E_BINDING, E_CORE_INPUT, E_CORE_NOT_FOUND, E_CORE_STATE};
use super::{ClientCore, ClientError, Own, wire};
use crate::cbor::Encoder;
use crate::identity::{CredentialIdentity, Kind, SignerTier, Tier, sha256};
use crate::ids::{CommunityId, DeviceId, InstanceId, UserId};
use crate::mls::{
    DillaBinding, DillaGroup, DillaInteractionBinding, GroupKind, StorageError, UnitScope,
    WelcomeLabel, external_senders,
};
use openmls::messages::group_info::VerifiableGroupInfo;
use openmls::prelude::*;
use rusqlite::{OptionalExtension, params};
use std::fmt::Write as _;
use tls_codec::Deserialize as _;

pub(crate) const STATE_REGISTERING: i64 = 0;
pub(crate) const STATE_JOINING: i64 = 1;
pub(crate) const STATE_ACTIVE: i64 = 2;
pub(crate) const STATE_NEEDS_RESYNC: i64 = 3;
#[allow(dead_code)] // Task 6 consumes the gone state.
pub(crate) const STATE_GONE: i64 = 4;

struct HumanInteractionWelcome {
    context: crate::bot::human_interaction_context::CurrentContext,
    now: u64,
}

#[allow(dead_code)] // The full persisted row is shared with task 6.
#[derive(Clone)]
pub(super) struct GroupRow {
    pub kind: i64,
    pub community_id: Option<Vec<u8>>,
    pub target_id: Vec<u8>,
    pub interaction_installation_id: Option<Vec<u8>>,
    pub interaction_device_id: Option<Vec<u8>>,
    pub state: i64,
    pub next_seq: i64,
    pub resync: i64,
    pub was_gone: i64,
    pub max_epoch: i64,
}
impl GroupRow {
    /// Whether the row is the group of `kind` (0 text, 1 call) of `channel` in `community`: a join
    /// never changes this.
    fn bound_to(&self, kind: i64, community: Option<&[u8; 16]>, channel: &[u8; 16]) -> bool {
        self.kind == kind
            && self.community_id.as_deref() == community.map(|c| c.as_slice())
            && self.target_id.as_slice() == channel.as_slice()
    }
}
fn rebound() -> ClientError {
    ClientError::new(E_CORE_STATE, "group is bound to another channel")
}
/// The lowest epoch a rejoin may land in: the highest this device has held for the group (the
/// row's `max_epoch`, and the stale group's epoch while it is still stored).
fn epoch_floor(row: Option<&GroupRow>, prior: Option<&DillaGroup>) -> u64 {
    let stored = row.map_or(0, |r| u64::try_from(r.max_epoch).unwrap_or(0));
    stored.max(prior.map_or(0, DillaGroup::epoch))
}
fn below_floor(what: &str) -> ClientError {
    ClientError::new(
        E_CORE_INPUT,
        format!("{what} would rejoin at or below an epoch this device has held"),
    )
}

fn bot_welcome_receipt_key(group_id: &[u8; 16]) -> String {
    let mut key = String::from("bot_welcome/v1/");
    for byte in group_id {
        let _ = write!(key, "{byte:02x}");
    }
    key
}

fn human_interaction_welcome_receipt_key(group_id: &[u8; 16]) -> String {
    let mut key = String::from("human_interaction_welcome/v1/");
    for byte in group_id {
        let _ = write!(key, "{byte:02x}");
    }
    key
}

/// Stable idempotency receipt for one accepted human Welcome. The current-context proof is
/// deliberately omitted: its signature and 30-second validity window change on every fetch.
/// Registration identity and immutable MLS binding fields identify the accepted interaction;
/// the policy snapshot and proof timestamps are omitted because they can change on refresh.
fn human_interaction_welcome_receipt(
    context: &crate::bot::human_interaction_context::CurrentContext,
    item: &wire::WelcomeItem,
) -> Vec<u8> {
    let registration = &context.registration;
    let binding = &context.binding;
    let mut input = Encoder::new();
    input
        .array(4)
        .text("dilla.human.interaction.welcome.receipt.v2")
        .array(11)
        .bytes(registration.installation.as_bytes())
        .bytes(registration.operation.as_bytes())
        .bytes(registration.bot_user.as_bytes())
        .bytes(registration.bot_device.as_bytes())
        .bytes(registration.community.as_bytes())
        .bytes(registration.group.as_bytes())
        .bytes(&registration.body_sha256)
        .uint(registration.generation)
        .uint(registration.accepted_epoch)
        .uint(registration.next_seq)
        .uint(registration.accepted_at)
        .array(8)
        .bytes(binding.instance_id.as_bytes())
        .bytes(&binding.installation_id)
        .bytes(binding.community_id.as_bytes())
        .bytes(&binding.bot_user_id)
        .bytes(&binding.participant_user_id)
        .bytes(binding.participant_device_id.as_bytes())
        .uint(binding.e2ee_version)
        .uint(binding.media_version)
        .array(6)
        .bytes(&item.group_id)
        .uint(item.epoch)
        .uint(item.commit_seq)
        .bytes(&item.tree_hash)
        .bytes(&item.blob)
        .bytes(&item.ratchet_tree);
    let digest = sha256(&input.into_vec());
    let mut receipt = Encoder::new();
    receipt.array(2).uint(2).bytes(&digest);
    receipt.into_vec()
}

fn interaction_now(now: u64) -> Result<u64, ClientError> {
    if now == 0 || now > i64::MAX as u64 {
        return Err(ClientError::new(
            E_CORE_INPUT,
            "interaction time is out of range",
        ));
    }
    Ok(now)
}

fn interaction_target_matches(target: &[u8; 16], binding: &DillaInteractionBinding) -> bool {
    binding.bot_user_id == *target
}

fn clear_bot_welcome_receipt(
    c: &rusqlite::Connection,
    group_id: &[u8; 16],
) -> Result<(), StorageError> {
    super::schema::meta_del(c, &bot_welcome_receipt_key(group_id))
}

/// A receipt is a fixed-size, versioned record. The digest covers the exact encrypted Welcome,
/// ratchet tree, labels, and immutable MLS binding; delivery IDs and policy snapshots are omitted.
fn bot_welcome_receipt(
    bot: super::bot_intents::Binding,
    group_id: &[u8; 16],
    item: &wire::WelcomeItem,
    bind: &DillaBinding,
) -> Vec<u8> {
    let kind = u64::from(bind.kind.as_u8());
    let community_id = bind.community_id.as_ref().map(|c| c.0).unwrap_or([0; 16]);
    let mut input = Encoder::new();
    input
        .array(14)
        .text("dilla.bot.welcome.receipt.v1")
        .bytes(group_id)
        .bytes(&bind.instance_id.0)
        .bytes(&community_id)
        .bytes(&bind.target_id)
        .bytes(&bot.installation_id)
        .uint(kind)
        .uint(bind.e2ee_version)
        .uint(bind.media_version)
        .uint(item.epoch)
        .uint(item.commit_seq)
        .bytes(&item.tree_hash);
    input.bytes(&item.blob).bytes(&item.ratchet_tree);
    let digest = sha256(&input.into_vec());

    let mut receipt = Encoder::new();
    receipt
        .array(9)
        .uint(1)
        .bytes(&bind.instance_id.0)
        .bytes(&community_id)
        .bytes(&bot.installation_id)
        .bytes(&bind.target_id)
        .uint(kind)
        .uint(bind.e2ee_version)
        .uint(bind.media_version)
        .bytes(&digest);
    receipt.into_vec()
}
pub(super) fn group_row(
    c: &rusqlite::Connection,
    id: &[u8; 16],
) -> Result<Option<GroupRow>, StorageError> {
    c.query_row(
        "SELECT kind, community_id, target_id, interaction_installation_id, interaction_device_id, state, next_seq, resync, was_gone, max_epoch \
         FROM app_groups WHERE group_id = ?1",
        [id.as_slice()],
        |r| {
            Ok(GroupRow {
                kind: r.get(0)?,
                community_id: r.get(1)?,
                target_id: r.get(2)?,
                interaction_installation_id: r.get(3)?,
                interaction_device_id: r.get(4)?,
                state: r.get(5)?,
                next_seq: r.get(6)?,
                resync: r.get(7)?,
                was_gone: r.get(8)?,
                max_epoch: r.get(9)?,
            })
        },
    )
    .optional()
    .map_err(Into::into)
}
/// The group of `kind` (0 text, 1 call) another id holds for `channel`, if any: a DM's text group
/// and its call group share the DM's id as their target and never stand in for each other.
pub(super) fn holder(
    c: &rusqlite::Connection,
    channel: &[u8; 16],
    kind: i64,
    except: &[u8; 16],
) -> Result<Option<[u8; 16]>, StorageError> {
    c.query_row(
        "SELECT group_id FROM app_groups \
         WHERE target_id = ?1 AND kind = ?2 AND state <> 4 AND group_id <> ?3",
        params![channel.as_slice(), kind, except.as_slice()],
        |r| r.get(0),
    )
    .optional()
    .map_err(Into::into)
}

/// Find another active kind-3 group only when its authenticated binding names this exact
/// installation/community/user/device tuple. The app row alone stores the participant user and
/// cannot distinguish a second device for the same account.
const MAX_INTERACTION_HOLDER_CANDIDATES: usize = 2048;

struct InteractionScope<'a> {
    installation: &'a [u8; 16],
    community: &'a [u8; 16],
    target: &'a [u8; 16],
    participant: &'a [u8; 16],
    device: &'a [u8; 16],
}

fn interaction_row_matches_binding(
    binding: &DillaInteractionBinding,
    community: &[u8; 16],
    target: &[u8; 16],
    participant: &[u8; 16],
    stored_installation: Option<&[u8]>,
    stored_device: Option<&[u8]>,
) -> bool {
    if binding.community_id.as_bytes() != community
        || binding.bot_user_id != *target
        || binding.participant_user_id.as_slice() != participant
    {
        return false;
    }
    match (stored_installation, stored_device) {
        (None, None) => true,
        (Some(installation), Some(device)) => {
            installation == binding.installation_id.as_slice()
                && device == binding.participant_device_id.as_bytes()
        }
        _ => false,
    }
}

/// Decide whether a candidate is a duplicate only after its authenticated v2 binding is available.
/// Legacy kind-3 v1 contexts do not carry a participant device ID, so they cannot establish safe
/// per-device uniqueness and must fail closed.
fn interaction_candidate_is_duplicate(
    binding: Option<&DillaInteractionBinding>,
    is_interaction: bool,
    scope: &InteractionScope<'_>,
    stored_installation: Option<&[u8]>,
    stored_device: Option<&[u8]>,
) -> Result<bool, ()> {
    let binding = binding.filter(|_| is_interaction).ok_or(())?;
    if !interaction_row_matches_binding(
        binding,
        scope.community,
        scope.target,
        scope.participant,
        stored_installation,
        stored_device,
    ) {
        return Err(());
    }
    Ok(
        binding.installation_id.as_slice() == scope.installation.as_slice()
            && binding.participant_device_id.as_bytes() == scope.device,
    )
}

fn interaction_holder(
    core: &mut ClientCore,
    scope: &InteractionScope<'_>,
    except: &[u8; 16],
) -> Result<Option<[u8; 16]>, ClientError> {
    let candidates = core.read(|c| {
        let mut statement = c.prepare(
            "SELECT group_id,interaction_installation_id,interaction_device_id FROM app_groups WHERE kind=3 AND community_id=?1 AND target_id=?2 AND state<>4 AND group_id<>?3 ORDER BY group_id LIMIT ?4",
        )?;
        let rows = statement.query_map(
            params![scope.community.as_slice(), scope.target.as_slice(), except.as_slice(), (MAX_INTERACTION_HOLDER_CANDIDATES + 1) as i64],
            |row| Ok((row.get::<_, Vec<u8>>(0)?, row.get::<_, Option<Vec<u8>>>(1)?, row.get::<_, Option<Vec<u8>>>(2)?)),
        )?;
        rows.map(|row| {
            let (raw, installation, device) = row?;
            let group_id = raw.try_into().map_err(|_| StorageError::Codec("interaction group id malformed".into()))?;
            Ok((group_id, installation, device))
        }).collect::<Result<Vec<([u8; 16], Option<Vec<u8>>, Option<Vec<u8>>)>, StorageError>>()
    })?;
    if candidates.len() > MAX_INTERACTION_HOLDER_CANDIDATES {
        return Err(ClientError::new(
            E_CORE_STATE,
            "interaction group scope exceeds the local candidate limit",
        ));
    }
    for (group_id, stored_installation, stored_device) in candidates {
        if stored_installation.is_some() != stored_device.is_some() {
            return Err(ClientError::new(
                E_CORE_STATE,
                "interaction group device scope is malformed",
            ));
        }
        let Some(group) = core.take_group_unchecked(&group_id)? else {
            return Err(ClientError::new(
                E_CORE_STATE,
                "interaction group state is unavailable",
            ));
        };
        let binding = group.interaction_binding().ok();
        let duplicate = match interaction_candidate_is_duplicate(
            binding.as_ref(),
            group.kind() == GroupKind::Interaction,
            scope,
            stored_installation.as_deref(),
            stored_device.as_deref(),
        ) {
            Ok(duplicate) => duplicate,
            Err(()) => {
                core.keep_group(group_id, group);
                return Err(ClientError::new(
                    E_CORE_STATE,
                    "interaction group binding or scope is unavailable",
                ));
            }
        };
        core.keep_group(group_id, group);
        if duplicate {
            return Ok(Some(group_id));
        }
    }
    Ok(None)
}
fn binding(
    own: &Own,
    community: Option<&[u8; 16]>,
    channel: &[u8; 16],
    policy: u64,
    kind: GroupKind,
) -> DillaBinding {
    DillaBinding {
        v: 1,
        instance_id: InstanceId::from_bytes(own.instance_id),
        community_id: community.map(|c| CommunityId::from_bytes(*c)),
        target_id: *channel,
        kind,
        policy_version: policy,
        e2ee_version: 1,
        media_version: kind.media_version(),
    }
}
/// The `app_groups.kind` of a text or call group.
pub(super) fn row_kind(kind: GroupKind) -> i64 {
    match kind {
        GroupKind::Call => 1,
        GroupKind::Interaction => 3,
        _ => 0,
    }
}
fn credential(own: &Own) -> CredentialWithKey {
    CredentialWithKey {
        credential: BasicCredential::new(own.credential.clone()).into(),
        signature_key: own.dsk_pub.to_vec().into(),
    }
}
pub(super) fn checked(name: &str, v: u64) -> Result<i64, ClientError> {
    i64::try_from(v).map_err(|_| ClientError::new(E_CORE_INPUT, format!("{name} out of range")))
}
fn missing() -> ClientError {
    ClientError::new(E_CORE_NOT_FOUND, "")
}
fn state(n: i64) -> ClientError {
    ClientError::new(E_CORE_STATE, format!("group state {n}"))
}
fn hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{x:02x}")).collect()
}
fn group_info(bytes: &[u8]) -> Result<VerifiableGroupInfo, ClientError> {
    let m = MlsMessageIn::tls_deserialize_exact(bytes)
        .map_err(|_| ClientError::new(E_CORE_INPUT, "group_info is not an MLS GroupInfo"))?;
    match m.extract() {
        MlsMessageBodyIn::GroupInfo(v) => Ok(v),
        _ => Err(ClientError::new(
            E_CORE_INPUT,
            "group_info is not an MLS GroupInfo",
        )),
    }
}
fn tree(bytes: &[u8]) -> Result<RatchetTreeIn, ClientError> {
    RatchetTreeIn::tls_deserialize_exact(bytes)
        .map_err(|_| ClientError::new(E_CORE_INPUT, "ratchet_tree is not a TLS RatchetTree"))
}
fn welcome(bytes: &[u8]) -> Result<Welcome, ClientError> {
    let m = MlsMessageIn::tls_deserialize_exact(bytes)
        .map_err(|_| ClientError::new(E_CORE_INPUT, "blob is not an MLS Welcome"))?;
    match m.extract() {
        MlsMessageBodyIn::Welcome(v) => Ok(v),
        _ => Err(ClientError::new(E_CORE_INPUT, "blob is not an MLS Welcome")),
    }
}
fn in_unit<T>(
    u: &UnitScope<'_>,
    f: impl FnOnce(&rusqlite::Connection) -> Result<T, StorageError>,
) -> Result<T, ClientError> {
    u.with_conn(f).map_err(Into::into)
}

pub(super) fn set_mls(
    c: &rusqlite::Connection,
    id: &[u8; 16],
    epoch: i64,
    pending: bool,
) -> Result<(), StorageError> {
    c.execute(
        "UPDATE app_groups SET epoch = ?2, pending_commit = ?3 WHERE group_id = ?1",
        params![id.as_slice(), epoch, i64::from(pending)],
    )?;
    Ok(())
}
pub(super) fn set_pending(
    c: &rusqlite::Connection,
    id: &[u8; 16],
    pending: bool,
) -> Result<(), StorageError> {
    c.execute(
        "UPDATE app_groups SET pending_commit = ?2 WHERE group_id = ?1",
        params![id.as_slice(), i64::from(pending)],
    )?;
    Ok(())
}

/// The select of one `groups()` row, as a macro so each query is one literal (`concat!`), not a
/// `format!` at run time.
macro_rules! listed {
    ($tail:literal) => {
        concat!(
            "SELECT group_id, kind, community_id, target_id, state, epoch, next_seq, \
             (SELECT COUNT(*) FROM app_proposals WHERE app_proposals.group_id = app_groups.group_id), \
             pending_commit FROM app_groups ",
            $tail
        )
    };
}
type ListedGroup = (
    [u8; 16],
    i64,
    Option<Vec<u8>>,
    Vec<u8>,
    i64,
    i64,
    i64,
    i64,
    i64,
);
fn listed_row(r: &rusqlite::Row<'_>) -> rusqlite::Result<ListedGroup> {
    Ok((
        r.get(0)?,
        r.get(1)?,
        r.get(2)?,
        r.get(3)?,
        r.get(4)?,
        r.get(5)?,
        r.get(6)?,
        r.get(7)?,
        r.get(8)?,
    ))
}
fn encode_listed(e: &mut Encoder, row: &ListedGroup) {
    let (id, kind, community, target, state, epoch, next, proposals, pending) = row;
    e.array(9)
        .bytes(id)
        .uint(*kind as u64)
        .opt_bytes(community.as_deref())
        .bytes(target)
        .uint(*state as u64)
        .uint(*epoch as u64)
        .uint(*next as u64)
        .uint(*proposals as u64)
        .uint(*pending as u64);
}

impl ClientCore {
    /// [[group_id b16, kind uint, community_id b16|null, target_id b16, state uint, epoch uint,
    ///   next_seq uint, proposals_pending uint, pending_commit uint 0|1]] ordered by group_id —
    /// epoch and pending_commit are app_groups.epoch and app_groups.pending_commit; no MLS state is loaded.
    pub fn groups(&self) -> Result<Vec<u8>, ClientError> {
        let rows: Vec<ListedGroup> = self.read(|c| {
            let mut s = c.prepare(listed!("ORDER BY group_id"))?;
            let it = s.query_map([], listed_row)?;
            it.collect::<Result<_, _>>().map_err(Into::into)
        })?;
        let mut e = Encoder::new();
        e.array(rows.len());
        for row in &rows {
            encode_listed(&mut e, row);
        }
        Ok(e.into_vec())
    }

    /// CBOR null for an unknown id, else the one row of groups() for it (the same 9 elements).
    pub fn group_row(&self, group_id: &[u8; 16]) -> Result<Vec<u8>, ClientError> {
        let row: Option<ListedGroup> = self.read(|c| {
            c.query_row(
                listed!("WHERE group_id = ?1"),
                [group_id.as_slice()],
                listed_row,
            )
            .optional()
            .map_err(Into::into)
        })?;
        let mut e = Encoder::new();
        match row {
            Some(row) => encode_listed(&mut e, &row),
            None => {
                e.null();
            }
        }
        Ok(e.into_vec())
    }

    pub fn group_create(
        &mut self,
        id: &[u8; 16],
        community: Option<&[u8; 16]>,
        channel: &[u8; 16],
        policy: u64,
        external: &[u8; 32],
    ) -> Result<Vec<u8>, ClientError> {
        self.group_create_kind(id, community, channel, policy, external, GroupKind::Text)
    }

    /// `group_create`'s body for a text or a call group (`call_group_create`).
    pub(super) fn group_create_kind(
        &mut self,
        id: &[u8; 16],
        community: Option<&[u8; 16]>,
        channel: &[u8; 16],
        policy: u64,
        external: &[u8; 32],
        kind: GroupKind,
    ) -> Result<Vec<u8>, ClientError> {
        self.group_create_kind_with_intent(id, community, channel, policy, external, kind, None)
    }

    #[allow(clippy::too_many_arguments)]
    pub(super) fn group_create_kind_with_intent(
        &mut self,
        id: &[u8; 16],
        community: Option<&[u8; 16]>,
        channel: &[u8; 16],
        policy: u64,
        external: &[u8; 32],
        kind: GroupKind,
        intent: Option<super::bot_intents::IntentWrite>,
    ) -> Result<Vec<u8>, ClientError> {
        let own = self.own()?;
        let row = self.read(|c| group_row(c, id))?;
        if let Some(r) = row {
            return Err(ClientError::new(
                E_CORE_STATE,
                format!("group exists in state {}", r.state),
            ));
        }
        if let Some(h) = self.read(|c| holder(c, channel, row_kind(kind), id))? {
            return Err(ClientError::new(
                E_CORE_STATE,
                format!("channel has group {}", hex(&h)),
            ));
        }
        let bind = binding(&own, community, channel, policy, kind);
        self.signer()?;
        let (g, result) = self.write(|ctx, u| {
            let signer = ctx
                .signer
                .ok_or_else(|| ClientError::new(super::error::E_CORE_NO_IDENTITY, ""))?;
            let g = DillaGroup::create(
                ctx.provider,
                signer,
                credential(&own),
                GroupId::from_slice(id),
                bind.clone(),
                Some(external_senders(
                    SignaturePublicKey::from(&external[..]),
                    &bind.instance_id,
                )),
            )?;
            let info = wire::tls(&g.export_group_info(ctx.provider, signer)?)?;
            let tree = wire::tls(&g.export_ratchet_tree())?;
            let mut e = Encoder::new();
            e.array(4)
                .bytes(id)
                .bytes(&bind.encode())
                .bytes(&info)
                .bytes(&tree);
            let result = e.into_vec();
            if intent.is_some() {
                super::bot_intents::check_result_size(&result)?;
            }
            in_unit(u, |c| {
                clear_bot_welcome_receipt(c, id)?;
                c.execute(
                    "INSERT INTO app_groups(group_id,kind,community_id,target_id,state) \
                     VALUES(?1,?4,?2,?3,0)",
                    params![
                        id.as_slice(),
                        community.map(|c| c.as_slice()),
                        channel.as_slice(),
                        row_kind(kind)
                    ],
                )?;
                if let Some(intent) = intent {
                    super::bot_intents::persist(c, intent, id, &result)?;
                }
                Ok(())
            })?;
            Ok::<_, ClientError>((g, result))
        })?;
        self.keep_group(*id, g);
        Ok(result)
    }

    pub fn group_registered(&mut self, id: &[u8; 16], next_seq: u64) -> Result<(), ClientError> {
        self.group_registered_with_intent(id, next_seq, None)
    }

    pub(super) fn group_registered_with_intent(
        &mut self,
        id: &[u8; 16],
        next_seq: u64,
        intent: Option<super::bot_intents::Resolution>,
    ) -> Result<(), ClientError> {
        if let Some(intent) = intent
            && super::bot_intents::already_resolved(self, intent)?
        {
            return Ok(());
        }
        self.own()?;
        let next = checked("next_seq", next_seq)?;
        if next < 1 {
            return Err(ClientError::new(
                E_CORE_INPUT,
                "next_seq must be at least 1",
            ));
        }
        let r = self.read(|c| group_row(c, id))?.ok_or_else(missing)?;
        if r.state != STATE_REGISTERING {
            return Err(state(r.state));
        }
        self.write(|_, u| {
            in_unit(u, |c| {
                clear_bot_welcome_receipt(c, id)?;
                c.execute(
                    "UPDATE app_groups SET state=2,next_seq=?2 WHERE group_id=?1",
                    params![id.as_slice(), next],
                )?;
                Ok(())
            })?;
            if let Some(intent) = intent {
                super::bot_intents::resolve(u, intent)?;
            }
            Ok(())
        })
    }

    pub fn group_discard(&mut self, id: &[u8; 16]) -> Result<(), ClientError> {
        self.group_discard_with_intent(id, None)
    }

    pub(super) fn group_discard_with_intent(
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
        let r = self.read(|c| group_row(c, id))?.ok_or_else(missing)?;
        if r.state != STATE_REGISTERING && r.state != STATE_JOINING {
            return Err(state(r.state));
        }
        let mut group = self.take_group_unchecked(id)?;
        self.write(|ctx, u| {
            if let Some(g) = group.as_mut() {
                g.delete(ctx.provider)?;
            }
            in_unit(u, |c| {
                clear_bot_welcome_receipt(c, id)?;
                c.execute(
                    "DELETE FROM app_proposals WHERE group_id=?1",
                    [id.as_slice()],
                )?;
                if r.state == STATE_JOINING && r.resync == 1 {
                    c.execute(
                        "UPDATE app_groups SET state=3,resync=0,epoch=0,pending_commit=0 WHERE group_id=?1",
                        [id.as_slice()],
                    )?;
                } else if r.state == STATE_JOINING && r.was_gone == 1 {
                    // A rejoin of a gone row that did not complete: the row goes back to gone with
                    // its timeline and outbox (history outranks the discard of a fresh join).
                    c.execute(
                        "UPDATE app_groups SET state=4,was_gone=0,epoch=0,pending_commit=0 WHERE group_id=?1",
                        [id.as_slice()],
                    )?;
                } else {
                    c.execute(
                        "DELETE FROM app_human_interaction_response_claims WHERE group_id=?1",
                        [id.as_slice()],
                    )?;
                    for table in [
                        "app_handshake_tail",
                        "app_messages",
                        "app_outbox",
                        "app_read_state",
                        "app_pins",
                        "app_reactions",
                        "app_authority",
                        "app_authority_known",
                        "app_authority_mentionable",
                    ] {
                        c.execute(
                            &format!("DELETE FROM {table} WHERE group_id=?1"),
                            [id.as_slice()],
                        )?;
                    }
                    c.execute("DELETE FROM app_groups WHERE group_id=?1", [id.as_slice()])?;
                }
                Ok(())
            })?;
            if let Some(intent) = intent { super::bot_intents::resolve(u, intent)?; }
            Ok(())
        })
    }

    pub fn group_join_external(
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
            GroupKind::Text,
        )
    }

    /// `group_join_external`'s body for a text or a call group (`call_group_join_external`). A call
    /// join runs the call gate on the joined tree inside the join's unit, right after the audit
    /// (L-CORE-50): any verdict but a pending leaf rolls the whole unit back.
    #[allow(clippy::too_many_arguments)] // The join's ABI plus the kind it is for.
    pub(super) fn group_join_kind(
        &mut self,
        id: &[u8; 16],
        community: Option<&[u8; 16]>,
        channel: &[u8; 16],
        policy: u64,
        info_body: &[u8],
        tree_body: &[u8],
        kind: GroupKind,
    ) -> Result<Vec<u8>, ClientError> {
        self.group_join_kind_with_intent(
            id, community, channel, policy, info_body, tree_body, kind, None,
        )
    }

    #[allow(clippy::too_many_arguments)]
    pub(super) fn group_join_kind_with_intent(
        &mut self,
        id: &[u8; 16],
        community: Option<&[u8; 16]>,
        channel: &[u8; 16],
        policy: u64,
        info_body: &[u8],
        tree_body: &[u8],
        kind: GroupKind,
        intent: Option<super::bot_intents::IntentWrite>,
    ) -> Result<Vec<u8>, ClientError> {
        let own = self.own()?;
        let info = wire::decode_info_body(info_body)?;
        let tree_body = wire::decode_tree_body(tree_body)?;
        if info.epoch != tree_body.epoch || info.tree_hash != tree_body.tree_hash {
            return Err(ClientError::new(
                E_CORE_INPUT,
                "info_body and tree_body disagree",
            ));
        }
        let gi = group_info(&info.group_info)?;
        // The group this GroupInfo describes must be the one the join is for; checked before
        // anything is joined or written (OpenMLS would store the joined state under the
        // GroupInfo's own id).
        if gi.group_id().as_slice() != id {
            return Err(ClientError::new(
                E_CORE_INPUT,
                "GroupInfo group id does not match",
            ));
        }
        let tree = tree(&tree_body.ratchet_tree)?;
        let r = self.read(|c| group_row(c, id))?;
        if let Some(r) = &r
            && (r.state == STATE_REGISTERING || r.state == STATE_JOINING)
        {
            return Err(state(r.state));
        }
        if let Some(r) = &r
            && !r.bound_to(row_kind(kind), community, channel)
        {
            return Err(rebound());
        }
        if let Some(h) = self.read(|c| holder(c, channel, row_kind(kind), id))? {
            return Err(ClientError::new(
                E_CORE_STATE,
                format!("channel has group {}", hex(&h)),
            ));
        }
        let mut prior = self.take_group_unchecked(id)?;
        // The join lands this device in epoch e + 1 of a GroupInfo at e: refused when e + 1 is
        // not above the highest epoch held, so a GroupInfo at exactly that epoch is accepted.
        let info_epoch = gi.epoch().as_u64();
        if r.is_some() && info_epoch < epoch_floor(r.as_ref(), prior.as_ref()) {
            // Not put back in the cache: it was taken unchecked; the next use reloads it.
            return Err(below_floor("GroupInfo"));
        }
        let bind = binding(&own, community, channel, policy, kind);
        let result = self.write(|ctx, u| {
            let signer = ctx
                .signer
                .ok_or_else(|| ClientError::new(super::error::E_CORE_NO_IDENTITY, ""))?;
            let resync = matches!(
                r.as_ref().map(|r| r.state),
                Some(STATE_ACTIVE | STATE_NEEDS_RESYNC)
            );
            // The stale group's epoch is already the floor (written when it was reached), so it
            // survives this deletion and a discard of the join. The joined epoch is not held
            // until the server accepts the external commit: group_joined records it.
            if resync && let Some(g) = prior.as_mut() {
                g.delete(ctx.provider)?;
            }
            let (g, commit, _) = DillaGroup::join_by_external_commit(
                ctx.provider,
                signer,
                credential(&own),
                gi,
                tree,
                &bind,
            )?;
            u.with_conn(|c| super::trust::audit_tree(c,&g,id,&own.user_id,&own.device_id,None,None))?;
            if kind == GroupKind::Call {
                // Ruling RV-1: a pending leaf is settled by `call_join_check` before the worker
                // posts this commit; every other verdict refuses here, before a commit exists.
                super::calls::join_gate(ctx.provider, u, &g, id, &own)?;
            }
            let joined = checked("epoch", g.epoch())?;
            let commit = wire::tls(&commit)?;
            let exported = wire::tls(&g.export_group_info(ctx.provider, signer)?)?;
            let mut result = Encoder::new();
            result.array(2).bytes(&commit).bytes(&exported);
            let result = result.into_vec();
            if intent.is_some() { super::bot_intents::check_result_size(&result)?; }
            in_unit(u, |c| {
                clear_bot_welcome_receipt(c, id)?;
                c.execute(
                    "DELETE FROM app_proposals WHERE group_id=?1",
                    [id.as_slice()],
                )?;
                if let Some(r) = &r {
                    // The row's binding (kind, community, target) was checked equal above and
                    // is never rewritten by a join.
                    let was_gone = r.state == STATE_GONE;
                    c.execute(
                        "UPDATE app_groups SET state=1,resync=?2,was_gone=?3,epoch=?4,pending_commit=0 WHERE group_id=?1",
                        params![id.as_slice(), i64::from(resync), i64::from(was_gone), joined],
                    )?;
                } else {
                    c.execute(
                        "INSERT INTO app_groups(group_id,kind,community_id,target_id,state,epoch) \
                         VALUES(?1,?5,?2,?3,1,?4)",
                        params![id.as_slice(), community.map(|c| c.as_slice()), channel.as_slice(), joined, row_kind(kind)],
                    )?;
                }
                if let Some(intent) = intent {
                    super::bot_intents::persist(c, intent, id, &result)?;
                }
                Ok(())
            })?;
            Ok::<_, ClientError>((g, result))
        })?;
        self.keep_group(*id, result.0);
        Ok(result.1)
    }

    pub fn group_joined(&mut self, id: &[u8; 16], seq: u64) -> Result<(), ClientError> {
        self.group_joined_with_intent(id, seq, None)
    }

    pub(super) fn group_joined_with_intent(
        &mut self,
        id: &[u8; 16],
        seq: u64,
        intent: Option<super::bot_intents::Resolution>,
    ) -> Result<(), ClientError> {
        if let Some(intent) = intent
            && super::bot_intents::already_resolved(self, intent)?
        {
            return Ok(());
        }
        self.own()?;
        let seq = checked("seq", seq)?;
        if seq < 1 || seq == i64::MAX {
            return Err(ClientError::new(E_CORE_INPUT, "seq out of range"));
        }
        let r = self.read(|c| group_row(c, id))?.ok_or_else(missing)?;
        if r.state != STATE_JOINING {
            return Err(state(r.state));
        }
        // The server accepted the external commit: its epoch is now held (F6 floor).
        let loaded;
        let joined = match self.groups.get(id) {
            Some(g) => Some(g),
            None => {
                loaded = DillaGroup::load(&self.provider, &GroupId::from_slice(id))?;
                loaded.as_ref()
            }
        };
        let held = checked("epoch", joined.map_or(0, DillaGroup::epoch))?;
        self.write(|_, u| {
            in_unit(u, |c| {
                // next_seq never moves back: rows below the stored value were applied or skipped.
                c.execute(
                    "UPDATE app_groups SET state=2,next_seq=MAX(next_seq,?2),resync=0, \
                     was_gone=0,max_epoch=MAX(max_epoch,?3),epoch=?3 WHERE group_id=?1",
                    params![id.as_slice(), seq + 1, held],
                )?;
                Ok(())
            })?;
            if let Some(intent) = intent {
                super::bot_intents::resolve(u, intent)?;
            }
            Ok(())
        })
    }

    pub fn welcomes_apply(
        &mut self,
        welcomes_body: &[u8],
        expected: &[u8],
    ) -> Result<Vec<u8>, ClientError> {
        self.welcomes_apply_kind(welcomes_body, expected, GroupKind::Text, None, None)
    }

    /// Apply one kind-3 v2 interaction Welcome after checking the server-signed current-context
    /// proof against the enrolled instance key and the authenticated MLS state. The proof is
    /// bound to this core's own instance, user and device; callers cannot supply those pins.
    pub fn human_interaction_welcome_apply(
        &mut self,
        welcomes_body: &[u8],
        current_context_proof: &[u8],
        now: u64,
    ) -> Result<Vec<u8>, ClientError> {
        let now = interaction_now(now)?;
        let own = self.own()?;
        if own.kind != Kind::User.as_u8() {
            return Err(ClientError::new(
                E_BINDING,
                "human interaction Welcome requires a user identity",
            ));
        }
        let items = wire::decode_welcomes_body(welcomes_body)?;
        if items.len() != 1 {
            return Err(ClientError::new(
                E_CORE_INPUT,
                "interaction Welcome requires exactly one item",
            ));
        }
        let item = &items[0];
        let pinned_key = self.pinned_instance_signer_key()?;
        let expected_context = crate::bot::human_interaction_context::Expected {
            instance: crate::ids::InstanceId::from_bytes(own.instance_id),
            group: crate::ids::MsgId::from_bytes(item.group_id),
            participant_user: UserId::from_bytes(own.user_id),
            participant_device: DeviceId::from_bytes(own.device_id),
            now,
        };
        let context = crate::bot::human_interaction_context::verify(
            current_context_proof,
            &pinned_key,
            &expected_context,
        )
        .map_err(|_| ClientError::new(E_BINDING, "interaction Welcome context rejected"))?;
        if *context.registration.group.as_bytes() != item.group_id {
            return Err(ClientError::new(
                E_BINDING,
                "interaction Welcome group mismatch",
            ));
        }
        // This tuple is derived only after signature, pin, identity and device-list checks.
        // TypeScript deliberately forwards the proof as opaque bytes and cannot choose the group
        // target, community or policy that becomes the local app_groups binding. The authenticated
        // MLS binding and roster are checked before persistence in apply_human_interaction_welcome.
        let mut expected = Encoder::new();
        expected
            .array(1)
            .array(4)
            .bytes(item.group_id.as_slice())
            .bytes(context.binding.community_id.as_bytes())
            .bytes(&context.binding.bot_user_id)
            .uint(context.binding.policy_version);
        let expected = expected.into_vec();
        self.welcomes_apply_kind(
            welcomes_body,
            &expected,
            GroupKind::Interaction,
            None,
            Some(HumanInteractionWelcome { context, now }),
        )
    }

    /// Apply server-expected welcomes for a bound native Bot installation. Unlike the general
    /// client API, this path requires every expected group to name the installation community.
    pub fn bot_welcomes_apply(
        &mut self,
        welcomes_body: &[u8],
        expected: &[u8],
        kind: u64,
    ) -> Result<Vec<u8>, ClientError> {
        let bot = super::bot_intents::binding(self)?;
        let group_kind = match kind {
            0 => GroupKind::Text,
            1 => GroupKind::Call,
            _ => return Err(ClientError::new(E_CORE_INPUT, "invalid bot group kind")),
        };
        let expected_groups = wire::decode_expected(expected)?;
        if expected_groups
            .iter()
            .any(|v| v.community_id != Some(bot.community_id))
        {
            return Err(ClientError::new(E_BINDING, "welcome community mismatch"));
        }
        self.welcomes_apply_kind(welcomes_body, expected, group_kind, Some(bot), None)
    }

    fn welcomes_apply_kind(
        &mut self,
        welcomes_body: &[u8],
        expected: &[u8],
        group_kind: GroupKind,
        bot: Option<super::bot_intents::Binding>,
        human_interaction: Option<HumanInteractionWelcome>,
    ) -> Result<Vec<u8>, ClientError> {
        let own = self.own()?;
        let items = wire::decode_welcomes_body(welcomes_body)?;
        let expected = wire::decode_expected(expected)?;
        let mut e = Encoder::new();
        e.array(items.len());
        for item in items {
            let mut outcome = 3;
            let mut reason = "";
            if let Some(want) = expected.iter().find(|v| v.group_id == item.group_id) {
                let id = &item.group_id;
                let row = self.read(|c| group_row(c, id))?;
                // A Bot can miss its own removal while offline: the server then
                // denies member-only source reads and later sends a new Add Welcome.
                // Removal and a later Add need two commits, so the Welcome must
                // be at least two epochs above the last locally held one. The
                // labelled Welcome and epoch floor are verified below.
                let bot_readmission = bot.is_some()
                    && row.as_ref().is_some_and(|r| {
                        r.state == STATE_ACTIVE
                            && u64::try_from(r.max_epoch)
                                .ok()
                                .and_then(|epoch| epoch.checked_add(1))
                                .is_some_and(|after_one_commit| item.epoch > after_one_commit)
                    });
                if let (Some(human), Some(row)) = (human_interaction.as_ref(), row.as_ref()) {
                    if self.human_interaction_welcome_replay_matches(
                        id, &item, want, &own, human, row,
                    )? {
                        outcome = 1;
                    } else {
                        outcome = 2;
                        reason = E_BINDING;
                    }
                } else if bot.is_none()
                    && human_interaction.is_none()
                    && row.as_ref().is_some_and(|r| r.state <= STATE_ACTIVE)
                {
                    // Preserve the longstanding human client idempotency behavior and ordering.
                    outcome = 1;
                } else if row.as_ref().is_some_and(|r| {
                    !r.bound_to(
                        row_kind(group_kind),
                        want.community_id.as_ref(),
                        &want.channel_id,
                    )
                }) {
                    outcome = 2;
                    reason = rebound().code;
                } else if row.as_ref().is_some_and(|r| r.state <= STATE_ACTIVE) && !bot_readmission
                {
                    if let Some(bot) = bot {
                        if row.as_ref().is_some_and(|r| r.state == STATE_ACTIVE) {
                            let actual = self.take_group(id)?;
                            if let Some(group) = actual {
                                let expected_receipt =
                                    group.binding().ok().and_then(|actual_binding| {
                                        let binding_matches = actual_binding.instance_id.0
                                            == own.instance_id
                                            && actual_binding.community_id.as_ref().map(|c| c.0)
                                                == Some(bot.community_id)
                                            && actual_binding.target_id == want.channel_id
                                            && actual_binding.kind == group_kind
                                            && actual_binding.e2ee_version == 1
                                            && actual_binding.media_version
                                                == group_kind.media_version();
                                        binding_matches.then(|| {
                                            bot_welcome_receipt(bot, id, &item, actual_binding)
                                        })
                                    });
                                let key = bot_welcome_receipt_key(id);
                                let stored = self.read(|c| super::schema::meta_get(c, &key))?;
                                self.keep_group(*id, group);
                                if expected_receipt
                                    .as_ref()
                                    .is_some_and(|receipt| stored.as_ref() == Some(receipt))
                                {
                                    outcome = 1;
                                } else {
                                    outcome = 2;
                                    reason = E_BINDING;
                                }
                            } else {
                                outcome = 2;
                                reason = E_BINDING;
                            }
                        } else {
                            outcome = 2;
                            reason = E_BINDING;
                        }
                    }
                } else if match human_interaction.as_ref() {
                    Some(human) => interaction_holder(
                        self,
                        &InteractionScope {
                            installation: human.context.registration.installation.as_bytes(),
                            community: human.context.binding.community_id.as_bytes(),
                            target: &want.channel_id,
                            participant: &human.context.binding.participant_user_id,
                            device: human.context.binding.participant_device_id.as_bytes(),
                        },
                        id,
                    )?,
                    None => self.read(|c| holder(c, &want.channel_id, row_kind(group_kind), id))?,
                }
                .is_some()
                {
                    outcome = 2;
                    reason = E_CORE_STATE;
                } else {
                    let parsed =
                        welcome(&item.blob).and_then(|w| tree(&item.ratchet_tree).map(|t| (w, t)));
                    match parsed {
                        Err(err) => {
                            outcome = 2;
                            reason = err.code;
                        }
                        Ok((w, t)) => {
                            if let Some(human) = human_interaction.as_ref() {
                                let result = self.apply_human_interaction_welcome(
                                    id, &item, w, t, want, &own, human,
                                );
                                match result {
                                    Ok(g) => {
                                        self.keep_group(*id, g);
                                        outcome = 0;
                                    }
                                    Err(err) => {
                                        if err.code == super::error::E_CORE_STORAGE
                                            || err.code == super::error::E_CORE_RELOAD
                                        {
                                            return Err(err);
                                        }
                                        outcome = 2;
                                        reason = err.code;
                                    }
                                }
                            } else {
                                let mut prior = self.take_group_unchecked(id)?;
                                let floor = epoch_floor(row.as_ref(), prior.as_ref());
                                let bind = binding(
                                    &own,
                                    want.community_id.as_ref(),
                                    &want.channel_id,
                                    want.policy_version,
                                    group_kind,
                                );
                                let welcome_receipt =
                                    bot.map(|bot| bot_welcome_receipt(bot, id, &item, &bind));
                                let next = checked("commit_seq", item.commit_seq).and_then(|n| {
                                    n.checked_add(1).ok_or_else(|| {
                                        ClientError::new(E_CORE_INPUT, "commit_seq out of range")
                                    })
                                });
                                let result = next.and_then(|next| {
                                self.write(|ctx, u| {
                                    if let Some(g) = prior.as_mut() {
                                        g.delete(ctx.provider)?;
                                    }
                                    // The Welcome's group id, epoch and tree hash are inside its
                                    // encrypted GroupInfo, so they are compared with the served
                                    // label on the staged welcome (id: E_BINDING; epoch or tree
                                    // hash: E_CORE_INPUT). Any refusal here rolls the whole unit
                                    // back: the KeyPackage and keys OpenMLS consumed while
                                    // staging, the deleted prior, and no floor is raised.
                                    let g = DillaGroup::join_from_welcome_labelled(
                                        ctx.provider,
                                        w,
                                        t,
                                        &bind,
                                        WelcomeLabel {
                                            group_id: id.as_slice(),
                                            epoch: item.epoch,
                                            tree_hash: item.tree_hash.as_slice(),
                                        },
                                    )?;
                                    u.with_conn(|c| super::trust::audit_tree(c,&g,id,&own.user_id,&own.device_id,None,None))?;
                                    // With the epoch the server's (checked above): over an
                                    // existing row, a Welcome
                                    // into an epoch at or below one this device has held is a
                                    // replay (a real re-admission is committed after the last
                                    // held epoch), rolled back the same way.
                                    if row.is_some() && g.epoch() <= floor {
                                        return Err(below_floor("Welcome"));
                                    }
                                    let held = checked("epoch", g.epoch())?;
                                    in_unit(u, |c| {
                                        c.execute(
                                            "DELETE FROM app_proposals WHERE group_id=?1",
                                            [id.as_slice()],
                                        )?;
                                        if row.is_some() {
                                            // The binding was checked equal to the row's above.
                                            c.execute(
                                                "UPDATE app_groups SET state=2, \
                                                 next_seq=MAX(next_seq,?2),resync=0, \
                                                 max_epoch=MAX(max_epoch,?3),epoch=?3,pending_commit=0 WHERE group_id=?1",
                                                params![id.as_slice(), next, held],
                                            )?;
                                        } else {
                                            c.execute(
                                                "INSERT INTO app_groups \
                                                 (group_id,kind,community_id,target_id,state, \
                                                 next_seq,max_epoch,epoch) \
                                                 VALUES(?1,?6,?2,?3,2,?4,?5,?5)",
                                                params![
                                                    id.as_slice(),
                                                    want.community_id.as_ref().map(|c| c.as_slice()),
                                                    want.channel_id.as_slice(),
                                                    next,
                                                    held,
                                                    row_kind(group_kind),
                                                ],
                                            )?;
                                        }
                                        if let Some(receipt) = welcome_receipt.as_ref() {
                                            super::schema::meta_put(
                                                c,
                                                &bot_welcome_receipt_key(id),
                                                receipt,
                                            )?;
                                        }
                                        Ok(())
                                    })?;
                                    Ok::<_, ClientError>(g)
                                })
                            });
                                match result {
                                    Ok(g) => {
                                        self.keep_group(*id, g);
                                        outcome = 0;
                                    }
                                    Err(err) => {
                                        if err.code == super::error::E_CORE_STORAGE
                                            || err.code == super::error::E_CORE_RELOAD
                                        {
                                            return Err(err);
                                        }
                                        outcome = 2;
                                        reason = err.code;
                                    }
                                }
                            }
                        }
                    }
                }
            }
            e.array(4)
                .uint(item.welcome_id)
                .bytes(&item.group_id)
                .uint(outcome)
                .text(reason);
        }
        Ok(e.into_vec())
    }

    /// Recognize only an exact, previously accepted human Welcome. This path reads the durable
    /// receipt and current MLS state but performs no database or ratchet mutation.
    fn human_interaction_welcome_replay_matches(
        &mut self,
        id: &[u8; 16],
        item: &wire::WelcomeItem,
        want: &wire::ExpectedGroup,
        own: &Own,
        human: &HumanInteractionWelcome,
        row: &GroupRow,
    ) -> Result<bool, ClientError> {
        let context = &human.context;
        if row.state != STATE_ACTIVE
            || !row.bound_to(
                row_kind(GroupKind::Interaction),
                Some(context.binding.community_id.as_bytes()),
                &context.binding.bot_user_id,
            )
            || context.registration.group.as_bytes() != id
            || want.community_id != Some(*context.binding.community_id.as_bytes())
            || want.channel_id != context.binding.bot_user_id
            || want.policy_version != context.binding.policy_version
        {
            return Ok(false);
        }
        let key = human_interaction_welcome_receipt_key(id);
        let expected_receipt = human_interaction_welcome_receipt(context, item);
        if self.read(|c| super::schema::meta_get(c, &key))?.as_ref() != Some(&expected_receipt) {
            return Ok(false);
        }

        let group = match self.take_group(id) {
            Ok(Some(group)) => group,
            Ok(None) => return Ok(false),
            Err(error)
                if error.code == super::error::E_CORE_STORAGE
                    || error.code == super::error::E_CORE_RELOAD =>
            {
                return Err(error);
            }
            Err(_) => return Ok(false),
        };
        let checked = (|| {
            if group.kind() != GroupKind::Interaction || group.member_count() != 2 {
                return Err(ClientError::new(
                    E_BINDING,
                    "interaction replay group rejected",
                ));
            }
            let actual_binding = group
                .interaction_binding()
                .map_err(|_| ClientError::new(E_BINDING, "interaction replay binding rejected"))?;
            let tree_hash = group
                .tree_hash(&self.provider, self.signer()?)
                .map_err(ClientError::from)?;
            if actual_binding != context.binding
                || group.epoch() != context.current_epoch
                || tree_hash != context.tree_hash
            {
                return Err(ClientError::new(
                    E_BINDING,
                    "interaction replay state changed",
                ));
            }
            let mut leaves = Vec::with_capacity(2);
            let mut saw_bot = false;
            let mut saw_participant = false;
            for member in group.members() {
                let basic = BasicCredential::try_from(member.credential).map_err(|_| {
                    ClientError::new(E_BINDING, "interaction replay credential rejected")
                })?;
                let credential = CredentialIdentity::decode(basic.identity()).map_err(|_| {
                    ClientError::new(E_BINDING, "interaction replay credential rejected")
                })?;
                let dsk: [u8; 32] =
                    member.signature_key.as_slice().try_into().map_err(|_| {
                        ClientError::new(E_BINDING, "interaction replay key rejected")
                    })?;
                credential.verify_signatures(&dsk).map_err(|_| {
                    ClientError::new(E_BINDING, "interaction replay credential rejected")
                })?;
                let snapshot = if credential.user_id.as_bytes()
                    == context.registration.bot_user.as_bytes()
                    && credential.device_id.as_bytes() == context.registration.bot_device.as_bytes()
                {
                    if credential.kind != Kind::Bot
                        || credential.tier != Tier::Native
                        || credential.signer_tier != SignerTier::Native
                    {
                        return Err(ClientError::new(
                            E_BINDING,
                            "interaction replay Bot rejected",
                        ));
                    }
                    saw_bot = true;
                    &context.bot
                } else if credential.user_id.as_bytes() == &own.user_id
                    && credential.device_id.as_bytes() == &own.device_id
                {
                    if credential.kind != Kind::User {
                        return Err(ClientError::new(
                            E_BINDING,
                            "interaction replay participant rejected",
                        ));
                    }
                    saw_participant = true;
                    &context.participant
                } else {
                    return Err(ClientError::new(
                        E_BINDING,
                        "interaction replay contains another device",
                    ));
                };
                let verdict = self.read(|c| {
                    super::trust::validate_interaction_leaf(
                        c,
                        id,
                        group.epoch(),
                        tree_hash,
                        member.index.u32(),
                        &credential,
                        dsk,
                        snapshot,
                    )
                })?;
                verdict.map_err(|code| {
                    ClientError::new(code, "interaction replay identity is not trusted")
                })?;
                leaves.push((
                    u64::from(member.index.u32()),
                    crate::bot::interaction_admission::AuthenticatedRosterLeaf {
                        user: credential.user_id,
                        device: credential.device_id,
                        dsk_pub: dsk,
                        tier: credential.tier,
                    },
                ));
            }
            if !saw_bot || !saw_participant {
                return Err(ClientError::new(
                    E_BINDING,
                    "interaction replay roster rejected",
                ));
            }
            context
                .verify_authenticated_group(&actual_binding, group.epoch(), &tree_hash, &leaves)
                .map_err(|_| ClientError::new(E_BINDING, "interaction replay proof mismatch"))
        })();
        self.keep_group(*id, group);
        match checked {
            Ok(()) => Ok(true),
            Err(error)
                if error.code == super::error::E_CORE_STORAGE
                    || error.code == super::error::E_CORE_RELOAD =>
            {
                Err(error)
            }
            Err(_) => Ok(false),
        }
    }

    #[expect(
        clippy::too_many_arguments,
        reason = "Inputs are the authenticated welcome and its scoped provenance."
    )]
    fn apply_human_interaction_welcome(
        &mut self,
        id: &[u8; 16],
        item: &wire::WelcomeItem,
        welcome: Welcome,
        ratchet_tree: openmls::treesync::RatchetTreeIn,
        want: &wire::ExpectedGroup,
        own: &Own,
        human: &HumanInteractionWelcome,
    ) -> Result<DillaGroup, ClientError> {
        if let Some(existing) = self.take_group_unchecked(id)? {
            self.keep_group(*id, existing);
            return Err(ClientError::new(
                E_BINDING,
                "interaction Welcome already exists",
            ));
        }
        let context = &human.context;
        if context.registration.group.as_bytes() != id
            || context.binding.policy_version != want.policy_version
            || context.binding.community_id.as_bytes().as_slice()
                != want
                    .community_id
                    .as_ref()
                    .map(|v| v.as_slice())
                    .unwrap_or(&[])
            || !interaction_target_matches(&want.channel_id, &context.binding)
        {
            return Err(ClientError::new(
                E_BINDING,
                "interaction Welcome context mismatch",
            ));
        }
        let receipt_key = human_interaction_welcome_receipt_key(id);
        self.write(|ctx, unit| {
            let signer = ctx
                .signer
                .ok_or_else(|| ClientError::new(E_CORE_STATE, "identity signer is unavailable"))?;
            let fresh_now = human.now;
            if !crate::bot::human_interaction_context::is_current(
                context.issued_at,
                context.not_after,
                fresh_now,
            ) {
                return Err(ClientError::new(E_BINDING, "interaction Welcome context expired"));
            }
            let already_used = unit.with_conn(|c| super::schema::meta_get(c, &receipt_key))?;
            if already_used.is_some() {
                return Err(ClientError::new(E_BINDING, "interaction Welcome was already used"));
            }
            let group = DillaGroup::join_interaction_from_welcome_labelled(
                ctx.provider,
                welcome,
                ratchet_tree,
                WelcomeLabel {
                    group_id: id,
                    epoch: item.epoch,
                    tree_hash: item.tree_hash.as_slice(),
                },
            )?;
            if group.kind() != GroupKind::Interaction || group.member_count() != 2 {
                return Err(ClientError::new(E_BINDING, "interaction Welcome roster rejected"));
            }
            let actual_binding = group.interaction_binding().map_err(|_| {
                ClientError::new(E_BINDING, "interaction Welcome binding rejected")
            })?;
            let actual_tree_hash = group.tree_hash(ctx.provider, signer)?;
            let mut leaves = Vec::with_capacity(2);
            let mut saw_bot = false;
            let mut saw_participant = false;
            for member in group.members() {
                let basic = BasicCredential::try_from(member.credential).map_err(|_| {
                    ClientError::new(E_BINDING, "interaction Welcome credential rejected")
                })?;
                let credential = CredentialIdentity::decode(basic.identity()).map_err(|_| {
                    ClientError::new(E_BINDING, "interaction Welcome credential rejected")
                })?;
                let dsk: [u8; 32] = member.signature_key.as_slice().try_into().map_err(|_| {
                    ClientError::new(E_BINDING, "interaction Welcome signature key rejected")
                })?;
                credential.verify_signatures(&dsk).map_err(|_| {
                    ClientError::new(E_BINDING, "interaction Welcome credential rejected")
                })?;
                if credential.user_id.as_bytes() == context.registration.bot_user.as_bytes()
                    && credential.device_id.as_bytes() == context.registration.bot_device.as_bytes()
                {
                    if credential.kind != Kind::Bot
                        || credential.tier != Tier::Native
                        || credential.signer_tier != SignerTier::Native
                    {
                        return Err(ClientError::new(E_BINDING, "interaction Bot credential rejected"));
                    }
                    saw_bot = true;
                } else if credential.user_id.as_bytes() == &own.user_id
                    && credential.device_id.as_bytes() == &own.device_id
                {
                    if credential.kind != Kind::User {
                        return Err(ClientError::new(E_BINDING, "interaction participant credential rejected"));
                    }
                    saw_participant = true;
                } else {
                    return Err(ClientError::new(E_BINDING, "interaction Welcome contains another device"));
                }
                let signed_list = if credential.user_id.as_bytes()
                    == context.registration.bot_user.as_bytes()
                {
                    &context.bot
                } else {
                    &context.participant
                };
                let verdict = unit.with_conn(|c| {
                    super::trust::validate_interaction_leaf(
                        c,
                        id,
                        group.epoch(),
                        actual_tree_hash,
                        member.index.u32(),
                        &credential,
                        dsk,
                        signed_list,
                    )
                })?;
                verdict.map_err(|code| {
                    ClientError::new(code, "interaction Welcome identity is not trusted")
                })?;
                leaves.push((
                    member.index.u32() as u64,
                    crate::bot::interaction_admission::AuthenticatedRosterLeaf {
                        user: credential.user_id,
                        device: credential.device_id,
                        dsk_pub: dsk,
                        tier: credential.tier,
                    },
                ));
            }
            if !saw_bot || !saw_participant {
                return Err(ClientError::new(E_BINDING, "interaction Welcome roster rejected"));
            }
            context
                .verify_authenticated_group(
                    &actual_binding,
                    group.epoch(),
                    &actual_tree_hash,
                    &leaves,
                )
                .map_err(|_| ClientError::new(E_BINDING, "interaction Welcome proof mismatch"))?;
            let receipt = human_interaction_welcome_receipt(context, item);
            let next_seq = checked("commit_seq", item.commit_seq)?
                .checked_add(1)
                .ok_or_else(|| ClientError::new(E_CORE_INPUT, "commit_seq out of range"))?;
            let held = checked("epoch", group.epoch())?;
            unit.with_conn(|c| {
                c.execute(
                    "INSERT INTO app_groups (group_id,kind,community_id,target_id,state,next_seq,max_epoch,epoch,interaction_installation_id,interaction_device_id) VALUES(?1,3,?2,?3,2,?4,?5,?5,?6,?7)",
                    params![id.as_slice(), want.community_id.as_ref().map(|v| v.as_slice()), want.channel_id.as_slice(), next_seq, held, context.registration.installation.as_bytes().as_slice(), context.binding.participant_device_id.as_bytes().as_slice()],
                )?;
                super::schema::meta_put(c, &receipt_key, &receipt)
            })?;
            Ok::<_, ClientError>(group)
        })
    }
}

#[cfg(test)]
mod interaction_welcome_tests {
    use super::*;

    #[test]
    fn interaction_welcome_clock_is_bounded_at_the_core_boundary() {
        assert_eq!(interaction_now(1).unwrap(), 1);
        assert_eq!(interaction_now(0).unwrap_err().code, E_CORE_INPUT);
        assert_eq!(
            interaction_now(i64::MAX as u64 + 1).unwrap_err().code,
            E_CORE_INPUT
        );
    }

    fn test_context() -> crate::bot::human_interaction_context::CurrentContext {
        use crate::bot::human_interaction_context::{CurrentContext, Registration, RosterEntry};
        use crate::bot::interaction_snapshot::SnapshotList;
        use crate::ids::{CommunityId, DeviceId, InstanceId, MsgId};
        use crate::mls::DillaInteractionBinding;

        let msg = |v| MsgId::from_bytes([v; 16]);
        let list = |v| SnapshotList {
            user: msg(v),
            umk: [v; 32],
            ssk: [v; 32],
            binding_signature: [v; 64],
            version: 1,
            blob: vec![v],
            signature: [v; 64],
            prev_hash: [v; 32],
            hash: [v; 32],
        };
        CurrentContext {
            instance: msg(1),
            instance_key_id: msg(2),
            instance_signer: [3; 32],
            issued_at: 100,
            not_after: 130,
            registration: Registration {
                installation: msg(4),
                operation: msg(5),
                bot_user: msg(6),
                bot_device: msg(7),
                community: msg(8),
                group: msg(9),
                body_sha256: [10; 32],
                generation: 1,
                accepted_epoch: 2,
                next_seq: 3,
                accepted_at: 99,
            },
            binding: DillaInteractionBinding {
                instance_id: InstanceId::from_bytes([1; 16]),
                bot_user_id: [6; 16],
                installation_id: [4; 16],
                community_id: CommunityId::from_bytes([8; 16]),
                participant_user_id: [11; 16],
                participant_device_id: DeviceId::from_bytes([12; 16]),
                policy_version: 4,
                e2ee_version: 1,
                media_version: 0,
            },
            current_epoch: 2,
            tree_hash: [13; 32],
            roster: [
                RosterEntry {
                    leaf_index: 0,
                    user: msg(6),
                    device: msg(7),
                    signature_key: [14; 32],
                },
                RosterEntry {
                    leaf_index: 1,
                    user: msg(11),
                    device: msg(12),
                    signature_key: [15; 32],
                },
            ],
            eligibility_revision: 20,
            policy_version: 4,
            e2ee_version: 1,
            media_version: 0,
            bot: list(6),
            participant: list(11),
        }
    }

    #[test]
    fn welcome_receipt_is_stable_across_proof_refresh_and_binds_welcome_bytes() {
        let context = test_context();
        let mut item = wire::WelcomeItem {
            welcome_id: 1,
            group_id: [9; 16],
            epoch: 2,
            commit_seq: 3,
            blob: vec![1, 2],
            ratchet_tree: vec![3, 4],
            tree_hash: [16; 32],
        };
        let original = human_interaction_welcome_receipt(&context, &item);
        let mut refreshed = context.clone();
        refreshed.issued_at += 10;
        refreshed.not_after += 10;
        refreshed.eligibility_revision += 1;
        refreshed.policy_version += 1;
        refreshed.binding.policy_version += 1;
        assert_eq!(
            original,
            human_interaction_welcome_receipt(&refreshed, &item)
        );
        item.blob.push(5);
        assert_ne!(original, human_interaction_welcome_receipt(&context, &item));
    }

    #[test]
    fn kind3_welcome_target_is_the_bot_user_not_the_participant_user() {
        let binding = DillaInteractionBinding {
            instance_id: InstanceId::from_bytes([1; 16]),
            bot_user_id: [2; 16],
            installation_id: [3; 16],
            community_id: CommunityId::from_bytes([4; 16]),
            participant_user_id: [5; 16],
            participant_device_id: DeviceId::from_bytes([6; 16]),
            policy_version: 1,
            e2ee_version: 1,
            media_version: 0,
        };
        assert_ne!(binding.bot_user_id, binding.participant_user_id);
        assert!(interaction_target_matches(&binding.bot_user_id, &binding));
        assert!(!interaction_target_matches(
            &binding.participant_user_id,
            &binding
        ));
    }

    #[test]
    fn legacy_interaction_rows_are_matched_from_the_authenticated_binding() {
        let binding = DillaInteractionBinding {
            instance_id: InstanceId::from_bytes([1; 16]),
            bot_user_id: [2; 16],
            installation_id: [3; 16],
            community_id: CommunityId::from_bytes([4; 16]),
            participant_user_id: [5; 16],
            participant_device_id: DeviceId::from_bytes([6; 16]),
            policy_version: 1,
            e2ee_version: 1,
            media_version: 0,
        };
        assert!(interaction_row_matches_binding(
            &binding, &[4; 16], &[2; 16], &[5; 16], None, None,
        ));
        assert!(interaction_row_matches_binding(
            &binding,
            &[4; 16],
            &[2; 16],
            &[5; 16],
            Some(&[3; 16]),
            Some(&[6; 16]),
        ));
        assert!(!interaction_row_matches_binding(
            &binding,
            &[4; 16],
            &[2; 16],
            &[5; 16],
            Some(&[7; 16]),
            Some(&[6; 16]),
        ));
        assert!(!interaction_row_matches_binding(
            &binding,
            &[4; 16],
            &[2; 16],
            &[5; 16],
            Some(&[3; 16]),
            None,
        ));
    }

    #[test]
    fn legacy_v1_interaction_binding_fails_closed_for_duplicate_admission() {
        // A v1 kind-3 context has no authenticated participant device identity. Treat its
        // unavailable v2 binding as an error instead of admitting a potentially duplicate group.
        let scope = InteractionScope {
            community: &[4; 16],
            target: &[2; 16],
            participant: &[5; 16],
            installation: &[3; 16],
            device: &[6; 16],
        };
        assert!(interaction_candidate_is_duplicate(None, true, &scope, None, None).is_err());
    }
}
