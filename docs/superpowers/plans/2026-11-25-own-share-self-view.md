# Own screen share and camera self-view in the GPUI call stage: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan.

**Goal:** while you share your screen or your camera in a call, your own tile shows what you send, as the old client
did. Today the GPUI app's own screen tile reads "sharing screen", with no picture.

**Cause:** `clients/desktop/dilla-desktop/src/screens/voice/mod.rs` `refresh_images` builds tile images only from
`MediaSession::videos()` (`screens/voice/session.rs:459`), which holds received remote frames. Nothing keeps the frames
this device captures and sends.

**Spec:** the old client, which Jonas reported working (2026-11-25, screenshot: own share tile is blank).
- `packages/web/src/call/session.ts:84-87`: `localCamera` and `localScreen` are "what this device sends, for its own
  tile's self-view: the camera and the screen as local views (never a received stream), null while not sent".
- `packages/web/src/screens/voice/VoiceView.tsx:210-222`: the own device's tiles get `screen: localScreen` and
  `camera: localCamera` with `mirrorCamera: true`.
- The GPUI rewrite rule is parity with the old client (design, not pixels).

## Global Constraints

- The self-view comes from the local capture this device sends (a tap on the capture pipeline, after scaling to the
  sent resolution or before; implementer's call, stated in the report). Never from a received or looped-back stream.
- Own screen: the own screen tile shows the local frames while sharing. It goes back to nothing when the share stops,
  ends, or its source closes, and the old frames' images leave the GPU atlas the same way remote ones do
  (`drop_image`).
- Own camera: the own person tile shows the local camera, mirrored horizontally (the old client's `mirrorCamera: true`).
  Check first whether the GPUI app already shows its own camera. If it does, leave it and say so; if not, add it.
- A tile hidden by the existing rules (`shows_media`, `media_hidden`, paused call) hides the self-view too.
- Cost: the self-view must not copy every captured frame at full rate when that's avoidable. Keep at most the newest
  frame (as `videos()` does) and rebuild an image only when the frame counter moves. Pick a self-view frame cap; at
  most the send frame rate is fine.
- Privacy: a self-view frame is the screen's content. It never goes to logs, Debug output or test output (no pixel
  dumps); tests assert sizes, counters and presence only.
- The tile stat on the own screen tile (`frame_stat`: height and fps) may show the sent stream's numbers, as
  `tile-stats.ts` `ownScreen` did. Optional, and only if it's cheap.
- Update `clients/desktop/MANUAL.md` (screen share: "your own tile shows your share") and the desktop user docs if they
  describe the call stage tiles.

## Review Focus

1. A stopped share or a closed source leaves no stale picture on the own tile.
2. No unbounded growth: one frame kept per own stream, and images freed when replaced.
3. The camera is mirrored only on the own tile, never on what is sent or on others' tiles.
4. Nothing in the self-view path can block the capture or send path (no lock held across encode, no blocking send).
5. Screen content never reaches logs or test output.

### Task 1: own screen and camera self-view

**Files:** find them. Most likely `clients/desktop/dilla-media/src` (the capture or publish path: `capture_source.rs`,
`room.rs`, `sources/*`), `clients/desktop/dilla-desktop/src/screens/voice/{session.rs,mod.rs}`, and tests in
`clients/desktop/dilla-desktop/tests/` or `clients/desktop/dilla-media/tests/`, following the existing voice-stage test
style (the `test-hooks` feature and `put_test_frame` exist).

- [ ] **Step 1, failing tests:**
  - `the_own_screen_tile_shows_the_local_share`: while sharing, the own screen tile has media; after stop it has none,
    and its image is dropped.
  - `the_own_camera_tile_shows_the_local_camera_mirrored`, if the camera self-view is missing.
  - A media-level test where one exists: the local tap yields frames of the sent stream while publishing, and none
    after unpublish.
- [ ] **Step 2, implement**, within the constraints.
- [ ] **Step 3, verify:**
  - `cargo test -p dilla-desktop` and `cargo test -p dilla-media` for the touched tests. Env:
    `CARGO_TARGET_DIR=$PWD/target CC=clang CXX=clang++ CARGO_BUILD_JOBS=4`.
  - `cargo clippy` on the touched crates, with `-D warnings`.
  - `cargo fmt --check` on your own files.
  - Afterwards delete `target/debug/incremental`.
- [ ] **Step 4, commit:** `desktop: your own screen share and camera show on your own tile, as in the old client`.

### Task 2: no blinking caret after DILLA in the top bar

**Spec:** Jonas, 2026-11-25: "remove the blinking [] next to DILLA in the top". His call overrides the Signal prototype's
TopBar `d-caret`.

- [ ] Remove the brand caret child in `clients/desktop/dilla-desktop/src/screens/titlebar.rs` (the
  `motion::blink(... "topbar-caret" ...)` after the "DILLA" line) and its `BRAND_CURSOR_W` and `BRAND_CURSOR_H`
  constants.
- [ ] If `dilla_desktop_ui::motion::blink` and its caret clock (`CaretEpoch`, `Blink`, `caret_*`) have no other caller,
  remove them and their tests, or keep whatever another caller or test still uses. No dead code, no
  `#[allow(dead_code)]`.
- [ ] Keep the brand's layout otherwise as is (the gap after "DILLA" closes with it). Check the macOS traffic-light
  clearance probe (`topbar-brand`) still holds.
- [ ] Grep the tests, behaviour gates (`clients/desktop/BEHAVIOUR-GATES.md`, `behaviour-gates-titles.tsv`) and docs for
  the caret, and update what names it.
- [ ] Commit: `desktop: no blinking caret after DILLA in the top bar`.
