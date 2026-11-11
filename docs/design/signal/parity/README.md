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

The tool's own tests: `node --test docs/design/signal/parity/shoot.test.mjs` (helpers and a local browser readiness check; no world or screenshots; not part of the root
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
- `prototype.steps` run in order after load: one of `click`, `rightClick`, `hover`, `waitFor` (selectors; default 30s, or `{ "waitFor": { "selector": ".d-poll-card", "timeout": 120000 } }` for a
  Bot post or other state reached on its own clock), `press` (a key), `type`
  (`[selector, text]`) or `wait` (ms) per step.
- `wheel: [selector, deltaX, deltaY]` moves the pointer over the target and sends native wheel input in pixels. Use it when the UI must observe manual scrolling (such as leaving the conversation's pinned end); it does not assign scroll offsets. Wheel's incidental hover is cleared unless the side also requests an explicit `hover` step.
- `clip` (either side) crops to the first element matching the selector; without it the whole viewport is shot.
- `story.id` is the Storybook story id (`<title-slug>--<story-slug>`, see `storybook-static/index.json`).
  `story.globals` defaults to `theme:signal;motion:reduced`.
- Both sides run with `prefers-reduced-motion: reduce` at deviceScaleFactor 2, wait for `document.fonts.ready` and 600 ms.
  A side can set `settleMs` to override its post-step delay (including zero for a short pending result); fonts are still awaited.
  The prototype's Google Fonts requests are intercepted and answered with `@font-face` rules built from the repo's own
  `@fontsource-variable/jetbrains-mono` and `@fontsource/dm-serif-display` files, so no network is needed and both sides
  use the same font files.
- Signed-in world pages wait for a visible app shell before steps. Every app side waits for the actual Signal theme before capture; no theme setting is overridden. Standing chat pairs explicitly wait for the seeded GIF button. Inside the visible capture region it also waits for required sidebar/account names and the online stat to load, while allowing blank custom status and zero online. Before capturing, the tool also waits for visible `aria-busy="true"` and “Loading…” copy to clear inside the crop (or viewport). Unrelated loading outside a crop does not block it. Incidental click hover is cleared; an explicit `hover` step preserves the requested state.
- A `clip` crop gets 8px of padding on every side (both frames), so edges, rings and shadows are visible.

## The rule

A pair matches when **colour, size, spacing, radius, type and copy** agree. The contrast fixes in the plan's Global
Constraints (for example `--edge` instead of the prototype's `#2A303A` on controls whose border is their only boundary,
and `fg-3` text never on `surfaceHi`), together with Jonas's explicitly recorded decisions below, are the only accepted deviations. Anything else that differs is a bug in the
story or component, not in the prototype.

## Shoot the running app instead of a story (`--app`)

A pair has `prototype` plus **either** `story` **or** `app`; one spec may mix both. An `app` side is the real web client:

```json
{ "name": "gap16-reminders-empty-due", "viewport": { "width": 1280, "height": 800 },
  "prototype": { "props": { "startAt": "app" }, "steps": [...] },
  "app": { "who": "ada", "path": "{server}", "steps": [{ "click": "role=button[name=\"Reminders\"s]" }], "clip": "...", "reset": "..." } }
```

- `locale` (optional): shoots the app under that language (a fresh context's locale, or a CDP locale override on the world's browser). Message times must read `20:27` under en-US too, so `examples/message-gutter.json` shoots the gutter under en-GB and en-US.
- `who`: `ada`, `bjorn`, `mira`, `phone` are the world's browsers, attached over CDP (their window is emulated at the
  pair's viewport and scale 2 and restored afterwards); `fresh` and `freshPhone` are new signed-out contexts at
  `state.publicUrl` (1280x800 and 390x844 touch), with the same supported virtual platform authenticator as the passkey e2e tests. Fresh contexts use `state.authUrl` (`localhost`) with a matching WebAuthn relying party, while connected world profiles retain numeric `state.publicUrl` for RTC/CSP compatibility. The browser’s conditional passkey request remains real.
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

The world, its builds and ports are in `e2e/ux/README.md`. Its wall clock opts out of forced per-second test-clock retention (`wallClockSweep: false`), and the shooter waits for natural TOTP counters instead of advancing that clock. This fixture adaptation avoids triggering the known Bot `E_PRUNED` scan failure on every tick; production retention and the underlying Bot bug are unchanged. `app/standing.json` is the five standing whole screens (desktop
chat, members open, voice focus, phone list, phone conversation); `app/wave-a-gaps.json` is the wave A states that were
never compared with the shipped client.

The prototype requests Lucide 0.460.0 `home.svg`, an unpublished static filename. The shooter resolves that exact URL to the pinned official `house.svg` asset: the same release exports `Home = House`. Its source and hash are in `app/fixtures/lucide-house-0.460.0.provenance.txt`. The prototype source stays unchanged, and captures wait for the visible Home glyph to finish loading.

**Pairs that change the world.** A pair marked `"mutates": true` sends messages, adds a device or role or switches a setting
(gap04, gap44, gap52, gap53, gap79, gap83). The tool runs `standing-voice-focus` first, before other standing frames reload Ada's page and can lose the live screen or Music fixture. Other `standing-*` screens follow, then other read-only pairs, then mutating pairs. Order stays stable inside each group. The
world cannot be rewound under live browsers (restoring the server would leave the browsers' own state ahead of it), so
**restart the world before shooting again**, and shoot `standing.json` before a spec with mutating pairs or on a fresh world.
Never point `--out` (the world's or the shooter's) into a tracked path: `state.json` and some PNGs hold throwaway passwords and
recovery keys.

The GIF pairs depend on the world's dummy Giphy key (set by `ux:world` through Server settings -> GIFs; without a key the GIF
button does not show). Giphy is unreachable, so only the picker frame is comparable. The standing voice-focus pair needs the
world's screen share (Mira shares in `harbour`, `state.miraShares`); the world and shooter fail explicitly if that fixture is missing. The phone is enrolled into #general before seeded messages, and the world seeds its topic, a hoisted Raid leaders role with the prototype’s Pin messages permission (and real DJ playback permission), and a thread with a reply. Poll-card steps wait up to 120 seconds for the actual Bot card. A bounded optional Music seed uses real provider tools and playback; `state.botPlays` and `state.botPlayError` record whether the voice Bot fixture is available. The Bot menu pair targets its voice tile, so a provider failure remains a reported capture failure.

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
`task-3` (sign-in only), `task-3b` (the replacement signup flow), `task-4`, `task-5`, `task-6`, `task-7` and `task-8`.
Task 8 also has two ReactionTooltip story pairs (Q6), using the existing `conversation-reactiontooltip--default` story. Build and serve `packages/ui/storybook-static` so `/iframe.html?id=conversation-reactiontooltip--default` is reachable, and pass `--storybook http://127.0.0.1:<port>` for those.

Do not pass `--judge` for the closeout. Read each pair and record **match**,
**differs**, or **accepted deviation**, with its file path and a reason, in
`task-9/results.md`. A capture failure is **differs** with its error and intended
path; it is not evidence of a visual match. Targets explicitly removed by a decision are absent from the manifest: the phone Status pair was dropped (accepted absence, Jonas 2026-10-30). Route differences to the controller.
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
has no invite link. The phone You tab has no Status menu. Shortcut keycaps follow the platform. Signup steps 4–6 drop the Sign-in row; steps 4 and 6 drop Back; step 5 (`task3b-code`) keeps Back to the choice. The code step has no passkey or backup links. The done step folds backup codes behind Show at every width, keeping the codes out of the DOM until disclosure (Jonas, 2026-10-31).
The following decisions also apply:

- Recovery-key actions keep **Print** and **Save as file**; **Copy** remains forbidden (Jonas, 2026-10-30).
- A manager keeps the new-channel **+** in the server header while the server has no categories, on desktop and phone (Ruling AB; Jonas, 2026-10-27, explicitly reaffirmed 2026-10-30). With categories it moves to their headers. The uncategorized capture world therefore shows this deliberate extra control.
- The sign-in code step offers Use a passkey only when the account has one and the browser can use it; unavailable routes are hidden (Jonas, 2026-10-30, Task 3 / passkey-after-password decision).
- The command picker lists all available real commands with their proposed icons, including commands absent from the prototype (Q2; Jonas, 2026-10-30, wave B plan §Decisions). Extra command rows are this explicit decision, not sample-data exclusions.
- Person menus omit Move to, Server mute and Disconnect until wave C.
- DM GIFs require a named shared-server privacy notice and account/device-local consent; only an explicit button press checks keys, and changing the proxy asks again. Privacy settings can revoke the choice (Jonas, 2026-10-31 security follow-up).
- The DM list keeps Message someone (✎) and the notification-default menu (⌄). DMs have no Threads button until wave C (Jonas, 2026-10-27/28).
- Keyboard settings list the prototype’s shortcuts first, then other real shortcuts such as Open search and Call the open DM. Keycaps and device names reflect the actual platform (“Ctrl K”, “System default”; Jonas, 2026-10-30).
- The role editor folds real extra permissions into the prototype’s four groups: Attach files, Add reactions, Read message history, Create invites, Kick members and the other supported permissions (Ruling AS; Jonas, 2026-10-29).
- Settings navigation keeps live extra sections **after the prototype’s entries** in each workspace. You keeps Appearance, Privacy and Diagnostics; Server keeps Channels, GIFs, Members, Self-assignable roles, Welcome messages, Join roles, Automoderation, Moderation log, Bans and Ownership (D2; Jonas, 2026-10-31).
- Overview shows the Server name row only to people with Manage server (D3; Jonas, 2026-10-31). The owner fixture therefore sees this row.
- Noise suppression defaults to Lightweight until the Automatic engine qualifies. Its selected state therefore has no Automatic hint or BETA tag (D8; Jonas, 2026-10-31).
- Poll dialog focus matches the prototype: the Question field has an accent border plus its 3px soft outer ring; answer fields retain their native-prototype accent border without that shadow. This replaces the earlier D6 border-only decision, which rested on a wrong premise (Jonas, 2026-10-31, round 3).
- Poll creation has no private Working card while its dialog is open (D10; Jonas, 2026-10-31). After Post poll, keep the private **Poll posted** success result alongside the poll card, although the prototype draws only the card (Jonas, 2026-10-31, round-3 explicit decision).
- Reduced-motion reply jumps keep the prototype's initial tint and accent bar without animation (Jonas, 2026-10-30).
- Phone buttons follow each prototype context's actual height; the former global 44px minimum is removed, including inside the phone shell. Preserve the prototype's explicit auth and dialog sizes rather than impose a blanket minimum (Jonas, 2026-10-31, round 3).
- Phone inputs keep the [phone grammar's 16px text](../README.md) to prevent iOS focus zoom, including sign-in and poll fields.
- The Bot settings page keeps its own layout: the identity row is a card above the features card, and the features are finer-grained (Welcome, Roles, Announcements, Warnings, Message removal, Kick, Ban) than the prototype's single card with Moderation (Jonas, 2026-11-05, parity sweep).
- Body text weight is the prototype's 400 (`--weight-body`), not 420; keycaps read ⌘ and ⌥ on macOS and Ctrl and Alt on Windows and Linux in Settings → Keyboard, as in the top bar; this browser's row in Settings → Devices carries no "Browser · Chrome" tag; the Music page's instance defaults are volume 50 and leave when idle 15m, while saved settings are kept; ban shows the optional "reason?" chip (Jonas, 2026-11-05, parity sweep).
- The kick confirm has neither a "reason?" chip nor a Reason row: kick records no reason (kickMember carries none), so either would take or show a reason and drop it (Jonas, 2026-11-05, Task 11 review). Ban keeps both, its Reason row reading "none given". Contract: `packages/web/src/screens/commands/CommandHost.tsx` (the confirm dialog), `packages/client-core/src/commands/registry.ts` (kick's options are the user alone).

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

### Desktop beta.5 accepted deviations (Jonas, 2026-11-11)

- **Always show the dock:** the voice dock (call card and compact Now playing) stays visible while the voice channel's own stage is on screen, instead of the prototype's showDock hiding it (§5.6). The connected phase also draws no animated signal bars (§5.4), and neither does the top bar's voice chip, which keeps only its text (the `d-bar` keyframes stay for the CallTile and Now playing meters; the unused `--duration-bar` token is gone). Parity shots of the voice stage with the dock (standing-voice-*) differ from the prototype by design; the images are not re-shot yet.
- **Bot card ⋯ button:** a ⋯ button beside Restart on the Bot card (Settings → Bot) opens Rename, Refresh status and Disable, which the prototype's card draws no button for; the row's context menu still opens the same menu (Ruling X). Story `BotMoreMenuOpen`.

- **Thumbnail screen picker (approved addition):** the desktop app's screen share opens a picker with live thumbnails on macOS, Windows and Linux X11 (Wayland keeps the system picker). The prototype draws no picker, so there is no pair to match.
- **Private Bot replies auto-dismiss after 60 s:** a finished private reply goes away a minute after it finishes; a reply still working or offering Retry stays, and pointing at it or focusing it pauses the countdown. The prototype keeps them until dismissed.
- **Calm "Voice moved to your other device." notice:** when another of the user's devices takes over voice, the device that lost it shows this line and does not rejoin. The prototype has no such state.

### Task 6 accepted deviations (Jonas, 2026-10-30)

| Pairs | Accepted difference from the raw prototype | Decision |
|---|---|---|
| `private-result-cancelled-{kick,ban}-{desktop,phone}` | Cancel closes command mode and shows a private “Not confirmed” result, although the raw prototype returns to its command chips. | Q4 in the task-9 brief explicitly pulls the confirm modal and “Not confirmed” into wave B. |

### Task 7 intentional deviations (Jonas, 2026-10-31)

- **Q-A:** the question counter counts Unicode code points, matching the server's rune limit. The prototype counts UTF-16 code units, so supplementary characters such as emoji count differently. The displayed counter and the 300-character validation deliberately agree with the server.
- **Q-B:** an uncertain vote retries only from the answer that triggered it; the other answers are inert until it settles. Copy: “Your vote may not have counted. Tap it again to retry.”
- **Q-C:** a capped multi-choice voter walk displays the distinct count read so far as “{n}+ voters”. Reads are bounded to ten pages per poll and ten per answer, with at most two answer walks in flight.

Task 8 upload captures can use `@bytes/26214401/huge.bin` in an upload step. The runner creates the bytes in memory, so a 25 MB fixture is not stored in Git. An app side with `holdUploads: true` holds blob uploads until its frame is captured, then releases them; the waiting-toast state is reached by picking a file and pressing Send normally. ReactionTooltip pairs use a UI story under Q6 (web wiring is wave C).

### Task 9d audit corrections (2026-11-01)

The parity world now saves the server's inactive timeout as 15 minutes through Server Settings →
Overview, so `task-4/t4-afk-phone` uses the prototype's “After 15 minutes”. Apply these corrections
when regenerating the 9d report:

| Row | Classification | Reason |
|---|---|---|
| `task-4/t4-afk-phone` | match | The world's AFK timeout is 15 minutes, matching the prototype's copy. Header, empty-state treatment and Go AFK button size agree; the prototype phone bezel is excluded. |
| `task-5/gap102-phone-settings-signin` | accepted deviation | This account has an authenticator and no passkey, so its removal guard and Add passkey state differ from the prototype's factor inventory. Phone navigation, Sign-in heading, factor-row geometry/icons and 32px action buttons agree. |
| `wave-a-gaps/gap102-phone-settings-signin` | accepted deviation | This account has an authenticator and no passkey, so its removal guard and Add passkey state differ from the prototype's factor inventory. Phone navigation, Sign-in heading, factor-row geometry/icons and 32px action buttons agree. |

`parity/task-9d/results.md` and `counts.json` are absent from this checkout. No report or counts
were regenerated, and historical screenshots were not relabeled as new captures. The AFK change
moves one row to match; aligning the duplicate Sign-in rows moves one row to accepted deviation,
so these two corrections leave the audit's aggregate counts unchanged (42 match, 130 accepted
deviation, 0 differs, 172 total).
