# Desktop Bot tests wait for channel bits before reading state: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan.

**Goal:** the desktop Bot tests no longer fail now and then with "Manage Community permission is required.". That
failure blocked the alpha.3 release (issue #198, nightly run 45616827421).

**Cause** (confirmed; report:
/tmp/claude-1000/-home-thim-Repositories-dilla/da223a73-3d7b-43a1-bad9-8a8d719639d6/scratchpad/announce-perm-report.md):
- The test calls `client.threads().set_channel_bits(general, …)` (clients/desktop/dilla-desktop/tests/bot.rs:1842) and
  then copies `client.state()` into `SlotCx` (bot.rs:1851-1852).
- `set_channel_bits` only queues the change on the client's update loop (client.rs:907-909, applied at :1668). When
  the loop hasn't run, the snapshot has no bits for #general, and the form's local check
  (announcement_form.rs:239-241, 254-260) refuses before anything is sent.
- This is a test bug. The local check fails closed (it can only refuse), and the Bot checks every operation against
  the server.

**Spec:** Jonas, 2026-11-26: "Fix first, move tag".

## Global Constraints
- Test code only. No product code changes. The local permission check stays fail-closed.
- **New helper** in the tests' `world` support module (find it next to `world::wait_for`):
  `set_channel_bits_and_wait(rt, client, channel, bits) -> State`. It sets the bits, then waits up to 10 s with
  `world::wait_for` until `s.threads.channel_bits.get(&channel).copied() == Some(bits)` (match the real field types),
  and returns that snapshot.
- **Use it at all four sites:** bot.rs ~1842 (announcement), ~2140 (role panel), ~2687 (uncertain schedule or poll)
  and ~2947 (poll card). Each `SlotCx.state` is built from the snapshot the helper returns, not from a fresh
  `client.state()`.
- **Grep for the pattern elsewhere:** search dilla-desktop/tests for other `set_channel_bits` (or similar setters
  followed by `client.state()`). Fix any other site the same way, and list them.

### Task 1
- [ ] **Prove the race first.** Temporarily build `SlotCx` straight after `set_channel_bits` without yielding, and
  see the announcement test fail with the CI assertion. Then revert that.
- [ ] **Add the helper and use it at all sites.** With the bad ordering forced, the test now passes.
- [ ] **Run the whole Bot test file live**, as CI's bot shard does (read .github/workflows/desktop-gpui.yml for the
  command and env: `DILLA_TESTHOST`, `DILLA_CORE_WASI`, `--ignored` if needed). Run it twice. Also run
  `cargo clippy -p dilla-desktop --all-targets -- -D warnings`, rustfmt on your files, and
  `bash -c 'node --test scripts/*.test.mjs'` (behaviour gates).
- [ ] **Commit:** `test(desktop): Bot tests wait for a channel's bits before they read the client's state`.
