-- +goose Up
-- Bot v2 Task 23: bot_interaction_eligibility_revisions is dropped by migration 00058_drop_bot_v1.
-- The number stays so the schema files run 001..049 with no gap.
-- Bot v2 Task 23: the AutoModeration step-up grant tables are dropped by migration 00058_drop_bot_v1.
-- The number stays so the schema files run 001..049 with no gap.
-- Bot v2 (Task 23): the v1 interaction machinery is gone. Drop the kind-3 admission and eligibility tables and their
-- triggers, the kind-3 group registration receipts, the AutoModeration step-up grants, and every kind-3 (interaction)
-- group with its members, handshakes, proposals, messages, Welcomes, trees, cursors, queued joins and Bot send and
-- registration rows. The child rows are deleted explicitly, ahead of the groups, so the purge does not depend on
-- foreign-key enforcement.
DROP INDEX bot_interaction_admission_receipts_expiry;
DROP INDEX bot_interaction_admission_receipts_live;

DROP TABLE automod_stepup_grants;

DROP TABLE bot_group_registration_admission_receipts;

DROP TRIGGER bot_interaction_eligibility_account_update;

DROP TABLE bot_interaction_admission_receipts;

DROP TRIGGER bot_interaction_eligibility_device_list_delete;
DROP TRIGGER bot_interaction_eligibility_device_list_update;
DROP TRIGGER bot_interaction_eligibility_device_list_insert;
DROP TRIGGER bot_interaction_eligibility_device_delete;
DROP TRIGGER bot_interaction_eligibility_device_update;
DROP TRIGGER bot_interaction_eligibility_device_insert;
DROP TRIGGER bot_interaction_eligibility_ban_delete;
DROP TRIGGER bot_interaction_eligibility_ban_update;
DROP TRIGGER bot_interaction_eligibility_ban_insert;
DROP TRIGGER bot_interaction_eligibility_member_delete;
DROP TRIGGER bot_interaction_eligibility_member_insert;
DROP TABLE bot_interaction_eligibility_revisions;

DELETE FROM bot_group_registration_receipts WHERE group_id IN (SELECT group_id FROM mls_groups WHERE kind = 3);
DELETE FROM bot_send_operations WHERE group_id IN (SELECT group_id FROM mls_groups WHERE kind = 3);
DELETE FROM mls_welcomes WHERE group_id IN (SELECT group_id FROM mls_groups WHERE kind = 3);
DELETE FROM mls_welcome_payloads WHERE group_id IN (SELECT group_id FROM mls_groups WHERE kind = 3);
DELETE FROM mls_epoch_trees WHERE group_id IN (SELECT group_id FROM mls_groups WHERE kind = 3);
DELETE FROM device_cursors WHERE group_id IN (SELECT group_id FROM mls_groups WHERE kind = 3);
DELETE FROM fork_reports WHERE group_id IN (SELECT group_id FROM mls_groups WHERE kind = 3);
DELETE FROM pending_joins WHERE group_id IN (SELECT group_id FROM mls_groups WHERE kind = 3);
DELETE FROM moderation_removals WHERE group_id IN (SELECT group_id FROM mls_groups WHERE kind = 3);
DELETE FROM moderation_removal_heads WHERE group_id IN (SELECT group_id FROM mls_groups WHERE kind = 3);
DELETE FROM mls_app_messages WHERE group_id IN (SELECT group_id FROM mls_groups WHERE kind = 3);
DELETE FROM mls_members WHERE group_id IN (SELECT group_id FROM mls_groups WHERE kind = 3);
DELETE FROM mls_pending_proposals WHERE group_id IN (SELECT group_id FROM mls_groups WHERE kind = 3);
DELETE FROM mls_handshakes WHERE group_id IN (SELECT group_id FROM mls_groups WHERE kind = 3);
DELETE FROM mls_groups WHERE kind = 3;

-- +goose Down
-- The rollback recreates the dropped tables and triggers empty (spec rollback: interaction groups self-register, so a
-- 00057 binary rebuilds what it needs). The purged kind-3 groups are not restored.
CREATE TABLE bot_interaction_eligibility_revisions (
  community_id BLOB NOT NULL CHECK(length(community_id)=16),
  user_id BLOB NOT NULL CHECK(length(user_id)=16),
  revision INTEGER NOT NULL CHECK(revision > 0),
  PRIMARY KEY(community_id,user_id)
) STRICT;


-- +goose StatementBegin
CREATE TRIGGER bot_interaction_eligibility_member_insert AFTER INSERT ON members BEGIN
  INSERT INTO bot_interaction_eligibility_revisions(community_id,user_id,revision)
  VALUES(NEW.community_id,NEW.user_id,1)
  ON CONFLICT(community_id,user_id) DO UPDATE SET revision=revision+1;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER bot_interaction_eligibility_member_delete AFTER DELETE ON members BEGIN
  INSERT INTO bot_interaction_eligibility_revisions(community_id,user_id,revision)
  VALUES(OLD.community_id,OLD.user_id,1)
  ON CONFLICT(community_id,user_id) DO UPDATE SET revision=revision+1;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER bot_interaction_eligibility_ban_insert AFTER INSERT ON bans BEGIN
  INSERT INTO bot_interaction_eligibility_revisions(community_id,user_id,revision)
  VALUES(NEW.community_id,NEW.user_id,1)
  ON CONFLICT(community_id,user_id) DO UPDATE SET revision=revision+1;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER bot_interaction_eligibility_ban_update AFTER UPDATE ON bans BEGIN
  INSERT INTO bot_interaction_eligibility_revisions(community_id,user_id,revision)
  SELECT OLD.community_id,OLD.user_id,1
  WHERE OLD.community_id IS NOT NEW.community_id OR OLD.user_id IS NOT NEW.user_id
  ON CONFLICT(community_id,user_id) DO UPDATE SET revision=revision+1;
  INSERT INTO bot_interaction_eligibility_revisions(community_id,user_id,revision)
  VALUES(NEW.community_id,NEW.user_id,1)
  ON CONFLICT(community_id,user_id) DO UPDATE SET revision=revision+1;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER bot_interaction_eligibility_ban_delete AFTER DELETE ON bans BEGIN
  INSERT INTO bot_interaction_eligibility_revisions(community_id,user_id,revision)
  VALUES(OLD.community_id,OLD.user_id,1)
  ON CONFLICT(community_id,user_id) DO UPDATE SET revision=revision+1;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER bot_interaction_eligibility_device_insert AFTER INSERT ON devices BEGIN
  INSERT INTO bot_interaction_eligibility_revisions(community_id,user_id,revision)
  SELECT community_id,NEW.user_id,1 FROM members WHERE user_id=NEW.user_id
  ON CONFLICT(community_id,user_id) DO UPDATE SET revision=revision+1;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER bot_interaction_eligibility_device_update AFTER UPDATE OF user_id,dsk_pub,tier,signer_tier,credential_blob,revoked_at,quarantined_at,verified_at ON devices
WHEN OLD.user_id IS NOT NEW.user_id OR OLD.dsk_pub IS NOT NEW.dsk_pub OR OLD.tier IS NOT NEW.tier OR OLD.signer_tier IS NOT NEW.signer_tier OR OLD.credential_blob IS NOT NEW.credential_blob OR OLD.revoked_at IS NOT NEW.revoked_at OR OLD.quarantined_at IS NOT NEW.quarantined_at OR OLD.verified_at IS NOT NEW.verified_at BEGIN
  INSERT INTO bot_interaction_eligibility_revisions(community_id,user_id,revision)
  SELECT community_id,OLD.user_id,1 FROM members WHERE user_id=OLD.user_id
  ON CONFLICT(community_id,user_id) DO UPDATE SET revision=revision+1;
  INSERT INTO bot_interaction_eligibility_revisions(community_id,user_id,revision)
  SELECT community_id,NEW.user_id,1 FROM members WHERE user_id=NEW.user_id AND NEW.user_id IS NOT OLD.user_id
  ON CONFLICT(community_id,user_id) DO UPDATE SET revision=revision+1;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER bot_interaction_eligibility_device_delete AFTER DELETE ON devices BEGIN
  INSERT INTO bot_interaction_eligibility_revisions(community_id,user_id,revision)
  SELECT community_id,OLD.user_id,1 FROM members WHERE user_id=OLD.user_id
  ON CONFLICT(community_id,user_id) DO UPDATE SET revision=revision+1;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER bot_interaction_eligibility_device_list_insert AFTER INSERT ON device_lists BEGIN
  INSERT INTO bot_interaction_eligibility_revisions(community_id,user_id,revision)
  SELECT community_id,NEW.user_id,1 FROM members WHERE user_id=NEW.user_id
  ON CONFLICT(community_id,user_id) DO UPDATE SET revision=revision+1;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER bot_interaction_eligibility_device_list_update AFTER UPDATE ON device_lists BEGIN
  INSERT INTO bot_interaction_eligibility_revisions(community_id,user_id,revision)
  SELECT community_id,OLD.user_id,1 FROM members WHERE user_id=OLD.user_id
  ON CONFLICT(community_id,user_id) DO UPDATE SET revision=revision+1;
  INSERT INTO bot_interaction_eligibility_revisions(community_id,user_id,revision)
  SELECT community_id,NEW.user_id,1 FROM members WHERE user_id=NEW.user_id AND NEW.user_id IS NOT OLD.user_id
  ON CONFLICT(community_id,user_id) DO UPDATE SET revision=revision+1;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER bot_interaction_eligibility_device_list_delete AFTER DELETE ON device_lists BEGIN
  INSERT INTO bot_interaction_eligibility_revisions(community_id,user_id,revision)
  SELECT community_id,OLD.user_id,1 FROM members WHERE user_id=OLD.user_id
  ON CONFLICT(community_id,user_id) DO UPDATE SET revision=revision+1;
END;
-- +goose StatementEnd

CREATE TABLE bot_interaction_admission_receipts (
  installation_id BLOB NOT NULL REFERENCES bot_installations(id) ON DELETE CASCADE CHECK(length(installation_id)=16),
  request_id BLOB NOT NULL CHECK(length(request_id)=16),
  request_hash BLOB NOT NULL CHECK(length(request_hash)=32),
  payload BLOB NOT NULL,
  signature BLOB NOT NULL CHECK(length(signature) IN (0,64)),
  not_after INTEGER NOT NULL CHECK(not_after > 0),
  PRIMARY KEY(installation_id,request_id)
) STRICT;

-- Account disable and tombstone change whether a human can hold an admission.
-- Bump every community fence only when those eligibility fields actually change.
-- +goose StatementBegin
CREATE TRIGGER bot_interaction_eligibility_account_update
AFTER UPDATE OF disabled_at,deleted_at ON users
WHEN OLD.disabled_at IS NOT NEW.disabled_at OR OLD.deleted_at IS NOT NEW.deleted_at
BEGIN
  INSERT INTO bot_interaction_eligibility_revisions(community_id,user_id,revision)
  SELECT community_id,NEW.id,1 FROM members WHERE user_id=NEW.id
  ON CONFLICT(community_id,user_id) DO UPDATE SET revision=revision+1;
END;
-- +goose StatementEnd

-- Durable binding between a kind-3 admission request and the accepted group
-- registration it consumed. Keep this independently of admission payload
-- compaction so retries/recovery cannot spend the admission twice.
CREATE TABLE bot_group_registration_admission_receipts (
  installation_id BLOB NOT NULL REFERENCES bot_installations(id) ON DELETE CASCADE CHECK(length(installation_id)=16),
  admission_request_id BLOB NOT NULL CHECK(length(admission_request_id)=16),
  operation_id BLOB NOT NULL CHECK(length(operation_id)=16),
  user_id BLOB NOT NULL CHECK(length(user_id)=16),
  device_id BLOB NOT NULL CHECK(length(device_id)=16),
  community_id BLOB NOT NULL CHECK(length(community_id)=16),
  group_id BLOB NOT NULL CHECK(length(group_id)=16),
  body_sha256 BLOB NOT NULL CHECK(length(body_sha256)=32),
  generation INTEGER NOT NULL CHECK(generation >= 0),
  epoch INTEGER NOT NULL CHECK(epoch >= 0),
  next_seq INTEGER NOT NULL CHECK(next_seq > 0),
  accepted_at INTEGER NOT NULL,
  PRIMARY KEY(installation_id, admission_request_id),
  UNIQUE(group_id),
  UNIQUE(installation_id, operation_id)
) STRICT;

CREATE TABLE automod_stepup_grants (
  token_hash BLOB PRIMARY KEY CHECK (length(token_hash) = 32),
  community_id BLOB NOT NULL CHECK (length(community_id) = 16) REFERENCES communities(id) ON DELETE CASCADE,
  installation_id BLOB NOT NULL CHECK (length(installation_id) = 16) REFERENCES bot_installations(id) ON DELETE CASCADE,
  group_id BLOB NOT NULL CHECK (length(group_id) = 16),
  request_id BLOB NOT NULL CHECK (length(request_id) = 16),
  actor_user BLOB NOT NULL CHECK (length(actor_user) = 16),
  actor_device BLOB NOT NULL CHECK (length(actor_device) = 16),
  expected_revision INTEGER NOT NULL CHECK (expected_revision >= 0),
  request_expires INTEGER NOT NULL CHECK (request_expires >= 0),
  config_hash BLOB NOT NULL CHECK (length(config_hash) = 32),
  expires INTEGER NOT NULL,
  consumed_at INTEGER,
  accepted_request_hash BLOB CHECK (accepted_request_hash IS NULL OR length(accepted_request_hash) = 32),
  receipt BLOB CHECK (receipt IS NULL OR length(receipt) = 16),
  CHECK ((consumed_at IS NULL AND accepted_request_hash IS NULL AND receipt IS NULL) OR
         (consumed_at IS NOT NULL AND accepted_request_hash IS NOT NULL AND receipt IS NOT NULL))
) STRICT;
CREATE INDEX automod_stepup_grants_expiry ON automod_stepup_grants(expires);

-- Every admission compacts its installation's receipts inside the writer transaction. Searched by
-- installation alone, both compaction statements visited every receipt kept in the 30-day window
-- (CT 144, 2026-10-30: about 6 s per admission, with every other write queued behind it). The
-- partial index serves the payload compaction (only receipts that still hold a payload), the full
-- one the tombstone deletion. Neither changes what is kept or for how long.
CREATE INDEX bot_interaction_admission_receipts_live ON bot_interaction_admission_receipts(installation_id, not_after) WHERE length(payload) > 0;
CREATE INDEX bot_interaction_admission_receipts_expiry ON bot_interaction_admission_receipts(installation_id, not_after);
