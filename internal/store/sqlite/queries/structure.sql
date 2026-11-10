-- name: CreateCommunity :exec
INSERT INTO communities (id, owner, name, icon_blob, policy_json, policy_version,
                         min_account_age_seconds, require_mod_2fa, created, deleted_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetCommunity :one
SELECT id, owner, name, icon_blob, policy_json, policy_version,
       min_account_age_seconds, require_mod_2fa, created, deleted_at
FROM communities
WHERE id = ? AND deleted_at IS NULL;

-- name: ListCommunities :many
SELECT id, owner, name, icon_blob, policy_json, policy_version,
       min_account_age_seconds, require_mod_2fa, created, deleted_at
FROM communities
WHERE id > ? AND deleted_at IS NULL
ORDER BY id
LIMIT sqlc.arg(max_rows);

-- name: ListCommunitiesForUser :many
-- GET /v1/communities (dilla-web-1 L-SQL-02): the live communities the user is a member of, by id
-- (the same order on both engines), over members_by_user (00013_members_by_user.sql).
SELECT c.id, c.owner, c.name, c.icon_blob, c.policy_json, c.policy_version,
       c.min_account_age_seconds, c.require_mod_2fa, c.created, c.deleted_at
FROM members m
JOIN communities c ON c.id = m.community_id
WHERE m.user_id = ? AND c.deleted_at IS NULL
ORDER BY c.id;

-- name: UpdateCommunityPolicy :execrows
-- The version is MONOTONE: two writers that read the same version both compute
-- the same successor, and the second must not land a different policy under a
-- number clients already hold. Zero rows is either an unknown community or a
-- lost race; the adapter tells the two apart.
UPDATE communities
SET policy_json = sqlc.arg(policy_json), policy_version = sqlc.arg(policy_version)
WHERE id = sqlc.arg(id) AND deleted_at IS NULL
  AND policy_version < CAST(sqlc.arg(policy_version) AS INTEGER);

-- name: UpdateCommunityMeta :execrows
UPDATE communities
SET name = ?, min_account_age_seconds = ?, require_mod_2fa = ?
WHERE id = ? AND deleted_at IS NULL;

-- name: SoftDeleteCommunity :execrows
UPDATE communities SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL;

-- name: SetCommunityOwner :exec
-- POST /v1/communities/{id}/owner (dilla-web-3-4 S5): the hand-over, under LockCommunity.
UPDATE communities SET owner = sqlc.arg(owner) WHERE id = sqlc.arg(id) AND deleted_at IS NULL;

-- name: LockCommunity :one
-- SQLite has no row locks and needs none: every write transaction is BEGIN
-- IMMEDIATE on a one-connection pool, so the join's and the ban's transactions
-- are already exclusive. This is the existence read the Postgres form shares.
SELECT id FROM communities WHERE id = ? AND deleted_at IS NULL;

-- name: PutMember :exec
INSERT INTO members (community_id, user_id, joined, nick)
VALUES (?, ?, ?, ?)
ON CONFLICT (community_id, user_id) DO UPDATE SET nick = excluded.nick;

-- name: DeleteMember :execrows
DELETE FROM members WHERE community_id = ? AND user_id = ?;

-- name: GetMember :one
SELECT community_id, user_id, joined, nick
FROM members WHERE community_id = ? AND user_id = ?;

-- name: ListMembersOfCommunity :many
SELECT community_id, user_id, joined, nick
FROM members
WHERE community_id = ? AND user_id > ?
ORDER BY user_id
LIMIT sqlc.arg(max_rows);

-- name: PutRole :exec
INSERT INTO roles (id, community_id, name, color, position, allow, deny, hoist, mentionable, created)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
  name = excluded.name, color = excluded.color, position = excluded.position,
  allow = excluded.allow, deny = excluded.deny, hoist = excluded.hoist,
  mentionable = excluded.mentionable;

-- name: GetRole :one
SELECT id, community_id, name, color, position, allow, deny, hoist, mentionable, created
FROM roles WHERE id = ?;

-- name: ListRoles :many
SELECT id, community_id, name, color, position, allow, deny, hoist, mentionable, created
FROM roles WHERE community_id = ? ORDER BY position, id;

-- name: PutMemberRole :exec
INSERT INTO member_roles (community_id, user_id, role_id)
VALUES (?, ?, ?)
ON CONFLICT (community_id, user_id, role_id) DO NOTHING;

-- name: DeleteMemberRole :execrows
DELETE FROM member_roles WHERE community_id = ? AND user_id = ? AND role_id = ?;

-- name: ListMemberRoles :many
SELECT role_id FROM member_roles WHERE community_id = ? AND user_id = ? ORDER BY role_id;

-- name: ListCommunityMemberRoles :many
SELECT user_id, role_id FROM member_roles WHERE community_id = ? ORDER BY user_id, role_id;

-- Channels (Plan 2 task 2, 00005_channels.sql).

-- name: CreateChannel :exec
INSERT INTO channels (id, community_id, kind, mode, visibility, parent_id, name, topic,
                      position, settings_json, host_policy_version, slowmode_seconds,
                      seq, created, deleted_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetChannel :one
SELECT id, community_id, kind, mode, visibility, parent_id, name, topic, position,
       settings_json, host_policy_version, slowmode_seconds, seq, created, deleted_at
FROM channels WHERE id = ? AND deleted_at IS NULL;

-- name: ListChannels :many
SELECT id, community_id, kind, mode, visibility, parent_id, name, topic, position,
       settings_json, host_policy_version, slowmode_seconds, seq, created, deleted_at
FROM channels
WHERE community_id = ? AND deleted_at IS NULL
ORDER BY position, id;

-- name: UpdateChannel :execrows
-- kind, community_id, seq and created are not written: a channel's kind and home
-- are immutable, and seq moves only through NextChannelSeq.
UPDATE channels
SET mode = ?, visibility = ?, parent_id = ?, name = ?, topic = ?, position = ?,
    settings_json = ?, host_policy_version = ?, slowmode_seconds = ?
WHERE id = ? AND deleted_at IS NULL;

-- name: DeleteChannel :execrows
UPDATE channels SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL;

-- name: DeleteChannelsOfCommunity :execrows
UPDATE channels SET deleted_at = ? WHERE community_id = ? AND deleted_at IS NULL;

-- name: NextChannelSeq :one
UPDATE channels SET seq = seq + 1 WHERE id = ? AND deleted_at IS NULL RETURNING seq;

-- name: DeleteRole :execrows
DELETE FROM roles WHERE id = ? AND community_id = ?;

-- Channel overwrites (Plan 2 task 3, 00006_overwrites.sql).

-- name: PutOverwrite :exec
INSERT INTO channel_overwrites (channel_id, target_kind, target_id, allow, deny)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (channel_id, target_kind, target_id) DO UPDATE SET
  allow = excluded.allow, deny = excluded.deny;

-- name: DeleteOverwrite :execrows
DELETE FROM channel_overwrites WHERE channel_id = ? AND target_kind = ? AND target_id = ?;

-- name: DeleteUserOverwrites :execrows
-- Fix wave I2: a kick, ban or leave drops the user's own (kind 1) overwrites in every channel of
-- the community, in the transaction that removes the membership.
DELETE FROM channel_overwrites
WHERE target_kind = 1 AND target_id = sqlc.arg(user_id)
  AND channel_id IN (SELECT id FROM channels WHERE community_id = sqlc.arg(community_id));

-- name: ListOverwrites :many
SELECT channel_id, target_kind, target_id, allow, deny
FROM channel_overwrites WHERE channel_id = ? ORDER BY target_kind, target_id;

-- Bans (Plan 2 task 4, 00007_bans.sql).

-- name: PutBan :exec
INSERT INTO bans (community_id, user_id, reason, by_user, created, expires)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (community_id, user_id) DO UPDATE SET
  reason = excluded.reason, by_user = excluded.by_user,
  created = excluded.created, expires = excluded.expires;

-- name: GetBan :one
SELECT community_id, user_id, reason, by_user, created, expires
FROM bans WHERE community_id = ? AND user_id = ?;

-- name: ListBans :many
SELECT community_id, user_id, reason, by_user, created, expires
FROM bans WHERE community_id = ? ORDER BY created DESC, user_id;

-- name: DeleteBan :execrows
DELETE FROM bans WHERE community_id = ? AND user_id = ?;

-- Channel members (Plan 2 task 6, 00008_channel_members.sql).

-- name: PutChannelMember :exec
INSERT INTO channel_members (channel_id, user_id, added)
VALUES (?, ?, ?) ON CONFLICT (channel_id, user_id) DO NOTHING;

-- name: DeleteChannelMember :execrows
DELETE FROM channel_members WHERE channel_id = ? AND user_id = ?;

-- name: DeleteCommunityChannelMembers :execrows
-- Fix wave C3: a kick, ban or leave drops the user from every channel of the community, in the
-- transaction that removes the membership. A DM has no community and is never matched.
DELETE FROM channel_members
WHERE user_id = sqlc.arg(user_id)
  AND channel_id IN (SELECT id FROM channels WHERE community_id = sqlc.arg(community_id));

-- name: ListChannelMembers :many
SELECT user_id FROM channel_members WHERE channel_id = ? ORDER BY user_id;

-- name: ListChannelsForUser :many
-- P2-D11: GET /v1/dms. The live DMs and group DMs (kinds 3 and 4) the user is a
-- participant of, newest first, ties broken by id.
SELECT c.id, c.community_id, c.kind, c.mode, c.visibility, c.parent_id, c.name, c.topic,
       c.position, c.settings_json, c.host_policy_version, c.slowmode_seconds, c.seq,
       c.created, c.deleted_at
FROM channels c
JOIN channel_members m ON m.channel_id = c.id
WHERE m.user_id = ? AND c.kind IN (3, 4) AND c.deleted_at IS NULL
ORDER BY c.created DESC, c.id;

-- Voice sessions (Plan 2 task 16, P2-D22, 00011_voice.sql).

-- name: PutVoiceSession :exec
-- A call is keyed by its call group's call id (R9), so the next call of the same group reopens
-- the ended row: the room, the group and the start are rewritten and ended is cleared. A live
-- row is left exactly as it is, so two devices starting the same call at once both land in the
-- one room the first wrote; the caller reads the row back to learn which.
INSERT INTO voice_sessions (call_id, channel_id, group_id, livekit_room, started, ended)
VALUES (?, ?, ?, ?, ?, NULL)
ON CONFLICT (call_id) DO UPDATE SET
  channel_id = excluded.channel_id, group_id = excluded.group_id,
  livekit_room = excluded.livekit_room, started = excluded.started, ended = NULL
WHERE voice_sessions.ended IS NOT NULL;

-- name: GetVoiceSession :one
SELECT call_id, channel_id, group_id, livekit_room, started, ended
FROM voice_sessions WHERE call_id = ?;

-- name: EndVoiceSession :execrows
UPDATE voice_sessions SET ended = CAST(sqlc.arg(at) AS INTEGER)
WHERE call_id = sqlc.arg(call_id) AND ended IS NULL;

-- name: ListLiveVoiceSessions :many
SELECT call_id, channel_id, group_id, livekit_room, started, ended
FROM voice_sessions WHERE channel_id = ? AND ended IS NULL
ORDER BY started, call_id;

-- name: EndAllVoiceSessionRows :exec
-- Invariant 11's "Live calls end", the voice_sessions half of store.MLS.EndAllVoiceSessions
-- (P2-D19): the call-group half is mls.sql's EndAllVoiceSessions.
UPDATE voice_sessions SET ended = CAST(sqlc.arg(at) AS INTEGER) WHERE ended IS NULL;
