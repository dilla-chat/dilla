-- Bot v2 (Task 9): 051_bot_v2.sql.

-- name: PutBotCatalog :exec
INSERT INTO bot_catalogs(community_id, revision, body) VALUES ($1, $2, $3)
ON CONFLICT(community_id) DO UPDATE SET revision = excluded.revision, body = excluded.body;

-- name: GetBotCatalog :one
SELECT community_id, revision, body FROM bot_catalogs WHERE community_id = $1;

-- name: PutBotCommandKey :exec
INSERT INTO bot_command_keys(community_id, key_id, publication, published_at) VALUES ($1, $2, $3, $4)
ON CONFLICT(community_id) DO UPDATE SET key_id = excluded.key_id, publication = excluded.publication, published_at = excluded.published_at;

-- name: GetBotCommandKey :one
SELECT community_id, key_id, publication, published_at FROM bot_command_keys WHERE community_id = $1;

-- name: MarkBotChannel :execrows
INSERT INTO bot_channels(channel_id, community_id, marked_at, marked_by) VALUES ($1, $2, $3, $4)
ON CONFLICT(channel_id) DO NOTHING;

-- name: UnmarkBotChannel :execrows
DELETE FROM bot_channels WHERE community_id = $1 AND channel_id = $2;

-- name: ListBotChannels :many
SELECT b.channel_id, b.community_id, b.marked_at, b.marked_by
FROM bot_channels b JOIN channels c ON c.id = b.channel_id
WHERE b.community_id = $1 AND c.deleted_at IS NULL AND c.kind = 0 AND c.mode = 0
ORDER BY b.marked_at, b.channel_id;

-- name: BumpBotStateRevision :one
INSERT INTO bot_state_revisions(scope_kind, scope_id, community_id, revision) VALUES ($1, $2, $3, 1)
ON CONFLICT(scope_kind, scope_id, community_id) DO UPDATE SET revision = bot_state_revisions.revision + 1
RETURNING revision;

-- name: ListBotUserStateRevisions :many
SELECT community_id, revision FROM bot_state_revisions WHERE scope_kind = 0 AND scope_id = $1
ORDER BY community_id;

-- name: DeleteBotStateRevisionsOfCommunity :exec
DELETE FROM bot_state_revisions WHERE community_id = $1;

-- name: DeleteBotChannelsOfCommunity :many
DELETE FROM bot_channels WHERE community_id = $1 RETURNING channel_id;

-- name: DeleteBotCatalog :exec
DELETE FROM bot_catalogs WHERE community_id = $1;

-- name: DeleteBotCommandKey :exec
DELETE FROM bot_command_keys WHERE community_id = $1;

-- name: DeleteBotWebhooksOfCommunity :exec
DELETE FROM bot_webhooks WHERE community_id = $1;
