# Desktop platform findings and launch gates

Status: **NO-GO for desktop-only launch**. The approved 2026-10-31 plan replaces the
2026-10-21 Linux-first/server-loaded plan. Linux, Windows and macOS must be accepted
together; mobile, package-manager distributions and global push-to-talk are excluded.

## Implemented foundation

- Electron 45.2.1 shell, bundled React/core/media, sandbox/context isolation, typed
  and validated preload commands, frame ownership checks, navigation/resource locks,
  strict CSP and verified bundled-asset manifest. Encryption keys remain in the core
  worker. Per-URL partitions preserve scheme, host, port and path identity.
- Instance chooser and native unfamiliar-host confirmation; one window per instance;
  persistent non-secret shell preferences; tray/close-to-tray, optional autostart,
  native notification routing and unread badges, strict invite parsing.
- Authenticated update descriptors and hashes, fixed stable/beta GitHub feeds,
  startup/four-hour polling in installed builds, manual checking, retryable errors,
  background download and quit-only installation guards. Cache hashes are checked
  again immediately before installation; corrupt cache entries are evicted. Linux
  stages replacement on the destination filesystem with a retained working backup.
- AppImage, per-user NSIS and ad-hoc macOS packaging configuration. Hardened packaged
  fuses are checked on the binary. CI adds all four native targets without deleting
  any browser job. Validation does not publish artifacts as desktop releases.

The empty release public-key list, missing application audio, missing browser auth
handoff and missing Sparkle bridge are deliberate visible blockers, not supported
capabilities or substitutes for the approved design.

## Evidence from this Linux session

Final local checks passed: 16 desktop unit tests; 8 Electron tests including the
real subpath WebSocket handshake; shared core/web/desktop typechecks; 348 CI-checker
tests; a Linux unpacked package and verification of its actual fuse wire. The shared
workspace suite also runs before handoff. Windows/macOS matrix jobs have not been
executed in this session.

Tests exercise real Electron renderers with Chromium sandboxing enabled. The media
validation bundle imports the existing `packages/media/harness/failclosed.ts` and
real WASM; it is excluded from packaged apps. The encrypted control must render and
increment encrypted/decrypted counters. Unbound senders send zero bytes, and a
failed worker stops sending. JSON counter attachments are saved in the Playwright
report. These are loopback video-transform tests, **not** an SFU call or application
audio acceptance.

The shell tests use a hostile HTTP loopback server returning executable HTML. The
installed client still boots, no script/document is fetched from that server,
malformed IPC is rejected, prohibited navigation retains the existing document,
reload keeps storage, and closing a window keeps the renderer alive. Unit tests
cover tampered signatures/hashes, wrong channels/targets/URLs, expired metadata,
download and installation failures, call/draft guards and AppImage rollback.
Subpath configuration is delivered to the core worker before boot, constrained to
the same origin, so the gateway uses the full base path before creating a socket.
The executable `/assets/` namespace is reserved for installed bytes, even when an
instance attempts to use a conflicting base path.

Run the validation with an actual display (or Xvfb on a Linux CI runner):

```sh
npm run build -w @dilla/desktop
npm run build:validation -w @dilla/desktop
npm test -w @dilla/desktop
npm run typecheck -w @dilla/desktop
npm run test:electron -w @dilla/desktop
npm run pack -w @dilla/desktop
node packages/desktop/scripts/check-fuses.mjs
```

Headless Ozone is not a substitute for a display: native GTK/tray setup failed in
that mode. The test launcher explicitly uses `chromiumSandbox: true`; the default
Playwright Electron launcher otherwise disables the sandbox on Linux. The CI runner
installs the standard setuid Chromium sandbox helper instead of disabling OS
protections. No physical microphone, camera or selected application is simulated as
passing a hardware gate.

## Mandatory acceptance, still open

| Gate | Linux x64 | Windows x64 | macOS Intel / Apple Silicon |
|---|---|---|---|
| Bundled shell and encrypted loopback video | Locally exercised | CI configured, not run in this session | CI configured, not run in this session |
| Real encrypted SFU call; mic/camera permission denial | Open | Open | Open |
| PipeWire / WASAPI / ScreenCaptureKit selected application audio | Not implemented | Not implemented | Not implemented |
| Listener receives selected app and excludes call audio; no plaintext | Open | Open | Open |
| Persistent sign-in, enrolment and recovery with bundled client | Open | Open | Open |
| Passkey/OIDC system-browser handoff, single-use PKCE completion | Not implemented | Not implemented | Not implemented |
| Background notification Answer/Decline, ring, sleep/resume/reconnect | Open | Open | Open |
| Multiple live instances, permissions and deep links end to end | Open | Open | Open |
| Installed upgrades: corruption, interruption, disk space, drafts, recovery | Open | Open | Open |
| Unsigned Sparkle installed upgrade with OS protections intact | N/A | N/A | **Not implemented; blocks launch** |
| Full client E2E parity in Electron | Open | Open | Open |
| Startup, memory, idle CPU, latency and call/audio comparison to browser | Not measured | Not measured | Not measured |

The code review in this session is an implementation review. It does not satisfy the
required independent security review of the completed native application.

After every gate passes, prepare migration instructions and download routes while
preserving API/WSS, authentication handoff, invite routes and existing browser device
data. Only then may browser visits become downloads. Browser CI can be removed only
after equivalent Electron coverage exists. No server rollout or browser retirement
has been made here.

## References checked during implementation

- [Electron protocol/session API](https://www.electronjs.org/docs/latest/api/protocol)
  supports per-session handling; network forwarding bypasses the custom handler.
- [Electron security](https://www.electronjs.org/docs/latest/tutorial/security)
  covers sandboxing, frame validation and navigation restrictions.
- [Sparkle documentation](https://sparkle-project.org/documentation/) describes
  EdDSA archive signatures and signed feeds. Documentation alone does not prove the
  required unsigned Electron upgrade.
- [GitHub hosted runners](https://docs.github.com/en/actions/reference/runners/github-hosted-runners)
  distinguishes `macos-15-intel` from the Apple Silicon `macos-15` runner.
