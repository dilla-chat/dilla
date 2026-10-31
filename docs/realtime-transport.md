# Realtime transport and retained requests

Wire 5 uses the existing authenticated gateway connection for build notices (56), genuine
presence input (57), and coarse call reports (58). Production HTTPS pages use WSS. HTTP
continues to serve initial state, authorization-bearing commands and uploads.
There is no second socket, broker, durable telemetry queue or change to MLS/media encryption.
Jonas explicitly removed web compatibility fallbacks: unavailable telemetry is dropped, and versions
are never polled periodically. Native wire-3 Bots continue to work.

## Timer and request inventory

| Purpose | Current trigger and transport | Retained HTTP purpose |
| --- | --- | --- |
| Web build | Verified entry on gateway ready/resume; startup, foreground, online and reconnect reconcile against `/app-version.json`. Versions are never polled periodically. | No periodic polling, including before login, on older wires or during an outage. Startup, foreground, online and reconnect still verify the manifest. One check in flight and at most one queued reconciliation. An unresolved new hint retries after one second with exponential backoff capped at five minutes (thirty seconds during a call); successful verification stops those retries. |
| Presence | Visible pointer/key input, at most once per five seconds, sends 57. Mount, foreground and heartbeats do not manufacture input. | The web app drops unavailable input, with no HTTP fallback or replay. Existing server HTTP and WSS adapters retain their shared five-second device allowance. |
| Coarse call report | Current six-field report every ten seconds through 58; one page stats read/request in flight, missed ticks discarded. | The web app drops unavailable samples with no HTTP fallback. Existing server adapters still share authorization and a five-second device reservation. Congested or ambiguously sent WSS samples are never resent. |
| Call stage RTT | Local RTC read every two seconds; changed RTT (at least 5 ms) reports through `callRtt`, one in flight. Paused/reconnecting clears it. | `/v1/calls/:id/rtt` updates the existing SFU participant attribute. This separate value has different recipients and side effects from the coarse six-field report. |
| AFK voice policy | Pointer/speech observations, at most once per five seconds; exemption retries back off to thirty seconds. Away roster changes already use gateway 55. | Existing AFK activity/exemption commands enforce the voice policy; presence input does not replace them. |
| Activity list | Reads local notice, badge, membership and channel slices. | No independent Activity-screen HTTP polling loop. |
| Reminder inbox | Initial read, serialized sixty-second refresh and foreground reconciliation. Paused-provider availability retries every fifth poll. | Existing encrypted Bot interactions, capability checks and acknowledgements. No encrypted-inbox protocol redesign in this change. |
| Visible poll card | Thirty-second refresh, one pending read. | Existing encrypted Bot poll reads while the card is visible. |
| Bot command status | Checked on demand around command interactions. | The current installation/authority snapshot at `/v1/communities/:id/bot/command-status`; no general five-second background status loop. |
| Pending Bot response | Two-hundred-millisecond response checks, bounded by thirty seconds. | Existing encrypted interaction response/acknowledgement transport; only while a request is pending. |
| Bot setup/auth panels | Installation joining: two seconds, at most thirty attempts. Music authentication: one-second UI refresh while pending/open. Role/welcome pending panels also have bounded refresh loops. | Reconciliation of an explicit setup or authorization action. |
| Music player | Shared per-channel twenty-five-second visible refresh, foreground and track-end reconciliation. Local playhead renders every second. | Existing encrypted Bot music snapshot. The one-second playhead timer does not fetch. |

Sources are `web/src/update.ts`, `App.tsx`, `screens/Shell.tsx`, `call/session.ts`,
`screens/commands/{UtilityCommandProvider,PollCard}.tsx`, `screens/voice/music.tsx`, and
`client-core/src/bot/interactions.ts` under `packages/`.

## Bounds and authorization

Frames 56–58 are ephemeral (`n = 0`, no group) and only used on an actually selected wire-5
connection. Ready/resumed shapes remain unchanged. The server retains wires 2–4 and native wire-3 Bots; current web producers have no legacy HTTP fallback. Build hints are validated paths, then verified through the manifest;
they cannot themselves select an arbitrary reload URL.

Stats bodies are at most 1 KiB. Eight workers serve at most 128 device slots, including
running jobs; a device has one running sample and one replaceable latest sample. Jobs expire
after ten seconds and have a two-second processing deadline. Disconnect cancels pending work.
Processing resolves the current bearer hash and rechecks session, membership, leaf, permission,
barred device and live call before aggregation. HTTP uses the same acceptance service. Gateway
backpressure drops telemetry without delaying its reader or heartbeats. Client sends stop above
64 KiB of buffered socket output. Aggregate diagnostics contain counts, not device identities.

## Upgrade and rollback

Deploy the API/web and Bot together after the combined release gates. New sessions prefer wire 5;
existing selected wires remain compatible. Gateway reconnect supplies the current immutable web
handler's verified build entry. Updates wait for open dialogs and live/recovering calls, and retain
the existing draft/file checkpoint behavior. Pre-login pages show a manual notice.

Migration 55 and the combined schema admit wire 5. Its down migration maps selected wire 5 to
wire 4 without deleting bearer sessions; wire 5 retains the wire-4 HTTP/attestation baseline.
Rollback must still honor all earlier account-kind/data-version and native-state compatibility
requirements in the deployment runbook. Do not delete moderation checkpoints to roll back.
Keep the pre-upgrade database/native-state backups and the exact previous compatible artifacts.

Ordinary request bodies now default to a thirty-second read deadline (`server.read_body_timeout`).
Commit and blob routes retain their existing longer budgets. An unsupported deadline writer fails
closed. Bot limiter cache capacities default to 1,024 devices, 2,048 targets per device and 16,384
targets overall; zero selects defaults and invalid capacities fail configuration validation.
Only idle, fully replenished entries with no waiters can be evicted. Capacity pressure defers
requests rather than resetting a rate allowance.

Moderation checkpoints and pending actions live in the existing protected native store. Pending
capacity is 1,024 per installation; overflow defers without advancing delivery. Accepted receipts
finish local case/evidence persistence, while expired or changed-policy uncertain actions use
receipt-only lookup and cannot create a new effect. These records and policy activation boundaries
must survive restarts and backup/restore together with the installation's other native state.
Signed removal proofs are committed with their decision before submission and reused after membership
loss. Durable moderation completion/checkpoint failures do not use the journal's ordinary age-out
policy: until completion is durable, delivery remains unacknowledged. Other text/held-event age
limits retain their existing behavior.
