# v0.1.0-bot2.2: Bot helper lifecycle, answer delivery and health Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The Bot keeps answering after a pass fails for a reason that has nothing to do with its helper. Every helper close logs why. A failed answer reaches the user or is retried. `dillad doctor` sees failing commands. dilla.db stops growing with free pages.

**Architecture:** The Bot keeps one native helper (`*native.Client`) per installation in `musicRegistry`. Passes, the sealed intake, the text poller and the due tick all use it. On bot2.1 a pass error closes that helper (`cleanupUnreadyInstallation` / `cleanupFailedMusicRestore` → `musicRegistry.Disable`). The same pass can re-register the closed client (`RetainInteractions`), and the intake, poller and handlers keep using it. The fix keeps a usable helper on errors that are not about the helper. It refuses to register an unusable client, and it makes every consumer re-resolve the current helper (or refuse retryably) instead of holding a stale pointer. Logging and health changes make the next fault visible within seconds.

**Tech Stack:** Go 1.25 (`cmd/dillabot`, `cmd/dillad`, `internal/bot/...`, `internal/api`, `internal/store/sqlite`), log/slog, SQLite (modernc).

**Spec:** No new spec. The evidence is the diagnosis at `.superpowers/sdd/2026-11-10-ci-red.md` (section "2026-11-11"), plus the diagnosis report copied to `docs/superpowers/plans/2026-11-11-bot2-2-diagnosis.md`. Bot v2 behaviour stays as `docs/superpowers/specs/2026-11-06-bot-v2-design.md` defines it. Rulings made without a spec are provisional.

## Global Constraints

- Never log message content, command arguments, keys or sealed bytes. Logs carry only ids, op numbers, stages, `error_class` and reasons.
- `error_class` values come only from `helper.Class(err)` (internal/bot/helper/class.go).
- Close reasons come from one fixed vocabulary in `internal/bot/helper`: `pass_unready`, `music_restore_failed`, `retained_unusable`, `pass_unretained`, `lifecycle_bind`, `start_dropped`, `manager_restart`, `invalidated_decode`, `transfer_invalid`, `outcome_unknown`, `shutdown_timeout`, `shutdown`, `installation_gone`.
- User-facing strings never say "encrypted".
- No wire version or schema change. `dillad` and `dillabot` from this release interoperate with bot2/bot2.1 peers.
- Go tests use a short `TMPDIR` (`/tmp/tNN`) and clean it up. Run `npm run ci:local` before pushing.
- Commit trailer: `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>` and `Claude-Session: https://claude.ai/code/session_012Nny5K2B969mk49QX3XSXG`.

## Review Focus

1. The owner unmarks and then marks a Bot channel while commands are queued. The Bot keeps answering, joins the channel, and logs the join outcome.
2. The helper really dies (process exit) while a command is waiting to be sealed. The user gets a retryable "Bot is having trouble" answer within the client's retry window, not silence, and the next pass replaces the helper once (not every 10 s).
3. A music restore fails with a provider error (YouTube 403). The helper stays up, music pauses, and other commands still answer.
4. A flapping helper (closes every ~10 s) makes `dillad doctor` show `bot_health` WARN within one report period (≤15 s), even though probes on the replacement pass.
5. dillad starts on a 197 MiB `auto_vacuum=0` database. It logs `compacting the database` with page counts before and after, finishes before serving, and a second start does not compact again.

---

### Task 1: Close reasons and error classes in the logs (F1 + F5)

**Files:**
- Modify: `internal/bot/helper/client.go` (Close, abortCurrent branches at ~503-589, Close timeout at ~745-777), `internal/bot/helper/class.go` (reason constants)
- Modify: `internal/bot/native/client.go` (Close, invalidateLocked ~172-196)
- Modify: `cmd/dillabot/music_runtime.go` (Disable/retire/finishNativeClose ~1271-1348), `cmd/dillabot/rate_limit.go`, `cmd/dillabot/main.go` (~506-515, 581, 713, 807, 874-897), `cmd/dillabot/feed_handlers.go` (~471-479), `cmd/dillabot/helper_start.go` (~340-372)
- Modify: `internal/bot/intake/intake.go` (~386-390, 647-651), `cmd/dillabot/gateway_runtime.go` (~416-426)
- Test: `internal/bot/helper/stopped_log_test.go`, `internal/bot/native/client_test.go`, `cmd/dillabot/music_runtime_test.go`, `internal/bot/intake/intake_test.go`

**Interfaces:**
- Produces: `func (c *helper.Client) CloseWithReason(reason string) error` and `func (c *native.Client) CloseWithReason(reason string) error`. `Close()` keeps working as `CloseWithReason(helper.ReasonShutdown)`.
- Produces: reason constants `helper.ReasonPassUnready` … `helper.ReasonInstallationGone` (exact strings from Global Constraints).
- Produces: `musicRegistry.Disable(installation id.ID, reason string)`; every caller passes its reason.
- Produces: one line per client lifetime, `bot native helper closed`, with `reason`, `operation` (when a call triggered it), `error_class` (when an error did), `exit`. The existing `bot native helper stopped` line becomes this line with `reason=outcome_unknown`.

- [ ] **Step 1: Write failing tests**
  - `TestCloseLogsReasonOnce`: CloseWithReason(ReasonPassUnready) twice → exactly one `bot native helper closed` record with `reason=pass_unready`.
  - `TestTransferInvalidLogsClose`: drive one abortCurrent transfer-validation branch (bad digest) → one record with `reason=transfer_invalid` and `operation=group_apply`.
  - `TestInvalidateOnDecodeLogsClose` (native): a mutation whose result fails decode → one record with `reason=invalidated_decode`.
  - `TestCloseTimeoutLogsShutdownTimeout`: a call holds the gate past stopTime → `reason=shutdown_timeout`.
  - `TestDisablePassesReason` (cmd/dillabot): `Disable(inst, helper.ReasonMusicRestoreFailed)` → the record carries that reason and `installation_id`.
  - `TestAnswerNotSealedLogsClass` (intake): SealReply returns `helper.ErrClosed` → `bot command answer not sealed` with `error_class=closed`, `op` and `seq`.
  - The same check for `bot command open failed`, `bot text command intake deferred` and `bot installation reconciliation failed` (each carries `error_class`).
- [ ] **Step 2: Run them and confirm they fail** — `TMPDIR=/tmp/t51 go test ./internal/bot/helper/ ./internal/bot/native/ ./internal/bot/intake/ ./cmd/dillabot/ -run 'Close|Reason|Class|Sealed' -count=1`
- [ ] **Step 3: Implement.** Use a `sync.Once` per client for the record. The reason is the first one given. Replace every bare `Close()`/`Disable(inst)` call in cmd/dillabot with the reason that fits the call site (S1-S10 in the diagnosis report).
- [ ] **Step 4: Run the package tests in full and confirm they pass.**
- [ ] **Step 5: Commit** `fix(bot): log why every native helper closes, and the error class on intake lines`

### Task 2: Keep a usable helper; never register a closed one (F2 + F3)

**Files:**
- Modify: `cmd/dillabot/rate_limit.go` (`cleanupUnreadyInstallation` ~23-27, `cleanupFailedMusicRestore` ~30-36)
- Modify: `cmd/dillabot/music_runtime.go` (`RetainInteractions` ~905-929, `ReconcileWithStarterFactory` ~1143-1199)
- Modify: `cmd/dillabot/main.go` (~713, 807, 874)
- Test: `cmd/dillabot/music_runtime_test.go`, `cmd/dillabot/main_test.go` (or the existing pass test file)

**Interfaces:**
- Consumes: `CloseWithReason` and the reasons (Task 1).
- Produces: `RetainInteractions(...)` returns `errHelperUnusable` (new, `cmd/dillabot`) when `source.Usable()` is false, and registers nothing.
- Produces: `cleanupUnreadyInstallation` / `cleanupFailedMusicRestore` keep the retained helper when it is `Usable()` and the installation still exists and is enabled. They pause music (`pauseMusic`) instead of closing. They close only an unusable helper (`retained_unusable`) or the helper of a gone installation (`installation_gone`).

- [ ] **Step 1: Write failing tests**
  - `TestRetainRefusesClosedClient`: Disable, then RetainInteractions with the closed client → `errors.Is(err, errHelperUnusable)` and `Native(inst) == nil`.
  - `TestPassErrorKeepsUsableHelper`: a pass whose membership reconcile returns a non-deferred error (a 409 from the fake API) leaves `musicRegistry.Native(inst)` the same usable client. A sealed command relayed right after is answered and sealed.
  - `TestMusicRestoreErrorKeepsHelper`: the music restore returns `http_403` → helper same and usable, music paused, `bot music queue restore paused` logged once.
  - `TestPassErrorClosesUnusableHelper`: the same pass error with `Usable()==false` → closed with `reason=retained_unusable` and replaced by the next pass.
- [ ] **Step 2: Run them and confirm they fail.**
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run `go test ./cmd/dillabot/ -count=1` and confirm it passes.**
- [ ] **Step 5: Commit** `fix(bot): a pass error no longer closes a working helper, and a closed helper is never registered again`

### Task 3: Consumers follow the current helper; a dead one gives a retryable answer (F4 + F8)

**Files:**
- Modify: `cmd/dillabot/command_worker.go` (`sealedEntry` ~109-131, worker ~386-423)
- Modify: `cmd/dillabot/gateway_runtime.go` (text poller, maintenance wiring)
- Modify: `cmd/dillabot/background_pass.go` (~118-128)
- Modify: `cmd/dillabot/feed_handlers.go` (`retainedNativeClient` ~471-479)
- Test: `cmd/dillabot/command_load_test.go`, `cmd/dillabot/background_pass_test.go`

**Interfaces:**
- Consumes: `musicRegistry.Native(inst)` and `errHelperUnusable` (Task 2).
- Produces: `sealedEntry` resolves the helper from `musicRegistry.Native(inst)` on each Open/Seal call instead of holding `cfg.helper`. A nil or `!Usable()` helper returns `errSealedNotConfigured`, the existing retryable `E_BOT_OFFLINE` path. The text poller and the health probe resolve the same way, and skip rather than call a closed client.
- Produces: if sealing fails with a helper error after the command was opened, the worker parks the answer and retries it once on the replacement helper. If the reply key is no longer valid, it answers `E_BOT_OFFLINE`. A `seal_error` never ends silently.
- Produces: the due tick logs `bot native helper gone` once per client (keyed by the client pointer), with `installation_id`, and logs each respawn ask. It stops writing the invented `helper client is closed` error every 10 s.

- [ ] **Step 1: Write failing tests**
  - `TestSealedIntakeFollowsReplacement`: replace the registry's client between Open and Seal → the answer is sealed with the replacement. No `seal_error`.
  - `TestSealedIntakeOfflineWhenHelperClosed`: the registry has no usable client → the relayed command gets the retryable offline refusal and the load counter records a failure (see Task 4).
  - `TestTextPollerSkipsClosedHelper`: the poller does not call a closed client and logs nothing per tick.
  - `TestDueTickLogsGoneOncePerClient`: three ticks on the same unusable client → one `bot native helper gone` line, and respawn asks follow the backoff (10 s, 20 s …).
  - `TestBackoffNotResetByShortLivedReplacement`: a replacement serves one due tick, then dies → the next ask waits 20 s, not 10 s. `helperServed` resets the backoff only once the client has been usable for ≥60 s (CT 144 showed 10/10, 20/10, 40/30 spacing).
  - `TestSelfClosingHelperOnPendingWelcome`: each new helper closes itself (invalidated_decode) applying the same pending Welcome → the close is logged with its reason each time, the respawns back off, and sealed commands get the retryable offline answer in between, never silence.
- [ ] **Step 2: Run them and confirm they fail.**
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run `go test ./cmd/dillabot/ ./internal/bot/... -count=1` and confirm it passes.**
- [ ] **Step 5: Commit** `fix(bot): commands follow the current helper, and a dead helper answers "try again" instead of nothing`

### Task 4: Join outcome logs and health that sees failing commands (F6 + F7)

**Files:**
- Modify: `internal/bot/service/membership_reconcile.go` (~370-391, 661-665)
- Modify: `cmd/dillabot/command_worker.go`, `cmd/dillabot/health_probe.go`, `internal/bot/health/health.go`
- Modify: `internal/api/bot_health.go` (~163-269), `cmd/dillad/doctor.go` (~206-233)
- Test: `internal/bot/service/membership_reconcile_test.go`, `cmd/dillabot/health_probe_test.go`, `internal/api/bot_health_test.go`, `cmd/dillad/doctor_test.go`

**Interfaces:**
- Produces: `bot joined Bot channel group` (`channel_id`) after a text-group Welcome is applied and acknowledged. `bot Bot channel join failed` (`channel_id`, `phase`, `error_class`) on failure.
- Produces: the health report gains `failed_commands uint32` (an additive JSON field, which older dillad ignores). It counts open failures, seal or delivery failures and offline refusals in the period. When the installation's helper is closed or replaced, the Bot probes at once instead of waiting up to 60 s.
- Produces: dillad judges an installation degraded when one report has `failed_commands > 0`, or on the existing rules. It clears only after a report with zero failures and a passing probe. `bot-health` is written on the change. The doctor WARN text names the count and points at `journalctl -u dilla`.

- [ ] **Step 1: Write failing tests**
  - `TestWelcomeApplyLogsJoined` / `TestWelcomeApplyFailureLogsJoinFailed` (phase `welcome_apply`, `error_class`).
  - `TestSealFailureCountsAsFailed`: a seal_error → the next report has `failed_commands=1`.
  - `TestHelperChangeProbesAtOnce`: replacing the helper triggers a probe within one fake-clock tick.
  - `TestOneFailedReportIsDegraded` (api): one report with `failed_commands=2` → degraded and the file is written; a later clean report with a passing probe clears it.
  - `TestDoctorWarnsOnFailedCommands` (dillad).
- [ ] **Step 2: Run them and confirm they fail.**
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run the four packages' tests and confirm they pass.**
- [ ] **Step 5: Commit** `fix(bot): log Bot channel join outcomes; health and doctor count failing commands`

### Task 5: dilla.db reclaims free pages (SQLite only)

**Files:**
- Modify: `internal/store/sqlite/open.go` (OpenWrite, WantPragmas) and the dillad start path that calls it
- Create: `internal/store/sqlite/vacuum.go`
- Test: `internal/store/sqlite/vacuum_test.go`

**Interfaces:**
- Produces: `func EnsureIncrementalVacuum(ctx context.Context, db *sql.DB, log *slog.Logger) error`. When `PRAGMA auto_vacuum` is 0 it logs `compacting the database` with `page_count`/`freelist_count` before, sets `auto_vacuum=INCREMENTAL`, runs `VACUUM` (outside any transaction, on a single dedicated connection), then logs `database compacted` with the counts after and the duration. When the value is already 2 it does nothing. dillad calls it after migrations and before it serves anything.
- Produces: `func IncrementalVacuum(ctx context.Context, db *sql.DB, pages int) (freed int, err error)`. dillad runs it hourly with 1000 pages and logs only when it frees pages.
- New databases are created with `auto_vacuum=INCREMENTAL` (set before the first table).

- [ ] **Step 1: Write failing tests**
  - `TestEnsureIncrementalVacuumConverts`: an `auto_vacuum=0` DB with deleted rows → afterwards auto_vacuum=2, freelist_count==0, data intact, two log lines with counts.
  - `TestEnsureIncrementalVacuumIdempotent`: second call → no VACUUM, no log.
  - `TestIncrementalVacuumFrees`: delete rows on an incremental DB → `IncrementalVacuum` frees >0 pages.
  - `TestNewDatabaseIsIncremental`: OpenWrite on a new path → auto_vacuum=2.
- [ ] **Step 2: Run them and confirm they fail.**
- [ ] **Step 3: Implement.** Postgres is untouched.
- [ ] **Step 4: Run `go test ./internal/store/... ./cmd/dillad/ -count=1` and confirm it passes.**
- [ ] **Step 5: Commit** `fix(store): dilla.db reclaims free pages (auto_vacuum=INCREMENTAL, one compaction at start)`

### Task 6: yt-dlp pin (only if the CT 144 A/B test shows the newer version plays) and release notes

**Files:**
- Modify (only on a passing A/B): `experiments/bot-providers/youtube-requirements.txt` (yt-dlp 2026.10.26 with wheel hashes, yt-dlp-ejs 0.8.0 unchanged), `docs/deploy/README.md` (~859-883), the doctor's checked version in `cmd/dillabot/doctor.go`
- Create: `docs/deploy/releases/v0.1.0-bot2.2.md` (bot2.1 notes are the template; "What's fixed" in plain words, the upgrade steps, the operators section with the new log lines `bot native helper closed reason=…`, `bot joined Bot channel group`, `compacting the database`, and a note that the first start may take a while to compact)

- [ ] **Step 1:** Apply the pin change only if the ledger records the A/B result as "old 403, new plays". Otherwise leave the pin and say in the notes that YouTube's 403 is outside the Bot.
- [ ] **Step 2:** Write the release notes.
- [ ] **Step 3: Commit** `docs(release): v0.1.0-bot2.2 notes`

### Task 8: Voice takeover — the device that joins last keeps voice (server)

Jonas, 2026-11-11: "the last joined client should take over voice - just like discord". He chose: server in bot2.2, client notice in the next desktop batch; cut A only once B is connected; A shows a calm notice (desktop batch).

**Files:**
- Modify: `internal/api/calls.go` (`Calls.Admitted` ~1350), `internal/api/afkaway.go` (`cutUserEverywhere` ~879 as the template, retry queue ~816-865, `latestAdmitSeqLocked` ~395), `internal/api/afk.go` (pending reasons ~48-49)
- Modify: `protocol/02-delivery-service.md` (AFK snapshot pending reason values), `protocol/09-*.md` (the wire 6 change log: a new value of an existing field, no bump), `protocol/07-versioning.md` entry
- Test: `internal/api/afkaway_test.go` / `calls_test.go`

**Interfaces:**
- Produces: AFK pending reason `afkPendingTakenOver = 3` (beside 1 swept, 2 chose), served in the same snapshot field. Old clients see an unknown reason and fall back to the generic "removed from voice", which is acceptable until the desktop batch.
- Produces: `func (h *Calls) takeOverVoiceLocked(user, newDev id.ID)` (or the existing lock discipline's equivalent). Called from `Admitted` for the admitted device. For every OTHER device of the same user that is a participant in ANY room, it sets pending reason 3, `RemoveParticipants`, `revokeRelay`, and reuses the cut retry queue. It never touches the admitting device or another user.
- Tiebreak: two of a user's devices admitted close together must not cut each other. Only the device with the latest admission sequence (`latestAdmitSeqLocked`) performs the cut. A cut queued for a device that is itself admitted later is dropped.
- Bots and shadow identities (`id#...`) are excluded.
- The removed device's MLS leave goes through the normal participant_left → proposeLeave path, with no new MLS code.

- [ ] **Step 1: Write failing tests**
  - `TestAdmitTakesOverOtherDevice`: user U in room R1 on device A; B admitted in R2 → A removed from R1 via the SFU fake, A's pending reason == 3, B untouched.
  - `TestAdmitSameRoomTakesOver`: A and B in the same room → A removed.
  - `TestTakeoverTiebreak`: A admitted, B admitted, then a late Admitted webhook for A → B is NOT cut (A's admission seq is older), and A is cut exactly once.
  - `TestTakeoverOtherUsersUntouched` and `TestTakeoverSkipsShadowIdentity`.
  - `TestTakeoverRetriesFailedRemove`: the SFU RemoveParticipants fails once → retried through the queue.
  - `TestTakeoverReasonClearedOnReadmit`: A rejoins → its pending reason cleared (existing behaviour, asserted for reason 3).
- [ ] **Step 2: Run them and confirm they fail.**
- [ ] **Step 3: Implement, and update the protocol docs.**
- [ ] **Step 4: Run `go test ./internal/api/ -count=1` and confirm it passes.**
- [ ] **Step 5: Commit** `feat(voice): joining voice on another device moves you there (server)`

Release notes (Task 6) gain a line: "Joining voice on a second device now moves you there; the first device leaves voice. Until the next desktop app, that device says you were removed from voice."

### Task 7: Music — failed tracks leave the queue, starts survive a fresh Remove, every line names its queue item

Evidence: `docs/superpowers/plans/2026-11-11-bot2-2-music-diagnosis.md`. Jonas, 2026-11-11: a new /play always goes last in the queue (unchanged). "Failed track is removed": a track that fails to start, or stops on an error, is dropped and the Bot says so. A track the user paused stays and resumes as today.

**Files:**
- Modify: `internal/bot/service/music_interactions.go` (~411 item ids, ~519-563 failure → answer), `internal/bot/music/supervisor.go` (~369, ~692-714 end handling), `internal/bot/music/queue.go` (~141-148, ~248), `internal/bot/music/persistence/store.go` (~137), `internal/bot/music/start_stage.go`, `internal/bot/music/session/session.go` (~149-182), `internal/bot/music/livekit/pool.go` (~37, 276-283, 750-805), `cmd/dillabot/music_call_guards.go` (~52), `cmd/dillabot/music_runtime.go` (~491)
- Test: the matching `_test.go` files in those packages

**Interfaces / behaviour:**
- Logging (F1): every `bot music playback start*`, `bot music playback ended` and `bot music operation refused` line carries `item_id` (the queue item's existing random `id.New()`, never derived from content) and `restored=true|false` (whether the item was loaded from storage). Read these from the context, because session.go replaces the context logger. Each failed catch-up check logs `bot music call group check failed` with `attempt`, `error_class` and `stage`. A `media_epoch` failure adds `epoch_reason` from a fixed set (`leaf_missing`, `epoch_stale`, `group_unknown`). The URL, video id, title and query are never logged.
- Failed track removed (F2): when a start fails, or playback ends with any class other than `eof` or a user stop or pause, the current item is removed from the queue and from persistence. The answer, or a channel notice if no command is waiting, says the track could not be played and was removed. If more items are queued, the next one starts. A user-paused item is untouched. A new /play still appends last.
- Start retry (F3): on a `media_epoch` binding error (the Bot's leaf is missing from the roster), the start rejoins the call group once (a forced rejoin and catch-up) and retries the handoff once. If it fails again, F2 applies.
- Catch-up on the current helper (F4): the pooled monitor resolves the installation's current native client and session on each check, the same way as Task 3. A closed or unusable helper ends the track at once with `error_class=closed`, instead of after three checks.

- [ ] **Step 1: Write failing tests**
  - `TestPlaybackLinesCarryItemID`: start, ended and refused lines carry `item_id` (equal to the queue item's id) and `restored`. No line contains the track URL or title (assert on a fixture URL and title).
  - `TestFailedStartRemovesItem`: the provider returns http_403 → the item is gone from the queue and from persistence, the answer says it was removed, and the next queued item starts.
  - `TestErrorEndRemovesItemButUserPauseKeeps`: ended with ctx_canceled or catch-up failure → removed. User /pause → kept and resumable.
  - `TestPlayAfterFailureStartsNewTrack`: /play A fails; /play B → B plays (not A), and the queue does not contain A.
  - `TestPlayAppendsLastWhenPausedQueue`: paused current X and queued Y; /play Z → order X(paused), Y, Z. This pins Jonas's rule.
  - `TestMediaEpochRetriesAfterRejoin`: the first handoff fails leaf_missing → one rejoin, retry succeeds. Both failing → removed once, no third try.
  - `TestCatchUpClosedHelperEndsAtOnce`: the current helper is closed → the track ends at the first check with error_class=closed.
  - `TestCatchUpCheckFailureLogged`: each failed check logs attempt, error_class and stage.
- [ ] **Step 2: Run them and confirm they fail.**
- [ ] **Step 3: Implement.** User-facing strings must never say "encrypted".
- [ ] **Step 4: Run `go test ./internal/bot/... ./cmd/dillabot/ -count=1` and confirm it passes.**
- [ ] **Step 5: Commit** `fix(bot): failed tracks leave the queue, music starts survive a fresh Remove, and music logs name the queue item`
