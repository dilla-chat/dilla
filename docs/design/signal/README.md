# Handoff: Signal, the dilla web client redesign

> Target: `jonasthim/dilla`, `packages/design-tokens` and `packages/ui` (React 19, Vite, Storybook 10).
> Spec: `docs/design/brief.md` (the merged design brief; `docs/design/brief-signal.md` is the original handoff brief, included here as `brief-signal.md`).
> Reference build: `Dilla Signal Prototype.dc.html` (included).

---

## 1. Overview

Signal is the new visual language for the dilla web client. It replaces Mesh (`docs/design/brief.md`). The goal
was a client that feels finished enough to pay for, and calm enough to bring into a company. It keeps what
made dilla recognisable: dark UI, mono type everywhere, the serif-italic server name, and silent encryption.

What changes:

- **Colour roles.** A blue accent (`#5AA8E6`, a tint of Göteborgsblå `#005799`) means *you*: selection,
  focus, primary actions, your own reactions. Green (`#7CFF8E`) now means only *live*: voice connected,
  speaking, online, success. Yellow is attention, red is stop or failed.
- **Shape and depth.** Radii go from 0–3px to 4–12px. Floating layers get soft drop shadows. Still no blur.
- **Motion.** A motion system with one easing curve replaces the seven permitted animations.
- **Layout.** A 44px top bar runs across the whole window, the rail is 56px, the sidebar 244px.
- **New features in the design.** Glenn (the built-in bot): command picker, private results, polls, music,
  reminders, moderation. Also code blocks with highlighting, reply previews, reaction tooltips, the
  attachment tray, people context menus and drag between voice channels, a focused voice stage, the share
  picker, and detailed voice settings (sensitivity threshold on a live meter).

## 2. About the design files

The files in this bundle are **design references built in HTML**. They are a prototype that shows the
intended look and behaviour; they are **not production code to copy**. Rebuild them in `packages/ui` with
its existing patterns: one folder per component (`X.tsx`, `X.css`, `X.stories.tsx`, `X.test.tsx`), CSS
variables from `@dilla/design-tokens`, `rem` type, `Icon` for glyphs, `Tooltip`, `Dialog` and `ActionMenu`
for overlays, and the vitest and axe tests that already exist.

The prototype uses inline styles and literal hex values because of the tool it was built in. In the
codebase every value must come from a token. If a value isn't in section 6, read it off the prototype and
add a token rather than hard-coding it.

## 3. Fidelity

**High fidelity.** Colours, type, spacing, radii, motion and copy are final for the dark theme. Rebuild them
pixel-for-pixel, with two exceptions:

1. **Contrast fixes in section 6.4 take priority over the prototype.** A few prototype borders don't reach
   the WCAG 1.4.11 3:1 non-text contrast that `contrast.test.ts` enforces.
2. **Light and high-contrast themes were not redesigned.** See section 6.5.

## 4. How to implement, step by step

Work in this order. Each step should leave Storybook green (`npm run test:storybook` plus the a11y
`test: 'error'` setting already in `preview.tsx`).

### Step 1: tokens (`packages/design-tokens`)

1. Add a `signal` theme to `themes` in `src/tokens.ts` with the values in `tokens/signal-theme.ts`. Add
   `'signal'` to `ThemeName`.
2. Add the new colour keys `fgBody`, `fgStrong` and `brand` to `ColorTokens`, and fill them for `mesh`,
   `light` and `high-contrast` as well. For those themes, reuse their existing `fg`, `fg2` and `accent`
   values, so nothing changes visually.
3. Replace `structural.radius`, `structural.shadow`, `structural.motion` and the changed `structural.layout`
   entries with the Signal values. Structural tokens are shared by every theme, so this restyles all of them.
   That's intended.
4. In `src/css.ts`, emit `:root` from `themes.signal` instead of `themes.mesh`. Keep `[data-theme="mesh"]` so
   the old look stays reachable until removal. Add the new duration keys (`press`, `enter`, `badge`) to
   the reduced-motion block.
5. Extend `TEXT_PAIRS` and `UI_PAIRS` with the pairs in section 6.4, then run the tests. They must pass.
6. Rebuild (`npm run build` in the package), so that `dist/tokens.css` updates.

### Step 2: Storybook (`packages/ui/.storybook`)

Storybook is already set up (`@storybook/react-vite` and `addon-a11y`). Replace `preview.tsx` with
`storybook/preview.tsx` from this bundle. It:

- adds `signal` to the theme toolbar and makes it the default;
- adds a **Motion** toolbar item (`full` / `reduced`) that sets `data-motion="reduced"` on `<html>`. Add one
  rule to `base.css` that zeroes the duration tokens under `[data-motion="reduced"]`, so reviewers can
  check reduced motion without changing their OS setting;
- adds a 390×844 **phone** viewport and a 1280×800 **desktop** viewport;
- sets a `--bg` page background so stories render on the real ground.

### Step 3: restyle existing components

Most existing components change only in CSS. Do them in the order in section 7.1 (shell first, then rows,
then overlays). For each one, open its story next to the matching prototype state, then fix values until they
match. Add any missing states as stories (section 8).

### Step 4: new components

Build the components in section 7.2. Each one gets a folder, stories covering every state in section 8, a
vitest render test, and an axe test.

### Step 5: shell stories

Update `AppShell.stories.tsx`, `PhoneShell.stories.tsx` and `VoiceStates.stories.tsx` to use the Midgard
Crew sample data from the prototype (section 11). Whole screens can then be reviewed in Storybook.

### Step 6: guard against drift

`scripts/shoot-stories.test.mjs` and `check-stories.mjs` already exist. Add the new stories to the screenshot
set and commit the baselines. Point design review at the Storybook build.

## 5. Screens and views

All sizes are at a 16px root, desktop unless noted. Below **770px of shell width** the phone layout applies
(section 5.10).

### 5.1 Shell frame

```
┌────────────────────────── top bar 44px (bg-2, hairline below) ─────────────────────────┐
│ [D] DILLA▌ (262px)   │    [ search 420px max, centred ]    │ voice chip · bell · gear │
├──────┬──────────────┬───────────────────────────────────────────────┬────────────────┤
│ rail │ sidebar      │ main                                          │ aside          │
│ 56px │ 244px        │ minmax(0,1fr)                                 │ 0 / 232 / 340  │
│ bg-2 │ bg-2         │ bg                                            │ bg-2           │
└──────┴──────────────┴───────────────────────────────────────────────┴────────────────┘
```

- Grid: `grid-template-columns: 56px 244px minmax(0,1fr) auto`, `grid-template-rows: 44px minmax(0,1fr)`.
- Vertical hairlines (`--hairline`) between every pane.
- The aside animates `width` 0 ↔ 232px (members) or 340px (thread), over 280ms with `--ease`. Its content
  keeps a fixed inner width, so text doesn't reflow during the transition.

### 5.2 Top bar

- **Brand** (left, 262px wide, so the brand area spans the rail and sidebar): an 18px square D-tile, radius
  4px, background `--brand` `#005799`, white 11px/700 "D". Next to it, "DILLA" in 11px/700, tracking 0.12em,
  then a 6×12px `--brand` caret that blinks (`--duration-caret` 1s, steps).
- **Search** (centre): max 420px, 28px tall, radius 6px, `--surface-2` fill, `--hairline-2` border, a 13px
  search icon, "Search or jump to…" in `--fg-4`, and a ⌘K keycap (10px, radius 4px, `--edge` border). On
  hover the border turns `--accent` and the text `--fg-2`. Click or ⌘K opens the palette (5.8).
- **Right group**, gap 6px:
  - **Voice chip** (only while connected): 28px tall, radius 6px, `rgba(124,255,142,.08)` fill,
    `rgba(124,255,142,.25)` border. Contents: three 2×12px green meter bars (`sBar` 600ms, staggered
    200ms), then "harbour · 24 ms" in 11.5px `#9EF5AC`. Click opens the voice stage. Enters with `sUp`
    300ms.
  - **Reminders bell**: 28×28px icon button. When reminders are due, a `--warn` count badge pops in
    (`--duration-badge`). Click opens the reminders popover (6.x).
  - **Settings gear**: 28×28px. On hover it rotates 45° over 200ms.

### 5.3 Rail (56px)

- Server tiles are 38×38px. At rest they are circles (`--r-pill`) on `--hover` `#14171C`. When selected, the
  radius morphs to 10px, the fill becomes `--accent` with `--accent-ink` text, and a ring
  `0 0 0 2px var(--bg-2), 0 0 0 3.5px var(--accent)` appears. The morph takes 220ms with `--ease`. Hovering a
  tile at rest also morphs it to 10px.
- The Home tile (@ icon): selected fill `--selected`, icon `--accent`.
- 20×1px `--hairline-2` rule under Home.
- Mention badge: bottom-right, 16px tall, `--warn` fill, `--mention-ink` 9.5px/700 text, ring
  `0 0 0 2.5px var(--bg-2)`, pops in.
- Add-server button: 38px dashed `--edge` circle. On hover it rotates 90° and turns `--accent`.

### 5.4 Sidebar (244px)

- **Header**: 14px 12px 8px 14px padding. Server name in DM Serif Display italic 19px, ellipsis. Then a 26px
  chevron button that opens server settings.
- **Stat line**: 10.5px `--fg-4`, for example "● 3 online · 2 in voice". The dot is 6px `--live`.
- **Scroller**: 4px 8px padding, hairline above.
- **Category row**: 26px tall, margin-top 10px. Name in 10px/600 uppercase, tracking 0.12em, `--fg-4`,
  followed by a hairline filling the rest of the row. The chevron rotates −90° when the category is collapsed
  (200ms). A collapsed category still shows its active and unread rows.
- **Channel row**: 28px tall (44px on phone), radius 5px, padding 0 8px, gap 8px. A 14px glyph (# for text,
  speaker for voice, moon for AFK), then the name. States:

  | state | text | glyph | bg | weight | extra |
  |---|---|---|---|---|---|
  | idle | `--fg-3` | `--fg-5` | — | 400 | |
  | hover | `--fg` | — | `--hover` | — | |
  | active | `--fg` | `--accent` | `--selected` | 600 | 2px `--accent` bar on the left edge, inset 6px top and bottom, animates `scaleY(0→1)` over 220ms |
  | unread | `--fg` | — | — | 600 | "1 new" tag: 16px tall, radius 4px, `--accent` fill, `--accent-ink` 10px/700, pops in |
  | mention | `--fg` | — | — | 600 | "@2" tag in `--warn` / `--mention-ink` |
  | voice, you're in it | `--fg` | `--live` | — | 600 | occupant count on the right, 10.5px `--fg-4` |

- **Voice occupant sub-row**: 26px tall, padding-left 30px. An 18px avatar, the name in 12px `--fg-2`, then
  status glyphs on the right (12px each): camera `--fg-3`, a LIVE tag, mic-off and headphone-off in
  `--danger`. **Speaking is shown only as a ring on the avatar**:
  `0 0 0 2px var(--bg-2), 0 0 0 3.5px var(--live)`, 150ms. There is no audio meter in the list (it was
  removed on purpose). Glenn shows a rounded-square avatar, a BOT tag and "playback only".
- **Drag to move**: anyone with Move members can drag an occupant row onto another voice row. The target row
  gets an `--accent-soft` fill and an `--accent` 1px inset outline. Dropping moves the person and shows a
  toast "Moved Björn to lounge".
- **Context menu** on an occupant (right-click, or long-press on phone): a User volume slider (0–200%), Mute
  for me, Move to ▸ (voice channels), Server mute and Disconnect (in `--danger`, behind the permission),
  Copy username. Use `ActionMenu`.
- **Voice dock** (above the user panel, while connected and not already on that stage): margin 0 8px 8px,
  padding 10px, radius 8px, `rgba(124,255,142,.04)` fill, `rgba(124,255,142,.22)` border. It holds the meter
  bars, the room name (12px/600 `#9EF5AC`), "24 ms · ↑ 38 ↓ 71 kb/s" (10.5px tabular `--fg-4`) and a
  leave button (28px, `--danger` icon; on hover the fill turns `--danger` with dark ink). When Glenn is
  playing, a compact now-playing row sits inside the dock (6.6).
- **User panel**: **64px tall, the same height as the voice call bar**, so their top edges line up across
  the window. Hairline above. A 30px avatar with a status dot (9px, ring 2px `--bg-2`; online `--live`, idle
  `--warn`, dnd `--danger`, invisible `--fg-4`). Then the name (12px/600) and status line (10.5px `--fg-4`),
  and mute and deafen buttons (30px, radius 6px; when on, `--danger` icon on `rgba(255,110,110,.1)`). Click
  the avatar block to open the status menu.

### 5.5 Conversation (main)

- **Header**: 44px tall, padding 0 10px 0 20px, hairline below. Topic in 12px `--fg-3` with ellipsis. On the
  right, a Threads button (icon plus label) and a Members button (icon plus count): 28px tall, radius 6px.
  While its aside is open, a button shows `--accent` text on `--accent-soft`.
- **Feed**: a `column-reverse` scroller, so it stays pinned to the bottom unless the user scrolls up.
  - **Welcome block**: a 40px # tile (radius 8px, `--surface`, `--hairline-2` border, `--accent` glyph),
    then "Welcome to #general" (18px/700) and "This is the start of #general." (12px `--fg-3`).
  - **Day divider**: "TODAY" in 10px/600 uppercase, tracking 0.12em, `--fg-4`, followed by a hairline.
  - **Message row**: grid `44px 30px minmax(0,1fr)` with a 10px gap. Padding is 7px 16px 3px 0 for the
    first message in a group and 1px 16px for follow-ups. The time gutter (10.5px tabular) is always shown
    on the first message and appears on hover for follow-ups. A 30px circular avatar. The name is
    12.5px/700 in the person's role colour. The body is 13px, line-height 1.55, `--fg-body`, `pre-wrap`,
    `overflow-wrap: anywhere`.
  - **Hover**: `#0D1014` fill and a 2px `--accent` inset bar on the left. A toolbar floats at top −12px,
    right 16px (react, reply, thread, more): radius 6px, `--surface-2` fill, `--edge` border, shadow
    `--shadow-1`. It fades and rises 3px over 120–180ms.
  - **Mention of you**: `rgba(255,209,106,.05)` fill, a 2px `--warn` bar, and the "@Mira" text set as a
    chip (radius 3px, `--warn` fill, `--mention-ink`).
  - **Entrance**: `sUp` (rise 8px and fade in) over 380ms, staggered 40ms per row on first paint. Newly
    arriving messages get no delay.
  - **Channel switch**: the feed fades out with a 6px rise over 140ms, the content swaps, then it fades back in.
- **Composer**: padding 0 16px 14px. Box radius 8px, `--surface` fill, `--hairline-2` border; on focus the
  border turns `--accent` with a 3px `--accent-soft` ring.
  - A 2px typing sweep line sits on the box's top edge (a gradient travelling across in 1.4s, visible only
    while someone is typing). "Björn is typing…" appears in `--accent` in the footer row.
  - Input row: a `›` prompt in `--accent`, then a multi-line textarea. Enter sends, Shift+Enter adds a
    line. Then the Send button ("send ↵", 28px, radius 5px). Send is `--accent`/`--accent-ink` when there's
    text, `--selected`/`--fg-5` when empty.
  - Footer row (26×24px buttons): attach (25 MB per file), emoji, GIF, `</>` (inserts a fenced code block),
    then on the right the typing text and the "⇧↵ new line" hint (hidden on phone).
  - Reply bar (above the box when replying): ↳ icon, "Replying to **Ada**", a one-line excerpt, and a ×
    button. The box's top corners become square so the two join. The replied-to message is tinted
    `--accent-soft` while composing. Esc cancels.

### 5.6 Voice stage

- **Tiles are 16:9 and never stretch to fill the height.** Grid view: `repeat(auto-fill, minmax(280px,1fr))`,
  `aspect-ratio: 16/9`, centred vertically in the space.
- Tile: radius 10px, `--bg-2` fill. An avatar tile shows a 76px circle and the name. A camera tile shows the
  video. A share tile shows the shared screen with a LIVE tag. Stats top-left (10px tabular `--fg-4`, for
  example "18 ms · OPUS 48k"). The speaking tile gets an inset 1px `rgba(124,255,142,.6)` edge, the avatar a
  5px `--live` ring, and `scale(1.05)` over 260ms.
- **Focus**: click a tile to focus it. It fills the stage at 16:9 and the others move to a filmstrip below
  (tiles about 160px wide). A **Grid** button returns to grid view. A new screen share takes focus
  automatically. Your own share isn't auto-focused.
- **Now playing**: when Glenn is playing in this room, the now-playing bar sits at the top of the stage
  (6.6).
- **Call bar**: **64px**, hairline above, `--bg-2` fill. On the left: the room name, then
  "24 ms · ↑ 38 ↓ 71 kb/s · hearing 2 of 2". On the right, 40px buttons, each with an icon, a label and a
  keycap: Mute ⌘⇧M, Deafen ⌘⇧D, Camera, Share. Then Leave (`--danger` fill, `#1A0606` ink). Toggled-on
  states use `rgba(255,110,110,.1)` fill with a `rgba(255,110,110,.45)` border, except Camera on, which is
  `--accent`. On phone the buttons show icons only, centred.
- **Share picker** (popover above the Share button): the source (screen, window or tab), resolution (720p,
  1080p, source), frame rate (15, 30, 60), a live upload estimate (for example "≈ 2.5 Mb/s up"), a
  system-audio toggle, then Start sharing.
- **Not-joined preview**: overlapping 48px avatars of the people inside (with speaking rings), the room
  name, "Ada and Björn are here", and a green **Join voice** button.

### 5.7 Members aside (232px) and profile card

- Header 44px, "Members" and a close ×. Groups by role: a 10px uppercase heading in the role colour with a
  3×10px colour bar, then "— n".
- Row: 44px tall, radius 6px, a 28px avatar with a status dot, the name in the role colour (crown for the
  owner), and the status line in 10.5px `--fg-4`. Bots sit in their own "Bots" group with the rounded-square
  avatar.
- **Profile card**: a 300px popover beside the clicked element (on phone, a bottom sheet radius 16px 16px 0 0
  with a scrim). A 56px colour band (the person's hue, a 135° gradient), a 56px avatar overlapping it with a
  4px ring, name and WEB tag, @handle and status, bio, role chips, then Message (`--accent`), a safety-number
  shield and Edit roles. Enters with `sModal` 260ms.

### 5.8 Search palette (⌘K)

- Centred, `top: 14vh`, width `min(600px, 92%)`, radius 12px, `--surface` fill, `--edge` border, shadow
  `0 30px 80px rgba(0,0,0,.65)`, over a scrim `rgba(3,4,6,.6)`. Enters with `sModal` 220ms. **Centre it with
  `left:0; right:0; margin:0 auto`, not with a transform**: a transform clashes with the entry animation.
- Input row 50px. Results are grouped (Channels, People, Messages, Actions). Each row is at least 38px with a
  24px icon or avatar, a label and a sub-line. The selected row has `--selected` fill and a 2px `--accent`
  inset bar. ↑↓ move, ↵ opens, Esc closes. Footer: "Searches messages held in this browser".

### 5.9 Settings (full viewport)

- Covers the app, enters with `sModal` 260ms. Close × at the top right (34px circle, rotates 90° on hover)
  with ESC under it.
- Grid `240px minmax(0,1fr)`, max 1280px, centred. The nav has grouped headings "You" and the server
  name. Items are 34px, radius 6px, with the same active bar as channel rows. Content padding 36px 40px 60px,
  column max 720px, title 20px/700, sub-line 12px `--fg-3`.
- **Setting row**: at least 56px, padding 10px 16px, wraps on narrow widths. A 32px icon tile (radius 8px,
  `--accent` glyph), label (12.5px/600) and description (11px `--fg-3`), and the control on the right. Rows
  sit in a 10px-radius bordered group with hairlines between them.
- Controls: toggle 36×20px (knob 16px, slides over 240ms); segmented control (radius 7px, the picked option
  `--accent`); buttons (30px, radius 6px; destructive ones have a `rgba(255,110,110,.4)` border and `--danger`
  text); keycaps (22px, 2px bottom border).
- **Voice section** (the most detailed):
  - Input and output device selects, each with a volume slider 0–200% showing an `--accent` fill.
  - **Input sensitivity**: a 50-segment live meter from −100 to 0 dB. Segments below the threshold are
    `--fg-5`; above it they are `--live`, and above −10 dB `--warn`. A **white draggable threshold line**
    sits on the meter with a dB label above it, under a scale reading −100 / −50 / 0 dB. **Automatic
    sensitivity** (on by default) fixes the line at the computed value and disables dragging, with the
    cursor `not-allowed`. Under the meter: a status pill ("You're being heard" in green, or "Too quiet to
    send"), the current level and the threshold value. **Let's check** starts and stops a 10-second test
    that only you hear.
  - Input mode: Voice activity or Push to talk. With push to talk you also get the key (Change → "Press a
    key…") and a release delay slider.
  - Toggles for noise suppression (Automatic / Lightweight / Off, tagged BETA), echo cancellation and
    automatic gain.
- **Roles**: a 200px role list (colour dot, name, member count, "New role") next to an editor: name, six
  colour swatches, and permission groups (General, Moderation, Voice) as toggle rows. **Voice includes
  "Control music (DJ)"**. That is where DJ is set; the Music page only links to it.
- **Invites**: expiry (1 hour, 1 day, 7 days, never) and uses (1, 10, ∞) as segmented controls. "Make
  invite link" shows the link once with Copy, then lists active links with Revoke.
- **Bot**: Glenn's card (managed bot, version, Restart). Toggles for Music, Polls, Reminders and Moderation;
  turning one off removes its commands from the picker.
- **Music**: YouTube source (needs yt-dlp on the host; shows a status), default volume, and an idle-leave
  timeout. "Who can DJ" shows the roles that have Control music, with a link to Roles.
- **Devices, Sign-in, Notifications, Keyboard, Overview**: the existing content, restyled to the row
  pattern above.

### 5.10 Phone (< 770px)

- One pane at a time. Grid `56px minmax(0,1fr)` while the list shows; the conversation takes the full width.
- No top bar. The tab bar at the bottom (Home, Activity, You) is 56px with
  `padding-bottom: env(safe-area-inset-bottom)`. The active tab has `--fg` text and a 2px `--accent` bar on
  top that grows with `scaleX`.
- Conversation header 52px, with a 44px back button.
- Every target is at least 44px. Channel rows are 44px and DM rows 52px. Inputs use 16px text so iOS
  doesn't zoom.
- The message toolbar appears on tap, not hover. The time gutter is hidden.
- Members aside and profile card become full-screen or bottom sheets (`sSheetUp` 300ms).
- The search palette sits 12px from the top at full width minus 24px.
- Settings: the nav becomes a horizontal scrolling row of chips at the top; group headings are hidden.

## 6. Glenn and the other new features

### 6.1 Command picker (`/`)

- Typing `/` at the start opens a list above the composer. Each command shows its name, a description and
  its required permission. The list filters as you type. ↑↓ move, Tab or Enter picks.
- Picking a command turns the composer into **inline field chips**: `/remind  when: [in 2h ▾]  what: [____]`.
  Field types and their suggestions:
  - member: list of people;
  - duration: 10m, 1h, 1d, …;
  - time: shows an exact preview, for example "Fri 30 Oct, 20:00 CET";
  - text;
  - YouTube query or URL.
- Tab and Shift+Tab move between fields. Enter runs the command; it's disabled until required fields are
  valid. Esc leaves the command but keeps the draft text. An error shows under its field in `--danger`
  10.5px.
- Typing `//` posts a literal "/".
- Commands: /play, /pause, /resume, /skip, /stop, /queue, /volume, /poll, /remind, /kick, /ban, /timeout,
  /warn.

### 6.2 Private result

- A Glenn message (rounded-square 30px avatar in `--brand`, BOT tag) with a status chip:

  | status | icon | label | fg | bg |
  |---|---|---|---|---|
  | pending | loader (spins 900ms) | Working | `--accent-2` | `--accent-soft` |
  | success | check | Done | `--live` | `rgba(124,255,142,.1)` |
  | failed | x | Failed | `--danger` | `rgba(255,110,110,.1)` |
  | cancelled | — | Not confirmed | `--fg-3` | `--hover` |

- Under it: "Only you can see this · Dismiss" in 10.5px `--fg-4`.
- Example failure copy:
  - "Join a voice channel first. Glenn plays where you are."
  - "Playback needs a role with Control music. An owner sets it in Server settings → Roles."
  - "Glenn is in harbour. Join it to control playback."
- **Kick and ban** show an inline confirm step inside the result first ("Kick Björn? They can rejoin with a
  new invite." with Kick and Cancel buttons). Cancelling gives "Not confirmed".

### 6.3 Poll card

- Question, single or multiple choice, and one row per answer with a live bar (fill `--accent-soft`, your
  pick `--accent` border), the count, and voter avatars (names on hover). Footer: "n votes · closes …".
  The creator sees Close. When closed, the leader is marked and voting stops.

### 6.4 Code blocks

- Written with fenced ```` ```lang ````. Card: radius 8px, `--surface` fill, `--hairline-2` border. Header row:
  language label (uppercase 10px), line count and Copy ("Copied" plus a toast).
- Line numbers in `--fg-5`, the body in 12.5px mono with horizontal scroll and no wrapping.
- Highlighting for yaml, json, js/ts and bash: keys `--accent-2`, strings `#9EF5AC`, numbers and booleans
  `--warn`, comments `--fg-4` italic, punctuation `--fg-3`. **YAML keys are matched with optional leading
  indent and an optional `- ` list prefix**; an earlier version missed indented keys.

### 6.5 Replies and reactions

- Sent reply: above the message, a 2px connector curve from the avatar column, a 16px mini avatar, the
  author in bold and a one-line excerpt in `--fg-3`. Click it to scroll to the original, which flashes
  `--accent-soft` for 1.4s.
- Reactions: 22px chips, radius 5px. Your own are `--accent-soft` with an `--accent` border. Hover shows a
  tooltip ("Ada, Björn and you reacted with 🛡️"). Click toggles.

### 6.6 Music (now playing)

- **Card in the channel** where /play ran: a 48px art placeholder, the title, "YouTube · requested by Ada ·
  harbour", a progress bar with times, play/pause, skip and stop, and a Queue toggle that expands the upcoming
  list. **Compact player**: one row inside the voice dock and at the top of the voice stage. All three are
  views of the same player state.
- Controls only work if you're in the same voice channel and hold Control music. Otherwise a toast explains:
  "Join harbour to control playback".

### 6.7 Reminders

- Bell in the top bar (Activity tab on phone). Popover with Due and Upcoming tabs. Each row shows the text,
  when ("Today, 20:00"), where ("#general"), and Done or Snooze. Due items have a `--warn` dot.

### 6.8 Attachments

- Tray above the composer input with 112px image previews or 200px file cards. Each shows upload progress
  (a 2px `--accent` bar), name and size, and remove ×. Files over 25 MB are rejected with the toast "Files
  over 25 MB can't be sent from a browser". Up to 10 files.
- GIFs picked in the GIF picker **go into the tray** (with a GIF tag). They are sent when you press Send,
  never on pick.
- Dropping files anywhere on the conversation shows `DropOverlay`.

## 7. Component mapping

### 7.1 Existing components to restyle (all in `packages/ui/src`)

Shell: `AppShell`, `CommunityRail`, `ChannelList`, `ChannelRow`, `VoiceChannelRow`, `UserPanel`,
`VoiceDock`, `ChannelHeader`, `StatusBar` (becomes the top bar), `TabBar`, `PhoneHeader`.
Feed: `MessageLog`, `MessageRow`, `MessageToolbar`, `ReactionBar`, `ReplyChip`, `TypingIndicator`,
`AttachmentCard`, `AttachmentTray`, `DropOverlay`, `Composer`, `MessageEditor`, `MentionList`, `EmojiGrid`,
`GifPicker`.
Voice: `CallStage`, `CallTile`, `CallBar`, `CallControls`, `AudioMeter`, `RingBanner`, `MediaPlayer`.
Overlays: `Dialog`, `ConfirmDialog`, `ActionMenu`, `Tooltip`, `ProfileCard`, `SearchPalette`, `ThreadPane`,
`PinsDialog`, `Lightbox`.
Settings: `SettingsFrame`, `SettingsNav`, `SettingsRow`, `SettingsFooter`, `VoiceSettings`,
`PermissionGrid`, `ColorSwatches`, `InviteLink`, `ManageList`, `DeviceRow`, `FactorRow`, `KeyboardTable`,
`ChannelSettings`.
Primitives: `Button`, `IconButton`, `Toggle`, `Segmented`, `Select`, `TextField`, `Tag`, `Pill`, `KeyHint`,
`Avatar` (add a `shape: 'circle' | 'bot'` prop), `StateDot`, `Banner`, `EmptyState`, `Icon`.
Auth: `OnboardingFrame`, `RecoveryKey`, `RecoveryKeyField`, `ChoiceCards`, `CodeList`, `QrCode`.

### 7.2 New components

| component | purpose | built from |
|---|---|---|
| `TopBar` | 44px bar: brand, search trigger, voice chip, bell, settings | replaces `StatusBar` on desktop |
| `CommandPicker` | `/` list and inline field chips | `Composer`, `MentionList` patterns |
| `CommandField` | one chip: label, value, suggestions, error | `TextField` |
| `BotResult` | private result with status chip, confirm step, dismiss | `MessageRow` |
| `PollCard` | poll with live bars and voters | — |
| `NowPlaying` | card, compact and stage variants of one player | `MediaPlayer` |
| `RemindersPopover` | Due / Upcoming list | `Dialog` (non-modal) |
| `CodeBlock` | fenced code with highlighting, copy and line numbers | — (keep the tokenizer tiny; see 6.4) |
| `ReactionTooltip` | who reacted | `Tooltip` |
| `SharePicker` | source, resolution, fps, estimate, audio | `ActionMenu` shell |
| `SensitivityMeter` | 50-segment meter with draggable threshold | `AudioMeter` |
| `StageFilmstrip` | focus view strip | `CallTile` |
| `PersonContextMenu` | user volume, mute for me, move, server mute, disconnect | `ActionMenu` |

## 8. Storybook stories to write

One story per state. Story names follow the existing style (`Primitives/ChannelRow`, `Shell/…`).

- **ChannelRow**: Text, Active, Unread, Mention, Collapsed-but-unread, VoiceJoined, Afk.
- **VoiceChannelRow**: Empty, WithPeople, Speaking (ring only), MutedDeafened, CameraAndLive, WithGlenn,
  DragTarget.
- **MessageRow**: First, FollowUp, Hover (forced), MentionOfYou, Reply, ReplyFlash, Reactions (yours and
  others'), Thread, CodeBlockYaml, CodeBlockJson, Attachments, GifAttachment.
- **Composer**: Empty, Focused, Typing (someone else), Reply, AttachmentsUploading, AttachmentTooLarge,
  CommandPicking, CommandFields, CommandFieldError, Phone.
- **BotResult**: Pending, Success, Failed, ConfirmKick, NotConfirmed.
- **PollCard**: Open, Voted, MultiChoice, Closed, Creator.
- **NowPlaying**: Card, CardQueueOpen, Compact, Stage, Idle, NoPermission.
- **CallStage**: Grid2, Grid3, Grid5, SpeakingTile, FocusShare, FocusPerson, NotJoinedPreview, Phone.
- **CallControls**: Default, Muted, Deafened, CameraOn, SharePickerOpen.
- **SensitivityMeter**: Idle, TestingHeard, TestingTooQuiet, AutoOn (line locked), ManualDrag.
- **VoiceSettings**: VoiceActivity, PushToTalk, PushToTalkListening.
- **PermissionGrid**: RaidLead (DJ on), Everyone.
- **SearchPalette**: Empty, Query, MessagesResults, NoResults, Phone.
- **ProfileCard**: Person, Owner, Bot, You, PhoneSheet.
- **TopBar**: Default, InVoice, RemindersDue.
- **RemindersPopover**: Due, Upcoming, Empty.
- **AppShell**: Chat, ChatMembersOpen, ChatThreadOpen, Voice, Dm, Settings/Voice, Settings/Roles.
- **PhoneShell**: List, Conversation, Activity, You, ThreadSheet.

Use args for `muted`, `deafened`, `speaking`, `status` and `unread`. For speaking, story with a fixed
speaker. Don't use timers in stories: screenshot tests must be deterministic. The prototype rotates the
speaker every 1.3s only for demo purposes.

`stories/` in this bundle has two worked examples (`BotResult.stories.tsx`, `SensitivityMeter.stories.tsx`)
written against the component APIs proposed in section 9.

## 9. State and props (proposed APIs)

```ts
type BotStatus = 'pending' | 'success' | 'failed' | 'cancelled';
interface BotResultProps { status: BotStatus; title?: string; body: React.ReactNode;
  confirm?: { label: string; danger?: boolean; onConfirm(): void; onCancel(): void };
  onDismiss(): void; }

interface CommandFieldSpec { name: string; kind: 'member' | 'duration' | 'time' | 'text' | 'media';
  required?: boolean; placeholder?: string; }
interface CommandSpec { name: string; description: string; permission?: string; capability: 'music' | 'polls' | 'reminders' | 'moderation';
  fields: CommandFieldSpec[]; }
interface CommandPickerProps { commands: CommandSpec[]; enabled: Record<CommandSpec['capability'], boolean>;
  onRun(name: string, values: Record<string, string>): void; onCancel(draft: string): void; }

interface SensitivityMeterProps { levelDb: number /* -100..0 */; thresholdDb: number; auto: boolean;
  testing: boolean; onThresholdChange(db: number): void; }

interface NowPlayingProps { variant: 'card' | 'compact' | 'stage'; track?: { title: string; by: string; durationS: number };
  positionS: number; playing: boolean; room?: string; canControl: boolean; queue: Array<{ title: string; by: string }>;
  onToggle(): void; onSkip(): void; onStop(): void; }

interface CallStageProps { tiles: Tile[]; focusId: string | 'grid'; onFocus(id: string | 'grid'): void; /* … */ }
```

App-level state touched by Signal (in `packages/web` or `client-core`, not in `ui`): the open aside
(`'members' | 'thread' | null`), palette open and query, the settings page, the stage focus id, reply
target, attachment tray, command draft, the music player (one source of truth that all three NowPlaying
variants read), reminders, and the sensitivity settings (`auto`, `thresholdDb`, `mode`, `pttKey`,
`pttReleaseMs`, `noise`, `echo`, `agc`).

## 10. Design tokens

### 10.1 Colour (Signal, dark)

| token (`ColorTokens` key) | value | use |
|---|---|---|
| `bg` | `#08090B` | page, main pane |
| `bg2` | `#0B0D10` | rail, sidebar, top bar, call bar, asides |
| `bg3` | `#0D1014` | message hover, inset areas |
| `surface` | `#0D1014` | feed cards, composer box |
| `surface2` | `#0F1216` | popovers, menus, dialogs, inputs |
| `surfaceHi` | `#171B22` | selected row, active palette result |
| `hairline` | `#1A1D23` | dividers |
| `hairline2` | `#232831` | card and decorative control borders |
| `edge` | `#5C636E` | **interactive boundaries that need 3:1** (input borders, toggle-off track outline, focusable outlines). See 10.4 |
| `fg` | `#E4E8EE` | primary text |
| `fgBody` *(new)* | `#D5DAE1` | message body |
| `fgStrong` *(new)* | `#B9BFC8` | strong secondary text |
| `fg2` | `#8C939E` | secondary text, idle icons |
| `fg3` | `#767D89` | muted labels and meta |
| `fg4` | `#5C636E` | decorative only: idle glyphs, gutters, never meaningful text |
| `fgLink` | `#5AA8E6` | links |
| `accent` | `#5AA8E6` | selection, primary buttons, focus ring |
| `accent2` | `#8CC4F0` | accent text on tints, code keys |
| `accentInk` | `#03121E` | text on accent |
| `accentSoft` | `rgba(90,168,230,0.12)` | pressed toggles, own reactions |
| `brand` *(new)* | `#005799` | D-tile, caret, Glenn avatar only |
| `ok` | `#7CFF8E` | live, speaking, online, success |
| `danger` | `#FF6E6E` | leave, destructive, failed, mute/deafen on |
| `warn` | `#FFD16A` | idle, due, owner crown |
| `mention` | `#FFD16A` | mention chip |
| `mentionInk` | `#1A1300` | text on mention |
| `linkUnderline` | `0` | |

In the brief, `--fg-2 … --fg-5` were numbered differently. **This table, keyed to the real `ColorTokens`
names, is the one to implement.**

### 10.2 Structural

```ts
radius: { sm: '4px', md: '6px', lg: '8px', xl: '10px', xxl: '12px', pill: '999px', avatar: '999px', bot: '6px' }
shadow: { s1: '0 8px 20px rgba(0,0,0,0.4)',  s2: '0 20px 50px rgba(0,0,0,0.55)', s3: '0 30px 80px rgba(0,0,0,0.65)' }
motion: { press: '120ms', fast: '150ms', normal: '220ms', slow: '300ms', enter: '380ms', badge: '450ms',
          easeOut: 'cubic-bezier(0.16, 1, 0.3, 1)', toast: '220ms', pulse: '2s', caret: '1s', flash: '1.4s', meter: '120ms' }
layout: { railW: '56px', railTile: '38px', sidebarW: '244px', membersW: '232px', threadW: '340px',
          topbarH: '2.75rem' /*44*/, channelHeaderH: '2.75rem', barH: '2.75rem', panelH: '4rem' /*64, user panel and call bar*/,
          rowH: '1.75rem' /*28; 2.75rem on phone*/, rowHSub: '1.625rem' /*26*/, rowH2: '2.75rem' /*44*/, listGap: '0.0625rem' }
```

`text`, `weight`, `labelTracking`, `font` and `densities` stay as they are. Signal was designed at the
`regular` density. Its micro labels use 0.12em tracking, against 0.08em today, so add
`labelTrackingWide: '0.12em'` and use it for uppercase micro labels.

### 10.3 Motion keyframes (as used in the prototype)

| name | from → to | use |
|---|---|---|
| `sUp` | opacity 0, `translateY(8px)` → none | messages, rows, tiles |
| `sIn` | opacity 0, `translateX(-6px)` → none | sidebar rows |
| `sModal` | opacity 0, `translateY(-8px) scale(.985)` → none | popovers, dialogs, palette, settings |
| `sSheetUp` | `translateY(40px)`, opacity 0 → none | phone sheets |
| `sPop` | scale .3 → 1.2 → 1 | badges |
| `sToast` | opacity 0, `translateY(10px) scale(.96)` → none | toasts |
| `sBar` | `scaleY(.2)` ↔ `scaleY(1)` | live meters |
| `sSweep` | `translateX(-100%)` → `translateX(100%)` | typing line |
| `sCaret` | opacity steps 1/0 | brand caret |
| `sRing` | scale 1 → 1.5, opacity .6 → 0 | ringing call avatar |
| `sSpin` | rotate 360° | pending loader |

All of them use `var(--ease-out)` unless they're linear (sweep, spin). Reduced motion zeroes every duration
token. The one exception is `SensitivityMeter`, whose live level carries information.

### 10.4 Contrast: required pairs to add to the tests

Add these to `TEXT_PAIRS` (≥ 4.5:1): `fg2/surfaceHi`, `fg3/bg`, `fg3/bg2`, `fg3/surface`, `fg3/surface2`,
`fgBody/bg`, `fgStrong/surface2`, `accentInk/accent`, `accentInk/accent2`, `fgLink/bg`, `mentionInk/mention`,
`ok/bg2`.
Add these to `UI_PAIRS` (≥ 3:1): `accent/bg`, `accent/bg2`, `edge/bg`, `edge/surface2`, `danger/surface2`,
`ok/bg2`.

Approximate ratios: `fg3 #767D89` reaches about 4.5:1 on `surface2 #0F1216`, the tightest pair, but **only
about 4.35:1 on `surfaceHi #171B22`. Don't put `fg3` text on selected rows**; use `fg2` there (about 5.6:1).
`accentInk` on `accent` is about 7.4:1.

**Deviation from the prototype**: the prototype draws input and toggle-off borders in `#252A33` / `#2A303A`.
Those are about 1.5:1, which fails 1.4.11. Use `edge #5C636E` (about 3.3:1 on `bg`, about 3.1:1 on
`surface2`) for the border of any control whose border is its only visible boundary: text fields, the
composer at rest, segmented control outlines, the toggle track when off. Decorative card borders can stay
`hairline2`.

### 10.5 Other themes

Only the dark Signal theme was designed. Until light and high-contrast are redesigned:

- **light**: set `accent`/`fgLink` to `#005799` (Göteborgsblå reads well on light grounds) and
  `accentInk` to `#FFFFFF`, and keep green only for `ok`. Run the contrast tests.
- **high-contrast**: keep as is, but move `accent` from green to a light blue, for example `#9CCBFF`, so
  "you" and "live" stay distinct.
- `mesh`: keep until Signal ships, then remove.

## 11. Copy and sample data

Use the prototype's sample data in stories:

- Servers: **Midgard Crew** (`#general`, voice `general`, raids/`#loot`, boats/`harbour`, AFK) and
  **Valhalla Kitchen** (`#general`, `#recipes`, voice `stove`).
- People: Ada (owner, raid lead, `hsl(298 45% 42%)`), Björn (`hsl(211 45% 42%)`), Mira (you,
  `hsl(234 45% 42%)`), Sven, Liv, and Glenn (bot, `#005799`).
- Status strings, failure copy and settings descriptions are final. Copy them from the prototype verbatim;
  tone rules are in `brief-signal.md`.

## 12. Assets

- Icons: Lucide 1.57.0, already vendored in `packages/ui/src/Icon/icons.ts`. The prototype's
  `dilla-icons.js` copies that set, with a few extras loaded from `lucide-static` (smile, reply,
  paperclip, bell, inbox, copy, download, loader, moon, log-out, monitor-smartphone, keyboard, server,
  link, shield, square-pen, trash-2, chevron-up, fingerprint, smartphone, key, square, arrow-right).
  **Add those extras to `icons.ts`.** Don't load them from a CDN.
- Fonts: `@fontsource-variable/jetbrains-mono` and `@fontsource/dm-serif-display`, already dependencies.
- No raster images. Album art and video are placeholders.

## 13. Not covered yet

These were scoped but not finished in the prototype. Keep today's behaviour, restyle them with Signal tokens,
and design them in Storybook:

- Server settings → **Channels** (reorder, topic, slow mode, private) and **Bans** (list, lift).
  `ChannelSettings` and `ManageList` already exist.
- **DM calls**: ringing, a compact two-tile call above the DM, and the incoming-call banner. `RingBanner`
  already exists; give it the Signal surface (`surface2`, live border, `sRing` avatar).
- **Phone thread** as a bottom sheet (`ThreadPane` in a sheet, radius 16px 16px 0 0, `sSheetUp`).

## 14. Files in this bundle

| file | what |
|---|---|
| `README.md` | this document |
| `brief-signal.md` | the design brief (rules, rationale, tone) |
| `tokens/signal-theme.ts` | drop-in values for `tokens.ts` |
| `storybook/preview.tsx` | replacement `.storybook/preview.tsx` |
| `stories/BotResult.stories.tsx` | worked example story |
| `stories/SensitivityMeter.stories.tsx` | worked example story |
| `prototype/Dilla Signal Prototype.dc.html` | the interactive reference (open in a browser; needs the two files next to it) |
| `prototype/dilla-icons.js`, `prototype/support.js` | runtime for the prototype |

Open the prototype with props `startAt=app` to skip sign-in, or `device=phone` to see the phone layout. ⌘K,
⌘⇧M and ⌘⇧D work as they will in the app.
