-- name: PutPasswordCredential :exec
INSERT INTO password_credentials (user_id, phc, updated) VALUES ($1, $2, $3)
ON CONFLICT (user_id) DO UPDATE SET phc = excluded.phc, updated = excluded.updated;

-- name: GetPasswordCredential :one
SELECT phc FROM password_credentials WHERE user_id = $1;

-- name: ChangePasswordCredential :exec
-- Fix round 2 (security L1): a change writes the new hash and its change time in one statement.
INSERT INTO password_credentials (user_id, phc, updated, changed_at) VALUES ($1, $2, $3, $4)
ON CONFLICT (user_id) DO UPDATE SET phc = excluded.phc, updated = excluded.updated, changed_at = excluded.changed_at;

-- name: GetPasswordChangedAt :one
SELECT changed_at FROM password_credentials WHERE user_id = $1;

-- name: PutTOTP :exec
INSERT INTO totp_secrets (user_id, secret, digits, period, algorithm, confirmed_at, last_counter, created)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (user_id) DO UPDATE SET secret = excluded.secret, digits = excluded.digits,
  period = excluded.period, algorithm = excluded.algorithm, confirmed_at = excluded.confirmed_at,
  last_counter = excluded.last_counter;

-- name: GetTOTP :one
SELECT * FROM totp_secrets WHERE user_id = $1;

-- name: ConsumeTOTPCounter :execrows
UPDATE totp_secrets SET last_counter = $1 WHERE user_id = $2 AND last_counter < $3;

-- name: DeleteRecoveryCodes :exec
DELETE FROM recovery_codes WHERE user_id = $1;

-- name: PutRecoveryCode :exec
INSERT INTO recovery_codes (user_id, code_hash, created, used_at) VALUES ($1, $2, $3, NULL);

-- name: ConsumeRecoveryCode :execrows
UPDATE recovery_codes SET used_at = $1 WHERE user_id = $2 AND code_hash = $3 AND used_at IS NULL;

-- name: CountRecoveryCodes :one
SELECT count(*) FROM recovery_codes WHERE user_id = $1 AND used_at IS NULL;

-- name: PutWebauthnUser :exec
-- DO NOTHING, not DO UPDATE: a user handle is minted once and never rotated.
-- The authenticator stores the handle it saw at registration, so overwriting it
-- on a later ceremony makes GetWebauthnUserByHandle miss and permanently breaks
-- discoverable login for that account (deviation ID11).
INSERT INTO webauthn_users (rp_id, user_id, user_handle, created) VALUES ($1, $2, $3, $4)
ON CONFLICT (rp_id, user_id) DO NOTHING;

-- name: GetWebauthnUserHandle :one
SELECT user_handle FROM webauthn_users WHERE rp_id = $1 AND user_id = $2;

-- name: GetWebauthnUserByHandle :one
SELECT user_id FROM webauthn_users WHERE rp_id = $1 AND user_handle = $2;

-- name: PutWebauthnCredential :exec
INSERT INTO webauthn_credentials (cred_id, rp_id, user_id, public_key, sign_count, attestation_type, attestation_format, transports, flags, extensions_json, name, created, last_used)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13);

-- name: ListWebauthnCredentials :many
SELECT * FROM webauthn_credentials WHERE rp_id = $1 AND user_id = $2 ORDER BY created;

-- name: GetWebauthnCredential :one
SELECT * FROM webauthn_credentials WHERE cred_id = $1;

-- name: UpdateWebauthnCredential :exec
UPDATE webauthn_credentials SET sign_count = $1, flags = $2, last_used = $3 WHERE cred_id = $4;

-- name: PutCeremony :exec
INSERT INTO webauthn_ceremonies (id, kind, user_id, session_json, created, expires)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: GetCeremony :one
SELECT * FROM webauthn_ceremonies WHERE id = $1 AND expires > $2;

-- name: DeleteCeremony :execrows
DELETE FROM webauthn_ceremonies WHERE id = $1;

-- name: PruneCeremonies :execrows
DELETE FROM webauthn_ceremonies WHERE expires <= $1;

-- name: PutOIDCIdentity :exec
INSERT INTO oidc_identities (issuer, subject, user_id, created) VALUES ($1, $2, $3, $4)
ON CONFLICT (issuer, subject) DO NOTHING;

-- name: GetOIDCIdentity :one
SELECT user_id FROM oidc_identities WHERE issuer = $1 AND subject = $2;

-- name: RecordLoginAttempt :exec
INSERT INTO login_attempts (user_id, ip, method, ok, at) VALUES ($1, $2, $3, $4, $5);

-- name: CountLoginFailures :one
SELECT count(*) FROM login_attempts WHERE user_id = $1 AND ok = 0 AND at >= $2;

-- name: ClearLoginFailures :exec
DELETE FROM login_attempts WHERE user_id = $1 AND ok = 0;

-- name: DeleteTOTP :exec
DELETE FROM totp_secrets WHERE user_id = $1;

-- name: DeleteWebauthnCredential :execrows
-- Only the caller's own row: a credential id another account holds deletes nothing.
DELETE FROM webauthn_credentials WHERE user_id = $1 AND cred_id = $2;

-- name: DeleteWebauthnCredentialsByUser :exec
DELETE FROM webauthn_credentials WHERE user_id = $1;
