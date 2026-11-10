# The AFK channel

Every server has one built-in channel called "AFK" (since v0.1.0-web3.9). Since v0.1.0-web3.11 it
hosts **no call**: being in the AFK channel means having no voice connection. The server opens no
LiveKit room for it and grants no media token for it, and a client never asks for a microphone
there. What the channel holds is the **away roster**: the members who are away, each with the time
they went away.

The database stores which channel is the server's AFK channel and the inactive timeout
(`community_afk`, one row per server), and the away roster (`community_afk_away`, schema 21,
migration 00021): one row per (server, member) with `since`, the unix second the member went away.
A second mark keeps the first `since`. Leaving the server, a kick or a ban drops the row by cascade
from `members`. Device activity, the admission sequence and the device's pending reason live in the
server process with the voice calls; they are short-lived and never contain media keys.

- The server writes the channel when it makes a server (audit detail `afk.builtin`), and once at
  the first start of a new `dillad` for every server that has none. A web-3.8 server keeps its
  timeout; the channel it chose stays as an ordinary voice channel.
- A manager can rename the channel and move it between categories. The server refuses to delete it,
  to change its topic, mode, visibility or slow mode, and to give it permission overwrites. Any
  member who can see the server sees its away roster.
- Starting a call in the AFK channel (`POST /v1/channels/{id}/calls`) and the `/rtc` admission of a
  token for one of its rooms are both refused with `403 E_FORBIDDEN`, "nobody joins a call in the
  AFK channel".
- A phone is never cut. A client on a phone or a coarse pointer reports itself exempt when it
  joins a call, and the server then stops counting that device.

## How a member becomes away

1. **The sweep (idle).** A device in a call that has had no activity for the timeout is cut from its
   room. The server marks its user away unless another of the user's devices is in a call or is
   still joining, or the user has no gateway connection. The cut device gets `pending` = 1.
2. **Choosing AFK.** The client leaves its own call, then sends `PUT /v1/communities/{id}/afk/away`
   with `[]`. The server cuts every device of the user from every call room (this server's, any
   other server's, DMs), marks the user away, and gives each cut device `pending` = 2.
3. **The first start after the upgrade.** Every call still live in an AFK channel (web3.9 and
   web3.10 left them) is ended, and the users with a current leaf in its group are marked away. A
   60-second grace follows; then a maintenance pass clears rows of users with no connection.

`pending` is a device-scoped reason code: 0 none, 1 sweep, 2 AFK chosen elsewhere. A read of the
snapshot does not consume it; it lasts until that device's next admission to any call, or 24
hours. The client uses it to say "Moved to AFK" (1) or "You went AFK on another device." (2) and to
not rejoin the cut call.

## How a member comes back

Away clears, and the server tells everyone who may see the roster, when:

- the user is **admitted to a call on any device**, in any server or DM;
- the user **sends a message** (a readable post, an upload into a text channel) or **types**
  (`typing_set` on) in that server;
- the user has had **no gateway connection for 30 continuous seconds** (every tab and device closed or
  offline). A reload or a short network drop keeps the user away; a second open tab keeps them too.

There is no "leave AFK" action and no route that clears the roster. Away and calls never overlap:
if a user is away in any server, no device of that user is in any call.

```mermaid
sequenceDiagram
    participant Page as Client (this device)
    participant Other as Client (other device)
    participant API as dillad AFK routes
    participant SFU as LiveKit room
    participant DB as community_afk_away
    participant GW as Gateway (op 55)

    Note over Page,GW: The sweep
    Page->>API: POST /afk/activity [source channel]
    API->>API: Record the server receipt time for this device
    API->>API: pending = 1 for this device
    API->>SFU: Cut an idle device from its room
    API->>DB: Mark the user away (skipped if another device is in a call)
    API->>GW: afk_away [community, user, since]
    GW-->>Page: afk_away and community_changed
    Page->>API: GET /afk
    API-->>Page: [AFK channel, timeout, [], 1, away roster]
    Page->>Page: Say "Moved to AFK"; join nothing

    Note over Page,GW: Choosing AFK
    Page->>SFU: Leave its own call
    Page->>API: PUT /afk/away []
    API->>DB: Mark the user away
    API->>GW: afk_away [community, user, since]
    API->>API: pending = 2 for each device in a room, and each admitted in the last 2 min
    API->>SFU: Cut every device of the user, in every room
    GW-->>Other: afk_away and community_changed
    Other->>Other: Say "You went AFK on another device."

    Note over Page,GW: Coming back
    Page->>SFU: Join a voice channel, or send a message, or type
    API->>DB: Clear the user's row
    API->>GW: afk_away [community, user, 0]
```

## The snapshot and the frames

`GET /v1/communities/{id}/afk` answers `[channel_id, timeout_seconds, [], pending, away]`. The third
element is always empty (it held the occupants in web-3.8). `pending` is the calling device's
reason code (above). `away` is `[[user_id, since_ts]]`, ordered by user id, at most 1024 rows, and
`[]` when the community has no AFK channel.

Op 55 `afk_away` is `[community_id, user_id, since_ts]`, with `since_ts` 0 for "cleared". It goes
to every live connection of every user who may view the AFK channel, only when a row was actually
set or cleared. It is never stored or replayed, so a client reads the roster from the snapshot on
`ready` and whenever it reads the community again, and applies the frames that arrived during the
read to the newest read (`protocol/02`, `protocol/09`).

The writes and their frames keep row order: every change is appended to a queue under one lock, and
one drainer sends them with no lock held, coalescing to the last note per (community, user) in a
batch. An in-memory index of who may be away lets typing, uploads and admissions of everyone else
skip the database entirely.

## Rollback

A rollback to web3.10 is `dillad migrate down --to=20` run with the **new** binary, then the old
binary. It is not the usual `dillad.prev` swap: web3.10 refuses a database whose schema is newer
than its last migration. The down step drops the table. See
[the release notes](deploy/releases/v0.1.0-web3.11.md).

## Retired routes

The routes `POST` and `DELETE /v1/communities/{id}/afk` of web-3.8 (move and leave by hand) are not
registered any more (Bot v2 keeps no compatibility with old clients); a request for one is a plain `404`.
