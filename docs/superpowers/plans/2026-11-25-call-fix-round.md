# Call fix round: epoch catch-up, self-Remove after an end, media close order, and review findings

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan.

**Goal:** close the defects that the 2026-11-25 lockout investigation and the two final reviews of branch
fix/own-share-self-view found.

**Sources (read them):**
- the investigation, /tmp/claude-1000/-home-thim-Repositories-dilla/da223a73-3d7b-43a1-bad9-8a8d719639d6/scratchpad/old-media-rejoin-report.md;
- the lockout review, .../scratchpad/lockout-final-review.md;
- the self-view review, .../scratchpad/selfview-final-review.md.

**Spec:** Jonas, 2026-11-25:
- all three call fixes, in one PR;
- keep the 30 s rejoin wait (parity with join.ts);
- keep the boot splash's static caret.

## Global Constraints

- **E2EE invariants, never broken:**
  - no leaf is removed without a member's commit;
  - the instance never commits or drops a Remove;
  - the resync guard never passes a Remove target;
  - bots aren't elected;
  - epochs stay monotonic.
- **Logs:** ids, epochs and reason codes only. Never message content, sealed arguments, keys, member names or screen
  pixels.
- **Kept as is:** `REJOIN_WAIT` stays 30 s, and the boot splash's caret stays.
- **Lane A** owns dilla-client/**, internal/** and docs/user/**.
- **Lane B** owns dilla-desktop/**, dilla-media/**, dilla-desktop-ui/** and clients/desktop/*.md.
- Each lane stages only its own files, by explicit path.

## Lane A: client E2EE and server tests

### A1: the call group's epoch follows other members' commits (investigation section 4b)
- **Failing real-server test**, `the_call_creator_reaches_epoch_1_after_another_member_joins`: the creator registers the
  call group, a second member external-joins, and the creator's core row reaches epoch 1 within a bound. Use the
  existing call world harness and the `-- --ignored` live-test env.
- **Make the swallowed failures visible:** log `catch_up_now`'s error branch (sync/mod.rs:1066-1088) and
  controller.rs:142, ids and codes only.
- **Fix the root cause the test shows.** The investigation suspects join.rs marks the call group active directly on the
  core instead of via `Engine::activate`, so held frames are never replayed and no catch-up is queued. Route it through
  `Engine::activate`, or whatever the test proves.
- Commit: `desktop: a call's group follows other members' commits, so the creator's keys don't stay at the first epoch`.

### A2: no self-Remove into a call the leave already ended (investigation section 4a)
- controller.rs:82-87: when the conditional end ended the call, skip the self-Remove. Don't spend the up-to-3 s
  catch-up, and don't log the misleading stale-epoch warning.
- Test: a leave with only the Bot left ends the call, sends no self-Remove, and the next join starts at once.
- Commit: `desktop: a leave that ended the call sends no self-Remove into the closed group`.

### A3: lockout review Minors
- **M1:** docs/user wording must state the real worst case (about 2.5-3 min when the dropped session's SDK keeps
  reconnecting), not "about a minute". Make the c23d60259 commit's claims accurate in the docs.
- **M3:** the proposal-read-failure Go test must be able to fail. Space its sweeps at least 30 s apart.
- **M4:** add Go tests for the TTL re-issue and same-second cases, and assert the end reason `bots_only`.
- **M2:** the resync's closed-group refusal now answers before the capability check. Decide whether it must follow
  commit step 1/1b's order. If yes, reorder; if no, give the reason in a code comment.
- **Server change wording:** add a doc comment on the 942372756 rule saying it shortens the bots-only end by at most
  one admission window. It does not cover a lockout caused by an uncommitted Remove of a leaf that left.
- Commit: `server: bots-only end tests pin re-issue and reason; docs state the real rejoin bound`.

## Lane B: desktop UI and media

### B1: media close order hardening (investigation fix location 1)
- dilla-desktop/src/screens/voice/session.rs ~1074-1075: `drop(live)` only after `call.leave().await`.
- voice/mod.rs ~1536-1546: the window's own join waits for its own session's `media_live()` to clear.
- Test: hold the room's close open, and check the join's first route isn't called until it's released.
- Commit: `desktop: a rejoin waits for its own old media room to close`.

### B2: self-view review Minors
- **m1:** scale the self-view frame down to at most the tile's needed size (for example, cap the long side at 640 px)
  before the RGBA copy, colour swap and mirror. Keep the 15 fps cap. Don't repaint the whole window per frame if a
  narrower notify is possible; if it isn't, say so. State the measured or computed MB/s at 1080p and 4K in the report.
- **m2:** `RemoteFrame` (and any frame type holding pixels) gets no `Debug` or `PartialEq` over its pixels. Write a
  manual `Debug` with size and counter only.
- **m3:** the placeholder guard's `.replace(` escape hatch must check that every `{name}` in the string is replaced at
  that call site.
- **m5:** the own screen tile doesn't read "0 fps" for a second after a quality change. Keep the last rate until a new
  one is measured.
- **m6:** fix the TOLERANCES.md wording about the caret.
- **m4:** the rejoin announcement isn't said with an empty place. Wait for the name, or say it once with the name.
- Commit: `desktop: the self-view is scaled before it is copied; review fixes`.

## Review Focus
1. A1's fix must not replay or apply a frame or commit out of order, or across groups.
2. A2 must still send the self-Remove whenever the call is not ended (another human remains).
3. B1 must not deadlock a join when the old session's close fails or hangs. Bound it and log it.
4. B2's scaling must not touch what is sent.
