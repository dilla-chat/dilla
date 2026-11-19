# GPUI desktop: close the GUI gaps before alpha.2 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close every GUI gap between the Rust/GPUI desktop app and the web client's desktop layout before the signed
`desktop-gpui-v0.2.0-alpha.2`. That means built-in slash commands and their forms, load earlier, row menus, banners, the Bot's
music and Welcome surfaces, poll posts, thread commands, the visual-parity residue and the missing references. It also fixes the
PeerConnections that outlive their call.

**Architecture:** Each gap is closed at its existing seam:
- Client logic (plans, attempts, protocol answers) goes into `dilla-client` as pure functions with unit tests.
- Drawing goes into `dilla-desktop` screens on top of the existing `dilla-desktop-ui` components.
- Every new visible state gets a parity fixture and a reference: a web reference shot by `parity/shoot.mjs`, or a `native/`
  reference where Decisions say so.
- The work runs as a dependency-ordered spine with a few disjoint lanes in parallel (see Execution order), on one branch with one PR.

**Tech Stack:** Rust (GPUI, gpui-component, tokio, jiff, cxx into the vendored `webrtc-sys`), the web client's string tables
(`packages/web/src/strings`), the parity harness (`clients/desktop/parity`: `shoot.mjs` + Playwright for web references,
`dilla-parity` for comparison), `dilla-testhost` for `#[ignore]`d tests against a real dillad (with its Bot and SFU where noted).

**Spec:** The web client's desktop layout (`packages/web/src`, rendered in the retired Electron beta.5) for behaviour, copy and
states, and the Signal prototype (`docs/design/signal/prototype`) for the look. Each task cites the web file and line it ports.
The gap inventory and the visual-pass evidence were scratch files of the drafting session and are not kept. In summary:
- **Gap inventory.** Composer: 27 built-ins answer `UNAVAILABLE` (the catch-all at `dilla-client/src/features/bot.rs:224-229`);
  `/poll create`, announcements and `/rolepanel create` have no form; there is no command memory. Shell: no "load earlier" in a
  channel, DM or thread; the row menu has no notify modes or Edit channel; ban rows have no details; there is no removal or
  "instance too old" banner, no link-quality colour, and no search jump to an unloaded message. Bot: no music card, stage bar or
  Welcome page (the page is a stub). Polls: posts are never verified or worded ("{name} asked:"), and polls in threads are missing.
- **Visual pass (Linux, scale 2).** Several gated screens are `#[ignore]`d with a measured residue: self roles, roles,
  channel-permissions, the music page, keyboard, voice settings, profile, cards, the emoji grid, the poll card and the ring banner.
  The 1-2px offsets are in onboarding, sign-in and the server rail. The DM screens have no references at all. There is no group
  call, picker, toast or reply-compose state.
- **Exit crash follow-up.** After all 10 `#[ignore]`d tests in `dilla-desktop/tests/voice.rs` leave their calls, 52 Dilla frame
  transformers are still registered at exit (found while fixing the exit SIGSEGV in commit 6dbe6c202).

**Path shorthands used in the tasks:**
- W = `packages/web/src`
- D = `clients/desktop/dilla-desktop/src`; `D/../tests` is `clients/desktop/dilla-desktop/tests`
- U = `clients/desktop/dilla-desktop-ui/src`
- C = `clients/desktop/dilla-client/src`
- P = `clients/desktop/parity`
- `$ENV` = `CARGO_TARGET_DIR=$PWD/target CC=clang CXX=clang++`, run from the worktree root
  `/home/thim/Repositories/dilla/.claude/worktrees/signal-design`

A task that says "W file:line" ports exactly that rule.

## Global Constraints

- Pixel parity with the web desktop layout (`packages/web/src`) and the Signal prototype; any deviation only where listed in Decisions below — anything else goes to Jonas first.
- Strings reuse the web's keys verbatim; a new desktop-only key goes into `packages/web/src/strings/en.ts` under a `// Native desktop only (Jonas 2026-11-19): …` comment, then `node scripts/gen-gpui-strings.mjs` (and `--check` passes). Overrides of an existing key's wording go in `packages/web/src/strings/desktopOverrides.ts`.
- Every new visible state gets a parity fixture (`clients/desktop/parity/fixtures/<area>/<state>.json`) and a reference: a web reference shot by `shoot.mjs`, or a `native/` GPUI-only reference where Decisions say so.
- Every `native/` reference goes through Task 1's `assert_native_screen(fixture, golden, prepare)`, is blessed with `DILLA_PARITY_BLESS=1` after it has been looked at by eye, and is listed in the `native/` table of `clients/desktop/parity/references/linux/README.md`. Where a task builds "a state in the test", that state is a GPUI-only fixture JSON (no web sidecar).
- BEHAVIOUR-GATES rows can't be added (the titles are frozen in `clients/desktop/behaviour-gates-titles.tsv`). Only append test names to existing rows, and `node --test scripts/behaviour-gates.test.mjs` must pass.
- Build env: `CARGO_TARGET_DIR=<worktree>/target CC=clang CXX=clang++`.
- `npm run ci:local -- --changed` before every push; `cargo clippy --all-targets --locked -- -D warnings` and `cargo fmt --check` for every touched crate.
- One branch (`release/desktop-gpui-alpha.2`), one PR (#190). No new branch, no second PR.
- Code comments are explanatory prose in the surrounding style; test names in comments must exist; cite the PR or merge commit, never a squashed branch SHA.
- A `#[ignore]`d live test names its prerequisites in the ignore reason, like the existing ones (`"needs dillad: …"`, `"needs dillad with its Bot: …"`).
- E2EE content (message bodies, keys, evidence) never leaves the client in logs or test output.
- User docs (`docs/user/desktop.md`, README "What works today", re-shot screenshots) are current at the end (Task 43).

## Decisions (Jonas, 2026-11-19)

Composer:
- **D1** `/modlog` opens a `Dialog` over the conversation, titled `admin.bot.moderationLog.title` and drawing the existing `moderation_log_section`; it never jumps to settings (Task 24).
- **D2** The announcement and role-panel editors are the shared `Dialog` with U `Select`/`TextField`, in the web's field order with its labels and copy, and get a GPUI-only `native/` reference (Tasks 28, 29). The role list follows D20.
- **D3** Uncertain built-ins and forms match the web: the uncertain run's request id is kept, the draft reopens, and a resend reconciles (Task 30).
- **D4** Commands work in the thread composer this wave (Task 31, after Task 30).

Shell:
- **D5** Gap 5, "More options" in the invite dialog, is dropped: the web never shows `invite.options`, so there is no task.
- **D6** The channel row menu keeps the two GPUI-only items "Mark as read" and "copy link", after the web's items. "Mark as read" gets the desktop-only key `shell.channels.markRead` (Task 13).
- **D7** Edit channel from the row menu opens over the conversation (Task 13).
- **D8** The thread pane gets "load earlier" this wave. This is a deliberate deviation from the web, with a GPUI-only reference (Task 17).

Bot and polls:
- **D9** Bot parity references use the real test Bot. The world gets a `"bot": {"capabilities": [...], "botChannels": [...]}` ServerSpec extension seeded through `enable_bot` (Task 38). The music card and the stage bar get `native/` references, looked at by eye against `packages/ui/src/NowPlaying` `np.fixture.tsx` (Tasks 35, 36).
- **D10** Card visibility means viewport intersection: the row itself intersects the viewport (`top < at + viewport && bottom > at`), not the drawn band (Task 35).
- **D11** Hints for a card outside your call use watched channels: a set of channels whose op-63 kind-2 hints a card on screen follows (Task 4).
- **D12** Poll post verification stays in this wave (Task 27). Re-reading on Bot hints (PollCard.tsx:197) stays out (tracked for later).
- **D13** Polls inside threads are in this wave (Task 41).
- **D14** The search jump has no fallback timer for a row that never draws (Task 16).

Visual:
- **D15** Checkboxes match the web's 22px box everywhere. This is one shared component change in `dilla-desktop-ui` (the CheckList box, made a standalone checkbox), and every user follows it (Task 2).
- **D16** The bio textarea draws the resize grip and is drag-resizable vertically, with a minimum of 34px (Task 33).
- **D17** App-wide compact density (V5) is NOT in this wave and is tracked for later. The fixture-local density fix and its ignored live test (Task 19) stay.
- **D18** The attachment image crop gets fixed (sub-texel UVs). Colour emoji screens get measured per-screen Linux tolerances, as macOS has (Task 7).
- **D19** Server settings → Self roles follows the prototype's controls (CheckList, Select), not the web's unstyled browser `<fieldset>`, and gets a GPUI-only reference (Task 18).

Controller reconciliation (from merging the drafts; tell Jonas):
- **D20** The role-panel editor's per-role choice is the shared 22px checkbox from D15 (the web's checkbox), not a `Toggle`. D2 chose `Toggle` before D15 made a parity checkbox available, and D15 says "everywhere".

## Review Focus

1. **`/delete` of another person's message.** An `Unknown` removal must be resolved with the same proof (`resolve_removal`) and never sent a second time. The reason is required, and `manage_messages` is checked again after the confirmation (builtin.ts:136-145, 337-376). Test: Task 10, `removal_steps_never_send_a_second_removal`.
2. **A form submitted twice.** Enter pressed twice, or Post clicked while sending, in the poll form, the announcement editor or the role-panel editor must send one request: one request id per opened form, and Post disabled while busy. Tests: Task 26, `a_second_post_while_busy_sends_nothing`; Task 28, `a_second_save_while_busy_sends_nothing`; Task 29, `a_second_publish_while_busy_sends_nothing`.
3. **"load earlier" pressed twice before the first read lands, or while a message arrives or the timeline refreshes.** Each press grows the page by exactly 100 (the web does not guard a second press either), the rows in view do not jump, and a refresh does not shrink the page back. Tests: Task 12, `load_earlier_grows_the_channel_page_by_one_hundred` (two presses in flight) and `earlier_rows_put_in_front_keep_the_reader_in_place` (an arrival in the same frame).
4. **A forged `[dilla-music:v1:…]` line.** A line posted by a member (not the kind-1 Bot account), a post flagged `bot` whose sender is not kind 1, or an edited post must stay plain text with no card. Test: Task 35, `a_forged_music_marker_stays_text`.
5. **A late Welcome answer after Reload.** After an uncertain save, Reload must not discard the request id that Retry resends, and the save's late answer must not overwrite the newer read. Test: Task 38, `a_late_save_answer_does_not_overwrite_a_newer_read`.

(Command-memory reconcile, the other candidate, is already pinned by Task 30's `an_uncertain_attempt_keeps_its_request_and_reopens` and `timeout_attempts_bind_their_request`.)

## Execution order

Draft ids map to task numbers as follows. Composer is C, shell S, Bot B, threads/search/polls X, visual V, and the voice
teardown Z.

| Task | Id | Size | Task | Id | Size | Task | Id | Size |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | V1 | S | 16 | X2 | M | 31 | X5 | L |
| 2 | V8 | S | 17 | X1 | S | 32 | V3 | M |
| 3 | Z1 | M | 18 | V2 | M | 33 | V6 | M |
| 4 | B1 | S | 19 | V4 | S | 34 | V16 | L |
| 5 | B4 | S | 20 | V7 | M | 35 | B2 | L |
| 6 | V10 | S | 21 | V9 | M | 36 | B3 | M |
| 7 | V11 | M | 22 | V12 | S | 37 | S5 | S |
| 8 | C1 | M | 23 | V14 | M | 38 | B5 | L |
| 9 | C2 | M | 24 | C3 | L | 39 | X4 | S |
| 10 | C4 | L | 25 | C6 | M | 40 | V13 | M |
| 11 | C5 | M | 26 | C7 | M | 41 | X6 | M |
| 12 | S1 | L | 27 | X3 | M | 42 | V15 | L |
| 13 | S2 | M | 28 | C8 | L | 43 | — (docs) | S |
| 14 | S3 | M | 29 | C9 | M | | | |
| 15 | S4 | M | 30 | C10 | L | | | |

Hard dependencies:
- Composer: C1 → C2–C9 → C10 → X5 (8 → 9–11, 24–29 → 30 → 31). C6 needs C3, because its "every built-in has a handler" test
  covers C3's ids. C9 needs V8's checkbox (D20).
- Shell: S1 → X2 (12 → 16).
- Bot: B1 → B2 → X4 (4 → 35 → 39).
- Polls: X3 → X4 → X6 (27 → 39 → 41). X6 also needs V13's poll seeding (40 → 41).
- V13 (40) needs X3, X4, C7 (its fixtures) and B5's Bot world extension (38). V15 (42) needs C2 and C6.
- Every `native/` reference needs Task 1. Tasks 18 and 29 need Task 2.
- Z1 (3) is independent.

Parallel work: lanes run in separate copies of the worktree and land on the branch in order.
- Within a lane, tasks run in sequence.
- Lanes in the same wave share no source file.
- The only shared files are `clients/desktop/parity/references/linux/README.md` (table rows) and `clients/desktop/BEHAVIOUR-GATES.md`
  (appended test names). Both are append-only, and a rebase keeps both sides.

**Wave 0. Foundations: all seven may run in parallel.**
- 1 (V1, parity crate `screen.rs`, `tests/quit.rs`)
- 2 (V8, U `check_list.rs`)
- 3 (Z1, `webrtc-sys`, `dilla-media`, `D/screens/voice/session.rs`)
- 4 (B1, C `state/bot_admin.rs`, `features/music.rs`, `features/bot.rs`)
- 5 (B4, C `features/bot_admin.rs`, C `tests/bot_admin.rs`)
- 6 (V10, notify fixtures and `D/fixture/notify.rs`)
- 7 (V11, the attachments crop, parity `diff.rs`/`tolerance.toml`)

**Wave 1. Three lanes in parallel.**
- Lane A, commands: 8 → 9 → 10 → 11. Files: C `features/bot.rs`, `features/moderation.rs`, D `screens/commands/*`,
  D `screens/composer.rs`, `D/../tests/bot.rs`.
- Lane B, shell: 12 → 13 → 14 → 15 → 16 → 17. Files: C `client.rs`, `state/*`, `features/server_admin.rs`, `features/signup.rs`,
  `testing/spec.rs`; D `screens/timeline.rs`, `shell.rs`, `channel_list.rs`, `messages/ui.rs`, `server_settings/*`,
  `members/mod.rs`, `search/*`, `diagnostics/*`, `slots.rs`, `threads/view.rs`; U `threads/pane.rs`; the strings.
- Lane C, visual: 18 → 19 → 20 → 21 → 22 → 23. Files: D `members/self_roles.rs`, `user_settings/*`, `messages/editor.rs`,
  `messages/markdown.rs`, `sign_in.rs`, `rail.rs`, `dms/*`, `automation.rs`; U `segmented.rs`, `threads/reply_line.rs`,
  `onboarding/*`, `voice/*`; their test files and fixtures.

**Wave 2. The commands spine, with a visual lane beside it.**
- Spine (in sequence): 24 → 25 → 26 → 27 → 28 → 29 → 30 → 31. Task 24 touches `shell.rs`, `voice/mod.rs`, `members/mod.rs`,
  `search/mod.rs`, `dms/mod.rs` and `slots.rs`, so it waits for all of wave 1.
- Visual lane (in sequence, beside the spine): 32 → 33 → 34.
  - Task 32 (V3) touches `timeline.rs` and the strings.
  - Task 33 (V6) touches `server_settings/body/draw.rs`, `music_page.rs` and U `permission_grid.rs`, `key_hint.rs`,
    `sensitivity_meter.rs`, `select.rs` and `text_field.rs`. The spine only consumes `Select` and `TextField`.
  - Task 34 (V16) touches C `testing/spec.rs`, the parity world seed, `D/fixture/*`, `automation.rs` and `shoot.mjs`.
  - None of these files is in the spine.

**Wave 3. The Bot and polls spine, with V15 beside it.**
- Spine: 35 → 36 → 37 → 38 → 39 → 40 → 41.
- Task 42 (V15) may overlap Tasks 35–39. It adds fixtures and `parity_*` tests in `tests/messages.rs`, `bot.rs`, `dms.rs` and
  `toasts.rs`. It must land before Task 40, because both edit `D/../tests/bot.rs`.
- A fix that V15 finds in a file the spine holds waits as a commit after the spine task that holds that file.

**Close.** Task 43 (docs, release-notes inputs) runs after everything above has merged into the branch.

---

### Task 1: A shared GPUI-only screen check (`assert_native_screen`) [V1]

**Size:** S

The quit tests carry their own bless, measure and compare code (D/../tests/quit.rs:71-104). Tasks 13, 17,
18, 23, 28, 29, 35 and 36 all need the same thing for a fixture. Make it one function.

**Files:** Modify P/src/screen.rs (next to `assert_screen`, :129) · Modify D/../tests/quit.rs:71-104 to call it ·
Modify P/references/linux/README.md "GPUI-only references".
**Interfaces:** Produces `pub fn assert_native_screen(fixture: &Path, golden: &str, prepare: impl FnOnce(&mut Driver)) -> Result<ScreenReport, ParityFailure>`.
It launches the fixture with no sidecar and runs `prepare` (steps that have no web counterpart). Then it captures
like `assert_screen` and reads `references/linux/native/<golden>-2x.png`. `DILLA_PARITY_BLESS` writes the golden and
returns `Ok`. `MEASURE_VAR` prints `measure_line("native/<golden>", ..)`. Otherwise it runs
`assert_parity_masked(.., Os::current(), &mask)`.
- [ ] Step 1: Failing test `P/tests/harness.rs::native_screen_blesses_then_compares`. Bless `shell/channel-view` into a
      temp references root (env `DILLA_PARITY_NATIVE_ROOT`), then compare with it: assert `Ok`. Then compare with a
      golden shifted 1px down: assert `Err(ParityFailure::Chrome{..})`.
- [ ] Step 2: `$ENV cargo test -p dilla-parity --test harness native_screen` fails to compile.
- [ ] Step 3: Implement it. Port `shoot_quit` onto it, with `prepare` sending `quit-dialog {state}`.
- [ ] Step 4: `$ENV cargo test -p dilla-parity --test harness native_screen` and `$ENV cargo test -p dilla-desktop --test quit parity_` pass with the goldens unchanged.
- [ ] Step 5: README: say that a `native/` reference is made with `assert_native_screen` and blessed by eye. No fixture.
- [ ] Step 6: Commit `parity: one GPUI-only screen check for every native reference`.

### Task 2: The web's 22px checkbox, shared by every CheckList [V8]

**Size:** S

**Files:** U/components/check_list.rs:115-146 · Test U/components/check_list.rs `mod tests`; D/../tests/create.rs (parity).
**Interfaces:** Produces `pub fn check_box(id: impl Into<ElementId>, checked: bool, disabled: bool) -> Stateful<Div>` in
U/components/check_list.rs: the 22px box alone, which `CheckList` rows draw and Task 29 (the role-panel editor, D20) and Task 18 use.
- [ ] Step 1: Failing test `check_list::tests::box_is_22px`. The drawn box is 22×22, centred in the 24px hit area
      (`.d-check-list__box` is 1.5rem; Chromium paints the visible box at about 22px inside it; take the exact edge from layout.json). Unchecked: fill `#3B3B3B`-class grey, taken from the reference pixel at the
      box centre. Checked: `color::ACCENT` with a 16px `IconName::Check` in `ACCENT_INK`.
- [ ] Step 2: `$ENV cargo test -p dilla-desktop-ui --lib check_list` fails.
- [ ] Step 3: Draw it in `check_box`, and make `CheckList` rows call it. Take the colours from `create/channel-private-2x.png` at the two box centres.
- [ ] Step 4: Passes. `$ENV cargo test -p dilla-desktop --test create parity_create_channel_private` passes. Re-run the
      members parity tests (the roles dialog uses CheckList).
- [ ] Step 5: No new fixture; the existing create/channel-private and member-roles references now pass.
- [ ] Step 6: Commit `desktop: check boxes are the web's 22px boxes everywhere`.

### Task 3: PeerConnections outlive their call [Z1]

**Size:** M (investigation and fix)

Commit 6dbe6c202 made exit safe while a PeerConnection is still alive: the hook and the registry in
`core/vendor/webrtc-sys/src/dilla_transform.cpp` (`registry()`, `live_cores()`) are never destroyed. It did not explain why
PeerConnections were alive at all. After all 10 `#[ignore]`d tests in `D/../tests/voice.rs` had left their calls, 52 Dilla frame
transformers were still registered at exit. That is one transformer per transceiver, and the PeerConnections hold them. A call that
ended must release its PeerConnections. Otherwise every call of a long session keeps its sockets, threads, decoders and transformers
until the process exits.

**Files:**
- Modify: `core/vendor/webrtc-sys/src/dilla_transform.cpp`. Add `size_t dilla_live_transformer_count()`, the number of
  `registry().cores` entries whose `weak_ptr` still locks, counted under the registry mutex. Declare it in the header the bridge
  includes.
- Modify: `core/vendor/webrtc-sys/src/dilla_transform.rs`. Add the declaration to the `unsafe extern "C++"` block, and add
  `pub fn live_transformers() -> usize`.
- Modify: `clients/desktop/dilla-media/src/ffi.rs`. Add `pub fn live_transformers() -> usize`, a re-export next to `drain` (:30).
- Investigate, then modify whichever of these holds the room. The candidates:
  - `clients/desktop/dilla-media/src/room.rs`: `Call::leave` :1540, `Drop for Call` :1555, `EndOnDrop` :673, the spawned
    tasks, `router`, `media`.
  - `D/screens/voice/session.rs`: who keeps the `Call`, or a clone of its room or tracks, after a leave.
- Test: `clients/desktop/dilla-media/tests/call.rs` (next to `seat_both`, :409) and `D/../tests/voice.rs`.

**Interfaces:**
- Produces: `webrtc_sys::dilla_transform::live_transformers() -> usize` and `dilla_media::ffi::live_transformers() -> usize`.
  Each is a snapshot count, for tests and diagnostics only.

- [ ] **Step 1: Write the failing tests.**
  - dilla-media `tests/call.rs`: `#[ignore = "needs dillad with its SFU: target/dilla-testhost and the wasi core"]`
    `leaving_a_call_releases_every_frame_transformer`. Take the lock with `serial()`, then seat two people with `seat_both`.
    - `assert!(live_transformers() > 0)`.
    - Both calls `leave().await`.
    - Poll every 50 ms for at most 5 s until `live_transformers() == 0`. On a timeout, fail with the count.
  - The same file: `a_dropped_call_releases_every_frame_transformer`. The same set-up, but both calls are dropped without
    `leave`. Expect the count to reach 0 within 5 s.
  - `D/../tests/voice.rs`: an `#[ignore]`d test with the same reason as the file's other live tests,
    `leaving_from_the_dock_releases_the_call_media`. Use the harness of the file's existing live join and leave test.
    - Join, then click the dock's leave button.
    - `wait_for` `dilla_media::ffi::live_transformers() == 0` within 5 s.
- [ ] **Step 2: Run them and expect them to fail.**
  - `$ENV cargo test -p dilla-media --test call releases_every_frame_transformer -- --ignored`
  - `$ENV cargo test -p dilla-desktop --test voice leaving_from_the_dock -- --ignored`

  Expected: FAIL with a count above 0. Write the counts down for the commit body.
- [ ] **Step 3: Find the owner (superpowers:systematic-debugging).** After each teardown stage (`stop_shares`, `media.close`,
  the task abort, `room.close().await`, `router.end`, the `Call` drop, the D session's drop of its call state), log
  `live_transformers()` and the `Arc::strong_count` of the room handle. Check these hypotheses in this order:
  - (a) `Drop for Call` never closes the room. It relies on the livekit `Room` being dropped, which needs every
    `Arc<Room>` holder gone.
  - (b) A spawned task or thread outside `self.tasks` holds the room, its event receiver or a track: `EdgeTracker` (:229),
    the link reader or the capture.
  - (c) The D voice session keeps the `Call`, or a clone of the room, tracks or media, after leaving.
  - (d) An `Arc` cycle: a callback registered on a track or on the room captures the room or the `CallMedia`.

  Name the cause in one sentence with the line it lives on.
- [ ] **Step 4: Fix it at the owner.** At call end, the teardown drops every strong reference to the room and its
  PeerConnections. If (a) is the cause, `Drop for Call` also closes the room: it spawns `room.close()` on the runtime that
  `leave` uses. Do not add a "close again" or a sleep workaround.
- [ ] **Step 5: Run and expect the tests to pass.** Run Step 2's commands, then:
  - `$ENV cargo test -p dilla-desktop --test voice -- --ignored` (all ten live tests)
  - `$ENV cargo test -p dilla-media --test hook_exit`
  - `$ENV cargo test -p dilla-media`

  Expected: PASS.
- [ ] **Step 6: Gates.** Add no BEHAVIOUR-GATES row (no e2e title covers it) and no fixture.
- [ ] **Step 7: Commit.** Message: `dilla-media: a call that ends drops its PeerConnections and their frame transformers`. The body
  holds the cause from Step 3 and the before and after counts.

### Task 4: Music marker parser, the room name, and watched channels (dilla-client) [B1]

**Size:** S

**Files:** Modify C state/bot_admin.rs (next to `MusicSnapshot`, ~l.600-680) · Modify C features/music.rs (`Local`, l.95-100; `impl Music`)
· Modify C features/bot.rs:813-825 (`music_hint`) and its test at :945-962 · Modify C state/bot_admin.rs `BotAdminState` (add field).
**Interfaces:**
- Produces `pub struct MusicMarker { pub voice_channel: ChannelId, pub requester: UserId, pub item: String }` and
  `pub fn music_marker(body: &str) -> Option<MusicMarker>`. It ports `MARKER` (W screens/voice/music.tsx:267-271), which is an optional `\n` and then
  `[dilla-music:v1:<32 lower hex>:<32 lower hex>:<32 lower hex>]` at the very end of the body. `item` is the third group as text.
- Produces `BotAdminState.music_watched: BTreeSet<ChannelId>`, `Music::set_watched(&self, channels: BTreeSet<ChannelId>)`. It publishes only
  when the set changed.
- Changes `music_hint(state, frame)`: it now returns `Some(channel)` for a kind-2 scope whose channel is this device's In call **or** is in
  `state.bot_admin.music_watched`.
- [ ] Step 1: Write the failing tests:
  - `state::bot_admin::tests::the_music_marker_is_the_final_line_only`: with `id="ab".repeat(16)` and
    `body=format!("Now playing\n[dilla-music:v1:{id}:{id}:{id}]")`, `music_marker(&body).unwrap().item == id`. It returns `None` for an upper-case id,
    for a trailing `\nx`, and for a 31-hex group. `music_marker(&format!("[dilla-music:v1:{id}:{id}:{id}]"))` is `Some` because `\n` is optional.
  - Extend `a_call_scope_hint_reads_the_player_of_this_devices_call`. With no call and `s.bot_admin.music_watched.insert(lounge)`,
    `music_hint(&s, &hint(2, lounge.0)) == Some(lounge)`, and `hint(1, lounge.0)` still gives `None`.
- [ ] Step 2: `CARGO_TARGET_DIR=$PWD/target CC=clang CXX=clang++ cargo test -p dilla-client music_marker a_call_scope_hint`. Expected result: the tests fail to compile.
- [ ] Step 3: Implement `music_marker` by hand (strip the suffix, split on `:`, check three 32-char lower-hex groups; no regex
      crate). Add the field. Implement `set_watched` as `inner.publish` guarded by an inequality check. Make the one-line change in `music_hint`.
- [ ] Step 4: Run the same command. Expected result: all pass.
- [ ] Step 5: None (no visible state).
- [ ] Step 6: Commit `dilla-client: read the Bot's music marker and follow hints for watched players`.

### Task 5: Welcome settings read/save/retry over the sealed Bot operations (dilla-client) [B4]

**Size:** S

**Already present:** `encode_welcome`/`decode_welcome` (C features/server_admin.rs:460-530, with tests), `WelcomeSettings` and
the `server_admin.welcome` slice (C state/server_admin.rs:185-203, never written), and `BotOperations::request`/`read` with
the 1→101 and 2→102 mapping and the revision placed first (C features/bot_admin.rs:262-340; test at :1353). What is missing is the
welcome-specific answer handling.
**Web rules (W screens/admin/WelcomeSettingsPanel.tsx):**
- A read is operation 1 with args `[5]` and no revision (:82, :96-100).
- A save is operation 2 with `[5, encode(saved)]` and the read revision (:82, :248-249).
- The answer is valid only if `status == 0` and the revision is `Some(r)` with `r <= i64::MAX` (`validRevision`, :45-46).
- Status 4 means conflict, and any other non-zero status means responseFailed (:126, :202).
- A read's data decodes to `Option<WelcomeSettings>` (:129). A save's data must decode to `None`, otherwise it is responseFailed (:137-138).

**Interfaces** (C features/bot_admin.rs, new section `// ---- welcome settings (operations 1 and 2) ----`):
```rust
pub const WELCOME_FEATURE: u64 = 5;
pub enum WelcomeAnswer { Read { config: Option<WelcomeSettings>, revision: u64 }, Saved { revision: u64 },
    Conflict, Failed, Uncertain { request_id: [u8; 16] } }
pub async fn read_welcome(ops: &BotOperations) -> Result<WelcomeAnswer, OpError>;
pub async fn save_welcome(ops: &BotOperations, value: &WelcomeSettings, revision: u64) -> Result<WelcomeAnswer, OpError>; // OpError::Invalid on encode failure
pub async fn retry_welcome(ops: &BotOperations, request_id: [u8; 16], operation: u64) -> Result<WelcomeAnswer, OpError>; // ops.read; None → Failed
impl BotAdmin { pub fn welcome(&self, community: CommunityId, op: WelcomeOp) -> oneshot::Receiver<Result<WelcomeAnswer, ClientError>> }
pub enum WelcomeOp { Read, Save { value: WelcomeSettings, revision: u64 }, Retry { request_id: [u8; 16], operation: u64 } }
```
`OpError::Uncertain` maps to `Ok(Uncertain)`, and `Refused` stays `Err`. `BotAdmin::welcome` publishes `server_admin.welcome[community]` on
`Read` and `Saved`.
- [ ] Step 1: Write the failing tests (bot_admin.rs inline, with `fake`/`ok`):
  - `a_welcome_read_decodes_the_document_and_its_revision`: the answer `ok(Some(7), encode_welcome(&cfg))` gives
    `Read { config: Some(cfg), revision: 7 }`, and `ok(Some(7), [0x80])` gives `config: None`.
  - `a_welcome_answer_without_a_revision_failed`: `ok(None, [0x80])` gives `Failed`.
  - `a_welcome_save_that_returns_data_failed`: `save_welcome` answered `ok(Some(8), encode_welcome(&cfg))` gives `Failed`, and
    `ok(Some(8), [0x80])` gives `Saved { revision: 8 }`. `sent[0].args[0] == BotArg::Uint(7)` (the revision passed).
  - `a_welcome_conflict_is_status_4`: a body of `"E_CONFLICT"` with status 409 gives `Conflict`, and `"E_BUSY"` gives `Failed`.
  - `an_unanswered_welcome_save_is_retried_with_its_request_id`: `fake` returns `Err(SendError::Timeout)` once and then `ok(Some(9), [0x80])`.
    The first call gives `Uncertain { request_id }`. `retry_welcome(&ops, request_id, 2)` gives `Saved { revision: 9 }`, and both sent commands carry the same `request_id`.
- [ ] Step 2: `cargo test -p dilla-client welcome`. Expected result: it fails to compile.
- [ ] Step 3: Implement. The answer→enum mapping is one shared `fn welcome_answer(op: u64, r: BotResult) -> WelcomeAnswer`.
- [ ] Step 4: `cargo test -p dilla-client welcome`. Expected result: it passes.
- [ ] Step 5: Add a real-Bot test, `dilla-client::tests/bot_admin::a_welcome_saved_in_settings_greets_a_new_member` (`#[ignore = "needs dillad
      with its Bot: …"]`, using the `bot_owner` pattern at tests/bot_admin.rs:182-240). It enables with `cap::WELCOME`, marks `general` as a Bot channel,
      reads (None), saves `"Welcome {member} to {server}!"`, and has a second account join by invite. It then waits for a Bot message in `general`
      containing that member's name. Point the BEHAVIOUR-GATES row "mark a channel, enable welcome, a new member is welcomed there" at
      this test, keeping the existing `welcome_settings_are_app_operations…` reference beside it.
- [ ] Step 6: Commit `dilla-client: read and save the Welcome settings through the Bot`.

### Task 6: notify/unread-badge-b: settle which frame is right [V10]

**Size:** S

GPUI shows design, weekly-triage and general bold with "1 new". The web reference shows none, plus an AFK row that
GPUI lacks. The fixture's `northern` server declares no `system` channel (C/testing/spec.rs:203), but dillad makes one
for every new server, so the web lists "AFK". GPUI's notify facet (D/fixture/notify.rs:14-47) counts each other
channel's messages as unread. That matches how the shooter works (see its module doc).
- [ ] Step 1: Add `{"id":"afk","kind":"voice","name":"AFK","system":true}` to `northern` (and to `midgard`, as the
      other fixtures do; copy shell/channel-view-b's form).
- [ ] Step 2: Re-shoot to scratch: `node clients/desktop/parity/shoot.mjs clients/desktop/parity/fixtures/notify/unread-badge-b.json --out target/shots/ub`.
- [ ] Step 3: Decide from the fresh shot.
      (a) Pills show: the committed reference was stale. Shoot without `--out` and commit the pair.
      (b) No pills: the web marks those channels read. Read shoot.mjs's visit step and W's unread reducer for why. If
      the shooter's visit reads them, change notify.rs to count only channels the browser never visited (`SeedPlan.visit`,
      spec.rs:308) and add the unit test `notify::tests::visited_channels_are_read`. If the web drops unread for
      messages sent while it was online but elsewhere, that is a web bug to report to Jonas, not to copy.
- [ ] Step 4: `$ENV cargo test -p dilla-desktop --test notify parity_notify_unread_badge` (both) passes.
- [ ] Step 5: Do the same check for `notify/unread-badge.json` (A), which passes today. Add the AFK channel if its world lacks it.
- [ ] Step 6: Commit `parity: unread-badge fixtures carry the servers' AFK channel; <outcome>`.

### Task 7: Picture and emoji screens: fix the crop, document the rest [V11]

**Size:** M

attachments/cards(-b): GPUI crops a cover image in whole texels (`Window::paint_image` sub-tile rounding, the reason in
tests/attachments.rs:82). Its bilinear upscale also differs from Skia's filter. reactions emoji-grid and many-wrapping:
the colour emoji bitmap is scaled otherwise, and a chip's width follows the emoji advance.
**Files:** the image-crop call in D/screens/attachments (cover crop) · P/tolerance.toml (new `[linux_screens."<fixture>"]`) ·
P/src/diff.rs:234-249 (`tolerance_for_screen` reads `linux_screens` as it reads `macos_screens`) · P/TOLERANCES.md.
- [ ] Step 1: Failing test `diff::tests::linux_screen_override_applies` (a toml string with `[linux_screens."a/b"] chrome_max_pct = 0.9`).
- [ ] Step 2: `$ENV cargo test -p dilla-parity --lib diff::tests::linux_screen` fails.
- [ ] Step 3: (a) Crop with fractional UVs. Draw the whole image into a clipped, `overflow_hidden` box offset by the
      cover offset, instead of a texel sub-tile. Re-measure cards and cards-b. (b) Emoji: measure each side's advance for
      the 12 grid emoji (`layout.json` widths vs GPUI `text-bounds`). If the advances match, the residue is raster only.
      (c) Add `linux_screens` and set each remaining screen's limit to 2× its honest score, as for macOS.
- [ ] Step 4: The step 1 test passes. Remove the ignores on `parity_attachments_cards(_b)` and
      `parity_reactions_emoji_grid`/`many_wrapping(_b)`, then they pass.
- [ ] Step 5: TOLERANCES.md gets a "Linux per-screen" table: screen, score before and after (a), limit, and the reason
      (raster resampling, invisible to the eye). Attach the trip crops' paths in the commit body.
- [ ] Step 6: Commit `parity: cover images crop with sub-texel UVs; picture and emoji screens get measured limits`.

### Task 8: Built-in command plumbing (plan with a channel, pending row, generic confirmation) [C1]

**Size:** M

The catch-all at C/features/bot.rs:224-229 answers every built-in it does not know with `UNAVAILABLE`. This task adds the
path the 27 stubbed built-ins take; Tasks 10, 11, 24, 25 [C3–C6] move ids off the catch-all onto it.

**Files:**
- Modify: C/features/bot.rs (`plan` :124, `Plan` :278, catch-all :224-229; new `plan_in`, `Builtin`, `Bot::finish`, `mutation_error_of`)
- Modify: D/screens/commands/mod.rs (`on_send` :315; `SendDecision` unchanged)
- Modify: D/screens/commands/confirm.rs (second kind of question beside `Confirming`)
- Modify: D/screens/commands/copy.rs (ConfirmDialog labels)
- Modify: D/screens/composer.rs:505 (pass its encoder to `on_send`)
- Test: C/features/bot.rs `mod tests` (:884); D/screens/commands/confirm.rs `mod tests`

**Interfaces:**
- Produces (C): `pub fn plan_in(state: &AppState, community: Option<CommunityId>, channel: Option<ChannelId>, draft: &str) -> Plan`; `plan(s, c, d)` becomes `plan_in(s, c, None, d)` (its 28 call sites stay).
- Produces (C): `Plan::Builtin { run: Builtin, confirm: Option<Ask> }`, `pub struct Ask { pub title: String, pub body: String }`, `pub enum Builtin` (variants added by Tasks 10, 11, 24, 25 [C3–C6]; this task adds none but the type).
- Produces (C): `pub fn exact_message<'a>(state: &'a AppState, channel: ChannelId, id: &str) -> Option<&'a TimelineItem>` — builtin.ts:62-64: `msg_id` hex == `id`, `state == Ok`, `seq.is_some()`, from `state.timelines[&channel].items`.
- Produces (C): `impl Bot { pub fn finish(&self, conversation: Conversation, row: u64, status: ResultStatus, text: String) }` (wraps `self.runs().patch(.., private::Patch { status: Some, text: Some, hold: None })`).
- Produces (C): `pub fn mutation_error_of(e: &ClientError) -> String` — `ClientError::Sync(code)` read as `mutation_error` reads an `ApiError` with that code and status 0; every other variant is the uncertain sentence without a code.
- Produces (D): `on_send(ctx, draft, encode: &dyn Fn(&str) -> String, window, cx) -> SendDecision`; `confirm::ask_builtin(window, AskingBuiltin, cx)` with `pub struct AskingBuiltin { pub ask: Ask, pub run: Builtin, pub row: u64, pub conversation: Conversation, pub client: Option<Rc<Client>> }`.
- Consumes (D, per kind of `Builtin`): `builtins::run(run, row, conversation, ctx, window, cx)` (D/screens/commands/builtins.rs, created here with an empty `match` that Tasks 10, 11, 24, 25 [C3–C6] fill).

- [ ] Step 1: Write the failing tests.
  C `plan_in_reads_the_channels_timeline`: a state with timeline `ch` holding one Ok item (`msg_id` M, `seq` 5) and one Pending item (`msg_id` P, `seq` None); `assert_eq!(exact_message(&s, ch, &M.hex()).map(|i| i.seq), Some(Some(5)))`; `assert!(exact_message(&s, ch, &P.hex()).is_none())`; `assert!(exact_message(&s, ch, "nothex").is_none())`.
  C `mutation_error_of_names_definitive_codes`: `ClientError::Sync("E_FORBIDDEN")` → `"Command was refused (E_FORBIDDEN)."`; `ClientError::Network(..)` → `"The command result is uncertain. Check the current server state before retrying."`.
  D (gpui::test) `a_cancelled_builtin_question_leaves_no_row`: with a fixture-less `Client` absent (`client: None`), `ask_builtin` then `answer_builtin(w, Some(false), cx)` → `is_open(w)` false and no `PrivateResult` remains (CommandHost.tsx:294: `cancel && !moderation` dismisses the row); `answer_builtin(w, None, cx)` likewise.
- [ ] Step 2: `$ENV cargo test -p dilla-client --lib features::bot::tests::plan_in_reads` → fails (no `plan_in`/`exact_message`); `$ENV cargo test -p dilla-desktop --lib screens::commands::confirm` → fails to compile.
- [ ] Step 3: Implement in C/features/bot.rs `plan_in` (the body of `plan` with `channel` threaded to the built-in arms), `exact_message`, `Plan::Builtin`, `Ask`, `Builtin`, `Bot::finish`, `mutation_error_of`. In D: `on_send` calls `plan_in(&ctx.state, ctx.community, Some(channel), draft)`; for `Plan::Builtin { run, confirm }` with a client it adds a `Pending` row with `copy::pending(id)` (CommandHost.tsx:236: non-moderation commands show their row from the start), then `confirm::ask_builtin` when `confirm` is `Some`, else `builtins::run`; without a client (a fixture) it still asks (so the dialog can be shot) and runs nothing. `confirm.rs` gets a second per-window `OpenBuiltin(Option<AskingBuiltin>)`, `answer_builtin(w, Option<bool>, cx)`: `Some(true)` → `builtins::run`; `Some(false)`/`None` → `client.bot().dismiss(conversation, row)`. Its overlay is U `ConfirmDialog` with `title`, `body`, confirm `copy::CONTINUE` ("Continue"), busy `copy::WORKING` ("Working…"), cancel `copy::CANCEL` ("Cancel") (CommandHost.tsx:418), abandoned like the moderation one when `ctx.channel` moved (`still_current`, confirm.rs:91) and cleared by `confirm::release`. The composer passes `&|t| self.encoded(t)`.
- [ ] Step 4: `$ENV cargo test -p dilla-client --lib features::bot` and `$ENV cargo test -p dilla-desktop --lib screens::commands` pass.
- [ ] Step 5: Parity: fixture `clients/desktop/parity/fixtures/commands/confirm-delete.json` (world of commands/reminders.json, Ada's own message `m1`; steps: type `/delete` into `composer`/`css=.d-composer__input`, `key tab`, `key tab` (first message suggestion, Task 9), `key enter`, `settle`) is shot after Task 10 lands (it needs `/delete`); this task only adds the dialog. No BEHAVIOUR-GATES row (no frozen title).
- [ ] Step 6: Commit `desktop: built-in commands get a channel, a pending row and the generic confirmation`.

### Task 9: Chip suggestions for messages, roles, voice channels, DM peers, and gated members [C2]

**Size:** M

Today a `Suggest::Provider` chip offers nothing (D/screens/commands/chips.rs:434), so `/reply`, `/pin`, `/role add`,
`/join`, `/selfrole add` cannot be filled from the picker, and the member list ignores the timeout/role gates.

**Files:**
- Modify: D/screens/commands/chips.rs (`eligible_members` :243, `offered_with` :373, popup that builds `members`)
- Modify: C/features/bot.rs (`moderation_gate` :383 → by action; new `assignable_role`)
- Modify: D/screens/commands/copy.rs (`YOUR_MESSAGE = "Your message"`, `VOICE_CHANNEL = "Voice channel"` — CommandHost.tsx:334, :375)
- Test: D/screens/commands/chips.rs `mod tests`; C/features/bot.rs `mod tests`

**Interfaces:**
- Produces (C): `pub enum GateAction { Kick, Ban, Timeout }`; `pub fn moderation_gate_for(state, community, action: GateAction) -> Option<ModerationGate<'_>>` (Timeout reads `access.moderate_members`, builtin.ts:329); `moderation_gate(s, c, kick)` delegates. `moderation_target_for(state, community, who: &str, action: GateAction) -> Option<Target>`.
- Produces (C): `pub fn assignable_role(state: &AppState, community: CommunityId, user: &str, role: &str, add: bool) -> Option<(RoleSummary, Target)>` — builtin.ts:384-393: access loaded and `manage_roles`; target and self members present; target ≠ self, ≠ owner; `everyone_id` known and role ≠ @everyone, `position != 0`; add requires the role not held, remove requires it held; `roles::assign_lock(role, &access, mine)` is `None` where `mine = roles::held_bits(..)` (roles.rs:629, :734).
- Produces (D): `pub fn provided_suggestions(state: &AppState, community: Option<CommunityId>, channel: Option<ChannelId>, command: &CommandDefinition, option: &CommandOption, query: &str, values: &Args) -> Vec<Suggestion>`; `offered_with` gains `provided: &[Suggestion]` used for `Suggest::Provider` (title `copy::SUG_SUGGESTIONS`).

- [ ] Step 1: Failing tests (D, chips.rs), each against a built `AppState` like `kick_and_ban_ask_first…` (C/features/bot.rs:1048):
  `message_chips_offer_the_last_twenty_matching_messages`: 25 Ok items "m0".."m24" plus one Pending → 20 suggestions, first label "m5", last "m24", values their `msg_id` hex, the own ones' `sub == "Your message"`; query "m2" → labels `["m2","m20",…,"m24"]` (CommandHost.tsx:333: filter, then the last 20, label first 100 chars).
  `join_offers_voice_channels_only`: one text, one voice mode 0, one voice mode 1 → one suggestion, `sub == "Voice channel"` (CommandHost.tsx:374).
  `role_add_offers_only_roles_assignable_to_the_picked_member`: values `user` = Bea → only the rank-safe role not held (CommandHost.tsx:366-370); no `user` value → empty.
  `selfrole_chips_offer_the_self_roles_read`: `state.roles.self_roles[c]` with two rows → both, by name (CommandHost.tsx:357-360).
  `dm_in_a_dm_offers_dm_peers`: community `None`, two pair DMs (kind 3) and one group (kind 4) → two peers, label the DM's `name` (CommandHost.tsx:350-355).
  `timeout_members_need_moderate_members`: access without `moderate_members` → `member_suggestions(.., "timeout", "")` empty; with it and owner → all but self and Bots (builtin.ts:329, CommandHost.tsx:347).
  C `assignable_role_follows_the_web_rules`: @everyone, a role at the caller's top (not owner), a role whose `allow` has a bit not held, self, owner → `None`; a lower role not held → `Some` for add, `None` for remove.
- [ ] Step 2: `$ENV cargo test -p dilla-desktop --lib screens::commands::chips` fails.
- [ ] Step 3: Implement `moderation_gate_for`, `moderation_target_for`, `assignable_role` in C; `eligible_members` gates `"timeout" | "untimeout" | "warn"` with `GateAction::Timeout` and `"role add" | "role remove"` with "some role is assignable" (CommandHost.tsx:346-349); `provided_suggestions` by `option.kind` (Message, Channel for `join`, Role for `selfrole *` from `state.roles.self_roles`, Role otherwise from `state.roles.roles_of(c)` filtered by `assignable_role` for `role add|remove`, User in a DM for `dm`). When a `selfrole` chip opens and `self_roles[c]` is absent, the popup asks once per chip: `client.roles().run(move |r| async move { r.load_self_roles(c).await })` and refreshes.
- [ ] Step 4: `$ENV cargo test -p dilla-desktop --lib screens::commands::chips` and `$ENV cargo test -p dilla-client --lib features::bot` pass.
- [ ] Step 5: No new visible state beyond the existing suggestions popup (same component, other rows); no gate row.
- [ ] Step 6: Commit `desktop: command chips suggest messages, roles, voice channels and DM peers as the web does`.

### Task 10: Message built-ins (`/reply /edit /delete /pin /unpin /topic`) [C4]

**Size:** L

builtin.ts:42-60 (`publish`), :116-152, :337-376 (`removeOtherMessage`), :414-423 (`pinMessage`).

**Files:** Modify C/features/bot.rs (variants, checks); D/screens/commands/builtins.rs (runs); Test C/features/bot.rs tests, D/tests/bot.rs (ignored).

**Interfaces:**
- `Builtin::Reply { to: MessageId, text: String }`, `Edit { msg: MessageId, text: String }`, `DeleteOwn { msg: MessageId }`, `RemoveOther { msg: MessageId, seq: u64, reason: String, excerpt: String }`, `Pin { msg: MessageId, on: bool }`, `Topic { channel: ChannelId, text: String }`.
- Consumes: `client.threads().reply(channel, to, text)` (C/features/threads.rs:302), `client.messages().edit/delete/pin` (C/features/messages.rs:114/:129/:171), `client.moderation().run(|mo| mo.remove_message(..))` then `resolve_removal(proof)` / `send_removal_evidence(..)` (C/features/moderation.rs:807/:944/:1033), `client.server_admin().update_channel(channel, None, Some(text), Parent::Keep, None)` (C/features/server_admin.rs:1324), `threads::excerpt` (D/screens/threads/mod.rs:44) for the excerpt, and the composer's encoder.
- Produces (C, bot.rs): `pub enum RemovalStep { Resolve([u8; 16]), SendEvidence([u8; 16]), Done { status: ResultStatus, text: String, uncertain: bool } }`
  and `pub fn removal_step(o: &RemovalOutcome, resolved: bool, evidence_sent: bool) -> RemovalStep`. These are the `/delete`-of-another
  sequence of builtin.ts:355-379 as data. The D run calls `remove_message` exactly once, then only follows the steps: `Resolve(p)` → `resolve_removal(p)`
  with `resolved = true`; `SendEvidence(p)` → one `send_removal_evidence(.., p, ..)` with `evidence_sent = true`, the outcome's `evidence` replaced by
  its answer; `Done` → the row. `uncertain: true` marks the web's `result('uncertain', …)` lines (Task 30 maps them to `Outcome::Uncertain`).

Rules (plan_in unless marked run):
- reply: `exact_message` none → "That message is not available in this conversation."; run: text blank → "Message cannot be empty."; `encode(text).len() > MAX_LEN (4000)` → "Message exceeds the 4000 byte limit."; Ok(id) → Success `Sent. Message {id hex} is in the conversation outbox.`; Err `Sync(code)` with code in [E_BAD_INPUT, E_FORBIDDEN, E_NOT_FOUND, E_ENVELOPE_LIMIT, E_NOT_READY, E_TRAY_NOT_READY, E_OFFLINE] → Failed `Message was not sent ({code}).`; any other Err → Failed "The send result is uncertain and this attempt cannot safely be retried. Check the conversation and outbox before starting another send." (builtin.ts:42-60).
- edit: not found, not own (`!item.own`) → "You can edit only your own loaded message."; run: encoded > 4000 → "Message exceeds the 4000 byte limit."; Ok → "Message edited."; Err → `mutation_error_of` (builtin.ts:130-137).
- delete: not found → "That message is not available in this conversation."; not own and (channel bits lack `MANAGE_MESSAGES` or reason blank) → "Deleting another person’s message requires Manage Messages and a reason."; confirm `Ask { title: "Delete this message?", body: own ? "This permanently deletes your message." : "Remove this message from everyone? Reason: {reason}" }` (builtin.ts:142); run (own): "Message deleted."; run (other): re-check bits → same refusal sentence; `remove_message` Err `E_BAD_INPUT|E_FORBIDDEN|E_NOT_FOUND` or 400/403/404 → "The message could not be removed with the current permissions or message state."; other Err → "Message removal outcome is uncertain. Check the conversation before retrying."; `Rejected` → "The message could not be removed."; `Unknown` → `resolve_removal(proof)`: Rejected → same; not Accepted → `Message removal is unresolved. Recovery proof: {proof hex}.`; evidence not Accepted after one `send_removal_evidence` → `Message removal was accepted; moderation evidence is {pending|rejected|unknown}. Recovery proof: {proof hex}.` (Unknown branch) / `Message removed; moderation evidence is {…}. Recovery proof: {…}.` (Accepted branch); all accepted → "Message removed and moderation evidence recorded." (builtin.ts:337-376). Excerpt is `threads::excerpt(body)` cut to 512 UTF-8 bytes (builtin.test.ts:204).
- pin/unpin: not found → "That message is not available in this channel."; in a community and channel bits lack `PIN_MESSAGES` (bit 3; DMs need none, builtin.ts:417) → "You cannot pin messages here."; `item.pinned == on` → Success "Message is already pinned." / "Message is already unpinned." without a send; run → "Message pinned." / "Message unpinned.".
- topic: DM or bits lack `MANAGE_CHANNELS` (bit 14) → "You cannot manage this channel."; run → "Channel topic updated." (builtin.ts:116-120).

- [ ] Step 1: Failing tests. C `message_builtins_check_before_running` with a timeline (own msg A pinned, Bea's msg B, pending P) and channel bits without manage/pin: `/edit message:B text:x` → "You can edit only your own loaded message."; `/delete message:B` → the Manage Messages sentence; `/delete message:A` → `Plan::Builtin { run: DeleteOwn{..}, confirm: Some(Ask { title: "Delete this message?", body: "This permanently deletes your message." }) }`; `/pin message:A` (community) → "You cannot pin messages here."; with bit 3 → `Plan::Private(Success, "Message is already pinned.", Builtin)`; `/reply message:P text:x` → "That message is not available in this conversation."; `/topic text:x` without bit 14 → "You cannot manage this channel."; with bits in a DM channel `/pin message:A` (not pinned there) → `Plan::Builtin { run: Pin{on:true}, .. }`. D ignored `message_commands_reply_edit_pin_and_delete` (dillad: Ada `/reply` to her message → a reply row in the timeline whose `reply` names it; `/edit` → body changed and `edited`; `/pin` → `pinned`; `/delete` + `command-confirm` click → item gone; each `bot-result-success`).
  C `removal_steps_never_send_a_second_removal` (Review Focus 1), with proof `P = [7; 16]`:
  `removal_step(&outcome(Unknown, P, Pending), false, false) == RemovalStep::Resolve(P)`;
  `removal_step(&outcome(Unknown, P, Pending), true, false) == Done { status: Failed, text: format!("Message removal is unresolved. Recovery proof: {}.", hex(P)), uncertain: true }`;
  `removal_step(&outcome(Rejected, P, Pending), b, false)` for both `b` → `Done { Failed, "The message could not be removed.", uncertain: false }`;
  `removal_step(&outcome(Accepted, P, Pending), false, false) == SendEvidence(P)`;
  `removal_step(&outcome(Accepted, P, Pending), false, true)` → `Done { Failed, "Message removed; moderation evidence is pending. Recovery proof: {P hex}.", uncertain: true }`;
  `removal_step(&outcome(Accepted, P, Rejected), true, true)` → text starts `"Message removal was accepted; moderation evidence is rejected."`;
  `removal_step(&outcome(Accepted, P, Accepted), _, _)` → `Done { Success, "Message removed and moderation evidence recorded.", uncertain: false }`.
  No input yields a step that removes again (`RemovalStep` has no such variant: the type is the proof).
- [ ] Step 2: `$ENV cargo test -p dilla-client --lib features::bot::tests::message_builtins features::bot::tests::removal_steps` fails.
- [ ] Step 3: Implement the checks in `plan_in`, `removal_step`, and the runs in `builtins::run`. `on_send` encodes `Reply`/`Edit` text with
      its `encode` before it calls `builtins::run`, so the `Builtin` carries the encoded text and the 4000-byte check reads it (the run signature
      stays Task 8's). Remove the six ids from the catch-all.
- [ ] Step 4: the two C/D test commands of Task 8 Step 4, and `$ENV cargo test -p dilla-desktop --test bot message_commands_reply_edit_pin_and_delete -- --ignored`.
- [ ] Step 5: Shoot `commands/confirm-delete.json` (Task 8 Step 5): `node clients/desktop/parity/shoot.mjs clients/desktop/parity/fixtures/commands/confirm-delete.json`, add `parity_commands_confirm_delete` in D/tests/bot.rs beside `parity_commands_reminders` (:68). Append `dilla-desktop::tests/bot::message_commands_reply_edit_pin_and_delete` to the BEHAVIOUR-GATES row "composer keeps private results private, publishes only explicit text…" (it publishes only explicit text).
- [ ] Step 6: Commit `desktop: /reply /edit /delete /pin /unpin /topic run from the composer`.

### Task 11: Moderation built-ins (`/timeout /untimeout /warn /unban`) [C5]

**Size:** M

builtin.ts:183-261, :315-333.

**Files:** Modify C/features/bot.rs; C/features/moderation.rs (`timeout_for`); D/screens/commands/builtins.rs; Test C/features/bot.rs tests, C/features/moderation.rs tests, D/tests/bot.rs (ignored).

**Interfaces:**
- C: `pub async fn timeout_for(&self, community, user, request: [u8; 16], expires: Option<u64>) -> Result<(), ApiError>` in moderation.rs — the body of `timeout` (:403) with the caller's request id and absolute expiry (`None` clears); `timeout(c, u, length)` calls `timeout_for(c, u, request_id(), length.seconds().map(|s| now_s() + s))`. Task 11 passes `request_id()` and `now_s() + seconds`; Task 30 passes the remembered attempt's (builtin.ts:236-240).
- `Builtin::Timeout { community, user: UserId, name: String, seconds: u64 }`, `Untimeout { community, user, name }`, `Warn { community, user, name, reason: String }`, `Unban { community, user, name }`.

Rules:
- timeout: no target by `moderation_target_for(.., Timeout)` → "This member cannot be timed out with your current permissions and rank."; `run::duration_seconds(duration)` outside 60..=2_419_200 → "Timeout duration must be from 1m to 28d."; confirm `Ask { title: "Timeout {name}?", body: "Restrict this member for {duration as typed}." }`; run: target again → else "Your permissions or the member’s rank changed."; Ok → `{name} was timed out.`; Err → `mutation_error`.
- untimeout: same refusal sentence; run first `timeout_of` (C/features/moderation.rs:369): `None` → Failed `{name} is not timed out.` and no question; else the question `Ask { title: "Remove {name}’s timeout?", body: "This restores the member’s ability to send messages." }` (D asks from the run's answer: `builtins::run` for `Untimeout` keeps the pending row, awaits `timeout_of`, then `confirm::ask_builtin` with a `Builtin::UntimeoutConfirmed`); confirmed → re-check target, `timeout_for(.., request_id(), None)`, `{name}’s timeout was removed.` (builtin.ts:245-254).
- warn: target refusal "This member cannot be warned with your current permissions and rank."; reason blank or `reason_problem` (bot.rs:448) → "Enter a reason of at most 512 UTF-8 bytes without control characters."; confirm `Ask { title: "Warn {name}?", body: reason }`; run: re-check, `warn(c, u, roles::request_id(), &reason)` → `{name} was warned.` (builtin.ts:255-264).
- unban: access not loaded or no `ban_members` → "You cannot unban members here."; no row in `state.people.bans[c]` with that user → "That member is not currently banned." (the chip already read the bans, Task 9/chips.rs:273); confirm `Ask { title: "Unban {ban name}?", body: "This restores the member’s ability to join the community." }`; run: `ban_members` again → else "Your permissions or conversation changed."; `load_bans` again: gone → "That member is no longer banned."; `unban` → `{name} was unbanned.` (builtin.ts:168-182).

- [ ] Step 1: Failing tests. C `moderation_builtins_check_before_running`: no `moderate_members` → the timeout sentence; with it, `/timeout user:<bea> duration:30s` → "Timeout duration must be from 1m to 28d."; `duration:10m` → `Plan::Builtin { run: Timeout { seconds: 600, name: "bea", .. }, confirm: Some(Ask { title: "Timeout bea?", body: "Restrict this member for 10m." }) }`; `/warn user:<bea> reason:"a\u{1}b"` → the reason sentence; `/unban user:<bea>` with no bans read → "That member is not currently banned."; with `ban_members` false → "You cannot unban members here.". C moderation.rs `timeout_for_none_clears` mirrors the existing timeout test with `expires: None` → `DeleteMemberTimeout` route, and `timeout_for_sends_the_given_request_and_expiry` (`Some(E)` → the body is `[request, E]`). D ignored `timeout_and_warn_commands_apply_after_confirmation` (dillad: `/timeout` Bea 10m confirm → `bea`'s `timeout_of` is `Some`; `/untimeout` → `None`; `/warn` → row `bot-result-success`).
- [ ] Step 2: `$ENV cargo test -p dilla-client --lib features::bot::tests::moderation_builtins features::moderation::tests::timeout_for` fails to compile (no `timeout_for`, no variants).
- [ ] Step 3: Implement; remove the four ids from the catch-all.
- [ ] Step 4: the C/D test commands; `$ENV cargo test -p dilla-desktop --test bot timeout_and_warn_commands_apply_after_confirmation -- --ignored`.
- [ ] Step 5: Append `dilla-desktop::tests/bot::timeout_and_warn_commands_apply_after_confirmation` to the BEHAVIOUR-GATES row "moderation requires confirmation and applies to a distinct joined member". The question is the Task 8 ConfirmDialog (no new state).
- [ ] Step 6: Commit `desktop: /timeout /untimeout /warn /unban run after their confirmation`.

### Task 12: Load earlier messages in channels and DMs [S1]

**Size:** L

Web spec:
- The worker keeps a per-open limit, starting at `TIMELINE_PAGE` (100), and `loadEarlier` adds 100 and re-reads the newest `limit` rows (client-core worker/controller.ts:4395, 4450-4457, 4483-4489). Opening a channel resets the limit to 100 (controller.ts:4395).
- The log shows a ghost button `shell.log.earlier` ("load earlier") above the first row while `hasEarlier` (Conversation.tsx:396). It shows the welcome only when `!hasEarlier` (Conversation.tsx:399). While the load runs, the log is `aria-busy` (Conversation.tsx:397, Shell.tsx:1029-1042).
- Prepended rows keep the reader's place: when the first row changes and the log is not pinned, `scrollTop += scrollHeight - prevHeight` (packages/ui MessageLog.tsx:103-114). The button sits centred with `margin-bottom: var(--group-gap)` (MessageLog.css:9).
- A reply chip whose original is not loaded keeps the existing `shell.message.replyNotLoaded` notice and does not load by itself (Conversation.tsx:232). Task 12 makes the notice's advice actionable.

**Files:**
- Modify C/client.rs:
  - `Publisher` (1261-1290): add `limits: HashMap<ChannelId, u32>`.
  - `enum Update` (95-131): add `LoadEarlier(ChannelId, oneshot::Sender<Result<(), ClientError>>)`.
  - The `Update::Focus` arm (1477-1483).
  - `read_timeline` (1958-1984).
  - Add `Client::load_earlier` beside `open_channel` (641).
- Modify D/screens/timeline.rs:
  - `render` (1050-1260): the earlier button, the welcome rule, the anchoring.
  - The module doc (lines 9-10, "at most the client's 100-row page").
- Modify D/screens/messages/ui.rs: keep `prev_first: Option<String>`, `prev_height: f32` and `loading_earlier: Option<ChannelId>` on the messages UI entity.
- Modify D/screens/shell.rs (1440-1464): pass an `on_earlier` callback into `timeline::render`.
- Test: C tests/messages.rs, and inline tests in D/screens/timeline.rs.

**Interfaces:**
- Produces:
  - `pub fn load_earlier(&self, channel: ChannelId) -> oneshot::Receiver<Result<(), ClientError>>` on `Client`.
  - `pub fn log_head(t: Option<&Timeline>) -> LogHead` with `pub enum LogHead { Loading, Earlier, Welcome }` in timeline.rs.
  - `pub fn anchored_offset(prev_first: Option<&str>, first: Option<&str>, prev_height: f32, height: f32, offset: f32, pinned: bool) -> f32` in timeline.rs.
- Consumes: `TIMELINE_PAGE` (C state/messages.rs:14), `build_timeline(.., limit)`, whose `has_earlier: rows.len() >= limit` is already right (messages.rs:402).
- `timeline::render` gains `on_earlier: Rc<dyn Fn(&mut App)>` after `on_wheel`.

- [ ] **Step 1: Write the failing tests.**
  - C tests/messages.rs, `#[ignore = "needs dillad: …"]`, same harness as `tests/threads.rs::two_people`: `load_earlier_grows_the_channel_page_by_one_hundred`. Alice sends 101 messages `m0..m100` to general.
    - `wait_for` a timeline with `items.len() == 100 && has_earlier`, with `items[0].body == "m1"`.
    - `alice.load_earlier(ch).await` → `Ok(Ok(()))`; then `wait_for` `items.len() == 101 && !has_earlier && items[0].body == "m0"`.
    - Alice sends `m101`: `items.len() == 102`. The page is now 200, and a refresh must not shrink it.
    - Review Focus 3: `let (a, b) = (alice.load_earlier(ch), alice.load_earlier(ch));` (two presses before the first read lands, as the web
      allows, Shell.tsx:1029-1042): both answer `Ok(Ok(()))`, and `items.len() == 102 && !has_earlier` (no row lost or doubled).
    - `alice.focus(Some(other))` then `alice.focus(Some(ch))`: `wait_for` `items.len() == 100 && has_earlier`. This is the reset on reopen (controller.ts:4395).
  - D timeline.rs inline `#[test] fn the_welcome_waits_for_the_start_of_the_history`:
    - `log_head(None) == LogHead::Loading`
    - `has_earlier: true` → `LogHead::Earlier`
    - `has_earlier: false` → `LogHead::Welcome`
  - D timeline.rs inline `#[test] fn earlier_rows_put_in_front_keep_the_reader_in_place`:
    - `anchored_offset(Some("k5"), Some("k0"), 1000.0, 1600.0, 40.0, false) == 640.0`
    - same first row: `anchored_offset(Some("k5"), Some("k5"), 1000.0, 1600.0, 40.0, false) == 40.0`
    - pinned: `anchored_offset(Some("k5"), Some("k0"), 1000.0, 1600.0, 40.0, true) == 40.0` (the pin path sets it)
    - first draw: `anchored_offset(None, Some("k0"), 0.0, 1600.0, 0.0, false) == 0.0`
    - Review Focus 3, a message arriving at the bottom in the same frame as the earlier page (MessageLog.tsx:103-114 adds the whole growth):
      `anchored_offset(Some("k5"), Some("k0"), 1000.0, 1640.0, 40.0, false) == 680.0`
- [ ] **Step 2: Run them, expect failure.**
  - `CARGO_TARGET_DIR=$PWD/target CC=clang CXX=clang++ cargo test -p dilla-desktop --lib timeline::` fails to compile (no `log_head`, no `anchored_offset`).
  - The client test fails to compile (no `load_earlier`).
- [ ] **Step 3: Implement the client side.**
  - `read_timeline` reads `let limit = self.limits.get(&channel).copied().unwrap_or(TIMELINE_PAGE);` and passes `limit` to both `c.timeline(&g, 0, limit)` and `build_timeline`.
  - The `Update::LoadEarlier(c, done)` arm: `*self.limits.entry(c).or_insert(TIMELINE_PAGE) += TIMELINE_PAGE`, then add the channel's group to the set that `refresh_channels` re-reads (as `Update::Focus` does with `groups.insert`). Answer `done` once that read is published.
  - The `Update::Focus(channel)` arm: when the focus changes to another channel, `self.limits.retain(|c, _| Some(*c) == channel)`. Opening a conversation starts at 100 again.
  - `Client::load_earlier` sends the update (shaped like `mark_read`, 651-664).
- [ ] **Step 4: Implement the desktop side.** In `timeline::render`:
  - **`LogHead::Earlier`:** place a `Button` (`ButtonVariant::Ghost`, id `"log-earlier"`, label `t("shell.log.earlier")`) centred above the first row, in the welcome's place. Its height plus `GROUP_GAP` replaces `welcome_height` in `content`. On click, call `on_earlier`.
  - **`LogHead::Welcome`:** keep today's welcome. **`LogHead::Loading`:** keep today's path.
  - **The shell's `on_earlier`:** sets `loading_earlier = Some(channel)` and spawns `client.load_earlier(channel)`, clearing it when the receiver answers. A failure goes through the shell's existing error toast path (`report` in the web, Shell.tsx:1036). The log region carries `aria-busy` while `loading_earlier == Some(channel.id)`.
  - **Anchoring:** before the `pinned` branch (timeline.rs:1252), when not pinned and not jumping, set the offset to `anchored_offset(prev_first, first_key, prev_height, inner, -offset.y, pinned)`. Then store `prev_first`/`prev_height` in the ui entity, as MessageLog.tsx:103-114 does.
- [ ] **Step 5: Run, expect pass.**
  - `CARGO_TARGET_DIR=$PWD/target CC=clang CXX=clang++ cargo test -p dilla-desktop --lib timeline::`
  - `CARGO_TARGET_DIR=$PWD/target CC=clang CXX=clang++ cargo test -p dilla-client --test messages load_earlier_grows -- --ignored`
- [ ] **Step 6: Parity and gates.**
  - Add the parity fixtures `clients/desktop/parity/fixtures/messages/log-earlier.json` and `log-earlier-b.json`. Copy `messages/mention.json`'s world, give it 101 messages in `midgard/general` from mira and bjorn, 30 s apart, and use the `-b` window size. The frame shows the button at the top after `{"do":"key","chord":"ctrl-home"}` or a scroll to the top. Shoot both with `shoot.mjs` and add the pair to the README table.
  - No BEHAVIOUR-GATES row: no e2e title covers "load earlier", and the table check rejects a title with no e2e source.
- [ ] **Step 7: Commit.** `desktop: load earlier messages in channels and DMs, keeping the reader's place`

### Task 13: Channel row menu: notify mode and Edit channel [S2]

**Size:** M

Web spec:
- A text channel's row menu holds pins, Mute (a check), then the four modes as single-choice items, then Edit channel for managers (Shell.tsx:1411).
  - The four modes are `signalSettings.menu.notifyDefault|All|Mentions|Nothing`, values `default|all|mentions|nothing` (Shell.tsx:122-126).
  - The mode reads `SETTINGS.notifyChannel(id)`, defaulting to `default` (Shell.tsx:1289-1292).
  - Edit channel is `admin.channel.open` (Shell.tsx:1411), shown when `canManageChannels = access?.loaded ? access.manageChannels : owner` (Shell.tsx:1245).
- A DM row's menu holds call?, pins, Mute, then the four modes; it has no edit (Shell.tsx:1388-1392).
- A mode choice writes the key, and `default` deletes it (Shell.tsx:1296-1302). `mode:` ids are handled before the other items (Shell.tsx:1413).
- `edit` opens ChannelEditFlow over the conversation (Shell.tsx:1415, 2321).
- The worker reads access on every community select (controller.ts:3289).
- The modes' `group` is accessible-only (packages/ui ActionMenu.tsx:191-195): no visible heading. GPUI `MenuItem` has no group, and the server menu (D channel_list.rs:131) leaves it out too, so the accessible group is left out here as well, consistent with the server menu.
- GPUI-only (Decision D6): a text channel's menu keeps "Mark as read" (`read`, `client.mark_read`) and "copy link" (`link`, `invite.copy`, the route path), after the web's items (after Edit channel when shown). A DM row's menu stays as the web's (it has neither today).

**Files:**
- Modify D/screens/channel_list.rs (116-146): make `NOTIFY_MODES` `pub`. Add `pub const ROW_MODE: &str = "mode:";` and `pub fn row_mode_items(settings: &dyn SettingsSource, channel: ChannelId) -> Vec<MenuItem>`.
- Modify D/screens/shell.rs:
  - `menu_items` (805-829) and `choose` (831-890).
  - `settle_selection`: the access read on a community change.
  - Delete the `MARK_READ` const and its doc (100-102); the `read` item's label becomes `t("shell.channels.markRead")`. Keep the `read` and `link` arms of `choose` (849-857, 879-887) as they are.
  - Add the `channel_edit: Option<Entity<server_settings::body::Body>>` field and its drawing.
- Modify D/screens/server_settings/body.rs:
  - Make `open_edit` (950) `pub`.
  - Add `pub fn dialog_open(&self) -> bool` (`!matches!(self.dialogue, Dialogue::None)`).
- Modify D/screens/server_settings/body/draw.rs: add `pub fn edit_dialogs(&mut self, window: &mut Window, cx: &mut Context<Self>) -> Vec<AnyElement>`, which returns `[self.edit_dialog(..), self.private_dialog(cx)]` (the two dialogs reachable from Edit; draw.rs:409-412).
- Modify packages/web/src/strings/en.ts: add `'shell.channels.markRead': 'Mark as read',` after `'shell.channels.rowLabelMuted'` (:381), under a comment in the style of the `readAloud.*` block (en.ts:208-209): `// Native desktop only (Jonas 2026-11-19): the channel row menu's "Mark as read"; the web's row menu has no such item.` The web has no key for it (packages/web/src/strings holds no "Mark as read"/`markRead`), and `desktopOverrides.ts` cannot add one (scripts/gen-gpui-strings.mjs:54 refuses a key missing from `en`), so a desktop-only key goes into the web's `en` table as `readAloud.*` and `desktopUpdate.ts` (spread at en.ts:63) did.
- Regenerate clients/desktop/dilla-desktop/src/strings.rs with `node scripts/gen-gpui-strings.mjs` (`--check` must then pass).
- Test: inline in D/screens/shell.rs tests (pattern: `folds_and_mutes_persist`, 2253-2292).

**Interfaces:**
- Consumes:
  - `notify::key_channel_mode(ChannelId)` and `NotifyMode::parse`.
  - `client.notify().set_channel_mode(channel, Option<NotifyMode>)` (C features/notify.rs:958, currently unused).
  - `server_admin::access_in(&state, community)`, `client.server_admin().load_access(community)` (C features/server_admin.rs:1089), and `Body::new(community, state, client, clock, window, cx)`.
- Produces: `fn can_manage_channels(state: &AppState, community: CommunityId) -> bool` in shell.rs: `access.loaded ? access.manage_channels : owner == me`.

- [ ] **Step 1: Write the failing test.** `#[gpui::test] fn the_row_menu_sets_the_channel_notify_mode_and_offers_edit_to_managers`. Use `world()` with the viewer as owner (as `folds_and_mutes_persist`) and channel `ChannelId([3; 16])`.
  ```rust
  let ids: Vec<_> = s.menu_items(ch).iter().map(|i| i.id.to_string()).collect();
  assert_eq!(ids, ["pins", "mute", "mode:default", "mode:all", "mode:mentions", "mode:nothing", "edit", "read", "link"]);
  assert_eq!(s.menu_items(ch)[7].label.as_ref(), t("shell.channels.markRead"));
  assert_eq!(t("shell.channels.markRead"), "Mark as read");
  assert_eq!(s.menu_items(ch)[2].checked, Some(true));
  s.choose(ch, "mode:mentions", window, cx);
  assert_eq!(prefs.get::<String>(&notify::key_channel_mode(ch)).as_deref(), Some("mentions"));
  assert_eq!(s.menu_items(ch)[4].checked, Some(true));
  s.choose(ch, "mode:default", window, cx);
  assert!(prefs.get::<String>(&notify::key_channel_mode(ch)).is_none());
  ```
  - Then a non-owner viewer with no access read: `["pins", "mute", "mode:default", "mode:all", "mode:mentions", "mode:nothing", "read", "link"]` (no `"edit"`).
  - The same viewer with `state.roles.access` loaded and `manage_channels`: `"edit"` is listed.
  - A DM's items: `["pins", "mute", "mode:default", "mode:all", "mode:mentions", "mode:nothing"]`. There is no call item in the fixture (no live call).
- [ ] **Step 2: Run it, expect failure.** `cargo test -p dilla-desktop --lib shell::tests::the_row_menu_sets` (same env) fails on the ids.
- [ ] **Step 3: Implement the menu.**
  - `row_mode_items` mirrors `server_menu_items` (channel_list.rs:131-146), reading `key_channel_mode`.
  - `menu_items` for a text channel: pins, mute, `row_mode_items`, edit (when `can_manage_channels`), then `read` (`t("shell.channels.markRead")`) and `link` (`t("invite.copy")`). A DM: call?, pins, mute, `row_mode_items`.
  - `choose`: a `mode:` id writes through `client.notify().set_channel_mode`. Without a client, write `prefs` directly, the same fallback the `mute` arm uses (shell.rs:858-875).
  - `edit`: `self.channel_edit = Some(cx.new(|cx| Body::new(community, state, client, clock, window, cx)))`, then `body.update(cx, |b, cx| b.open_edit(channel, window, cx))`.
  - Each render: draw `body.edit_dialogs(..)` over the shell, and call `body.sync(state, client, cx)` on a state change. Drop `channel_edit` once `!dialog_open()`.
  - `settle_selection`: when the community changes and `!access_in(..).loaded`, spawn `client.server_admin().load_access(community)` once per community. Its result is ignored; a failure leaves the owner fallback, as controller.ts:3287-3290 does.
- [ ] **Step 4: Run, expect pass.** Same command, plus `cargo test -p dilla-desktop --lib server_settings::`, `node scripts/gen-gpui-strings.mjs --check` and `node --test scripts/gen-gpui-strings.test.mjs`.
- [ ] **Step 5: Parity and gates.**
  - The open menu carries two rows the web's lacks (Decision D6), so a web pair would fail on them. Shoot the web frame once for looking, with the fixture `shell/row-menu.json` based on `shell/conversation.json` (steps: `{"do":"click","web":"css=.d-channel-list [data-channel-id] >> nth=0","gpui":"channel-row-0"}`, the context key `{"do":"key","chord":"shift-f10"}`, then settle; GPUI ids from `channel_list.rs` row ids). Commit the fixture, not the web frame as a reference.
  - Commit a GPUI-only reference `native/row-menu`: `tests/shell.rs::parity_shell_row_menu` calls `assert_native_screen` (Task 1) on that fixture (viewer the owner, so Edit channel shows): its first seven rows looked at by eye against the web frame (labels, ticks, spacing), the last two drawn as the same `MenuItem` rows. Bless it (`DILLA_PARITY_BLESS=1`) and add it to the README `native/` table with the note "the web's items, then Mark as read and copy link (desktop only, Jonas 2026-11-19)".
  - BEHAVIOUR-GATES: no new e2e title. The edit save itself stays held by `dilla-client::tests/server_settings::server_rename_channel_edit_leave_hand_over_delete`.
- [ ] **Step 6: Commit.** `desktop: a channel's notify mode and Edit channel in its row menu; Mark as read gets its string`

### Task 14: Settings polish: ban details and settings search access [S3]

**Size:** M

Web spec, bans (W screens/admin/BansSection.tsx):
- Each row's detail lines are, in order:
  1. `admin.bans.by {name, date: formatDay(created)}`, shown only once the members are read. The banner who is not a member is `admin.member.former` (lines 55-59, 65).
  2. `lastsLine`: `forever` / `lapsed {date: day}` when `expires <= now` / `until {date}`, with the day plus time when it ends within 2 days (`SOON_S = 172_800`, lines 11-19).
  3. The reason, or `admin.bans.noReason`.
- ManageList draws each line as its own `d-manage-list__detail` span (ManageList.tsx:46).
- A failed read shows an alert with `admin.bans.error.load {code}` and a `admin.bans.retry` button, which reads again (lines 38-42, 96-100).

Web spec, settings search: the server results are listed only when `access?.loaded === true`, using the real access (Shell.tsx:1702-1705). There is no owner stand-in.

**Files:**
- Modify D/screens/members/mod.rs:
  - `bans_section` (1073-1180).
  - `ensure_loaded` (231-270): a failed `"bans"` read keeps its `asked` entry and records `ui(w).bans_error.insert(community, code)`. Retry clears both.
- Modify D/screens/search/settings_index.rs (17-48): `Access::of` reads `server_admin::access_in`. Delete the `// T14` stand-in.
- Test: inline tests in D/screens/members/mod.rs and in D/screens/search/settings_index.rs (beside `server_sections_follow_the_access`, 564).

**Interfaces:**
- Produces in members/mod.rs: `pub fn ban_details(ban: &BanSummary, members: Option<&[Member]>, now: u64) -> Vec<String>`.
- Consumes: `crate::format::day(ts)` and `day_time(ts)` (D format.rs:64, 69), and `dilla_client::state::server_admin::access_in`.

- [ ] **Step 1: Write the failing tests.**
  - `#[test] fn a_ban_row_names_who_banned_and_when_it_ends`. With `ban = BanSummary { by_user: ada, created: T, expires: Some(T + 3600), reason: "" .. }`, `now = T + 60`:
    - `ban_details(&ban, Some(&[ada_member]), now) == [tf("admin.bans.by", [("name","Ada"),("date",&day(T))]), tf("admin.bans.until", [("date",&day_time(T+3600))]), t("admin.bans.noReason")]`
    - `members: None` → 2 lines, with no `by`.
    - Banner not a member → line 0 uses `t("admin.member.former")`.
    - `expires: Some(T + 3 * 86_400)` → `until` with `day(..)`, not `day_time`.
    - `expires: Some(now - 1)` → `tf("admin.bans.lapsed", [("date", &day(now - 1))])`.
    - `expires: None` → `t("admin.bans.forever")`.
  - `#[test] fn a_member_with_ban_rights_finds_the_bans_section`. A non-owner whose `state.roles.access` for the community is loaded with `ban_members` (and nothing else): `settings_index::search(true, "bans", &Access::of(&state, c))` finds `Section::Server(ServerSection::Bans)`; with `access` unloaded it finds nothing.
- [ ] **Step 2: Run, expect failure.** `cargo test -p dilla-desktop --lib members:: search::settings_index::`
- [ ] **Step 3: Implement.**
  - **Detail lines:** `bans_section` draws `ban_details(..)`, one `line(.., text::META, .., color::FG_3)` per entry, in place of the joined `"{reason} · {until}"` line (1152-1160). The row's `min_h(ROW_H_2)` grows with the third line.
  - **Load error:** when `bans_error` holds a code, draw `alert(tf("admin.bans.error.load", [("code", code)]))` and a `Button` `t("admin.bans.retry")`. Its click removes `bans_error` and `asked("bans", community)`, so the next draw reads once.
  - **Search access:** `Access::of` maps `access_in(state, community)` field by field (`loaded`, `manage_community`, `manage_channels`, `manage_roles`, `create_invite`, `ban_members`, `view_audit_log`). It is read by Task 13's community-select access load.
- [ ] **Step 4: Run, expect pass.** Same command, plus `cargo test -p dilla-desktop --test search`.
- [ ] **Step 5: Parity.**
  - Add `bans: Vec<BanSpec>` to `ServerSpec` (C testing/spec.rs:142), with `BanSpec { user: String, by: String, reason: String, #[serde(default)] hours: Option<u32> }`. Seed it in `examples/parity_world/seed/members.rs` through the moderation ban route, as `by`, after the members join.
  - Give `server-settings/bans.json` and `bans-b.json` one ban each: `bjorn` by `ada`, reason "spam", `hours: Some(24)` for the until-with-time line; in `-b`, `hours: None` with an empty reason.
  - Re-shoot both. Check that the sidecar's clock shift makes `created` fall on the fixture's day; if it does not, drop the `by` line from the fixture and say so in the README.
  - The lapsed line and the load error are unit-held only: a world cannot age a ban or fail the read.
- [ ] **Step 6: Commit.** `desktop: ban rows say who banned and when it ends; settings search uses the real access`

### Task 15: Banners: "You are no longer in {server}" and "instance is older than this client" [S4]

**Size:** M

Web spec, removal notice:
- A server listed before a community-list read and missing after it, which this account did not leave or delete itself, adds `{id: ++seq, communityId, name}` to `removals`, keeping only the newest `REMOVALS_MAX = 8` (controller.ts:3073-3100, 944).
- **The self-removed mark:**
  - Set before a leave or delete request (admin/controller.ts:197-214).
  - A 4xx other than 404 clears it. 404, 5xx and status 0 keep it.
  - It is cleared when the server is listed again with no leave or delete in flight (controller.ts:3087), and when the server is joined (controller.ts:3155-3165).
- Joining a server drops its notice (3162-3165). Sign-out empties the notices (4343).
- The banner is `Banner tone="warn"` with `removed.notice {server: name}` and the action `shell.banner.dismiss`. Dismissing hides that id for this page and focuses the composer (Shell.tsx:1861-1866).

Web spec, instance too old: a version refusal followed by `instanceOutdated` becomes `reason: 'instance'` (controller.ts:2438-2443) and shows `shell.banner.instanceTooOld`, warn, with no action (Shell.tsx:1857). A plain `version` reason keeps `clientTooOld` with Reload.

**Files:**
- Create C/state/removed.rs:
  - `RemovalNotice { id: u64, community: CommunityId, name: String }`
  - `pub const REMOVALS_MAX: usize = 8`
  - `SelfRemoved { removed: HashSet<CommunityId>, pending: HashSet<CommunityId> }`, with methods `begin(c)`, `end(c, status: Option<u16>)`, `listed(c)`, `joined(c)`, `contains(c) -> bool`
  - `pub fn notices(before: &HashMap<CommunityId, String>, now: &HashSet<CommunityId>, mine: &SelfRemoved, seq: &mut u64) -> Vec<RemovalNotice>`
- Modify C/state/mod.rs:
  - Add `pub removal_notices: Vec<RemovalNotice>` to `AppState`.
  - Change `AccountPhase::VersionRefused { upgrade: bool }` (124) to `VersionRefused { upgrade: bool, instance: bool }`.
- Modify C/client.rs:
  - `listed` (1276) becomes `Option<HashMap<CommunityId, String>>`, and its use at 1839 changes to match.
  - Add `self_removed: Mutex<SelfRemoved>` on `Inner`.
  - Clear the notices on sign-out.
  - In the refusal arms (1641-1653), `Refusal::InstanceOutdated` sets `instance: true`; `VersionUnsupported` sets `instance: false`; `UpgradeRequired` sets `instance: false`.
- Modify C/features/server_admin.rs `leave`/`delete_community` (1230-1248): wrap them in `begin`/`end`.
- Modify C/features/signup.rs `join` (1081): `joined(c)`, then drop that server's notice.
- Modify D/screens/diagnostics/banner.rs: add `BannerPlan::InstanceTooOld`, and update the module doc (12-14).
- Modify D/slots.rs: `banners` (286) adds the removal notices. Add `composer: Option<FocusHandle>` to `SlotCx`, filled by the shell from `composer_focus` (D shell.rs:205).
- Update the matches on `VersionRefused`: D tests/diagnostics.rs:604-628 and C testing/spec.rs.

**Interfaces:**
- Produces: `pub fn removal_banners(state: &AppState, dismissed: &HashSet<u64>) -> Vec<(u64, String)>` (id, text) in a new D/screens/diagnostics/removed.rs. The dismissed ids live in a per-window `HashMap<WindowId, HashSet<u64>>` global.

- [ ] **Step 1: Write the failing tests.**
  - C state/removed.rs inline, `#[test] fn a_server_gone_from_the_list_is_told_unless_this_account_left_it`:
    ```rust
    let mut seq = 0;
    let before = HashMap::from([(a, "Midgard".into()), (b, "Asgard".into())]);
    let mut mine = SelfRemoved::default();
    mine.begin(b);
    let n = notices(&before, &HashSet::new(), &mine, &mut seq);
    assert_eq!(n, [RemovalNotice { id: 1, community: a, name: "Midgard".into() }]);
    ```
    - `mine.end(b, Some(409)); assert!(!mine.contains(b));`
    - `mine.begin(b); mine.end(b, Some(502)); assert!(mine.contains(b));`
    - `mine.end(b, Some(404))` keeps it.
    - `mine.listed(b)` with nothing pending clears it.
  - C tests/members.rs `kicked_client_told_loses_server_readmitted_flows` (1462), extended:
    - After "ada loses the server", `wait_for` `s.removal_notices.len() == 1 && s.removal_notices[0].community == cid && s.removal_notices[0].name == <server name>`.
    - After `listed(&ada, cid, 2)` again, `wait_for` `s.removal_notices.is_empty()`. The join here happens over HttpDs, so the clear comes from the list reading the server again. Implement that clear as well: a re-listed server drops its notice, a strict superset of controller.ts:3162.
  - C tests/server_settings.rs `server_rename_channel_edit_leave_hand_over_delete`: after the leave, `assert!(state.removal_notices.is_empty())` once the next list read lands.
  - D diagnostics/banner.rs inline:
    - `#[test] fn an_older_instance_says_so_without_reload`: `plan(&state(VersionRefused { upgrade: false, instance: true }, Offline), Required) == Some(BannerPlan::InstanceTooOld)`.
    - `instance: false` → `ClientTooOld`.
  - D diagnostics/removed.rs inline `#[test] fn a_dismissed_notice_is_not_shown_again`: two notices give two `(id, tf("removed.notice", [("server", name)]))`; dismissing id 1 leaves one.
- [ ] **Step 2: Run, expect failure.** `cargo test -p dilla-client --lib state::removed` and `cargo test -p dilla-desktop --lib diagnostics::` fail to compile.
- [ ] **Step 3: Implement the client side.**
  - In `Publisher::on_loaded`, before replacing `listed`, compute `notices(&before, &now_ids, &self_removed.lock(), &mut self.removal_seq)`. Append the result to `state.removal_notices` and truncate from the front to `REMOVALS_MAX`. For every listed id: `self_removed.listed(id)`, and drop any notice for it.
  - `leave`/`delete_community`: call `begin(c)` before `request`, and `end(c, err.status)` on error. The 404-is-success rule follows controller.ts:206-209.
  - `InstanceTooOld` draws `Banner::new("banner-instance-too-old-line", BannerTone::Warn, t("shell.banner.instanceTooOld"))` with no action.
  - The removal banners each draw `Banner::new(("banner-removed", id), BannerTone::Warn, text).action(t("shell.banner.dismiss"), ..)`. The action inserts the id and focuses `ctx.composer`.
  - Order in the slot: the version banners, then the removal notices, matching Shell.tsx:1845-1866.
- [ ] **Step 4: Run, expect pass.** Same commands, plus `cargo test -p dilla-client --test members kicked_client_told -- --ignored` and `cargo test -p dilla-desktop --test diagnostics -- --ignored older_client_refused`.
- [ ] **Step 5: Gates and parity.**
  - BEHAVIOUR-GATES row `e2e/web/membership.spec.ts | a kicked browser is told…` (line 82): append `; dilla-desktop::src/screens/diagnostics/removed::a_dismissed_notice_is_not_shown_again`.
  - No screen fixture. The world spec cannot remove the viewer, or change the instance's wire, before a shot, and the `Banner` warn-with-action state is already referenced by `components/banner`.
- [ ] **Step 6: Commit.** `desktop: say when a server took you out, and when the instance is the older side`

### Task 16: A search hit on a channel or DM message jumps to its row [X2]

**Size:** M

The web's behaviour (Shell.tsx:1745-1806):
- **Known target.** The hit's target must be known: a community channel in the rail, or a DM in the DM list (:1746-1748).
  Otherwise the pick fails, the palette says `search.unavailable`, and focus goes back to its textbox (Search.tsx:44-49).
- **Reply hit.** A reply opens its thread. GPUI already does this (D/screens/shell.rs:583-590).
- **Open and wait.** Navigate to the conversation. If its timeline is not published yet, wait for it for at most 1000 ms (:1765-1777).
  If there is still none, the pick fails.
- **Load earlier.** The row is available when `item.seq == hit.seq && item.state == Ok` (:1763). While it is not available and
  `has_earlier` is true, call `loadEarlier`. Stop when a load publishes nothing new (:1780-1784). There is no other cap.
  If the row is still not available, the pick fails (:1785).
- **Focus.** Otherwise the palette closes, the row takes the focus, and the log scrolls the row to the centre of the viewport
  (`scrollIntoView({ block: 'center' })`, :1788-1793). The row does not flash.
- **During the wait.** The palette stays open while all this runs: `pick` awaits `run()` (Search.tsx:47).

**Files:**
- Modify C/client.rs: add `Client::find_loaded` beside Task 12's `load_earlier`.
- Modify D/screens/search/mod.rs:84: `OnNavigate` returns `NavOutcome`. Also `pick` (:312-331).
- Modify D/screens/shell.rs:559-614: the `Navigate::Message` arm, including DMs.
- Modify D/screens/messages/ui.rs:600: add `focus_row`. Modify D/screens/timeline.rs:1249-1256: the centred placement and the focus.
- Test: C tests/messages.rs, `#[ignore]`. D tests/search.rs and inline tests in D/screens/timeline.rs.

**Interfaces:**
- Produces (C): `pub fn find_loaded(&self, channel: ChannelId, seq: u64) -> oneshot::Receiver<Option<String>>`. It answers the
  item's `key`, or `None`. It runs the loop above on the client's runtime over `self.state()` (C/client.rs:574) and calls Task 12's
  `load_earlier`. "Published nothing new" means the channel's `Timeline` before the load `==` the one after it.
- Produces (D): `pub enum NavOutcome { Done, Gone, Later(futures::channel::oneshot::Receiver<bool>) }`, with
  `pub type OnNavigate = Rc<dyn Fn(Navigate, &mut Window, &mut App) -> NavOutcome>`. For `Later`, `pick` keeps the palette open and
  then acts on `true` (close) or `false` (`unavailable`, refocus the textbox) as it does today.
- Produces (D): `MessagesUi::focus_row(&mut self, key: String, cx)`. It sets `active = Some(key)`, `jump = Some(key)` and
  `centre = true`, with no flash.
- Produces (D): `pub fn centred_offset(top: f32, height: f32, viewport: f32, inner: f32) -> f32` in timeline.rs. It returns
  `(top + height / 2 - viewport / 2)` clamped to `0..=max(inner - viewport, 0)`.

- [ ] Step 1: Write the failing tests.
  - C tests/messages.rs `#[ignore]` `find_loaded_reaches_an_old_message`. Alice sends `m0..m150` in general. `find_loaded(ch, seq_of_m0)`
    answers `Some(key)` with the `items` containing `m0`, and `items.len() == 151`. `find_loaded(ch, 999_999)` answers `None` after
    the page stops growing.
  - D timeline.rs inline `a_search_hit_is_centred`:
    - `centred_offset(1000.0, 40.0, 600.0, 3000.0) == 720.0`
    - `centred_offset(100.0, 40.0, 600.0, 3000.0) == 0.0`
    - `centred_offset(2950.0, 40.0, 600.0, 3000.0) == 2400.0`
  - D tests/search.rs `a_late_result_keeps_the_palette_open_until_it_answers`. This is the `host` pattern (:252) with a navigate
    that returns `Later(rx)`. While waiting, `is_open` is true. Send `false`: the palette stays open and `status() == Some("That message is no longer available")`.
    A second run sends `true`, and `is_open` becomes false.
- [ ] Step 2: `$ENV cargo test -p dilla-desktop --lib timeline::tests::a_search_hit` and `--test search a_late_result` fail to compile.
- [ ] Step 3: Implement the C side: `find_loaded`.
- [ ] Step 4: Implement the shell's `Navigate::Message` arm.
  - With `thread_root`: as today.
  - Community hit: `open(..)`. DM hit: `self.state.dms.get(hit.channel)` must be `Some`, then `self.navigate(Route::Dm(hit.channel))`.
  - Without a client (fixture): return `Done`.
  - Otherwise return `Later(rx)`. Spawn `client.find_loaded(channel, seq)`. On `Some(key)`, if `self.channel == Some(channel)` still
    holds, call `messages_ui.focus_row(key)` and answer `true`. Otherwise answer `false`.
  - `Navigate::Channel` and the others answer `Done` or `Gone` in place of `true`/`false`.
- [ ] Step 5: Implement the timeline side. In the `jumped` branch (timeline.rs:1249), when `centre` is set, use
      `centred_offset(tops[at], row_h, viewport, inner)`, then clear `centre`. Otherwise keep `tops[at] - 48`. Then `window.focus` the
      row's `row_focus` handle.
- [ ] Step 6: `$ENV cargo test -p dilla-desktop --lib timeline::`, `$ENV cargo test -p dilla-desktop --test search`, and
      `$ENV cargo test -p dilla-client --test messages find_loaded -- --ignored` pass.
- [ ] Step 7: Add no parity shot: no new visible state, since the focus ring is the existing one. Add no BEHAVIOUR-GATES row, since no
      e2e title covers it.
- [ ] Step 8: Commit `desktop: a search result scrolls to its message, loading earlier pages until it is there`.

### Task 17: "load earlier" in the thread pane [X1]

**Size:** S

**Files:**
- Modify U/components/threads/pane.rs: add `PaneParts.earlier: Option<AnyElement>` (:50-69). `pane` draws it between the rule and
  the first reply.
- Modify D/screens/threads/view.rs: `PaneView` gains `loading_earlier: bool` (:100-110), and `render` (:359-535) fills `earlier`.
- Test: C tests/threads.rs. Inline `#[cfg(test)]` in D/screens/threads/view.rs.

**Interfaces:**
- Consumes `client.threads().load_earlier() -> Done<()>` (C/features/threads.rs:231). Consumes `ThreadData.has_earlier`
  (C/state/threads.rs:32). A refresh keeps the pages already loaded (`with_earlier`, threads.rs:694).
- Produces `pub fn earlier_shown(data: Option<&ThreadData>, loading: bool) -> Option<bool>` in view.rs. It returns `None` when there
  is no button, otherwise `Some(enabled)`. The button shows while `data.has_earlier`, and it is disabled while `loading`.

- [ ] Step 1: Write the failing tests.
  - C tests/threads.rs, `#[ignore]` like the others, using `two_people`: `load_earlier_pages_a_long_thread`. Alice posts a root and
    201 replies `r0..r200`. `bob.threads().open(ch, root)`, then `wait_for` `replies.len() == 200 && has_earlier && replies[0].body == "r1"`.
    `bob.threads().load_earlier().await` gives `Ok(Ok(()))`. Then `replies.len() == 201 && !has_earlier && replies[0].body == "r0"`.
    Alice sends `r201`, and `wait_for` `replies.len() == 202`: the refresh does not drop the earlier page.
  - D view.rs inline `the_earlier_button_follows_has_earlier`:
    - `earlier_shown(None, false) == None`
    - `has_earlier: false` gives `None`
    - `has_earlier: true` gives `Some(true)`
    - `has_earlier: true` with `loading = true` gives `Some(false)`
- [ ] Step 2: `$ENV cargo test -p dilla-desktop --lib threads::view::tests::the_earlier` fails to compile.
- [ ] Step 3: Implement `earlier_shown`.
  - In `render`, build `Button` (`ButtonVariant::Ghost`, id `"thread-earlier"`, label `t("shell.log.earlier")`, `.disabled(!enabled)`),
    centred, with `GROUP_GAP` below it, the same as Task 12's `log-earlier`.
  - Its click sets `loading_earlier = true` and spawns `client.threads().load_earlier()`. The answer clears the flag. An `Err` goes
    through the toast path the pane already uses.
  - The reply list carries `aria-busy` while `loading_earlier` is true (MessageLog `busy`, Conversation.tsx:397).
- [ ] Step 4: `$ENV cargo test -p dilla-desktop --lib threads::view` and
      `$ENV cargo test -p dilla-client --test threads load_earlier_pages -- --ignored` pass.
- [ ] Step 5: Parity. The web never draws this button, so there is no web reference. Take a GPUI-only reference
      `native/thread-earlier`: `tests/threads.rs::parity_thread_earlier` calls `assert_native_screen` on a GPUI-only fixture
      `P/fixtures/threads/thread-earlier.json` (threads/pane-open.json's world with 201 replies in the open thread; the test asserts the
      pane's `has_earlier` before the shot). Bless it (`DILLA_PARITY_BLESS=1`) and add it to the README `native/` table. Add no BEHAVIOUR-GATES row, since no e2e title covers it.
- [ ] Step 6: Commit `desktop: load earlier replies in a long thread`.

### Task 18: Server settings → Self roles drawn with the Signal controls (GPUI-only reference) [V2]

**Size:** M

Decision D19: follow the prototype, not the browser's `<fieldset>`, checkboxes and `<select>` that SelfRolesSection.tsx
leaves unstyled. The prototype has no self-roles screen, so build it from the design system's own parts. The role
lists use `CheckList` (packages/ui CheckList: label, 32px rows, hairlines, colour dot). The channel uses U `Select`,
which is already there. Text and order stay the web's (W/screens/admin/SelfRolesSection.tsx): legends become the
CheckList labels, then "Publish a role panel", its body lines, the channel row, Publish and the empty line.

**Files:** Modify D/screens/members/self_roles.rs. Delete the fieldset constants and drawing (:36-60 and their users) and
use `CheckList`/`CheckListItem` for the "Allow members to assign these roles" box and the "Your roles" box.
Modify D/../tests/members.rs:891-892.
**Interfaces:** Consumes `CheckList::new(id, label, items, on_toggle)` (U/components/check_list.rs) and the existing role
toggles in self_roles.rs. Element ids stay `self-roles-approve-<hex>` and `self-roles-mine-<hex>` (the a11y and
keyboard tests use them).
- [ ] Step 1: Failing test in tests/members.rs, `self_roles_draw_checklists`, on fixture `server-settings/self-roles.json`.
      Assert `wait_for("self-roles-approve")` is a CheckList (child row `self-roles-approve-<crew hex>` height 32.0), and
      that no element `self-roles-fieldset` exists.
- [ ] Step 2: `$ENV cargo test -p dilla-desktop --test members self_roles_draw` fails.
- [ ] Step 3: Swap the drawing. Keep the toggle handlers and the busy dimming (CheckList's in-flight row).
- [ ] Step 4: The test passes. `$ENV cargo test -p dilla-desktop --test a11y` passes too (the tree lint).
- [ ] Step 5: Replace the two ignored `parity_server_settings_self_roles(_b)` with `assert_native_screen(fixture,
      "server-settings-self-roles(-b)", |_| {})`. Bless the goldens. Look at them next to the prototype's Roles page
      (`sRoles`) for hairlines, label style and Select field. Add both rows to the README `native/` table. Delete the
      web references `server-settings/self-roles(-b)-2x.png` and their sidecars (they show browser widgets).
- [ ] Step 6: Commit `desktop: self roles drawn with the Signal check lists and select (GPUI-only reference)`.

### Task 19: Choosing Compact density takes effect [V4]

**Size:** S

In the appearance-b frame the click on Compact leaves Regular selected. `UserSettings::set_density` returns early when
`self.ctx.client` is `None` (D/screens/user_settings/appearance.rs:34-45), and a fixture window has no client
(D/fixture.rs:1-3). The live path writes `s.profile.density` through `put_setting` (C/features/profile/mod.rs:290-297).
First confirm whether the live app has the bug too (Step 1b). Notify already keeps a window-local choice when there
is no client (D/screens/notify/mod.rs:150-162). Do the same here.

**Files:** Modify D/screens/user_settings/appearance.rs (and `set_motion`) · Modify D/screens/user_settings/section.rs:283 (`density()` reads the local value first) · Test D/../tests/user_settings.rs.
**Interfaces:** Produces `UserSettings.local_density: Option<DensitySetting>` and `local_motion: Option<MotionSetting>`,
which are set only when `ctx.client.is_none()`.
- [ ] Step 1a: Failing test `appearance_compact_click_selects_compact` (Driver on `user-settings/appearance-b.json`):
      after the fixture's steps, `element("appearance-density-compact")` has `selected == true`, and the section's group
      gap is `Density::Compact.group_gap()` (8px, tokens.css:338).
- [ ] Step 1b: Live check `#[ignore] appearance_density_saves_on_a_real_instance` in tests/user_settings.rs (dilla-testhost,
      pattern of the ignored notify-settings live test). Click Compact. Then `client.state().profile.density == Compact`,
      and after a restart of the client it still is. If 1b fails, the live bug is in `put_setting` or the state
      broadcast. Fix it there and keep 1b.
- [ ] Step 2: `$ENV cargo test -p dilla-desktop --test user_settings appearance_` fails (1a). Also run 1b with `-- --ignored`.
- [ ] Step 3: With no client, write the local value and `cx.notify()`. `density()` and the Segmented's value read it.
- [ ] Step 4: Both pass.
- [ ] Step 5: Re-measure `parity_user_settings_appearance_b`. Segmented labels sit 1px low: fix in U/components/segmented.rs
      (label top from layout.json). Append `dilla-desktop::tests/user_settings::appearance_compact_click_selects_compact`
      to BEHAVIOUR-GATES row "Appearance controls fit desktop rows…" (BEHAVIOUR-GATES.md:106).
- [ ] Step 6: Commit `desktop: the density and motion choices show at once, also without a client`.

### Task 20: Message area: reply line ellipsis, editor width, code block edge [V7]

**Size:** M

- **threads/reply-chip-b:** the reply line's excerpt is clipped at the log edge, with no "…". `ReplyLine` already calls
  `.ellipsis()` (U/components/threads/reply_line.rs:109,115), but its row has no `min_w_0()`/`overflow_hidden()`, so the
  line never shrinks.
- **messages/edit-b:** the editor wraps one word early because the content box is a few px narrower. The width must be
  the row's body width less `.d-message-editor__input` padding `0.3125rem 0.75rem` (MessageEditor.css:2).
- **messages/code-block(-b):** the block's right edge and the Copy button sit about 2px left. The block spans the body
  width, and Copy sits at the web's `right` inset.

**Files:** U/components/threads/reply_line.rs · D/screens/messages/editor.rs · D/screens/messages/markdown.rs (the code card).
- [ ] Step 1: Failing tests.
      - `reply_line::tests::a_long_excerpt_ends_in_an_ellipsis` (a gpui test window at width 600 with a 400-char
        excerpt; the drawn text run ends in `…` and its right ≤ the row's right).
      - `editor::tests::editor_content_width` (`content_width(body_w) == body_w - 24.0`).
      - `markdown::tests::code_card_spans_the_body` (card right == body right; copy right inset from layout.json).
- [ ] Step 2: `$ENV cargo test -p dilla-desktop-ui --lib reply_line` and `$ENV cargo test -p dilla-desktop --lib editor::tests markdown::tests::code_card` fail.
- [ ] Step 3: Fix the three.
- [ ] Step 4: Pass. Then `$ENV cargo test -p dilla-desktop --test threads --test messages parity_`.
- [ ] Step 5: No new fixtures (reply-chip-b, edit-b and code-block-b already exist).
- [ ] Step 6: Commit `desktop: reply lines end in an ellipsis; the editor and code cards are the body's width`.

### Task 21: Fine offsets: onboarding keys, sign-in cards, server rail tile [V9]

**Size:** M

- onboarding/keys: the subtitle wraps at a different word. Its max-width or font size is off. Take the `p` box width from layout.json.
- sign-in/factor, form, error: the card contents sit 1–2px low. Compare the card's padding-top and the first heading's line box.
- shell/*: the selected server tile's initials and ring are about 1px off. The ring is `0 0 0 2px bg-2, 0 0 0 3.5px accent`
  (D/screens/rail.rs:34,62-75). Put the 1.5px ring on half-pixels, as Chromium does at scale 2: outset 3.5 css = 7 device px.
  Centre the initials on the line box, not the cap height.

**Files:** U/components/onboarding/frame.rs or forms.rs · D/screens/sign_in.rs (card layout) · D/screens/rail.rs.
- [ ] Step 1: Failing tests. `rail::tests::selected_ring_outset_is_3_5`, plus a Driver test `sign_in_card_heading_top`
      (form fixture: `element("sign-in-title").y` equals the layout.json value).
- [ ] Step 2: The two fail (`$ENV cargo test -p dilla-desktop --lib rail::tests`, `--test sign_in sign_in_card`).
- [ ] Step 3: Fix.
- [ ] Step 4: Pass. `parity_onboarding_keys`, `parity_sign_in_*` and `parity_shell_*` pass, with chrome % lower.
- [ ] Step 5: No new fixtures.
- [ ] Step 6: Commit `desktop: onboarding, sign-in and the server rail land on the web's pixels`.

### Task 22: References and parity tests for the DM screens [V12]

**Size:** S

`dms/home(-b)` and `dms/conversation(-b)` are valid fixtures (D/../tests/dms.rs:309-326) with empty reference folders.
- [ ] Step 1: Add `parity_dms_home`, `_home_b`, `_conversation`, `_conversation_b` in tests/dms.rs, using the `parity(name)`
      helper pattern of tests/notify.rs:30-36. They fail with "no sidecar".
- [ ] Step 2: `$ENV cargo test -p dilla-desktop --test dms parity_` fails.
- [ ] Step 3: Shoot all four (`node clients/desktop/parity/shoot.mjs clients/desktop/parity/fixtures/dms/<n>.json`).
      Fix what differs inside the DM screens. Anything over the gate becomes its own commit with a unit test that asserts the geometric rule with the value read from layout.json (the pattern of Task 33).
- [ ] Step 4: All four pass.
- [ ] Step 5: Add the four to the README table.
- [ ] Step 6: Commit `parity: web references for the DM home and conversation`.

### Task 23: Calls with several people (high traffic: every voice session) [V14]

**Size:** M

The voice references today are the viewer alone or one other person (P/fixtures/voice/*). Others in a call are seeded
as voice_state frames with no media (D/examples/parity_world/seed/calls.rs:1-12). The web can show muted, deafened and
"sharing" flags. Speaking rings, camera tiles and shared-screen pixels need media, which a web reference can't hold
still. Those get GPUI-only references drawn from state.
**Files:** P/fixtures/voice/stage-group.json and stage-group-b.json (new) · D/../tests/voice.rs.
- [ ] Step 1: `voice/stage-group.json`: Ada (viewer, joins via `click:-` as voice/stage) plus mira, bjorn (`muted`),
      lin (`deafened`) and sam (`sharing`) in midgard/lounge, 1280×800. `-b` is the same at 1440×900 with 8 people
      (the grid wraps). Shoot both.
- [ ] Step 2: `parity_voice_stage_group(_b)` (assert_screen). Fix what differs. Each fix gets a unit test in
      U/components/voice asserting the geometric rule with the value read from layout.json.
- [ ] Step 3: GPUI-only: `native/voice-speaking`, `native/voice-camera-tile` and `native/voice-screen-tile` through
      `assert_native_screen(voice/stage-group.json, …, prepare)`. `prepare` sends the automation command
      `voice-sim speaking <person>`, `camera <person> <test-pattern>` or `screen <person> <test-pattern>`. Add that command
      to D/automation.rs: it sets `state.voice` levels and a static RGBA test frame. Bless by eye against
      `packages/ui/src/VoiceStage` stories.
- [ ] Step 4: `$ENV cargo test -p dilla-desktop --test voice parity_voice_stage_group native_voice` passes.
- [ ] Step 5: README rows (web pair and native/ table).
- [ ] Step 6: Commit `parity: a group call (muted, deafened, sharing) and native speaking, camera and screen tiles`.

### Task 24: Navigation, UI-opening and voice built-ins [C3]

**Size:** L

`/giphy /invite /search /thread /dm /modlog /join /leave /mute /unmute /deafen /undeafen` (builtin.ts:88-130, :153-159, :272-277, :282-307, :400-408).

**Files:**
- Modify: C/features/bot.rs (`Builtin` variants and their checks in `plan_in`)
- Create/modify: D/screens/commands/builtins.rs (the UI runs)
- Create: D/screens/commands/modlog_dialog.rs (the moderation-log dialog, Decision D1)
- Modify: D/screens/commands/picker.rs:247 (`gifs_enabled`), D/screens/attachments/ui.rs:1258 (`open_gifs`), D/screens/search/mod.rs:168 (`request_open_with`), D/screens/dms/mod.rs:36 (`Nav::ServerSettings`), D/screens/shell.rs:1283 (map it), D/screens/create/flows.rs (invite outcome), D/screens/voice/mod.rs:1230 (`join_then`)
- Test: C/features/bot.rs tests; D/screens/commands/builtins.rs tests; D/tests/bot.rs (ignored, dillad)

**Interfaces:**
- `Builtin::Giphy { query: String }`, `Invite { community }`, `Search { query: String }`, `Thread { root: MessageId }`, `Dm { user: UserId }`, `Modlog { community }`, `Join { channel: ChannelId }`, `Leave`, `Voice { muted: bool, deafened: bool }`.
- D: `pub fn open_gifs(client: Rc<Client>, owner: WindowId, channel: ChannelId, community: CommunityId, query: String, cx: &mut App)` (opens, never toggles closed; the search field is made with `query`, then `search_gifs(query, false, cx)`); `pub fn request_open_with(window: WindowId, query: String, cx: &mut App)` (fills the palette's input; Shell.tsx:2175 ignores the scope); `create::open_invite_then(window: &Window, community, done: Box<dyn FnOnce(bool, &mut App)>, cx)` (`true` on the flow's `InviteState::Ready`, `false` on its close, flows.rs:300/:328/:863); `voice::join_then(channel, window, done: Box<dyn FnOnce(Result<(), ClientError>, &mut App)>, cx)`; `modlog_dialog::open(window: WindowId, community: CommunityId, conversation: Conversation, cx: &mut App)` and `modlog_dialog::overlay(ctx: &SlotCx, window, cx) -> Option<AnyElement>` (per-window `OpenModlog(Option<(CommunityId, Conversation)>)` via `crate::per_window`, drawn from D/slots.rs overlays, cleared by `commands::release_windows`).

Rules and copy, each `plan_in` check → `Plan::Private(Failed, text, Builtin)`; each run patches the row with `Bot::finish`:
- giphy: DM → "GIF search is available only in a community channel."; run → `open_gifs`, Success "GIF search opened." (builtin.ts:88-92). Listed only when `ui::gif_on(client, community)` is `Some(true)`; `picker::definitions` gains `gifs: bool` that `picker()` reads from `attachments::ui::gif_on` (CommandHost.tsx:186).
- invite: DM or channel bits lacking `CREATE_INVITE` (bit 11, `roles::viewer_channel_bits`) → "You cannot create invites here."; run → Success "Invite created and shown privately." / Failed "Invite creation cancelled." (builtin.ts:93-97).
- search: `scope` not in channel|community|all → "Scope must be channel, community, or all."; `community` in a DM → "Community search is unavailable here."; run → `request_open_with(query)`, Success "Search opened." (builtin.ts:108-115).
- thread: DM or `exact_message` none → "That message is not available in this channel."; run → `threads::open_thread(channel, root, ctx, cx)`, Success "Thread opened." (builtin.ts:121-127).
- dm: run → `client.dms().open_dm(user)`; Err → Failed "Could not open that direct message."; Ok but `ctx.channel` moved → Failed "The direct message was opened, but navigation was skipped because the conversation changed."; else `dms::navigate(Nav::Dm(ch))`, Success "Direct message opened." (builtin.ts:98-106).
- modlog: DM or no `VIEW_AUDIT_LOG` (bit 17) → "Moderation history needs a community and View Audit Log permission."; run → `modlog_dialog::open`, Success "Moderation history opened." (builtin.ts:272-276). The dialog (Decision D1, CommandHost.tsx:411) is U `Dialog` over the conversation, title `admin.bot.moderationLog.title`, close `dialog.close`, its body the existing `moderation_log_section(ctx, community, window, cx)` (D/screens/members/mod.rs:1239, private today: make it `pub(crate)`) at the dialog's width; Escape or close dismisses it, and it closes itself when `ctx.channel` is no longer `conversation.channel` (CommandHost.tsx:130 `setModlogOpen(false)` on a key change). Module name: the members area already has a `modlog` module (`modlog::with_log`, mod.rs:1247), so the commands one is `commands::modlog_dialog` (file D/screens/commands/modlog_dialog.rs) to keep the two apart.
- join: target not a voice channel (`kind == 1`) of this community → "Choose an accessible voice channel."; run → `voice::join_then`; Ok → "Joined voice."; Err(e) → Failed `Could not join voice ({key}).` with `{key}` = `voice::join_failure_key(&e)` (D/screens/voice/mod.rs:1182; the web prints its notice key too), and for a `None` key (a cancelled join) the uncertain sentence below (builtin.ts:301); no answer in 35 s → Failed "Voice join is still pending. Check the call before trying again." (builtin.ts:304).
- leave: `state.voice.call.channel` none → "You are not in a voice call."; run → `voice::leave`, Success "Left voice." (builtin.ts:153-158).
- mute/unmute/deafen/undeafen: `call.phase != CallPhase::In` → "Join a voice call first."; plan computes `(muted, deafened)` as builtin.ts:159-162 from `self_mute`/`self_deaf`; run → `set_deafened` then `set_muted` only where they differ (builtin.ts:400-407), Success "Voice settings updated.".

- [ ] Step 1: Failing tests. C `navigation_builtins_check_before_running`: one `assert_eq!` per refusal above with its exact sentence, plus `plan_in(.., "/search query:x scope:all")` → `Plan::Builtin { run: Builtin::Search { query: "x".into() }, confirm: None }`, `"/deafen"` in a call with `self_mute=false` → `Voice { muted: true, deafened: true }`, `"/undeafen"` with `self_mute=true, self_deaf=true` → `Voice { muted: true, deafened: false }`. D `giphy_is_listed_only_where_gifs_are_on`: `picker::definitions(&s, Some(c), false)` has no `giphy`, `(…, true)` has it. D ignored `a_dm_command_opens_the_conversation` (dillad: `/dm` Bea from Midgard → route is the DM with Bea, row `bot-result-success`), pattern of D/tests/bot.rs:562.
- [ ] Step 2: `$ENV cargo test -p dilla-client --lib features::bot::tests::navigation_builtins` and `$ENV cargo test -p dilla-desktop --lib screens::commands::picker` fail.
- [ ] Step 3: Implement the variants and checks in `plan_in`, the D hooks above, and `builtins::run` arms; remove these 12 ids from the catch-all.
- [ ] Step 4: `$ENV cargo test -p dilla-client --lib features::bot`; `$ENV cargo test -p dilla-desktop --lib screens::commands`; `$ENV cargo test -p dilla-desktop --test bot a_dm_command_opens_the_conversation -- --ignored`.
- [ ] Step 5: Parity fixture `clients/desktop/parity/fixtures/commands/modlog-dialog.json` (world of commands/reminders.json, viewer Ada as owner; steps type `/modlog` into `composer`/`css=.d-composer__input`, `key enter`, `settle`), shot with `shoot.mjs` (the web draws the same dialog), test `parity_commands_modlog_dialog` beside `parity_commands_reminders` (D/tests/bot.rs:68), README table row; no gate row.
- [ ] Step 6: Commit `desktop: /giphy /invite /search /thread /dm /modlog and the voice commands run`.

### Task 25: Role built-ins (`/role add|remove`, `/selfrole list|add|remove`) and no catch-all left [C6]

**Size:** M

builtin.ts:265-271, :395-413.

**Files:** Modify C/features/bot.rs (variants; the catch-all arm becomes `unreachable`-free: every built-in id has an arm); D/screens/commands/builtins.rs; Test C/features/bot.rs tests, D/tests/bot.rs (ignored).

**Interfaces:** `Builtin::Role { community, user: UserId, role: RoleId, add: bool, role_name: String, member_name: String }`, `SelfRoleList { community }`, `SelfRole { community, role: String /*hex*/, add: bool }`. Consumes `assignable_role` (Task 9), `Roles::grant_role/revoke_role/load_self_roles/select_self_role` (C/features/roles.rs:1251/:1270/:1294/:1423) through `client.roles().run(..)`.

Rules:
- role add/remove: `assignable_role` none → "This member or role is not assignable with your current permissions and rank."; remove asks `Ask { title: "Remove {role} from {member}?", body: "This changes the member’s roles." }`, add asks nothing; run: `assignable_role` again → else "Your permissions or role rank changed."; Ok → `{role} added to {member}.` / `{role} removed from {member}.`.
- selfrole list: run → `load_self_roles`: empty → "No self-assignable roles are available."; else rows as `[x] {name}` / `[ ] {name}` joined by " · ".
- selfrole add/remove: run → `load_self_roles`; role id absent → Failed "That role is not currently self-assignable."; `self_selected == add` → Success `You already have {name}.` / `You do not have {name}.`; else `select_self_role(c, role, add)` → `{name} added.` / `{name} removed.`.

- [ ] Step 1: Failing tests. C `role_builtins_check_before_running` (`role add` @everyone → refusal; a held role for add → refusal; `role remove` of a held lower role → `confirm: Some(Ask { title: "Remove Crew from bea?", body: "This changes the member’s roles." })`; `role add` → `confirm: None`). C `every_builtin_has_a_handler` — port of builtin.test.ts:501: for each `registry::commands()` id, `plan_in` with valid arguments in a fully permitted state never returns text `UNAVAILABLE`; remove the `UNAVAILABLE` const's last use from the catch-all (an unknown built-in id can no longer occur: `registry::commands()` is closed). D ignored `selfrole_commands_list_add_and_remove` (dillad: an approved self role → `/selfrole list` row text "[ ] Crew"; `/selfrole add` → "Crew added."; list → "[x] Crew").
- [ ] Step 2: `$ENV cargo test -p dilla-client --lib features::bot::tests::role_builtins features::bot::tests::every_builtin_has_a_handler` fails.
- [ ] Step 3: Implement; delete the catch-all arm.
- [ ] Step 4: C/D commands; `$ENV cargo test -p dilla-desktop --test bot selfrole_commands_list_add_and_remove -- --ignored`.
- [ ] Step 5: No new visible state; no gate row (no frozen title covers it).
- [ ] Step 6: Commit `desktop: /role and /selfrole run; every built-in command has its handler`.

### Task 26: `/poll create` opens the poll form [C7]

**Size:** M

The web runs `poll create` through its own handler (W/screens/commands/UtilityCommandProvider.tsx:354-391), not as a
typed Bot command; GPUI sends it to the Bot as typed args (mod.rs:345 `Plan::Bot`). U `PollDialog` (U/components/commands/poll_dialog.rs)
is drawn nowhere; `encode_poll_create` (C/features/utilities/polls.rs:59) has no caller.

**Files:**
- Modify: C/features/utilities.rs (new `create_poll`)
- Create: D/screens/commands/poll_form.rs (per-window form state, overlay, submit)
- Modify: D/screens/commands/mod.rs (`on_send` intercepts `definition.id == "poll create"`), D/slots.rs overlays
- Test: C/features/utilities.rs tests; D/screens/commands/poll_form.rs tests; D/tests/bot.rs (parity + ignored)

**Interfaces:**
- C: `pub async fn create_poll(&self, community: CommunityId, input: polls::PollCreate, request: [u8; 16]) -> Result<PollView, UtilityError>` — `BotRequest { operation: 70, channel: Some(ChannelId(input.channel)), args: vec![BotArg::Bytes(encode_poll_create(&input)?)], revision: Some(0), request_id: Some(request) }` through `self.send` (utilities.rs:162), decoded with `wire::decode_poll` (utilities.ts:649-653, `decodePollCreate` :325: `my_choices` must be `Some(empty)` else `Invalid`).
- D: `pub struct PollForm { question: Entity<InputState>, answers: Vec<Entity<InputState>>, duration: usize, multi: bool, error: Option<String>, busy: bool, request: [u8; 16], row: Option<u64>, conversation: Conversation, community: CommunityId, client: Option<Rc<Client>> }`; `pub fn validate(question: &str, answers: &[String], multi: bool, duration: usize, channel: [u8;16]) -> Result<PollCreate, &'static str /* strings key */>`;
  `impl PollForm { pub fn begin_post(&mut self) -> Option<[u8; 16]>; pub fn end_post(&mut self, keep_open: bool) }`. `begin_post` returns the form's one
  `request` and sets `busy` when the form is idle, and `None` while `busy` (Post and Enter both go through it); `end_post` clears `busy`.

Rules:
- Opening: only in a community channel with `SEND_MESSAGES` (`canCreateFromContext`, :54-58) and the catalog offering v1 op 70 (`state.bot.catalogs[c].commands` has `operation == sealed_operation(70)`, operations.ts:48) → else Failed "Create a poll in a community channel." / "The Bot cannot create polls in this channel." (:357, :362). Prefill: `question` arg; `answers` arg split on `\s*,\s*` into rows, at least 2 rows (:124-125, :374); `multiple == "on"`; duration arg index in `["1h","4h","8h","24h","3d","7d"]`, default `24h` (:126).
- Validate in this order (:131-139): trimmed question empty → `poll.questionRequired`; > 300 chars → `poll.questionLong`; < 2 non-empty trimmed answers → `poll.answersRequired`; any > 100 chars → `poll.answerLong`; > 10 or duplicates → `poll.duplicates`; `encode_poll_create` → `TooLarge` → `poll.aggregateValidation`. Post enabled when question non-blank and ≥ 2 non-blank answers (:142).
- Post: `busy = true` (Post disabled), pending row `copy::pending("poll create")` (`startResult`, :379), `create_poll(.., request)`; Ok → toast `poll.created` and the row Success `poll.created`; `Uncertain` → Failed `poll.createdUncertain` (form closed; Task 30 keeps the request and reopens the draft so a resend reconciles); `Unavailable` → "The Bot is unavailable in this community."; `Status(4)` → "The record changed. Refresh it and try again."; `Status(2|3)` → "This action is no longer available."; `Payload` from `TooLarge` → "The answers use too much space. Shorten them and try again."; other → "Check the fields and try again." (`caught`, :76-85).
- Cancel / Escape / leaving the conversation: form closes, no row (CommandHost.tsx:297: a cancelled form leaves nothing). The catalog stops offering op 70 while open → close, Failed "The Bot stopped offering polls. Try again." (:376).
- Labels from `poll-dialog-labels.ts` keys: `poll.title` with the channel name, `poll.dismiss`, `poll.cancel`, `poll.post`, `poll.question`, `poll.counter`, `poll.answers`, `poll.answerRange`, `poll.addAnswer`, `poll.duration`, `poll.multiple`, `poll.footer`, `poll.duration.*`; bot name from `picker::bot_name`.

- [ ] Step 1: Failing tests. C `create_poll_sends_operation_70_at_revision_0` with a fake `SealedBot` (pattern: bot_admin.rs:1318 test transport): asserts the sealed op `sealed_operation(70)`, revision 0, one `Bytes` arg equal to `encode_poll_create(&input)`, and that a reply with `my_choices` non-empty is `Err(UtilityError::Invalid)`. D `poll_form_validates_as_the_web` — `validate("", ..)` → `"poll.questionRequired"`; 301 × "a" → `"poll.questionLong"`; answers `["x"," "]` → `"poll.answersRequired"`; 101-char answer → `"poll.answerLong"`; `["a","a"]` → `"poll.duplicates"`; 10 answers of 100 chars + 300-char question → `"poll.aggregateValidation"`; a good one → `Ok(PollCreate { duration: 86_400, .. })`. D `the_answers_argument_prefills_rows`: `"red, green ,blue"` → three rows.
  D `a_second_post_while_busy_sends_nothing` (Review Focus 2): a form with request `R`: `begin_post() == Some(R)`, a second `begin_post() == None`;
  after `end_post(true)` (a definitive failure keeps the form open), `begin_post() == Some(R)` again: the same id, never a fresh one while the form is open.
- [ ] Step 2: `$ENV cargo test -p dilla-client --lib features::utilities` and `$ENV cargo test -p dilla-desktop --lib screens::commands::poll_form` fail.
- [ ] Step 3: Implement `create_poll`, `poll_form.rs` (state per window via `crate::per_window`, released in `commands::release_windows`), the overlay drawing U `PollDialog` with its inputs, and the `on_send` intercept (works with `client: None` for the fixture: Post does nothing).
- [ ] Step 4: the two commands; `$ENV cargo test -p dilla-desktop --test bot a_poll_posted_from_the_form_shows_its_card -- --ignored` (dillad with the Bot: Post → row `bot-result-success`; Task 27 adds the `poll-card` assertion once posts are verified).
- [ ] Step 5: Fixture `clients/desktop/parity/fixtures/polls/dialog.json` (world of polls/card.json; steps type `/poll create` → `key enter` → type "What should we raid on Friday?" into the question field (gpui: wrap it in `tracked_box("poll-question", …)`; web: `css=.d-poll-dialog textarea`) → `settle`), committed here; the web can open the form only with a real Bot offering op 70, so its reference and the test `parity_polls_dialog` land in Task 40 with the world's poll seeding. Append `dilla-desktop::tests/bot::a_poll_posted_from_the_form_shows_its_card` to the BEHAVIOUR-GATES row "polls publish in channels and threads, accept real votes, reject forged markers, and close by the creator".
- [ ] Step 6: Commit `desktop: /poll create opens the poll form and publishes through the Bot`.

### Task 27: Verify a Bot's poll post when it is drawn [X3]

**Size:** M

The web's behaviour (W/screens/commands/PollCard.tsx):
- **Card owner.** A poll post's card is only for the pinned Bot's unedited, live post. `poll_claim` (C/features/utilities.rs:133)
  already encodes this.
- **Loading.** While unverified, the card says `poll.loading` with `role="status"` (:208).
- **Retries.** A failed read retries after 2, 5 and 10 s (`RETRY_DELAYS`, :8, :184). After that it gives up: the card is an empty
  `aria-hidden` box (:207), and the post's own text shows (Task 39).
- **Definitive failures.** These give up at once with no retry: no ready Bot (`botDefinitelyUnavailable`, command-bot-store.ts:76-78;
  op-3/status-5 refusals are transient), and a record for another channel or thread (:131-136).
- **When reading starts.** The card reads only while it is near the viewport (IntersectionObserver `rootMargin: 160px`, :162-166).

**Files:**
- Modify C/state/bot.rs:193-202: add `BotState.poll_checks: BTreeMap<MessageId, PollCheck>`.
- Modify C/features/utilities.rs: add `verify_poll`, `definitive`, and `retry_after`.
- Modify D/screens/commands/mod.rs:135-179 (`poll_card`).
- Test: C/features/utilities.rs `mod tests`. D/screens/commands/mod.rs tests.

**Interfaces:**
- Produces (C): `pub enum PollCheck { Pending, GaveUp }`.
- Produces (C): `pub fn retry_after(failures: usize) -> Option<Duration>`. It gives `[2 s, 5 s, 10 s]`, then `None`.
- Produces (C): `pub fn definitive(e: &UtilityError) -> bool`. It is true for `NotAPoll`, `Invalid`, and `Unavailable(code)` with
  `code != "E_UNAVAILABLE" && code != "E_STOPPED"`.
- Produces (C): `pub fn verify_poll(&self, community: CommunityId, channel: ChannelId, thread: Option<MessageId>, item: TimelineItem)`.
  It does nothing when `poll_checks` or `polls` already has the message. Otherwise it publishes `Pending`, then spawns `read_poll`.
  - `Ok`: remove the check.
  - A `definitive` error, or `retry_after` returning `None`: publish `GaveUp`.
  - Anything else: sleep, then read again.
- Produces (D): `pub fn poll_card_in(item: &TimelineItem, ctx: &SlotCx, thread: Option<MessageId>, window, cx) -> Option<Extra>`; `poll_card(item, ctx, ..)` becomes `poll_card_in(item, ctx, None, ..)` (Task 41 passes the thread's root). It draws the loading line when `poll_checks` is `Pending`, or when the post has a claim and no check yet.
  In that second case it also calls `verify_poll(community, channel, thread, item)` once. It draws nothing for `GaveUp`.
  - The loading line is `t("poll.loading")` in the row's body text style (no CSS styles `dw-poll-card--loading`), with `Role::Status`
    and id `"poll-loading"`.
  - Rows are drawn only within one viewport above and below (timeline.rs:1271-1297), which is the "near the viewport" rule.

- [ ] Step 1: Write the failing tests.
  - C `poll_retries_follow_the_web`: `retry_after(0) == Some(2s)`, `(1) == Some(5s)`, `(2) == Some(10s)`, `(3) == None`.
  - C `definitive_failures_give_up_at_once`: `definitive(&NotAPoll)`, `definitive(&Unavailable("E_NO_BOT".into()))`, `!definitive(&Unavailable("E_UNAVAILABLE".into()))`,
    `!definitive(&Status(3))`.
  - D `a_claimed_poll_shows_loading_until_verified`: a state whose `bot.poll_checks` holds `Pending` for the post makes `poll_card`
    return an `Extra` with height `> 0`. `GaveUp` returns `None`. A held `PollView` returns the card. Use the existing
    `poll(..)` helper (mod.rs:535).
- [ ] Step 2: `$ENV cargo test -p dilla-client --lib features::utilities` and `$ENV cargo test -p dilla-desktop --lib screens::commands`
      fail to compile.
- [ ] Step 3: Implement the interfaces above. The sleep uses the client runtime's `tokio::time::sleep`.
- [ ] Step 4: The same two commands pass. Extend Task 26's `a_poll_posted_from_the_form_shows_its_card` with
      `wait_for("poll-card", SOON)` after the success row, then run `$ENV cargo test -p dilla-desktop --test bot -- --ignored poll`: it passes.
- [ ] Step 5: Append `dilla-client::src/features/utilities::poll_retries_follow_the_web` to the BEHAVIOUR-GATES row "polls publish in
      channels and threads, accept real votes, reject forged markers, and close by the creator". Add no parity shot: the
      loading line exists only with a live client.
- [ ] Step 6: Commit `desktop: verify a Bot's poll post when it is drawn, with the web's retries`.

### Task 28: Scheduled announcements (`/announcement list|create|edit|delete`) and the editor [C8]

**Size:** L

The web runs all four through `announcementHandler` (W/screens/commands/bot-handlers.ts:376-470, :599) with
sealed ops 50/51/52; GPUI sends them as typed catalog commands, which the Bot cannot read as a schedule. No GPUI code
lists, encodes or saves a schedule; only the picker icon exists (D/screens/commands/picker.rs:157).

**Files:**
- Create: C/features/bot/announcements.rs (`pub mod announcements;` in C/features/bot.rs)
- Create: D/screens/commands/announcement_form.rs
- Modify: D/screens/commands/mod.rs (intercept the four ids), D/screens/commands/chips.rs (`ArgKind::Announcement` suggestions), D/slots.rs overlays
- Test: C/features/bot/announcements.rs tests; D/screens/commands/announcement_form.rs tests; D/tests/bot.rs

**Interfaces (C):**
- `pub enum ScheduleKind { OneOff, Daily, Weekly }` (wire `"one_off" | "daily" | "weekly"`).
- `pub struct ScheduleRow { pub id: [u8;16], pub channel: ChannelId, pub text: String, pub kind: ScheduleKind, pub timezone: String, pub date: String, pub hour: u8, pub minute: u8, pub weekday: u8, pub enabled: bool, pub revision: u64 }`; `pub struct Schedule { channel, text, kind, timezone, date, hour, minute, weekday }`.
- `pub fn child_id(parent_hex: &str, label: &str) -> [u8; 16]` — bot-handlers.ts:31-39 (four FNV-1a 32-bit lanes seeded `0x811c9dc5 ^ (seed+1)*0x9e3779b1` over the UTF-8 of `"{parent}:{label}"`, all-zero → `…01`).
- `pub fn decode_list(data: &[u8], revision: Option<u64>, offset: u64) -> Result<(Vec<ScheduleRow>, u64 /*total*/), Invalid>` — :139-162 exactly (`[1, offset, total ≤ 128, rows ≤ 8]`, 11-field rows, ids not zero, text non-blank ≤ 1800 bytes, hour ≤ 23, minute ≤ 59, weekday ≤ 6, enabled ≤ 1, 1 ≤ revision ≤ 2^63-1; no revision → error).
- `pub fn encode_schedule(id: [u8;16], s: &Schedule, enabled: bool, item_revision: u64) -> Vec<u8>` — `[1, id, channel, text, kind, timezone, date, hour, minute, weekday, enabled, item_revision]` (:437-438; `date` is `""` unless one-off).
- `pub fn summary(row: &ScheduleRow) -> String` — `{text} — {kind}, {timezone} {HH}:{MM}` + `" (disabled)"` (:187-189).
- `pub fn confirmed(result: &BotResult, previous: u64) -> bool` — revision present and > previous, data is an empty CBOR array (:164-170).
- `impl Bot { pub async fn list_announcements(&self, community, parent: [u8;16]) -> Result<(Vec<ScheduleRow>, u64 /*list revision*/), String> }` — pages op 50 `args [Uint(offset)]`, request id `child_id(parent, "announcement:list:{offset}")`, until `offset >= total`; status 2 → "Manage Community permission is required."; other non-zero → "The announcement list is unavailable."; a page whose total or revision moved → "The announcement list changed between pages. Refresh it before editing."; empty page short of total → "The announcement list page made no progress." (:171-186). Publishes rows to `state.bot.announcements[c]` (new field) for the chip suggestions.
- `impl Bot { pub async fn save_announcement(&self, community, parent, existing: Option<&ScheduleRow>, s: &Schedule, list_revision: u64) -> Result<String, (ResultStatus, String)> }` and `delete_announcement(&self, community, parent, row: &ScheduleRow, list_revision: u64)` — op 51 `args [existing id | Null, Bytes(encode_schedule(id, s, existing.enabled != false, existing.revision+1 | 1))]`, op 52 `args [Bytes(id), Uint(row.revision)]`, revision `list_revision`, request id `child_id(parent, "announcement:save" | "announcement:delete")`, through `BotAdmin::operations(c)` (bot_admin.rs:464) so `read(request, op)` reconciles (:109-137). Results: 0 + `confirmed` → "Announcement scheduled." / "Announcement updated." / "Scheduled announcement deleted."; 0 without `confirmed` → uncertain "The Bot accepted the schedule, but its revision receipt was malformed. Refresh the list before retrying." (deletion: "The Bot accepted deletion, …"); 5 → "The Bot reported an ambiguous schedule save. Retry this same attempt to reconcile it; the schedule was not resent." (deletion: "…ambiguous deletion…; deletion was not resent."); 4 → "The schedule changed before saving. Refresh the announcement list and try again." (deletion: "…before deletion. Refresh the announcement list and choose it again."); 2 → "Manage Community permission is required."; else "The announcement could not be saved." / "…deleted." (:447-461, :418-427).

**Interfaces (D):** `pub fn validate_schedule(form: &FormValues, now: jiff::Timestamp) -> Result<Schedule, &'static str>` with `FormValues { channel: Option<ChannelId>, text: String, kind: ScheduleKind, date: String, hour: String, minute: String, weekday: String, timezone: String }`; `pub fn canonical_zone(name: &str) -> Option<String>` (`jiff::tz::db().get(name)` → its `iana_name()`); `pub fn occurrence(date: &str, hour: u8, minute: u8, zone: &str) -> Option<jiff::Timestamp>` (`None` when that local minute does not exist in the zone, as BotCommandDialogs.tsx:103-119 returns 0); `impl AnnouncementForm { pub fn begin_save(&mut self) -> Option<[u8; 16]>; pub fn end_save(&mut self, keep_open: bool) }` — the same contract as Task 26's `begin_post`/`end_post`, returning the editor's one `parent` id.

Rules (bot-handlers.ts:376-447, BotCommandDialogs.tsx:26-70):
- Every id: DM or another community → "Announcements are available only in this community."; channel bits lack `MANAGE_COMMUNITY` → "Manage Community permission is required."; catalog lacks `sealed_operation(50)` → "Announcements are unavailable." (:388).
- list → `list_announcements` → "No scheduled announcements." or rows `{id hex}: {summary}` joined by `\n`.
- delete → `announcement` arg not in the list → "That announcement is no longer available. Refresh the list and choose it again."; Task 8 question `Ask { title: "Delete this scheduled announcement?", body: summary(row) }`; cancel → no row (Task 8); confirmed → bits again, else "Manage Community permission changed before deletion."; `delete_announcement`.
- create/edit → edit with an unknown id → the "no longer available" sentence; the editor opens: title `botCommand.announcement.create|edit`; fields `botCommand.announcement.destination` (text channels `kind == 0`, first one preselected for create, placeholder `botCommand.announcement.chooseDestination`), `.message` (prefill: the `text` arg when non-blank, else the row's), `.recurrence` (`.once|.daily|.weekly`), `.date` (one-off only), `.weekday` (weekly only, `botCommand.weekday.0..6`), `.hour`, `.minute`, `.timezone` (default the system zone, `jiff::tz::TimeZone::system().iana_name()`, else "UTC"); defaults now + 60 s local; buttons `chperm.cancel`, `botCommand.save`; close `dialog.close`.
- Validation (:40-52): no channel, blank text, > 1800 bytes, hour ∉ 0..=23, minute ∉ 0..=59, weekday ∉ 0..=6, unknown zone → `botCommand.dialog.invalid`; one-off with a date not `YYYY-MM-DD`, a missing local minute, or `floor(occ/60) <= floor(now/60)` → `botCommand.dialog.futureMinute`.
- Cancel / leaving → Failed "Announcement editing cancelled." (:432); Save → bits again, else "Manage Community permission changed before saving."; `save_announcement` with one `parent` request id per opened editor; Save disabled while busy.
- Chips: `ArgKind::Announcement` offers the published rows matching id or `summary` (≤ 25, label first 100 chars of text, sub `summary`, :571-575); opening that chip runs `list_announcements` once when `state.bot.announcements[c]` is absent.

- [ ] Step 1: Failing tests (C): `child_id_matches_the_web` — `child_id("00112233445566778899aabbccddeeff", "announcement:save")` hex == `"0c063b0494e2958b2059b29ef049dae5"`, `"announcement:list:0"` → `"196ce6cfc39bc650f796a7e9b537d02a"` (computed with the web's function); `decode_list_refuses_what_the_web_refuses` (wrong offset, total 129, 9 rows, hour 24, empty text, missing revision → `Err`; one valid row → `Ok((rows, 1))`); `encode_schedule_is_the_web_array` (decode the bytes back: 12 elements, `date == ""` for daily); `save_reconciles_with_the_same_request` (fake transport answering status 5 first, then 0 with revision 8 and `[]` on `read`: the second call sends the same request id, result "Announcement scheduled."). (D): `validate_schedule_follows_the_dialog` — hour "24" → `"botCommand.dialog.invalid"`; zone "Mars/Base" → invalid; one-off `2026-03-29 02:30` in `Europe/Stockholm` (a DST gap) → `"botCommand.dialog.futureMinute"`; a date in the past → futureMinute; D `a_second_save_while_busy_sends_nothing` (Review Focus 2): `begin_save() == Some(parent)`, again `None`, after `end_save(true)` `Some(parent)` (same id); `canonical_zone("europe/stockholm")` → `Some("Europe/Stockholm")` iff jiff's db is case-insensitive, else assert `None` and keep the input verbatim — assert whichever jiff 0.2.38 does and say so in the test's comment.
- [ ] Step 2: `$ENV cargo test -p dilla-client --lib features::bot::announcements` and `$ENV cargo test -p dilla-desktop --lib screens::commands::announcement_form` fail.
- [ ] Step 3: Implement the C module, the D form (per-window like Task 26; Decision D2: U `Dialog` with `Select` for destination/recurrence/weekday, `TextField` for message (multi-line), date, hour, minute and timezone, in the web's field order and with its labels and copy, BotCommandDialogs.tsx:56-70), the `on_send` intercept for the four ids, the chip suggestions. `parent` is one fresh id per opened editor here; Task 30 replaces it with the remembered attempt id.
- [ ] Step 4: the two commands; `$ENV cargo test -p dilla-desktop --test bot an_announcement_scheduled_from_the_editor_is_listed -- --ignored` (dillad with the Bot: create → "Announcement scheduled."; `/announcement list` → a row ending `— daily, UTC 09:00`; delete with confirmation → "Scheduled announcement deleted.").
- [ ] Step 5: Decision D2: a GPUI-only reference `native/announcement-editor` (the web's are browser widgets): `tests/bot.rs::parity_announcement_editor` calls `assert_native_screen` (Task 1) on a new GPUI-only fixture `P/fixtures/commands/announcement-editor.json` (the world of polls/card.json with its Bot person, viewer Ada as owner; steps type `/announcement create` into `composer`, `key enter`, `settle`); looked at by eye against BotCommandDialogs.tsx's field order and copy, blessed with `DILLA_PARITY_BLESS=1`, added to the README `native/` table. No frozen gate title covers announcements, so no BEHAVIOUR-GATES change.
- [ ] Step 6: Commit `desktop: scheduled announcements list, create, edit and delete from the composer`.

### Task 29: `/rolepanel create` opens the role-panel editor [C9]

**Size:** M

Publishing works from Server settings → Self roles (D/screens/members/self_roles.rs:593 → `RolePanels::publish_job`,
C/features/role_panels.rs:495); the command has no dialog (web: W/screens/commands/bot-handlers.ts:466-540, BotCommandDialogs.tsx:73-93).

**Files:** Create D/screens/commands/role_panel_form.rs; Modify D/screens/commands/mod.rs (intercept `rolepanel create`), D/slots.rs overlays; Test D/screens/commands/role_panel_form.rs tests, D/tests/bot.rs.

**Interfaces:** consumes `Roles::load_self_roles` (C/features/roles.rs:1294), `RolePanels::publish_job(community, channel, options, request_id)` and `panel_valid` (role_panels.rs:454), `PanelError`; produces `pub fn options_of(selected: &[(RoleId, String, bool)], emojis: &HashMap<RoleId, String>) -> Vec<PanelOption>` and a per-window `RolePanelForm { channel: Option<ChannelId>, roles: Vec<SelfRole>, selected: HashSet<RoleId>, emojis: HashMap<RoleId, Entity<InputState>>, error: Option<String>, busy: bool, request: [u8;16], row: Option<u64>, .. }` with `begin_publish(&mut self) -> Option<[u8; 16]>` / `end_publish(&mut self, keep_open: bool)` (Task 26's contract). Consumes Task 2's `check_box(id, checked, disabled)`.

Rules:
- Opening: DM/other community → "Role panels are available only in this community."; bits lack `MANAGE_ROLES` → "Manage Roles permission is required."; catalog lacks `sealed_operation(31)` → "The Bot installation is unavailable for role panel publication."; `load_self_roles` empty → "Approve at least one self-service role before creating a panel."; conversation or bits changed while loading → "The community or permission changed before opening the role panel." (:470-479).
- Editor: title `botCommand.rolepanel.title`; destination as Task 28 (text channels `kind == 0`, first preselected); fieldset `botCommand.rolepanel.roles` with Task 2's 22px `check_box` per self role (the web's checkbox, Decision D20; ids `rolepanel-role-<hex>`) and its emoji `TextField` labelled `botCommand.rolepanel.emojiFor` (`{role}`), at most 16 chars typed (maxLength 16); buttons `chperm.cancel`, `botCommand.rolepanel.publish`. Invalid (no channel, no role, `!panel_valid`) → `botCommand.rolepanel.invalid` (:79-84).
- Publish: bits again → else "Manage Roles permission changed before publication."; channel still a text channel of the community → else "Choose a text channel that still exists in this community."; `load_self_roles` again, every chosen role still listed with its name → else "A selected role is no longer approved for self-service. Reopen the role panel editor and choose current roles." (:481-486); pending row; `publish_job(.., request)` with one request id per opened form (Task 30 replaces it with the remembered attempt id): Ok → "Role panel published and registered."; `Uncertain` → Failed "The role panel publication result is not confirmed. Retry this command to reconcile it."; `Refused("E_FORBIDDEN")` (status 2) → "Manage Roles permission is required."; other `Refused`/`Invalid` → `mutation_error`-style "Command was refused ({code})." (:503-509, `resultError` :17-29).
- Cancel / leaving → Failed "Role panel creation cancelled." (:480).

- [ ] Step 1: Failing tests (D): `role_panel_options_keep_the_chosen_roles_in_list_order` (three roles, two checked, emojis "🎮"/"🎨" → two `PanelOption`s in the list's order, names kept); `an_emoji_repeated_or_missing_is_invalid` (`panel_valid(&options_of(..))` false for "🎮"/"🎮" and for ""); `the_editor_refuses_without_manage_roles` (plan of `/rolepanel create` in a state without bit 15 → Failed "Manage Roles permission is required."); `a_second_publish_while_busy_sends_nothing` (Review Focus 2): `begin_publish() == Some(R)`, again `None`, after `end_publish(true)` `Some(R)`.
- [ ] Step 2: `$ENV cargo test -p dilla-desktop --lib screens::commands::role_panel_form` fails.
- [ ] Step 3: Implement the form and the intercept (fixture: opens with `client: None` from `state.roles.self_roles`; Publish does nothing).
- [ ] Step 4: `$ENV cargo test -p dilla-desktop --lib screens::commands::role_panel_form`; `$ENV cargo test -p dilla-desktop --test bot a_role_panel_published_from_the_command_registers -- --ignored` (dillad with the Bot: approve Crew, `/rolepanel create`, check Crew, emoji "🎮", Publish → "Role panel published and registered."; Bea's reaction 🎮 on the panel grants Crew).
- [ ] Step 5: Decision D2: a GPUI-only reference `native/rolepanel-editor`: `tests/bot.rs::parity_rolepanel_editor` calls `assert_native_screen` on a new GPUI-only fixture `P/fixtures/commands/rolepanel-editor.json` (Ada owner, one approved self role, the Bot person in the world; steps type `/rolepanel create`, `key enter`, `settle`), drawn with U `Dialog`, `Select` (destination), a 22px `check_box` per self role (D20) and a `TextField` for its emoji (BotCommandDialogs.tsx:73-93 order and copy), looked at by eye, blessed with `DILLA_PARITY_BLESS=1`, added to the README `native/` table. Append `dilla-desktop::tests/bot::a_role_panel_published_from_the_command_registers` to the BEHAVIOUR-GATES row "a human publishes a bot-authored self-service role panel and its reaction grant keeps its own source".
- [ ] Step 6: Commit `desktop: /rolepanel create opens the role panel editor and publishes it`.

### Task 30: Command memory: an uncertain run keeps its request id and reopens its draft [C10]

**Size:** L

The web remembers, per conversation, the last command draft and its request id (CommandHost.tsx:24, :48-51, LIMIT 128 :28).
A run takes the remembered id when the invocation is the same one, else a new id (:243-246). A definitive answer
(success or failure) retires the id (:285-288). An uncertain answer keeps it and reopens the draft in the composer, or
keeps it ready for when the person comes back to that conversation (:289-298, :123-129). The handlers that write use the
id so the resend reconciles instead of acting twice: `publish` (builtin.ts:39-58), `/timeout` (:217-243), `/untimeout`
(:256), `/warn` (:267), `/selfrole` (:428), `poll create` (UtilityCommandProvider.tsx:355-396), announcements
(bot-handlers.ts:107-137, :378-460), and the role panel (bot-handlers.ts:466-540). GPUI today mints a fresh id for every
run (Tasks 8, 9, 10, 11, 24, 25, 26, 28, 29 [C1–C9]). This task ports the memory and those attempt stores. Bot catalog commands keep their existing row Retry
(C/features/bot/run.rs:402, :568), which already resends the held id; they are not changed here.

**Files:**
- Create: C/features/bot/memory.rs (`pub mod memory;` in C/features/bot.rs).
- Modify: C/features/bot/sealed.rs (hold `memory: Arc<Mutex<memory::CommandMemory>>` beside the pins; `forget_all` empties it, so sign-out drops it as it drops the held runs, run.rs:962).
- Modify: C/features/bot.rs (`Bot::memory`, `invocation_key`, `Outcome`, `Attempt`, `Bot::settle`).
- Modify: C/features/roles.rs:1423 (`select_self_role` gains `request: [u8; 16]`; its caller D/screens/members/self_roles.rs:372 passes `request_id()`).
- Modify: C/features/utilities.rs (`poll_by_id`).
- Modify: C/features/bot/announcements.rs (Task 28: `run_mutation` over the attempt store).
- Modify: D/screens/commands/mod.rs (`on_send` builds the attempt; `reopen`, `take_reopen`; `PickerState.reopen`), D/screens/commands/builtins.rs (runs end through `Bot::settle`), D/screens/commands/poll_form.rs, announcement_form.rs, role_panel_form.rs, D/screens/composer.rs (load at :294-304, `render`).
- Test: C/features/bot/memory.rs `mod tests`; C/features/bot.rs tests; D/screens/composer.rs tests.

**Interfaces (C, memory.rs):**
- `pub const LIMIT: usize = 128;` (CommandHost.tsx:28; also each store's cap, builtin.ts:57, :232, bot-handlers.ts:124).
- `pub struct Saved { pub text: String, pub request: [u8; 16], pub invocation: String, pub reopen: bool }`.
- `pub struct CommandMemory { rows: VecDeque<(Conversation, Saved)>, pub publications: Publications, pub timeouts: TimeoutAttempts, pub polls: HashMap<[u8; 16], PollAttempt>, pub announcements: HashMap<([u8; 16], CommunityId), AnnouncementAttempt>, pub role_panels: HashMap<([u8; 16], CommunityId), RolePanelAttempt> }` (each store evicts its oldest past `LIMIT`).
- `impl CommandMemory`:
  - `pub fn attempt(&mut self, at: Conversation, invocation: &str, text: &str) -> [u8; 16]` — same `invocation` as the row → its `request`, else `new_request_id()`; stores `reopen: false` (the draft leaves the composer while it runs, `leaveAfterRun` :198-205); the row moves to the newest place (`remember`, :48-51).
  - `pub fn settled(&mut self, at: Conversation, request: [u8; 16])` — when the row's `request == request`: a new id and `invocation: ""` (:285-288).
  - `pub fn uncertain(&mut self, at: Conversation, text: &str, request: [u8; 16], invocation: &str)` — the row becomes `{ text, request, invocation, reopen: true }` (:289-292).
  - `pub fn take_reopen(&mut self, at: Conversation) -> Option<String>` — the text once while `reopen`, then `reopen: false` (id and invocation stay).
- `pub struct Publications(VecDeque<String>)`: `pub fn key(request, channel, thread: Option<MessageId>, reply_to: Option<MessageId>, text: &str) -> String` (builtin.ts:41: the five parts joined by `\0`, hex ids, `""` for none), `contains`, `insert` (move to newest, cap `LIMIT`).
- `pub struct TimeoutAttempt { pub community: CommunityId, pub user: UserId, pub expires: u64, pub seconds: u64, pub settled: bool }`; `pub struct TimeoutAttempts(HashMap<[u8; 16], TimeoutAttempt>)` with `check(&self, request, community, user, seconds) -> Result<Option<&TimeoutAttempt>, &'static str>` (another target or length → `Err("This timeout request ID already belongs to a different timeout.")`, builtin.ts:222-224) and `begin(&mut self, request, attempt) -> Result<(), &'static str>` (at `LIMIT`: drop settled ones; still full → `Err("Reconcile a pending timeout before starting another timeout command.")`, :232-235).
- `pub struct PollAttempt { pub at: Conversation, pub community: CommunityId, pub bot_user: UserId, pub value: PollCreate }` (the web's `scope` and `installationId`; the GPUI view holds no installation id, so the pinned `bot_user` of `bot.sealed().view(c)` stands for it).
- `pub struct AnnouncementAttempt { pub operation: u64 /* 51 | 52 */, pub request: [u8; 16], pub args: Vec<BotArg>, pub revision: u64, pub sent: bool }` (bot-handlers.ts:78).
- `pub struct RolePanelAttempt { pub channel: ChannelId, pub options: Vec<PanelOption>, pub sent: bool }`.

**Interfaces (C, elsewhere):**
- bot.rs `pub fn invocation_key(state: &AppState, community: Option<CommunityId>, draft: &str) -> Option<String>` — for `Parsed::Invocation { command_id, args }`: `command_id`, then each `name=value` of `args` (`Args` is a `BTreeMap`, so sorted), joined by `\0`; `None` otherwise. (The web's key is `contextKey + commandId + JSON(args)`, :243; its `contextKey` is this memory's scope: one memory per client, rows per `Conversation`.)
- bot.rs `pub enum Outcome { Success(String), Failure(String), Uncertain(String) }`; `pub struct Attempt { pub request: [u8; 16], pub invocation: String, pub text: String }`.
- bot.rs `impl Bot { pub fn memory(&self) -> Arc<Mutex<CommandMemory>>; pub fn settle(&self, at: Conversation, row: u64, attempt: &Attempt, outcome: Outcome) }` — Success → `finish(.., Success, text)` + `settled`; Failure → `finish(.., Failed, text)` + `settled`; Uncertain → `finish(.., Failed, text)` + `uncertain(at, &attempt.text, attempt.request, &attempt.invocation)` (an uncertain row reads failed, :280-284).
- utilities.rs `pub async fn poll_by_id(&self, community, id: [u8; 16], channel: ChannelId, thread: Option<MessageId>) -> Result<PollView, UtilityError>` — op 71 with `Text(hex(id))`, decoded as `read_poll` does (utilities.rs:193); a record for another channel or thread → `Invalid` (the web's `pollGet(requestId, {channelId, threadId})`, :369: a poll's id is its create request id).

**Interfaces (D):** `on_send` (Task 8) builds `Attempt { request: memory.attempt(at, &key, draft), invocation: key, text: draft.to_owned() }` for `Plan::Builtin` and the form intercepts (Tasks 26, 28, 29 [C7–C9]), and passes it to `builtins::run(run: Builtin, row: u64, attempt: Attempt, conversation: Conversation, ctx: &SlotCx, window: WindowId, cx: &mut App)` (Task 8's signature with `attempt` inserted after `row`) and to the forms, whose `request` becomes `attempt.request` (so `begin_post`/`begin_save`/`begin_publish` return the remembered id). `pub fn reopen(window: WindowId, at: Conversation, text: String, cx: &mut App)` sets `PickerState.reopen = Some((at, text))` when `at` is the window's open conversation; `pub fn take_reopen(window: WindowId, at: Conversation, cx: &mut App) -> Option<String>`.

Rules (each run ends with `Bot::settle`; "Uncertain" is `Outcome::Uncertain`, every other end Success/Failure):
- Every uncertain sentence Tasks 10, 11, 24, 25, 26, 28, 29 [C3–C9] already produce is `Outcome::Uncertain`: `mutation_error_of`'s code-less sentence; `/reply`'s "The send result is uncertain…"; `/delete`'s "Message removal outcome is uncertain…" and every `RemovalStep::Done { uncertain: true, .. }` (Task 10); `/join`'s cancelled-join sentence and "Voice join is still pending…"; `poll.createdUncertain`; the announcement status-5 and malformed-receipt sentences; the role panel's `Uncertain` sentence.
- Reopen: on Uncertain, `reopen(window, at, attempt.text)` while the window still shows `at` (`isCurrent()`, :293); the composer's `render` takes it (`set_text(&text, None, ..)`, then `commands::draft_changed`), so the picker opens on the same draft. For a conversation the person left, `CommandMemory::take_reopen` when the composer next loads that channel (composer.rs:294-304) wins over the `drafts.<channel>` pref (:123-129: an `active` row restores its text).
- `/reply` (publish, builtin.ts:39-58): `Publications::key(request, channel, None, Some(to), text)` already present → Uncertain "This send attempt has no returned message ID. Check the conversation and outbox before starting another send." with nothing sent; an uncertain send inserts the key, then answers the existing uncertain sentence. (`/me /shrug /tableflip /unflip /code` stay `Plan::Post` through the composer's outbox, as today.)
- `/timeout` (builtin.ts:217-243): `check` → `Err(s)` → Failure `s`; `Ok(Some(attempt))` → no question and no send: `timeout_of(c, u)` (moderation.rs:369) `== Some(attempt.expires)` → settled, Success `{name} was timed out.`; else Uncertain "The current timeout differs from this attempt. It may have expired or another moderator may have changed it. Review the current timeout before starting a new command.". `Ok(None)` → Task 11's question, then `begin(request, { expires: now_s() + seconds, .. })` (Err → Failure), `timeout_for(c, u, request, Some(expires))`; Ok → settled; a definitive `mutation_error_of` (a code) → remove the attempt.
- `/untimeout`, `/warn`, `/selfrole add|remove` send `attempt.request` (`timeout_for(c, u, request, None)`, `warn(c, u, request, &reason)`, `select_self_role(c, role, add, request)`) in place of a fresh `request_id()` (builtin.ts:256, :267, :428).
- `poll create` (UtilityCommandProvider.tsx:355-396), with a prior `polls[request]`: another `at` or `community`, the creation checks failing, or another `bot_user` → Uncertain `poll.createdUncertain`; `BotOperations::read(request, 70)` (bot_admin.rs:318) status 4 → remove, Failure "Poll creation was rejected. Start a new poll command."; `poll_by_id` equal in question, answers and multi and published (`pollPublished`, :59-66: an Ok, unedited `item.bot` post from `bot_user` whose last line is `[dilla-poll:v1:{request hex}]`) → remove, toast + Success `poll.created`; a read with status 0, or `Some` and not 5, or an Err → Uncertain; otherwise resend the saved `value` without opening the form (:376-377: a retained mutation is immutable). No prior: insert just before `create_poll`; Ok → remove; `Status(4)`, or any Err other than `Uncertain` → remove (:393-394).
- Announcements (bot-handlers.ts:378-460): Task 28's `parent` is `attempt.request`. A sent `announcements[(request, c)]` with bits lacking `MANAGE_COMMUNITY` → Uncertain "This schedule attempt is still pending. Restore Manage Community permission in its original community to reconcile it."; a sent attempt skips the catalog check and the editor and reconciles its own operation through `run_mutation` with its stored args and revision: 0 + `confirmed` → remove, the op's success sentence; 0 without → Uncertain "The Bot accepted the schedule change, but its revision receipt was malformed. Refresh the list before retrying."; 5 → Uncertain "The Bot reported an ambiguous schedule mutation. Retry this same attempt to reconcile it; the schedule was not resent."; 4/2/other → remove, Task 28's sentences. `run_mutation` (:107-137): an attempt with another operation or revision: not sent or another op → Uncertain "This announcement attempt is bound to a different schedule revision. Reconcile it before starting another command."; else `read`, `None` → Uncertain "The prior announcement result is still pending.". A sent one → `read`: Err(e) → Uncertain `Could not reconcile the prior schedule mutation: {e}`; `None` → Uncertain "The prior announcement result is still pending. Reconcile this same request before editing the schedule again."; a refusal removes it. Any other error while an attempt is sent → Uncertain "This schedule mutation may have been sent. Its original request and body remain reserved; retry this same request to reconcile it." (:455-458).
- Role panel (bot-handlers.ts:466-540): a sent `role_panels[(request, c)]` with bits lacking `MANAGE_ROLES` → Uncertain "This role panel attempt is still pending. Restore Manage Roles permission in its original community to reconcile publication or registration."; a sent one skips the editor and runs `publish_job(c, attempt.channel, attempt.options, request)` (it reconciles by request id, role_panels.rs:513-516); `Uncertain` → keep, Task 29's uncertain sentence; Ok or a refusal → remove. A new attempt is inserted (`sent: true`) just before `publish_job`.

- [ ] Step 1: Failing tests.
  C memory.rs `an_uncertain_attempt_keeps_its_request_and_reopens`: `let r1 = m.attempt(at, K, T)` with `K = "timeout\0duration=10m\0user=a"`, `T = "/timeout user:a duration:10m"`; `m.attempt(at, K, T) == r1`; `m.settled(at, r1)`; `let r2 = m.attempt(at, K, T); assert_ne!(r2, r1)`; `m.uncertain(at, T, r2, K)`; `m.attempt(at, K, T) == r2`; `m.uncertain(at, T, r2, K); m.take_reopen(at) == Some(T.into())`, a second `take_reopen` is `None`; `m.attempt(at, "timeout\0duration=10m\0user=b", ..) != r2`; after 128 other conversations, `at`'s row is gone.
  C memory.rs `timeout_attempts_bind_their_request`: `begin(r, {c, bea, 600, ..})` Ok; `check(r, c, bob, 600)` → `Err("This timeout request ID already belongs to a different timeout.")`; `check(r, c, bea, 600)` → `Ok(Some(_))`; 128 unsettled → `begin` → `Err("Reconcile a pending timeout before starting another timeout command.")`; one settled → `begin` Ok.
  C memory.rs `a_publication_is_remembered_per_attempt`: `key(r, ch, None, Some(m), "x")` inserted → `contains`; 129 keys → the first is gone.
  C bot.rs `invocation_key_reads_the_parsed_arguments`: `"/timeout user:<a> duration:10m"` and `"/timeout duration:10m user:<a>"` give the same key; `duration:1h` another; `"hello"` → `None`.
  D composer.rs gpui test `an_uncertain_command_reopens_its_draft`: a composer on channel A; `commands::reopen(w, Conversation { channel: A, thread: None }, "/warn user:x reason:y".into(), cx)`; after a draw, `value()` is that text. Then, with `memory.uncertain(Conversation { channel: B, .. }, "/topic text:z", ..)` and a `drafts.B` pref "old", switching the composer to B shows "/topic text:z", not "old".
- [ ] Step 2: `$ENV cargo test -p dilla-client --lib features::bot` and `$ENV cargo test -p dilla-desktop --lib screens::composer` fail to compile.
- [ ] Step 3: Implement memory.rs and the C interfaces; `select_self_role`'s request parameter; `poll_by_id`.
- [ ] Step 4: Implement the D side: `on_send` builds the `Attempt`; `builtins::run`, the three forms and the Task 28 calls end through `Bot::settle` with the rules above; `reopen`/`take_reopen` and the composer's two hooks.
- [ ] Step 5: `$ENV cargo test -p dilla-client --lib -- features::bot features::utilities features::roles`; `$ENV cargo test -p dilla-desktop --lib -- screens::commands screens::composer`; `$ENV cargo test -p dilla-desktop --test bot -- --ignored` (Tasks 10, 11, 24, 25, 26, 28, 29 [C3–C9]'s dillad tests still pass: a definitive answer retires the id).
- [ ] Step 6: No new visible state (the reopened draft is the existing composer and picker); no gate row (no e2e title covers a resend).
- [ ] Step 7: Commit `desktop: an uncertain command keeps its request id and reopens its draft, so a resend reconciles`.

### Task 31: The thread composer runs slash commands [X5]

**Size:** L

The web's behaviour: one `CommandHost` serves both composers. The thread binding adds `threadId` (ThreadView.tsx:236-239, ComposerArea.tsx:292,
CommandHost.tsx:84-86).
- **Same commands.** The thread offers the same list and chips. The picker does not filter on `threadId`.
- **Message lookups.** These (`exactMessage`, builtin.ts:62-64) read the thread's replies without the root (ThreadView.tsx:94, :237 `items`).
- **Posts.** `/me`, `/shrug`, `/code`, `//x`, a built-in's text and `/reply` all go into the thread (`targetThread`, builtin.ts:24, :47;
  ComposerArea.tsx:222).
- **`/pin` and `/unpin`.** After the not-found check, these refuse with "Messages in threads cannot be pinned from the command composer."
  (builtin.ts:434-435).
- **Private results.** These are kept per thread (`…/{channel}/{root}`, CommandHost.tsx:74, ThreadView.tsx:44). They are drawn after
  the replies (ThreadView.tsx:243-244).
- **Bot commands.** The Bot command itself does not carry the thread (bot-run.ts has none). Its result row is in the thread.
- **`/poll create`.** This carries the thread (UtilityCommandProvider.tsx:379, :391).

**Files:**
- Modify C/features/bot.rs: add `plan_at`, `exact_message_in`, and the `/pin` thread refusal.
- Modify C/features/threads.rs:302: add `reply_in_thread`.
- Modify D/screens/commands/mod.rs: `on_send_in`, `private_results_in`, and the owner gate in `picker`.
- Modify D/screens/commands/chips.rs:995: the owner gate in `view`.
- Modify D/screens/commands/builtins.rs (Task 8): `Reply` in a thread.
- Modify D/screens/commands/poll_form.rs (Task 26): `PollCreate.thread` from the conversation.
- Modify D/slots.rs:223-230: `composer_popup` and `composer_chips` take the composer's focus. Modify D/screens/composer.rs:784-786 to pass it.
- Modify D/screens/threads/view.rs:141-205 and :359-535: the Enter path, the popup, the chips, and the private rows.
- Test: C/features/bot.rs tests. D/screens/threads/view.rs tests. D/tests/threads.rs.

**Interfaces:**
- Produces (C): `pub fn plan_at(state: &AppState, community: Option<CommunityId>, conversation: Option<Conversation>, draft: &str) -> Plan`.
  Task 8's `plan_in(s, c, ch, d)` becomes `plan_at(s, c, ch.map(|channel| Conversation { channel, thread: None }), d)`.
- Produces (C): `pub fn exact_message_in<'a>(state: &'a AppState, conversation: Conversation, id: &str) -> Option<&'a TimelineItem>`.
  With `thread: Some(root)` it searches `state.threads.threads[&root].replies` where `data.channel == conversation.channel`.
  Otherwise it is Task 8's `exact_message`, which now delegates to it.
- Produces (C): `pub fn reply_in_thread(&self, channel: ChannelId, root: MessageId, to: MessageId, text: String) -> Done<MessageId>`,
  which is `self.send(channel, Some(to), Some(root), text)`.
- Produces (D): `pub fn on_send_in(ctx: &SlotCx, thread: Option<MessageId>, draft: &str, encode: &dyn Fn(&str) -> String, window: WindowId, cx: &mut App) -> SendDecision`.
  Task 8's `on_send` delegates with `None`. Every `Conversation { channel, thread: None }` inside it takes `thread`.
- Produces (D): `pub fn private_results_in(ctx: &SlotCx, thread: Option<MessageId>, width: f32, window, cx) -> Vec<(AnyElement, f32)>`.
  `private_results` delegates with `None`.
- Consumes composer Task 30: `on_send_in` builds the `Attempt` with `memory.attempt(Conversation { channel, thread }, ..)`, so the thread's command memory is its own row (CommandHost.tsx:92: the key ends in the thread id); `reopen`/`take_reopen` take that `Conversation`, and the thread composer restores a reopened draft as the channel composer does (Task 30's two hooks: its `render` and its load of the thread's draft, view.rs:212-217). `Publications::key` gets `Some(root)` for the thread (builtin.ts:41).
- Produces (D): `commands::picker(ctx, owner: &FocusHandle, window, cx)` and `chips::view(ctx, owner, window, cx)`. Each returns `None`
  unless `picker_state(wid).composer` upgrades to `owner`, so only the composer that reported the draft draws the list.

- [ ] Step 1: Write the failing tests.
  - C `thread_commands_bind_to_the_thread`. The state has channel `ch`, an open thread `root` with own reply `R` (pinned: false), and
    channel bits with `PIN_MESSAGES`.
    - `plan_at(.., Some(Conversation { channel: ch, thread: Some(root) }), "/pin message:<R hex>")` gives
      `Plan::Private(Failed, "Messages in threads cannot be pinned from the command composer.", Builtin)`.
    - The same with the id of a channel-only message gives "That message is not available in this channel.".
    - `"/reply message:<R hex> text:x"` gives `Plan::Builtin { run: Builtin::Reply { to: R, .. }, confirm: None }`.
    - With `thread: None`, `"/reply message:<R hex> text:x"` gives "That message is not available in this conversation.".
  - D view.rs inline `the_list_belongs_to_the_composer_that_reported_the_draft`, a gpui test.
    - `set_composer(w, &thread_focus)` then `draft_changed(w, "/", ..)`: `picker(ctx, &channel_focus, ..)` is `None` and
      `picker(ctx, &thread_focus, ..)` is `Some`.
  - D view.rs inline `an_uncertain_thread_command_reopens_in_the_thread`, a gpui test: `commands::reopen(w, Conversation { channel: ch, thread: Some(root) }, "/pin message:x".into(), cx)`;
    after a draw the thread composer's value is `"/pin message:x"` and the channel composer's is unchanged.
  - D tests/threads.rs (fixture driver, pattern :71 `thread_pane_opens_in_the_aside_with_its_composer_unfocused`)
    `a_slash_in_the_thread_composer_opens_the_list_over_it`. Open `threads/pane-open.json`, focus `thread-composer`, and type `/`.
    `command-picker` is present, and its box is above the `thread-composer` box. Type `shrug`, press Enter, and Escape out of the chips.
    No `command-picker` stays.
  - D tests/threads.rs `#[ignore]` (dillad) `a_thread_command_posts_into_the_thread`. Ada types `/shrug hi` in the thread composer, and
    the thread's replies gain `"hi ¯\\_(ツ)_/¯"` while the channel timeline does not. `/pin message:<reply>` shows `bot-result-failed`
    in the pane.
- [ ] Step 2: `$ENV cargo test -p dilla-client --lib features::bot::tests::thread_commands` and
      `$ENV cargo test -p dilla-desktop --lib threads::view` fail to compile.
- [ ] Step 3: Implement the C side.
  - `plan_at` and `exact_message_in`.
  - The `/pin` and `/unpin` arm: first `exact_message_in` gives the not-found sentence. Then `conversation.thread.is_some()` gives the
    thread sentence. Then Task 10's bits check.
  - `reply_in_thread`.
- [ ] Step 4: Implement the D side.
  - **Owner gates.** `picker` and `chips::view` check `owner`. `slots::composer_popup(ctx, focus, ..)` and `composer_chips(ctx, focus, ..)`
    pass the field's focus. The channel composer passes `self.input.read(cx).focus_handle(cx)`.
  - **Thread composer Change.** The `InputEvent::Change` arm calls `commands::set_composer(w, &focus)` and `draft_changed(w, &value)`,
    as composer.rs:317-329 does.
  - **Thread composer Enter.** Start from `chips::draft` when chips show (composer.rs:479-486). Call
    `on_send_in(&ctx, Some(open.root), &draft, &|t| t.to_owned(), w, cx)`:
    - `Post(text)`: today's `send_in_thread(open.channel, open.root, text)` path.
    - `Handled`: clear the field and the thread's draft.
    - `Keep`: return.

    The thread composer encodes no mentions today, so `encode` is the identity.
  - **Pane drawing.** The popup from `slots::composer_popup(&ctx, &focus, ..)` goes above the composer box (absolute, bottom-anchored,
    `PICKER_INSET_X`). Chips from `composer_chips` take the field's place; the `Textarea` stays in the tree at zero size, as
    composer.rs:902-905 does. `private_results_in(&ctx, Some(open.root), reply_width, ..)` rows go after the replies in `PaneParts.replies`.
  - **Builtins.** In builtins.rs, `Builtin::Reply` uses `client.threads().reply_in_thread(channel, root, to, text)` when
    `conversation.thread` is `Some(root)`. Task 26's form fills `PollCreate.thread` from `conversation.thread.map(|m| m.0)`.
  - **Memory (Task 30).** The thread composer's `render` takes `commands::take_reopen(w, Conversation { channel, thread: Some(root) }, cx)`
    and its draft load prefers `memory.take_reopen` for that conversation over `client.threads().draft(root)`.
- [ ] Step 5: Run these and expect them to pass:
  - `$ENV cargo test -p dilla-client --lib features::bot`
  - `$ENV cargo test -p dilla-desktop --lib screens::commands threads::`
  - `$ENV cargo test -p dilla-desktop --test threads`
  - `$ENV cargo test -p dilla-desktop --test threads a_thread_command -- --ignored`
  - `$ENV cargo test -p dilla-desktop --test bot`, to check that nothing in the channel composer changed.
- [ ] Step 6: Parity.
  - Add `threads/command-list.json` and `command-list-b.json`: the world of `threads/pane-open.json`, then the steps `focus thread-composer`
    (web: `css=.d-thread-pane__composer .d-composer__input`), type `/`, and `settle`.
  - Shoot both, add `parity_threads_command_list` in tests/threads.rs, and add them to the README table.
  - Append `dilla-desktop::tests/threads::a_thread_command_posts_into_the_thread` to the BEHAVIOUR-GATES row "polls publish in channels
    and threads, …" (it is the thread binding of the same composer).
- [ ] Step 7: Commit `desktop: the thread composer runs slash commands in its thread`.

### Task 32: The empty channel's note, and "device" in place of "browser" [V3]

**Size:** M

The web shows `shell.log.empty` under the welcome when a community channel's loaded log is empty. It shows
`shell.log.emptyMade` instead when this client made the channel in this session, and nothing in a DM
(W/screens/conversation/Conversation.tsx:394-395, `madeHere` W/screens/Create.tsx:65-75). The web remembers the made
channels at Create.tsx:160 (a new server's starter channels) and :368 (Create channel). The CSS:
`.d-message-log__welcome + .d-message-log__empty { margin: 0; padding: 0 1.25rem 0.5rem; text-align: left }`, with
`font-size: var(--text-sm); color: var(--fg-3)` (packages/ui/src/MessageLog/MessageLog.css:10,15). GPUI draws no note.
desktopOverrides.ts has no `shell.log.empty`, so the GPUI string would still say "browser".

**Files:** Modify W/strings/desktopOverrides.ts (add the key under "The app, its window…") · Modify scripts/gen-gpui-strings.mjs
(the guard below) · regenerate D/strings.rs · Modify C/state/mod.rs (`AppState.made_channels: BTreeSet<ChannelId>`) ·
Modify C/features/signup.rs `create_channel_with` (insert on `Ok`) and the server-creation path that makes starter channels ·
Modify D/screens/timeline.rs (the welcome placement, near `Placer::welcome` :561, and the height sum) · Modify D/../tests/ring.rs:1147.
**Interfaces:** Produces `fn empty_note(state: &AppState, channel: ChannelId, dm: bool, items: usize, loaded: bool) -> Option<&'static str>`
in timeline.rs. It returns the key, or `None` for a DM, a non-empty log or one still loading.
- [ ] Step 1: Failing tests.
      - `timeline::tests::an_empty_channel_says_older_messages_are_not_shown`: `empty_note(&s, ch, false, 0, true) == Some("shell.log.empty")`.
        Insert `ch` into `s.made_channels` and expect `Some("shell.log.emptyMade")`. `dm == true` gives `None`.
        `items == 1` gives `None`. `loaded == false` gives `None`.
      - `strings::tests::the_empty_log_says_device`: `t("shell.log.empty") == "nothing here yet. Messages sent before this device joined are not shown."`
        (the wording follows `onboarding.done.note`'s override).
      - C `signup::tests::a_made_channel_is_remembered`, following the fake-API pattern of the other create_channel tests.
- [ ] Step 2: `$ENV cargo test -p dilla-desktop --lib timeline::tests::an_empty strings::tests::the_empty_log` and `$ENV cargo test -p dilla-client --lib signup::tests::a_made` fail.
- [ ] Step 3: Add the override. Then `node scripts/gen-gpui-strings.mjs`. Add a refusal to the generator's `--check`: a key
      whose `en` value matches `/browser/i` and that is quoted in D/src outside strings.rs must have an override or be
      in `browserMeansBrowser = ['devices.platform.browser', 'devices.platform.browserUnknown', 'devices.renameSuggestion']`.
      Implement `empty_note` and draw the note as one `paragraph(text::SM, line_height(text::SM, 1.5), color::FG_3)` at
      `WELCOME_X`, right under the welcome, with 8px below. Add that height to the log's content height.
- [ ] Step 4: Step 2's commands pass. `node scripts/gen-gpui-strings.mjs --check` passes.
- [ ] Step 5: `npm run build:desktop-strings -w @dilla/web`, then re-shoot `ring/banner` (no `--out`, commit the PNG and
      sidecar). Remove the ignore on `parity_ring_banner`. `$ENV cargo test -p dilla-desktop --test ring parity_ring_banner`
      passes. Re-shoot any other committed reference with an empty channel (find them with
      `grep -l "this browser joined" P/references/linux/**/*.layout.json` from a `--out` run, or by eye).
- [ ] Step 6: Commit `desktop: an empty channel says older messages are not shown on this device`.

### Task 33: Settings pages: switches, wraps, keycaps, buttons, meters [V6]

**Size:** M

Each line is one fix. Take the value from the web's layout.json and keep or un-ignore the named test.

| Fix | Where | Test |
| --- | --- | --- |
| A one-line permission row's switch sits 1 css px low. Centre it on the label's line box, not the row's. | U/components/server_settings/permission_grid.rs | un-ignore `parity_server_settings_roles`, `_roles_b` (members.rs:889-890) |
| "Bot channel" description wraps to 2 lines. Give the text column the web's width (flex 1, no fixed max). | D/screens/server_settings/body/draw.rs | `parity_server_settings_channel_permissions(_b)` |
| "Permissions" `<h3>` (W/screens/admin/ChannelPermissions.tsx:201,209): use the computed weight and colour from layout.json, not bold FG. | same | same |
| Keycaps about 2px narrow: min-width and padding from `.d-key-hint` CSS. | U/components/key_hint.rs | `parity_user_settings_keyboard(_b)` |
| Disabled "Check again" is smaller than the web's. Use the Button size the web's class names. | D/screens/bot_admin/music_page.rs:291 | `parity_bot_admin_music(_b)` |
| Mic-test threshold line about one meter cell left. Use the web's `left = threshold * width` (SensitivityMeter.tsx), not per-cell. Device select text 1px high. | U/components/voice_settings/sensitivity_meter.rs, select.rs | `parity_voice_settings_*` |
| Bio textarea: draw the resize grip and make it drag-resizable vertically, min 34px (Decision D16). Text 1px low (padding-top 8, line-height 1.4). | U/components/text_field.rs | `parity_user_settings_profile(_b)` |

- [ ] Step 1: Failing unit tests, one per geometric rule, in each file's `tests`:
      `permission_grid::tests::a_single_line_switch_centres_on_its_label` (switch top == label line top + (line − 18)/2);
      `sensitivity_meter::tests::threshold_x_is_proportional` (`threshold_x(0.5, 200.0) == 100.0`);
      `key_hint::tests::keycap_min_width` (value from layout.json);
      `text_field::tests::a_resizable_textarea_drags_vertically` (a drag of the grip by +40px grows the field by 40px; a drag of −500px stops at 34px).
- [ ] Step 2: `$ENV cargo test -p dilla-desktop-ui --lib permission_grid sensitivity_meter key_hint text_field` fails.
- [ ] Step 3: Apply the fixes in the table.
- [ ] Step 4: Unit tests pass. `$ENV cargo test -p dilla-desktop --test members --test server_settings --test user_settings --test voice_settings --test bot_admin parity_` passes, with the roles ignores removed.
- [ ] Step 5: No new fixture. Record before and after `DILLA_PARITY_MEASURE` chrome % in the commit body.
- [ ] Step 6: Commit `desktop: settings pages match the web to the pixel (switches, wraps, keycaps, buttons, meter)`.

### Task 34: States that need the world spec extended [V16]

**Size:** L

Sizes are for the world extension (C/testing/spec.rs, D/examples/parity_world/seed, D/fixture/*).

| State | Extension | Seed (web side) | GPUI side | Size |
| --- | --- | --- | --- | --- |
| typing indicator (`messages/typing`) | `WorldSpec.typing: Vec<{person, channel}>` | testhost route `/debug/parity/typing` sends the op's typing frame to the browser, like `/debug/parity/voice` (cmd/dilla-testhost/parity_calls.go) | D/fixture/messages.rs fills `state.typing` | M |
| new-messages divider (`messages/new-divider`) | `ServerSpec`-level `read_up_to: BTreeMap<channel, message key>` for the viewer | the browser opens the channel after the read point is posted, then the rest is posted while it is elsewhere. Needs a shooter phase between two history batches | D/fixture/notify.rs sets the read marker | M |
| reconnecting banner (`shell/reconnecting`) | Step `{"do":"offline"}` | shoot.mjs `context.setOffline(true)` then settle | the automation command `connection reconnecting` sets `ConnectionStatus::Reconnecting` | S |

- [ ] Step 1: Failing spec tests in C/testing/spec.rs: `typing_and_reads_parse_and_validate`. An unknown person or channel is refused with the error text of the existing validators.
- [ ] Step 2: `$ENV cargo test -p dilla-client --lib testing::spec` fails.
- [ ] Step 3: Implement each row: the spec field, the seed, the fixture mapping, the automation command, and the shoot.mjs step (with a `shoot.test.mjs` case).
- [ ] Step 4: Spec tests pass. `node --test clients/desktop/parity/shoot.test.mjs` passes.
- [ ] Step 5: Fixtures, references and `parity_messages_typing`, `parity_messages_new_divider` and `parity_shell_reconnecting`, plus README rows.
- [ ] Step 6: Commit per row.

### Task 35: The now-playing card under the Bot's "started music" message [B2]

**Size:** L

**Files:** Modify D screens/timeline.rs (`body_parts` l.337-356; the draw loop l.1280-1300; extract `user_name` from `author` l.238-258).
Modify D screens/bot_admin/player.rs: replace the unused `card()` (l.403-422) with `music_card`, and add `card_view_of` plus tests.
Modify D slots.rs:115-121 (`message_extras`). Modify U components/bot_admin/now_playing.rs (add `card_height`). Test in D screens/bot_admin/player.rs
(inline) and D screens/timeline.rs (inline).
**Web rules carried over (W screens/conversation/MessageItem.tsx:114, 120, 164-186; W screens/voice/music.tsx:231-249, 290-315):**
- An *announcement* is a message that meets all four conditions:
  - `bot` is true: `item.bot` and its sender is a member of account kind 1 (:114; D timeline.rs:826-830 already has this test).
  - It is not a poll post (`final_poll_marker` is `None`).
  - It is not edited.
  - `music_marker(body)` returns `Some`.
- The body of an announcement is replaced entirely by `tf("music.started", name, room)` (:180-181). `name` is the requester as the
  author line names a user. `room` is the voice channel's name in the community's channel list, else `t("music.someRoom")` (music.tsx:304-306).
  A webhook label is never read on an announcement (:171 `music === null`).
- The card shows only when the state is Ok, Pending or Failed (`shown`, :120, :185). It comes first in the extras, before the attachments,
  6px under the body (packages/ui MessageRow.tsx:271, MessageRow.css:76 `margin-top: 6px`).
- The card's states (`useMusicCard`):
  - (a) The Bot is not in that voice channel's call (`botInRoom` false; the participant is the catalog's `bot_user`, the same test as
    `feed_of` in player.rs:67-72). The card shows `view_of(None, …)`: meta `music.left`, no control.
  - (b) The Bot is in the room but no snapshot has been read yet. The card is neutral: `meta = room`, the idle title is `t("music.region")`,
    there is no control and the queue is empty.
  - (c) A snapshot is available and its `track` is `None` or `track.id != marker.item`. The card is ended: `meta` and `blocked` are
    `tf("music.ended", bot, room)`, the idle title is `t("music.finished")`, there is no control and the queue is empty.
  - (d) Otherwise the card shows `view_of(Some(s), room, bot, …, in_call, now)`. `in_call` means this device's call phase is not Idle
    and its channel is the marker's channel (music.tsx:299). DJ gating is `s.can_control`. A press without it says `music.blocked.dj` in
    the call, else `music.blocked.join`.
  - A snapshot that is `Unavailable` while the Bot is in the room counts as (a) (`musicView` with `state.status !== 'available'`).
- The controls are pause/play (`!paused`), skip and stop on `client.music()` for the marker's channel. A refusal shows a toast with
  `MusicError::toast_key()` (reuse `control`).
- The card reads the player only while the Bot is in the room **and** the card is on screen (Decision D10). It then re-reads one second after
  the track should end (`refresh_after_ms`).

**Interfaces:**
- `pub enum CardPhase { Left, Neutral, Ended, Live }`.
  `pub fn card_view_of(bot_in_room: bool, state: Option<&MusicState>, item: &str, room: &str, bot: &str, name_of: NameOf<'_>, in_call: bool, now_ms: u64) -> (CardPhase, PlayerView)`.
  It is in player.rs and is pure.
- `pub fn music_card(item: &TimelineItem, ctx: &SlotCx, window: &mut Window, cx: &mut App) -> Option<Extra>` in player.rs. The
  ElementId is `("music-card", item.key)`. The queue-open state is `window.use_keyed_state(("now-playing-card-queue", item.key))`.
- `pub fn card_on_screen(window: WindowId, channels: BTreeSet<ChannelId>, cx: &mut App)` in player.rs. The timeline's draw loop calls
  it once per frame with the marker channels of rows that intersect the viewport. Per window, it does three things:
  - For a channel that is newly on screen with the Bot in the room, it calls `client.music().refresh(ch)`.
  - It keeps a `TrackEnd` per channel.
  - It calls `client.music().set_watched(union over windows)`.
- `pub fn user_name(state: &AppState, members: &[Member], user: UserId) -> String` in timeline.rs. `author` delegates to it.
- `pub fn announcement_marker(item: &TimelineItem, sender_kind: Option<u8>) -> Option<MusicMarker>` in timeline.rs: the four conditions
  above as one gate (`sender_kind` is the sender's member account kind, `None` when not a member). `body_parts` and `music_card` both read
  it, so the text and the card can never disagree.
- `impl NowPlaying { pub fn card_height(queue_rows: usize, queue_open: bool) -> f32 }` in U. The value comes from NowPlaying.css:23-47:
  - The head is 12+52+10 px.
  - The progress block is 4+4+the micro line.
  - The controls are 6+30+8 px.
  - The edge is 2 px.
  - When the queue is open, add 1+4+30·rows+6 px.
- [ ] Step 1: Write the failing tests (player.rs inline, using the existing `snapshot()` and `track()` helpers):
  - `the_card_says_the_bot_left_when_it_is_not_in_the_room`: `card_view_of(false, None, "i1", "harbour", "Glenn", …)` gives
    phase `Left`, `meta == "Glenn left voice"` and `can_control == false`.
  - `the_card_is_neutral_before_the_first_read`: `card_view_of(true, None, …)` gives phase `Neutral` and `meta == "harbour"`.
  - `a_card_whose_track_no_longer_plays_says_it_ended`: with a snapshot whose track id is `"i2"` and item `"i1"`, the phase is `Ended`
    and `meta == "Glenn played this in harbour"`. Do the same with `track: None`.
  - `the_announced_track_playing_is_live_and_gated_by_dj`: with track id `"i1"` and `can_control: false`, `in_call: false`, the phase is `Live`
    and `blocked == "Join harbour to control playback"`. With `in_call: true`, `blocked == t("music.blocked.dj")`.
  - timeline.rs inline `a_bot_music_announcement_reads_started_music`: `body_parts` of a kind-1 Bot item with body
    `"x\n[dilla-music:v1:{v}:{u}:{i}]"` gives `[Body::Text("Ada started music in harbour")]`. The same body edited, or from a member
    of kind 0, keeps its text verbatim.
  - timeline.rs inline `a_forged_music_marker_stays_text` (Review Focus 4), with the body above: `announcement_marker` is `None` for a
    kind-0 member's post (`item.bot == false`), for `item.bot == true` with `sender_kind == Some(0)` or `None`, for an edited kind-1 post,
    and for a kind-1 post that also carries a final poll marker; it is `Some` for the unedited kind-1 post. For each `None` case,
    `body_parts` returns the body verbatim (marker line included).
  - U inline `the_card_height_follows_the_css`: `card_height(0, false)` equals the summed constant written out, and
    `card_height(3, true) - card_height(3, false) == 1.0 + 4.0 + 90.0 + 6.0`.
- [ ] Step 2: `cargo test -p dilla-desktop --lib player:: timeline::a_bot_music timeline::a_forged_music` and `cargo test -p dilla-desktop-ui card_height`. Expected result: they fail to compile.
- [ ] Step 3: Implement the interfaces above. `message_extras` puts `bot_admin::player::music_card` first in `parts`. Delete the
      old `card()` and its `_shared` stub. `card_view_of` overrides `NowPlayingLabels.idle` for Neutral and Ended.
- [ ] Step 4: Run the same commands, then `cargo test -p dilla-desktop --test bot_admin`. Expected result: all pass.
- [ ] Step 5: Add `native/music-card-live` and `native/music-card-ended` references (Decision D9) from a new
      `tests/bot_admin.rs::parity_music_card_live` and `parity_music_card_ended`, each calling `assert_native_screen` on a GPUI-only fixture
      `P/fixtures/bot-admin/music-card-live.json` / `music-card-ended.json` (the music facet the `bot-admin/music` fixtures use, plus a kind-1 Bot
      message whose body ends in the marker for the playing item, or for another item). Bless them with
      `DILLA_PARITY_BLESS=1` and add both to the README `native/` table. BEHAVIOUR-GATES: no e2e title covers the card, so add no row.
- [ ] Step 6: Commit `desktop: the now-playing card under the Bot's started-music message`.

### Task 36: The now-playing bar on top of the voice stage [B3]

**Size:** M

**Files:** Modify U components/bot_admin/now_playing.rs:62-67 (the `Stage` variant, its render, and `NowPlayingLabels.up_next`) ·
Modify U components/avatar.rs:69-125 (add `AvatarSize::NpStage`) · Modify D screens/bot_admin/player.rs (`labels`, and new `stage_bar`) ·
Modify D screens/voice/mod.rs:1993-2002 (the stage body column) · Test in D tests/bot_admin.rs.
**Web rules:**
- The bar shows only when this device is joined to *this* channel, the channel is not a DM, and the player has a track
  (W screens/voice/VoiceView.tsx:568, 614; NowPlaying.tsx:159-160 returns nothing without a track).
- It sits first in the stage body and is separated from the tiles by `--d-stage-gap` 10px (CallStage.tsx:239-241, CallStage.css:3, :36).
- It is drawn from the same `MusicFeed` as the dock (player.rs `feed`), only when `feed.channel == channel.id`.
- Layout, from NowPlaying.css:69-81:
  - The bar is 48px high, with padding 0 8 0 10 and a 10px gap. It is `--surface` with a 1px `--hairline-2` edge and `--r-xl` corners.
  - The bot avatar is a 28px square with `--text-badge` and `--r-md`.
  - While playing, three bars at 11px are shown.
  - The text column has a 3px gap. Line 1 is the title (`--text-ui`, display weight) and the meta (`--text-meta`, `--fg-3`), baseline-aligned
    with an 8px gap, nowrap and ellipsis.
  - Line 2 is a 3px track (flex 1) and `pos / dur` in `--text-micro`.
  - The play/skip/stop buttons are 32x32, `--r-seg`, with 14/14/13px glyphs (NowPlaying.tsx:122).
  - "n up next" uses `--text-meta`, `--fg-3`, 0 6px padding (`music.upNext`). Its count is `up_next(queue)`.
- Blocked presses behave as in the card. In the stage, `in_call` is always true, so the blocked text is `music.blocked.dj`.

**Interfaces:**
- Add `NowPlayingVariant::Stage`. `NowPlaying::stage(self) -> AnyElement` returns an empty div without a track, as `compact` does.
- Add `NowPlayingLabels.up_next: SharedString`, filled. `player::labels(n)` sets it to `tf("music.upNext", n)`.
- `pub fn stage_bar(channel: ChannelId, window: &mut Window, cx: &mut App) -> Option<AnyElement>` in player.rs, with id `"now-playing-stage"`.
- Add the `AvatarSize::NpStage => (px(28.0), text::BADGE)` metric and `bot_radius` `radius::MD`.
- [ ] Step 1: Write the failing tests in tests/bot_admin.rs. Give the `Player` host a `variant` field.
  - `the_stage_bar_names_its_up_next_count`: a Stage with 3 queue rows plus the current one shows the text `"3 up next"`
    (assert with `cx.debug_bounds`/a11y name, as the existing tests do).
  - `keyboard_stage_bar_focus_order`: Tab reaches toggle, skip and stop in that order, and Enter on each logs `toggle`/`skip`/`stop`.
  - `the_stage_bar_is_not_drawn_without_a_track`: a Stage with `track(None)` has no `now-playing-stage` bounds.
  - player.rs inline `the_stage_bar_is_only_for_the_calls_own_channel`: build a feed for channel A with `in_call(true, In)`. `stage_bar`'s
    guard `feed.channel == channel` is false for channel B. Test the pure guard `fn stage_shows(feed: Option<&MusicFeed>, channel: ChannelId, has_track: bool) -> bool`.
- [ ] Step 2: `cargo test -p dilla-desktop --test bot_admin stage` and `cargo test -p dilla-desktop --lib stage_bar`. Expected result: they fail.
- [ ] Step 3: Implement. In voice/mod.rs, the body column gets `.gap(px(m::STAGE_GAP))` (a new const of 10.0 that cites CallStage.css
      `--d-stage-gap`) and `.children(in_call.then(|| player::stage_bar(channel.id, window, cx)).flatten())` before `body`.
- [ ] Step 4: Run the same commands, plus `cargo test -p dilla-desktop --test voice`, to show the stage layout is unchanged without music.
- [ ] Step 5: Add the reference `native/music-stage-bar` (Decision D9): `tests/voice.rs::parity_music_stage_bar` calls `assert_native_screen` on a GPUI-only
      fixture `P/fixtures/voice/music-stage-bar.json` (voice/stage's world joined, plus the music facet with a playing track and 3 queued); add it to the README `native/` table. The
      gate row for `e2e/voice/csp-axe.spec.ts` ("…a player…") is unchanged.
- [ ] Step 6: Commit `desktop: the now-playing bar on top of the voice stage`.

### Task 37: The dock's link quality (good, poor, bad) [S5]

**Size:** S

Web spec:
- `linkQuality(rtt, loss) = rtt > 300 || loss > 5 ? 'bad' : rtt >= 150 ? 'poor' : 'good'` (W call/stats.ts:87-89).
- `lossPct` is the larger of two values, rounded to 0.1 (stats.ts:120-158):
  - the remote-inbound `fractionLost` max ×100 (stats.ts:51-52);
  - the inbound streams' `lost / (received + lost)` delta since the previous read.
- The dock's line 2 colours `poor` with `--warn` and `bad` with `--danger`; `good` stays `--fg-3` (VoiceDock.css:21-24).
- The line has a spoken text, `voice.dock.stats {ms, quality: voice.dock.quality.*, up, down}` (VoiceDock.tsx:116-118, VoiceView.tsx:836-841). It is never announced.
- The quality applies only in the dock. The call bar has none.

**Files:**
- Modify clients/desktop/dilla-media/src/room.rs `LinkRead` (456-505):
  - Add `inbound_packets: HashMap<String, (u64, i64)>`, from `InboundRtp` `received.packets_received` / `received.packets_lost`.
  - Add `remote_fraction_lost: Option<f64>`, the max over `RtcStats::RemoteInboundRtp(r) => r.remote_inbound.fraction_lost` (libwebrtc 0.4.0 stats.rs:174-186, 454).
  - Add `pub fn loss_since(&self, before: &LinkRead) -> f32`.
- Modify C/state/voice.rs (93-98): add `pub loss_pct: Option<f32>` to the call. Modify C/features/voice.rs: add `pub fn set_loss(&self, call: [u8; 16], pct: Option<f32>)`, guarded like `set_rate` (1104-1113).
- Modify D/screens/voice/session.rs (716): call `ctx.voice.set_loss(link.call_id, Some(read.loss_since(before)))` beside `set_rate`.
- Modify D/screens/voice/mod.rs, the dock (1509-1570):
  - Add `pub enum LinkQuality { Good, Poor, Bad }`.
  - Add `pub fn link_quality(ms: u32, loss_pct: f32) -> LinkQuality`.
  - Add `pub fn link_spoken(ms: u32, q: LinkQuality, rate: Option<(u32, u32)>) -> String`.

**Interfaces:** `link_quality` is applied only when `ping_ms` and `loss_pct` are both `Some` and the phase is `In`; otherwise the line keeps `FG_3` and has no spoken text. An unknown rate gives `—` in the spoken text (VoiceDock.tsx:118).

- [ ] **Step 1: Write the failing tests.**
  - voice/mod.rs inline `#[test] fn the_link_quality_follows_the_web_s_thresholds`:
    - `link_quality(149, 0.0) == Good`
    - `link_quality(150, 0.0) == Poor`
    - `link_quality(300, 5.0) == Poor`
    - `link_quality(301, 0.0) == Bad`
    - `link_quality(20, 5.1) == Bad`
    - `link_spoken(24, Poor, Some((38, 71))) == "Ping 24 milliseconds, poor. Sending 38, receiving 71 kilobits per second."`
    - `link_spoken(24, Good, None)` holds `"Sending —, receiving —"`
  - room.rs inline `#[test] fn loss_is_the_worse_of_the_remote_report_and_the_inbound_delta`:
    - before `{"a": (100, 0)}`, now `{"a": (190, 10)}`, `remote_fraction_lost: Some(0.02)` → `10.0`
    - the same with now `{"a": (199, 1)}` → `2.0`
    - a stream only in `now` adds nothing
- [ ] **Step 2: Run, expect failure.** `cargo test -p dilla-desktop --lib voice::` and `cargo test -p dilla-media --lib room::`
- [ ] **Step 3: Implement.**
  - `loss_since` follows stats.ts:136-143: a counter that went down is skipped, and the result is rounded to one decimal.
  - In the dock, line 2's colour is `color::WARN` for Poor and `color::DANGER` for Bad. Wrap it in `div().id("voice-dock-stats").a11y(Role::Label, link_spoken(..))`, so the reader gets the sentence (aria-hidden visual plus sr-only text, VoiceDock.tsx:117-118).
- [ ] **Step 4: Run, expect pass.** Same commands, plus `cargo test -p dilla-desktop --test voice`.
- [ ] **Step 5: Parity.**
  - No reference: a fixture's call has no media read, so its line stays "— ms" (voice/mod.rs:1497) on both sides, and the colour states cannot be shot.
  - No BEHAVIOUR-GATES row: no e2e title covers it.
- [ ] **Step 6: Commit.** `desktop: the voice dock colours and names its link quality`

### Task 38: The Welcome settings page (replace the stub) [B5]

**Size:** L

**Files:** Create D screens/server_settings/welcome.rs (the `WelcomeDraft` model, its tests, and `Body::welcome_panel`) · Modify
D screens/server_settings/body.rs (add a `welcome: Rc<RefCell<WelcomeDraft>>` field next to `join_role` at l.250/368, an InputState
`welcome_template` (multiline), a load hook in the section switch at l.560-566, and `flush_welcome` called from `flush_save` at l.698) ·
Modify D screens/server_settings/body/draw.rs:1560-1593 (draw `welcome_panel` in place of the stub) · Modify D screens/server_settings/view.rs:59-82,
484-500 (`WelcomeSection` attached to the workspace like `JoinRoleSection`) · Modify D screens/server_settings/mod.rs (`mod welcome`).
**Web rules (W screens/admin/WelcomeSettingsPanel.tsx unless noted):**
- Inputs:
  - `enabled` is true when the installation has `state == 3` and `capabilities & (1 << 1) != 0` (W screens/admin/BotSection.tsx:377).
  - `can_manage` is `access.loaded && access.manage_community` (BotSection.tsx:597).
  - `channels` are the community's kind-0 channels whose ids are in `catalog.bot_channels`, in channel-list order (BotSection.tsx:110-114).
  - `server_name` is the community's name (BotSection.tsx:105).
- `unavailable = !enabled || !can_manage || channels.is_empty()` (:73).
- Default template: the literal `"Welcome {member} to {server}!"` (:56). It has no string key.
- Order of the content (:266-305):
  - title `admin.bot.welcomeSettings.title`, then `.help`;
  - `.requiresCapability` if `!enabled`;
  - `.readOnly` if `!can_manage`;
  - `.noChannels` if there are no channels;
  - `bot.feature.needsChannel` if the page is loaded, a config exists, and its channel is not among `channels` (:274-275);
  - then **either** the Load button (`.load`, or `.loading` while a read is pending; disabled when `unavailable || pending`) **or**
    the form, described next.
- The form has:
  - a Select `.channel` with a first option `("", .chooseChannel)` and then `channels`;
  - a multiline `.template` field with the hint `.tokens`;
  - a Reload button (`.reload`, disabled while pending).
  - Both inputs are disabled when `unavailable || pending`.
  - There is no in-panel Save, because the desktop is `managed` (:291) and the settings footer saves.
- Under the form, in this order:
  - the status line (`.waiting`, or `.uploadUncertain` while an Uncertain request is outstanding, or the notice);
  - the Retry button `.retry` while Uncertain (:300-302);
  - the alert (the error key);
  - `.configured` when the page is loaded and the draft equals the saved config (:304).
- Answers (:120-147, :178-203):
  - A Read sets `config` and `revision`. If the draft is not dirty, it resets the channel to `config.channel`, or `channels[0]`, and the template to the
    config's or the default. It then sets `loaded`, clears `must_reload`, the notice and the error.
  - Saved sets config to the sent value, sets `revision` and the notice `.saved`, and clears `must_reload`.
  - Conflict sets the error `.conflict` and `must_reload = true`.
  - Failed sets `.responseFailed`.
  - Err (refused) sets `.uploadFailed`.
  - Uncertain keeps the request id and sets the notice `.uploadUncertain`.
  - A Retry that comes back Uncertain again sets the error `.readFailed`.
- `configured_draft` is true when the config exists and its channel, template and `server_name` equal the draft's channel, the draft's template and the current server name (:77-78).
  `dirty = loaded && (config.is_none() || !configured_draft)` (:256).
- `can_save` requires all of the following (:258-259):
  - `dirty` and not `unavailable`;
  - a revision exists and `must_reload` is false;
  - a channel is chosen;
  - the template is not blank after trim, and is at most 2000 UTF-8 bytes;
  - no request is pending.
- A Save that fails validation sets `.invalid` (:242-244).
- Reset puts the channel back to `config.channel`, or `channels[0]`, and the template back to the config's or the default. If there is no config, `loaded` becomes false (:260-262).
- Switching community clears everything (:151-165). This holds because the Body is per community.

**Interfaces** (welcome.rs):
```rust
pub const DEFAULT_TEMPLATE: &str = "Welcome {member} to {server}!";
pub const TEMPLATE_MAX_BYTES: usize = 2000;
pub struct WelcomeInputs<'a> { pub enabled: bool, pub can_manage: bool, pub channels: &'a [(ChannelId, String)], pub server_name: &'a str }
#[derive(Default)] pub struct WelcomeDraft { pub config: Option<WelcomeSettings>, pub revision: Option<u64>, pub channel: Option<ChannelId>,
  pub template: String, pub loaded: bool, pub must_reload: bool, pub pending: Option<u64 /*op*/>, pub uncertain: Option<([u8;16], u64)>,
  pub saving: Option<WelcomeSettings>, pub error: Option<&'static str>, pub notice: Option<&'static str>, pub save_requested: bool,
  pub ticket: u64 /* the newest request's ticket */ }
impl WelcomeDraft {
  pub fn unavailable(i: &WelcomeInputs) -> bool;  pub fn configured_draft(&self, server_name: &str) -> bool;
  pub fn dirty(&self, server_name: &str) -> bool;  pub fn can_save(&self, i: &WelcomeInputs) -> bool;
  pub fn to_save(&mut self, i: &WelcomeInputs) -> Option<(WelcomeSettings, u64)>; // None + error "admin.bot.welcomeSettings.invalid"
  pub fn begin(&mut self, op: u64) -> u64; // sets `pending = Some(op)`, bumps and returns `ticket`; never clears `uncertain`
  pub fn answer(&mut self, ticket: u64, op: u64, result: Result<WelcomeAnswer, String>, fallback: Option<ChannelId>, retried: bool);
  // an answer whose `ticket` is older than `self.ticket` changes nothing (a late answer never overwrites a newer read)
  pub fn reset(&mut self, fallback: Option<ChannelId>);
}
pub fn welcome_inputs(state: &AppState, community: CommunityId) -> (bool, bool, Vec<(ChannelId, String)>, String);
```
- [ ] Step 1: Write the failing tests (welcome.rs inline; each `answer(op, …)` below is `answer(d.begin(op), op, …)`):
  - `the_page_is_unavailable_without_capability_manage_or_channels`: each of the three inputs false/empty gives `unavailable`, and all set gives `!unavailable`.
  - `a_first_read_of_an_unconfigured_server_picks_the_first_bot_channel_and_the_default`: `answer(1, Ok(Read{config: None, revision: 3}), Some(g), false)`
    gives `loaded`, `channel == Some(g)`, `template == DEFAULT_TEMPLATE`, and `dirty("Midgard Crew") == true`.
  - `a_read_keeps_a_dirty_draft`: after a read that is then edited, a second read with another config keeps the edited template and updates `config`/`revision`.
  - `the_save_is_refused_locally_past_2000_bytes`: a template of `"é".repeat(1001)` (2002 bytes) gives `to_save → None` and `error == Some("admin.bot.welcomeSettings.invalid")`.
    `"é".repeat(1000)` gives `Some`.
  - `a_conflict_must_be_reloaded_before_saving`: `answer(2, Ok(Conflict), …)` gives `must_reload` and `!can_save(…)`.
  - `renaming_the_server_makes_the_saved_draft_dirty`: when the config equals the draft, `dirty("Midgard") == false`, and `dirty("Midgard Crew") == true`.
  - `an_uncertain_save_offers_retry_and_a_second_timeout_says_read_failed`: `answer(2, Ok(Uncertain{request_id: [1;16]}), …, false)` gives
    `uncertain == Some(([1;16], 2))` and `notice == Some("admin.bot.welcomeSettings.uploadUncertain")`. The same answer with `retried: true` gives `error == Some("…readFailed")`.
  - `reset_without_a_config_unloads`.
  - `a_late_save_answer_does_not_overwrite_a_newer_read` (Review Focus 5): `let t1 = d.begin(2); d.answer(t1, 2, Ok(Uncertain { request_id: [1; 16] }), …)`;
    then Reload: `let t2 = d.begin(1); d.answer(t2, 1, Ok(Read { config: Some(cfg9), revision: 9 }), …)` gives `revision == Some(9)` and
    `uncertain == Some(([1; 16], 2))` still (Retry is still offered with the same id); then the save's late answer
    `d.answer(t1, 2, Ok(Saved { revision: 8 }), …)` leaves `revision == Some(9)` and `config == Some(cfg9)`. A Retry
    (`let t3 = d.begin(2); d.answer(t3, 2, Ok(Saved { revision: 10 }), …, true)`) then applies: `revision == Some(10)`, `uncertain == None`.
  - In tests/server_settings.rs: `the_welcome_page_without_a_bot_shows_why_and_a_disabled_load` (GPUI test, fixture `welcome`). The page has the
    `requiresCapability` and `noChannels` lines, no `readOnly` line (the viewer is the owner), and `welcome-load` is disabled. This replaces the stub's
    unconditional three paragraphs with the web's conditions.
- [ ] Step 2: `cargo test -p dilla-desktop --lib welcome::` and `cargo test -p dilla-desktop --test server_settings welcome`. Expected result: they fail.
- [ ] Step 3: Implement the model, then `Body::welcome_panel(&mut self, cx) -> AnyElement` (it follows `join_role_panel`'s builders:
      `line`, `para`, `alert`, `Select::new("welcome-channel", …).bare()`, `SettingsRow`, `TextField::multiline` on `welcome_template`).
      The model is wired as follows:
      - Load and Reload call `client.bot_admin().welcome(community, WelcomeOp::Read)`.
      - `flush_welcome` (from the footer Save via `WelcomeSection::save` → `save_requested`) calls `to_save`, then `WelcomeOp::Save`.
      - Retry calls `WelcomeOp::Retry`.
      - `view.rs` publishes `Flags { dirty, busy: pending.is_some(), can_save }` for Welcome like Join roles.
      - The section's `reset` calls `WelcomeDraft::reset`.
      Remove the stub comment at draw.rs:1561 ("until the bot installation (Task 21) fills it", an older plan's task).
- [ ] Step 4: Run the same commands plus `cargo test -p dilla-desktop --test server_settings`. Expected result: all pass, including the unchanged
      `parity_server_settings_welcome`/`_b` (that world has no Bot, so the frame is unchanged).
- [ ] Step 5a: The Bot world extension (Decision D9; Task 40 and Task 41 reuse it). Add to `ServerSpec` (C/testing/spec.rs:142)
      `#[serde(default)] pub bot: Option<BotSpec>` with `#[serde(deny_unknown_fields, rename_all = "camelCase")] pub struct BotSpec { pub capabilities: Vec<String>, pub bot_channels: Vec<String> }`.
      Capabilities are the names of `cap::*` (`"welcome"`, `"polls"`, …); an unknown name, or a `botChannels` entry that is not one of the server's
      text channels, is refused with the existing validators' error text. Test first: `testing::spec::tests::a_bot_extension_parses_and_validates`
      (`$ENV cargo test -p dilla-client --lib testing::spec`). The parity world seed (D/examples/parity_world/seed) then calls
      `enable_bot(owner_client, community, caps, …)` (C/testing/host/bot.rs:391) after the channels exist and marks each Bot channel, for both the
      web shooter's world and the GPUI fixture's state.
- [ ] Step 5b: Add the fixtures `server-settings/welcome-loaded.json` and `welcome-loaded-b.json` (a world with the `bot` extension,
      `general` marked; the steps click `web: "button:has-text('Load welcome settings')"` / `gpui: "welcome-load"`). Shoot them with `shoot.mjs`, and add
      `parity_server_settings_welcome_loaded`/`_b` to the `parity!` list in tests/server_settings.rs:42-60. The BEHAVIOUR-GATES row is updated in Task 5 step 5.
- [ ] Step 6: Commit `desktop: the Welcome settings page loads, edits and saves through the Bot`.

### Task 39: A poll post's body reads "{name} asked:" (or its own text once verification gives up) [X4]

**Size:** S

The web's rules (W/screens/conversation/MessageItem.tsx:114, 163, 172-182):
- `bot` means `item.bot` and a sender who is a kind-1 member. This is the same test as D timeline.rs:826-830.
- `marker` is `bot ? final_poll_marker(body) : None` (C/features/utilities/polls.rs:29).
- **verified**: `marker && !edited && state == Ok && state.bot.polls[msg_id]` exists. The body is `tf("poll.asked", name)`, where
  `name` names `poll.creator` as the author line does (Task 35's `user_name`). The name is the poll's creator, not the Bot
  (MessageItem.poll.test.tsx:34-35).
- **pending**: `marker && !edited && state == Ok && live && community && self id && sender`, not verified, and `poll_checks` is not
  `GaveUp`. The body is empty, and the card's loading line (Task 27) stands for it.
- **fallback** (every other case with a marker). Remove a final `\n?[dilla-poll:v1:<32 hex>]`. Replace each `<t:(\d+):[tTdDfFR]>`
  with `local_utility_time(seconds)`, or leave the raw tag when that gives `None`. Then parse the result as markdown.
- **No marker**: today's path, unchanged.
- **Formatting `<t:…>`**: `local_utility_time` follows utility-time.ts:2-7, Intl `en-GB` `{weekday short} {day} {month short}, {HH}:{mm} {zone abbr}`.
  At 1_800_003_600 that is `Fri 15 Jan, 09:00 UTC` in UTC and `Fri 15 Jan, 10:00 CET` in Europe/Stockholm (checked with node).

**Files:**
- Modify D/screens/timeline.rs: `body_parts` (:337-356) gains `live: bool`. Update its callers (:392, :1125) and the tests (:1590-1591).
- Modify D/format.rs: add `local_utility_time`.
- Test: inline tests in D/screens/timeline.rs and D/format.rs.

**Interfaces:**
- Produces `pub enum PollBody { NoMarker, Asked(String), Pending, Fallback(String) }` and `pub fn poll_body(item: &TimelineItem, state: &AppState, community: Option<CommunityId>, live: bool, zone: &jiff::tz::TimeZone) -> PollBody` in timeline.rs: the four rules above as data (`Fallback` holds the text before markdown). `body_parts` matches on it; Task 41's thread rows use it too.
- Produces `pub fn local_utility_time(seconds: i64, zone: &jiff::tz::TimeZone) -> Option<String>` in D/format.rs. The zone
  abbreviation comes from `zone.to_offset_info(ts).abbreviation()`. Callers pass `jiff::tz::TimeZone::UTC` under `Clock::Fixed { utc: true }`
  and `TimeZone::system()` otherwise.
- Consumes `user_name` (Task 35), `PollCheck` and `poll_checks` (Task 27), and `final_poll_marker`.

- [ ] Step 1: Write the failing tests (timeline.rs inline). Use a kind-1 Bot member, a creator `bob`, and the body
      `"Raid?\nBridge\nHarbour\nVote with /poll vote.\nCloses: <t:1800003600:R>\n[dilla-poll:v1:abab…ab]"` (from MessageItem.poll.test.tsx:11).
  - `a_verified_poll_post_reads_who_asked`: with the poll held (creator bob), `spoken_body(&body_parts(.., true))` is `"bob asked:"`.
  - `a_poll_post_being_verified_shows_no_text`: `live = true` with no held poll gives `""`.
  - `an_unverified_poll_post_shows_its_text_with_local_time`: `live = false` (or `GaveUp`) gives text that contains
    `"Vote with /poll vote."` and `"Closes: Fri 15 Jan, 09:00 UTC"`, and that contains neither `"<t:"` nor `"[dilla-poll"`.
  - `an_edited_poll_post_shows_its_text`: `edited: true` with the poll held gives the fallback text, not `"bob asked:"`.
  - D format.rs `utility_time_matches_intl_en_gb`: `local_utility_time(1_800_003_600, &UTC) == Some("Fri 15 Jan, 09:00 UTC")`
    and `Europe/Stockholm` gives `"Fri 15 Jan, 10:00 CET"`.
- [ ] Step 2: `$ENV cargo test -p dilla-desktop --lib timeline::tests::a_ format::tests::utility_time` fails to compile.
- [ ] Step 3: Implement `poll_body` and use it in `body_parts`. It comes before Task 35's music arm: a poll marker excludes music.
- [ ] Step 4: `$ENV cargo test -p dilla-desktop --lib timeline:: format::` passes. Then run `$ENV cargo test -p dilla-desktop --test bot parity_polls_card`.
- [ ] Step 5: Parity.
  - `polls/card.json` holds the verified poll, so its row now reads `… asked:` as the web reference does. If the reference row differs,
    re-shoot it with `node clients/desktop/parity/shoot.mjs clients/desktop/parity/fixtures/polls/card.json`.
  - Add `polls/fallback.json` and `fallback-b.json`: the world of `polls/card.json` with no `poll` facet and no `bot` extension; the
    `dillabot` message's body is the poll text above ending in a final `[dilla-poll:v1:<32 hex>]` line. With no ready Bot the web gives up
    at once (`botDefinitelyUnavailable`, command-bot-store.ts:76-78) and shows the text; the fixture window has no client (`live = false`).
    Shoot them, add
    `parity_polls_fallback` beside `parity_polls_card` (D/tests/bot.rs:52), and add the rows to the README table.
  - Add no BEHAVIOUR-GATES row.
- [ ] Step 6: Commit `desktop: a verified poll post reads "{name} asked:", an unverified one its own text`.

### Task 40: Poll card references [V13]

**Size:** M

The world can't seed a poll (`seed/polls.rs` refuses the facet, tests/bot.rs:1-6). This depends on Task 38's `bot`
ServerSpec extension (a real test Bot through `enable_bot`) and on Tasks 27 and 39 (GPUI verifies and words the post).
**Files:** D/examples/parity_world/seed/polls.rs (`unseedable` drops the poll refusal when the server has `bot`) ·
P/fixtures/polls/card(-b).json (add `"bot": {"capabilities": ["polls"], "botChannels": ["general"]}`) · tests/bot.rs:51-58.
- [ ] Step 1: Seed. The creator's client posts op 70 through the Bot (`PollSpec` question and options; a message with `thread_of` posts it
      with that thread, for Task 41). Each `VoteSpec.by`
      client votes. `closed` closes it. The Bot's message key maps to the fixture's message key in the sidecar `ids`.
- [ ] Step 2: `node … shoot.mjs …/polls/card.json --out target/shots/poll` produces a card. If not, fix the seed.
- [ ] Step 3: Shoot both without `--out`. Remove the two ignores.
- [ ] Step 4: `$ENV cargo test -p dilla-desktop --test bot parity_polls_card` passes for both.
- [ ] Step 5: Add the `bot` extension to Task 26's committed `polls/dialog.json`, shoot it, and add `parity_polls_dialog` beside
      `parity_polls_card` (D/tests/bot.rs:52); `$ENV cargo test -p dilla-desktop --test bot parity_polls_dialog` passes. README rows for all three.
- [ ] Step 6: Commit `parity: the world seeds polls through the test Bot; poll card references`.

### Task 41: Polls inside threads [X6]

**Size:** M

The web draws a thread reply with the same `MessageItem` as the log (ThreadView.tsx:242-244), so a Bot poll posted in a thread
(`/poll create` from the thread composer, Task 31; UtilityCommandProvider.tsx:379, :391) gets its `PollCard` and the `{name} asked:` /
fallback body there too (MessageItem.tsx:114, 163, 172-182). GPUI thread rows draw the raw body with no extras
(D/screens/threads/view.rs:241-260), so the post shows `[dilla-poll:v1:…]` and no card.

**Files:**
- Modify U/components/threads/thread_row.rs: `ThreadRow::extra(self, element: AnyElement, height: f32) -> Self` (beside `replace_body`, :114),
  drawn under the body at the body's left inset (`body_width`, :28) with the log's extras gap (`slots::stack_after`, D/slots.rs:148);
  the row's height adds `height` plus that gap.
- Modify D/screens/threads/view.rs `row` (:220-260): the body from Task 39's `poll_body`; the card from Task 27's `poll_card_in`.
- Modify D/fixture/polls.rs:27-36, :66-71: also find the message among `state.threads.threads[*].replies`, and hold its
  `PollView` with `thread: Some(root)` (today `thread: None` and the timelines only).
- Test: inline in D/screens/threads/view.rs and D/fixture/polls.rs; D/tests/threads.rs.

**Interfaces:** Consumes `poll_body`/`PollBody` (Task 39), `poll_card_in`, `PollCheck`, `verify_poll` (Task 27) and `local_utility_time` (Task 39).
Produces `fn thread_body(item: &TimelineItem, ctx: &SlotCx, live: bool) -> String` in view.rs: for an Ok item, `PollBody::Asked(s)` →
`s`, `Pending` → `""`, `Fallback(s)` → `s`, `NoMarker` → `item.body`; other states as today (:241-252). `live = ctx.client.is_some()`; the
zone is Task 39's (UTC under `Clock::Fixed { utc: true }`, else `TimeZone::system()`).

- [ ] Step 1: Write the failing tests.
  - view.rs inline `a_poll_in_a_thread_reads_who_asked`: the Task 39 body and Bot member, the reply in thread `root`, the poll held with
    creator bob → `thread_body(.., true) == "bob asked:"`; no poll held and `live = true` → `""`; `GaveUp` in `poll_checks` →
    contains `"Closes: Fri 15 Jan, 09:00 UTC"` and not `"[dilla-poll"`; a plain reply → its body unchanged.
  - fixture/polls.rs inline `a_poll_posted_in_a_thread_is_held_for_that_thread`: a world whose poll message has `"thread_of": "m1"` →
    `state.bot.polls[&msg_id].thread == Some(<m1's msg id>)` and `.channel` is general.
  - tests/threads.rs `a_poll_in_a_thread_draws_its_card`: launch `poll-card`, open the thread (`thread-chip` click), `wait_for("poll-card", SOON)`;
    its box's `x` is at least the `thread-composer` box's `x` (it is in the aside, not the log).
- [ ] Step 2: `$ENV cargo test -p dilla-desktop --lib -- threads::view fixture::polls` fails to compile (no `thread_body`).
- [ ] Step 3: Implement `ThreadRow::extra`, `thread_body`, and in `row`: `ThreadRow::new(.., thread_body(item, ctx, live), ..)`, then
      `commands::poll_card_in(item, ctx, Some(root), window, cx)` → `.extra(e.element, e.height)` when `Some`. `root` is the open
      thread's root (the pane's `shown`, view.rs:212). The fixture's lookup and `thread`.
- [ ] Step 4: `$ENV cargo test -p dilla-desktop --lib -- threads::view fixture::polls` and `$ENV cargo test -p dilla-desktop --test threads` pass.
- [ ] Step 5: Parity. Add `clients/desktop/parity/fixtures/threads/poll-card.json` and `poll-card-b.json`: the worlds of `threads/pane-open.json` and `pane-open-b.json`
      plus the `dillabot` person (`"bot": true`, a member of midgard, as polls/card.json), midgard's
      `"bot": {"capabilities": ["polls"], "botChannels": ["general"]}` (Task 40 seeds the thread poll through the Bot) and a reply
      `{ "key": "tp", "sender": "dillabot", "body": "Mira asked:", "thread_of": "m1", "poll": { "question": "What should we raid on Friday?", "options": ["the bridge", "the harbour"] } }`,
      with pane-open's steps (open the thread). Shoot both with `shoot.mjs` (the web draws the card in the pane), add
      `parity_threads_poll_card` and `parity_threads_poll_card_b` to the `parity!` list (tests/threads.rs:39), and the README table rows.
      Append `dilla-desktop::tests/threads::a_poll_in_a_thread_draws_its_card` to the BEHAVIOUR-GATES row "polls publish in channels and
      threads, accept real votes, reject forged markers, and close by the creator".
- [ ] Step 6: Commit `desktop: a poll posted in a thread gets its card and its "{name} asked:" body`.

### Task 42: Composer and message states (no world extension) [V15]

**Size:** L

All use existing worlds and steps. Each state is a fixture + web reference + `parity_*` test. Any difference found is
fixed in the same task with a unit test. Size L for the count, not the depth.

| Fixture | World | Steps | Why |
| --- | --- | --- | --- |
| `messages/reply-compose(-b)` | messages/mention | hover m2 → `message-action-reply` | Replying is a daily action |
| `commands/picker-open` | commands/reminders | focus composer, type `/` | Every slash command starts here |
| `commands/picker-filtered` | same | type `/rem` | |
| `commands/picker-empty` | same | type `/zzz` (no-match line) | |
| `commands/picker-chip` | same, plus a role `crew` | type `/role add `, Tab | Needs Task 9 (chips) and Task 25 merged. Chip and suggestion list |
| `commands/private-result` | same, plus one approved self role | `/selfrole list`, Enter | Needs Task 25. The "only you can see this" row |
| `app/toast` | messages/mention | message menu → Copy link | The most common toast. The browser clock is frozen, so it stays on screen |
| `dms/picker(-b)` | dms/home | click the new-DM button | Starting a DM. GPUI D/screens/dms/picker.rs |

- [ ] Step 1: Write the fixtures. `gpui` ids come from the automation probes. Use `-` where the web has no counterpart.
      `node … shoot.mjs <f> --check` must print the same fixture line as the GPUI driver.
- [ ] Step 2: Add tests `parity_<area>_<state>` in the area's test file. They fail with "no sidecar".
- [ ] Step 3: Shoot. Fix differences, each as its own commit with a unit test.
- [ ] Step 4: `$ENV cargo test -p dilla-desktop --test messages --test bot --test dms --test toasts parity_` passes.
- [ ] Step 5: README rows. picker-chip and private-result wait until Tasks 9 and 25 are merged. Order them last.
- [ ] Step 6: Commit per area, e.g. `parity: the command picker's open, filtered and empty states`.

### Task 43: User docs, screenshots, gates and the alpha.2 release-notes inputs

**Size:** S

Jonas's rule: the user docs are part of every iteration. This task runs after Tasks 1–42 are on the branch.

**Files:**
- Modify: `docs/user/desktop.md`. Update "A tour" (:25), the feature sections and "Platform notes" (:215).
- Modify: `README.md`, "What works today" (:17).
- Modify: `docs/user/bot-commands.md`. The desktop app now runs every built-in, the poll form, announcements and `/rolepanel create`.
- Modify: `docs/user/music.md`. Add the now-playing card under the Bot's message and the bar on the voice stage.
- Modify or add: `docs/user/screenshots/gpui-*.png`.
- Check: `clients/desktop/parity/references/linux/README.md` and `clients/desktop/BEHAVIOUR-GATES.md`.

- [ ] **Step 1: Bring `docs/user/desktop.md` up to date with what this wave adds.** One line or short paragraph each:
  - slash commands, also in threads, with uncertain commands reopening their draft
  - the poll form and polls in threads
  - scheduled announcements and role panels from the composer
  - load earlier in channels, DMs and threads
  - the row menu's notify modes and Edit channel
  - ban details
  - the "no longer in {server}" and "instance is older" banners
  - the dock's link quality
  - the search jump to older messages
  - the music card and the stage bar
  - Welcome settings
  - the 22px checkboxes and the resizable bio

  The desktop-only items (D6, D8) are listed as such. Remove every "Known gaps" entry this wave closed. Keep the entries that
  are still open, including the items under "Tracked for later" below that a user would notice: compact density only inside
  settings, and system-wide push-to-talk.
- [ ] **Step 2: Bring README.md "What works today" and the two Bot docs up to date.** The desktop line now covers these items as well.
- [ ] **Step 3: Re-shoot the user-doc screenshots whose screen changed this wave, the same way #173 made them.** Take each from
  the GPUI frame of the named fixture at scale 2:
  - `gpui-thread-open.png` (`threads/pane-open`)
  - `gpui-voice-stage.png` (`voice/stage`)
  - `gpui-user-settings.png` (`user-settings/profile`, for the bio grip)

  Add three new ones:
  - `gpui-poll-form.png` (`polls/dialog`)
  - `gpui-music-card.png` (the `native/music-card-live` fixture)
  - `gpui-welcome-settings.png` (`server-settings/welcome-loaded`)

  Each one is linked from desktop.md with alt text written like the existing ones (:23-39).
- [ ] **Step 4: Check the gates and the references.**
  - `node --test scripts/behaviour-gates.test.mjs` passes.
  - Every `native/` golden on disk has a row in the README `native/` table, and every fixture with a committed web reference
    has its row.
  - `git status` shows no stray `target/shots` output committed.
- [ ] **Step 5: Run `npm run ci:local -- --changed`.** Expected: PASS. Commit
  `docs: the GPUI desktop's GUI gaps closed for alpha.2 (desktop, README, Bot docs, screenshots)`.
- [ ] **Step 6: Write the alpha.2 release-notes inputs into the PR #190 description,** under a heading
  "Release notes (alpha.2)". Do not add a file. Write one bullet per user-visible area from Step 1, the two desktop-only
  deviations (D6, D8), the fixed teardown (Task 3: calls release their media when they end), and "Still open" from the list
  below. The controller uses them for the `desktop-gpui-v0.2.0-alpha.2` release (clients/desktop/RELEASING.md).

---

## Tracked for later (not this wave)

- **V5, app-wide compact density (D17).** The web's `data-density` (W/App.tsx:105; tokens.css:334-350) is read by Avatar.css,
  MessageLog.css:9-10, Banner.css, CallBar.css, ChannelList.css:26-32, LinkBanner.css and W/app.css. GPUI applies density only
  inside user settings (D/screens/user_settings/section.rs:283). Size L. Every screen under compact density stays unfixtured with it.
- **The visual pass's uncovered states:**
  - a live speaking ring and camera from a real media peer on the web side (this needs a LiveKit publisher in testhost; Task 23
    has only native refs)
  - the share picker's `native/share-picker` (fake sources)
  - the poll dialog's validation errors
  - the Bot command's pending, timeout and retry rows
  - toasts other than Copy link (upload too large, command error, invite blocked)
  - the member list with presence states (idle, dnd, offline grouping) at 50+ members
  - search with no results, and a jump-highlighted hit
  - Invites with expired or used-up invites
  - moderation-log paging
  - the dock's link-quality colours (not shootable)
  - the DM call ring, incoming and outgoing, at 1440×900
  - phone and narrow layouts (the desktop has none below 900×600)
- **The Bot hint re-reads for poll cards** (PollCard.tsx:197, D12).
- **The SFU UDP-mux leak on a lost port race** (test host): when `dilla-testhost` loses the race for an SFU port and retries
  (commit c19d90b6c), the losing attempt's UDP mux is not closed.
- **System-wide push-to-talk** (a global shortcut that works while another app has focus).
