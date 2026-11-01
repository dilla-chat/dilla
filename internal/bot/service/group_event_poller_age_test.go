package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

// ageTestQueue holds two command events from private interaction groups.
func ageTestQueue() *durableGroupEventQueue {
	community := id.New()
	first, second := id.New(), id.New()
	return &durableGroupEventQueue{binding: native.Binding{Instance: id.New(), Community: community, Installation: id.New()},
		events:      []native.GroupEvent{{DeliveryID: 1, Community: community, Group: first}, {DeliveryID: 2, Community: community, Group: second}},
		interaction: []id.ID{first, second}}
}

// Jonas 2026-10-30: a text-channel (automatic moderation) event keeps 24 hours, not one.
func TestGroupEventPollerGivesTextEventsTwentyFourHours(t *testing.T) {
	q := ageTestQueue()
	q.interaction = q.interaction[1:] // event 1 is from a text group; event 2 stays a command event
	fail := errors.New("moderation action endpoint unavailable")
	handler := groupEventHandlerFunc(func(_ context.Context, e native.GroupEvent) error {
		if e.DeliveryID == 1 {
			return fail
		}
		return nil
	})
	start := time.Unix(1_800_000_000, 0)
	for _, at := range []time.Time{start, start.Add(2 * time.Hour), start.Add(TextEventMaxPendingAge - time.Second)} {
		result, err := pollAt(t, context.Background(), q, at, handler)
		if !errors.Is(err, fail) || q.acked != 0 || len(result.Skipped) != 0 {
			t.Fatalf("at +%s: err=%v acked=%d skipped=%v; a text event younger than 24 h must stay pending", at.Sub(start), err, q.acked, result.Skipped)
		}
	}
	result, err := pollAt(t, context.Background(), q, start.Add(TextEventMaxPendingAge), handler)
	if err != nil || q.acked != 2 || len(result.Skipped) != 1 || result.Skipped[0].DeliveryID != 1 ||
		!result.Skipped[0].FirstFailure.Equal(start) || !errors.Is(result.Skipped[0].Cause, ErrGroupEventPendingTooLong) || !errors.Is(result.Skipped[0].Cause, fail) {
		t.Fatalf("at 24 h: err=%v acked=%d skipped=%+v", err, q.acked, result.Skipped)
	}
}

func TestGroupEventPollerNeverAgesOutDurableModerationCompletion(t *testing.T) {
	for _, stage := range []string{"checkpoint_persistence", "pending_capacity", "case_state_write", "pending_completion"} {
		t.Run(stage, func(t *testing.T) {
			q := ageTestQueue()
			q.interaction = q.interaction[1:]
			fail := AutoModerationStageError{Stage: stage, cause: ErrModerationCheckpoint}
			handler := failingHead(fail)
			start := time.Unix(1_800_000_000, 0)
			for _, at := range []time.Time{start, start.Add(time.Hour), start.Add(2 * time.Hour), start.Add(48 * time.Hour)} {
				result, err := pollAt(t, t.Context(), q, at, handler)
				if err == nil || q.acked != 0 || len(result.Skipped) != 0 {
					t.Fatalf("durable %s abandoned at %s: result=%+v ack=%d err=%v", stage, at.Sub(start), result, q.acked, err)
				}
			}
			result, err := pollAt(t, t.Context(), q, start.Add(49*time.Hour), groupEventHandlerFunc(func(context.Context, native.GroupEvent) error { return nil }))
			if err != nil || result.Handled != 2 || q.acked != 2 {
				t.Fatalf("completion did not resume: %+v ack=%d err=%v", result, q.acked, err)
			}
		})
	}
}

// Jonas 2026-10-30: an event HELD because its sender's trust cannot be refreshed is dropped after
// one hour (with the same three failed passes), whether it is a command or a text-channel event;
// it is never dispatched. A text event failing for any other reason keeps its 24 hours.
func TestGroupEventPollerDropsAHeldEventAfterAnHourWhateverItsGroup(t *testing.T) {
	for _, kind := range []string{"text", "interaction"} {
		t.Run(kind, func(t *testing.T) {
			q := ageTestQueue()
			if kind == "text" {
				q.interaction = q.interaction[1:] // event 1 is from a text group
			}
			stale := id.New()
			q.events[0].SenderUser = stale
			dispatched := 0
			handler := HoldUnrefreshedSenders(groupEventHandlerFunc(func(_ context.Context, e native.GroupEvent) error {
				if e.DeliveryID == 1 {
					dispatched++
				}
				return nil
			}), map[id.ID]bool{stale: true})
			start := time.Unix(1_800_000_000, 0)
			for _, at := range []time.Time{start, start.Add(10 * time.Minute), start.Add(InteractionEventMaxPendingAge - time.Second)} {
				result, err := pollAt(t, context.Background(), q, at, handler)
				if !errors.Is(err, ErrGroupEventHeld) || q.acked != 0 || len(result.Skipped) != 0 {
					t.Fatalf("at +%s: err=%v acked=%d skipped=%v; a held event younger than an hour stays pending", at.Sub(start), err, q.acked, result.Skipped)
				}
			}
			result, err := pollAt(t, context.Background(), q, start.Add(HeldEventMaxPendingAge), handler)
			if err != nil || len(result.Skipped) != 1 || result.Skipped[0].DeliveryID != 1 || !errors.Is(result.Skipped[0].Cause, ErrGroupEventHeld) {
				t.Fatalf("at one hour: err=%v skipped=%+v; the held event must be dropped", err, result.Skipped)
			}
			if dispatched != 0 {
				t.Fatalf("the held event was dispatched %d times", dispatched)
			}
		})
	}
}

// The held-event age counts only consecutive HELD passes: a pass that fails for another reason
// resets it, so an event that was held, then failed otherwise, then is held again only becomes
// droppable an hour (and three held passes) after the hold resumed.
func TestGroupEventPollerHeldAgeCountsOnlyConsecutiveHeldPasses(t *testing.T) {
	q := ageTestQueue()
	q.interaction = q.interaction[1:] // a text event: its non-held limit is 24 hours
	stale := id.New()
	q.events[0].SenderUser = stale
	other := errors.New("moderation action endpoint unavailable")
	held := HoldUnrefreshedSenders(groupEventHandlerFunc(func(context.Context, native.GroupEvent) error { return nil }), map[id.ID]bool{stale: true})
	failing := groupEventHandlerFunc(func(_ context.Context, e native.GroupEvent) error {
		if e.DeliveryID == 1 {
			return other
		}
		return nil
	})
	start := time.Unix(1_800_000_000, 0)
	pass := func(at time.Duration, handler GroupEventHandler) GroupEventPollResult {
		t.Helper()
		result, _ := pollAt(t, context.Background(), q, start.Add(at), handler)
		return result
	}
	pass(0, held)
	pass(20*time.Minute, held)
	pass(40*time.Minute, failing) // a non-held failure resets the hold age
	resumed := 50 * time.Minute
	for _, at := range []time.Duration{resumed, resumed + 20*time.Minute, resumed + 40*time.Minute, resumed + HeldEventMaxPendingAge - time.Second} {
		if result := pass(at, held); len(result.Skipped) != 0 || q.acked != 0 {
			t.Fatalf("at +%s: skipped=%v acked=%d; the hold resumed at +%s, so it is younger than an hour", at, result.Skipped, q.acked, resumed)
		}
	}
	result := pass(resumed+HeldEventMaxPendingAge, held)
	if len(result.Skipped) != 1 || !result.Skipped[0].FirstFailure.Equal(start.Add(resumed)) || !errors.Is(result.Skipped[0].Cause, ErrGroupEventHeld) {
		t.Fatalf("an hour after the hold resumed: skipped=%+v; want the held event dropped with the resumed hold time", result.Skipped)
	}
}

// A text event that fails for a reason other than a held sender still keeps 24 hours, even after
// held events got the shorter limit.
func TestGroupEventPollerKeepsTwentyFourHoursForAFailingTextEventThatIsNotHeld(t *testing.T) {
	q := ageTestQueue()
	q.interaction = q.interaction[1:]
	fail := errors.New("moderation action endpoint unavailable")
	handler := HoldUnrefreshedSenders(groupEventHandlerFunc(func(_ context.Context, e native.GroupEvent) error {
		if e.DeliveryID == 1 {
			return fail
		}
		return nil
	}), map[id.ID]bool{id.New(): true}) // someone else is held; event 1's sender is not
	start := time.Unix(1_800_000_000, 0)
	for _, at := range []time.Time{start, start.Add(time.Hour), start.Add(2 * time.Hour)} {
		result, err := pollAt(t, context.Background(), q, at, handler)
		if !errors.Is(err, fail) || q.acked != 0 || len(result.Skipped) != 0 {
			t.Fatalf("at +%s: err=%v acked=%d skipped=%v; a failing text event keeps 24 h", at.Sub(start), err, q.acked, result.Skipped)
		}
	}
}

// pollAt builds a fresh poller per call, as every reconcile pass and every restart does.
func pollAt(t *testing.T, ctx context.Context, q *durableGroupEventQueue, at time.Time, handler GroupEventHandler) (GroupEventPollResult, error) {
	t.Helper()
	p, err := newGroupEventPoller(q)
	if err != nil {
		t.Fatal(err)
	}
	p.now = func() time.Time { return at }
	return p.PollOnce(ctx, handler)
}

func TestGroupEventPollerAgeLimitSkipsOnlyAfterAnHourOfFailuresAndSurvivesRestarts(t *testing.T) {
	q := ageTestQueue()
	transient := errors.New("send receipt is terminal after restore")
	var handled []uint64
	handler := groupEventHandlerFunc(func(_ context.Context, event native.GroupEvent) error {
		if event.DeliveryID == 1 {
			return transient
		}
		handled = append(handled, event.DeliveryID)
		return nil
	})
	start := time.Unix(1_800_000_000, 0)
	// Failing for just under the limit, across several fresh pollers (restarts), stays pending.
	for _, at := range []time.Time{start, start.Add(10 * time.Minute), start.Add(InteractionEventMaxPendingAge - time.Second)} {
		result, err := pollAt(t, context.Background(), q, at, handler)
		if !errors.Is(err, transient) || q.acked != 0 || len(result.Skipped) != 0 || len(handled) != 0 {
			t.Fatalf("at +%s: err=%v acked=%d skipped=%v; a failure younger than the limit must stay pending", at.Sub(start), err, q.acked, result.Skipped)
		}
	}
	// At the limit the first-failure time recorded by the first poller still counts.
	result, err := pollAt(t, context.Background(), q, start.Add(InteractionEventMaxPendingAge), handler)
	if err != nil || q.acked != 2 || len(handled) != 1 || handled[0] != 2 || len(result.Skipped) != 1 {
		t.Fatalf("at the limit: err=%v acked=%d handled=%v skipped=%v", err, q.acked, handled, result.Skipped)
	}
	skip := result.Skipped[0]
	if skip.DeliveryID != 1 || !skip.FirstFailure.Equal(start) || !errors.Is(skip.Cause, ErrGroupEventPendingTooLong) || !errors.Is(skip.Cause, transient) {
		t.Fatalf("skip = %+v; want delivery 1, its first-failure time and its last cause", skip)
	}
}

func TestGroupEventPollerAgeRestartsForANewHeadEvent(t *testing.T) {
	q := ageTestQueue()
	start := time.Unix(1_800_000_000, 0)
	fail := errors.New("transient")
	// Event 1 fails at start, then succeeds; event 2 first fails much later and gets its own age.
	if _, err := pollAt(t, context.Background(), q, start, groupEventHandlerFunc(func(context.Context, native.GroupEvent) error { return fail })); !errors.Is(err, fail) {
		t.Fatal(err)
	}
	late := start.Add(3 * InteractionEventMaxPendingAge)
	result, err := pollAt(t, context.Background(), q, late, groupEventHandlerFunc(func(_ context.Context, e native.GroupEvent) error {
		if e.DeliveryID == 2 {
			return fail
		}
		return nil
	}))
	if !errors.Is(err, fail) || q.acked != 1 || len(result.Skipped) != 0 {
		t.Fatalf("err=%v acked=%d skipped=%v; a newly failing event must not inherit an older event's age", err, q.acked, result.Skipped)
	}
}

func TestGroupEventPollerNeverSkipsUnderCancellation(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	t.Run("undeliverable error", func(t *testing.T) {
		q := ageTestQueue()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		_, err := pollAt(t, ctx, q, start, groupEventHandlerFunc(func(context.Context, native.GroupEvent) error {
			cancel()
			return goneGroupError{}
		}))
		if err == nil || q.acked != 0 {
			t.Fatalf("err=%v acked=%d; nothing may be acknowledged under cancellation", err, q.acked)
		}
	})
	t.Run("past the age limit", func(t *testing.T) {
		q := ageTestQueue()
		fail := errors.New("transient")
		if _, err := pollAt(t, context.Background(), q, start, groupEventHandlerFunc(func(context.Context, native.GroupEvent) error { return fail })); !errors.Is(err, fail) {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		_, err := pollAt(t, ctx, q, start.Add(2*InteractionEventMaxPendingAge), groupEventHandlerFunc(func(context.Context, native.GroupEvent) error {
			cancel()
			return fail
		}))
		if err == nil || q.acked != 0 {
			t.Fatalf("err=%v acked=%d; an aged event may not be skipped under cancellation", err, q.acked)
		}
	})
}

func TestGroupEventPollerKeepsEventPendingWhenItsAgeCannotBeRecorded(t *testing.T) {
	q := ageTestQueue()
	q.stateErr = errors.New("helper state unavailable")
	fail := errors.New("transient")
	start := time.Unix(1_800_000_000, 0)
	for _, at := range []time.Time{start, start.Add(5 * InteractionEventMaxPendingAge)} {
		if _, err := pollAt(t, context.Background(), q, at, groupEventHandlerFunc(func(context.Context, native.GroupEvent) error { return fail })); !errors.Is(err, fail) || q.acked != 0 {
			t.Fatalf("err=%v acked=%d; an unprovable age must keep the event pending", err, q.acked)
		}
	}
}

func TestGroupEventPollerRetriesASkipWhoseAcknowledgementFailed(t *testing.T) {
	q := ageTestQueue()
	q.ackFails = 1
	calls := 0
	handler := groupEventHandlerFunc(func(_ context.Context, e native.GroupEvent) error {
		if e.DeliveryID == 1 {
			calls++
			return goneGroupError{}
		}
		return nil
	})
	result, err := pollAt(t, context.Background(), q, time.Unix(1_800_000_000, 0), handler)
	if err == nil || q.acked != 0 || len(result.Skipped) != 0 {
		t.Fatalf("err=%v acked=%d skipped=%v; an unacknowledged skip must not be reported", err, q.acked, result.Skipped)
	}
	result, err = pollAt(t, context.Background(), q, time.Unix(1_800_000_000, 0), handler)
	if err != nil || q.acked != 2 || calls != 2 || len(result.Skipped) != 1 || result.Skipped[0].DeliveryID != 1 {
		t.Fatalf("retry: err=%v acked=%d calls=%d skipped=%v", err, q.acked, calls, result.Skipped)
	}
}

func failingHead(fail error) groupEventHandlerFunc {
	return func(_ context.Context, e native.GroupEvent) error {
		if e.DeliveryID == 1 {
			return fail
		}
		return nil
	}
}

// pollUntil runs failing passes at each time and reports the pass that first skipped event 1.
func pollUntil(t *testing.T, q *durableGroupEventQueue, handler GroupEventHandler, times ...time.Time) int {
	t.Helper()
	for i, at := range times {
		result, _ := pollAt(t, context.Background(), q, at, handler)
		if len(result.Skipped) > 0 {
			return i
		}
	}
	return -1
}

// I-A: a first-failure time far in the future (the clock went backwards) restarts the age at now.
func TestGroupEventPollerResetsAFirstFailureFromTheFuture(t *testing.T) {
	q := ageTestQueue()
	fail := errors.New("transient")
	start := time.Unix(1_800_000_000, 0)
	future := start.Add(365 * 24 * time.Hour)
	if got := pollUntil(t, q, failingHead(fail), future); got != -1 {
		t.Fatal("skipped on the first pass")
	}
	// The clock is corrected a year back: the age restarts at start, and the skip comes an hour later.
	got := pollUntil(t, q, failingHead(fail), start, start.Add(10*time.Minute), start.Add(20*time.Minute),
		start.Add(InteractionEventMaxPendingAge-time.Second), start.Add(InteractionEventMaxPendingAge))
	if got != 4 || q.acked != 2 {
		t.Fatalf("skipped at pass %d acked=%d; want the skip exactly one hour after the reset", got, q.acked)
	}
	// Within the tolerance a slightly future first failure is kept, not reset.
	q2 := ageTestQueue()
	if pollUntil(t, q2, failingHead(fail), start.Add(4*time.Minute)) != -1 {
		t.Fatal("skipped on the first pass")
	}
	if got := pollUntil(t, q2, failingHead(fail), start, start.Add(time.Minute),
		start.Add(4*time.Minute+InteractionEventMaxPendingAge-time.Second), start.Add(4*time.Minute+InteractionEventMaxPendingAge)); got != 3 {
		t.Fatalf("skipped at pass %d; a first failure within the 5-minute tolerance must be kept", got)
	}
}

// I-B: a forward clock jump alone never skips; at least three failed passes are required.
func TestGroupEventPollerNeedsThreeFailedPassesBesidesTheAge(t *testing.T) {
	q := ageTestQueue()
	fail := errors.New("transient")
	start := time.Unix(1_800_000_000, 0)
	if got := pollUntil(t, q, failingHead(fail), start, start.Add(2*time.Hour)); got != -1 {
		t.Fatalf("skipped at pass %d after a 2 h jump with two failed passes", got)
	}
	if got := pollUntil(t, q, failingHead(fail), start.Add(2*time.Hour+time.Second)); got != 0 || q.acked != 2 {
		t.Fatalf("third failed pass past the age: skipped=%d acked=%d", got, q.acked)
	}
}

// m1: an unreadable or unknown-version record is replaced, so the limit still applies.
func TestGroupEventPollerReplacesAnUnreadableFailureRecord(t *testing.T) {
	for name, value := range map[string][]byte{
		"not a record":    mustCBOR(t, []any{"garbage"}),
		"unknown version": mustCBOR(t, []uint64{99, 1, 2}),
		"version 1":       mustCBOR(t, []uint64{1, 1, 1_000}),
	} {
		t.Run(name, func(t *testing.T) {
			q := ageTestQueue()
			key := fmt.Sprintf("%d/%s", groupEventFailureArea, groupEventFailureKey+q.binding.Installation.String())
			q.state = map[string]native.StateValue{key: {Revision: 7, Value: value}}
			fail := errors.New("transient")
			start := time.Unix(1_800_000_000, 0)
			got := pollUntil(t, q, failingHead(fail), start, start.Add(time.Minute), start.Add(2*time.Minute), start.Add(InteractionEventMaxPendingAge))
			if got != 3 || q.acked != 2 {
				t.Fatalf("skipped at pass %d acked=%d; an unreadable record must be replaced and the limit applied", got, q.acked)
			}
		})
	}
}

// m2: a reused delivery ID with a different group or source sequence does not inherit the age.
func TestGroupEventPollerDoesNotLetAReusedDeliveryIDInheritAnAge(t *testing.T) {
	q := ageTestQueue()
	fail := errors.New("transient")
	start := time.Unix(1_800_000_000, 0)
	pollUntil(t, q, failingHead(fail), start, start.Add(time.Minute), start.Add(2*time.Minute))
	// The helper journal is rebuilt; delivery 1 now names another event.
	q.events[0].SourceSequence = 9
	if got := pollUntil(t, q, failingHead(fail), start.Add(2*time.Hour), start.Add(2*time.Hour+time.Minute), start.Add(2*time.Hour+2*time.Minute)); got != -1 {
		t.Fatalf("a new event under a reused delivery ID was skipped at pass %d with the old age", got)
	}
	q.events[0].Group = id.New()
	q.interaction = append(q.interaction, q.events[0].Group)
	if got := pollUntil(t, q, failingHead(fail), start.Add(3*time.Hour), start.Add(3*time.Hour+time.Minute), start.Add(3*time.Hour+2*time.Minute)); got != -1 {
		t.Fatalf("a new group under a reused delivery ID was skipped at pass %d with the old age", got)
	}
}

// m3: if the interaction-group lookup fails, the event gets the 24-hour limit.
func TestGroupEventPollerUsesTwentyFourHoursWhenTheKindLookupFails(t *testing.T) {
	q := ageTestQueue()
	q.interactionErr = errors.New("helper busy")
	fail := errors.New("transient")
	start := time.Unix(1_800_000_000, 0)
	if got := pollUntil(t, q, failingHead(fail), start, start.Add(time.Hour), start.Add(2*time.Hour), start.Add(TextEventMaxPendingAge-time.Second)); got != -1 {
		t.Fatalf("skipped at pass %d before 24 h although the kind was unknown", got)
	}
	if got := pollUntil(t, q, failingHead(fail), start.Add(TextEventMaxPendingAge)); got != 0 {
		t.Fatal("not skipped at 24 h")
	}
}

// m3: a lost revision race keeps the event pending; the next pass recovers.
func TestGroupEventPollerRecoversFromAFailureRecordWriteConflict(t *testing.T) {
	q := ageTestQueue()
	fail := errors.New("transient")
	start := time.Unix(1_800_000_000, 0)
	q.conflicts = 1
	if _, err := pollAt(t, context.Background(), q, start, failingHead(fail)); !errors.Is(err, fail) || q.acked != 0 {
		t.Fatalf("conflicting pass: err=%v acked=%d", err, q.acked)
	}
	if got := pollUntil(t, q, failingHead(fail), start.Add(time.Minute), start.Add(2*time.Minute), start.Add(3*time.Minute), start.Add(time.Minute+InteractionEventMaxPendingAge)); got != 3 || q.acked != 2 {
		t.Fatalf("skipped at pass %d acked=%d; the next pass must start the record", got, q.acked)
	}
}

func mustCBOR(t *testing.T, v any) []byte {
	t.Helper()
	b, err := cborx.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBusyTrustHoldDoesNotStartFailureAge(t *testing.T) {
	q := ageTestQueue()
	busy := id.New()
	q.events[0].SenderUser = busy
	dispatched := 0
	next := groupEventHandlerFunc(func(context.Context, native.GroupEvent) error { dispatched++; return nil })
	handler := HoldBusySenders(next, map[id.ID]bool{busy: true})
	start := time.Unix(1800000000, 0)
	for _, at := range []time.Time{start, start.Add(time.Hour), start.Add(24 * time.Hour)} {
		result, err := pollAt(t, t.Context(), q, at, handler)
		if !errors.Is(err, ErrGroupEventTrustBusy) || q.acked != 0 || len(result.Skipped) != 0 || dispatched != 0 {
			t.Fatalf("busy hold: result=%+v err=%v ack=%d dispatched=%d", result, err, q.acked, dispatched)
		}
		if len(q.state) != 0 {
			t.Fatal("trust contention persisted failure age")
		}
	}
	held := HoldUnrefreshedSenders(next, map[id.ID]bool{busy: true})
	if _, err := pollAt(t, t.Context(), q, start.Add(25*time.Hour), held); !errors.Is(err, ErrGroupEventHeld) {
		t.Fatal(err)
	}
	for _, value := range q.state {
		var record groupEventFailureRecord
		if err := cborx.Unmarshal(value.Value, &record); err != nil {
			t.Fatal(err)
		}
		if record.HeldAttempts != 1 || record.HeldFirst != uint64(start.Add(25*time.Hour).Unix()) {
			t.Fatalf("actual trust failure inherited busy age: %+v", record)
		}
	}
}

func TestBusyTrustHoldLeavesExistingFailureHistoryUntouched(t *testing.T) {
	q := ageTestQueue()
	busy := id.New()
	q.events[0].SenderUser = busy
	next := groupEventHandlerFunc(func(context.Context, native.GroupEvent) error { return nil })
	start := time.Unix(1800000000, 0)
	_, _ = pollAt(t, t.Context(), q, start, HoldUnrefreshedSenders(next, map[id.ID]bool{busy: true}))
	before := make(map[string]native.StateValue)
	for key, value := range q.state {
		before[key] = value
	}
	_, err := pollAt(t, t.Context(), q, start.Add(2*time.Hour), HoldBusySenders(next, map[id.ID]bool{busy: true}))
	if !errors.Is(err, ErrGroupEventTrustBusy) || q.acked != 0 {
		t.Fatalf("busy hold err=%v ack=%d", err, q.acked)
	}
	for key, value := range q.state {
		if value.Revision != before[key].Revision || !bytesEqual(value.Value, before[key].Value) {
			t.Fatal("busy hold mutated existing failure record")
		}
	}
}

func TestGroupEventCooperativeStopFinishesCurrentHandlerAndAcknowledgement(t *testing.T) {
	q := ageTestQueue()
	stop := false
	ctx := client.WithCooperativeStop(t.Context(), func() bool { return stop })
	var dispatched []uint64
	handler := groupEventHandlerFunc(func(ctx context.Context, event native.GroupEvent) error {
		dispatched = append(dispatched, event.DeliveryID)
		stop = true
		if err := client.CooperativeCheckpoint(ctx); err != nil {
			t.Fatalf("current event interrupted: %v", err)
		}
		return nil
	})
	result, err := pollAt(t, ctx, q, time.Unix(1800000000, 0), handler)
	if !errors.Is(err, client.ErrCooperativeStop) || q.acked != 1 || result.AcknowledgedThrough != 1 || len(dispatched) != 1 {
		t.Fatalf("stop result=%+v err=%v ack=%d dispatch=%v", result, err, q.acked, dispatched)
	}
	if len(q.state) != 0 {
		t.Fatal("cooperative stop persisted a failure age")
	}
}
