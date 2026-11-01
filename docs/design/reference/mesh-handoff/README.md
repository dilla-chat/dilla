# Handoff: Dilla — UI refinement (Mesh direction)

## Overview

This bundle is the design output of a thorough UI refinement pass on **dilla-chat**, an end-to-end encrypted, federated, self-hostable chat app (the existing repo is at `github.com/dilla-chat/dilla-chat`). The mocks lift, modernize, and dramatically extend the existing client UI under a single direction nicknamed **"Mesh"** — brutalist / terminal / mono-typographic, neon-green accent on near-black surfaces, with the federation + crypto chrome made first-class instead of hidden.

The goal of this handoff is for a developer (you, via Claude Code) to **reimplement these designs against the real codebase** at `client/src/` — replacing/refactoring existing components in React + TypeScript + Zustand + Vite — not to copy the HTML directly.

## About the Design Files

The files in this bundle are **design references created as HTML/JSX prototypes**. They render at high fidelity and behave like the real app would, but:

- They use plain React 18 via `<script type="text/babel">` from CDN, not the project's Vite + TS + Zustand toolchain.
- All data is mocked in `data.js` and lives in component state. There is no API client, no WebSocket, no Tauri IPC, no Signal Protocol calls — every backend interaction is faked with `setTimeout` and `dispatchEvent` notifications.
- File and module names are descriptive, not prescriptive — the file split makes sense for a single-page prototype, not for the actual `client/src/` layout (which already has `components/`, `pages/`, `services/`, `stores/`).

Your task is to **recreate this UI inside the existing codebase** following its conventions:

- TypeScript (the prototype is plain JS / JSX)
- The existing CSS variable system in `client/src/styles/theme-default.css` — add new tokens to it, don't replace
- The existing Zustand stores for state
- The existing API client at `client/src/services/api.ts` and WebSocket client at `client/src/services/websocket.ts`
- The existing Signal Protocol bridge at `client/src/services/crypto.ts` (which calls into Rust via Tauri IPC)
- Conventional commits (`feat:`, `fix:`, etc.)

## Fidelity

**High-fidelity (hifi).** Every screen has final colors, type, spacing, density, hover/focus/active states, transitions, and motion. Recreate pixel-perfectly using the existing design-token system — extend `theme-default.css` rather than hardcoding values.

## How to view the design

Open `Dilla Mesh.html` in a browser. The page boots into a brief splash, then loads with `#design` open and a voice call active in `#voice-lounge` (left sidebar voice dock visible). Try:

- Switch kanals via the sidebar
- Press `⌘K` for the command palette — includes "simulate" commands for testing onboarding-failure, peer drops, incoming voice calls, safety-number verification, etc.
- Press `/` for search (scopes to the current kanal by default, toggleable to all)
- Right-click anything (channels, members, messages, server icons, voice cards)
- Click any author name / avatar for a profile popover
- Click the bookmark icon in the header for saved messages, thread icon for the thread index
- The Tweaks panel (bottom-right) toggles density, sidebar widths, federation chrome on/off, accent hue

`Dilla Onboarding.html` is the standalone first-run flow (5 steps: connect → identity → keys → safety → done).

## Screens / Views

The app has **one main layout** with several pane-level states. Reuse the existing `AppLayout` page if it exists.

### Layout shell (CSS Grid)

```
60px  | sidebar (resizable, default 240px) | 1fr   | members (resizable, default 232px)
─────── ─────────────────────────────────── ─────── ───────────────────────────────────
server   channel sidebar                     main    member list  OR  thread panel (380px)
rail
```

Three horizontal regions besides the rail; member list collapses for DMs and for the `People` toggle in the channel header. Replaced by a 380px thread panel when a thread is open. Drag handles between columns at `--sidebar-w` and `--members-w` boundaries.

Optional top status bar (32px) and bottom status bar (26px) wrap the grid — both can be toggled in Tweaks.

### Server rail (60px)

- Vertical stack of 40×40 team icons, 2px radius
- Active team: filled with accent color, white left bar at -10px
- Hover: 1px translate-y
- Drag to reorder (custom HTML5 drag, drop target shows green ring)
- Right-click → team menu (Settings / Invites / Federation / Mark all read / Leave)
- Bottom: `+ Add team` dashed-accent button (opens modal: Create or Join via invite)
- Federated peer indicator: amber dot at top-right of non-federated teams

### Channel sidebar (var width 200–360px)

- Header (14px padding): team name in serif italic + lightning icon, mono node line below ("gbg-1.dilla.local · mesh ok"), settings cog
- Tabs (Kanals / PMs), accent-underlined when active; PMs shows an aggregated unread pill
- Kanals tab content:
  - **Active voice** category — voice channels with participants, expanded inline showing participants with speaking pulse, mic-off badge, screen-share icon
  - **Kanals** category — text channels, with `+` action on hover
    - Drag-reorder text channels
    - Unread count pill (accent), mention pill (amber)
    - Muted channels show bell-slash glyph, dim type
    - Right-click → Mark as read / Mute / Copy link / Settings / Leave
  - **Voice** category — voice channels with 0 participants, lock icon if private
  - Voice dock (when voice connected) — pulsing dot + name + mic/headphone/cam/screen/disconnect buttons. Mute/Deafen turn red when active. Cam/Screen turn green when transmitting.
  - User panel (bottom) — avatar with presence dot, name, status, status-picker emoji button, settings cog
- PMs tab content:
  - `+ New DM` action — opens a filterable member-picker modal
  - DM list with avatars, group icon for group DMs, unread pills
  - Right-click → Mark as read / Mute / Close DM

### Main pane

Three modes based on the active view:

**1. Text channel** (e.g. `#design`)

- Header (48px) — `#name` in serif display, optional `[E2E]` shield badge, topic with left border, action buttons (threads · saved · pinned · members toggle · scoped search), all with hover effects
- Feed — message list with day dividers, system messages italic, message groups (consecutive same-author within 5min collapse avatar/name)
  - Empty state for new kanals: `#`-glyph, "Welcome to #name" title, topic + encryption pills
  - Day-1 `#general` opens with a system event + welcome message from thim (in `data.js`)
- Composer (16px bottom padding) — pill-shaped, focus ring (accent-tinted), attach + textarea + emoji + send (32×32)
  - Drag-and-drop overlay covers the main pane with green dashed border, "Drop to attach" message
  - Upload tray above composer: per-file rows with progress, phase label (reading → encrypting → uploading → sealed)
  - Mention popup: `@` triggers member picker (filter by name prefix), tab/enter completes
  - Slash popup: `/` at start triggers command picker; `/me`, `/code <lang>`, `/poll q | a | b`, `/giphy <q>`, `/shrug`, `/remind`, `/topic`, `/invite`, `/dm`, `/help`
  - Typing indicator below composer with animated dots
  - Reply chip above composer when replying to a message — × to cancel
- Message row — avatar / head (author, time, admin badge, read-receipts ✓✓) / body / reactions / thread preview / hover toolbar
  - Hover toolbar (top-right): React · Reply · Open thread · Edit (own) · Delete (own)
  - Right-click full menu: Add reaction · Reply in thread · Quote reply · Forward to… · Pin to channel · Mark unread from here · Copy link to message · Save message · Edit · Delete
  - Reactions: clickable to toggle yours; `+` opens emoji picker
  - Inline `code`, **bold**, autolinked URLs with **unfurl cards** (special cases for github.com/figma.com)
  - Triple-backtick fenced **code blocks** with language tag
  - @mentions (member, `@everyone`, `@here`) get amber pill, broadcast variants have outline
  - Reply reference banner above body: click to scroll-flash-highlight the original
  - "edited" indicator next to time

**2. Voice channel** (e.g. `#voice-lounge`)

- Same header treatment (`Icon.Speaker`, SRTP badge, "N connected")
- Grid of participant cards (min 220px), centered, gap 14px
- Each card: media area (avatar 64px, or mock webcam, or mock screen-share), name, state line, top-right badge cluster (mute / deafen / cam / screen / network-quality), bottom volume slider on hover (skipped for own card)
- **Focus mode** — cam/screen cards have an expand button (top-right on hover). Click expands to fill the area, others collapse to a centered bottom strip. Focused mini gets amber outline + "● VIEWING" pill; speaking participant separately glows accent.
- Empty state: "Quiet here · Click *Join* to be the first in #voice-lounge"
- Right-click cards: View profile · Focus stream · Mute for me · Server-mute (admin) · Disconnect
- Bottom controls bar: 44×44 round buttons (mic / deafen / cam / screen / disconnect)

**3. Direct message**

- Header shows partner avatar with presence dot + name + topic (their custom status); E2E badge
- Members panel auto-hides for DMs (set `membersOpen` via toggle is fine but DMs default closed)
- Same composer, same message rendering
- Empty state: avatar 72px + name + "first private conversation with X" + Signal/X3DH/server-sees-ciphertext pills
- Threads/Pins/Members icons are hidden for DMs

### Member sidebar (var 180–340px)

- Sections: Admin / Online / Offline with counts
- Each row: avatar (presence dot), name, status (mono), federated tag for non-local nodes (only when federation is on)
- Hover federated tag → tooltip ("Account hosted on federated node X — relayed to gbg-1 over the dilla mesh")
- Click a row → ProfilePopover; right-click → DM / Mention / View profile / Verify safety number / Mute / Kick

### Thread panel (380px, replaces member list)

- Header: "Thread · #channel-name" + close ×
- Original message at top (full-fidelity render)
- "N replies" mono label divider
- Reply list (compact message rows with clickable reactions)
- Mini composer at the bottom (single textarea + send)

### Top bar (32px, toggleable via Tweaks)

- Brand mark (custom mono "D" tile + DILLA + blinking accent caret) ← do not use the imported logo SVGs for this view; the mono brand mark is part of the visual language
- `team BERRALITOS · node gbg-1 · ● MESH OK` (or `● MESH DEGRADED` amber, or `● READY` solo)
- Center: live clock (HH:MM:SS)
- Right: keybind hints — `⌘K CMD`, `/ SEARCH`, `? HELP` (each clickable)

### Bottom bar (26px, toggleable via Tweaks)

- Sequential mono chunks divided by 1px borders, clickable chunks open relevant settings:
  - `node gbg-1.dilla.local` → Federation
  - `peers 2/2 ▲` (degraded: `1/2 ⚠`) → Federation
  - `lamport 12,944↑` (live counter)
  - `latency 14ms p50` (live)
  - `e2e SIGNAL · X3DH · AES-256-GCM` → Privacy
  - `voice SRTP · OPUS 48kHz @ 96kbps` + 12-bar audio meter (when connected) → Voice & video
  - `db SQLCIPHER · AES-256` (when not connected)
  - `v 0.4.2-nightly · build c0ffee` (right-aligned)

### Command palette (⌘K)

- Centered above viewport (80px top offset)
- 560px wide, monospace, accent border
- `>` prompt + text input + ESC/↑↓/↵ hint
- Sectioned list: NAVIGATE / VOICE / FEDERATION / ENCRYPTION / ACCOUNT
- Hover or arrow-key selects; Enter executes
- Commands either pick a channel or dispatch a window event

### Search palette (`/`)

- Similar shell, prompt `/`, header-search box opens it scoped to the current channel
- Scope toggle pill (`✓ #channel-name` ↔ `all kanals`)
- Live filtered message results: channel · author (colored) · time, body snippet with `<mark>` highlight on the query
- Empty state pre-typing: tips for `from:`, `in:`, `has:` operators (mock — not actually parsed)

### Settings prototype (superseded)

The prototype below is historical reference. Current settings use the fullscreen workspace in [Flow 18](../../flows/18-fullscreen-settings.md): a 15rem desktop sidebar with independently scrolling content, and a grouped phone index opening individual sections. The prototype's floating modal dimensions and tab organization are superseded.

- Fullscreen layout and current navigation: follow Flow 18
- Header: "User" or "Team" eyebrow + title
- User tabs: Account / Notifications / Voice & video / Appearance / Privacy & encryption / Keyboard shortcuts
- Team tabs: Team info / Invites / Roles & permissions / Federation / Audit log
- Each tab is a `<Group>` stack with `<Row label hint>` items
- Form controls: text input, select, segmented control, toggle, stepper, slider-meter, kbd table
- Footer: Cancel · esc + Save changes · ⌘↵ — Save shows a sync-to-peers toast
- Privacy tab includes the user's safety number block + a "Verify contacts" list (each member with a Verify button → opens SafetyCompare overlay)
- Invites tab: revocable table with "+ New invite link" that generates a code and copies to clipboard
- Federation tab: this node + peers table with Disconnect (confirm) + + Add peer + Generate join command (both open AddPeerWizard)

### Modals

- **NewChannelModal** — segmented (Text / Voice), name input with `#`/`🔊` prefix, slug preview, optional topic, private toggle
- **NewServerModal** — segmented (Create / Join), name + server URL OR invite token
- **NewDmModal** — filter-search, member rows with avatars
- **ForwardModal** — preview of source message + filter-search + list of channels and DMs
- **CommandPalette / SearchPalette** — see above
- **AddPeerWizard** — 4-step (Token / Confirm / Handshake / Done), terminal animation during handshake
- **SafetyCompare** — side-by-side fingerprint comparison, "Highlight blocks" pulse, Mark verified / Doesn't match
- **IncomingCall** — pulsing avatar overlay, accept/decline (large round buttons), ↵/esc keybinds
- **FirstRunSplash** — brand mark + boot log lines, runs for ~1.2s on every load
- All modals have green accent border, Esc to close, click-outside to dismiss

### Onboarding (separate page)

5-step wizard: Connect → Identity → Keys → Safety → Done.

- Connect: bootstrap/invite/already-enrolled tri-toggle, server URL field, token field. Tokens containing "invalid"/"bad"/"expired" trigger error path (red log + danger callout). Otherwise auto-advances.
- Identity: username + protection method (Passphrase / Hardware key / Both). Hardware key shows tap-animation UI with three chips for compatible authenticators (USB / OS / Passkey). "Both" requires both.
- Keys: animated terminal-style log simulating ed25519 keygen → Argon2id derivation OR WebAuthn PRF wrap OR both → X3DH prekey upload.
- Safety: QR-style grid + fingerprint number + Copy/Print/Save QR buttons + amber callout.
- Done: summary table (handle / team / role / e2e / node) + "Open Dilla →" link.

## Interactions & Behavior

### Global keyboard shortcuts (only when no text input is focused)

- `⌘K` — toggle command palette
- `/` — open search (palette)
- `Esc` — close any open palette / modal / popover
- `⌘+1..5` — jump to general / design / dev / mesh-status / random
- `M` — toggle mute (when voice connected)
- `D` — toggle deafen (when voice connected)
- `↵` — accept (in IncomingCall)

### Composer keyboard

- `Enter` sends
- `Shift+Enter` newline
- `Tab` or `Enter` accepts mention/slash popup selection
- `↑/↓` navigates mention/slash popup

### Animations

- Toast: pop-in (0.22s cubic-bezier), shrink-out at 5.3s
- Splash: fade-in then fade-out (0.3s)
- Drag-over: 0.12s background flash
- Speaking pulse: 2s infinite (subtle box-shadow)
- Hardware-tap pulse: 1.6s ease-out infinite ripple
- Connection banner: slide-down from top (0.18s)
- Message-flash highlight (after reply-ref click): 1.4s background fade
- Audio meter: bars redraw every 120ms with smooth height transitions

### State transitions

- **Voice connection**: drives voice dock visibility, bottom bar voice chunk, mic/deafen keyboard shortcuts, voice card "● speaking" indicator
- **Federation toggle**: hides node tags on members, hides mesh-summary panel, simplifies bottom bar, changes top-bar status to "● READY"
- **Degraded state**: changes top bar to "● MESH DEGRADED" amber, bottom bar to "peers 1/2 ⚠", clears latency
- **Connection banner**: separate transient state from federation/degraded; shows full-width banner across the top
- **Settings save**: dispatches a toast confirming sync to peers; per-tab state currently doesn't persist across reopens (your implementation should wire it to Zustand)

### Read receipts (mock)

Every message authored by `thim` (the viewer) shows a small accent-colored double-check next to the timestamp with tooltip "seen by ada, mira, ben". Wire to the real seen-by data when implementing.

## State Management

When implementing in the real codebase, these are the state slices needed:

- **`auth`** — current user identity, jwt, role
- **`servers`** — ordered list of teams the user is in + active server id
- **`channels`** — per-team channel lists, ordered, with unread state and per-channel muted flag
- **`messages`** — keyed by channel-id, with reactions/edits/thread state
- **`dms`** — DM list + per-DM message buffer
- **`voice`** — connection state (channelId, mute, deafen, cam, screen, per-participant volume map, focused participant id)
- **`presence`** — per-member status + custom status
- **`composer`** — per-channel draft + reply-to + mention/slash autocomplete state
- **`ui`** — palette open/closed, modal stack, settings tab, mutedChannels set, savedMsgs set, drawer state
- **`mesh`** — peer health + lamport clock + connection banner state

Mock data structure is documented in `data.js`. Use it as the type contract.

## Design Tokens

### Colors (Mesh / brutalist)

```
--bg:           #070809   (page background)
--bg-2:         #0C0D0F   (top/bottom bars, sidebar)
--bg-3:         #101214
--surface:      #0C0D0F   (cards, modals)
--surface-2:    #14171A   (hover, sub-surface)
--surface-hi:   #1A1E22   (active hover)
--hairline:     #1F2226
--hairline-2:   #363B41
--fg:           #E8ECE8
--fg-2:         #A0A6A0
--fg-3:         #5E635E
--fg-link:      #7CFF8E
--accent:       #7CFF8E
--accent-2:     #A8FFB6
--accent-ink:   #06150A
--accent-soft:  rgba(124,255,142,0.14)
--danger:       #FF6E6E
--warn:         #FFD16A
--ok:           #7CFF8E
--mention:      #FFD16A
```

Accent variants exposed via Tweaks: green (default), amber, cyan, magenta. The amber/cyan/magenta variants override `--accent`, `--accent-2`, `--accent-ink`, `--accent-soft`, `--fg-link`, `--ok` only — the rest of the palette stays.

### Typography

```
--font-display: 'JetBrains Mono'  (used uppercase for headings / kanal names in Mesh)
--font-body:    'JetBrains Mono'
--font-mono:    'JetBrains Mono'
--font-ui:      'JetBrains Mono'
--fw-display: 700
--fw-body: 420
--display-tracking: 0.01em
```

Sizes: 10–11px for mono labels, 12.5–13.5px for body, 14–17px for headings, 22px+ for hero titles. Line-heights driven by density token (compact 1.4 / regular 1.5 / cozy 1.55).

### Density

```
compact: rowPad 4px 16px,  rowGap 0,  groupGap 8px,  avatar 28px
regular: rowPad 6px 18px,  rowGap 2px, groupGap 14px, avatar 32px
cozy:    rowPad 10px 20px, rowGap 4px, groupGap 22px, avatar 36px
```

### Radii (Mesh is brutalist — almost everything is 0–3px)

```
--r-sm:   0px
--r-md:   2px
--r-lg:   3px
--r-pill: 0px
--avatar-shape: 2px
```

(Reactions, ring buttons, and a few status pills override to `999px` where they explicitly should look rounded.)

### Shadows

```
--shadow-1: 0 0 0 1px rgba(124,255,142,0.08)
--shadow-2: 0 0 0 1px rgba(124,255,142,0.18), 0 12px 30px rgba(0,0,0,0.6)
```

### Spacing

Use 4px base; columns spaced via `gap` not margins. Density token covers row-level spacing.

## Assets

- **Brand assets** in `branding/` (icon, logo, wordmark) lifted from the real repo at `github.com/dilla-chat/dilla-chat/tree/main/branding`. These are real assets — use them in places where the brand should appear (browser tab favicon, splash thumbnail, README). **The Mesh top bar uses a custom mono mark (D-tile + DILLA + caret), not these SVGs** — keep it that way.
- All other icons are inline SVGs in `chat-icons.jsx` — re-implement as React components or import from your icon library. Maintain stroke widths and proportions; they're tuned for the small (12–16px) display sizes.

## Files

`Dilla Mesh.html` — main entry point. Open this to see the full app.

`Dilla Onboarding.html` — onboarding flow entry point.

Component files (load order matters in HTML, see the `<script>` tags in the HTML files):
- `themes.js` — theme tokens, density presets, `themeVars()` helper
- `data.js` — mock data: servers, members, channels, messages, DMs, threads
- `chat-icons.jsx` — inline SVG icons exported on `window.Icon`
- `tweaks-panel.jsx` — Tweaks shell (vendor — from a starter component, do not reimplement)
- `settings.jsx` — Settings modal + tab components
- `extras.jsx` — Notification stack, splash, incoming call, safety compare, add-peer wizard, connection banner
- `mesh-chrome.jsx` — Top bar, bottom bar, command palette, search palette, member node-origin data
- `onboarding.jsx` — 5-step wizard
- `chat-app.jsx` — root ChatApp + ServerRail + ChannelSidebar + TextChannel + VoiceChannel + MemberList + ThreadPanel + ProfilePopover + EmojiPicker + Modals + Empty states

CSS:
- `chat.css` — main stylesheet; all layout + most component styles
- `mesh-chrome.css` — top/bottom bar + command palette styles
- `settings.css` — settings modal styles
- `extras.css` — toast / splash / ring / safety / wizard / connection-banner styles
- `onboarding.css` — wizard styles

## Implementation order suggestion

1. **Tokens first** — extend `client/src/styles/theme-default.css` with Mesh tokens. Add a theme switcher if you want the existing themes to coexist.
2. **Layout shell** — refactor `AppLayout` to the new 4-column grid with resizable middle handles.
3. **ServerRail / ChannelSidebar** — biggest visual changes; many right-click menus and DnD reorder.
4. **TextChannel + VoiceChannel** — the meat of the work. Message rendering with full formatting, hover toolbar, context menu, reply system, reactions, emoji picker, mention/slash autocomplete.
5. **MemberList + ThreadPanel** — small, depend on message rendering being done.
6. **Settings modal** — large but self-contained. Lift to a single overlay route.
7. **CommandPalette + SearchPalette** — global, easy to keep modular.
8. **Onboarding** — standalone page; can be done in parallel.
9. **Extras** — splash, banner, toasts, ring, safety compare, peer wizard. Many of these are global overlays that depend on a global event/state bus (or your Zustand stores).
10. **Wire to real backend** — replace mock dispatch calls with API + WebSocket + crypto calls.

## What's mocked vs. real

| Surface | Mocked | Needs real wiring |
|---|---|---|
| Server identity, JWT | Decorative | `services/auth.ts` + ed25519 challenge-response |
| Channels, messages | All in `data.js` | REST `/api/v1/channels` + WS events |
| Reactions, edits, deletes, pins | Local state | WS event echoes |
| Voice connection, audio meter | Local toggles, sine-wave bars | `webrtc-rs` SFU + actual stats poll |
| Webcam preview, screen-share | Static gradient + SVG editor mock | `getUserMedia()` / `getDisplayMedia()` |
| Federation peer status | Local state + ⌘K simulations | WS heartbeat + lamport sync |
| Encryption + safety number | UI only, no real keys | Real Signal Protocol bridge via Tauri |
| File upload + encryption | `setTimeout` ladder | Real upload + Signal sender-key encryption |
| Onboarding terminal output | Pure animation | Real REST + WebAuthn + key generation |
| Search | In-memory linear scan over mock messages | Server-side search index (encrypted messages need client-side decrypt-then-filter) |

## Caveats / known imperfections

- Settings field state doesn't persist across modal close/reopen (typed values reset). Implementation should bind to Zustand.
- Toast manager isn't deduped — implementation should add an id-based dedupe.
- Mock pinned messages are hardcoded per-channel in `chat-app.jsx`. Real pinning is a separate event type.
- `@everyone` / `@here` are styled but don't drive any actual broadcast behavior.
- No keyboard navigation between channels (vim j/k) — would be nice to add.
- ARIA landmarks / screen-reader audit pending — implementation should add proper roles + `aria-current` + focus rings on all interactive elements.
- Tweaks panel (in the bottom-right) is a prototyping tool — **do not ship it**. It's there for the design review only.
- The "Save changes" footer in Settings shows a toast but doesn't actually persist state across modal reopens.
