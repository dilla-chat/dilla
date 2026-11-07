//go:build linux

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/intake"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/bot/reminders"
	"github.com/dilla-chat/dilla/internal/bot/service"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
)

// probeWorld is a Bot with a ready sealed intake whose workers are running (unlike the load tests, which stop them).
func probeWorld(t *testing.T, ready bool) (*sealedEntry, *botGatewayRuntime, *loadWorld) {
	t.Helper()
	old := probeAnswerLimit
	probeAnswerLimit = 400 * time.Millisecond
	t.Cleanup(func() { probeAnswerLimit = old })
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	installation, community, botUser := id.New(), id.New(), id.New()
	w := newLoadWorld(t, 1, true)
	r := w.runtime
	h := &loadHelper{requests: map[string]protocol.SealedRequest{}, replays: map[string][32]byte{}}
	state := &countingState{utilityStateFake: newUtilityStateFake()}
	rem := service.ReminderInteractions{Store: reminders.NewStore(state, native.Binding{Instance: id.New(), Community: community, Installation: installation}), CommunityID: community}
	session, _ := w.entry.current()
	r.SetSealedIntake(sealedIntakeConfig{installation: installation, community: community, botUser: botUser, capabilities: 1 << 9,
		helper: h, session: session.session, reminders: &rem, held: installationHeld{r, installation}, log: log})
	e := r.sealed.lookup(installation)
	t.Cleanup(e.stop)
	feed := r.feedFor(installation, community, botUser, feedDeps{})
	if ready {
		tee := r.sealed.feed(installation, community, feed)
		tee.SnapshotBegin(ctx)
		tee.Role(ctx, RoleEvent{Community: community, Role: id.New(), Allow: intake.PermViewChannel, FromSnapshot: true})
		tee.SnapshotDone(ctx)
	}
	return e, r, w
}

// Task 17: the probe goes through the real intake. It is answered when the intake is ready, and fails when it is not,
// when no worker takes it and when the queue has no room: health that can fail.
func TestProbePassesTheIntakeWhenReadyAndFailsWhenItIsNot(t *testing.T) {
	ready, _, _ := probeWorld(t, true)
	if err := ready.Probe(context.Background()); err != nil {
		t.Fatalf("probe of a ready intake: %v", err)
	}
	notReady, _, _ := probeWorld(t, false)
	if err := notReady.Probe(context.Background()); err == nil {
		t.Fatal("a probe of an intake with no feed snapshot (503 E_BOT_OFFLINE) was reported as answered")
	}
}

// armedClock is a fake clock that says when a timer was armed on it.
type armedClock struct {
	*clock.Fake
	armed chan struct{}
}

func (c *armedClock) NewTimer(d time.Duration) clock.Timer {
	t := c.Fake.NewTimer(d)
	c.armed <- struct{}{}
	return t
}

// With no worker to take it, a probe waits for its deadline and fails. The entry has no workers at all and the deadline
// runs on a fake clock, so nothing here waits on wall time or races a worker.
func TestProbeFailsWhenNoWorkerTakesIt(t *testing.T) {
	clk := &armedClock{Fake: clock.NewFake(time.Unix(1_800_000_000, 0)), armed: make(chan struct{}, 4)}
	e := &sealedEntry{community: id.New(), installation: id.New(), queue: make(chan intake.RelayedCommand, 2), clk: clk,
		cfg: sealedIntakeConfig{helper: &loadHelper{}, botUser: id.New()}}
	result := make(chan error, 1)
	go func() { result <- e.Probe(context.Background()) }()
	<-clk.armed // the probe is queued and waiting
	select {
	case err := <-result:
		t.Fatalf("a probe nobody answered returned early: %v", err)
	default:
	}
	if len(e.queue) != 1 {
		t.Fatalf("the probe is not in the queue: %d", len(e.queue))
	}
	clk.Advance(probeAnswerLimit)
	if err := <-result; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("an unanswered probe = %v, want a deadline failure", err)
	}
	// A full queue fails at once.
	for len(e.queue) < cap(e.queue) {
		e.queue <- intake.RelayedCommand{}
	}
	if err := e.Probe(context.Background()); !errors.Is(err, errProbeQueueFull) {
		t.Fatalf("probe of a full queue: %v, want %v", err, errProbeQueueFull)
	}
}

// A probe's request id is random and local: a relayed command that carries the marker for another id is a plain command
// for the helper, which refuses it.
func TestProbeMarkerFromTheNetworkIsNotAProbe(t *testing.T) {
	e, _, _ := probeWorld(t, true)
	guessed := id.New()
	if _, ok := e.probes.probeRequest(probeBytes(guessed)); ok {
		t.Fatal("a marker for a request id no probe is waiting for was taken as a probe")
	}
	if e.probes.has(guessed) {
		t.Fatal("unknown probe")
	}
	_, forget := e.probes.begin(guessed)
	defer forget()
	if request, ok := e.probes.probeRequest(probeBytes(guessed)); !ok || request != guessed {
		t.Fatal("the waiting probe's own marker must be recognised")
	}
	if _, ok := e.probes.probeRequest(probeBytes(guessed)[:20]); ok {
		t.Fatal("a truncated marker was taken as a probe")
	}
}

func TestCommandLoadReportsDepthAndOldestWait(t *testing.T) {
	var load commandLoad
	now := time.Unix(1_800_000_000, 0)
	if depth, oldest := load.snapshot(now); depth != 0 || oldest != 0 {
		t.Fatalf("empty load = %d, %v", depth, oldest)
	}
	a, b := id.New(), id.New()
	load.enqueue(a, now.Add(-12*time.Second))
	load.enqueue(b, now.Add(-2*time.Second))
	if depth, oldest := load.snapshot(now); depth != 2 || oldest != 12*time.Second {
		t.Fatalf("load = %d, %v, want 2 and 12s", depth, oldest)
	}
	load.started(a)
	if depth, oldest := load.snapshot(now); depth != 1 || oldest != 2*time.Second {
		t.Fatalf("after a worker took the oldest: %d, %v", depth, oldest)
	}
}
