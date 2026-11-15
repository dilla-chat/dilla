# Vendored crates

Third-party crates that dilla builds from a patched copy, through `[patch.crates-io]` in the root
`Cargo.toml`. Each copy is the published crate as it was downloaded from crates.io, minus the
registry's `.cargo-ok` and `Cargo.lock`, plus the changes listed below. Every change is marked
`DILLA PATCH` in the source. The directory is excluded from the workspace, so `cargo fmt`, `cargo
clippy` and the workspace lints do not touch it.

## libsqlite3-sys 0.39.1

Upstream: <https://github.com/rusqlite/rusqlite> (`libsqlite3-sys/`; `.cargo_vcs_info.json` has the
commit). MIT.

Two changes. The second is what the native store builds with today.

### 2. `bundled-sqlite3mc`: SQLite3 Multiple Ciphers, no OpenSSL (Task 6b, 2026-11-12)

A new feature, `bundled-sqlite3mc = ["bundled", "dep:sqlite3mc-src"]`, with the build-dependency
`sqlite3mc-src = "=206.1.0"` (SQLite3MC 2.6.1 on SQLite 3.54.0; MIT, with the CC0-1.0 / Unlicense
parts `deny.toml` already allows for that crate). It is the same release, from the same crate,
that the browser core compiles through sqlite-wasm-rs 0.6.3, so the native and browser stores are
one cipher implementation. Edits, all marked `DILLA PATCH`:

- `Cargo.toml` and `Cargo.toml.orig`: the feature and the optional build-dependency.
- `build.rs`:
  - a `compile_error!` when `bundled-sqlcipher` and `bundled-sqlite3mc` are both on (one SQLite
    per binary);
  - `build_bundled` compiles `sqlite3mc_src::source_dir()/sqlite3mc_amalgamation.c` instead of
    `sqlite3/sqlite3.c`, with the usual bundled flags plus `-DCODEC_TYPE=CODEC_TYPE_CHACHA20` (the
    default cipher), `-DSQLITE3MC_SECURE_MEMORY=1` (makes `PRAGMA memory_security = fill`
    available: freed SQLite memory is wiped; sqlite3mc has no mlock), `-DSQLITE_TEMP_STORE=2` and
    `-DHAVE_STDINT_H=1`. The crypto is sqlite3mc's own C code; nothing links OpenSSL or a system
    crypto library (`cargo tree -i openssl-sys` is empty).

Rusqlite has no such feature, so each native crate that opens a store (`dilla-core`,
`dilla-bot-core`, `clients/desktop/dilla-client`, `testkit`) asks for
`rusqlite = { features = ["bundled"] }` plus `libsqlite3-sys = { features = ["bundled-sqlite3mc"] }`,
and feature unification hands rusqlite that one copy.

Known leftovers:
- The committed `sqlite3/bindgen_bundled_version.rs` (SQLite **3.54.1** headers) is used against
  the **3.54.0** library, so the compile-time `SQLITE_VERSION*` constants say 3.54.1 while
  `sqlite3_libversion()` / `SELECT sqlite_version()` report 3.54.0. Patch releases add no API, so
  the bindings are sound. `buildtime_bindgen` is not supported with this feature (the
  amalgamation's header is `sqlite3mc_amalgamation.h`, not `sqlite3.h`).
- `DEP_SQLITE3_INCLUDE` points at the sqlite3mc directory, which has no `sqlite3.h` (commented in
  `build.rs`); nothing in the graph reads it.

Legacy stores: sqlite3mc opens SQLCipher 4 files that are clean or in WAL mode (a hot WAL
included), but not one with a hot **rollback journal** from SQLCipher, whose journal checksums
cover ciphertext where sqlite3mc's cover plaintext; `dilla_core::native_store` refuses such a file
untouched (`KeyError::LegacyHotJournal`).

Why: dilla-core's SQLCipher linked vendored OpenSSL, and LiveKit's prebuilt libwebrtc carries
BoringSSL with an incomplete rename list, so the desktop client could not link dilla-core and
LiveKit into one binary (208 duplicate symbols; Jonas's ruling R14). Regression tests:
`core/dilla-core/tests/sqlite3mc_build.rs` (sqlite3mc linked, SQLite >= 3.54.0, no
openssl-sys / sqlcipher-src in Cargo.lock), `core/dilla-core/tests/native_store.rs` (ChaCha20
stores, and SQLCipher 4 files from the old build still open) and
`clients/desktop/dilla-media/tests/link.rs` (one binary uses both).

RUSTSEC-2026-0459 is fixed in SQLite 3.54.0, which sqlite3mc-src 206.1.0 bundles.

Keep this patch until upstream libsqlite3-sys can build SQLite3 Multiple Ciphers itself.

### 1. `sqlcipher-src` 420 (RUSTSEC-2026-0459; no longer built)

No crate enables `bundled-sqlcipher` since Task 6b, so this pin is dormant (sqlcipher-src and
openssl-sys left Cargo.lock); it stays so the feature still builds a fixed SQLite if anyone turns
it on, and is dropped with the copy.

One change, in `Cargo.toml` (and the same line in `Cargo.toml.orig`): the `sqlcipher-src`
build-dependency requirement moves from `=419.0.6` to `=420.0.0`. Nothing else is edited; `build.rs`
uses the same `sqlcipher_src::source_dir()` / `SOURCE_FILE` API, which 420.0.0 keeps unchanged
(its `src/` differs from 419.0.6 only in the version constants).

Why: RUSTSEC-2026-0459 / CVE-2026-119375 (SQLite before 3.54.0 can return uninitialized
page-cache memory when a database is opened with a crafted sibling `-wal` file of a different page
size). The advisory itself notes that `bundled-sqlcipher` in libsqlite3-sys 0.39.x stays affected. rusqlite 0.41.1 / libsqlite3-sys 0.39.1, the newest release, pins `sqlcipher-src =419.0.6`,
which bundles SQLCipher 4.19.0 on SQLite 3.53.4, so the native store dilla-core, dilla-bot-core and
the testkit build with `bundled-sqlcipher-vendored-openssl` stayed vulnerable even with the
advisory resolved in Cargo.lock. `sqlcipher-src 420.0.0` bundles SQLCipher 4.20.0 on SQLite 3.54.1.
Its regression test, `core/dilla-core/tests/sqlcipher_build.rs`, became `sqlite3mc_build.rs` in
Task 6b.

Known leftover: the pre-generated `sqlcipher/bindgen_bundled_version.rs` still comes from the
3.53.4 headers, so the compile-time `SQLITE_VERSION*` constants in the bindings say 3.53.4. The
runtime (`sqlite3_libversion*`, `SELECT sqlite_version()`) reports 3.54.1; SQLite's C ABI is
append-only, so older bindings against the newer library are sound.

The browser core (`wasm32-unknown-unknown`) does not use libsqlite3-sys; it compiles SQLite3MC
from sqlite-wasm-rs, which carries its own SQLite. That side is fixed without a vendored copy:
sqlite-wasm-rs 0.6.3 builds SQLite3MC 2.6.1 on SQLite 3.54.0 (`sqlite3mc-src =206.1.0`), see
"Retired" below.

Since Task 6b the native store builds its own SQLite3MC too (change 2), from the same
`sqlite3mc-src` release.

## webrtc-sys 0.3.53

Upstream: <https://github.com/livekit/rust-sdks> (`webrtc-sys/`; `.cargo_vcs_info.json` has the
commit). Apache-2.0. The published copy is the first commit that adds it (`vendor: webrtc-sys 0.3.53
as published`), so `git diff` of that commit's successor shows the whole patch.

Patched into the root workspace (`[patch.crates-io]` in `Cargo.toml`, Task 11) for `dilla-media`,
which installs its hook (`dilla_media::transform::Router`) before the process's first
PeerConnection. `experiments/gpui-voice-proof` (its own workspace and `[patch.crates-io]`) holds the
original evidence, `experiments/gpui-voice-proof/REPORT.md`; `clients/desktop/dilla-media/tests/`
(`glue.rs`, `hook_enforced.rs`, `call.rs`) covers the current glue.

What it adds: a process-wide encoded-frame hook for dilla-sframe/1 (protocol/05), on LiveKit's
prebuilt libwebrtc as it is downloaded (libwebrtc itself is not rebuilt). LiveKit's Rust SDK exposes
no transformer of its own (its `FrameCryptor` writes LiveKit's AES-GCM frame format, and its packet
trailer transformer only adds metadata), so the hook is glue implementing libwebrtc's
`FrameTransformerInterface`:

- New files, wholly dilla's: `include/livekit/dilla_transform.h`, `src/dilla_transform.cpp`,
  `src/dilla_transform.rs` (the cxx bridge and the Rust API: `dilla_transform::install(Box<dyn
  FrameTransform>)`, `FrameTransform::{attached, detached, transform, hold_expired, pass_refused,
  sif_trailer, key_frame_requests}`, `Verdict::{Drop, Replace, Hold, PassUnchanged}`, `drain`,
  `request_key_frame`, `receiver_handle` / `sender_handle`).
- Test support, only with the crate feature `dilla-test-frames` (which only dilla-media's
  dev-dependency enables, so no shipping binary contains it): `include/livekit/dilla_transform_testing.h`,
  `src/dilla_transform_testing.{cpp,rs}` (`webrtc_sys::dilla_transform_testing`: a transformer fed
  synthetic frames without a PeerConnection, through libwebrtc's own test-mock frame class names,
  which the prebuilt library does not define); `build.rs` compiles them and defines
  `DILLA_TEST_FRAMES` only then.
- `build.rs`, `src/lib.rs`: build and export the new bridge.
- The desktop client's camera (GPUI parity plan Package 1, 2026-11-15), Linux and Windows only:
  `include/livekit/dilla_camera.h`, `src/dilla_camera.{cpp,rs}` (`webrtc_sys::dilla_camera`:
  `dilla_camera_devices`, `dilla_camera_open(id, sink)`, `DillaCamera::start(w, h, fps)`). It wraps
  libwebrtc's own `VideoCaptureModule` of the prebuilt library (V4L2 on Linux, DirectShow on
  Windows), converts each frame to I420 there and hands the planes to a Rust sink; the destructor
  stops the capture and deregisters the sink before it returns. macOS's prebuilt has no
  `VideoCaptureModule` implementation, so the bridge is not built there. Covered by
  `clients/desktop/dilla-media/src/sources/camera.rs` (its packing test) and the manual
  `capture-probe camera`.
- `src/peer_connection_factory.cpp`: the hook is mandatory. `create_peer_connection` throws
  `E_DILLA_NO_FRAME_HOOK` when no hook is installed, so a missing or late `install` can never fall
  back to LiveKit's own FrameCryptor or to no transform
  (`clients/desktop/dilla-media/tests/hook_enforced.rs`).
- `src/peer_connection.cpp`: `add_track`, `add_transceiver` and `add_transceiver_for_media`
  attach a `DillaFrameTransformer` to the new sender before the negotiation that starts its RTP (a
  sender without a track gets one bound to no track, which drops every frame), so no sender exists
  without one; `OnTrack` attaches one to the receiver synchronously, before Rust learns of the
  track.
- `src/rtp_sender.cpp`: `set_track` never replaces a sender's transformer. libwebrtc's ChannelSend
  takes one transformer delegate per stream (a second is an `RTC_DCHECK`), so under the hook a
  sender takes back only the track its transformer was attached for (after a null swap); any other
  track is refused (`set_track` answers false). dilla-media never needs one: it switches the
  microphone device under the same track. (Task 2's glue attached a fresh transformer here; upstream
  dereferenced a null track here.)
- `include/livekit/frame_cryptor.h`: LiveKit's `KeyProvider::set_sif_trailer` (the room's
  server-injected-frame trailer from the join response) is also handed to the hook, which drops a
  frame ending in it before any SFrame header is parsed.
- `src/frame_cryptor.cpp`, `src/packet_trailer.cpp`: while a hook is installed, LiveKit's
  `FrameCryptorTransformer`, its chained packet-trailer transformer and the packet-trailer handlers
  are never set on a sender or receiver, so nothing replaces the hook (protocol/05 refuses packet
  trailers anyway).

Every transformer has an opaque handle, process-unique and never reused, which Rust learns when the
transformer is attached (`attached`) and which names it in every later call. A receiver is bound by
that handle, found from the subscribed track's own C++ object (`receiver_handle`, compared by
identity; see libwebrtc 0.4.0 below), never by a track id string the SFU chose; dilla-media refuses a
second bind of a handle.

The transformer has no pass-through of its own: every frame goes to Rust, and only a frame Rust
answers `Replace` (sealed or authenticated bytes) or `PassUnchanged` goes on. `PassUnchanged` is
enforced in C++: it forwards only a zero-byte Opus frame on an audio transformer and drops (and
reports, `pass_refused`) anything else so answered. `Hold` keeps the frame in a strict
per-transformer FIFO bounded by protocol/05's 2 000 ms, 256 frames and 8 MiB; the FIFO is retried on
every later frame, on every `drain` (dilla-media calls it after each key install and each binding)
and every 20 ms on the hook's own timer thread, so a held frame of a silent track still expires.
Past a bound the oldest is dropped and reported, never released unauthenticated. The same timer
sends the key-frame requests the hook asks for (`key_frame_requests`) through
`VideoTrackSourceInterface::GenerateKeyFrame` on the receiving track's source, which libwebrtc turns
into an RTCP PLI to the publisher.

Remove this copy when upstream exposes an equivalent custom frame transformer.

Also added (GPUI parity plan Package 12, inline video): a standalone video decoder for the desktop's attachment
player, wholly dilla's: `include/livekit/dilla_video_decoder.h`, `src/dilla_video_decoder.{cpp,rs}`
(`webrtc_sys::dilla_video_decoder::VideoDecoder`), built and exported from `build.rs` and `src/lib.rs`. It makes
libwebrtc's own software decoders directly (libvpx VP8/VP9, dav1d AV1, the bundled FFmpeg H.264 on Linux and
Windows; the macOS prebuilt's FFmpeg has no H.264, so macOS sends H.264 to the system player). VideoToolbox (macOS
H.264/H.265, through LiveKit's `VideoDecoderFactory`) only with the crate feature `dilla-video-toolbox`
(`DILLA_VIDEO_TOOLBOX`), off by default: that path crashed the decode test on macOS CI (SIGSEGV) and waits for a
Mac to fix it on. Teardown releases the decoder before dropping its callback, and a callback during teardown keeps
nothing. It feeds the decoder access units Rust demuxed from the decrypted file, and converts
each picture to BGRA with libyuv (the colour matrix the bitstream names, BT.601 when it names none), scaled down to
fit the box Rust gives. No PeerConnection, transformer or network is involved, and nothing in libwebrtc is rebuilt.

## libwebrtc 0.4.0

Upstream: <https://github.com/livekit/rust-sdks> (`libwebrtc/`; `.cargo_vcs_info.json` has the
commit). Apache-2.0. The published copy is the first commit that adds it (`vendor: libwebrtc 0.4.0 as
published`), so the diff of that commit's successor shows the whole patch. Patched into the root
workspace (`[patch.crates-io]`, Task 11).

The one change (`src/media_stream_track.rs`, `DILLA PATCH`): `sys_handle()` of `RtcAudioTrack`,
`RtcVideoTrack` and `MediaStreamTrack` is public instead of `pub(crate)`, so dilla-media can hand a
subscribed track's own C++ object to `webrtc_sys::dilla_transform::receiver_handle` and bind the
receiver's transformer by identity rather than by a track id the SFU chose.

Remove this copy when upstream makes the handle public or the webrtc-sys hook is upstreamed.

## gpui-pre-wgpu 0.3.12

Upstream: <https://crates.io/crates/gpui-pre-wgpu> (Zed's GPUI, wgpu renderer and cosmic-text system). Apache-2.0. The
copy is the published crate minus `.cargo-ok` and `Cargo.lock`.

**Platform coverage: Linux only.** `gpui-pre-platform` pulls this crate in only under `cfg(target_os = "linux")`. Windows
renders with GPUI's DirectX renderer (`directx_renderer.rs`, `shaders.hlsl`) and lays text out with DirectWrite; macOS
renders with Metal and Core Text. Every patch below therefore changes Linux only; on Windows and macOS the client draws
upstream's colour-emoji fallback, dash pattern and gradient geometry until the matching follow-ups land (a DirectX/HLSL
port next to the Metal one, each with its own tolerances measured), a known parity gap there, not a crash.

One change (Task 10, ruling R20, 2026-11-13), marked `DILLA PATCH`: in `src/cosmic_text_system.rs`, `load_family`'s
`allowed_bad_font_names` gains `"NotoColorEmoji"`. `load_family` skips every face whose cmap has no `'m'` (the editor measures
with its width), and Noto Color Emoji has none, so `font_family("Noto Color Emoji")` loaded nothing and cosmic-text's unix
fallback list reached the monochrome Noto Sans Symbols faces before the colour emoji face: a reaction chip drew a flat heart
where the web draws the colour one. Test: `noto_color_emoji_family_loads_a_face` in
`clients/desktop/dilla-desktop/tests/reactions.rs` (the chip of a heart shows red pixels; skipped where the font is not
installed).

Keep this patch until upstream lets a colour emoji face load as a named family.

A second change (Task 6c, 2026-11-13; final review I7, 2026-11-14), marked `DILLA PATCH`: in `src/shaders.wgsl`, a dashed
border thinner than 3 CSS px dashes 3 widths on and 2 off (Blink's `StrokeData` ratios for thin strokes) instead of
upstream's 2 on and 1 off, so the rail's 1px dashed "+" circle has the web's dash count (about 23 at scale 2, upstream drew
about 39). Thicker borders keep upstream's pattern. Blink's threshold is in CSS px, so the shader divides the device width
by the scene's scale factor: `GlobalParams`' padding word (`src/wgpu_renderer.rs`, same size) carries
`Scene::scale_factor` (a gpui-pre patch, below). Until 2026-11-14 it compared device px, so at scale 2 a 2px border (the
drop overlay) or a 1.5px one fell back to 2:1, and at scale 3 every 1px dash did. Test:
`the_dashed_border_follows_blink_s_pattern` and `the_scene_carries_the_scale_it_was_painted_at` in
`clients/desktop/dilla-desktop/tests/vendored_patches.rs`. Linux only (see above).

A third change (Task 28 parity lane, 2026-11-13), marked `DILLA PATCH`: in `src/shaders.wgsl`, `gradient_color`'s linear
gradient follows CSS (`linear-gradient(135deg, ...)`): the angle is a true angle and the gradient line, through the centre,
is `|w sin a| + |h cos a|` long. Upstream stretched the direction to the box's aspect and measured `t` along the longer
side, so a 135deg band on a 258 x 52 box ran almost left to right (the profile preview's band: 5-6 levels off at its right
end; the colour gate failed at 0.63 %). Test: `parity_user_settings_profile` in `clients/desktop/dilla-desktop/tests/
user_settings.rs` (the band against the web's). Linux only (see above): the DirectX renderer of Windows and the Metal
renderer of macOS keep upstream's stretched geometry until their ports' tolerances are measured.

## gpui-pre-linux 0.3.12

Upstream: <https://crates.io/crates/gpui-pre-linux> (Zed's GPUI, the Linux platform: Wayland, X11, xkb). Apache-2.0. The
copy is the published crate minus `.cargo-ok` and `Cargo.lock`. Linux only (nothing else builds it).

One change (Package 9, 2026-11-15), marked `DILLA PATCH`: AltGr's state is kept for the app. xkb's Level3
(ISO_Level3_Shift) is none of gpui's `Modifiers`, so AltGr with a key arrived as the bare key, and push to talk (which
refuses Control, Alt, Shift and Super with its key, as the web's ptt.ts refuses those and `AltGraph`) opened the microphone.
`src/linux/platform.rs` `modifiers_from_xkb`, which both backends call on every modifier change, records whether
`LevelThree` (or `Mod5`, for libxkbcommon before 1.8) is active in a thread-local on the thread that handles the keyboard's
events (the app's); `level3_shift_active()` reads it and `set_level3_shift_active()` sets it (tests), both re-exported from
`src/linux.rs` and `src/gpui_linux.rs`. gpui's own key handling is unchanged: AltGr still types its characters in text
fields. Test: `altgr_with_the_ptt_key_does_not_open_the_mic` in `clients/desktop/dilla-desktop/tests/voice.rs` and
`altgr_s_state_is_kept_for_the_app` in `clients/desktop/dilla-desktop/tests/vendored_patches.rs`.

Keep this patch until gpui reports AltGr itself (a Level3 modifier on `Modifiers`).

## Retired: sqlite-wasm-vfs 0.2.0

Vendored from 2026-10-08 until the RUSTSEC-2026-0459 fix moved the browser core to sqlite-wasm-rs
0.6.3 and sqlite-wasm-vfs 0.3.0. The one change was in `src/sahpool.rs` `xCheckReservedLock`: it
answered 0 ("no other connection holds a RESERVED lock") instead of 1, because SQLite plays a
leftover rollback journal back only when no connection holds RESERVED. With 1, the journal of a
write that a page unload interrupted was never played back, and the next open read the half-written
database (`.superpowers/sdd/2026-10-08-dilla-web-2c/opfs-report.md`).

sqlite-wasm-vfs 0.3.0 answers from the handle's own lock level (`self.lock_level >=
LockLevel::Reserved`), so a fresh connection reports no RESERVED lock and plays the journal back.
Still covered by `e2e/tests/mls-store.spec.ts` ("a write interrupted by a reload rolls back on the
next open"), and across the upgrade by `core/dilla-core-wasm/tests/sqlite_compat.rs` (a hot journal
written by the 0.5.5 build is played back by the current one).

## gpui-pre 0.3.12

Upstream: <https://crates.io/crates/gpui-pre> (Zed's GPUI). Apache-2.0. The copy is the published crate minus `.cargo-ok`,
`.cargo_vcs_info.json`, `Cargo.lock` and `examples/` (with their `[[example]]` targets dropped from `Cargo.toml`).

**Platform coverage: every platform.** This crate is GPUI's platform-neutral core (layout, elements, the window), so its
patches below apply on Linux, Windows and macOS alike; none has a `cfg(target_os)`. The fourth only carries a value that
just the Linux renderer reads.

One change (Task 6c, 2026-11-13), marked `DILLA PATCH`: in `src/taffy.rs`, `layout_bounds` snaps a box's outer edges to
whole logical px, half up, when the scale factor is whole; upstream rounds them to device px, midpoints toward zero. The web
client's frames (Chromium, `deviceScaleFactor: 2`) paint a box laid out at 51.5px from 52px (two device px further than
upstream GPUI), so every centred control, avatar, tile and field with a half-px offset (the 28px header buttons in the 43px
bar, the 30px panel avatar in the 63px panel, the 28px search field in the 44px top bar, the 17px `@` glyph in its 38px tile)
drew one device px off, which read as anti-aliasing on their rounded edges. With the patch `shell/conversation` goes from
0.21 % to 0.09 % chrome difference. A fractional scale keeps upstream's device-pixel snapping, so edges stay crisp there.

Before that snap, at a whole scale factor, authored lengths lie on Chromium's layout unit (1/64 css px, truncated toward zero as `LayoutUnit(float)` is, a whole unit off only by f32 noise kept whole:
`util::layout_length`) and measured sizes (text) are ceiled onto it (`util::measured_length`), where upstream rounds
both to whole device px: a box's fractions (a 42.609375px button whose "3" is 6.609375px) carry to the edge snap as
in Chromium. A fractional scale keeps upstream's device-px lengths.

The edge rule is `util::snap_layout_edge`, re-exported with the two length rules and their shared unit rule as `gpui::{snap_layout_edge, layout_length, measured_length, whole_layout_units}` so the client can
pin them. `util::whole_layout_units` (Task 28) is the shared "a whole unit but
for f32 noise" rule both length rules use; the client's text painter (`dilla-desktop-ui/src/text.rs`) reads it too. Tests: `box_edges_snap_as_chromium_paints_them` and `lengths_lay_out_on_chromiums_layout_unit` in `clients/desktop/dilla-desktop-ui/src/metrics.rs`.

`Window::laid_out_size` (with `TaffyLayoutEngine::laid_out_size`, Task 28) answers a box's laid-out size before its edges
snap (a prepaint read, as `Window::layout_bounds` is: it asserts so in debug builds). Chromium decides `text-overflow: ellipsis` against that width, in layout units; the snapped box can be up to 1px
narrower, so a decision on it kept whole a text that overflowed by under a pixel (the web shows "…"). The client's text
painter (`dilla-desktop-ui/src/text.rs`, `LinePaint`) reads it; the test `laid_out_size_is_the_unsnapped_box` in
`clients/desktop/dilla-desktop/tests/vendored_patches.rs` pins it, and `laid_out_size_is_asked_in_prepaint_only` its assert.

Keep this patch while the Linux parity references are Chromium frames.

A second change (Task 28 wave 2, 2026-11-14), marked `DILLA PATCH` in `src/elements/div.rs`: a focused element with a
click listener arms its keyboard click (Enter or Space key-down, fired on the matching key-up) from a recorder that
`paint_keyboard_listeners` registers after the element's own key-down listeners (`Interactivity::pending_activation_recorder`);
upstream registers it before them, in `paint_mouse_listeners`. A key-down the element handles itself (it stops propagation
or prevents the default) therefore no longer also clicks it on key-up, as a browser runs a button's activation behaviour
only for a keydown whose default was not prevented. Upstream, Enter on a member row opened its card on key-down and the
key-up click closed it again; every row, chip and menu item that handles Enter on the focused element itself did the
same. A key-down handler on an ancestor of the focused clickable still runs after the recorder, so that pattern would
still click as well (none is used today). Upstream's place keeps the half of the recorder that disarms (final review
M2): every key-down that reaches the element clears an armed activation first, so a key-down the element handles between
an activation key's down and up (Escape while Space is held) still cancels the click, as upstream's did. Tests:
`a_key_down_the_element_handles_does_not_also_click`, `a_key_down_that_prevents_the_default_does_not_also_click`,
`a_handled_key_down_between_cancels_an_armed_activation` and `enter_on_a_member_row_leaves_its_card_open` in
`clients/desktop/dilla-desktop/tests/vendored_patches.rs`. Keep it until upstream checks the key-down's handling before
arming the click.

A third change (Task 28 wave 2), marked `DILLA PATCH` in `src/window.rs`: `Window::line_height` answers the text style's
line height on Chromium's layout unit (`util::layout_length`) at a whole scale factor, where upstream rounds it to a whole
px. gpui-component's inputs and textareas lay their lines out by it, so a 13px textarea at 1.5 drew its lines 20px apart
where the web's are 19.5px. `TextStyle::line_height_in_pixels` (the text elements' own path) keeps upstream's rounding;
a fractional scale keeps it too. Test: `the_line_height_lies_on_the_layout_unit` in the same file.

So there are two line heights at a whole scale (final review M3): `Window::line_height` in 1/64 units and
`TextStyle::line_height_in_pixels` in whole px. Upstream's `TextView` `max_lines` (`text_view.rs`) and inline flow mix the
two, so a clamp can show a sliver (about 5 device px at scale 2) of the line after the last; dilla's client uses neither
today. A client change that does must take one of them for both the clamp and the lines.

The first change also rounds a stretched size onto the layout unit in `TaffyLayoutEngine` (`src/taffy.rs`, the size of an
`auto` box handed in from outside; marked since final review M4), and the long upstream comment on pixel snapping there
now carries a note that its device-pixel rules hold only at a fractional scale. `util::measured_length` ceils a measured
size onto the layout unit, but a size that is a whole unit but for f32 noise is kept whole, which may round it down by up
to about 0.03 units: a latent wrap or ellipsis risk for a text measured exactly at a unit boundary, not seen in the parity
screens.

A fourth change (final review I7, 2026-11-14), marked `DILLA PATCH`: `Scene` gains `scale_factor`, set by
`Window::draw` from the window's scale before the frame finishes (`src/scene.rs`, `src/window.rs`), and
`Window::painted_scale_factor` (test support) reads it back. The wgpu renderer (gpui-pre-wgpu, above) hands it to its
shader to measure dashed borders in CSS px; the Metal and DirectX renderers ignore it. Test:
`the_scene_carries_the_scale_it_was_painted_at` in `clients/desktop/dilla-desktop/tests/vendored_patches.rs`.

A fifth change (Package 11, screen readers, 2026-11-15), marked `DILLA PATCH`, test support only: the test platform's
window has no AccessKit adapter, so nothing ever switches its accessibility tree on and no test could see one.
`Window::activate_a11y_for_test` does what a platform adapter's activation callback does (`A11y::activate_for_test` sets
the window's active flag) and refreshes; `Window::a11y_tree_update` hands back the last frame's `TreeUpdate`
(`A11y::last_tree_update`, from the copy `A11yDebug` already keeps for `debug_a11y_tree_json`). Both are
`#[cfg(any(test, feature = "test-support"))]` (`src/window.rs`, `src/window/a11y.rs`, `src/window/a11y/debug.rs`), so a
release build has neither. The automation build's `a11y` command reads the tree through them and
`clients/desktop/dilla-desktop/tests/a11y.rs` lints it per screen. Keep it until upstream's test window can be activated.
