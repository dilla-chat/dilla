-- Wire 6 (Bot v2) is the only wire dillad offers; a session that negotiated an older wire stays readable until it renegotiates.
ALTER TABLE sessions ADD COLUMN wire_version_next INTEGER NOT NULL DEFAULT 0 CHECK (wire_version_next IN (0, 2, 3, 4, 5, 6));
UPDATE sessions SET wire_version_next = wire_version;
ALTER TABLE sessions DROP COLUMN wire_version;
ALTER TABLE sessions RENAME COLUMN wire_version_next TO wire_version;
