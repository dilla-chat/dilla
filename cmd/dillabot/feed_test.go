//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/gatewayclient"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
)

func feedID(b byte) id.ID { var v id.ID; v[0], v[15] = 0x7a, b; return v }

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// botEventRaw is an op 65 payload as dillad encodes it (gateway.BotEventPayload), split into its two elements.
func botEventRaw(t *testing.T, kind uint64, body any) []cbor.RawMessage {
	t.Helper()
	p, err := gateway.BotEventPayload(kind, body)
	if err != nil {
		t.Fatal(err)
	}
	var out []cbor.RawMessage
	if err := cborx.Unmarshal(p, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestFeedDecodesEveryKindAsDilladEncodesIt: the Bot reads each op 65 kind exactly in the shape dillad's gateway
// encodes it (Task 12 as landed: role bits as [allow, deny, position], kind 8 owner, uint flags, kind 1 push as bstr),
// and refuses a wrong shape.
func TestFeedDecodesEveryKindAsDilladEncodesIt(t *testing.T) {
	c, u, r, ch, g := feedID(1), feedID(2), feedID(3), feedID(4), feedID(5)
	push := []byte{0xa1, 0x01, 0x02}
	for _, tc := range []struct {
		kind uint64
		body any
		want func(feedEvent) bool
	}{
		{gateway.BotEventDeviceList, []any{u, push}, func(e feedEvent) bool { return e.user == u && bytes.Equal(e.push, push) }},
		{gateway.BotEventMember, []any{c, u, uint64(1), []id.ID{r}}, func(e feedEvent) bool {
			return e.member.Community == c && e.member.User == u && e.member.Joined && slices.Equal(e.member.Roles, []id.ID{r})
		}},
		{gateway.BotEventMember, []any{c, u, uint64(0), []id.ID{}}, func(e feedEvent) bool { return !e.member.Joined && len(e.member.Roles) == 0 }},
		{gateway.BotEventRole, []any{c, r, []uint64{7, 2, 3}, uint64(0)}, func(e feedEvent) bool {
			return e.role == RoleEvent{Community: c, Role: r, Allow: 7, Deny: 2, Position: 3}
		}},
		{gateway.BotEventRole, []any{c, r, []uint64{0, 0, 1}, uint64(1)}, func(e feedEvent) bool { return e.role.Deleted }},
		{gateway.BotEventChannelGroup, []any{c, ch, g, uint64(9)}, func(e feedEvent) bool {
			return e.group == ChannelGroupEvent{Community: c, Channel: ch, Group: g, Epoch: 9}
		}},
		{gateway.BotEventBotChannel, []any{c, ch, uint64(1)}, func(e feedEvent) bool { return e.channel == ch && e.marked }},
		{gateway.BotEventSnapshotBegin, []any{c}, func(e feedEvent) bool { return e.community == c }},
		{gateway.BotEventSnapshotEnd, []any{c}, func(e feedEvent) bool { return e.community == c }},
		{gateway.BotEventCommunity, []any{c, u}, func(e feedEvent) bool { return e.community == c && e.owner == u }},
	} {
		ev, err := decodeFeedEvent(botEventRaw(t, tc.kind, tc.body))
		if err != nil || ev.kind != tc.kind || !tc.want(ev) {
			t.Fatalf("kind %d: %+v, %v", tc.kind, ev, err)
		}
	}
	for _, bad := range []struct {
		kind uint64
		body any
	}{
		{gateway.BotEventDeviceList, []any{u, uint64(1), push}}, // the brief's three-element form is not what dillad sends
		{gateway.BotEventMember, []any{c, u, uint64(2), []id.ID{}}},
		{gateway.BotEventRole, []any{c, r, uint64(7), uint64(0)}},
		{gateway.BotEventBotChannel, []any{c, ch, uint64(2)}},
		{99, []any{c}},
	} {
		if _, err := decodeFeedEvent(botEventRaw(t, bad.kind, bad.body)); err == nil {
			t.Fatalf("kind %d with %v decoded", bad.kind, bad.body)
		}
	}
}

// fakeDillad is dillad's Bot socket for the feed tests: /gateway on a Unix socket, speaking hello, identify (with the
// launch token), ready, resume and heartbeats, and sending what the test scripts.
type fakeDillad struct {
	t                   *testing.T
	sock                string
	instance, dev, user id.ID
	launch              []byte
	mu                  sync.Mutex
	identifies, resumes int
	bearers, launches   []string
	acked               uint64 // the highest last_n a heartbeat or resume reported
	ackedAt             map[uint64]time.Time
	session             func(ctx context.Context, ws *websocket.Conn, resumeFrom uint64, resumed bool)
	// onIdentify, when set, decides an identify: "ready", "drop" (close before ready) or "refuse" (E_UNAUTHENTICATED).
	onIdentify    func(n int, launch string) string
	lastResumeAck uint64
	srv           *http.Server
}

func newFakeDillad(t *testing.T, session func(ctx context.Context, ws *websocket.Conn, resumeFrom uint64, resumed bool)) *fakeDillad {
	t.Helper()
	d := &fakeDillad{t: t, sock: filepath.Join(t.TempDir(), "gateway.sock"), instance: feedID(0x11), dev: feedID(0x12), user: feedID(0x13),
		launch: bytes.Repeat([]byte{0x42}, 32), session: session, ackedAt: map[uint64]time.Time{}}
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", d.sock)
	if err != nil {
		t.Fatal(err)
	}
	d.srv = &http.Server{Handler: http.HandlerFunc(d.serve), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = d.srv.Serve(l) }()
	t.Cleanup(func() { _ = d.srv.Close() })
	return d
}

func (d *fakeDillad) send(ctx context.Context, ws *websocket.Conn, op gatewayclient.Opcode, n uint64, payload []any) error {
	b, err := cborx.Marshal([]any{uint64(op), n, nil, payload})
	if err != nil {
		return err
	}
	return ws.Write(ctx, websocket.MessageBinary, b)
}

func (d *fakeDillad) sendRaw(ctx context.Context, ws *websocket.Conn, n uint64, payload []cbor.RawMessage) error {
	b, err := cborx.Marshal([]any{uint64(gatewayclient.OpBotEvent), n, nil, payload})
	if err != nil {
		return err
	}
	return ws.Write(ctx, websocket.MessageBinary, b)
}

func (d *fakeDillad) noteAck(n uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if n > d.acked {
		for k := d.acked + 1; k <= n; k++ {
			d.ackedAt[k] = time.Now()
		}
		d.acked = n
	}
}

func (d *fakeDillad) ackedThrough() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.acked
}

func (d *fakeDillad) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/gateway" {
		http.NotFound(w, r)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"dilla.v1"}})
	if err != nil {
		return
	}
	defer ws.CloseNow()
	ws.SetReadLimit(1 << 20)
	ctx := r.Context()
	hello := []any{[]any{uint64(6)}, []any{uint64(1)}, []any{uint64(1)}, uint64(1000), uint64(131584), d.instance, uint64(7), uint64(10), uint64(10)}
	if d.send(ctx, ws, gatewayclient.OpHello, 0, hello) != nil {
		return
	}
	_, raw, err := ws.Read(ctx)
	if err != nil {
		return
	}
	var frame []cbor.RawMessage
	var payload []cbor.RawMessage
	var op uint64
	if cborx.Unmarshal(raw, &frame) != nil || len(frame) != 4 || cborx.Unmarshal(frame[0], &op) != nil || cborx.Unmarshal(frame[3], &payload) != nil {
		d.t.Errorf("first frame does not decode")
		return
	}
	token := bytes32For(0x20)
	resumed := false
	var resumeFrom uint64
	d.mu.Lock()
	d.bearers = append(d.bearers, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	d.mu.Unlock()
	switch gatewayclient.Opcode(op) {
	case gatewayclient.OpIdentify:
		var launch string
		_ = cborx.Unmarshal(payload[0], &launch)
		d.mu.Lock()
		d.identifies++
		n := d.identifies
		d.launches = append(d.launches, launch)
		decide := d.onIdentify
		d.mu.Unlock()
		if decide != nil {
			switch decide(n, launch) {
			case "drop":
				_ = ws.Close(4000, "drop")
				return
			case "refuse":
				_ = d.send(ctx, ws, gatewayclient.OpError, 0, []any{uint64(0), "E_UNAUTHENTICATED", "identify"})
				_ = ws.Close(4003, "E_UNAUTHENTICATED")
				return
			}
		}
		ready := []any{d.dev, d.user, uint64(7), token, uint64(6), uint64(1), uint64(1), uint64(0), []any{}, feedID(0x77)}
		if d.send(ctx, ws, gatewayclient.OpReady, 1, ready) != nil {
			return
		}
	case gatewayclient.OpResume:
		var lastN uint64
		_ = cborx.Unmarshal(payload[3], &lastN)
		d.noteAck(lastN)
		d.mu.Lock()
		d.resumes++
		d.lastResumeAck = lastN
		d.mu.Unlock()
		resumed, resumeFrom = true, lastN+1
	default:
		d.t.Errorf("first frame op %d", op)
		return
	}
	go func() {
		for {
			_, raw, err := ws.Read(ctx)
			if err != nil {
				return
			}
			var f []cbor.RawMessage
			var o uint64
			var p []cbor.RawMessage
			if cborx.Unmarshal(raw, &f) != nil || cborx.Unmarshal(f[0], &o) != nil || cborx.Unmarshal(f[3], &p) != nil {
				continue
			}
			if gatewayclient.Opcode(o) == gatewayclient.OpHeartbeat {
				var lastN uint64
				_ = cborx.Unmarshal(p[0], &lastN)
				d.noteAck(lastN)
				_ = d.send(ctx, ws, gatewayclient.OpHeartbeatAck, 0, []any{uint64(time.Now().Unix())})
			}
		}
	}()
	d.session(ctx, ws, resumeFrom, resumed)
}

func bytes32For(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

// botGatewayClient is the supervised Bot's gateway client against d: the Unix socket transport and the launch token.
func botGatewayClient(t *testing.T, d *fakeDillad) *gatewayclient.Client {
	t.Helper()
	api, err := client.New(client.Config{BaseURL: supervisedOrigin, InstanceID: d.instance, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	launch, err := gatewayclient.NewLaunchToken(bytes.Clone(d.launch)) // it takes ownership and clears what it was given
	if err != nil {
		t.Fatal(err)
	}
	gw, err := gatewayclient.New(gatewayclient.Config{Client: api, AllowLoopbackHTTP: true, ExpectedDeviceID: d.dev, ExpectedUserID: d.user,
		HTTPClient: &http.Client{Transport: unixTransport(d.sock)}, Launch: launch, QueueSize: botGatewayQueue, Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	return gw
}

// runFeed runs a connection's delivery loop and feed exactly as botGatewayRuntime.Ensure does.
func runFeed(ctx context.Context, gw *gatewayclient.Client, community id.ID, h Feed) *feedConsumer {
	log := quietLogger()
	acks := &orderedAcks{log: log}
	feed := newFeedConsumer(community, acks, log)
	feed.setFeed(h)
	go feed.run(ctx)
	go func() { _ = gw.Run(ctx, func(context.Context) (string, error) { return "session-bearer", nil }) }()
	go consumeDeliveries(ctx, gw.Events(), acks, feed, log, func(gatewayclient.Delivery) {})
	return feed
}

// recordingFeed records what it applied, in order; block, when set, holds the event whose key it names.
type recordingFeed struct {
	mu      sync.Mutex
	applied []string
	lists   int
	done    int
	block   string
	release chan struct{}
	seenEnd *atomic.Bool
	early   atomic.Bool
}

func (f *recordingFeed) note(key string) {
	if f.block != "" && key == f.block {
		<-f.release
	}
	f.mu.Lock()
	f.applied = append(f.applied, key)
	f.mu.Unlock()
}
func (f *recordingFeed) DeviceList(_ context.Context, _ id.ID, _ []byte, snapshot bool) error {
	if snapshot && f.seenEnd != nil && !f.seenEnd.Load() {
		f.early.Store(true) // a snapshot body applied before dillad sent snapshot_end
	}
	f.mu.Lock()
	f.lists++
	f.mu.Unlock()
	return nil
}
func (f *recordingFeed) SnapshotBegin(context.Context)       {}
func (f *recordingFeed) Member(context.Context, MemberEvent) {}
func (f *recordingFeed) Role(context.Context, RoleEvent)     {}
func (f *recordingFeed) Community(context.Context, id.ID, id.ID, bool) {
}
func (f *recordingFeed) ChannelGroup(context.Context, ChannelGroupEvent) {}
func (f *recordingFeed) BotChannel(_ context.Context, _, channel id.ID, _, _ bool) {
	f.note(channel.String())
}
func (f *recordingFeed) SnapshotDone(context.Context) {
	f.mu.Lock()
	f.done++
	f.mu.Unlock()
}
func (f *recordingFeed) appliedKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.applied)
}

func waitFor(t *testing.T, what string, timeout time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestBotConnectsOverUnixSocketWithLaunchToken: the supervised Bot dials dillad's Bot socket (a Unix socket; there is
// no URL to resolve), its first identify carries the launch token from fd 3 beside its session bearer, and after a drop
// it resumes without the token.
// TestALostReadyAfterTheTokenWasSpentRecovers (fix round 1, I-4): dillad spends the launch token on the first
// identify, then the connection drops before ready. The token presented again is refused E_UNAUTHENTICATED (spent);
// the client then identifies once without it, which dillad admits for the same child, and reaches ready without a
// restart. Each attempt waits on the reconnect backoff.
func TestALostReadyAfterTheTokenWasSpentRecovers(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	ready := make(chan struct{})
	var once sync.Once
	d := newFakeDillad(t, func(ctx context.Context, _ *websocket.Conn, _ uint64, _ bool) {
		once.Do(func() { close(ready) })
		<-ctx.Done()
	})
	token := base64.RawURLEncoding.EncodeToString(d.launch)
	spent := false
	d.onIdentify = func(n int, launch string) string {
		switch {
		case launch == token && !spent:
			spent = true
			return "drop" // spent, but ready never reaches the client
		case launch == token:
			return "refuse"
		case launch == "" && spent:
			return "ready"
		}
		return "refuse"
	}
	runFeed(ctx, botGatewayClient(t, d), feedID(1), &recordingFeed{})
	select {
	case <-ready:
	case <-ctx.Done():
		d.mu.Lock()
		defer d.mu.Unlock()
		t.Fatalf("no ready after a lost one; identifies carried %q", d.launches)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.launches) != 3 || d.launches[0] != token || d.launches[1] != token || d.launches[2] != "" {
		t.Fatalf("identifies carried %q, want the token, the token (refused), then none", d.launches)
	}
}

func TestBotConnectsOverUnixSocketWithLaunchToken(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var conns atomic.Int32
	d := newFakeDillad(t, func(ctx context.Context, ws *websocket.Conn, _ uint64, resumed bool) {
		if conns.Add(1) == 1 {
			_ = ws.Close(4000, "drop") // resumable
			return
		}
		if resumed {
			_ = (&fakeDillad{}).send(ctx, ws, gatewayclient.OpResumed, 2, []any{uint64(2), uint64(1), bytes32For(0x21)})
		}
		<-ctx.Done()
	})
	gw := botGatewayClient(t, d)
	runFeed(ctx, gw, feedID(1), &recordingFeed{})
	waitFor(t, "a resume", 15*time.Second, func() bool { d.mu.Lock(); defer d.mu.Unlock(); return d.resumes > 0 })
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.identifies != 1 || len(d.launches) != 1 || d.launches[0] != base64.RawURLEncoding.EncodeToString(d.launch) {
		t.Fatalf("identifies %d with launch tokens %q, want one carrying the launch token", d.identifies, d.launches)
	}
	for _, b := range d.bearers {
		if b != "session-bearer" {
			t.Fatalf("upgrade bearer %q, want the device session's", b)
		}
	}
}

// TestFeedResumeAfterSocketDrop: live op 65 events 2..6; the handler is held on event 4, so the Bot acknowledges
// through 3 and no further. The socket drops; the Bot resumes from its last ack (3), dillad replays 4..6, and every
// event is applied exactly once, in order. No acknowledgement ever runs ahead of what was applied.
func TestFeedResumeAfterSocketDrop(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	community := feedID(1)
	channels := []id.ID{feedID(2), feedID(3), feedID(4), feedID(5), feedID(6)} // events n = 2..6
	event := func(i int) []cbor.RawMessage {
		return botEventRaw(t, gateway.BotEventBotChannel, []any{community, channels[i], uint64(1)})
	}
	h := &recordingFeed{block: channels[2].String(), release: make(chan struct{})} // event n = 4
	var d *fakeDillad
	var conns atomic.Int32
	d = newFakeDillad(t, func(ctx context.Context, ws *websocket.Conn, from uint64, resumed bool) {
		if conns.Add(1) == 1 {
			for i := range channels {
				if d.sendRaw(ctx, ws, uint64(i+2), event(i)) != nil {
					return
				}
			}
			// Wait until the Bot reported its ack through 3 and, held on 4, nothing beyond it; then drop.
			deadline := time.Now().Add(10 * time.Second)
			for d.ackedThrough() < 3 && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			time.Sleep(1500 * time.Millisecond) // more heartbeats: the ack must stay at 3
			_ = ws.Close(4000, "drop")
			return
		}
		if !resumed || from != 4 {
			d.t.Errorf("second connection resumed=%v from %d, want a resume from 4", resumed, from)
			return
		}
		if d.send(ctx, ws, gatewayclient.OpResumed, 7, []any{uint64(4), uint64(6), bytes32For(0x22)}) != nil {
			return
		}
		for i := 2; i < len(channels); i++ {
			if d.sendRaw(ctx, ws, uint64(i+2), event(i)) != nil {
				return
			}
		}
		<-ctx.Done()
	})
	gw := botGatewayClient(t, d)
	runFeed(ctx, gw, community, h)
	waitFor(t, "the resume", 20*time.Second, func() bool { d.mu.Lock(); defer d.mu.Unlock(); return d.resumes == 1 })
	if got := d.ackedThrough(); got != 3 {
		t.Fatalf("acknowledged through %d before event 4 was applied, want 3", got)
	}
	close(h.release)
	waitFor(t, "every event applied and acknowledged", 10*time.Second, func() bool { return len(h.appliedKeys()) >= 5 && d.ackedThrough() >= 6 })
	want := make([]string, len(channels))
	for i, c := range channels {
		want[i] = c.String()
	}
	time.Sleep(200 * time.Millisecond)
	if got := h.appliedKeys(); !slices.Equal(got, want) {
		t.Fatalf("applied %v, want each of %v once, in order", got, want)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lastResumeAck != 3 || d.identifies != 1 {
		t.Fatalf("resume from ack %d after %d identifies, want 3 and 1", d.lastResumeAck, d.identifies)
	}
}

// TestColdSnapshotIsAppliedAsOneJobWithin5s: from the start of the Bot's connection, a cold-start snapshot of 200 members with
// 600 devices (one device-list push each member) is applied as one job, and the first op 64 command behind it is
// acknowledged within the spec's 5 s. The snapshot body is applied only after snapshot_end arrived, and snapshot_end is
// acknowledged only after SnapshotDone ran.
func TestColdSnapshotIsAppliedAsOneJobWithin5s(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	community := feedID(1)
	const members = 200
	var endSent atomic.Bool
	h := &recordingFeed{seenEnd: &endSent}
	push := bytes.Repeat([]byte{0x33}, 3*400) // three devices' worth of a signed list
	var d *fakeDillad
	d = newFakeDillad(t, func(ctx context.Context, ws *websocket.Conn, _ uint64, _ bool) {
		send := func(n uint64, kind uint64, body any) bool {
			return d.sendRaw(ctx, ws, n, botEventRaw(t, kind, body)) == nil
		}
		if !send(0, gateway.BotEventSnapshotBegin, []any{community}) {
			return
		}
		for i := range members {
			u := feedID(byte(i))
			u[1] = byte(i >> 8)
			if !send(0, gateway.BotEventDeviceList, []any{u, push}) {
				return
			}
		}
		for i := range members {
			u := feedID(byte(i))
			u[1] = byte(i >> 8)
			if !send(0, gateway.BotEventMember, []any{community, u, uint64(1), []id.ID{}}) {
				return
			}
		}
		endSent.Store(true)
		if !send(2, gateway.BotEventSnapshotEnd, []any{community}) {
			return
		}
		cmd := []any{feedID(0x50), community, feedID(0x51), feedID(0x52), []byte{0x85}}
		if d.send(ctx, ws, gatewayclient.OpBotCommand, 3, cmd) != nil {
			return
		}
		<-ctx.Done()
	})
	started := time.Now()
	gw := botGatewayClient(t, d)
	feed := runFeed(ctx, gw, community, h)
	if err := feed.readyWithin(ctx, readyBudget); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the command acknowledged", readyBudget, func() bool { return d.ackedThrough() >= 3 })
	if took := time.Since(started); took > readyBudget {
		t.Fatalf("first command accepted %v after start, want within %v", took, readyBudget)
	}
	if h.early.Load() {
		t.Fatal("a snapshot body event was applied before snapshot_end arrived")
	}
	h.mu.Lock()
	lists, done := h.lists, h.done
	h.mu.Unlock()
	if lists != members || done != 1 {
		t.Fatalf("applied %d device lists and %d snapshot ends, want %d and 1", lists, done, members)
	}
}

// TestSnapshotEndIsNotAcknowledgedBeforeTheSnapshotRan: a handler that never finishes the snapshot leaves
// snapshot_end, and the command behind it, unacknowledged.
func TestSnapshotEndIsNotAcknowledgedBeforeTheSnapshotRan(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	community := feedID(1)
	h := &blockingSnapshot{release: make(chan struct{})}
	defer close(h.release)
	var d *fakeDillad
	d = newFakeDillad(t, func(ctx context.Context, ws *websocket.Conn, _ uint64, _ bool) {
		_ = d.sendRaw(ctx, ws, 0, botEventRaw(t, gateway.BotEventSnapshotBegin, []any{community}))
		_ = d.sendRaw(ctx, ws, 0, botEventRaw(t, gateway.BotEventBotChannel, []any{community, feedID(9), uint64(1)}))
		_ = d.sendRaw(ctx, ws, 2, botEventRaw(t, gateway.BotEventSnapshotEnd, []any{community}))
		_ = d.send(ctx, ws, gatewayclient.OpBotCommand, 3, []any{feedID(0x50), community, feedID(0x51), feedID(0x52), []byte{0x85}})
		<-ctx.Done()
	})
	runFeed(ctx, botGatewayClient(t, d), community, h)
	waitFor(t, "the ready acknowledged", 5*time.Second, func() bool { return d.ackedThrough() >= 1 })
	time.Sleep(1500 * time.Millisecond)
	if got := d.ackedThrough(); got != 1 {
		t.Fatalf("acknowledged through %d while the snapshot is still being applied, want 1", got)
	}
}

type blockingSnapshot struct {
	recordingFeed
	release chan struct{}
}

func (b *blockingSnapshot) SnapshotDone(context.Context) { <-b.release }

// countingPass is a pass that records each run and the requests it sent.
type countingPass struct {
	passes, requests atomic.Int64
	perPass          int64
}

func (p *countingPass) run(context.Context, map[id.ID]bool) error {
	p.passes.Add(1)
	p.requests.Add(p.perPass)
	return nil
}

// TestBotMakesNoPeriodicRequests: over 10 minutes of fake clock with no commands and no events, the Bot sends no
// request frame. Its one pass runs at start; nothing re-runs it on a timer. The 60 s probe and the health report are
// Task 17's and do not exist yet, so the allowance is zero. The due-work tick runs only the local maintenance, which
// with nothing scheduled sends no request.
func TestBotMakesNoPeriodicRequests(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	pass := &countingPass{perPass: 12}
	var maintained atomic.Int64
	ctx, cancel := context.WithCancel(t.Context())
	loop := botLoop{clock: clk, log: quietLogger(), pass: pass.run, passes: make(chan struct{}), take: noTargets, reload: make(chan os.Signal),
		wake: make(chan struct{}), poll: func(context.Context) { t.Error("a text poll ran without a hint") },
		maintain: func(context.Context) error { maintained.Add(1); return nil }, due: dueInterval}
	done := make(chan error, 1)
	go func() { done <- loop.run(ctx) }()
	waitFor(t, "the start pass", 5*time.Second, func() bool { return pass.passes.Load() == 1 })
	startRequests := pass.requests.Load()
	for range 600 {
		clk.Advance(time.Second)
		time.Sleep(200 * time.Microsecond)
	}
	waitFor(t, "the due ticks", 5*time.Second, func() bool { return maintained.Load() >= 50 })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := pass.passes.Load(); got != 1 {
		t.Fatalf("%d passes in 10 minutes without an event, want only the start pass", got)
	}
	if got := pass.requests.Load() - startRequests; got != 0 {
		t.Fatalf("%d request frames in 10 idle minutes, want 0", got)
	}
}

// fakeFeedDeps records what the installation feed asked of its dependencies.
type fakeFeedDeps struct {
	mu       sync.Mutex
	state    native.DeviceListState
	changed  bool
	ingested []id.ID
	welcomed []protocol.Event
	episodes map[id.ID]id.ID
}

func (f *fakeFeedDeps) deps() feedDeps {
	return feedDeps{
		ingest: func(_ context.Context, user id.ID, _ []byte) (native.DeviceListIngest, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.ingested = append(f.ingested, user)
			return native.DeviceListIngest{Changed: f.changed, Version: 2, State: f.state}, nil
		},
		episodes: f,
		welcome: func(_ context.Context, ev protocol.Event) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.welcomed = append(f.welcomed, ev)
			return nil
		},
	}
}

func (f *fakeFeedDeps) ResolveCurrentEpisode(_ context.Context, user id.ID) (id.ID, bool, error) {
	e, ok := f.episodes[user]
	return e, ok, nil
}

// TestInstallationFeedAppliesMembersAndDeviceLists: a snapshot states members without welcoming them; a live join
// welcomes the member (with an event id and time fixed per episode); a live device-list change of a member goes to
// helper op 24; a held list is ingested; the Bot's own events are not welcomed; a later snapshot welcomes the joins it
// missed. Nothing is staged: the v1 interaction registration is gone (Bot v2 Task 23).
func TestInstallationFeedAppliesMembersAndDeviceLists(t *testing.T) {
	ctx := t.Context()
	community, bot, ada, bea, cid := feedID(1), feedID(2), feedID(3), feedID(4), feedID(5)
	deps := &fakeFeedDeps{episodes: map[id.ID]id.ID{ada: feedID(0x31), bea: feedID(0x32), cid: feedID(0x33), bot: feedID(0x34)}, changed: true}
	var passes atomic.Int64
	f := newInstallationFeed(feedID(9), community, bot, quietLogger(), func() { passes.Add(1) })
	f.setDeps(deps.deps())
	f.SnapshotBegin(ctx)
	_ = f.DeviceList(ctx, ada, []byte{1}, true)
	f.Member(ctx, MemberEvent{Community: community, User: ada, Joined: true, Snapshot: true})
	f.Member(ctx, MemberEvent{Community: community, User: bot, Joined: true, Snapshot: true})
	f.SnapshotDone(ctx)
	if len(deps.welcomed) != 0 || len(deps.ingested) != 1 {
		t.Fatalf("after the first snapshot: welcomed %d, ingested %d; want 0, 1", len(deps.welcomed), len(deps.ingested))
	}
	f.Member(ctx, MemberEvent{Community: community, User: bea, Joined: true})
	f.Member(ctx, MemberEvent{Community: community, User: bea, Joined: true}) // a repeat of a known member is no join
	if len(deps.welcomed) != 1 || deps.welcomed[0].Data[0] != bea || deps.welcomed[0].Data[1] != feedID(0x32) {
		t.Fatalf("live join: welcomed %v", deps.welcomed)
	}
	first := deps.welcomed[0]
	_ = f.DeviceList(ctx, bea, []byte{2}, false)
	deps.mu.Lock()
	deps.state, deps.changed = native.DeviceListHeld, false
	deps.mu.Unlock()
	_ = f.DeviceList(ctx, ada, []byte{3}, false)
	waitFor(t, "both lists ingested", time.Second, func() bool {
		deps.mu.Lock()
		defer deps.mu.Unlock()
		return len(deps.ingested) == 3
	})
	// Bea leaves and rejoins under the same episode id: the welcome sees the same event id and time.
	f.Member(ctx, MemberEvent{Community: community, User: bea, Joined: false})
	f.Member(ctx, MemberEvent{Community: community, User: bea, Joined: true})
	if len(deps.welcomed) != 2 || deps.welcomed[1].ID != first.ID || deps.welcomed[1].Timestamp != first.Timestamp {
		t.Fatalf("a retried episode welcomed with %v, want the first event's id and time", deps.welcomed)
	}
	// A reconnect's snapshot names Cid, who joined while the Bot was away: welcomed now.
	f.SnapshotBegin(ctx)
	for _, u := range []id.ID{ada, bea, bot, cid} {
		f.Member(ctx, MemberEvent{Community: community, User: u, Joined: true, Snapshot: true})
	}
	f.SnapshotDone(ctx)
	if len(deps.welcomed) != 3 || deps.welcomed[2].Data[0] != cid {
		t.Fatalf("missed join welcomed %v, want cid", deps.welcomed)
	}
	for _, ev := range deps.welcomed {
		if ev.Data[0] == bot {
			t.Fatal("the Bot welcomed itself")
		}
		if _, err := protocol.EncodeEvent(ev); err != nil {
			t.Fatalf("welcome event does not encode: %v", err)
		}
	}
	if passes.Load() == 0 {
		t.Fatal("no pass was asked for after live changes")
	}
}
