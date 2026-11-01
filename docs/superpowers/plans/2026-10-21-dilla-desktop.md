# dilla desktop: plan

> Superseded by the approved 2026-10-31 desktop-only-apps plan: ship all desktop
> platforms together with a bundled client, no package-manager distribution, and
> mandatory unsigned Sparkle upgrade acceptance. Do not execute the conflicting
> Linux-first or server-loaded decisions below. See
> [current foundation status](../../desktop/FOUNDATIONS.md).

> **For agentic workers:** REQUIRED SUB-SKILL: use superpowers:subagent-driven-development. Phase 0 spikes come first, and each ends in a FINDINGS note and a go/no-go.

## Context
On 2026-10-21 Jonas asked how to share a game's sound. He runs a Linux desktop (CachyOS) and browses with Zen. A browser can see audio devices, never applications. The "sound from" picker in web3.9.2 (docs/user/voice.md, "Share sound from Firefox or Linux") works around this only with a manual PipeWire recipe. He chose to plan a desktop app, like Discord's.

The desktop app also closes four gaps in the README's "What is not there yet":
- voice from the desktop app;
- ringing with no tab open, and answering from the notification;
- notifications after every tab closes;
- more than one working tab.

## Rulings (Jonas, 2026-10-21; binding)
- **D1 Shell: Electron.** It ships its own Chromium, so WebRTC, LiveKit and the E2EE frame transforms behave exactly as in Chrome on every OS. Tauri's WebKitGTK cannot carry Linux voice reliably, and its WKWebView inherits Safari's gaps.
- **D2 Platforms: Linux first** (v1.0), then Windows (v1.1), then macOS (v1.2), all on the same native audio crate.
- **D3 Distribution: unsigned for now.**
  - Linux ships as an AUR `dilla-desktop-bin` package and an AppImage. Updates are verified with a pinned minisign key; that signs the updates, not the OS install.
  - Windows and macOS builds ship unsigned when they come. SmartScreen will warn on Windows, and Gatekeeper on macOS needs the user's approval. Paid signing (Azure Trusted Signing, an Apple Developer account) is decided later. The release notes and docs/user/desktop.md must say all this plainly.
- **D4 Defaults kept:**
  - The app loads the user's own instance URL; there is no bundled client.
  - Updates come only from GitHub Releases and are checked against a pinned minisign public key. They never come from an instance.
  - The code lives in `packages/desktop` in this monorepo, the binary is `dilla-desktop`, and links use the `dilla://` scheme.

## v1 scope
- **Per-application sound in a screen share.** It carries only the chosen app's sound, never the call's. The audio is E2EE like every other track and goes in the existing `screen_share_audio` slot, so watchers keep their per-stream volume and mute.
  - **Linux:** a native PipeWire client lists `Stream/Output/Audio` nodes by `application.name` and `application.process.binary`, excluding dilla itself. It creates a virtual `Audio/Source` node, `dilla-share-<app>`, and links the app's ports to it. Video goes through the xdg-desktop-portal ScreenCast, which Chromium already uses. Prior art: Vesktop's venmic.
  - **Windows (v1.1):** WASAPI process loopback (`AUDIOCLIENT_ACTIVATION_TYPE_PROCESS_LOOPBACK`, which includes the target process tree).
  - **macOS (v1.2):** ScreenCaptureKit with `capturesAudio`, an `SCContentFilter` for one app, and `excludesCurrentProcessAudio`.
- **Background notifications and ringing,** with answer and decline from the notification.
- **Tray and startup:** a tray icon, close-to-tray, and autostart that starts hidden.
- **`dilla://` invite deep links.**
- **The minisign-verified auto-update channel.**
- **One window per instance.**

**Not in v1:** mobile, signed installers, Flatpak (after v1), and global push-to-talk hotkeys (a v1.1 candidate). Nothing changes the wire protocol or the server.

## Architecture
- **Loading:**
  - Each instance gets its own window with `session.fromPartition('persist:dilla-<host>')`. IndexedDB, OPFS, cookies and the leader lock in packages/client-core/src/worker/controller.ts therefore stay per instance.
  - The desktop app is enrolled as a new device: the recovery key or another device adds it.
  - Keys stay in the existing core worker. The native code never sees them.
- **Bridge:**
  - A sandboxed preload exposes a typed `window.dillaDesktop` through `contextBridge`, only in the top frame of the instance origin. It offers `version`, `capabilities()`, `requestAppAudio()`, `stopAppAudio()`, `notify()`, `ring()`, `setBadge()`, `onAction()` and `onDeepLink()`.
  - The app picker is main-process UI behind a user gesture. The page learns only the user's choice.
  - Every IPC message is schema-validated. The bridge offers no file system, shell or arbitrary IPC.
  - The web client feature-detects the bridge, so browsers are unaffected.
- **Track injection:**
  - On Linux, the virtual PipeWire source appears in `enumerateDevices()`. The page opens it with `getUserMedia({deviceId:{exact}})`, which is the FX2 path. There is no IPC.
  - On Windows and macOS, the source is an AudioWorklet fed over a `MessagePort`: 10 ms frames at 48 kHz in stereo, published from a `MediaStreamAudioDestinationNode`.
  - Spike S5 measures the latency and CPU of both.
- **Hardening:**
  - `contextIsolation` and `sandbox`, with no node in the renderer;
  - navigation is locked to the instance origin, and `setWindowOpenHandler` sends other links to the system browser;
  - the permission handler grants only media and notifications, and only to that origin;
  - no `<webview>`, no certificate bypass;
  - Electron fuses: RunAsNode, node CLI inspect and NODE_OPTIONS are off; asar-only with integrity, and cookie encryption are on;
  - capture runs in a utility process, so a crash there never takes the window down;
  - a tray "sharing app sound" indicator is visible while capture runs;
  - Electron security releases are taken within 2 weeks.
- **Updates:**
  - GitHub Releases, with stable and beta channels.
  - The metadata and every artifact carry an ed25519 (minisign) signature, and the public key is pinned in the app.
  - The signing key is kept offline, separate from the GitHub token.
  - Linux builds are reproducible: pinned toolchains, `SOURCE_DATE_EPOCH`, and a diff of two CI builds.
- **Deep links:** `dilla://invite/<host>/<code>` is parsed strictly and must be https. A dialog naming the host comes before any new instance is added. Links are never passed to a shell.

## Phases and tasks (Linux v1.0 first)
### Phase 0: spikes (opus; time-boxed; each ends with a FINDINGS note and a go/no-go)
- **S1:** per-app capture on Linux PipeWire. Enumerate nodes, create the virtual source and link it; Chromium in Electron must see it as an input. Test with a game under Proton, and with Firefox as the source, on KDE and on GNOME under Wayland.
- **S4:** E2EE capability in Electron. Run packages/media/harness/failclosed.ts plus an encrypted call.
- **S5:** injection cost. Latency (a loopback click test), CPU, and underruns over 30 minutes.
- **S2 (Windows) and S3 (macOS)** run before v1.1 and v1.2.

### Phase 1: the shell (lane A)
- **A1 (opus):** the `packages/desktop` skeleton: hardening, fuses, partitions, the navigation lock and the permission handler.
- **A2 (opus):** the typed preload bridge and the schema-validated IPC.
- **A3 (sonnet):** the instance list and the first run ("enter your instance").

### Phase 2: native audio (lane B, after S1)
- **B1 (opus):** the `dilla-desktop-audio` Rust crate with its Linux backend and a napi-rs binding.
- **B4 (opus):** worklet transport, kept as the Linux fallback.
- **B5 (sonnet):** the native app-picker UI.

### Phase 3: web integration (lane C)
- **C1 (opus):** the `app` audio kind in `packages/web/src/call/devices.ts` and the session. It publishes in `screen_share_audio`, and `shareAudioAdvice` returns `app` inside the desktop app.
- **C2 (sonnet):** copy. The "sound from" list gains "an application…". Update the docs.
- **C3 (sonnet):** `packages/web/src/notify.ts` routes to `dillaDesktop.notify`. Ring actions.

### Phase 4: OS integration (sonnet, opus review)
- The tray, close-to-tray with background throttling off, autostart and the badge.
- The `dilla://` handler; its parser gets an opus review.

### Phase 5: release (opus)
- electron-builder targets for AppImage and the AUR PKGBUILD.
- The minisign update check and the release workflow.
- A security review of the whole app.
- `docs/user/desktop.md`, which covers enrolling the app as a device and the unsigned-build notes. Update the README gaps.

### Then
- **v1.1 Windows:** S2, the B2 backend, an NSIS installer, unsigned.
- **v1.2 macOS:** S3, the B3 backend, a dmg, unsigned.

## CI
A new `desktop.yml`, which `check:ci` must learn.
- **ubuntu-24.04:**
  - `dbus-run-session` with headless pipewire, wireplumber and xvfb;
  - `pw-play` plays a 1 kHz tone under a known `application.name`;
  - Playwright `_electron` starts a call against a test dillad;
  - the watcher must receive the 1 kHz tone (checked with an FFT), and must not receive the 440 Hz call tone.
- **Also:** the build, a fuse check, the fail-closed harness, and the reproducibility diff.

## Milestones (honest effort)
| Milestone | Content | Effort |
|---|---|---|
| M0 | S1, S4 and S5 | ~1 week |
| M1 | The shell, the bridge and instances (Linux) | ~1.5 weeks |
| M2 | Per-app sound on Linux end to end, CI green | ~1.5–2 weeks |
| M3 | Notifications, ringing, tray, autostart and deep links | ~1 week |
| M4 | AUR, AppImage, minisign updates and the security review: **v1.0 Linux** | ~1 week |
| M5 / M6 | Windows v1.1 / macOS v1.2 | ~1.5–2 weeks each |

**Risks:**
- Portal behaviour differs across compositors (KDE, GNOME, Hyprland).
- Anti-cheat tools may object to process loopback (Windows).
- Electron's upgrade cadence.
- The UX of the native picker.
- Unsigned builds deter Windows and macOS users until signing is funded.
