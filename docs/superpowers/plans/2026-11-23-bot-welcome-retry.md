# Bot welcome survives a busy group: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task.

**Goal:** a new member is always greeted. Today the Bot sends the welcome the moment it sees the join. At that moment the
welcome channel's MLS group usually still has the newcomer's Add pending, or the Bot has not joined a freshly marked
channel's group yet. The server refuses the post, and the welcome is lost: one fixed attempt ID, no retry, only a log
line.

**Architecture:** the change stays inside the Bot's welcome feature (Go, `cmd/dillabot` and `internal/bot/...`).
- Transient states defer the welcome instead of failing it.
- A closed attempt is followed by a successor attempt, as announcements already do.
- A retry driver in the installation feed re-runs pending joins.
- No change to the shared send path (`SendCoordinator`), the server, or the protocol.

**Tech Stack:** Go (dillabot), Rust test (dilla-client live test).

**Spec:** the root-cause report `.superpowers/sdd/2026-11-19-gpui-gui-gaps/welcome-flake-report.md` (git-ignored) and Jonas's
decision of 2026-11-23: "Fix first, then release" (alpha.2 waits for this fix). Web parity is the behaviour that
e2e/web/bot-channels.spec.ts expects: "a new member is welcomed there".

## Global Constraints

- Welcome text, member names and message bodies never go in logs, errors or test output. Log counts, IDs and error
  codes only.
- One welcome per membership episode, never two. Every retry carries `PredecessorAttemptID` set to the closed attempt,
  so the server's attempt chain stays authoritative.
- Bounded:
  - give up after the existing 24 h `maxCatchupAge`;
  - at most 8 attempts per episode;
  - backoff between retries no shorter than 2 s;
  - on give-up, the episode records a terminal failed status and logs one warning.
- `ErrBotNotJoined`, a destination group with pending proposals or a pending commit (`native.GroupInfo`), and a closed
  not-accepted receipt are all retryable. Any other error keeps today's behaviour: logged and dropped.
- The member must still be a current member when the retry runs. `ResolveCurrentMember` is already checked, so a member
  who left by then gets no welcome.

## Review Focus

1. A join while the Bot is not yet in the channel's group, the first second after marking: greeted once it joins.
2. A join whose Add commit is still pending: greeted after the commit, readable by the newcomer.
3. A Bot restart with a pending episode: the welcome is not sent twice. The attempt chain and the stored episode
   status decide.
4. A member who leaves before the retry: no welcome.
5. A server that keeps refusing: the retries stop at the bound, with no busy loop.

### Task 1: Retryable welcome, with a successor attempt and a retry driver

**Files:**
- Modify `internal/bot/community/welcome/worker.go`:
  - the episode stores `Attempt int`;
  - `attemptID(op, n)`;
  - the retryable classification;
  - pending and deferred handling.
- Modify `internal/bot/service/welcome_runtime.go`: the readiness check on the destination group (pending
  proposals or commit) returns a retryable error.
- Modify `cmd/dillabot/feed_handlers.go`: a pending-join set with backoff, driven by the feed's existing reconcile tick,
  and by group commits if a hook exists.
- Tests:
  - `internal/bot/community/welcome/worker_test.go`;
  - `cmd/dillabot/feed_handlers_test.go` (or the existing feed test file);
  - `clients/desktop/dilla-client/tests/bot_admin.rs`.

**Interfaces:**
- `welcome.ErrRetryLater` (sentinel; `errors.Is`) is returned by `Processor.Process` when the send should be retried.
- Attempt 0 keeps today's ID, `attemptID(op)` hashes. Attempt n ≥ 1 uses the domain string
  `"dilla.bot.welcome.attempt.v2\x00"` plus op plus n as a big-endian uint32. That keeps the IDs of episodes already
  in flight stable.

- [ ] **Step 1: Failing Go tests in the welcome package.**
  - `TestClosedReceiptRetriesWithSuccessor`: the first send returns a valid closed receipt → `Process` returns
    `ErrRetryLater` and the episode stays Pending with Attempt 1. The next `Process` sends attempt 1 with
    `PredecessorAttemptID == attemptID(op)` and, when accepted, marks it Posted.
  - `TestNotJoinedIsRetryable`.
  - `TestPendingGroupDefers`: no send happens while the group reports pending proposals.
  - `TestRetryBoundGivesUp`: the 8th closed receipt ends the episode as failed.
  - `TestLeftMemberNotWelcomedOnRetry`.
  - `TestPostedEpisodeNeverResent`: after a restart, a Posted episode isn't sent again.
- [ ] **Step 2: Failing feed test:** a join whose welcome returns `ErrRetryLater` is retried on the next tick after its
  backoff, and stops once it succeeds or gives up.
- [ ] **Step 3: Implement.** Follow `internal/bot/announcements/scheduler.go:715-745` for the closed-receipt successor
  pattern.
- [ ] **Step 4: Rust test.** In `a_welcome_saved_in_settings_greets_a_new_member`, drop the leave and rejoin workaround
  (bot_admin.rs ~437-451). Bea joins once, and the greeting is awaited in Alice's timeline and, if the fixture can read
  it, in Bea's.
- [ ] **Step 5: Verify:**
  - `go test ./internal/bot/... ./cmd/dillabot/...`;
  - `go vet`, plus the repo's golangci-lint if present (`npm run ci:local -- --only go-lint` style);
  - the live test, run 3 times locally at low parallelism: `CARGO_BUILD_JOBS=4`, with the env from the report.
- [ ] **Step 6: Commit** `bot: a welcome refused while the group is busy is retried, never lost`.
