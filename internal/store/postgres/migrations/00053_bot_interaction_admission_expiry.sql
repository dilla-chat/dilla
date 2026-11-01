-- +goose Up
-- Every admission compacts its installation's receipts inside the writer transaction. Searched by
-- installation alone, both compaction statements visited every receipt kept in the 30-day window
-- (CT 144, 2026-10-30: about 6 s per admission on SQLite). The partial index serves the payload
-- compaction (only receipts that still hold a payload), the full one the tombstone deletion.
-- Neither changes what is kept or for how long.
CREATE INDEX bot_interaction_admission_receipts_live ON bot_interaction_admission_receipts(installation_id, not_after) WHERE octet_length(payload) > 0;
CREATE INDEX bot_interaction_admission_receipts_expiry ON bot_interaction_admission_receipts(installation_id, not_after);

-- +goose Down
DROP INDEX bot_interaction_admission_receipts_expiry;
DROP INDEX bot_interaction_admission_receipts_live;
