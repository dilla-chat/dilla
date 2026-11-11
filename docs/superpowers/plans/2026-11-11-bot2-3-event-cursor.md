# v0.1.0-bot2.3: the Bot's event cursor survives the v1 upgrade; /play is idempotent Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop the Bot's helper being closed on every gateway wake, and make sure a /play whose answer was lost doesn't queue the track twice.

**Architecture:**
- The Bot's event journal (Rust, `core/dilla-core/src/client/bot_events.rs`) reports a page with `next` (the acknowledged cursor) and `high_water` (the MAX of the remaining delivery ids).
- After the v1→bot2 upgrade removed the v1 interaction groups' rows, which were the newest ones, `next` is greater than `high_water`.
- Go (`internal/bot/native/groups.go:1063`) rejects that page as corrupt. Because ReadGroupEvents runs as a mutation, the helper is invalidated and closed. This happens on every text poll, which runs on every gateway wake.
- The fix: report `high_water ≥ ack` in Rust, stop treating a bad read page as a helper-killing mutation, and make /play enqueue idempotent.

**Tech Stack:** Rust (dilla-core, dilla-bot-core), Go (internal/bot/native, internal/bot/service, cmd/dillabot), shared CBOR vectors.

**Spec / evidence:** `docs/superpowers/plans/2026-11-11-bot2-3-event-cursor-rootcause.md`. Production evidence from CT 144: 8 closes in 4 min, all `reason=invalidated_decode operation=group_apply kind=result_decode`, with the backoff growing to 80 s. Bot v2 spec: `docs/superpowers/specs/2026-11-06-bot-v2-design.md`.

## Global Constraints

- Never log message content, command arguments, keys, sealed bytes, or a track URL, title, video id or query.
- No schema migration. No wire version change. The helper↔Go CBOR shape is unchanged; only the values change.
- The item-id derivation must not use content. It uses only the request id, which is random, and the item index.
- User strings never say "encrypted".
- Commit trailer: `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>` and `Claude-Session: https://claude.ai/code/session_012Nny5K2B969mk49QX3XSXG`.

## Review Focus

1. An instance upgraded from v1 whose journal's newest rows were deleted. The first read after the upgrade decodes, the poller acknowledges, and no helper close follows.
2. A healthy instance, where ack ≤ MAX(delivery_id): behaviour and bytes are unchanged.
3. A /play whose helper dies after the enqueue commits, followed by a NEW /play for the same track. Two queue entries are expected, because these are two user requests. A same-request-id replay or retry gives one.
4. A ReadGroupEvents page that's still malformed in some other way fails that poll with a logged detail token, and doesn't close the helper.
5. The ack path: acknowledging `next` when `next > MAX(delivery_id)` succeeds, and isn't refused by `bot_events.rs:401`.

---

### Task 1: Event cursor clamp (Rust) + cross-language vector + non-mutating read (Go)

**Files:**
- Modify: `core/dilla-core/src/client/bot_events.rs` (~282 read high_water, ~290 next, ~368 encode, ~395/401/410 ack path)
- Modify: `internal/bot/native/groups.go` (~1063 decoder check stays; ~1075 ReadGroupEvents mutation flag → false)
- Modify: `internal/bot/native/client.go` (the `CloseDetailed` detail token for a decode failure, if a mutation still fails)
- Test: the Rust unit test in `bot_events.rs` (or its test module); a shared vector under the repo's existing vectors directory (find it with `vectors` in ci-local / Cargo); a Go decode test in `internal/bot/native`.

- [ ] **Step 1: Write failing tests**
  - Rust `events_page_after_newest_rows_deleted`: journal rows 1..5, ack 5; delete rows 3..5. The read page has `next == 5` and `high_water == 5`. An acknowledge of 5 succeeds.
  - Rust `events_page_unchanged_when_healthy`: rows 1..5, ack 3 → `next 3`, `high_water 5`. The bytes are identical to the current encoding.
  - Shared vector: the page `83 80 05 05` (after the deletion) must decode in Go. `83 80 05 03` stays a negative vector → "event cursors".
  - Go `TestReadGroupEventsIsNotAMutation`: a malformed page (`83 80 05 03`) makes ReadGroupEvents return an error. `Usable()` stays true, and no `bot native helper closed` line is logged.
- [ ] **Step 2: Run them and confirm they fail.** (`cargo test -p dilla-core bot_events`, `go test ./internal/bot/native/ -run 'ReadGroupEvents|Events' -count=1`, and the vectors check.)
- [ ] **Step 3: Implement.**
  - In Rust, `high_water = max(MAX(delivery_id), ack)` on the read path and the ack path. Accept an ack equal to that clamped high water.
  - In Go, set ReadGroupEvents' mutation flag to false. When a page fails to decode, return an error carrying `events.cursor_above_high_water` or `events.page_invalid`. This token is a fixed string, never bytes.
- [ ] **Step 4: Run the full checks.** `cargo test` for dilla-core and dilla-bot-core, `go test ./internal/bot/... ./cmd/dillabot/`, and the vectors check.
- [ ] **Step 5: Commit** `fix(bot): the event journal's cursor survives deleted v1 rows; a bad event page no longer kills the helper`

### Task 2: /play enqueue is idempotent per request

**Files:**
- Modify: `internal/bot/service/music_interactions.go` (~373 item ids, ~532-536 conflict loop)
- Test: `internal/bot/service/music_interactions_test.go`, plus a cmd/dillabot rig test if one exists for /play replay.

- [ ] **Step 1: Write failing tests**
  - `TestPlayEnqueueIdempotentPerRequest`:
    - Enqueue via /play with request id R. The helper "dies" after the commit. The answer isn't sealed.
    - The same request R is handled again, as a replay after a respawn.
    - The queue holds ONE entry, and the answer reports it as queued or playing.
  - `TestPlayConflictRetryKeepsDerivedIDs`: the conflict re-apply loop reuses the same derived ids. It doesn't mint fresh ones.
  - `TestDistinctRequestsStillQueueTwice`: two different request ids for the same query give two entries. That is two user actions.
- [ ] **Step 2: Run them and confirm they fail.**
- [ ] **Step 3: Implement.**
  - Item id = the first 16 bytes of SHA-256("dilla/music-item/v1" ‖ request id ‖ index).
  - An enqueue whose first derived id is already in the queue, or current, is treated as already applied: answer with the existing position, don't add again.
  - The id is still random to any observer, because the request id is random. Never derive it from the URL or title.
- [ ] **Step 4: Run** `go test ./internal/bot/... ./cmd/dillabot/`.
- [ ] **Step 5: Commit** `fix(bot): a replayed /play does not queue the track twice`

### Task 3: Release notes v0.1.0-bot2.3

**Files:** Create `docs/deploy/releases/v0.1.0-bot2.3.md`, using bot2.2's notes as the template.

The notes say:
- **The fault:** on instances upgraded from v1, the helper closed on every gateway wake, so the Bot stalled under use and the restart wait kept growing. Name the log signature: `bot native helper closed reason=invalidated_decode operation=group_apply kind=result_decode`.
- **The fix,** and that no data migration is needed.
- **A workaround for anyone who can't upgrade yet:** post one message in a Bot channel.
- **/play idempotency.**
- **Upgrade:** the same steps as bot2.2.
- **Rollback note:** bot2.1 or bot2.2 on a compacted database keeps auto_vacuum=INCREMENTAL but doesn't run the incremental pass.
- **Versions table.**

- [ ] **Step 1:** Write the notes. Check every claim against the code.
- [ ] **Step 2:** Run `npm run check:docs`.
- [ ] **Step 3: Commit** `docs(release): v0.1.0-bot2.3 notes`
