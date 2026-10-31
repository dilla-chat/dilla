-- Wire 5 adds ephemeral gateway transports; existing wire 4/3/2 sessions survive.
ALTER TABLE sessions ADD COLUMN wire_version_next BIGINT NOT NULL DEFAULT 0 CHECK (wire_version_next IN (0, 2, 3, 4, 5));
UPDATE sessions SET wire_version_next = wire_version;
ALTER TABLE sessions DROP COLUMN wire_version;
ALTER TABLE sessions RENAME COLUMN wire_version_next TO wire_version;
