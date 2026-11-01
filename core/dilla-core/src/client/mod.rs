//! Browser and desktop client engine: Rust decides and persists; the caller transports.
//! Every structured value crossing the boundary is one deterministic CBOR item. Every state
//! change runs inside exactly one `DillaStorage::unit`.

mod account_kind;
mod bot_events;
mod bot_identity;
pub use bot_identity::BotInteractionGroupDescriptor;
mod bot_intents;
mod bot_interaction_create;
mod bot_keypackages;
mod calls;
mod error;
mod fold;
mod groups;
mod heal;
mod human_interaction_events;
mod identity;
mod messages;
mod moderation;
mod removals;
mod schema;
mod search;
mod settings;
mod sync;
mod trust;
mod wire;

pub use error::ClientError;
pub use identity::{recovery_key_check, session_preimage};
pub use messages::{mentions_me, mentions_me_with_roles};
pub use moderation::{ManualRemovalEvidence, ManualWarningEvidence};
pub use removals::RemovalFeedEntry;
#[cfg(feature = "test-hooks")]
pub use schema::test_force_missing_fts5;
pub use schema::{APP_SCHEMA_SUPPORTED, HANDSHAKE_TAIL, migrate_app};

use crate::identity::{CredentialIdentity, Kind, Tier};
use crate::mls::{
    CIPHERSUITE, ConnHandle, DillaGroup, DillaProvider, DillaStorage, StorageError, UnitScope,
};
use error::{E_CORE_NO_IDENTITY, E_CORE_RELOAD, E_CORE_STATE, E_CORE_STORAGE};
use openmls::prelude::GroupId;
use openmls_basic_credential::SignatureKeyPair;
use rusqlite::OptionalExtension;
use std::collections::BTreeMap;

pub struct ClientCore {
    provider: DillaProvider,
    signer: Option<SignatureKeyPair>,
    groups: BTreeMap<[u8; 16], DillaGroup>,
}

struct Ctx<'a> {
    provider: &'a DillaProvider,
    signer: Option<&'a SignatureKeyPair>,
}

struct Own {
    instance_id: [u8; 16],
    user_id: [u8; 16],
    device_id: [u8; 16],
    dsk_pub: [u8; 32],
    credential: Vec<u8>,
    kind: u8,
    tier: u8,
}

impl ClientCore {
    /// Ingest a proof-bearing server removal feed page. Each revision must be the next
    /// per-group revision, or an exact replay of an already stored entry. This verifies the
    /// removal proof against locally trusted device credentials. The caller must authenticate
    /// the server feed and bind it to this group before calling; the core cannot authenticate
    /// HTTP/feed transport metadata.
    pub fn ingest_removals(
        &mut self,
        group_id: &[u8; 16],
        entries: &[RemovalFeedEntry],
    ) -> Result<u64, ClientError> {
        removals::ingest(self, group_id, entries)
    }

    fn own(&self) -> Result<Own, ClientError> {
        let raw: Option<Vec<u8>> = self.read(|c| {
            c.query_row("SELECT v FROM app_meta WHERE k = 'identity'", [], |r| {
                r.get(0)
            })
            .optional()
            .map_err(StorageError::from)
        })?;
        let raw = raw.ok_or_else(|| ClientError::new(E_CORE_NO_IDENTITY, ""))?;
        let record = identity::IdentityRecord::decode(&raw)?;
        let credential = CredentialIdentity::decode(&record.credential).map_err(|_| {
            ClientError::new(
                E_CORE_STORAGE,
                "identity record: credential does not decode",
            )
        })?;
        Ok(Own {
            instance_id: record.instance_id,
            user_id: record.user_id,
            device_id: record.device_id,
            dsk_pub: record.dsk_pub,
            credential: record.credential,
            kind: credential.kind.as_u8(),
            tier: credential.tier.as_u8(),
        })
    }

    fn signer(&self) -> Result<&SignatureKeyPair, ClientError> {
        self.signer
            .as_ref()
            .ok_or_else(|| ClientError::new(E_CORE_NO_IDENTITY, ""))
    }

    /// The group, from the cache or loaded from the store. A loaded group must be the one its
    /// row describes: its MLS group id and its authenticated binding match the row and, for an
    /// interaction group, this identity's instance/user/device. Otherwise `E_CORE_STATE` and
    /// nothing is cached; the caller repairs the row with a
    /// resync (`group_join_external`, states 2 and 3), which replaces the stored group. The cache
    /// only ever holds a group that passed this check or that a join of this core wrote.
    fn take_group(&mut self, group_id: &[u8; 16]) -> Result<Option<DillaGroup>, ClientError> {
        if let Some(group) = self.groups.remove(group_id) {
            if group.kind() == crate::mls::GroupKind::Interaction {
                let row = self
                    .read(|c| groups::group_row(c, group_id))?
                    .ok_or_else(mismatched)?;
                let own = self.own()?;
                if !matches_row(&group, group_id, &row, &own) {
                    return Err(mismatched());
                }
            }
            return Ok(Some(group));
        }
        let Some(group) = self.take_group_unchecked(group_id)? else {
            return Ok(None);
        };
        let row = self.read(|c| groups::group_row(c, group_id))?;
        if let Some(row) = row {
            let own = self.own()?;
            if !matches_row(&group, group_id, &row, &own) {
                return Err(mismatched());
            }
        }
        Ok(Some(group))
    }

    /// Load a kind-3 group only from the proof-bearing Bot sync path. The caller has already
    /// revalidated the signed admission snapshot; this check binds the stored Bot leaf and row.
    fn take_bot_interaction_group(
        &mut self,
        group_id: &[u8; 16],
        participant_user: &[u8; 16],
        participant_device: &[u8; 16],
    ) -> Result<Option<DillaGroup>, ClientError> {
        let Some(group) = self.take_group_unchecked(group_id)? else {
            return Ok(None);
        };
        let row = self
            .read(|c| groups::group_row(c, group_id))?
            .ok_or_else(mismatched)?;
        let own = self.own()?;
        if !matches_bot_interaction_row(&group, group_id, &row, &own)
            || !matches_bot_interaction_roster(&group, &own, participant_user, participant_device)
        {
            return Err(mismatched());
        }
        Ok(Some(group))
    }

    /// The group from the cache or the store without comparing it with its row: for the paths
    /// that delete it (joins over a stale group, discard).
    fn take_group_unchecked(
        &mut self,
        group_id: &[u8; 16],
    ) -> Result<Option<DillaGroup>, ClientError> {
        if let Some(group) = self.groups.remove(group_id) {
            return Ok(Some(group));
        }
        DillaGroup::load(&self.provider, &GroupId::from_slice(group_id)).map_err(Into::into)
    }

    fn keep_group(&mut self, group_id: [u8; 16], group: DillaGroup) {
        self.groups.insert(group_id, group);
    }

    fn retry_reload<T>(
        &mut self,
        group_id: &[u8; 16],
        mut f: impl FnMut(&mut Self) -> Result<T, ClientError>,
    ) -> Result<T, ClientError> {
        let first = f(self);
        if matches!(&first, Err(e) if e.code == E_CORE_RELOAD) {
            self.groups.remove(group_id);
            f(self)
        } else {
            first
        }
    }
    fn write<T>(
        &mut self,
        f: impl FnOnce(&Ctx<'_>, &UnitScope<'_>) -> Result<T, ClientError>,
    ) -> Result<T, ClientError> {
        let Self {
            provider,
            signer,
            groups,
        } = self;
        let result = provider
            .storage()
            .unit(|u| {
                f(
                    &Ctx {
                        provider,
                        signer: signer.as_ref(),
                    },
                    u,
                )
            })
            .map_err(ClientError::from);
        if result.is_err() {
            groups.clear();
        }
        result
    }

    fn read<T>(
        &self,
        f: impl FnOnce(&rusqlite::Connection) -> Result<T, StorageError>,
    ) -> Result<T, ClientError> {
        self.provider
            .storage()
            .unit(|u| u.with_conn(f))
            .map_err(|e| match e {
                crate::mls::TxError::RolledBack(e) => e.into(),
                other => ClientError::new(E_CORE_STORAGE, other.to_string()),
            })
    }

    pub fn open(conn: ConnHandle) -> Result<Self, ClientError> {
        let provider = DillaProvider::new(conn);
        // C4x: a store a newer client wrote is refused (E_STORE_NEWER) by a read before any
        // migration, the OpenMLS storage's included, can create or change anything in it.
        provider.storage().read(schema::refuse_newer)?;
        provider.storage().exec("PRAGMA secure_delete = ON")?;
        provider.storage().migrate()?;
        let version = provider
            .storage()
            .unit(|u| -> Result<u64, ClientError> {
                let found = u.with_conn(schema::migrate_conn).map_err(schema_error)?;
                if found == 1 {
                    backfill(provider.storage(), u).map_err(schema_error)?;
                }
                if found == 1 || found == 2 {
                    backfill_v3(u).map_err(schema_error)?;
                }
                if found <= schema::APP_SCHEMA_SUPPORTED as u64 {
                    u.with_conn(trust::seed_own_pin).map_err(schema_error)?;
                }
                Ok(found)
            })
            .map_err(ClientError::from)?;
        // A store newer than this build was refused untouched by `migrate_conn` (E_STORE_NEWER).
        debug_assert!(version <= u64::from(schema::APP_SCHEMA_SUPPORTED));
        let (identity, signup, enrol) = provider
            .storage()
            .unit(|u| u.with_conn(identity::load_phase))
            .map_err(|e| match e {
                crate::mls::TxError::RolledBack(e) => e.into(),
                other => ClientError::new(E_CORE_STORAGE, other.to_string()),
            })?;
        let phase = identity::decode_phase(identity, signup, enrol)?;
        let bot_pending_dsk = bot_identity::validate_open(&provider, &phase)?;
        let dsk_pub = match &phase {
            identity::Phase::None => None,
            identity::Phase::Pending(rec) => Some(rec.dsk_pub()),
            identity::Phase::Ready(rec) => Some(rec.dsk_pub),
            identity::Phase::Enrolling(rec) => Some(rec.dsk_pub),
        }
        .or(bot_pending_dsk);
        let signer = dsk_pub
            .map(|pubkey| {
                SignatureKeyPair::read(
                    provider.storage(),
                    &pubkey,
                    CIPHERSUITE.signature_algorithm(),
                )
                .ok_or_else(|| {
                    ClientError::new(E_CORE_STATE, "the device key is missing from the store")
                })
            })
            .transpose()?;
        Ok(Self {
            provider,
            signer,
            groups: BTreeMap::new(),
        })
    }

    pub fn unload(&mut self) {
        self.groups.clear();
    }
}

/// Whether a loaded group is the one its row describes: its MLS group id and authenticated
/// binding match the row, including this identity's participant tuple for interaction groups.
fn matches_row(group: &DillaGroup, group_id: &[u8; 16], row: &groups::GroupRow, own: &Own) -> bool {
    if group.group_id().as_slice() != group_id {
        return false;
    }
    if group.kind() == crate::mls::GroupKind::Interaction {
        let Ok(binding) = group.interaction_binding() else {
            return false;
        };
        return row.kind == i64::from(crate::mls::GroupKind::Interaction.as_u8())
            && binding.community_id.as_bytes().as_slice()
                == row.community_id.as_deref().unwrap_or_default()
            && interaction_binding_matches_own(&binding, own)
            && binding.bot_user_id.as_slice() == row.target_id.as_slice()
            && interaction_row_device_scope_matches(&binding, row);
    }
    let Ok(binding) = group.binding() else {
        return false;
    };
    matches!(
        binding.kind,
        crate::mls::GroupKind::Text | crate::mls::GroupKind::Call
    ) && row.kind == groups::row_kind(binding.kind)
        && binding.target_id.as_slice() == row.target_id.as_slice()
        && binding
            .community_id
            .as_ref()
            .map(|c| c.as_bytes().as_slice())
            == row.community_id.as_deref()
}

fn matches_bot_interaction_row(
    group: &DillaGroup,
    group_id: &[u8; 16],
    row: &groups::GroupRow,
    own: &Own,
) -> bool {
    if group.group_id().as_slice() != group_id
        || row.kind != i64::from(crate::mls::GroupKind::Interaction.as_u8())
    {
        return false;
    }
    let Ok(binding) = group.interaction_binding() else {
        return false;
    };
    binding.community_id.as_bytes().as_slice() == row.community_id.as_deref().unwrap_or_default()
        && interaction_binding_matches_bot(group, &binding, row, own)
}

fn interaction_row_device_scope_matches(
    binding: &crate::mls::DillaInteractionBinding,
    row: &groups::GroupRow,
) -> bool {
    match (
        row.interaction_installation_id.as_deref(),
        row.interaction_device_id.as_deref(),
    ) {
        (None, None) => true,
        (Some(installation), Some(device)) => {
            installation == binding.installation_id.as_slice()
                && device == binding.participant_device_id.as_bytes()
        }
        _ => false,
    }
}

fn matches_bot_interaction_roster(
    group: &DillaGroup,
    own: &Own,
    participant_user: &[u8; 16],
    participant_device: &[u8; 16],
) -> bool {
    if group.member_count() != 2 {
        return false;
    }
    let Ok(binding) = group.interaction_binding() else {
        return false;
    };
    if binding.participant_user_id.as_slice() != participant_user
        || binding.participant_device_id.as_bytes() != participant_device
    {
        return false;
    }
    let mut own_seen = false;
    let mut participant_seen = false;
    for member in group.members() {
        let Ok(basic) = openmls::prelude::BasicCredential::try_from(member.credential) else {
            return false;
        };
        let Ok(credential) = CredentialIdentity::decode(basic.identity()) else {
            return false;
        };
        if member.index.u32() == group.own_leaf_index().u32() {
            own_seen = credential.kind == Kind::Bot
                && credential.tier == Tier::Native
                && credential.user_id.as_bytes() == &own.user_id
                && credential.device_id.as_bytes() == &own.device_id;
        } else {
            participant_seen = credential.kind == Kind::User
                && credential.user_id.as_bytes() == participant_user
                && credential.device_id.as_bytes() == participant_device;
        }
    }
    own_seen && participant_seen
}

fn interaction_binding_matches_own(
    binding: &crate::mls::DillaInteractionBinding,
    own: &Own,
) -> bool {
    own.kind == Kind::User.as_u8()
        && binding.instance_id.as_bytes() == &own.instance_id
        && binding.participant_user_id.as_slice() == own.user_id
        && binding.participant_device_id.as_bytes() == &own.device_id
}

fn interaction_binding_matches_bot(
    group: &DillaGroup,
    binding: &crate::mls::DillaInteractionBinding,
    row: &groups::GroupRow,
    own: &Own,
) -> bool {
    if !interaction_bot_binding_matches_identity(binding, &row.target_id, own)
        || !interaction_row_device_scope_matches(binding, row)
        || group.members().len() != 2
    {
        return false;
    }
    let Some(own_leaf) = group.member_at(group.own_leaf_index().u32()) else {
        return false;
    };
    let Ok(basic) = openmls::prelude::BasicCredential::try_from(own_leaf.credential) else {
        return false;
    };
    interaction_bot_leaf_identity_matches(own, basic.identity(), &own_leaf.signature_key)
}

fn interaction_bot_binding_matches_identity(
    binding: &crate::mls::DillaInteractionBinding,
    row_target: &[u8],
    own: &Own,
) -> bool {
    own.kind == Kind::Bot.as_u8()
        && own.tier == Tier::Native.as_u8()
        && binding.instance_id.as_bytes() == &own.instance_id
        && binding.bot_user_id == own.user_id
        && row_target == binding.participant_user_id
}

fn interaction_bot_leaf_identity_matches(
    own: &Own,
    credential: &[u8],
    signature_key: &[u8],
) -> bool {
    let Ok(identity) = CredentialIdentity::decode(credential) else {
        return false;
    };
    own.kind == Kind::Bot.as_u8()
        && own.tier == Tier::Native.as_u8()
        && credential == own.credential.as_slice()
        && signature_key == own.dsk_pub
        && identity.kind == Kind::Bot
        && identity.tier == Tier::Native
        && identity.user_id.as_bytes() == &own.user_id
        && identity.device_id.as_bytes() == &own.device_id
}

#[cfg(test)]
#[expect(
    clippy::items_after_test_module,
    reason = "The reload tests stay beside the identity predicates they cover."
)]
mod interaction_reload_tests {
    use super::{
        Own, interaction_binding_matches_own, interaction_bot_binding_matches_identity,
        interaction_bot_leaf_identity_matches,
    };
    use crate::{
        ids::{CommunityId, DeviceId, InstanceId},
        mls::DillaInteractionBinding,
    };

    fn binding() -> DillaInteractionBinding {
        DillaInteractionBinding {
            instance_id: InstanceId::from_bytes([1; 16]),
            bot_user_id: [2; 16],
            installation_id: [3; 16],
            community_id: CommunityId::from_bytes([4; 16]),
            participant_user_id: [5; 16],
            participant_device_id: DeviceId::from_bytes([6; 16]),
            policy_version: 1,
            e2ee_version: 1,
            media_version: 1,
        }
    }

    fn own() -> Own {
        Own {
            instance_id: [1; 16],
            user_id: [5; 16],
            device_id: [6; 16],
            dsk_pub: [0; 32],
            credential: Vec::new(),
            kind: crate::identity::Kind::User.as_u8(),
            tier: 1,
        }
    }

    fn bot() -> Own {
        let mut own = own();
        own.user_id = [2; 16];
        own.device_id = [3; 16];
        own.dsk_pub = [9; 32];
        own.credential = vec![0x42];
        own.kind = crate::identity::Kind::Bot.as_u8();
        own.tier = crate::identity::Tier::Native.as_u8();
        own
    }

    #[test]
    fn interaction_reload_binding_must_belong_to_this_instance_user_and_device() {
        let mut b = binding();
        assert!(interaction_binding_matches_own(&b, &own()));
        b.instance_id = InstanceId::from_bytes([9; 16]);
        assert!(
            !interaction_binding_matches_own(&b, &own()),
            "wrong instance"
        );
        b = binding();
        b.participant_user_id = [9; 16];
        assert!(
            !interaction_binding_matches_own(&b, &own()),
            "wrong participant user"
        );
        b = binding();
        b.participant_device_id = DeviceId::from_bytes([9; 16]);
        assert!(
            !interaction_binding_matches_own(&b, &own()),
            "wrong participant device"
        );
        let mut human = own();
        human.kind = crate::identity::Kind::Bot.as_u8();
        assert!(
            !interaction_binding_matches_own(&binding(), &human),
            "Bot identity cannot load the human participant path"
        );
    }

    #[test]
    fn bot_interaction_reload_binds_bot_identity_and_participant_target() {
        let b = binding();
        let own = bot();
        assert!(interaction_bot_binding_matches_identity(&b, &[5; 16], &own));

        let mut wrong_user = bot();
        wrong_user.user_id = [9; 16];
        assert!(!interaction_bot_binding_matches_identity(
            &b,
            &[5; 16],
            &wrong_user
        ));

        assert!(!interaction_bot_binding_matches_identity(
            &b, &[9; 16], &own
        ));

        let mut wrong_instance = bot();
        wrong_instance.instance_id = [9; 16];
        assert!(!interaction_bot_binding_matches_identity(
            &b,
            &[5; 16],
            &wrong_instance
        ));

        let mut wrong_tier = bot();
        wrong_tier.tier = crate::identity::Tier::Browser.as_u8();
        assert!(!interaction_bot_binding_matches_identity(
            &b,
            &[5; 16],
            &wrong_tier
        ));

        let mut wrong_device = bot();
        wrong_device.device_id = [8; 16];
        assert!(!interaction_bot_leaf_identity_matches(
            &wrong_device,
            &[0x42],
            &[9; 32],
        ));
        assert!(!interaction_bot_leaf_identity_matches(
            &own,
            &[0x43],
            &[9; 32],
        ));
    }
}

fn mismatched() -> ClientError {
    ClientError::new(
        E_CORE_STATE,
        "the stored group does not match its row; resync the group",
    )
}

/// `open`'s mapping of the migration unit's storage errors: a `Codec` error carries its own
/// detail ("app_meta schema is malformed", "app_meta identity is malformed"), every other one is
/// `ClientError::from` (`StoreNewer` is `E_STORE_NEWER`).
fn schema_error(e: StorageError) -> ClientError {
    match e {
        StorageError::Codec(m) => ClientError::new(E_CORE_STORAGE, m),
        other => other.into(),
    }
}

/// The v1 -> v2 backfill (L-CORE-20), inside the unit that ran `MIGRATE_V1_TO_V2`, so a failure
/// anywhere rolls the `ALTER TABLE`s back with it and the store stays v1 until the next open:
/// (a) the identity record, when present, is rewritten as v2; (b) `mention` is set on every
/// status-0 type-0 row from another user (by `sender_user`: the own user's other devices are
/// excluded too, ruling 29) whose body `mentions_me`; (c) `epoch` and `pending_commit` of every
/// row in states 0-3 are read from its stored MLS group.
///
/// A row with no stored MLS group (a discarded resync) keeps `0, 0`, which is what the writers
/// leave for it. Any other load error (storage, decode, binding) fails the migration: a store
/// this build cannot read now may read fine at the next open (a transient storage error), and
/// `take_group` compares only the id, kind, target and community, so a column backfilled as 0
/// for a group that does load would be wrong until its next merge, with nothing to report it.
/// The store is device-local and written only by this browser: only a store this build did not
/// write, or a failing disk, fails here.
///
/// Loads through `storage`, the unit's own: no second `DillaStorage` over the connection.
fn backfill(storage: &DillaStorage, u: &UnitScope<'_>) -> Result<(), StorageError> {
    let own_user =
        identity::upgrade_identity_record(u).map_err(|e| StorageError::Codec(e.detail))?;
    // (b) and the ids of (c), in one `with_conn`. The mention flag is `mentions_me` evaluated by
    // SQLite: the same three needles (`mention_needle`, "<@everyone>", "<@here>") and the same
    // byte match (`instr` compares the bytes of its two text arguments), in one statement, which
    // costs the browser build far less than a row loop calling `mentions_me`.
    // `a_v1_store_flags_mentions_as_mentions_me_does` pins the equivalence on mentions_me's own
    // vectors.
    let ids = u.with_conn(|c| {
        if let Some(user) = own_user {
            c.execute(
                "UPDATE app_messages SET mention = 1 \
                 WHERE status = 0 AND type = 0 AND sender_user <> ?1 \
                 AND (instr(body, CAST(?2 AS TEXT)) > 0 \
                 OR instr(body, '<@everyone>') > 0 OR instr(body, '<@here>') > 0)",
                rusqlite::params![user.as_slice(), messages::mention_needle(&user).as_slice()],
            )?;
        }
        let mut s = c.prepare(
            "SELECT group_id FROM app_groups WHERE state IN (0, 1, 2, 3) ORDER BY group_id",
        )?;
        let mut rows = s.query([])?;
        let mut ids: Vec<[u8; 16]> = Vec::new();
        while let Some(r) = rows.next()? {
            ids.push(r.get(0)?);
        }
        Ok(ids)
    })?;
    for id in ids {
        // Outside `with_conn`: the load reads through the storage, which takes the connection.
        match DillaGroup::load_stored(storage, &GroupId::from_slice(&id)) {
            Ok(Some(g)) => {
                let epoch = i64::try_from(g.epoch())
                    .map_err(|_| StorageError::Codec("epoch out of range".into()))?;
                u.with_conn(|c| groups::set_mls(c, &id, epoch, g.has_pending_commit()))?;
            }
            Ok(None) => {}
            Err(crate::mls::MlsError::Storage(e)) => return Err(e),
            Err(e) => return Err(StorageError::Codec(e.to_string())),
        }
    }
    Ok(())
}

/// Migrates stored envelope references and derived rows in the same unit as the v3 DDL.
fn backfill_v3(u: &UnitScope<'_>) -> Result<(), StorageError> {
    u.with_conn(|c| {
        let envelopes: Vec<([u8; 16], i64, Vec<u8>)> = {
            let mut stmt = c.prepare("SELECT group_id, seq, envelope FROM app_messages WHERE status = 0 AND envelope IS NOT NULL")?;
            stmt.query_map([], |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?)))?
                .collect::<Result<_, _>>()?
        };
        for (group, seq, envelope) in envelopes {
            if let Some(reply) = stored_reply_to(&envelope)? {
                c.execute("UPDATE app_messages SET reply_to = ?3 WHERE group_id = ?1 AND seq = ?2",
                    rusqlite::params![group.as_slice(), seq, reply.as_slice()])?;
            }
        }
        let targets: Vec<([u8; 16], [u8; 16])> = {
            let mut stmt = c.prepare("SELECT DISTINCT group_id, reply_to FROM app_messages WHERE status = 0 AND type BETWEEN 1 AND 6 AND reply_to IS NOT NULL ORDER BY group_id, reply_to")?;
            stmt.query_map([], |r| Ok((r.get(0)?, r.get(1)?)))?.collect::<Result<_, _>>()?
        };
        for (group, target) in targets {
            fold::refold(c, &group, &target, &fold::FoldScope::Full, None, None)?;
        }
        Ok(())
    })
}

fn stored_reply_to(envelope: &[u8]) -> Result<Option<[u8; 16]>, StorageError> {
    crate::cbor::decode_strict(envelope, |d| {
        d.array(9)?;
        for _ in 0..4 {
            d.skip()?;
        }
        let reply = d.opt_bytes_exact::<16>()?;
        for _ in 0..4 {
            d.skip()?;
        }
        Ok(reply)
    })
    .map_err(|_| StorageError::Codec("app_messages envelope does not decode".into()))
}
