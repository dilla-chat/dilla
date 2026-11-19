# GPUI gaps and CI follow-ups Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the GPUI desktop's remaining Electron-parity gaps (Linux icon, Linux notification withdraw, ring with no
window, Windows and macOS application sound in a share, macOS camera), finish the CI follow-ups from #188, and end in a
signed `desktop-gpui-v0.2.0-alpha.2`.

**Architecture:** Each gap is fixed at its existing seam: the Linux app id in `dilla-desktop` and the Linux packaging;
notification close in the vendored `gpui-pre-linux`; rings move from a per-window effect to a per-instance one;
application sound gets `windows.rs` and `macos.rs` beside `app_audio/linux.rs` behind the same `sources()`/`start()`;
the macOS camera gets an AVFoundation backend behind the vendored `webrtc-sys` `dilla_camera_*` bridge. CI and docs
changes are small edits to existing files. One branch, one PR.

**Tech Stack:** Rust (GPUI, cxx, `windows` crate, objc2 / CoreAudio, AVFoundation via Objective-C++), Node CI scripts,
GitHub Actions.

**Spec:** none written; this plan argues from Jonas's rulings of 2026-11-19 in the session (recorded here):
- "start the follow-up branch and fix those gaps. Also theres no desktop icon?"; the icon is missing in the **taskbar /
  task switcher and the app launcher** (KDE Plasma, Wayland, `dilla-bin 0.2.0alpha1-1`).
- Linux app id: **`chat.dilla.Dilla`**. macOS and Windows keep `dev.dilla.desktop`.
- macOS checks: **Jonas grants permission once on the MacBook**; afterwards the MacBook runs real capture tests headless.
- The branch ends in a **signed alpha.2 release**; Jonas sets or confirms the signing secrets himself.
- Agreed 2026-11-18: the CI follow-ups (sccache tuning, desktop-gpui in `ci-rerun-starved`, the drop-in `ExecStart=`
  warning) ride on this branch.

Root cause of the icon (verified on Jonas's machine): every icon is installed and resolves (`kiconfinder6
dev.dilla.desktop` → hicolor 48x48), KWin reports `desktopFileName=dev.dilla.desktop`, but Plasma strips a trailing
`.desktop` from an app id before looking up the entry, so it looks for `dev.dilla` and draws a generic icon. App ids
ending in `.desktop` are invalid by Flathub/AppStream rules for the same reason.

## Global Constraints

- Linux app id `chat.dilla.Dilla`: the desktop entry `chat.dilla.Dilla.desktop`, `Icon=chat.dilla.Dilla`,
  `StartupWMClass=chat.dilla.Dilla`, icons `chat.dilla.Dilla.png` / `.svg` in every hicolor size that exists today,
  metainfo `chat.dilla.Dilla.metainfo.xml` with `<id>chat.dilla.Dilla</id>`, the Wayland app id / X11 WM_CLASS of every window.
- macOS bundle id and Windows AppUserModelID stay `dev.dilla.desktop`; the keychain service stays
  `dev.dilla.desktop.preview`; the data root keeps its name; the Linux autostart file stays `dilla-desktop.desktop`.
- Application sound: 48 kHz stereo f32, 10 ms frames of `FRAME_SAMPLES` (960) interleaved samples, never Dilla's own
  process tree, `MAX_SOURCES` 256, `MAX_LABEL` 512 — the constants in `dilla-media/src/app_audio/mod.rs`.
- Windows application sound needs Windows 10 2004 (build 19041) or newer; older answers `AppAudioError::Unsupported`.
- macOS application sound needs macOS 14.2 or newer; older answers `AppAudioError::Unsupported`.
- Pixel parity with the Signal design; any UI deviation goes to Jonas first. No UI change is planned here.
- No secret is handled by an agent. Signing secrets are set by Jonas.
- The MacBook is a headless build server: no prompt is answered there by an agent; a step needing a prompt stops and
  reports.
- `npm run ci:local` (or `--only` the touched jobs) passes before every push; `cargo clippy --all-targets --locked -- -D
  warnings` and `cargo fmt --check` for every touched crate.
- User docs (`docs/user/desktop.md`, README "what works") are current at the end.

## Review Focus

1. Upgrade from alpha.1 on Linux: the old `dev.dilla.desktop.desktop` and old icons must disappear with the package
   upgrade (package-owned files), and a `dilla://` link must still open the app afterwards (`MimeType` on the new
   entry; `update-desktop-database` runs via the package hooks). Pinned taskbar launchers pointing at the old entry
   break: say so in the docs' upgrade note.
2. A ring that arrives while the app runs with every window closed (tray only) must play the ring sound and raise one
   notification, and the notification must be withdrawn when the ring ends; answering from it opens the window.
3. Choosing an application that exits mid-share (Windows, macOS) must end the capture once (`ended` called once) and
   not crash or hang the call.
4. A macOS camera the user has denied must surface as the existing "camera unavailable/denied" path, not a hang or a
   black track.
5. On Linux a notification replaced by tag (a second message in the same conversation) must replace, not stack.

---

### Task 1: Linux app id `chat.dilla.Dilla`

**Files:**
- Modify: `clients/desktop/dilla-desktop/src/lib.rs` (add `LINUX_APP_ID`; `main_window_options`, `window_options_for`)
- Modify: `clients/desktop/dilla-desktop/src/windows.rs:414`, `src/screens/attachments/ui.rs:1113` (every `app_id:`)
- Modify: `clients/desktop/dilla-desktop/src/native/notifier.rs:271` (Linux identity)
- Rename: `clients/desktop/packaging/linux/dev.dilla.desktop.desktop` → `chat.dilla.Dilla.desktop`;
  `dev.dilla.desktop.metainfo.xml` → `chat.dilla.Dilla.metainfo.xml`; every
  `packaging/linux/icons/hicolor/*/apps/dev.dilla.desktop.*` → `chat.dilla.Dilla.*`
- Modify: `clients/desktop/packaging/linux/nfpm.yaml`, `scripts/desktop-gpui-package-linux.sh`,
  `scripts/desktop-gpui-aur.mjs`, `scripts/desktop-gpui-smoke-linux.sh`, `.github/workflows/desktop-gpui*.yml` (Linux
  file names only), `clients/desktop/packaging/README.md`
- Test: `scripts/desktop-gpui-package-linux.test.mjs`, `scripts/desktop-gpui-aur.test.mjs`, a unit test in `lib.rs`

**Interfaces:**
- Produces: `pub const LINUX_APP_ID: &str = "chat.dilla.Dilla";` and `pub fn window_app_id() -> &'static str`
  (`LINUX_APP_ID` on Linux, `APP_ID` elsewhere). `APP_ID` keeps meaning the macOS bundle id / Windows AUMID.

- [ ] **Step 1: Failing tests.** In `lib.rs` tests: `linux_app_id_is_not_a_desktop_name` asserting
  `!LINUX_APP_ID.ends_with(".desktop")` and that `LINUX_APP_ID == "chat.dilla.Dilla"`; `every_window_uses_window_app_id`
  asserting `main_window_options(..).app_id.as_deref() == Some(window_app_id())` and the same for `manager_options`.
  In the two `.test.mjs` files: the staged tree / AUR `package()` installs
  `usr/share/applications/chat.dilla.Dilla.desktop`, `usr/share/metainfo/chat.dilla.Dilla.metainfo.xml`,
  `usr/share/icons/hicolor/{16..512,scalable}/apps/chat.dilla.Dilla.{png,svg}`, and **no** path containing
  `dev.dilla.desktop`; the desktop entry has `Icon=chat.dilla.Dilla` and `StartupWMClass=chat.dilla.Dilla` and keeps
  `MimeType=x-scheme-handler/dilla;`.
- [ ] **Step 2:** Run `cargo test -p dilla-desktop --lib linux_app_id` and `node --test scripts/desktop-gpui-package-linux.test.mjs scripts/desktop-gpui-aur.test.mjs`; expect FAIL.
- [ ] **Step 3: Implement.** `git mv` the files; update every Linux reference; every `WindowOptions.app_id` takes
  `window_app_id()`; on Linux `set_app_identity(LINUX_APP_ID, PRODUCT_NAME)` so notifications carry the
  `desktop-entry` hint of the new entry. The AppImage keeps its `.DirIcon` and top-level icon under the new name. Do
  not touch `apply_mac.rs`, `Info.plist.in`, `dilla.iss`, `keychain.rs`, `autostart.rs`.
- [ ] **Step 4:** Re-run Step 2; expect PASS. Run `bash scripts/desktop-gpui-package-linux.sh` per its usage on a
  local release build if feasible, else the test suite is the gate.
- [ ] **Step 5: Commit** `GPUI Linux: app id chat.dilla.Dilla (KDE strips a trailing .desktop, so no icon)`.

### Task 2: Linux notifications close and replace by tag

**Files:**
- Modify: `core/vendor/gpui-pre-linux/src/linux/system_notifications.rs`
- Modify: `core/vendor/README.md` (the patch list) and `clients/desktop/dilla-desktop/tests/vendored_patches.rs` if it pins vendor patches
- Test: a unit test module in `system_notifications.rs`; an integration test there behind a live session bus

**Interfaces:**
- Consumes: nothing new. `notifier.rs` keeps calling `cx.dismiss_system_notification(&tag)`.
- Produces: `struct LiveIds` (tag → server id, `Arc<Mutex<HashMap<SharedString, u32>>>`) with
  `fn shown(&self, tag, id) -> Option<u32>` (returns the replaced id), `fn take(&self, tag) -> Option<u32>`,
  `fn closed(&self, tag, id)` (removes only if the stored id still equals `id`).

- [ ] **Step 1: Failing tests.** `live_ids_replace_returns_the_old_id`, `take_removes`, `closed_ignores_a_newer_id`
  (a `closed(tag, 1)` after `shown(tag, 2)` keeps 2). Integration test `dismiss_sends_close_notification`, skipped
  unless `DILLA_PRIVATE_BUS=1` (set only under `dbus-run-session`, never on a desktop session whose real notification
  server owns the name): a zbus mock `org.freedesktop.Notifications` on the session bus returns id
  41 from `Notify`, the test shows a notification with tag `t`, dismisses `t`, and asserts the mock received
  `CloseNotification(41)`; showing tag `t` twice asserts the second `Notify` carried `replaces_id == 41`.
- [ ] **Step 2:** `cargo test -p gpui-pre-linux system_notifications` (and under `dbus-run-session -- cargo test ...`); FAIL.
- [ ] **Step 3: Implement.** In `show`: if `LiveIds` holds the tag, pass that id via `notify_rust::Notification::id`
  so the server replaces it; after `built.show()` record `handle.id()` before `wait_for_action`; on `__closed` or an
  action call `closed(tag, id)`. `dismiss(tag)`: `take(tag)` and send `CloseNotification(id)` over the session bus
  (zbus, already a dependency of notify-rust) off the UI thread. Replace the no-op comment.
- [ ] **Step 4:** Re-run Step 2 plain and under `dbus-run-session`; PASS. Make the Linux client CI job run this test
  under `dbus-run-session` if the job has no session bus.
- [ ] **Step 5: Commit** `gpui-pre-linux: close and replace notifications by tag (XDG CloseNotification)`.

### Task 3: Rings without an open window

**Files:**
- Modify: `clients/desktop/dilla-desktop/src/screens/ring/effects.rs` (`sync`, `ring_notification`, `close_ring_notification`)
- Modify: `clients/desktop/dilla-desktop/src/native/notifier.rs` (an `Owner::Instance` ring owner beside `Owner::Window`; `withdraw_rings_of`)
- Modify: where instances are started (the per-instance client wiring that `notifier::attach` uses today)
- Test: `clients/desktop/dilla-desktop/tests/ring.rs`, `tests/notify.rs`

**Interfaces:**
- Produces: `pub fn sync_instance(instance: InstanceId, rings: &[IncomingRing], any_window_active: bool, cx: &mut App)`
  in `effects.rs`, called from an app-level observer of each running instance's `RingState` (not from a frame). The
  per-window `sync` keeps only what needs a window (the banner); sound and notifications come from `sync_instance`.

- [ ] **Step 1: Failing tests** in `tests/ring.rs`: `a_ring_with_no_window_open_sounds_and_notifies` (instance
  running, all windows closed, inject an `IncomingRing` → ring sound started, one ring notification raised with
  Answer/Decline); `the_ring_notification_is_withdrawn_when_the_ring_ends` (ring removed → notification withdrawn,
  sound stopped); `answering_from_the_notification_opens_the_window_and_joins`; and the existing focused-window case
  still raises none.
- [ ] **Step 2:** `cargo test -p dilla-desktop --test ring`; FAIL on the new tests.
- [ ] **Step 3: Implement** the move: one ring sound for the app (start with the first audible ring of any instance,
  stop with the last), one notification per ring key per instance, closed when the ring ends or any window of that
  instance becomes active.
- [ ] **Step 4:** `cargo test -p dilla-desktop --test ring --test notify`; PASS.
- [ ] **Step 5: Commit** `GPUI rings: sound and notify with no window open`.

### Task 3b: Connect on a hidden start (added 2026-11-19, Jonas: "Yes, connect on hidden start")

A `--hidden` start (autostart at login) returns before any window opens (`dilla-desktop/src/main.rs:~102`); clients
start only in a window's `ShellApp::start_client` (`app.rs:~773`, `Client::start` ~:829) and `windows::keep_client` is
reached only after that, so a tray-only start connects to nothing: no rings, no message notifications. beta.5's hidden
window kept its renderer running, so it connected. Parity: a hidden start connects every listed instance.

**Files:**
- Modify: `clients/desktop/dilla-desktop/src/main.rs` (hidden start), `src/app.rs` (factor the client start out of the
  window so it can run headless), `src/windows.rs` (`keep_client` for headless clients; a window opened later adopts
  the running client instead of starting a second), `src/native/notifier.rs` (message notifications start with the
  client, not on the first window frame), `src/screens/ring/effects.rs`, `src/screens/ring/mod.rs`, `src/native/tray.rs`
- Test: `tests/ring.rs`, `tests/notify.rs`, a new `tests/hidden_start.rs` if no existing file fits

**Interfaces:**
- Produces: `pub fn start_headless_clients(cx: &mut App)` — for every listed instance whose database key the keychain
  answers without a prompt, starts its client and calls `keep_client`; an instance whose key is unavailable (locked
  keychain, Secret Service absent) is skipped with a log line and starts when its window opens, as today.
  `tray::show_instance` returns `bool` (whether it opened or focused a window).

- [ ] **Step 1: Failing tests:** `a_hidden_start_connects_and_rings` (hidden start, an instance with a stored key →
  its client runs, an injected ring sounds and notifies with no window); `a_hidden_start_raises_message_notifications`;
  `opening_the_window_after_a_hidden_start_reuses_the_client` (exactly one client per instance);
  `a_locked_key_skips_the_instance_without_a_prompt`; `a_failed_window_open_clears_the_pending_answer`
  (review Minor 2: `show_instance` → false clears `await_window`); `a_dropped_state_sender_removes_its_watch`
  (review Minor 1).
- [ ] **Step 2:** `cargo test -p dilla-desktop --test ring --test notify --test hidden_start`; FAIL.
- [ ] **Step 3: Implement.** No visible UI change; no keychain prompt at login (use the keychain's non-interactive
  lookup; if the platform API cannot say "would prompt", skip headless start on that platform and say so).
  Rename `answering_from_the_notification_opens_the_window_and_joins` to what it checks (review Minor 3).
- [ ] **Step 4:** Step 2 PASS; `cargo test -p dilla-desktop --lib`; fmt; clippy.
- [ ] **Step 5: Commit** `GPUI: a hidden start connects, so rings and messages notify from the tray`.

### Task 4: Windows application sound (WASAPI process loopback)

**Files:**
- Create: `clients/desktop/dilla-media/src/app_audio/windows.rs`
- Modify: `clients/desktop/dilla-media/src/app_audio/mod.rs` (cfg branches, module doc), `dilla-media/Cargo.toml`
  (`windows` features under `[target.'cfg(windows)'.dependencies]`)
- Test: unit tests in `windows.rs`; `dilla-media/tests/app_audio.rs` (a Windows-only `#[ignore]` hardware test)

**Interfaces:**
- Produces: `pub(super) fn sources(own_pid: u32) -> Result<Vec<AppSource>, AppAudioError>` and
  `pub(super) fn start(source: &AppSource, own_pid: u32, frames: Box<dyn FnMut(Vec<f32>) + Send>, ended: Box<dyn FnOnce() + Send>) -> Result<AppCapture, AppAudioError>`
  — the same shape as `linux.rs`. `AppSource.serial` is the session's index in that enumeration (only `pid` is used to capture).

- [ ] **Step 1: Failing unit tests** (pure, run on every OS by keeping the helpers outside `cfg(windows)`):
  `frames_are_cut_to_960_samples` (an arbitrary-length f32 stream yields exact 960-sample frames, the rest carried),
  `non_finite_samples_end_the_capture`, `own_process_tree_is_excluded` (given a pid→parent map and sessions, Dilla's
  tree is dropped), `labels_are_capped_at_512`.
- [ ] **Step 2:** `cargo test -p dilla-media --lib app_audio`; FAIL.
- [ ] **Step 3: Implement.** Sources: `IMMDeviceEnumerator` default render endpoint → `IAudioSessionManager2` →
  sessions in `AudioSessionStateActive`, their pid and the process's product name (fallback: exe stem). Capture:
  `ActivateAudioInterfaceAsync(VIRTUAL_AUDIO_DEVICE_PROCESS_LOOPBACK, …)` with
  `AUDIOCLIENT_ACTIVATION_TYPE_PROCESS_LOOPBACK`, `PROCESS_LOOPBACK_MODE_INCLUDE_TARGET_PROCESS_TREE`, format
  48 kHz stereo `WAVE_FORMAT_IEEE_FLOAT`, event-driven capture thread; `ended` once when the process exits
  (`WaitForSingleObject` on its handle) or the client errors. Below build 19041 → `Unsupported`.
- [ ] **Step 4:** Unit tests PASS on Linux; `cargo clippy --target x86_64-pc-windows-msvc` is covered by the
  `gpui-win-x64` CI job — push only after Task 7's ci:local. The `#[ignore]` hardware test
  `captures_a_tone_from_a_child_process` (spawns a helper playing 440 Hz, asserts a peak within 20 Hz) is documented
  for a manual run on a Windows machine with an audio endpoint; the PR says it is unverified on hardware unless run.
- [ ] **Step 5: Commit** `dilla-media: Windows application sound (WASAPI process loopback)`.

### Task 5: macOS application sound (CoreAudio process taps)

**Files:**
- Create: `clients/desktop/dilla-media/src/app_audio/macos.rs`
- Modify: `app_audio/mod.rs`, `dilla-media/Cargo.toml` (objc2 CoreAudio/AppKit crates, macOS-only)
- Modify: `clients/desktop/packaging/macos/Info.plist.in` (add `NSAudioCaptureUsageDescription`, copy: "Dilla shares the
  sound of the application you choose when you share your screen.")
- Test: shared pure tests from Task 4; `dilla-media/tests/app_audio.rs` macOS hardware test

**Interfaces:**
- Consumes: the pure frame/exclusion helpers Task 4 put outside `cfg(windows)` (move them to `app_audio/frames.rs` if
  Task 4 left them in `windows.rs`).
- Produces: `macos::sources` / `macos::start` with the same signatures as Task 4.

- [ ] **Step 1: Failing test** `captures_a_tone_from_afplay` (macOS only, runs when `DILLA_MAC_CAPTURE=1`): spawns
  `afplay` on a generated 440 Hz WAV, finds it in `sources()` by pid, captures 2 s, asserts a peak within 20 Hz; and
  `ended_fires_once_when_the_app_exits`.
- [ ] **Step 2:** On the MacBook (headless, via the dilla-MacBook session, after the one-time grant in Task 6 Step 0):
  `DILLA_MAC_CAPTURE=1 cargo test -p dilla-media --test app_audio`; FAIL.
- [ ] **Step 3: Implement.** Sources: `kAudioHardwarePropertyProcessObjectList`, keep processes with
  `kAudioProcessPropertyIsRunningOutput`, pid via `kAudioProcessPropertyPID`, label via `NSRunningApplication`
  `localizedName`. Capture: `CATapDescription` (stereo mixdown of the chosen process objects) →
  `AudioHardwareCreateProcessTap` → private aggregate device with the tap → IOProc, converting to 48 kHz stereo f32
  with `AudioConverter` when the device rate differs. Tear down tap and aggregate on drop. Below 14.2 → `Unsupported`.
- [ ] **Step 4:** Re-run Step 2 on the MacBook; PASS. `gpui-mac-arm64` CI builds and runs the pure tests.
- [ ] **Step 5: Commit** `dilla-media: macOS application sound (CoreAudio process taps)`.

### Task 6: macOS camera (AVFoundation)

**Files:**
- Create: `core/vendor/webrtc-sys/src/dilla_camera_mac.mm`
- Modify: `core/vendor/webrtc-sys/build.rs:69-126` (macOS builds `dilla_camera.rs` with the `.mm`, `-fobjc-arc`,
  frameworks AVFoundation, CoreMedia, CoreVideo), `core/vendor/README.md` (patch list)
- Modify: `clients/desktop/dilla-desktop/src/native/permissions.rs` (macOS camera authorisation, if not already handled)
- Modify: `clients/desktop/dilla-media/src/sources/camera.rs` (module doc: macOS AVFoundation)
- Test: `dilla-media/tests/capture.rs` macOS camera test behind `DILLA_MAC_CAPTURE=1`

**Interfaces:**
- Consumes: the existing cxx bridge in `webrtc-sys/src/dilla_camera.rs` and `include/livekit/dilla_camera.h` —
  `dilla_camera_devices() -> Vec<DillaCameraDevice>`, `dilla_camera_open(unique_id, …) -> UniquePtr<DillaCamera>`,
  `start`. The macOS `.mm` implements the same functions; the Rust side does not change.

- [ ] **Step 0 (Jonas, once):** on the MacBook, System Settings → Privacy & Security: grant **Camera** and **Screen &
  System Audio Recording** to the terminal app the dilla-MacBook session runs in. Child test processes inherit it, so
  rebuilds keep the grant. The controller asks Jonas for this and waits; no agent answers a prompt.
- [ ] **Step 1: Failing test** `mac_camera_delivers_frames` (`DILLA_MAC_CAPTURE=1`): `devices()` is non-empty,
  `open(first)` delivers ≥ 10 I420 frames of the requested size within 5 s; `denied_camera_is_an_error_not_a_hang`
  (unit-level: authorisation `Denied` maps to `OpenError::Unavailable` immediately).
- [ ] **Step 2:** On the MacBook: `DILLA_MAC_CAPTURE=1 cargo test -p dilla-media --test capture mac_camera`; FAIL.
- [ ] **Step 3: Implement.** Devices: `AVCaptureDeviceDiscoverySession` (built-in, external, Continuity cameras).
  Open: `AVCaptureSession` + `AVCaptureVideoDataOutput` (NV12), nearest format to the request, frames converted with
  libyuv `NV12ToI420` into `webrtc::I420Buffer` and handed to the same sink the Linux/Windows path uses. Authorisation:
  `authorizationStatusForMediaType:` — `NotDetermined` requests access once (that prompt is the user's, in the app),
  `Denied`/`Restricted` → error.
- [ ] **Step 4:** Re-run Step 2; PASS. `gpui-mac-arm64` CI builds it.
- [ ] **Step 5: Commit** `webrtc-sys: macOS camera through AVFoundation`.

### Task 7: CI follow-ups

**Files:**
- Modify: `clients/desktop/dilla-client/tests/calls.rs:1588-1596` (rejoin diagnostics)
- Modify: `.github/workflows/ci-rerun-starved.yml` (add `desktop-gpui` to the watched workflows)
- Modify: `.github/workflows/desktop-gpui.yml:62` (cron `'17 3 * * *'`; the comment says why: top-of-hour schedules are dropped)
- Modify: `.github/workflows/desktop-gpui.yml` sccache env (Windows), per Step 3's finding
- Modify: `docs/deploy/README.md` ~455-481 (drop-in warning)

- [ ] **Step 1: Rejoin diagnostics.** In `rejoin_waits_for_pending_remove`, when `phase.wait_for` returns `Err`
  (the join task ended), await `rejoin.outcome()` and panic with it:
  `"the rejoin ended before it waited for the Remove: {outcome:?}"`. Run the test 3× locally; PASS.
- [ ] **Step 2: ci-rerun-starved + cron.** Add `desktop-gpui` to the `workflow_run.workflows` list and to any name
  filter in the job; change the cron; `node --test scripts/*.test.mjs` for any script reading the list.
- [ ] **Step 3: sccache.** From run 44367708742 (PR) and the nightly 44469529895, read the Windows and Linux
  `sccache --show-stats` (hits, misses by reason, "Cache write errors 11" on Linux). Fix the cause you find (e.g.
  non-cacheable reasons from absolute paths or `CARGO_INCREMENTAL`, GHA cache rate-limit write errors →
  `SCCACHE_GHA_ENABLED` with fewer writers). Rule: if after the fix the second-run Windows hit rate is still below
  40 %, turn sccache off on Windows (it costs setup time for nothing) and record the numbers in the commit message.
- [ ] **Step 4: Docs.** In `docs/deploy/README.md` beside the drop-in recipe: a second `ExecStart=` in a drop-in
  replaces the whole command line, so repeat `--desktop-downloads` next to `--desktop-downloads-channel`; the
  recipe's `ExecStart=` must be preceded by an empty `ExecStart=`.
- [ ] **Step 5:** `npm run ci:local -- --only <touched jobs> --no-deps`; PASS. Commit
  `CI: rejoin diagnostics, desktop-gpui starved reruns, nightly at :17, sccache on Windows, drop-in docs`.

### Task 8: User docs

**Files:**
- Modify: `docs/user/desktop.md` ("Known gaps" → "Platform notes": only what still differs; the Linux id change in
  "Upgrading"; macOS 14.2 / Windows 10 2004 floors for application sound; macOS camera), `README.md` "what works"
- Test: `npm run ci:local -- --only docs --no-deps` (or the docs link/lint job ci:local has)

- [ ] **Step 1:** Rewrite "Known gaps": what works moves to the feature sections; what remains is listed with its
  platform. Upgrade note: alpha.2 renames the Linux entry to `chat.dilla.Dilla`; re-pin a pinned launcher.
- [ ] **Step 2:** Docs lint PASS. Commit `docs: GPUI desktop platform notes after the gap fixes`.

### Task 9 (controller, after merge): signed alpha.2

Not a subagent task. After Jonas's merge go-ahead and merge:
1. Bump the desktop crate version to `0.2.0-alpha.2` per `clients/desktop/RELEASING.md` on a release branch/PR as
   RELEASING says.
2. Ask Jonas (AskUserQuestion) to set or confirm `DESKTOP_METADATA_KEY_A` and the macOS secrets (`CSC_LINK`,
   `CSC_KEY_PASSWORD`, `APPLE_API_KEY`, `APPLE_API_KEY_ID`, `APPLE_API_ISSUER`) in the `desktop-release` environment
   himself with `!`.
3. Tag `desktop-gpui-v0.2.0-alpha.2`; follow `desktop-gpui-release.yml` → `desktop-gpui-sign.yml`; check the `feed`
   job published and `scripts/desktop-gpui-feed.mjs verify` passes.
4. Tell the Homelab session to switch CT 144's downloads to alpha.2 (Jonas's own go there).
