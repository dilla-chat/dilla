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

- Linux PipeWire, Windows WASAPI process-loopback and macOS ScreenCaptureKit
  capture backends, with native process identity checks, bounded audio buffers and
  the existing encrypted screen-share-audio source. Cross-platform implementation
  alone does not satisfy selected-application exclusion acceptance.
- Passkey/OIDC system-browser handoff with short-lived, single-use PKCE completion;
  the renderer never receives browser credentials in a deep link. Password and
  second-factor flows use the bundled client and existing encrypted device store.
- Sparkle native bridge and signed feeds/archives. Actual unsigned-to-unsigned
  installed upgrades passed on both macOS architectures with ad-hoc signatures and
  OS protections intact. Embedded public trust is provisioned; release signing
  keys stay outside CI and ordinary publishing credentials.
- One integrated Signal header, compact sign-in card, native desktop minimum size,
  branded window/dock assets and opaque full-height startup/workspace surfaces.
  The theming infrastructure remains, with Signal as the only supported palette.

## Evidence as of 2026-11-01

The Linux native suite passed 17 tests, including actual encrypted selected-app
capture and exclusion. GitHub's Linux native and voice jobs passed at commit
`0401ed40617e03fc9e16b16c3148b7f14b4d5759`. Client parity is still being completed;
no aggregate all-platform release acceptance is claimed. Local regression checks
also cover actual OS pointer hits on search/window controls, copied text through
the native clipboard boundary, external-browser passkeys, device recovery, signed
moderation recovery after a process restart, and ciphertext class tampering.

Explicit protocol-refusal reloads preserve encrypted draft/file checkpoints while
server web-build hints remain unable to update or reload installed executable UI.
Native quit probes protect active/recovering calls and unsaved drafts. Closing to
tray does not authorize update installation.

Tests exercise real Electron renderers with Chromium sandboxing enabled. The media
validation bundle imports the existing `packages/media/harness/failclosed.ts` and
real WASM; it is excluded from packaged apps. The encrypted control must render and
increment encrypted/decrypted counters. Unbound senders send zero bytes, and a
failed worker stops sending. JSON counter attachments are saved in the Playwright
report. The separate application-audio test uses a real native source and authenticated
encrypted receiver, checks exclusion of unrelated and own-call audio, and rejects
plaintext fallback. Passing the loopback transforms alone never passes that gate.

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
| Bundled shell and encrypted loopback video | Passed locally and CI | Native tests pass; audio preflight blocks | Native CI gates present |
| Real encrypted SFU call; mic/camera permission denial | Voice CI passed; denial coverage open | Voice CI pending | Voice CI pending |
| Selected application audio and own-call exclusion; no plaintext | Actual native gate passed | Implemented; hosted runner has no audio endpoint | Implemented; unrelated-app leakage still blocks launch pending LaunchServices test |
| Persistent sign-in, enrolment and recovery | Actual bundled-client tests pass; full parity open | Full parity pending | Full parity pending |
| Passkey/OIDC system-browser handoff, single-use PKCE completion | Actual passkey ceremony and protocol tests pass | Platform acceptance pending | Platform acceptance pending |
| Background Answer/Decline, ringing, sleep/resume/reconnect | Native ringing actions pass; sleep/resume open | Platform acceptance pending | Platform acceptance pending |
| Multiple instances, permissions and deep links end to end | Partial native acceptance; full gate open | Platform acceptance pending | Platform acceptance pending |
| Installed upgrades: corruption, interruption, disk space, drafts, recovery | Open | Open | Complete fault matrix open |
| Unsigned Sparkle installed upgrade with OS protections intact | N/A | N/A | Actual upgrade passed on both architectures |
| Full client E2E parity in Electron | Remaining adaptations under test | Pending | Pending |
| Startup, memory, idle CPU, interaction and call/audio comparison | Measurement harness under validation | Pending | Pending |

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

## Comparative performance protocol

`node packages/desktop/scripts/run-performance.mjs` runs the browser baseline and
bundled Electron shell sequentially on the same host and bundled UI source. It
uses three fresh processes/profiles per client, headed windows with Chromium
sandboxing and the production UI bundle, and publishes every raw sample with units and environment metadata.
Electron runs unpackaged in this comparison; installer startup and installed-only
update checks need separate installed acceptance. No performance advantage is assumed.

The measurements cover process-launch-to-login readiness (including fixture
attachment), aggregate resident memory, idle CPU, actual search pointer-to-paint
latency, and authenticated remote call frames per second with audible decoded
output and zero plaintext fallback. The two fake microphones use distinct
1000/600 Hz tones with shipping echo cancellation enabled. CPU uses cumulative seconds from public
Chromium/Electron process inventories; memory uses OS resident-set counters.
Shared pages are counted in each process. Call CPU samples start after audible
media and 100 real encrypted sender frames. A process exiting during a window
makes its cumulative CPU accounting incomplete: those windows are published as
discarded, at most three, and persistent churn fails measurement. Browser disk
caches are not controlled. Results describe this harness and host, not a general
claim about all machines or physical audio latency.

CI attaches `performance-<target>.json` and the raw browser/desktop records. The
release verifier requires the matching successful comparison step, source commit,
and authenticated report digest for all four targets before publication.
