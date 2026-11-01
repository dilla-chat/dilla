-- name: CreateGroup :exec
INSERT INTO mls_groups (group_id, binding, kind, community_id, target_id, call_id, ciphersuite,
                        epoch, seq, group_info_blob, tree_hash, public_group_state,
                        external_sender_key_id, e2ee_version, media_version, policy_version,
                        epoch_unknown, heal_deadline, created, closed_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetGroup :one
SELECT * FROM mls_groups WHERE group_id = ?;

-- name: ListOpenGroups :many
SELECT * FROM mls_groups WHERE closed_at IS NULL AND group_id > ?
ORDER BY group_id LIMIT sqlc.arg(max_rows);

-- name: ListGroupsForRetention :many
-- The retention walk, and deliberately NOT `ListOpenGroups`: invariant 10 caps application
-- ciphertext at thirty days for every group, and a group invariant 11 closed is still ciphertext
-- on the disk. Filtering on `closed_at IS NULL` here would mean a closed group's blobs are never
-- swept again by either half of retention -- neither the delivery floor nor archival `expires` --
-- and no other path reclaims them, so the promise inverts into "kept forever" at exactly the
-- moment the group stops being useful.
SELECT * FROM mls_groups WHERE group_id > ?
ORDER BY group_id LIMIT sqlc.arg(max_rows);

-- name: GroupsForTarget :many
-- Plan 2's P2-D3 (task 4): the open groups bound to one target, of one kind, over
-- mls_groups_by_target. A membership change finds the text and call groups of a channel
-- here instead of scanning every open group of the instance.
SELECT * FROM mls_groups
WHERE target_id = ? AND kind = ? AND closed_at IS NULL
ORDER BY created, group_id;

-- name: ListIdleCallGroups :many
-- web-3 (L-HTTP-105, RV-2): the idle call-group close. The open call groups (kind 1) that no live
-- voice session names and that were created at or before the cutoff, oldest first; the caller
-- closes at most max_rows of them per pass.
SELECT * FROM mls_groups g
WHERE g.kind = 1 AND g.closed_at IS NULL AND g.created <= ?
  AND NOT EXISTS (
    SELECT 1 FROM voice_sessions v WHERE v.group_id = g.group_id AND v.ended IS NULL
  )
ORDER BY g.created, g.group_id LIMIT sqlc.arg(max_rows);

-- name: CloseGroup :exec
UPDATE mls_groups SET closed_at = ? WHERE group_id = ?;

-- name: BumpGroupSeq :one
UPDATE mls_groups SET seq = seq + 1 WHERE group_id = ? RETURNING seq;

-- name: PutGroupState :exec
UPDATE mls_groups
SET epoch = ?, public_group_state = ?, group_info_blob = ?, tree_hash = ?, epoch_unknown = 0
WHERE group_id = ?;

-- name: SetGroupHealing :exec
UPDATE mls_groups SET epoch_unknown = ?, heal_deadline = ? WHERE group_id = ?;

-- name: MarkAllGroupsEpochUnknown :exec
-- Invariant 11's first half, as ONE statement rather than a paged loop: a restore runs once and
-- correctness, not latency, governs it, while a loop that stopped at a fixed batch would leave
-- every group past the batch serving state the restored database no longer matches. Closed groups
-- are skipped -- a closed group has nothing left to heal.
UPDATE mls_groups SET epoch_unknown = 1, heal_deadline = CAST(sqlc.arg(heal_deadline) AS INTEGER)
WHERE closed_at IS NULL;

-- name: ClearEpochUnknown :exec
-- An adopted heal. The deadline goes with the flag: closeUnhealedGroups reads the pair, and a
-- healed group that kept its deadline would be one restart away from looking overdue again.
UPDATE mls_groups SET epoch_unknown = 0, heal_deadline = NULL WHERE group_id = ?;

-- name: EndAllVoiceSessions :exec
-- Invariant 11's "Live calls end." A live call IS its call group (R9 puts the call id in the
-- companion column), and `voice_sessions` is Plan 2's table -- so on a Plan-1 database the whole
-- of "end every live call" is closing the call groups. Plan 2 task 1 extends the same statement
-- to `voice_sessions` rather than declaring a second method (deviation B13, P2-D19).
UPDATE mls_groups SET closed_at = CAST(sqlc.arg(at) AS INTEGER)
WHERE call_id IS NOT NULL AND closed_at IS NULL;

-- name: AppendHandshake :exec
INSERT INTO mls_handshakes (group_id, seq, epoch, kind, sender_leaf, sender_device, blob, created)
VALUES (?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListHandshakes :many
SELECT * FROM mls_handshakes WHERE group_id = ? AND seq >= ?
ORDER BY seq LIMIT sqlc.arg(max_rows);

-- name: OldestHandshakeSeq :one
SELECT seq FROM mls_handshakes WHERE group_id = ? ORDER BY seq LIMIT 1;

-- name: GetCommitAtEpoch :one
SELECT * FROM mls_handshakes
WHERE group_id = ? AND epoch = ? AND kind IN (1, 2)
ORDER BY seq LIMIT 1;

-- name: RaiseHandshakesPrunedThrough :exec
-- Runs in PruneHandshakes' transaction, BEFORE the DELETE with the same cutoff: every group whose
-- handshakes the DELETE is about to take records the highest seq it loses. MONOTONE by the `<`.
UPDATE mls_groups
   SET handshakes_pruned_through = (SELECT MAX(h.seq) FROM mls_handshakes h
                                     WHERE h.group_id = mls_groups.group_id
                                       AND h.created < CAST(sqlc.arg(created) AS INTEGER))
 WHERE handshakes_pruned_through < (SELECT COALESCE(MAX(h.seq), 0) FROM mls_handshakes h
                                     WHERE h.group_id = mls_groups.group_id
                                       AND h.created < CAST(sqlc.arg(created) AS INTEGER));

-- name: PruneHandshakes :execrows
DELETE FROM mls_handshakes WHERE created < ?;

-- name: PutProposal :exec
INSERT INTO mls_pending_proposals (group_id, ref, epoch, kind, target_leaf, target_device,
                                   key_package, origin, action_id, issued_at, ttl, void_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetProposal :one
SELECT * FROM mls_pending_proposals WHERE group_id = ? AND ref = ?;

-- name: ListLiveProposals :many
SELECT * FROM mls_pending_proposals WHERE group_id = ? AND epoch = ? AND void_at IS NULL
ORDER BY issued_at, ref;

-- name: ListAllProposals :many
SELECT * FROM mls_pending_proposals WHERE group_id = ? AND epoch = ?
ORDER BY issued_at, ref;

-- name: ListClosedGroupLiveProposals :many
-- The non-void proposals, any epoch, of the CLOSED groups: no commit can land in a closed group and
-- no group is ever reopened, so the sweep voids them (every other proposal walk reads open groups).
SELECT p.* FROM mls_pending_proposals p
JOIN mls_groups g ON g.group_id = p.group_id
WHERE g.closed_at IS NOT NULL AND p.void_at IS NULL
ORDER BY p.group_id, p.ref LIMIT sqlc.arg(max_rows);

-- name: CountLiveInstanceProposals :many
-- The gauge counts current-epoch instance proposals in open groups and all epochs in closed
-- groups, whose rows the closed-group void clears. Old open-group rows cannot be committed.
SELECT p.kind, count(*) AS n FROM mls_pending_proposals p
JOIN mls_groups g ON g.group_id = p.group_id
WHERE p.origin = 0 AND p.void_at IS NULL AND (g.closed_at IS NOT NULL OR p.epoch = g.epoch)
GROUP BY p.kind ORDER BY p.kind;

-- name: VoidProposal :exec
UPDATE mls_pending_proposals SET void_at = ? WHERE group_id = ? AND ref = ?;

-- name: DeleteProposal :exec
DELETE FROM mls_pending_proposals WHERE group_id = ? AND ref = ?;

-- name: DeleteMembers :exec
DELETE FROM mls_members WHERE group_id = ?;

-- name: PutMemberLeaf :exec
INSERT INTO mls_members (group_id, leaf_index, user_id, device_id, signature_key, added_epoch,
                         removed_epoch)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: ListMembers :many
SELECT * FROM mls_members WHERE group_id = ? AND removed_epoch IS NULL ORDER BY leaf_index;

-- name: ListBarredMembers :many
SELECT m.* FROM mls_members m JOIN devices d ON d.id = m.device_id
WHERE m.group_id = ? AND m.removed_epoch IS NULL
  AND (d.quarantined_at IS NOT NULL OR d.revoked_at IS NOT NULL)
ORDER BY m.leaf_index;

-- name: GroupsForDevice :many
SELECT DISTINCT group_id FROM mls_members WHERE device_id = ? AND removed_epoch IS NULL
ORDER BY group_id;

-- name: PutKeyPackage :exec
INSERT INTO key_packages (device_id, kp_ref, blob, last_resort, expires, created, consumed_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (device_id, kp_ref) DO NOTHING;

-- name: DeleteOtherLastResortKeyPackages :exec
DELETE FROM key_packages
WHERE device_id = ? AND last_resort = 1 AND kp_ref <> ?;

-- name: DeleteKeyPackage :exec
DELETE FROM key_packages WHERE device_id = ? AND kp_ref = ?;

-- name: TakeKeyPackage :one
UPDATE key_packages SET consumed_at = sqlc.arg(now)
WHERE key_packages.device_id = sqlc.arg(device_id) AND key_packages.kp_ref = (
  SELECT kp.kp_ref FROM key_packages kp
  WHERE kp.device_id = sqlc.arg(device_id) AND kp.consumed_at IS NULL AND kp.last_resort = 0
        AND kp.expires > sqlc.arg(now)
  ORDER BY kp.expires, kp.kp_ref LIMIT 1)
RETURNING *;

-- name: GetLastResortKeyPackage :one
SELECT * FROM key_packages
WHERE device_id = ? AND consumed_at IS NULL AND last_resort = 1 AND expires > ?
ORDER BY expires DESC, kp_ref LIMIT 1;

-- name: CountKeyPackages :one
SELECT count(*) FROM key_packages
WHERE device_id = ? AND consumed_at IS NULL AND last_resort = 0 AND expires > ?;

-- name: PurgeKeyPackagesKeepingLastResort :execrows
DELETE FROM key_packages WHERE last_resort = 0;

-- name: PurgeAllKeyPackages :execrows
DELETE FROM key_packages;

-- name: PutWelcomePayload :exec
INSERT INTO mls_welcome_payloads (blob_sha256, group_id, epoch, blob, created)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (blob_sha256) DO NOTHING;

-- name: PutEpochTree :exec
INSERT INTO mls_epoch_trees (group_id, epoch, ratchet_tree, tree_hash, created)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (group_id, epoch) DO NOTHING;

-- name: PutWelcome :exec
INSERT INTO mls_welcomes (device_id, group_id, epoch, commit_seq, blob_sha256, created, expires,
                          delivered_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (device_id, blob_sha256) DO NOTHING;

-- name: ListWelcomes :many
SELECT mls_welcomes.welcome_id, mls_welcomes.device_id, mls_welcomes.group_id, mls_welcomes.epoch,
       mls_welcomes.commit_seq, mls_welcomes.blob_sha256, mls_welcomes.created,
       mls_welcomes.expires, mls_welcomes.delivered_at, mls_welcome_payloads.blob,
       mls_epoch_trees.ratchet_tree, mls_epoch_trees.tree_hash
FROM mls_welcomes
JOIN mls_welcome_payloads ON mls_welcome_payloads.blob_sha256 = mls_welcomes.blob_sha256
LEFT JOIN mls_epoch_trees ON mls_epoch_trees.group_id = mls_welcomes.group_id
                         AND mls_epoch_trees.epoch = mls_welcomes.epoch
WHERE mls_welcomes.device_id = ? AND mls_welcomes.delivered_at IS NULL
      AND mls_welcomes.welcome_id > ?
ORDER BY mls_welcomes.welcome_id LIMIT sqlc.arg(max_rows);

-- name: DeleteWelcome :exec
UPDATE mls_welcomes SET delivered_at = ? WHERE device_id = ? AND welcome_id = ?;

-- name: PruneWelcomes :execrows
DELETE FROM mls_welcomes WHERE expires < ?;

-- name: PutForkReport :exec
INSERT INTO fork_reports (group_id, seq, reporter_device, epoch, reason, created)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (group_id, seq, reporter_device) DO NOTHING;

-- name: CountForkReporters :one
SELECT count(*) FROM fork_reports WHERE group_id = ? AND seq = ?;

-- name: QuarantineDevice :exec
UPDATE devices SET quarantined_at = ?, quarantine_reason = ? WHERE id = ?;

-- name: QueuePendingJoin :exec
-- pending_joins (Plan 1 follow-up card 8, Plan 2 task 7): one device waiting for a slice of a join
-- storm. A device already queued keeps its row and its place.
INSERT INTO pending_joins (group_id, device_id, queued) VALUES (?, ?, ?)
ON CONFLICT (group_id, device_id) DO NOTHING;

-- name: ListPendingJoins :many
SELECT device_id FROM pending_joins WHERE group_id = ?
ORDER BY queued, device_id LIMIT sqlc.arg(max_rows);

-- name: DeletePendingJoin :exec
DELETE FROM pending_joins WHERE group_id = ? AND device_id = ?;

-- name: CountPendingJoins :one
SELECT count(*) FROM pending_joins WHERE group_id = ?;

-- name: ListPendingJoinGroups :many
SELECT DISTINCT group_id FROM pending_joins WHERE group_id > ?
ORDER BY group_id LIMIT sqlc.arg(max_rows);
