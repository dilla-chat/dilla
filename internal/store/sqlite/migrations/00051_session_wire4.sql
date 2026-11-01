-- +goose Up
-- Wire 4 (Task S1 part 2, the account-kind attestation): a session may negotiate wire version 4.
-- SQLite cannot alter a column CHECK, so the column is swapped for one that admits 4.
ALTER TABLE sessions ADD COLUMN wire_version_next INTEGER NOT NULL DEFAULT 0 CHECK (wire_version_next IN (0, 2, 3, 4));
UPDATE sessions SET wire_version_next = wire_version;
ALTER TABLE sessions DROP COLUMN wire_version;
ALTER TABLE sessions RENAME COLUMN wire_version_next TO wire_version;

-- +goose Down
DELETE FROM sessions WHERE wire_version = 4;
ALTER TABLE sessions ADD COLUMN wire_version_prev INTEGER NOT NULL DEFAULT 0 CHECK (wire_version_prev IN (0, 2, 3));
UPDATE sessions SET wire_version_prev = wire_version;
ALTER TABLE sessions DROP COLUMN wire_version;
ALTER TABLE sessions RENAME COLUMN wire_version_prev TO wire_version;
