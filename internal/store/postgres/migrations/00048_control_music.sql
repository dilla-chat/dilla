-- +goose Up
UPDATE roles SET allow = allow | 33554432
WHERE id IN (
  SELECT p.role_id FROM dj_role_policy p
  JOIN bot_installations b ON b.id = p.installation_id
  WHERE b.removed_at IS NULL AND b.community_id = roles.community_id
);
-- Task 10c: "Control music (DJ)" is the role permission bit 25 (control_music). Migration 00048
-- grants it to every role a community's current (not removed) Bot installation had approved in
-- dj_role_policy, then drops that table. Roles approved only by a removed installation are not
-- mapped. The OR is idempotent: a role that already holds the bit is unchanged.
DROP TABLE dj_role_policy;

-- +goose Down
-- The bit stays on the roles: a binary from before 00048 masks bit 25 away on read.
CREATE TABLE dj_role_policy (
  installation_id BYTEA NOT NULL CHECK(octet_length(installation_id)=16) REFERENCES bot_installations(id) ON DELETE CASCADE,
  role_id BYTEA NOT NULL CHECK(octet_length(role_id)=16) REFERENCES roles(id) ON DELETE CASCADE,
  approved_by BYTEA NOT NULL CHECK(octet_length(approved_by)=16) REFERENCES users(id),
  approved_at BIGINT NOT NULL,
  PRIMARY KEY(installation_id, role_id)
);
