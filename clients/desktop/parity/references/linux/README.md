# Linux parity references

| File | What |
| --- | --- |
| `channel-view-2x.png` | `basic`: the web client's channel view, 1280x800 at device scale 2 (2560x1600) |
| `channel-view.fixture.json` | the same data as the client that drew it holds it: the input of the GPUI side |
| `channel-view-wide-2x.png` | `wide`: 1440x900 at scale 2 (2880x1800): a second server selected, a category with a text and a voice channel, a long channel name (cut in the list, whole in the header), eleven members, a message that wraps over three lines and a follow-up row |
| `channel-view-wide.fixture.json` | its data |

Used by `clients/desktop/dilla-desktop/tests/slice.rs` (`slice_parity_channel_view`, `slice_parity_channel_view_wide` and
the negative control `slice_parity_fails_with_a_fallback_font`), all under the same Linux tolerances of
`../../tolerance.toml`, with no per-fixture setting.

## How they were made

From the current web client (`packages/web`, the beta.5 code the Electron app loads; controller ruling R8), not from a
mock-up, by `../shoot-channel-view.mjs <basic|wide>`:

1. `cargo run -p dilla-desktop --example parity_world -- <scenario> <out>` starts dilla-testhost serving
   `packages/web/dist`, signs the scenario's people up the way the app does (each with an authenticator app, a password and
   a sealed backup), makes its servers and channels and stops for the browser.
2. A fresh Chromium (Playwright 1.64.1) signs Ada in on a new device the way a person does: password, authenticator code,
   recovery key, "Open dilla". It opens the scenario's server and channel (and joins its group there), so the history that
   follows is readable on it.
3. The world posts the scenario's messages in order, writes what Ada's own headless client holds as the fixture, and stops
   every client, so only the browser is online.
4. The browser waits for the last message, leaves the member pane closed (the slice has no presence), moves the pointer to
   the corner, blurs the focus, awaits `document.fonts` and takes one viewport screenshot.

Chromium runs with `--font-render-hinting=none --enable-font-subpixel-positioning` (Jonas's 2026-11-12 decision for the Linux
reference, Task 3), `deviceScaleFactor: 2`, `reducedMotion: 'reduce'`, `timezoneId: 'UTC'`, `locale: 'en-US'`, at the
scenario's viewport. The fixture's times are the host's clock; the GPUI side draws them in UTC at the fixture's `now`, in a
window of the fixture's size.

To shoot them again (Linux, from the repository root):

```
npm ci && npm run build -w @dilla/web          # wasm-pack builds packages/core-wasm/pkg first
go build -o target/dilla-testhost ./cmd/dilla-testhost
cargo build -p dilla-core-wasi --target wasm32-wasip1 --release
node clients/desktop/parity/references/shoot-channel-view.mjs basic target/parity-world-basic
node clients/desktop/parity/references/shoot-channel-view.mjs wide target/parity-world-wide
```

Each overwrites its pair here. The fixture's ids, times and keys change on every shoot, so a PNG and its JSON are only valid
as a pair. `<out>/<name>.layout.json` keeps every box and text run of the frame with its computed style (for porting);
`<out>/state.json` holds throwaway secrets and never goes into the repository.

## Shot

2026-11-12, Linux (CachyOS, kernel 7.3), Playwright 1.64.1 Chromium 156.0.8078.4 (revision 1248), the web client built from
this branch (`feat/gpui-desktop` with Tasks 9b and 11 merged).
