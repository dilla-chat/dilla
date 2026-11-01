# Security and Realtime Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** Fix the four reviewed reliability/security issues and move version notices, activity and call telemetry onto the existing gateway, in one PR.

**Architecture:** Keep HTTP snapshots and recovery; remove web telemetry fallbacks and periodic version polling per Jonas’s later clarification. Keep native Bots working. Share acceptance services across transports; negotiate wire 5 for bounded ephemeral messages. Treat enforcement effects as durable work and recovery/cache changes as independent commits on the same branch.

**Tech Stack:** Existing Go, Rust protocol/core validation, TypeScript, CBOR, native state transactions, Go tests, Vitest and Playwright. No new infrastructure.

**Spec:** `docs/superpowers/specs/2026-10-31-security-realtime-design.md` (approved by Jonas’s “Go”).

## Global Constraints

- One implementation branch, one PR, one complete release; preserve unrelated dirty UI work.
- Preserve wire 4/3/2 and managed Bot wire 3, E2EE/media versions, MLS state and instance generation.
- Ordinary CBOR read budget 30 seconds; existing commit budget 60 seconds; existing blob budgets unchanged.
- Activity maximum once per five seconds/device; idle expiry ten minutes; stats client cadence ten seconds and shared server minimum five seconds.
- New frames 56/57/58 and wire 5 are proposed reservations: recheck allocation before editing.
- Cache and worker limits, unresolved-hint retry timing and confidentiality rules are those in the design.
- No deployment from a partial task or without applicable deployment authorization and all required gates; preserve standing authorization.

## Review Focus

- Lost acceptance/write replies and process death must not lose or duplicate a moderation action (task 2).
- Two queued membership-loss observations separated by a successful repair must not skip a peer message (task 3).
- Cache eviction during token rotation or while waiters exist must not replenish spent tokens (task 4).
- Queued telemetry after revocation, permission loss or call end must be refused; floods must not delay heartbeats (task 7).
- Legacy resume, missed build notices, background tabs and an active call must preserve update detection and drafts without new-frame failures (tasks 5/8).

## Task 1: Bound ordinary body reads

**Files:** `internal/server/codec.go`, `internal/api/commitguard.go`, `internal/api/api.go`, `internal/api/auth.go`; tests `internal/server/codec_test.go`, `internal/api/commitguard_internal_test.go`, `internal/dillad/wiring_test.go`.
**Interface:** Introduce a shared route body-deadline wrapper; common CBOR handlers receive a 30-second budget before reads. Existing specialized upload wrappers retain their budgets. Keep `ReadBody`'s byte-limit contract.

- [x] Port the isolated slow-body probe from `/home/thim/.cache/dilla-tmp/main-review-oct31/slowbody_review_test.go` into a real TCP regression, with an injected short test budget. Assert expiry while bytes are withheld, explicit timeout refusal, and connection cleanup; demonstrate red first.
- [x] Cover HTTP/1.1 keep-alive next request, HTTP/2 stream isolation, larger commit/blob budgets, unsupported deadline transports and WebSocket upgrades; implement the shared wrapper without a global write timeout.
- [x] Run `go test ./internal/server ./internal/api -run 'Body|Deadline|Commit|Upload|Gateway'` and the wiring tests. Require all targeted tests green; commit only task files.

## Task 2: Make moderation effects independently resumable

**Files:** `internal/bot/service/automod_runtime.go`, `moderation_cases.go`, `internal/bot/moderation/evaluator.go`, `internal/bot/service/automod_config_interactions.go`, `cmd/dillabot/automod_event_state.go`; create `internal/bot/service/automod_pending.go` and tests.
**Interfaces:** Add a native-state-backed pending-action store with `Load`, compare-and-set `Put`, and completion operations keyed by installation/message/revision. `Handle` checks it before evaluator dedup/replay suppression. Keep the sink's existing deterministic server operation identity and receipt validation. Add versioned, bounded actor-sharded evaluator checkpoint encode/restore operations; one native transaction commits affected rolling/dedup state, evaluation frontier and pending decision before ACK. Persist/reuse the configuration activation boundary across restart; seed once for old installations.

- [x] Port `automod_retry_review_test.go` from the same review evidence directory. Assert a transient first sink failure is retried on identical delivery and rolling evaluation counts the source once; demonstrate red first.
- [x] Add process reconstruction, lost server reply, accepted effect plus failed evidence/case write, lost native CAS reply, concurrent delivery, authorization revocation, expiry and pending-capacity tests. Include crash before pending Put, full capacity followed by restart, prior rolling observations needed for the next violating message, changed policy during retry, and one-time upgrade migration. Assert exactly one accepted effect and one case, or an explicit terminal refusal.
- [x] Implement atomic protected evaluation checkpoints/pending records before effects and journal ACK; restore them before ordinary replay classification. Reuse the durable policy activation boundary rather than a fresh process-start fence, and resume pending work with current authority/policy/window checks. At capacity defer before mutating evaluator state; after definitive expiry record the refusal instead of silently ACKing an ignored decision. Never persist message text in logs or unprotected storage; preserve existing startup replay safety for events without pending work.
- [x] Run `go test -race ./internal/bot/service ./internal/bot/moderation ./cmd/dillabot -run 'AutoModeration|ModerationCase|ModerationPending'`; commit with state-format compatibility notes.

## Task 3: Fence membership recovery on the group lane

**Files:** `packages/client-core/src/sync/engine.ts`, `channel.ts`, `internals.ts`; tests `engine.test.ts`, `channel.test.ts`, `e2e/web/membership.spec.ts`.
**Interface:** Track a local recovery-generation token per group. A queued loss-repair operation carries the observed token; repairing advances it. The original read is retried if its token became stale.

- [x] Write a deterministic test: moderation-feed 404, intervening proposal-driven successful repair, peer message, then delayed moderation repair. Require one external join and receipt of the peer message. Run red before changing recovery.
- [x] Implement the generation check inside the same lane that serializes apply/catch-up/commit. Cover repeated actual loss, failed repair, stopped client, two channels and cursor invariants; do not treat state 2 alone as proof that a prior 404 remains current.
- [x] Run `npm test -w @dilla/client-core -- src/sync/engine.test.ts src/sync/channel.test.ts`; run existing membership browser acceptance unchanged on Chromium and Firefox. Commit; if the candidate does not reproduce, include the disproving test/evidence rather than a speculative fix.

## Task 4: Bound limiter lifetime without quota resets

**Files:** `internal/bot/client/rate_limit.go`, `client.go`, `cmd/dillabot/main.go` (the `config.RateLimits` field), associated configuration/doctor tests; new `internal/bot/client/rate_cache.go` and tests.
**Interface:** Cache capacity configuration and active-identity lifecycle cleanup; `SessionForDevice` preserves the existing shared budget contract. Cache admission can return retry-safe capacity deferral before allocation.

- [x] Add small-capacity tests for device churn, per-target churn, global target capacity, idle/full eviction, active waiter pinning, rotation, clock changes and concurrent cleanup. Assert strict map bounds and no fresh burst after eviction of a partially spent identity; demonstrate current growth red.
- [x] Implement the spec defaults and configurable limits, safe idle eviction and backpressure when every entry is pinned/non-full. Preserve continuity for both old and new session objects during rotation.
- [x] Run `go test -race ./internal/bot/client ./cmd/dillabot -run 'Rate|Limiter|Config|Doctor'`; check public metrics contain no tokens/user IDs; commit.

## Task 5: Negotiate realtime extension and push web builds

**Files:** `protocol/02-delivery-service.md`, `protocol/07-versioning.md`, `protocol/09-http-api.md`; `internal/gateway/frame.go`, `codec.go`, `session.go`, `gateway.go`, `internal/dillad/server.go`, `internal/web/web.go`; `packages/client-core/src/gateway/{frames,gateway}.ts`, worker protocol/controller, `packages/web/src/update.ts`, `screens/Shell.tsx`; relevant Rust/version gates found by repository-wide search.
**Interfaces:** Propose `Handler.BuildEntry() string`, a gateway `CurrentAppEntry func() string` provider, and worker-to-page `appBuild` notification. App-build frame 56 is `[entry]`, ephemeral, sent only after negotiated wire-5 ready/resume and when the served handler changes.

- [x] Audit all wire/version/opcode validators and reserve version 5 plus slots 56–58 consistently. Write mixed-version tests that fail before implementation: old client/new server, new client/old server, legacy resume under a new frontend, managed Bot wire 3.
- [x] Expose the validated served entry, wire it once in the composition root, and send a fresh app-build notice after identify/resume without changing ready/resumed arrays or replay sequence numbers. No raw filesystem watcher or notice for staged files.
- [x] Implement deduplicated manifest verification and the existing update prompt. Remove periodic version HTTP checks in every connection state. Test startup, pre-login, reconnect, repeated/malformed notices, tab visibility, proxy/socket outage, active call, drafts and rollback; apply bounded retries only for unresolved new hints.
- [x] Run `go test ./internal/gateway ./internal/web ./internal/dillad`, gateway/update Vitest suites and existing `e2e/web/version-reload.spec.ts` / `e2e/web/z/reload.spec.ts`. Commit protocol documentation with implementation.

## Task 6: Move real user activity onto WSS

**Files:** `internal/gateway/presence.go`, new `activity.go`, `internal/api/presence.go`, worker/controller and gateway client files, actual input-activity producer located from `presenceActivity` callers; presence/AFK tests.
**Interfaces:** Frame 57 has empty payload; existing connection-derived identity enters one shared activity acceptance service/budget from both HTTP and WSS. Expose a typed gateway client activity method, not arbitrary frame sending from the page.

- [x] Write tests for authentic input, no input with repeating heartbeats, five-second coalescing/shared cross-transport budget, idle expiry, DND/invisible, AFK, reconnect, multiple tabs/devices, revoked and managed-Bot sessions.
- [x] Implement empty-payload validation and current-session gates; keep heartbeat liveness separate. Send through WSS when wire 5 is ready; drop unavailable input with no web HTTP fallback and never replay stale input after reconnect.
- [x] Run gateway/presence/AFK Go tests, client worker/gateway tests and presence browser tests; assert zero activity HTTP requests on the healthy wire-5 path. Commit.

## Task 7: Share call-stats authority and add bounded telemetry

**Files:** `internal/api/callstats.go`, `calls.go`, new shared acceptance helper/tests; new `internal/gateway/telemetry.go` and tests; composition root, `packages/client-core/src/calls/controller.ts`, gateway/worker files, `packages/web/src/call/session.ts`.
**Interfaces:** Define `gateway.CallStatsReport` matching the current report tuple and an `AcceptCallStats(context.Context, auth.Session, id.ID, gateway.CallStatsReport) error` adapter shared with HTTP. Frame 58 carries call ID/report; authenticated identity is never client-supplied. Inject the callback rather than import API handlers into the gateway.

- [x] Extract existing HTTP acceptance without weakening any gate. Add HTTP/WSS parity tests for ended call, wrong device/call, absent leaf, missing connect, barred/revoked/factor-owed session, malformed report and the shared five-second allowance, including simultaneous cross-transport admission.
- [x] Implement the spec's bounded pool, per-device in-flight/latest slots and 1-KiB shape cap. Test saturation, slow repository, disconnect/revocation while queued, call end while queued, cancellation and cleanup; heartbeats and commit ACKs must remain responsive.
- [x] Send the ten-second sample through the typed client path with the 64-KiB bufferedAmount guard. Drop stale samples, avoid dual-send after ambiguity, drop unavailable samples without web HTTP fallback and preserve local-only UI stats. Test reconnect, hidden live call and slow socket with a fake clock.
- [x] Run API/gateway/call Go race tests and calls/worker/gateway/web stats tests; run voice acceptance on Chromium/Firefox; commit.

## Task 8: Whole-PR acceptance and delivery

**Files:** create `e2e/web/realtime-transport.spec.ts`; modify `e2e/playwright.web.config.ts` to include it in WebKit's explicit testMatch; extend compatible core/voice fixtures, protocol and deploy docs and CI area selection where needed.
**Interfaces:** Consume all prior tasks; no new transport or policy behavior.

- [x] Build a timer/request inventory for app versions, presence input, call reports, Activity inbox and Bot command-status refreshes. Record retained HTTP purposes explicitly; do not bundle an unreviewed encrypted-inbox redesign.
- [x] Add 15-minute virtual-time request assertions: zero recurring version/activity/stats HTTP with a ready wire-5 socket, expected WSS cadence and bounds, and no web HTTP fallback or periodic polling on legacy/offline. Exercise duplicate/dropped notices, reconnect storms, resumed old sessions and a live call during upgrade.
- [ ] Run full required Go/Rust/TypeScript checks and the real three-browser web / two-browser voice suites. Run focused fault injection as well as existing acceptance; no skipped regressions, reduced assertions or timeout inflation to hide defects.
- [ ] Obtain independent security/protocol and whole-branch review. Open **one PR** for the entire implementation, with results and limitations in its description. Rebase/re-run affected checks if main changes.
- [ ] After authorized merge, require final PR checks, merged-main CI, CodeQL and an official complete release. Delete the merged feature branch remotely/locally after detaching worktrees. When execution is requested, honor applicable standing deployment authorization and deploy with verified backups, call guard and compatible rollback; retain initial failures and complete 15-minute monitoring. Update homelab docs and offer a focused wiki save.

## Plan self-review

All four review items and three WSS migrations map to tasks. Failure-mode coverage maps to tasks 2,
3, 4, 5/8 and 7 respectively. One PR overrides any default preference for separate subsystem PRs.
Protocol/service names above are proposed interfaces, not existing APIs. Implementation must verify
actual filenames/version allocations after rebase, and must not change this plan's authority or
compatibility requirements silently. Jonas approved implementation with “Go”.

Builder review completed: the restart/capacity gap was resolved with atomic evaluator checkpoints and a durable policy activation boundary; the reviewer found no remaining consequential question. Self-review corrected the configuration owner, sync interface path and explicit WebKit inclusion.
