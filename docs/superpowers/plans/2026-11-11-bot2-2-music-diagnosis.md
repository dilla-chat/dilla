# Music cancel / media_epoch / wrong-track diagnosis: v0.1.0-bot2.1 (09c7c28a)

This is a read-only diagnosis. All line numbers are `git show 09c7c28a:<path>`. I ran no tests.

## 1. Which ctx does playback publish on, and what ends it 6.01 s after `published`?

### The ctx chain (not the command ctx; confidence high)

- The op 12 handler calls `OnPlay = registry.playPersistedSnapshot` (`cmd/dillabot/music_runtime.go:577, 1117-1127`). That calls `Supervisor.ApplyPersistedSnapshot(ctx, …, true)`.
- `ApplyPersistedSnapshot` ignores the request ctx for playback. It calls `s.startLocked(s.ctx, true, nil)` (`internal/bot/music/supervisor.go:369`).
  - `s.ctx` is the supervisor's lifetime ctx (`supervisor.go:196, 201`), built from the reconcile pass by `newMusicSupervisor` (`music_runtime.go:730`).
- `startLocked` derives `runCtx` (`supervisor.go:541`). Only these cancel it:
  - the 45 s start timeout, which is stopped on success (`:658`);
  - `closePlaybackLocked` while the track is still starting;
  - `go func(){ <-p.Done(); cancel() }()` (`:659`), which fires after the playback has already ended.
- `session.Start` (`internal/bot/music/session/session.go:255-321`) passes that ctx to `RetainedLiveKitPublisher.Start` → `Pool.startSource` (`internal/bot/music/livekit/pool.go:258`).
- The track itself runs on `trackCtx = WithCancel(roomCtx)` (`pool.go:310`). `roomCtx` is `WithTimeout(detachedLoggerContext(ctx), 30 min)` (`pool.go:472`), which is Background-based. `stopParent = context.AfterFunc(startCtx, trackCancel)` (`pool.go:311`) ties it to `runCtx` only.
- The coordinator's timing argument agrees: the command ctx is excluded.

### Why "playback ended" means the track ended by itself

`Supervisor.monitor` logs `playback ended` only when `!s.closed && s.playback == p` (`supervisor.go:674`).

- A supervisor Close (helper flap → `registry.retire` → `queues.Close`, `music_runtime.go:1292-1310`) sets `closed` first and clears `playback` (`supervisor.go:850, 813-824`), so it never logs this line.
- A pause, skip or stop clears `playback` too.
- So these 3 lines are tracks whose publisher or encoder ended on its own.

### Timers that start at `published` (`track_publish` starts `monitorPooled`, `pool.go:376-377`)

| Candidate | Fires at | Verdict |
|---|---|---|
| `monitorPooled` ticker `epochPollInterval` = 2 s (`publisher.go:29`, `pool.go:750`) → `followEpoch` | 2, 4, 6 s … | **Matches.** `followEpoch` counts failed catch-ups; the 3rd consecutive failure (`maxCatchUpFailures = 3`, `pool.go:37, 803-805`) returns `ErrCallBehind: <catch-up error>` → `markDead` + `finish` (`pool.go:784-786`). With fast-failing catch-ups that is 6.0 s + a few ms, every time. |
| Epoch changed / Bot leaf removed under the publisher | 2 s (first tick after the change) | Refuted as the 6 s cause. A same-leaf epoch advance is re-keyed in place (`pool.go:827-849`). A changed or missing own leaf, a lookup error or an Admit refusal fails at the **first** tick (`pool.go:811-836`) with class `invalid`/`binding`, not `ctx_canceled` and not at exactly 6 s. |
| `catchUpDeadline` 3 s (`pool.go:41`) | 2+3 = 5, then about 7-8, … | Not 6.01. Timed-out catch-ups would also be class `deadline`. |
| `sourceRecoveryWindow` 5 s (`session.go:339, 388`) | — | Only classifies a source error after a *Resume* (`resumedAt` is zero on a fresh session). Not a timer. |
| `musicAdmitReadDeadline` 2 s, `musicJoinFresh` 2 s | — | Fail on the first tick or do not apply. |

**Conclusion (mechanism confidence medium-high).** The 6.01 s end is the 3-strikes rule of the pooled monitor: three consecutive `CatchUp` failures at ticks 2, 4 and 6 s, each returning quickly. No other post-publish timer gives a constant 6.0x s.

### The class `ctx_canceled` (open; confidence low on the inner error)

`FailureClass` returns `ctx_canceled` whenever anything in the chain `errors.Is(context.Canceled)` (`start_stage.go:63-65`). The catch-up is `musicCallCatchUp.run` (`cmd/dillabot/music_call_guards.go:52-92`), whose steps are local `InspectGroup`, remote `Session.GroupInfo` and `SourceSync.Sync`.

- A closed helper would give `helper.ErrClosed` (class `invalid`).
- A 3 s catch-up timeout gives `deadline`.
- I found no code that returns a bare `context.Canceled` quickly here. Candidates are:
  - a native or helper call that sees a cancelled internal ctx;
  - a gateway socket route of a superseded session;
  - the source sync's own work.
- **The fix should log the class of each failed catch-up tick**, so the next journal names it. See F1.

### Ruling on "one root cause"

This is only partly confirmed.

- The **6 s end is not a Remove advancing the epoch under the publisher.** The Remove comes 11 ms *after* the end (see 2), and an epoch or leaf change ends a track at the first tick with a different class.
- The 6 s end is the call-group **catch-up** failing three times in a row.
- What fails inside the catch-up is very likely helper- or session-state churn, which the flap fix should remove. That is not proven, because the class does not fit a plain closed helper.

## 2. Who proposes the Remove for leaf 1?

### Confidence high that dillad issues it, medium-high that it is the Bot's own device

- `proposed a Remove` is a **dillad** log line (`internal/ds/proposal.go:773`), not the Bot's. The Bot never proposes Removes.
- The DS issues it for a call group from `CallEvents.proposeLeave` (`internal/api/callevents.go:473-512`, DEV-45) when LiveKit reports a **participant left** the room.
- The sequence that fits the 11 ms gap:
  1. The track ends.
  2. `Session.finish` → `pool.markDead` → `teardown` → `room.Disconnect()` (`pool.go:784, 551-575, 589-596`).
  3. LiveKit sends `participant_left` for the Bot's identity.
  4. dillad proposes a Remove of the Bot's leaf.
- So **da5b3ce4 leaf 1 is almost certainly the Bot's own device**, its leaf in the call group b5fe2bda. Check: the `bot music operation refused` line carries `device_id=` = the Bot device (`music_interactions.go:186`, `h.DeviceID`). If that equals da5b3ce4, this is confirmed.
- The other Removes that track the flap follow the same path:
  - each flap → `retainedNativeClient` sees `!Usable` → `Disable` → `retire` → `queues.Close` → `Supervisor.Close` → `starter.Close` → `pool.Close` → room disconnect → `participant_left` → Remove;
  - also warm-abandon and idle-leave (`music_runtime.go:322-343`, `pool.LeaveIdle`).
- `queued add dropped … device=da5b3ce4` fits the same device: the DS drops a queued Add of an ineligible device (`internal/ds/proposal.go:604`).

### Is it the cause of the epoch churn in (A)?

It is a **consequence of the Bot leaving the room**, and it drives churn on the *next* start. Each Remove commit advances the call group's epoch, and the Bot's leaf goes. The next `/play` must rejoin before `media_epoch`, so a flap that keeps leaving and rejoining the room keeps moving the epoch. The Bot is not removing the owner's device.

## 3. Why `media_epoch` at start

### Which epoch the start reads (confidence high)

- `snapshot_handoff` is only the *reason label* for any `OnPlay` error (`music_interactions.go:522-529`). It reads no epoch of its own.
- The epoch is read in `Pool.startSource`:
  - optional catch-up, skipped when `FreshJoin` holds;
  - then `epochs.CallMediaEpoch(ctx, call.GroupID)` (`pool.go:276`);
  - `validateSenderEpoch`, which needs a non-zero key and the Bot's own leaf exactly once in the roster (`publisher.go:160-177`);
  - `guards.Admit` (`pool.go:280-283`).
- `epochs` is `deps.Epochs = source`, the `nativeClient` of the pass that built the starter (`music_runtime.go:491`, `main.go:688`). The group comes from `StartCall` (`music_runtime.go:208`).
- `group_id` is all zeros on the earlier stages by design. `PlaybackGroup` is set only after `PrepareCall` (`session.go:303-305`).

### What `class=media_epoch` excludes

`FailureClass` checks `Canceled`, `Deadline`, `APIError` and classified errors first (`start_stage.go:63-76`). So the error is plain:

- `ErrBinding` from `validateSenderEpoch` (own leaf missing or zero key);
- `helper.ErrClosed` or another helper error;
- or an Admit refusal: `errMusicInstallationNotReady`, `errMusicBotCannotSpeak`, `errMusicCallJoinHeld` (`music_call_guards.go:109-131`).

### Why 4 lines

- `StartExplicit` re-runs `Start` once on `ErrBinding` (`session.go:149-158`). `Start`'s own retry covers only Canceled, EpochChanged and RoomConnection (`:198-200`).
- Four lines therefore fit two explicit starts × (1 + ErrBinding retry), or two /plays. This points to **`ErrBinding`: the helper's view of the call group's current epoch does not contain the Bot's leaf** (medium confidence).
- How it goes stale:
  - The previous end's or the flap's Remove commit (2) removes the Bot's leaf.
  - The join step says OK: `already_member` runs a catch-up, while a warm call prepared earlier, or `FreshJoin`, skips the catch-up before the epoch read (`music_runtime.go:276-290, 440-443`; `pool.go:269`).
  - Or a self-closed helper (diagnosis S8/S9) still sits in the starter until the next pass.

### Does "consumers follow the current helper + never close a usable helper" fix it?

- It removes the flap-driven room leaves → Removes → epoch churn. Most `media_epoch` refusals should go (medium confidence).
- It does **not** close these:
  - **(a)** a start that meets a just-committed Remove of its own leaf still fails both attempts. `StartExplicit`'s retry calls `ResetConnection`, which is a no-op on a healthy pool, and takes a call again without forcing a rejoin;
  - **(b)** the wrong-track bug (4).
- Recommended separate fix: on `media_epoch` with `ErrBinding` (own leaf absent), forget the prepared call, rejoin the call group (fresh `JoinCallGroup`, which runs catch-up and the forgot-stale-leaf branch), re-read the epoch, and retry once.

## 4. Wrong track (confidence high)

This is not the restore replaying an older item by itself. It is the **/play resume semantics on a queue that still holds an earlier attempt as `Current`.**

**How the earlier attempt stays `Current`:**
- A failed `/play` → `pauseUnstartedPlayback` saves `OpPause` and keeps `Current` (`music_interactions.go:563-589`).
- The 6 s end → `Supervisor.monitor` → `pauseLocked` keeps `Current` (`supervisor.go:714`).
- A respawn → `persistence.Store.Load` forces `Paused=true` and keeps `Current` (`internal/bot/music/persistence/store.go:133-138`). The "retained paused" line every 10 s is just the reconcile pass log (`main.go:859`). It is not a new restore.

**What the next `/play` does:**
1. `OpEnqueue` → `appendTracks` appends the new item at the **end** of `Queue` (`internal/bot/music/queue.go:217-249`).
2. Because `play && next.Paused`, the handler applies `OpResume` (`music_interactions.go:441-450`).
3. `OpResume` keeps the existing `Current` and only unpauses it (`queue.go:141-148`). `startNext` runs only when `Current == nil`.

**Result:** the Bot resumes the earlier failed or ended attempt. The new request waits behind it. The `Now playing` note is built from `saved.State` (`music_interactions.go:533`), which is the old `Current`.

## 5. Minimal fixes

### F1 — Log per-item id, `restored`, and catch-up classes (do first)

- **Opaque per-item id already exists.** `tracks[i].ID = id.New().String()` is assigned when the item is queued (`music_interactions.go:411`). It is random and never derived from the URL, title or query. Log it as `item_id`. Never log `CanonicalID`, the URL, the title or the query.
- **`restored` flag.**
  - The supervisor records the item IDs present when it loaded its state (`NewSupervisor`'s `store.Load`, and `adoptLocked` / `RefreshState` adoptions of durable state). These are the "restored" items.
  - Items that arrive through `ApplyPersistedSnapshot` / `SchedulePersistedSnapshot` with a revision this process committed are "live".
  - `restored = item.ID ∈ restoredSet`.
- **Plumbing.**
  - Add `music.WithPlaybackItem(ctx, itemID, restored)`. In `supervisor.startLocked`, set it on `startCtx` (`supervisor.go:588`).
  - `StartStage` (`start_stage.go:43-56`) appends `item_id` and `restored` to both its lines.
  - Note: `QueueStarter.Start` replaces the ctx logger with `s.Logger` (`session.go:182-187`). Its `.With(...)` must re-add the item attributes from ctx, or StartStage must read them from ctx directly, which is preferred.
  - `Supervisor.monitor` `playback ended` (`supervisor.go:692-694`): keep the playing track's ID beside `s.playback` and log `item_id` and `restored`.
  - `operation refused` (`music_interactions.go:186`): log `item_id` = the first enqueued track's ID (or empty), plus `restored` = whether `saved.State.Current` is not one of the tracks this command enqueued.
- **Catch-up classes.**
  - In `followEpoch`, log every failed catch-up with `failures=n` and `class=FailureClass(err)` (`pool.go:802-807`).
  - On `ErrCallBehind`, log the inner class.
  - At `media_epoch`, log a sub-reason: `own_leaf_missing|zero_key|helper_closed|admit_not_ready|admit_cannot_speak|admit_held|lookup`.
- **Tests.**
  - `internal/bot/music/start_stage_test.go`: the item attributes are present and nothing from `CanonicalID` appears.
  - `supervisor_test.go`: the `playback ended` line carries `item_id` and `restored=true` for an item loaded from the store, `false` for a committed /play.
  - `music_interactions_test.go`: the refused line carries `item_id`, and a log-capture assertion finds no URL or query in any music line.
  - `pool_test.go`: three failing catch-ups log three classed lines and end at the 3rd tick.

### F2 — `/play` plays the requested item

- In the handler's play path (`music_interactions.go:441-450`), when nothing is playing, make the newly enqueued first track `Current`:
  - add `music.OpPlayNow`, or apply `appendTracks` at the head and then `skip` past the stale `Current`;
  - put the stale `Current` back at the head of `Queue`, or drop it. **This is a product decision for Jonas.**
- Tests:
  - `queue_test.go`: PlayNow on a paused state with a Current.
  - `music_interactions_test.go`: a /play after a failed /play starts the new item; the `Now playing` note names it.

### F3 — Retry a start once after the Bot's own leaf was removed

- In `QueueStarter.StartExplicit` / `musicCallPrep.take` (`session.go:135-159`, `music_runtime.go:416-458`): on a `media_epoch` StartError whose cause is `ErrBinding`, call `prep.forget()` and run a fresh `prepare` (`JoinCallGroup` with catch-up) once before the second `Start`.
- Do not count `FreshJoin` for a group whose own leaf was missing.
- Test (`music_runtime_test.go`): the first epoch has no own leaf → rejoin → the second epoch is valid → published.

### F4 — Follow-up, depends on F1's classes

If the 6 s end proves to be a closed or superseded helper or session seen by `musicCallCatchUp`:

- make the catch-up dependencies (`local`, `remote`, `sync`, `Epochs`) resolve the registry's current helper and session per call ("consumers follow the current helper"), instead of the pass-captured ones (`main.go:680-683`, `music_runtime.go:491`);
- treat `helper.ErrClosed` in the catch-up as terminal (`ErrCallCatchUpTerminal`), with a clear class, instead of burning three ticks.

### Expected effect of the helper fix alone

It removes the flap → leave → Remove churn, and probably most of the `media_epoch` refusals and the 6 s ends. The wrong-track bug (F2) remains in every case. The stale-own-leaf start (F3) remains after any ordinary track end. The 6 s class cannot be confirmed without F1.
