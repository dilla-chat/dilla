# Desktop 0.1.0-beta.5 + v0.1.0-bot2.4 batch Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship Jonas's queued client changes (dock, private replies, Bot card menu, signal bars, timestamps, screen picker, voice-takeover notice, poll form) together with three small server follow-ups from the bot2.3 verification, as desktop 0.1.0-beta.5 and server v0.1.0-bot2.4.

**Architecture:**
- Client changes live in `packages/ui` (components), `packages/web` (screens, strings), `packages/client-core` (routes, catalog) and `packages/desktop` (Electron main: screen capture, locale).
- The web client ships inside `dillad` too, so the server release carries the same client.
- The server follow-ups touch `cmd/dillabot` and `internal/bot/music` (logging and health) and the bot2.3 docs.

**Tech Stack:** TypeScript/React (packages/ui, packages/web), client-core, Electron (packages/desktop), Go (dillabot), Vitest, Playwright/Electron e2e, Storybook parity shots (`docs/design/signal/parity/shoot.mjs`).

**Spec / decisions:**
- The Signal prototype is the design authority: `docs/design/signal/prototype/Dilla Signal Prototype.dc.html` and `docs/design/signal/README.md`.
- Jonas approved these deviations, recorded in `.superpowers/sdd/2026-11-10-ci-red.md` and in memory `jonas-pixel-parity-over-wcag.md`:
  - the Bot card ⋯ button by Restart;
  - removing the VoiceDock bars;
  - the dock always shown in voice;
  - private-reply auto-dismiss;
  - the thumbnail screen picker;
  - the calm voice-takeover notice.

## Global Constraints

- Pixel parity with the prototype everywhere except the approved deviations above. Ask Jonas before any other deviation.
- The UI never says "encrypted".
- Never log message content, keys, sealed bytes, or a track URL, title, video id, query or duration. Diagnostics emit fixed verdicts or opaque ids.
- No wire version bump. No schema change.
- Run vitest from each package's own directory. Run `npm run check:ci` before pushing any workflow edit. Run `npm run ci:local` before pushing.
- Commit trailer: `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>` and `Claude-Session: https://claude.ai/code/session_012Nny5K2B969mk49QX3XSXG`.

## Review Focus

1. Reading a private reply with the pointer over it, then moving away: it stays while hovered, then goes after the rest of its minute. A screen-reader user tabbing onto it must not lose it mid-read.
2. An en-US Mac with the 24-hour system setting, and a sv-SE Linux box. The message gutter shows `20:27` on both. Date separators follow the system region.
3. macOS screen share with 3 screens and 20 windows: the picker shows thumbnails, opens fast and is keyboard operable. Escape cancels and starts no share. Wayland still uses the portal.
4. Device A is in voice, and the user joins on B: A shows "Voice moved to your other device.", calm, with no rejoin. A DM-only user gets the generic notice.
5. A Bot catalog refresh while the poll form is open doesn't close it. A Bot that really stops offering /poll does close it, with a visible message.

---

### Task 1: Voice dock always shown; signal bars removed

**Files:**
- `packages/web/src/screens/voice/VoiceView.tsx` (~815-839: `data-on-stage` hides the dock; the compact music player renders only when `!stageShown`)
- `packages/ui/src/VoiceDock/VoiceDock.tsx` (:80 `BARS`), `VoiceDock.css` (:7-8), and the phone dock if it has its own bars
- Tests: `VoiceDock.test.tsx` (:196 asserts the bars), the VoiceView tests, Storybook stories, parity shots

**Behaviour:**
- **Dock in voice.** The dock, both the call card and the compact Now playing, renders while the voice channel's own stage is on screen, exactly as it does elsewhere. The stage's own NowPlaying (`:614`) stays. Drop the `data-on-stage` hide and the `!stageShown` music condition.
- **No bars.** Remove the animated signal bars from the connected phase entirely, on desktop and phone. Keep the CallTile speaking meter. Keep the other phase markers: connecting and paused. The connected phase shows no marker; the text and stats stay.

- [ ] Tests first:
  - the dock is present when stageShown, with the compact music player when a track plays;
  - the connected phase has no `.d-voice-dock__bars`;
  - the CallTile meter is unchanged.
- [ ] Implement. Update the stories and parity shots (an approved deviation; note it in the parity notes).
- [ ] `vitest` in packages/ui and packages/web.
- [ ] Commit `feat(voice): the dock stays in the voice channel; the signal bars are gone`.

### Task 2: Private Bot replies auto-dismiss after a minute

**Files:**
- `packages/web/src/screens/commands/private-results.ts` (the in-memory store, with no TTL)
- `packages/ui/src/BotResult/BotResult.tsx` (hover/focus)
- `packages/web/src/screens/conversation/Conversation.tsx:421`, `ThreadView.tsx:243`
- Tests in those packages

**Behaviour:**
- A row whose status is `success` or `failed` is dismissed 60 s after it reached that status.
- A row stays while its status is `pending`/`uncertain`, while it offers Retry, or while it shows a confirm step.
- The timer pauses while the row is hovered (pointerenter/leave) or has focus within (focusin/out). On leave it resumes with the time remaining. Restarting the full 60 s would also be acceptable.
- Dismissing moves focus as the existing manual Dismiss does: to the next row's Dismiss, or to the composer.
- No announcement on auto-dismiss.
- Constant: `PRIVATE_RESULT_TTL_MS = 60_000`.

- [ ] Tests first, on fake timers:
  - Done goes at 60 s;
  - Working stays, then goes 60 s after it turns Done;
  - a row offering Retry stays;
  - hover at 50 s, wait 30 s: still there; leave, then 10 s more: gone;
  - focus pauses the same way;
  - manual Dismiss still works.
- [ ] Implement. The timer lives with the visible row. The store keeps a `settledAt`.
- [ ] Commit `feat(commands): private Bot replies go away a minute after they finish`.

### Task 3: Bot card ⋯ button by Restart

**Files:**
- `packages/web/src/screens/admin/BotSection.tsx` (~455-465: `glennMenuItems`; today the menu opens only from the row context menu, per Ruling X)
- the shared ⋯/kebab button component used elsewhere in packages/ui (find it)
- strings
- tests

**Behaviour:**
- A ⋯ icon button sits beside the Restart button on the Bot card. Its accessible name is "More actions for {bot}".
- It opens the same menu items (Rename, Refresh status, Disable), anchored to the button.
- The context menu keeps working.
- Hidden when the items list is empty.

- [ ] Tests:
  - the button opens the menu with the items for each permission case;
  - Escape closes the menu and returns focus to the button;
  - a parity story.
- [ ] Commit `feat(admin): a ⋯ button on the Bot card opens Rename, Refresh status and Disable`.

### Task 4: 24-hour message times; dates follow the system region

**Files:**
- `packages/web/src/strings/index.ts:12` (`Intl.DateTimeFormat(undefined, {hour:'2-digit', minute:'2-digit'})`)
- `packages/ui/src/MessageRow/MessageRow.tsx:244` and `MessageRow.css` (the `data-long` 9px gutter path)
- `docs/design/signal/parity/shoot.mjs:383` (locale)
- `packages/desktop` main process (locale for the renderer)

**Behaviour:**
- Message-row times always render as 24-hour `HH:MM` (`hourCycle: 'h23'`), as in the prototype (msgCols `44px 30px …`, time 10.5px right-aligned, `20:27`).
- Remove the `data-long` 9px gutter path, because the time never exceeds 5 characters.
- Date separators and longer dates use the system region. In Electron, pass `app.getSystemLocale()` (or `app.getPreferredSystemLanguages()[0]`) to the renderer and use it as the Intl locale for dates. On the web, use `navigator.language`.
- Add an en-US parity shot, `shoot.mjs` with locale en-US, plus a test asserting `20:27` under en-US.

- [ ] Tests first: en-US, sv-SE and en-GB all give `20:27`; there is no `data-long` attribute; a date separator under sv-SE uses the Swedish month format.
- [ ] Commit `fix(chat): message times are 24-hour as designed; dates follow the system region`.

### Task 5: Thumbnail screen and window picker (macOS and Windows; Wayland keeps the portal)

**Files:**
- `packages/desktop/src/screen-capture.ts` (~105: `dialog.showMessageBox` list)
- a new picker window or renderer overlay
- preload IPC
- `packages/ui` picker component + stories
- tests

**Design** (an approved addition; the prototype has no picker), Discord-like:
- A modal titled "Share your screen".
- Two tabs, **Screens** and **Windows**. Each shows a grid of `desktopCapturer.getSources({types, thumbnailSize: 320×180, fetchWindowIcons: true})` thumbnails, with the source name under each. Windows also get their app icon.
- Choosing a tile then **Share**, or double-clicking, starts the share. **Cancel** or Escape starts nothing.
- The thumbnails refresh every 2 s while the picker is open.
- Keyboard: arrow keys move between tiles, Enter shares.
- The picker uses the Signal tokens (surface, borders, radius, fonts) like the other dialogs.
- Linux Wayland keeps the system portal (no picker). Linux X11 uses the picker.

**Security:**
- The picker runs in the app's renderer with the existing sandboxed preload.
- Only the selected source id goes back to the main process, which validates it against the ids it just enumerated before granting `setDisplayMediaRequestHandler`.

- [ ] Write the picker spec as a short design note in this task's brief, with an ASCII mock. Jonas approves it as part of this plan.
- [ ] Tests:
  - main: an id not in the last enumeration is refused, cancel grants nothing, and Wayland takes the portal path;
  - UI: tabs, keyboard navigation, Escape.
- [ ] Commit `feat(desktop): pick a screen or window from thumbnails`.

### Task 6: Calm voice-takeover notice (reason 3)

**Files:**
- `packages/client-core/src/http/routes.ts` (:49-52 `reason?: 0|1|2`; the `pendingReason` mapping ~314 maps 3 to 2)
- `packages/web/src/screens/voice/useCall.tsx` (`cutReason` ~425, `sayRemoved` ~448, `endRecovery` ~903)
- `packages/web/src/strings/afk.ts`
- tests

**Behaviour:**
- Pending reason 3 is its own value. The device that lost voice shows a calm notice in the AFK-elsewhere style: "Voice moved to your other device." It never rejoins on its own.
- When the pending reason can't be read (a DM call with no community snapshot), fall back to the generic removed message.

- [ ] Tests: reason 3 gives the new notice, no recovery or rejoin, and the notice isn't red; reason 2 is unchanged.
- [ ] Commit `feat(voice): say "Voice moved to your other device." when another device takes over`.

### Task 7: The poll form no longer closes on a catalog refresh

**Files:**
- `packages/web/src/screens/commands/UtilityCommandProvider.tsx` (:315-316: cancel on every `botCatalog:<community>` publication)
- `packages/client-core/src/bot/catalog.ts` (~436-462: `load()` notifies even when unchanged)
- `CommandHost.tsx:288` (the swallowed cancel)
- e2e support (attach the worker trace and dillad/dillabot logs on a desktop-client failure)

**Behaviour:**
- The form cancels only when the catalog stops offering op 70, or its `botUserId` changes from the one the form opened with.
- When it does cancel, the user sees a private failed row: "The Bot stopped offering polls. Try again." It is no longer swallowed silently.
- `load()` notifies only when the entry changed.
- The e2e harness attaches the worker trace and the server logs to failures.

- [ ] Tests first: a catalog republish with an unchanged entry keeps the form open; dropping op 70 closes it with the message; a botUserId change closes it.
- [ ] Commit `fix(polls): the poll form stays open when the Bot's command list refreshes`.

### Task 8: Server follow-ups from the bot2.3 verification

**Files:**
- `internal/bot/music/supervisor.go` (~770 `playback ended`)
- `internal/bot/music` end classification
- `cmd/dillabot/gateway_runtime.go` (PollText) and the health counters
- `docs/deploy/releases/v0.1.0-bot2.3.md` and `docs/deploy/README.md`

**Behaviour:**
- `bot music playback ended` carries a fixed `detail` verdict decided inside the Bot: `eof` when the track ended within about 3 s of its known duration, otherwise `invalid` / `cancelled` / `connection`. Never log the duration.
- Repeated events-stage poll failures count into bot_health. Three consecutive ones make it degraded, and it clears on the next clean poll. Log once per transition, not per wake.
- **Docs:**
  - Correct `needs the Bot re-added: no Welcome to join it from`: it means "no Welcome on this pass" and can precede a successful join.
  - Note that the first item in an already-connected call logs 9 stages (no call_token/call_fields).
  - State the diagnostics rule: verdicts and opaque ids, never track attributes.
  - The bot2.3 notes are edited in place, and the release body is re-synced after merge.

- [ ] Tests:
  - end at duration−1 s gives detail=eof;
  - end at half the duration gives invalid;
  - no duration in any log line;
  - three events-stage failures give degraded, then clean gives cleared.
- [ ] Commit `fix(bot): playback-ended verdicts; repeated poll failures reach bot_health; docs corrections`.

### Task 9: Release notes, versions, user docs

- **Desktop:** bump `packages/desktop/package.json` to `0.1.0-beta.5`, and add the release notes the desktop release workflow expects (find them via release-notes.test.mjs).
- **Server:** add `docs/deploy/releases/v0.1.0-bot2.4.md`.
- **User docs:** update docs/user for the dock, private replies, the screen picker and voice takeover. Re-shoot the screenshots the docs use.
- [ ] Commit `docs(release): desktop 0.1.0-beta.5 and v0.1.0-bot2.4 notes`.
