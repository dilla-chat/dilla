-- name: CreateBotRuntime :exec
INSERT INTO bot_runtimes(id, token_hash, created, rotated, revoked_at) VALUES ($1, $2, $3, $4, $5);

-- name: GetBotRuntime :one
SELECT * FROM bot_runtimes WHERE id = $1;

-- name: GetBotRuntimeByHash :one
SELECT * FROM bot_runtimes WHERE token_hash = $1;

-- name: LockBotRuntime :one
SELECT id FROM bot_runtimes WHERE id = $1 FOR UPDATE;

-- name: RotateBotRuntime :execrows
UPDATE bot_runtimes SET token_hash = $1, rotated = $2 WHERE id = $3 AND token_hash = $4 AND revoked_at IS NULL;

-- name: RevokeBotRuntime :execrows
UPDATE bot_runtimes SET revoked_at = $1 WHERE id = $2 AND revoked_at IS NULL;

-- name: ListBotRuntimes :many
SELECT * FROM bot_runtimes WHERE id > $1 ORDER BY id LIMIT sqlc.arg(max_rows)::bigint;

-- name: CreateBotInstallation :exec
INSERT INTO bot_installations(id, community_id, runtime_id, user_id, device_id, role_id, state, revision, capabilities, approved_by, approved_at, credential_verified_at, created, updated, removed_at, feature_capabilities)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16);

-- name: GetBotInstallation :one
SELECT * FROM bot_installations WHERE id = $1;

-- name: GetActiveBotInstallationByCommunity :one
SELECT * FROM bot_installations WHERE community_id = $1 AND removed_at IS NULL;

-- name: ListBotInstallationsAfter :many
SELECT * FROM bot_installations WHERE id > $1 AND removed_at IS NULL ORDER BY id LIMIT sqlc.arg(max_rows)::bigint;

-- name: ListBotInstallationsByDevice :many
SELECT * FROM bot_installations WHERE device_id = $1 AND id > $2 ORDER BY id LIMIT sqlc.arg(max_rows)::bigint;

-- name: UpdateBotInstallation :execrows
UPDATE bot_installations SET runtime_id = $1, user_id = $2, device_id = $3, role_id = $4, state = $5, revision = $6, capabilities = $7, approved_by = $8, approved_at = $9, credential_verified_at = $10, updated = $11, removed_at = $12, feature_capabilities = $13
WHERE id = $14 AND revision = $15 AND removed_at IS NULL;

-- name: LockBotInstallation :one
SELECT id FROM bot_installations WHERE id = $1 FOR UPDATE;

-- name: LockBotCommunity :one
SELECT id FROM communities WHERE id = $1 FOR UPDATE;

-- name: GetBotClaim :one
SELECT * FROM bot_claims WHERE installation_id = $1;

-- name: PutBotClaim :exec
INSERT INTO bot_claims(installation_id, runtime_id, claim_id, request_id, expires) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (installation_id) DO UPDATE SET runtime_id = EXCLUDED.runtime_id, claim_id = EXCLUDED.claim_id, request_id = EXCLUDED.request_id, expires = EXCLUDED.expires;

-- name: DeleteBotClaim :execrows
DELETE FROM bot_claims WHERE installation_id = $1;

-- name: DeleteBotClaimsByRuntime :execrows
DELETE FROM bot_claims WHERE runtime_id = $1;

-- name: CreateBotOperation :exec
INSERT INTO bot_operations(community_id, request_id, installation_id, actor_kind, actor_id, actor_device, operation, payload_hash, result_cbor, created)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- name: GetBotOperation :one
SELECT * FROM bot_operations WHERE community_id = $1 AND request_id = $2;

-- name: CreateBotSessionBinding :exec
INSERT INTO bot_session_bindings(token_hash, installation_id, user_id, device_id, created) VALUES ($1, $2, $3, $4, $5);

-- name: GetBotSessionBinding :one
SELECT * FROM bot_session_bindings WHERE token_hash = $1;

-- name: DeleteBotSessionBindingsByInstallation :execrows
DELETE FROM bot_session_bindings WHERE installation_id = $1;

-- name: UpdateDeviceCredential :execrows
UPDATE devices SET credential_blob = $1 WHERE id = $2;

-- name: FinalizeBotDeviceCredential :execrows
UPDATE devices SET credential_blob = $1, verified_at = $2 WHERE id = $3;

-- name: LockDeviceDSK :exec
-- The 64-bit transaction advisory lock is derived from the DSK bytes. Every repository device
-- insertion takes the same lock, so lookup plus insert is atomic without a global unique index.
SELECT pg_advisory_xact_lock(sqlc.arg(lock_key)::bigint);

-- name: DeviceDSKConflictCount :one
SELECT COUNT(*) FROM devices d JOIN users u ON u.id = d.user_id
WHERE d.dsk_pub = sqlc.arg(dsk_pub) AND (sqlc.arg(new_is_bot)::boolean OR u.kind = 1);

-- name: ListBotInstallationsForRuntime :many
SELECT * FROM bot_installations
WHERE id > $1 AND removed_at IS NULL
  AND (runtime_id = $2 OR (state = 0 AND runtime_id IS NULL))
ORDER BY id LIMIT sqlc.arg(max_rows)::bigint;

-- name: SetBotMusicSettings :execrows
UPDATE bot_installations SET music_volume = $1, music_idle_minutes = $2
WHERE id = $3 AND removed_at IS NULL;

-- name: RequestBotRestart :one
UPDATE bot_installations SET restart_requested = CASE WHEN restart_requested > restart_handled THEN restart_requested ELSE restart_requested + 1 END
WHERE id = $1 AND removed_at IS NULL
RETURNING restart_requested;

-- name: SetBotRuntimeStatus :execrows
UPDATE bot_installations SET runtime_version = sqlc.narg(runtime_version)::text,
  restart_handled = CASE WHEN sqlc.arg(handled)::bigint > restart_handled THEN LEAST(sqlc.arg(handled)::bigint, restart_requested) ELSE restart_handled END
WHERE id = sqlc.arg(id) AND runtime_id = sqlc.arg(runtime_id) AND removed_at IS NULL;
