-- +goose Up
-- Bot v2 (Task 9): the per-community command catalog, the Bot's command-key publication, Bot
-- channels, incoming webhooks and the state revisions the Bot's hints bump.
-- bot_catalogs.body is the deterministic-CBOR commands list the Bot published; dillad filters it
-- per member when it serves or pushes it. revision only grows.
CREATE TABLE bot_catalogs (
  community_id BLOB    NOT NULL PRIMARY KEY CHECK (length(community_id) = 16)
                       REFERENCES communities(id) ON DELETE CASCADE,
  revision     INTEGER NOT NULL CHECK (revision > 0),
  body         BLOB    NOT NULL
) STRICT;
-- The signed publication as the Bot sent it (dilla_core::bot::command, [1, community_id, key_id,
-- public_key, not_after, bot_device_id, sig]); clients verify it themselves.
CREATE TABLE bot_command_keys (
  community_id BLOB    NOT NULL PRIMARY KEY CHECK (length(community_id) = 16)
                       REFERENCES communities(id) ON DELETE CASCADE,
  key_id       BLOB    NOT NULL CHECK (length(key_id) = 16),
  publication  BLOB    NOT NULL,
  published_at INTEGER NOT NULL CHECK (published_at >= 0),
  -- The key this one replaced (all zero for none), which dillad still relays sealed commands to until
  -- prev_until (published_at + 24 h, the helper's overlap); then only key_id is current (Task 21b).
  prev_key_id  BLOB    NOT NULL CHECK (length(prev_key_id) = 16),
  prev_until   INTEGER NOT NULL CHECK (prev_until >= 0)
) STRICT;
-- A text channel an admin marked as a Bot channel: the only channels the Bot is an MLS member of
-- and posts in.
CREATE TABLE bot_channels (
  channel_id   BLOB    NOT NULL PRIMARY KEY CHECK (length(channel_id) = 16)
                       REFERENCES channels(id) ON DELETE CASCADE,
  community_id BLOB    NOT NULL CHECK (length(community_id) = 16)
                       REFERENCES communities(id) ON DELETE CASCADE,
  marked_at    INTEGER NOT NULL CHECK (marked_at >= 0),
  marked_by    BLOB    NOT NULL CHECK (length(marked_by) = 16)
                       REFERENCES users(id) ON DELETE RESTRICT
) STRICT;
CREATE INDEX bot_channels_by_community ON bot_channels(community_id, channel_id);
-- Incoming webhooks (their routes come later). The secret is stored only as SHA-256(secret).
CREATE TABLE bot_webhooks (
  webhook_id     BLOB    NOT NULL PRIMARY KEY CHECK (length(webhook_id) = 16),
  community_id   BLOB    NOT NULL CHECK (length(community_id) = 16)
                         REFERENCES communities(id) ON DELETE CASCADE,
  channel_id     BLOB    NOT NULL CHECK (length(channel_id) = 16)
                         REFERENCES channels(id) ON DELETE CASCADE,
  name           TEXT    NOT NULL,
  avatar_blob    BLOB,
  secret_hash    BLOB    NOT NULL CHECK (length(secret_hash) = 32),
  allow_everyone INTEGER NOT NULL CHECK (allow_everyone IN (0, 1)),
  created_at     INTEGER NOT NULL CHECK (created_at >= 0),
  rotated_at     INTEGER CHECK (rotated_at IS NULL OR rotated_at >= created_at)
) STRICT;
CREATE INDEX bot_webhooks_by_community ON bot_webhooks(community_id, webhook_id);
-- The last revision of each Bot state scope: scope_kind 0 user (scope_id a user id), 1 channel
-- (a Bot channel), 2 call (the voice channel the call is in), each within one community. A user
-- scope's revision is the user's in that community alone (Bot v2 Task 10), so one community's Bot
-- learns nothing of another's hints. Nothing else is stored.
CREATE TABLE bot_state_revisions (
  scope_kind   INTEGER NOT NULL CHECK (scope_kind IN (0, 1, 2)),
  scope_id     BLOB    NOT NULL CHECK (length(scope_id) = 16),
  community_id BLOB    NOT NULL CHECK (length(community_id) = 16)
                       REFERENCES communities(id) ON DELETE CASCADE,
  revision     INTEGER NOT NULL CHECK (revision > 0),
  PRIMARY KEY (scope_kind, scope_id, community_id)
) STRICT;
CREATE INDEX bot_state_revisions_by_community ON bot_state_revisions(community_id);
-- +goose Down
DROP INDEX bot_state_revisions_by_community;
DROP TABLE bot_state_revisions;
DROP TABLE bot_webhooks;
DROP TABLE bot_channels;
DROP TABLE bot_command_keys;
DROP TABLE bot_catalogs;
