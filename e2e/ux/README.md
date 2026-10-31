# UX audit scripts

The world and the screen shooters behind the alignment audit (web3.8's `ux-audit.md`, web3.9's AUDIT-2) and the design
parity tool (`docs/design/signal/parity`, `--app`). Everything is written to `target/ui-screens/signal-b/` unless `--out` says
otherwise; the repo root comes from the script's own location.

Build first: `npm run build -w @dilla/web`, `go build -o target/dilla-testhost ./cmd/dilla-testhost`,
`go build -o target/dillabot ./cmd/dillabot`, and the core wasm (`internal/mlswasi/testdata/dilla_core_wasi.wasm`,
`packages/core-wasm/pkg`). The world never builds the web client or the wasm core (it starts the host with `prebuilt: true`, so
two worlds in one tree cannot race on `packages/web/dist`); it checks the four artifacts first and exits 2 naming the build
command of each one that is missing. The Bot's own helper binary is still built incrementally by `startTestHost`.

**Never point `--out` into a tracked path.** `state.json` (passwords, recovery keys, TOTP secrets) and some shots (recovery-key
screens) hold throwaway secrets; the default `target/ui-screens/signal-b` is git-ignored.

1. `npm run ux:world -w @dilla/e2e -- [--base <n>] [--out <dir>]` starts an empty test host with the Bot runtime
   (`startTestHost` from `e2e/web/support/host.ts`, bundled with esbuild so plain node can load it) and builds the server through
   the client: Ada, Björn, Mira and Liv (offline), text, announcement and voice channels (the AFK channel is the built-in
   one), the Bot installed through Server settings -> Bot with Polls, Reminders and Music on (the op 25 command-discovery fix is included), Ada's phone enrolled as a second device **before** the #general history, a conversation, a real thread with a reply, unread counts, a DM
   from Mira, Björn and Mira in `harbour`, a channel topic and a hoisted Raid leaders role assigned to Björn and Mira with Music control. Ports from `--base n` (default 8611): host
   `n`/`n+1`, SFU `n+2`/`n+4`, browsers' CDP from `n+10`, Ada's phone `n+19`. `state.json` (in `--out`) is written when it
   prints `READY`; it holds the people with their TOTP secrets, recovery keys and passwords, `invitePath`, `serverPath`,
   `voicePath`, `afkPath`, `botName`, `botPlays` / `botPlayError` and the CDP ports. Music playback is requested once from Mira’s persistent voice/share session only when yt-dlp, ffmpeg and bwrap are present; Ada stays independent, and the fixture checks that the Bot tile survives her reload; a provider failure is recorded and does not fail the world. It holds until Ctrl+C. Two worlds can run side by side with different
   `--base` and `--out`. Mira shares a screen in `harbour` (a fake display track), so the stage opens in the share focus layout
   (`state.miraShares`). The world waits for call phase `in` and a connected voice dock before sharing, clicks only a visible stage Join voice action when needed, and waits for the screen tile on both participants. A failed call writes `voice-<who>-failure.json` and `.png` with its phase and stage/body reason; a missing call or share fails the world; the parity shooter refuses voice-focus pairs without this fixture. Ada's server gets a dummy Giphy key (`abcdEFGH12345678`, set by the world itself through Server settings
   -> GIFs) so the composer's GIF button shows; Giphy is unreachable, so the pickers stay on "searching". A failed phone sign-in
   fails the world (`state.ready=false`, `state.notReady`, exit 1) instead of printing READY. A shoot of mutating pairs (see
   `docs/design/signal/parity/README.md`) changes the world: restart it before shooting again.
Connected profiles use `publicUrl: http://127.0.0.1:<base>` so their RTC signalling matches the testhost's numeric URL and the page CSP. Fresh authentication captures use the separate `authUrl: http://localhost:<base>` and matching WebAuthn origin, because numeric loopback is not a valid Chromium relying-party hostname. Control and Bot transport remain on numeric loopback. The world phone and the parity shooter’s fresh auth pages use the existing CDP virtual platform authenticator; conditional requests retain real browser behavior (the world phone stays numeric; its internal sign-in snapshots can show the invalid-RP banner before password submission). Invite, server and reset paths are relative; only fresh-page expansion uses `authUrl`. Shipping CSP is unchanged.

The UX world uses `advanceClockWithWall: true, wallClockSweep: false`: its one-second clock posts include `sweep: false`. This keeps signed Bot timestamps aligned with real time while deferring the **forced test-clock retention sweep**. Hooks and the election watchdog still run; ordinary server background retention is unchanged. The fixture avoids repeatedly provoking the Bot's known `E_PRUNED` scan failure; it does not fix that Bot bug or change production retention semantics. Other test hosts and ordinary `Host.Advance` calls still sweep by default. Visual TOTP draws use wall-clock mode, waiting up to 30 seconds for an unused natural counter instead of posting a synthetic clock advance.

2. `npm run ux:shots -w @dilla/e2e` shoots every step; `node e2e/ux/shots.mjs --list` names them, and
   `node e2e/ux/shots.mjs shell dm --size 1440 --theme signal` runs some. Files are `<d|p><width>-<theme>-<step>.png`.
3. `npm run ux:sheets -w @dilla/e2e` composes one contact sheet per width and theme into `sheets/`.

`run.mjs` is the runner a one-off step file uses: `node e2e/ux/run.mjs <ada|bjorn|mira|phone> <step.mjs> [args] [--out <dir>]`.
The audit's CSS-variable check (`vars.mjs`) is replaced by `npm run check:css`.
