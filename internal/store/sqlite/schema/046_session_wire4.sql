ALTER TABLE sessions ADD COLUMN wire_version_next INTEGER NOT NULL DEFAULT 0 CHECK (wire_version_next IN (0, 2, 3, 4));
UPDATE sessions SET wire_version_next = wire_version;
ALTER TABLE sessions DROP COLUMN wire_version;
ALTER TABLE sessions RENAME COLUMN wire_version_next TO wire_version;
