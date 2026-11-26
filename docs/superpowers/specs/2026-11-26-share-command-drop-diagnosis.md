# Flake: `closing_the_owner_window_stops_its_capture` — "alice's share is on: never"

Worktree: `/home/thim/Repositories/dilla/.claude/worktrees/signal-design` (fix/desktop-test-flakes = origin/main, efd94e910).
Nothing in the repo was edited.

## Root cause (one line)

The media session silently throws away every share command that reaches it before its run loop has taken up the
call's media link, but the client reports `CallPhase::In` (which gates the Share button and the picker's choice)
*before* that happens. A share pressed in that gap is dropped with no notice, no log, no retry.

## The chain, step by step

1. `dilla-client/src/features/voice.rs:733-734` — the join publishes the media link (`r.link.send_replace(Some(link))`)
   and then, at `:749-764`, publishes `phase: CallPhase::In`. Both happen on alice's client runtime, back to back.
2. The window's `MediaSession` (created by `voice::sync` at `screens/voice/mod.rs:1186`, its own `dilla-call` tokio
   runtime) learns of the link only when its task is scheduled again: `session.rs:692-719` `run()` sits in
   `tokio::select! { biased; None = commands.recv() => return, r = links.changed() => ... }`.
3. Test thread (or the UI) sees `phase == In` (test `share.rs:269-271`), resyncs, presses Share (`toggle_share`,
   `mod.rs:2061`, gate `phase == In` at `:2075`), the picker lists (`wait_listed`), and `share_picker::share`
   (`share_picker/mod.rs:233`) calls `voice::share_source` (`mod.rs:2136`, gate `phase == In` at `:2144`), which sends
   `Command::ShareScreen` (`session.rs:406-418`). The send cannot fail and returns nothing.
4. Two places then discard that command if the session task had not yet entered the call:
   - `session.rs:808` — first statement of `in_call`: `while commands.try_recv().is_ok() {}` ("A share asked for
     outside a call never carries into this one"). Everything queued before this line is dropped, including a command
     sent *after* phase In for *this* call.
   - `session.rs:715` — in `run()`'s idle select, `None = commands.recv()` with `biased;`: if a command and the link
     change are both ready, the command branch is polled first, `Some(cmd)` does not match `None`, so the command is
     consumed and discarded and the branch disabled; then the link branch runs.
5. Nothing else ever re-sends it: `in_call` goes on to join the room normally, the loop runs (the dump shows
   `ping_ms: Some(0)`, `rate_kbps: Some((30, 0))`, `loss_pct: Some(0.0)` — those come only from the live loop's
   `link_read` arm, `session.rs:1080-1104`), phase stays `In`, `share` stays all-false, `notice: None`. That is exactly
   the CI dump. A failed open would have set `notice: ShareCancelled` (`session.rs:1522`), a failed join
   `notice: MediaFailed` and a leave (`session.rs:1185-1203`) — neither is present, so the command never reached
   `opens.start`.

The race window is "client published the link and phase In" -> "dilla-call worker polls `run()`/`in_call` up to line
808". Normally microseconds; on a loaded 4-vCPU CI runner (dillad + LiveKit + two clients' runtimes + gpui test
executor + other test binaries' leftovers) a freshly-spawned `dilla-call` worker can be descheduled for the few
milliseconds the test thread needs to resync, press, list and pick (the gpui `TestAppContext` runs the picker's
background listing synchronously in `run_until_parked`, so the whole press->pick path is a few ms of the test thread).

## Proof (deterministic reproduction, no source edit)

gdb non-stop, holding only the `dilla-call` thread for 2 s at a breakpoint; all other threads run. Scripts:
`scratchpad/loop/delay.py` (breakpoint `session.rs:808`, the drain) and `scratchpad/loop/delay809.py` (breakpoint
`session.rs:809`, the line right after the drain). Binary `target/debug/deps/share-60a15a2965b061f7`.

| Breakpoint | Result |
|---|---|
| `session.rs:808` (before the drain) | FAILED at `share.rs:223:9` "alice's share is on: never" after 46.8 s; dump: `phase: In`, `share: LocalShare { camera: false, screen: false, screen_audio: false }`, `notice: None` — identical to CI |
| `session.rs:809` (after the drain, same 2 s hold) | ok, 4.9 s |

Same delay, only the position relative to the drain differs, so the delay itself is harmless; the drain is what loses
the command. Logs: `scratchpad/loop/gdb2.log` (fail), `scratchpad/loop/gdb3.log` (pass).

Statistical runs: 2/2 pass unloaded; loaded run (test pinned to 4 cores with 8 busy loops, 20 iterations):
**13 pass, 7 fail** — every failure the same panic at `share.rs:223:9` with `phase: In`, `share` all false, `notice: None` (logs `scratchpad/loop/load-*.log`). So the flake reproduces naturally at ~35 % under CPU contention, and the deterministic gdb hold reproduces it 1/1.

## Why this test and not the others; the manager window

The race is in `alice_shares`, shared by `closing_the_owner_window_stops_its_capture`,
`another_instance_cannot_stop_or_reconfigure_a_capture` and others; which test loses it is chance. The manager window
(`windows::open_manager`) is not a cause:
- `share_picker::share` returned `true` (the `assert!` at `share.rs:267` passed), so the picker opened in alice's
  window (the `is_window_active` gate at `share_picker/mod.rs:166` and `mod.rs:2075` passed after `focus`), it had
  listed, and the key was found: window-id routing and focus were right.
- `share_source` then reached `session.share_screen` (the call phase gate held, the window had a session — otherwise
  the share could never have turned on in the passing runs of the same code).
- The manager only adds rendering work to `run_until_parked`, which if anything lengthens the press path; it does not
  touch voice areas (`prune` only drops windows that are gone).

## Classification

Product bug (a lost user action, silently), surfaced by a test race. The test is correct: it waits for the documented
readiness signal (`phase == In`) — the same one the UI gates Share on — before pressing.

User reachability:
- Plain join: the gap is scheduler latency, so a human (press + pick takes >= hundreds of ms) essentially never hits it
  unless the machine is starved.
- Wider window: whenever the client reports `In` for a call while the session is still busy with something else —
  `run()` (`session.rs:697-708`) handles "a move replaces the link while the old call runs": the old `in_call` then
  ends media and awaits `close_room` for up to `ROOM_CLOSE_WAIT` = 5 s (`session.rs:610, 616-631`) before the new
  call's `in_call` starts and drains. A Share or camera press made in those seconds after landing in the new call is
  dropped with no feedback. The same drop applies to `Command::Share(Camera, true)` and to `Quality`/`AppAudio`.
- Automation/Bot-style callers that share right after a join hit it like the test does.

Side note (not this flake, same family): `publish_share` returns `Published::Kept` with no notice and no log when
`set_share` is refused (`session.rs:1531-1545`); a refused `POST /v1/calls/{id}/share` (e.g. `E_CALL_SHARERS_FULL`,
404 "not in the call's room") is also silent to the person.

## Proposed fix

Make "outside a call" mean *for another call*, not *before the loop noticed this call*.

1. `clients/desktop/dilla-desktop/src/screens/voice/session.rs`
   - Tag starting commands with the call they were made for: `Command::ShareScreen { call: [u8; 16], .. }` and
     `Command::Share(kind, on, options, call)` (or a wrapper `Tagged { call, command }`). `share`/`share_screen` take
     the call id.
   - `run()`: replace `None = commands.recv() => return` with a receive that returns on `None` and otherwise stashes
     the command in a small `pending: Vec<Command>` (keep the newest per kind; untagged/stop commands outside a call
     are dropped as today).
   - `in_call` (`:808`): instead of draining blindly, `while let Ok(c) = commands.try_recv()` keep those whose
     `call == link.call_id` (plus the stashed ones that match), drop the rest; after the join succeeds and before the
     loop, feed the kept ones through the same handling as the `commands.recv()` arm (factor that arm into a
     `handle_command(...)` fn so both use it). Commands for another call id stay dropped, so the original guarantee
     ("a press as the last call ended never carries into the next one") holds.
2. `clients/desktop/dilla-desktop/src/screens/voice/mod.rs` — `toggle_share` (`:2061`) and `share_source` (`:2136`)
   pass `ui.state.voice.call.call_id` (they already gate on `phase == In`, so it is `Some`).
3. Optional, same PR (fewer PRs): give `publish_share`'s refused-`set_share` path a `log::warn!` and
   `VoiceNotice::ShareCancelled` (or a dedicated notice) so the remaining refusal path is not silent.

## Tests that prove it

- Deterministic regression test in `clients/desktop/dilla-desktop/tests/share.rs`, e.g.
  `a_share_pressed_before_the_media_takes_the_call_still_starts`: add a `test-hooks` gate (pattern of
  `put_test_room_open`, `mod.rs:1655`) that holds the session's `run()` before it enters `in_call` (e.g.
  `voice::hold_media_pickup(window, Rc<Cell<bool>>)` read in `run()` via a `watch`), join, wait for `phase == In`,
  press Share, pick `screen:1`, release the hold, and assert `share.screen` turns on and `rec.opened() == 1`. Without
  the fix it fails exactly like CI; with it it passes. A second case: a press tagged with an old call id (join, leave,
  rejoin while held) is still dropped (`rec.opened() == 0`), keeping the original intent.
- Until the hook exists the gdb script `scratchpad/loop/delay.py` is a deterministic external repro: it must pass
  after the fix.
- Then loop `closing_the_owner_window_stops_its_capture` (and the whole `--test share` binary) >= 50x on 4 pinned
  cores with busy loops; zero failures expected.
