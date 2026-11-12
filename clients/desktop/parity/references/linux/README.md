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
per-fixture setting. The two shell sidecars were written from the data of the shots of 2026-11-12 (Playwright 1.64.1
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
reference), `deviceScaleFactor: 2`, `reducedMotion: 'reduce'`, `timezoneId: 'UTC'`, `locale: 'en-US'`. Build first:

```
npm ci && npm run build -w @dilla/web          # wasm-pack builds packages/core-wasm/pkg first
go build -o target/dilla-testhost ./cmd/dilla-testhost
cargo build -p dilla-core-wasi --target wasm32-wasip1 --release
```

`node shoot.mjs <fixture> --check` prints the fixture line (`fixture <name> <sha256> <kind:local=hex;...>`) without a
browser; the GPUI driver's `fixture` command prints the same line (`shooters_agree_on_fixture`). The parity world seeds
plain text history only; each area adds the seeding of its own facets (reactions, replies, polls, ...) in
`dilla-desktop/examples/parity_world.rs` (`unseedable` and `seed_facets`) and the mapping into its state slice in
`dilla-desktop/src/fixture.rs`.
