-- name: CreateBotRuntime :exec
INSERT INTO bot_runtimes(id, token_hash, created, rotated, revoked_at) VALUES (?, ?, ?, ?, ?);

-- name: GetBotRuntime :one
SELECT * FROM bot_runtimes WHERE id = ?;

-- name: GetBotRuntimeByHash :one
SELECT * FROM bot_runtimes WHERE token_hash = ?;

-- name: LockBotRuntime :one
UPDATE bot_runtimes SET rotated = rotated WHERE id = ? RETURNING id;

-- name: RotateBotRuntime :execrows
UPDATE bot_runtimes SET token_hash = ?, rotated = ? WHERE id = ? AND token_hash = ? AND revoked_at IS NULL;

-- name: RevokeBotRuntime :execrows
UPDATE bot_runtimes SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL;

-- name: ListBotRuntimes :many
SELECT * FROM bot_runtimes WHERE id > ? ORDER BY id LIMIT sqlc.arg(max_rows);

-- name: CreateBotInstallation :exec
INSERT INTO bot_installations(id, community_id, runtime_id, user_id, device_id, role_id, state, revision, capabilities, approved_by, approved_at, credential_verified_at, created, updated, removed_at, feature_capabilities)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetBotInstallation :one
SELECT * FROM bot_installations WHERE id = ?;

-- name: GetActiveBotInstallationByCommunity :one
SELECT * FROM bot_installations WHERE community_id = ? AND removed_at IS NULL;

-- name: ListBotInstallationsAfter :many
SELECT * FROM bot_installations WHERE id > ? AND removed_at IS NULL ORDER BY id LIMIT sqlc.arg(max_rows);

-- name: ListBotInstallationsByDevice :many
SELECT * FROM bot_installations WHERE device_id = ? AND id > ? ORDER BY id LIMIT sqlc.arg(max_rows);

-- name: UpdateBotInstallation :execrows
UPDATE bot_installations SET runtime_id = ?, user_id = ?, device_id = ?, role_id = ?, state = ?, revision = ?, capabilities = ?, approved_by = ?, approved_at = ?, credential_verified_at = ?, updated = ?, removed_at = ?, feature_capabilities = ?
WHERE id = ? AND revision = ? AND removed_at IS NULL;

-- name: LockBotInstallation :one
UPDATE bot_installations SET updated = updated WHERE id = ? RETURNING id;

-- name: LockBotCommunity :one
UPDATE communities SET name = name WHERE id = ? RETURNING id;

-- name: GetBotClaim :one
SELECT * FROM bot_claims WHERE installation_id = ?;

-- name: PutBotClaim :exec
INSERT INTO bot_claims(installation_id, runtime_id, claim_id, request_id, expires) VALUES (?, ?, ?, ?, ?)
ON CONFLICT(installation_id) DO UPDATE SET runtime_id = excluded.runtime_id, claim_id = excluded.claim_id, request_id = excluded.request_id, expires = excluded.expires;

-- name: DeleteBotClaim :execrows
DELETE FROM bot_claims WHERE installation_id = ?;

-- name: DeleteBotClaimsByRuntime :execrows
DELETE FROM bot_claims WHERE runtime_id = ?;

-- name: CreateBotOperation :exec
INSERT INTO bot_operations(community_id, request_id, installation_id, actor_kind, actor_id, actor_device, operation, payload_hash, result_cbor, created)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetBotOperation :one
SELECT * FROM bot_operations WHERE community_id = ? AND request_id = ?;

-- name: CreateBotSessionBinding :exec
INSERT INTO bot_session_bindings(token_hash, installation_id, user_id, device_id, created) VALUES (?, ?, ?, ?, ?);

-- name: GetBotSessionBinding :one
SELECT * FROM bot_session_bindings WHERE token_hash = ?;

-- name: DeleteBotSessionBindingsByInstallation :execrows
DELETE FROM bot_session_bindings WHERE installation_id = ?;

-- name: UpdateDeviceCredential :execrows
UPDATE devices SET credential_blob = ? WHERE id = ?;

-- name: FinalizeBotDeviceCredential :execrows
UPDATE devices SET credential_blob = ?, verified_at = ? WHERE id = ?;

-- name: LockDeviceDSK :exec
-- The first DML statement in a deferred SQLite transaction reserves its single writer, serializing
-- this key check and insert with every other device creation.
UPDATE devices SET last_seen = last_seen
WHERE CAST(sqlc.arg(lock_key) AS INTEGER) = CAST(sqlc.arg(lock_key) AS INTEGER) AND 0;

-- name: DeviceDSKConflictCount :one
SELECT COUNT(*) FROM devices d JOIN users u ON u.id = d.user_id
WHERE d.dsk_pub = sqlc.arg(dsk_pub) AND (CAST(sqlc.arg(new_is_bot) AS BOOLEAN) OR u.kind = 1);

-- name: ListBotInstallationsForRuntime :many
SELECT * FROM bot_installations
WHERE id > ? AND removed_at IS NULL
  AND (runtime_id = ? OR (state = 0 AND runtime_id IS NULL))
ORDER BY id LIMIT sqlc.arg(max_rows);

-- name: SetBotMusicSettings :execrows
UPDATE bot_installations SET music_volume = ?, music_idle_minutes = ?
WHERE id = ? AND removed_at IS NULL;

-- name: RequestBotRestart :one
UPDATE bot_installations SET restart_requested = CASE WHEN restart_requested > restart_handled THEN restart_requested ELSE restart_requested + 1 END
WHERE id = ? AND removed_at IS NULL
RETURNING restart_requested;

-- name: SetBotRuntimeStatus :execrows
UPDATE bot_installations SET runtime_version = sqlc.narg(runtime_version),
  restart_handled = CASE WHEN sqlc.arg(handled) > restart_handled THEN MIN(sqlc.arg(handled), restart_requested) ELSE restart_handled END
WHERE id = sqlc.arg(id) AND runtime_id = sqlc.arg(runtime_id) AND removed_at IS NULL;
