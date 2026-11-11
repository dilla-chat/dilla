# Bot helper `invalidated_decode / group_apply / result_decode`: root cause

Read-only analysis of main at 91d51203 (v0.1.0-bot2.2). Nothing in the worktree was edited. The Go repro ran through `go test -overlay`, from a test file kept in the scratchpad.

## TL;DR

- **Mismatch (high confidence).** The Rust core can return a Bot event page whose `next` cursor is above its `high_water`. Go's `decodeGroupEvents` rejects that page. `ReadGroupEvents` (group sub-op 18) is called with `mutation=true`, so the rejection invalidates and closes the helper.
  - Rust: `core/dilla-core/src/client/bot_events.rs:282`, where `high_water = COALESCE(MAX(delivery_id),0)`. Line 290 sets `next = max(after, ack)`, and line 368 encodes `[events, next, high_water]` without clamping.
  - Go: `internal/bot/native/groups.go:1063-1064`, `out.Next > out.HighWater` → `"event cursors"`. The call is at `groups.go:1075`, through `client.go:188-193` → `invalidateLocked` → `CloseDetailed(invalidated_decode, HelperGroupApply, "result_decode")`.
  - The log says `operation=group_apply` for **every** group sub-op (0-20), because they all travel as `HelperGroupApply` (`helper/client.go:491-496`). The label does not mean MLS apply.
- **Trigger: the v1 upgrade.** v1 journaled interaction-group (kind 3) events into `app_bot_events`. That was the command intake, so those rows hold the newest delivery ids, and the Bot acked through them. Bot v2's `remove_interaction_groups` (`core/dilla-core/src/client/mod.rs:286-345`) deletes those groups' `app_bot_events` rows (line 331) at the first bot2 open. The ack in `app_bot_event_state` stays, so `MAX(delivery_id) < ack` from then on. Every journal read then returns `next = ack > high_water`.
- **Recurs: yes.** Nothing consumes it. Only a newly journaled kind-0 text event, whose AUTOINCREMENT id exceeds the ack, heals it.
- **Fix.** Fix the encoder: report `high_water = max(MAX(delivery_id), ack)` in both `bot_events_read` and `bot_events_ack`. Add the cross-language vector below. Optionally add a fixed decode-detail token, and stop treating a journal read as an invalidating mutation.

## Q1: which result shapes Go rejects

I checked every group sub-op that runs with `mutation=true` in production against its Rust encoder:

| sub-op | Go decoder | Rust encoder | verdict |
|---|---|---|---|
| 7 ApplyWelcomes | `groups.go:609-649` | `group_adapter.rs:240-360`, `groups.rs:783-1018` | consistent: outcomes 0-3, short code text, input order kept, one row per Welcome |
| 8 ApplyGroup | `decodeApply` `groups.go:685-720` | `sync.rs:744-1105`, `wire.rs:580-592` | consistent: state ≤4, next_seq ≥1, BTreeMap-ascending nonzero seqs, flags ≤3. A non-active (JOINING) group gives E_CORE_STATE, a status, not a decode error (`sync.rs:779-783`) |
| 2/5 Create/ExternalJoin | `groups.go:460-516`, `544-558` | `bot_intents.rs:406-457`, `groups.rs:686-712` | consistent; an intent retry returns the stored `[commit, info]` or a conflict |
| 3/4/6/10/11/19 void | `voidResult` | `empty()` | consistent |
| 13 Revalidate | `[u32]` unwrapped | `e.array(1).uint(n)` unwrapped | consistent |
| **18 ReadGroupEvents** | `decodeGroupEvents` `groups.go:980-1069` | `bot_events.rs:261-375` | **MISMATCH: next > high_water** |

On sub-op 18:
- The page is `[rows, next, high]`, with `next = max(after, ack)` and `high = COALESCE(MAX(delivery_id),0)` over the rows that still exist.
- Any delete of the top journal rows leaves `ack > high`. The poller always calls `ReadGroupEvents(ctx, 0, 100)` (`group_event_poller.go:156`), so `next = ack`.
- Go rejects at `groups.go:1063`. The poller repeats the same check at `group_event_poller.go:160`. Go treats `next ≤ high_water` as an invariant; Rust does not keep it.
- **The encoder is the wrong side.** The core's own ack path enforces the same invariant: `bot_events_ack` refuses `through > high_water` (`bot_events.rs:401`, `412`). So even if Go accepted the page, the follow-up `AcknowledgeGroupEvents(page.Next)` (`group_event_poller.go:213`) would be refused with E_CORE_INPUT, on every poll.

The candidate classes you listed check out like this:
- No unknown enum variant.
- No required-vs-null field mismatch.
- No integer width or sign issue: Go `groupUint` caps at MaxInt64, and the core keeps every value in i64.
- No map or tag issue: cborx forbids both, and Rust never emits them.
- No list-limit mismatch: rows ≤100 on both sides.
- No empty-result case for a no-op Welcome: the outcome is always a 4-row.

## Q2: trigger on CT 144

- **Primary cause: the v1 → v2 upgrade.**
  - At v1 (`git show 247572896:core/dilla-core/src/client/bot_events.rs:185-195`), `record_accepted` journaled kind 3 (Interaction) groups as well as kind 0.
  - v2 `remove_interaction_groups` runs at every open and finds the kind-3 groups only once, at the first bot2 start. It deletes their rows, including `app_bot_events`.
  - `ack_delivery_id` is untouched, so `MAX(delivery_id)` falls below `ack`, or to 0.
  - This matches "silent helper closes on bot2 and bot2.1".
  - Shown with SQLite AUTOINCREMENT: ack 5, delete the newest group's rows → `high_water 2`, `next 5`. The next insert gets id 6, which is the self-heal.
- **Why it never healed.** Under v2, commands are sealed requests, not channel messages, so nothing journals a new event. Only a human text message in a Bot channel group the Bot holds and syncs would. The owner's unmark and remark keeps the one group that matters out of reach: there is no Welcome, re-add is pending, and `bot joining Bot channel group` never completes. A quiet installation therefore stays with `ack > high_water` indefinitely.
- **Where it fires: the text poller, not the pass.** `newTextPoller` (`cmd/dillabot/membership_pass.go:48-61`) runs ReconcileOnce, then subscribe, then `PollOnce`. `PollText` (`gateway_runtime.go:449-465`) runs it on every gateway wake: feed changes (`feed_handlers.go:242/311/362`), hints, and music call-group joins and subscriptions.
- **How the new evidence fits:**
  - `bot text command intake deferred error_class=outcome_unknown` fired 8 times, matching the 8 closes. That log line exists only in `PollText` (`gateway_runtime.go:462`). `outcome_unknown` is the `ErrOutcomeUnknown` that `client.go:194` joins to a mutation decode error.
  - The main pass runs the same ReconcileOnce through `reconcileReadiness`, and it never logged `reconciliation failed`. The only extra helper work in the poller is `PollOnce`, which calls `ReadGroupEvents` (sub-op 18).
  - The "60 ms after the music queue log" and "right after call_group_join" correlations are the wake that the pass or the music join causes. The wake runs `PollText` concurrently, and the close lands in whichever music or command call is in flight.
  - The first close at 5 s after cold start is the wake from the first feed snapshot (`SnapshotDone` → `f.changed()`).
- **Not the cause:**
  - The device da5b3ce4 Remove and the dropped queued add touch `RevalidateGroup` and ProposedAdds paths, whose shapes match.
  - The pending Welcome cannot be the cause either: no ApplyWelcomes runs for it, because there is no Welcome for an unmarked-then-remarked group.

## Q3: why the earlier replacement survived, and whether it recurs

- Nothing was consumed, acknowledged or cursor-advanced:
  - `bot_events_read` only redacts invalid rows (disposition 1).
  - Go never reaches the ack.
  - The ack would be refused anyway (`bot_events.rs:401`).
- The replacement at 13:25 survived only because, with no load, no wake arrived, so `PollText` never ran.
- Under load every wake kills the helper. That is the 13:29-13:33 loop: the backoff climbed 0, 0, 10, 20, 40, 80 s and never reset, because no helper lived `helperServedFor`.
- **It recurs at every restart and every wake until the fix ships, or until one new kind-0 event is journaled.**
- Immediate mitigation without a deploy: a trusted member posts one ordinary text message in a Bot channel whose group the Bot currently holds. After the next sync it is journaled with id > ack, and the condition clears permanently.

## Q4: the invalidate policy, and the minimal fix

The "outcome unknown, so close" policy is right for MLS and state mutations. It is wrong to classify the journal read as one:
- `bot_events_read`'s only write is an idempotent redaction of rows that fail rechecks.
- A rejected page leaves nothing ambiguous.

The minimal correct fix:

1. **Encoder (required), `core/dilla-core/src/client/bot_events.rs`.** Make the high-water never lower than the durable ack:
   - Read path, line 281-285: `SELECT MAX(COALESCE(MAX(delivery_id),0), (SELECT ack_delivery_id FROM app_bot_event_state WHERE singleton=1)) FROM app_bot_events`. Or in Rust: `let high = high.max(ack);`.
   - Ack path: apply the same clamp at line 395 and in the in-unit recheck at line 410.
   - With this, `next ≤ high_water` always holds, and `AcknowledgeGroupEvents(ack)` is a no-op re-ack.
   - No migration is needed; it heals the CT 144 store on first read.
2. **Cross-language vector.**
   - Rust test in `core/dilla-core/tests/client_bot_events.rs`, following `bot_with_trusted_peer`:
     1. Journal one event and ack it.
     2. Delete its `app_bot_events` row. This mirrors `remove_interaction_groups`; alternatively, insert a kind-3 group row and reopen.
     3. Assert that `bot_events_read(0,100)` equals the bytes `83 80 <ack> <ack>`, and that `bot_events_ack(ack)` is Ok.
   - Go: add the same hex to the decode tests in `internal/bot/native` and assert that `decodeGroupEvents` accepts it.
   - Keep a negative Go vector, `83 80 05 03` → `"event cursors"`, so the invariant stays explicit.
3. **Policy (recommended).**
   - Call `ReadGroupEvents` with `mutation=false` (`groups.go:1075`). A bad page then fails the poll without killing the helper.
   - Keep `AcknowledgeGroupEvents` as a mutation.
4. **Detail token (optional).**
   - Return fixed sentinel tokens from the decoders, e.g. `events.cursor_above_high_water`, `apply.state`, `welcome.status`.
   - Pass the token as an extra attr into `CloseDetailed` instead of the constant `"result_decode"` (`client.go:213`). Never pass bytes or the error text.
   - Also log `sub_op` (the payload's first uint). Today every group sub-op shows as `group_apply`, which is what pointed this hunt at MLS apply.

## Q5: reproduction

Go, exact Rust page bytes, run through the overlay without touching the tree:

```
cd /home/thim/Repositories/dilla/.claude/worktrees/signal-design
TMPDIR=/tmp/t76 GOTMPDIR=/tmp/t76 go test -overlay <scratchpad>/overlay.json \
  -run TestReproEventsCursorAboveHighWater -v -count=1 ./internal/bot/native/
=== RUN   TestReproEventsCursorAboveHighWater
    zz_repro_events_cursor_test.go:19: next=5 high_water=3 -> err=event cursors
    zz_repro_events_cursor_test.go:22: next=5 high_water=0 -> err=event cursors
    zz_repro_events_cursor_test.go:25: wrapped (ReadGroupEvents decoder) -> err=event cursors
--- PASS
```

The test file is `<scratchpad>/repro_events_cursor_test.go`. The healthy page `83 80 05 05` decodes.

Rust side:
- The SQLite behaviour is shown with an in-memory DB: after deleting the newest group's rows, ack 5 gives high_water 2.
- `remove_interaction_groups` deleting `app_bot_events` is at `mod.rs:331`.
- v1 journaling kind 3 is in git at 247572896.
- I did not build a Rust integration test, because that needs a new test file in the crate.

## Side notes from the new evidence

- **"bot installation or interaction group still joining"** is `passIncomplete` (`cmd/dillabot/event_loop.go:81-84`, `203`). After every helper start the pass marks the installation `incomplete[...] = helperStartRecheck` (`main.go:~560-566`), so the loop runs it again once the asynchronous start finishes. It is not an MLS group state and it is benign.
  - A real JOINING (state 1) group fed to group_apply returns E_CORE_STATE, a status, not a decodable result (`sync.rs:779-783`). It cannot cause `result_decode`.
- **Music refusal `op=10 status=9 reason=shared_call_read`, group all zeros.** This op is the interaction op (music.state), not a helper op.
  - `coalescedState` sets `reason="shared_call_read"` before `Authority.CurrentCall`, and assigns `group` only after it (`internal/bot/service/music_state_reads.go:155-171`). A dillad 400 on the call read therefore logs a zero group.
  - It is unrelated to the decode loop.
- **Duplicate /play (second finding, medium).**
  - Client Retry reuses the request id (`packages/client-core/src/bot/commands.ts:320`).
  - The helper persists replay rows (`core/dilla-bot-core/src/command.rs:319-326`).
  - The Go intake keeps answers for 5 min and re-seals them on Replay without rerunning (`internal/bot/intake/intake.go:585-628`).
  - So a same-id Retry does **not** run the handler twice, as long as the open was recorded.
  - The duplicates come from new /play commands with new request ids:
    1. Each /play's enqueue commits (`Store.Save`).
    2. Playback start then joins the call group, and the join's wake kills the helper.
    3. The success answer is "not sealed" (op 23 closed), the room ends idle, and the item stays queued.
    4. The user, who saw a failure, sends /play again.
  - The enqueue path has no idempotency key:
    - Item ids are random per run (`internal/bot/service/music_interactions.go:373`).
    - Enqueue is exempt from the client revision check (line 307).
    - The CAS-conflict loop re-applies with fresh ids (lines 532-536).
  - If the handler ever reruns for the same request (the helper replay row is lost, or `remember` replaces a record at `intake.go:684-686`), it enqueues again.
  - Minimal idempotency fix:
    - Derive each track's item id as `H("dilla.bot.music.enqueue.v1" ‖ request_id ‖ index)` instead of `id.New()`.
    - In the apply step, treat an enqueue whose first item id is already in the queue or current as already applied, and answer from the current state.
    - This makes a replay or rerun of the same request id a no-op.
    - Fixing the decode loop removes the main source of the user-visible "failed but actually queued" outcome.
