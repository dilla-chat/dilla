-- +goose Up
-- Task 11 (Jonas, Q7): when the password was last changed, for Settings → Sign-in's "Changed {relative}". NULL until the next
-- change: an existing account's change date is unknown, and sign-up is not a change.
ALTER TABLE password_credentials ADD COLUMN changed_at INTEGER;

-- +goose Down
ALTER TABLE password_credentials DROP COLUMN changed_at;
