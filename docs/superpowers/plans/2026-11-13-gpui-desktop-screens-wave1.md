# GPUI desktop — screens, wave 1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build every signed-in and signed-out screen of desktop beta.5 in the native GPUI app. Each screen passes the parity gate against the web client and has a behaviour test for each e2e spec it replaces. Before that, close the client correctness gaps the foundations plan left (R17, R19).

**Architecture:** The same four crates as the foundations plan (`dilla-client`, `dilla-media`, `dilla-desktop-ui`, `dilla-desktop`, plus `dilla-parity`). Task 1 first cuts every area its own files:
- a client feature module and state slice per area;
- a screen module per area;
- named slots where one area draws inside another's view.

After that, every Wave 1 task owns a disjoint set of files and runs in its own worktree.

**Tech Stack:** Rust 1.98.1 (edition 2024), `gpui-pre =0.3.12`, `gpui-component =0.9.1`, `livekit =0.10.1` (vendored `webrtc-sys`/`libwebrtc` hook), tokio, reqwest and tokio-tungstenite (rustls, aws-lc), `keyring`, `zeroize`, SQLite3 Multiple Ciphers (ChaCha20), `deep-filter-rt =0.2.0`, `cpal`, `webrtc-audio-processing`, `qrcodegen`, `image`, `sys-locale`, `open`.

**Spec:** `docs/superpowers/specs/2026-11-12-gpui-desktop-design.md` (§7 gates, §9 inventory, §10a, §11a, §12)

**Builds on:** `docs/superpowers/plans/2026-11-12-gpui-desktop-foundations.md` and its ledger `.superpowers/sdd/2026-11-12-gpui-desktop-foundations/progress.md` (rulings R1–R21). This plan starts when the foundations plan lands, including the Task 12 rework under R21.

## Global Constraints

Carried from the foundations plan, updated by its rulings:

- No server, protocol or wire change. Every frame and route matches what `packages/client-core` sends today.
- Voice frames are `dilla-sframe/1`, built only from `dilla_core::sframe`. If any task finds it needs our own libwebrtc build, it stops and reports; Jonas decides.
- Receivers never fetch link previews (`protocol/04`).
- Keys at rest: the store is SQLite3 Multiple Ciphers (ChaCha20) (§11a). Its key is held only in the OS keychain:
  - service `dev.dilla.desktop.preview`;
  - account `<install-id>/instance-<n>` (R10);
  - in `Zeroizing` buffers;
  - no plaintext fallback, ever.
- Preview identity:
  - app id `dev.dilla.desktop.preview`;
  - product name `Dilla Preview`;
  - data dir `<OS data dir>/Dilla Preview/instances/<n>`;
  - never touch the Electron app's data.
- At most 32 instances, ids `0..=31`.
- Pinned exactly: `gpui-pre =0.3.12`, `gpui-component =0.9.1`, `livekit =0.10.1`. A bump is its own PR.
- No GPL code. `cargo deny check licenses` stays green. Any new exception is scoped to one crate (R6, R20).
- `#![forbid(unsafe_code)]`:
  - applies to `dilla-client`, `dilla-desktop-ui` and `dilla-desktop`;
  - `dilla-media` may use `unsafe` only in its FFI module.
- The UI is GPUI builder style. Colours, sizes and fonts come only from the generated tokens; user-visible text comes only from the generated `strings.rs` (Task 6).
- Automation is test-only: it sits behind the `automation` feature and never appears in release builds.
- Every push runs `npm run ci:local` first.
- Electron (`packages/desktop`, `packages/web`, `packages/ui`) gets no changes from this plan.

New for this plan:

- **Parity is honest (R21).** Views paint from their own layout boxes, using a general sub-pixel rule and token-derived constants.
  - No per-element window coordinates, no constants measured from a reference image, and no fixture data in UI code.
  - Every screen has at least 2 varied fixtures. Both must pass the unchanged `tolerance.toml` with no per-fixture tuning.
  - If general layout cannot reach tolerance: report the honest numbers and stop. Jonas decides; never recalibrate.
- **Text masks (R13).** The chrome gate's mask comes from the automation `text-bounds` query through `parity::screen::text_mask`. It never comes from `from_bright_pixels_fixture_only`.
- **References:**
  - shot from the shipping web client by `clients/desktop/parity/shoot.mjs` (Task 5);
  - device scale 2 (R8), reduced motion;
  - Linux shots use Chromium `--font-render-hinting=none` (§11a);
  - desktop sizes only (1280×800 and 1440×900): phone, narrow, tablet, light and high-contrast shots are out of scope.
- **Pixel parity wins over fixes.** Port the prototype and web behaviour exactly; ask before any visual deviation.
- **Keyboard is parity.**
  - Focus order, roving focus and every binding listed in `packages/web/src/screens/settings/Keyboard.tsx` for the area are tested.
  - Screen readers come after the switch.
- **File ownership.** A task edits only the files the Task 1 ownership map gives it, plus its own new files under its area paths.
  - It never edits `Cargo.lock` by hand; on a conflict at merge, the controller re-resolves (R1).
  - Only Task 7 edits workflows, `scripts/ci-local*.mjs` and `deny.toml` (R3). Other tasks write what they need into their report.
- **Worktree safety.** An agent works only in its own worktree. It never checks out, resets or runs git in another one. Before every merge, the controller runs `git branch --show-current`.
- **Real servers.** Behaviour tests run against a real dillad (testkit or testhost), with two devices where the spec has two. They are `#[ignore]`d, and Task 7 runs them in CI with `--ignored`, as the foundations suites are.

## Review Focus

1. A moderator removes a message while this device is offline. After a restart it shows a tombstone, and the removed plaintext is never rendered (Task 2, `removal_while_offline_is_a_tombstone_after_restart`).
2. A rejoin happens while the Remove of this device's previous call leaf is still uncommitted. The client waits, shows the notice, and joins the same call once the Remove commits, never a second leaf (Task 3, `rejoin_waits_for_pending_remove`).
3. A contact's identity changes in a DM, a channel or a call group. Sending and joining are blocked until an exact-key compare. Copy alone never approves (Tasks 13 and 18, `copy_alone_does_not_approve_exact_compare_admits` and `blocked_contact_refuses_join_before_any_call_write`).
4. A private Bot result never leaves this device, never reaches the timeline of another device, and auto-dismisses after its TTL (Task 20, `private_results_stay_private_and_auto_dismiss`).
5. Attachments:
   - a 25 MB file goes through and one byte more is refused before upload;
   - a download is checked against size + 16 before decrypting;
   - a link in a received message triggers no HTTP request to the link's host.

   Tests: Task 12 `file_cap_exact_passes_one_more_refused` and Task 9 `receiver_never_fetches_link_preview`.
6. Echo: with the speaker on and AEC on, the peer does not hear its own tone back above -30 dBFS (Task 4, `aec_removes_playout_echo`).
7. Parity honesty: each screen's second fixture passes with no per-fixture constant. A reviewer greps for literal pixel offsets in `screens/**` (Task 5, `no_window_coordinates_in_screens`).

## Execution shape

- **Wave 0a (sequential):** Task 1, the area scaffold and ownership map. It is small and stubs only, and it unblocks everything else.
- **Wave 0b (parallel, one worktree each, from Task 1):** Tasks 2, 3, 4, 5, 6 and 7. Task 7 merges last, because it wires CI for the others' tests.
- **Wave 1 (parallel, one worktree per task):** Tasks 8–27. A task starts as soon as its own dependencies have merged; it does not wait for the whole of Wave 0.
  - Every Wave 1 task needs Tasks 1, 5 and 6.
  - Tasks 17 and 18 also need Task 3.
  - Tasks 17 and 19 also need Task 4.
  - Tasks 25 and 26 also need Task 2.
  - Dispatch order, when agent slots are limited: 8, 9, 13, 17, 20, 26, 25, 12, 14, 11, 10, 18, 19, 15, 16, 22, 23, 21, 24, 27. Core reading and writing comes first, then security-bearing flows.
- **Wave 2 (sequential):** Task 28, the integration and closing task.
- **Merging:** merge into `feat/gpui-desktop` one task at a time, rebasing the next task on the result. Reviews use opus for Tasks 2, 3, 12, 13, 14, 18, 20, 25 and 26 (E2EE, authz and protocol), and sonnet for the rest.
- **Implementer models:**
  - opus for Tasks 1, 2, 3, 12, 13, 14, 17, 18, 20, 25 and 26;
  - sonnet for the others;
  - haiku for string-only fix rounds.

### Standard procedure for every screen task (Wave 1)

Each Wave 1 task lists only what differs. The common steps are:

- [ ] **S1 States.** Read the listed web and ui sources and their unit tests.
  - Enumerate every visual state from three sources:
    - `docs/design/signal/parity/app/*.json`;
    - `docs/user/screenshots/*` (desktop shots only);
    - the components' stories.
  - Write the state list into the task report, and mark the states this task does not cover, with the reason.
- [ ] **S2 Client.** Implement the area's commands and state slice in its owned `dilla-client` files.
  - Tests go in `clients/desktop/dilla-client/tests/<area>.rs` against a real dillad.
  - Port the logic tests of the TS unit tests named in the task, so the same inputs give the same outputs.
- [ ] **S3 Fixtures and references.**
  - **Fixture A:** each state group's docs-screenshot state at 1280×800.
  - **Fixture B:** varied in at least 3 ways: 1440×900, long names or long text, 2-digit counts, a second server, or a different viewer role.
  - Fixtures are `clients/desktop/parity/fixtures/<area>/<state>[-b].json`.
  - Shoot the references with `node clients/desktop/parity/shoot.mjs clients/desktop/parity/fixtures/<area>/<state>.json`. They go to `clients/desktop/parity/references/linux/<area>/<state>[-b]-2x.png`.
- [ ] **S4 Parity tests.** In `clients/desktop/dilla-desktop/tests/<area>.rs`, add one test `parity_<area>_<state>[_b]` per fixture, each calling `parity::screen::assert_screen(fixture)`. All of them pass on Linux.
- [ ] **S5 Behaviour tests.** Port the named e2e tests to Rust tests in the same file. Each one drives the app through `Driver` against a real dillad, with the assertions the e2e spec makes.
  - Name each test after its e2e title, and put the spec path in a doc comment.
  - A test that needs another Wave 1 area goes to Task 28's list, not here.
- [ ] **S6 Keyboard.** Add a test `keyboard_<area>_focus_order` that tabs through the area and asserts the automation ids in order. Add one test per shortcut the task lists.
- [ ] **S7 Verify.** Run:
  - `cargo test -p dilla-client --test <area> -- --include-ignored`;
  - `cargo test -p dilla-desktop --features automation --test <area> -- --include-ignored`;
  - clippy `-D warnings`;
  - `npm run ci:local`.

  Commit with the message `gpui <area>: <summary>`. Attach the A/B shots, the GPUI shots and the diff numbers to the report.

---

## Wave 0

### Task 1: Area scaffold, slots and ownership map

**Files:**
- Create: `clients/desktop/dilla-client/src/features/mod.rs`, plus one stub per area: `messages.rs reactions.rs threads.rs attachments.rs gifs.rs dms.rs trust.rs members.rs roles.rs moderation.rs server_admin.rs profile.rs devices.rs voice.rs ring.rs voice_prefs.rs bot.rs utilities.rs bot_admin.rs music.rs notify.rs search.rs diagnostics.rs signup.rs enrol.rs factors.rs browser_auth.rs`
- Create: `clients/desktop/dilla-client/src/state/<area>.rs`, one per feature that has observable state
- Create: `clients/desktop/dilla-client/src/sync/route.rs`
- Create: `clients/desktop/dilla-client/src/prefs.rs`
- Modify: `clients/desktop/dilla-client/src/{lib.rs,client.rs,state/mod.rs,sync/mod.rs}`
- Create: `clients/desktop/dilla-desktop/src/screens/<area>/mod.rs`, one stub per area in the map below
- Create: `clients/desktop/dilla-desktop/src/{slots.rs,router.rs,keys.rs}`
- Modify: `clients/desktop/dilla-desktop/src/{lib.rs,app.rs,screens/mod.rs}`
- Create: `clients/desktop/dilla-desktop-ui/src/components/<area>/mod.rs` stubs
- Modify: crate `Cargo.toml`s, adding every dependency this plan names (R1), and `Cargo.lock`
- Create: `clients/desktop/OWNERSHIP.md`, the ownership map below as a table. The controller and reviewers check diffs against it.

**Interfaces** (stub bodies; Wave 1 tasks fill them and must not change the signatures without the controller):

```rust
// dilla_client: one handle per area, cheap to clone, no I/O on construction
impl Client { pub fn reactions(&self) -> features::reactions::Reactions; /* … one per area */ }
// AppState gets one field per slice, Default = empty:
pub struct AppState { /* existing fields */ pub dms: state::dms::DmState, pub voice: state::voice::VoiceState, /* … */ }
// sync::route: ops the core sync engine does not consume are handed to their area
pub(crate) fn route(op: Op) -> Option<Area>;
pub(crate) trait AreaFrames { fn on_frame(inner: &Arc<Inner>, f: &Frame); }
// A feature publishes its slice through the existing Publisher
pub(crate) fn publish(&self, f: impl FnOnce(&mut AppState));
// Socket transport for areas that request over the gateway (Task 3 first)
pub(crate) fn socket_requests(&self) -> Arc<SocketRequests>;
// prefs: non-secret, per instance (<data_dir>/prefs.json) and app-wide (<root>/desktop.json), unknown keys kept
pub struct Prefs; impl Prefs { pub fn get<T: DeserializeOwned>(&self, key: &str) -> Option<T>; pub fn set<T: Serialize>(&self, key: &str, v: &T) -> Result<(), PrefsError>; }

// dilla_desktop::slots: where one area draws inside another's view. The stubs return an empty element.
pub fn message_extras(item: &TimelineItem, ctx: &SlotCx, cx: &mut App) -> AnyElement; // reply chip, attachments, poll card, reactions: in this order
pub fn message_actions(item: &TimelineItem) -> Vec<MessageAction>;                      // toolbar and context menu entries per area
pub fn composer_leading(ctx: &SlotCx, cx: &mut App) -> AnyElement;                       // reply bar (11), attachment tray (12)
pub fn composer_buttons(ctx: &SlotCx, cx: &mut App) -> AnyElement;                       // attach and GIF (12), emoji (10)
pub fn composer_popup(ctx: &SlotCx, cx: &mut App) -> Option<AnyElement>;                 // command picker (20); mentions stay in 9
pub fn channel_row_extras(ch: &Channel, cx: &mut App) -> AnyElement;                     // voice people (17)
pub fn aside(route: &Route, cx: &mut App) -> Option<AnyElement>;                         // thread (11), members (14)
pub fn dock(cx: &mut App) -> Option<AnyElement>;                                         // voice dock (17), now playing (21)
pub fn overlays(cx: &mut App) -> Vec<AnyElement>;                                        // ring (18), toasts (6), dialogs (any)
pub fn settings_sections() -> Vec<SettingsSection>;                                      // user and server registries; entries point into area modules
// router.rs mirrors packages/web/src/router.ts: Welcome, Channel{community, channel}, Dm(id), Dms, Activity, Settings(section), ServerSettings(community, section)
// keys.rs: bind_all(cx) calls screens::<area>::keys::bind(cx) for every area
```

**Ownership map** (the area, the task that owns it, and its files; "+" means the files are new under that path):

| Area | Task | dilla-client | dilla-desktop | ui components |
|---|---|---|---|---|
| shell | 8 | `prefs.rs` (keys `layout.*`) | `screens/{shell,channel_list,rail,header,user_panel,titlebar}.rs`, `router.rs` | `components/shell/+` |
| messages | 9 | `features/messages.rs`, `state/messages.rs` | `screens/{timeline,composer}.rs`, `screens/messages/+` | `components/messages/+` |
| reactions | 10 | `features/reactions.rs` | `screens/reactions/+` | `components/reactions/+` |
| threads | 11 | `features/threads.rs`, `state/threads.rs` | `screens/threads/+` | `components/threads/+` |
| attachments | 12 | `features/{attachments,gifs}.rs`, `state/attachments.rs` | `screens/attachments/+` | `components/attachments/+` |
| dms | 13 | `features/{dms,trust}.rs`, `state/{dms,trust}.rs`, `sync/trust.rs` | `screens/dms/+` | `components/dms/+` |
| members | 14 | `features/{members,roles,moderation}.rs`, `state/{members,roles}.rs` | `screens/members/+` | `components/members/+` |
| server settings | 15 | `features/server_admin.rs` | `screens/server_settings/+` | `components/server_settings/+` |
| user settings | 16 | `features/{profile,devices}.rs` | `screens/user_settings/+` | — |
| voice | 17 | `features/voice.rs`, `state/voice.rs` | `screens/voice/+` | `components/voice/+` |
| ring | 18 | `features/ring.rs`, `state/ring.rs` | `screens/ring/+` | `components/ring/+` |
| voice settings | 19 | `features/voice_prefs.rs` | `screens/voice_settings/+` | `components/voice_settings/+` |
| bot | 20 | `features/{bot,utilities}.rs`, `state/bot.rs` | `screens/commands/+` | `components/commands/+` |
| bot admin | 21 | `features/{bot_admin,music}.rs` | `screens/bot_admin/+` | `components/bot_admin/+` |
| notify | 22 | `features/notify.rs`, `state/unread.rs` | `screens/notify/+` | — |
| search | 23 | `features/search.rs` | `screens/search/+` | `components/search/+` |
| diagnostics | 24 | `features/diagnostics.rs` | `screens/diagnostics/+`, `screens/boot.rs` | — |
| onboarding | 25 | `features/{signup,enrol}.rs` | `screens/onboarding/+`, `screens/create/+` | `components/onboarding/+` |
| factors | 26 | `features/{factors,browser_auth}.rs`, `auth.rs` | `screens/sign_in.rs`, `screens/factor/+` | `components/factor/+` |
| instances | 27 | `instances.rs` | `screens/instances/+`, `windows.rs` | — |

`calls/*`, `seams.rs`, `http/*` and `sync/requests.rs` belong to Task 3. `client.rs`, `sync/mod.rs`, `store.rs` and `account/*` belong to Task 2. `dilla-media` belongs to Task 4 until Wave 1, and to Task 17 after that. `slots.rs`, `keys.rs` and `screens/mod.rs` belong to Task 1; in Wave 1 only the controller edits them, at merge.

- [ ] **Step 1:** Test `route_table_covers_ts_ops`.
  - A node helper dumps the ops that `packages/client-core/src/worker/controller.ts` and `sync/*` dispatch.
  - Assert that every op is either consumed by `sync` or mapped to an `Area` by `route`, and that no op is mapped twice.
- [ ] **Step 2:** Test `prefs_round_trip_keeps_unknown_keys`, and test `prefs_never_hold_secrets`: a set of a key that matches `token|key|secret` panics in debug.
- [ ] **Step 3:** Test `stubs_change_nothing`:
  - with every slot stubbed, Task 12's `slice_parity_channel_view` and `slice_sign_in_read_send` still pass;
  - `router::Route::parse` round-trips every path form in `router.ts`, using the TS tests' examples.
- [ ] **Step 4:** Fix the Task 1 minor from the foundations plan: the window title is set once, through `set_window_title`, and the test pins that.
- [ ] **Step 5:** Run S7 and commit with the message `gpui: area scaffold, slots, prefs, ownership map`.

### Task 2: Client correctness carry-overs (R17 and the "next client task" minors)

**Files:**
- Create: `clients/desktop/dilla-client/src/sync/removals.rs` (port of `packages/client-core/src/sync/removals.ts`)
- Create: `clients/desktop/dilla-client/src/account/{mod.rs,keypackages.rs,backups.rs}` (ports of `account/keypackages.ts` and of `ensureBackups`/`repairBackupState` in `account/enrol.ts`)
- Modify: `clients/desktop/dilla-client/src/{client.rs,sync/mod.rs,store.rs,gateway/mod.rs}`
- Test: `clients/desktop/dilla-client/tests/{removals,keypackages,backups,lifecycle}.rs`

**Interfaces:**
- Consumes: `ClientCore` removals, key packages and backup state (`core/dilla-core/src/client/{removals,moderation,groups}.rs`).
- Produces:
  - `sync::removals::recover_moderation_removals(&Inner)`, run after every ready and on each removal frame;
  - `account::refill_key_packages(&Inner, remaining: u32)`, which publishes up to the TS target whenever the server reports fewer than the TS threshold;
  - `account::ensure_backups` and `account::repair_backup_state`, called at the same points as in `worker/controller.ts`;
  - `Client::factor_completed(&self)`, which leaves `AccountPhase::FactorSetup` without rebuilding the `Client`.

- [ ] **Step 1:** Test `removal_reaches_live_client_as_tombstone` and test `removal_while_offline_is_a_tombstone_after_restart`, mirroring `e2e/web/moderation-removal-recovery.spec.ts`. In both, the removed body is absent from `AppState` and from `TimelineItem.body`.
- [ ] **Step 2:** Test `refills_key_packages_below_threshold`: drain the server to 1 key package, run one ready, and assert the count is back at the TS target. Test `fresh_device_can_be_added_by_welcome`: a second account adds this device's user to a group, and the device reads the next message.
- [ ] **Step 3:** Test `ensure_backups_uploads_missing_state` and test `repair_backup_state_after_list_refresh`. Port the cases from `account/enrol.test.ts`.
- [ ] **Step 4:** Lifecycle fixes. Each gets a test:
  - `disconnect_during_connect_stays_offline`: the Link TOCTOU. Check `app_offline` inside `Link::connect` under the gateway mutex.
  - `renewed_token_after_ready_does_not_restart`.
  - `factor_setup_clears_after_factor_completed`.
  - `engine_panic_reports_failed_not_stopped`: a non-poison panic while the engine runs.
  - `server_timings_are_clamped`: a heartbeat of 0 is raised to the TS floor, and a huge `after_ms` is capped (Task 8 minor).
  - Also: remove the dead `Refusal::Revoked` arm, and correct the `start` doc.
- [ ] **Step 5:** Store fixes (Task 6b minors). Each gets a test:
  - `absent_store_removes_stale_wal_shm_journal`;
  - `store_sets_synchronous_full` (the same as the Bot);
  - `wal_unsupported_filesystem_is_a_clear_error`: an injected `SQLITE_IOERR_SHMOPEN` gives `StoreError::Unsupported`, with a message naming the filesystem;
  - `copied_data_root_does_not_share_keys`: a per-root generation id bound into the keychain account.
- [ ] **Step 6:** Run S7 and commit with the message `gpui client: removals, key packages, backups, lifecycle and store fixes`.

### Task 3: E_ codes, calls over the gateway, end-to-end call (R19b)

**Files:**
- Modify: `clients/desktop/dilla-client/src/{seams.rs,http/mod.rs,http/routes.rs,sync/requests.rs,calls/*}`
- Modify: `clients/desktop/dilla-media/src/transform.rs` (only the `decode_media_epoch` zeroize) and `clients/desktop/dilla-media/tests/glue.rs` (test placement)
- Test: `clients/desktop/dilla-client/tests/{http.rs,calls.rs}`, `clients/desktop/dilla-media/tests/e2e_call.rs`

**Interfaces:**
- Changes (seams): `HttpError::Status { status: u16, code: Option<String> }`, where `code` is the server's `E_*` string. Every matcher in the crate is updated in this task.
- Produces:
  - call signalling (join, media epoch, leave, rotate, decline) goes over `SocketRequests` while the socket is up, and falls back to HTTP exactly where `packages/client-core/src/http/socket-routes.ts` does;
  - the `E_REMOVE_PENDING` rejoin wait, ported from `calls/join.ts`, is surfaced as `CallPhase::WaitingForRemove`.

- [ ] **Step 1:** Test `http_error_carries_e_code`: a refused route gives `Status{ 403, Some("E_PERM") }`. Port the cases in `http/errors.test.ts`.
- [ ] **Step 2:** Test `call_signalling_uses_socket_when_up`, mirroring `e2e/web/socket-only.spec.ts` "joining and leaving voice goes over the socket". A counting `Http` double sees 0 call routes.
- [ ] **Step 3:** Test `rejoin_waits_for_pending_remove`, mirroring `e2e/voice/leave-reload.spec.ts` test 2. Drop A mid-call and rejoin before anyone commits A's Remove. Assert:
  - the phase is `WaitingForRemove`;
  - the join completes once the Remove is committed;
  - the roster has exactly one leaf for A.
- [ ] **Step 4:** Test `desktop_call_through_dillad`: two `dilla-client` + `dilla-media` peers join one voice channel through dillad's real call flow (ticket, MLS call group, LiveKit) and hear each other's 440 Hz and 660 Hz tones. Test `desktop_and_web_call_through_dillad` does the same with the web client (Playwright, reusing `clients/desktop/dilla-media/browser`).
- [ ] **Step 5:** Task 7 and Task 11 minors:
  - `dump-routes.mjs` exports its slot validators and covers same-method/path collapse (`route_table_matches_ts` extended);
  - `Count` refuses `"0"`;
  - password and assertion request bodies are `Zeroizing`;
  - `decode_media_epoch` temporaries are zeroized;
  - the `glue.rs` test is moved below the `use` block.
- [ ] **Step 6:** Run S7 and commit with the message `gpui client: E_ codes, socket call signalling, end-to-end call`.

### Task 4: Device audio, AEC and enhanced noise on the device mic (R19a)

**Files:**
- Create: `clients/desktop/dilla-media/src/{capture.rs,playout.rs,apm.rs}`
- Modify: `clients/desktop/dilla-media/src/{devices.rs,noise.rs,room.rs,lib.rs}`
- Test: `clients/desktop/dilla-media/tests/audio.rs`

**Interfaces:**
- Produces:
  - `capture::Mic::open(device: Option<DeviceId>, mode: NoiseMode) -> Result<Mic, MediaError>`, which runs `cpal` input (48 kHz, 10 ms frames) through APM (AEC, AGC, standard NS) and, for `Enhanced`, DeepFilterNet. It feeds the call's `NativeAudioSource`.
  - `playout::Speaker::open(device)`, which mixes remote tracks to `cpal` output and hands every rendered frame to APM as the AEC reference.
  - `devices::{inputs, outputs, default_input, default_output, watch() -> Receiver<DeviceList>}`.
  - `Call::set_mic` now accepts device mics in every `NoiseMode`; Task 11's `Unsupported` is removed.

- [ ] **Step 1:** Test `aec_removes_playout_echo`: play a 1 kHz tone through `Speaker` into a loopback mixed into `Mic` with 40 ms delay. After AEC, the tone in the sent stream is at least 25 dB lower, and below -30 dBFS.
- [ ] **Step 2:** Test `enhanced_lowers_noise_10db_on_device_path`. Reuse the noise fixture and threshold in `e2e/voice/enhanced-noise.spec.ts`, through the full capture chain with a fake `cpal` host.
- [ ] **Step 3:** Test `enhanced_within_80ms_budget`, from `e2e/voice/enhanced-latency.spec.ts`. Capture to the encrypted sender, measured with an impulse.
- [ ] **Step 4:** Test `missing_model_falls_back_visibly`, from `e2e/voice/enhanced-fallback.spec.ts`: `NoiseMode::Enhanced` without weights gives `Standard` plus a `MediaEvent::EnhancedUnavailable`. `Off` loads no model.
- [ ] **Step 5:** Test `device_hot_unplug_switches_to_default`: a fake host removes the device, and capture continues on the default with one `DevicesChanged` event.
- [ ] **Step 6:** Note for Task 7: the CI images need `libasound2-dev`. Run S7 and commit with the message `gpui media: device capture and playout with AEC, enhanced noise on device mics`.

### Task 5: Screen parity and behaviour harness

**Files:**
- Create: `clients/desktop/parity/src/{screen.rs,fixture.rs}`
- Create: `clients/desktop/parity/shoot.mjs`, which generalises Task 12's `references/shoot-channel-view.mjs`
- Move: Task 12's `channel-view` fixture and reference to `fixtures/shell/channel-view.json` and `references/linux/shell/channel-view-2x.png`
- Modify: `clients/desktop/dilla-desktop/src/automation.rs`, `clients/desktop/parity/src/{driver.rs,lib.rs}`
- Create: `clients/desktop/dilla-client/src/testing.rs` (feature `testing`): the sign-up, server and world helpers taken out of `tests/sync.rs` and `examples/peer.rs` (Task 9b minor)
- Modify: `clients/desktop/dilla-desktop/tests/support/*` and `examples/parity_world.rs`, making them general over a fixture
- Test: `clients/desktop/parity/tests/screen.rs`

**Interfaces:**

```rust
/// One parity state, in a JSON file both shooters read.
pub struct ScreenFixture {
    pub name: String,                // "<area>/<state>[-b]"
    pub window: (u32, u32),          // logical size; shot at scale 2
    pub world: WorldSpec,            // people, servers, channels, messages (reactions, replies, threads, attachments, polls), DMs, roles, calls
    pub viewer: String,              // whose device is shot
    pub route: String,               // router.ts path
    pub clock: u64,                  // frozen "now", unix seconds
    pub steps: Vec<Step>,            // the same actions on both sides
}
pub enum Step {
    Click { web: String, gpui: String },
    Hover { web: String, gpui: String },
    Key { chord: String },
    Type { web: String, gpui: String, text: String },
    Settle,
}
pub fn assert_screen(fixture: &Path) -> Result<ScreenReport, ParityFailure>; // seed → launch → replay → shoot → R13 mask → compare with references/<os>/<name>-2x.png
pub fn text_mask(driver: &mut Driver, grow_px: f32) -> TextMask;            // from `text-bounds` only
```

- Automation additions:
  - `key <chord>`, for named keys and chords (`enter`, `tab`, `shift-tab`, `escape`, `up`, `ctrl-k`, `ctrl-shift-m`, …);
  - `hover <id>`;
  - `focused` (the id of the focused element);
  - `scroll <id> <dy>`;
  - `wait-idle`.
- A `compile_error!` in `automation.rs` when `debug_assertions` is off and the feature is on (Task 10 minor).

- [ ] **Step 1:** Test `text_mask_matches_painted_lines`: every rect lies inside the window, and on the channel view the masks cover all glyph pixels, so masked text differences are 0.
- [ ] **Step 2:** Test `channel_view_two_fixtures`: Task 12's fixture and its varied fixture pass `assert_screen` with the committed tolerance.
- [ ] **Step 3:** Negative fixtures. Each must fail (Task 10b minor):
  - `wrong_fill_colour_fails`: the surface is one token step off;
  - `wrong_border_colour_fails`;
  - `fallback_font_fails`, kept from Task 12.

  Crop the fixture images (Task 10b minor).
- [ ] **Step 4:** Tests `named_keys_reach_focused_element` (Enter, Tab, Escape and Ctrl+K reach the focused element), `focused_reports_id`, and `hover_changes_state`.
- [ ] **Step 5:** Test `no_window_coordinates_in_screens`: a source scan of `dilla-desktop/src/screens/**` fails on `px(<literal>)` and on `point(` with literal arguments. Allowed are only `tokens::*` and the documented sub-pixel helper in `text.rs`. This is the R21 tripwire.
- [ ] **Step 6:** Test `shooters_agree_on_fixture`: the web shooter and the GPUI driver log the same fixture digest and world ids.
- [ ] **Step 7:** Remaining Task 10 and Task 3 minors:
  - `symbol_needles_positive_control`;
  - strict hex parsing (no `+`);
  - `target_dir` through `serde_json`;
  - a test for the driver timeout path;
  - `validate-tolerance.sh` reports a crash as a failure.
- [ ] **Step 8:** Run S7 and commit with the message `gpui parity: screen fixtures, shooter, R13 masks, named keys`.

### Task 6: Shared components, settings frame, strings, time formats

**Files:**
- Create: `clients/desktop/dilla-desktop-ui/src/components/{dialog,confirm_dialog,action_menu,tooltip,toast,text_field,toggle,segmented,select,banner,link_banner,empty_state,pill,tag,check_list,choice_cards,icon_button,key_hint,trailing_slot,state_dot,audio_meter,qr_code}.rs`
- Create: `clients/desktop/dilla-desktop-ui/src/components/settings/{frame,nav,row,footer}.rs`
- Modify: `clients/desktop/dilla-desktop-ui/src/components/{button,row,badge,avatar}.rs`: add the focus ring, press scale, mention ring, DM person row, unread dot and density variants (Task 5 minors)
- Create: `scripts/gen-gpui-strings.mjs` (+ `.test.mjs`) and `clients/desktop/dilla-desktop/src/strings.rs` (`// @generated` from `packages/web/src/strings/*.ts`)
- Create: `clients/desktop/dilla-desktop/src/{format.rs,edit_state.rs}`
- Ports:
  - `packages/ui/src/{Dialog,ConfirmDialog,ActionMenu,Tooltip,Toast,TextField,Toggle,Segmented,Select,Banner,LinkBanner,EmptyState,Pill,Tag,CheckList,ChoiceCards,IconButton,KeyHint,TrailingSlot,StateDot,AudioMeter,QrCode,SettingsFrame,SettingsNav,SettingsRow}`;
  - `packages/ui/src/internal/roving.ts`;
  - `packages/web/src/screens/settings/edit-state.tsx`;
  - the date and time parts of `packages/web/src/screens/conversation/log-model.ts`.

**Interfaces:**
- `strings::t("activity.title") -> &'static str` and `strings::tf("activity.newCount", &[("n", "3")]) -> String`.
- `format::{time, day_divider, relative}` follow the OS locale and the 24-hour setting (`sys-locale`).
- `EditState`: dirty-draft guard with Keep editing and Discard, the same as `edit-state.tsx`.
- `Roving`: arrow and Home/End focus in lists.
- Every component has a stable automation id prefix.

- [ ] **Step 1:** Test `strings_generator_is_fresh` and test `every_ts_key_present`.
- [ ] **Step 2:** Test `format_matches_log_model_dates`: port the cases of `log-model.dates.test.ts`, including the 24-hour and locale variants.
- [ ] **Step 3:** Parity: a component gallery example, `dilla-desktop/examples/gallery.rs`, renders every component state. For the stories each component has, write fixtures `components/<name>[-b]`. The web side is shot through the parity tooling's story path (`docs/design/signal/parity/shoot.mjs`). All of them pass.
- [ ] **Step 4:** Behaviour:
  - `dialog_traps_focus_and_escape_returns_it`;
  - `action_menu_opens_on_shift_f10_and_menu_key`;
  - `toast_auto_dismisses_after_ui_timeout`;
  - `dirty_settings_draft_needs_confirmation`, from `fullscreen-settings-dirty-draft-confirmation.png` and the test in `e2e/web/search-settings.spec.ts`.
- [ ] **Step 5:** Run S7 and commit with the message `gpui ui: shared components, settings frame, strings, formats`.

### Task 7: CI wiring and chores

**Files:**
- Modify: `.github/workflows/desktop-gpui.yml`, `scripts/ci-local*.mjs`, `scripts/check-ci-workflow*`, `deny.toml`
- Modify: `core/testkit` (rename the guard)
- Modify: `e2e/web/support/gpui-peer.ts` (stale text only)

- [ ] **Step 1:** Add to CI:
  - `libasound2-dev` (Task 4);
  - the strings freshness test (Task 6);
  - `--ignored` steps for the new client suites (Tasks 2 and 3);
  - the parity suite on Linux, with the shooter's Chromium;
  - one matrix entry per Wave 1 test file, as each task merges. The controller appends these lines; the pattern is set here.
- [ ] **Step 2:** `check-ci-workflow` pins the new steps, the `DILLA_*` env and the `--ignored` runs. Remove the stale "no Go toolchain" comment in `gateway.rs`, add the deliberate-WASI-rebuild comment, and strip the debug peer binary or build it with `--release`.
- [ ] **Step 3:** `deny.toml` triage, the Task 1 minor flagged FINAL REVIEW MUST TRIAGE:
  - version-scope the crypto `bans.skip` entries (sha2, hmac, hkdf, aes, cipher, digest, sha1, rand);
  - give each skip a real reason;
  - fix the blank line.
- [ ] **Step 4:** Rename the testkit `lock_file` guard to match what it runs (`cargo tree --offline`).
- [ ] **Step 5:** Run `npm run ci:local` (all green) and commit with the message `gpui ci: wave 0 wiring and chores`.

---

## Wave 1 (one agent per area; standard procedure S1–S7)

### Task 8: Shell and navigation

- **Ports:**
  - web: `packages/web/src/screens/{Shell.tsx,shell-model.ts,UserPanel.tsx}`, `packages/web/src/{router.ts,layout.ts,DesktopTitlebar.tsx}`;
  - ui: `packages/ui/src/{AppShell,Splitter (in AppShell),CommunityRail,ChannelList,ChannelRow,ChannelHeader,TopBar,UserPanel,Tooltip}`;
  - window shapes: `packages/desktop/src/window-layout.ts`.
- **Owns:** shell files per the map; `prefs` keys `layout.*`.
- **Builds:**
  - rail and channel list with categories, collapse and the AFK channel listed last;
  - channel context menu (mark read, mute, copy link; "+" opens Task 25's create-channel flow through `router`);
  - header with tooltip;
  - splitter for sidebar and aside, with keys and widths that persist;
  - user panel (mute and deafen buttons call Task 17's handle);
  - custom titlebar and window controls;
  - window modes: start 400×280, authentication, and workspace 1240 × clamp(600..820) with a minimum of 900×600 and bounds restored;
  - route history.
- **Parity:** `shell/conversation`, `shell/categories`, `shell/panes-resized`, `shell/header-tooltip`, `shell/unread-badge`, `shell/removed-server`, each with a `-b` fixture. Sources: `docs/user/screenshots/{shell-conversation,categories-desktop,panes-resized-desktop,header-tooltip-desktop,shell-unread-badge,shell-removed}.png`.
- **Behaviour:**
  - `e2e/web/panes.spec.ts` → `sidebar_splitter_drags_takes_keys_and_persists`, and `aside_splitter_only_with_aside`;
  - `e2e/web/layout.spec.ts`, desktop cases → `layout_grammar_holds_at_1280_and_1440`, and `desktop_home_title_is_not_server_name`;
  - `e2e/web/afk.spec.ts` test 1 → `afk_channel_listed_last_for_owner_and_member`;
  - `e2e/web/keyboard.spec.ts` → `messaging_flow_on_keyboard_alone_in_fixed_order`;
  - `packages/desktop/test/electron/shell.spec.ts` → `startup_and_sign_in_compact_window_expands_for_workspace`, and `resize_cannot_shrink_below_desktop_minimum`.
- **Keyboard:** Tab order rail → channel list → header → log → composer; roving arrows in the rail and the channel list; splitter arrows; every navigation binding listed in `Keyboard.tsx`.

### Task 9: Messages: row, markdown, code, mentions, edit/delete, pins, composer

- **Ports:**
  - web conversation: `packages/web/src/screens/conversation/{Conversation,ComposerArea,MessageItem,Pins}.tsx` and `{log-model,slow,manual-removal,webhook}.ts`;
  - web: `packages/web/src/screens/markdown.ts`;
  - ui: `packages/ui/src/{MessageLog,MessageRow,MessageToolbar,MessageEditor,Composer,CodeBlock,MentionList,TypingIndicator,PinsDialog}`.
  - Logic tests to port: `markdown.test.ts`, `MessageItem.markdown.test.tsx`, `slow.test.ts`, `log-model*.test.ts`.
- **Builds:**
  - message grouping, date dividers and jump flash;
  - markdown with the exact `markdown.ts` grammar;
  - code blocks with the `highlight.ts` port;
  - sender-made link previews, rendered only (never fetched);
  - mention highlight and the mention popup;
  - edit, delete and pin/unpin;
  - pins dialog;
  - typing indicator, sent and received;
  - per-channel drafts, kept in prefs;
  - slow-mode wait and queue, and the read-only notice for announcement channels;
  - webhook and bot author labels;
  - the toolbar built from `slots::message_actions`.
- **Also:** strengthen the Task 4 proof tests:
  - the dead key goes through the real keystroke path;
  - the `mention_at` cursor stays inside the text;
  - the paste fallback is a real binding;
  - emoji backspace asserts the exact text.
- **Parity:** `messages/{conversation-actions,edit,mention,pins,code-block,slow-mode,announcement}` plus `-b` fixtures. Sources: `docs/user/screenshots/conversation-{actions,edit,mention,pins}.png` and `shell-conversation.png`.
- **Behaviour:**
  - `e2e/web/messaging.spec.ts` → `text_both_ways_mls_sender_and_once_after_update`;
  - `e2e/web/conversation.spec.ts`:
    - "edit, reply, react, pin and delete" (its edit, pin and delete parts) → `edit_pin_delete_between_two_devices`;
    - "an edit or a delete by someone else changes nothing" → `foreign_edit_or_delete_changes_nothing`;
    - "mentions" → `mention_highlights_and_badges`;
    - "B sees what A makes" → `peer_sees_what_author_makes`;
  - `e2e/web/slowmode-announce.spec.ts` → `slow_mode_wait_queue_and_announcement_read_only`;
  - `e2e/web/native-interop.spec.ts`: kept as is (Task 9b);
  - own → `receiver_never_fetches_link_preview`: a recording `Http` double and a recording DNS see nothing for the link's host.
- **Keyboard:** Enter sends; Shift+Enter adds a newline; Up edits the last own message; Escape cancels an edit; in the mention popup, arrows, Tab and Enter choose and Escape closes; PageUp/PageDown scroll the log.

### Task 10: Reactions and emoji

- **Ports:** `packages/ui/src/{ReactionBar,ReactionTooltip,EmojiGrid}` and `packages/web/src/screens/emoji.ts`, plus the reaction parts of `MessageItem.tsx`.
- **Builds:** `features::reactions` (react and unreact through `ClientCore`); the reaction bar slot; the emoji grid from the composer button and from the toolbar action; the "who reacted" tooltip.
- **Parity:** `reactions/{bar,many-wrapping,tooltip,emoji-grid}` plus `-b` fixtures (2-digit counts, 12 distinct emoji, a long reactor list).
- **Behaviour:**
  - `e2e/web/conversation.spec.ts` (its react part) → `react_and_unreact_between_two_devices`;
  - `e2e/web/core-worker.spec.ts` (its fold of reactions) → `own_second_device_sees_reaction`.
- **Keyboard:** the emoji grid takes arrows, Enter and Escape, and focus returns to the opener; on a reaction chip, Enter toggles.

### Task 11: Replies and threads

- **Ports:** `packages/web/src/screens/conversation/ThreadView.tsx`, `packages/web/src/thread-request.ts`, `packages/ui/src/{ReplyChip,ThreadPane}`, strings `thread.ts`.
- **Builds:**
  - `features::threads`: reply, open a thread, page it, separate thread drafts, and thread mention badges;
  - the reply chip that jumps to the original;
  - the reply bar in `composer_leading`;
  - the thread pane in `slots::aside` (340 px);
  - the thread count on messages.
- **Parity:** `threads/{reply-chip,pane-open,count}` plus `-b` fixtures (a long first line, 2-digit reply counts, 1440×900).
- **Behaviour:** `e2e/web/threads.spec.ts`:
  - test 1 → `thread_reply_count_both_sides_aside_and_kept_after_restart`;
  - test 2 → `thread_mention_badges_channel_reply_stays_out`;
  - test 3 → `edit_and_delete_own_thread_reply`;
  - tests 4 and 5 go to Task 28, because they need search and pins.

  Also `e2e/web/conversation.spec.ts` (its reply part) → `reply_chip_jumps_to_original`.
- **Keyboard:** Escape closes the pane and returns focus to the message; the toolbar's reply action focuses the composer with the reply bar.

### Task 12: Attachments, media and GIFs

- **Ports:**
  - web: `packages/web/src/screens/conversation/{Attachments,GifPicker,useBlobUrl}.tsx`, `{use-gif-community,dm-gif-consent}.ts`, `settings/DmGifPrivacySetting.tsx`;
  - ui: `packages/ui/src/{AttachmentTray,AttachmentCard,DropOverlay,Lightbox,MediaPlayer,GifPicker}`;
  - client-core: `packages/client-core/src/attachments/{crypto,download,name,thumb,tray}.ts`, and the gif routes.
- **Builds:**
  - `features::attachments`:
    - encryption through `dilla_core::attachment`;
    - upload with the 25 MB cap;
    - download checks `size + 16` before decrypting (Task 7 minor);
    - thumbnails;
    - save to disk, only after a user action;
  - `features::gifs`: search through the instance's GIF routes, and DM GIF consent;
  - drag and drop, and paste of images;
  - the attachment tray;
  - cards and image grid;
  - lightbox;
  - GIF picker;
  - `MediaPlayer` card. Audio/video playback goes behind a `MediaDecoder` trait; the implementation waits for Jonas (see Questions). Images and animated GIFs work fully.
- **Parity:** `attachments/{cards,lightbox,media-player,gif-picker,drop-overlay,oversized-toast}` plus `-b` fixtures (9 images, long file names, 1440×900). Sources: `docs/user/screenshots/conversation-{attachments,lightbox}.png` and `media-player-desktop.png`.
- **Behaviour:** `e2e/web/conversation.spec.ts`:
  - "images and files go both ways" → `images_and_files_both_ways`;
  - "a 25 MB file … one byte more" → `file_cap_exact_passes_one_more_refused`;
  - DM GIF consent (`DmGifPrivacy.test.tsx`) → `dm_gif_needs_consent`;
  - `e2e/voice/media-attachments.spec.ts` → `audio_attachment_plays`. This one is `#[ignore]` until the `MediaDecoder` decision.
- **Keyboard:** Ctrl+V pastes an image; in the lightbox, arrows move and Escape closes; the GIF picker takes arrows and Enter.

### Task 13: DMs and trust

- **Ports:**
  - web: the DM list and `DmPicker` in `packages/web/src/screens/Shell.tsx` (~l. 2329), `settings/Privacy.tsx`;
  - ui: `packages/ui/src/{TrustAlert,SafetyCompare}`;
  - client-core: `packages/client-core/src/state/trust.ts`.
  - Logic tests to port: `Privacy.test.tsx`, `trust.test.ts`.
- **Builds:**
  - `features::dms`: open or create a DM, the DM list, and DM unread;
  - `features::trust`:
    - the blocks `E_UMK_CHANGED`, `E_CREDENTIAL` and `E_DEVICE_*`;
    - compare by safety number and QR;
    - verify;
    - remove the changed device;
  - the Home/DMs view;
  - DM conversations, reusing Task 9's views;
  - the Privacy settings section.
- **Parity:** `dms/{home,conversation}` and `trust/{changed,compare,conversation,privacy}` plus `-b` fixtures. Sources: `home-dms-desktop`, `dm-conversation`, `trust-{changed,compare,conversation,privacy}-desktop` and `fullscreen-settings-privacy-desktop-signal`.
- **Behaviour:** `e2e/web/trust-polish.spec.ts`:
  - → `rotation_keeps_visible_text_independent_group_works`;
  - → `copy_alone_does_not_approve_exact_compare_admits`;
  - → `privacy_names_and_compare_after_restart`;
  - → `remove_changed_device_restores_sending_here_only`.
- **Keyboard:** the DM picker has type-ahead, arrows and Enter; the Compare dialog traps focus, and Escape returns it.

### Task 14: Members, profiles, roles and moderation

- **Ports:**
  - web admin: `packages/web/src/screens/admin/{MemberPane,MemberCardHost,MemberRolesFlow,Moderation,ModerationLog,BansSection,RolesSection,SelfRolesSection,MembersSection}.tsx` and `{member-card,members-model,role-palette}.ts`;
  - ui: `packages/ui/src/{MemberList,ProfileCard,RoleEditor,PermissionGrid,ColorSwatches,ManageList,PersonContextMenu}`;
  - client-core: `packages/client-core/src/admin/{authority,controller,perm,rank,roles}.ts`, with the vectors from their tests.
- **Builds:**
  - the members aside and the profile card;
  - the person context menu;
  - kick, ban, unban, timeout and warn with evidence;
  - the role editor and permission grid;
  - assigning roles;
  - self roles;
  - the server settings sections Roles, Self roles, Members, Bans and Moderation log.
- **Parity:** `members/{pane,profile-card,context-menu,moderation-dialog}` and `server-settings/{members,roles,self-roles,bans,moderation-log}` plus `-b` fixtures (2-digit member counts, long names, 15 roles). Sources: `members-desktop` and `fullscreen-settings-members-desktop`.
- **Behaviour:**
  - `e2e/web/roles.spec.ts` → `roles_make_assign_pin_by_right_mention_demote_delete`;
  - `e2e/web/admin.spec.ts` (its members, kick, ban and unban parts) → `members_kick_ban_unban`;
  - `e2e/web/membership.spec.ts` → `kicked_client_told_loses_server_readmitted_flows`;
  - `e2e/web/timeout.spec.ts` → `timeout_blocks_community_writes_and_recovers`.
- **Keyboard:** roving in the member list; Shift+F10 or the Menu key opens the person menu; Escape closes the profile card.

### Task 15: Server settings

- **Ports:**
  - web admin: `packages/web/src/screens/admin/{ServerSettings,OverviewSection,ChannelsSection,ChannelEditFlow,ChannelPermissions,GifsSettings,InvitesSection,WelcomeSettingsPanel,DangerSection}.tsx` and `{welcome-settings,shared}.ts`;
  - ui: `packages/ui/src/{ChannelSettings,InvitePanel,InviteLink}`.
- **Builds:** `features::server_admin`, and the sections:
  - Overview (name, AFK channel and timeout);
  - Channels (with per-channel permissions, private channels and reorder);
  - GIFs (key, rating);
  - Invites;
  - Welcome and join roles;
  - Danger zone (transfer, leave, delete).

  Sections are hidden according to permissions, as in `ServerSettings.tsx`.
- **Parity:** `server-settings/{overview,afk,channels,channel-permissions,gifs,invites,welcome,danger}` plus `-b` fixtures. Sources: `fullscreen-settings-overview-desktop` and `afk-settings-desktop`.
- **Behaviour:**
  - `e2e/web/admin.spec.ts` (its rename, channel edit, leave, hand-over and delete parts) → `server_rename_channel_edit_leave_hand_over_delete`;
  - `e2e/web/afk.spec.ts` tests 2 and 3 → `afk_rename_and_move_into_category_seen_live`, and `afk_has_no_delete_permissions_or_reorder`;
  - `e2e/web/private.spec.ts` → `private_channels_create_gain_lose_toggle_lockout`;
  - `e2e/web/sidebar-actions.spec.ts` (its member-makes-a-link part) → `member_makes_invite_link_under_invites`.
- **Keyboard:** the settings nav takes arrows; Escape closes the settings; the dirty-draft guard comes from Task 6.

### Task 16: User settings

- **Ports:** `packages/web/src/screens/settings/{Profile,Devices,RenameDialog,SignOut,KeyDialog,Appearance,Keyboard}.tsx`, `{device-name,device-platform}.ts`, `packages/web/src/screens/Settings.tsx` (user registry), `packages/ui/src/KeyboardTable`.
- **Builds:** the sections Profile, Devices (rename, sign out and remove, key dialog), Appearance (density compact/regular/cozy, reduced motion, wired into Task 6's density variants) and Keyboard. The other user sections belong to other tasks: Privacy to 13, Voice to 19, Notifications to 22, Diagnostics to 24 and Sign-in to 26.
- **Parity:** `user-settings/{profile,profile-short-wide,devices,devices-sign-out,appearance,keyboard}` plus `-b` fixtures. Sources: `fullscreen-settings-profile-desktop-signal`, `fullscreen-settings-profile-short-wide-signal`, `fullscreen-settings-devices-desktop-signal`, `settings-devices` and `settings-devices-signout`.
- **Behaviour:**
  - `e2e/web/enrol.spec.ts`:
    - "… the first can remove it" → `remove_other_device_from_devices`;
    - "sign out and remove …" → `sign_out_and_remove_leaves_account_and_group`;
  - `e2e/web/search-settings.spec.ts` (dirty Profile draft) → `dirty_profile_draft_keep_editing_then_discard`.
- **Keyboard:** the Keyboard section lists exactly the bindings `keys.rs` registers. Test `keyboard_table_matches_bindings`.

### Task 17: Voice call UI: dock, stage, takeover, controls, share UI

- **Ports:**
  - web voice: `packages/web/src/screens/voice/{VoiceView.tsx,useCall.tsx,tile-stats.ts,shortcuts.ts}`;
  - web call: `packages/web/src/call/{session.ts,share-options.ts,share-audio-advice.ts}`;
  - ui: `packages/ui/src/{CallStage,CallTile,CallControls,CallBar,VoiceDock,VoiceChannelRow,SharePicker,UnsupportedVoice}`.
- **Owns:** `dilla-media` from Wave 1 on.
- **Builds:**
  - `features::voice`: the voice-state roster, join and leave, mute and deafen, and the speaking signal;
  - the voice channel row with nested people (`slots::channel_row_extras`);
  - the dock (`slots::dock`);
  - the stage in grid and focus layouts, which takes over the conversation area, plus tile stats;
  - the controls, including the stream-quality split button;
  - publish and subscribe of camera and screen-share video through a `CaptureSource` trait, in `dilla-media/src/capture_source.rs`. Real screen and camera sources come in the next plan; tests use the `dilla-test-frames` test-pattern source;
  - GPUI remote video tiles. Record the `VIDEO <os> <cpu%> <dropped>` numbers Task 2 Step 4b still owes.
- **Parity:** `voice/{dock,stage,stage-focused,stage-grid,controls-share-menu}` plus `-b` fixtures (12 participants, long names). Sources: `voice-dock-desktop`, `dock-desktop` and `voice-stage{,-focused,-grid}-desktop`.
- **Behaviour:**
  - `e2e/voice/join-talk.spec.ts` → `two_clients_join_see_and_hear_no_key_in_state`;
  - `e2e/voice/join-muted.spec.ts` → `mute_before_join_mutes_every_sender`, and `muted_join_shows_glyph_and_self_mute`;
  - `e2e/voice/controls.spec.ts` → `ctrl_shift_m_d_and_push_to_talk_while_held`;
  - `e2e/voice/speaking.spec.ts` → `speaking_lights_tile_for_peer`;
  - `e2e/voice/stage.spec.ts` → `stage_tile_per_stream_focuses_share`, and `dm_call_box_keeps_compact_tiles_on_share`;
  - `e2e/voice/share.spec.ts` → `share_and_camera_reach_peer_verified_and_stop`, `share_without_sound_says_so`, and `cancelled_selection_publishes_nothing`;
  - `e2e/voice/reload.spec.ts` and `leave-reload.spec.ts` test 1 → `restart_mid_call_is_a_leave_rejoin_is_fresh_leaf`, and `leave_returns_focus_to_voice_row`;
  - `e2e/web/channel-list.spec.ts` → `voice_channels_in_order_people_nested`;
  - `e2e/voice/realtime-transport.spec.ts` → `call_reports_over_socket_update_deferred`.
- **Keyboard:** Ctrl+Shift+M mutes; Ctrl+Shift+D deafens; push-to-talk works while the window is focused; arrows move between stage tiles; Escape leaves focus mode.

### Task 18: Ring, DM calls, blocked calls, AFK in calls

- **Ports:** `packages/web/src/screens/voice/Ring.tsx`, the DmCall part of `VoiceView.tsx`, `packages/ui/src/RingBanner`, `packages/client-core/src/calls/{ring,sweep,voice}.ts`, and `packages/client-core/src/admin/afk.ts`.
- **Builds:**
  - `features::ring`: ring, answer, decline, busy and timeout;
  - DM call start;
  - the blocked-call flows;
  - the AFK move and its notice.

  Sounds come from Task 19's synth. OS notifications go through the `Notifier` trait from Task 22.
- **Parity:** `ring/{incoming,declined,busy}`, `voice/blocked` and `voice/afk-call` plus `-b` fixtures. Sources: `call-ring-desktop`, `voice-blocked-desktop` and `afk-call-desktop`.
- **Behaviour:**
  - `e2e/voice/ring.spec.ts`: answer, decline, busy, blocked DM and timeout → five tests with those names;
  - `e2e/voice/blocked.spec.ts` (a)–(c) and DM → `blocked_contact_refuses_join_before_any_call_write`, `unlisted_device_mid_call_loses_dock_and_leaf`, `identity_change_mid_call_pauses_until_verified`, and `blocked_dm_call_keeps_composer_enabled`;
  - `e2e/web/afk.spec.ts` tests 4 and 5 → `idle_desktop_moved_told_and_listed_away`, and `back_to_general_keeps_mute_state`.
  - Test 6 (a phone) is out of scope.
- **Keyboard:** while the ring banner is shown, Enter answers and Escape declines; focus moves to the banner when a ring arrives only if the window is focused.

### Task 19: Voice settings, devices, microphone test, sounds

- **Ports:**
  - web settings: `packages/web/src/screens/settings/{Voice,MicrophoneTest}.tsx`;
  - web call: `packages/web/src/call/{devices,keys,ptt,speaking,volume,voice-prefs,microphone-test,sounds}.ts`;
  - web voice: `packages/web/src/screens/voice/useSounds.tsx`;
  - ui: `packages/ui/src/{VoiceSettings,DeviceRow,SensitivityMeter}`.
- **Builds:**
  - `features::voice_prefs`: devices, sensitivity, noise mode, push-to-talk key, per-user volume;
  - the Voice settings section;
  - the microphone test;
  - synthesised sounds, matching the table in `call/sounds.ts`, played through Task 4's `Speaker`.
- **Parity:** `voice-settings/{section,mic-test-running,enhanced-unavailable}` plus `-b` fixtures. Sources: `voice-settings-desktop` and `fullscreen-settings-voice-desktop-signal`.
- **Behaviour:**
  - `e2e/voice/microphone-test.spec.ts` → `lets_check_plays_back_shows_level_releases_mic`;
  - `e2e/voice/sensitivity.spec.ts` → `under_threshold_not_sent_threshold_persists`;
  - `e2e/voice/noise-beta.spec.ts` → `enhanced_opt_in_preview_and_call_leave_during_call`;
  - `e2e/voice/enhanced-noise.spec.ts` → `automatic_enhances_test_and_call_enforces_ptt`;
  - `e2e/voice/enhanced-fallback.spec.ts` → `missing_model_falls_back_visibly_off_loads_nothing`;
  - own → `sounds_match_synth_table`.
- **Keyboard:** binding the push-to-talk key captures the next chord, and Escape cancels the capture.

### Task 20: Bot commands, private results, polls, reminders

- **Ports:**
  - web commands: `packages/web/src/screens/commands/*`, which is `CommandHost`, `CommandPicker`, `picker-catalog`, `picker-when`, `UtilityCommandProvider`, `bot-handlers`, `bot-run`, `builtin`, `command-bot-store`, `PollCard`, `BotCommandDialogs`, `private-results`, `capabilities`, `result-bot-name` and `utility-time`;
  - ui: `packages/ui/src/{CommandPicker,BotResult,PollCard,PollDialog,RemindersPopover}`;
  - client-core: `packages/client-core/src/bot/{catalog,commands,utilities,hints,operations,protocol,utility-time}.ts` and `commands/{classic,registry}.ts`.
- **Builds:**
  - `features::{bot,utilities}` over `dilla_core` bot commands (sealed);
  - the picker in `slots::composer_popup`, with chips, invalid ring and source tags;
  - private results ("only you can see"), which auto-dismiss by the TTL in `private-results.ts`;
  - polls, with the dialog, voting and closing;
  - reminders and the Upcoming popover;
  - moderation confirmations.
- **Parity:** `commands/{list,chips,private-result,picker-greyed}` and `polls/{card,dialog}` plus `-b` fixtures. Sources: `signal-command-{list,chips}`, `signal-private-result`, `signal-poll-{card,dialog}` and `bot-picker-greyed-desktop`.
- **Behaviour:** five tests from `e2e/web/slash-commands.spec.ts` and three from `e2e/web/bot-commands.spec.ts`.
  - From `slash-commands.spec.ts`:
    - → `private_results_stay_private_and_auto_dismiss`;
    - → `polls_publish_vote_reject_forged_close_by_creator`;
    - → `moderation_needs_confirmation_targets_distinct_member`;
    - → `reminders_sync_and_stay_due_after_restart`;
    - → `reminder_from_chips_under_upcoming`.
  - From `bot-commands.spec.ts`:
    - → `picker_shows_bot_commands_offline_after_sync`;
    - → `remind_round_trip`;
    - → `enter_to_first_answer_p95_within_rtt_plus_100ms`.
- **Keyboard:** `/` opens the picker; arrows move; Tab moves to the next chip; Enter runs; Escape closes and keeps the draft.

### Task 21: Bot admin and music

- **Ports:**
  - web admin: `packages/web/src/screens/admin/{BotSection,BotHealth,BotWebhooks,BotSettingsPages,MusicProviderStatus}.tsx` and `bot-settings-context.tsx`;
  - web voice: `packages/web/src/screens/voice/music.tsx`;
  - ui: `packages/ui/src/NowPlaying`;
  - client-core: `packages/client-core/src/bot/{admin,moderation-log,provider-status}.ts` and `voice/music.ts`.
- **Builds:**
  - the Bot server settings: health, Bot channels, webhooks, the moderation log page, and the music provider;
  - the now-playing card and queue in the dock slot.
- **Parity:** `bot-admin/{health-channels,channel-toggle,webhook-post,music}` and `music/now-playing` plus `-b` fixtures. Sources: `bot-page-health-channels-desktop`, `bot-channel-toggle-desktop`, `bot-webhook-post-desktop` and `fullscreen-settings-music-desktop`.
- **Behaviour:**
  - `e2e/web/bot-channels.spec.ts` → `welcome_channel_greets_new_member`, and `webhook_post_appears`;
  - `e2e/web/bot-runtime.spec.ts`, the desktop-relevant tests → `bot_enabled_reaches_ready`, `music_text_never_executes`, `moderation_log_across_bot_restart`, `join_role_applied_on_join_and_rejoin`, and `self_role_panel_reaction_grant`;
  - `e2e/web/moderation-evidence.spec.ts` → `warning_retrievable_from_bot_settings`;
  - `e2e/voice/bot-music.spec.ts` → its six tests, with the same names snake-cased and the same thresholds.
- **Keyboard:** the now-playing controls are reachable by Tab; Space plays and pauses when the card is focused.

### Task 22: Notifications, badges, activity

- **Ports:** `packages/web/src/{notify,notify-permission}.ts`, `packages/web/src/screens/{Activity.tsx,community-activity.ts}`, `settings/Notifications.tsx`, `packages/client-core/src/state/{badges,mentions}.ts`.
- **Builds:**
  - `features::notify`:
    - mention, DM and ring decisions;
    - mute per channel and per server;
    - the device-wide default;
    - the "channel on screen" suppression;
    - a total badge count for the later tray and dock icon;
  - a `Notifier` trait (`dilla-client::features::notify::Notifier`) with an in-app recording implementation. OS delivery comes in the next plan;
  - the Notifications settings section;
  - Activity, but only if the desktop layout reaches it (check `shell-model.ts`; otherwise mark it out of scope in S1).
- **Parity:** `notify/{settings,unread-badge,dm-badge}` plus `-b` fixtures. Sources: `settings-notifications`, `fullscreen-settings-notifications-desktop-signal` and `shell-{unread,dm}-badge`.
- **Behaviour:** `e2e/web/badges.spec.ts`, all five tests:
  - → `unopened_channels_badge_open_reads_mute_hides_count`;
  - → `dm_list_menu_sets_device_notify_default`;
  - → `dm_from_tab_reaches_peer_reply_badges_until_read`;
  - → `notifications_follow_default_and_channel_on_screen`;
  - → `dm_opened_by_peer_appears_badges_notifies_live`.
- **Keyboard:** none beyond Task 6's components; covered by S6.

### Task 23: Search palette

- **Ports:** `packages/web/src/screens/{Search.tsx,palette.ts}`, `packages/ui/src/SearchPalette`, `packages/client-core/src/state/search.ts` (core `search.rs`).
- **Builds:** `features::search`; a palette with the scopes channels, people, messages, settings and actions; a result list; opening a hit, including a settings section.
- **Parity:** `search/{palette,results,scope-settings}` plus `-b` fixtures. Source: `search-results-desktop`.
- **Behaviour:** `e2e/web/search-settings.spec.ts`:
  - → `edit_found_by_prefix_deleted_text_gone`;
  - → `palette_finds_setting_opens_section`;
  - → `scopes_and_selection_stay_on_textbox`.
- **Keyboard:** Ctrl+K opens the palette; arrows move; Enter opens; Tab cycles the scopes; Escape closes.

### Task 24: Diagnostics and app status

- **Ports:** `packages/web/src/screens/settings/Diagnostics.tsx`, `packages/ui/src/{DiagnosticsPanel,Splash}`, `packages/web/src/screens/Boot.tsx`, `packages/web/src/{update-view,update-checkpoint}.ts` (the "update required" surface), and `LinkBanner`.
- **Builds:**
  - `features::diagnostics` (routes table, connection and wire info);
  - the Diagnostics section;
  - boot splash and fatal screen;
  - the offline link banner;
  - the version-refused banner (`AccountPhase::VersionRefused`), which keeps the draft.
- **Parity:** `diagnostics/{section,routes}`, `app/{boot,fatal,link-offline,update-required}` plus `-b` fixtures. Sources: `settings-diagnostics-desktop`, `fullscreen-settings-diagnostics-desktop-signal` and `link-offline-desktop`.
- **Behaviour:**
  - `e2e/web/version-reload.spec.ts` → `older_client_refused_banner_no_reconnect_keeps_draft`;
  - `e2e/web/socket-only.spec.ts` test 1 → `no_app_http_while_socket_up`;
  - `e2e/web/realtime-transport.spec.ts` → `idle_socket_no_periodic_http`, over 15 server-driven heartbeat intervals.
- **Keyboard:** S6 only.

### Task 25: Onboarding, sign-up, create and join

- **Ports:**
  - web: `packages/web/src/screens/{Onboarding,Create,JoinCommunity}.tsx`, `recovery-file.ts`, `packages/web/src/invite.ts`;
  - ui: `packages/ui/src/{OnboardingFrame,RecoveryKey,RecoveryKeyField,CreateServer,CreateChannel,AddServerDialog}`;
  - client-core: `packages/client-core/src/account/{signup,enrol}.ts` and `recovery-key.ts`.
- **Builds:**
  - `features::{signup,enrol}`;
  - the sign-up steps: invite, name, recovery key (save to file), browser/passkey, factor choice (which embeds Task 26's factor views), done;
  - enrolling a second device by password and recovery key;
  - the create server and create channel flows (text, announcement, voice, category);
  - join by invite;
  - the add-server dialog.
- **Parity:** `onboarding/{invite,name,recovery-key,browser,factor-choose,done}` and `create/{empty,server,channel,invite,rail}` plus `-b` fixtures. Sources: `onboarding-{1..5}-*` and `create-{empty,server,channel,invite}-desktop`, `create-rail`.
- **Behaviour:**
  - `e2e/web/signup.spec.ts` → `invite_name_written_key_lands_in_server`;
  - `e2e/web/create.spec.ts` → `empty_instance_start_server_invite_reply_dm_add_channel`;
  - `e2e/web/enrol.spec.ts` tests 1, 2 and 4 → `enrol_by_password_and_recovery_key`, `wrong_recovery_key_refused_lowercase_hyphens_ok`, and `peer_enrolled_device_enters_and_leaves_group`;
  - `e2e/web/admin-keypackages.spec.ts` → `admin_enrols_second_device_finds_bot_commands`;
  - `e2e/web/sidebar-actions.spec.ts` (its owner part) → `owner_adds_channel_with_plus`.
- **Keyboard:** Enter advances each step; Escape backs out of the create dialogs; the recovery key field accepts a paste with hyphens.

### Task 26: Second factor, passkeys, backup codes, recovery sign-in

- **Ports:**
  - web sign-in and factor: `packages/web/src/screens/SignIn.tsx` (entry, password, code, recovery key, passkey, done, plus the race and evicted notices) and `factor/{FactorSetup,ForcedFactor,ForgetBrowser}.tsx`, `doneRefusal.ts`;
  - web settings: `settings/{SignInFactors,PasswordDialog,StepUpDialog}.tsx`;
  - web: `packages/web/src/{webauthn,desktop-auth-browser}.ts`;
  - desktop: `packages/desktop/src/browser-auth.ts`;
  - ui: `packages/ui/src/{CodeBoxes,CodeList,FactorRow}`;
  - client-core: `packages/client-core/src/account/{factors,session}.ts`.
- **Builds:**
  - `features::factors`: TOTP with QR (Task 6 `QrCode`), backup codes, passkeys, password change and step-up;
  - `features::browser_auth`: the system-browser ceremony, the same as `browser-auth.ts`:
    - kinds get, create and oidc;
    - 2 min timeout;
    - SHA-256 verifier;
    - the routes `desktop-auth/request` and `desktop-auth/<id>/consume`;
    - the browser is opened with `open`;
  - the full sign-in order;
  - the forced-factor screen with Forget this device;
  - the Settings → Sign-in section.

  It calls Task 2's `factor_completed`.
- **Parity:** `sign-in/{entry,login,code,recovery-key,done}`, `factor/{app,codes,forced}` and `user-settings/sign-in` plus `-b` fixtures. Sources: `signin-{0..4}-*`, `signal-code-step`, `onboarding-5-factor-{app,codes}`, `settings-signin` and `fullscreen-settings-signin-desktop-signal`.
- **Behaviour:**
  - `e2e/web/second-factor.spec.ts` → `signup_factor_code_backup_code_wrong_code_settings`;
  - `e2e/web/signin-order.spec.ts` → `password_then_factor_then_recovery_key_backup_code_waits`;
  - `e2e/web/required-factor.spec.ts` tests 1, 2, 3, 4 and 6 → `abandoned_factor_choice_meets_forced_screen`, `forced_setup_then_joins_invited_server`, `operator_reset_reaches_live_app`, `forget_this_device_keeps_account_device_listed`, and `after_reset_password_then_setup_not_code`;
  - `e2e/web/passkey.spec.ts` → `passkey_sign_up_sign_in_add_app_remove`. The "system browser" is a Playwright Chromium with a virtual authenticator, given to the app as its browser opener.
- **Keyboard:** the code boxes advance automatically and take a paste of 6 digits; Backspace moves back; Enter submits.

### Task 27: Multi-instance manager

- **Ports:**
  - desktop: `packages/desktop/src/main.ts` (instances), `preferences.ts`, `manager.{html,ts,css}`, and `policy.ts` (`canonicalInstance`, `parseInvite`).
  - Behaviour reference: `packages/desktop/test/electron/{shell,forwarding}.spec.ts`.
- **Builds:**
  - the "Dilla - your instances" window (720×760, minimum width 480): list, connect by URL, connect by pasted invite, remove;
  - one window and one `Client` per instance in one process;
  - a dev flag `--instance=<n>`.
  - Task 6 minors:
    - a cross-process lock on `instances.json`;
    - no resurrection of a removed id from a stale in-memory list;
    - the store lock is held while the instance's window is open.
- **Parity:** `instances/{list,connect-url,connect-invite}` plus `-b` fixtures (12 instances, long URLs). These are shot from the beta.5 Electron manager window through `e2e/playwright.desktop.config.ts`, an extension of `shoot.mjs`.
- **Behaviour:**
  - → `manager_lists_and_connects_by_url_and_invite`;
  - → `thirty_third_instance_refused_in_ui`;
  - → `two_instances_same_host_isolated` (from `forwarding.spec.ts` "isolates storage for two complete URLs");
  - → `removed_instance_not_resurrected`;
  - → `closing_a_window_keeps_client_alive` (from `shell.spec.ts`).
- **Keyboard:** the instance list is roving; Enter opens; Delete removes after a confirmation.

---

## Wave 2

### Task 28: Integration, cross-area tests, parity sweep, docs

**Files:** `clients/desktop/dilla-desktop/tests/integration.rs`, `clients/desktop/BEHAVIOUR-GATES.md`, `README.md` ("what works"), `docs/user/desktop.md`, and `docs/user/screenshots` (re-shot, for the GPUI preview section only).

- [ ] **Step 1:** Cross-area tests:
  - `e2e/web/threads.spec.ts` tests 4 and 5 → `thread_opens_from_search_hit`, and `thread_opens_from_pin_of_reply`;
  - `e2e/web/conversation.spec.ts` "edit, reply, react, pin and delete" as one flow → `edit_reply_react_pin_delete_one_flow`;
  - `e2e/web/core-worker.spec.ts` (two devices fold edits, reactions, replies, pins and deletes, and files go through) → `own_devices_fold_everything`;
  - `e2e/web/reload-mid-sync.spec.ts` → `restart_mid_sync_reads_every_group`;
  - `e2e/web/installed-upgrade-account.spec.ts`, its parts that apply to a fresh native install → `restart_keeps_keystore_history_draft_guard_recovery`.
- [ ] **Step 2:** `BEHAVIOUR-GATES.md`: a table from every test in `e2e/web`, `e2e/voice`, `e2e/media` and `packages/desktop/test/electron` to its Rust test, or to "n/a (browser/phone only)" with the reason, or to "next plan (native)". CI test `behaviour_gate_table_complete` fails on any unmapped e2e title.
- [ ] **Step 3:** Full parity sweep on Linux: every fixture of every area in one run, with the numbers in the report. Then run the 3-OS workflow, which shoots the macOS and Windows references in CI and reports their measured tolerances (see Questions).
- [ ] **Step 4:** Docs: README "what works in the preview", `docs/user/desktop.md` preview section with re-shot screenshots, and a known-gaps list.
- [ ] **Step 5:** Run S7, then push and open the draft PR into `main` once the 3-OS workflow is green.

---

## Next plans (not this plan)

1. **Native integrations:**
   - single-instance lock and forwarding a second launch;
   - `dilla://invite` deep links routed to the matching instance;
   - tray, close-to-tray and badge count (from `features::notify`);
   - autostart;
   - OS notifications behind the `Notifier` trait, including ring Answer and Decline actions;
   - quit guards (active call, unsaved draft);
   - OS permission prompts;
   - the app menu and window menu.
2. **Capture:**
   - the screen-capture picker window, with ScreenCaptureKit, Windows Graphics Capture, the PipeWire portal and X11 behind `CaptureSource`;
   - camera capture;
   - per-application audio capture (`packages/desktop/native/*` behaviour).
3. **Updater:** the signed feed (ed25519, the `update.ts` rules), with a Sparkle-equivalent on macOS.
4. **Packaging:** DMG, the Windows installer, AppImage, deb, rpm and AUR; memory hardening equivalent to R15 (no swap for key material).
5. **Release job:**
   - builds `-p dilla-desktop` alone, with a `cargo tree` check that `dilla-test-frames` is absent (Task 11 deferred);
   - checks the automation symbols;
   - signing.
6. **Electron bridge release:** the only exception to "fixes only". Its updater installs the native app.
7. **Switch:**
   - the switch gate (§7);
   - Jonas runs the IME `MANUAL.md` checklist from foundations Task 4;
   - the 3-OS voice interop in CI;
   - the switch notes ("keep another signed-in device at hand").
8. **After the switch:** screen readers (AccessKit), OS-wide push-to-talk, and removing `packages/desktop` and its Linux sandbox.
9. **Separate track (not desktop):** the Bot release with `MemorySwapMax=0` and the sqlite3mc comments (R15).

## Questions for Jonas

1. **Audio/video attachment playback** (Task 12). Every decoder option needs his call:
   - FFmpeg (LGPL, dynamic) or GStreamer would each need a licence exception;
   - the platform frameworks (AVFoundation, Media Foundation, GStreamer on Linux) mean more code per OS;
   - "open in the system player" for now would be a visible deviation.
2. **macOS/Windows parity:** gate every screen on Linux only in this plan, and measure the macOS and Windows tolerances once in Task 28? Or block each screen task on all three OSes, which is slower?
3. **Concurrency cap:** how many Wave 1 agents may run at once? The plan allows all 20.
