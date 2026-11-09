-- +goose Up
-- Bot v2 (Task 23): the v1 interaction machinery is gone. Drop the kind-3 admission and eligibility tables and their
-- triggers, the kind-3 group registration receipts, the AutoModeration step-up grants, and every kind-3 (interaction)
-- group with its members, handshakes, proposals, messages, Welcomes, trees, cursors, queued joins and Bot send and
-- registration rows. The child rows are deleted explicitly, ahead of the groups, so the purge does not depend on
-- foreign-key enforcement.
-- Schema files 037 and 040 keep their numbers (they run 001..049 with no gap) as these notes, which sqlc reads and
-- goose must carry verbatim:
-- schema 037_bot_interaction_eligibility_revisions: the table is dropped by migration 00058_drop_bot_v1 (Bot v2 Task 23).
-- schema 040_automod_stepup_grants: the AutoModeration step-up grants are dropped by migration 00058_drop_bot_v1 (Bot v2 Task 23).
DROP INDEX bot_interaction_admission_receipts_expiry;
DROP INDEX bot_interaction_admission_receipts_live;

DROP TABLE automod_stepup_grants;

DROP TABLE bot_group_registration_admission_receipts;

DROP TRIGGER bot_interaction_eligibility_account_update ON users;
DROP FUNCTION bot_interaction_eligibility_account_changed();

DROP TABLE bot_interaction_admission_receipts;

DROP TRIGGER bot_interaction_eligibility_device_list_delete ON device_lists;
DROP TRIGGER bot_interaction_eligibility_device_list_update ON device_lists;
DROP TRIGGER bot_interaction_eligibility_device_list_insert ON device_lists;
DROP TRIGGER bot_interaction_eligibility_device_delete ON devices;
DROP TRIGGER bot_interaction_eligibility_device_update ON devices;
DROP TRIGGER bot_interaction_eligibility_device_insert ON devices;
DROP TRIGGER bot_interaction_eligibility_ban_delete ON bans;
DROP TRIGGER bot_interaction_eligibility_ban_update ON bans;
DROP TRIGGER bot_interaction_eligibility_ban_insert ON bans;
DROP TRIGGER bot_interaction_eligibility_member_delete ON members;
DROP TRIGGER bot_interaction_eligibility_member_insert ON members;
DROP FUNCTION bot_interaction_eligibility_user_changed();
DROP FUNCTION bot_interaction_eligibility_ban_changed();
DROP FUNCTION bot_interaction_eligibility_member_changed();
DROP FUNCTION bot_bump_interaction_eligibility(BYTEA,BYTEA);
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
  community_id BYTEA NOT NULL CHECK(octet_length(community_id)=16),
  user_id BYTEA NOT NULL CHECK(octet_length(user_id)=16),
  revision BIGINT NOT NULL CHECK(revision > 0),
  PRIMARY KEY(community_id,user_id)
);


-- +goose StatementBegin
CREATE FUNCTION bot_bump_interaction_eligibility(p_community BYTEA,p_user BYTEA) RETURNS VOID AS $$
BEGIN
  INSERT INTO bot_interaction_eligibility_revisions(community_id,user_id,revision)
  VALUES(p_community,p_user,1)
  ON CONFLICT(community_id,user_id) DO UPDATE
  SET revision=bot_interaction_eligibility_revisions.revision+1;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION bot_interaction_eligibility_member_changed() RETURNS TRIGGER AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    PERFORM bot_bump_interaction_eligibility(OLD.community_id,OLD.user_id);
    RETURN OLD;
  END IF;
  PERFORM bot_bump_interaction_eligibility(NEW.community_id,NEW.user_id);
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
CREATE TRIGGER bot_interaction_eligibility_member_insert AFTER INSERT ON members
FOR EACH ROW EXECUTE FUNCTION bot_interaction_eligibility_member_changed();
CREATE TRIGGER bot_interaction_eligibility_member_delete AFTER DELETE ON members
FOR EACH ROW EXECUTE FUNCTION bot_interaction_eligibility_member_changed();

-- +goose StatementBegin
CREATE FUNCTION bot_interaction_eligibility_ban_changed() RETURNS TRIGGER AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    PERFORM bot_bump_interaction_eligibility(OLD.community_id,OLD.user_id);
    RETURN OLD;
  END IF;
  IF TG_OP = 'UPDATE' AND (OLD.community_id IS DISTINCT FROM NEW.community_id OR OLD.user_id IS DISTINCT FROM NEW.user_id) THEN
    PERFORM bot_bump_interaction_eligibility(OLD.community_id,OLD.user_id);
  END IF;
  PERFORM bot_bump_interaction_eligibility(NEW.community_id,NEW.user_id);
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
CREATE TRIGGER bot_interaction_eligibility_ban_insert AFTER INSERT ON bans
FOR EACH ROW EXECUTE FUNCTION bot_interaction_eligibility_ban_changed();
CREATE TRIGGER bot_interaction_eligibility_ban_update AFTER UPDATE ON bans
FOR EACH ROW EXECUTE FUNCTION bot_interaction_eligibility_ban_changed();
CREATE TRIGGER bot_interaction_eligibility_ban_delete AFTER DELETE ON bans
FOR EACH ROW EXECUTE FUNCTION bot_interaction_eligibility_ban_changed();

-- +goose StatementBegin
CREATE FUNCTION bot_interaction_eligibility_user_changed() RETURNS TRIGGER AS $$
DECLARE
  old_user BYTEA;
  new_user BYTEA;
  community BYTEA;
BEGIN
  IF TG_OP <> 'INSERT' THEN old_user := OLD.user_id; END IF;
  IF TG_OP <> 'DELETE' THEN new_user := NEW.user_id; END IF;
  IF old_user IS NOT NULL THEN
    FOR community IN SELECT community_id FROM members WHERE user_id=old_user LOOP
      PERFORM bot_bump_interaction_eligibility(community,old_user);
    END LOOP;
  END IF;
  IF new_user IS NOT NULL AND new_user IS DISTINCT FROM old_user THEN
    FOR community IN SELECT community_id FROM members WHERE user_id=new_user LOOP
      PERFORM bot_bump_interaction_eligibility(community,new_user);
    END LOOP;
  END IF;
  IF TG_OP = 'DELETE' THEN RETURN OLD; END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
CREATE TRIGGER bot_interaction_eligibility_device_insert AFTER INSERT ON devices
FOR EACH ROW EXECUTE FUNCTION bot_interaction_eligibility_user_changed();
CREATE TRIGGER bot_interaction_eligibility_device_update AFTER UPDATE OF dsk_pub,tier,signer_tier,credential_blob,revoked_at,quarantined_at,verified_at,user_id ON devices
FOR EACH ROW WHEN (OLD.dsk_pub IS DISTINCT FROM NEW.dsk_pub OR OLD.tier IS DISTINCT FROM NEW.tier OR OLD.signer_tier IS DISTINCT FROM NEW.signer_tier OR OLD.credential_blob IS DISTINCT FROM NEW.credential_blob OR OLD.revoked_at IS DISTINCT FROM NEW.revoked_at OR OLD.quarantined_at IS DISTINCT FROM NEW.quarantined_at OR OLD.verified_at IS DISTINCT FROM NEW.verified_at OR OLD.user_id IS DISTINCT FROM NEW.user_id)
EXECUTE FUNCTION bot_interaction_eligibility_user_changed();
CREATE TRIGGER bot_interaction_eligibility_device_delete AFTER DELETE ON devices
FOR EACH ROW EXECUTE FUNCTION bot_interaction_eligibility_user_changed();
CREATE TRIGGER bot_interaction_eligibility_device_list_insert AFTER INSERT ON device_lists
FOR EACH ROW EXECUTE FUNCTION bot_interaction_eligibility_user_changed();
CREATE TRIGGER bot_interaction_eligibility_device_list_update AFTER UPDATE ON device_lists
FOR EACH ROW EXECUTE FUNCTION bot_interaction_eligibility_user_changed();
CREATE TRIGGER bot_interaction_eligibility_device_list_delete AFTER DELETE ON device_lists
FOR EACH ROW EXECUTE FUNCTION bot_interaction_eligibility_user_changed();

CREATE TABLE bot_interaction_admission_receipts (
  installation_id BYTEA NOT NULL REFERENCES bot_installations(id) ON DELETE CASCADE CHECK(octet_length(installation_id)=16),
  request_id BYTEA NOT NULL CHECK(octet_length(request_id)=16),
  request_hash BYTEA NOT NULL CHECK(octet_length(request_hash)=32),
  payload BYTEA NOT NULL,
  signature BYTEA NOT NULL CHECK(octet_length(signature) IN (0,64)),
  not_after BIGINT NOT NULL CHECK(not_after > 0),
  PRIMARY KEY(installation_id,request_id)
);

-- Account disable and tombstone change whether a human can hold an admission.
-- +goose StatementBegin
CREATE FUNCTION bot_interaction_eligibility_account_changed() RETURNS TRIGGER AS $$
DECLARE community BYTEA;
BEGIN
  FOR community IN SELECT community_id FROM members WHERE user_id=NEW.id LOOP
    PERFORM bot_bump_interaction_eligibility(community,NEW.id);
  END LOOP;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
CREATE TRIGGER bot_interaction_eligibility_account_update
AFTER UPDATE OF disabled_at,deleted_at ON users
FOR EACH ROW WHEN (OLD.disabled_at IS DISTINCT FROM NEW.disabled_at OR OLD.deleted_at IS DISTINCT FROM NEW.deleted_at)
EXECUTE FUNCTION bot_interaction_eligibility_account_changed();

-- Keep the consumed admission binding after admission payload compaction so
-- retries and interrupted-registration recovery cannot spend it twice.
CREATE TABLE bot_group_registration_admission_receipts (
  installation_id BYTEA NOT NULL REFERENCES bot_installations(id) ON DELETE CASCADE CHECK(octet_length(installation_id)=16),
  admission_request_id BYTEA NOT NULL CHECK(octet_length(admission_request_id)=16),
  operation_id BYTEA NOT NULL CHECK(octet_length(operation_id)=16),
  user_id BYTEA NOT NULL CHECK(octet_length(user_id)=16),
  device_id BYTEA NOT NULL CHECK(octet_length(device_id)=16),
  community_id BYTEA NOT NULL CHECK(octet_length(community_id)=16),
  group_id BYTEA NOT NULL CHECK(octet_length(group_id)=16),
  body_sha256 BYTEA NOT NULL CHECK(octet_length(body_sha256)=32),
  generation BIGINT NOT NULL CHECK(generation >= 0),
  epoch BIGINT NOT NULL CHECK(epoch >= 0),
  next_seq BIGINT NOT NULL CHECK(next_seq > 0),
  accepted_at BIGINT NOT NULL,
  PRIMARY KEY(installation_id, admission_request_id),
  UNIQUE(group_id),
  UNIQUE(installation_id, operation_id)
);

CREATE TABLE automod_stepup_grants (
  token_hash BYTEA PRIMARY KEY CHECK (octet_length(token_hash) = 32),
  community_id BYTEA NOT NULL REFERENCES communities(id) ON DELETE CASCADE CHECK (octet_length(community_id) = 16),
  installation_id BYTEA NOT NULL REFERENCES bot_installations(id) ON DELETE CASCADE CHECK (octet_length(installation_id) = 16),
  group_id BYTEA NOT NULL CHECK (octet_length(group_id) = 16),
  request_id BYTEA NOT NULL CHECK (octet_length(request_id) = 16),
  actor_user BYTEA NOT NULL CHECK (octet_length(actor_user) = 16),
  actor_device BYTEA NOT NULL CHECK (octet_length(actor_device) = 16),
  expected_revision BIGINT NOT NULL CHECK (expected_revision >= 0),
  request_expires BIGINT NOT NULL CHECK (request_expires >= 0),
  config_hash BYTEA NOT NULL CHECK (octet_length(config_hash) = 32),
  expires BIGINT NOT NULL,
  consumed_at BIGINT,
  accepted_request_hash BYTEA CHECK (accepted_request_hash IS NULL OR octet_length(accepted_request_hash) = 32),
  receipt BYTEA CHECK (receipt IS NULL OR octet_length(receipt) = 16),
  CHECK ((consumed_at IS NULL AND accepted_request_hash IS NULL AND receipt IS NULL) OR
         (consumed_at IS NOT NULL AND accepted_request_hash IS NOT NULL AND receipt IS NOT NULL))
);
CREATE INDEX automod_stepup_grants_expiry ON automod_stepup_grants(expires);

-- Every admission compacts its installation's receipts inside the writer transaction. Searched by
-- installation alone, both compaction statements visited every receipt kept in the 30-day window
-- (CT 144, 2026-10-30: about 6 s per admission on SQLite). The partial index serves the payload
-- compaction (only receipts that still hold a payload), the full one the tombstone deletion.
-- Neither changes what is kept or for how long.
CREATE INDEX bot_interaction_admission_receipts_live ON bot_interaction_admission_receipts(installation_id, not_after) WHERE octet_length(payload) > 0;
CREATE INDEX bot_interaction_admission_receipts_expiry ON bot_interaction_admission_receipts(installation_id, not_after);
