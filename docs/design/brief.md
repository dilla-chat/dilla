# dilla design brief

## Purpose

dilla's interface is the Signal design, with encryption made silent. Signal replaced Mesh in the web client
on 2026-10-27; Mesh is no longer a theme. This brief is the contract for every UI card: what the chrome
looks like, what words it uses, what it never shows, and the accessibility floor. Where this brief and a
component disagree, the brief wins; where this brief and the Signal reference build
(`docs/design/signal/`, `Dilla Signal Prototype.dc.html`) disagree, this brief wins, because it records
the decisions taken after the reference was made. The original handoff brief is kept as
`docs/design/brief-signal.md`.

### What changed from Mesh

| Mesh rule | Signal | Why |
|---|---|---|
| Radii 0-3px | 4-12px by size (see Radii) | Square chrome read as unfinished, not deliberate |
| No blur, ring-only shadows | Soft drop shadows on floating layers only | Popovers and menus need to separate from the page |
| Green accent for everything | Blue `--accent` (`#5AA8E6`) for selection and actions; green (`--ok`) only for live, presence and speaking | Green now always means "someone is there" |
| Seven permitted animations | A motion system (see Motion) | The client should feel responsive |
| Square avatars | Circles for people, rounded squares for bots | A bot never reads as a person |
| Status-dense chrome | One 44px top bar: brand, centred search, voice chip, reminders, settings | The breadcrumb repeated the channel header |
| DJ is a bot role approval in Server settings, Music | **Control music (DJ)** is a role permission under Voice | One place to say who can do what |

Kept from Mesh: mono everywhere (JetBrains Mono), DM Serif Display italic for the server name only,
sentence case in prose, uppercase mono micro labels, the vocabulary table, silent encryption, the
accessibility model, rem sizing.

## Audience and tone

A gaming friend group that lives in voice all evening, and the self-hoster who runs their server.
The tone is a tool's: plain verbs, sentence case in prose, uppercase mono for chrome labels, no
marketing, no reassurance. Errors say what happened and what to do. Empty states invite an action.
Nothing on screen exists to make the user feel secure; the product is secure by default and says
nothing about it in the chrome.

## Vocabulary

| concept | UI word | never |
|---|---|---|
| a community on an instance | **server** | team, guild |
| a text or voice room | **channel** | kanal |
| private conversations | **DMs** | PMs, private messages |
| the voice room you are in | **voice** | call, room (call is used only for DM calls) |
| the person who runs the instance | **host** | operator (in copy) |
| a bot user | **bot** | app, integration |

Code, API and protocol keep *community* and *channel*; only copy changes.

## Design system source

`docs/design/signal/README.md` (the handoff), its `tokens/signal-theme.ts` and the reference build
`Dilla Signal Prototype.dc.html`. The tokens that ship are in `packages/design-tokens`; the names below
are the real ones. The Mesh handoff in `docs/design/reference/mesh-handoff/` is kept as history only.

## Tokens

Colours, `signal` theme (the default and the only dark theme):

| token | value | role |
|---|---|---|
| `--bg` | `#08090B` | page, main pane |
| `--bg-2` | `#0B0D10` | rail, sidebar, top bar, call bar, asides |
| `--bg-3` | `#0D1014` | tertiary surface, message hover |
| `--surface` | `#0D1014` | cards in the feed (code, poll, now-playing), composer box |
| `--surface-2` | `#0F1216` | popovers, menus, dialogs, inputs |
| `--surface-hi` | `#171B22` | selected row, active palette result |
| `--hover` | `#14171C` | row hover |
| `--hairline` | `#1A1D23` | dividers |
| `--hairline-2` | `#232831` | borders of floating layers and cards (decorative; never the only boundary of a control) |
| `--edge` | `#5C636E` | borders of interactive controls (inputs, toggle-off track, segmented outline, composer at rest, keycaps): 3:1 or better on `--bg` and `--surface-2`, so a control is identifiable without hover (WCAG 1.4.11) |
| `--fg` | `#E4E8EE` | primary text |
| `--fg-body` | `#D5DAE1` | message body |
| `--fg-strong` | `#B9BFC8` | strong secondary text |
| `--fg-2` | `#8C939E` | secondary text, idle icons (not on `--surface-hi`) |
| `--fg-3` | `#767D89` | muted labels and meta (4.5:1 on `--bg`, `--bg-2` and `--surface-2`) |
| `--fg-4` | `#5C636E` | decorative only: idle glyphs, gutters; never text that carries meaning |
| `--fg-link` | `#5AA8E6` | links |
| `--accent` | `#5AA8E6` | selection bar, primary buttons, focus ring, links, command chip |
| `--accent-2` | `#8CC4F0` | accent text on tinted grounds, code keys, bot tag, accent hover |
| `--accent-ink` | `#03121E` | text on accent |
| `--accent-soft` | `rgba(90,168,230,0.12)` | pressed toggles, own reactions, selected tint |
| `--brand` | `#005799` | Goteborgsbla: D-tile, caret, Glenn's avatar. Never text, never a control |
| `--ok` | `#7CFF8E` | live: voice connected, speaking, online, success |
| `--ok-text` | `#9EF5AC` | live text on dark grounds |
| `--ok-soft`, `--ok-soft-border` | `rgba(124,255,142,0.04)`, `rgba(124,255,142,0.22)` | voice dock ground and border |
| `--ok-tint`, `--ok-border` | `rgba(124,255,142,0.08)`, `rgba(124,255,142,0.25)` | voice chip ground and border |
| `--danger` | `#FF6E6E` | leave, mute/deafen on, destructive, failed, LIVE tag |
| `--danger-soft`, `--danger-border` | `rgba(255,110,110,0.1)`, `rgba(255,110,110,0.45)` | toggled-on mute and deafen |
| `--danger-ink` | `#1A0606` | text on danger (leave button) |
| `--warn` | `#FFD16A` | idle, due reminder, owner crown |
| `--warn-soft` | `rgba(255,209,106,0.05)` | mention row ground |
| `--mention` | `#FFD16A` | mention pill background |
| `--mention-ink` | `#1A1300` | text on mention and warn |
| `--scrim` | `rgba(3,4,6,0.6)` | behind dialogs |

Role colours are user data and appear only on names, group headings and role chips.

Rule: blue means *you* (your selection, your action, your reaction). Green means *live*. Yellow means
*attention*. Red means *stop or failed*. Never two of them on one control. Every value is a token; a
component never carries a hex, rgb or px colour literal.

Type: `--font-mono` = "JetBrains Mono Variable", ui-monospace, monospace (everything);
`--font-serif-italic` = "DM Serif Display", serif, italic (only the server name in the sidebar
header). The type scale, the control and bar heights, the row padding and the avatar sizes are rem,
never px, so the browser's text-size setting and 200 % zoom scale the text and the boxes around it
together; the pane widths (rail, sidebar, member list, thread panel) stay px, because they are
furniture the user drags rather than boxes around text. The px equivalent at the 16px default root
is given in parentheses: `--text-micro`
0.625rem (10px), `--text-xs` 0.6875rem (11px), `--text-sm` 0.78125rem (12.5px), `--text-base`
0.84375rem (13.5px), `--text-md` 0.875rem (14px), `--text-lg` 1.0625rem (17px), `--text-xl`
1.375rem (22px). Weights: `--weight-display` 700,
`--weight-body` 420, `--weight-label` 600. Labels in chrome: uppercase, `letter-spacing: 0.08em`
(`--label-tracking`); uppercase micro labels use `--label-tracking-wide` 0.12em. Phone inputs are 16px
text so iOS does not zoom.

Radii: `--r-sm` 4px (keycaps, tags), `--r-md` 6px (rows, chips, small buttons), `--r-lg` 8px (buttons,
inputs, composer), `--r-xl` 10px (cards, tiles), `--r-xxl` 12px (dialogs, popovers), `--r-seg` 7px
(segmented control), `--r-bot` 6px (bot avatars), `--r-pill` 999px (reactions, presence dots, toggles,
counters), `--r-avatar` 999px (people are circles).

Elevation: flat panes are separated by hairlines only. Floating layers (menus, popovers, dialogs,
toasts, the message toolbar) use `--shadow-2` `0 20px 50px rgba(0,0,0,0.55)` plus a `--hairline-2`
border; `--shadow-1` `0 8px 20px rgba(0,0,0,0.4)` is for small layers and `--shadow-3`
`0 30px 80px rgba(0,0,0,0.65)` for dialogs over a scrim. No backdrop blur.

Opacity: `--opacity-muted` 0.8, applied to de-emphasised rows (e.g. a muted channel/member row) on
top of their existing colour, never as the only signal of the muted state. 0.8 is the smallest round
value that keeps secondary text on `--bg-2` at or above 4.5:1 in all three themes.

Density (`data-density`), rem with the px equivalent in parentheses: compact row padding 0.25rem
1rem (4px 16px), gap 0, group gap 0.5rem (8px), avatar 1.75rem (28px); regular 0.375rem 1.125rem
(6px 18px), 0.125rem (2px), 0.875rem (14px), 2rem (32px); cozy 0.625rem 1.25rem (10px 20px),
0.25rem (4px), 1.375rem (22px), 2.25rem (36px). Line height 1.4 / 1.5 / 1.55, unitless, so it
already scales with its own font size.

Layout dimensions. Pane widths in px, because the user drags them and a rail that grew with the
text size would eat the message pane: `--rail-w` 56px, `--sidebar-w` 244px (resizable 200-360),
`--members-w` 232px (180-340), `--thread-w` 340px. Bar and header heights in rem, because each one
is a box around text: `--topbar-h` 2.75rem (44px), `--bottombar-h` 1.625rem (26px),
`--channel-header-h` 2.75rem (44px), `--bar-h` 2.75rem (44px, every pane's top bar), `--panel-h` 4rem
(64px, the user panel and the voice call bar, so their top edges line up), `--row-h` 1.75rem (28px,
2.75rem on a phone), `--row-h-sub` 1.625rem (26px, 2.25rem on a phone).

Focus: `--focus-ring` = `2px solid var(--accent)`, applied as `outline: var(--focus-ring)` with
`outline-offset: 2px` on every interactive element via `:focus-visible`. Inputs add a 3px
`--accent-soft` ring as well. The ring is an outline and never a `box-shadow`: `box-shadow` is
reserved for component state (the active channel row's inset bar, a pressed toggle's underline),
which must not be able to swallow the ring, and an outline keeps the Windows forced-colors indicator.

Sound tokens (names; files come with the sound card): `join`, `leave`, `mention`, `mute`,
`unmute`, `deafen`, `undeafen`, `ping`, `error`. Short, dry, mono-era clicks and one ping; no
Discord sounds, no chimes.

## Layout

Desktop: a 44px top bar across the window; under it a 56px server rail, a 244px sidebar, the main
pane, and an optional aside (232px members, 340px thread). The user panel and the voice call bar are
both 64px, so their top edges line up. Below 770px of shell width the phone layout shows one pane at
a time with a bottom tab bar (Servers / DMs / Activity / You); every target is at least 44px, inputs
are 16px text, and there is no top bar.

- **Top bar**: brand left, search centred in a 420px box, then a right group: voice chip (when
  connected), reminders bell, settings. Command palette on ⌘K, search on `/`.
- **Rail**: 38px tiles with initials, rounded 10px, the accent selection bar when active, a dashed
  accent "+" to add a server.
- **Sidebar**: serif italic server name with its menu, an online / in-voice line, collapsible
  categories with unread (accent) and mention (warn) pills, voice rows with the people in them. Each
  person row shows camera and LIVE glyphs. Speaking shows as a green ring on the avatar; there is no
  meter in the list. A voice dock when connected, then the user panel.
- **Channel header**: icon, name, topic; Threads and Members on the right.
- **Feed**: welcome block, day dividers, grouped messages, a time gutter on hover. Pinned to the
  bottom (column-reverse scroller) unless you scroll up. Message hover uses `--bg-3`.
- **Composer**: multi-line; attach, emoji, GIF and code buttons; Enter sends, Shift+Enter adds a line.
- **Member list** (Admin / Online / Offline) swaps for the 340px thread panel.

Settings fills the viewport as a native modal workspace: a 15rem grouped sidebar and independently scrolling content, centered together within 80rem. Ordinary forms use at most 48rem, lists 64rem; padding is 2rem on spacious screens and 1rem on constrained screens. There is no floating perimeter, backdrop dismissal, rounded outer border or modal shadow. The persistent Close control shows an Escape hint.

Settings content is left aligned with the pane padding. Simple preference rows use balanced,
flexible columns that stack when their contents no longer fit. Compound forms (provider keys,
Spotify configuration and welcome templates) sit below their labels and descriptions; a whole
form must never be squeezed into a compact control column. Bot identity, name, lifecycle and
capability controls have separate visual groups. On phones, Back and the section title occupy
the first header row; ongoing call controls occupy a separate row so the title stays readable.

The optional bottom bar is `white-space: nowrap; overflow: hidden`, so at 200 % zoom, at a raised
browser text size, or in a narrow window it clips chunks off the right edge instead of wrapping
and eating vertical space. It clips horizontally, never vertically: the bar heights are rem, so
the text inside keeps its room however large the reader sets it. The horizontal clipping is allowed
because the bar is optional and carries no information that is not also available elsewhere (the
channel header, the voice dock, Settings → Diagnostics).

## Removed from the chrome

Encryption is the default and says nothing. These Mesh elements are not built:

- the shield badge in channel headers
- the SRTP badge in voice headers
- the composer footer line about encryption
- the e2e and db chunks in the bottom bar
- the pills on empty channels and DMs
- the lock in notification teasers
- the drop-overlay copy about encryption

Encryption details, safety numbers, device verification and the recovery-key ceremony live in
Settings → Privacy and Settings → Devices, the onboarding steps, and their own modals.

## Trust facts that stay visible

- A small muted `web` tag after the name of a member whose message or presence comes from a
  browser session (member list, message header, profile popover).
- On a managed bot present in a voice channel: the bot tag plus "playback only"; managed bots cannot subscribe to call media.
- The existing lock glyph on private voice channels (a permission fact, not encryption).

## The readable-channel glyph

A channel anyone can join by public invite is readable by the server. It shows a small muted
dotted-circle glyph `◌` after its name in the channel list, the same weight as the private-channel
lock, with the accessible name "Readable by this server". No text, no colour, nothing else in the
chrome; the mode is set and explained in channel settings.

## Brand mark

Top bar: the mono D-tile (1.125rem square, 18×18 at the default root, `--brand` background, white
"D", rounded 4px) followed by "DILLA" in 11px uppercase mono at 700 and a 6×12px `--brand` caret that
blinks at 1 Hz (static under reduced motion). `--brand` is never text and never a control. The SVGs in
`docs/design/reference/mesh-handoff/branding/` (teal D, amber wordmark) are used only for the favicon,
the first-run splash and the README, never in the app chrome.

## Sound

Every sound is under 300 ms, dry, and mono-era: a click for mute/unmute/deafen, a two-tone tick
for join/leave, one short ping for mentions, a low buzz for errors. Sounds are user-toggleable per
event in Settings → Notifications and are off in the `browser` tier until the user turns them on.
Amended 2026-10-13 (founder, web-3 round 2): in the web client the join, leave and ring sounds are on by default and each can be turned off in Settings → Notifications → sounds; there is no sound for one's own join, leave, mute or deafen, no ringback tone, and no ring sound or notification for a muted DM.

## Motion

One curve: `--ease-out` = `cubic-bezier(0.16, 1, 0.3, 1)`.

| kind | duration | what |
|---|---|---|
| state | 140-150ms (`--duration-hover` 140, `--duration-fast` 150); the toggle track 200ms (`--duration-track`) | hover, colour, border, opacity |
| press | 120ms (`--duration-press`) | `scale(.92-.96)` on buttons while pressed |
| enter | 220ms (`--duration-normal`); 380ms (`--duration-enter`) for a message's rise | rows and messages rise 8px and fade in; lists stagger 40-60ms per item |
| popover / dialog | 200-260ms (`--duration-popover` 200, `--duration-modal` 260) | from `translateY(-8px) scale(.985)` |
| layout | 220-340ms (`--duration-normal` 220, `--duration-aside` 280, `--duration-dock` 340) | selection bar `scaleY`, aside width, category chevron |
| toggle | 240ms (`--duration-toggle`) | the switch knob |
| badge | 450ms (`--duration-badge`) | pop with overshoot on unread, mention and reminder counts |
| toast | 220ms (`--duration-toast`) | rise and scale in, out after 2.4s |
| splash | 300ms (`--duration-slow`) | the first-run splash fades in |
| drop | 150ms (`--duration-fast`) fade, 180ms (`--duration-spin`) panel | the drag-over overlay while files are held over a conversation |
| flash | 1.4s (`--duration-flash`) | reply-jump highlight in `--accent-soft` |
| live | continuous | speaking meters (520ms bars), typing sweep (1.4s), caret (1s), speaking pulse (2s) |

Under `prefers-reduced-motion: reduce` (and `[data-motion="reduced"]` in Storybook) every duration
token is 0ms and the caret and pulse are static, except the audio level meter, which is data, not
decoration: it is the only thing on screen that says whether a microphone is picking anything up, so
it keeps updating at `--duration-meter` (120ms).

## Themes

`signal` is the default and is what the reference renders show; `mesh` no longer exists, and a stored
`mesh` preference reads as `signal`. `light` and `high-contrast` are derived from the same structure
with their own values (see `packages/design-tokens`): the light theme is not a colour inversion but a
re-pick that keeps the accent meaning (blue = yours, green = live, warn = attention, danger =
destructive) at AA on light surfaces, with the accent and links in `--brand` blue; the high-contrast
theme uses pure black, white text, a light-blue accent, 1px white hairlines and underlined links. The
app follows the OS setting until the user chooses. Until then it also follows `prefers-contrast: more`,
which selects the high-contrast theme even when the OS colour scheme is light; an explicit theme choice
wins over both.

## Accessibility model

- Full keyboard operation: every action in the chrome reachable by Tab and the documented keybinds
  (⌘K palette, `/` search, `?` keys, ⌘1–9 channel jumps, M mute, D deafen, Esc closes).
- Amended 2026-10-13 (founder, web-3 decision 1): mute is Ctrl/⌘+Shift+M and deafen is Ctrl/⌘+Shift+D; there are no single-key shortcuts (WCAG 2.1.4).
- Visible focus everywhere: a 2px accent outline offset by 2px (`--focus-ring`), applied with
  `outline`, never `box-shadow` — `box-shadow` is reserved for component state, so a state cue can
  never swallow the ring. Nothing in the component library sets `outline: none`.
- The message feed is a `log` region with a polite live region for new messages; the virtualised
  list keeps DOM order equal to reading order.
- Presence and speaking are shown with a glyph or text as well as colour.
- 200 % zoom without loss; font sizes and the boxes around them in rem, so a raised browser text
  size grows both together; no `min-width` wider than the viewport.
- Dialogs use the native `<dialog>` element: focus trap, Esc, focus return.
- Ceremonies (recovery key, safety number, pairing) are readable as text, copyable as text where
  allowed, and operable by keyboard alone.
- Every story and component test passes axe with zero serious or critical violations; the W12 pass
  adds NVDA, Orca and VoiceOver.

## Flows to wireframe

One week before each build week, as annotated ASCII screens in `docs/design/flows/`:

| flow | wireframe by | build week |
|---|---|---|
| first launch and invite join | W1 | W2 |
| signup with recovery-key ceremony | W2 | W3 |
| channel list and chat, DMs and group DMs | W2 | W3 |
| voice join, leave, mute, deafen, push-to-talk, speaking | W3 | W4 |
| server settings, roles and overwrites | W4 | W5 |
| desktop chrome, tray, notifications | W5 | W6 |
| screen-share picker and audio options | W6 | W7 |
| new-device pairing and recovery-key entry | W7 | W8 |
| diagnostics page | W8 | W9 |
| bot install and bot tags | W10 | W11 |
| report dialog and "what is revealed" copy | W11 | W12 |

## Attribution

The Mesh design system is the work of the dilla-chat authors (github.com/dilla-chat/dilla-chat,
AGPLv3) and is reused here by the same authors under the same licence family. The reference copy
in `docs/design/reference/mesh-handoff/` is unmodified except for the removal of the `mesh-clean`
working directory.
