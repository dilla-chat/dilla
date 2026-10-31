# Prototype-vs-story parity

`shoot.mjs` opens the Signal prototype (`../prototype/Dilla Signal Prototype.dc.html`, from `file://`) and a built
Storybook story at the same viewport, and writes three PNGs per pair: the prototype, the story, and the two side by side
(prototype left, story right, 1px grey divider).

## Run it

```
npm run build-storybook -w @dilla/ui
npx http-server packages/ui/storybook-static -p 6006 --silent &
node docs/design/signal/parity/shoot.mjs <spec.json> <outDir> [--storybook http://127.0.0.1:6006]
```

Output goes to `<outDir>/<name>.prototype.png`, `<name>.story.png` and `<name>.pair.png`. Keep `<outDir>` outside the
repo. One line is printed per pair (`ok` or `FAIL`); the exit code is non-zero if any side fails to load.
`examples/shell.json` is a working spec (desktop shell and phone shell).

The tool's own tests: `node --test docs/design/signal/parity/shoot.test.mjs` (pure helpers; not part of the root
`npm test`, there is no pattern for design-tooling tests there).

## Write a spec

A spec is a JSON array of pairs:

```json
{
  "name": "channel-row-active",
  "viewport": { "width": 1280, "height": 800 },
  "prototype": {
    "props": { "startAt": "app" },
    "steps": [{ "click": "text=general" }, { "hover": ".row" }, { "press": "Escape" }, { "type": ["input", "hi"] }, { "wait": 300 }],
    "clip": ".some-selector"
  },
  "story": { "id": "primitives-channelrow--active", "globals": "theme:signal;motion:reduced", "clip": ".d-chrow" }
}
```

- `viewport`: 1280x800 for desktop, 390x844 for phone.
- `prototype.props` are the prototype's component props (`startAt: 'signin' | 'app'`, `device: 'auto' | 'phone'`).
  The prototype reads them from the `data-props` attribute of its `script[data-dc-script]` (`parseDataProps` in
  `support.js`; each entry's `default` becomes `this.props`). The tool rewrites that attribute before the prototype's
  scripts run (a MutationObserver installed with `addInitScript`), so the file on disk is never modified.
- `prototype.steps` run in order after load: one of `click`, `rightClick`, `hover`, `waitFor` (selectors; `waitFor` waits up to 30s for a
  state the prototype reaches on its own clock, such as a speaker), `press` (a key), `type`
  (`[selector, text]`) or `wait` (ms) per step.
- `clip` (either side) crops to the first element matching the selector; without it the whole viewport is shot.
- `story.id` is the Storybook story id (`<title-slug>--<story-slug>`, see `storybook-static/index.json`).
  `story.globals` defaults to `theme:signal;motion:reduced`.
- Both sides run with `prefers-reduced-motion: reduce` at deviceScaleFactor 2, wait for `document.fonts.ready` and 600 ms.
  The prototype's Google Fonts requests are intercepted and answered with `@font-face` rules built from the repo's own
  `@fontsource-variable/jetbrains-mono` and `@fontsource/dm-serif-display` files, so no network is needed and both sides
  use the same font files.
- A `clip` crop gets 8px of padding on every side (both frames), so edges, rings and shadows are visible.

## The rule

A pair matches when **colour, size, spacing, radius, type and copy** agree. The contrast fixes in the plan's Global
Constraints (for example `--edge` instead of the prototype's `#2A303A` on controls whose border is their only boundary,
and `fg-3` text never on `surfaceHi`) are the only accepted deviations. Anything else that differs is a bug in the
story or component, not in the prototype.

## Shoot the running app instead of a story (`--app`)

A pair has `prototype` plus **either** `story` **or** `app`; one spec may mix both. An `app` side is the real web client:

```json
{ "name": "gap16-reminders-empty-due", "viewport": { "width": 1280, "height": 800 },
  "prototype": { "props": { "startAt": "app" }, "steps": [...] },
  "app": { "who": "ada", "path": "{server}", "steps": [{ "click": "role=button[name=\"Reminders\"s]" }], "clip": "...", "reset": "..." } }
```

- `who`: `ada`, `bjorn`, `mira`, `phone` are the world's browsers, attached over CDP (their window is emulated at the
  pair's viewport and scale 2 and restored afterwards); `fresh` and `freshPhone` are new signed-out contexts at
  `state.publicUrl` (1280x800 and 390x844 touch).
- `path` is opened before the steps. `{invite}` expands to `state.invitePath`, `{server}` to `#general` of the world's
  server, `{community}` to the server's own path (the phone's server list). Without a `path` the page stays where it is.
  After the capture the tool presses Escape twice and reloads `reset` (default: `path`, else the URL before the steps).
- Extra step kinds on top of the prototype ones: `{ "goto": "/path" }`, `{ "totp": [selector, who] }` (fills the next
  authenticator code of `state.people[who]`), `{ "totpFrom": [keySelector, codeSelector] }` (reads a setup key shown on the page, for an account
  the pair signs up itself, and fills its next code; wave B task 3b), `{ "upload": [selector, file...] }` (files relative to the repo, see
  `app/fixtures/`), `{ "dispatch": selector }` (a click event without hit testing). Typed text expands
  `{password:who}` and `{recoveryKey:who}` from `state.people`.
- Selectors on the app side can use Playwright roles: `role=button[name="Reminders"s]` (`s` = exact, `i` = case-insensitive).
- Output: `<name>.prototype.png`, `<name>.app.png`, `<name>.pair.png` (app on the right).

```
npm run ux:world -w @dilla/e2e -- --base 8611 --out target/ui-screens/signal-b   # waits for READY, keeps running
node docs/design/signal/parity/shoot.mjs docs/design/signal/parity/app/standing.json <outDir> --app [target/ui-screens/signal-b/state.json]
```

The world, its builds and ports are in `e2e/ux/README.md`. `app/standing.json` is the five standing whole screens (desktop
chat, members open, voice focus, phone list, phone conversation); `app/wave-a-gaps.json` is the wave A states that were
never compared with the shipped client.

**Pairs that change the world.** A pair marked `"mutates": true` sends messages, adds a device or role or switches a setting
(gap04, gap44, gap52, gap53, gap79, gap83). The tool runs those after all other pairs of a spec, and says so at the end. The
world cannot be rewound under live browsers (restoring the server would leave the browsers' own state ahead of it), so
**restart the world before shooting again**, and shoot `standing.json` before a spec with mutating pairs or on a fresh world.
Never point `--out` (the world's or the shooter's) into a tracked path: `state.json` and some PNGs hold throwaway passwords and
recovery keys.

The GIF pairs depend on the world's dummy Giphy key (set by `ux:world` through Server settings -> GIFs; without a key the GIF
button does not show). Giphy is unreachable, so only the picker frame is comparable. The standing voice-focus pair needs the
world's screen share (Mira shares in `harbour`, `state.miraShares`).

**What is not judged.** The app's sample data (server Midgard Crew; Ada, Björn, Mira, Liv; timestamps, device names,
counts) differs from the prototype's (Mira, Ada, Björn, Kari, Ole, Glenn playing music). Compare styling, structure and
copy, not names. Other known capture differences: the prototype draws a phone frame around its 390px shots, uploads in
the app finish instantly (the prototype's progress bar is mid-way at capture), Giphy is unreachable from the world (the
picker shows its error state), and reduced motion (both sides) replaces animations such as the reply-jump flash.

### Closing a wave with app captures

Build the prerequisites in `e2e/ux/README.md`, then start one world on a private
base port block. Pass an **absolute** `--out` path: npm workspace scripts run
from `e2e/`, while a shooter started at the repository root resolves relative
paths from that root. Wait for `READY` before driving any world browser.

```
npm run ux:world -w @dilla/e2e -- --base 9311 --out "$PWD/target/ui-screens/task-9-world"
node docs/design/signal/parity/shoot.mjs docs/design/signal/parity/app/standing.json .superpowers/sdd/2026-10-30-signal-wave-b/parity/task-9/standing --app target/ui-screens/task-9-world/state.json
```

Shoot one spec at a time. Stop with Ctrl+C and restart the world after a spec
with mutating pairs. Use a subdirectory per spec: several specs repeat the
standing pair names. The closeout set is `standing`, `wave-a-gaps`, `task-2`,
`task-3`, `task-3b`, `task-4`, `task-5`, `task-6`, `task-7` and `task-8`.
Task 8 also has two ReactionTooltip story pairs (Q6); build and serve Storybook
and pass `--storybook http://127.0.0.1:<port>` for those.

Do not pass `--judge` for the closeout. Read each pair and record **match**,
**differs**, or **accepted deviation**, with its file path and a reason, in
`task-9/results.md`. A capture failure is **differs** with its error and intended
path; it is not evidence of a visual match. If its target was explicitly removed
by an accepted decision (such as phone Status), record that accepted absence and
the capture failure together. Route differences to the controller.
Keep world state and screenshots containing throwaway secrets in ignored paths.

The user-doc shooter can reuse a READY world with the managed Bot:

```
npm run docs:shots -w @dilla/e2e -- --wave-b-state "$PWD/target/ui-screens/task-9-world/state.json"
```

This adds the command list, chips, private result, poll dialog/card and sign-in
code step to `docs/user/screenshots`. It posts a reminder and a poll, so restart
the world before further parity captures. The ordinary `docs:shots` command
refreshes the rest of the guide's screenshots using its own host.

### Other accepted deviations recorded by Jonas

The wave B ledger records these alongside Q1–Q10 and the task-specific rulings
below: recovery-key entry keeps the character count, 52-character requirement
and disabled button; authenticator setup keeps the QR code, setup key and hint;
removing another browser keeps recovery-key confirmation; the sign-in done step
has no invite link. The phone You tab has no Status menu. Shortcut keycaps follow the platform. The ledger accepts removing Back and Sign in on signup steps 4–6. On phones the done step folds backup codes behind Show.
These are accepted only for the named state; unrelated differences still need
routing.

## Judge a pair (TypeSafe, advisory only)

`judge.mjs` asks TypeSafe's Jev (`jev-1.14.0`, pinned) the same questions about the prototype frame and the app (or story)
frame of a pair. **It is advisory only**: it points a reviewer at what to look at, next to pixel diff and the reviewer's
eye, and never gates a merge.

```
node docs/design/signal/parity/judge.mjs <spec.json> <shotDir> [--out dir] [--only a,b] [--budget 0.50] [--dry-run] [--reuse]
node docs/design/signal/parity/judge.mjs --replay <outDir>/<name>.request.<side>.json   # resend a logged request byte for byte
node docs/design/signal/parity/shoot.mjs <spec.json> <outDir> --app --judge            # shoot, then judge the pairs that shot ok
```

### Reading the summary

`judge-summary.md` has one row per pair. The **judge** column is the one to read:

| judge | Meaning | What to do |
|---|---|---|
| `ok` | Every element's yes/no answers agree on both frames | Nothing from the judge; still glance at the pair |
| `CHECK a, b` | Elements `a`, `b` differ: missing, extra, lost or gained state (selected, focused), different fixed copy | Open the pair and confirm; the `reasons` column says which |
| `unsure c` | The answers for `c` were between 0.3 and 0.7 on one side | Look at `c` yourself |
| `leads: d` | `d`'s box moved or resized by more than twice the tolerance | Look at `d`, measure with pixel diff |
| `skip (secret)`, `no list`, `ERROR` | Not sent (it types a secret), no `judge` block, or a failed call (exit code non-zero) | - |

**Verdicts come only from yes/no and choice answers** (presence, state, copy, tone). **Geometry is a lead, never a
verdict.** Box answers come back in steps of 0.01 of the sent image (12.8 x 8 CSS px on a desktop frame, 3.9 x 8.4 on a
phone frame), move by a step between identical requests and sometimes land on the wrong element. The `leads` column gives
each delta in px and in Box steps: `strong` is more than twice the tolerance (two steps), `weak` between one and two.
A strong lead of a few steps on a plausible element is worth measuring; a lead of dozens of steps on an element whose
answers agree is usually a Box on the wrong element. Shifts under about 16-26 px are below what the judge can see.

### Writing a `judge` block

A pair's `judge.elements` (see `app/wave-a-gaps.json`, `app/standing.json` and `app/task-2.json`) lists what to ask about.
Each element gets a presence Noul and a Box on both images, plus optional `state` Nouls, a fixed-UI `copy` Noul and a
`tone` Choice. Rules (checked by `validateJudge`; details in `judge-questions.mjs`):

- **Names** describe UI roles, never sample data (no Mira, no Midgard Crew), and pick out one thing on both frames. Name a
  text line by what it says ("a line of text saying that nobody is in the channel yet"), not by its colour or place ("the
  grey line under ..." stayed unsettled). When the app may show the thing twice (a second code block), say which one.
- **`data: true`** for elements whose size or place follows sample data (code blocks, message lists): presence-only.
- **Phone pairs**: every positioned element has an `anchor` or an `edge` (`top` or `bottom`). The prototype's phone bezel
  insets the top and the bottom differently, so the two groups are aligned separately.
- **`small: true`** for icons, dots and badges under about 16 CSS px, with a `region` (CSS px) for a close-up crop.
- `expect: "absent"` asks for something that must not be there; `anchor` aligns position on another element; `accepted`
  marks a decided deviation (reported as accepted, not as a difference).

### Data, key, files, cost

- **Never sent**: any pair that types a password, recovery key or code, fills a TOTP, or waits for the recovery-key screen
  (`secretReason`, a rule in code).
- **Key**: read at run time from `~/.config/typesafe/api-key` (or `TYPESAFE_API_KEY_FILE`, else `TYPESAFE_API_KEY`), sent
  only in the `Authorization` header; request logs carry `Bearer [REDACTED]`.
- **Output** (in `--out`, default `shotDir`; keep it outside the repo): `<name>.<side>.judge.png` (the bytes sent),
  `<name>.request.<side>.json`, `<name>.response.<side>.json`, `<name>.verdict.json`, `judge-summary.md|json` and
  `judge-ledger.json` (spend from the API's own `usage`). `--budget` (default $0.50) stops before the next request once the
  run has spent it; `--dry-run` prepares and logs without sending; `--reuse` decides again from stored answers to
  byte-identical requests.
- **Cost** (measured 2026-10-30, $0.042 per million input tokens): a desktop image is about 4,200 tokens, a phone image
  about 3,200, a close-up crop about 1,100. **A pair costs about $0.0003-0.0005** (7,000-12,000 tokens); a run over the 12
  judged pairs costs about $0.0045, and every app pair in the specs would be about $0.02-0.03.
- Tests: `node --test docs/design/signal/parity/judge.test.mjs` (pure; no network).

### Task 6 accepted deviations (Jonas, 2026-10-30)

| Pairs | Accepted difference from the raw prototype | Decision |
|---|---|---|
| `private-result-confirm-kick-desktop`, `private-result-confirm-kick-phone` | The kick modal omits “Duration —” and “Reason none given”. Its Logged row remains. | Q4 / M2: kick has no reason field until wave C; duration is shown only for ban. This is intentional, not a parity defect. |

### Task 7 intentional deviations (Jonas, 2026-10-31)

- **Q-A:** the question counter counts Unicode code points, matching the server's rune limit. The prototype counts UTF-16 code units, so supplementary characters such as emoji count differently. The displayed counter and the 300-character validation deliberately agree with the server.
- **Q-B:** an uncertain vote retries only from the answer that triggered it; the other answers are inert until it settles. Copy: “Your vote may not have counted. Tap it again to retry.”
- **Q-C:** a capped multi-choice voter walk displays the distinct count read so far as “{n}+ voters”. Reads are bounded to ten pages per poll and ten per answer, with at most two answer walks in flight.

Task 8 upload captures can use `@bytes/26214401/huge.bin` in an upload step. The runner creates the bytes in memory, so a 25 MB fixture is not stored in Git. An app side with `holdUploads: true` holds blob uploads until its frame is captured, then releases them; the waiting-toast state is reached by picking a file and pressing Send normally. ReactionTooltip pairs use a UI story under Q6 (web wiring is wave C).
