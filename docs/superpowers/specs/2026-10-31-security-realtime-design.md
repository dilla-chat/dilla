# Security and realtime transport design

Status: approved by Jonas's ‘Go’; implementation and acceptance in progress. One branch, one PR, one release candidate.
Baseline: `f53e213fa6e56a1c4dd10d0c233a0b2af51026b2` (main, 2026-10-31).

## Intent and scope

Combine all four review items with the requested WSS improvements: moderation retry durability,
request-body deadlines, channel recovery races, bounded limiter caches, pushed web-build notices,
user activity and call statistics. The aim is reliable enforcement and recovery, and fewer recurring
HTTP requests while a healthy gateway connection exists. This is not a rewrite of HTTP APIs.

Jonas requires one PR for the entire change. Internally use focused commits, but do not merge or
release partial work. Preserve existing standing deployment authorization; do not ask for it again when it already covers execution.

## Approved design decisions

Use existing Go, TypeScript, CBOR, gateway, native state transactions and test harnesses; add no
broker or second socket. Jonas approved these defaults with the combined plan; later implementation rulings are recorded in the execution ledger.

1. **Moderation:** separate evaluation deduplication from completion of an enforcement action.
   Persist a bounded pending action keyed by installation, source message and revision before the
   first external effect. Retrying uses the same decision and operation identity, without counting
   the message twice. Accepted receipts resume local case/evidence persistence; an uncertain result
   is reconciled before another effect. Definitive authorization refusals may finish log-only, but
   retain an explicit refusal outcome. Preserve existing server request/source acceptance windows: a pending record does not extend them. Resume an already accepted receipt to finish its local case; if no effect was accepted and authority or the acceptance window is definitively gone, store a terminal refusal and release the delivery cursor. Never persist plaintext outside the existing protected native
   store. Bound pending records at 1,024 per installation; at capacity defer delivery without ACK.
   Delete pending state only after durable completion. Persist a versioned, bounded evaluator
   checkpoint (rolling observations and dedup state, sharded by actor) and its processed frontier
   atomically with the evaluation result/pending action in native StateTransaction, before journal
   ACK. After a crash before that transaction, the prior checkpoint re-evaluates the unACKed event;
   after it, the same decision/action resumes without counting twice. At capacity defer before
   evaluation/checkpoint advancement, leaving the event eligible after restart.
   Persist the policy activation receipt-time/delivery boundary with the acknowledged configuration;
   reuse it across process restarts instead of replacing it with the startup journal high-water.
   On first upgrade from an uncheckpointed installation, seed a conservative boundary once and do
   not punish pre-upgrade history. Historical events at/before that boundary remain non-punitive.
   Process live events after it subject to existing freshness/acceptance windows; an elapsed window
   is an explicit terminal refusal, never permission to extend enforcement indefinitely. Pending
   actions carry their policy revision: a changed/disabled policy cannot authorize a new old-policy
   effect, while an already accepted receipt may still finish its case/evidence. Checkpoint shards
   must fit native per-value/transaction bounds; abort/defer before ACK if a checkpoint cannot fit.
   Add an operator-visible checkpoint/capacity refusal counter without message contents.
2. **Bodies:** give ordinary CBOR routes a 30-second read deadline before consuming body bytes.
   Keep existing 60-second commit and configured blob-upload budgets. Clear a route's deadline on
   completion; do not install a global write timeout or break WebSocket upgrades, streaming or
   keep-alive. Failure to apply a deadline on a real production transport is observable and fails
   closed for body-bearing routes; test recorders may explicitly provide the unsupported seam.
3. **Recovery:** bind a membership-loss observation to the group's local recovery generation.
   Recheck on the group lane before external join. If another task already repaired that generation,
   retry the original read; do not join again or move its cursor. Add a deterministic interleaving
   test before changing the source-reviewed race candidate; discard that candidate only if the test
   proves the existing code already prevents it, documenting the evidence in the same PR.
4. **Limiter caches:** defaults: at most 1,024 device/fallback buckets, 2,048 targets per device,
   and 16,384 key-package target buckets globally per Client. Make limits operator-configurable.
   Evict only entries idle for at least five minutes, with all tokens replenished and no waiters.
   Retain budgets across token rotation; remove retired identities only when no live user remains.
   If no safe victim exists, defer with bounded retry-safe capacity feedback rather than reset a
   live budget or allocate beyond the cap. Expose counts, evictions and capacity deferrals.

## WSS versus HTTP

Jonas’s later clarification supersedes the original web compatibility defaults: remove web
HTTP telemetry fallbacks and periodic version polling; keep Bots working. Existing server
HTTP routes and wire support remain.

| Purpose | Healthy new gateway | Compatibility / recovery |
|---|---|---|
| Web-build notice | Current build after identify/resume; push when served build changes | HTTP verification at startup, explicit lifecycle events and after a notice; retry unresolved hints |
| User activity | Explicit coalesced activity message | Drop if WSS is unavailable; no web HTTP fallback |
| Call telemetry | Latest sample per live call/device, every 10 seconds | Drop if WSS is unavailable; no web HTTP fallback |
| Presence/typing/voice fan-out | Existing gateway events | Existing authorized snapshots on reconnect |
| History, files, durable commands, encrypted Bot requests | Existing HTTP/native paths | Unchanged |

Local WebRTC `getStats()` sampling stays local where it already is. Sending a telemetry sample
over WSS must not turn it into a reliable replay queue. The Activity inbox and Bot command-status
refreshes are distinct from user-presence activity: inventory their timers in this PR and record
which existing events could invalidate them, but do not promise their removal without a separate
authorized, confidentiality-preserving invalidation design. No claim of zero HTTP traffic.

## Protocol and compatibility

Propose human gateway wire **5**, keeping 4/3/2 supported and managed Bots on their existing wire 3.
Do not change E2EE/media versions, instance generation, existing ready/resumed tuple shapes, or
silently add semantics to the presently unused identify capability bits. Rebase before implementation
and verify that wire 5 and these opcode slots remain free; if occupied, allocate the next free
wire/opcodes and update this design, all codecs and the compatibility matrix together.

New ephemeral frames have `n=0`, null group, and never enter the replay ring:

| Op | Direction | Name and payload |
|---|---|---|
| 56 | S→C | `app_build`: `[entry(tstr)]`, same validated entry identifier as `/app-version.json` |
| 57 | C→S | `presence_activity`: `[]`; device/user come exclusively from the authenticated connection |
| 58 | C→S | `call_stats`: `[call_id(bstr16), report(existing fixed stats tuple)]` |

Only negotiated wire 5 enables these messages. Resume preserves the connection's actual negotiated
wire; a resumed legacy session must not start sending new opcodes. Legacy clients receive no new
frames. Test both deployment orders and rollback with an already open tab/call. Audit core Rust,
native, worker and server version validation, not only gateway preference constants.

## Transport-independent authority and bounded work

Extract the HTTP stats handler's acceptance logic into one typed service used by both transports.
Retain enrolled-session/device restrictions, live-call/current-leaf checks, connect permission,
barred-device checks, existing report bounds, and the existing shared minimum five-second interval. Admission must be atomic across concurrent HTTP and WSS requests, not a check-then-set after validation.
Revalidate current session authority when processing a queued sample, not only at socket handshake.
HTTP and WSS spend the same reporting allowance; transport switching cannot double it.

Use a bounded telemetry worker pool: eight workers, 128 queued device slots globally, at most one
in-flight and one replaceable latest pending sample per device. Reject unknown/oversized shapes
before queueing (stats payload <=1 KiB). Under load discard superseded telemetry; preserve heartbeat,
commit and message processing. Cancel queued work for disconnected/revoked sessions and ended calls;
recheck authority for already running work. No goroutine per report and no broad client RPC facility.

Activity requires genuine input, not connection liveness. Heartbeats currently send `active=1` even
without input and must not reset idle state. Coalesce activity to one message per device per five
seconds, share that budget across HTTP/WSS, and preserve ten-minute idle expiry, DND/invisible
privacy, AFK semantics and multi-tab/device aggregation. Send no input details or client timestamps.

Client outbound telemetry drops/coalesces if WebSocket bufferedAmount exceeds 64 KiB or the socket
is not ready; do not enqueue historical samples for reconnect. Never reroute activity or coarse reports through HTTP; an ambiguous result is dropped.

## Build notifications and missed events

Use the entry from the verified, actually served `internal/web.Handler` as the single source for
both HTTP and gateway notices. Do not watch raw installer file writes or advertise staged assets.
The current handler is loaded at server startup: normal deployment closes sockets, and the next
identify/resume supplies the new entry. No hot-reload mechanism is required; any existing supported
handler replacement must atomically publish its corresponding build notice after activation.

A notice triggers one deduplicated HTTP manifest fetch and the existing update prompt, never an
automatic reload during a call or loss of drafts. Ignore duplicate notices. Reconcile on reconnect
and foreground/online transitions; with a healthy wire-5 gateway, do not run recurring version HTTP
checks, including before login, on older wires and during outages. Retry only an unresolved actual
new build hint with bounded backoff and one request in flight. Pre-login startup/foreground/online
checks still use HTTP without interrupting forms.
The web entry, protocol compatibility and instance generation remain separate concepts.

## Verification, delivery and non-goals

Acceptance: all four findings have regression coverage; new realtime transports preserve HTTP
authorization outcomes; mixed versions and reconnect work; 15 minutes on an idle connected wire-5
tab generate zero periodic app-version, activity or call-stats HTTP requests. Offline/legacy connections must not trigger HTTP telemetry fallback or periodic manifest checks. A call still produces ten-second telemetry, and local UI stats still update.
Measure unrelated HTTP separately rather than hide it in a total-request assertion.

One integration branch, one PR with task commits, one complete release after final-head CI,
merged-main CI, CodeQL and official release checks pass. Independently review security/protocol
boundaries and the whole branch. Do not weaken tests to pass the gate. Deploy when execution is requested and existing deployment authorization covers it, with verified backup, official artifacts, current call guard and rollback compatibility
checks. Monitor 15 minutes; retain initial failures as well as any later clean window.

Non-goals: migrate chat writes/history/uploads to WSS, remove HTTP APIs, alter MLS cryptography,
add durable telemetry, replace call transport, introduce a service/broker, or clean unrelated UI work.
