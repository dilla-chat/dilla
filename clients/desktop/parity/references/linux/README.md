# Linux parity references

| File | What |
| --- | --- |
| `channel-view-2x.png` | The web client's channel view, 1280x800 at device scale 2 (2560x1600) |
| `channel-view.fixture.json` | The same data as the client that drew it holds it: the input of the GPUI side |

Used by `clients/desktop/dilla-desktop/tests/slice.rs` (`slice_parity_channel_view`, and its negative control
`slice_parity_fails_with_a_fallback_font`) under the Linux tolerances of `../../tolerance.toml`.

## How it was made

From the current web client (`packages/web`, the beta.5 code the Electron app loads; controller ruling R8), not from a
mock-up, by `../shoot-channel-view.mjs`:

1. `cargo run -p dilla-desktop --example parity_world` starts dilla-testhost serving `packages/web/dist` and signs up Ada,
   Björn and Mira the way the app does (each with an authenticator app, a password and a sealed backup), makes the community
   "Midgard Crew" with #general and stops for the browser.
2. A fresh Chromium (Playwright 1.64.1) signs Ada in on a new device the way a person does: password, authenticator code,
   recovery key, "Open dilla". It opens #general (and joins its group there), so the history that follows is readable on it.
3. The world posts twelve messages from the three of them in order (the text of the Task 3 fixture without its code block
   and reactions), writes what Ada's own headless client holds as `channel-view.fixture.json`, and stops every client, so
   only the browser is online.
4. The browser waits for the last message, leaves the member pane closed (the slice has no presence), moves the pointer to
   the corner, blurs the focus, awaits `document.fonts` and takes one viewport screenshot.

Chromium runs with `--font-render-hinting=none --enable-font-subpixel-positioning` (Jonas's 2026-11-12 decision for the Linux
reference, Task 3), `deviceScaleFactor: 2`, `reducedMotion: 'reduce'`, `timezoneId: 'UTC'`, `locale: 'en-US'`. The fixture's
times are the host's (fake) clock; the GPUI side draws them in UTC at the fixture's `now`, as the browser did.

To shoot it again (Linux, from the repository root):

```
npm ci && npm run build -w @dilla/web          # wasm-pack builds packages/core-wasm/pkg first
go build -o target/dilla-testhost ./cmd/dilla-testhost
cargo build -p dilla-core-wasi --target wasm32-wasip1 --release
node clients/desktop/parity/references/shoot-channel-view.mjs target/parity-world
```

It overwrites both files here. The fixture's ids, times and keys change on every shoot, so the PNG and the JSON are only
valid as a pair. `target/parity-world/channel-view.layout.json` keeps every box and text run of the frame with its computed
style (for porting); `target/parity-world/state.json` holds throwaway secrets and never goes into the repository.

## Shot

2026-11-12, Linux (CachyOS, kernel 7.3), Playwright 1.64.1 Chromium 156.0.8078.4 (revision 1248), the web client built from this branch
(`feat/gpui-desktop` at "Merge Task 9: sync engine and state model").
