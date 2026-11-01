-- Task 10c. feature_capabilities holds capability bits 8 (polls) and 9 (reminders) beside the
-- original 0..255 capabilities byte, whose check stays as it is: the Go row reads both as one
-- 10-bit mask. Existing installations get both on (DEFAULT 3), which is how they behaved before.
-- music_volume / music_idle_minutes are the Music page's defaults (prototype values only; 100 and
-- 30 keep today's behaviour). restart_requested counts manager restart requests and
-- restart_handled the last one the runtime finished; runtime_version is what the runtime last
-- reported (NULL for a runtime that never reports one).
ALTER TABLE bot_installations ADD COLUMN feature_capabilities INTEGER NOT NULL DEFAULT 3 CHECK (feature_capabilities BETWEEN 0 AND 3);
ALTER TABLE bot_installations ADD COLUMN music_volume INTEGER NOT NULL DEFAULT 100 CHECK (music_volume IN (25, 50, 75, 100));
ALTER TABLE bot_installations ADD COLUMN music_idle_minutes INTEGER NOT NULL DEFAULT 30 CHECK (music_idle_minutes IN (5, 15, 30));
ALTER TABLE bot_installations ADD COLUMN restart_requested INTEGER NOT NULL DEFAULT 0 CHECK (restart_requested >= 0);
ALTER TABLE bot_installations ADD COLUMN restart_handled INTEGER NOT NULL DEFAULT 0 CHECK (restart_handled >= 0);
ALTER TABLE bot_installations ADD COLUMN runtime_version TEXT CHECK (runtime_version IS NULL OR length(runtime_version) BETWEEN 1 AND 32);
