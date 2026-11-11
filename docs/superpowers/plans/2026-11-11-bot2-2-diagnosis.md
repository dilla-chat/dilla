# Bot helper closure diagnosis: v0.1.0-bot2.1 (origin/main 09c7c28a)

Read-only. Line numbers refer to the signal-design worktree at 09c7c28a. I ran no tests. Every conclusion comes from reading the code against the CT 144 journal summary I was given. I did not see the raw journal.

## 0. Key deductions from "no `bot native helper stopped` line"

The ordinary ways a helper dies all go through `OutcomeUnknownError` and `Client.requestWithin`. That function logs once, with the operation, the error class and the exit:

- `internal/bot/helper/client.go:473-479`: `if errors.Is(err, ErrOutcomeUnknown) { c.abortCurrent(); c.logStopped(operation, err) }`.
- `internal/bot/helper/client_linux.go:178-236`. Every transport failure returns `unknown(err)`:
  - a per-call deadline (`CallTimeout` 15 s through `SetDeadline`, line 186-192);
  - **ctx cancellation** (the watcher closes the conn at line 196-203, then the read fails at line 211-224);
  - a write or read error, EOF or EPIPE (helper exited, stdin/socket EOF);
  - a bad frame length;
  - a response-id mismatch.
- The Logger is set on the only start site (`cmd/dillabot/main.go:553`, `Logger: logger.With(...)`), so `c.log` is never nil in production.

**So these did NOT close the client:** a timeout on a long call (group join or commit), ctx cancellation mid-call, the helper exiting or crashing on a request, or socket EOF. Each would have logged `bot native helper stopped` (confidence high).

A ctx cancelled *before* the call (in `acquire`) returns `context.Canceled` and closes nothing (`client.go:718-733`, `prioritygate`).

What remains are the **silent** closes:

| # | Path | Where | Logs? |
|---|------|-------|-------|
| S1 | Deliberate `native.Client.Close()` by the Bot's runtime through `musicRegistry.Disable → retire → finishNativeClose` | `cmd/dillabot/music_runtime.go:1271-1348` (`retained.native.Close()` at 1330) | no |
| S2 | `cleanupUnreadyInstallation`: Disable on any non-deferred pass error while not ready | `cmd/dillabot/rate_limit.go:23-27`, called `main.go:874` | no |
| S3 | `cleanupFailedMusicRestore`: Disable on a non-deferred music channel or restore error | `rate_limit.go:30-36`, called `main.go:713` | only "bot music queue restore paused" with error text |
| S4 | `retainedNativeClient`: Disable when `!Usable()` | `cmd/dillabot/feed_handlers.go:471-479` | no |
| S5 | Pass-local close of a new helper that was never retained | `main.go:581` (lifecycle_bind), `main.go:877` (`!retainedNative && !runtimeOwnedNative`) | no |
| S6 | Dropped or cancelled helper starts | `cmd/dillabot/helper_start.go:340-342`, `369-372` | no |
| S7 | Manager restart (`statuses.Observe` → `Drop` + `DisableAndWait`) | `main.go:506-515` | yes ("restarting on a manager's request") |
| S8 | `native.invalidateLocked`: a **mutation** whose result fails Go-side decode closes the helper | `internal/bot/native/client.go:172-177, 189-196` | no |
| S9 | `helper.abortCurrent` on groupApply transfer-validation failures (bad begin, offset ack, descriptor, chunk, digest, CBOR) | `internal/bot/helper/client.go:503-589` | no (no `logStopped` on these branches) |
| S10 | `helper.Close` gives up waiting for the gate after `stopTime` (2 s) and calls `abort()` | `client.go:745-753, 771-777` | no |

Two things amplify every one of these "under load":

- `prioritygate.Gate.Close()` (`internal/prioritygate/gate.go` `Close`) **fails every queued acquisition immediately** with `ErrClosed`. Both `native.Client.call` (`native/client.go:137-145`) and `helper.acquire` (`helper/client.go:722-725`) map that to `helper.ErrClosed`.
  - Every command, text poll or maintenance call queued behind the in-flight call gets "helper client is closed" at the moment of a close.
  - `native.call` then sets `usable=false` (`native/client.go:164-170`).
  - The burst therefore appears only when there is work.
- `maintainAs` **invents** the text: `if err == nil { err = helper.ErrClosed }` whenever `task.usable()` is false (`cmd/dillabot/background_pass.go:54-61, 125-127`).
  - The 10 s "helper client is closed" line from the due tick (`event_loop.go:282-286` → `bot scheduled work deferred`) therefore does not prove a call failed at that tick. It proves only that the registry still held a client whose `Usable()` was false.

## 1. What closes the helper client under load?

### Most likely cause (confidence: medium-high on the mechanism, medium on the exact first trigger)

**The pass started by the owner's Bot-channel change closes the retained helper, while the consumers wired to that helper keep using it.**

The owner's unmark and mark reach the feed (`feed_handlers.go:342-363`). That logs `bot left non-Bot channel group reason=unmarked` (07:54:25.213) and then `f.changed()` → `requestPass` (`feed_handlers.go:202-205`, `gateway_runtime.go:115`). The pass runs on the loop goroutine (`event_loop.go:139-154`):

1. `ReconcileOnce` forgets the unmarked group (`membership_reconcile.go:683-699`, which logs `bot forgot non-Bot channel group` at .281). That call succeeded.
2. Any **non-deferred** error later in this pass leads to a close:
   - **Before readiness:** `ensureErr` with `ready=false` → `cleanupUnreadyInstallation` → `musicRegistry.Disable` → `retire` → `finishNativeClose` → `native.Close()`. This is S2.
     - Examples: a dillad 409 or 5xx during the owner's change, `ErrMembershipUnproven`, a Welcome apply or ack error for the newly marked channel, `client.ErrProtocol`.
     - `IsDeferred(nil)` is false and only rate limits, `ErrNoGateway`, cooperative stop and ctx errors count as deferred (`internal/bot/client/retry.go:53-82`).
   - **After readiness:** a non-deferred music channel or restore error → `cleanupFailedMusicRestore` → Disable. This is S3.
3. The consumers the previous pass wired to that `*native.Client` are **not** detached:
   - the sealed intake's `cfg.helper` (`command_worker.go:109-131`, set only by `SetSealedIntake` at `main.go:814`);
   - the text poller (`main.go:820-823`);
   - the music, welcome, moderation and utility handlers captured in closures;
   - the feed's `core` (`gateways.feedFor`).
4. Until a later pass gets all the way to `SetSealedIntake` again, every one of them meets a closed helper:
   - text polls → `bot text command intake deferred` (`gateway_runtime.go:416-426`; the 9 lines);
   - `OpenCommand` → `bot command open failed`;
   - `SealReply` → `seal_error`.

### Why the due tick logs "closed" every 10 s

For the due tick to log "closed", the registry must **still hold** the closed client. After S1-S4, `Native()` returns nil, the `current` check fails (`main.go:825`, `background_pass.go:90-92`) and the task is dropped silently. Two code paths keep a closed client in the registry:

**(a) S3 followed by re-retention in the same pass (code-proven bug).**

1. `cleanupFailedMusicRestore` Disables, which closes the helper and deletes the registry entry, then returns `nil` for a non-deferred error (`rate_limit.go:30-36`). The pass continues.
2. `musicRegistry.RetainInteractions(..., nativeClient)` (`main.go:807`) finds no current entry. It **creates a new entry holding the closed client** (`music_runtime.go:905-929`). Nothing checks `Usable()`.
3. `SetSealedIntake` and `SetMaintenance` then publish the closed client as current, with `usable: nativeClient.Usable` returning false.
4. From then on, each due tick:
   - finds `current()` true and `usable()` false;
   - logs "helper client is closed";
   - calls `requestPassAfterHelperGone` (`background_pass.go:118-122, 175-204`).
5. The pass that follows does the respawn:
   - `retainedNativeClient` sees `!Usable` → Disable → `passHelper` starts a new helper → "bot native helper starting" (`main.go:534-566`).
   - The new helper's pass runs the same music restore. If the error recurs, the new helper is closed and re-retained the same way.

This loop runs for as long as the triggering condition lasts, which matches "as long as the Bot had work".

**(b) A silent self-close (S8 or S9) of the retained client, outside a pass.**

- Main suspect: the text poller (`membership_pass.go:48-61`). It runs `ReconcileOnce` with no `cleanupUnready` step. If `ApplyWelcomes` (the Welcome for the newly marked channel, a GroupApply over the transfer path) hits:
  - a decode failure (S8, `mutation=true`), or
  - a transfer-validation failure (S9),
  then the helper closes itself while staying in the registry.
- Every due tick after that sees `usable=false`, logs, and asks for a respawn pass.
- Each replacement re-fetches the same un-acknowledged Welcome and can die on it again.
- `OpenCommand` (op 22) is also a mutation with a strict decoder (`native/command.go:31-43`, `helper/client.go:240-276`). An answer shape the decoder rejects would silently kill the helper on every retry of the same sealed command.

**Respawn cadence.**
- `helperRespawnBase = dueInterval` (10 s), doubling to 5 min (`background_pass.go:158-161`).
- Only `helperServed` resets it (`background_pass.go:103-105, 207-211`), so a steady 10 s cadence of real respawns means each replacement served one due tick before dying again.
- If the journal shows the respawn passes at 10 s, 20 s, 40 s spacing, then the 10 s cadence is only the due tick's log line (see section 0), not respawns.

### Candidates ruled out or unlikely

| Candidate | Verdict |
|---|---|
| Per-call `CallTimeout` 15 s on a long join or commit | Ruled out: it would log `bot native helper stopped` with `error_class=timeout` |
| ctx cancellation from a pass-scoped ctx | Ruled out mid-call (logs). Before the call, it returns `context.Canceled` and closes nothing |
| Helper process exit, crash or stdin/socket EOF | Ruled out: logs, `error_class=helper_exited` |
| Concurrent use of the single-flight helper | Serialized by two gates (`native` gate capacity 1, `helper` gate capacity 1). Contention only queues. It never closes, except that a close fails the whole queue at once |
| Respawn code closing a client still in use by intake | **Yes**, this is the main mechanism. `retainedNativeClient` (S4) and `cleanupUnready`/`cleanupFailedMusicRestore` (S2/S3) close the client without detaching intake, the text poller or the handlers. AcquireNative borrowing (`music_runtime.go:950-977`) protects only callers that borrow, and the sealed intake, text poller and feed do not borrow |
| Retained client replaced while a call is in flight | `native.Close` waits for in-flight calls (`<-c.gate.Close()`, `native/client.go:434-444`) but fails queued ones. A replacement that a later pass publishes does not affect the in-flight call |

### What logging would prove it

- At 07:54:25 look for `bot installation reconciliation failed` with its `stage`, and `bot music queue restore paused`. Their absence would point to (b).
  - `stage=identity` or `membership_source_sync` → S2.
  - `bot music queue restore paused` → S3 + re-retain.
- Add a close-reason line (see fix F1). Each close then names its path (S1-S10), and the next "helper client is closed" can be matched to one.

## 2. Why does the failed music op's answer end in `seal_error`?

Confidence: high that it is the closed helper; medium on which close path.

- Error replies have no separate path:
  - `MusicInteractions` returns `Status: interactionStatusUnavailable` (5) with `reason=snapshot_handoff` (`internal/bot/service/music_interactions.go:519-529`).
  - The sealed dispatcher turns that into an `intake.Answer`, and `Intake.answer` → `sendLocked` → `seal` calls `i.helper.SealReply` exactly as for success (`internal/bot/intake/intake.go:617-656`).
- The command ctx has no deadline: `command_worker.go:386` gives a worker ctx from `r.ctx` only, and the intake and dispatcher add none. A ctx error is therefore not the cause.
- Op 23 is `mutation=false` (`native/command.go:46-58`). A decode failure there would not close anything. An outcome-unknown failure would have logged `bot native helper stopped`. A group or epoch mismatch is impossible because op 23 seals to the request's one-time reply key and touches no MLS group.
- What remains: `sealedEntry.SealReply` forwards to `cfg.helper` (`command_worker.go:125-131`). At 07:54:50 that was the client closed at about 07:54:25, which no successful pass had yet replaced through `SetSealedIntake`. The call returns `helper.ErrClosed` from the native gate or the `!usable||closed` check (`native/client.go:137-153`).
- `errSealedNotConfigured` is not possible here: `cfg.helper` was non-nil.
- The log discards the error: `log.WarnContext(ctx, "bot command answer not sealed", "class", "seal_error")` (`intake.go:647-651`). The command is then counted as answered (`command_worker.go:419-423`), so neither the client nor health sees it. The client times out and retries, and each retry is more work on the dead helper.
- The music handler at :46 did reach `provider_check`. It runs on the music runtime's own native reference: queues and registry borrow, and `startsPlay`/`OnPlay` go through the registry. The 403 is a separate YouTube issue and is not what closed the helper.

## 3. Why does the Bot-channel join have no completion line? What happens if the helper closes mid-join?

Confidence: high.

**Why no completion line.**
- `bot joining Bot channel group` is only the feed's *intent* line when a live mark arrives (`feed_handlers.go:342-363`). It calls `f.changed()` (request a pass) and nothing else.
- The join itself is not done by the Bot. dillad proposes the Bot in with an Add and serves a Welcome. `MembershipReconciler.reconcileOnce` (`internal/bot/service/membership_reconcile.go:316-404`) fetches it, applies it (`ApplyWelcomes`, a GroupApply), revalidates and acknowledges it.
- A successful text-Welcome apply has **no log line**. Only `bot welcome to a non-Bot channel group refused` and `bot channel group needs the Bot re-added: no Welcome to join it from` (`membership_reconcile.go:661-665`) exist.
- On failure, the pass logs `bot installation reconciliation failed stage=membership_source_sync membership_phase=welcome_apply|…` (`main.go:888-897`). The text poller logs only the class-less `bot text command intake deferred`.

**What happens if the helper closes mid-join.**
- `ApplyWelcomes` returns an error, and `reconcileOnce` returns a `MembershipPhaseError` before the cursor is saved or the Welcome acknowledged.
- The Welcome stays on dillad, and the next `ReconcileOnce` re-fetches and re-applies it. An already-applied Welcome answers outcome 1 and is acknowledged. **It is retried and not stuck**, provided:
  - a later pass gets a working helper, and
  - the Welcome has not expired on dillad.
- Retries are not otherwise driven:
  - The failed pass is retried on `instFail` backoff: 1 s doubling to 5 min (`event_loop.go:183-190`).
  - The text poller retries only on a gateway hint.
  - If the Welcome expired or was pruned, the reconciler keeps logging the `needs the Bot re-added` warning (once per channel, `membership_reconcile.go:661-665`) and stays out of the group until the channel is re-marked.
- In the S2 loop, the same pass that fails the join also closes the helper: `ready=false` → `cleanupUnready` → Disable. For a newly started helper it is closed at `main.go:877`. **Join failure and helper closure feed each other.** Each retry starts a new helper, fails on the same Welcome step, and closes it again.

## 4. Why did bot_health report OK 60 s after failures?

Confidence: high.

**What `dillad doctor` reads.** `doctorBotHealthLeg` (`cmd/dillad/doctor.go:206-233`) reads `<data_dir>/bot-health` through `api.ReadBotHealthFile`. An empty or missing file prints "no Bot's commands are failing".

**Who writes the file, and when.** Only the running dillad writes it, in `BotHealth.Record` → `writeLocked` (`internal/api/bot_health.go:163-179, 207-269`). It writes only when its in-memory judgement **first becomes degraded**, or is cleared by a passing probe. Degraded means one of:
- `probeFails >= 2`: two reports with distinct `probe_at`, both not ok;
- `OldestWaitMS > 10 000`;
- queue depth rising across every report for 60 s (`queueGrowing`).

**What the Bot reports.**
- `health.ProbePeriod = 60 s`, `ReportEvery = 15 s` (`internal/bot/health/health.go:17-23`).
- The probe (`cmd/dillabot/health_probe.go:94-146`) seals an OpProbe with `cfg.helper.SealProbe`, relays it and opens the answer. With the helper closed it fails at step "seal", but that is **one** failed probe.
- A second failure needs another ≥60 s, so the earliest "degraded" is about 60-120 s after onset. Doctor ran at 07:55, about 35 s after onset.
- If a replacement helper is wired in before the next probe, the probe passes and `probeFails` resets to 0 (`bot_health.go:231-238`). A helper that flaps on a ~10 s cycle can therefore **never** reach two consecutive failures.

**None of the observed failures counts as a failing command:**

| Observed failure | Why it is not counted |
|---|---|
| `bot text command intake deferred` | The text poller is not a command and touches no load counter |
| `seal_error` | `Handle` returns and the worker calls `e.load.answered` (`command_worker.go:419-423`): the command counts as *answered* |
| `http_403` / status 5 | A normal, sealed (or seal-failed) final answer; status codes are not reported |
| `bot command open failed` | Same as `seal_error`: answered at once |

Fast-failing commands lower `oldest_wait` and never grow the queue. Only the probe can catch this failure mode, and it is slow and resets on any success.

The Bot's own `health_state` file in its state dir (`cmd/dillabot/health_state.go`, written by the reporter at `health_probe.go:209-215`) is a separate view. dillad's doctor does not read it.

## 5. Minimal fix set

Each fix lists the files to touch and the test that pins it. TDD: write the test first, and it must fail on 09c7c28a.

**F1. Log a close reason on every helper and native close.**
- Files: `internal/bot/helper/client.go`, `internal/bot/native/client.go`, `cmd/dillabot/music_runtime.go`, `cmd/dillabot/helper_start.go`, `cmd/dillabot/main.go`, `cmd/dillabot/feed_handlers.go`.
- Changes:
  - Add `CloseWithReason(reason string)` on `helper.Client` and `native.Client`, with a fixed vocabulary: `pass_unready`, `music_restore_failed`, `retained_unusable`, `pass_unretained`, `lifecycle_bind`, `start_dropped`, `manager_restart`, `invalidated_decode`, `transfer_invalid`, `outcome_unknown`, `shutdown_timeout`, `shutdown`.
  - Log `bot native helper closed` once per client with `reason`, `operation` (when there is one), `error_class` and `exit`.
  - Add a log line to the silent `abortCurrent` branches (S9) and to `invalidateLocked` (S8), and to `abort()` from the `Close` timeout (S10).
  - Pass the reason through `musicRegistry.Disable(installation, reason)` → `finishNativeClose`.
- Tests: extend `internal/bot/helper/stopped_log_test.go` with each S8/S9/S10 path, asserting exactly one line with the reason. Add `native/client_test.go` for invalidate on a mutation decode failure. Add `cmd/dillabot` tests for each Disable caller's reason.

**F2. Do not close the retained helper on pass errors that are not about the helper.**
- Files: `cmd/dillabot/rate_limit.go` (`cleanupUnreadyInstallation`, `cleanupFailedMusicRestore`), `cmd/dillabot/main.go:713, 874`.
- Change: when the registry entry has `interactions` and the client is `Usable()`, call `pauseMusic` and keep the helper. Disable only when the helper is unusable or the installation is gone or disabled.
- Test: `cmd/dillabot/main_test.go` or `background_pass_test.go`. A pass whose `ReconcileOnce` (or `Session.Channels`) returns a non-deferred error leaves `musicRegistry.Native(...)` equal to the same usable client, and a sealed command answered right after still seals.

**F3. `RetainInteractions` and `ReconcileWithStarterFactory` refuse an unusable client.**
- Files: `cmd/dillabot/music_runtime.go:905-929`, `1143-1199`.
- Change: return an error when `source.Usable()` is false, so a closed client is never re-retained.
- Test: `music_runtime_test.go`. Disable, then `RetainInteractions` with the closed client: expect an error and a nil `Native()`.

**F4. Detach consumers when their helper is closed, and fail retryable instead of silently.**
- Files: `cmd/dillabot/command_worker.go` (`sealedEntry`), `gateway_runtime.go` (text poller and maintenance), `music_runtime.go` (`retire`).
- Changes:
  - On retire/close of installation X's helper, clear X's sealed `cfg.helper`, its text poller and its maintenance. Intake then answers `errSealedNotConfigured`, the retryable 503 `E_BOT_OFFLINE` path.
  - Alternatively, have `sealedEntry` re-resolve the helper from the registry on each call and refuse when it is `!Usable()`.
  - Have `retainedNativeClient` (S4) request the pass and mark intake offline in the same step.
- Test: `command_load_test.go` / `health_probe_test.go`. With the helper closed under the intake, a relayed command gets a sealed retryable refusal or a parked answer, never a dropped `seal_error`. The text poller is not run against a closed client.

**F5. Put the error class on the class-less lines.**
- Files: `internal/bot/intake/intake.go:386-390, 647-651`, `cmd/dillabot/gateway_runtime.go:424`, `cmd/dillabot/main.go:888-897`.
- Change: add `error_class=helper.Class(err)` (and `op` and `seq` for the seal line) to `bot command answer not sealed`, `bot command open failed`, `bot text command intake deferred` and `bot installation reconciliation failed`.
- Test: `intake_test.go`. A `SealReply` returning `helper.ErrClosed` logs `error_class=closed`. Similar tests for the other three lines.

**F6. Log the Bot-channel join outcome.**
- Files: `internal/bot/service/membership_reconcile.go:370-391`, `cmd/dillabot/feed_handlers.go`.
- Changes:
  - Log `bot joined Bot channel group` (`channel_id`) when a text-group Welcome is applied and acknowledged.
  - On failure, log `bot Bot channel join failed` with `phase` and `error_class`.
  - Optionally, have the feed keep a pending-join set and warn once if it is still unjoined after N passes.
- Test: `membership_reconcile_test.go` (apply success logs joined; apply error logs failed with phase `welcome_apply`) and `feed_test.go`.

**F7. Make health see failing commands.**
- Files: `cmd/dillabot/command_worker.go` / `health_probe.go`, `internal/bot/health/health.go`, `internal/api/bot_health.go`.
- Changes:
  - Count commands whose answer could not be sealed or delivered, or whose open failed, as failed.
  - Report a failure count (or set `probe_ok=0` and trigger an immediate probe) when any command failed in the period.
  - Probe right away when an installation's helper is closed or replaced, rather than waiting up to 60 s.
  - In dillad, judge degraded on failed commands in the last report, not only on two probes.
- Tests:
  - `health_probe_test.go`: a `seal_error` → the next report carries the failure and probes immediately.
  - `internal/api/bot_health_test.go`: one report with failures → degraded, and `bot-health` is written.
  - `cmd/dillad/doctor_test.go`: yellow.

**F8 (optional, small). Stop a due-tick log storm.**
- File: `background_pass.go:118-128`.
- Change: log "helper gone" once per client generation, plus respawn attempts, instead of every 10 s.
- Test: `background_pass_test.go`. Three ticks on the same unusable client give one log line and one respawn ask, following the backoff.

**Verification order.** Ship F1 and F5 first: they are pure logging and safe. One more CT 144 occurrence then shows which close path fires. F2, F3 and F4 fix the loop seen here whichever trigger it was. F6 and F7 close the observability gaps.
