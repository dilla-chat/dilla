-- Task 10c: "Control music (DJ)" is the role permission bit 25 (control_music). Migration 00048
-- grants it to every role a community's current (not removed) Bot installation had approved in
-- dj_role_policy, then drops that table. Roles approved only by a removed installation are not
-- mapped. The OR is idempotent: a role that already holds the bit is unchanged.
DROP TABLE dj_role_policy;
