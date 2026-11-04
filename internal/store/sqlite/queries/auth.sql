-- name: PutPasswordCredential :exec
INSERT INTO password_credentials (user_id, phc, updated) VALUES (?, ?, ?)
ON CONFLICT (user_id) DO UPDATE SET phc = excluded.phc, updated = excluded.updated;

-- name: GetPasswordCredential :one
SELECT phc FROM password_credentials WHERE user_id = ?;

-- name: ChangePasswordCredential :exec
-- Fix round 2 (security L1): a change writes the new hash and its change time in one statement.
INSERT INTO password_credentials (user_id, phc, updated, changed_at) VALUES (?, ?, ?, ?)
ON CONFLICT (user_id) DO UPDATE SET phc = excluded.phc, updated = excluded.updated, changed_at = excluded.changed_at;

-- name: GetPasswordChangedAt :one
SELECT changed_at FROM password_credentials WHERE user_id = ?;

-- name: PutTOTP :exec
INSERT INTO totp_secrets (user_id, secret, digits, period, algorithm, confirmed_at, last_counter, created)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (user_id) DO UPDATE SET secret = excluded.secret, digits = excluded.digits,
  period = excluded.period, algorithm = excluded.algorithm, confirmed_at = excluded.confirmed_at,
  last_counter = excluded.last_counter;

-- name: GetTOTP :one
SELECT * FROM totp_secrets WHERE user_id = ?;

-- name: ConsumeTOTPCounter :execrows
UPDATE totp_secrets SET last_counter = ? WHERE user_id = ? AND last_counter < ?;

-- name: DeleteRecoveryCodes :exec
DELETE FROM recovery_codes WHERE user_id = ?;

-- name: PutRecoveryCode :exec
INSERT INTO recovery_codes (user_id, code_hash, created, used_at) VALUES (?, ?, ?, NULL);

-- name: ConsumeRecoveryCode :execrows
UPDATE recovery_codes SET used_at = ? WHERE user_id = ? AND code_hash = ? AND used_at IS NULL;

-- name: CountRecoveryCodes :one
SELECT count(*) FROM recovery_codes WHERE user_id = ? AND used_at IS NULL;

-- name: PutWebauthnUser :exec
-- DO NOTHING, not DO UPDATE: a user handle is minted once and never rotated.
-- The authenticator stores the handle it saw at registration, so overwriting it
-- on a later ceremony makes GetWebauthnUserByHandle miss and permanently breaks
-- discoverable login for that account (deviation ID11).
INSERT INTO webauthn_users (rp_id, user_id, user_handle, created) VALUES (?, ?, ?, ?)
ON CONFLICT (rp_id, user_id) DO NOTHING;

-- name: GetWebauthnUserHandle :one
SELECT user_handle FROM webauthn_users WHERE rp_id = ? AND user_id = ?;

-- name: GetWebauthnUserByHandle :one
SELECT user_id FROM webauthn_users WHERE rp_id = ? AND user_handle = ?;

-- name: PutWebauthnCredential :exec
INSERT INTO webauthn_credentials (cred_id, rp_id, user_id, public_key, sign_count, attestation_type, attestation_format, transports, flags, extensions_json, name, created, last_used)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListWebauthnCredentials :many
SELECT * FROM webauthn_credentials WHERE rp_id = ? AND user_id = ? ORDER BY created;

-- name: GetWebauthnCredential :one
SELECT * FROM webauthn_credentials WHERE cred_id = ?;

-- name: UpdateWebauthnCredential :exec
UPDATE webauthn_credentials SET sign_count = ?, flags = ?, last_used = ? WHERE cred_id = ?;

-- name: PutCeremony :exec
INSERT INTO webauthn_ceremonies (id, kind, user_id, session_json, created, expires)
VALUES (?, ?, ?, ?, ?, ?);

-- name: GetCeremony :one
SELECT * FROM webauthn_ceremonies WHERE id = ? AND expires > ?;

-- name: DeleteCeremony :execrows
DELETE FROM webauthn_ceremonies WHERE id = ?;

-- name: PruneCeremonies :execrows
DELETE FROM webauthn_ceremonies WHERE expires <= ?;

-- name: CountOpenDiscoverableLogins :one
SELECT COUNT(*) FROM webauthn_ceremonies WHERE user_id IS NULL AND kind = 1 AND expires > ?;

-- name: CountOpenUserCeremonies :one
SELECT COUNT(*) FROM webauthn_ceremonies WHERE user_id = ? AND expires > ?;

-- name: EvictOldestDiscoverableLogins :execrows
DELETE FROM webauthn_ceremonies WHERE id IN (
  SELECT c.id FROM webauthn_ceremonies c
  WHERE c.user_id IS NULL AND c.kind = 1 AND c.expires > sqlc.arg(now) AND c.id <> sqlc.arg(keep)
  ORDER BY c.created, c.id LIMIT sqlc.arg(n)
);

-- name: EvictOldestUserCeremonies :execrows
DELETE FROM webauthn_ceremonies WHERE id IN (
  SELECT c.id FROM webauthn_ceremonies c
  WHERE c.user_id = sqlc.arg(user_id) AND c.expires > sqlc.arg(now) AND c.id <> sqlc.arg(keep)
  ORDER BY c.created, c.id LIMIT sqlc.arg(n)
);

-- name: PutOIDCIdentity :exec
INSERT INTO oidc_identities (issuer, subject, user_id, created) VALUES (?, ?, ?, ?)
ON CONFLICT (issuer, subject) DO NOTHING;

-- name: GetOIDCIdentity :one
SELECT user_id FROM oidc_identities WHERE issuer = ? AND subject = ?;

-- name: RecordLoginAttempt :exec
INSERT INTO login_attempts (user_id, ip, method, ok, at) VALUES (?, ?, ?, ?, ?);

-- name: CountLoginFailures :one
SELECT count(*) FROM login_attempts WHERE user_id = ? AND ok = 0 AND at >= ?;

-- name: ClearLoginFailures :exec
DELETE FROM login_attempts WHERE user_id = ? AND ok = 0;

-- name: DeleteTOTP :exec
DELETE FROM totp_secrets WHERE user_id = ?;

-- name: DeleteWebauthnCredential :execrows
-- Only the caller's own row: a credential id another account holds deletes nothing.
DELETE FROM webauthn_credentials WHERE user_id = ? AND cred_id = ?;

-- name: DeleteWebauthnCredentialsByUser :exec
DELETE FROM webauthn_credentials WHERE user_id = ?;

-- name: LockCeremonies :exec
-- SQLite serialises writers already (one write connection, immediate transactions): a no-op kept so both engines
-- expose the same Querier; Postgres takes a transaction-scoped advisory lock.
SELECT CAST(sqlc.arg(key) AS INTEGER);
