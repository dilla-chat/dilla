package gateway

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

type harness struct {
	gw         *Gateway
	clk        *hookClock
	generation uint64

	mu           sync.Mutex
	sinks        map[*conn]*recordingSink
	groups       map[id.ID][]id.ID       // device -> its groups, what GroupsForDevice answers
	outstanding  map[id.ID]uint64        // group -> proposals_outstanding
	tokens       map[string]auth.Session // session token -> the session it resolves to
	groupScope   map[id.ID]GroupResource
	channelScope map[id.ID]*id.ID
	enabled      map[id.ID]bool
	scopeErr     error
	policyErr    error
}

// hookClock is the harness clock. It is a clock.Fake with one extra power: a test can arm a
// callback to fire on the Nth Clock.Now() call from that moment, which is the only deterministic
// way to reach INSIDE a gateway operation from the test goroutine. Ordering bugs — a frame that
// reaches a connection before its handshake frame, or two producers stamping n in one order and
// enqueueing in the other — are invisible to `go test -race`, because they are not data races.
type hookClock struct {
	*clock.Fake

	mu   sync.Mutex
	left int
	fn   func()
}

func (c *hookClock) Now() time.Time {
	c.mu.Lock()
	var fire func()
	if c.fn != nil {
		c.left--
		if c.left <= 0 {
			fire, c.fn = c.fn, nil
		}
	}
	c.mu.Unlock()
	if fire != nil {
		fire()
	}
	return c.Fake.Now()
}

// armNth fires fn once, on the nth Now() call after this returns. The hook clears itself first,
// so fn may call Now() as much as it likes.
func (c *hookClock) armNth(n int, fn func()) {
	c.mu.Lock()
	c.left, c.fn = n, fn
	c.mu.Unlock()
}

func (c *hookClock) disarm() {
	c.mu.Lock()
	c.fn = nil
	c.mu.Unlock()
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	clk := &hookClock{Fake: clock.NewFake(time.Unix(1_700_000_000, 0))}
	h := &harness{
		clk:          clk,
		generation:   1,
		sinks:        map[*conn]*recordingSink{},
		groups:       map[id.ID][]id.ID{},
		outstanding:  map[id.ID]uint64{},
		tokens:       map[string]auth.Session{},
		groupScope:   map[id.ID]GroupResource{},
		channelScope: map[id.ID]*id.ID{},
		enabled:      map[id.ID]bool{},
	}
	h.gw = New(Options{
		Clock:                  clk,
		Generation:             1,
		Store:                  h, // the harness is the store double: see the three methods below
		Auth:                   h, // and the Authenticator double
		ScopeResolver:          h,
		CommunityCompatibility: h,
		Outstanding: func(_ context.Context, g id.ID) uint64 {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.outstanding[g]
		},
	})
	t.Cleanup(func() {
		// The clock hook must be disarmed before the cleanup Shutdown: a hook still armed would
		// fire from inside Shutdown's own send and reach for locks the test no longer holds.
		clk.disarm()
		_ = h.gw.Shutdown(context.Background())
	})
	return h
}

func (h *harness) GroupResource(_ context.Context, groupID id.ID) (GroupResource, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.scopeErr != nil {
		return GroupResource{}, h.scopeErr
	}
	return h.groupScope[groupID], nil // unknown synthetic test groups default to unscoped
}

func (h *harness) ChannelCommunity(_ context.Context, channelID id.ID) (*id.ID, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.scopeErr != nil {
		return nil, h.scopeErr
	}
	return h.channelScope[channelID], nil
}

func (h *harness) Enabled(_ context.Context, communityID id.ID) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.policyErr != nil {
		return false, h.policyErr
	}
	return h.enabled[communityID], nil
}

// tokenFor is the session token that resolves to this device. connect mints one per device, so a
// resume can be authenticated the way protocol/02 line 135 requires: element 0 of a resume frame
// is a session_token, and the session it names must be the connection's own.
func (h *harness) tokenFor(device id.ID) string { return "session-" + device.String() }

func (h *harness) setGroupsForDevice(device id.ID, groups ...id.ID) {
	h.mu.Lock()
	h.groups[device] = groups
	h.mu.Unlock()
}

func (h *harness) setOutstanding(group id.ID, n uint64) {
	h.mu.Lock()
	h.outstanding[group] = n
	h.mu.Unlock()
}

func (h *harness) setGroupResource(group id.ID, resource GroupResource) {
	h.mu.Lock()
	h.groupScope[group] = resource
	h.mu.Unlock()
}

func (h *harness) setChannelCommunity(channel id.ID, community *id.ID) {
	h.mu.Lock()
	h.channelScope[channel] = community
	h.mu.Unlock()
}

func (h *harness) setCommunityEnabled(community id.ID, enabled bool) {
	h.mu.Lock()
	h.enabled[community] = enabled
	h.mu.Unlock()
}

// connect registers a ready connection without a socket: the transport is task 17's sink
// interface, so the registry and fan-out are testable without a WebSocket.
func (h *harness) connect(t *testing.T, device id.ID) *conn {
	t.Helper()
	sink := newRecordingSink(1024, false)
	hash := make([]byte, 32)
	copy(hash, device[:])
	session := auth.Session{DeviceID: device, UserID: id.New(), Scope: auth.ScopeEnrolled, TokenHash: hash}
	h.mu.Lock()
	h.tokens[h.tokenFor(device)] = session
	h.mu.Unlock()
	c, err := h.gw.register(context.Background(), session, sink)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	h.mu.Lock()
	h.sinks[c] = sink
	h.mu.Unlock()
	return c
}

// drainReady reads the `ready` frame connect's registration put on the wire, so a test that cares
// about the next frame starts from an empty sink.
func (h *harness) drainReady(t *testing.T, c *conn) {
	t.Helper()
	if in := h.waitFrame(t, c.deviceID); in.Op != OpReady {
		t.Fatalf("first frame op %d, want ready", in.Op)
	}
}

// stall and release drive the sink's own mutex-guarded gate. The gate is NEVER a channel field
// reassigned from the test goroutine: the writer goroutine reads it on every write, and
// `go test -race -count=4` is this package's gate.
func (h *harness) stall(c *conn)   { h.sink(c).stall() }
func (h *harness) release(c *conn) { h.sink(c).release() }

func (h *harness) sink(c *conn) *recordingSink {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sinks[c]
}

func (h *harness) suspend(c *conn)                                { h.gw.suspend(c) }
func (h *harness) setLeaves(group id.ID, leaves map[id.ID]uint32) { h.gw.SetGroupLeaves(group, leaves) }
func (h *harness) setBots(devices ...id.ID)                       { h.gw.SetBotDevices(devices) }

func (h *harness) waitRawFrame(t *testing.T, device id.ID) []byte {
	t.Helper()
	h.mu.Lock()
	var sink *recordingSink
	for c, s := range h.sinks {
		if c.deviceID == device {
			sink = s
			break
		}
	}
	h.mu.Unlock()
	if sink == nil {
		t.Fatalf("device %s has no connection", device)
	}
	select {
	case b := <-sink.frames:
		return b
	case <-time.After(2 * time.Second):
		t.Fatalf("no frame reached device %s", device)
		return nil
	}
}

func (h *harness) waitFrame(t *testing.T, device id.ID) Inbound {
	t.Helper()
	in, err := decodeAny(h.waitRawFrame(t, device))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return in
}

func (h *harness) waitClose(t *testing.T, c *conn) CloseCode {
	t.Helper()
	select {
	case code := <-h.sink(c).closed:
		return code
	case <-time.After(2 * time.Second):
		t.Fatal("no close")
		return 0
	}
}

// readFrom waits for one frame on a sink the harness did not register (serve's own sink).
func (h *harness) readFrom(t *testing.T, s *recordingSink) Inbound {
	t.Helper()
	select {
	case b := <-s.frames:
		in, err := decodeAny(b)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		return in
	case <-time.After(2 * time.Second):
		t.Fatal("no frame")
		return Inbound{}
	}
}

func (h *harness) waitCloseOn(t *testing.T, s *recordingSink) CloseCode {
	t.Helper()
	select {
	case code := <-s.closed:
		return code
	case <-time.After(2 * time.Second):
		t.Fatal("no close")
		return 0
	}
}

func mustEncode(t *testing.T, f Frame, n uint64) []byte {
	t.Helper()
	b, err := Encode(f, n)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return b
}

// --- the two doubles the gateway needs -----------------------------------------------------------
// GroupsForDevice, GetCursor and CountKeyPackages are the only three store methods the gateway
// calls, so the harness satisfies gateway.Store (the narrow interface Options.Store holds) rather
// than store.Repository: a real SQLite repository here would test the storage layer, not the
// gateway.

func (h *harness) GroupsForDevice(_ context.Context, device id.ID) ([]id.ID, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.groups[device]), nil
}

func (h *harness) GetCursor(_ context.Context, _, _ id.ID) (store.CursorRow, error) {
	return store.CursorRow{LastSeq: 0, LastEpoch: 0}, nil
}

func (h *harness) CountKeyPackages(_ context.Context, _ id.ID, _ int64) (int64, error) {
	return 32, nil
}

// Resolve answers a token connect minted with that device's own session, and any other non-empty
// token with a fresh enrolled session — which is what the identify-level tests need; auth's own
// refusals are internal/auth's tests, not the gateway's.
func (h *harness) Resolve(_ context.Context, bearer string) (auth.Session, error) {
	if bearer == "" {
		return auth.Session{}, errors.New("no token")
	}
	h.mu.Lock()
	session, ok := h.tokens[bearer]
	h.mu.Unlock()
	if ok {
		return session, nil
	}
	return auth.Session{UserID: id.New(), DeviceID: id.New(), Scope: auth.ScopeEnrolled}, nil
}

// session is the enrolled session of one device, the shape register takes.
func session(device id.ID) auth.Session {
	return auth.Session{DeviceID: device, UserID: id.New(), Scope: auth.ScopeEnrolled}
}

// failingStore is the Store that answers every call with an error, for the registration paths
// that have to roll back cleanly.
type failingStore struct{}

var errStore = errors.New("store is down")

func (failingStore) GroupsForDevice(context.Context, id.ID) ([]id.ID, error) { return nil, errStore }

func (failingStore) GetCursor(context.Context, id.ID, id.ID) (store.CursorRow, error) {
	return store.CursorRow{}, errStore
}

func (failingStore) CountKeyPackages(context.Context, id.ID, int64) (int64, error) {
	return 0, errStore
}

// revoke is what auth.Sessions.OnRevoke does in production: the sessions are gone, so the token
// stops resolving and the gateway is told to close the device.
func (h *harness) revoke(device id.ID) {
	h.mu.Lock()
	delete(h.tokens, h.tokenFor(device))
	h.mu.Unlock()
	h.gw.CloseDevice(device, CloseSessionRevoked, "session revoked")
}
