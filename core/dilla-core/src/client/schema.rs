//! App schema and metadata operations.

use crate::cbor::decode_strict;
use crate::mls::{DillaStorage, StorageError, TxError};
#[cfg(any(test, feature = "test-hooks"))]
use core::sync::atomic::{AtomicBool, Ordering};
use rusqlite::OptionalExtension;

#[cfg(any(test, feature = "test-hooks"))]
static FORCE_MISSING_FTS5: AtomicBool = AtomicBool::new(false);

/// Test-build-only simulation of an OPFS SQLite binary without FTS5.
#[cfg(any(test, feature = "test-hooks"))]
#[allow(dead_code)] // The browser fixture uses this only through the feature-gated facade.
pub fn test_force_missing_fts5(on: bool) {
    FORCE_MISSING_FTS5.store(on, Ordering::SeqCst);
}

/// Rows of `app_handshake_tail` kept per group; the oldest are deleted in the inserting unit.
pub const HANDSHAKE_TAIL: usize = 64;

/// The highest app schema this build reads and migrates to. A store whose `app_meta` schema is
/// above it was written by a newer client and is refused untouched (`StorageError::StoreNewer`).
pub const APP_SCHEMA_SUPPORTED: u32 = 13;

pub(crate) const SCHEMA: &str = "schema";
pub(crate) const SIGNUP: &str = "signup";
/// The separate native bot bootstrap phase; never interpreted as a human signup record.
pub(crate) const BOT_PENDING: &str = "bot_pending";
/// Installation binding retained after the bot identity is completed.
pub(crate) const BOT_BINDING: &str = "bot_binding";
/// External-sender key learned from authenticated Go `/v1/instance` discovery.
pub(crate) const BOT_INSTANCE_KEY: &str = "bot_instance_key";
/// External-sender signing key learned over an authenticated human enrollment session.
pub(crate) const INSTANCE_SIGNER_KEY: &str = "instance_signer_key";
pub(super) const BOT_INTENTS_SCHEMA: &str = "
CREATE TABLE IF NOT EXISTS app_bot_intents (
  operation_id BLOB PRIMARY KEY CHECK(length(operation_id)=16 AND operation_id != zeroblob(16)),
  installation_id BLOB NOT NULL CHECK(length(installation_id)=16),
  group_id BLOB NOT NULL CHECK(length(group_id)=16),
  kind INTEGER NOT NULL CHECK(kind IN (2,5,9)),
  input_hash BLOB NOT NULL CHECK(length(input_hash)=32),
  state INTEGER NOT NULL DEFAULT 0 CHECK(state IN (0,1,2)),
  result BLOB,
  resolution_hash BLOB CHECK(resolution_hash IS NULL OR length(resolution_hash)=32),
  CHECK((state=0 AND result IS NOT NULL AND resolution_hash IS NULL) OR
        (state IN (1,2) AND result IS NULL AND resolution_hash IS NOT NULL))
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS app_bot_intents_by_group ON app_bot_intents(group_id,kind,state);
CREATE UNIQUE INDEX IF NOT EXISTS app_bot_intents_one_pending_per_group
  ON app_bot_intents(group_id) WHERE state=0;
";
pub(super) const BOT_KEYPACKAGES_SCHEMA: &str = "
CREATE TABLE IF NOT EXISTS app_bot_keypackage_intents (
  operation_id BLOB PRIMARY KEY CHECK(length(operation_id)=16 AND operation_id != zeroblob(16)),
  instance_id BLOB NOT NULL CHECK(length(instance_id)=16),
  community_id BLOB NOT NULL CHECK(length(community_id)=16),
  installation_id BLOB NOT NULL CHECK(length(installation_id)=16),
  device_id BLOB NOT NULL CHECK(length(device_id)=16),
  input_hash BLOB NOT NULL CHECK(length(input_hash)=32),
  expected_generation INTEGER NOT NULL CHECK(expected_generation>=0),
  upload_body BLOB,
  package_refs BLOB NOT NULL,
  greatest_expiry INTEGER NOT NULL CHECK(greatest_expiry>=0),
  state INTEGER NOT NULL DEFAULT 0 CHECK(state IN (0,1,2)),
  resolution_hash BLOB CHECK(resolution_hash IS NULL OR length(resolution_hash)=32),
  CHECK((state=0 AND upload_body IS NOT NULL AND resolution_hash IS NULL) OR
        (state IN (1,2) AND upload_body IS NULL AND resolution_hash IS NOT NULL))
) WITHOUT ROWID;
CREATE UNIQUE INDEX IF NOT EXISTS app_bot_keypackage_one_pending
  ON app_bot_keypackage_intents(instance_id,community_id,installation_id,device_id) WHERE state=0;
";
pub(crate) const IDENTITY: &str = "identity";
pub(crate) const ENROL: &str = "enrol";
pub(crate) const SESSION: &str = "session";
pub(crate) const ROOT_SEALED: &str = "root_sealed";
pub(crate) const STATE_SEALED: &str = "state_sealed";
/// The version of the device list inside `state_sealed`, as a CBOR uint, written with it
/// (BACKUPS-RECOVERY-03): the browser cannot open its own state object to read it back.
pub(crate) const STATE_LIST: &str = "state_list";
/// The sealed state as it stood before a revocation's candidate replaced it (fix-wave review
/// NEW-2): `[state_sealed bstr|null, state_list bstr|null, state_uploaded uint]`, the two meta
/// values as stored. A dropped candidate restores it; a published one deletes it.
pub(crate) const STATE_PRIOR: &str = "state_prior";

/// App schema v3 column meanings (L-SQL-10, L-SQL-20, L-SQL-30):
///
/// - `app_groups.state`: 0 registering, 1 joining, 2 active, 3 needs_resync, 4 gone.
/// - `app_groups.next_seq`: every delivery-service seq below it is applied or skipped.
/// - `app_groups.resync`: 1 while a state-1 row is joining over a local group that was in state 2 or 3.
/// - `app_groups.was_gone`: 1 while a state-1 row is joining over a row that was gone (state 4);
///   `group_discard` returns such a row to state 4 with its history. A state-1 row with both 0 was
///   created by its join, and discard deletes it.
/// - `app_groups.max_epoch`: the highest epoch this device has held for the group id, only ever
///   raised, and written when the epoch is reached: by every commit merged in `group_apply` and
///   `commit_confirm` (the new epoch), by `group_joined` (the epoch the accepted external commit
///   opened) and by `welcomes_apply` (the joined epoch, which must equal the served label). These
///   are every epoch change of a stored group, so deleting a group (removal, resync, discard)
///   needs no write of its own. Over an existing row a Welcome into an epoch at or below the floor
///   (`max(max_epoch, stored group's epoch)`) is refused, and so is a GroupInfo below it (its join
///   would land at or below it); `E_CORE_INPUT`.
/// - `app_groups.epoch`: the MLS epoch of the stored group, 0 when none is stored, written by
///   every writer of L-CORE-21 in the unit that changes it (a join that is not yet accepted
///   writes it, unlike `max_epoch`).
/// - `app_groups.pending_commit`: 1 while this device holds a commit it built and has neither
///   confirmed, merged nor cleared, else 0.
/// - `app_messages.status`: 0 ok, 1 cannot_decrypt, 2 deleted.
/// - `app_messages.sender_user` (and `sender_leaf`, `sender_kind`, `sender_tier`, `msg_id`,
///   `type`): set on a status-0 row (`sender_leaf` NULL when `send_confirm` found no stored group);
///   NULL on a status-1 row. A status-2 row keeps the values of
///   the row it cleared (set if that row was status 0), or, inserted by rule 2 for a seq not
///   stored before, has them NULL, except the deleted marker of this device's own upload adopted
///   from the outbox, which carries this device's identity, `msg_id` and `type`.
/// - `app_messages.sender_device`: status 0, and a status-2 row that cleared one: the MLS
///   credential's device; otherwise the DS uploader_device.
/// - `app_messages.envelope`: the envelope CBOR (with k_f) of an ok row; NULL otherwise.
/// - `app_messages.mention`: 1 on a status-0 type-0 row from another user (by `sender_user`,
///   ruling 29: the own user's other devices are excluded too) whose body `mentions_me_gated`
///   (`<@everyone>`/`<@here>` and own-role tokens by the group's role view, web3.5 R1 and W2a
///   review finding 1), else 0. `authority_set` recomputes it for the group's rows naming a
///   `<@…>` token from senders whose broadcast bit changed (from every sender when the view's
///   mentionable roles changed), and that rescan reads today's own roles (`app_roles`), not the
///   roles held when the row arrived: a rescanned row with an own-role token counts by the current
///   roles (ruling 12 holds for rows the rescan does not touch).
/// - `app_messages.reply_to`: the envelope's `reply_to` of a status-0 row, retained by a
///   status-2 row that kept its identity; NULL otherwise.
/// - `app_messages.edit_body` and `edit_seq`: the latest author's edit applied to a type-0 row;
///   NULL and 0 when there is none (L-CORE-33 step 3).
/// - `app_messages.thread_id` (schema 5, web3.7): the envelope's `thread_id` of a type-0 row
///   stored from a decrypted envelope (kept by the status-2 row that clears it); NULL on every
///   other row, and on the rows a schema-4 store held before it migrated (no backfill, R26).
/// - `app_reactions` and `app_pins`: derived per target by `fold::refold`.
/// - `app_purges`: the task-3 purge records.
/// - `app_roles`: this device's own role ids per community, replaced by `own_roles_set`.
/// - `app_authority`, `app_authority_known`, `app_authority_mentionable`: see `APP_AUTHORITY`.
/// - `app_outbox.state`: 0 queued, 1 in_flight, 2 failed.
/// - `app_outbox.epoch`: the epoch send_encrypt framed the message in; read by send_confirm.
/// - `app_read_state`: the device-local read marker of a group, never lowered.
/// - `app_settings`: device-local settings.
///
/// `APP_SCHEMA` runs on v1 and v2 stores before their migrations, so indexes naming migrated
/// columns are created by `APP_INDEXES_V3` afterwards.
pub(super) const APP_SCHEMA: &str = "
CREATE TABLE IF NOT EXISTS app_meta (k TEXT PRIMARY KEY, v BLOB NOT NULL) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS app_groups (
  group_id     BLOB    PRIMARY KEY CHECK (length(group_id) = 16),
  kind         INTEGER NOT NULL,
  community_id BLOB    CHECK (community_id IS NULL OR length(community_id) = 16),
  target_id    BLOB    NOT NULL CHECK (length(target_id) = 16),
  state        INTEGER NOT NULL,
  next_seq     INTEGER NOT NULL DEFAULT 1,
  acked_seq    INTEGER NOT NULL DEFAULT 0,
  acked_epoch  INTEGER NOT NULL DEFAULT 0,
  resync       INTEGER NOT NULL DEFAULT 0 CHECK (resync IN (0, 1)),
  was_gone     INTEGER NOT NULL DEFAULT 0 CHECK (was_gone IN (0, 1)),
  max_epoch    INTEGER NOT NULL DEFAULT 0,
  epoch          INTEGER NOT NULL DEFAULT 0,
  pending_commit INTEGER NOT NULL DEFAULT 0,
  interaction_installation_id BLOB CHECK (interaction_installation_id IS NULL OR length(interaction_installation_id)=16),
  interaction_device_id BLOB CHECK (interaction_device_id IS NULL OR length(interaction_device_id)=16),
  CHECK ((interaction_installation_id IS NULL AND interaction_device_id IS NULL) OR
         (interaction_installation_id IS NOT NULL AND interaction_device_id IS NOT NULL))
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS app_groups_by_target ON app_groups (target_id, kind) WHERE state <> 4;
CREATE TABLE IF NOT EXISTS app_handshake_tail (
  group_id BLOB NOT NULL, seq INTEGER NOT NULL, epoch INTEGER NOT NULL, kind INTEGER NOT NULL,
  sender INTEGER, blob BLOB NOT NULL, PRIMARY KEY (group_id, seq)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS app_proposals (
  group_id BLOB NOT NULL, ref BLOB NOT NULL, epoch INTEGER NOT NULL, PRIMARY KEY (group_id, ref)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS app_messages (
  group_id      BLOB    NOT NULL,
  seq           INTEGER NOT NULL,
  epoch         INTEGER NOT NULL,
  recv_ts       INTEGER NOT NULL,
  status        INTEGER NOT NULL,
  reason        TEXT    NOT NULL DEFAULT '',
  sender_user   BLOB,
  sender_device BLOB    NOT NULL,
  sender_leaf   INTEGER,
  sender_kind   INTEGER,
  sender_tier   INTEGER,
  msg_id        BLOB,
  type          INTEGER,
  body          TEXT    NOT NULL DEFAULT '',
  envelope      BLOB,
  franking_tag  BLOB    NOT NULL,
  mention       INTEGER NOT NULL DEFAULT 0,
  reply_to      BLOB,
  edit_body     TEXT,
  edit_seq      INTEGER NOT NULL DEFAULT 0,
  sender_sig_key BLOB,
  PRIMARY KEY (group_id, seq)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS app_messages_by_msg ON app_messages (group_id, msg_id);
CREATE TABLE IF NOT EXISTS app_outbox (
  msg_id   BLOB    PRIMARY KEY CHECK (length(msg_id) = 16),
  group_id BLOB    NOT NULL,
  envelope BLOB    NOT NULL,
  created  INTEGER NOT NULL,
  state    INTEGER NOT NULL,
  error    TEXT    NOT NULL DEFAULT '',
  epoch    INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS app_outbox_by_group ON app_outbox (group_id, created, msg_id);
CREATE TABLE IF NOT EXISTS app_read_state (
  group_id      BLOB    PRIMARY KEY CHECK (length(group_id) = 16),
  last_read_seq INTEGER NOT NULL DEFAULT 0,
  last_read_at  INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS app_settings (k TEXT PRIMARY KEY, v TEXT NOT NULL) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS app_reactions (
  group_id BLOB NOT NULL, target BLOB NOT NULL CHECK (length(target) = 16),
  user_id  BLOB NOT NULL CHECK (length(user_id) = 16), emoji TEXT NOT NULL, seq INTEGER NOT NULL,
  PRIMARY KEY (group_id, target, user_id, emoji)) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS app_pins (
  group_id BLOB NOT NULL, target BLOB NOT NULL CHECK (length(target) = 16), seq INTEGER NOT NULL,
  by_user BLOB NOT NULL CHECK (length(by_user) = 16), PRIMARY KEY (group_id, target)) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS app_purges (
  group_id BLOB NOT NULL, seq INTEGER NOT NULL, channel_id BLOB NOT NULL CHECK (length(channel_id) = 16),
  blob_ids BLOB NOT NULL, PRIMARY KEY (group_id, seq)) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS app_roles (
  community_id BLOB NOT NULL CHECK (length(community_id) = 16), role_id BLOB NOT NULL CHECK (length(role_id) = 16),
  PRIMARY KEY (community_id, role_id)) WITHOUT ROWID;
INSERT OR IGNORE INTO app_meta (k, v) VALUES ('schema', x'03');
";

const MIGRATE_V2_TO_V3: &str = "
ALTER TABLE app_messages ADD COLUMN reply_to  BLOB;
ALTER TABLE app_messages ADD COLUMN edit_body TEXT;
ALTER TABLE app_messages ADD COLUMN edit_seq  INTEGER NOT NULL DEFAULT 0;
UPDATE app_meta SET v = x'03' WHERE k = 'schema';
";

pub(super) const APP_INDEXES_V3: &str = "
CREATE INDEX IF NOT EXISTS app_messages_by_reply ON app_messages (group_id, reply_to, type, sender_user, seq);
CREATE INDEX IF NOT EXISTS app_messages_by_pin ON app_messages (group_id, reply_to, type, seq);
CREATE INDEX IF NOT EXISTS app_messages_by_reaction ON app_messages (group_id, reply_to, sender_user, body, seq);
";

const APP_SCHEMA_V4: &str = "
CREATE TABLE IF NOT EXISTS app_device_lists (
  user_id BLOB PRIMARY KEY CHECK(length(user_id)=16),
  version INTEGER NOT NULL, list BLOB NOT NULL,
  umk_pub BLOB NOT NULL CHECK(length(umk_pub)=32),
  ssk_pub BLOB NOT NULL CHECK(length(ssk_pub)=32),
  credential BLOB CHECK(credential IS NULL OR length(credential)>0)
);
CREATE TABLE IF NOT EXISTS app_trust_pins (
  user_id BLOB PRIMARY KEY CHECK(length(user_id)=16),
  umk_pub BLOB NOT NULL CHECK(length(umk_pub)=32),
  first_seen INTEGER NOT NULL,
  verified INTEGER NOT NULL CHECK(verified IN(0,1)),
  changed_umk BLOB CHECK(changed_umk IS NULL OR length(changed_umk)=32)
);
CREATE TABLE IF NOT EXISTS app_quarantine (
  group_id BLOB NOT NULL, user_id BLOB NOT NULL CHECK(length(user_id)=16),
  device_id BLOB NOT NULL CHECK(length(device_id)=16),
  signature_key BLOB NOT NULL DEFAULT x'' CHECK(length(signature_key) IN (0,32)),
  code TEXT NOT NULL,
  PRIMARY KEY(group_id,user_id,device_id,signature_key)
);
CREATE TABLE IF NOT EXISTS app_pending_history (
  group_id BLOB NOT NULL CHECK(length(group_id)=16),
  user_id BLOB NOT NULL CHECK(length(user_id)=16),
  device_id BLOB NOT NULL CHECK(length(device_id)=16),
  seq INTEGER NOT NULL,
  ciphertext BLOB NOT NULL,
  envelope BLOB NOT NULL,
  sender_leaf INTEGER NOT NULL,
  sender_credential BLOB NOT NULL,
  PRIMARY KEY(group_id,seq)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS app_pending_history_by_leaf_seq ON app_pending_history(group_id,user_id,device_id,sender_leaf,seq);
CREATE TABLE IF NOT EXISTS app_search_rows (
  rowid INTEGER PRIMARY KEY,
  group_id BLOB NOT NULL CHECK(length(group_id)=16),
  user_id BLOB NOT NULL CHECK(length(user_id)=16),
  device_id BLOB NOT NULL CHECK(length(device_id)=16),
  seq INTEGER NOT NULL, body TEXT NOT NULL, terms TEXT NOT NULL,
  UNIQUE(group_id,seq)
);
CREATE INDEX IF NOT EXISTS app_search_rows_recent ON app_search_rows(group_id DESC,seq DESC);
CREATE INDEX IF NOT EXISTS app_quarantine_device ON app_quarantine(user_id,device_id,group_id);
CREATE TABLE IF NOT EXISTS app_trust_audit (
  group_id BLOB NOT NULL CHECK(length(group_id)=16),
  user_id BLOB NOT NULL CHECK(length(user_id)=16),
  device_id BLOB NOT NULL CHECK(length(device_id)=16),
  epoch INTEGER NOT NULL, leaf_index INTEGER NOT NULL,
  signature_key BLOB NOT NULL, code TEXT NOT NULL,
  added_by_user BLOB CHECK(added_by_user IS NULL OR length(added_by_user)=16),
  added_by_device BLOB CHECK(added_by_device IS NULL OR length(added_by_device)=16),
  PRIMARY KEY(group_id,user_id,device_id,epoch,leaf_index)
) WITHOUT ROWID;
";

pub(super) const REMOVALS_SCHEMA: &str = "
CREATE TABLE IF NOT EXISTS app_removal_cursor (
  group_id BLOB PRIMARY KEY CHECK(length(group_id)=16), revision INTEGER NOT NULL CHECK(revision>=0)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS app_removal_tombstones (
  group_id BLOB NOT NULL CHECK(length(group_id)=16), seq INTEGER NOT NULL CHECK(seq>0),
  revision INTEGER NOT NULL CHECK(revision>0), PRIMARY KEY(group_id,seq)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS app_removal_entries (
  group_id BLOB NOT NULL CHECK(length(group_id)=16), revision INTEGER NOT NULL CHECK(revision>0),
  seq INTEGER NOT NULL CHECK(seq>0), proof BLOB NOT NULL, signature BLOB NOT NULL CHECK(length(signature)=64),
  accepted_at INTEGER NOT NULL CHECK(accepted_at>0), PRIMARY KEY(group_id,revision)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS app_removal_actor_lists (
  user_id BLOB PRIMARY KEY CHECK(length(user_id)=16), version INTEGER NOT NULL CHECK(version>0),
  list BLOB NOT NULL, credential BLOB NOT NULL CHECK(length(credential)>0)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS app_manual_removal_evidence (
  proof_request_id BLOB PRIMARY KEY CHECK(length(proof_request_id)=16 AND proof_request_id != zeroblob(16)),
  evidence_request_id BLOB NOT NULL CHECK(length(evidence_request_id)=16 AND evidence_request_id != zeroblob(16)),
  actor_user_id BLOB NOT NULL CHECK(length(actor_user_id)=16 AND actor_user_id != zeroblob(16)),
  actor_device_id BLOB NOT NULL CHECK(length(actor_device_id)=16 AND actor_device_id != zeroblob(16)),
  community_id BLOB NOT NULL CHECK(length(community_id)=16 AND community_id != zeroblob(16)),
  group_id BLOB NOT NULL CHECK(length(group_id)=16 AND group_id != zeroblob(16)),
  channel_id BLOB NOT NULL CHECK(length(channel_id)=16 AND channel_id != zeroblob(16)),
  message_id BLOB NOT NULL CHECK(length(message_id)=16 AND message_id != zeroblob(16)),
  target_sequence INTEGER NOT NULL CHECK(target_sequence>0),
  reason TEXT NOT NULL CHECK(length(CAST(reason AS BLOB)) BETWEEN 1 AND 1024),
  excerpt TEXT NOT NULL CHECK(length(CAST(excerpt AS BLOB))<=512),
  signed_body BLOB NOT NULL CHECK(length(signed_body) BETWEEN 1 AND 512),
  state INTEGER NOT NULL CHECK(state IN (0,1,2,3)),
  created_at INTEGER NOT NULL CHECK(created_at>=0),
  expires INTEGER NOT NULL CHECK(expires>0),
  evidence_expires INTEGER NOT NULL DEFAULT 0 CHECK(evidence_expires>=0)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS app_manual_removal_evidence_pending ON app_manual_removal_evidence(state,created_at);
CREATE TABLE IF NOT EXISTS app_manual_warning_evidence (
  request_id BLOB PRIMARY KEY CHECK(length(request_id)=16 AND request_id != zeroblob(16)),
  evidence_request_id BLOB NOT NULL CHECK(length(evidence_request_id)=16 AND evidence_request_id != zeroblob(16)),
  actor_user_id BLOB NOT NULL CHECK(length(actor_user_id)=16 AND actor_user_id != zeroblob(16)),
  actor_device_id BLOB NOT NULL CHECK(length(actor_device_id)=16 AND actor_device_id != zeroblob(16)),
  community_id BLOB NOT NULL CHECK(length(community_id)=16 AND community_id != zeroblob(16)),
  target_user_id BLOB NOT NULL CHECK(length(target_user_id)=16 AND target_user_id != zeroblob(16)),
  reason TEXT NOT NULL CHECK(length(CAST(reason AS BLOB)) BETWEEN 1 AND 1024),
  state INTEGER NOT NULL CHECK(state IN (0,1,2)),
  created_at INTEGER NOT NULL CHECK(created_at>=0),
  action_id BLOB CHECK(action_id IS NULL OR (length(action_id)=16 AND action_id != zeroblob(16))),
  revision INTEGER CHECK(revision IS NULL OR revision>0),
  accepted_at INTEGER CHECK(accepted_at IS NULL OR accepted_at>0),
  evidence_expires INTEGER NOT NULL DEFAULT 0 CHECK(evidence_expires>=0)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS app_manual_warning_evidence_pending ON app_manual_warning_evidence(state,created_at);
";

/// Schema 5 (web3.7 public threads): the thread column, added to a store found at schema 4 or
/// below (a fresh store reads 3), and its index, read by the thread summary, `thread_timeline` and
/// `threads`. Rows already stored keep a NULL `thread_id` (R26: no backfill).
pub(super) const MIGRATE_V4_TO_V5: &str = "ALTER TABLE app_messages ADD COLUMN thread_id BLOB CHECK (thread_id IS NULL OR length(thread_id) = 16);";
pub(super) const APP_INDEXES_V5: &str = "CREATE INDEX IF NOT EXISTS app_messages_by_thread ON app_messages (group_id, thread_id, type, seq);";

/// Schema 6: installation-local, authenticated native Bot delivery journal. Human/browser
/// stores may have empty tables; only validated native Bot operations write or export rows.
pub(super) const APP_BOT_EVENTS: &str = "
CREATE TABLE IF NOT EXISTS app_bot_events (
  delivery_id INTEGER PRIMARY KEY AUTOINCREMENT,
  group_id BLOB NOT NULL CHECK(length(group_id)=16),
  source_seq INTEGER NOT NULL,
  community_id BLOB NOT NULL CHECK(length(community_id)=16),
  channel_id BLOB NOT NULL CHECK(length(channel_id)=16),
  epoch INTEGER NOT NULL,
  recv_ts INTEGER NOT NULL,
  sender_user BLOB NOT NULL CHECK(length(sender_user)=16),
  sender_device BLOB NOT NULL CHECK(length(sender_device)=16),
  sender_kind INTEGER NOT NULL,
  sender_tier INTEGER NOT NULL,
  sender_leaf INTEGER NOT NULL,
  sender_credential BLOB NOT NULL,
  sender_sig_key BLOB NOT NULL CHECK(length(sender_sig_key)=32),
  message_id BLOB NOT NULL CHECK(length(message_id)=16),
  envelope_type INTEGER NOT NULL,
  original_body TEXT,
  reply_to BLOB CHECK(reply_to IS NULL OR length(reply_to)=16),
  thread_id BLOB CHECK(thread_id IS NULL OR length(thread_id)=16),
  disposition INTEGER NOT NULL DEFAULT 0 CHECK(disposition IN (0,1,2)),
  UNIQUE(group_id, source_seq)
);
CREATE INDEX IF NOT EXISTS app_bot_events_page ON app_bot_events(delivery_id);
CREATE TABLE IF NOT EXISTS app_bot_event_state (
  singleton INTEGER PRIMARY KEY CHECK(singleton=1),
  ack_delivery_id INTEGER NOT NULL DEFAULT 0
);
INSERT OR IGNORE INTO app_bot_event_state(singleton,ack_delivery_id) VALUES(1,0);
";

/// Durable bounded-on-pending human kind-3 Bot response journal. Event bodies are exposed only
/// after the human interaction binding, Bot MLS leaf, current trust and local request are rechecked.
pub(super) const APP_HUMAN_INTERACTION_EVENTS: &str = "
CREATE TABLE IF NOT EXISTS app_human_interaction_events (
  delivery_id INTEGER PRIMARY KEY AUTOINCREMENT,
  group_id BLOB NOT NULL CHECK(length(group_id)=16),
  community_id BLOB NOT NULL CHECK(length(community_id)=16),
  request_id BLOB NOT NULL CHECK(length(request_id)=16),
  source_seq INTEGER NOT NULL CHECK(source_seq>0),
  epoch INTEGER NOT NULL,
  recv_ts INTEGER NOT NULL,
  sender_user BLOB NOT NULL CHECK(length(sender_user)=16),
  sender_device BLOB NOT NULL CHECK(length(sender_device)=16),
  sender_leaf INTEGER NOT NULL,
  sender_credential BLOB NOT NULL,
  sender_sig_key BLOB NOT NULL CHECK(length(sender_sig_key)=32),
  message_id BLOB NOT NULL CHECK(length(message_id)=16),
  body TEXT,
  disposition INTEGER NOT NULL DEFAULT 0 CHECK(disposition IN (0,1,2)),
  UNIQUE(group_id,source_seq)
);
CREATE INDEX IF NOT EXISTS app_human_interaction_events_page ON app_human_interaction_events(delivery_id);
-- Consumed Bot responses stay encrypted in the native client database so an acknowledged
-- result can be recovered after a process restart. This is deliberately a separate bounded
-- archive: it never counts against or evicts pending journal rows.
CREATE TABLE IF NOT EXISTS app_human_interaction_receipts (
  receipt_id INTEGER PRIMARY KEY AUTOINCREMENT,
  delivery_id INTEGER NOT NULL UNIQUE,
  group_id BLOB NOT NULL CHECK(length(group_id)=16),
  community_id BLOB NOT NULL CHECK(length(community_id)=16),
  request_id BLOB NOT NULL CHECK(length(request_id)=16),
  source_seq INTEGER NOT NULL CHECK(source_seq>0),
  epoch INTEGER NOT NULL,
  recv_ts INTEGER NOT NULL,
  sender_user BLOB NOT NULL CHECK(length(sender_user)=16),
  sender_device BLOB NOT NULL CHECK(length(sender_device)=16),
  sender_leaf INTEGER NOT NULL,
  sender_credential BLOB NOT NULL,
  sender_sig_key BLOB NOT NULL CHECK(length(sender_sig_key)=32),
  message_id BLOB NOT NULL CHECK(length(message_id)=16),
  body TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS app_human_interaction_receipts_request
  ON app_human_interaction_receipts(community_id,request_id,receipt_id);
CREATE TABLE IF NOT EXISTS app_human_interaction_event_state (
  singleton INTEGER PRIMARY KEY CHECK(singleton=1),
  ack_delivery_id INTEGER NOT NULL DEFAULT 0,
  high_water INTEGER NOT NULL DEFAULT 0
);
INSERT OR IGNORE INTO app_human_interaction_event_state(singleton,ack_delivery_id,high_water) VALUES(1,0,0);
CREATE TABLE IF NOT EXISTS app_human_interaction_response_claims (
  group_id BLOB NOT NULL CHECK(length(group_id)=16),
  request_id BLOB NOT NULL CHECK(length(request_id)=16),
  source_seq INTEGER NOT NULL CHECK(source_seq>0),
  PRIMARY KEY(group_id,request_id)
);
";

/// web3.5 (W2a) authority, created by every open of a store at schema 4 or below, so a v3 store
/// and a schema-4 store from before W2a both gain the tables (empty: no group has a known view).
///
/// - `app_authority`: per group, the users the host-served role view allows `pin_messages`
///   (`bit` 3) or `mention_everyone` (`bit` 18) in the group's channel; never another bit.
/// - `app_authority_mentionable`: per group, the server's role ids the view marks "anyone can
///   mention" (W2a review finding 1); an own-role token from a sender without bit 18 counts only
///   for these.
/// - `app_authority_known`: the groups whose view is known; replaced with the rows above by
///   `authority_set`. A group without a row keeps the former rule (any member counts).
pub(super) const APP_AUTHORITY: &str = "
CREATE TABLE IF NOT EXISTS app_authority (group_id BLOB NOT NULL, bit INTEGER NOT NULL, user_id BLOB NOT NULL,
  PRIMARY KEY (group_id, bit, user_id)) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS app_authority_mentionable (group_id BLOB NOT NULL, role_id BLOB NOT NULL,
  PRIMARY KEY (group_id, role_id)) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS app_authority_known (group_id BLOB PRIMARY KEY, set_at INTEGER NOT NULL) WITHOUT ROWID;
";

const APP_FTS5: &str = "
CREATE VIRTUAL TABLE IF NOT EXISTS app_search USING fts5(body, content='', tokenize='unicode61 remove_diacritics 2');
CREATE TRIGGER IF NOT EXISTS app_search_rows_insert AFTER INSERT ON app_search_rows BEGIN
  INSERT INTO app_search(rowid,body) VALUES(new.rowid,new.body);
END;
CREATE TRIGGER IF NOT EXISTS app_search_rows_delete AFTER DELETE ON app_search_rows BEGIN
  INSERT INTO app_search(app_search,rowid,body) VALUES('delete',old.rowid,old.body);
END;
CREATE TRIGGER IF NOT EXISTS app_search_rows_update AFTER UPDATE OF body ON app_search_rows BEGIN
  INSERT INTO app_search(app_search,rowid,body) VALUES('delete',old.rowid,old.body);
  INSERT INTO app_search(rowid,body) VALUES(new.rowid,new.body);
END;
";

fn open_fts5(c: &rusqlite::Connection) -> Result<(), StorageError> {
    // Retire the earlier shared-terms FTS table before opening the specified body index.
    c.execute_batch(
        "DROP TRIGGER IF EXISTS app_search_rows_insert_v2;
        DROP TRIGGER IF EXISTS app_search_rows_delete_v2;
        DROP TRIGGER IF EXISTS app_search_rows_update_v2;
        DROP TABLE IF EXISTS app_search_v2",
    )?;
    c.execute_batch("SAVEPOINT search_fts5")?;
    #[cfg(any(test, feature = "test-hooks"))]
    if FORCE_MISSING_FTS5.load(Ordering::SeqCst) {
        c.execute_batch("ROLLBACK TO search_fts5; RELEASE search_fts5")?;
        meta_put(c, "search_fts5", &[0])?;
        meta_put(c, "search_contract_v4", &[1])?;
        return Ok(());
    }
    let result = c.execute_batch(APP_FTS5);
    match result {
        Ok(()) => {
            c.execute_batch(
                "INSERT INTO app_search(app_search) VALUES('delete-all');
                INSERT INTO app_search(rowid,body) SELECT rowid,body FROM app_search_rows;
                RELEASE search_fts5",
            )?;
            meta_put(c, "search_fts5", &[1])?;
            meta_put(c, "search_contract_v4", &[1])?;
        }
        Err(e) if e.to_string().contains("no such module: fts5") => {
            c.execute_batch("ROLLBACK TO search_fts5; RELEASE search_fts5")?;
            meta_put(c, "search_fts5", &[0])?;
            meta_put(c, "search_contract_v4", &[1])?;
        }
        Err(e) => {
            c.execute_batch("ROLLBACK TO search_fts5; RELEASE search_fts5")?;
            return Err(e.into());
        }
    }
    Ok(())
}

fn backfill_search_rows(c: &rusqlite::Connection) -> Result<(), StorageError> {
    let mut stmt = c.prepare(
        "SELECT group_id,seq FROM app_messages WHERE status=0 AND type=0 ORDER BY group_id,seq",
    )?;
    let rows: Vec<([u8; 16], i64)> = stmt
        .query_map([], |r| Ok((r.get(0)?, r.get(1)?)))?
        .collect::<Result<_, _>>()?;
    drop(stmt);
    for (group, seq) in rows {
        super::search::index_fold(c, &group, seq as u64)?;
    }
    Ok(())
}

/// L-SQL-20, run on a v1 store only, after `APP_SCHEMA`; the backfill follows in the same unit.
const MIGRATE_V1_TO_V2: &str = "
ALTER TABLE app_groups   ADD COLUMN epoch          INTEGER NOT NULL DEFAULT 0;
ALTER TABLE app_groups   ADD COLUMN pending_commit INTEGER NOT NULL DEFAULT 0;
ALTER TABLE app_messages ADD COLUMN mention        INTEGER NOT NULL DEFAULT 0;
CREATE TABLE IF NOT EXISTS app_read_state (
  group_id      BLOB    PRIMARY KEY CHECK (length(group_id) = 16),
  last_read_seq INTEGER NOT NULL DEFAULT 0,
  last_read_at  INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS app_settings (k TEXT PRIMARY KEY, v TEXT NOT NULL) WITHOUT ROWID;
UPDATE app_meta SET v = x'02' WHERE k = 'schema';
";

/// Refuses a store whose `app_meta` schema is above `APP_SCHEMA_SUPPORTED`, reading only: it runs
/// before `APP_SCHEMA`, whose `CREATE ... IF NOT EXISTS` would otherwise recreate in a newer
/// client's store whatever its migrations dropped. A store with no `app_meta` table or no schema
/// row passes (a fresh store), and so does a malformed row, which `migrate_conn` refuses as before.
pub(crate) fn refuse_newer(c: &rusqlite::Connection) -> Result<(), StorageError> {
    let has_meta: bool = c.query_row(
        "SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'app_meta')",
        [],
        |r| r.get(0),
    )?;
    if !has_meta {
        return Ok(());
    }
    let Some(bytes) = meta_get(c, SCHEMA)? else {
        return Ok(());
    };
    match decode_strict(&bytes, |d| d.uint()) {
        Ok(found) if found > u64::from(APP_SCHEMA_SUPPORTED) => Err(StorageError::StoreNewer {
            found: u32::try_from(found).unwrap_or(u32::MAX),
            supported: APP_SCHEMA_SUPPORTED,
        }),
        _ => Ok(()),
    }
}

/// Refuses a newer store untouched (`refuse_newer`), then runs `APP_SCHEMA`, then v1-to-v2,
/// v2-to-v3, the v4 repairs and v4-to-v5 as needed. Returns the version it found before migrating, at most
/// `APP_SCHEMA_SUPPORTED`; the caller's unit runs the appropriate backfills.
pub(crate) fn migrate_conn(c: &rusqlite::Connection) -> Result<u64, StorageError> {
    refuse_newer(c)?;
    c.execute_batch(APP_SCHEMA)?;
    let bytes = meta_get(c, SCHEMA)?
        .ok_or_else(|| StorageError::Codec("app_meta schema is malformed".into()))?;
    let found = decode_strict(&bytes, |d| d.uint())
        .map_err(|_| StorageError::Codec("app_meta schema is malformed".into()))?;
    if found == 1 {
        c.execute_batch(MIGRATE_V1_TO_V2)?;
    }
    if found == 1 || found == 2 {
        c.execute_batch(MIGRATE_V2_TO_V3)?;
    }
    // Every supported store runs this block on each open: its steps are idempotent repairs (the
    // v4 tables, indexes and pair migrations), so a schema-6 store keeps them as a schema-4 store
    // had them.
    if found <= 6 {
        c.execute_batch(APP_INDEXES_V3)?;
        c.execute_batch(APP_SCHEMA_V4)?;
        c.execute_batch(APP_AUTHORITY)?;
        if !has_column(c, "app_messages", "sender_sig_key")? {
            c.execute_batch("ALTER TABLE app_messages ADD COLUMN sender_sig_key BLOB")?;
        }
        c.execute_batch("CREATE INDEX IF NOT EXISTS app_messages_legacy_sender ON app_messages(sender_user,group_id,sender_device) WHERE sender_sig_key IS NULL AND status=0")?;
        // Retraction seeks a leaf's still-displayed rows by its authenticated key, so repeating
        // a verdict for an already retracted leaf costs nothing per stored message.
        c.execute_batch("CREATE INDEX IF NOT EXISTS app_messages_live_by_sig ON app_messages(group_id,sender_sig_key,seq) WHERE status=0")?;
        migrate_quarantine_pairs(c)?;
        migrate_trust_artifact_pairs(c)?;
        if found < 4 {
            backfill_search_rows(c)?;
        }
        if found < 4 || meta_get(c, "search_contract_v4")?.is_none() {
            open_fts5(c)?;
        }
        if found <= 4 && !has_column(c, "app_messages", "thread_id")? {
            c.execute_batch(MIGRATE_V4_TO_V5)?;
        }
        c.execute_batch(APP_INDEXES_V5)?;
        meta_put(c, SCHEMA, &[6])?;
    }
    c.execute_batch(APP_BOT_EVENTS)?;
    c.execute_batch(APP_HUMAN_INTERACTION_EVENTS)?;
    if found <= 6 {
        meta_put(c, SCHEMA, &[6])?;
    }
    c.execute_batch(BOT_INTENTS_SCHEMA)?;
    if found <= 7 {
        meta_put(c, SCHEMA, &[7])?;
    }
    c.execute_batch(BOT_KEYPACKAGES_SCHEMA)?;
    if found <= 8 {
        meta_put(c, SCHEMA, &[8])?;
    }
    c.execute_batch(REMOVALS_SCHEMA)?;
    if !has_column(c, "app_device_lists", "credential")? {
        c.execute_batch("ALTER TABLE app_device_lists ADD COLUMN credential BLOB CHECK(credential IS NULL OR length(credential)>0)")?;
    }
    if found <= 11 {
        meta_put(c, SCHEMA, &[11])?;
    }
    if found < 12 {
        if !has_column(c, "app_groups", "interaction_installation_id")? {
            c.execute_batch("ALTER TABLE app_groups ADD COLUMN interaction_installation_id BLOB CHECK (interaction_installation_id IS NULL OR length(interaction_installation_id)=16)")?;
        }
        if !has_column(c, "app_groups", "interaction_device_id")? {
            c.execute_batch("ALTER TABLE app_groups ADD COLUMN interaction_device_id BLOB CHECK (interaction_device_id IS NULL OR length(interaction_device_id)=16)")?;
        }
        // Kind-3 interaction groups are scoped by the authenticated participant device and
        // installation. Legacy rows keep NULL metadata and are checked against their MLS binding
        // before a new group is accepted.
        c.execute_batch(
            "DROP INDEX IF EXISTS app_groups_by_target;
             CREATE UNIQUE INDEX app_groups_by_target ON app_groups(target_id,kind) WHERE state<>4 AND kind<>3;
             CREATE UNIQUE INDEX IF NOT EXISTS app_groups_by_interaction_device ON app_groups(community_id,target_id,interaction_installation_id,interaction_device_id) WHERE kind=3 AND state<>4 AND interaction_installation_id IS NOT NULL AND interaction_device_id IS NOT NULL;
             CREATE TRIGGER IF NOT EXISTS app_groups_interaction_scope_insert BEFORE INSERT ON app_groups
               WHEN (NEW.interaction_installation_id IS NULL AND NEW.interaction_device_id IS NOT NULL) OR (NEW.interaction_installation_id IS NOT NULL AND NEW.interaction_device_id IS NULL)
               BEGIN SELECT RAISE(ABORT,'interaction scope pair required'); END;
             CREATE TRIGGER IF NOT EXISTS app_groups_interaction_scope_update BEFORE UPDATE OF interaction_installation_id,interaction_device_id ON app_groups
               WHEN (NEW.interaction_installation_id IS NULL AND NEW.interaction_device_id IS NOT NULL) OR (NEW.interaction_installation_id IS NOT NULL AND NEW.interaction_device_id IS NULL)
               BEGIN SELECT RAISE(ABORT,'interaction scope pair required'); END;",
        )?;
        meta_put(c, SCHEMA, &[12])?;
    }
    // Schema 13 (Task S1 part 2): the instance-signed account-kind attestations. An older client
    // refuses a schema-13 store (`refuse_newer`), so a downgrade cannot shed the "attestation seen"
    // flag and fall back to trusting a credential's self-signed kind.
    c.execute_batch(super::account_kind::ACCOUNT_KINDS_SCHEMA)?;
    // An unreleased schema-13 build (S1 fix rounds 2-3) staged attestations in a table that fix
    // round 4 removed; a development store that ran it drops the orphan (Task 11).
    c.execute_batch("DROP TABLE IF EXISTS app_account_kind_staged;")?;
    if found < 13 {
        meta_put(c, SCHEMA, &[13])?;
    }
    Ok(found)
}

// Task 2's earlier v4 preview stored one quarantine verdict per bare device id.
// Preserve an ambiguous old block under an unknown owner so opening the store
// cannot silently enable sending or assign another user's message as its owner.
fn migrate_quarantine_pairs(c: &rusqlite::Connection) -> Result<(), StorageError> {
    let has_user = has_column(c, "app_quarantine", "user_id")?;
    if has_user && has_column(c, "app_quarantine", "signature_key")? {
        return Ok(());
    }
    let old_user = if has_user {
        "q.user_id"
    } else {
        "zeroblob(16)"
    };
    c.execute_batch(
        "ALTER TABLE app_quarantine RENAME TO app_quarantine_old;
         DROP INDEX IF EXISTS app_quarantine_device;",
    )?;
    c.execute_batch(&format!(
        "
         CREATE TABLE app_quarantine (
           group_id BLOB NOT NULL, user_id BLOB NOT NULL CHECK(length(user_id)=16),
           device_id BLOB NOT NULL CHECK(length(device_id)=16),
           signature_key BLOB NOT NULL DEFAULT x'' CHECK(length(signature_key) IN (0,32)),
           code TEXT NOT NULL, PRIMARY KEY(group_id,user_id,device_id,signature_key)
         );
         INSERT INTO app_quarantine(group_id,user_id,device_id,signature_key,code)
         SELECT q.group_id,{old_user},q.device_id,x'',q.code
           FROM app_quarantine_old q;
         DROP TABLE app_quarantine_old;
         CREATE INDEX app_quarantine_device ON app_quarantine(user_id,device_id,group_id);"
    ))?;
    Ok(())
}

#[cfg(test)]
mod pair_migration_tests {
    use super::*;

    #[test]
    fn pair_quarantine_upgrade_preserves_a_changed_key_block() {
        let c = rusqlite::Connection::open_in_memory().unwrap();
        c.execute_batch("CREATE TABLE app_quarantine(group_id BLOB,user_id BLOB,device_id BLOB,code TEXT,PRIMARY KEY(group_id,user_id,device_id)); CREATE INDEX app_quarantine_device ON app_quarantine(user_id,device_id,group_id);").unwrap();
        c.execute("INSERT INTO app_quarantine(group_id,user_id,device_id,code) VALUES(?1,?2,?3,'E_UMK_CHANGED')", rusqlite::params![[1u8;16].as_slice(),[2u8;16].as_slice(),[3u8;16].as_slice()]).unwrap();
        migrate_quarantine_pairs(&c).unwrap();
        let (code,key_len): (String,i64) = c.query_row("SELECT code,length(signature_key) FROM app_quarantine WHERE group_id=?1 AND user_id=?2 AND device_id=?3",rusqlite::params![[1u8;16].as_slice(),[2u8;16].as_slice(),[3u8;16].as_slice()],|r|Ok((r.get(0)?,r.get(1)?))).unwrap();
        assert_eq!((code.as_str(), key_len), ("E_UMK_CHANGED", 0));
    }

    #[test]
    fn per_device_pins_collapse_per_user_without_granting_trust() {
        let c = rusqlite::Connection::open_in_memory().unwrap();
        c.execute_batch(
            "CREATE TABLE app_trust_pins(user_id BLOB,device_id BLOB,umk_pub BLOB,first_seen INTEGER,verified INTEGER,changed_umk BLOB,PRIMARY KEY(user_id,device_id));",
        )
        .unwrap();
        let pin = |user: u8, device: u8, umk: u8, first: i64, verified: i64| {
            c.execute(
                "INSERT INTO app_trust_pins VALUES(?1,?2,?3,?4,?5,NULL)",
                rusqlite::params![
                    [user; 16].as_slice(),
                    [device; 16].as_slice(),
                    [umk; 32].as_slice(),
                    first,
                    verified
                ],
            )
            .unwrap();
        };
        pin(1, 1, 0x11, 5, 1);
        pin(1, 2, 0x11, 7, 1);
        pin(2, 1, 0x21, 3, 1);
        pin(2, 2, 0x22, 9, 0);
        pin(3, 1, 0x31, 4, 1);
        pin(3, 2, 0x31, 4, 0);
        migrate_trust_pins_per_user(&c).unwrap();
        type Collapsed = (Vec<u8>, Vec<u8>, i64, i64, Option<Vec<u8>>);
        let rows: Vec<Collapsed> = c
            .prepare("SELECT user_id,umk_pub,first_seen,verified,changed_umk FROM app_trust_pins ORDER BY user_id")
            .unwrap()
            .query_map([], |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?, r.get(3)?, r.get(4)?)))
            .unwrap()
            .collect::<Result<_, _>>()
            .unwrap();
        assert_eq!(
            rows,
            vec![
                (vec![1; 16], vec![0x11; 32], 5, 1, None),
                (vec![2; 16], vec![0x21; 32], 3, 0, Some(vec![0x22; 32])),
                (vec![3; 16], vec![0x31; 32], 4, 0, None),
            ],
            "agreeing rows keep their pin; another key stays a pending change; mixed flags are unverified"
        );
        assert!(!has_column(&c, "app_trust_pins", "device_id").unwrap());
    }

    #[test]
    fn ambiguous_legacy_quarantine_does_not_inherit_a_colliding_senders_user() {
        let c = rusqlite::Connection::open_in_memory().unwrap();
        c.execute_batch(
            "CREATE TABLE app_quarantine(group_id BLOB,device_id BLOB,code TEXT,PRIMARY KEY(group_id,device_id));
             CREATE TABLE app_messages(group_id BLOB,sender_user BLOB,sender_device BLOB);",
        )
        .unwrap();
        let group = [0x31u8; 16];
        let honest = [0x32u8; 16];
        let device = [0x33u8; 16];
        c.execute(
            "INSERT INTO app_messages VALUES(?1,?2,?3)",
            rusqlite::params![group.as_slice(), honest.as_slice(), device.as_slice()],
        )
        .unwrap();
        c.execute(
            "INSERT INTO app_quarantine VALUES(?1,?2,'E_UMK_CHANGED')",
            rusqlite::params![group.as_slice(), device.as_slice()],
        )
        .unwrap();
        migrate_quarantine_pairs(&c).unwrap();
        let migrated: [u8; 16] = c
            .query_row("SELECT user_id FROM app_quarantine", [], |r| r.get(0))
            .unwrap();
        assert_eq!(
            migrated, [0; 16],
            "the message sender cannot identify the blocked leaf"
        );
    }
}

fn has_column(c: &rusqlite::Connection, table: &str, name: &str) -> Result<bool, StorageError> {
    let mut stmt = c.prepare(&format!("PRAGMA table_info({table})"))?;
    Ok(stmt
        .query_map([], |r| r.get::<_, String>(1))?
        .collect::<Result<Vec<_>, _>>()?
        .iter()
        .any(|column| column == name))
}

// The UMK pin is a property of the user (protocol/03 rule 2: "the pinned UMK for user_id"), so
// app_trust_pins holds one row per user, the L-SQL-32 shape. Task 2's interim v4 stored one row
// per (user, device); per-device flags could disagree, and a new device of a verified contact
// hid the contact (T2-LAST-02) or was trusted under a pending key. Collapse such a store per user
// without granting anything: rows that disagree keep the earliest pinned key, unverified, with
// any other key they name as the pending change.
fn migrate_trust_pins_per_user(c: &rusqlite::Connection) -> Result<(), StorageError> {
    if !has_column(c, "app_trust_pins", "device_id")? {
        return Ok(());
    }
    type PairPin = ([u8; 16], [u8; 32], i64, i64, Option<[u8; 32]>);
    let rows: Vec<PairPin> = {
        let mut stmt = c.prepare(
            "SELECT user_id,umk_pub,first_seen,verified,changed_umk FROM app_trust_pins \
             ORDER BY user_id,first_seen,umk_pub",
        )?;
        stmt.query_map([], |r| {
            Ok((r.get(0)?, r.get(1)?, r.get(2)?, r.get(3)?, r.get(4)?))
        })?
        .collect::<Result<_, _>>()?
    };
    c.execute_batch(
        "CREATE TABLE app_trust_pins_user (
          user_id BLOB PRIMARY KEY CHECK(length(user_id)=16),
          umk_pub BLOB NOT NULL CHECK(length(umk_pub)=32),
          first_seen INTEGER NOT NULL,
          verified INTEGER NOT NULL CHECK(verified IN(0,1)),
          changed_umk BLOB CHECK(changed_umk IS NULL OR length(changed_umk)=32));",
    )?;
    for user_rows in rows.chunk_by(|a, b| a.0 == b.0) {
        let (user, umk, first_seen, _, changed) = user_rows[0];
        let consistent = user_rows.iter().all(|r| r.1 == umk && r.4 == changed);
        let (verified, changed) = if consistent {
            (i64::from(user_rows.iter().all(|r| r.3 == 1)), changed)
        } else {
            let other = user_rows
                .iter()
                .find_map(|r| r.4.filter(|k| *k != umk))
                .or_else(|| user_rows.iter().map(|r| r.1).find(|k| *k != umk));
            (0, other)
        };
        c.execute(
            "INSERT INTO app_trust_pins_user(user_id,umk_pub,first_seen,verified,changed_umk) VALUES(?1,?2,?3,?4,?5)",
            rusqlite::params![user.as_slice(), umk.as_slice(), first_seen, verified, changed.as_ref().map(|k| k.as_slice())],
        )?;
    }
    c.execute_batch(
        "DROP TABLE app_trust_pins; ALTER TABLE app_trust_pins_user RENAME TO app_trust_pins;",
    )?;
    Ok(())
}

fn migrate_trust_artifact_pairs(c: &rusqlite::Connection) -> Result<(), StorageError> {
    migrate_trust_pins_per_user(c)?;
    if !has_column(c, "app_pending_history", "user_id")? {
        c.execute_batch("ALTER TABLE app_pending_history ADD COLUMN user_id BLOB NOT NULL DEFAULT x'00000000000000000000000000000000';
            ALTER TABLE app_pending_history ADD COLUMN device_id BLOB NOT NULL DEFAULT x'00000000000000000000000000000000';")?;
        let rows: Vec<([u8; 16], i64, Vec<u8>)> = {
            let mut stmt =
                c.prepare("SELECT group_id,seq,sender_credential FROM app_pending_history")?;
            stmt.query_map([], |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?)))?
                .collect::<Result<_, _>>()?
        };
        for (group, seq, credential) in rows {
            if let Ok(identity) = crate::identity::CredentialIdentity::decode(&credential) {
                c.execute("UPDATE app_pending_history SET user_id=?3,device_id=?4 WHERE group_id=?1 AND seq=?2",
                    rusqlite::params![group.as_slice(),seq,identity.user_id.as_bytes(),identity.device_id.as_bytes()])?;
            }
        }
        c.execute_batch("DROP INDEX IF EXISTS app_pending_history_by_leaf_seq;
            CREATE INDEX app_pending_history_by_leaf_seq ON app_pending_history(group_id,user_id,device_id,sender_leaf,seq);")?;
    }
    if !has_column(c, "app_search_rows", "user_id")? {
        c.execute_batch("ALTER TABLE app_search_rows ADD COLUMN user_id BLOB NOT NULL DEFAULT x'00000000000000000000000000000000';
            ALTER TABLE app_search_rows ADD COLUMN device_id BLOB NOT NULL DEFAULT x'00000000000000000000000000000000';
            UPDATE app_search_rows SET
              user_id=COALESCE((SELECT sender_user FROM app_messages m WHERE m.group_id=app_search_rows.group_id AND m.seq=app_search_rows.seq),zeroblob(16)),
              device_id=COALESCE((SELECT sender_device FROM app_messages m WHERE m.group_id=app_search_rows.group_id AND m.seq=app_search_rows.seq),zeroblob(16));")?;
    }
    Ok(())
}

/// Runs the schema migrations and their backfills in one unit. Returns the value it found: 1 to 3
/// (migrated now; a fresh store reads 3), 4 (migrated to 5 now) or 5 (current). A value above `APP_SCHEMA_SUPPORTED` is
/// `StorageError::StoreNewer`, and the store is left untouched.
///
/// The backfill runs here as in `ClientCore::open` (pre-flight ruling (a)): a v1 store must never
/// end at schema 3 with a v1 identity record, which `open` would then refuse for ever. It loads
/// the stored groups through `storage` itself. `open` does not call this (units do not nest).
pub fn migrate_app(storage: &DillaStorage) -> Result<u64, StorageError> {
    storage
        .unit(|u| {
            let found = u.with_conn(migrate_conn)?;
            if found == 1 {
                super::backfill(storage, u)?;
            }
            if found == 1 || found == 2 {
                super::backfill_v3(u)?;
            }
            if found <= 5 {
                u.with_conn(super::trust::seed_own_pin)?;
            }
            Ok(found)
        })
        .map_err(|e| match e {
            TxError::RolledBack(e) => e,
            other => StorageError::Sqlite(other.to_string()),
        })
}

pub(crate) fn meta_get(c: &rusqlite::Connection, k: &str) -> Result<Option<Vec<u8>>, StorageError> {
    c.query_row("SELECT v FROM app_meta WHERE k = ?1", [k], |r| r.get(0))
        .optional()
        .map_err(Into::into)
}
pub(crate) fn meta_put(c: &rusqlite::Connection, k: &str, v: &[u8]) -> Result<(), StorageError> {
    c.execute(
        "INSERT INTO app_meta (k, v) VALUES (?1, ?2) ON CONFLICT (k) DO UPDATE SET v = excluded.v",
        rusqlite::params![k, v],
    )?;
    Ok(())
}
pub(crate) fn meta_del(c: &rusqlite::Connection, k: &str) -> Result<(), StorageError> {
    c.execute("DELETE FROM app_meta WHERE k = ?1", [k])?;
    Ok(())
}

#[cfg(test)]
mod search_upgrade_tests {
    use super::migrate_conn;

    #[test]
    fn old_shared_terms_index_is_removed_when_body_index_opens() {
        let db = rusqlite::Connection::open_in_memory().expect("db");
        migrate_conn(&db).expect("new schema");
        db.execute_batch("CREATE VIRTUAL TABLE app_search_v2 USING fts5(terms, content='', tokenize='unicode61 remove_diacritics 0');
            INSERT INTO app_search_v2(rowid,terms) VALUES(1,' old visible text');
            DELETE FROM app_meta WHERE k='search_contract_v4';
            UPDATE app_meta SET v=x'06' WHERE k='schema'").expect("simulate old v4 index in a v6 store");
        migrate_conn(&db).expect("upgrade index");
        let old: i64 = db
            .query_row(
                "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='app_search_v2'",
                [],
                |r| r.get(0),
            )
            .expect("old table");
        assert_eq!(old, 0);
    }
}

/// C4x (web3.6): a store written by a newer client (a rollback) is refused before anything
/// touches it; schema 6 and below migrate as before (web3.7 TB1 moved the prior bound from 4 to 5).
#[cfg(test)]
mod newer_store_tests {
    use super::{APP_SCHEMA_SUPPORTED, SCHEMA, has_column, meta_get, migrate_conn};
    use crate::mls::StorageError;

    type Snapshot = (
        Vec<(String, String, Option<String>)>,
        Vec<(String, Vec<u8>)>,
    );

    /// Every schema object and every `app_meta` row, in a stable order: the store as a reader
    /// sees it.
    fn snapshot(db: &rusqlite::Connection) -> Snapshot {
        let objects = db
            .prepare("SELECT type, name, sql FROM sqlite_master ORDER BY type, name")
            .expect("schema query")
            .query_map([], |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?)))
            .expect("schema rows")
            .collect::<Result<Vec<_>, _>>()
            .expect("schema");
        let meta = db
            .prepare("SELECT k, v FROM app_meta ORDER BY k")
            .expect("meta query")
            .query_map([], |r| Ok((r.get(0)?, r.get(1)?)))
            .expect("meta rows")
            .collect::<Result<Vec<_>, _>>()
            .expect("meta");
        (objects, meta)
    }

    fn schema_of(db: &rusqlite::Connection) -> Vec<u8> {
        db.query_row("SELECT v FROM app_meta WHERE k = 'schema'", [], |r| {
            r.get(0)
        })
        .expect("schema")
    }

    fn has_thread_index(db: &rusqlite::Connection) -> bool {
        db.query_row(
            "SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type = 'index' AND name = 'app_messages_by_thread')",
            [],
            |r| r.get(0),
        )
        .expect("index query")
    }

    /// A store as this build leaves it, then moved on as a newer client might: schema 14 and one
    /// app table gone (renamed or dropped by its migration), which `APP_SCHEMA` would recreate.
    fn v14_store() -> rusqlite::Connection {
        let db = rusqlite::Connection::open_in_memory().expect("db");
        assert_eq!(migrate_conn(&db).expect("fresh"), 3);
        db.execute_batch("UPDATE app_meta SET v = x'0e' WHERE k = 'schema'; DROP TABLE app_roles;")
            .expect("simulate a v14 store");
        db
    }

    /// A store as a web3.6 build left it: schema 4, no `thread_id` column and no thread index.
    fn v4_store() -> rusqlite::Connection {
        let db = rusqlite::Connection::open_in_memory().expect("db");
        migrate_conn(&db).expect("fresh");
        db.execute_batch(
            "DROP INDEX IF EXISTS app_messages_by_thread;
             UPDATE app_meta SET v = x'04' WHERE k = 'schema';",
        )
        .expect("simulate a v4 store");
        if has_column(&db, "app_messages", "thread_id").expect("columns") {
            db.execute_batch("ALTER TABLE app_messages DROP COLUMN thread_id;")
                .expect("drop the v5 column");
        }
        db
    }

    #[test]
    fn the_supported_schema_is_13() {
        assert_eq!(APP_SCHEMA_SUPPORTED, 13);
    }

    #[test]
    fn kind3_groups_allow_one_authenticated_group_per_device_for_a_shared_user() {
        let db = rusqlite::Connection::open_in_memory().expect("db");
        migrate_conn(&db).expect("fresh schema");
        db.execute(
            "INSERT INTO app_groups(group_id,kind,community_id,target_id,state,interaction_installation_id,interaction_device_id) VALUES(?1,3,?2,?3,0,?4,?5)",
            rusqlite::params![[1u8; 16].as_slice(), [2u8; 16].as_slice(), [3u8; 16].as_slice(), [7u8; 16].as_slice(), [8u8; 16].as_slice()],
        ).expect("first per-device interaction group");
        db.execute(
            "INSERT INTO app_groups(group_id,kind,community_id,target_id,state,interaction_installation_id,interaction_device_id) VALUES(?1,3,?2,?3,0,?4,?5)",
            rusqlite::params![[4u8; 16].as_slice(), [2u8; 16].as_slice(), [3u8; 16].as_slice(), [7u8; 16].as_slice(), [9u8; 16].as_slice()],
        ).expect("second per-device interaction group for the same user");
        assert!(db.execute(
            "INSERT INTO app_groups(group_id,kind,community_id,target_id,state,interaction_installation_id,interaction_device_id) VALUES(?1,3,?2,?3,0,?4,?5)",
            rusqlite::params![[10u8; 16].as_slice(), [2u8; 16].as_slice(), [3u8; 16].as_slice(), [7u8; 16].as_slice(), [8u8; 16].as_slice()],
        ).is_err(), "the same device cannot hold two live groups for one installation");
        db.execute(
            "INSERT INTO app_groups(group_id,kind,community_id,target_id,state,interaction_installation_id,interaction_device_id) VALUES(?1,3,?2,?3,0,?4,?5)",
            rusqlite::params![[11u8; 16].as_slice(), [2u8; 16].as_slice(), [3u8; 16].as_slice(), [12u8; 16].as_slice(), [8u8; 16].as_slice()],
        ).expect("another installation has a separate per-device group");
        db.execute(
            "INSERT INTO app_groups(group_id,kind,community_id,target_id,state,interaction_installation_id,interaction_device_id) VALUES(?1,3,?2,?3,0,?4,?5)",
            rusqlite::params![[13u8; 16].as_slice(), [14u8; 16].as_slice(), [3u8; 16].as_slice(), [7u8; 16].as_slice(), [8u8; 16].as_slice()],
        ).expect("same device can hold a separate community's interaction group");
        assert!(db.execute(
            "INSERT INTO app_groups(group_id,kind,community_id,target_id,state,interaction_device_id) VALUES(?1,3,?2,?3,0,?4)",
            rusqlite::params![[15u8; 16].as_slice(), [2u8; 16].as_slice(), [3u8; 16].as_slice(), [16u8; 16].as_slice()],
        ).is_err(), "interaction scope metadata must be complete");
        assert!(db.execute(
            "INSERT INTO app_groups(group_id,kind,community_id,target_id,state) VALUES(?1,0,?2,?3,0)",
            rusqlite::params![[5u8; 16].as_slice(), [2u8; 16].as_slice(), [3u8; 16].as_slice()],
        ).is_ok());
        assert!(db.execute(
            "INSERT INTO app_groups(group_id,kind,community_id,target_id,state) VALUES(?1,0,?2,?3,0)",
            rusqlite::params![[6u8; 16].as_slice(), [2u8; 16].as_slice(), [3u8; 16].as_slice()],
        ).is_err(), "text channel uniqueness remains enforced");
    }

    #[test]
    fn schema11_migration_replaces_the_shared_user_unique_index() {
        let db = rusqlite::Connection::open_in_memory().expect("db");
        db.execute_batch(
            "CREATE TABLE app_meta (k TEXT PRIMARY KEY, v BLOB NOT NULL) WITHOUT ROWID;
             INSERT INTO app_meta(k,v) VALUES('schema',x'0b');
             CREATE TABLE app_groups (
               group_id BLOB PRIMARY KEY CHECK(length(group_id)=16),
               kind INTEGER NOT NULL,
               community_id BLOB CHECK(community_id IS NULL OR length(community_id)=16),
               target_id BLOB NOT NULL CHECK(length(target_id)=16),
               state INTEGER NOT NULL,
               next_seq INTEGER NOT NULL DEFAULT 1,
               acked_seq INTEGER NOT NULL DEFAULT 0,
               acked_epoch INTEGER NOT NULL DEFAULT 0,
               resync INTEGER NOT NULL DEFAULT 0 CHECK(resync IN(0,1)),
               was_gone INTEGER NOT NULL DEFAULT 0 CHECK(was_gone IN(0,1)),
               max_epoch INTEGER NOT NULL DEFAULT 0,
               epoch INTEGER NOT NULL DEFAULT 0,
               pending_commit INTEGER NOT NULL DEFAULT 0
             ) WITHOUT ROWID;
             CREATE TABLE app_device_lists (
               user_id BLOB PRIMARY KEY CHECK(length(user_id)=16),
               version INTEGER NOT NULL, list BLOB NOT NULL,
               umk_pub BLOB NOT NULL CHECK(length(umk_pub)=32),
               ssk_pub BLOB NOT NULL CHECK(length(ssk_pub)=32)
             );
             CREATE UNIQUE INDEX app_groups_by_target ON app_groups(target_id,kind) WHERE state<>4;",
        ).expect("create schema 11 shape");
        db.execute(
            "INSERT INTO app_groups(group_id,kind,community_id,target_id,state) VALUES(?1,3,?2,?3,2)",
            rusqlite::params![[20u8; 16].as_slice(), [21u8; 16].as_slice(), [22u8; 16].as_slice()],
        ).expect("legacy interaction group");
        assert_eq!(migrate_conn(&db).expect("upgrade schema 11"), 11);
        let legacy_scope: (Option<Vec<u8>>, Option<Vec<u8>>) = db.query_row(
            "SELECT interaction_installation_id,interaction_device_id FROM app_groups WHERE group_id=?1",
            [[20u8; 16].as_slice()],
            |row| Ok((row.get(0)?, row.get(1)?)),
        ).expect("legacy row remains readable");
        assert_eq!(legacy_scope, (None, None));
        db.execute(
            "INSERT INTO app_groups(group_id,kind,community_id,target_id,state,interaction_installation_id,interaction_device_id) VALUES(?1,3,?2,?3,0,?4,?5)",
            rusqlite::params![[1u8; 16].as_slice(), [21u8; 16].as_slice(), [22u8; 16].as_slice(), [7u8; 16].as_slice(), [8u8; 16].as_slice()],
        ).expect("first interaction group");
        db.execute(
            "INSERT INTO app_groups(group_id,kind,community_id,target_id,state,interaction_installation_id,interaction_device_id) VALUES(?1,3,?2,?3,0,?4,?5)",
            rusqlite::params![[4u8; 16].as_slice(), [21u8; 16].as_slice(), [22u8; 16].as_slice(), [7u8; 16].as_slice(), [9u8; 16].as_slice()],
        ).expect("second device interaction group after upgrade");
        assert!(db.execute(
            "INSERT INTO app_groups(group_id,kind,community_id,target_id,state,interaction_installation_id,interaction_device_id) VALUES(?1,3,?2,?3,0,?4,?5)",
            rusqlite::params![[5u8; 16].as_slice(), [21u8; 16].as_slice(), [22u8; 16].as_slice(), [7u8; 16].as_slice(), [8u8; 16].as_slice()],
        ).is_err(), "same device cannot acquire a second group after migration");
        assert!(db.execute(
            "INSERT INTO app_groups(group_id,kind,community_id,target_id,state,interaction_device_id) VALUES(?1,3,?2,?3,0,?4)",
            rusqlite::params![[17u8; 16].as_slice(), [21u8; 16].as_slice(), [22u8; 16].as_slice(), [18u8; 16].as_slice()],
        ).is_err(), "upgraded schema rejects a partial interaction scope");
        assert_eq!(meta_get(&db, SCHEMA).expect("schema read"), Some(vec![13]));
    }

    #[test]
    fn a_v4_store_gains_the_thread_column_and_index_and_reads_5() {
        let db = v4_store();
        assert_eq!(migrate_conn(&db).expect("migrates"), 4);
        assert!(has_column(&db, "app_messages", "thread_id").expect("columns"));
        assert!(has_thread_index(&db), "app_messages_by_thread exists");
        assert_eq!(schema_of(&db), vec![0x0d]);
        // The column's check: a thread id is exactly 16 bytes or NULL.
        let insert = |thread: &[u8]| {
            db.execute(
                "INSERT INTO app_messages (group_id,seq,epoch,recv_ts,status,sender_device,franking_tag,thread_id) \
                 VALUES (x'01',?1,0,0,0,x'02',x'03',?2)",
                rusqlite::params![thread.len() as i64, thread],
            )
        };
        assert!(insert(&[7; 15]).is_err(), "a 15-byte thread id is refused");
        assert!(insert(&[7; 16]).is_ok(), "a 16-byte thread id is stored");
    }

    #[test]
    fn a_fresh_store_ends_at_13_with_the_thread_column_and_bot_journals() {
        let db = rusqlite::Connection::open_in_memory().expect("db");
        assert_eq!(migrate_conn(&db).expect("fresh"), 3);
        assert_eq!(schema_of(&db), vec![0x0d]);
        assert!(has_column(&db, "app_messages", "thread_id").expect("columns"));
        assert!(has_thread_index(&db));
        db.query_row(
            "SELECT ack_delivery_id FROM app_bot_event_state WHERE singleton=1",
            [],
            |r| r.get::<_, i64>(0),
        )
        .expect("journal state");
        db.query_row("SELECT count(*) FROM app_bot_intents", [], |r| {
            r.get::<_, i64>(0)
        })
        .expect("intent table");
    }

    /// Task S1 fix round 1: a schema-12 store gains the account-kind tables and reads 13, and the
    /// "attestation seen" flag is absent: the earlier rule holds until a verified attestation.
    #[test]
    fn a_v12_store_gains_the_account_kind_tables_without_the_seen_flag() {
        let db = rusqlite::Connection::open_in_memory().expect("db");
        migrate_conn(&db).expect("fresh");
        db.execute_batch(
            "DROP TABLE app_account_kinds; UPDATE app_meta SET v = x'0c' WHERE k = 'schema';",
        )
        .expect("simulate a v12 store");
        assert_eq!(migrate_conn(&db).expect("migrates"), 12);
        assert_eq!(schema_of(&db), vec![0x0d]);
        let n: i64 = db
            .query_row(
                "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='app_account_kinds'",
                [],
                |r| r.get(0),
            )
            .expect("sqlite_master");
        assert_eq!(n, 1, "app_account_kinds exists");
        assert_eq!(
            meta_get(&db, super::super::account_kind::ACCOUNT_KIND_SEEN).expect("meta"),
            None
        );
    }

    /// Task 11: a schema-13 development store holding the removed staging table loses it on open.
    #[test]
    fn a_v13_store_drops_the_orphan_staging_table() {
        let db = rusqlite::Connection::open_in_memory().expect("db");
        migrate_conn(&db).expect("fresh");
        db.execute_batch("CREATE TABLE app_account_kind_staged (user_id BLOB PRIMARY KEY);")
            .expect("simulate the orphan");
        assert_eq!(migrate_conn(&db).expect("current"), 13);
        let n: i64 = db
            .query_row(
                "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='app_account_kind_staged'",
                [],
                |r| r.get(0),
            )
            .expect("sqlite_master");
        assert_eq!(n, 0, "the staging table is gone");
    }

    #[test]
    fn a_v13_store_opens_untouched() {
        let db = rusqlite::Connection::open_in_memory().expect("db");
        migrate_conn(&db).expect("fresh");
        let before = snapshot(&db);
        assert_eq!(migrate_conn(&db).expect("current"), 13);
        assert_eq!(snapshot(&db), before, "no table, index or meta row changed");
    }

    #[test]
    fn a_v14_store_is_refused_with_both_numbers_and_left_unchanged() {
        let db = v14_store();
        let before = snapshot(&db);
        let changes = db.total_changes();
        match migrate_conn(&db) {
            Err(StorageError::StoreNewer { found, supported }) => {
                assert_eq!((found, supported), (14, 13));
            }
            other => panic!("a v14 store must be refused as StoreNewer, got {other:?}"),
        }
        assert_eq!(snapshot(&db), before, "no table, index or meta row changed");
        assert_eq!(db.total_changes(), changes, "no row written");
    }

    #[test]
    fn the_refusal_names_both_numbers() {
        let e = StorageError::StoreNewer {
            found: 13,
            supported: 12,
        };
        assert_eq!(
            e.to_string(),
            "app schema 13 is newer than this build, which supports up to 12"
        );
    }

    #[test]
    fn schemas_up_to_6_still_migrate_and_end_at_13() {
        for found in [3u8, 4, 5, 6] {
            let db = if found == 5 || found == 6 {
                let db = rusqlite::Connection::open_in_memory().expect("db");
                migrate_conn(&db).expect("fresh");
                if found == 6 {
                    db.execute_batch(
                        "DROP TABLE app_bot_intents;
                         INSERT INTO app_meta(k,v) VALUES('legacy_marker',x'42');",
                    )
                    .expect("restore a schema-6 shape");
                }
                let version = [found];
                db.execute(
                    "UPDATE app_meta SET v=?1 WHERE k='schema'",
                    [version.as_slice()],
                )
                .expect("set prior schema version");
                db
            } else {
                let db = v4_store();
                db.execute(
                    "UPDATE app_meta SET v = ?1 WHERE k = 'schema'",
                    [[found].as_slice()],
                )
                .expect("set schema");
                db
            };
            assert_eq!(migrate_conn(&db).expect("migrates"), u64::from(found));
            assert_eq!(schema_of(&db), vec![0x0d]);
            assert!(has_column(&db, "app_messages", "thread_id").expect("columns"));
            if found == 6 {
                db.query_row("SELECT v FROM app_meta WHERE k='legacy_marker'", [], |r| {
                    r.get::<_, Vec<u8>>(0)
                })
                .expect("schema-6 application data remains");
                db.query_row("SELECT count(*) FROM app_bot_intents", [], |r| {
                    r.get::<_, i64>(0)
                })
                .expect("schema-7 intent table is created");
                db.query_row(
                    "SELECT count(*) FROM sqlite_master WHERE type='index' AND name='app_bot_intents_one_pending_per_group'",
                    [],
                    |r| r.get::<_, i64>(0),
                )
                .expect("partial uniqueness index is created");
            }
        }
    }

    #[test]
    fn a_store_whose_meta_has_no_schema_row_opens_as_fresh() {
        let db = rusqlite::Connection::open_in_memory().expect("db");
        db.execute_batch(
            "CREATE TABLE app_meta (k TEXT PRIMARY KEY, v BLOB NOT NULL) WITHOUT ROWID;",
        )
        .expect("meta table");
        assert_eq!(migrate_conn(&db).expect("fresh"), 3);
        assert_eq!(migrate_conn(&db).expect("current"), 13);
    }
}
