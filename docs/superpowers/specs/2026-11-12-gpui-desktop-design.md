# dilla desktop on GPUI — design

Status: draft for Jonas's review, 2026-11-12.
Owner decisions are marked **(Jonas)**; decisions I made and stated are marked **(call)**.

## 1. Intent

Replace the Electron desktop app with a native Rust application drawn with GPUI,
the UI framework from Zed. **(Jonas)** The reason is that Electron is an outdated
foundation for dilla: we should not ship a browser to show a chat window. Discord
announced the same direction on 2026-11-04 (Rust core, GPUI desktop, native
mobile UIs).

dilla is unusually well placed for this. The whole client engine — identity,
MLS groups, send/receive, timeline, threads, pins, search, trust, device lists,
calls, Bot commands, attachment crypto, SFrame — is already Rust
(`dilla_core::client::ClientCore`) and already builds natively with SQLCipher.
Today it reaches the UI through WASM; the new app links it directly.

## 2. Goals

1. A desktop app for macOS, Windows and Linux (X11 and Wayland) **(call)**,
   written in Rust with GPUI in its builder style (`div().flex().gap_2()…`)
   **(Jonas)**, that does everything desktop beta.5 does (inventory: §9).
2. It looks exactly like today's Signal design, pixel for pixel, "for now"
   **(Jonas)**. The reference is the running beta.5 app and its parity set
   (115 screenshots in `docs/user/screenshots`, 394 pair specs in
   `docs/design/signal/parity`).
3. It ships as fast as possible, built with as many subagents in parallel as
   the work allows **(Jonas)**. The architecture below exists to make that
   parallelism safe: narrow crates with typed seams, so each screen and each
   native integration can be built and verified independently.
4. Once it does everything beta.5 does, the normal desktop update replaces the
   Electron app **(call)**.

## 3. Non-goals

- No server, protocol or wire changes. dillad, the gateway (wire v6), HTTP
  routes, MLS, the Bot and LiveKit stay as they are. If the rewrite finds a
  protocol bug, it is fixed separately and both clients follow.
- No phone changes. iOS and Android stay on Capacitor + React **(call)**. The
  new Rust client crate (§5) is designed so native phone UIs could sit on it
  later, but that is a separate decision.
- No design changes. New features and visual changes wait until after the
  switch.
- Screen readers (VoiceOver, NVDA, Orca via AccessKit) come after the switch
  **(Jonas)**. Keyboard navigation and focus order are NOT deferred: they are
  part of the design's behaviour and of parity.
- The browser client stays deprecated; nothing here revives it.

## 4. Anti-goals (failure even if it "works")

- Any loss of end-to-end guarantees: plaintext or keys leaving `dilla-core`
  boundaries in a new way, receivers fetching link previews (`protocol/04`
  forbids it), keys at rest less protected than today.
- Voice that cannot talk to phone and web clients. Desktop must produce and
  read exactly `dilla-sframe/1` frames.
- A "close enough" look. Parity is measured, not eyeballed.
- Two desktop apps living side by side for months. The test build is a
  bridge to the switch, not a second product.
- New Electron features during the rewrite. **(Jonas)** The Electron app gets
  fixes only until it is replaced.

## 5. Architecture

New Rust workspace members under `clients/desktop/` (exact layout is the
builder's), as four layers with typed seams:

1. **`dilla-client`** (headless, no GPUI). Replaces `packages/client-core`:
   gateway WebSocket client (`gateway/*`), HTTP client (`http/routes.ts`),
   sync engine (`sync/*`), account boot/sign-in/KEK/factors (`account/*`),
   Bot catalog and commands (`bot/*`), admin and roles (`admin/*`), call
   signalling (`calls/*`). It owns a `ClientCore` and exposes an
   observable state model (the equivalent of `state/*` slices and derived
   selectors) plus async commands. Runs on a background executor; the UI
   never blocks on it. Tested against `protocol/vectors/*.json` and against a
   real dillad in integration tests, like the TS client is today.
2. **`dilla-media`**. LiveKit Rust SDK (`livekit` crate, pinned) with
   dilla's own SFrame transform from `dilla_core::sframe`, keyed from the
   call MLS group exactly as `DillaE2EEManager` does. Audio processing
   (echo cancellation, noise suppression — today RNNoise/DeepFilter, AGC),
   device selection, push-to-talk, screen and per-application capture
   (ScreenCaptureKit on macOS, Windows Graphics Capture, PipeWire portal on
   Wayland, X11 capture), remote video frames handed to the UI.
3. **`dilla-desktop-ui`**. The Signal design system in GPUI: theme tokens
   generated from the existing CSS custom properties (about 120 colours,
   341 properties) so both apps read one source of truth during the
   transition; JetBrains Mono Variable and DM Serif Display italic; the 100
   Lucide icons as SVG; the ~105 `packages/ui` components rebuilt on
   gpui-component primitives where they fit (virtual list, menus, dialogs,
   input, markdown) and by hand where they don't.
4. **`dilla-desktop`** (the app). Screens from `packages/web` (Shell,
   channels, messages, composer, voice, Bot, settings, …) plus native
   integrations: single instance, tray and close-to-tray, badge count,
   `dilla://invite` deep links, autostart, notifications, signed-feed
   updater, system-browser passkey sign-in, screen-share picker.

Dependency direction is strictly 4 → 3 → (1, 2) → dilla-core. Screens talk
to `dilla-client` only through its state model and commands, never to
`ClientCore` directly.

### Dependencies

- GPUI: the weekly `gpui-pre` snapshots (0.3.x) plus `gpui-component`
  (0.9.x), which pins an exact `gpui-pre`. The official `gpui` crate is 13
  months stale and Zed main is not on crates.io. **(call)** Pin both;
  upgrade deliberately, one bump per PR, never mixed with feature work.
- Zed's call and capture code is GPL-3.0: read it for approach, never copy.
  Everything we add must stay Apache/MIT/BSD-compatible.
- Missing pieces we add ourselves: tray (`tray-icon`), single instance,
  updater, keychain (`keyring`).

### Keys at rest

Today keys live in the page's IndexedDB/OPFS (sqlite3mc), with no OS keychain.
The native app stores its SQLCipher key-encryption key in the OS keychain
(Keychain, Credential Manager, Secret Service), with the same password/MFA
unlock flow as today. That is at least as strong as today, never weaker.

## 6. Hard risks, proven first

Before any screen work, three throwaway proofs run in parallel. Each either
passes or changes this design:

1. **Voice interop.** A Rust client in a call with the current desktop and
   a phone, sending and receiving audio and a screen share, encrypted with
   `dilla-sframe/1` through LiveKit's Rust SDK (custom frame transformer or
   a libwebrtc patch). If the SDK cannot host a custom transform, that is
   the first thing to solve, because everything voice depends on it.
2. **Pixel fidelity.** The same message list and composer rendered in GPUI
   and in beta.5 at the same size, compared per pixel: font rasterisation
   (JetBrains Mono), 1px borders, radii, colours. This sets the tolerance
   the parity gate (§7) uses.
3. **Text input.** A multi-line composer with IME (Japanese/Chinese input,
   dead keys, emoji picker) on macOS, Windows, X11 and Wayland.

Also measured early: remote video on Windows/Linux is CPU-converted per frame
in GPUI today; a 1080p screen share must stay smooth, or video rendering
needs its own work item.

## 7. How "done" is judged

- **Parity gate per screen.** Every state in the parity set is rendered by
  the GPUI app with the same fixture data and compared against the beta.5
  screenshot, within the tolerance from proof 2. A screen is done when all
  its states pass, including hover, focus and keyboard states.
- **Behaviour gate.** Every behaviour the e2e suites pin today (web 49
  specs, voice 22, media 12, desktop Electron ~37 tests) has a counterpart
  that runs against the GPUI app. Playwright cannot drive GPUI, so the app
  gets a test-only automation interface (compiled out of release builds)
  that a Rust test harness drives: inspect the element tree, click, type,
  read state. Two real dillad-backed devices in one test, as today.
- **Switch gate.** All parity and behaviour gates green on all three OSes,
  the voice interop proof repeated end to end in CI, and Jonas using the
  test build daily. Then the update replaces Electron.

## 8. Rollout

- **Test build** **(call)**: a separately named app ("dilla (GPUI preview)"),
  installable next to beta.5, its own data directory, signing in as a new
  device of the same account.
- **Switch**: the normal desktop update installs the GPUI app in place of
  Electron on every platform and format we ship today (macOS DMG, Windows
  installer, Linux AppImage/deb/rpm/AUR).
- **Electron during the rewrite** **(Jonas)**: fixes only. Its tests stay
  green until it is retired; then `packages/desktop` and its Linux sandbox
  component are removed (the sandbox exists only for Electron).
- **Existing installs at the switch**: a new device (§12).

## 9. Scope inventory (what parity means)

From the recon (full inventory in the session notes, summarised here):

- `packages/web/src`: ~30,900 lines of screens and logic (largest:
  `Shell.tsx` 2,354, `call/session.ts` 1,970).
- `packages/ui/src`: ~12,200 lines, 105 components, 3,100 lines CSS.
- `packages/client-core/src`: the TS client layer replaced by `dilla-client`.
- `packages/media`: WebRTC, Opus, noise suppression, SFrame worker.
- `packages/desktop/src`: ~2,700 lines of Electron integration (single
  instance, tray, badge, deep links, autostart, updater with Sparkle on
  macOS, screen-share picker, per-app audio capture, system-browser
  passkeys, ~16 IPC commands).
- Behaviour areas: auth/MFA/passkeys, onboarding and create flow, servers and
  channels, messages/composer/attachments/GIFs/emoji/reactions/threads/polls,
  DMs, voice/video/screen share/VoiceDock/takeover, Bot (commands, private
  results with auto-dismiss), members/profiles/roles/moderation, settings,
  diagnostics, notifications, search, 24-hour times and locale.

## 10. Parallel build shape

Phases, each fanned out to parallel subagents in isolated worktrees, merged
through one integration branch with ci:local before every push:

0. The three proofs (§6) in parallel.
1. Foundations in parallel: `dilla-client` (split by area: gateway+sync,
   HTTP+account, Bot+admin, calls), `dilla-media`, the design system and
   token codegen, the test automation interface and parity harness.
2. Screens in parallel, one agent per screen area, each gated by its
   parity and behaviour tests.
3. Native integrations in parallel (tray, updater, deep links, passkeys,
   capture picker, notifications, packaging per OS).
4. Switch release.

Each phase starts only when the seams it builds on are merged; inside a
phase, agents never share files.

## 10a. Settled after the builder check (calls)

- **Multiple instances are in scope.** beta.5 runs up to 32 instances
  (each its own window, session and server), with an instances manager
  window and URL/invite connect (`packages/desktop/src/main.ts`,
  `preferences.ts`). Parity includes them. `dilla-client` therefore runs
  several independent clients (each with its own `ClientCore`, gateway, data
  directory and keychain entry) in one process; deep links route to the
  matching instance. The instances manager gets parity references shot from
  beta.5 like any other screen.
- **CI.** Desktop CI already runs Linux, macOS and Windows jobs
  (`.github/workflows/desktop.yml`); the GPUI gates run there. Reference
  images are shot from beta.5 on each OS, because each OS rasterises text
  differently; proof 2 sets the per-OS tolerance. The existing pair specs
  are DOM-driven and are rewritten against the automation interface.
- **Noise suppression matches today**: off / standard / DeepFilter
  "enhanced", the same modes the voice e2e suite pins. DeepFilterNet is a
  Rust project, so it links natively; WebRTC APM covers echo cancellation
  and AGC.
- **Push-to-talk stays window-focused**, as today. OS-wide PTT is a new
  feature and waits until after the switch.

## 11. Left to the builder

Crate names and directory layout, async runtime choice inside
`dilla-client`, how state observation maps onto GPUI entities, the automation
interface's wire format, the token codegen tool, how fixtures are seeded for
parity shots, task split inside each phase.

## 11a. Decisions from Jonas during the foundations build (2026-11-12)

- **Linux text spacing follows GPUI's exact (fractional) glyph advances.** Linux
  parity references are shot with Chromium's hinting off
  (`--font-render-hinting=none`); no GPUI patch. Measured: 1.3% differing at 2× vs
  2.4% against hinted Chromium.
- **The native store leaves SQLCipher + OpenSSL for SQLite3 Multiple Ciphers with
  built-in crypto** (the browser already uses it, `chacha20`). Reason: the voice
  proof passed through the LiveKit Rust SDK, but dilla-core's vendored OpenSSL and
  LiveKit's prebuilt libwebrtc (BoringSSL) cannot link into one binary (208
  duplicate symbols). The desktop app stays one process. Existing Bot stores
  (SQLCipher 4) must keep opening (sqlite3mc's SQLCipher-compatible scheme) or get
  a tested migration in the Bot release that ships the change.

## 12. Decisions from Jonas after the builder check

- **The switch makes a new device.** **(Jonas)** No key or history export
  from Electron (its KEK is a non-extractable WebCrypto key in a Chromium
  partition). After the update, a user signs in again and links the device
  from an existing one; older E2EE history shows only as far as today's
  new-device rules allow. Electron still needs one small bridge release so
  its updater (electron-updater NSIS/AppImage, Sparkle on macOS) can install
  the non-Electron app; that release is the only exception to "fixes only",
  and it carries nothing else. The switch notes tell users to keep another
  signed-in device at hand.
- **Voice uses the LiveKit Rust SDK for everything except the frame
  format.** Jonas asked "why not use the SDK?". Connection, tracks, devices
  and simulcast come from the SDK. Encryption stays `dilla-sframe/1`,
  because phones, the web client and the Bot's Go media publisher all speak
  it, and because it binds each frame to the sender's MLS leaf and epoch
  (`protocol/05-media-frames.md`), which LiveKit's built-in FrameCryptor
  format does not. libwebrtc's public `FrameTransformerInterface` is the hook
  LiveKit's own cryptor uses, so the expected cost is a small glue addition
  in the SDK's binding layer (`webrtc-sys`) on LiveKit's prebuilt libwebrtc,
  offered upstream. Proof 1 confirms this. **If it would need our own
  libwebrtc builds, the work stops and Jonas decides.**
