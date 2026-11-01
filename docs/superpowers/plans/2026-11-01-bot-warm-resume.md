# Bot warm pause/resume and start latency (v0.1.0-signal1.4.2) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Pause then play on the music Bot continues the same track from the same second in well under 1 s, and a cold start loses its avoidable seconds.

**Architecture:**
- Pause gates the live session instead of closing it: the sandbox, encoder, publication and room stay up, and silent Opus frames keep the timeline running.
- A paused session is bounded at 10 minutes and then closed. A later resume cold-starts at the paused position (YouTube) or from the start (Spotify).
- Explicit starts run at interactive priority, Enqueue stops failing on a stale client revision, and the music join's trust refresh takes the per-user claim.

**Tech Stack:** Go (internal/bot, cmd/dillabot), Python sandbox worker, TypeScript client-core.

**Spec:** the root-cause report `.superpowers/sdd/2026-11-01-bot-resume/rootcause.md` (a copy of the investigation of 15504a69) plus Jonas's decisions of 2026-11-01, below. Production evidence: Homelab's CT 144 measurements of 2026-11-01:
- five resumes of one video each ran the full 11-stage chain in 8.1–12.5 s;
- provider_check→provider_pcm had a median of 9.0 s, against yt-dlp's own 3.7 s;
- encoder→call_group_join had a median of 2.87 s.

## Decisions (Jonas, 2026-11-01)
- Pause holds the stream and sends silence; resume continues where it paused. After **10 minutes** paused, the Bot frees the stream, and a later resume restarts from where it was paused.
- Ships as hotfix **v0.1.0-signal1.4.2** from main on `fix/bot-resume-cold-start`, with its own PR.
- All five fixes (A–E). B's fix waits on CT 144's read-only IPv6/cache check; instrumentation goes in regardless.
- Ruling (controller): Spotify's helper has no seek, so a Spotify resume after the 10-minute bound restarts the track from 0. This is documented in the notes as a known limit. Cost if wrong: one more helper change later.

## Global Constraints
- No wire, protocol or schema change: the client still sends op 14 Resume, and op 13 and op 15 stay immediate.
- The pause gate must not leak audio: no samples from the paused source reach the track while paused.
- A paused session counts as idle for idle-leave, and the 10-minute bound closes it even if the room stays.
- Skip, Stop and Clear still close the playback exactly as today.
- The existing latency tests in `cmd/dillabot/music_control_latency_test.go` stay green.

## Review Focus
1. Pause during start (before `published`): there is nothing live to gate, so it must fall back to today's close, never leave a half-started session paused.
2. Resume after the track's source died while paused (googlevideo dropped the connection, so yt-dlp exits): resume must detect the dead source and cold-start at the paused position, not publish silence forever.
3. Two quick pause/resume pairs, or a resume racing the 10-minute bound's close: exactly one playback, never two published tracks.
4. A persisted-snapshot Resume that arrives after a restart of the Bot (no live playback): a cold start at PositionMS.
5. Enqueue with a stale revision while the queue is at its limit: it is refused for the limit, never silently dropped.

---

### Task 1: Warm pause and resume (A)

**Files:**
- Modify: `internal/bot/music/supervisor.go` (applyLocked ~231, ApplyPersistedSnapshot ~290, SchedulePersistedSnapshot ~351-375, adoptLocked ~336, idleCheckLocked ~86)
- Modify: `internal/bot/music/queue.go:147-150` (Resume keeps PositionMS)
- Modify: `internal/bot/music/session/session.go` and `internal/media/live_opus.go:103` (the gate)
- Modify: `internal/bot/music/youtube/sandbox_worker.py` and `pcm_sandbox.go` (start offset)
- Tests: `internal/bot/music/supervisor_test.go`, `internal/bot/music/session/*_test.go`, `internal/media/live_opus_test.go`, `cmd/dillabot/music_control_latency_test.go`

**Interfaces:**
- Produces:
  - `music.Pausable` with methods `Pause()`, `Resume() error` and `PositionMS() int64`;
  - `session.Session` implements it;
  - `Resume` returns `ErrSourceGone` when the source died while paused.
- Produces: a start offset on the YouTube PCM start (`StartAtMS int64`), passed to ffmpeg as output-side `-ss` seconds. Spotify ignores it.

- [ ] **Step 1: Failing tests**
  - `TestPauseKeepsThePlaybackAndResumeContinuesIt`: a fake starter counts Start and returns a fake Pausable. Run ApplyImmediate(Pause), then SchedulePersistedSnapshot(Resume, start=true). Assert Start==1, Pause==1, Resume==1, and that state PositionMS equals the fake's PositionMS (non-zero).
  - `TestPausedPlaybackIsClosedAfterTenMinutes`: an injected clock advanced to 10m+1s; assert Close was called, then Resume causes Start==2 with StartAtMS == the paused position.
  - `TestResumeAfterSourceGoneColdStartsAtThePausedPosition`: the fake Resume returns ErrSourceGone; assert one Close and one Start with StartAtMS.
  - `TestPauseDuringStartStillCloses`: Review Focus 1.
  - `TestQuickPauseResumePairsKeepOnePlayback`: two pairs plus a racing bound timer; assert at most one live playback and one publish.
  - `TestPausedPlaybackCountsAsIdle`: idle-leave fires while paused.
  - live_opus: `TestPausedProviderSendsSilenceAndNoSourceSamples`.
  - latency test `resume_after_pause`: a starter whose second Start sleeps 5 s; audio is running again within 1 s of the Resume reply.
- [ ] **Step 2:** Run them and see them fail.
- [ ] **Step 3:** Implement:
  - The gate goes in `LiveOpusProvider.NextSample`. While paused it returns a cached silent Opus frame every 20 ms and does not read the source.
  - Position is counted from the source frames delivered (20 ms each).
  - `applyLocked(OpPause)`: if the playback is Pausable and published, Pause it, record PositionMS in the state and arm a 10-minute timer; otherwise close it as today.
  - `OpResume` with a paused live playback for the same `Current.ID`: Resume, and skip startLocked. Otherwise go through startLocked with StartAtMS = PositionMS (YouTube).
  - queue.go Resume keeps PositionMS. Update the "Ruling AI" comment to say why.
- [ ] **Step 4:** Run `go test -race -count=1 ./internal/bot/... ./internal/media/... ./cmd/dillabot/...` and expect PASS.
- [ ] **Step 5:** Commit `fix(bot): pause holds the live stream and resume continues it; a pause past 10 minutes frees it`.

### Task 2: Explicit starts at interactive priority, and join branch logs (C)

**Files:** `internal/bot/music/session/session.go:210`, `internal/bot/music/supervisor.go:299,386`, `cmd/dillabot/music_call_join.go:120-289`, tests beside them.

- [ ] **Step 1: Failing tests**
  - `TestExplicitStartRunsAtInteractivePriority`: `client.RequestPriority(ctx)` observed in a fake JoinCallGroup is ≥ 1 for SchedulePersistedSnapshot(start=true), and 0 for EOF advance.
  - `TestMemberBranchMakesNoJoinWrites`: native state 2 and GroupPage ok give exactly 2 HTTP reads and 2 helper calls, and no ExternalJoin, Resync or TrustSender.
- [ ] **Step 2:** Implement:
  - session.Start keeps `WithoutCooperativeStop` but no longer forces `WithBackground`;
  - person-triggered starts pass `client.WithInteractive`;
  - the join attempt logs `branch` (`already_member|external_join|resumed_intent|forgot_stale_leaf`) and the milliseconds per sub-call at Debug, plus the branch at Info on the published line.
- [ ] **Step 3:** Run the gates above and commit `fix(bot): person-triggered music starts run at interactive priority; the call join logs its branch`.

### Task 3: Enqueue applies to the latest queue (D)

**Files:** `internal/bot/service/music_interactions.go:239-263,442-447`, its test file; `packages/client-core/src/voice/music.ts:261-266` and its test.

- [ ] **Step 1: Failing tests**
  - `TestEnqueueWithAStaleRevisionAppendsToTheLatestQueue`: persisted state at R+1 and a request at R give status 0 and the track appended at R+2.
  - `TestEnqueueAtTheQueueLimitIsRefusedEvenWithAStaleRevision`: Review Focus 5.
  - `TestConflictReasonsAreDistinct`: the logs say `runtime_revision_mismatch` and `client_revision_stale`.
- [ ] **Step 2:** Implement:
  - OpMusicEnqueue skips the client-revision check and the runtime mismatch refusal;
  - after the resolve, apply to a freshly loaded snapshot with a bounded compare-and-save retry (3 attempts);
  - the limit and validation run against the current state;
  - other mutations are unchanged.
- [ ] **Step 3:** Run the gates and commit `fix(bot): adding to the queue applies to the latest queue instead of failing on a stale panel`.

### Task 4: The music join's trust refresh takes the per-user claim (E)

**Files:** `cmd/dillabot/music_call_join.go:379-407`, `cmd/dillabot/command_worker.go:349-351`, `internal/bot/service/source_trust.go`, tests.

- [ ] **Step 1: Failing test** `TestMusicJoinTrustAndIntakeTrustNeverFailEachOther`: run `refreshMusicCallJoinLeaves` and `SourceTrust.RefreshPendingLeaves` concurrently for one user against a fake core that rejects a stale cached version. Intake returns the user in Busy with a nil error, never a TrustPhaseError.
- [ ] **Step 2:** Implement: the music join holds `TryClaimTrust` for the user around the history read and TrustSender, or routes through SourceTrust. The intake warning logs `phase` and a bounded cause class.
- [ ] **Step 3:** Run the gates and commit `fix(bot): the music join's trust refresh takes the per-user claim; intake logs the trust phase`.

### Task 5: Provider start timing (B)

**Files:** `internal/bot/music/youtube/egress.go:86-97,241-264`, `sandbox_worker.py:183-205`, `internal/bot/music/session/adapters.go:50,67`, tests.

- [ ] **Step 1: Instrument; this ships regardless.**
  - The broker logs per CONNECT, at Debug: the host class (youtube|googlevideo|other), DNS ms, dial ms, the winning address family and the number of failed addresses. It never logs a full URL or the video id.
  - The worker writes monotonic marks to stderr: spawn, first yt-dlp byte, first PCM.
  - `YouTubePCM.Start` logs sandbox_ready separately from first_sample.
- [ ] **Step 2: The fix, decided by CT 144's check** (the controller fills this in before dispatch):
  - if IPv6 is blackholed or slow, dial the validated addresses happy-eyeballs style (IPv4/IPv6 raced with a 250 ms stagger), keep the all-public validation, and add the test `TestBrokerConnectsFastWhenIPv6Blackholes` (a fake resolver with [blackholed v6, good v4] connects in under 400 ms);
  - if the cold yt-dlp cache is the cost, bind-mount a persistent bot-owned cache directory (player cache only) and add a test that the mount is present and is not HOME.
- [ ] **Step 3:** Run the gates and commit `fix(bot): the YouTube start logs its timing and <fix>`.

### Task 6: Close

- [ ] Write `docs/deploy/releases/v0.1.0-signal1.4.2.md`, on top of signal1.4.1:
  - pause and resume;
  - the 10-minute bound;
  - the Spotify limit;
  - faster starts;
  - enqueue no longer failing on a stale panel;
  - the logs.
- [ ] Update docs/user music section: pause holds your place.
- [ ] Gates: `go build ./...`, `go test -race ./internal/bot/... ./internal/media/... ./cmd/dillabot/...`, golangci-lint, client-core vitest, then `npm run ci:local -- --only go,go-native,go-lint,node` (memory is tight; full CI runs on the PR).
