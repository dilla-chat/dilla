# Bot v2: sealed commands, instant replies, webhooks

Status: draft for Jonas's review (2026-11-06). Replaces the transport in
[protocol/10-community-bot.md](../../../protocol/10-community-bot.md). It keeps the Bot's features.

## Why

A Bot command in dilla is slow and keeps breaking. Each command needs a working chain:

- the device's KeyPackages;
- a private two-member MLS group (kind 3) between the Bot and **each** device;
- a signed admission;
- a capability round trip before **every** command (`authorizeOperation` calls `capabilities()`, `packages/client-core/src/bot/utilities.ts:599`);
- the client polling for the reply every 200 ms for up to 30 s (`packages/client-core/src/bot/interactions.ts:4-5`);
- the Bot polling its groups over HTTP.

Each link has failed in production in two weeks:

- KeyPackage publish 403 for admins (#154);
- a refused registration retried for ever (signal3);
- the picker dropping its list (signal3);
- on 2026-11-05/06 the Bot's command intake jammed: a "sender trust refresh" pass re-verifies every member of every group, passes collide on a shared lock, and one failed read fails the whole pass. Report: `jobs/da223a73/tmp/bot-intake-root-cause.md`.

The Bot's health log said "healthy" throughout. `/play` takes seconds when it works.

The production measurements from CT 144 (2026-11-06) are worse still:
- **The Bot reaches dillad over the public internet.** Its `base_url` is the public name. Every request goes out to the VPS, through the tunnel and back into the same container.
- **Device-list reads grow with every device.** They run at about 23,000 an hour and rise by 1,500–3,700 an hour with each device added.
- **Intake failures are steady, not a one-off.** They have occurred at 1–3 an hour since at least 2026-11-01. One answered command took 2 s.

Hence the local-socket default and the O(changed users) rule below.

Jonas, 2026-11-06:
- "it needs to be completely architecturally redesigned";
- "in order for it to be a music player, we can't have any lag";
- it should be "easy to integrate towards, lets say a terraria server bot or something like that, or weather";
- "the bot should be highly integrated into dilla, but dilla can receive webhooks or similar to the bot endpoint".

## Decisions Jonas made

1. **Complete redesign, not patches.** The per-device interaction groups, admissions, interaction receipts and sender trust refresh go. No compatibility shim keeps them alive.
2. **The Bot sees commands, never ordinary chat, by a simpler path than per-device groups** (prompt answer "Only commands, but simpler"). Writing to an MLS channel means being able to read it, so decision 7 limits the Bot to dedicated Bot channels.
3. **Integrations live inside our Bot.** Third parties don't write MLS bots. A game server or script reaches dilla through a **webhook endpoint on the Bot**, and the Bot posts into a channel.
4. **No lag.**
   - A command reacts within one round trip (your ping).
   - `/play` gives first sound in about one second.
   - A queued or repeated track starts at once.
5. **The Bot uses a persistent WebSocket.** It uses a local socket when it runs beside dillad, and `wss://` when it runs elsewhere. No HTTP polling.
6. **Automod is removed** ("lets remove automod").
7. **Dedicated Bot channels** (prompt answer). The Bot is an MLS member only of text channels an admin has marked as Bot channels, and it posts only there. It leaves every other channel group.
8. **Live updates are a pushed hint followed by one sealed read** (prompt answer). The server pushes a content-free "Bot state changed" hint, and the app fetches the new state with one sealed request.
9. **The ~1 s `/play` target includes the Bot joining a call it isn't in yet** (prompt answer "Yes, redesign the join").
10. **The Bot is bundled with the server but runs as its own process** (prompt answer "Bundled, separate process").
    - The dilla server package ships the Bot. dillad starts it as a sandboxed child process: own OS user, own state and keys. They talk over a local socket.
    - There is no separate dillabot package, unit, `bot.toml` or `base_url` to configure.
    - Bot keys and command plain text never enter dillad's memory, so the server stays blind.
11. **Outgoing integrations come in a later release** (prompt answer). `/weather` calling an admin's URL is out of v2. The catalog must not preclude it.
12. **Move as much as possible from REST to the WebSocket** (Jonas, 2026-11-06: "no move as much as you can to WSS"). This overrides Claude's recommendation to keep app REST. See "App requests over the socket".

## Goals

- **G1 Instant picker.** Typing `/` shows the community's commands from a list already on the device. The server pushes changes to the list. No request at open time.
- **G2 One-round-trip commands.** Enter sends one sealed request. The Bot's first answer ("Searching…", "Saved", an error) is pushed back over the device's existing gateway connection.
  - Target: client-measured Enter → first answer ≤ RTT + 100 ms at p95 on a healthy instance.
  - There is no capability round trip before a command. The Bot refuses an operation it doesn't support.
- **G3 Music without lag** (measured from Enter on the client).
  - New track: first audio frame in the call p50 ≤ 1 s, p95 ≤ 2 s.
  - Next queued track: gap ≤ 150 ms.
  - Repeated track within the cache window: p95 ≤ 500 ms.
- **G4 Nothing per device.** A new device can use Bot commands the moment it has a session. Restarting the Bot leaves nothing half-registered.
  - A Bot restart is ready for commands within 5 s.
  - Per-command Bot work is O(1) in community size. The Bot never fans out over members per command.
- **G5 Incoming webhooks.** An admin creates a webhook for a channel. Anything that can send an HTTPS POST posts there through the Bot under a name the admin chose. Examples: a Terraria server, Home Assistant, GitHub, `curl`.
- **G6 Honest health.** The Bot's health is a probe of the real command path: a sealed command through the server and back, every 60 s. The Bot also reports intake progress. The admin Bot page and `dillabot doctor` show "commands failing since …" when it fails.
- **G7 Keep every feature except automod.** Feature behaviour and stored data survive the redesign. Channel posts move to Bot channels (decision 7).
  - music;
  - polls;
  - reminders;
  - welcome;
  - role panels;
  - scheduled announcements;
  - moderation cases and evidence;
  - join roles.

## Non-goals

- Third-party bot programs that are their own MLS members, or a public bot SDK. Integrations go through the Bot's webhooks (decision 3).
- A Bot that reads ordinary chat for new features.
- Music providers other than YouTube.
- Outgoing integrations (decision 11). The catalog's command record keeps room for an admin-defined handler later.
- **Automod.** Jonas, 2026-11-06: "lets remove automod". It is the one feature that reads every channel message, so it is removed, not ported (see Migration).

## Anti-goals (failure even if it "works")

- Any background job that does per-member or per-device work on a timer or per command. This is the failure class of 2026-11-05.
- A health signal that can't fail.
- The client polling for a Bot answer.
- A pushed command list that the server can't keep current (a stale picker).
- Plaintext command arguments, replies or reminder text stored on, or readable by, dillad.

## Architecture

### Parts

| Part | Role |
|---|---|
| dillad | Keeps the per-community **command catalog** and the Bot's published **command key**. Forwards sealed requests and replies between gateway connections. Receives webhook POSTs and forwards them to the Bot. Stores no command content. |
| dillabot | One process: one persistent gateway connection to dillad, plus the feature workers (music, polls, reminders, welcome, announcements, moderation). Its state stays in the existing encrypted store (dilla-bot-core, SQLCipher). |
| client-core / web | Holds the catalog locally (picker). Seals commands, receives sealed answers on the gateway, opens them. |

### Command catalog (G1)

- dillad holds, per community, the list of commands available to members: name, arguments, description, the operation it maps to, and who may use it.
  - Built-in features contribute their commands when an admin enables the feature.
  - Webhooks add nothing to the catalog: they are inbound only.
- **The catalog is not secret.** The server sees command names and arguments' *shape*, never values. It is filtered per member by their permissions.
- **Distribution:**
  - It is pushed on the gateway as one event when it changes: a new revision, sent to the community's connected members.
  - A client fetches it once on connect if its cached revision is older, as part of the normal sync (no extra periodic request; wire-5 rule).
- The picker reads only the local copy, so it never shows "Loading Bot commands…" once synced.
- A disabled or broken Bot shows its commands greyed with a reason ("The Bot is offline since 06:24"). Commands don't vanish.

### Sealed commands (G2, decision 2)

- **The command key.**
  - The Bot holds a **command key pair per community** and publishes the public half to dillad, signed by the Bot's device identity key.
  - Clients fetch it with the catalog and check the signature against the Bot's device in the device list.
  - Admins see its fingerprint on the Bot page.
- **Request.**
  - The client builds `{request_id, community_id, channel_id?, operation, args, sent_at, reply_key}`, where `reply_key` is a fresh one-time X25519 public key.
  - It signs the request with its **device identity key** and seals the signed request to the command key with HPKE (RFC 9180; X25519, HKDF-SHA256, ChaCha20-Poly1305 unless the core already standardises another suite).
  - It POSTs the sealed request to dillad (one HTTPS request on the open connection; it answers `accepted` or a clear refusal).
  - dillad authenticates the session, attaches the authenticated `user_id` and `device_id` outside the seal, and pushes the request to the Bot's socket.
  - dillad stores nothing beyond a short-lived `request_id` dedupe entry (≤ 5 min).
- **Verification in the Bot.**
  - The Bot opens the seal and checks the device signature against its **cached device list** for that user.
  - The cache is updated by the server's device-list change event, pushed on the Bot's socket. It is never refreshed per command or per pass.
  - It checks that the signed device matches dillad's attached device, rejects stale `sent_at` (> 2 min) and replayed `request_id`s, and authorises the operation by the user's current permissions, read from the same pushed member/role events.
- **Reply.**
  - The Bot seals each answer to `reply_key` (HPKE) and sends it to dillad addressed to that device.
  - dillad pushes it on the device's gateway connection.
  - A command can stream several answers ("Searching…" then "Now playing: …").
  - If the device is offline, the answer is dropped after 2 min. Durable results, such as a reminder or poll, live in Bot state and are readable from any of the user's devices.
- **Timeouts.** With no first answer within 5 s, the client shows "The Bot didn't answer" with Retry. Retry reuses `request_id`, so it is idempotent. It never spins indefinitely.
- **Key rotation.**
  - The Bot rotates the command key weekly and keeps the previous private key for 24 h, then deletes it.
  - A later leak of the current key can't open replies (one-time reply keys), and can't open requests older than the rotation window.
  - This is the trade accepted against MLS's per-epoch updates.

### Threat model

- **First sight of a user is trust on first push** (Jonas, 2026-11-07).
  - The Bot pins a user's account key (UMK) the first time dillad pushes that user's device list. The push carries the UMK, its certificate over the self-signing key, and the signed list history from version 1.
  - So for a user the Bot never met in an MLS group, the Bot trusts the server for that first sight, as the app does when it first meets a member.
  - A later push under a different account key does not replace anything. It is held as a pending key change, and that user's commands are refused until the change is resolved: by the user's MLS leaf, by a newer list under the old key, or by an admin re-pin.
  - A pushed head that does not chain from the Bot's cache marks the user behind (commands refused) only when the user's own key signed it. A forged claim changes nothing.

### Bot channels (decision 7)

- **Marking.** An admin marks a text channel as a **Bot channel** in channel settings or on the Bot page.
  - The channel shows a Bot badge, and its member list includes the Bot.
  - The Bot joins its MLS group when the channel is marked and leaves when the mark is removed.
  - The Bot page lists every Bot channel.
- **Where Bot posts go.** All of these go only to Bot channels, and are verified by the client as today (author = the community's current Bot user):
  - welcomes;
  - scheduled announcements;
  - role panels;
  - poll posts;
  - music "now playing" cards;
  - webhook posts.

  Each feature's settings pick its target Bot channel from that list.
- **Commands typed elsewhere** (my call).
  - Commands are sealed requests, so they work from any channel or DM.
  - A command whose result is a channel post (`/poll create`, `/rolepanel create`, `/announcement create`) takes its target Bot channel as an argument. It defaults to the feature's configured channel, and the reply says where it went ("Poll posted in #bot").
  - With no Bot channel, those commands are greyed: "Mark a channel as a Bot channel first".
  - `/play` and the other music controls need no text channel. The "now playing" card is posted only when music has a target Bot channel.
- **Migration.** On upgrade, the Bot leaves every channel group that is not a Bot channel. No channel is marked automatically: the admin chooses.
  - Features that posted into ordinary channels keep their settings but post nothing until a target Bot channel is chosen.
  - The Bot page flags them as "needs a Bot channel".
  - Existing Bot posts stay where they are.
- **Membership trust, event-driven.** The Bot remains an MLS member of Bot channels and of call groups while it plays music, so it must still check new and changed members' leaves.
  - It does that from the pushed device-list cache, only for leaves whose user's device list changed since it last checked.
  - That is O(changed users) per event, never per pass, per command or on a timer.
  - A user whose device list can't be read is held alone, with a bounded retry. It never blocks others. (This replaces the "sender trust refresh" whose all-member passes jammed production.)

### Live updates (decision 8)

- **Hints.**
  - **User-scoped** (a reminder fell due or was changed elsewhere): dillad pushes `bot_state_changed{scope: user, revision}` to that user's connected devices.
  - **Channel-scoped** (poll tally, music queue and now-playing state): it pushes `bot_state_changed{scope: channel|call, id, revision}` to members currently viewing that channel or in that call.
  - A hint carries no content.
  - dillad learns only that something changed for that user, channel or call. It keeps the last revision number per scope; that is all it stores.
- **Fetch.** A client that sees a newer revision fetches once with a sealed read: op `reminders.due`, `poll.get` or `music.state`.
  - Bursts are coalesced: at most one fetch per scope per 250 ms.
  - Nothing is fetched for a scope the client isn't showing, except user-scoped reminders.
- **Offline devices.** A device that was offline compares its stored revision with the current one during normal connect sync, and fetches once if behind. A reminder that fell due while every device was offline shows on the next connect.
- **Removes:**
  - polling of op 84;
  - the PollCard 30 s refresh (`PollCard.tsx:215`);
  - the music panel's op-10 polling (`voice/music.tsx:59`).

### Webhooks (G5)

- An admin creates a webhook on the Bot page for one channel, with a display name and avatar. They get `https://<instance>/hooks/<id>/<secret>` once. It can be rotated and deleted.
- `POST` with JSON `{"text": "...", "username"?: "...", "avatar_url"?: "..."}` (Discord-compatible field names, so existing integrations work), at most 4000 bytes of text.
- dillad checks the secret (constant time, stored hashed) and rate-limits per webhook (default 30/min, burst 5). It forwards the body to the Bot over the socket and returns `204` once the Bot has accepted it. It never stores the body.
- The Bot posts it in the channel within the push path. Target: POST → message visible to a connected member ≤ 500 ms p95.
- A webhook post is plain text with links, shown with a "via webhook <name>" label. It can't run commands or mention @everyone unless the admin allows it.
- A webhook's channel must be a Bot channel.

### App requests over the socket (decision 12)

- **One generic request frame on the existing gateway connection:**
  - `request{id, method, route, body}` → `response{id, status, body}`.
  - The server dispatches it to **the same handler** the REST route uses: same authentication (the socket's session), the same authorization, per-route rate limits, validation, error codes and CBOR bodies.
  - No handler is rewritten per route, and REST and socket can't drift apart.
  - Responses may arrive out of order and are matched by `id`. Many requests can be in flight at once.
- **Safe retries.**
  - Every mutating request carries the idempotency key or request id its REST form already uses.
  - A request whose socket dropped before its response is retried on the new connection with the same id, and the server answers from its dedupe record.
  - The client never resends a mutation without its id.
- **What moves:** every authenticated call the app makes while signed in, including:
  - message send, edit, delete, reactions and history;
  - groups, commits, proposals, handshakes, heal, cursor and welcomes;
  - KeyPackages and device lists;
  - communities, channels, DMs, members, roles, invites (create/list/revoke), moderation and admin;
  - profile, settings, presence, search, reminders and Bot commands;
  - call control.
- **What stays on HTTP** (my call, each for a stated reason):
  - **Before a session exists:** sign-in, passkey, TOTP, recovery, enrollment, session ticket for the socket.
  - **Large binary bodies:** blob upload and download (streaming, range requests, resumable).
  - **Unauthenticated or public:** `/v1/instance`, invite preview, web assets, desktop downloads, `/healthz`, incoming webhooks (G5).
- **No REST fallback** (Jonas, 2026-11-06: "We dont need to care about old clients. dont keep a backup route").
  - Every moved route's REST form is **deleted** from the server, its route table and the protocol docs.
  - While the socket is down, the app queues requests (mutations keep their ids) and shows its existing reconnecting state. Queued requests go out in order after reconnect.
  - A tests-only REST path is not kept either. Tests drive the socket.
- **Back-pressure:** a cap on in-flight requests per connection (my call: 64). Over the cap the server answers `E_BUSY`, and the client queues instead of failing.
- **Measurement:**
  - The client records round-trip time per route for socket requests.
  - Diagnostics shows the slowest routes.
  - Tests assert that a socket request costs no more than its REST form on the same connection, and that no app request goes over REST while the socket is up, except the "stays on HTTP" list.

### Bot connection (decision 5)

- **Process model** (decision 10).
  - dillad supervises the Bot as a child process: it starts it, restarts it with backoff, and stops it with dillad. It runs under its own OS user (`dillabot`), its own state dir, and a sandbox no weaker than today's unit hardening.
  - The socket is a Unix socket created by dillad and passed to the child. There is no network path and no URL to misconfigure.
  - The Bot is enabled per instance in `dilla.toml` and per community in the Bot page.
  - `dillad doctor` covers the Bot.
- **Transport.** dillabot keeps one gateway connection to dillad over that socket. `wss://` to a remote dillad is kept only as a development and test mode.
- **What it carries, in both directions:**
  - sealed requests;
  - replies;
  - webhook posts;
  - catalog and command-key publication;
  - device-list, member and role change events;
  - channel group events the Bot needs.
- **Resume.** The socket has sequence numbers and resume: a reconnect replays missed events since the Bot's last ack, with nothing lost.
- **HTTP.** dillabot makes HTTP requests only for one-off actions. Nothing is periodic.

### Music (G3)

- **Lookup.** A **warm resolver**: one long-lived sandboxed yt-dlp worker per Bot (same bubblewrap and egress rules as today). A single resolve returns both metadata and the stream URL, so there is no second yt-dlp run. Lookups are cached by query and URL for 6 h.
- **Playback.**
  - It starts from the first audio bytes.
  - One ffmpeg goes from the stream to Opus 48 kHz 20 ms frames, straight to the LiveKit publisher.
  - The Bot stays joined to the call while the queue is non-empty (no per-track join).
  - It pre-resolves and pre-buffers the next queued track at most 30 s before the current one ends.
- **Cold join** (decision 9). G3 includes the first `/play` into a call the Bot isn't in.
  - The join runs **in parallel** with the resolve.
  - Today's join re-checks the device-list history of every leaf in the call before it gets keys (`cmd/dillabot/music_call_join.go:116-454`). The new join verifies leaves against the pushed device-list cache. Only users whose list changed since the cache's last update are re-checked, as in "Membership trust, event-driven".
  - The LiveKit connection and the call-group join start as soon as the `/play` request is opened, before the track resolves.
  - The Bot never joins a call uninvited: no early join.
- **Feedback.** "Searching…" is answered immediately (G2). "Now playing" arrives when audio starts.
- **Measurement.** Every start records stage timings: resolve, first byte, first frame published. The Bot page shows the last 20; tests assert G3 on a fixture stream.

### Health (G6)

- The Bot probes itself every 60 s with a sealed no-op command through dillad and back.
- It reports to dillad: last probe result, intake queue depth, oldest unanswered request age, and music start p95.
- dillad shows the Bot as degraded when any of these holds:
  - the probe fails twice;
  - a request waits more than 10 s;
  - the queue grows for 60 s.
- The client's greyed picker reason and the admin Bot page read this.

## What goes away

- Kind-3 interaction groups and their binding (`protocol/01-groups.md` kind-3 rows).
- Interaction admissions and context snapshots.
- `bot_interaction_admission_receipts` (359k rows on CT 144, 99.99 % expired, never pruned).
- `bot_interaction_eligibility_revisions` and group registration for kind 3.
- Sender trust refresh and the group event hold. Event-driven membership trust in Bot channels and call groups replaces them.
- The Bot's membership of every channel group that is not a Bot channel.
- Client response polling and per-command capability reads.
- Structural-event HTTP polling: it moves onto the socket.
- Op 3 as a network call: capabilities become the catalog.

## Migration and compatibility

- **Packaging.** The installer and upgrade path:
  - move the existing `dillabot` install (binary, `dilla-bot-core`, wasm, state dir `/var/lib/dillabot/<installation>` with its database key, runtime credential) under dillad's supervision;
  - remove `dillabot.service` and `/etc/dillabot/bot.toml`;
  - keep the state and keys in place;
  - log each step;
  - leave a backup of the old unit and config.
- **Automod is removed.** That means:
  - ops 62/63;
  - the automod runtime and event state in dillabot;
  - `/v1/groups/{g}/automod`, `/v1/groups/{g}/automod-grants/arm` and `…/automod-grants/redeem`;
  - the Automoderation settings page, with its strings, tests and docs.

  Stored automod config in Bot state is deleted with a log line. Manual moderation stays: cases, evidence, warn, timeout, kick and ban.
- **New wire version** (wire 6) for clients and Bot. **No compatibility with older clients** (Jonas: "We dont need to care about old clients").
  - dillad refuses a wire-5 or older identify with a clear `E_UPGRADE_REQUIRED`.
  - The desktop app turns that into "Update dilla to continue" with its update button.
  - A browser tab reloads to the new client.
  - Release notes tell everyone to update.
- **Release order:** dillad + dillabot first, then the desktop app. Same rule as signal3.
- **Bot state survives:**
  - reminders, polls, settings, moderation evidence and the music queue stay in the encrypted store under their existing keys;
  - the Bot's account, device and channel memberships stay;
  - only interaction-group state is deleted, with a one-time cleanup and a log line.
- **Server:**
  - a migration drops the interaction tables after the new path is live;
  - the catalog and command-key tables are added;
  - `bot_installations` stays.
- **Rollback:** a binary rollback to signal3 also needs the signal3 desktop app, and interaction groups re-created (they self-register). The release notes say so.
- **Docs:** `protocol/10-community-bot.md` is rewritten. The Bot sections of 09/01/02/07/08 and docs/user, plus the operator docs, are updated in the same change.

## Testing requirements

- **End-to-end (desktop lane):**
  - picker shows Bot commands with no network after sync;
  - `/remind` round trip;
  - `/play` on a fixture stream measured against G3;
  - a new device uses `/play` within its first session;
  - a Bot restart mid-queue resumes;
  - a webhook POST appears in the channel.
- **Load:** a community with 200 members and 600 devices. Per-command Bot CPU and dillad requests are constant, and device-list reads per command are 0.
- **Fault:**
  - a dropped socket resumes without losing a request;
  - a failed device-list event for one user does not affect others;
  - health goes degraded within 2 probe periods when intake is stopped.
- **Crypto:** HPKE vectors; a wrong device signature, wrong device and replay are each refused; replies can't be opened by another device.

## Left to the builder

- Exact wire frames, op numbers, table names and the HPKE library.
- Catalog schema and per-member permission filtering, as long as G1 holds.
- How feature workers are restructured inside dillabot.
- Whether dilla-bot-core stays a separate process. Keeping it is fine; it no longer runs MLS for interactions.

## Calls made by Claude (check these)

- Posting commands take a target Bot channel, defaulting to the feature's configured one. With no Bot channel they are greyed.
- Migration marks no channel automatically. Features that posted into ordinary channels wait for the admin to pick a Bot channel.
- Sealed-request timeout 5 s, offline reply drop 2 min, key rotation weekly with a 24 h overlap, dedupe 5 min, `sent_at` window 2 min.
- Webhook rate limit 30/min with burst 5, and Discord-compatible JSON fields.
- Lookup cache 6 h, and the next track pre-buffered from 30 s before the current one ends.
- Hint coalescing 250 ms.
- Health probe every 60 s; degraded after 2 failed probes, a request waiting more than 10 s, or a queue growing for 60 s.
