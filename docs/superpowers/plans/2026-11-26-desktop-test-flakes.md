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

### Task 5: your own speaking indicator lights (review: opus)

Jonas, 2026-11-26: "speaking indicator for myself is not visible?" He added it to this PR ("Into the flake PR").

**The bug:**
- The desktop builds `voice.speaking` only from remote levels, and drops its own device
  (`clients/desktop/dilla-desktop/src/screens/voice/session.rs` ~1189-1195, `set.retain(|d| Some(*d) != own)`).
- `dilla-media` exposes no level for the local microphone.
- So the own stage tile, the own sidebar entry and the user panel avatar never show speaking. The panel ring lights
  only while push to talk is held.

**Spec (web parity):** `packages/web/src/call/session.ts` ~704-760:
- Own speaking comes from the local mic level, through the same `SpeakingDetector` thresholds as remote devices
  (`dilla_client::features::voice_prefs::speaking`).
- It counts only while the mic is sending: not muted, deafened or paused, and with push to talk only while the key is
  held.

**Files:**
- Modify: `clients/desktop/dilla-media/src` (the mic/capture pipeline): publish the local input RMS on a `watch`,
  `Call::local_audio_level()`. The level is measured after the noise suppressor and gain, on what would be sent.
- Modify: `clients/desktop/dilla-desktop/src/screens/voice/session.rs`: an own `SpeakingDetector` fed from that
  level. The own device is added to the speaking set only while the gate holds.
- Modify: `clients/desktop/dilla-desktop/src/screens/user_panel.rs`: the avatar ring lights when the own device is
  speaking, as well as while push to talk is held.
- Test: unit tests in `session.rs`, and a dilla-media test for the level.

- [ ] **Step 1: Write the failing tests:**
  - `own_device_speaks_from_the_local_level`: a level over `SPEAK_RMS` for over `SPEAK_ON_MS` puts the own device in
    the set.
  - `own_device_never_speaks_while_muted_deafened_paused_or_ptt_up`: one case for each.
  - `own_speaking_holds_for_the_hangover`.
  - A dilla-media test: a call fed a tone through the test mic reports a local level over `SPEAK_RMS`, and silence
    reports one under it. A muted mic reports 0.
  - Run them and verify they fail.
- [ ] **Step 2: Implement the level and the own detector.** The ring and the tile use the existing speaking styles.
- [ ] **Step 3: Run** the crates' unit tests, the `--ignored` live call tests that touch speaking, and
  `cargo clippy -D warnings`. Commit: `desktop: your own tile and avatar light while you speak`.

### Task 6: the own share's self-view at full resolution (review: sonnet)

Jonas, 2026-11-26: "why is the quality so bad?" (a screenshot of his own screen share on the stage). He chose "Full
resolution always".

**The cause:** the self-view tap scales each sent frame to at most `SELF_VIEW_MAX_SIDE` = 640 px at
`SELF_VIEW_FPS` = 15 (`clients/desktop/dilla-media/src/room.rs` ~478-560). The stage then stretches that frame about
4.5× on a 2× display. What viewers receive (about 1080p) is not affected.

**Files:**
- Modify: `clients/desktop/dilla-media/src/room.rs`:
  - Remove the `SELF_VIEW_MAX_SIDE` downscale. The copy is the sent frame's own size.
  - Raise `SELF_VIEW_FPS` to 30.
  - Keep the existing tap, live-flag and stop semantics, the `MAX_VIDEO_W/H` guard, and "the sent buffer is never
    written".
- Test: update the `self_view_tests` module:
  - A 1920×1080 sent frame reaches the self-view as 1920×1080.
  - A frame within one 30 fps interval of the last is skipped.
  - The existing stop and newest-frame tests stay green.

- [ ] **Step 1:** Change the tests to the new values and verify they fail.
- [ ] **Step 2:** Implement, then run the dilla-media unit tests and clippy (`-D warnings`).
- [ ] **Step 3:** Check whether dilla-desktop's own-tile drawing or a test assumes a 640 px self-view (grep
  `SELF_VIEW`), and adjust it.
- [ ] **Step 4:** Commit: `media: your own share's preview is the sent frame at full size`.

### Task 7: the title bar's call pill never overlaps the search (review: sonnet)

Jonas, 2026-11-26: a screenshot of an ~890 px wide window where the green "Stark's Pond" voice chip draws over the
search field's "Ctrl K" hint. He added it to this PR.

**The cause:**
- `clients/desktop/dilla-desktop/src/screens/titlebar.rs` ~507-523: the search slot is a fixed `SEARCH_SLOT` 420 px
  with `flex_none`, and the field a fixed `SEARCH_W` 360 px.
- The end group (~602-606) is `flex_1().min_w_0()`, and it also holds the window buttons. With too little room, its
  `justify_end` contents overflow leftwards over the search.

**Spec:** `packages/ui/src/TopBar/TopBar.css:4-28`:
- `.d-topbar__center { flex: 0 1 420px; min-width: 0 }`.
- `.d-topbar__search { width: 100%; max-width: 360px }`.
- The text ellipsizes and the kbd is `flex: none`.

**Files:** modify `titlebar.rs`; add the test beside the title bar's existing layout tests.

- [ ] **Step 1: Write the failing test** `the_search_shrinks_before_the_end_group_overlaps_it`. Draw the bar in a call
  with a long room name at window widths 1280, 890, 760 and 640, and assert three things at each width:
  - the end group's left edge ≥ the search slot's right edge;
  - the voice chip's bounds don't intersect the search field's;
  - the search field's width = min(360, its slot).

  Use the `bounds_probe` automation pattern, or GPUI test bounds, whichever the existing title-bar tests use. Run it
  and verify it fails at 890.
- [ ] **Step 2: Implement.**
  - The search slot gets flex-basis 420 px, can shrink, and has `min_w_0`.
  - The field is `w_full` with `max_w` 360.
  - The end group keeps its content width (no `min_w_0`; its items are `flex_none`), except the voice chip, whose
    room-name line truncates with an ellipsis at a sensible max width.
  - The brand keeps `flex_1().min_w_0()`.
- [ ] **Step 3:** Run the crate's title-bar and shell tests and clippy (`-D warnings`). Commit:
  `desktop: in a narrow window the title bar's search shrinks instead of sliding under the call pill`.

### Task 8: the first channel opened at startup is the route (review: sonnet)

Jonas, 2026-11-26: "when starting the app, the channel member list is not visible and you cant click the members icon.
It works directly if you change the textchannel". Spec: `docs/superpowers/specs/2026-11-26-members-startup-diagnosis.md`.

**The cause:**
- `ShellView::new` starts the history at `Route::Root`, because the client's first state has no servers.
- `settle_selection` later opens the first text channel (`clients/desktop/dilla-desktop/src/screens/shell.rs` ~388,
  ~423, ~538) without updating the history.
- `members::pane` draws only on `Route::Channel` (`screens/members/mod.rs` ~1045).

- [ ] **Step 1: Write the failing test** `the_first_channel_opened_after_the_server_list_lands_is_the_route` (the
  diagnosis's version). Start the shell with an empty state, then `set_state(world())`, and assert that:
  - the route is the first text channel;
  - the members aside is drawn;
  - one press of the Members toggle hides it.

  Verify it fails today.
- [ ] **Step 2: Implement.** At the end of `settle_selection`'s community branch, when the history head is `Root` or a
  `Channel` that differs from `current_route()`, call `history.replace(current_route())`. Use `replace`, not `push`,
  so Back doesn't land on an empty root.
- [ ] **Step 3:** Run the shell, members and navigation tests, and clippy. Commit:
  `desktop: the channel opened at startup is the route, so its member list shows`.

### Task 9: the profile card closes like the web's, its corners are round, and its actions work (review: sonnet)

Jonas, 2026-11-26: "it should be rounded but you see blue sticking out the edges", and "theres no close button". He
chose web parity for closing and to fix the smaller differences too. Spec:
`docs/superpowers/specs/2026-11-26-profile-card-diagnosis.md`, plus `packages/ui` `ProfileCard.tsx`/`.css` and
`packages/web` `MemberCardHost.tsx`.

**Files:**
- `clients/desktop/dilla-desktop-ui/src/components/members/mod.rs` (the card: banner, ring, icons, name, bio, height).
- `clients/desktop/dilla-desktop/src/screens/members/mod.rs` (`card_overlay`, `choose`).
- `clients/desktop/dilla-desktop/src/screens/user_settings/profile.rs` (the preview's banner).
- `clients/desktop/dilla-desktop/src/keys.rs` if Escape needs a binding.
- `parity/tolerance.toml` (`members/profile-card`, `user-settings/profile*`).

- [ ] **Step 1: Write the failing tests.**
  - The corners:
    - Extract a `band()` helper for the banner. Its top-left and top-right radii equal the card radius minus the 1 px
      border: 11 px for the card, 9 px for the settings preview.
    - A render check: the pixel at the card's (1,1) is not the banner colour.
    - Lower the two parity tolerances, so the corner wedges fail.
  - Closing, with `#[gpui::test]`s in the style of `voice.rs` ~457-502:
    - a click outside closes the card;
    - Escape closes it wherever focus is;
    - clicking the open card's row again doesn't close and reopen it;
    - Message and Compare each close the card;
    - Tab reaches the ×, and the × closes the card.
  - The actions: Message opens the DM (`dms::navigate`), and the shield opens the compare view
    (`dms::compare::open`).
  - The smaller differences:
    - a Bot's ring follows the web's shape;
    - action icons are 15 px;
    - long names wrap;
    - the card has a max height and scrolls;
    - the bio is capped at 8rem.

    Each gets a test or a parity check.
  - Verify the new tests fail.
- [ ] **Step 2: Implement.**
  - Follow the voice screen's card (`voice/menu.rs` ~513-528) for the outside click and Escape.
  - The card takes focus on open, and returns it to the row on close.
  - The × is visible only while it has keyboard focus (ProfileCard.css ~41-45).
- [ ] **Step 3:** Run the crates' tests, the parity check for those two shots, and clippy. Commit:
  `desktop: the profile card closes like the web's, its banner follows the round corners, Message and Compare work`.

### Task 10: the desktop reports call activity, so a live conversation isn't moved to AFK (review: opus)

Jonas, 2026-11-26: two people talking in Stark's Pond, both moved to AFK ("but we got moved to afk").

**The cause:** the desktop never calls `SendAfkActivity` (`POST /v1/communities/{id}/afk/activity`,
`clients/desktop/dilla-client/src/http/routes.rs:627`). The server's AFK clock only refreshes on those reports.

**Spec:** `packages/web/src/screens/Shell.tsx` ~606-680. Sends go through client-core `afkActivity` (body: the
channel, or `[channel, true]` for exempt).
- Reports only while in a call on a community voice channel that isn't that community's AFK channel, and only when
  the community has an AFK channel.
- One report on joining (the tracking key `community:channel:afkChannel` changes).
- While in the call:
  - every 5 s while own speaking is on (Task 5's own detector, `voice.speaking` contains the own device);
  - on pointer move, pointer down, key down or touch while the window is visible;
  - never more than one report per 5 s.
- Reports are fire-and-forget; errors are ignored.
- The exempt report is for phones and coarse pointers only, so the desktop never sends it.

**Files:**
- `dilla-client`: an `afk_activity(community, channel)` feature call over the existing route.
- `dilla-desktop`: a small `screens/voice/afk.rs` (or the closest existing home), holding the tracker: its state,
  the throttle and the triggers.
- Wiring: the voice sync (`screens/voice/mod.rs`) and the window's input hooks.

`shell.rs` belongs to Task 8 until it commits. Wire the input hooks without it if possible; otherwise wait for
Task 8's commit.

- [ ] **Step 1: Write the failing tests**, unit tests on the tracker with a fake clock:
  - `joining_a_call_reports_once`;
  - `own_speech_reports_every_5_s`;
  - `input_while_visible_reports_but_not_more_than_once_per_5_s`;
  - `input_while_hidden_does_not_report`;
  - `the_afk_channel_and_a_community_without_one_never_report`;
  - `a_new_call_resets_the_throttle`.

  Add a live or integration test: a desktop client in a call on a community with a short AFK timeout, speaking (or
  with input), is not moved. Use the testhost's AFK settings, if they allow a short timeout. Verify the tests fail.
- [ ] **Step 2: Implement.**
- [ ] **Step 3:** Run the crates' tests (unit and the live voice tests) and clippy. Commit:
  `desktop: report call activity so a conversation is not moved to AFK`.

**Order:** Tasks 1 and 2 run in parallel (disjoint files). Task 5 runs beside Task 3 (disjoint: `dilla-media/src`
and `dilla-desktop` vs Go and `call.rs`). Task 3 follows Task 2, because both touch the G3 tests and
the Bot logs. Task 4 runs last. Then the final review, `npm run ci:local`, and one PR.
