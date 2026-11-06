-- +goose Up
-- Wire 6 (Bot v2) is the only wire dillad offers; a session that negotiated an older wire stays readable until it renegotiates.
ALTER TABLE sessions ADD COLUMN wire_version_next INTEGER NOT NULL DEFAULT 0 CHECK (wire_version_next IN (0, 2, 3, 4, 5, 6));
UPDATE sessions SET wire_version_next = wire_version;
ALTER TABLE sessions DROP COLUMN wire_version;
ALTER TABLE sessions RENAME COLUMN wire_version_next TO wire_version;

-- +goose Down
-- A rollback to a wire-5 binary: wire 6 selections become wire 5, the newest wire that binary speaks.
UPDATE sessions SET wire_version = 5 WHERE wire_version = 6;
ALTER TABLE sessions ADD COLUMN wire_version_prev INTEGER NOT NULL DEFAULT 0 CHECK (wire_version_prev IN (0, 2, 3, 4, 5));
UPDATE sessions SET wire_version_prev = wire_version;
ALTER TABLE sessions DROP COLUMN wire_version;
ALTER TABLE sessions RENAME COLUMN wire_version_prev TO wire_version;
