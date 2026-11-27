# G3 timing flake — diagnosis (gpui-linux-x64-media, 2026-11-26 14:19–14:20Z)

Worktree: `/home/thim/Repositories/dilla/.claude/worktrees/signal-design` (fix/desktop-test-flakes = origin/main, efd94e910).
Log: `media-a1.log` in the scratchpad (failed attempt). Nothing was edited, committed or built.

## TL;DR

Neither failure is CPU starvation. Both are product latency on the Bot side, and each comes from a specific wait,
not from CPU. The test cadence makes one of them worse.

1. **Queued-track gap (256 ms)**, classified **(a) product bug**, two parts:
   - **Every track loses its last ~1 s.** The publisher unpublishes a track as soon as the *encoder* has read the
     source to EOF. At that point the bounded provider queue still holds up to 50 × 20 ms = 1 s of audio, and
     `UnpublishTrack → CloseTrack` throws it away.
   - Because of that, the switch to the next track is **fully serial**: unpublish, then a durable helper `Save` at
     background priority, then `Start`, then publish/renegotiate, then the SFU's subscriber renegotiation, then the
     receiver's new stream. Whenever the `Save` stalls (410 ms in the failed run, 1–47 ms in the passing runs), the
     listener hears the stall. The test can't see the truncation, and the receiver's packet-loss concealment
     usually covers short switches. That is why it flakes instead of failing every time.
2. **Repeated start p95 (503 ms)**, classified **(a) latent product bug made worse by the test's cadence (b)**.
   The Bot's HTTP **read** budget (8/s, burst 40) runs out after about 10 back-to-back `/stop`+`/play` cycles.
   From then on, every start's call-group catch-up `GET groups/{id}` (and something earlier on the intake path)
   waits for the next 125 ms token. The Bot-side timestamps line up on an exact 125 ms grid, and the start
   latency climbs steadily across the 20 runs.

## Environment / concurrency (rules out (c) "parallel tests")

- The job is the standard hosted `ubuntu-24.04` image (log lines 16–20, Azure westus2). The log doesn't print `nproc`.
  A hosted runner has 4 vCPU for a public repo and 2 for a private one.
- CI runs the four G3 tests in a separate invocation with `--test-threads=1`
  (`.github/workflows/desktop-gpui.yml:525`, log line 2460). They also share the `MUSIC` mutex
  (`clients/desktop/dilla-media/tests/call.rs:401`). So in that binary nothing else ran beside them. The only
  other processes were this test's own dillad, SFU, Bot, helper and the two `call-peer`s.
- The Bot-side stages are fast at the millisecond level the whole time: helper reads take about 1 ms and
  `track_publish` about 0.5 ms (e.g. 14:19:43.988→43.993, all 9 stages in 5 ms). A starved CPU would stretch every
  stage. Instead the latency sits in two discrete waits (below).

## 1. `bot_next_queued_track_gap_is_under_150_ms` — 256 ms

### Evidence: every 5 s track plays for only ~4.0 s

Bot log, stage `published` of the 5 s track to `bot music playback ended` (eof):

| run | published | ended | played |
|---|---|---|---|
| 0 | 14:19:39.556 | 14:19:43.578 | **4.02 s** |
| 1 | 14:19:46.555 | 14:19:50.580 | **4.03 s** |
| 2 | 14:19:53.220 | 14:19:57.248 | **4.03 s** |

On the receiver, run 0's edges are `[(3953, false), (4209, true)]` (log line 5014). The first track went quiet
**3953 ms** after it became audible, not ~5000 ms.

### Why (code path: "track ends" → unpublish)

1. `cmd/dillabot/music_runtime.go:492`: the provider queue has `Capacity: 50`, i.e. 1 s of 20 ms Opus packets
   (`internal/media/live_opus.go:18-20`).
2. `internal/bot/music/runtime/runtime.go:49-55`: the session's `done` closes when `encode.BridgeOgg` returns, which
   happens once ffmpeg's last packet has been *written into the queue* (`bridge.go:44-63`: `CloseInput` on return).
   It does not wait for the queue to drain. ffmpeg decodes faster than real time, so the queue is full at EOF.
3. `internal/bot/music/livekit/pool.go:808-815` (`monitorPooled`): `case <-sourceDone: s.finish(s.source.Wait())`.
4. `internal/bot/music/livekit/publisher.go:279-298` (`finish`): calls `s.unpublish()`, which is
   `LocalParticipant.UnpublishTrack` (`pool.go:125`). In server-sdk-go
   `localparticipant.go:692-695` that runs `pub.unpublish`, then `Negotiate()`, then **`pub.CloseTrack()`**, which
   stops the write loop with the queued second still unsent.
5. `media.PublishLiveOpus` passes `nil` as `onWriteComplete` (`internal/media/publish.go:43-45`), so nothing anywhere
   waits for the track to actually finish writing.

### Why the gap is then serial and variable (code path: unpublish → next track's first frame)

When `finish` returns, `Session.done` closes and `Supervisor.monitor` wakes
(`internal/bot/music/supervisor.go:862-987`):

1. `Apply(OpComplete)` (pure).
2. **`s.store.Save(saveCtx, …)`** (`supervisor.go:963`). This becomes `persistence.Store.Save` and then
   `native.StateTransaction` (`internal/bot/music/persistence/store.go:217`). It is a durable write on the Bot's
   **single helper gate** (`internal/bot/native/client.go:157`, `gate.Acquire(ctx, RequestPriority(ctx))`) with
   SQLite `journal_mode=WAL; synchronous=FULL` (`core/dilla-bot-core/src/store.rs:70,80`), so every commit is an
   fsync. It runs under `s.ctx`, which carries neither `WithInteractive` nor `WithImmediateControl`. That makes it
   **background priority**, and it yields the helper to interactive work (`internal/bot/client/rate_limit.go:118-130`).
3. `startLocked(…, explicit=false)` uses `client.WithBackground` (`supervisor.go:703-707`), then
   `QueueStarter.Start` (`session/session.go:188`), then `Pool.startSource` (`livekit/pool.go:284-417`). That calls
   catch-up (helper `InspectGroup` + dillad `GET groups/{id}`), then `CallMediaEpoch`, `Admit`, the spare counter
   block, `revalidateEpoch` and `PublishTrack`. `PublishTrack` does an AddTrack signal round trip plus a 20 ms
   debounced renegotiation, and frames start only `OnBind` (`internal/media/publish.go:83-86`).
4. Then the SFU sends a subscriber offer to the receiver, the receiver creates a new stream (a new `<device>:<sid>`,
   `dilla-media/src/room.rs:183-184`), and NetEq fills and decodes.

"ended" log to the first `Start` stage (`provider_check`). That window is essentially just step 2, the `Save`:

| run | ended | provider_check | Save window | Start (check→published) | receiver gap |
|---|---|---|---|---|---|
| 0 | 43.578 | **43.988** | **410 ms** | 5 ms | **256 ms** (fail) |
| 1 | 50.580 | 50.626 | 47 ms | 5 ms | 0 (edges `[]`) |
| 2 | 57.248 | 57.249 | 1 ms | 4 ms | 0 (edges `[]`) |

The receiver hears less than the Bot-side window (256 of ~415 ms). After RTP stops, the old stream stays "audible"
through its jitter buffer, PLC and the unsubscribe renegotiation (`room.rs:250-262`: a quiet edge comes only when
the decoded RMS drops or the reader drops). When the switch is short (runs 1 and 2), the new stream becomes audible
before the old one goes quiet, so `heard()` (`room.rs:193-208`) records **no edge at all**. So the test only fails
when the serial switch runs longer than the receiver's concealment tail. That depends on one durable helper write:
a WAL fsync on a shared Azure disk, or the background-priority `Save` waiting behind interactive helper work. The
logs can't tell which, because the helper's gate waits are not logged.

A structural coincidence: a 5 s fixture minus the dropped 1 s is ~4.0 s = 2 × `epochPollInterval` (2 s,
`publisher.go:66`). So the track always ends ~25 ms after the old publication's monitor tick (catch-up + epoch read
on the same helper). In run 1 the 47 ms is probably that tick's helper work still in flight.

### Classification: (a)

A real listener hears (i) the last ~1 s of every track cut off, at every track change, on every machine, and
(ii) a gap at the switch whenever one helper fsync or a gate wait is slow. A busy Bot (many rooms, many listeners'
sealed reads) makes the gate wait more likely in production than in CI. The test is also partly (b): it can't see
the truncation, and PLC overlap hides switches shorter than ~150–250 ms.

## 2. `bot_repeated_track_starts_within_500_ms_at_p95` — 503 ms

### Evidence: read-token waits, 125 ms grid, latency rises over the session

Values in run order (log line 5466): `181, 276, 220, 349, 231, 292, 503, 281, 334, 290, 349, 374, 451, 463, 353, 460,
523, 403, 470, 441`. The first 10 have a mean of ~296 ms and the last 10 ~419 ms.

Bot stages, early run vs late run:

- 14:20:29.2596 `call_group_join` → 29.2624 `call_catch_up`: **2.8 ms**
- 14:20:36.3425 → 36.4237: **81 ms**; 37.3503 → 37.4232: **73 ms**; 38.4650 → 38.5484: **83 ms**;
  39.5906 → 39.6735: **83 ms**

The late catch-ups end on a 125 ms grid: .4237, .4232, .5484, .6735. So do the late `provider_check`s, which come
before any music start work: 36.3006, 37.3010, 38.4250, 39.5500, 40.9255, 41.9263, 44.0506, 45.1754 (all ≡ .050
mod .125). So something on the command intake path also waits for a read token.

### Why

- `internal/bot/client/rate_limit.go:70-71`: default `ReadPerSecond 8, ReadBurst 40`, which is 1 token per
  **125 ms** after the burst. Every GET is class `read` (`rate_limit.go:283-285`).
- Every start's catch-up does `c.remote(ctx, group)`, i.e. `Session.GroupInfo` → `GET groups/{id}`
  (`cmd/dillabot/music_call_guards.go:104-109`, `internal/bot/client/api.go:851`). It does this even when the helper
  is already current (the read is how it finds out). The catch-up is `WithImmediateControl`, so it gets the *next*
  token first, but it still has to wait for that token to refill.
- The burst is spent by the test's tight loop: 20 × (`/stop`, wait quiet, `/play`) in ~17 s, plus each playing
  track's monitor tick (`pool.go:790`, catch-up GET every 2 s). There is also a `client_revision_stale` refusal of
  op 16 on **every** `/stop` (one WARN per cycle, e.g. 14:20:28.875, 29.403, …), which costs a refused round trip
  and a retry each cycle. A rough budget: the bucket ran dry ~8 s in (40 burst + 64 refill ≈ 104 reads), which is
  roughly 9–10 Bot reads per cycle.
- `call.rs:402-404` already notes an earlier CI spike (585 ms). The REPEAT_RUNS=20 change spread out a single
  outlier, but it can't help against a drift that grows across the session.

### Classification: (a) latent + (b) cadence

The metric (Enter → first audible frame) is the right user-facing one. One user rarely runs 20 restarts in 17 s.
But the read bucket is **per Bot device**, shared by every room's 2 s monitor tick, every catch-up and the intake.
A community with several rooms playing (0.5 reads/s per playing room from ticks alone) plus normal command traffic
reaches the same state, and then every interactive `/play` pays ≥ 1–2 token waits. Not (c): the stage gaps sit
exactly on token boundaries, not on random CPU stalls.

## Proposed fixes

### Fix A: play every track to its end, and overlap the switch with the drain (gap + truncation)

Goal: the next track's publish, durable `Save`, catch-up and renegotiation all happen while the old track's last
second is still playing.

1. `internal/media/live_opus.go`: add a drained signal. `NextSample` returns `io.EOF` once `closed && size==0`; at
   that moment close a `drained chan struct{}`, exposed as `Drained() <-chan struct{}`.
2. `internal/bot/music/runtime/runtime.go`: split the session's lifecycle into `InputDone()` (today's `done`, encoder
   exited) and `Done()`. `Done()` closes on encoder exit **and** `provider.Drained()`, or on cancel/close. With only
   that change, the tail plays and nothing is cut.
3. `internal/bot/music/livekit/pool.go`: in `monitorPooled`, `finish` (unpublish) only on `Done()` (drained), not on
   input EOF. Allow one *draining* publication next to the active slot. Today `p.active != nil || p.connecting` →
   `ErrPoolBusy` (`pool.go:355-360`), so the next track can't publish while the old one drains. Alternatively, and
   preferably, keep **one persistent publication per room** and feed queue items into it through a chaining sample
   provider (swap the `FrameEncryptor`'s sender per item the way `followEpoch` already re-keys in place). That
   removes the per-track AddTrack/renegotiation on the Bot, the SFU's subscriber renegotiation, and the receiver's
   new stream. The gap becomes zero by construction, because `PrefetchLead` (30 s) already has the next track's
   audio buffered.
4. `internal/bot/music/supervisor.go` (`monitor`): start the advance (Apply, `Save`, `startLocked`) on the old
   playback's *input-ended* signal, not its `Done()`. Hold the new provider's first sample until the old one is
   drained, so the two tracks don't overlap. Run the advance's `Save` with `client.WithInteractive` (at least),
   because it is on the audible path.
5. Instrumentation (cheap, do first): log an `advance_save` stage with the class and duration of the `Save` in
   `monitor`, and log helper-gate waits over 50 ms by priority (no IDs). That tells fsync apart from gate queueing
   if it shows up again.

Tests that prove it:
- Go unit tests: `internal/media/live_opus_test.go`: `Drained` closes only after the last queued sample was handed
  out. `internal/bot/music/livekit/pool_test.go`: a pooled session with 50 queued packets at input EOF doesn't call
  `unpublish` until they are consumed. `internal/bot/music/supervisor_test.go`: with a fake starter, the next
  `Start` is called on input-EOF before the previous playback's `Done`, and a 400 ms fake `Save` causes no gap in
  the fake publisher's sample stream.
- Rust e2e (`clients/desktop/dilla-media/tests/call.rs`, `bot_next_queued_track_gap_is_under_150_ms`): also assert
  that the first track's audible span is ≥ 4.9 s (the 5 s fixture; this fails today at ~4.0 s, catching the
  truncation), and keep gap < 150 ms. Optionally inject a slow helper write (a test-host knob that delays
  `StateTransaction` by 400 ms) so the overlap is proven, not left to luck.

### Fix B: take the read token off the start's critical path (repeat start)

1. `cmd/dillabot/music_call_guards.go` (`musicCallCatchUp.catchUp`): don't GET `groups/{id}` on every start. The
   Bot already subscribes to the call group's handshake/epoch hints (`newMusicCallGuards` → `Watch`). Keep the last
   hinted head per group, and skip the remote read when `local.NextSequence` ≥ that head and the hint stream is
   connected. Fall back to the GET on reconnect or when no hint was seen. Another option: let the monitor tick reuse
   a remote head no older than its interval.
2. Find and fix the per-cycle `client_revision_stale` refusal of `/stop` (op 16). The client sends `/stop` with the
   revision from before the `/play`'s save reached it. Each refusal is a wasted sealed round trip and Bot reads.
3. Consider a separate small read bucket (or immediate-class reservation) for the music start path, so background
   ticks and app reads can't drain it. dillad's server-side meter must agree (`internal/api/dsmeter.go`).
4. Instrumentation: log read-limiter waits over 20 ms with class and route template (no IDs).

Tests that prove it:
- A Go unit test in `cmd/dillabot` (music_call_guards): with a hinted head equal to the local one, `catchUp` makes
  zero `GroupInfo` calls.
- `bot_repeated_track_starts_within_500_ms_at_p95`: add an assertion that the second half's median is not
  materially above the first half's (a drift guard). Keep 20 runs. Optionally drop the bucket to burst 1 in the
  test host to prove the start path no longer waits on read tokens.

### What not to do

Don't raise the thresholds or add retries. The 150 ms gap and the 500 ms p95 are both met with margin by the
unloaded paths (Bot start ≈ 5 ms, early repeat runs ≈ 180–290 ms). The misses come from two identifiable waits
(one durable helper write on a serial switch, and the 125 ms read-token cadence), not from CPU noise.

## Local run

None. The CI log's Bot-side timestamps already separate the stages, and a local run would need the full
dillad/Bot/SFU/wasi build. Nothing was built, so there was no `target/debug/incremental` to prune.
