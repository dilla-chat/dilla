# GPUI desktop — feature parity with Electron beta.5 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The native GPUI client (`clients/desktop`) does everything the Electron app did at `desktop-v0.1.0-beta.5`:
its main process (`packages/desktop/src`), the React UI it rendered (`packages/web/src`), and the platform services
Chromium gave it for free (screen readers, inline video, spell-check). Only what Electron itself never had is left out.
Jonas, 2026-11-15: "finish the entire migration to GPUI — we need the same feature set".

**Architecture:**
- One new module tree for the OS integrations, `clients/desktop/dilla-desktop/src/native/`. Task 0 lays it down so
  the tray, notifications, deep links and quit guards each own one file and never edit `main.rs`, `app.rs` or
  `windows.rs` at the same time:
  - `native/mod.rs`: `NativeShell` (a GPUI global), with registries the other packages fill in: tray menu sections,
    badge sinks, quit guards, link handlers.
  - `native/{tray,badge,autostart,notifier,links,single_instance,quit,menus,permissions}.rs`: one file per package.
- Capture lives in `dilla-media` behind the existing `CaptureSource` trait (`dilla-media/src/capture_source.rs`;
  today only `TestPattern`). The share picker is a new screen, `dilla-desktop/src/screens/share_picker/`.
- Application audio is a new `dilla-media/src/app_audio/{mod,linux,mac,windows}.rs`. It ports beta.5's
  `packages/desktop/native/{linux.cpp,AudioCapture.swift,windows.cpp}` to Rust, in-process (no helper binary).
- Accessibility uses what the pinned toolkit already ships. `gpui-pre 0.3.12` (`core/vendor/gpui-pre`) depends on
  `accesskit 0.24.1`, with the AT-SPI (`accesskit_unix`, pulled in by the `wayland`/`x11` features dilla already
  enables), macOS and Windows adapters. Elements opt in with `div().id(..).role(Role::..)` and `.aria_label(..)`
  (`elements/div.rs:1357,1379`; guide in `src/_accessibility.rs`). `gpui-component 0.9.1` sets roles in 25 files.
  dilla sets none today.

**What the pinned GPUI already provides** (checked in `core/vendor/gpui-pre` and `~/.cargo/registry/.../gpui-pre-{linux,macos,windows}-0.3.12`):

| Need | GPUI 0.3.12 | Gap to fill |
|---|---|---|
| OS notifications with action buttons | `App::show_system_notification`, `dismiss_system_notification`, `on_system_notification_response`. Linux uses `notify-rust` (XDG), with macOS and Windows implementations | Windows toasts need an AUMID shortcut (installer); macOS needs the bundle |
| Deep links | `App::on_open_urls`; `register_url_scheme` on macOS and Windows; on Linux it returns "unimplemented" | Linux: `.desktop` `MimeType=x-scheme-handler/dilla` from packaging, plus argv |
| App menu, dock menu, reopen | `App::set_menus`, `set_dock_menu`, `on_reopen` | Not called by dilla today |
| Screen capture | Optional `screen-capture` feature: `zed-scap` on Linux/Windows, ScreenCaptureKit on macOS. Already in `Cargo.lock`, **not enabled** | Window sources, the Wayland portal and per-source thumbnails need checking; camera has nothing |
| Screen readers | AccessKit adapters on all three OSes | Roles and names on every dilla element |
| Tray, badge, autostart, single instance | **none** | New code (crates below) |

**Tech Stack:** Rust 1.98.1, `gpui-pre =0.3.12`, `gpui-component =0.9.1`, `livekit =0.10.1` (libwebrtc), `ashpd 0.13`
(already a dependency: ScreenCast portal), `zbus 5.19` (in the lock: Unity LauncherEntry badge, SNI tray), `notify-rust
4.19` (in the lock, through gpui-pre-linux). New crates need `deny.toml` review: `ksni` (Linux tray, pure D-Bus) or
`tray-icon` + `muda` (macOS, Windows), `pipewire` (Rust bindings, Linux app audio and the portal stream), `windows`
(WASAPI process loopback, Windows Graphics Capture, Run key), `objc2-*` (ScreenCaptureKit, CoreAudio process tap,
dock badge, SMAppService), and one camera crate (`nokhwa`, or AVFoundation / Media Foundation / V4L2 directly).

**Spec:** `docs/superpowers/specs/2026-11-12-gpui-desktop-design.md`. The "Next plans" items 1, 2 and 8 of
`docs/superpowers/plans/2026-11-13-gpui-desktop-screens-wave1.md:879` are this plan. Items 3 to 6 (updater, packaging,
release job, bridge) are **`gpui-release-plan.md`** (scratchpad, Decisions D1–D15). It is referenced here, not redone.

**Sources of truth for the gaps:**
- (a) `docs/user/desktop.md` "Known gaps".
- (b) `clients/desktop/BEHAVIOUR-GATES.md` (22 rows "next plan (native)", listed per package below).
- (c) beta.5 `packages/desktop/src/{main,contract,preload,screen-capture,native-audio,autostart,quit-install,preferences,policy}.ts`.
- (d) beta.5 `packages/web/src` against `dilla-desktop/src/screens`: a string-key diff (web `t('…')` keys with no GPUI
  use: 359, most of them aria labels or phone-only) followed by checking each feature by hand.
- (e) AccessKit, above.
- (f) The no-speaker join (`dilla-media/src/room.rs:576` `Speaker::open_with(..)?`).

---

## Global Constraints

- **Pixel parity with the Signal design** (Claude Design handoff; the web client's Signal screens are the reference):
  - every new surface is drawn as the web/Electron one was: the share picker (beta.5 `screen-picker-page.tsx` +
    `callStage.share.*`), the stage tile menu, the tray-adjacent dialogs, the quit dialog;
  - each gets parity fixtures in `parity/fixtures/<area>/` and Linux references, as in wave 1.
- **Ask before any design deviation.** If a surface has no Signal reference (native OS dialogs, the tray menu, a
  notification's layout, a new notice such as "no audio output"), stop and ask Jonas with an AskUserQuestion prompt
  before inventing copy or layout. Copy comes from `packages/web/src/strings/` via `scripts/gen-gpui-strings.mjs`.
  Never hand-edit `strings.rs`.
- **Security boundaries stay as in beta.5:**
  - capture only from a user gesture on a focused window;
  - a capture belongs to one instance's window and ends with it;
  - an unbound or unencrypted sender sends nothing (fail closed);
  - Dilla's own call audio is never in an application-audio capture;
  - notifications carry no message text when the device setting says so;
  - links never carry credentials, and an unknown instance is confirmed before it is added.
- **E2EE content never leaves the process** except to the OS notification (title/body, as beta.5 did) and the clipboard
  from a focused action.
- **Ownership:** each package owns the files listed on its card plus new files under its directories. Shared files
  (`main.rs`, `app.rs`, `windows.rs`, `lib.rs`, `Cargo.toml`, `BEHAVIOUR-GATES.md`, `deny.toml`, `desktop-gpui.yml`)
  are edited only by Task 0 or by the controller at merge.
- **Behaviour gates:** every ported beta.5 title moves its `BEHAVIOUR-GATES.md` row from `next plan (native)` to a real
  Rust test name. `scripts/behaviour-gates.test.mjs` checks that the name exists.
- **Process:**
  - `npm run ci:local` before every push;
  - fewer PRs: one PR per wave below;
  - every wave's closing task updates README "what works", `docs/user/desktop.md` "Known gaps" and the screenshots;
  - opus for capture, app audio, notifications and the a11y tree reviews (security and E2EE boundaries); sonnet for
    recipe tasks.
- **CI:** Linux runs everything headless. New OS services (D-Bus notifications, SNI, the portal) run against a session
  bus in CI (`dbus-run-session`) or through recording fakes behind the `native/` traits. macOS and Windows code compiles
  in the existing matrix and is proven by a manual checklist where CI cannot reach the OS (as foundations Task 4's
  `MANUAL.md`).

## Review Focus

1. **Capture boundary:**
   - no capture without a press on a focused window;
   - cancel publishes nothing;
   - a closed source ends the track;
   - quit or window close stops every capture;
   - another instance's window cannot stop or reconfigure a capture;
   - AV1 (no prefix rule) sends 0 bytes;
   - VP8 is negotiated even when the SFU offers AV1.
2. **Application audio:**
   - only the chosen process tree is captured, never a sink or system monitor;
   - Dilla's own process is excluded;
   - no plaintext fallback.
3. **Notifications:**
   - the device switch and OS permission are honoured;
   - ring Answer and Decline act only on the ring that is still current;
   - closing a window withdraws that instance's notifications;
   - at most 64 live.
4. **Deep links:**
   - the grammar is exactly `dilla://invite/<host[:port]>/<code>` (beta.5 `policy.ts` `parseInvite`);
   - an unknown instance is confirmed;
   - no link opens anything during quit;
   - the second-launch IPC accepts only argv-shaped messages from the same user (socket mode 0600 in the data root,
     or a per-user named pipe).
5. **Quit:**
   - a window that does not answer within 3 s counts as "in use";
   - the dialogs come in beta.5's order;
   - an update restart re-collects the guards.
6. **Accessibility:**
   - every focusable element has a role and a name;
   - ids are stable across frames (the `text!` footgun in `_accessibility.rs`);
   - nothing secret goes into a name (recovery key, TOTP secret: the web hid these from axe too).

---

## Work packages

Gap counts are in brackets. Sizes: S ≤ 2 days of one agent, M ≤ 1 week, L > 1 week.

| # | Package | Gaps | Size | Depends on | Risk |
|---|---|---|---|---|---|
| 0 | Native shell skeleton | — | S | — | low |
| 1 | Capture sources + video send (dilla-media) | 7 | L | 0 | **high** |
| 2 | Share picker + share UI | 7 | M | 1 | med |
| 3 | Application audio | 5 | L | 1 (send path), 2 (the picker's checkbox) | **high** |
| 4 | OS notifications | 6 | M | 0; Windows toasts need release-plan Task 13 | med |
| 5 | Tray, background, autostart, badge, menus, zoom | 7 | M | 0; menu entries from 3, 4, 7 plug in later | med |
| 6 | Single instance + `dilla://` deep links | 6 | M | 0; release-plan Tasks 2, 12, 13 register the scheme | med |
| 7 | Quit guards + permission prompts | 6 | M | 0; update restart needs release-plan Task 14 | med |
| 8 | Voice gaps (no-speaker join, stage menu, volumes, stage views) | 13 | M | — | low–med |
| 9 | PTT AltGr on Linux | 1 | S | — | low |
| 10 | Screen gaps (messages, create, Bot, boot, docs) | 13 | M (4 S lanes) | — | low |
| 11 | Accessibility (AccessKit) | 5 | L | all screen work merged first (or rebased per area) | **high** |
| 12 | Inline video playback | 1 | L | — | med (decision D4) |
| 13 | Spell-check | 1 | M | — | low (decision D5) |
| — | Packaging, updater, release (`gpui-release-plan.md`) | see that plan | — | — | — |

**Total: 78 gaps** (packages 1–13), plus the release plan.

**Excluded because Electron never had them:**
- operating-system-wide push to talk: beta.5 registered no `globalShortcut` and `ptt.ts` is window-focused. It stays
  "after the switch" as a new feature;
- the Linux chrome-sandbox launcher (`native/linux-sandbox/*`), the renderer, IPC and CSP guards, the protected reload,
  HTML fullscreen of the document, and the startup flash. These rows are already n/a in the gates.

**Critical path:** Task 0 → Package 1 (L) → Package 2 (M) → Package 3 (L). Package 11 (L) runs beside it. Everything
else fits inside that span in parallel lanes. The release plan's M2 (updater, Mac, Windows installer) is a second
critical chain: Packages 4 (Windows toasts), 6 (scheme registration) and 7 (restart-to-update) close only after its
Tasks 12–14.

### Waves (one PR each)

- **Wave A** (parallel after Task 0): Packages 1, 4, 5, 6, 8, 9, 10 (four lanes), and 11's component layer (11.1).
- **Wave B:** Packages 2 and 7, and Package 11's screen sweep (11.2–11.4).
- **Wave C:** Package 3, Package 12 (if D4 says "before the switch"), closing docs, and the gates table with no
  `next plan (native)` row left.

---

### Task 0 — Native shell skeleton  *(controller, S)*

**Files:**
- new `dilla-desktop/src/native/mod.rs`;
- `lib.rs` (`pub mod native`), `main.rs` (installs `NativeShell` before the first window), `app.rs` (the quit hook calls
  `NativeShell::request_quit`), `windows.rs` (window close and open events to the shell);
- empty stubs for `native/{tray,badge,autostart,notifier,links,single_instance,quit,menus,permissions}.rs`;
- `OWNERSHIP.md` gets a "Parity waves" section with the rows below.

**Interfaces:**
- `NativeShell::{add_tray_section, set_badge(instance, n), add_quit_guard(instance, Box<dyn Fn(&App) -> GuardState>),
  on_link(Box<dyn Fn(Link, &mut App)>), request_quit(&mut App)}`;
- `GuardState { active_call: bool, unsaved_draft: bool, answered: bool }`.

**Steps:**
- [ ] `cargo test -p dilla-desktop native::` (the registry order, and quit with no guards quits).
- [ ] `npm run ci:local`.

---

### Package 1 — Capture sources and the video send path  *(L, opus, Wave A)*

**beta.5 behaviour** (`packages/desktop/src/screen-capture.ts`, `screen-capture-broker.ts`, `screen-capture-contract.ts`,
`main.ts` `dilla:screen-capture`; web `call/desktop-screen.ts`, `call/devices.ts`):
- screens and windows through `desktopCapturer` (macOS ScreenCaptureKit, Windows, X11) or the Wayland portal;
- options `{width, height, frameRate}` that can be changed live (`configure-screen-capture`);
- the track ends when the source closes or the owner navigates or quits;
- the camera comes through `getUserMedia`;
- video goes out VP8 and encrypted; a codec with no prefix rule (AV1) sends nothing.

**Gaps (7):**
1. screen source;
2. window source;
3. Wayland portal path;
4. camera source;
5. video send, VP8 negotiated even when AV1 is offered;
6. AV1 fails closed;
7. lifecycle: source closed ends the track, `stop_all` on quit and window close, ownership per instance.

**Files:**
- `dilla-media/src/capture_source.rs` (real sources beside `TestPattern`);
- new `dilla-media/src/capture/{screen_linux.rs,screen_mac.rs,screen_win.rs,camera.rs}`;
- `dilla-media/src/room.rs` (video publish, codec policy);
- `dilla-media/Cargo.toml` (the `gpui-pre` `screen-capture` feature, or `ashpd` ScreenCast + `pipewire`; camera crate);
- `dilla-desktop/src/screens/voice/session.rs` (`ShareSourceFactory` at `:187`: real sources instead of `None`, the
  "treated as cancelled" path).

**Feasibility first (step 1):**
- check whether `zed-scap` (through gpui's `screen-capture`) gives window sources and Wayland. If not, use `ashpd`
  ScreenCast + PipeWire on Linux (beta.5's Wayland path), WGC through `windows` and SCK through `objc2` directly;
- write the finding into the PR before building.

**Tests (port these gate rows):**

| beta.5 title | New Rust test |
|---|---|
| `screen-capture.spec.ts` "a selected native application window stays video-only and encrypted through the real media worker" | `dilla-media::tests/capture::a_window_source_is_video_only_and_encrypted` |
| … "an unbound native screen source is blocked while its real input remains live" | `dilla-media::tests/capture::an_unbound_screen_source_sends_nothing` |
| … "a foreign instance cannot stop or reconfigure the capture and stopping its track destroys the broker" | `dilla-desktop::tests/share::another_instance_cannot_stop_or_reconfigure_a_capture` |
| … "owner navigation releases its native broker" | `dilla-desktop::tests/share::closing_the_owner_window_stops_its_capture` |
| … "closing the selected source ends its consumer track and destroys the broker" | `dilla-media::tests/capture::a_closed_source_ends_the_track` |
| `fail-closed.spec.ts` "a sender labelled with a server-forced codec (AV1) still attaches and encrypts what it sends" | `dilla-media::tests/call::a_forced_codec_sender_attaches_and_encrypts` |
| … "a sender whose frames have no prefix rule (AV1) sends 0 media bytes" | `dilla-media::tests/call::av1_without_prefix_rule_sends_nothing` |
| `unsupported-sfu-codec.spec.ts` "the real SFU offers AV1 but the client negotiates and sends VP8" | `dilla-media::tests/live::sfu_offers_av1_client_sends_vp8` |

Also: a camera test with a fake V4L2 device (`v4l2loopback` in CI if available, otherwise a recording fake behind
`CaptureSource`), and `MANUAL.md` steps for SCK and WGC.

**Risk:**
- Wayland portal UX (the portal draws its own picker; beta.5 did the same);
- macOS TCC prompts;
- `zed-scap` maturity;
- libwebrtc video encoder availability in the prebuilt `webrtc-sys`.

---

### Package 2 — Share picker and share UI  *(M, Wave B)*

**beta.5 behaviour:**
- `screen-capture.ts:241`: a modal 720×560 picker with Screen and Window tiles (320×180 JPEG thumbnails, window icons)
  on macOS, Windows and X11, and the system portal on Wayland;
- web `VoiceView.tsx` and `strings/stage.ts` `callStage.share.*`: Screen / Window / Source, preview `Display · {w}×{h}`,
  "Stream quality", Apply, "Also share system audio", live indicator;
- `voice.cameraOn` and `voice.shareOn` controls; the share-without-sound advice shown to both sides.

**Gaps (7):**
1. the picker window;
2. its thumbnails;
3. the quality options with Apply (live reconfigure);
4. the camera button that starts a camera;
5. the share button that starts a share;
6. "Also share system audio" (wired up in Package 3);
7. the no-sound mark seen by the viewer.

**Files:**
- new `dilla-desktop/src/screens/share_picker/{mod,view,keys}.rs`;
- new `dilla-desktop-ui/src/components/share_picker/`;
- `dilla-desktop/src/screens/voice/{mod,session}.rs` (controls);
- `parity/fixtures/share_picker/`, `parity/references/linux/share_picker/`.

**Tests:**

| beta.5 title | New Rust test |
|---|---|
| `share.spec.ts` "a share without sound says so on both sides" | `dilla-desktop::tests/voice::a_share_without_sound_says_so_on_both_sides` |
| `share.spec.ts` "cancelling native screen selection publishes no video or sound and leaves the voice call usable" | `dilla-desktop::tests/share::cancel_publishes_nothing_and_the_call_goes_on` |
| `permissions.spec.ts` "cancelling the typed native screen picker returns null without creating a broker" | `dilla-desktop::tests/share::cancelling_the_picker_creates_no_capture` |
| `permissions.spec.ts` "screen capture without a real user gesture is refused without opening a picker" | `dilla-desktop::tests/share::no_picker_without_a_press_on_a_focused_window` |

Plus parity shots of the picker (Screen tab, Window tab, quality open) and the stage with a local share.

**Risk:** there is no Signal reference for the native picker frame, so ask Jonas before drawing (Global Constraint).

---

### Package 3 — Application audio  *(L, opus, Wave C)*

**beta.5 behaviour:**
- `native-audio.ts` and `packages/desktop/native/{linux.cpp (PipeWire per-process ports), AudioCapture.swift (macOS
  process tap), windows.cpp (WASAPI process loopback), common.hpp, process-parent.hpp}`;
- the chooser is a native dialog that lists applications that are playing (`main.ts:423`: "Choose the application others
  will hear", "Dilla’s own call audio is excluded"; with none, "No playback applications are available…");
- the audio is 48 kHz stereo f32, 960-sample frames, validated and clamped;
- the tray shows "Sharing application sound: {label} — stop".

**Gaps (5):**
1. Linux capture;
2. macOS capture;
3. Windows capture;
4. the chooser;
5. the publish path: encrypted, with an authenticated source slot, never plaintext, plus the tray stop entry.

**Files:**
- new `dilla-media/src/app_audio/{mod,linux,mac,windows}.rs`;
- `dilla-media/src/room.rs` (a second audio track);
- `dilla-desktop/src/screens/share_picker/` (the checkbox, the chooser);
- `native/tray.rs` section (through the registry).

**Tests:**

| beta.5 title | New Rust test |
|---|---|
| `application-audio.spec.ts` "selected application reaches an encrypted listener while other applications and Dilla call audio stay excluded" | `dilla-media::tests/app_audio::only_the_chosen_app_reaches_the_listener_encrypted` (Linux: two `pw-play` processes in CI under PipeWire) |
| … "native application audio without an authenticated source slot cannot fall back to plaintext" | `dilla-media::tests/app_audio::no_source_slot_sends_nothing` |

Plus the unit tests ported from beta.5 `test/app-audio-lifetime.test.ts`, `audio-lifecycle.test.ts` and `audio.test.ts`
(frame parsing, limits, lifetime).

**Risk:**
- macOS process taps need macOS 14.2+ (beta.5's floor);
- PipeWire in CI;
- FFI safety.

---

### Package 4 — Operating-system notifications  *(M, opus review, Wave A)*

**beta.5 behaviour** (`main.ts:484-512`, `contract.ts` `notify`, `notify-ring`, `dismiss-notification`; web
`desktop.ts:12 desktopNotificationCtor`):
- title ≤256 and body ≤4096 characters;
- a ring is silent, with Answer and Decline actions, `timeoutType: never`, critical urgency;
- a click focuses the window and opens the conversation;
- closing the notification withdraws it;
- notifications are keyed per instance, at most 64;
- closing or reloading a window withdraws its notifications;
- shown only with `Notification.isSupported()` and the per-instance grant;
- Windows needs `setAppUserModelId('dev.dilla.desktop')`.

**Gaps (6):**
1. message and mention notifications;
2. ring notifications with Answer and Decline;
3. click to focus and navigate;
4. dismiss and withdraw lifecycle;
5. the device switch (Settings → Notifications) and the OS permission;
6. Windows AUMID and macOS bundle identity.

**Files:**
- `native/notifier.rs` (an `OsNotifier` that implements `dilla_client::features::notify::Notifier` on gpui's
  `show_system_notification`);
- `dilla-client/src/features/notify.rs` (the trait gains `withdraw(tag)`, and the in-app `RecordingNotifier` stays for
  tests);
- `dilla-desktop/src/screens/ring/effects.rs:118,165`;
- `dilla-desktop/src/screens/notify/`.

**Tests:**

| beta.5 title | New Rust test |
|---|---|
| `00-mac-notifications.spec.ts` "macOS accepts a real native notification from the shipping sandboxed app" | `MANUAL.md` step + `dilla-desktop::tests/notify::os_notifier_shows_through_the_platform` (the Linux D-Bus run under `dbus-run-session` with a fake notification server) |
| `ring.spec.ts` "answer: bob rings (banner, sound, one notification while hidden)…" (its OS part) | `dilla-desktop::tests/ring::a_hidden_window_raises_one_os_ring_with_answer_and_decline` |

New tests:
- `notify::a_click_focuses_the_window_and_opens_the_conversation`;
- `notify::closing_the_window_withdraws_its_notifications`;
- `notify::at_most_64_live`;
- `ring::decline_from_the_notification_acts_only_on_the_current_ring`.

**Risk:**
- Windows toasts without an installed shortcut do nothing, so the feature depends on release-plan Task 13. Until then
  it is tested on Linux and macOS;
- GNOME drops action buttons on some versions.

---

### Package 5 — Tray, background, autostart, badge, app menu, zoom  *(M, Wave A)*

**beta.5 behaviour** (`main.ts:169, 270-277, 338-367, 546-571`; `autostart.ts`):
- the tray icon; a click focuses the first window or opens the manager;
- tray menu entries:
  - the app-audio stop row;
  - ring Answer and Decline submenus;
  - "Your instances…";
  - one row per open instance;
  - "Start Dilla at login" (checkbox);
  - "Check for updates", "Update failed — retry", "Update using your package manager", "Restart to update";
  - "Quit Dilla".
- the tooltip "Dilla — {n} unread" and the update-failed tooltip;
- closing a window hides it; `window-all-closed` does not quit; macOS `activate` reopens;
- `app.setBadgeCount(sum of unread)`;
- autostart:
  - Linux writes `~/.config/autostart/dilla-desktop.desktop` with `Exec="…" --hidden` (mode 0600);
  - macOS and Windows use `setLoginItemSettings` with `--hidden`;
  - `--hidden` starts with no window shown;
- macOS application menu: Dilla (the tray entries), Edit, View (reset zoom, zoom in, zoom out, toggle fullscreen),
  Window.

**Gaps (7):**
1. the tray icon and menu;
2. background lifetime: quit only explicitly, macOS reopen;
3. the badge (macOS dock, Windows overlay icon, Linux Unity LauncherEntry);
4. the tooltip;
5. autostart with `--hidden`;
6. the application menu bar (`set_menus`);
7. zoom in, zoom out, reset and fullscreen.

**Files:**
- `native/{tray,badge,autostart,menus}.rs`;
- `dilla-desktop/src/screens/titlebar.rs` (window menu entries only);
- zoom as a per-app UI scale in `dilla-client/src/prefs.rs`. Ask Jonas: Electron's zoom was per-instance Chromium zoom,
  so the scale must not break pixel parity at 100 %.

**Tests:**

| beta.5 title | New Rust test |
|---|---|
| `shell.spec.ts` "closing a window keeps the client alive for background work" (the close-to-tray part) | `dilla-desktop::tests/instances::closing_the_last_window_keeps_the_app_in_the_tray` |

New tests:
- `native::tray::menu_lists_open_instances_and_quit`;
- `native::badge::sum_of_unread_across_instances`;
- `native::autostart::desktop_entry_quotes_exec` (port beta.5 `test/autostart` cases);
- `tests/shell::hidden_start_opens_no_window`;
- `tests/shell::zoom_in_out_reset`.

**Risk:** GNOME without the AppIndicator extension shows no tray. beta.5 had the same limit, so document it. A tray
crate on Linux that drags in GTK: prefer `ksni`.

---

### Package 6 — Single instance and `dilla://` deep links  *(M, opus review, Wave A)*

**beta.5 behaviour:**
- `main.ts:534-542`: `requestSingleInstanceLock`. A second launch forwards argv: a `dilla://` link opens the invite;
  otherwise it focuses the first window or the manager;
- `open-url` (macOS); links that arrive before the app is ready are queued (`pendingInvites`);
- `openInvite` → `parseInvite` (`policy.ts`) → `confirmInstance` ("Connect to {host}? Only continue if you recognize
  this address.") → open at `/welcome?invite=<code>`;
- `setAsDefaultProtocolClient('dilla')` when packaged.

**Gaps (6):**
1. the single-process lock;
2. second-launch handoff;
3. the invite grammar and routing to join-with-invite in the right instance window;
4. confirming an unknown instance;
5. macOS `on_open_urls` and early-link queueing;
6. scheme registration on Linux (`.desktop` MimeType) and Windows (registry), which go to release-plan Tasks 2 and 13
   (that plan's D12 lifts when this lands).

**Files:**
- `native/{single_instance,links}.rs`;
- `dilla-desktop/src/screens/instances/` (the confirm dialog, the join-with-invite entry);
- `dilla-client/src/instances.rs` (an invite parser shared with the manager's "paste an invite").

**Tests (new; beta.5 had them as unit tests in `test/policy.test.ts`, port its cases):**
- `links::parses_only_the_invite_grammar`;
- `links::unknown_instance_asks_first`;
- `single_instance::second_launch_focuses_the_running_app`;
- `single_instance::second_launch_with_a_link_opens_the_invite`;
- `links::no_link_opens_during_quit`.

**Decision D2** (one process versus `--instance=n` copies) sets the scope.

---

### Package 7 — Quit guards and permission prompts  *(M, opus review, Wave B)*

**beta.5 behaviour:**
- `main.ts:281-333` `collectQuitGuards` and `requestQuit`: it asks every window for a fresh guard, with a 3 s timeout;
- the dialogs, in order:
  - an unresponsive window: "Dilla is not responding… Keep Dilla open / Quit anyway";
  - in use: "Quit Dilla? Quitting leaves your call and discards text you haven't sent." Cancel / Quit;
  - still protected while updating: "Finish your call and save your drafts before quitting or updating.";
- an update restart re-collects the guards (`quit-install.ts`);
- web `desktop.ts:119-140` `setDesktopGuard` and `quit-ack` (including pending input), and the `update.*` strings
  (`update.finishCall`, `update.finishDialog`, `update.sharingStopped`, `update.rejoiningVoice`);
- permissions (`main.ts:215-232`):
  - a per-instance "Allow {host} to use microphone or camera / notifications / audio output selection?" with
    persisted grants (`preferences.ts`);
  - display capture only through the broker.

**Gaps (6):**
1. guard collection per window (call, draft, unsent composer input);
2. the quit dialog;
3. the unresponsive-window dialog;
4. update-restart integration (release-plan Task 14);
5. macOS usage strings (`NSMicrophoneUsageDescription`, camera, screen recording), and a denied permission shown as
   the listen-only / no-camera notices;
6. per-instance grants (see D1).

**Files:**
- `native/{quit,permissions}.rs`;
- the guard providers in `dilla-desktop/src/screens/voice/` (call) and `screens/messages/` (drafts), registered through
  `NativeShell::add_quit_guard`;
- the Info.plist keys go to release-plan Task 12.

**Tests:**

| beta.5 title | New Rust test |
|---|---|
| `voice/realtime-transport.spec.ts` "…defers an update until the call ends" (the quit part) | `dilla-desktop::tests/quit::an_update_restart_waits_for_the_call` |
| `permissions.spec.ts` "the shipping permission boundary refuses ${kind} capture after native Deny" | `dilla-desktop::tests/permissions::a_denied_device_refuses_capture` (or n/a per D1) |
| `permissions.spec.ts` "a grant is reusable by its owner but cannot authorize another complete instance URL" | `dilla-desktop::tests/permissions::a_grant_is_per_instance` (or n/a per D1) |

New tests: `quit::active_call_asks_before_quitting`, `quit::unsaved_draft_asks`, `quit::silent_window_counts_as_in_use`.

---

### Package 8 — Voice gaps  *(M, Wave A; three lanes with disjoint files)*

**8a No-speaker join (S):**
- GPUI today: `room.rs:576` `Speaker::open_with(..)?` fails the whole join, and `session.rs:522 failed()` sets
  `VoiceNotice::MediaFailed` and leaves.
- beta.5 joins, plays nowhere, and the person can still talk.
- Fix:
  - make the speaker optional on `MediaError::Audio`, with no echo reference (APM without a reverse stream);
  - raise `VoiceNotice::SpeakerLost` (existing copy: ask if a new string is wanted);
  - re-open the speaker when a device appears (`DevicesChanged`).
- Test: `dilla-media::tests/call::a_join_without_an_output_device_still_sends_and_receives_signalling` and
  `dilla-desktop::tests/voice::no_output_device_joins_with_a_notice` (`asound-null.conf` / an empty ALSA config, as the
  scratchpad's `empty-asound.conf`).

**8b Stage person menu and volumes (M):** web `VoiceView.tsx:93-170`, `call/volume.ts`, `strings/stage.ts:23-34`.
- The menu items:
  - Profile, Message, Mention;
  - "User volume" or "Stream volume" (0–200 % in steps of 10);
  - "Mute for me";
  - Copy username;
  - "Move to" another voice room (with move_members);
  - "Leave voice";
  - the `callStage.menu.moved` and `copied` announcements.
- `voice_prefs` already stores `volumes` and `stream_volumes` (`dilla-client/src/features/voice_prefs/values.rs:107`),
  but nothing reads them. Apply a per-participant gain in `dilla-media/src/playout.rs`.
- Tests:
  - `dilla-media::tests/audio::per_participant_gain_scales_playout`;
  - `dilla-desktop::tests/voice::the_tile_menu_sets_volume_and_mute_for_me`;
  - `…::move_to_moves_the_member`;
  - a parity shot of the menu.

**8c Stage views and notices (M):**
- grid toggle (`callStage.grid`);
- full-screen stage (`stage.fullscreen`, window fullscreen);
- the voice channel's Messages view (`voice.messages`, `VoiceView.tsx:744`);
- the dock's link rate (`voice.dock.rate`);
- the `voice.leftWhileAway` and `voice.removedNotMember` notices;
- the voice row tags `web` and `bot` (`shell-model.ts:344`);
- the PTT tag in the user panel (`layout.ptt.tag`);
- "Return to call" in settings (`workspace.returnCall`).
- Each item starts with a check that it is really absent (the key diff found no GPUI use). Write one test per item in
  `dilla-desktop::tests/voice`.

---

### Package 9 — Push to talk with AltGr on Linux  *(S, Wave A)*

- Bug: `dilla-desktop/src/screens/voice/mod.rs:1398-1407` refuses control, alt, shift, platform and function. On Linux,
  GPUI's xkb layer reports AltGr (ISO_Level3_Shift, Mod5) as none of these, so AltGr+key opens the microphone.
- Fix: a `DILLA PATCH` in vendored `gpui-pre` (or `gpui-pre-linux`, vendored for this) that surfaces Level3 as a
  modifier, or a check of the keystroke's `key_char` against the bare key. Record it in `core/vendor/README.md` and
  `tests/vendored_patches.rs`.
- Test: `dilla-desktop::tests/voice::altgr_with_the_ptt_key_does_not_open_the_mic` (a synthesized Level3 keystroke).
- Docs: remove the AltGr line from `desktop.md` and the README.

---

### Package 10 — Screen gaps  *(M as 4 S lanes, Wave A)*

From the web key diff, checked by hand. Each lane owns its area's files.

- **10a Messages** (`screens/messages/`, `screens/timeline.rs`):
  1. a failed send offers Retry and Discard (`shell.message.retry`, `.discard`; `MessageItem.tsx:129,201`). Today
     `timeline.rs:538` draws only "failed";
  2. the `send.tooLarge` detail;
  3. `privacy.revokedMessage` for `E_DEVICE_REVOKED` items;
  4. the timed-out composer (`shell.composer.timedOut`, `Shell.tsx:1456`).
  - Tests: `tests/messages::a_failed_send_retries`, `…::discard_removes_the_failed_message`, `…::timed_out_member_sees_until`.
- **10b Create and roles** (`screens/create/`, `screens/members/roles.rs`):
  5. the private-channel switch with role ticks at create, and the announcement channel's role list (`chperm.create.*`;
     `create/flows.rs:8` names this gap);
  6. "current colour" for a custom role colour (`role-palette.ts:13`).
  - Tests: `tests/server_settings::create_private_channel_with_roles`, `tests/members::custom_colour_is_listed`.
- **10c Bot** (`screens/bot_admin/`, `screens/commands/`):
  7. the Bot "more actions" menu: Rename (bot display name, `admin.bot.name.*`), Refresh status, Disable
     (`BotSection.tsx:462-464,609`);
  8. the `/announcement` editor dialog (`BotCommandDialogs.tsx` `AnnouncementEditor`: destination, once, daily or
     weekly, date, hour, minute, weekday, timezone);
  9. the `/rolepanel create` editor (`RolePanelEditor`);
  10. the Music page's DJ-roles hint (`serverSettings.music.djRoles`), the queue list and the volume name.
  - First check items 8 and 9 against `commands/chips.rs`: the named-field chips may already cover them.
  - Tests: `tests/bot_admin::rename_refresh_disable`, `tests/bot::announcement_editor_schedules_weekly`,
    `tests/bot::rolepanel_editor_publishes`.
- **10d Boot, settings and docs** (`screens/boot.rs`, `screens/settings_host.rs`):
  11. a revoked device gets the web's explanation and reset (`boot.revoked.*`) rather than a bare sign-in;
  12. a lost store or keychain key gets `boot.storeLost.*` rather than Fatal; `workspace.permissionChanged` when your
      access changes while settings are open;
  13. docs: `desktop.md` and the README still say "Delete is only offered on your own messages". Remove-for-everyone
      shipped (`screens/messages/actions.rs:84`, commit 056dadb57).
  - Tests: `tests/shell::revoked_device_boot_explains_and_resets`, `…::lost_key_boot_offers_reset`,
    `tests/server_settings::permission_change_is_announced`.

---

### Package 11 — Screen readers (AccessKit)  *(L, opus, 11.1 in Wave A, the rest in Wave B)*

**beta.5 behaviour:** Chromium's accessibility tree over ARIA-labelled React:
- regions: rail, channel list, log, composer, dock, members (the web `*.region`, `*.label`, `*.rowLabel` keys);
- live announcements: `voice.announce.joined` and `.left`, `callStage.menu.copied`, `workspace.permissionChanged`;
- axe-clean in every Signal state, including target-size.

**Gaps (5):**
1. roles and names on every `dilla-desktop-ui` component (button, icon_button, row, dialog, confirm_dialog,
   action_menu, text_field, toggle, select, segmented, tabs, badge, avatar, tooltip, toast);
2. landmarks and regions per screen;
3. live announcements (check `accesskit::Live` support in gpui-pre's `window/a11y.rs`; if missing, a `DILLA PATCH`);
4. focus sync between GPUI focus and AccessKit focus;
5. a tree-lint test to replace axe.

**Steps:**
- 11.1 the components;
- 11.2 the shell, messages and threads;
- 11.3 voice, ring and stage;
- 11.4 settings, onboarding, factor and the instances window.

**Tests (port the 5 axe rows):**
- one harness, `dilla-desktop::tests/a11y`, builds the AccessKit tree for a fixture state (through gpui's
  `window/a11y.rs` tree update) and checks:
  - every focusable node has a role and a non-empty name;
  - no duplicate ids;
  - dialogs are modal;
  - target size ≥ 24 px.

| beta.5 title | New Rust test |
|---|---|
| `afk.spec.ts` "10. accessible: the channel list, the AFK row with its away roster and server settings" | `a11y::afk_channel_list_and_server_settings` |
| `csp-axe.spec.ts` "every state of the slice passes axe with target-size…" | `a11y::every_slice_state_passes_the_tree_lint` |
| `search-settings.spec.ts` "desktop, phone and reflow keep controls accessible in Signal" | `a11y::search_and_settings_pass_the_tree_lint` |
| `support.spec.ts` "axe evaluates under the served CSP without a policy violation" | `a11y::the_tree_lint_runs_on_every_fixture` |
| `voice/csp-axe.spec.ts` "the ring, the call, a player and Settings → Voice pass axe…" | `a11y::ring_call_player_voice_settings_pass_the_tree_lint` |

Plus `MANUAL.md`: Orca (GNOME, Wayland and X11), VoiceOver and NVDA walkthroughs of sign-in, send, join call and
answer a ring.

**Risk:**
- GPUI's a11y support is new (the `text!` id footgun, release builds dropping duplicates);
- Orca on Wayland through AT-SPI;
- the work touches every screen, so run it after the other screen lanes merge, or rebase per area.

**Decision D3** sets whether this blocks the switch.

---

### Package 12 — Inline video playback  *(L, Wave C if D4 = before the switch)*

- beta.5 played attachments inline in HTML `<video>` (webm, mp4).
- GPUI shows a poster and opens the system player (`screens/attachments/media.rs:539`).
- The 2026-11-12 decision already names the stack: openh264 runtime binary, dav1d, vpx-rs, and Apple's decoder for
  HEVC, with no FFmpeg.
- Files: new `dilla-desktop/src/screens/attachments/video/`, `dilla-desktop-ui/src/components/attachments/player.rs`.
- Test: port `voice/media-attachments.spec.ts` "bob plays alice's webm…" to `tests/attachments::webm_plays_inline`.

### Package 13 — Spell-check  *(M, after the switch if D5 = defer)*

- Chromium underlined misspelled words in the composer (`spellcheck` defaults to on; the web turned it off only on
  code and name fields).
- beta.5 had **no** context-menu suggestions, only the underline.
- Native options: the system spell checker (NSSpellChecker, Windows ISpellChecker) and Hunspell on Linux, drawn as an
  underline in `gpui-component` input.

---

### Closing task (each wave)

- [ ] Every row this wave ported in `BEHAVIOUR-GATES.md` names its Rust test. The totals line is updated.
  `node --test scripts/behaviour-gates.test.mjs` passes.
- [ ] `docs/user/desktop.md` "Known gaps" and README "For the native desktop preview" list only what is left.
- [ ] Screenshots are re-shot (`scratchpad/shoot_docs.py` pattern).
- [ ] `npm run ci:local`, then one PR per wave and merge on green.
- [ ] Offer `/claude-obsidian:save` for the outcome.

---

## Owner decisions (recommendation first)

| # | Question | Options | Recommendation |
|---|---|---|---|
| D1 | Keep beta.5's in-app per-instance permission prompts ("Allow {host} to use the microphone?")? | (a) OS prompts only, plus the existing per-device notification switch. (b) Port the per-instance prompt and grants. | **(a).** The prompt guarded a renderer that loaded each instance's web code. The native app runs only shipped code, so the 2 permission rows become n/a with that reason. Saves part of Package 7. |
| D2 | One process for all instances (as Electron), with second launches handing over? | (a) Yes: a single lock, and `--instance=n` becomes debug-only. (b) Keep side-by-side copies per instance. | **(a)** matches beta.5 and makes deep links, tray and badge one place. |
| D3 | Do screen readers block the switch? | (a) Yes, Package 11 before the switch. (b) After the switch (the wave-1 plan's position). | **(a).** "Same feature set": Electron had a working screen-reader tree, and the switch would remove it for users who need it. |
| D4 | Does inline video playback block the switch? | (a) Yes, Package 12 before the switch. (b) The system player until the Rust video stack lands. | **(b).** Video still plays (in the system player), and the stack is large and was already sequenced "then" on 2026-11-12. |
| D5 | Spell-check underline? | (a) Before the switch. (b) After the switch. (c) Never. | **(b).** Electron had only the underline, with no suggestions. |

## Decisions (final, Jonas 2026-11-15)
- D1 OS permission prompts only (no per-instance in-app prompts).
- D2 One process for all instances; a second launch hands over.
- D3 Screen readers (AccessKit) required before the switch.
- D4 Inline video playback BEFORE the switch (package 12 is in scope).
- D5 Spell-check underline after the switch (package 13 deferred).
