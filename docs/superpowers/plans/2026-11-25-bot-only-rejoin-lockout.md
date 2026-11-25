# A call with only the Bot left no longer locks a dropped person out: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan.

**Goal:** when a person's device drops out of a call and only a managed Bot is left with them, the person can rejoin
within about a minute. Today every rejoin is refused `409 E_REMOVE_PENDING` until the Bot leaves.

**Cause** (investigation 2026-11-25, on origin/main ead2cb3a8 = v0.1.0-gpui2; full report at
/tmp/claude-1000/-home-thim-Repositories-dilla/da223a73-3d7b-43a1-bad9-8a8d719639d6/scratchpad/remove-pending-report.md):
- **The outstanding Remove blocks the rejoin.** The instance proposed a Remove of the dropped person's call leaf. While
  it stands, every resync or rejoin is refused (`guardResyncTarget`, internal/ds/resync.go:181-201).
- **Nobody can commit it.** Only a member commit clears it. The gateway never elects a managed bot to commit
  (internal/gateway/registry.go:183-187), and the target is excluded (internal/ds/election.go:78-86).
- **The timeout doesn't help.** The call sweep voids the Remove after 30 s and re-issues it in the same tick
  (internal/ds/proposal.go:1180-1210, 454-490).
- **The safety net doesn't fire.** The bots-only call end (#135, internal/api/callmembers.go:246-316) counts the
  absent, pending-removal human leaf as an active human (`trackAbsentLocked`, :296-306), so it never ends the call.
  Only the Bot leaving lets LiveKit's room_finished close the call and its group.
- **The client agrees.** `leave_acknowledged`, clients/desktop/dilla-client/src/features/voice.rs:1006-1012, counts the
  Bot as company in `alone`.

**Spec:** Jonas, 2026-11-25: "Server + client, one PR". The E2EE invariants (below) are binding.

## Global Constraints

- **E2EE invariants, never broken:**
  - no leaf is removed without a member's commit;
  - the instance never commits or drops a Remove;
  - the resync guard never lets a Remove target through (one device would get two leaves);
  - bots aren't elected to commit;
  - epochs stay monotonic.
  - Ending the whole call and closing its group (the existing bots-only end) is the allowed way out.
- **Server:** a human leaf counts as an active human for the bots-only end only if it isn't both (a) the target of an
  outstanding instance Remove and (b) absent from the LiveKit room. After the end, a resync is refused
  `E_GROUP_CLOSED`, not `E_REMOVE_PENDING`, and the waiting client's join registers a fresh group, as
  join.rs:510-512 already does.
- **Client:** in `leave_acknowledged`, bot participants don't count as company for `alone`, so a leave with only the
  Bot left ends the call. The server already ignores bot leaves for that.
- **Unchanged:**
  - a human who is present in the room, or a second human who can commit, keeps the call alive;
  - the Remove proposal, election and commit paths keep their behaviour.
- **Logs:**
  - ids and reason codes only;
  - never message content, sealed arguments, keys or member names.
- **Bounds:** the lockout ends within 2 sweep intervals after the Remove is outstanding and the leaf is absent. State
  the sweep interval and the real bound in the report.

## Review Focus

1. A human with a live connection but slow to commit, present in the room, must never have the call ended under them.
2. Two humans plus the Bot, where one drops: the other human still commits the Remove, and the call is not ended.
3. A race: the dropped human returns (re-enters the room) just as the sweep runs. The end must not fire on stale room
   state.
4. Account-kind lookup failures (the third reason the investigation named) must fail safe: never end the call on an
   unknown kind, and don't silently loop either. Log it and say what happens.
5. The client's `alone` change must not end a DM call or a call where the "bot" flag is spoofable. Check where the
   bot flag comes from (server-asserted or not).

### Task 1: the bots-only end counts an absent, pending-removal human as gone (server)

**Files:** internal/api/callmembers.go and its tests. Touch internal/ds/* only if a read helper is needed (no
behaviour change there).

- [ ] **Failing Go tests** in the existing callmembers or sweep test files, following their fixtures:
  - `TestBotsOnlyEndsWhenTheOnlyHumanIsAbsentAndPendingRemoval`: a human plus a bot; the human's Remove is
    outstanding and the human is absent from the room. After two sweeps the call ends `bots_only`, and a resync is
    then refused `E_GROUP_CLOSED`.
  - `TestBotsOnlyKeepsACallWhoseHumanIsStillInTheRoom`: same, but the human is present. The call is kept.
  - `TestBotsOnlyKeepsACallWithAnotherHuman`: two humans plus a bot, one human pending removal and absent. The call is
    kept.
  - A test for the account-kind lookup failure path (Review Focus 4).
- [ ] **Implement** in `trackAbsentLocked` or where the active-human count is formed.
- [ ] **Run** `go test ./internal/api/... ./internal/ds/...`, `go vet ./...` and `gofmt -l`.
- [ ] **Commit:** `server: a call with only a bot and a dropped person pending removal ends, so the person can rejoin`.

### Task 2: a leave with only the Bot left ends the call (client)

**Files:** clients/desktop/dilla-client/src/features/voice.rs, plus a test in dilla-client/tests or the module's
tests.

- [ ] **Failing test** `a_leave_with_only_the_bot_left_ends_the_call`, and a negative test
  `a_leave_with_another_person_left_keeps_the_call`.
- [ ] **Implement** in `leave_acknowledged`.
- [ ] **Run** `cargo test -p dilla-client` on these tests, plus clippy `-D warnings` and fmt on your own files.
- [ ] **Commit:** `desktop: leaving a call with only the Bot left ends it`.

### Task 3: real-server scenario test with a Bot member

- [ ] Add a variant of the real-server test `rejoin_waits_for_pending_remove` (find it with grep under
  clients/desktop/dilla-client/tests) in which the committing peer is a bot member, the human drops, and the rejoin
  ends `In` in a fresh group within the bound. If the test host (`dilladtest`) can't model a managed bot member, say
  exactly what is missing and add the smallest test-host support for it.
- [ ] **Run** it live:
  `DILLA_TESTHOST=$PWD/target/dilla-testhost DILLA_CORE_WASI=$PWD/target/wasm32-wasip1/release/dilla_core_wasi.wasm cargo test -p dilla-client --test <file> -- --ignored <name>`.
  Build the test host per the repo's docs if it's missing.
- [ ] **Commit:** `test: a dropped person rejoins a call whose only other member is the Bot`.

### Task 4: docs

- [ ] docs/user/voice.md or music.md: one line saying that if your connection drops while only the Bot is in the call,
  rejoining can take up to about a minute.
- [ ] Add a "two clients plus the Bot: join, share, drop, rejoin" row to clients/desktop/MANUAL.md. Coordinate: another
  task on this branch edits MANUAL.md's screen-share section, so touch only a new row or section.
