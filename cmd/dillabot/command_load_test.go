//go:build linux

package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/gatewayclient"
	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/intake"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/bot/reminders"
	"github.com/dilla-chat/dilla/internal/bot/service"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
)

// loadTransport is the Bot device's gateway as the fake dillad: every socket route is one request frame, counted by
// route. Replies are 204, the catalog PUT answers its revision, the command-key PUT 204.
type loadTransport struct {
	mu      sync.Mutex
	frames  map[string]int
	first   chan time.Time
	answers []intake.Answer
	// relay, when set, is the fake dillad's sealed command relay and reply route (health_probe_test.go): it answers a
	// route it handles.
	relay func(method, route string, body []byte) (status int, handled bool)
}

func (t *loadTransport) Do(_ context.Context, method, route string, body, _ []byte) (int, []byte, error) {
	t.mu.Lock()
	relay := t.relay
	t.mu.Unlock()
	if relay != nil {
		if status, ok := relay(method, route, body); ok {
			if status == 202 {
				return status, []byte{0x81, 0x00}, nil // [0]
			}
			return status, nil, nil
		}
	}
	t.mu.Lock()
	t.frames[method+" "+route]++
	if strings.HasSuffix(route, "/bot-runtime/replies") {
		// [device, request, seq, sealed, final]; loadHelper's seal is 0xa0 then the answer plaintext.
		var reply []any
		if cborx.Unmarshal(body, &reply) == nil && len(reply) == 5 {
			if sealed, ok := reply[3].([]byte); ok && len(sealed) > 1 {
				if a, err := intake.DecodeAnswer(sealed[1:]); err == nil {
					t.answers = append(t.answers, a)
				}
			}
		}
	}
	t.mu.Unlock()
	switch {
	case strings.HasSuffix(route, "/catalog"):
		b, _ := cborx.Marshal([]uint64{1})
		return 200, b, nil
	case strings.HasSuffix(route, "/bot-runtime/replies"):
		if t.first != nil {
			select {
			case t.first <- time.Now():
			default:
			}
		}
		return 204, nil, nil
	}
	return 204, nil, nil
}

func (t *loadTransport) take() map[string]int {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := t.frames
	t.frames = map[string]int{}
	return out
}

// loadHelper is the installation's helper: it opens the prepared commands as verified requests and seals an answer
// by copying it. It counts every op.
type loadHelper struct {
	mu       sync.Mutex
	requests map[string]protocol.SealedRequest
	replays  map[string][32]byte // a Retry's seal: answered Replay with this reply key
	ops      int
	ingests  int
	seals    int   // op 23 calls that sealed
	closed   bool  // a closed helper refuses every op, as helper.Client does
	sealErr  error // when set, SealReply fails with it while the helper is still usable
	// onOpen, when set, runs after each successful open, off the helper's lock: a helper that closes itself, or is
	// replaced in the registry, between a command's open and its answer.
	onOpen func()
}

// Usable is native.Client's: false once closed.
func (h *loadHelper) Usable() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.closed
}

func (h *loadHelper) close() {
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
}

func (h *loadHelper) sealCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seals
}

func (h *loadHelper) OpenCommand(ctx context.Context, sealed []byte, user, device id.ID, now time.Time) (helper.OpenedCommand, error) {
	opened, err := h.open(ctx, sealed, user, device, now)
	h.mu.Lock()
	hook := h.onOpen
	h.mu.Unlock()
	if err == nil && hook != nil {
		hook()
	}
	return opened, err
}

func (h *loadHelper) open(_ context.Context, sealed []byte, user, device id.ID, _ time.Time) (helper.OpenedCommand, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ops++
	if h.closed {
		return helper.OpenedCommand{}, helper.ErrClosed
	}
	if strings.HasPrefix(string(sealed), "unknown-key") {
		return helper.OpenedCommand{Refusal: helper.RefusalUnknownKey}, nil
	}
	if key, ok := h.replays[string(sealed)]; ok {
		// A Retry's seal is "sealed:" + request id + its key bytes: the helper names the seal's own request (M-1).
		var request id.ID
		copy(request[:], sealed[len("sealed:"):])
		return helper.OpenedCommand{Refusal: helper.RefusalReplay, ReplyKey: key, ReplayRequestID: request}, nil
	}
	req, ok := h.requests[string(sealed)]
	if !ok {
		return helper.OpenedCommand{Refusal: helper.RefusalMalformed}, nil
	}
	req.UserID, req.DeviceID = user, device
	return helper.OpenedCommand{Request: req}, nil
}

func (h *loadHelper) SealReply(_ context.Context, _ [32]byte, _ id.ID, _ uint32, body []byte) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ops++
	if h.closed {
		return nil, helper.ErrClosed
	}
	if h.sealErr != nil {
		return nil, h.sealErr
	}
	h.seals++
	return append([]byte{0xa0}, body...), nil
}

func (h *loadHelper) CommandKey(context.Context, id.ID, time.Time) ([]byte, error) {
	return []byte{0x87, 0x01}, nil
}

func (h *loadHelper) counts() (ops, ingests int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ops, h.ingests
}

// countingState is the reminders' state (helper ops 11/12 in production), counted separately from the intake's ops.
type countingState struct {
	*utilityStateFake
	mu  sync.Mutex
	ops int
}

func (s *countingState) count() { s.mu.Lock(); s.ops++; s.mu.Unlock() }
func (s *countingState) StateGet(ctx context.Context, area uint64, key []byte) (native.StateValue, error) {
	s.count()
	return s.utilityStateFake.StateGet(ctx, area, key)
}
func (s *countingState) StateScan(ctx context.Context, area uint64, after []byte, limit uint64) (native.StatePage, error) {
	s.count()
	return s.utilityStateFake.StateScan(ctx, area, after, limit)
}
func (s *countingState) StateTransaction(ctx context.Context, tx id.ID, changes []native.StateChange) ([]uint64, error) {
	s.count()
	return s.utilityStateFake.StateTransaction(ctx, tx, changes)
}
func (s *countingState) taken() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.ops
	s.ops = 0
	return n
}

type loadWorld struct {
	session   *client.Session
	runtime   *botGatewayRuntime
	entry     *sealedEntry
	transport *loadTransport
	helper    *loadHelper
	state     *countingState
	community id.ID
	members   []id.ID
	devices   map[id.ID][]id.ID
	commands  []intake.RelayedCommand
	next      int
}

const loadCommands = 200

// newLoadWorld is a Bot whose feed snapshot held members members with 3 devices each, and loadCommands /remind
// commands ready to relay (from the first 20 members, round robin, so the reminders' state is the same at any size).
func newLoadWorld(t testing.TB, members int, cancel bool) *loadWorld {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	installation, community, botUser, botDevice, instance := id.New(), id.New(), id.New(), id.New(), id.New()
	transport := &loadTransport{frames: map[string]int{}}
	// HTTPS serves only discovery; anything else on it is counted as a request and fails the frame count.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			data, _ := cborx.Marshal([]any{[]any{uint64(6)}, []any{uint64(1)}, []any{uint64(1)}, instance, uint64(1), "test.local", uint64(0), []any{uint64(0)}, uint64(1), id.New(), make([]byte, 32)})
			w.Header().Set("Content-Type", "application/cbor")
			_, _ = w.Write(data)
			return
		}
		transport.mu.Lock()
		transport.frames["HTTPS "+r.Method+" "+r.URL.Path]++
		transport.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	unlimited := client.RateLimits{ReadPerSecond: 1e9, ReadBurst: 1 << 20, WritePerSecond: 1e9, WriteBurst: 1 << 20}
	api, err := client.New(client.Config{BaseURL: srv.URL, InstanceID: instance, AllowLoopbackHTTP: true, RateLimits: unlimited})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.Discover(ctx); err != nil {
		t.Fatal(err)
	}
	api.UseTransport(botDevice, transport)
	session, err := api.SessionForDevice("bot-session", botDevice)
	if err != nil {
		t.Fatal(err)
	}
	r := newBotGatewayRuntime(ctx, api, true, log)
	w := &loadWorld{session: session, runtime: r, transport: transport, helper: &loadHelper{requests: map[string]protocol.SealedRequest{}}, community: community, devices: map[id.ID][]id.ID{}}
	// The feed: the installation's handler, teed to the intake's Authz, applies a cold-start snapshot.
	feed := r.feedFor(installation, community, botUser, feedDeps{ingest: func(context.Context, id.ID, []byte) (native.DeviceListIngest, error) {
		w.helper.mu.Lock()
		w.helper.ingests++
		w.helper.mu.Unlock()
		return native.DeviceListIngest{Changed: true, Version: 1}, nil
	}})
	tee := r.sealed.feed(installation, community, feed)
	everyone := id.New()
	tee.SnapshotBegin(ctx)
	tee.Community(ctx, community, id.New(), true)
	tee.Role(ctx, RoleEvent{Community: community, Role: everyone, Allow: intake.PermViewChannel | intake.PermSendMessages, FromSnapshot: true})
	for range members {
		user := id.New()
		w.members = append(w.members, user)
		w.devices[user] = []id.ID{id.New(), id.New(), id.New()}
		// One device-list push per member, naming its three devices (the helper's op 24 reads it).
		if err := tee.DeviceList(ctx, user, []byte{0x84, 3}, true); err != nil {
			t.Fatal(err)
		}
		tee.Member(ctx, MemberEvent{Community: community, User: user, Joined: true, Snapshot: true})
	}
	tee.SnapshotDone(ctx)
	w.state = &countingState{utilityStateFake: newUtilityStateFake()}
	binding := native.Binding{Instance: id.New(), Community: community, Installation: installation}
	rem := service.ReminderInteractions{Store: reminders.NewStore(w.state, binding), CommunityID: community}
	r.SetSealedIntake(sealedIntakeConfig{installation: installation, community: community, botUser: botUser, capabilities: 1 << 9,
		helper: w.helper, session: session, reminders: &rem, held: installationHeld{r, installation}, log: log})
	w.entry = r.sealed.lookup(installation)
	if w.entry == nil {
		t.Fatal("no sealed intake")
	}
	// The publisher's start: the key and the catalog. Then the command phase starts from zero.
	waitUntil(t, func() bool { transport.mu.Lock(); defer transport.mu.Unlock(); return len(transport.frames) >= 2 })
	if cancel {
		w.entry.stop()
	}
	transport.take()
	w.state.taken()
	now := uint64(time.Now().Unix())
	for i := range 2 * loadCommands {
		user := w.members[i%min(20, members)]
		device := w.devices[user][i%3]
		request := id.New()
		sealed := append([]byte("sealed:"), request[:]...)
		var key [32]byte
		key[0] = 1
		w.helper.requests[string(sealed)] = protocol.SealedRequest{RequestID: request, CommunityID: community, Operation: uint64(protocol.OpReminderCreate),
			Args: []any{uint64(600), "stand up"}, SentAt: now, ReplyKey: key}
		w.commands = append(w.commands, intake.RelayedCommand{RequestID: request, CommunityID: community, UserID: user, DeviceID: device, Sealed: sealed})
	}
	return w
}

func (w *loadWorld) handleNext(ctx context.Context) {
	cmd := w.commands[w.next]
	w.next++
	w.entry.in.Handle(ctx, cmd)
}

func waitUntil(t testing.TB, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(time.Millisecond)
	}
}

func cpuTime() time.Duration {
	var u syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &u); err != nil {
		return 0
	}
	return time.Duration(u.Utime.Nano() + u.Stime.Nano())
}

// TestCommandCostIsFlatAt200Members600Devices (Task 15 load bound, spec "200 members / 600 devices"): after the
// snapshot, 200 /remind commands cost no device-list read, one request frame per answer, at most 3 intake helper
// ops each, and the same allocations and CPU time per command at 200 members as at 20.
func TestCommandCostIsFlatAt200Members600Devices(t *testing.T) {
	ctx := context.Background()
	w := newLoadWorld(t, 200, true)
	if len(w.devices) != 200 || len(w.members)*3 != 600 {
		t.Fatalf("world has %d members", len(w.members))
	}
	_, ingestsBefore := w.helper.counts()
	opsBefore, _ := w.helper.counts()
	for range loadCommands {
		w.handleNext(ctx)
	}
	ops, ingests := w.helper.counts()
	frames := w.transport.take()
	stateOps := w.state.taken()
	replies := frames["POST /v1/bot-runtime/replies"]
	total := 0
	for route, n := range frames {
		total += n
		if strings.Contains(route, "device-list") || strings.Contains(route, "/members") {
			t.Fatalf("a command read %s %d times", route, n)
		}
	}
	if ingests != ingestsBefore {
		t.Fatalf("commands ingested device lists: %d", ingests-ingestsBefore)
	}
	if replies != loadCommands || total != replies {
		t.Fatalf("request frames %v: want exactly one reply per command (%d)", frames, loadCommands)
	}
	perCommand := float64(ops-opsBefore) / loadCommands
	if perCommand > 3 {
		t.Fatalf("intake helper ops per command = %.2f, want ≤ 3", perCommand)
	}
	t.Logf("per /remind: %d request frame(s), %.0f intake helper ops (open, seal), %.0f reminder state ops, 0 device-list reads",
		total/loadCommands, perCommand, float64(stateOps)/loadCommands)

	// Allocations and CPU time per command: 200 members against 20.
	small := newLoadWorld(t, 20, true)
	large := newLoadWorld(t, 200, true)
	for range 20 { // warm both
		small.handleNext(ctx)
		large.handleNext(ctx)
	}
	allocSmall := testing.AllocsPerRun(150, func() { small.handleNext(ctx) })
	allocLarge := testing.AllocsPerRun(150, func() { large.handleNext(ctx) })
	if allocLarge > 1.1*allocSmall {
		t.Fatalf("allocations per command: %.0f at 200 members, %.0f at 20 (ratio %.2f > 1.1)", allocLarge, allocSmall, allocLarge/allocSmall)
	}
	// CPU is process-wide (rusage), so it also charges the collector for whatever heap is live. Each round builds both
	// worlds before measuring either, so both runs share one heap, collects before each run, and alternates which goes
	// first; the best of five rounds per size discards a noisy neighbour on a shared runner.
	run := func(lw *loadWorld) time.Duration {
		runtime.GC()
		start := cpuTime()
		for range loadCommands {
			lw.handleNext(ctx)
		}
		return cpuTime() - start
	}
	cpuSmall, cpuLarge := time.Duration(1<<63-1), time.Duration(1<<63-1)
	for round := range 5 {
		s, l := newLoadWorld(t, 20, true), newLoadWorld(t, 200, true)
		if round%2 == 0 {
			cpuSmall = min(cpuSmall, run(s))
			cpuLarge = min(cpuLarge, run(l))
		} else {
			cpuLarge = min(cpuLarge, run(l))
			cpuSmall = min(cpuSmall, run(s))
		}
	}
	if float64(cpuLarge) > 1.2*float64(cpuSmall) {
		t.Fatalf("CPU per 200 commands: %v at 200 members, %v at 20 (ratio %.2f > 1.2)", cpuLarge, cpuSmall, float64(cpuLarge)/float64(cpuSmall))
	}
	t.Logf("allocations per command %.0f (200 members) vs %.0f (20); CPU per 200 commands %v vs %v", allocLarge, allocSmall, cpuLarge, cpuSmall)
}

// TestRelayedRemindAnswersWithin100ms: an op 64 for /remind, relayed on the delivery loop, is answered "Saved" by the
// real reminder handler within 100 ms, through the queue and a worker.
func TestRelayedRemindAnswersWithin100ms(t *testing.T) {
	w := newLoadWorld(t, 20, false)
	w.transport.first = make(chan time.Time, 1)
	cmd := w.commands[0]
	payload := []cbor.RawMessage{}
	for _, v := range []any{cmd.RequestID[:], cmd.CommunityID[:], cmd.UserID[:], cmd.DeviceID[:], cmd.Sealed} {
		b, err := cborx.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		payload = append(payload, b)
	}
	installation := w.entry.installation
	start := time.Now()
	w.runtime.relayCommand(installation, payload, nil)
	select {
	case at := <-w.transport.first:
		if took := at.Sub(start); took > 100*time.Millisecond {
			t.Fatalf("relay to first answer took %v", took)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no answer")
	}
	if n := w.transport.take()["POST /v1/bot-runtime/replies"]; n != 1 {
		t.Fatalf("replies = %d", n)
	}
}

func TestRelayedCommandDecodeIsStrict(t *testing.T) {
	good := []any{id.New(), id.New(), id.New(), id.New(), []byte{1}}
	encode := func(vs []any) []cbor.RawMessage {
		var out []cbor.RawMessage
		for _, v := range vs {
			b, _ := cborx.Marshal(v)
			out = append(out, b)
		}
		return out
	}
	if _, err := decodeRelayedCommand(encode(good)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]any{good[:4], {id.ID{}, id.New(), id.New(), id.New(), []byte{1}}, {id.New(), id.New(), id.New(), id.New(), []byte{}}, {id.New(), "x", id.New(), id.New(), []byte{1}}} {
		if _, err := decodeRelayedCommand(encode(bad)); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
}

// TestSealedIntakeFailsClosedUntilTheFeedIsReady (Task 14 ruling): Held is false for an installation with no feed, so
// the intake refuses every command with 503 E_BOT_OFFLINE until the installation's feed is bound and has applied a
// snapshot; then the same command (its Retry) is served.
func TestSealedIntakeFailsClosedUntilTheFeedIsReady(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	installation, community, botUser, user, device := id.New(), id.New(), id.New(), id.New(), id.New()
	w := newLoadWorld(t, 1, true) // for its runtime, transport and session
	r := w.runtime
	h := &loadHelper{requests: map[string]protocol.SealedRequest{}, replays: map[string][32]byte{}}
	state := &countingState{utilityStateFake: newUtilityStateFake()}
	rem := service.ReminderInteractions{Store: reminders.NewStore(state, native.Binding{Instance: id.New(), Community: community, Installation: installation}), CommunityID: community}
	session, _ := w.entry.current()
	r.SetSealedIntake(sealedIntakeConfig{installation: installation, community: community, botUser: botUser, capabilities: 1 << 9,
		helper: h, session: session.session, reminders: &rem, held: installationHeld{r, installation}, log: log})
	e := r.sealed.lookup(installation)
	e.stop()
	request := id.New()
	var key [32]byte
	key[0] = 1
	sealed := append([]byte("sealed:"), request[:]...)
	h.requests[string(sealed)] = protocol.SealedRequest{RequestID: request, CommunityID: community, Operation: uint64(protocol.OpReminderCreate),
		Args: []any{uint64(600), "x"}, SentAt: uint64(time.Now().Unix()), ReplyKey: key}
	cmd := intake.RelayedCommand{RequestID: request, CommunityID: community, UserID: user, DeviceID: device, Sealed: sealed}
	last := func() intake.Answer {
		t.Helper()
		w.transport.mu.Lock()
		defer w.transport.mu.Unlock()
		if len(w.transport.answers) == 0 {
			t.Fatal("no answer")
		}
		return w.transport.answers[len(w.transport.answers)-1]
	}
	retry := func(k byte) {
		resealed := append(append([]byte(nil), sealed...), k)
		var rk [32]byte
		rk[0] = k
		h.mu.Lock()
		h.replays[string(resealed)] = rk
		h.mu.Unlock()
		c := cmd
		c.Sealed = resealed
		e.in.Handle(ctx, c)
	}
	// After the start-up window (no park, final review I2), the fail-closed answers come at once.
	e.in.SetParkWindow(0)
	// No feed at all: Held cannot know, so the intake is offline.
	e.in.Handle(ctx, cmd)
	if a := last(); a.Status != 503 || string(a.Body) != "E_BOT_OFFLINE" {
		t.Fatalf("no feed: %+v", a)
	}
	// A feed, but no snapshot applied yet: still offline.
	feed := r.feedFor(installation, community, botUser, feedDeps{})
	tee := r.sealed.feed(installation, community, feed)
	retry(2)
	if a := last(); a.Status != 503 {
		t.Fatalf("no snapshot: %+v", a)
	}
	tee.SnapshotBegin(ctx)
	tee.Role(ctx, RoleEvent{Community: community, Role: id.New(), Allow: intake.PermViewChannel, FromSnapshot: true})
	tee.Member(ctx, MemberEvent{Community: community, User: user, Joined: true, Snapshot: true})
	tee.SnapshotDone(ctx)
	retry(3)
	if a := last(); a.Status != 200 || !strings.HasPrefix(string(a.Body), "Saved") {
		t.Fatalf("ready: %+v", a)
	}
}

// Fix round 1, M-2: a command sealed to a key the helper does not hold, and a new Bot session, each make the publisher
// send the key and the catalog again even though their bytes did not change (dillad may hold an older publication, or
// none after a restore), so the client's refetch finds the current key.
func TestStaleKeyAndNewSessionRepublishUnchangedBytes(t *testing.T) {
	w := newLoadWorld(t, 3, false)
	puts := func() int {
		w.transport.mu.Lock()
		defer w.transport.mu.Unlock()
		n := 0
		for route, c := range w.transport.frames {
			if strings.HasSuffix(route, "/command-key") || strings.HasSuffix(route, "/catalog") {
				n += c
			}
		}
		return n
	}
	if n := puts(); n != 0 {
		t.Fatalf("puts before = %d", n)
	}
	cmd := w.commands[0]
	cmd.Sealed = []byte("unknown-key")
	w.entry.in.Handle(context.Background(), cmd)
	waitUntil(t, func() bool { return puts() == 2 })
	cfg, _ := w.entry.current()
	api, err := client.New(client.Config{BaseURL: "http://127.0.0.1:1", InstanceID: id.New(), AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := api.Session("another-bot-session")
	if err != nil {
		t.Fatal(err)
	}
	// A new session of the same Bot: republished through it (it has no transport here, so the PUTs fail and are
	// retried; what matters is that the publisher forgot what dillad held and tried again).
	cfg.session = fresh
	w.runtime.SetSealedIntake(cfg)
	w.entry.mu.Lock()
	forgot := w.entry.publisher != nil
	w.entry.mu.Unlock()
	if !forgot {
		t.Fatal("no publisher")
	}
	// Back on the original session: the next publish sends both again.
	cfg.session = w.session
	w.runtime.SetSealedIntake(cfg)
	waitUntil(t, func() bool { return puts() >= 4 })
}

// blockRole holds every role event until released: a feed whose worker lags behind the delivery loop.
type blockRole struct {
	Feed
	release chan struct{}
}

func (b blockRole) Role(ctx context.Context, ev RoleEvent) {
	<-b.release
	b.Feed.Role(ctx, ev)
}

// Fix round 1, I-3: a command waits until every op 65 delivered before it is applied. A role removal delivered just
// before a command, still being applied when the command arrives, is applied before the command is authorised: the
// command is refused, never served against the old role.
func TestRoleRemovalDeliveredBeforeACommandIsAppliedFirst(t *testing.T) {
	w := newLoadWorld(t, 3, true)
	ctx := context.Background()
	e := w.entry
	cfg, _ := e.current()
	cfg.capabilities = 1<<8 | 1<<9 // polls: poll create is channel-scoped and needs send_messages
	w.runtime.SetSealedIntake(cfg)
	installation, community := e.installation, e.community
	channel := id.New()
	everyone := id.New()
	tee := w.runtime.sealed.feed(installation, community, w.runtime.feedHandler(installation))
	tee.SnapshotBegin(ctx)
	tee.Role(ctx, RoleEvent{Community: community, Role: everyone, Allow: intake.PermViewChannel | intake.PermSendMessages, FromSnapshot: true})
	for _, u := range w.members {
		tee.Member(ctx, MemberEvent{Community: community, User: u, Joined: true, Snapshot: true})
	}
	tee.(authzFeed).ChannelOverwrites(ctx, community, channel, nil, false, true)
	tee.SnapshotDone(ctx)

	// The feed's worker is behind: it holds role events until released.
	block := blockRole{Feed: tee, release: make(chan struct{})}
	fc := newFeedConsumer(community, &orderedAcks{log: quietLogger()}, quietLogger())
	fc.setFeed(block)
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go fc.run(runCtx)

	// op 65: @everyone loses send_messages; then op 64: a poll created in the channel.
	removal := botEventRaw(t, feedRole, []any{community, everyone, []uint64{intake.PermViewChannel, 0, 0}, uint64(0)})
	fc.deliver(gatewayclient.Delivery{Frame: gatewayclient.Frame{Op: gatewayclient.OpBotEvent, BotPayload: removal}}, nil)
	ready := fc.barrier()

	cmd := w.commands[0]
	req := w.helper.requests[string(cmd.Sealed)]
	req.Operation, req.ChannelID, req.Args = uint64(protocol.OpPollCreate), channel, []any{"Lunch?", "a | b", nil, nil, nil}
	w.helper.requests[string(cmd.Sealed)] = req
	cmd.Ready = ready
	done := make(chan struct{})
	go func() { w.entry.in.Handle(ctx, cmd); close(done) }()
	select {
	case <-done:
		t.Fatal("the command was handled before the role removal delivered ahead of it was applied")
	case <-time.After(200 * time.Millisecond):
	}
	close(block.release)
	<-done
	w.transport.mu.Lock()
	defer w.transport.mu.Unlock()
	if len(w.transport.answers) == 0 {
		t.Fatal("no answer")
	}
	if a := w.transport.answers[len(w.transport.answers)-1]; a.Status != 403 {
		t.Fatalf("answer = %+v, want 403 from the applied role", a)
	}
}

// Fix round 1, I-3: past the 2 s bound the command is answered offline (retryable), never authorised against a feed
// that is still behind.
func TestCommandBehindAStalledFeedIsOffline(t *testing.T) {
	w := newLoadWorld(t, 3, true)
	ctx := context.Background()
	fc := newFeedConsumer(w.community, &orderedAcks{log: quietLogger()}, quietLogger())
	// No handler is set: the feed holds every job.
	member := botEventRaw(t, feedMember, []any{w.community, id.New(), uint64(1), []id.ID{}})
	fc.deliver(gatewayclient.Delivery{Frame: gatewayclient.Frame{Op: gatewayclient.OpBotEvent, BotPayload: member}}, nil)
	feedBarrierBound = 50 * time.Millisecond
	defer func() { feedBarrierBound = 2 * time.Second }()
	cmd := w.commands[0]
	cmd.Ready = fc.barrier()
	w.entry.in.SetParkWindow(0) // past the start-up window (final review I2): no park
	w.entry.in.Handle(ctx, cmd)
	w.transport.mu.Lock()
	defer w.transport.mu.Unlock()
	if len(w.transport.answers) != 1 || w.transport.answers[0].Status != 503 || string(w.transport.answers[0].Body) != "E_BOT_OFFLINE" {
		t.Fatalf("answers = %+v", w.transport.answers)
	}
}

// gatedRoles holds each role event until its own gate is released, in order: a feed worker behind the delivery loop.
type gatedRoles struct {
	Feed
	mu      sync.Mutex
	gates   []chan struct{}
	applied []RoleEvent
}

func (g *gatedRoles) gate() chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	c := make(chan struct{})
	g.gates = append(g.gates, c)
	return c
}

func (g *gatedRoles) Role(_ context.Context, ev RoleEvent) {
	g.mu.Lock()
	var c chan struct{}
	if len(g.gates) > 0 {
		c, g.gates = g.gates[0], g.gates[1:]
	}
	g.mu.Unlock()
	if c != nil {
		<-c
	}
	g.mu.Lock()
	g.applied = append(g.applied, ev)
	g.mu.Unlock()
}

func (g *gatedRoles) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.applied)
}

func roleDelivery(t *testing.T, community id.ID) gatewayclient.Delivery {
	t.Helper()
	raw := botEventRaw(t, feedRole, []any{community, id.New(), []uint64{intake.PermViewChannel, 0, 0}, uint64(0)})
	return gatewayclient.Delivery{Frame: gatewayclient.Frame{Op: gatewayclient.OpBotEvent, BotPayload: raw}}
}

// Fix round 2, N-1 (first scenario), as Task 21b fix round 2 changed it: a command of the old stream waiting on an
// event that was never applied is not released by the fresh identify alone. It waits for the new stream's snapshot,
// which supersedes that event, and runs then: a session reset does not answer it offline.
func TestOldStreamCommandWaitsForTheNewStreamsSnapshot(t *testing.T) {
	community := id.New()
	fc := newFeedConsumer(community, &orderedAcks{log: quietLogger()}, quietLogger())
	// No handler: the role event waits, unapplied.
	fc.deliver(roleDelivery(t, community), nil)
	wait := fc.barrier()
	result := make(chan bool, 1)
	go func() { result <- wait(context.Background()) }()
	time.Sleep(20 * time.Millisecond)
	fc.resetStream()
	select {
	case <-result:
		t.Fatal("the fresh identify alone answered a command whose earlier event was never applied")
	case <-time.After(100 * time.Millisecond):
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	fc.setFeed(&recordingFeed{})
	go fc.run(ctx)
	deliverSnapshot(t, fc, community)
	select {
	case ok := <-result:
		if !ok {
			t.Fatal("the old stream's command was answered offline after the new stream's snapshot")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the new stream's snapshot did not release the waiting command")
	}
}

// deliverSnapshot hands fc an empty cold-start snapshot (begin, end) on its current stream.
func deliverSnapshot(t *testing.T, fc *feedConsumer, community id.ID) {
	t.Helper()
	for _, kind := range []uint64{gateway.BotEventSnapshotBegin, gateway.BotEventSnapshotEnd} {
		fc.deliver(gatewayclient.Delivery{Frame: gatewayclient.Frame{Op: gatewayclient.OpBotEvent, BotPayload: botEventRaw(t, kind, []any{community})}}, nil)
	}
}

// Task 21b fix round 2: a command waiting on a connection that ended ("bot gateway requires session reconciliation")
// runs once the installation's next connection applied its snapshot; with no next connection it is answered offline
// past the bound, never held for ever.
func TestEndedConnectionsCommandRunsAfterTheNextConnectionsSnapshot(t *testing.T) {
	community := id.New()
	old := newFeedConsumer(community, &orderedAcks{log: quietLogger()}, quietLogger())
	old.deliver(roleDelivery(t, community), nil) // never applied: the connection ends first
	wait := old.barrier()
	result := make(chan bool, 1)
	go func() { result <- wait(context.Background()) }()
	var mu sync.Mutex
	var next *feedConsumer
	old.end(func() *feedConsumer { mu.Lock(); defer mu.Unlock(); return next })
	select {
	case <-result:
		t.Fatal("the ended connection answered its command before a next connection was ready")
	case <-time.After(150 * time.Millisecond):
	}
	fresh := newFeedConsumer(community, &orderedAcks{log: quietLogger()}, quietLogger())
	fresh.setFeed(&recordingFeed{})
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go fresh.run(ctx)
	mu.Lock()
	next = fresh
	mu.Unlock()
	select {
	case <-result:
		t.Fatal("the command ran before the next connection's snapshot")
	case <-time.After(150 * time.Millisecond):
	}
	deliverSnapshot(t, fresh, community)
	select {
	case ok := <-result:
		if !ok {
			t.Fatal("the command was answered offline after the next connection's snapshot")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the next connection's snapshot did not release the command")
	}

	feedNextStreamBound = 100 * time.Millisecond
	defer func() { feedNextStreamBound = readyBudget }()
	lone := newFeedConsumer(community, &orderedAcks{log: quietLogger()}, quietLogger())
	lone.deliver(roleDelivery(t, community), nil)
	wait = lone.barrier()
	lone.end(func() *feedConsumer { return nil })
	if wait(context.Background()) {
		t.Fatal("a command of an ended connection with no successor was released")
	}
}

// Fix round 2, N-1 (second scenario): a job of the old stream still running at the reset settles nothing of the new
// stream: a command behind a new-stream event waits for that event, not for the old job.
func TestAJobRunningAtTheResetDoesNotAdvanceTheNewStream(t *testing.T) {
	community := id.New()
	fc := newFeedConsumer(community, &orderedAcks{log: quietLogger()}, quietLogger())
	h := &gatedRoles{Feed: &recordingFeed{}}
	oldGate, newGate := h.gate(), h.gate()
	fc.setFeed(h)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go fc.run(ctx)
	fc.deliver(roleDelivery(t, community), nil) // the old stream's event: the worker takes it and holds
	waitFor(t, "the worker holds the old job", time.Second, func() bool { fc.mu.Lock(); defer fc.mu.Unlock(); return len(fc.jobs) == 0 })
	fc.resetStream()
	fc.deliver(roleDelivery(t, community), nil) // the new stream's live event
	feedBarrierBound = 150 * time.Millisecond
	defer func() { feedBarrierBound = 2 * time.Second }()
	wait := fc.barrier()
	close(oldGate) // the old job ends
	waitFor(t, "the old job is applied", time.Second, func() bool { return h.count() == 1 })
	if wait(ctx) {
		t.Fatal("the old stream's job settled the new stream's event")
	}
	close(newGate)
	waitFor(t, "the new event is applied", time.Second, func() bool { return h.count() == 2 })
	if !fc.barrier()(ctx) {
		t.Fatal("a command after the applied new event was not released")
	}
}

// Task 21b fix round 2: a restarted Bot gets the commands dillad carried across its reset right after its snapshot,
// before its first pass configured the sealed intake. They wait in the intake's queue for the pass instead of being
// dropped ("bot command relay for no sealed intake"); one for another community is still dropped.
func TestACommandBeforeTheFirstPassWaitsForTheSealedIntake(t *testing.T) {
	r := newBotGatewayRuntime(context.Background(), nil, true, quietLogger())
	installation, community := id.New(), id.New()
	command := func(c id.ID) []cbor.RawMessage {
		request, user, device := id.New(), id.New(), id.New()
		return []cbor.RawMessage{rawCBOR(t, request[:]), rawCBOR(t, c[:]), rawCBOR(t, user[:]), rawCBOR(t, device[:]), rawCBOR(t, []byte("sealed"))}
	}
	r.relayCommandFor(installation, community, command(id.New()), nil)
	if r.sealed.lookup(installation) != nil {
		t.Fatal("a command of another community created the installation's intake")
	}
	r.relayCommandFor(installation, community, command(community), nil)
	e := r.sealed.lookup(installation)
	if e == nil || len(e.queue) != 1 {
		t.Fatal("the command before the first pass was not kept for the sealed intake")
	}
	if r.sealed.entry(installation, community) != e {
		t.Fatal("the pass does not find the entry the early command waits in")
	}
}

// lockedLog is a log sink the intake's workers and the test read at once.
type lockedLog struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *lockedLog) count(s string) int { return strings.Count(l.String(), s) }

// registeredHelper is the installation's registered helper as the sealed intake resolves it (musicQueueRegistry.Native
// in production): a pass replaces it, and a helper that closed itself stays registered until the pass retires it.
type registeredHelper struct {
	mu sync.Mutex
	h  *loadHelper
}

func (r *registeredHelper) set(h *loadHelper) { r.mu.Lock(); r.h = h; r.mu.Unlock() }

func (r *registeredHelper) resolve() sealedHelper {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.h == nil {
		return nil
	}
	return r.h
}

// followRegistry makes w's sealed intake resolve its helper from reg on every call, and log to log.
func followRegistry(w *loadWorld, reg *registeredHelper, log *slog.Logger) {
	w.entry.mu.Lock()
	w.entry.cfg.resolve = reg.resolve
	w.entry.cfg.log = log
	w.entry.mu.Unlock()
	w.entry.in.SetLogger(log)
	w.entry.in.SetParkWindow(0) // long past the start-up window, as on CT 144
}

func (w *loadWorld) lastAnswer() (intake.Answer, int) {
	w.transport.mu.Lock()
	defer w.transport.mu.Unlock()
	if len(w.transport.answers) == 0 {
		return intake.Answer{}, 0
	}
	return w.transport.answers[len(w.transport.answers)-1], len(w.transport.answers)
}

func shortHelperPark(t *testing.T, limit time.Duration) {
	t.Helper()
	oldLimit, oldStep := helperParkLimit, helperParkStep
	helperParkLimit, helperParkStep = limit, time.Millisecond
	t.Cleanup(func() { helperParkLimit, helperParkStep = oldLimit, oldStep })
}

// v0.1.0-bot2.2 (F4): the intake held the pass's helper, so after a close every command went to the stale client
// (seal_error). It now resolves the registered helper on every call: a replacement between a command's open and its
// answer seals the answer.
func TestSealedIntakeFollowsReplacement(t *testing.T) {
	w := newLoadWorld(t, 3, true)
	logs := &lockedLog{}
	reg := &registeredHelper{}
	a, b := w.helper, &loadHelper{requests: w.helper.requests}
	reg.set(a)
	followRegistry(w, reg, slog.New(slog.NewTextHandler(logs, nil)))
	a.onOpen = func() { reg.set(b); a.close() }
	w.handleNext(t.Context())
	answer, n := w.lastAnswer()
	if n == 0 || !answer.Final || answer.Status >= 400 {
		t.Fatalf("answer %+v (%d answers), want the command's final answer", answer, n)
	}
	if a.sealCount() != 0 || b.sealCount() != 1 {
		t.Fatalf("seals: closed helper %d, replacement %d; want 0 and 1", a.sealCount(), b.sealCount())
	}
	if c := logs.count("bot command answer not sealed"); c != 0 {
		t.Fatalf("seal_error logged %d times:\n%s", c, logs)
	}
}

// F4: a helper that dies after a command's open leaves the command unanswerable on it. The command is refused
// retryable (503 E_BOT_OFFLINE) and that answer waits for the replacement helper to seal it, never silence. With no
// replacement in time the failure is logged with its class, and the member's Retry runs the command on the next helper.
func TestSealedIntakeOfflineWhenHelperClosed(t *testing.T) {
	t.Run("sealed_on_the_replacement", func(t *testing.T) {
		shortHelperPark(t, 5*time.Second)
		w := newLoadWorld(t, 3, true)
		logs := &lockedLog{}
		reg := &registeredHelper{}
		a, b := w.helper, &loadHelper{requests: w.helper.requests}
		reg.set(a)
		followRegistry(w, reg, slog.New(slog.NewTextHandler(logs, nil)))
		a.onOpen = a.close
		done := make(chan struct{})
		go func() { w.handleNext(t.Context()); close(done) }()
		waitUntil(t, func() bool { return logs.count("bot command answer waiting for a replacement helper") == 1 })
		reg.set(b)
		<-done
		answer, n := w.lastAnswer()
		if n != 1 || answer.Status != intake.StatusOffline || string(answer.Body) != intake.CodeOffline || !answer.Final {
			t.Fatalf("answers %d, last %+v; want one 503 E_BOT_OFFLINE", n, answer)
		}
		if b.sealCount() != 1 || logs.count("class=helper_gone") != 1 {
			t.Fatalf("replacement seals %d; logs:\n%s", b.sealCount(), logs)
		}
		if c := logs.count("bot command answer not sealed"); c != 0 {
			t.Fatalf("seal_error logged %d times:\n%s", c, logs)
		}
	})
	t.Run("no_replacement_in_time", func(t *testing.T) {
		shortHelperPark(t, 20*time.Millisecond)
		w := newLoadWorld(t, 3, true)
		logs := &lockedLog{}
		reg := &registeredHelper{}
		a, b := w.helper, &loadHelper{requests: w.helper.requests, replays: map[string][32]byte{}}
		reg.set(a)
		followRegistry(w, reg, slog.New(slog.NewTextHandler(logs, nil)))
		a.onOpen = a.close
		cmd := w.commands[0]
		w.entry.in.Handle(t.Context(), cmd)
		if _, n := w.lastAnswer(); n != 0 {
			t.Fatalf("%d answers sealed with no helper", n)
		}
		if logs.count("bot command answer not sealed") != 1 || logs.count("error_class=closed op=23") != 1 {
			t.Fatalf("the unsealed answer is not logged with its class:\n%s", logs)
		}
		// The member's Retry: a new seal of the same request, which the replacement opens as a Replay with a new key.
		reg.set(b)
		retry := cmd
		retry.Sealed = append(append([]byte(nil), cmd.Sealed...), 'r')
		var key [32]byte
		key[0] = 2
		b.replays[string(retry.Sealed)] = key
		w.entry.in.Handle(t.Context(), retry)
		answer, n := w.lastAnswer()
		if n != 1 || !answer.Final || answer.Status >= 400 {
			t.Fatalf("Retry on the replacement: %d answers, last %+v; want the command run", n, answer)
		}
	})
}

// A helper that closes itself (invalidated_decode) applying the same pending Welcome after every respawn: each close is
// logged with its reason, the due tick logs each dead helper once, the respawns back off (a replacement that served one
// tick does not reset the backoff), and every sealed command in between is answered 503 E_BOT_OFFLINE on the next
// helper, never silence.
func TestSelfClosingHelperOnPendingWelcome(t *testing.T) {
	shortHelperPark(t, 5*time.Second)
	var delays []time.Duration
	var scheduled []func()
	var schedMu sync.Mutex
	defer func(f func(time.Duration, func())) { helperRespawnAfter = f }(helperRespawnAfter)
	helperRespawnAfter = func(d time.Duration, f func()) {
		schedMu.Lock()
		delays, scheduled = append(delays, d), append(scheduled, f)
		schedMu.Unlock()
	}
	w := newLoadWorld(t, 3, true)
	logs := &lockedLog{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	reg := &registeredHelper{}
	followRegistry(w, reg, log)
	installation := w.entry.installation
	loop := newBotGatewayRuntime(t.Context(), nil, true, log)
	defer loop.Close()
	now := time.Now()
	loop.now = func() time.Time { return now }
	// The pass: a new helper, registered, with the installation's maintenance on it. Applying the pending Welcome
	// makes it invalidate itself (native.Client logs that close once, with its reason).
	respawn := func() *loadHelper {
		h := &loadHelper{requests: w.helper.requests}
		h.onOpen = func() {
			h.close()
			log.Warn("bot native helper closed", "installation_id", installation.String(), "reason", helper.ReasonInvalidatedDecode)
		}
		reg.set(h)
		loop.SetMaintenance(installation, backgroundMaintenance{client: new(native.Client), usable: h.Usable, run: func(context.Context) error { return nil }})
		return h
	}
	tick := func() {
		now = now.Add(dueInterval)
		if err := loop.maintainDue(t.Context()); err != nil {
			t.Fatalf("due tick = %v, want no invented error", err)
		}
	}
	asked := func() bool { targets, _ := loop.takePassTargets(); return targets[installation] }
	respawn()
	for cycle := range 3 {
		tick() // the helper serves one tick
		done := make(chan struct{})
		go func() { w.handleNext(t.Context()); close(done) }()
		waitUntil(t, func() bool { return logs.count("bot command answer waiting for a replacement helper") == cycle+1 })
		tick() // the dead helper
		if cycle > 0 {
			if asked() {
				t.Fatalf("cycle %d: asked for a pass at once, the short-lived replacement reset the backoff", cycle)
			}
			schedMu.Lock()
			last := scheduled[len(scheduled)-1]
			schedMu.Unlock()
			last()
		}
		if !asked() {
			t.Fatalf("cycle %d: no respawn asked", cycle)
		}
		next := respawn()
		<-done
		answer, n := w.lastAnswer()
		if n != cycle+1 || answer.Status != intake.StatusOffline || string(answer.Body) != intake.CodeOffline {
			t.Fatalf("cycle %d: %d answers, last %+v; want 503 E_BOT_OFFLINE", cycle, n, answer)
		}
		if next.sealCount() != 1 {
			t.Fatalf("cycle %d: the replacement sealed %d answers, want 1", cycle, next.sealCount())
		}
	}
	if !slices.Equal(delays, []time.Duration{10 * time.Second, 20 * time.Second}) {
		t.Fatalf("respawn delays %v, want [10s 20s]", delays)
	}
	if c := logs.count("reason=invalidated_decode"); c != 3 {
		t.Fatalf("%d close lines with the reason, want 3:\n%s", c, logs)
	}
	if c := logs.count("bot native helper gone"); c != 3 {
		t.Fatalf("%d gone lines, want one per dead helper (3):\n%s", c, logs)
	}
	if c := logs.count("bot native helper respawn requested"); c != 3 {
		t.Fatalf("%d respawn lines, want 3:\n%s", c, logs)
	}
	if c := logs.count("bot command answer not sealed"); c != 0 {
		t.Fatalf("a command ended in seal_error:\n%s", logs)
	}
}

// kickCounter is a sealed intake's kick: it counts the asks and, like the pass it asks for, registers start() as the
// replacement helper after delay (a fake helper start).
type kickCounter struct {
	n     atomic.Int32
	delay time.Duration
	start func()
}

func (k *kickCounter) kick() {
	if k.n.Add(1) == 1 && k.start != nil {
		time.AfterFunc(k.delay, k.start)
	}
}

func setKick(w *loadWorld, k *kickCounter) {
	w.entry.mu.Lock()
	w.entry.cfg.kick = k.kick
	w.entry.mu.Unlock()
}

// v0.1.0-bot2.2 final review I3: a parked command asks for the replacement helper at once (the runtime's respawn, on its
// backoff), instead of waiting for the next due tick (up to 10 s) to find the dead one. It asks once per park, and a
// replacement that is ready inside the park budget answers the command.
func TestParkedCommandKicksRespawnOnce(t *testing.T) {
	t.Run("open_parked_on_a_closed_helper", func(t *testing.T) {
		shortHelperPark(t, 2*time.Second)
		w := newLoadWorld(t, 3, true)
		reg := &registeredHelper{}
		a, b := w.helper, &loadHelper{requests: w.helper.requests}
		a.close()
		reg.set(a)
		followRegistry(w, reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
		k := &kickCounter{delay: 50 * time.Millisecond, start: func() { reg.set(b) }}
		setKick(w, k)
		w.handleNext(t.Context())
		answer, n := w.lastAnswer()
		if n != 1 || !answer.Final || answer.Status >= 400 {
			t.Fatalf("answers %d, last %+v; want the command run on the replacement", n, answer)
		}
		if got := k.n.Load(); got != 1 {
			t.Fatalf("kicks = %d, want 1", got)
		}
	})
	t.Run("answer_parked_after_a_self_close", func(t *testing.T) {
		shortHelperPark(t, 2*time.Second)
		w := newLoadWorld(t, 3, true)
		reg := &registeredHelper{}
		a, b := w.helper, &loadHelper{requests: w.helper.requests}
		reg.set(a)
		followRegistry(w, reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
		a.onOpen = a.close
		k := &kickCounter{delay: 50 * time.Millisecond, start: func() { reg.set(b) }}
		setKick(w, k)
		w.handleNext(t.Context())
		answer, n := w.lastAnswer()
		if n != 1 || answer.Status != intake.StatusOffline || string(answer.Body) != intake.CodeOffline || b.sealCount() != 1 {
			t.Fatalf("answers %d, last %+v, replacement seals %d; want one 503 E_BOT_OFFLINE sealed on the replacement", n, answer, b.sealCount())
		}
		if got := k.n.Load(); got != 1 {
			t.Fatalf("kicks = %d, want 1", got)
		}
	})
	t.Run("no_registered_helper", func(t *testing.T) {
		shortHelperPark(t, 30*time.Millisecond)
		w := newLoadWorld(t, 3, true)
		followRegistry(w, &registeredHelper{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		k := &kickCounter{}
		setKick(w, k)
		w.handleNext(t.Context())
		if got := k.n.Load(); got != 1 {
			t.Fatalf("kicks = %d, want 1 for the whole park", got)
		}
	})
	t.Run("usable_helper_never_kicks", func(t *testing.T) {
		w := newLoadWorld(t, 3, true)
		reg := &registeredHelper{}
		reg.set(w.helper)
		followRegistry(w, reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
		k := &kickCounter{}
		setKick(w, k)
		w.handleNext(t.Context())
		if answer, n := w.lastAnswer(); n != 1 || answer.Status >= 400 || k.n.Load() != 0 {
			t.Fatalf("answers %d, last %+v, kicks %d", n, answer, k.n.Load())
		}
	})
}

// The pass's kick is the runtime's respawn ask, on its backoff: the first park asks at once, the parks after it within
// the backoff schedule one ask between them, never a storm.
func TestSealedIntakeKickIsTheRespawnBackoff(t *testing.T) {
	delays, fire := captureRespawns(t)
	r := newBotGatewayRuntime(t.Context(), nil, true, quietLogger())
	defer r.Close()
	installation := id.New()
	cfg := newSealedIntakeConfig(r, installation, id.New(), id.New(), 0, &loadHelper{}, nil, nil, nil)
	if cfg.kick == nil {
		t.Fatal("no kick wired")
	}
	asked := func() bool { targets, _ := r.takePassTargets(); return targets[installation] }
	cfg.kick()
	if !asked() || len(delays()) != 0 {
		t.Fatalf("first kick: want a pass at once, delays %v", delays())
	}
	for range 5 {
		cfg.kick()
	}
	if asked() || !slices.Equal(delays(), []time.Duration{helperRespawnBase}) {
		t.Fatalf("kicks during the backoff: delays %v, want one ask after %v", delays(), helperRespawnBase)
	}
	fire()
	if !asked() {
		t.Fatal("the scheduled ask asked for no pass")
	}
}
