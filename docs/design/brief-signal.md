# dilla design brief: Signal

Supersedes `docs/design/brief.md` (Mesh) for the web client. Where this brief and a component disagree, this
brief wins. The reference build is `Dilla Signal Prototype.dc.html`; every value below can be read off it.

## What changed from Mesh, and why

| Mesh rule | Signal | Why |
|---|---|---|
| Radii 0–3px | 4–12px by size (see Radii) | Square chrome read as unfinished, not deliberate |
| No blur, ring-only shadows | Soft drop shadows on floating layers only | Popovers and menus need to separate from the page |
| Green accent for everything | Blue `#5AA8E6` for selection and actions; green only for live, presence and speaking | Green now always means "someone is there" |
| Seven permitted animations | A motion system (see Motion) | The client should feel responsive |
| Square avatars | Circles for people, rounded squares for bots | A bot never reads as a person |
| Status-dense chrome | One 44px top bar: brand, centred search, voice chip, reminders, settings | The breadcrumb repeated the channel header |
| DJ is a bot role approval in Server settings → Music | **Control music (DJ)** is a role permission under Voice | One place to say who can do what |

Kept from Mesh: mono everywhere (JetBrains Mono), DM Serif Display italic for the server name only, sentence
case in prose, uppercase mono micro labels, the vocabulary table, silent encryption, the accessibility model,
rem sizing.

## Audience and tone

Unchanged: friend groups who live in voice, and the host who runs the server. Signal must also hold up in a
small team or company (calm, legible, no novelty). Plain verbs, no reassurance, errors say what to do next.

## Tokens

### Colour (dark, the only theme in this pass)

| token | value | role |
|---|---|---|
| `--bg` | `#08090B` | page, main pane |
| `--bg-2` | `#0B0D10` | rail, sidebar, top bar, call bar, asides |
| `--surface` | `#0D1014` | cards in the feed (code, poll, now-playing), composer box |
| `--surface-2` | `#0F1216` | popovers, menus, dialogs, inputs |
| `--hover` | `#14171C` | row hover |
| `--selected` | `#171B22` | selected row, active palette result |
| `--hairline` | `#1A1D23` | dividers |
| `--hairline-2` | `#232831` | card and control borders |
| `--edge` | `#2A303A` | interactive borders (buttons, keycaps, inputs at rest) |
| `--fg` | `#E4E8EE` | primary text |
| `--fg-body` | `#D5DAE1` | message body |
| `--fg-2` | `#B9BFC8` | strong secondary |
| `--fg-3` | `#8C939E` | secondary text, idle icons |
| `--fg-4` | `#767D89` | muted labels and meta (≥ 4.5:1 on `--bg` and `--bg-2`) |
| `--fg-5` | `#5C636E` | decorative only: idle glyphs, gutters; never text that carries meaning |
| `--accent` | `#5AA8E6` | selection bar, primary buttons, focus ring, links, command chip |
| `--accent-2` | `#8CC4F0` | accent text on tinted grounds, code keys, bot tag |
| `--accent-ink` | `#03121E` | text on accent |
| `--accent-soft` | `rgba(90,168,230,.12)` | pressed toggles, own reactions, selected tint |
| `--brand` | `#005799` | Göteborgsblå: D-tile, caret, Glenn's avatar. Never text, never a control |
| `--live` | `#7CFF8E` | voice connected, speaking, online, success |
| `--danger` | `#FF6E6E` | leave, mute/deafen on, destructive, failed, LIVE tag |
| `--warn` | `#FFD16A` | mention, idle, due reminder, owner crown |
| `--mention-ink` | `#1A1300` | text on warn |

Role colours are user data (for example raid lead `#E9B6EC`, DJ `#FFD16A`) and appear only on names, group
headings and role chips.

Rule: blue means *you* (your selection, your action, your reaction). Green means *live*. Yellow means
*attention*. Red means *stop or failed*. Never two of them on one control.

### Type

`--font-mono` "JetBrains Mono" 400/500/600/700; `--font-display` "DM Serif Display" italic (server name only).
Scale in rem: micro 0.625 (10px, uppercase labels, tracking 0.12em), xs 0.6875 (11px, meta), sm 0.75 (12px),
base 0.8125 (13px, body), md 0.84375 (13.5px, titles in bars), lg 1.25 (20px, page titles), display
1.1875 (19px, the serif server name). Line height 1.55 for message bodies, 1.3 for two-line rows. Phone inputs
16px so iOS doesn't zoom.

### Radii

4px keycaps and tags · 5–6px rows, chips, small buttons · 7–8px buttons, inputs, composer · 10px cards and
tiles · 12px dialogs and popovers · 999px avatars, presence dots, toggles, counters · 6px bot avatars.

### Elevation

Flat panes are separated by hairlines only. Floating layers (menus, popovers, dialogs, toasts, the message
toolbar) use `0 20px 50px rgba(0,0,0,.55)` plus a `--edge` border. Scrims are `rgba(3,4,6,.6)`. No
backdrop blur.

### Focus

Unchanged from Mesh: `2px solid var(--accent)`, offset 2px, outline not box-shadow. Inputs add a 3px
`--accent-soft` ring as well.

## Motion

One curve: `--ease: cubic-bezier(.16,1,.3,1)`.

| kind | duration | what |
|---|---|---|
| state | 120–160ms | hover, colour, border, opacity |
| press | 120ms | `scale(.92–.96)` on buttons while pressed |
| enter | 200–380ms | rows and messages rise 8px and fade in; lists stagger 40–60ms per item |
| popover / dialog | 160–260ms | from `translateY(-8px) scale(.985)` |
| layout | 220–320ms | selection bar `scaleY`, aside width, category chevron |
| badge | 400–500ms | pop with overshoot on unread, mention and reminder counts |
| toast | 220ms | rise and scale in, out after 2.4s |
| flash | 1.4s | reply-jump highlight in `--accent-soft` |
| live | continuous | speaking meters (520ms bars), typing sweep (1.4s), caret (1s) |

Under `prefers-reduced-motion: reduce` everything above is 0ms, except the audio meter in Settings → Voice,
which carries information.

## Layout

Desktop: 44px top bar across the window; under it a 56px rail, a 244px sidebar, the main pane, and an
optional aside (232px members, 340px thread). The user panel and the voice call bar are both 64px, so their
top edges line up. Below 770px of shell width the phone layout shows one pane at a time with a Home / Activity
/ You tab bar; every target is at least 44px.

- **Top bar**: brand left, search centred in a 420px box, then a right group: voice chip (when connected),
  reminders bell, settings.
- **Sidebar**: serif server name with menu, an online / in-voice line, collapsible categories, voice rows with
  the people in them. Each person row shows camera and LIVE glyphs. Speaking shows as a green ring on the
  avatar; there's no meter in the list.
- **Channel header**: icon, name, topic; Threads and Members on the right.
- **Feed**: welcome block, day dividers, grouped messages, a time gutter on hover. Pinned to the bottom
  (column-reverse scroller) unless you scroll up.
- **Composer**: multi-line; attach, emoji, GIF and code buttons; Enter sends, Shift+Enter adds a line.

## Components added in Signal

- **Command picker** (`/`): filtered list; picking a command turns the composer into named field chips. Each
  field type offers suggestions (member, duration, time with exact preview, YouTube query). Tab moves between
  fields, Enter runs, Esc leaves and keeps the draft. Validation errors show on the field. `//` posts a
  literal slash.
- **Private result**: a Glenn message with a status chip (Working / Done / Failed / Not confirmed), "Only you
  can see this" and Dismiss.
- **Poll card**: named voters per answer, single or multi choice, live bars, Close for the creator.
- **Now-playing card** in the channel; the same player sits in the sidebar voice dock and at the top of the
  voice stage.
- **Reminders popover** under the bell, with Due and Upcoming tabs.
- **Code block**: language label, line count, Copy, line numbers, highlighting for yaml, json, js/ts and bash.
- **Reply**: the target row is tinted while you compose; the sent message shows a connector, mini avatar,
  author and excerpt; clicking it scrolls to the original and flashes it.
- **Reactions**: hover a reaction to see who reacted; your own are tinted blue.
- **Attachments**: tray with upload progress, 25 MB limit per file; images preview; GIFs from the picker go
  into the tray and are sent with Send.
- **Context menu** on people (user volume, mute for me, move to, server mute, disconnect) and drag between
  voice channels.
- **Voice stage**: 16:9 tiles that never stretch to full height. Click any tile to focus it, with a filmstrip
  below and a Grid button to go back. The share picker sets source, resolution and frame rate, shows an upload
  estimate, and has a system-audio toggle.
- **DM calls**: ringing, a compact two-tile call above the DM, and an incoming-call banner.

## Bot: Glenn

Built-in managed bot, rounded-square avatar in `--brand`, BOT tag, "playback only" in voice. Server settings →
Bot switches music, polls, reminders and moderation on and off; a capability that's off removes its commands
from the picker. Server settings → Music covers the YouTube source, default volume and idle timeout. Who can DJ
is the **Control music (DJ)** role permission.

## Settings

Full-viewport workspace: a 240px grouped nav (You / the server), content capped at 720px, Esc to close. Rows:
label and description on the left, the control on the right, stacking on narrow widths. Server sections:
Overview, Channels, Roles, Invites, Bans, Bot, Music.
