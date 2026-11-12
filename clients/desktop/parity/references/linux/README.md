# Linux parity references

`<area>/<state>[-b]-2x.png` is the web client's frame of the screen fixture `../../fixtures/<area>/<state>[-b].json`, shot
at device scale 2 (so 1280x800 is 2560x1600). `<area>/<state>[-b].shot.json` beside it is the sidecar: the SHA-256 of the
fixture file the shot was made from (a reference shot from another version of the fixture is refused as stale), the clock
shift the shot ran under and the ids the real dillad handed out (the web client sorts equal positions by id, so the GPUI
side draws the same ids). A reference without a sidecar is drawn with the fixture's own times and derived ids.

| Reference | What |
| --- | --- |
| `shell/channel-view` | the channel view, 1280x800, one server |
| `shell/channel-view-b` | 1440x900, a second server selected, a category with a text and a voice channel, a long channel name (cut in the list, whole in the header), eleven members, a message that wraps over three lines |

Both are compared by `parity/tests/screen.rs` (`channel_view_two_fixtures`) under `../../tolerance.toml`, with no
per-fixture setting. The two shell sidecars carry `"hand_migrated": true`: they were written by hand from the data of the shots of 2026-11-12 (Playwright 1.64.1
Chromium 156.0.8078.4, the web client of `feat/gpui-desktop` with Tasks 9b and 11 merged), which predate the screen
fixtures; shoot them again with `shoot.mjs` to make them pairs of the shooter.

## How they are made

`node clients/desktop/parity/shoot.mjs clients/desktop/parity/fixtures/<area>/<state>.json` (from the repository root,
Linux):

1. `cargo run -p dilla-desktop --example parity_world -- <fixture> <out>` starts dilla-testhost serving `packages/web/dist`,
   signs the world's people up the way the app does (each with an authenticator app, a password and a sealed backup), makes
   its servers and channels and stops for the browser.
2. A fresh Chromium signs the viewer in on a new device the way a person does (password, authenticator code, recovery key,
   "Open dilla"), visits every channel that will hold history (joining its group), with the browser's clock frozen at the
   fixture's `clock` (lifted by the shift, below).
3. The world posts the history in time order. The host's clock is frozen and moves only forward from the moment the host
   starts, so the world lifts the whole fixture by a shift (every message time and the clock) and records it in the
   sidecar. It stops every client, so only the browser is online.
4. The browser opens the fixture's route, replays the steps' `web` selectors (`-`: no web counterpart; `if:` a selector
   that clicks only when the element is there), moves the pointer to the corner (unless a step hovers), blurs the focus,
   awaits `document.fonts` and takes one viewport screenshot.

Chromium runs with `--font-render-hinting=none --enable-font-subpixel-positioning` (Jonas's 2026-11-12 decision for the Linux
reference), `deviceScaleFactor: 2`, `reducedMotion: 'reduce'`, `timezoneId: 'UTC'`, `locale: 'en-US'`.

## Prerequisites (once per worktree)

Verified end to end on 2026-11-12 (CachyOS, clang 23.1.4, a machine shared with other builds; wall-clock times). From the
worktree root, every step in order. `CC=clang CXX=clang++` is needed on Linux (the sqlite3mc and WASI builds want clang >= 21):

```
export CC=clang CXX=clang++
npm ci                                                                  #   3 s
wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install \
  --out-dir ../../packages/core-wasm/pkg                                # 1 m 48 s: the web client imports @dilla/core-wasm
npm run build -w @dilla/web                                             #  15 s (fails with "Failed to resolve entry for package
                                                                        #  @dilla/core-wasm" if the wasm-pack step is missing)
go build -o target/dilla-testhost ./cmd/dilla-testhost                  #   9 s
cargo build -p dilla-core-wasi --target wasm32-wasip1 --release         # 2 m 01 s
```

`npm run build -w @dilla/web` does NOT build `packages/core-wasm/pkg`; run `wasm-pack` yourself as above. Playwright's
Chromium (revision 1248) must be in `~/.cache/ms-playwright` (`npx playwright install chromium` otherwise).

## Shooting

```
node clients/desktop/parity/shoot.mjs clients/desktop/parity/fixtures/shell/channel-view.json --out target/shots/run1
```

The first run in a worktree also compiles `parity_world` (gpui, dilla-client, sqlite3mc): about 15 min on a shared machine;
every later shot takes about 2 min (`shell/channel-view-b`: 1 m 59 s). With `--out <dir>` the PNG and the sidecar go to
`<dir>/shots/<area>/<state>-2x.png` and `<state>.shot.json`, and the world's files (`state.json` with throwaway secrets,
`world.json`, `<state>.layout.json` with every box and text run of the frame) to `<dir>`. Without `--out` the shot REPLACES the
committed reference and its sidecar in this directory: do that only to refresh a reference on purpose, then commit the PNG and
the sidecar together (a PNG and its sidecar are only valid as a pair: the sidecar carries the ids and clock shift the world
handed out). To try a fresh shot against the GPUI frame without touching the committed pair, copy
`fixtures/<area>/` and your shot (PNG and sidecar) into a scratch `parity/{fixtures,references/linux}/<area>/` tree and call
`dilla_parity::screen::assert_screen` on the scratch fixture path (it derives the references from the fixture's location).

Re-shooting the same fixture is not byte-identical to the committed shot: the clock shift differs (the message times in the
header rows are the only pixels that change in `shell/channel-view`: 0.13 % of the pixels, all inside x 626..687, one band per
message header) and the host hands out new ids, which reorder anything sorted by id (`shell/channel-view-b`: 1.3 % differ).

`node shoot.mjs <fixture> --check` prints the fixture line (`fixture <name> <sha256> <kind:local=hex;...>`) without a
browser; the GPUI driver's `fixture` command prints the same line (`shooters_agree_on_fixture`). The parity world seeds
plain text history only; each area adds the seeding of its own facets (reactions, replies, polls, ...) in
`dilla-desktop/examples/parity_world.rs` (`unseedable` and `seed_facets`) and the mapping into its state slice in
`dilla-desktop/src/fixture.rs`.
