# GPUI desktop — proofs and foundations Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prove the three hard risks and build the four-layer foundation of the native desktop client, ending in a vertical slice (sign in, channel list, read and send a message) rendered in the Signal design and checked against beta.5.

**Architecture:** Four new Rust workspace crates under `clients/desktop/`: `dilla-client` (headless transport, sync and state over `dilla_core::client::ClientCore`), `dilla-media` (LiveKit Rust SDK with `dilla-sframe/1`), `dilla-desktop-ui` (Signal design system on GPUI), `dilla-desktop` (the app). Each task in a wave owns disjoint files so the wave runs in parallel worktrees.

**Tech Stack:** Rust 1.98.1 (edition 2024), `gpui-pre =0.3.12`, `gpui-component =0.9.1`, `livekit =0.10.1`, tokio, reqwest (rustls), tokio-tungstenite (rustls), `keyring`, `zeroize`, SQLCipher through dilla-core's vendored `libsqlite3-sys`.

**Spec:** `docs/superpowers/specs/2026-11-12-gpui-desktop-design.md`

**Follow-on plans (not this plan):** screens by area, native integrations (tray, updater, deep links, passkeys, capture picker, notifications, packaging), Electron bridge release, switch.

## Global Constraints

- No server, protocol or wire change. Every frame and route matches what `packages/client-core` sends today; the negotiated versions are exactly those in `packages/client-core/src/gateway/gateway.ts`.
- Voice frames are `dilla-sframe/1` (`protocol/05-media-frames.md`), built only from `dilla_core::sframe`. If any task finds this needs our own libwebrtc build: stop and report; Jonas decides.
- Receivers never fetch link previews (`protocol/04`).
- Keys at rest: the SQLCipher key is held only in the OS keychain (service `dev.dilla.desktop.preview`, account `instance-<n>`), in `Zeroizing` buffers; no plaintext fallback, ever.
- Preview identity: app id `dev.dilla.desktop.preview`, product name `Dilla Preview`, data dir `<OS data dir>/Dilla Preview/instances/<n>`; never touch the Electron app's `dev.dilla.desktop` data.
- At most 32 instances; instance ids are `0..=31`.
- Pinned exactly: `gpui-pre =0.3.12`, `gpui-component =0.9.1`, `livekit =0.10.1`. A bump is its own PR.
- No GPL code. Zed's call/capture code may be read, never copied. `cargo deny check licenses` stays green.
- `dilla-client` and `dilla-desktop-ui` have `#![forbid(unsafe_code)]`; `dilla-media` may use `unsafe` only inside its FFI module.
- The UI uses GPUI's builder style (`div().flex().gap_2()…`); colours, sizes and fonts come only from the generated tokens, never literals.
- Every push runs `npm run ci:local` first.
- Electron (`packages/desktop`, `packages/web`, `packages/ui`) gets no changes from this plan.

## Review Focus

1. Gateway drops while the laptop sleeps → on wake the client resumes and the timeline has no gap and no duplicate (Task 9 test `resume_after_sleep_no_gap_no_dup`).
2. No OS keychain available (Linux without Secret Service) → sign-in fails with a clear `StoreError::KeychainUnavailable`, never a plaintext key file (Task 6 test `no_keychain_is_an_error_not_a_fallback`).
3. Two instances on the same server with different accounts → separate stores, keychain entries and gateways (Task 6 test `instances_are_isolated`).
4. Call MLS epoch advances mid-call → audio keeps flowing both ways (Task 2 test `epoch_change_mid_stream`).
5. Display scale 1.0, 2.0 and Linux fractional 1.25 → the parity harness compares at the reference image's scale and fails on any mismatch, not silently rescales (Task 10 test `scale_mismatch_fails`).

## Execution shape

- **Wave A (sequential):** Task 1.
- **Wave B (parallel, one worktree each, branched from Wave A):** Tasks 2, 3, 4, 5, 6, 7, 8, 10.
- **Wave C (parallel):** Task 9 (after 6, 7, 8 merge) and Task 11 (after Task 2's verdict is PASS).
- **Wave D (sequential):** Task 12.
- Merge each task into `feat/gpui-desktop` one at a time, rebasing the next on it. Proof tasks (2, 3, 4) write a `REPORT.md` with a verdict line; the controller reads it before Wave C.

---

### Task 1: Workspace scaffold, pinned dependencies, CI

**Files:**
- Modify: `Cargo.toml` (workspace members)
- Create: `clients/desktop/{dilla-client,dilla-media,dilla-desktop-ui,dilla-desktop}/Cargo.toml` and `src/lib.rs` (`src/main.rs` for `dilla-desktop`)
- Create: `clients/desktop/dilla-client/src/seams.rs`
- Create: `.github/workflows/desktop-gpui.yml`
- Modify: `scripts/ci-local.mjs` (add the GPUI crates' check, clippy and test on Linux)
- Modify: `deny.toml` only if a new licence must be allowed (Apache-2.0/MIT/BSD/ISC/Zlib/Unicode only)

**Interfaces:**
- Produces, in `dilla_client::seams` (later tasks implement these and must not change their signatures without the controller):

```rust
pub struct InstanceId(pub u8); // 0..=31
pub struct InstanceConfig { pub id: InstanceId, pub server: url::Url, pub data_dir: std::path::PathBuf }

pub trait Keychain: Send + Sync {
    fn get(&self, account: &str) -> Result<Option<zeroize::Zeroizing<Vec<u8>>>, KeychainError>;
    fn set(&self, account: &str, secret: &[u8]) -> Result<(), KeychainError>;
    fn delete(&self, account: &str) -> Result<(), KeychainError>;
}

pub trait Route { const METHOD: http::Method; const PATH: &'static str; type Request: serde::Serialize + Send; type Response: serde::de::DeserializeOwned + Send; }
#[async_trait::async_trait]
pub trait Http: Send + Sync { async fn call<R: Route>(&self, path_args: &[&str], req: R::Request) -> Result<R::Response, HttpError>; }

pub enum GatewayEvent { Connected { session: Vec<u8> }, Frame(Vec<u8>), Disconnected { reason: DisconnectReason } }
#[async_trait::async_trait]
pub trait Gateway: Send + Sync {
    async fn send(&self, frame: Vec<u8>) -> Result<(), GatewayError>;
    fn events(&self) -> tokio::sync::mpsc::Receiver<GatewayEvent>;
}
```

- Produces: `dilla-desktop` binary opens one empty 1280×800 window titled `Dilla Preview` and exits cleanly on close.

- [ ] **Step 1:** Add the four crates as workspace members (not `default-members`), with the pins from Global Constraints. Add `#![forbid(unsafe_code)]` where required.
- [ ] **Step 2:** Write `seams.rs` exactly as above, plus `KeychainError`, `HttpError`, `GatewayError`, `DisconnectReason` as `thiserror` enums with the variants the later tasks name (`KeychainError::Unavailable`, `HttpError::{Status(u16), Transport(String), Decode(String)}`, `GatewayError::Closed`, `DisconnectReason::{Network, Server(u16), Shutdown}`).
- [ ] **Step 3:** Test `dilla-desktop/tests/window.rs::opens_and_closes`, using GPUI's `TestAppContext`: open the main window and assert its title is `"Dilla Preview"`; close it and assert no window remains.
- [ ] **Step 4:** `.github/workflows/desktop-gpui.yml`: `cargo build`, `clippy -D warnings` and `test` for the four crates on `ubuntu-24.04`, `windows-latest` and `macos-15` (the same matrix as `desktop.yml`), on PRs touching `clients/desktop/**` or `core/**`. On Linux, install the GPUI build dependencies through `scripts/ci-apt-install.sh`.
- [ ] **Step 5:** Run `npm run ci:local`. Expected: green, including the new crates.
- [ ] **Step 6:** Commit with the message `gpui: workspace scaffold, seams and CI`.

### Task 2: Proof — voice interop with `dilla-sframe/1`

**Files:**
- Create: `experiments/gpui-voice-proof/` (Cargo crate, test, `REPORT.md`)
- Create, only if needed: `core/vendor/webrtc-sys/` (patched copy, documented in `core/vendor/README.md` like `libsqlite3-sys`)

**Interfaces:**
- Consumes: `dilla_core::sframe` (`SframeSender`, `KeyRing::install_epoch`, `protect`, `open_frame`) and the browser receiver in `core/dilla-core-wasm/src/media.rs` (`MediaSender`, `MediaReceiver`).
- Produces: `REPORT.md`, whose first line is one of `VERDICT: PASS`, `VERDICT: PASS-WITH-GLUE <path>` or `VERDICT: STOP-NEEDS-LIBWEBRTC`, followed by the transformer API used and the measured latency.

- [ ] **Step 1:** Run a local LiveKit server the way `e2e/voice/support/voice.ts` does. Test `rust_to_browser_audio`:
  - a Rust `livekit` participant publishes a 440 Hz Opus tone, every frame passing through a frame transformer that applies `dilla_core::sframe` with epoch 1 and base key `[7u8; 16]`, roster `[(0, [1u8; 16]), (1, [2u8; 16])]`, own leaf 0;
  - a Playwright Chromium page subscribes using `MediaReceiver` with the same epoch and key;
  - assert the decoded audio has a 440 Hz peak (FFT) and the receiver counts zero decrypt failures.
- [ ] **Step 2:** Test `browser_to_rust_audio`: the same in reverse; the Rust side's `open_frame` counts at least 100 frames opened and 0 failed.
- [ ] **Step 3:** Test `epoch_change_mid_stream`: after 2 s, both sides install epoch 2 (base key `[9u8; 16]`) and the sender switches. Assert audio continues, there are at most 10 dropped frames, and 0 frames are accepted under a wrong key.
- [ ] **Step 4:** Test `video_vp8_rust_to_browser`: a 640×360 VP8 test pattern reaches the page with non-zero decoded frames.
- [ ] **Step 4b:** Measure 1920×1080 at 30 fps VP8 remote video drawn in a GPUI window on each OS (through `gpui-proofs.yml`):
  - record the main-thread CPU % and dropped frames over 60 s in `REPORT.md`, as `VIDEO <os> <cpu%> <dropped>`;
  - over 50% CPU or over 5% dropped frames marks that OS `VIDEO-NEEDS-WORK`, which becomes its own follow-on task and does not stop the plan.
- [ ] **Step 5:** Find the hook in this order:
  1. a frame transformer already exposed by `livekit`/`webrtc-sys`;
  2. a glue addition implementing libwebrtc's `FrameTransformerInterface` in a vendored `webrtc-sys`, on LiveKit's prebuilt libwebrtc;
  3. otherwise stop and write `VERDICT: STOP-NEEDS-LIBWEBRTC`.
- [ ] **Step 6:** Write `REPORT.md` and commit with the message `gpui proof: voice interop`.

### Task 3: Proof — pixel fidelity and tolerance

**Files:**
- Create: `experiments/gpui-pixel-proof/` (crate, fixture, `REPORT.md`)
- Create: `.github/workflows/gpui-proofs.yml` (`workflow_dispatch`, the 3-OS matrix, uploads images)

**Interfaces:**
- Consumes: `packages/design-tokens/src/tokens.ts` values, read by hand for this proof only.
- Produces: `REPORT.md` with a per-OS table of `max channel delta`, `% pixels differing` and the proposed tolerance, written as `TOLERANCE <os> <max_delta> <max_pct>`. Task 10 reads these lines.

- [ ] **Step 1:** Fixture: 12 messages from 3 authors, 1 code block and 1 reaction row at 1280×800, scale 1.0 and 2.0. Render it in beta.5 with the existing parity tooling. Render the same layout in GPUI with JetBrains Mono Variable at the base size of 13.5px, 1px borders, the `bg`/`surface`/`fg` tokens and radius 8.
- [ ] **Step 2:** Diff per OS. Record the numbers, and crops of the worst regions as PNGs.
- [ ] **Step 3:** Write `REPORT.md`. If the text cannot get within 2% of pixels differing at scale 2.0 on any OS, mark that OS with `NEEDS-JONAS`. Commit with the message `gpui proof: pixel fidelity`.

### Task 4: Proof — composer text input

**Files:**
- Create: `experiments/gpui-input-proof/` (crate, `REPORT.md`, `MANUAL.md`)

**Interfaces:**
- Produces: `REPORT.md`, with automated results and a `VERDICT:` line; `MANUAL.md`, a 10-minute IME checklist for Jonas (macOS Japanese, Windows Chinese Pinyin, Linux fcitx5 on Wayland and X11).

- [ ] **Step 1:** A multi-line composer on the gpui-component `Input`. Tests, using the GPUI test input simulation:
  - `enter_sends_shift_enter_newlines`
  - `dead_key_e_acute` (`´` then `e` gives `é`)
  - `paste_multiline_keeps_newlines`
  - `emoji_insert_at_caret`
  - `mention_trigger_opens_popup_at_at_sign`
- [ ] **Step 2:** Write `MANUAL.md`, then `REPORT.md`. Use `VERDICT: PASS` only if the automated tests pass; IME stays `PENDING-MANUAL` until Jonas runs the checklist. Commit with the message `gpui proof: composer input`.

### Task 5: Design system tokens, fonts and icons

**Files:**
- Create: `scripts/gen-gpui-tokens.mjs` (reads `packages/design-tokens/src/tokens.ts` and `packages/ui/src/Icon/icons.ts`)
- Create: `clients/desktop/dilla-desktop-ui/src/tokens.rs` (generated, with a `// @generated` header)
- Create: `clients/desktop/dilla-desktop-ui/assets/icons/*.svg` (generated)
- Create: `clients/desktop/dilla-desktop-ui/assets/fonts/` (JetBrains Mono Variable, DM Serif Display Italic, copied from `node_modules/@fontsource*`, with their licence files)
- Create: `clients/desktop/dilla-desktop-ui/src/{theme.rs,icon.rs,components/{button.rs,avatar.rs,badge.rs,row.rs}}`

**Interfaces:**
- Produces:
  - `tokens::color::*` constants (`gpui::Hsla`), such as `tokens::color::BG`, `SURFACE`, `FG`, `ACCENT`, `BRAND`, `DANGER`, `WARN`, `OK`;
  - `tokens::text::*` (`Pixels`, the 26 steps, `BASE` = 13.5px);
  - `tokens::radius::*`;
  - `tokens::layout::{RAIL, SIDEBAR, MEMBERS, THREAD, BAR, USER_PANEL, ROW}` = 56, 244, 232, 340, 44, 64 and 28px;
  - `theme::install(cx: &mut App)`, which registers the fonts;
  - `Icon::new(IconName::X)`, where `IconName` is generated, one variant per icon;
  - the components `Button`, `Avatar`, `Badge` and `Row`, built with `RenderOnce`.

- [ ] **Step 1:** Test `scripts/gen-gpui-tokens.test.mjs`: the generator's output matches the committed `tokens.rs`. This fails when either one is stale, and the test goes into `ci:local`.
- [ ] **Step 2:** Test `tokens.rs::matches_css`: `BG` equals `#08090B`, `ACCENT` equals `#5AA8E6`, and `text::BASE` equals 13.5px.
- [ ] **Step 3:** Test `icon_count_is_100`: `IconName::ALL.len() == 100`.
- [ ] **Step 4:** Build the four components to match their `packages/ui` counterparts. Use only tokens, and give every component a stable `id` for automation.
- [ ] **Step 5:** Run `cargo test -p dilla-desktop-ui` and the generator test. Commit with the message `gpui: Signal tokens, fonts, icons, base components`.

### Task 6: Instances, store and keychain

**Files:**
- Create: `clients/desktop/dilla-client/src/{instances.rs,store.rs,keychain.rs}`
- Test: `clients/desktop/dilla-client/tests/store.rs`

**Interfaces:**
- Consumes: `seams::{InstanceId, InstanceConfig, Keychain}`; `dilla_core::mls::ConnHandle` and `ClientCore::open` (see `core/dilla-bot-core/src/store.rs` for opening a native SQLCipher connection).
- Produces:
  - `OsKeychain` (the `keyring` crate, service `dev.dilla.desktop.preview`) and `MemoryKeychain` (tests);
  - `Instances::load(root: &Path) -> Result<Instances, StoreError>`, with `add(server: Url) -> Result<InstanceConfig, StoreError>` (fails past 32), `remove(id)` and `list()`;
  - `open_store(cfg: &InstanceConfig, kc: &dyn Keychain) -> Result<ClientCore, StoreError>`, which creates a 32-byte random key in the keychain on first open.

- [ ] **Step 1:** Test `first_open_creates_key_and_reopens`: two opens with a `MemoryKeychain` give the same data, and the keychain holds exactly 1 entry `instance-0`, of 32 bytes.
- [ ] **Step 2:** Test `no_keychain_is_an_error_not_a_fallback`: a keychain whose `get` returns `KeychainError::Unavailable` makes `open_store` return `StoreError::KeychainUnavailable`, and the data dir holds no file other than the encrypted database.
- [ ] **Step 3:** Test `instances_are_isolated`: two instances on the same server URL get different `data_dir`s and keychain accounts, and a message stored through one is invisible to the other.
- [ ] **Step 4:** Test `thirty_third_instance_refused`.
- [ ] **Step 5:** Test `wrong_key_does_not_open`: tamper with the keychain value, and assert `open_store` fails with `StoreError::Locked`.
- [ ] **Step 6:** Implement, run `cargo test -p dilla-client --test store`, and commit with the message `gpui client: instances, SQLCipher store, OS keychain`.

### Task 7: HTTP client

**Files:**
- Create: `clients/desktop/dilla-client/src/http/{mod.rs,routes.rs}`
- Test: `clients/desktop/dilla-client/tests/http.rs`

**Interfaces:**
- Consumes: `seams::{Http, Route, HttpError}`.
- Produces: `ReqwestHttp::new(base: Url, auth: AuthToken)`, and one `Route` type per route in `packages/client-core/src/http/routes.ts`. Same names in snake_case, same paths, and the same body encoding (CBOR or JSON, matching each TS route).

- [ ] **Step 1:** Test `route_table_matches_ts`: a test script dumps `routes.ts` (method, path template, encoding) to JSON. The Rust test asserts that every entry has a Rust `Route` with the same method, path and encoding, and that there are no extras.
- [ ] **Step 2:** Test `sign_in_round_trip`: against a real dillad started by `testkit`, password and TOTP sign-in returns a session. Use the test account fixture the web e2e uses.
- [ ] **Step 3:** Test `error_status_maps`: a 403 gives `HttpError::Status(403)`, and a dropped connection gives `HttpError::Transport`.
- [ ] **Step 4:** Implement, test, and commit with the message `gpui client: HTTP routes`.

### Task 8: Gateway client

**Files:**
- Create: `clients/desktop/dilla-client/src/gateway/{mod.rs,frames.rs,reconnect.rs}`
- Test: `clients/desktop/dilla-client/tests/gateway.rs`

**Interfaces:**
- Consumes: `seams::{Gateway, GatewayEvent, GatewayError, DisconnectReason}`; `packages/client-core/src/gateway/{gateway,frames,requests}.ts` as the behavioural reference; `protocol/vectors/*.json`.
- Produces:
  - `WsGateway::connect(base: Url, auth: AuthToken) -> WsGateway`, with heartbeat at the server-given `heartbeatMs`, the same `maxFrameBytes` limit, and the same backoff as `gateway.ts`;
  - `frames::{encode, decode}` for every frame type in `frames.ts`.

- [ ] **Step 1:** Test `vectors_round_trip`: every gateway vector in `protocol/vectors/` decodes and re-encodes byte-identically.
- [ ] **Step 2:** Test `hello_negotiates_same_versions_as_ts` against a real dillad.
- [ ] **Step 3:** Test `heartbeat_timeout_reconnects`: a server that stops answering gives `Disconnected{Network}`, then `Connected`, within the backoff `gateway.ts` uses.
- [ ] **Step 4:** Test `oversized_frame_refused`: a frame over `maxFrameBytes` is refused before it is sent.
- [ ] **Step 5:** Implement, test, and commit with the message `gpui client: gateway`.

### Task 9: Sync engine and state model

**Files:**
- Create: `clients/desktop/dilla-client/src/{client.rs,state/{mod.rs,channels.rs,messages.rs,members.rs,unread.rs},sync/{mod.rs,timeline.rs}}`
- Test: `clients/desktop/dilla-client/tests/sync.rs`

**Interfaces:**
- Consumes: Tasks 6, 7 and 8; `ClientCore` (receive, timeline, send); `packages/client-core/src/{sync,state}/*` as the reference.
- Produces:
  - `Client::start(cfg: InstanceConfig, kc: Arc<dyn Keychain>) -> Result<Client, ClientError>`, which runs on its own tokio runtime;
  - `Client::state(&self) -> tokio::sync::watch::Receiver<Arc<AppState>>`;
  - `Client::send_text(&self, channel: ChannelId, text: String) -> oneshot::Receiver<Result<MessageId, ClientError>>`;
  - `AppState { channels: Vec<Channel>, timelines: HashMap<ChannelId, Timeline>, members, unread }`, with types mirroring the TS state slices by name.

- [ ] **Step 1:** Test `two_devices_exchange_text`: two `Client`s against a real dillad, in one channel. A sends `"hello from gpui"`, and B's timeline shows it with A as the author within 5 s.
- [ ] **Step 2:** Test `interop_with_ts_client`: a TS client (the web e2e harness, headless) and a Rust `Client` exchange one message each way.
- [ ] **Step 3:** Test `resume_after_sleep_no_gap_no_dup`: B's gateway is dropped for 30 s while A sends 5 messages. After the reconnect, B's timeline has exactly those 5, in order, with no duplicates.
- [ ] **Step 4:** Test `unread_counts`: 3 messages arrive in an unfocused channel, so unread is 3; marking it read gives 0.
- [ ] **Step 5:** Implement, test, and commit with the message `gpui client: sync and state`.

### Task 10: Automation interface and parity harness

**Files:**
- Create: `clients/desktop/dilla-desktop/src/automation.rs` (behind cargo feature `automation`, never in release builds)
- Create: `clients/desktop/parity/` (crate `dilla-parity`: driver, image diff, reference loader)
- Test: `clients/desktop/parity/tests/harness.rs`

**Interfaces:**
- Consumes: the `TOLERANCE` lines from Task 3's `REPORT.md`; the beta.5 reference shots in `docs/user/screenshots`.
- Produces:
  - `Driver::launch(instance_fixture: &Path) -> Driver`, with `click(id)`, `type_text(id, &str)`, `element(id) -> Option<ElementInfo>` and `screenshot() -> RgbaImage`;
  - `assert_parity(actual: &RgbaImage, reference: &Path, os: Os) -> Result<(), ParityFailure>`.

- [ ] **Step 1:** Test `release_build_has_no_automation`: `cargo build --release -p dilla-desktop`, then assert that the binary has no `automation` symbol.
- [ ] **Step 2:** Test `identical_images_pass` and `one_pixel_over_tolerance_fails`.
- [ ] **Step 3:** Test `scale_mismatch_fails`: a 2.0-scale capture compared against a 1.0 reference gives `ParityFailure::Scale`, with no rescale.
- [ ] **Step 4:** Test `click_and_read_element`: drive the Task 1 window and read its root element's id and bounds.
- [ ] **Step 5:** Implement, test, and commit with the message `gpui: automation interface and parity harness`.

### Task 11: `dilla-media` call join

**Files:**
- Create: `clients/desktop/dilla-media/src/{lib.rs,transform.rs,room.rs,devices.rs,ffi.rs}`
- Test: `clients/desktop/dilla-media/tests/call.rs`

**Interfaces:**
- Consumes: Task 2's hook and its `REPORT.md`; `ClientCore` call-group epoch secrets (`client/calls.rs`).
- Produces:
  - `Call::join(token: LiveKitToken, keys: Arc<dyn EpochKeys>) -> Result<Call, MediaError>`;
  - `Call::{set_mic, set_muted, leave}`;
  - `Call::remote_audio_levels() -> watch::Receiver<…>`;
  - `devices::{inputs, outputs}`;
  - `trait EpochKeys { fn on_epoch(&self, f: Box<dyn Fn(u64, [u8;16], Vec<(u16,[u8;16])>, u16) + Send + Sync>); }`.

- [ ] **Step 1:** Promote Task 2's transformer into `transform.rs`. Tests 1 to 3 from Task 2 move here, using `ClientCore`-derived keys instead of fixed ones.
- [ ] **Step 2:** Test `desktop_and_web_in_one_call`: through dillad's real call flow, a Rust `Call` and a web client hear each other's tones.
- [ ] **Step 3:** Test `noise_modes_available`: `off`, `standard` and `enhanced` (DeepFilterNet) each process a noise fixture, and `enhanced` lowers the noise by at least 10 dB. The values come from `e2e/voice/enhanced-noise.spec.ts`.
- [ ] **Step 4:** Implement, test, and commit with the message `gpui media: call join with dilla-sframe/1`.

### Task 12: Vertical slice

**Files:**
- Create: `clients/desktop/dilla-desktop/src/{app.rs,screens/{sign_in.rs,shell.rs,channel_list.rs,timeline.rs,composer.rs}}`
- Test: `clients/desktop/dilla-desktop/tests/slice.rs`

**Interfaces:**
- Consumes: `Client` (Task 9), the components and tokens (Task 5), the composer approach (Task 4), and `Driver`/`assert_parity` (Task 10).
- Produces: a runnable preview app in which you can sign in with password and TOTP, see servers and channels, read, and send.

- [ ] **Step 1:** Test `slice_sign_in_read_send`, driving the app:
  - sign in;
  - open `#general`;
  - type `hello from the preview`;
  - press Enter;
  - assert that a second, headless `Client` receives it.
- [ ] **Step 2:** Test `slice_parity_channel_view`: the channel view with the Task 3 fixture passes `assert_parity` against the matching beta.5 screenshot on the current OS.
- [ ] **Step 3:** Run the app manually with `cargo run -p dilla-desktop` against a local dillad. Attach screenshots to the PR.
- [ ] **Step 4:** Commit with the message `gpui: vertical slice`. Open the PR into `main` once the 3-OS workflow is green.
