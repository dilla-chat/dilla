-- name: CreateInvite :exec
INSERT INTO invites (id, code_hash, community_id, created_by, grants_admin, max_uses, used_count, created, expires_at, revoked_at, unlimited_uses)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetInviteByHash :one
SELECT * FROM invites WHERE code_hash = ?;

-- name: RedeemInvite :one
UPDATE invites SET used_count = used_count + 1
WHERE code_hash = ? AND (unlimited_uses = 1 OR used_count < max_uses) AND revoked_at IS NULL AND expires_at > ?
RETURNING *;

-- name: RevokeInvite :exec
UPDATE invites SET revoked_at = ? WHERE id = ?;

-- name: ListInvites :many
SELECT * FROM invites ORDER BY created DESC;

-- name: ListInvitesByCommunity :many
SELECT * FROM invites WHERE community_id = ? ORDER BY created DESC;

-- name: PutInviteSignup :exec
INSERT INTO invite_signups (user_id, invite_id, created) VALUES (?, ?, ?);

-- name: TakeInviteSignup :execrows
DELETE FROM invite_signups WHERE user_id = ? AND invite_id = ?;
