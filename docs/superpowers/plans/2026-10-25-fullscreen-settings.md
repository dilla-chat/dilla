# Fullscreen user and server settings

Approved implementation plan supplied by the user on 2026-10-25. Baseline:
fresh `origin/main`, `4b5ca243`. Deliver one reviewed PR; no merge or deployment.

During acceptance the user also approved repairing the reproduced music playback
reconnect failure. The implementation incorporates main's subsequent music chat
commands and connectivity changes (`1d50f59c`), then verifies provider observations
and actual playback together. Spotify playback remains a separate acceptance check.

## Binding design

Evolve SettingsFrame/SettingsNav into native fullscreen modal workspaces. Keep
the shell and voice runtime mounted, covered content inert, incoming-call controls
inside the modal, return-to-call guarded, announcements inside the modal, opener
focus restored with visible fallback. Nested dialogs consume Escape first. Scope
heading and member-anchor queries to their container.

Desktop: grouped 15rem sidebar, independently scrolling content, combined max
80rem, normal forms max 48rem, list sections max 64rem, 2rem/1rem padding,
persistent Close with Escape hint, identity above search. Existing Mesh tokens,
typography, focus and density; no floating perimeter, backdrop dismissal or shadow.
Phone uses the existing breakpoint including landscape/coarse pointer: grouped
index → section with Back; direct section fallback is the index. Hide phone tabs
throughout settings. Respect safe areas, visual viewport, keyboard and touch sizes.
You retains identity/presence/connection and one settings entry.

## Coverage inventory and registry

User: Account/Profile (display/status/bio), Sign-in (password/TOTP/passkeys/recovery
codes/reauthentication), Devices (rename/revoke/recovery key/sign out/forget).
Preferences/Appearance (theme, density, reduced motion), Notifications (browser
permission/defaults/sounds/server and channel overrides/mutes), Voice (devices,
permissions, input mode/PTT/noise processing/microphone test), Keyboard (reference
and links to editable bindings). Privacy & support/Privacy (verification, changed
keys, comparison), Diagnostics (connection/instance/device/errors/copy). Separate
Sign out action. Preserve local versus account persistence. Participant/share
volume, channel collapse and search scope remain contextual.

Server: Server/Overview (name, AFK timeout), Channels (categories/text/voice/order,
editors and permissions), GIFs (key replacement/removal/rating). People/Members
(browse, existing role and moderation actions), Roles (create/edit/order/permissions),
Invites, Self-assignable roles (selection, configuration, publication).
Automation/Bot (installation, enable/disable, name, capabilities), Welcome messages,
Join roles, Music (DJ roles, Spotify application configuration/linking, providers).
Moderation/Automoderation, Moderation log (automatic and manual evidence), Bans.
Ownership / Leave preserves transfer/delete/leave restrictions.

One typed registry drives navigation, search and route authorization: ID, scope,
group, label key, keywords, visibility and searchable setting anchors. Management
requirements remain: manageCommunity for Overview/GIFs/Bot/Welcome/Music/Automod;
manageChannels for Channels; manageRoles for Roles; both community and roles for
Join roles; specific invitation/ban/audit permissions; Members and Self roles for
ordinary members with actions separately gated. Omit empty groups. Show loading
before permissions arrive. Permission loss cancels reads, removes privileged
content, clears inaccessible drafts and chooses an allowed section with explanation.
Disabled bot features remain discoverable with prerequisite/link to Bot.

## Routing, search and edits

Routes: /settings, /settings/:section, /c/:communityId/settings,
/c/:communityId/settings/:section. Preserve Spotify callback URL. Parse settings
before channels. User root is phone index/desktop Profile; server root is phone
index/desktop first permitted section. Preserve originating conversation separately.
Desktop sections replace; phone index→section pushes, Back returns to index.
Observe history-state changes in snapshots. Direct/reloaded/invalid/unauthorized
sections resolve only after permissions load without forbidden reads.

Search static labels, descriptions and synonyms only; all terms, case insensitive,
exact label then prefix then other matches. Results show setting and breadcrumb;
Enter activates, Escape clears search first. Recheck permission at activation;
focus stable anchor or heading. Never fetch data for search or index account,
member, configuration or secret values.

Shared edit state: `{dirty:boolean,busy:boolean,canSave:boolean,
save():Promise<boolean>,reset():void}`. Preferences and personal roles immediate
with pending/errors; forms Profile/Overview/Bot/Welcome/Join roles/DJ/Automod have
Save/Reset. Independent key/OAuth/publish/auth/invite/destructive actions keep named
actions. Guard every navigation: sections/search/close/Escape/history/notification/
OAuth. One Keep editing / Discard changes confirmation; beforeunload. Indexed
history restores current entry while confirmation pending then replays traversal.
Reset latest confirmed baseline; refresh never overwrites drafts. Partial saves
retain failed fields only. Accepted bot requests are not Saved; workspace operation
controller retains request/retry/response ownership across sections. Leaving pending
operations explains accepted requests are not cancelled. Mount only active section;
preserve operation-filtered responses and separate moderation journal.

## Provider status contract

New read-only encrypted `music.providers.status`, operation 24. Empty arguments;
null channel/revision/expiry. Live manage-community runtime authorization. Register
Go protocol/runtime, Rust, TS, worker and decoding. Recompute replay. Versioned
bounded snapshot: installation identity, observation timestamp, fixed enums/reasons.
Separate Spotify stored credentials from helper setup; YouTube prerequisites from
playback. No tokens/codes/account IDs/paths/raw errors; no playback, refresh or new
OAuth attempt on read. No DB migration.

Spotify account: Not linked / Linked / Needs attention / Unknown. Linked means
valid installation-bound stored credentials only, not Premium/current tokens/playback.
Spotify and YouTube playback setup: Configured / Missing requirements / Unknown.
OAuth callback says Authorization received; waiting for the bot, returns to Music
and requests runtime evidence. Refresh on entry/button/OAuth. Poll 1s up to 30s,
OAuth retries up to 60s. Observations older than 60s stale with timestamp. Unsupported
older bots unavailable/retry, configuration still usable. All statuses distinguish
loading/known/unavailable; unknown never becomes healthy or disconnected.

## Tasks

### Task 1: Baseline and contracts
Primary: isolated baseline, inventory, approved plan, registry/shared interfaces.
### Task 2: Fullscreen primitives
Bounded agent: frame/nav/grouped index/status presentation and stories; keyboard,
nested-dialog, viewport and theme checks.
### Task 3: Routing and guards
Bounded agent: routes/indexed history/edit coordinator/scoped focus; history,
deep links, dirty navigation and permission loss tests. Primary owns shell wiring.
### Task 4: User settings
Bounded agent: user sections/density/motion/footer/index; preserve auth/device flows.
### Task 5: Server core and people
Bounded agent: Overview/GIFs/channels/roles/invites/members/self roles/ownership;
permission/hierarchy/scoped member focus/partial-save tests.
### Task 6: Bot decomposition
Bounded agent: Bot/Welcome/Join roles/Music/Automod/log pages and persistent request
ownership, retry/ack tests; preserve current Spotify app and bot naming.
### Task 7: Runtime provider status
Bounded agent: operation 24 end to end/protocol vectors, authorization/malformed/
absent credentials/replay/unsupported tests.
### Task 8: Search and status wiring
Primary integration with bounded agent: registry search/anchors/status/OAuth.
### Task 9: Whole-app integration
Primary: shell/routes/entrypoints/notifications/phone/voice/calls/strings/docs/e2e.
### Task 10: Final review and PR
Fresh reviewer, fix findings, required checks, screenshots, one PR.

## Verification

Focused tests per task, self-review and commits; gpt-6-luna bounded agents with
explicit ownership, no simultaneous editors of shared routing/registry/strings/
exports. Parallel work only disjoint ownership (user/server may use separate worktrees).
Primary integrates all commits and reviews architecture.

Test fullscreen/scroll, all capabilities, search authorization, loading/revocation/
deleted/kicked, privacy links, reload/history/close, all dirty exits/invalid/partial
saves, nested dialogs/focus/member anchors, pending bot response ownership, callback
versus evidence, unknown/stale/offline/older bots/missing prerequisites, secret
redaction, ongoing voice, existing Spotify config/bot display name.

Before PR: docs/brief/copy/CSS/CI checks, lint/workspace types, affected UI/web/client
tests, web build/Storybook, protocol vectors, Go/Rust, Chromium/Firefox/WebKit web
e2e and affected voice/music. Browser review at 1440x900, 1024x768, 390x844, 320px,
landscape phone/keyboard, 200% zoom, Mesh/light/high contrast/reduced motion/keyboard.
Capture Profile/Sign-in/Voice/Members/Music/Automod/search/dirty views. Update brief,
flows, user docs, screenshots and protocol docs; replace old 960x640 and live-phone-
tab rules. Record real Spotify playback acceptance separately from contract tests.
