-- +goose Up
CREATE INDEX mls_pending_proposals_live_group ON mls_pending_proposals(group_id) WHERE void_at IS NULL;

-- +goose Down
DROP INDEX mls_pending_proposals_live_group;
