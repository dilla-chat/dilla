# Outbox drain keeps send order: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan.

**Goal:** a burst of sends is stored in the order it was sent. Today the desktop engine can post a just-prepared row
ahead of older queued rows. The alpha.2 release gate caught it: `load_earlier_grows_the_channel_page_by_one_hundred`
saw a DM page starting at "d2", with d1 posted later.

**Cause** (`clients/desktop/dilla-client/src/sync/mod.rs`):
- `send_prepared` (≈2137-2152) awaits `send_prepare`. The row is now in the core outbox, but its id is pushed onto
  `st.queued` only afterwards.
- `drain_one` (≈2287-2318) ranks rows by their position in `st.queued`, and a row missing from it ranks -1 (first).
- A drain that reads the outbox in that window posts the new row first.

The web client (`send.ts`) has no such window: it is single-threaded and synchronous between prepare and push.

**Spec:** web parity, send.ts `sendRequest` and `drainOne` `inQueueOrder`: "rows this engine queued keep their order;
older rows keep the core's place ahead of them". The root-cause report is in the scratchpad
`load-earlier-flake-report.md` (session-local). Jonas, 2026-11-23: "Root-cause first", before alpha.2.

## Global Constraints

- From the drain's view, "the row exists in the core outbox" and "its id is in `st.queued`" become one atomic step.
- Use one async gate (`tokio::sync::Mutex<()>`, an Engine field). It is held in `send_prepared` over
  `send_prepare` → decode → `queued.push`, and in `drain_one` over the `outbox(g)` read and the `queued` snapshot used
  for ranking.
- The gate never spans a network call (POST, fetch) or `hooks` callbacks. Never hold the std `st()` MutexGuard
  across an `.await`.
- Rows left from before this engine started (not in `queued`) keep ranking first, unchanged.
- Message bodies never appear in logs, Debug output or assertion messages. The fixture strings `d0..d100` / `m0..m102`
  are fine.
- No core (wasm) schema change. A monotonic outbox sequence in the core is a follow-up only.

## Review Focus

1. A drain started between `send_prepare` and the push: the new row isn't posted before the older queued rows.
2. Deadlock: the gate held while `outbox()` or `with_core` waits on something that itself needs the gate.
3. The trust-retry path (`send_prepared` called again after a leaf verdict): it stays ordered and takes no double lock.
4. Latency: no send waits on the network while holding the gate.
5. Several groups at once: correct. A per-group gate is optional, not required.

### Task 1: Gate prepare and drain snapshot; prove the order

**Files:** modify `clients/desktop/dilla-client/src/sync/mod.rs`, `clients/desktop/dilla-client/src/sync/tests.rs`
and `clients/desktop/dilla-client/tests/messages.rs`.

- [ ] **Step 1, the failing unit test** in `sync/tests.rs`, using the existing scripted-port harness there (see
  `a_background_join_retries_a_stale_epoch_only_once_the_group_moved` for the style):
  `a_drain_between_prepare_and_queue_never_posts_the_new_row_first`.
  - Make a drain run between the core's `send_prepare` returning and the push, for example with a port or core
    hook that triggers `request_drain` or awaits a notify inside `send_prepare`.
  - Assert that the POST order equals the send order.
  - If the harness can't interleave deterministically, write the smallest seam that can (a `#[cfg(feature =
    "testing")]` hook) and say so in the report.
- [ ] **Step 2: implement the gate** as the Global Constraints describe.
- [ ] **Step 3: strengthen `load_earlier_grows_the_channel_page_by_one_hundred`.**
  - Where it waits for the first page of 100, both in the channel and in the DM, also assert that the rows are
    exactly m1..m100 and d1..d100 in order.
  - Use the visible item order. If `holds` makes this awkward, add a helper next to it.
- [ ] **Step 4: verify.**
  - `cargo test -p dilla-client --lib sync`.
  - The live test 10× at parallelism 4, with the env from `.github/workflows/desktop-gpui.yml`'s Bot shard
    (`DILLA_TESTHOST`, `DILLA_CORE_WASI`, `--ignored`).
  - `cargo clippy -p dilla-client --all-targets -- -D warnings` (vendored livekit warnings are pre-existing).
  - `cargo fmt --check`.
  - Afterwards `rm -rf target/debug/incremental`.
- [ ] **Step 5: commit** with the message `desktop: a burst of sends is posted in the order it was sent`.
