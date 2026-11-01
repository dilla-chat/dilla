-- Task 10c: an invite may allow any number of uses ("∞"). max_uses keeps its 1..1000 check and
-- holds 1000 for such a row, so a reader that ignores the flag fails closed. A never-expiring invite
-- needs no column: its expires_at is the largest signed 64-bit second (store.InviteNeverExpires).
ALTER TABLE invites ADD COLUMN unlimited_uses BIGINT NOT NULL DEFAULT 0 CHECK (unlimited_uses IN (0, 1));
