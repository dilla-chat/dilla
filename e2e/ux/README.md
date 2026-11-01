# UX audit scripts

The world and the screen shooters behind the alignment audit (web3.8's `ux-audit.md`, web3.9's AUDIT-2). Everything is written
to `target/ui-screens/web3.9/`; the repo root comes from the script's own location.

Build first: `npm run build -w @dilla/web`, `go build -o target/dilla-testhost ./cmd/dilla-testhost`, and the core wasm
(`internal/mlswasi/testdata/dilla_core_wasi.wasm`, `packages/core-wasm/pkg`).

1. `npm run ux:world -w @dilla/e2e` starts an empty test host (8611/8612, SFU 7988/7990) and builds the server through the client:
   Ada, Björn, Mira and Liv (offline), text, announcement and voice channels (the AFK channel is the built-in one; none is picked),
   a conversation, unread counts, a DM from Mira, Björn and Mira in `harbour`, and Ada's phone as a second device. Each browser
   keeps a CDP port (9611 up, the phone 9620); `state.json` is written when it prints `READY`. It holds until Ctrl+C.
2. `npm run ux:shots -w @dilla/e2e` shoots every step; `node e2e/ux/shots.mjs --list` names them, and
   `node e2e/ux/shots.mjs shell dm --size 1440 --theme signal` runs some. Files are `<d|p><width>-<theme>-<step>.png`.
3. `npm run ux:sheets -w @dilla/e2e` composes one contact sheet per width and theme into `sheets/`.

`run.mjs` is the runner a one-off step file uses: `node e2e/ux/run.mjs <ada|bjorn|mira|phone> <step.mjs> [args]`.
The audit's CSS-variable check (`vars.mjs`) is replaced by `npm run check:css`.
