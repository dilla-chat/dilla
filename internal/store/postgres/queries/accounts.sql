-- name: CreateUser :exec
INSERT INTO users (id, username, display, kind, umk_pub, ssk_pub, sig_umk_ssk, flags, age_bracket, created, disabled_at, deleted_at, factor_owed)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13);

-- name: GetUser :one
SELECT * FROM users WHERE id = $1;

-- name: GetUserKind :one
SELECT kind FROM users WHERE id = $1;

-- name: GetUserByUsername :one
SELECT * FROM users WHERE username = $1;

-- name: ListUsers :many
SELECT * FROM users WHERE id > $1 ORDER BY id LIMIT sqlc.arg(max_rows)::bigint;

-- name: SetUserDisabled :exec
UPDATE users SET disabled_at = $1 WHERE id = $2;

-- name: UpdateProfile :execrows
-- A NULL argument leaves that column as it is, so a write names only the fields it changes.
UPDATE users SET display = COALESCE(sqlc.narg(display)::text, display), status_msg = COALESCE(sqlc.narg(status_msg)::text, status_msg),
                 bio = COALESCE(sqlc.narg(bio)::text, bio)
WHERE id = sqlc.arg(id) AND deleted_at IS NULL
  AND (display <> COALESCE(sqlc.narg(display)::text, display)
    OR status_msg <> COALESCE(sqlc.narg(status_msg)::text, status_msg)
    OR bio <> COALESCE(sqlc.narg(bio)::text, bio));

-- name: TombstoneUser :exec
UPDATE users SET deleted_at = $1, display = '', status_msg = '', bio = '', umk_pub = '\x0000000000000000000000000000000000000000000000000000000000000000'::bytea, ssk_pub = '\x0000000000000000000000000000000000000000000000000000000000000000'::bytea,
                 sig_umk_ssk = '\x00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000'::bytea, flags = 0
WHERE id = $2;

-- name: SetFactorOwed :execrows
UPDATE users SET factor_owed = $1 WHERE id = $2;

-- name: MarkFactorOwedWithoutFactor :execrows
-- The boot reconcile's first half (web3.6 S1): every live human account that holds no confirmed
-- TOTP secret and no passkey registered for the instance's rp_id owes a second factor.
UPDATE users SET factor_owed = 1
 WHERE kind = 0 AND deleted_at IS NULL AND factor_owed = 0
   AND id NOT IN (SELECT t.user_id FROM totp_secrets t WHERE t.confirmed_at IS NOT NULL)
   AND id NOT IN (SELECT w.user_id FROM webauthn_credentials w WHERE w.rp_id = sqlc.arg(rp_id));

-- name: ClearFactorOwedWithFactor :execrows
-- The boot reconcile's second half (web3.6 review I2): an account marked as owing a factor that
-- holds a confirmed TOTP secret or a passkey registered for the instance's rp_id owes nothing, so
-- owed holds exactly when no usable factor is held. It is what an rp_id changed and then put back
-- needs: the first start marks the passkey-only accounts, the start after the revert clears them.
-- The operator reset deletes the factors in the transaction that sets the flag, so it never
-- leaves a row this clears.
UPDATE users SET factor_owed = 0
 WHERE kind = 0 AND deleted_at IS NULL AND factor_owed = 1
   AND (id IN (SELECT t.user_id FROM totp_secrets t WHERE t.confirmed_at IS NOT NULL)
        OR id IN (SELECT w.user_id FROM webauthn_credentials w WHERE w.rp_id = sqlc.arg(rp_id)));

-- name: LockOwedHumans :many
-- Taken in ClearFactorOwedWithFactor's transaction before the UPDATE: the operator reset takes the
-- same row lock first, so a reset committed before this lock is seen by the UPDATE's snapshot and
-- one started after it waits. Without it a reset could commit between the UPDATE's snapshot and
-- its row lock, and the UPDATE would clear the mark from factors the reset had just deleted.
SELECT id FROM users WHERE kind = 0 AND deleted_at IS NULL AND factor_owed = 1 FOR UPDATE;
