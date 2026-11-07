-- +goose Up
-- Bot v2 (Task 9): the per-community command catalog, the Bot's command-key publication, Bot
-- channels, incoming webhooks and the state revisions the Bot's hints bump.
-- bot_catalogs.body is the deterministic-CBOR commands list the Bot published; dillad filters it
-- per member when it serves or pushes it. revision only grows.
CREATE TABLE bot_catalogs (
  community_id BYTEA  NOT NULL PRIMARY KEY CHECK (octet_length(community_id) = 16)
                      REFERENCES communities(id) ON DELETE CASCADE,
  revision     BIGINT NOT NULL CHECK (revision > 0),
  body         BYTEA  NOT NULL
);
-- The signed publication as the Bot sent it (dilla_core::bot::command, [1, community_id, key_id,
-- public_key, not_after, bot_device_id, sig]); clients verify it themselves.
CREATE TABLE bot_command_keys (
  community_id BYTEA  NOT NULL PRIMARY KEY CHECK (octet_length(community_id) = 16)
                      REFERENCES communities(id) ON DELETE CASCADE,
  key_id       BYTEA  NOT NULL CHECK (octet_length(key_id) = 16),
  publication  BYTEA  NOT NULL,
  published_at BIGINT NOT NULL CHECK (published_at >= 0)
);
-- A text channel an admin marked as a Bot channel: the only channels the Bot is an MLS member of
-- and posts in.
CREATE TABLE bot_channels (
  channel_id   BYTEA  NOT NULL PRIMARY KEY CHECK (octet_length(channel_id) = 16)
                      REFERENCES channels(id) ON DELETE CASCADE,
  community_id BYTEA  NOT NULL CHECK (octet_length(community_id) = 16)
                      REFERENCES communities(id) ON DELETE CASCADE,
  marked_at    BIGINT NOT NULL CHECK (marked_at >= 0),
  marked_by    BYTEA  NOT NULL CHECK (octet_length(marked_by) = 16)
                      REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX bot_channels_by_community ON bot_channels(community_id, channel_id);
-- Incoming webhooks (their routes come later). The secret is stored only as SHA-256(secret).
CREATE TABLE bot_webhooks (
  webhook_id     BYTEA  NOT NULL PRIMARY KEY CHECK (octet_length(webhook_id) = 16),
  community_id   BYTEA  NOT NULL CHECK (octet_length(community_id) = 16)
                        REFERENCES communities(id) ON DELETE CASCADE,
  channel_id     BYTEA  NOT NULL CHECK (octet_length(channel_id) = 16)
                        REFERENCES channels(id) ON DELETE CASCADE,
  name           TEXT   NOT NULL,
  avatar_blob    BYTEA,
  secret_hash    BYTEA  NOT NULL CHECK (octet_length(secret_hash) = 32),
  allow_everyone BIGINT NOT NULL CHECK (allow_everyone IN (0, 1)),
  created_at     BIGINT NOT NULL CHECK (created_at >= 0),
  rotated_at     BIGINT CHECK (rotated_at IS NULL OR rotated_at >= created_at)
);
CREATE INDEX bot_webhooks_by_community ON bot_webhooks(community_id, webhook_id);
-- The last revision of each Bot state scope: scope_kind 0 user (scope_id a user id), 1 channel
-- (a Bot channel), 2 call (the voice channel the call is in). Nothing else is stored.
CREATE TABLE bot_state_revisions (
  scope_kind BIGINT NOT NULL CHECK (scope_kind IN (0, 1, 2)),
  scope_id   BYTEA  NOT NULL CHECK (octet_length(scope_id) = 16),
  revision   BIGINT NOT NULL CHECK (revision > 0),
  PRIMARY KEY (scope_kind, scope_id)
);
-- +goose Down
DROP TABLE bot_state_revisions;
DROP TABLE bot_webhooks;
DROP TABLE bot_channels;
DROP TABLE bot_command_keys;
DROP TABLE bot_catalogs;
