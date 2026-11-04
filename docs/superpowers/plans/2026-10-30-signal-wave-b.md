# Signal wave B: real-app parity, command picker, the seven differs, polls — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The shipping web client matches the Signal prototype in the states wave A never compared, gains the
prototype's `/` command picker with inline field chips and private results, its New poll dialog and poll card, and
fixes the seven wave A screens known to differ — all on backends that already exist.

**Architecture:** Task 1 adds a parity mode that shoots the *running* web app (the e2e test host serving
`packages/web/dist`, driven like `e2e/ux/world.mjs`) against the prototype, and every later task proves its screens
with it. New visual pieces live in `packages/ui` (tokens only, stories, `check-stories` for computed style); the web
client wires them to the existing command registry (`packages/client-core/src/commands/registry.ts`), command host
(`packages/web/src/screens/commands/`), bot utilities (`createBotUtilities`: capabilities, polls, reminders) and the
voice/auth screens. No server, core, protocol or Go change: a task that finds it needs one stops and reports
NEEDS_CONTEXT (backend gaps are wave C).

**Tech Stack:** TypeScript, React 19, Vite, Storybook 10, vitest, Playwright 1.63, the Go test host
(`target/dilla-testhost`) with the Bot runtime (`target/dillabot`).

**Spec:** `docs/design/signal/README.md` (§6.1 command picker, §6.2 private result, §6.3 poll card, §7.2 new
components, §9 APIs; **its §10 token table wins**), `docs/design/brief.md` (merged Signal brief) and
`docs/design/brief-signal.md` (handoff record). Reference build: `docs/design/signal/prototype/Dilla Signal
Prototype.dc.html` (`startAt=app` / `startAt=signin`, `device=phone`); the command picker is `CMDS`, `cmdMatches`,
`sugsFor`, `runCommand`, `exec` and the composer template (lines ~326–350, ~855–1010, ~1320–1360); the poll dialog
is lines ~528–545 and `pd` (~1380); the poll card ~270–286 and `pollVals`; the private result ~314 and `result()`.
Decisions and rulings: `.superpowers/sdd/2026-10-27-signal-wave-a/progress.md` (do not relitigate) and Jonas's wave B
decisions of 2026-10-30 (recorded in the header of this plan's ledger
`.superpowers/sdd/2026-10-30-signal-wave-b/progress.md`, which the controller starts). Coverage input:
`.superpowers/sdd/2026-10-27-signal-wave-a/parity-coverage-audit.md`.

**Process.** This plan comes first, then SDD subagents build and verify task by task (plan → implementer → task
review → fixes). Pixel parity with the prototype is the acceptance condition of every task; any deviation is asked of
Jonas first (implementer reports NEEDS_CONTEXT, the controller asks him with AskUserQuestion). Models: **sonnet** for
fully specified tasks (1, 4, 5, 6, 7, 8, 9's code), **opus** for Task 2 (replaces a stateful, idempotent execution
flow) and Task 3 (sign-in factor steps, authentication), and **opus for every task review** (parity is a key review);
haiku only for Task 9's prose.

### Wave B scope check: what wave A already shipped from the B list

| B-list item (wave A plan, "Waves") | status | where it shipped |
|---|---|---|
| NowPlaying ×3 | **shipped in A** (Jonas 10-27, Task 8b) | `packages/ui/src/NowPlaying`, dock row, stage bar, Glenn's card |
| RemindersPopover with tabs | **shipped in A** (Jonas 10-27, Task 6b) | `packages/ui/src/RemindersPopover` (Due / Upcoming); empty state never compared → Task 1 pair |
| CodeBlock | **shipped in A** (Ruling AF, Task 7) | `packages/ui/src/CodeBlock`; "Copied" never compared → Task 1 pair |
| SharePicker shell | **shipped in A** (Jonas 10-28, Task 8) | `packages/ui/src/SharePicker`, full options |
| SensitivityMeter UI | **shipped in A** (Task 10b) | `packages/ui/src/SensitivityMeter`; mic test running never compared → Task 1 pair |
| StageFilmstrip | **shipped in A** inside `CallStage` (focused layout strip, Task 8) | whole-voice-desktop-focus matches; no separate component (YAGNI, no visible change) |
| PersonContextMenu | **partly**: volume, Mute for me, Copy username (Jonas 10-28) | the rest of "what exists" → **Task 4** |
| CommandPicker restyle into `ui` | open | **Task 2** |
| BotResult inline card | open | **Task 6** |
| ReactionTooltip shell | open | **Task 8** (see Questions, Q6) |
| Polls (dialog + card; Jonas 10-30) | open | **Task 7** |

Wave C stays out: kick/ban confirm modal and "Not confirmed" (unless Q4 pulls it in), drag-to-move, Glenn move /
disconnect, server mute / move / disconnect, reactor identities, reminder snooze, 10 files per message.

## Global Constraints

- **Pixel parity, signal theme** (Jonas 2026-10-27, restyle-rules §0): match the prototype exactly — control borders
  `--control-edge` family (`#2A303A` / `#252A33` / `#3A404A` as the prototype draws them), solid status dots,
  prototype phone sizes, no visible controls the prototype lacks (extra functions go to invisible routes: context
  menus, shortcuts), text colours as the prototype even below 4.5:1 (scoped story-level axe exception naming the
  element, Ruling Y). Light and high-contrast keep accessible values. The only accepted deviations are sample data
  and features deferred to wave C; everything else is asked of Jonas first.
- **The prototype wins over README prose** where they disagree (Ruling M); copy is final and verbatim from the
  prototype (curly quotes and apostrophes as drawn, `’`, `“ ”`, `…`), sentence case (Ruling AJ, T, AA); fix only real
  grammar errors (Ruling AN).
- Every value comes from a token; no hex, rgb or px colour literal in component CSS; `npm run check:css` green. A
  prototype value the scale lacks becomes a token (Ruling K) — never snap to the nearest one. Token additions go in a
  `// Task <N>` block at the end of each object in `packages/design-tokens/src/tokens.ts` with their test, then
  `npm run build -w @dilla/design-tokens`.
- Real token names, not brief names: `--fg-strong #B9BFC8`, `--fg-2 #8C939E`, `--fg-3 #767D89`, `--fg-4 #5C636E`,
  `--fg-body #D5DAE1`, `--ok` (live), `--surface-hi` (selected), `--hover #14171C`, `--line-strong #2A303A`,
  `--ease-out`. Count badges use `--mention` (Ruling O).
- Icons only from `packages/ui/src/Icon/icons.ts` (Lucide 1.57.0). A missing one is vendored from the
  `lucide-static@1.57.0` tarball (Ruling AJ); if 1.57.0 has no such icon, stop and report NEEDS_CONTEXT.
- Phone = shell width < 770px; inputs 16px text (no zoom); prototype phone sizes; keyboard hints hidden on phone
  (prototype `dLabels`).
- Story titles use the repo prefixes (`Primitives/`, `Shell/`, `Conversation/`, `Voice/`, `Settings/`, `Search/`,
  `Media/`, `Form/`, `Ceremony/`); stories use no timers; a story receives exactly the props `packages/web` passes
  for that state (restyle rule 3d).
- Layout and computed-style assertions go in `packages/ui/scripts/check-stories.mjs` (real Chromium), never vitest
  (Ruling C). Vitest is for behaviour, DOM and attributes.
- Every seeded fixture and every paired app state must be reachable from the shipping client.
- Silent encryption and the brief's exclusion list stay as in `docs/design/brief.md`.
- No backend change in wave B (no Go, Rust, protocol, migration). A needed one → NEEDS_CONTEXT.
- **Gates, every task**, from the worktree root, simple single commands (the worktree guard refuses `cd x && y`):
  `npm test` (root), `npm run build -w @dilla/web`, `npm run build-storybook -w @dilla/ui` then
  `npm run test:storybook:ci -w @dilla/ui` (check-stories), the task's named e2e
  (`npm run test:e2e:web:chromium -w @dilla/e2e -- <specs>` / `test:e2e:voice:chromium`), and `go test ./...` only
  if Go changed (it should not). A lane task runs everything except browser e2e; the controller runs its e2e in the
  worktree after merging (lane-rules).
- **Parity, every task**: the task's app-parity spec in `docs/design/signal/parity/app/task-<N>.json`, shot with
  Task 1's app mode into `.superpowers/sdd/2026-10-30-signal-wave-b/parity/task-<N>/`, whole screens (desktop
  1280×800 and phone 390×844) of every state the task touches plus the five standing whole screens (desktop chat,
  members open, voice focus, phone list, phone conversation). The implementer reads every `.pair.png` after the last
  change and reports a table `pair → match | deviates: <what, which task owns it>`. Never `match` unseen.
- Parallel lanes follow `.superpowers/sdd/2026-10-27-signal-wave-a/lane-rules.md` (own copy, own git, own Storybook
  port, own parity world base from Task 1's `--base`). Disjointness is stated per task below.
- Reports: `.superpowers/sdd/2026-10-30-signal-wave-b/task-<N>-report.md`; commits end with
  `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`; implementers never dispatch subagents.

## Review Focus

1. **A pasted or autofilled code** ("123 456", "123456\n", iOS/Android one-time-code autofill) must fill all six
   boxes, drop non-digits and submit once — test in Task 3 (`CodeBoxes.test.tsx`, `SignInFactor.test.tsx`).
2. **Enter pressed twice, or Enter while a run is pending** must start one invocation; a retry of an uncertain run
   reuses its `requestId` exactly as `CommandHost` does today — test in Task 2 (`CommandPicker.test.tsx`
   `enter twice runs once`, `retry keeps requestId`).
3. **Esc with filled chips** must leave command mode with the draft `/remind in 1 hour bring two ropes` (values
   joined, empty ones dropped), and Backspace in an empty first chip must leave with `/remind` — test in Task 2.
4. **A capability switched off while the picker is open** (owner turns Polls off in another tab) must drop the
   command from the list on the next capability read, and running an already-open chip form for it must end in the
   existing "unavailable" failure, shown as a failed private result — test in Task 2 (list) and Task 6 (result).
5. **Single-choice poll vote changes and withdrawals** — clicking another answer moves the vote, clicking your own
   pick withdraws it (empty set), clicks on a closed poll do nothing, and a vote racing a close shows the closed card
   — test in Task 7 (`PollCard.test.tsx` in web).

---

### Task 1: Real-app parity mode, and the 19 never-compared wave A states

Disjoint: runs alone first (every later task depends on it). Model: sonnet; review opus.

**Files:**
- Modify: `docs/design/signal/parity/shoot.mjs`, `docs/design/signal/parity/shoot.test.mjs`,
  `docs/design/signal/parity/README.md`, `e2e/ux/world.mjs`, `e2e/ux/run.mjs`, `e2e/ux/README.md`
- Create: `docs/design/signal/parity/app/wave-a-gaps.json`, `docs/design/signal/parity/app/standing.json`
- Output (git-ignored scratch): `.superpowers/sdd/2026-10-30-signal-wave-b/parity/task-1/`,
  `.superpowers/sdd/2026-10-30-signal-wave-b/findings.md`

**Interfaces:**
- Produces (spec format, used by every later task): a pair has `prototype` as today and **either** `story` **or**
  `app`:
  ```ts
  type AppSide = {
    who: 'ada' | 'bjorn' | 'mira' | 'phone' | 'fresh' | 'freshPhone'; // world browser over CDP, or a new signed-out context
    path?: string;            // goto before steps; `{invite}` expands to state.invitePath
    steps?: Step[];           // today's step kinds plus { goto: string } and { totp: [selector, who] }
    clip?: string;
    reset?: string;           // path reloaded after capture (default: `path`, else the URL before steps)
  };
  ```
  Output names `<name>.prototype.png`, `<name>.app.png`, `<name>.pair.png` (app right).
- Produces (CLI): `node docs/design/signal/parity/shoot.mjs <spec> <outDir> --app [<state.json>]` (default state
  `target/ui-screens/signal-b/state.json`); `--storybook` keeps working; a spec may mix story and app pairs.
- Produces (world): `npm run ux:world -w @dilla/e2e -- --base <n> --out <dir>` — host `n`/`n+1`, SFU `n+2`/`n+4`,
  CDP from `n+10` (phone `n+19`); defaults keep today's 8611 layout but out dir `target/ui-screens/signal-b`. The
  world also starts the Bot runtime (reuse `hostArgs` / `startBotRuntime` from `e2e/web/support/host.ts`) and installs
  the Bot through Server settings → Bot as `e2e/web/bot-runtime.spec.ts` does, with Polls and Reminders on; writes
  `state.people.<who>.totp` (already) and `state.botName`.

- [ ] **Step 1: Failing tool tests** in `shoot.test.mjs`: `validateSpec` accepts a pair with `app` and no `story`,
  rejects both or neither, rejects an unknown `who`, accepts `{goto}` and `{totp:[sel,who]}` steps and rejects a
  malformed `totp`; `appUrl(state, '/c/x?i={invite}')` expands `{invite}` from `state.invitePath`; output file stems
  are `.app.png` for app pairs. Run `node --test docs/design/signal/parity/shoot.test.mjs` → FAIL.
- [ ] **Step 2: Implement** `shootApp(state, pair, out)` in `shoot.mjs`: `who` in `state.cdp` →
  `chromium.connectOverCDP`, first `http` page, `goto` path; `fresh`/`freshPhone` → own context at
  `state.publicUrl` (1280×800 / 390×844, fonts routed, reduced motion, scale 2). Same `settle`, `runSteps`, `capture`
  as the story side; `totp` fills `nextCode(state.controlUrl, state.people[who].totp)` (import from
  `e2e/web/support/totp.ts`); after capture press Escape twice and reload `reset`. Note in the README that the
  app's sample data (Midgard Crew, Ada/Björn/Mira/Liv) differs from the prototype's and is not judged.
- [ ] **Step 3:** `world.mjs` gains `--base`, `--out`, the Bot runtime and Bot install; `run.mjs` reads the same
  `--out`. Build (`go build -o target/dilla-testhost ./cmd/dilla-testhost`, `go build -o target/dillabot
  ./cmd/dillabot`, `npm run build -w @dilla/web`), start the world in the background, wait for `READY`.
- [ ] **Step 4: Specs.** `standing.json`: the five standing whole screens. `wave-a-gaps.json`: one whole-screen pair
  (plus a component crop where the state is small) for each of the 19 states, numbered by the audit:
  #3/#4 sign-in recover and done (`fresh`, totp step); #11 auth on phone (`freshPhone`); #16 reminders popover empty
  (both tabs); #24 status menu desktop and #96 phone You → Status; #44 reply-jump flash (shot mid-flash with
  `wait`); #47 GIF picker; #49 tray with image previews and uploading cards; #52 sent image / GIF / file messages;
  #53 code-block Copied; #71 desktop and #99 phone not-joined preview, nobody here; #75 focused-tile full-screen
  button; #79 PTT listening and #80 mic test running; #83 Roles → New role; #85 Devices → remove another browser;
  #87 phone members sheet; #89 phone message toolbar on tap; #90 phone emoji, GIF picker and tray; #97 phone Home DM
  list and DM conversation; #102 phone settings Profile, Sign-in, Devices, Keyboard, Overview. Prototype props and
  steps come from the audit's "how to reach it" column.
- [ ] **Step 5:** Shoot both specs with `--app`; read every pair. Write `findings.md`: one row per pair,
  `pair | match | differs: <what>` with the owning task by area — auth → Task 3, voice → Task 4, conversation
  (composer, tray, feed, reply, GIF, code block) → Task 8, everything else → Task 5. A finding that is not a defect
  of the shipping UI (sample data, capture timing) is marked `accepted: <why>` only if it is one of the Global
  Constraints' accepted classes; otherwise it is a finding.
- [ ] **Step 6:** `node --test docs/design/signal/parity/shoot.test.mjs` → PASS; root `npm test` → PASS; stop the
  world.
- [ ] **Step 7:** Commit `feat(parity): shoot the running web app against the prototype; wave A gap pairs`.

### Task 2: The command picker (`/` list, inline field chips, suggestions) replaces the old composer flow

Disjoint with Tasks 3, 4, 5 (lanes); runs in the worktree. Blocks 6, 7, 8 (`CommandHost.tsx`, `ComposerArea.tsx`,
`UtilityCommandProvider.tsx`). Model: **opus**; review opus.

**Files:**
- Create: `packages/ui/src/CommandPicker/{CommandPicker.tsx,CommandPicker.css,CommandPicker.stories.tsx,CommandPicker.test.tsx}`,
  `packages/web/src/screens/commands/CommandPicker.tsx` (replaces `CommandComposer.tsx` + `.css`, deleted),
  `packages/web/src/screens/commands/picker-catalog.ts`, `packages/web/src/screens/commands/capabilities.ts`
- Modify: `packages/ui/src/index.ts`, `packages/ui/src/Icon/icons.ts`, `packages/ui/scripts/check-stories.mjs`,
  `packages/web/src/screens/commands/{CommandHost.tsx,UtilityCommandProvider.tsx,copy.ts}`,
  `packages/web/src/screens/conversation/ComposerArea.tsx`, `packages/web/src/strings/botCommand.ts`,
  `e2e/web/slash-commands.spec.ts`
- Test: `packages/web/src/screens/commands/CommandPicker.test.tsx` (moved from `CommandComposer.test.tsx`, which is
  deleted), `picker-catalog.test.ts`, `capabilities.test.ts`, `CommandHost.test.tsx`, `CommandLifecycle.test.tsx`
- Parity: `docs/design/signal/parity/app/task-2.json`

**Interfaces:**
- Produces (`@dilla/ui`):
  ```ts
  interface CommandListItem { id: string; icon: IconName; name: string; args: string; description: string; source: string }
  interface CommandListProps { id: string; items: readonly CommandListItem[]; activeId: string | null; showHint: boolean;
    labels: { title: string; hint: string; noMatch: string; sendAsText: string }; query: string;
    onActive(id: string): void; onPick(id: string): void; onSendAsText(): void }
  interface CommandFieldProps { label: string; optional: boolean; value: string; placeholder: string; active: boolean;
    invalid: boolean; ghost?: boolean; listId?: string; activeOptionId?: string; inputRef?: React.Ref<HTMLInputElement>;
    onChange(value: string): void; onFocus(): void; onKeyDown(e: React.KeyboardEvent<HTMLInputElement>): void }
  interface CommandSuggestion { id: string; label: string; sub?: string; avatar?: { name: string }; icon?: IconName }
  interface CommandSuggestionsProps { id: string; title: string; items: readonly CommandSuggestion[]; activeId: string | null;
    onActive(id: string): void; onPick(id: string): void }
  interface CommandBarProps { icon: IconName; text: string; tone: 'info' | 'danger'; hint?: string }
  ```
  Exports `CommandList`, `CommandField`, `CommandSuggestions`, `CommandBar` (README §7.2's CommandPicker and
  CommandField).
- Produces (web): `CommandPicker(props: CommandComposerProps)` with today's `CommandComposerProps` unchanged (so
  `CommandHost` keeps its draft memory, `requestId` and generation guards); `pickerEntry(command: CommandDefinition,
  botName: string): PickerEntry` where `PickerEntry = { icon: IconName; description: string; args: string; source:
  string; opensForm: boolean; fields: Record<string, { label: string; placeholder: string; suggest: 'member' |
  'duration' | 'when' | 'provider' | 'choice' | 'none' }> }`; `COMMAND_OPERATIONS: Readonly<Record<string, number>>`
  and `useCommandCapabilities(client: CoreClient, communityId: string | null): ReadonlySet<number> | null`
  (from `createBotUtilities(...).capabilities().supportedOperations`, re-read when the composer opens command mode and
  every 30 s while it is open; `null` while unknown).
- Consumes: `COMMANDS`, `commandAvailability`, `validateCommandArgs`, `parseCommand` (client-core);
  `parseReminderTime`, `resolveReminderTimeCandidate` (web utility); the existing `suggest` providers (members,
  roles, channels, music search for `query-or-url`, polls, reminders).

Behaviour to build (prototype values; the implementer reads the template for every size and colour):
- `/` at the start of an empty-or-slash draft opens the list above the composer (`sModal` 180ms, radius 10, header
  "Commands" + "↑↓ move · tab complete · esc close"), filtered by `startsWith` then `includes` of the command id; rows
  40px with a 26px icon tile, `/name` bold, args in `--fg-3` (`<required>`, `[optional]`, `min..max` for a bounded
  number, "opens a form" for `poll create`), description 10.5px `--fg-2`, a source tag "built-in" or the bot's
  display name (`state.botName`, "Glenn"); selected row `--surface-hi` with a 2px accent inset bar.
- No match: "No command matches {draft}" + "Send as text" (posts the draft literally).
- ↑/↓ move, Tab or Enter picks; Esc clears the `/` list draft. Picking turns the composer into chips; a command
  without fields shows one ghost chip "press ↵ to run". `poll create` opens the poll dialog instead (the existing
  `PollCreateDialog` until Task 7).
- Chip: label tile (accent when active, red when invalid), input sized `max(len(value), len(placeholder), 4)ch +
  16px`, active ring `0 0 0 3px` accent-soft (danger-soft when invalid); optional fields' label ends in "?".
- Suggestions above the active chip (width `min(340px, 100% − 32px)`), titled Members / Durations / When /
  Suggestions / Commands: members = the community's people minus you and bots, avatar + name + `@handle`, filtered
  by `eligibleModerationTarget` for moderation commands as today; durations "10 minutes", "1 hour", "1 day", "7 days"
  (+ "permanent" for `/ban`); when "in 1 minute", "in 10 minutes", "in 1 hour", "tonight 20:00", "tomorrow 09:00",
  each with its exact preview (`Intl.DateTimeFormat('en-GB', { weekday: 'short', day: 'numeric', month: 'short',
  hour: '2-digit', minute: '2-digit', timeZoneName: 'short' })`, e.g. "Fri 30 Oct, 20:00 CET"); a DST-ambiguous
  time lists both candidates with the existing `reminder.timeChoice` copy; `provider` kinds use today's providers.
- Keys in a chip: ↑/↓ move in suggestions; Tab accepts the highlighted suggestion when the value is non-empty and not
  an exact match, else moves to the next chip; Shift+Tab previous chip; Enter accepts likewise (not for
  `query-or-url` and numbers), else runs; Esc leaves keeping the draft (Review Focus 3); Backspace in an empty chip
  goes to the previous chip, or leaves with `/name` from the first.
- Command bar above the chips (11px): the description; music commands add " · {room}" or " · join voice first";
  moderation adds " · checked against your roles"; a `when` chip with a value shows "Reminds you {preview}" or
  "Try “in 1 hour” or “tomorrow 09:00”"; an error replaces the line in `--danger` with `triangle-alert` and marks its
  chip invalid (see Q1). Hint "tab next field · ↵ run · esc leave" on desktop only.
- Validation messages (prototype copy): "{label} is required", "No member called {value} here", "Try “in 1 hour” or
  “tomorrow 09:00”", "Between 1 minute and 365 days from now", "A whole number from 0 to 100" (from the option's
  min/max), otherwise today's `validateCommandArgs` messages.
- Enter runs only when the chips validate; one run at a time (Review Focus 2). `//` at the start posts the rest with
  one leading `/` (`//shrug` → `/shrug`).
- Visible commands: today's `definitions` (scope, permission bits, handlers present) **and**, for `provider: 'bot'`
  commands, their operation in `useCommandCapabilities` (`null` → bot commands hidden). `COMMAND_OPERATIONS`: music
  `play` 12, `pause` 13, `resume` 14, `skip` 15, `stop` 16, `queue`/`nowplaying` 10, `seek` 17, `volume` 18, `shuffle`
  19, `repeat` 20, `queue remove` 21, `queue move` 22, `queue clear` 23, `music`/`music help` 10; `rolepanel create`
  31; `announcement *` 50/51/51/52; `poll create` 70, `poll results` 71, `poll vote` 73, `poll close` 74; `remind`
  80, `reminders list` 81, `reminders cancel` 82, `reminders dismiss` 83.
- Catalog copy: for the commands the prototype lists, its icon, description, field labels and placeholders verbatim
  (`help` circle-help "List commands, or explain one"; `me` sparkles "Post an action in italics", text "packs the
  rope"; `shrug` smile "Appends ¯\_(ツ)_/¯ to your message"; `play` play "Play the first YouTube result, or a link,
  in your voice channel", "song, video or playlist"; `pause`, `resume`, `skip` skip-forward "Skip to the next track",
  `stop` square "Stop and clear the queue", `queue` list-music "Show what plays next", `volume` volume-2 label
  "0..100" placeholder "60"; `poll create` vote "Ask the channel a question"; `remind` alarm-clock "A private
  one-time reminder", "in 1 hour" / "bring two ropes"; `reminders list` bell "Open your reminders inbox"; `kick`
  user-x "Remove someone from the server"; `ban` ban "Ban for a while, or for good", "permanent"; `timeout` timer
  "Stop someone sending or speaking for a while", "10 minutes"; `warn` triangle-alert "A private warning, logged in
  modlog", "what happened"; optional fields "optional"). Every other registry command keeps its registry
  description and takes the icon from the table in Q2. Copy lives in `picker-catalog.ts`; the client-core registry
  is not edited.
- `/remind` runs from its chips: the `ReminderCreateDialog` is removed; the run passes `{ when, text, timezone:
  browser zone, chosen }` to today's reminder handler. `/reminders list` opens the bell popover on Upcoming.

- [ ] **Step 1: ui failing tests** (`CommandPicker.test.tsx`): CommandList renders `role="listbox"` with
  `aria-activedescendant`, rows named "/remind, A private one-time reminder"; the no-match row offers a "Send as
  text" button; CommandField marks `aria-invalid` and `data-active`; CommandSuggestions calls `onPick` on mousedown
  (keeps input focus). Stories `Conversation/CommandPicker`: List, ListFiltered, NoMatch, Fields, FieldSuggestionsMembers,
  FieldSuggestionsWhen, FieldError, NoFields, Phone. check-stories: list 260px max height, row ≥ 40px, chip ring
  values, phone hint hidden. → FAIL.
- [ ] **Step 2: web failing tests**: `picker-catalog.test.ts` (prototype copy for the listed commands; `args` for
  `ban` is "<user> [duration] [reason]", `volume` "<0..100>", `poll create` "opens a form"); `capabilities.test.ts`
  (hides `poll create` without op 70, keeps `poll vote` with 73; hides every bot command while `null`);
  `CommandPicker.test.tsx` (moved suite kept green where behaviour is unchanged, plus: `enter twice runs once`,
  `retry keeps requestId`, `esc keeps filled draft`, `backspace leaves from first chip`, `double slash posts
  literal`, `tab accepts suggestion`, `ambiguous time lists both candidates`, `capability dropped while open`).
  `e2e/web/slash-commands.spec.ts`: `/remind` via chips creates a reminder that appears in the popover's Upcoming;
  `/shrug` posts; `//x` posts `/x`. → FAIL.
- [ ] **Step 3: Implement** the ui pieces and the web `CommandPicker`, catalog, capabilities; wire into
  `CommandHost` and `ComposerArea` (the chips replace the composer's input row in place, the composer box keeps its
  Signal frame); delete `CommandComposer.*` and `ReminderCreateDialog`; vendor missing icons.
- [ ] **Step 4:** `npm test -w @dilla/ui`, `npm test -w @dilla/web` → PASS.
- [ ] **Step 5: Parity** `task-2.json` (app side `mira` in #general, `phone` for phone): list open (`/`), filtered
  (`/re`), no match (`/xyz`), `/remind` chips empty, when-suggestions, both chips filled with the "Reminds you" bar,
  field error, `/kick` member suggestions, `/pause` ghost chip, `/me` and `/shrug` results in the feed, phone list
  and chips; plus the standing screens. Iterate until each matches.
- [ ] **Step 6: Gates** (Global Constraints) with e2e `slash-commands conversation layout keyboard phone`.
- [ ] **Step 7:** Commit `feat(ui,web): Signal command picker with inline field chips`.

### Task 3: Auth — six-digit code step, passkey and backup links, the whole sign-in and new-account flows

Disjoint (lane): auth screens only. Parallel with 2, 4, 5. Model: **opus** (sign-in factor steps); review opus.

**Files:**
- Create: `packages/ui/src/CodeBoxes/{CodeBoxes.tsx,CodeBoxes.css,CodeBoxes.stories.tsx,CodeBoxes.test.tsx}`
- Modify: `packages/ui/src/index.ts`, `packages/web/src/screens/SignIn.tsx`,
  `packages/web/src/screens/factor/FactorSetup.tsx`, `packages/web/src/strings/{factorSignin.ts,factor.ts}`,
  `e2e/web/{second-factor.spec.ts,required-factor.spec.ts,signin-order.spec.ts,signup.spec.ts}`,
  `e2e/web/support/totp.ts` helpers if a selector changes, `e2e/ux/world.mjs` (its TOTP fill), `e2e/scripts/shoot-docs.mjs` (same)
- Test: `CodeBoxes.test.tsx`, `packages/web/src/screens/{SignInFactor.test.tsx,OnboardingFactor.test.tsx}`
- Parity: `docs/design/signal/parity/app/task-3.json`

**Interfaces:**
- Produces: `CodeBoxes(props: { id: string; label: string; value: string; onChange(value: string): void;
  onComplete?(value: string): void; invalid?: boolean; describedBy?: string; autoFocus?: boolean })` — one real
  `<input inputMode="numeric" autoComplete="one-time-code" maxLength={6}>` over six drawn boxes (prototype
  `codeBoxes`; the box at the caret gets the accent edge); `onComplete` fires once when the sixth digit lands.
- Consumes: Task 1's `totp` step.

- [ ] **Step 1: Failing tests.** `CodeBoxes.test.tsx`: typing "12a3" keeps "123"; pasting "123 456" gives
  "123456" and calls `onComplete` once (Review Focus 1); six boxes are `aria-hidden`, the input carries the label.
  `SignInFactor.test.tsx`: the code step renders CodeBoxes and the links "Use a passkey" (only when this account
  has a passkey and the browser can use one) and "Use a backup code"; a complete code submits once.
  `OnboardingFactor.test.tsx`: FactorSetup's TOTP step uses CodeBoxes. → FAIL.
- [ ] **Step 2: Implement**; the links switch to today's passkey and backup-code steps (no new route; a link
  whose route the account lacks is hidden, never dead — Ruling AR precedent). Update the e2e and world helpers that
  `fill` the old single code field.
- [ ] **Step 3: Parity** `task-3.json` (`fresh` / `freshPhone`): sign-in credentials, code step, recover ("Add
  this browser"), done; new account invite, account, recovery key, second factor choice, code step, done (via
  `{invite}`); each at desktop and phone. Iterate to match; Task 1's auth findings are fix steps here.
- [ ] **Step 4: Gates**; e2e `second-factor required-factor signin-order signup passkey enrol`.
- [ ] **Step 5:** Commit `feat(ui,web): six-digit code boxes and the Signal sign-in and new-account steps`.

### Task 3b: New-account flow to the prototype's six steps

Added 2026-10-30 (Jonas, Task 3 Q5: "Match the prototype"). After Task 3, in lane b3. Model: opus; review opus +
security review. The prototype's new-account flow is invite → account → save recovery key → second factor → code →
done, creating the account on its second step; today's app adds a "What this browser keeps" step, creates the
account there, and has separate QR and backup-code steps. Match the prototype's steps, copy and layout (values
from the prototype source), move account creation to where the prototype creates it, and fold the QR (kept per Q6)
and backup codes into its steps. Keep every guard: the recovery key is saved and confirmed before the account can
be used, the second factor is required (MFA required, 2026-10-15), nothing secret is logged or persisted beyond
today, and a flow abandoned at any step leaves the same state today's flow does (or a stricter one). Tests: each
step, abandon-and-reopen at every step (the forced factor screen), paste/autofill on the code step; e2e `signup
enrol required-factor second-factor phone`; app parity for all six steps, desktop and phone.

### Task 4: Voice — your own voice menu, the rest of the person menu, the AFK page

Disjoint (lane): `packages/web/src/screens/voice/*`, `packages/ui/src/PersonContextMenu`, `packages/ui/src/CallStage`
(AFK slot only). Parallel with 2, 3, 5. Model: sonnet; review opus.

**Files:**
- Modify: `packages/web/src/screens/voice/VoiceView.tsx` (`personMenu`, `AfkPage`),
  `packages/ui/src/PersonContextMenu/*` (Move-to group), `packages/web/src/strings/{afk.ts,stage.ts}`,
  `packages/ui/src/Icon/icons.ts` if needed, `e2e/web/afk.spec.ts`, `e2e/voice/controls.spec.ts`
- Test: `packages/web/src/screens/voice/{VoiceView.test.tsx,VoiceView.wb2.test.tsx}`,
  `packages/ui/src/PersonContextMenu/PersonContextMenu.test.tsx`
- Parity: `docs/design/signal/parity/app/task-4.json`

**Interfaces:**
- Produces: `PersonContextMenuProps.move?: { label: string; rooms: readonly { id: string; name: string; count:
  number }[]; onMove(id: string): void }` (prototype "Move to" block: heading, volume-2 icon, name, count).
- Consumes: `voiceShortcutText()` (`voice/shortcuts.ts`), the profile-card opener and the DM opener the member menu
  already uses (`MemberCardHost`), the composer mention insert from Ruling (Jonas 10-28 "Mention"), the roles
  settings route.

- [ ] **Step 1: Failing tests.** Own row/tile menu (in a call): "Mute"/"Unmute" with hint `⌘⇧M`/`Ctrl+Shift+M`,
  "Deafen"/"Undeafen" with `⌘⇧D`/`Ctrl+Shift+D`, "Profile", "Move to" listing the community's other voice channels
  with occupant counts (joining moves you), "Leave voice" (danger); no Copy username. Another person's menu: volume,
  then "Profile", "Message", "Mention", "Mute for me" (check), "Copy username", "Edit roles" (only with Manage
  roles); Glenn keeps volume, Mute for me, Copy (Move/Disconnect are C). AFK page: moon tile, title "Nobody is
  away" / "You are away", the explainer line, button "Go AFK" / "Come back" (Q3 decides what the latter does and the
  title with others away); no roster list. → FAIL.
- [ ] **Step 2: Implement.** The explainer reads the community's AFK timeout from `afkRead`'s `timeoutSeconds`:
  "After {minutes} minutes idle in voice, a computer is moved here. Phones are never moved." — first confirm in
  `useCall.tsx` that phones are never moved; if they are, stop (NEEDS_CONTEXT).
- [ ] **Step 3: Parity** `task-4.json`: own menu (desktop row, tile, phone long-press), another person's menu,
  Glenn's menu, AFK page empty and away, desktop and phone; Task 1's voice findings are fix steps here.
- [ ] **Step 4: Gates**; e2e web `afk layout`, voice `controls stage join-talk phone-call`.
- [ ] **Step 5:** Commit `feat(web,ui): Signal own voice menu, person menu items and AFK page`.

### Task 5: Parity sweep — Task 1 findings outside the area tasks

Disjoint (lane) once routed: only files of findings owned by Task 5 (settings, phone shell, reminders popover, status
menu, palette, toasts). Parallel with 2, 3, 4; the controller checks the findings' files against 2/3/4 before
dispatch and moves any overlap to the owning task. Model: sonnet; review opus.

**Files:** the components and web call sites named in `findings.md` rows owned by Task 5; their stories and
check-stories rules; `docs/design/signal/parity/app/task-5.json`.

- [ ] **Step 1:** For each Task 5 row of `findings.md`, write the failing test that pins the difference where it is
  behaviour or DOM (vitest) or computed style (check-stories), with the prototype's value.
- [ ] **Step 2:** Fix each in the component or the web call site (restyle rule 3d: change the app, not the story).
- [ ] **Step 3: Parity** re-shoot every Task 5 pair from `wave-a-gaps.json` plus the standing screens into
  `task-5/`; each row of `findings.md` it owns becomes `fixed (commit)` or a question for Jonas.
- [ ] **Step 4: Gates**; e2e `search-settings phone layout panes keyboard`.
- [ ] **Step 5:** Commit `fix(ui,web): wave A parity gaps found in the running app`.

### Task 6: Private results — the BotResult card in the feed

After Task 2 (same `CommandHost.tsx`). Parallel lane with Task 7. Model: sonnet; review opus.

**Files:**
- Create: `packages/ui/src/BotResult/{BotResult.tsx,BotResult.css,BotResult.stories.tsx,BotResult.test.tsx}`,
  `packages/web/src/screens/commands/private-results.ts`
- Modify: `packages/ui/src/index.ts`, `packages/web/src/screens/commands/{CommandHost.tsx,CommandPicker.tsx,copy.ts}`,
  `packages/web/src/screens/conversation/Conversation.tsx`, `e2e/web/slash-commands.spec.ts`
- Test: `BotResult.test.tsx`, `packages/web/src/screens/commands/private-results.test.ts`, `CommandHost.test.tsx`
- Parity: `docs/design/signal/parity/app/task-6.json`

**Interfaces:**
- Produces (`@dilla/ui`):
  ```ts
  type BotStatus = 'pending' | 'success' | 'failed' | 'cancelled';
  interface BotResultProps { author: { name: string; avatarName: string; shape: 'bot' | 'circle' }; time: string;
    status: BotStatus; body: React.ReactNode; onDismiss(): void;
    labels: { status: Record<BotStatus, string>; onlyYou: string; dismiss: string; tag: string } }
  ```
  Chip per README §6.2 / prototype: pending loader spinning 900ms "Working" `--accent-2` on `--accent-soft`; success
  check "Done" `--ok` on a new `okResult` token `rgba(124,255,142,.1)`; failed x "Failed" `--danger` on
  `--danger-soft`; cancelled "Not confirmed" `--fg-3` on `--hover`; then eye "Only you can see this" and "Dismiss" in
  10.5px `--fg-3`. The row is a MessageRow first-row layout with the 30px `--r-bot` avatar and the tag.
- Produces (web): `privateResults(client: object, account: string, conversation: string): { rows(): readonly
  PrivateResult[]; add(row: Omit<PrivateResult, 'id'>): string; update(id: string, patch: Partial<PrivateResult>):
  void; dismiss(id: string): void; subscribe(listener: () => void): () => void }` with `PrivateResult = { id: string;
  at: number; status: BotStatus; text: string; provider: 'builtin' | 'bot' }` — in memory, per client, account and
  conversation (like `CommandHost`'s draft memory), gone on reload, as in the prototype.

- [ ] **Step 1: Failing tests.** BotResult: each status renders its icon and label; Dismiss calls `onDismiss`;
  the status chip is `role="status"`. `private-results.test.ts`: rows are per conversation; dismiss removes;
  another account sees none. `CommandHost.test.tsx`: a run adds a pending row with the prototype's pending text
  (e.g. "Saving your reminder…"), the handler's result turns it success/failed with the handler's message; the
  composer footer live text (`runningPrivately`) is gone; an unknown `/xyz` sent with Enter adds a failed row
  "/xyz isn’t a command here. To post it as text, start with //." (Review Focus 4: the unavailable failure from a
  dropped capability lands here). → FAIL.
- [ ] **Step 2: Implement**; rows render after the last message in `Conversation.tsx`, in time order, with the
  existing entrance animation; `uncertain` per Q5; the author per Q7.
- [ ] **Step 2b: Kick/ban confirm (Q4).** `/kick` and `/ban` open the prototype's confirm modal (avatar, "Kick {name}
  from {server}?", Duration for ban, Logged note, Kick/Ban and Cancel; no Reason field for kick until C). Confirm runs
  the existing command path; Cancel adds a "Not confirmed" (`cancelled`) private result. Client only: if the modal
  needs a server change, stop and report. Tests: the modal renders per prototype, Confirm runs once, Cancel adds the
  row and runs nothing; parity pairs for the modal and the Not confirmed row.
- [ ] **Step 3: Parity** `task-6.json`: pending, success (`/remind`), failed (`/play` outside voice: "Join a voice
  channel first. Glenn plays where you are."), unknown command, desktop and phone, plus standing screens.
- [ ] **Step 4: Gates**; e2e `slash-commands conversation bot-runtime`.
- [ ] **Step 5:** Commit `feat(ui,web): private command results as Glenn's inline card`.

### Task 7: Polls — the New poll dialog and the poll card

After Task 2 (`UtilityCommandProvider.tsx`). Parallel lane with Task 6 (disjoint: poll files only). Model: sonnet;
review opus.

**Files:**
- Create: `packages/ui/src/PollCard/{PollCard.tsx,PollCard.css,PollCard.stories.tsx,PollCard.test.tsx}`,
  `packages/ui/src/PollDialog/{PollDialog.tsx,PollDialog.css,PollDialog.stories.tsx,PollDialog.test.tsx}`
- Modify: `packages/ui/src/index.ts`, `packages/web/src/screens/commands/{PollCard.tsx,UtilityCommandProvider.tsx,UtilityCards.css}`,
  `packages/web/src/strings/utility.ts`, `e2e/web/slash-commands.spec.ts` (or the poll spec it holds)
- Test: `PollCard.test.tsx`, `PollDialog.test.tsx` (ui), `packages/web/src/screens/commands/PollCard.test.tsx` (new)
- Parity: `docs/design/signal/parity/app/task-7.json`

**Interfaces:**
- Produces (`@dilla/ui`):
  ```ts
  interface PollCardOption { text: string; pct: number; mine: boolean; winner: boolean; voters: readonly { name: string }[] }
  interface PollCardProps { question: string; closed: boolean; multi: boolean; meta: string; options: readonly PollCardOption[];
    canVote: boolean; canClose: boolean; busy?: boolean; labels: { closed: string; close: string };
    onVote(index: number): void; onClose(): void }
  interface PollDraft { question: string; answers: string[]; duration: '1h' | '4h' | '8h' | '24h' | '3d' | '7d'; multi: boolean }
  interface PollDialogProps { open: boolean; channelName: string; bot: { name: string }; value: PollDraft; error: string | null;
    canPost: boolean; onChange(next: PollDraft): void; onPost(): void; onCancel(): void }
  ```
- Consumes: `PollSummary` (`counts`, `myChoices`, `closed`, `dueAt`, `createdAt`, `creatorId`, `multi`), the
  vote/close/voters operations already in `packages/web/src/screens/commands/PollCard.tsx` and the create path in
  `UtilityCommandProvider.tsx` (`DURATIONS`, byte checks).

Prototype values: dialog 520px at 12vh (phone `calc(100% − 24px)` at 20px), header bot tile + "New poll in
#{channel}" + close; "Question" with "{n} / 300" (danger above 300), placeholder "What should we raid on Friday?";
"Answers" with "2–10", numbered rows with placeholders "the bridge", "the harbour", "another answer", remove ×
(disabled-looking at two), "Add answer" up to 10; "Open for" segmented 1h 4h 8h 24h 3d 7d (default 24h); toggle
"Allow several answers"; error line with `triangle-alert`; footer "Voters are shown by name. Anyone can change or
withdraw their vote.", "Cancel", "Post poll" (accent only when the question is set and ≥ 2 answers). Errors in
order: "Write a question", "Questions are at most 300 characters", "Add at least two answers", "Answers are at most
100 characters", then today's duplicate and size messages. Toasts "Poll posted", "Poll closed". Card: vote icon +
question (+ "CLOSED" tag), one row per answer with a fill bar to its percentage (`--accent-soft`-family fill
`rgba(90,168,230,.2)` for your pick, `rgba(124,255,142,.12)` for a closed winner, `rgba(255,255,255,.05)` otherwise —
tokens), border accent for your pick, round check (square when multi), up to five 18px voter avatars with names on
hover, "{pct}%"; meta "Open for 24 hours · pick one · 3 voters" / "Final results · pick any · 1 voter"; "Close poll"
for whoever may close.

- [ ] **Step 1: Failing tests.** ui PollCard: percentages round like the prototype (`Math.round(count / votes ×
  100)`), winner weight 700 only when closed and the max count > 0, `onVote` not called when closed, voters capped
  at five. PollDialog: the remove button is inert at two answers, "Add answer" gone at ten, the counter turns danger
  past 300, Post calls `onPost`. web `PollCard.test.tsx`: single choice switches (choices `[1]`), clicking your pick
  sends `[]`, multi toggles within the set, a vote rejected because the poll closed refreshes to the closed card
  (Review Focus 5); the meta's duration comes from `dueAt − createdAt`; voter avatars come from the first voters page
  per answer (Q8 decides the multi-choice voter count). `/poll create` from the picker opens the new dialog. → FAIL.
- [ ] **Step 2: Implement**; delete `PollCreateDialog` and the old card markup and CSS.
- [ ] **Step 3: Parity** `task-7.json`: dialog empty, filled, error; card open, voted, multi, closed with winner,
  creator's Close; desktop and phone.
- [ ] **Step 4: Gates**; e2e `slash-commands bot-runtime conversation`.
- [ ] **Step 5:** Commit `feat(ui,web): Signal poll dialog and poll card`.

### Task 8: Conversation — Threads opens the thread, upload toasts and copy, reaction tooltip, conversation findings

After Tasks 6 and 7 (`ComposerArea.tsx`, `Conversation.tsx`). Runs in the worktree. Model: sonnet; review opus.

**Files:**
- Modify: `packages/web/src/screens/Shell.tsx` (~2090, the Threads aside), `packages/web/src/screens/conversation/{ComposerArea.tsx,Attachments.tsx,Conversation.tsx}`,
  `packages/ui/src/AttachmentTray/*`, `packages/ui/src/ReactionBar/*`, `packages/web/src/strings/en.ts`
  (`shell.tray.*`), `e2e/web/{threads.spec.ts,conversation.spec.ts}`
- Create (per Q6): `packages/ui/src/ReactionTooltip/{ReactionTooltip.tsx,.css,.stories.tsx,.test.tsx}`
- Test: `ShellThreads.test.tsx`, `ComposerArea.test.tsx`, `Attachments.test.tsx`, `ReactionBar.test.tsx`
- Parity: `docs/design/signal/parity/app/task-8.json`

**Interfaces:**
- Produces: `ReactionTooltipProps = { text: string; avatars: readonly { name: string }[] }` (prototype: up to six
  avatars and "Ada, Björn and you reacted with 🛡️"), anchored to a reaction chip on hover/focus via `Tooltip`.

- [ ] **Step 1: Failing tests.** Threads header button opens a thread directly (Q9 picks which), not `ThreadList`;
  a file over 25 MB stays in the tray as a card with meta "Over 25 MB" in `--danger` and a danger edge, and raises
  the toast "Files over 25 MB can’t be sent from a browser" once per pick; Send while any upload is below 100 %
  shows the toast "Waiting for uploads to finish" and sends nothing; the reaction tooltip shows on hover and focus
  per Q6. → FAIL.
- [ ] **Step 2: Implement**; remove `shell.tray.tooLarge`'s old sentence; keep 25 MB and the file count (C).
- [ ] **Step 3:** Fix every Task 1 finding routed to Task 8 (reply flash, GIF picker, tray previews, sent
  attachments, code-block Copied) with a pinning test each.
- [ ] **Step 4: Parity** `task-8.json`: Threads → thread aside, tray over 25 MB with its toast, waiting toast,
  reaction tooltip, and every Task 1 conversation pair, desktop and phone, plus standing screens.
- [ ] **Step 5: Gates**; e2e `threads conversation messaging phone layout`.
- [ ] **Step 6:** Commit `feat(web,ui): Threads opens the thread; Signal upload toasts and reaction tooltip`.

### Task 9: Close — user docs, screenshots, release notes, full gates

Last; alone. Model: sonnet (gates, shots), haiku for prose; review opus.

**Files:** `README.md` ("what works" row for wave B), `docs/user/bot-commands.md` (picker, chips, private
results, polls), `docs/user/getting-started.md` (code boxes), `docs/user/voice.md` (own menu, AFK page),
`docs/user/screenshots/*`, `e2e/scripts/shoot-docs.mjs` (new shots: command list, chips, private result, poll
dialog and card, code step), `docs/deploy/releases/unreleased-signal-wave-b.md` (create; becomes
`v0.1.0-signal2.md` at tag time), `docs/design/signal/parity/README.md` (app mode section final).

- [ ] **Step 1:** Re-shoot every app spec (`standing`, `wave-a-gaps`, `task-2`…`task-8`) into `task-9/`; every row
  must be `match` or an accepted deviation recorded in the ledger; anything else goes back to its task.
- [ ] **Step 2:** `npm run docs:shots -w @dilla/e2e`; update the docs listed above (plain verbs, the prototype's
  copy, no Mesh, no pre-Signal dialog descriptions); release notes: what changed for users, "no server change; web
  only", deploy = web assets only (dillad and dillabot unchanged), tag `v0.1.0-signal2`.
- [ ] **Step 3: Full gate:** `npm test`, `npm run typecheck -w @dilla/ui`, `npm run typecheck -w @dilla/web`,
  `npm run typecheck:e2e`, `npm run build -w @dilla/web`, `npm run test:storybook:ci -w @dilla/ui`,
  `npm run test:e2e:web -w @dilla/e2e` (three browsers), `npm run test:e2e:voice:chromium -w @dilla/e2e` → PASS.
- [ ] **Step 4:** Commit `docs: Signal wave B user docs, screenshots and release notes`.

---

## Lanes and order

| wave | tasks | where |
|---|---|---|
| 1 | Task 1 | worktree |
| 2 | Task 2 ∥ Task 3 ∥ Task 4 ∥ Task 5 | 2 in the worktree; 3, 4, 5 in lanes (world `--base` 8711 / 8811 / 8911, Storybook 6113–6115) |
| 3 | Task 6 ∥ Task 7 | 6 in the worktree; 7 in a lane |
| 4 | Task 8 | worktree |
| 5 | Task 9 | worktree |

Shared-file watch: `tokens.ts` (lane blocks), `packages/ui/src/index.ts` and `icons.ts` (append-only, merged by the
controller), `Shell.tsx` (Task 4 profile/roles openers vs Task 5 status menu vs Task 8 threads: different hunks;
merge in task order). The controller runs the lane task's e2e in the worktree after each merge.

## Decisions (Jonas, 2026-10-30)

Answered by AskUserQuestion; they bind every task. Q1 prototype (errors in the command bar). Q2 all available
commands with the proposed icons. Q3 default (Come back leaves the AFK channel; "{names} are away"). **Q4 pull the
kick/ban confirm modal and "Not confirmed" into B (Task 6), without a kick reason field; anything that needs the
server stays in C.** Q5 Failed chip with today's uncertain text. Q6 build ReactionTooltip in `ui` now, wire in C.
Q7 Glenn for bot commands, "dilla" with the D tile for built-ins. Q8 count distinct voters. Q9 last opened, else
most active, else the empty aside. Q10 the browser's zone, shown in the preview.

## Questions for Jonas (answered above)

1. **Command field errors: in the bar or under the field?** Your wave B note says "errors under the field" (README
   §6.1 prose); the prototype draws the error in the command bar above the chips (danger line with a warning icon)
   and turns the active chip red. Default in the plan: the prototype (Ruling M).
2. **Icons for commands the prototype doesn't list** (the registry has ~60, the prototype 16). Proposed: `tableflip`
   / `unflip` table, `code` code, `giphy` image, `dm` message-square, `invite` user-plus, `search` search, `topic`
   hash, `thread` messages-square, `reply` reply, `edit` pencil, `delete` trash-2, `pin`/`unpin` pin, `join`
   volume-2, `leave` phone-off, `mute` mic-off, `unmute` mic, `deafen` headphone-off, `undeafen` headphones,
   `nowplaying` music, `seek` fast-forward, `shuffle` shuffle, `repeat` repeat, `queue *` list-music, `music*`
   list-music, `untimeout` timer-off, `unban` ban, `modlog` scroll-text, `role *` / `selfrole *` / `rolepanel create`
   shield, `announcement *` megaphone, `poll vote/results/close` vote, `reminders cancel/dismiss` bell. And should all
   of them show, or only the prototype's 16 plus what a person needs? Default: all available ones, with these icons.
3. **AFK page "Come back", and the title when others are away.** dilla's AFK is a voice channel. Default: "Come
   back" leaves the AFK channel (you are no longer away); with others away and you not, the title reads "{names} are
   away" / "{name} is away" like the not-joined preview's "… are here". Alternatives: rejoin the channel you were
   moved from; or keep "Nobody is away" only for empty and show no title otherwise.
4. **Kick and ban confirm.** The prototype draws a modal (avatar, "Kick Björn from Midgard Crew?", Duration /
   Reason / Logged, Kick) and Cancel gives a "Not confirmed" private result; the wave A plan put the confirm in C.
   It is client-only UI except kick's Reason (the registry's `/kick` has no reason). Pull the modal and "Not
   confirmed" into B (Task 6), without a kick reason field until C? Default: stays in C; `/kick` keeps today's
   confirm until then.
5. **"Uncertain" results** (the run may or may not have happened, e.g. a lost response). The design has pending,
   done, failed and not confirmed. Default: show the Failed chip with today's uncertain message ("…may have been
   created; check …"). Alternative: a fifth chip "Unsure" in `--warn`.
6. **Reaction tooltip without reactor names** (identities are wave C; today the client knows only the count and
   whether you reacted). Default: build `ReactionTooltip` in `ui` with stories now and wire it in web in wave C, so
   the app shows no half tooltip. Alternative: wire now with "You and 2 others reacted with 🛡️" / "3 people reacted
   with 🛡️" and no avatars.
7. **Who authors built-in command results?** The prototype shows Glenn even for `/help`, a built-in, and DMs have
   no Glenn. Default: bot commands show Glenn (bot avatar, BOT tag); built-in results show the same card authored by
   "dilla" with the D tile. Alternative: Glenn for every result in a server, "dilla" only in DMs.
8. **Multi-choice poll "n voters".** The backend gives counts per answer, not distinct voters. Default: for
   multi-choice, read every voters page of each answer to count distinct people (more requests on big polls).
   Alternative: say "n votes" for multi-choice.
9. **Which thread does the Threads button open?** The prototype opens the last thread you had open (or its only
   thread). Default: the thread you last opened in this channel, else the most recently active one; with no thread
   in the channel the button opens the empty thread aside ("No threads yet"). Alternative: keep the thread list when
   the channel has more than one thread.
10. **Reminder time zone.** The old "Set a reminder" dialog had a time-zone field; the chips have none. Default:
    the browser's zone, shown in the "Reminds you Fri 30 Oct, 20:00 CET" preview. Alternative: an optional
    `[timezone]` chip.
