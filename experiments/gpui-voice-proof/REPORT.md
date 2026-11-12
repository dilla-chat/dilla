VERDICT: PASS-WITH-GLUE core/vendor/webrtc-sys

# Proof: desktop voice on LiveKit's Rust SDK with `dilla-sframe/1`

Date: 2026-11-12. livekit `=0.10.1`, webrtc-sys `=0.3.53` (vendored and patched), LiveKit's prebuilt
libwebrtc as webrtc-sys downloads it (`linux-x64-release-webrtc-a545de4`, not rebuilt), Chromium
from Playwright (`chromium` channel), dilla-testhost's in-process LiveKit.

**Transformer API used:** libwebrtc's `webrtc::FrameTransformerInterface`, implemented by
`livekit_ffi::DillaFrameTransformer` (core/vendor/webrtc-sys/src/dilla_transform.cpp) and set with
`RtpSenderInterface::SetEncoderToPacketizerFrameTransformer` /
`RtpReceiverInterface::SetDepacketizerToDecoderFrameTransformer`. Rust sees it as
`webrtc_sys::dilla_transform::{install, FrameTransform, FrameInfo, Verdict}`; the transform itself
is `src/media.rs`, built only from `dilla_core::sframe` (`SframeSender::encrypt`,
`KeyRing::install_epoch`, `KeyRing::decrypt`, with `protect` / `open_frame` underneath).

**Measured latency** (time inside the transform per frame, release build, this machine):

| path | p50 | p99 | max |
|---|---|---|---|
| Opus send (seal) | 2–3 µs | 5–6 µs | 8 µs |
| Opus receive (open) | 3 µs | 9–12 µs | 16 µs |
| VP8 640×360 send | 4 µs | 13 µs | 13 µs |
| VP8 1920×1080 send | 9 µs | 14 µs | 26 µs |
| VP8 1920×1080 receive | 11 µs | 21 µs | 44 µs |

First authenticated audio on the page came 3.1–3.2 s after the Rust participant started connecting;
that is connection and subscription time (the SFU's subscriber update interval, protocol/05
"Rotation"), not the transform.

## Results

All four interop tests pass (`browser/voice-proof.spec.ts`, run with debug and with release Rust
builds). Keys exactly as the brief: epoch 1, base key `[7u8; 16]`, roster `[(0, [1u8; 16]), (1,
[2u8; 16])]`; the Rust participant is leaf 0 (identity `0101…01`), the page leaf 1 (`0202…02`);
epoch 2 has base key `[9u8; 16]`. The page runs the real `DillaE2EEManager` and media worker with
`dilla-core-wasm`'s `MediaSender` / `MediaReceiver` (packages/media harness, `e2ee: 'dilla'`).

| test | result |
|---|---|
| `rust_to_browser_audio` | page authenticated 210 of 212 sealed frames under KID `1` (the rest in flight), all bound to device `0101…01`; AnalyserNode peak **439.45 Hz** (bin width 2.93 Hz), −33.7 dB against a −161 dB median floor; **0** drops for every reason (`aeadFail`, `parse`, `replay`, … all 0); Rust side 0 failures |
| `browser_to_rust_audio` | Rust `KeyRing` opened **219** frames under KID `101`, **0** failed; decoded peak 1000 Hz in every second (the page's fake microphone is the media suite's 1 kHz tone) |
| `epoch_change_mid_stream` | after 2 s both directions moved to epoch 2 (the Rust ring installs, then the page installs and its sender switches, then the Rust sender rekeys); page authenticated `{1: 119, 2: 216}`, Rust opened `{101: 114, 102: 216}`; **0** dropped frames on each side (limit 10); peak 439.45 Hz on the page and 1000 Hz in Rust after the switch; **0** frames accepted under a wrong key (no `aeadFail` / `E_SFRAME_AUTH`, and the only authenticated KIDs are each sender's own epoch-1 and epoch-2 KIDs; the unit test `an_epoch_change_holds_until_installed_and_never_accepts_a_wrong_key` installs epoch 2 under `[8u8; 16]` and opens none of its frames) |
| `video_vp8_rust_to_browser` | 640×360 VP8 test pattern: the page decoded **50** frames (1 key frame, 0 PLI) at 640×360, all 50 authenticated under KID `1`, 0 drops |

RED evidence: with the hook not installed (`VOICE_PROOF_NO_HOOK=1`: the same participant on stock
LiveKit E2EE) `rust_to_browser_audio` and `browser_to_rust_audio` both fail, 0 frames authenticated
in 30 s in either direction. The transform's unit tests went RED first against a transform that
answered `Drop` to everything (9 of the 10 failed; the tenth asserts drops), then GREEN.

Unit tests: `cargo test --lib` 83 passed: the 12 proof tests plus dilla-core's own 71 sframe tests,
including the `protocol/vectors/sframe.json` runner, compiled from the same source files.

## Step 4b: 1920×1080 at 30 fps in a GPUI window

```
VIDEO linux NOT-MEASURED NOT-MEASURED
VIDEO macos PENDING-CI PENDING-CI
VIDEO windows PENDING-CI PENDING-CI
```

Linux could not be measured on this machine: the desktop session was locked
(`loginctl show-session 2`: `LockedHint=yes`), and the compositor sends no frame callbacks to a
window on a locked session (Wayland and XWayland alike), so GPUI rendered once and never again
(`renders: 1, painted: 0` over 60 s). Everything before the window worked over the same 60 s: the
release `video-window` received and authenticated **1800 of 1800** expected 1080p frames (KID `1`,
0 failures, open p50 11 µs) at 16 % process CPU (decode, conversion, transform). No headless
compositor (Xvfb, weston, sway) is installed here. The measurement is the
`video_1080p_gpui_window` test (`VOICE_PROOF_WINDOW=1`); the CI job below runs it on all three
OSes, Linux under Xvfb. Per the brief, over 50 % CPU or over 5 % dropped marks that OS
`VIDEO-NEEDS-WORK` (its own follow-on task) and does not stop the plan.

The window draws through `img(Arc<RenderImage>)`: libyuv converts each decoded I420 frame to BGRA
off the main thread; the main thread uploads it as a new atlas texture and drops the previous one.
On macOS a `surface()` over a `CVPixelBuffer` would avoid the upload; the measurement decides
whether that is needed.

## Hook search (brief Step 5)

1. **A transformer already exposed by `livekit` / `webrtc-sys`: none usable.** `webrtc-sys` binds
   two transformers and no generic one: `FrameCryptor` (libwebrtc's `FrameCryptorTransformer`,
   LiveKit's own AES-GCM format with an IV and key-index trailer, keyed through `KeyProvider`; it
   cannot produce an SFrame header, the AAD `H || P` or the counter partition) and
   `PacketTrailerTransformer` (appends timing metadata). `livekit` keeps every `RtpSender` /
   `RtpReceiver` `pub(crate)` (`LocalTrack::transceiver`), except a test-only `publisher_senders()`
   behind the private `__lk-e2e-test` feature, with no receiver equivalent.
2. **Glue in a vendored `webrtc-sys` on the prebuilt libwebrtc: works.** The prebuilt package ships
   `api/frame_transformer_interface.h` (with `GetMimeType`, `GetDirection`, the video frame's
   `IsKeyFrame` and `Metadata().GetSpatialIndex()`), which is everything the transform needs; the
   glue compiled and linked against it unchanged. The patch adds 45 lines and removes 6 in six upstream
   files plus three new files, documented in `core/vendor/README.md` ("webrtc-sys 0.3.53").
   libwebrtc is not rebuilt.
3. Not reached.

How the glue stays fail-closed: a sender gets its transformer inside `add_track` /
`add_transceiver`, before negotiation starts its RTP (and again on `set_track`); a receiver inside
`OnTrack`, before Rust hears of it; while a hook is installed, LiveKit's `FrameCryptor` and
packet-trailer code never set a transformer, so nothing can replace it. The transformer has no
pass-through: a frame goes on only when Rust answers `Replace` (sealed or authenticated) or
`PassUnchanged` (only a zero-byte Opus frame, protocol/05 DTX). A sender track not bound to a slot,
a frame whose own codec has no prefix rule, a kind/slot mismatch, an empty video frame, and every
`SframeError` are dropped. A receiver track not yet bound to its participant's device and slot,
and `E_SFRAME_UNKNOWN_KID`, are held (strict FIFO, 2 000 ms / 256 frames / 8 MiB); every other
error is dropped and counted by its `E_SFRAME_*` code. The publication still carries
`Encryption::CUSTOM` (the SDK's `e2ee` option with `EncryptionType::Custom`), which the browser
requires (`noneFlagged` otherwise); its `KeyProvider` is never used. The deprecated `e2ee` field is
used because `encryption` also turns on LiveKit's data-channel encryption.

## Link conflict (blocks linking `dilla-core` and `livekit` into one binary)

`dilla-core` always builds its native store with `rusqlite`'s `bundled-sqlcipher-vendored-openssl`,
and LiveKit's prebuilt `libwebrtc.a` contains BoringSSL. LiveKit renames BoringSSL's symbols with a
list taken from m114 (`webrtc-sys/libwebrtc/boringssl_prefix_symbols.txt`), which no longer covers
the BoringSSL in this libwebrtc: linking both fails with **208 duplicate symbols**
(`EVP_MD_CTX_get0_md`, `d2i_PKCS8_PRIV_KEY_INFO`, `ASN1_*_it`, `X509_ALGOR_*`, `BIO_get_ex_data`, …).
Measured by adding `dilla-core` as a dependency of this crate and linking its unit tests.
`dilla-desktop` links `dilla-client` (which depends on `dilla-core`) and `dilla-media` (which depends
on `livekit`); it links today only because `dilla-media` has no code yet, so nothing pulls libwebrtc
in. Any task that gives `dilla-media` real LiveKit code will hit this. Forcing the link
(`--allow-multiple-definition`) would mix two crypto libraries' internals and is not an option.

This proof therefore compiles `dilla_core::sframe` from dilla-core's own source files
(`#[path = "../../../core/dilla-core/src/sframe/mod.rs"]`, plus the two `identity` helpers it calls,
written as dilla-core writes them) instead of linking the crate; dilla-core's own sframe tests and
the vector runner pass in this crate, and the page side is the real `dilla-core-wasm`. Ways out,
for Jonas to decide (none is done here):

- a `store` feature (or a split crate) in dilla-core, so media code can use `sframe` without
  SQLCipher; the media code then still cannot share a binary with the store;
- the desktop's media in its own process (a helper beside the UI), which also isolates libwebrtc
  crashes;
- extend the BoringSSL rename list and apply it to the downloaded `libwebrtc.a` in the vendored
  `webrtc-sys/build.rs` (`objcopy --redefine-syms` on Linux, equivalents on macOS and Windows); that
  edits the prebuilt binary, so it is close to the "no own libwebrtc build" line;
- SQLCipher on another crypto provider.

## Not covered (follow-ups for the real `dilla-media`)

- The hold drains only when a later frame of the same track arrives; the browser also drains on a
  timer and on install. A track that goes silent keeps its held frames until it is torn down (they
  are never released unauthenticated).
- No key-frame request after a dropped video frame (protocol/05 "Key frames"); recovery is
  libwebrtc's own 3 s PLI.
- No server-injected-frame (SIF) trailer check before the SFrame parse.
- Simulcast and SVC were off; the layer allocator mirrors the worker's (`(ssrc, spatial index)`
  first-seen per `(KID, slot)`) and is unit-tested, not tested live.
- Screen-share slots, H.264 and VP9 were not run live (the codec paths are dilla-core's, and its
  vectors pass).
- The hook must be installed before the first PeerConnection exists; `install` refuses a second
  hook but not a late first one.
- Upstream `RtpSender::set_track` dereferences a null track; unchanged here.
- Sender uniqueness (N1–N3) across process lives is the caller's: the proof creates one
  `SframeSender` per process, with `min_epoch` its first epoch.

## Running it

From the repository root (Linux; the e2e media suite's prerequisites):

```sh
cargo build -p dilla-core-wasi --target wasm32-wasip1 --release --locked
cp target/wasm32-wasip1/release/dilla_core_wasi.wasm internal/mlswasi/testdata/
npm ci && npm run build:wasm -w packages/media
(cd experiments/gpui-voice-proof && cargo test --locked --lib)
npx playwright test -c experiments/gpui-voice-proof/browser/playwright.config.ts
# Step 4b, with a display (not a locked session):
(cd experiments/gpui-voice-proof && cargo build --locked --release --bin voice-proof \
  && cargo build --locked --release --features window --bin video-window)
VOICE_PROOF_WINDOW=1 VOICE_PROOF_BIN=experiments/gpui-voice-proof/target/release/voice-proof \
  npx playwright test -c experiments/gpui-voice-proof/browser/playwright.config.ts
```

The global setup is the media suite's own (`e2e/media/support/testhost.ts`: dilla-testhost with
`-sfu` on 7880/7882, the packages/media harness on 5179). Results and logs land in
`experiments/gpui-voice-proof/target/proof-logs/` (one JSON per test; the `VIDEO` line is in
`video_1080p_gpui_window.json`).

## CI job for Step 4b (for `gpui-proofs.yml`, which Task 3 owns)

```yaml
  voice-proof:
    name: voice-proof-${{ matrix.platform }}
    runs-on: ${{ matrix.os }}
    timeout-minutes: 75
    strategy:
      fail-fast: false
      matrix:
        include:
          - { os: ubuntu-24.04, platform: linux }
          - { os: macos-15, platform: macos }
          - { os: windows-latest, platform: windows }
    defaults:
      run:
        shell: bash
    steps:
      - uses: actions/checkout@<pinned, as in desktop-gpui.yml>
        with: { persist-credentials: false }
      - uses: actions/setup-node@<pinned>
        with: { node-version-file: .nvmrc }
      - uses: actions/setup-go@<pinned>
        with: { go-version-file: go.mod }
      - name: Linux build and display dependencies
        if: runner.os == 'Linux'
        run: >-
          node scripts/retry-on.mjs --attempts 6 --wait 60
          --pattern 'Temporary failure resolving|Could not resolve|Failed to fetch|Unable to fetch'
          -- bash scripts/ci-apt-install.sh
          <desktop-gpui.yml's package list> xvfb mesa-vulkan-drivers
      - run: npm ci
      - run: npx playwright install --with-deps chromium
      - run: bash scripts/install-ci-tool.sh wasm-pack
      - run: npm run build:wasm -w packages/media
      - run: |
          cargo build -p dilla-core-wasi --target wasm32-wasip1 --release --locked
          cp target/wasm32-wasip1/release/dilla_core_wasi.wasm internal/mlswasi/testdata/
      - working-directory: experiments/gpui-voice-proof
        run: |
          cargo test --locked --lib
          cargo build --locked --release --bin voice-proof
          cargo build --locked --release --features window --bin video-window
      - name: Interop and the 60 s GPUI window measurement
        env:
          VOICE_PROOF_WINDOW: '1'
          VOICE_PROOF_BIN: ${{ github.workspace }}/experiments/gpui-voice-proof/target/release/voice-proof${{ runner.os == 'Windows' && '.exe' || '' }}
          VIDEO_WINDOW_BIN: ${{ github.workspace }}/experiments/gpui-voice-proof/target/release/video-window${{ runner.os == 'Windows' && '.exe' || '' }}
        run: |
          if [ "$RUNNER_OS" = Linux ]; then
            xvfb-run -a -s '-screen 0 1920x1080x24' npx playwright test -c experiments/gpui-voice-proof/browser/playwright.config.ts
          else
            npx playwright test -c experiments/gpui-voice-proof/browser/playwright.config.ts
          fi
      - name: VIDEO line
        if: always()
        run: grep -ho '"line": *"VIDEO[^"]*"' experiments/gpui-voice-proof/target/proof-logs/video_1080p_gpui_window.json >> "$GITHUB_STEP_SUMMARY" || true
      - uses: actions/upload-artifact@<pinned>
        if: always()
        with:
          name: voice-proof-${{ matrix.platform }}
          path: experiments/gpui-voice-proof/target/proof-logs
```

Risks in that job, not tried here: `e2e/media/support/testhost.ts` builds `dilla-testhost` without
an `.exe` suffix and spawns it by that name, which may fail on Windows; GitHub's macOS runners may
not give GPUI a Metal device; Linux under Xvfb renders through Mesa's software Vulkan (lavapipe),
so its CPU figure is an upper bound, not a desktop number.
