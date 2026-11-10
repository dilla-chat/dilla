//go:build linux

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/bot/gatewayclient"
	"github.com/dilla-chat/dilla/internal/bot/health"
	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/intake"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/bot/reminders"
	"github.com/dilla-chat/dilla/internal/bot/service"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
)

// Task 17 fix round 2 (C1, I1, M8): the probe is a sealed OpProbe that travels the whole command path, so health goes
// degraded whenever a member's command would fail.

// probeFaults are the faults a test injects along the probe's path.
type probeFaults struct {
	openErr      atomic.Bool   // the helper's op 22 errors (state DB, sandbox, key load)
	openHang     chan struct{} // when non-nil, op 22 blocks until it is closed
	sealErr      atomic.Bool   // the helper's op 25 errors
	dropCommands atomic.Bool   // dillad's relay accepts the POST but never pushes op 64
	dropReplies  atomic.Bool   // dillad's reply route accepts the reply but never delivers op 62
}

// probeHelperFake is loadHelper with ops 25 and 26: the probe's seal registers an OpProbe from the Bot's own device as
// loadHelper's op 22 opens it; loadHelper's op 23 prefixes 0xa0, which op 26 strips.
type probeHelperFake struct {
	*loadHelper
	faults  *probeFaults
	mu      sync.Mutex
	sealed  int
	opened  int
	handles map[uint32]id.ID
}

func (h *probeHelperFake) OpenCommand(ctx context.Context, sealed []byte, user, device id.ID, now time.Time) (helper.OpenedCommand, error) {
	if h.faults.openErr.Load() {
		return helper.OpenedCommand{}, errors.New("helper: state database unavailable")
	}
	if hang := h.faults.openHang; hang != nil {
		select {
		case <-hang:
		case <-ctx.Done():
		}
		return helper.OpenedCommand{}, errors.New("helper: no answer")
	}
	return h.loadHelper.OpenCommand(ctx, sealed, user, device, now)
}

func (h *probeHelperFake) SealProbe(_ context.Context, community, request id.ID, now time.Time) ([]byte, uint32, error) {
	if h.faults.sealErr.Load() {
		return nil, 0, errors.New("helper: no current command key")
	}
	sealed := append([]byte("probe:"), request[:]...)
	h.loadHelper.mu.Lock()
	h.requests[string(sealed)] = protocol.SealedRequest{RequestID: request, CommunityID: community, Operation: uint64(protocol.OpProbe),
		SentAt: uint64(now.Unix()), ReplyKey: [32]byte{2}}
	h.loadHelper.mu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sealed++
	handle := uint32(h.sealed)
	if h.handles == nil {
		h.handles = map[uint32]id.ID{}
	}
	h.handles[handle] = request
	return sealed, handle, nil
}

func (h *probeHelperFake) OpenProbeReply(_ context.Context, handle, _ uint32, sealed []byte, forget bool) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.handles[handle]; !ok || len(sealed) < 1 || sealed[0] != 0xa0 {
		return nil, errors.New("helper: the answer does not open")
	}
	if forget {
		delete(h.handles, handle)
	}
	h.opened++
	return sealed[1:], nil
}

// probeRelay is the fake dillad: POST …/bot/commands relays op 64 to the runtime as the Bot's own session (the delivery
// loop's relayCommand, with the feed barrier), and POST /v1/bot-runtime/replies delivers op 62 back to it.
type probeRelay struct {
	r                                        *botGatewayRuntime
	installation, community, botUser, botDev id.ID
	faults                                   *probeFaults
	barrier                                  func() func(context.Context) bool
	mu                                       sync.Mutex
	commands, replies                        int
}

func rawCBOR(t testing.TB, v any) cbor.RawMessage {
	b, err := cborx.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (p *probeRelay) handle(t testing.TB) func(method, route string, body []byte) (int, bool) {
	return func(method, route string, body []byte) (int, bool) {
		switch {
		case method == "POST" && strings.HasSuffix(route, "/communities/"+p.community.String()+"/bot/commands"):
			var fields []any
			if cborx.Unmarshal(body, &fields) != nil || len(fields) != 2 {
				return 400, true
			}
			request, _ := fields[0].([]byte)
			sealed, _ := fields[1].([]byte)
			p.mu.Lock()
			p.commands++
			p.mu.Unlock()
			if p.faults.dropCommands.Load() {
				return 202, true
			}
			payload := []cbor.RawMessage{rawCBOR(t, request), rawCBOR(t, p.community[:]), rawCBOR(t, p.botUser[:]), rawCBOR(t, p.botDev[:]), rawCBOR(t, sealed)}
			var ready func(context.Context) bool
			if p.barrier != nil {
				ready = p.barrier()
			}
			p.r.relayCommand(p.installation, payload, ready)
			return 202, true
		case method == "POST" && strings.HasSuffix(route, "/bot-runtime/replies"):
			var fields []any
			if cborx.Unmarshal(body, &fields) != nil || len(fields) != 5 {
				return 400, true
			}
			p.mu.Lock()
			p.replies++
			p.mu.Unlock()
			if p.faults.dropReplies.Load() {
				return 204, true
			}
			var reply gatewayclient.BotReply
			request, _ := fields[1].([]byte)
			copy(reply.RequestID[:], request)
			seq, _ := fields[2].(uint64)
			reply.Seq = uint32(seq)
			reply.Sealed, _ = fields[3].([]byte)
			final, _ := fields[4].(uint64)
			reply.Final = final == 1
			// Delivered on the Bot's connection, off the replying worker, as the gateway does.
			go p.r.probeReply(p.installation, &reply)
			return 204, true
		}
		return 0, false
	}
}

func (p *probeRelay) counts() (commands, replies int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.commands, p.replies
}

type probeEnv struct {
	entry  *sealedEntry
	r      *botGatewayRuntime
	helper *probeHelperFake
	relay  *probeRelay
	faults *probeFaults
}

// probeWorld is a Bot with a sealed intake whose workers run (unlike the load tests, which stop them), behind a fake
// dillad that relays its probe and its replies. ready says whether the feed has applied a snapshot.
func probeWorld(t *testing.T, ready bool) *probeEnv {
	t.Helper()
	old := probeAnswerLimit
	probeAnswerLimit = 300 * time.Millisecond
	t.Cleanup(func() { probeAnswerLimit = old })
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	installation, community, botUser, botDevice := id.New(), id.New(), id.New(), id.New()
	w := newLoadWorld(t, 1, true)
	r := w.runtime
	faults := &probeFaults{}
	h := &probeHelperFake{loadHelper: &loadHelper{requests: map[string]protocol.SealedRequest{}, replays: map[string][32]byte{}}, faults: faults}
	relay := &probeRelay{r: r, installation: installation, community: community, botUser: botUser, botDev: botDevice, faults: faults}
	w.transport.mu.Lock()
	w.transport.relay = relay.handle(t)
	w.transport.mu.Unlock()
	state := &countingState{utilityStateFake: newUtilityStateFake()}
	rem := service.ReminderInteractions{Store: reminders.NewStore(state, native.Binding{Instance: id.New(), Community: community, Installation: installation}), CommunityID: community}
	r.SetSealedIntake(sealedIntakeConfig{installation: installation, community: community, botUser: botUser, capabilities: 1 << 9,
		helper: h, session: w.session, reminders: &rem, held: installationHeld{r, installation}, log: log})
	e := r.sealed.lookup(installation)
	t.Cleanup(e.stop)
	feed := r.feedFor(installation, community, botUser, feedDeps{})
	if ready {
		tee := r.sealed.feed(installation, community, feed)
		tee.SnapshotBegin(ctx)
		tee.Role(ctx, RoleEvent{Community: community, Role: id.New(), Allow: intake.PermViewChannel, FromSnapshot: true})
		tee.SnapshotDone(ctx)
	}
	return &probeEnv{entry: e, r: r, helper: h, relay: relay, faults: faults}
}

// The probe is a command among the others: the helper seals it (op 25), dillad relays it, the intake opens it in the
// helper (op 22), answers it through the helper's seal (op 23) and the reply route, and op 62 brings the answer back
// to be opened (op 26). An intake with no feed snapshot answers 503, and the probe fails.
func TestProbeTravelsTheRelayTheIntakeAndTheReplyRoute(t *testing.T) {
	env := probeWorld(t, true)
	if err := env.entry.Probe(context.Background()); err != nil {
		t.Fatalf("probe of a working command path: %v", err)
	}
	if commands, replies := env.relay.counts(); commands != 1 || replies != 1 {
		t.Fatalf("the probe crossed the relay %d times and the reply route %d times, want once each", commands, replies)
	}
	env.helper.mu.Lock()
	sealed, opened := env.helper.sealed, env.helper.opened
	env.helper.mu.Unlock()
	if ops, _ := env.helper.counts(); sealed != 1 || opened != 1 || ops < 2 {
		t.Fatalf("helper ops: seal %d, open-answer %d, open+seal-reply %d; the probe must touch the helper both ways", sealed, opened, ops)
	}
	notReady := probeWorld(t, false)
	if err := notReady.entry.Probe(context.Background()); err == nil {
		t.Fatal("a probe of an intake with no feed snapshot (503 E_BOT_OFFLINE) was reported as answered")
	}
}

// judgeTwoProbes runs the Bot's monitor over two probe periods on a fake clock and returns dillad's judgement of the
// reports: catalog state (0 ready, 1 degraded, 2 offline) and since. The probe's own answer deadline runs on wall time.
func judgeTwoProbes(t *testing.T, e *sealedEntry) (state, since uint64, elapsed time.Duration) {
	t.Helper()
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	judge := api.NewBotHealth(clk)
	start := clk.Now()
	monitor := health.New(health.ProberFunc(e.Probe), health.ReporterFunc(func(_ context.Context, r health.Report) error {
		report := api.BotHealthReport{ProbeOK: r.ProbeOK, ProbeAt: uint64(r.ProbeAt.Unix()), QueueDepth: r.QueueDepth, OldestWaitMS: r.OldestWaitMS}
		judge.Record(e.community, report)
		return nil
	}), clk)
	monitor.SetLoad(func() health.Load {
		// The probe's own command can still be queued for a moment after its answer arrives. With only two reports
		// one growth window apart, that 0 -> 1 would read as a growing queue, so let it drain (bounded: a fault that
		// leaves commands queued still reports them).
		for settle := time.Now().Add(time.Second); time.Now().Before(settle); time.Sleep(5 * time.Millisecond) {
			if depth, _ := e.load.snapshot(time.Now()); depth == 0 {
				break
			}
		}
		depth, oldest := e.load.snapshot(time.Now())
		return health.Load{QueueDepth: depth, OldestWait: oldest}
	})
	ctx := context.Background()
	monitor.Tick(ctx)
	clk.Advance(health.ProbePeriod)
	monitor.Tick(ctx)
	state, since = judge.Status(e.community)
	return state, since, clk.Now().Sub(start)
}

func requireDegraded(t *testing.T, e *sealedEntry, fault string) {
	t.Helper()
	state, _, elapsed := judgeTwoProbes(t, e)
	if state != 1 {
		t.Fatalf("%s: dillad judged the Bot state %d, want 1 (degraded)", fault, state)
	}
	if elapsed > 2*health.ProbePeriod {
		t.Fatalf("%s: degraded after %v, want within two probe periods", fault, elapsed)
	}
}

// The control: a working command path is judged ready, so the faults below are what makes it degraded.
func TestHealthIsReadyWhileTheCommandPathWorks(t *testing.T) {
	env := probeWorld(t, true)
	if state, _, _ := judgeTwoProbes(t, env.entry); state != 0 {
		t.Fatalf("a working command path judged %d, want 0 (ready)", state)
	}
}

// C1: the helper returns errors on every op 22. Every member command is dropped at "bot command open failed" with no
// answer; so is the probe.
func TestHealthDegradesWhenTheHelperErrors(t *testing.T) {
	env := probeWorld(t, true)
	env.faults.openErr.Store(true)
	requireDegraded(t, env.entry, "helper errors")
	sealFails := probeWorld(t, true)
	sealFails.faults.sealErr.Store(true)
	requireDegraded(t, sealFails.entry, "helper cannot seal")
}

// C1: the helper hangs. The probe takes a worker like a member's command and never comes back; it fails at its deadline,
// with no member command needed to fill the workers first. M3: the command stuck inside a worker still counts as
// waiting.
func TestHealthDegradesWhenTheHelperHangs(t *testing.T) {
	env := probeWorld(t, true)
	hang := make(chan struct{})
	env.faults.openHang = hang
	t.Cleanup(func() { close(hang) })
	requireDegraded(t, env.entry, "helper hangs")
	if depth, _ := env.entry.load.snapshot(time.Now()); depth == 0 {
		t.Fatal("commands stuck inside a worker are not counted as waiting (M3)")
	}
}

// C1: the feed is stuck. A command waits out the feed barrier and is answered 503 feed_behind; so is the probe.
func TestHealthDegradesWhenTheFeedIsStuck(t *testing.T) {
	// The barrier holds the probe for feedBarrierBound (2 s, then 503 feed_behind); its 300 ms answer limit ends first.
	env := probeWorld(t, true)
	stuck := newFeedConsumer(env.entry.community, &orderedAcks{log: slog.New(slog.DiscardHandler)}, slog.New(slog.DiscardHandler))
	stuck.mu.Lock()
	stuck.posted = 1 // an event dillad sent before the probe, never applied
	stuck.mu.Unlock()
	env.relay.barrier = stuck.barrier
	requireDegraded(t, env.entry, "feed stuck")
}

// I1: dillad's relay (op 64 never reaches the Bot: a stalled ring, a wedged delivery loop) or its reply route (op 62
// never comes back) is broken.
func TestHealthDegradesWhenTheRelayOrTheReplyRouteIsBroken(t *testing.T) {
	relay := probeWorld(t, true)
	relay.faults.dropCommands.Store(true)
	requireDegraded(t, relay.entry, "relay broken")
	replies := probeWorld(t, true)
	replies.faults.dropReplies.Store(true)
	requireDegraded(t, replies.entry, "reply route broken")
	if _, delivered := replies.relay.counts(); delivered == 0 {
		t.Fatal("the intake never answered, so the reply route was not what failed")
	}
}

// M8: the intake stops (its workers end) at t = 0; dillad's status is degraded by t <= 120 s.
func TestDegradedWithinTwoProbePeriodsWhenTheIntakeStops(t *testing.T) {
	env := probeWorld(t, true)
	env.entry.stop()
	requireDegraded(t, env.entry, "intake stopped")
}

// An op 62 for a request no probe waits for (the Bot sends no other command) is dropped.
func TestBotReplyForNoWaitingProbeIsDropped(t *testing.T) {
	env := probeWorld(t, true)
	env.r.probeReply(env.relay.installation, &gatewayclient.BotReply{RequestID: id.New(), Sealed: []byte{0xa0}, Final: true})
	env.r.probeReply(id.New(), &gatewayclient.BotReply{RequestID: id.New(), Sealed: []byte{0xa0}, Final: true})
	env.r.probeReply(env.relay.installation, nil)
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

// A probe whose answer never comes fails at its deadline: the relay takes it and nothing answers. The deadline runs on
// a fake clock, so nothing here waits on wall time.
func TestProbeFailsWhenNoAnswerComes(t *testing.T) {
	env := probeWorld(t, true)
	env.faults.dropReplies.Store(true)
	clk := &armedClock{Fake: clock.NewFake(time.Unix(1_800_000_000, 0)), armed: make(chan struct{}, 4)}
	env.entry.clk = clk
	result := make(chan error, 1)
	go func() { result <- env.entry.Probe(context.Background()) }()
	<-clk.armed
	waitUntil(t, func() bool { _, replies := env.relay.counts(); return replies == 1 })
	select {
	case err := <-result:
		t.Fatalf("a probe nobody answered returned early: %v", err)
	default:
	}
	clk.Advance(probeAnswerLimit)
	if err := <-result; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("an unanswered probe = %v, want a deadline failure", err)
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
	load.answered(a)
	if depth, oldest := load.snapshot(now); depth != 1 || oldest != 2*time.Second {
		t.Fatalf("after the oldest was answered: %d, %v", depth, oldest)
	}
}
