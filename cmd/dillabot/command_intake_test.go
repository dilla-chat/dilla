//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/service"
	"github.com/jonasthim/dilla/internal/id"
)

type namedGroupSync struct {
	steps *[]string
	err   error
}

func (s namedGroupSync) ReconcileInteractionGroupOnce(_ context.Context, _ *client.Session, group id.ID) (service.MembershipResult, error) {
	*s.steps = append(*s.steps, "interaction_sync:"+group.String())
	return service.MembershipResult{GroupsSynced: 1}, s.err
}

// The gateway fast path for a participant's message reads that one interaction group, refreshes
// trust, then dispatches; it never runs the text-channel membership sweep or the sweep of every
// interaction group, which stay on the throttled general wake and on reconciliation.
func TestInteractionFastPathSyncsOnlyTheMessagedGroupBeforeDispatch(t *testing.T) {
	group := id.New()
	for _, denied := range []bool{false, true} {
		var steps []string
		refusal := errors.New("private source denied")
		sync := namedGroupSync{steps: &steps}
		if denied {
			sync.err = refusal
		}
		poll := newInteractionIntakePoller(sync, readinessTrust{steps: &steps}, nil, commandDrainFunc(func(context.Context) (service.GroupEventPollResult, error) {
			steps = append(steps, "dispatch")
			return service.GroupEventPollResult{}, nil
		}), noUnrefreshed(t))
		err := poll(t.Context(), group)
		want := []string{"interaction_sync:" + group.String(), "trust", "dispatch"}
		if denied {
			want = want[:1]
			if !errors.Is(err, refusal) {
				t.Fatalf("error=%v, want source refusal", err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(steps, want) {
			t.Fatalf("steps=%v, want %v", steps, want)
		}
	}
}

// pendingLeafCore models the native core: a message from a leaf whose signed device list is not
// yet validated (E_DEVICE_LIST_STALE) is held in app_pending_history until the trust pass
// revalidates the leaf; Drain dispatches only released events.
type pendingLeafCore struct{ held, released bool }

func (c *pendingLeafCore) ReconcileInteractionGroupOnce(context.Context, *client.Session, id.ID) (service.MembershipResult, error) {
	c.held = true // the command arrives held for its sender's device list
	return service.MembershipResult{GroupsSynced: 1}, nil
}
func (c *pendingLeafCore) RefreshPendingLeaves(context.Context, *client.Session) (service.TrustRefresh, error) {
	if !c.held {
		return service.TrustRefresh{}, nil
	}
	c.held, c.released = false, true
	return service.TrustRefresh{Recovered: 1}, nil
}

// A command held for its sender's device list reaches dispatch on the fast path: the fast path
// runs the trust pass after its group sync and before Drain, so the leaf that sync held is
// revalidated in the same poll. (The full path runs trust before its interaction sweep; a leaf
// that sweep holds waits for the next pass.)
func TestInteractionFastPathReleasesAPendingLeafBeforeDispatch(t *testing.T) {
	core := &pendingLeafCore{}
	dispatched := false
	poll := newInteractionIntakePoller(core, core, nil, commandDrainFunc(func(context.Context) (service.GroupEventPollResult, error) {
		dispatched = core.released
		return service.GroupEventPollResult{}, nil
	}), noUnrefreshed(t))
	if err := poll(t.Context(), id.New()); err != nil {
		t.Fatal(err)
	}
	if !dispatched {
		t.Fatal("the command from a pending leaf was still held when the fast path dispatched")
	}
}

// The intake paths report how many events the queue skipped as undeliverable, as a count only:
// nothing about the events (delivery IDs, senders, causes) reaches that log.
func TestIntakeReportsSkippedEventsAsACountOnly(t *testing.T) {
	inner := skippedDrain{result: service.GroupEventPollResult{Skipped: []service.SkippedGroupEvent{{DeliveryID: 4}, {DeliveryID: 9}}}}
	var counts []int
	drain := countSkipped{holdingEventDrainer: inner, report: func(n int) { counts = append(counts, n) }}
	if _, err := drain.DrainHolding(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(counts, []int{2}) {
		t.Fatalf("reported %v, want one count of 2", counts)
	}
	inner.result.Skipped = nil
	drain.holdingEventDrainer = inner
	if _, err := drain.DrainHolding(t.Context(), nil); err != nil || len(counts) != 1 {
		t.Fatalf("a pass without skips reported %v (err %v)", counts, err)
	}
}

type skippedDrain struct{ result service.GroupEventPollResult }

func (s skippedDrain) DrainHolding(context.Context, map[id.ID]bool) (service.GroupEventPollResult, error) {
	return s.result, nil
}

type commandDrainFunc func(context.Context) (service.GroupEventPollResult, error)

func (f commandDrainFunc) Drain(ctx context.Context) (service.GroupEventPollResult, error) {
	return f(ctx)
}

// DrainHolding refuses to drain while any sender is unrefreshed: a test that reaches it with a
// hold set must use queueDrain, which honours the hold.
func (f commandDrainFunc) DrainHolding(ctx context.Context, unrefreshed map[id.ID]bool) (service.GroupEventPollResult, error) {
	if len(unrefreshed) != 0 {
		return service.GroupEventPollResult{}, errors.New("commandDrainFunc cannot hold senders")
	}
	return f(ctx)
}

func (t readinessTrust) RefreshPendingLeaves(context.Context, *client.Session) (service.TrustRefresh, error) {
	*t.steps = append(*t.steps, "trust")
	return service.TrustRefresh{}, nil
}

func noUnrefreshed(t *testing.T) func(service.TrustRefresh) {
	return func(r service.TrustRefresh) {
		if len(r.Unrefreshed) != 0 {
			t.Fatalf("unexpected unrefreshed senders %v", r.Unrefreshed)
		}
	}
}

// queuedCommand is one command in the durable event queue, by sender.
type queuedCommand struct {
	delivery uint64
	sender   id.ID
}

// queueDrain models the ordered event queue under the production hold: a command from an
// unrefreshed sender is held (not dispatched, not acknowledged) and stops the pass there.
type queueDrain struct {
	queue      []queuedCommand
	dispatched []uint64
	calls      int
	held       map[id.ID]bool
}

func (q *queueDrain) DrainHolding(ctx context.Context, unrefreshed map[id.ID]bool) (service.GroupEventPollResult, error) {
	q.calls++
	q.held = unrefreshed
	for len(q.queue) > 0 {
		head := q.queue[0]
		if unrefreshed[head.sender] {
			return service.GroupEventPollResult{}, service.ErrGroupEventHeld
		}
		q.dispatched = append(q.dispatched, head.delivery)
		q.queue = q.queue[1:]
	}
	return service.GroupEventPollResult{}, nil
}
func (q *queueDrain) Drain(ctx context.Context) (service.GroupEventPollResult, error) {
	return q.DrainHolding(ctx, nil)
}

// trustOutcome is a trust pass with a fixed result.
type trustOutcome struct {
	steps   *[]string
	refresh service.TrustRefresh
	err     error
}

func (o trustOutcome) RefreshPendingLeaves(context.Context, *client.Session) (service.TrustRefresh, error) {
	*o.steps = append(*o.steps, "trust")
	return o.refresh, o.err
}

func (o trustOutcome) TrustPendingLeaves(context.Context, *client.Session) (uint32, error) {
	return 0, errors.New("command intake must use the isolating trust pass")
}

// User B's stale sender history does not block user A's command: the pass reports B unrefreshed,
// A's command is dispatched, and the unrefreshed sender is reported.
func TestFastPathDispatchesAnotherUsersCommandWhenOneUsersHistoryIsStale(t *testing.T) {
	a, b := id.New(), id.New()
	var steps []string
	drain := &queueDrain{queue: []queuedCommand{{1, a}}}
	var reported service.TrustRefresh
	poll := newInteractionIntakePoller(namedGroupSync{steps: &steps}, trustOutcome{steps: &steps, refresh: service.TrustRefresh{Unrefreshed: map[id.ID]bool{b: true}}},
		nil, drain, func(r service.TrustRefresh) { reported = r })
	if err := poll(t.Context(), id.New()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if !reflect.DeepEqual(drain.dispatched, []uint64{1}) {
		t.Fatalf("dispatched %v, want A's command", drain.dispatched)
	}
	if !drain.held[b] || !reported.Unrefreshed[b] {
		t.Fatalf("B was not held (%v) or not reported (%v)", drain.held, reported.Unrefreshed)
	}
}

// B's own command stays held: not dispatched, still queued.
func TestFastPathHoldsTheUnrefreshedUsersCommand(t *testing.T) {
	b := id.New()
	var steps []string
	drain := &queueDrain{queue: []queuedCommand{{4, b}}}
	poll := newInteractionIntakePoller(namedGroupSync{steps: &steps}, trustOutcome{steps: &steps, refresh: service.TrustRefresh{Unrefreshed: map[id.ID]bool{b: true}}},
		nil, drain, func(service.TrustRefresh) {})
	if err := poll(t.Context(), id.New()); !errors.Is(err, service.ErrGroupEventHeld) {
		t.Fatalf("poll error %v, want the held command reported", err)
	}
	if len(drain.dispatched) != 0 || len(drain.queue) != 1 {
		t.Fatalf("B's command dispatched=%v queued=%v, want held", drain.dispatched, drain.queue)
	}
}

// A failure that is not scoped to one sender (a general API failure, the pending-leaf read, a core
// limit) stops the poll before Drain: nothing is dispatched.
func TestIntakeStopsWithoutDispatchOnAGeneralTrustFailure(t *testing.T) {
	unavailable := errors.New("server unavailable")
	t.Run("fast path", func(t *testing.T) {
		var steps []string
		drain := &queueDrain{queue: []queuedCommand{{1, id.New()}}}
		poll := newInteractionIntakePoller(namedGroupSync{steps: &steps}, trustOutcome{steps: &steps, err: unavailable}, nil, drain, func(service.TrustRefresh) {})
		if err := poll(t.Context(), id.New()); !errors.Is(err, unavailable) {
			t.Fatalf("poll error %v, want the trust failure", err)
		}
		if drain.calls != 0 || len(drain.dispatched) != 0 {
			t.Fatal("drained after a general trust failure")
		}
	})
	t.Run("full path", func(t *testing.T) {
		var steps []string
		drain := &queueDrain{queue: []queuedCommand{{1, id.New()}}}
		membership := readinessMembership{steps: &steps, sync: func(context.Context, *client.Session) error { return nil }}
		poll := newCommandIntakePoller(membership, trustOutcome{steps: &steps, err: unavailable}, nil,
			func(context.Context) error { steps = append(steps, "subscribe"); return nil }, drain, func(service.TrustRefresh) {})
		if err := poll(t.Context()); !errors.Is(err, unavailable) {
			t.Fatalf("poll error %v, want the trust failure", err)
		}
		if drain.calls != 0 || !reflect.DeepEqual(steps, []string{"membership", "trust"}) {
			t.Fatalf("steps %v, drain calls %d: want the poll stopped at trust", steps, drain.calls)
		}
	})
}

// The full text path applies the same rule as the fast path: one stale sender is held, the rest
// is dispatched.
func TestFullPathHoldsOnlyTheUnrefreshedSender(t *testing.T) {
	a, b := id.New(), id.New()
	var steps []string
	drain := &queueDrain{queue: []queuedCommand{{1, a}, {2, b}}}
	membership := readinessMembership{steps: &steps, sync: func(context.Context, *client.Session) error { return nil }}
	poll := newCommandIntakePoller(membership, trustOutcome{steps: &steps, refresh: service.TrustRefresh{Unrefreshed: map[id.ID]bool{b: true}}}, nil,
		func(context.Context) error { steps = append(steps, "subscribe"); return nil }, drain, func(service.TrustRefresh) {})
	if err := poll(t.Context()); !errors.Is(err, service.ErrGroupEventHeld) {
		t.Fatalf("poll error %v, want B's command held", err)
	}
	if !reflect.DeepEqual(drain.dispatched, []uint64{1}) || len(drain.queue) != 1 {
		t.Fatalf("dispatched %v, queued %v: want A's command dispatched and B's held", drain.dispatched, drain.queue)
	}
}

func TestGatewayCommandIntakeReadsPrivateSourcesBeforeDispatch(t *testing.T) {
	for _, denied := range []bool{false, true} {
		name := "accepted private source"
		if denied {
			name = "denied private source"
		}
		t.Run(name, func(t *testing.T) {
			var steps []string
			sourceRead := false
			refusal := errors.New("private source denied")
			membership := readinessMembership{steps: &steps, sync: func(context.Context, *client.Session) error {
				if denied {
					return refusal
				}
				sourceRead = true
				return nil
			}}
			poll := newCommandIntakePoller(membership, readinessTrust{steps: &steps}, nil,
				func(context.Context) error { steps = append(steps, "subscribe"); return nil },
				commandDrainFunc(func(context.Context) (service.GroupEventPollResult, error) {
					if !sourceRead {
						t.Error("dispatch attempted before ingesting private command ciphertext")
					}
					steps = append(steps, "dispatch")
					return service.GroupEventPollResult{}, nil
				}), noUnrefreshed(t))
			err := poll(t.Context())
			want := []string{"membership", "trust", "interaction_sync", "subscribe", "dispatch"}
			if denied {
				want = []string{"membership", "trust", "interaction_sync"}
				if !errors.Is(err, refusal) {
					t.Fatalf("error=%v, want source refusal", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(steps, want) {
				t.Fatalf("steps=%v, want %v", steps, want)
			}
		})
	}
}

// probeIntakePriority checks scheduling priority and makes a client request at
// each intake boundary, including trust, dispatch and response delivery.
type probeIntakePriority struct {
	api   *client.Client
	calls int
	want  int
}

func (p *probeIntakePriority) probe(ctx context.Context) error {
	if got := client.RequestPriority(ctx); got != p.want {
		return fmt.Errorf("priority = %d, want %d", got, p.want)
	}
	p.calls++
	_, err := p.api.Discover(ctx)
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable {
		return fmt.Errorf("priority was lost: %w", err)
	}
	return nil
}
func (p *probeIntakePriority) ReconcileInteractionGroupOnce(ctx context.Context, _ *client.Session, _ id.ID) (service.MembershipResult, error) {
	return service.MembershipResult{}, p.probe(ctx)
}
func (p *probeIntakePriority) RefreshPendingLeaves(ctx context.Context, _ *client.Session) (service.TrustRefresh, error) {
	return service.TrustRefresh{}, p.probe(ctx)
}
func (p *probeIntakePriority) DrainHolding(ctx context.Context, _ map[id.ID]bool) (service.GroupEventPollResult, error) {
	return service.GroupEventPollResult{}, p.probe(ctx)
}

func TestInteractionFastPathPreservesPriorityThroughTrustAndDrain(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/cbor")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	api, err := client.New(client.Config{BaseURL: server.URL, InstanceID: id.New(), AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	probe := &probeIntakePriority{api: api, want: 1}
	poll := newInteractionIntakePoller(probe, probe, nil, probe, noUnrefreshed(t))
	ctx := client.WithInteractive(t.Context())
	if err := poll(ctx, id.New()); err != nil {
		t.Fatal(err)
	}
	if probe.calls != 3 {
		t.Fatalf("priority checked at %d intake boundaries", probe.calls)
	}
}

func TestInteractionEventPropagatesPriorityThroughReply(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/cbor")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	api, err := client.New(client.Config{BaseURL: server.URL, InstanceID: id.New(), AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	probe := &probeIntakePriority{api: api, want: 1}
	event := native.GroupEvent{DeliveryID: 1, Group: id.New()}
	handler := newInteractionEventHandler(
		interactionGroupInspectorFunc(func(ctx context.Context, group id.ID) (native.GroupInfo, error) {
			return native.GroupInfo{ID: group, Kind: 3}, probe.probe(ctx)
		}),
		interactionDispatcherFunc(func(ctx context.Context, _ native.GroupEvent) (string, error) { return "reply", probe.probe(ctx) }),
		interactionResponseSenderFunc(func(ctx context.Context, _ native.GroupEvent, _ string) (bool, error) { return true, probe.probe(ctx) }),
	)
	ctx := t.Context()
	if err := handler.HandleGroupEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	if probe.calls != 3 {
		t.Fatalf("priority checked at %d event boundaries", probe.calls)
	}
}

type scopedCatchupProbe struct {
	interactive, background int
}

func (p *scopedCatchupProbe) ReconcileInteractionGroupOnce(ctx context.Context, _ *client.Session, _ id.ID) (service.MembershipResult, error) {
	p.interactive++
	return service.MembershipResult{}, nil
}
func (p *scopedCatchupProbe) ReconcileInteractionGroupBackgroundOnce(ctx context.Context, _ *client.Session, _ id.ID) (service.MembershipResult, error) {
	p.background++
	if client.RequestPriority(ctx) != 0 {
		return service.MembershipResult{}, errors.New("background priority lost")
	}
	return service.MembershipResult{}, nil
}
func TestInteractionIntakeRoutesBackgroundCatchupToIdleRespectingSync(t *testing.T) {
	probe := &scopedCatchupProbe{}
	var steps []string
	poll := newInteractionIntakePoller(probe, readinessTrust{steps: &steps}, nil, commandDrainFunc(func(context.Context) (service.GroupEventPollResult, error) {
		return service.GroupEventPollResult{}, nil
	}), noUnrefreshed(t))
	if err := poll(t.Context(), id.New()); err != nil {
		t.Fatal(err)
	}
	if probe.background != 1 || probe.interactive != 0 {
		t.Fatalf("background=%d interactive=%d", probe.background, probe.interactive)
	}
	if err := poll(client.WithInteractive(t.Context()), id.New()); err != nil {
		t.Fatal(err)
	}
	if probe.background != 1 || probe.interactive != 1 {
		t.Fatalf("background=%d interactive=%d", probe.background, probe.interactive)
	}
}

func TestInteractionIntakePreservesBusyTrustHoldReason(t *testing.T) {
	busy := id.New()
	var steps []string
	handler := &recordingHandler{}
	queue := &queuePoller{events: []native.GroupEvent{{DeliveryID: 1, SenderUser: busy}}}
	runtime := newInteractionEventRuntime(queue, handler)
	poll := newInteractionIntakePoller(namedGroupSync{steps: &steps}, trustOutcome{steps: &steps, refresh: service.TrustRefresh{Busy: map[id.ID]bool{busy: true}}}, nil, countSkipped{holdingEventDrainer: runtime, report: func(int) { t.Fatal("busy event skipped") }}, func(service.TrustRefresh) {})
	if err := poll(t.Context(), id.New()); !errors.Is(err, service.ErrGroupEventTrustBusy) {
		t.Fatalf("busy trust error=%v", err)
	}
	if len(handler.handled) != 0 || len(queue.events) != 1 {
		t.Fatalf("handled=%v queued=%v", handler.handled, queue.events)
	}
}

type idleCatchupMembership struct{ installationMembership }

func (idleCatchupMembership) InteractionGroupDelay(native.InteractionGroupDescriptor) time.Duration {
	return time.Second
}
func TestCommandOwnedMembershipDoesNotQueueIdleInteractionGroups(t *testing.T) {
	installation, group := id.New(), id.New()
	runtime := &botGatewayRuntime{}
	membership := commandOwnedMembership{installationMembership: idleCatchupMembership{}, source: privateNotificationSource{group: group}, runtime: runtime, installation: installation}
	if _, err := membership.ReconcileInteractionGroupsOnce(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	runtime.pendingMu.Lock()
	defer runtime.pendingMu.Unlock()
	if runtime.pending[installation][group] {
		t.Fatal("idle interaction group queued on pass/text wake")
	}
}

func TestCountSkippedPreservesDeliveryAndFailureDetails(t *testing.T) {
	cause := errors.New("handler failed")
	first := time.Unix(1800000000, 0)
	skipped := []service.SkippedGroupEvent{{DeliveryID: 41, FirstFailure: first, Cause: cause}}
	var reported []service.SkippedGroupEvent
	drain := countSkipped{holdingEventDrainer: skippedDrain{result: service.GroupEventPollResult{Skipped: skipped}}, reportSkipped: func(events []service.SkippedGroupEvent) { reported = events }}
	if _, err := drain.DrainHolding(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if len(reported) != 1 || reported[0].DeliveryID != 41 || !reported[0].FirstFailure.Equal(first) || !errors.Is(reported[0].Cause, cause) {
		t.Fatalf("reported skips=%+v", reported)
	}
}
