//! Browser and desktop client engine: Rust decides and persists; the caller transports.
//! Every structured value crossing the boundary is one deterministic CBOR item. Every state
//! change runs inside exactly one `DillaStorage::unit`.

mod account_kind;
mod bot_command;
mod bot_events;
mod bot_identity;
mod bot_intents;
mod bot_keypackages;
mod bot_probe;
mod calls;
mod error;
mod fold;
mod groups;
mod heal;
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
pub use trust::PushedList;

use crate::identity::CredentialIdentity;
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
    /// The kind-3 groups `open` removed (Bot v2 Task 23); the native Bot helper logs it once.
    interaction_groups_removed: u64,
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
    /// row describes: its MLS group id and its authenticated binding match the row. Otherwise
    /// `E_CORE_STATE` and nothing is cached; the caller repairs the row with a
    /// resync (`group_join_external`, states 2 and 3), which replaces the stored group. The cache
    /// only ever holds a group that passed this check or that a join of this core wrote.
    fn take_group(&mut self, group_id: &[u8; 16]) -> Result<Option<DillaGroup>, ClientError> {
        if let Some(group) = self.groups.remove(group_id) {
            return Ok(Some(group));
        }
        let Some(group) = self.take_group_unchecked(group_id)? else {
            return Ok(None);
        };
        let row = self.read(|c| groups::group_row(c, group_id))?;
        if let Some(row) = row
            && !matches_row(&group, group_id, &row)
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
            ..
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
        let interaction_groups_removed = remove_interaction_groups(&provider)?;
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
            interaction_groups_removed,
        })
    }

    /// How many kind-3 (Bot interaction) groups this open removed from the store (Bot v2 Task 23). A
    /// later open finds none.
    pub fn interaction_groups_removed_at_open(&self) -> u64 {
        self.interaction_groups_removed
    }

    pub fn unload(&mut self) {
        self.groups.clear();
    }
}

/// Bot v2 Task 23: kind-3 (Bot interaction) groups are gone. A store that still holds one is cleaned
/// at open: the group's MLS state, its per-group rows and its `app_groups` row are deleted, each group in
/// one unit. It answers how many it removed. The MLS state is loaded without its binding, which no
/// longer decodes.
fn remove_interaction_groups(provider: &DillaProvider) -> Result<u64, ClientError> {
    let ids: Vec<Vec<u8>> = provider.storage().read(|c| {
        let mut stmt = c.prepare("SELECT group_id FROM app_groups WHERE kind = 3")?;
        let rows = stmt.query_map([], |r| r.get::<_, Vec<u8>>(0))?;
        rows.collect::<Result<_, _>>().map_err(StorageError::from)
    })?;
    for id in &ids {
        provider
            .storage()
            .unit(|u| -> Result<(), ClientError> {
                if let Some(mut group) =
                    openmls::group::MlsGroup::load(provider.storage(), &GroupId::from_slice(id))
                        .map_err(|e| ClientError::new(E_CORE_STORAGE, format!("{e:?}")))?
                {
                    group
                        .delete(provider.storage())
                        .map_err(|e| ClientError::new(E_CORE_STORAGE, format!("{e:?}")))?;
                }
                u.with_conn(|c| {
                    for table in [
                        "app_proposals",
                        "app_handshake_tail",
                        "app_quarantine",
                        "app_trust_audit",
                        "app_pending_history",
                        "app_messages",
                        "app_outbox",
                        "app_read_state",
                        "app_pins",
                        "app_reactions",
                        "app_authority",
                        "app_authority_known",
                        "app_authority_mentionable",
                        "app_bot_events",
                        "app_groups",
                    ] {
                        c.execute(
                            &format!("DELETE FROM {table} WHERE group_id = ?1"),
                            [id.as_slice()],
                        )?;
                    }
                    Ok(())
                })?;
                Ok(())
            })
            .map_err(|e| match e {
                crate::mls::TxError::RolledBack(e) => e,
                other => ClientError::new(E_CORE_STORAGE, other.to_string()),
            })?;
    }
    Ok(ids.len() as u64)
}

/// Whether a loaded group is the one its row describes: its MLS group id and authenticated
/// binding match the row.
fn matches_row(group: &DillaGroup, group_id: &[u8; 16], row: &groups::GroupRow) -> bool {
    if group.group_id().as_slice() != group_id {
        return false;
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
