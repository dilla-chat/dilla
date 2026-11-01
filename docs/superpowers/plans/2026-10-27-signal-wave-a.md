# Signal wave A: tokens, theme, restyle and top bar — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The shipping web client looks like Signal: the Signal dark theme replaces Mesh everywhere, light and
high-contrast get the blue accent, every existing `packages/ui` component is restyled to the handoff, and a 44px
top bar replaces the status bar.

**Architecture:** Values live in `packages/design-tokens` and reach components only as CSS variables. Most of the
work is CSS in `packages/ui/src/*/*.css`, checked against the prototype state by state in Storybook. `packages/web`
changes only to drop Mesh, migrate the stored preference and mount the top bar. No server, core or protocol change.

**Tech Stack:** TypeScript, React 19, Vite, Storybook 10 (react-vite, addon-a11y), vitest, Playwright, axe.

**Spec:** `docs/design/signal/README.md` (the handoff; **its §10 token table wins over the brief**) and
`docs/design/brief-signal.md`. Reference build: `docs/design/signal/prototype/Dilla Signal Prototype.dc.html`
(open with `startAt=app`; `device=phone` for the phone layout).

**Waves.** A (this plan): visual layer only. B: new UI on backends that already exist (BotResult inline card,
NowPlaying ×3, RemindersPopover with tabs, CodeBlock, CommandPicker restyle into `ui`, SensitivityMeter UI,
StageFilmstrip, ReactionTooltip shell, SharePicker shell, PersonContextMenu for what exists). C: server and core
gaps, where **the design wins** (Jonas, 2026-10-27): 10 files per message, Control music (DJ) as a role permission
under Voice, invites never / ∞, inline kick/ban confirm with "Not confirmed", reactor identities, server mute /
move / disconnect and drag, reminder snooze, VAD threshold and auto sensitivity, share resolution / fps, Polls and
Reminders bot capability bits, configurable idle leave and default volume. Wave A must not build any B or C
feature; where a restyled component needs a slot for one, leave the slot out.

## Global Constraints

- **Mesh is removed in this wave** (Jonas, 2026-10-27). `ThemeName` is `'signal' | 'light' | 'high-contrast'`. No
  `mesh` key, `[data-theme="mesh"]` block, toolbar item, Appearance option or string survives. A stored `mesh`
  preference reads as `signal`.
- Every value comes from a token. No hex, rgb or px colour literal in component CSS; `scripts/check-css-vars.mjs`
  must stay green (no undeclared `var(--x)`). A value the prototype uses that isn't a token becomes a token.
- **Token-name mapping.** README §5–§6 and the brief use brief names; implement them with these real tokens:

  | brief / §5 name | real CSS var | value (signal) |
  |---|---|---|
  | `--fg-2` (strong secondary) | `--fg-strong` | `#B9BFC8` |
  | `--fg-3` (secondary, idle icons) | `--fg-2` | `#8C939E` |
  | `--fg-4` (muted labels, meta) | `--fg-3` | `#767D89` |
  | `--fg-5` (decorative only) | `--fg-4` | `#5C636E` |
  | `--fg-body` | `--fg-body` | `#D5DAE1` |
  | `--live` | `--ok` | `#7CFF8E` |
  | `--selected` | `--surface-hi` | `#171B22` |
  | `--hover` | `--hover` (new key) | `#14171C` |
  | `--ease` | `--ease-out` | `cubic-bezier(0.16, 1, 0.3, 1)` |
  | `--edge` where it bounds a control (input, toggle-off track, segmented outline, composer at rest, keycap) | `--edge` | `#5C636E` (not the prototype's `#2A303A`) |
  | `--edge` on a floating layer, decorative card border | `--hairline-2` | `#232831` |
  | `--accent-soft`, `--accent-2`, `--accent-ink`, `--brand`, `--danger`, `--warn`, `--mention-ink` | same | §10.1 |

- Literal tints in §5 (`rgba(124,255,142,.08)` voice chip, `.04/.22` dock, `rgba(255,110,110,.1/.45)` toggled-on,
  `rgba(255,209,106,.05)` mention row, `#9EF5AC` live text, `#1A0606` leave ink, `rgba(3,4,6,.6)` scrim,
  `#0D1014` message hover) become tokens: `okSoft`, `okSoftBorder`, `okTint`, `okBorder`, `dangerSoft`,
  `dangerBorder`, `warnSoft`, `okText`, `dangerInk`, `scrim` in `ColorTokens`; message hover uses `bg3`.
- `--brand` is never text and never a control (brief, Tokens).
- No `fg3` (real name) text on `surfaceHi`; use `fg2` there (README §10.4).
- Type scale, weights, densities stay as they are (README §10.2); uppercase micro labels use the new
  `--label-tracking-wide: 0.12em`.
- Reduced motion zeroes every duration token except `--duration-meter`; `[data-motion="reduced"]` does the same
  for Storybook review.
- Phone (< 770px shell width): every target ≥ 44px, inputs 16px text, no top bar (README §5.10).
- Copy is final and verbatim from the prototype; tone per `brief-signal.md` (plain verbs, no reassurance).
- Silent encryption and the brief's exclusion list stay exactly as in `docs/design/brief.md`.
- Story titles follow the repo's prefixes (`Primitives/`, `Shell/`, `Conversation/`, `Voice/`, `Settings/`,
  `Search/`, `Media/`, `Form/`, `Ceremony/`), not the bundle's `Feed/`. Stories use no timers.
- Icons come from `packages/ui/src/Icon/icons.ts` (Lucide 1.57.0), never a CDN.

## Review Focus

1. **A user whose IndexedDB still holds `{v:1, theme:'mesh'}`** must land on Signal, not on `system` and not on a
   broken theme — test in Task 2 (`prefs.test.ts`).
2. **OS light or more-contrast with no explicit preference** must still pick light / high-contrast, and the
   `:root:not([data-theme])` media blocks must emit the new light/HC values — test in Task 1 (`css.test.ts`).
3. **Browser text size at 200 %** must not clip the 44px top bar, 28px rows or 64px panels: heights stay rem —
   assert in Task 1 that `topbarH`, `barH`, `panelH`, `rowH`, `rowHSub` end in `rem`.
4. **Axe `target-size` on strict story prefixes** with 22px reaction chips and 16px tags: interactive chips get a
   24px hit area (padding or `::before`) even where they draw 22px — enforced by `test:storybook` in Task 7.
5. **The contrast fix over the prototype**: an input, toggle-off track or segmented outline drawn in `hairline2`
   would pass visually and fail WCAG 1.4.11 — Task 4 adds a unit test that reads each control's computed border
   colour in the signal theme and asserts it equals `--edge`.

---

### Task 1: Signal tokens

**Files:**
- Modify: `packages/design-tokens/src/tokens.ts`, `packages/design-tokens/src/css.ts`
- Test: `packages/design-tokens/src/tokens.test.ts`, `packages/design-tokens/src/css.test.ts`
- Source values: `docs/design/signal/tokens/signal-theme.ts`

**Interfaces:**
- Produces: `ThemeName = 'signal' | 'light' | 'high-contrast'`; `ColorTokens` gains `fgBody, fgStrong, brand,
  hover, okSoft, okSoftBorder, okTint, okBorder, okText, dangerSoft, dangerBorder, dangerInk, warnSoft, scrim`;
  CSS vars `--shadow-3`, `--duration-press|enter|badge`, `--label-tracking-wide`, `--r-xl|xxl|bot`,
  `--panel-h`, and every new colour key in kebab case.

- [ ] **Step 1: Update the failing tests.** In `tokens.test.ts`: theme names are exactly
  `['signal','light','high-contrast']`; `structural.radius` equals the §10.2 object; `layout.railW '56px'`,
  `railTile '38px'`, `sidebarW '244px'`, `threadW '340px'`, `topbarH '2.75rem'`, `channelHeaderH '2.75rem'`,
  `barH '2.75rem'`, `panelH '4rem'`, `rowH '1.75rem'`, `rowHSub '1.625rem'`, `listGap '0.0625rem'`; `slotW` and
  `bottombarH` unchanged; every height key ends in `rem` (Review Focus 3). Add to `TEXT_PAIRS`: `fgBody/bg`,
  `fgStrong/surface2`, `fg2/surfaceHi`, `okText/bg2`, `fg3/surface2`. Add to `UI_PAIRS`: `ok/bg2`,
  `edge/surface`. `structural.radius` also has `seg: '7px'` (segmented control). In `css.test.ts`: `:root` has `--bg: #08090B`, `--accent: #5AA8E6`,
  `--r-md: 6px`, `--rail-w: 56px`, `--shadow-3: 0 30px 80px rgba(0,0,0,0.65)`, `--label-tracking-wide: 0.12em`;
  output contains no `mesh`; `[data-theme="signal"]` exists; the reduced-motion block and a
  `[data-motion="reduced"]` block each zero `press, fast, normal, slow, enter, badge, toast, pulse, caret, flash`
  and neither mentions `meter`; the `prefers-color-scheme: light` block contains `--accent: #005799`; the
  `prefers-contrast: more` block contains `--accent: #9CCBFF` (Review Focus 2).
- [ ] **Step 2:** `npm test -w @dilla/design-tokens` → FAIL on the new expectations.
- [ ] **Step 3: Implement.** Replace `mesh` by `signal` (values from `signal-theme.ts` plus `hover '#14171C'` and
  the tint tokens with the literal values listed in Global Constraints; `okTint` = voice chip fill `.08`,
  `okSoft` = dock fill `.04`, `okSoftBorder` = dock border `.22`, `okBorder` = chip border `.25`, `dangerSoft`
  `.1`, `dangerBorder` `.45`, `warnSoft` `.05`, `okText '#9EF5AC'`, `dangerInk '#1A0606'`, `scrim
  'rgba(3,4,6,0.6)'`). Light: `accent '#005799'`, `fgLink '#005799'`, `accent2 '#004A82'`, `accentInk '#FFFFFF'`,
  `accentSoft 'rgba(0,87,153,0.12)'`, `brand '#005799'`, `fgBody/fgStrong` from `backfill.light`, `hover
  '#E3E6E3'`, tints derived from its own `ok/danger/warn` at the same alphas, `scrim 'rgba(0,0,0,0.4)'`.
  High-contrast: `accent '#9CCBFF'`, `fgLink '#9CCBFF'`, `accent2 '#C7E2FF'`, `accentSoft
  'rgba(156,203,255,0.2)'`, `brand '#005799'`, backfill values, `hover '#1C1C1C'`, tints from its own colours,
  `scrim 'rgba(0,0,0,0.7)'`. `structural` takes `structuralSignal` (keep `slotW`, `bottombarH`). In `css.ts`:
  emit all `shadow` entries generically as `--shadow-<n>`, all motion durations generically (every key but
  `easeOut` → `--duration-<kebab>`), `--label-tracking-wide`; `:root` and `[data-theme="signal"]` from
  `themes.signal`; drop the mesh block; one `REDUCED` list shared by the media block and a new
  `[data-motion="reduced"]` block. If a contrast pair fails for light or HC, adjust that theme's new value (not
  the pair) and note the ratio in a comment.
- [ ] **Step 4:** `npm test -w @dilla/design-tokens && npm run build -w @dilla/design-tokens` → PASS; `dist/tokens.css` regenerated.
- [ ] **Step 5:** Commit `feat(tokens): Signal replaces Mesh; blue accent for light and high-contrast`.

### Task 2: Drop Mesh across the client, migrate the preference, merge the brief

**Files:**
- Modify: `packages/web/src/theme.ts`, `prefs.ts`, `screens/settings/Appearance.tsx`, `strings/en.ts`,
  `packages/ui/src/internal/roleColor.ts`, every `*.test.ts(x)` / `*.stories.tsx` that names `'mesh'` (list:
  `grep -rln "'mesh'" packages e2e`), `e2e/web/csp-axe.spec.ts`, `e2e/web/layout.spec.ts:986`,
  `e2e/web/search-settings.spec.ts:227`, `e2e/voice/support/voice.ts:216-220`, `e2e/web/core-worker.spec.ts:304`
  (the rejected value stays rejected; rename the literal to `'signal'`), `packages/ui/scripts/shoot-stories.mjs`
  and its test, `e2e/scripts/shoot-docs.mjs`, `e2e/ux/shots.mjs`, `docs/design/brief.md`,
  `scripts/check-design-brief.mjs`
- Test: `packages/web/src/prefs.test.ts`, `theme.test.ts`, `Appearance.test.tsx`

**Interfaces:**
- Consumes: Task 1 `ThemeName`.
- Produces: `ThemePref = 'system' | 'signal' | 'light' | 'high-contrast'`; `preferredTheme()` default `'signal'`.

- [ ] **Step 1: Failing tests.** `prefs.test.ts`: a stored `{v:1, theme:'mesh'}` reads as `'signal'` (Review
  Focus 1); `isThemePref('mesh')` is false. `theme.test.ts`: no OS preference → `'signal'`. `Appearance.test.tsx`:
  options are system / signal / light / high-contrast, labelled "signal".
- [ ] **Step 2:** `npm test -w @dilla/web -- prefs theme Appearance` → FAIL.
- [ ] **Step 3: Implement.** `themeOf` maps a legacy `'mesh'` to `'signal'` before `isThemePref`; no DB version
  bump (the next write stores `signal`). Replace every remaining `mesh` literal; `roleColor` falls back to
  `'signal'`; theme loops in e2e and shot scripts become `['signal','light','high-contrast']`; shot file names
  use `signal`. `shoot-docs.mjs` prose "Mesh theme" → "Signal theme".
- [ ] **Step 4: Brief.** Rewrite `docs/design/brief.md` sections `## Tokens`, `## Layout`, `## Motion`,
  `## Themes`, `## Brand mark` from `brief-signal.md` using the real token names (Global Constraints table); keep
  every other required heading, the vocabulary and the exclusion list verbatim; add a "What changed from Mesh"
  table under `## Purpose`. Delete `docs/design/brief-signal.md` (its content now lives in `brief.md`) and point
  `docs/design/signal/README.md` at `brief.md`. `npm run check:brief` → PASS.
- [ ] **Step 5:** `grep -rni "mesh" packages e2e scripts docs/design/brief.md --include='*.ts' --include='*.tsx'
  --include='*.mjs' --include='*.css' --include='*.md' | grep -v node_modules` → only historical comments; then
  `npm test -w @dilla/web && npm test -w @dilla/ui && npm run check:brief` → PASS.
- [ ] **Step 6:** Commit `feat(web): Signal is the default theme; a stored mesh preference becomes signal`.

### Task 3: Storybook, motion keyframes and icons

**Files:**
- Modify: `packages/ui/.storybook/preview.tsx` (from `docs/design/signal/storybook/preview.tsx`, minus the `mesh`
  item), `packages/ui/src/styles/base.css`, `packages/ui/src/Icon/icons.ts`, `packages/ui/src/Icon/Icon.test.tsx`

**Interfaces:**
- Produces: global keyframes `d-up, d-in, d-modal, d-sheet-up, d-pop, d-toast, d-bar, d-sweep, d-caret, d-ring,
  d-spin` (README §10.3 values; `d-` prefix replaces `s`); icon names `smile, reply, paperclip, bell, inbox, copy,
  download, loader, moon, link, trash-2, chevron-up, fingerprint, smartphone, key, square, arrow-right`.

- [ ] **Step 1:** Icon test: each of the 17 names renders an `<svg>` with Lucide 1.57.0 paths (copy from
  `lucide-static@1.57.0`, not from `dilla-icons.js`). → FAIL.
- [ ] **Step 2:** Add the icons, the keyframes and the preview (toolbar items `signal, light, high-contrast`,
  Motion toolbar, phone 390×844 and desktop 1280×800 viewports, `--bg` ground). The reduced-motion rule comes
  from Task 1's generated CSS; base.css adds nothing for it.
- [ ] **Step 3:** `npm test -w @dilla/ui && npm run build-storybook -w @dilla/ui && npm run test:storybook:ci -w @dilla/ui` → PASS.
- [ ] **Step 4:** Commit `feat(ui): Signal Storybook preview, motion keyframes, extra icons`.

### Task 4: Primitives

**Files:** CSS (and stories where a state is missing) of `Button, IconButton, Toggle, Segmented, Select,
TextField, Tag, Pill, KeyHint, Avatar, StateDot, Banner, EmptyState, Icon`; `Avatar.tsx` gains `shape`.
Test: `packages/ui/src/internal/controlEdges.test.tsx` (new).

**Interfaces:**
- Produces: `Avatar` prop `shape?: 'circle' | 'bot'` (default `'circle'`; `'bot'` → `--r-bot` and `--brand` fill).

- [ ] **Step 1:** `controlEdges.test.tsx` renders TextField, Select, Toggle (off), Segmented under
  `data-theme="signal"` with the built tokens loaded and asserts each control's boundary
  `getComputedStyle(...).borderColor` (Toggle: track outline) resolves to `--edge` `rgb(92, 99, 110)` (Review
  Focus 5). Avatar test: `shape="bot"` sets `data-shape="bot"`. → FAIL.
- [ ] **Step 2:** Restyle per README §5.9 controls (toggle 36×20, knob 16, 240ms slide; segmented radius
  `--r-seg`, picked option `--accent`; buttons 30px radius `--r-md`, destructive with a `--danger-border` border
  and `--danger` text; keycaps 22px with a 2px bottom border in `--edge`), avatars round (`--r-avatar` 999px), press `scale(.94)` over `--duration-press`, focus per brief.
- [ ] **Step 3:** `npm test -w @dilla/ui && npm run test:storybook:ci -w @dilla/ui` → PASS; compare each
  `Primitives/*` story with the prototype's settings and composer controls side by side.
- [ ] **Step 4:** Commit `feat(ui): Signal primitives; control borders meet 3:1`.

### Task 5: Top bar and shell frame

**Files:**
- Create: `packages/ui/src/TopBar/{TopBar.tsx,TopBar.css,TopBar.stories.tsx,TopBar.test.tsx}`
- Modify: `packages/ui/src/AppShell/{AppShell.tsx,AppShell.css}`, `packages/ui/src/styles/base.css` (phone rules),
  `packages/ui/src/index.ts`, `packages/web/src/screens/Shell.tsx`, `e2e/web/layout.spec.ts`,
  `e2e/web/panes.spec.ts`
- Delete: `packages/ui/src/StatusBar/` and its export, once nothing imports it.

**Interfaces:**
- Produces:
  ```ts
  interface TopBarProps {
    searchLabel: string;            // "Search or jump to…"
    searchShortcut: string;         // "⌘K" / "Ctrl K"
    onSearch(): void;
    voice?: { room: string; pingMs: number; onOpen(): void };   // chip only while connected
    remindersDue: number;           // badge when > 0
    onReminders(): void;
    onSettings(): void;
    labels: { reminders: string; settings: string; voice: string };
  }
  ```
  `AppShell` prop `topBar?: ReactNode` (replaces `statusBar`); grid per README §5.1 with the aside as the fourth
  column; the brand block spans rail + sidebar (`--rail-w` + sidebar width).

- [ ] **Step 1: Failing tests.** TopBar: renders the D-tile and "DILLA" with `aria-hidden` caret; the search
  trigger is a button named by `searchLabel` that calls `onSearch`; no voice chip without `voice`; chip text
  "harbour · 24 ms"; badge "3" only when `remindersDue = 3`; settings and bell are 28px icon buttons with
  accessible names. AppShell: with `topBar` the bar renders above rail and main; on a phone-width container it
  isn't rendered. e2e: `layout.spec.ts` B1 bars at y = 44 + 44 under the top bar (re-derive each `expectBar`
  `want` from §5), R22 rail 56 / tiles 38, settings side 240 stays; `panes.spec.ts` user panel 64, sidebar
  default 244. → FAIL.
- [ ] **Step 2:** Build TopBar and the grid (§5.1–§5.2: caret blink `d-caret` with `--duration-caret`, gear
  rotates 45° over 200ms, chip enters `d-up`). Web: mount `topBar` in `Shell.tsx`; search → the existing palette
  open, bell → the existing `ReminderInbox` dialog (the popover is wave B), gear → settings, chip → voice stage;
  due count from the existing reminders state. ⌘K behaviour unchanged.
- [ ] **Step 3:** `npm test -w @dilla/ui && npm test -w @dilla/web && npm run test:e2e:web:chromium -w @dilla/e2e -- layout panes` → PASS.
- [ ] **Step 4:** Commit `feat(ui,web): Signal top bar and shell frame`.

### Task 6: Rail, sidebar, user panel, voice dock

**Files:** CSS + stories of `CommunityRail, ChannelList, ChannelRow, VoiceChannelRow, UserPanel, VoiceDock,
ChannelHeader, TabBar, PhoneHeader`; e2e geometry already updated in Task 5.

- [ ] **Step 1:** Add the §8 stories that don't exist (ChannelRow: Text, Active, Unread, Mention,
  CollapsedButUnread, VoiceJoined, Afk; VoiceChannelRow: Empty, WithPeople, Speaking, MutedDeafened,
  CameraAndLive, WithGlenn — DragTarget is wave C). Unit tests: speaking is a ring only (no `AudioMeter` inside
  `VoiceChannelRow`); a collapsed category still lists its active and unread rows; Glenn uses `Avatar
  shape="bot"`, a BOT tag and "playback only". → FAIL where behaviour changes.
- [ ] **Step 2:** Restyle per README §5.3–§5.4 and §5.10 (rail tile morph 220ms, selected ring, mention badge
  `d-pop`, add-server rotate; category 26px with trailing hairline; row state table; active bar `scaleY`; user
  panel 64px with status dot colours; dock with tint tokens; channel header 44px; tab bar 56px with
  `env(safe-area-inset-bottom)` and a `scaleX` accent bar; phone header 52px, back 44px). Remove the in-list
  meter.
- [ ] **Step 3:** `npm test -w @dilla/ui && npm run test:storybook:ci -w @dilla/ui && npm run test:e2e:web:chromium -w @dilla/e2e -- layout panes` → PASS.
- [ ] **Step 4:** Commit `feat(ui): Signal rail, sidebar and panels`.

### Task 7: Conversation

**Files:** CSS + stories of `MessageLog, MessageRow, MessageToolbar, ReactionBar, ReplyChip, TypingIndicator,
AttachmentCard, AttachmentTray, DropOverlay, Composer, MessageEditor, MentionList, EmojiGrid, GifPicker`.

- [ ] **Step 1:** Stories per §8 for MessageRow (First, FollowUp, Hover forced via a `data-hover` story arg,
  MentionOfYou, Reply, ReplyFlash, Reactions, Thread, Attachments, GifAttachment — code-block stories are wave
  B) and Composer (Empty, Focused, Typing, Reply, AttachmentsUploading, AttachmentTooLarge, Phone). Unit tests:
  Send is disabled-styled (`data-empty`) with no text; Enter sends, Shift+Enter inserts a newline (keep existing
  tests); the reply bar squares the box's top corners (`data-replying`); interactive reaction chips have a ≥ 24px
  hit area (Review Focus 4). → FAIL where new.
- [ ] **Step 2:** Restyle per README §5.5 and §6.5/§6.8 visual parts (grid `44px 30px 1fr`, 10px gap, hover
  `bg3` + 2px accent bar, toolbar `d-up` 3px, mention row `warnSoft`, entrance `d-up` 380ms with 40ms stagger on
  first paint only, channel-switch fade 140ms; composer radius `--r-lg`, `--edge` at rest, accent + 3px soft ring
  on focus, typing sweep `d-sweep` 1.4s, `›` prompt, "send ↵", footer buttons 26×24 and the `</>` button
  inserting a fenced block; reply connector and mini avatar; reaction chips 22px radius 5px, own ones accent).
  The 25 MB and file-count limits keep today's values (C changes the count).
- [ ] **Step 3:** `npm test -w @dilla/ui && npm test -w @dilla/web && npm run test:storybook:ci -w @dilla/ui` → PASS.
- [ ] **Step 4:** Commit `feat(ui): Signal conversation and composer`.

### Task 8: Voice

**Files:** CSS + stories of `CallStage, CallTile, CallBar, CallControls, AudioMeter, RingBanner, MediaPlayer`.

- [ ] **Step 1:** Stories: CallStage Grid2, Grid3, Grid5, SpeakingTile, FocusShare, FocusPerson,
  NotJoinedPreview, Phone; CallControls Default, Muted, Deafened, CameraOn. Unit/story test: tiles keep
  `aspect-ratio: 16/9` and the grid never stretches them (tile height ≤ width × 9/16 + 1 at 1280×800 with 2
  tiles). → FAIL.
- [ ] **Step 2:** Restyle per README §5.6 (grid `repeat(auto-fill, minmax(280px,1fr))` centred; tile radius
  `--r-xl`; speaking edge, 5px ring and `scale(1.05)` over 260ms; focus fills at 16:9 with the existing
  `focusedId` and the others in a ~160px strip below, Grid button; call bar `--panel-h` with 40px labelled
  buttons and keycaps, toggled-on danger tints, Camera on accent, Leave `--danger` / `--danger-ink`; phone icons
  only; not-joined preview with overlapping 48px avatars and a green Join voice; RingBanner on `surface2`, live
  border, `d-ring`).
- [ ] **Step 3:** `npm test -w @dilla/ui && npm run test:storybook:ci -w @dilla/ui && npm run test:e2e:voice:chromium -w @dilla/e2e` → PASS.
- [ ] **Step 4:** Commit `feat(ui): Signal voice stage and call bar`.

### Task 9: Overlays

**Files:** CSS + stories of `Dialog, ConfirmDialog, ActionMenu, Tooltip, ProfileCard, SearchPalette, ThreadPane,
PinsDialog, Lightbox`.

- [ ] **Step 1:** Stories: SearchPalette Empty, Query, MessagesResults, NoResults, Phone; ProfileCard Person,
  Owner, Bot, You, PhoneSheet. Test: the palette is centred with `left:0; right:0; margin:0 auto` and has no
  `transform` once its entry animation ends; footer reads "Searches messages held in this browser". → FAIL.
- [ ] **Step 2:** Restyle per README §5.7–§5.8 and brief Elevation (floating layers `--shadow-2` plus a
  `--hairline-2` border, radius `--r-xxl`, scrim `--scrim`, `d-modal` 220–260ms; palette top 14vh, width
  `min(600px, 92%)`, 50px input row, ≥ 38px results with the selected accent bar; profile card 300px with a
  135° hue band, 56px avatar and 4px ring, phone bottom sheet `d-sheet-up`; thread aside 340px).
- [ ] **Step 3:** `npm test -w @dilla/ui && npm run test:storybook:ci -w @dilla/ui && npm run test:e2e:web:chromium -w @dilla/e2e -- search-settings` → PASS.
- [ ] **Step 4:** Commit `feat(ui): Signal overlays, palette and profile card`.

### Task 10: Settings and onboarding

**Files:** CSS + stories of `SettingsFrame, SettingsNav, SettingsRow, SettingsFooter, VoiceSettings,
PermissionGrid, ColorSwatches, InviteLink, ManageList, DeviceRow, FactorRow, KeyboardTable, ChannelSettings,
OnboardingFrame, RecoveryKey, RecoveryKeyField, ChoiceCards, CodeList, QrCode`; `packages/web/src/screens/settings/workspace.css`.

- [ ] **Step 1:** Stories: VoiceSettings VoiceActivity, PushToTalk, PushToTalkListening (existing controls only;
  the sensitivity meter is wave B, threshold wave C); PermissionGrid Everyone. Keep the existing settings-frame
  geometry and recovery-key print checks in `check-stories.mjs` green, updating their expected numbers to §5.9
  (nav 240px, content max 720px, row ≥ 56px). → FAIL where numbers change.
- [ ] **Step 2:** Restyle per README §5.9 and §5.10 settings bullets (close × 34px circle rotating 90° with ESC
  under it; `d-modal` 260ms; 32px icon tiles; 10px-radius grouped rows with hairlines; phone nav as a chip row,
  group headings hidden). Onboarding and auth pick up surfaces, radii and buttons only.
- [ ] **Step 3:** `npm test -w @dilla/ui && npm test -w @dilla/web && npm run test:storybook:ci -w @dilla/ui && npm run test:e2e:web:chromium -w @dilla/e2e` → PASS.
- [ ] **Step 4:** Commit `feat(ui): Signal settings and onboarding`.

### Task 11: Shell stories, screenshots, user docs

**Files:** `packages/ui/src/AppShell/AppShell.stories.tsx`, `PhoneShell.stories.tsx`,
`VoiceStates.stories.tsx`, `packages/ui/src/internal/sample/midgard.ts` (new), `README.md`, `docs/user/*`,
`docs/user/screenshots/*`

- [ ] **Step 1:** `midgard.ts` holds README §11 sample data (Midgard Crew and Valhalla Kitchen, Ada / Björn /
  Mira / Sven / Liv / Glenn with their hues). AppShell stories Chat, ChatMembersOpen, ChatThreadOpen, Voice, Dm,
  Settings/Voice, Settings/Roles; PhoneShell List, Conversation, Activity, You, ThreadSheet. Every seeded state
  must be one the shipping client can reach (no wave B/C UI in these stories).
- [ ] **Step 2:** `npm run test:storybook:ci -w @dilla/ui` and `node packages/ui/scripts/shoot-stories.mjs` →
  PASS; review the signal PNGs of every Shell story at 1280×800 and 390×844 next to the prototype.
- [ ] **Step 3:** `npm run docs:shots -w @dilla/e2e`, then update `README.md` "what works" (Signal look) and any
  `docs/user` text that names Mesh or describes green selection.
- [ ] **Step 4:** Full gate: `npm test && npm run typecheck -w @dilla/ui && npm run typecheck -w @dilla/web && npm run test:e2e:web -w @dilla/e2e` → PASS.
- [ ] **Step 5:** Commit `docs: Signal screenshots and user docs`.
