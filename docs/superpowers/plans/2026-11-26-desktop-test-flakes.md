# Two CI flakes that are product bugs: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan
> task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:**
- A Share or camera press is never lost because the media session hasn't taken up the call yet.
- Every Bot track plays to its end.
- The next queued track starts without an audible gap.
- A run of repeated /play starts doesn't drift upward behind read tokens.

**Architecture:**
- **Desktop:** share commands carry the call id they were made for. The media session keeps commands for the call it
  is entering and drops the rest.
- **Bot, track tail:** the encoder's EOF and the queue's drain become two separate signals. The track is unpublished
  only once its queue has drained.
- **Bot, switch:** the advance to the next track starts on input EOF, so the save, the start and the publish all
  overlap with the old track's last second.
- **Bot, catch-up:** a start's catch-up skips the remote group read when the call-group hints already show the helper
  is current.

**Tech Stack:** Rust (GPUI desktop, tokio), Go (cmd/dillabot, internal/bot, internal/media), pion/LiveKit
server-sdk-go.

**Spec:**
- `docs/superpowers/specs/2026-11-26-share-command-drop-diagnosis.md` (the share drop).
- `docs/superpowers/specs/2026-11-26-bot-track-gap-diagnosis.md` (the track tail, the serial switch and the read
  budget).
- Jonas, 2026-11-26: "Both, one PR".

## Global Constraints
- **No raised thresholds and no retries in tests.** The gap stays under 150 ms and p95 under 500 ms
  (`clients/desktop/dilla-media/tests/call.rs`), and `CALL_WAIT` stays at 45 s.
- **A press from an earlier call never carries into the next one.** That guarantee (`session.rs:808` comment) must
  still hold.
- **Logs and Debug output carry no E2EE content, track titles, member names, screen pixels or window titles.** Stage
  logs name a stage, a class and a duration only.
- **Builds use `CARGO_TARGET_DIR=$PWD/target CC=clang CXX=clang++ CARGO_BUILD_JOBS=4`.** Delete
  `target/debug/incremental` afterwards and never run `cargo clean`.
- **Checks before each commit:**
  - Go: `go build ./... && go vet ./...`, plus the touched packages' tests with `-race`.
  - Rust: `cargo test` for the touched crate, including its `--ignored` live tests where the task names them.
- **Builders share this worktree.** Each builder stages only its own files (`git commit --only <paths>`), and only
  runs `cargo fmt`/`gofmt` on the files it changed.

## Review Focus
1. **A move between calls while the old room closes (up to 5 s).** A Share pressed in the new call during the close
   starts the share in the new call. Pinned in Task 1, test 2.
2. **Leave and rejoin while the session is still busy.** A press tagged with the old call id opens nothing. Pinned in
   Task 1, test 3.
3. **/skip or /stop while the old track is draining.** The draining track stops at once, nothing is published twice,
   and the next track (on /skip) starts. Pinned in Task 2, the `pool_test` skip case.
4. **The last track in the queue ends.** It plays its full tail, and then the room goes quiet with no stray
   republish. Pinned in Task 2, the supervisor test.
5. **The hint stream disconnects.** The catch-up falls back to the remote GET. Pinned in Task 3.

---

### Task 1: the media session keeps share commands for the call it is entering (review: opus)

**Files:**
- Modify: `clients/desktop/dilla-desktop/src/screens/voice/session.rs`: `Command`, `run()` (~692-719), `in_call`
  (~808), `publish_share` (~1531-1545).
- Modify: `clients/desktop/dilla-desktop/src/screens/voice/mod.rs`: `toggle_share` (~2061), `share_source` (~2136),
  the camera toggle, and a test hook beside `put_test_room_open` (~1655).
- Test: `clients/desktop/dilla-desktop/tests/share.rs`.

**Interfaces:**
- Produces:
  - `Command::ShareScreen { call: [u8; 16], .. }`, `Command::Share(kind, on, options, call: [u8; 16])`,
    `Command::Quality`, `Command::AppAudio`, each carrying `call: [u8; 16]`.
  - `MediaSession::share(..., call)` and `share_screen(..., call)`.
  - Behind `test-hooks`: `voice::hold_media_pickup(window: WindowId, hold: bool, cx)`. While the hold is set, the
    session's `run()` waits before entering `in_call`.

- [ ] **Step 1: Write the failing tests** in `tests/share.rs` (`#[ignore]` live tests, the same style as the others):
  - `a_share_pressed_before_the_media_takes_the_call_still_starts`: hold the pickup, join, wait for `phase == In`,
    press Share, pick `"screen:1"`, release the hold. Assert `share.screen` turns on and `rec.opened() == 1`.
  - `a_share_pressed_while_a_move_closes_the_old_room_starts_in_the_new_call`: join call A, hold the pickup, move to
    call B, wait for `phase == In` in B, press Share and pick, release. Assert the share is on in B and
    `rec.opened() == 1`.
  - `a_press_for_an_earlier_call_does_not_carry_over`: hold the pickup, join, press Share and pick, leave and rejoin
    (a new call id), release. Assert `rec.opened() == 0` and `share.screen == false`.
- [ ] **Step 2: Run them and verify** that test 1 fails with "alice's share is on: never" and test 3 passes. Command:
  `cargo test -p dilla-desktop --features test-hooks --test share -- --ignored a_share_pressed a_press_for`.
- [ ] **Step 3: Implement.**
  - `run()` stashes commands received while idle (keep the newest per kind) instead of discarding them.
  - `in_call` keeps the stashed and queued commands whose `call == link.call_id` and drops the rest.
  - After the join succeeds, feed the kept commands through a `handle_command(..)` factored out of the loop's
    `commands.recv()` arm.
  - `toggle_share`, `share_source` and the camera toggle pass `ui.state.voice.call.call_id` (they already gate on
    `phase == In`).
  - `publish_share`'s refused `set_share` path logs `warn` (stage + error code only) and sets
    `VoiceNotice::ShareCancelled`.
- [ ] **Step 4: Run** all of `--test share -- --ignored` and the crate's unit tests until green. Then loop
  `closing_the_owner_window_stops_its_capture` 30 times under load
  (`taskset -c 0-3` + 8 `yes >/dev/null` busy loops), expecting 0 failures. Report the counts.
- [ ] **Step 5: Commit:** `desktop: a share pressed before the media session takes up the call is kept, not dropped`.

### Task 2: every track plays to its end, and the next one starts during the drain (review: opus)

**Files:**
- Modify: `internal/media/live_opus.go` (+ `live_opus_test.go`).
- Modify: `internal/bot/music/runtime/runtime.go` (+ test).
- Modify: `internal/bot/music/livekit/pool.go` (`monitorPooled` ~808, the busy check ~355) and `publisher.go`
  (`finish` ~279) (+ `pool_test.go`).
- Modify: `internal/bot/music/supervisor.go` (`monitor` ~862-987, `Save` ~963) (+ `supervisor_test.go`).
- Test: `clients/desktop/dilla-media/tests/call.rs` (`bot_next_queued_track_gap_is_under_150_ms`).

**Interfaces:**
- Produces:
  - `(*LiveOpus).Drained() <-chan struct{}` closes once the input has closed and the last queued sample has been
    handed out.
  - The runtime session exposes `InputDone() <-chan struct{}` (encoder exited, today's `done`) and
    `Done() <-chan struct{}` (input done and provider drained, or cancelled).

**Approach (ruling):**
- The pool may hold **one draining publication** next to the active slot. A persistent per-room publication would
  remove the per-track renegotiation too, but it is a larger change to the frame encryptor's per-track keying, so it
  is out of scope here.
- The new publication's provider holds its first sample until the draining one's `Drained()` closes, so the two
  tracks never overlap.

- [ ] **Step 1: Write the failing tests.**
  - `TestLiveOpusDrainedClosesAfterLastSample`: queue 3 samples and close the input. `Drained` stays open until the
    third `NextSample`, then closes, and the next call returns `io.EOF`.
  - `TestPooledSessionUnpublishesOnlyAfterDrain`: with 50 queued packets at input EOF, a fake participant's
    `UnpublishTrack` isn't called until all 50 are consumed.
  - `TestSkipDuringDrainStopsAtOnce`: /skip while the old track drains unpublishes it at once and starts the next.
  - `TestAdvanceStartsOnInputEOF`: with a fake starter and a fake store whose `Save` takes 400 ms, the next `Start`
    is called before the previous playback's `Done()`. The fake publisher's sample stream has no gap over 40 ms
    across the switch. The last queue item ends with no extra `Start`.
  - In `call.rs`: assert that the first track's audible span is at least 4.9 s (5 s fixture). The gap < 150 ms
    assertion stays.
- [ ] **Step 2: Run and verify they fail.**
  - Go: `go test -race ./internal/media/ ./internal/bot/music/...`.
  - The e2e: in the media job's G3 invocation (`.github/workflows/desktop-gpui.yml:525`), the span assertion fails at
    about 4.0 s.
- [ ] **Step 3: Implement.**
  - Wire in the drained signal (`live_opus` → runtime → `monitorPooled`, so `finish` runs on `Done()`).
  - Allow the draining slot.
  - `monitor` advances on `InputDone()`.
  - The advance's `Save` runs with `client.WithInteractive`.
  - Log an `advance_save` stage with its duration, and helper-gate waits over 50 ms by priority (no IDs).
- [ ] **Step 4: Run** the Go tests (`-race`) and the four G3 tests the way CI runs them (`--test-threads=1`) until
  green. Run the gap test 10 times and report the gaps and spans.
- [ ] **Step 5: Commit:** `bot: play each track to its end and start the next one while it drains`.

### Task 3: a start's catch-up waits for no read token (review: opus)

**Files:**
- Modify: `cmd/dillabot/music_call_guards.go` (`catchUp` ~104) (+ test).
- Modify: the `/stop` command path that sends a stale `client_revision` (find it from the WARN `client_revision_stale`
  on op 16; the spec, §2 "Why").
- Modify: `internal/bot/client/rate_limit.go`: log limiter waits over 20 ms with class and route template (no IDs).
- Test: `clients/desktop/dilla-media/tests/call.rs` (`bot_repeated_track_starts_within_500_ms_at_p95`).

- [ ] **Step 1: Write the failing tests.**
  - `TestCatchUpSkipsRemoteReadWhenHintIsCurrent`: a hinted head equal to the local `NextSequence` while the hint
    stream is connected makes zero `GroupInfo` calls.
  - `TestCatchUpReadsRemoteWithoutHint`: no hint seen yet, or the stream reconnected, makes one call.
  - A unit test that pins the root cause of the stale-revision /stop (the builder names it after finding the cause).
  - In `call.rs`: a drift guard. The median of runs 11-20 is at most 1.25 × the median of runs 1-10, plus 50 ms.
- [ ] **Step 2: Run them and verify they fail.**
- [ ] **Step 3: Implement.**
  - Keep the last hinted head per group from the existing `Watch` subscription, and skip the GET when the helper is
    current and the stream is connected.
  - Fix the stale revision at its source (the revision the /stop is built from), not by retrying.
  - Add the limiter wait log.
- [ ] **Step 4: Run** the Go tests (`-race`) and the G3 tests (as in Task 2). Report the 20 repeat values. The Bot
  log must show no `client_revision_stale` on /stop.
- [ ] **Step 5: Commit:** `bot: music starts skip the redundant group read, and /stop sends the current revision`.

### Task 4: docs and release note (review: sonnet)
- [ ] `docs/deploy/releases/unreleased.md`:
  - Bot music no longer cuts the last second of each track, and the next track follows without a gap.
  - Desktop: a Share pressed right after joining or moving calls no longer gets lost.
- [ ] Update `docs/user/` where Bot music or screen sharing is described, if a statement there changes.
  `npm run check:docs` must be green. Commit: `docs: note the Bot track-tail and share-press fixes`.

**Order:** Tasks 1 and 2 run in parallel (disjoint files). Task 3 follows Task 2, because both touch the G3 tests and
the Bot logs. Task 4 runs last. Then the final review, `npm run ci:local`, and one PR.
