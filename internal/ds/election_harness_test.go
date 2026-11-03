package ds_test

// election_harness_test.go is task 22's half of the delivery service's harness: the fixture group
// seen as a handful of driveable member devices, those devices on real gateway connections with
// every frame recorded, and the one seam invariant 7's deadline needs — a socket whose write can
// be parked so that the next frame really does wait in the writer's queue.
//
// It is a separate file rather than more of harness_test.go, which is already the longest file in
// the package; `dsHarness` is one type and its three new fields are declared with the rest, there.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/store"
)

// dsGroup is the registered fixture group plus the handful of its member devices a test drives.
// `members` and `leaves` are parallel: members[i] sits at leaf leaves[i] in the MLS tree, which is
// the leaf `mls_members` records and the one d.leafOf reads.
type dsGroup struct {
	id      id.ID
	members []id.ID
	leaves  []uint32

	// h is the harness that built the group, so a dsGroup can answer the three questions a test
	// asks of it directly — `sessionOf`, `leafOf`, `epoch` (fork_test.go) — without every call
	// site threading the harness back in.
	h *dsHarness
}

// groupWithMembers registers the committed 1,500-leaf fixture and exposes its first n member
// devices, ordered by leaf index. The devices are the ones the guest's own credentials name, read
// back out of `mls_members`, so `d.leafOf` and `ProposeRemove` see the identities the test does.
//
// It also issues ONE seed instance Remove, of the fixture's LAST leaf — a leaf no test names and
// no exposed member occupies. Invariant 7's election is defined over outstanding work:
// RequestCommit on a proposal-free group elects nobody by design, so a test that calls it directly
// (TestTheWatchdogNudgesTheNextCandidate) would otherwise be asserting against an election the
// delivery service correctly declined to hold. The seed is what election_test.go's header comment
// means by "every test in this file issues an instance proposal FIRST".
func (h *dsHarness) groupWithMembers(t *testing.T, n int) *dsGroup {
	t.Helper()
	ctx := context.Background()
	got, _ := h.mustRegister(t)

	members, err := h.repo.ListMembers(ctx, got.GroupID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	if len(members) <= n {
		t.Fatalf("the fixture has %d members, want more than %d", len(members), n)
	}
	slices.SortFunc(members, func(a, b store.MemberRow) int {
		return int(a.LeafIndex) - int(b.LeafIndex)
	})

	g := &dsGroup{id: got.GroupID, h: h}
	if h.sessions == nil {
		h.sessions = map[id.ID]auth.Session{}
	}
	for _, m := range members[:n] {
		g.members = append(g.members, m.DeviceID)
		g.leaves = append(g.leaves, m.LeafIndex)
		h.sessions[m.DeviceID] = auth.Session{
			UserID: m.UserID, DeviceID: m.DeviceID, Scope: auth.ScopeEnrolled,
		}
	}

	seed := members[len(members)-1].LeafIndex
	if err := h.ds.ProposeRemove(ctx, g.id, seed, id.New()); err != nil {
		t.Fatalf("the seed instance Remove of leaf %d: %v", seed, err)
	}
	return g
}

// proposeRemoveOf issues one instance Remove of a leaf. storeInstanceProposal ends in
// RequestCommit, so this is also what arms the election under test.
func (h *dsHarness) proposeRemoveOf(t *testing.T, g *dsGroup, leaf uint32) {
	t.Helper()
	if err := h.ds.ProposeRemove(context.Background(), g.id, leaf, id.New()); err != nil {
		t.Fatalf("ProposeRemove(leaf %d): %v", leaf, err)
	}
}

// setLeaves rewrites the leaf index the GATEWAY orders the election by, member index -> leaf. It
// is the registry's view, not the MLS tree's: invariant 7 orders candidates by the leaf the
// delivery service last published with SetGroupLeaves, and a test that wants a particular ordering
// says so rather than hunting for a fixture whose leaves happen to fall that way.
func (h *dsHarness) setLeaves(g *dsGroup, leaves map[int]uint32) {
	out := map[id.ID]uint32{}
	for i, leaf := range leaves {
		out[g.members[i]] = leaf
	}
	h.gw.SetGroupLeaves(g.id, out)
}

// markBot marks a device a bot, which puts it first in every election whatever its leaf.
func (h *dsHarness) markBot(device id.ID) { h.gw.SetBotDevices([]id.ID{device}) }

// policy is the Policy the harness built its delivery service with.
func (h *dsHarness) policy() ds.Policy { return ds.DefaultPolicy() }

// helloBackoff puts one member device online and returns the two back-off elements of the hello
// it was greeted with: backoff_ms and backoff_jitter_ms, elements 7 and 8 (deviation B23).
func (h *dsHarness) helloBackoff(t *testing.T) (backoff, jitter uint64) {
	t.Helper()
	g := h.groupWithMembers(t, 1)
	h.online(g.members[0])
	if len(h.hello.payload) != 9 {
		t.Fatalf("hello has %d elements, want 9", len(h.hello.payload))
	}
	if err := cborx.Unmarshal(h.hello.payload[7], &backoff); err != nil {
		t.Fatalf("hello backoff_ms: %v", err)
	}
	if err := cborx.Unmarshal(h.hello.payload[8], &jitter); err != nil {
		t.Fatalf("hello backoff_jitter_ms: %v", err)
	}
	return backoff, jitter
}

// armedElections is how many groups hold a live election.
func (h *dsHarness) armedElections() int { return ds.ArmedElectionsForTest(h.ds) }

// removedLeaves counts the outstanding instance Removes that target one of the group's OWN exposed
// members — which is what "the watchdog removed the device" looks like in SQL. Removes of any
// other leaf (the seed, and whatever a test proposed itself) are not counted.
func (h *dsHarness) removedLeaves(t *testing.T, g *dsGroup) int {
	t.Helper()
	rows, err := h.ds.Outstanding(context.Background(), g.id)
	if err != nil {
		t.Fatalf("Outstanding: %v", err)
	}
	n := 0
	for _, r := range rows {
		if r.Origin != 0 || r.VoidAt != nil || r.Kind != uint8(mlswasi.ProposalRemove) {
			continue
		}
		if r.TargetLeaf != nil && slices.Contains(g.leaves, *r.TargetLeaf) {
			n++
		}
	}
	return n
}

// advanceGroupEpoch moves the group one epoch on in SQL, which is what an ACCEPTED commit leaves
// behind: `persistState` writes the new epoch through the same UPDATE this uses, and the applied
// proposals are deleted in the same transaction.
//
// It is a direct write because no commit in this package can be accepted — the committed fixture
// ships one GroupInfo, at epoch 6, and invariant 4 wants epoch n+1, the blocker that moved
// `TestAnAcceptedCommitFansOutHandshakeEpochChangedAndWelcomes` to internal/testkit/accepted_test.go.
// The state blob is
// re-put unchanged: every path under test here (RequestCommit, RunWatchdogOnce) reads the group
// row and the proposal rows and never the cached PublicGroup.
func (h *dsHarness) advanceGroupEpoch(t *testing.T, groupID id.ID) uint64 {
	t.Helper()
	ctx := context.Background()
	row, err := h.repo.GetGroup(ctx, groupID)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	epoch := row.Epoch + 1
	if err := h.repo.PutGroupState(ctx, groupID, epoch,
		row.PublicGroupState, row.GroupInfoBlob, row.TreeHash); err != nil {
		t.Fatalf("PutGroupState: %v", err)
	}
	return epoch
}

// expectExactlyOneCommitNeeded asserts that invariant 7 elected ONE device: `elected` received
// mls.commit_needed, no other device did, and no second one followed on the elected device's own
// connection either.
func (h *dsHarness) expectExactlyOneCommitNeeded(t *testing.T, elected id.ID, others ...id.ID) {
	t.Helper()
	h.waitDeviceFrame(t, elected, "mls.commit_needed")
	// One settle: every frame of the operation under test was written from the same call, so a
	// second election's frame is already on the wire by now.
	time.Sleep(150 * time.Millisecond)
	c := h.connOf(t, elected)
	for {
		select {
		case f := <-c.frames:
			if f.name == "mls.commit_needed" {
				t.Fatalf("device %s was elected twice: a second mls.commit_needed arrived",
					elected.String()[:8])
			}
		default:
			h.expectNoFrames(t, others...)
			return
		}
	}
}

// ------------------------------------------------------------------ devices on the wire

// deviceConn is one device's live gateway connection: a real WebSocket over a real HTTP server,
// with two additions the election tests need. Every frame the instance sends is recorded as it
// arrives, and the SERVER SIDE of the socket can be parked inside Write — which is the only way to
// make a frame wait in the writer's queue, and so the only way to observe D11's re-based
// deadline_ms.
type deviceConn struct {
	deviceID id.ID
	ws       *websocket.Conn
	stall    *stallConn
	frames   chan recordedFrame
	closed   chan struct{}

	// url and bearer are what a RECONNECT needs: task 27's restore test drops this socket and
	// dials the same gateway again with the resume token below, which is the only way to observe
	// from outside the package that the new generation refused it.
	url    string
	bearer string
	// ready is the `ready` frame this connection was greeted with. Its elements 2 and 3 are the
	// generation and the resume token — the two values a `resume` frame must carry back.
	ready recordedFrame
}

// recordedFrame is one decoded S->C frame: [op, n, group_id, payload].
type recordedFrame struct {
	op      gateway.Op
	name    string
	payload []cbor.RawMessage
}

// dsFrameNames names the frames these tests wait for. `presence` is spelled "filler" because that
// is the only thing sendFiller uses it for — a well-formed frame that reaches the writer first and
// blocks it there. No test in this package asserts a real presence frame.
var dsFrameNames = map[gateway.Op]string{
	gateway.OpHello:           "hello",
	gateway.OpReady:           "ready",
	gateway.OpMLSHandshake:    "mls.handshake",
	gateway.OpMLSCommitNeeded: "mls.commit_needed",
	gateway.OpMLSEpochChanged: "mls.epoch_changed",
	gateway.OpPresence:        "filler",
	// Task 23's fan-out: R30 sends message.ct to every online member, the uploader included.
	gateway.OpMessageCT:      "message.ct",
	gateway.OpMessageDeleted: "message.deleted",
	// Task 24's addressed fan-out: one mls.welcome per joiner of a commit.
	gateway.OpMLSWelcome: "mls.welcome",
}

func dsFrameName(op gateway.Op) string {
	if name, ok := dsFrameNames[op]; ok {
		return name
	}
	return fmt.Sprintf("op %d", op)
}

// online puts each device on its own gateway connection and starts recording what it receives. A
// device already online is left alone, so a test may call it twice.
func (h *dsHarness) online(devices ...id.ID) {
	h.t.Helper()
	for _, device := range devices {
		if h.conns[device] != nil {
			continue
		}
		h.connect(device)
	}
}

// offline makes sure none of these devices holds a connection: it closes the ones the harness
// opened and waits until the gateway agrees. A device that was never online is already offline,
// which is what the nobody-online tests want to say out loud.
func (h *dsHarness) offline(devices ...id.ID) {
	h.t.Helper()
	for _, device := range devices {
		if c := h.conns[device]; c != nil {
			_ = c.ws.CloseNow()
			delete(h.conns, device)
		}
		deadline := time.Now().Add(15 * time.Second)
		for h.gw.Online(device) {
			if time.Now().After(deadline) {
				h.t.Fatalf("device %s is still online", device.String()[:8])
			}
			time.Sleep(time.Millisecond)
		}
	}
}

// connect dials one device in: hello, identify, ready, then a reader goroutine that records every
// later frame. The three handshake frames are read synchronously, because a connection that is not
// yet ready is not online and every invariant here is defined over the online predicate.
func (h *dsHarness) connect(device id.ID) *deviceConn {
	t := h.t
	t.Helper()
	session, ok := h.sessions[device]
	if !ok {
		t.Fatalf("no session for device %s; it is not a member the harness exposed",
			device.String()[:8])
	}
	h.auth.add(session)

	// NewUnstartedServer, so the accepted connection can be wrapped before it is served: stallConn
	// is what stallWriter parks the instance's writer inside.
	srv := httptest.NewUnstartedServer(h.gw.Handler())
	listener := &stallListener{Listener: srv.Listener}
	srv.Listener = listener
	srv.Start()
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	dialCtx, cancelDial := context.WithTimeout(ctx, 30*time.Second)
	defer cancelDial()

	ws, _, err := websocket.Dial(dialCtx, "ws"+strings.TrimPrefix(srv.URL, "http"), //nolint:bodyclose // websocket.Dial documents that the handshake response body never needs closing
		&websocket.DialOptions{
			HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + h.auth.token(session)}},
			Subprotocols: []string{"dilla.v1"},
		})
	if err != nil {
		t.Fatalf("dial the gateway: %v", err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })

	c := &deviceConn{
		deviceID: device,
		ws:       ws,
		stall:    listener.accepted(t),
		frames:   make(chan recordedFrame, 256),
		closed:   make(chan struct{}),
		url:      srv.URL,
		bearer:   h.auth.token(session),
	}
	hello := readRecordedFrame(t, dialCtx, ws)
	if hello.op != gateway.OpHello {
		t.Fatalf("the first frame is %s, want hello", hello.name)
	}
	h.hello = hello

	identify, err := cborx.Marshal([]any{"", uint64(1), uint64(1), uint64(1), uint64(0)})
	if err != nil {
		t.Fatalf("encode identify: %v", err)
	}
	frame, err := gateway.Encode(
		gateway.Frame{Op: gateway.OpIdentify, Payload: cborx.Raw(identify)}, 1)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if err := ws.Write(dialCtx, websocket.MessageBinary, frame); err != nil {
		t.Fatalf("write identify: %v", err)
	}
	ready := readRecordedFrame(t, dialCtx, ws)
	if ready.op != gateway.OpReady {
		t.Fatalf("the second frame is %s, want ready", ready.name)
	}
	c.ready = ready
	if !h.gw.Online(device) {
		t.Fatal("a device that reached ready is not online")
	}

	go func() {
		defer close(c.closed)
		for {
			typ, b, err := ws.Read(ctx)
			if err != nil || typ != websocket.MessageBinary {
				return
			}
			f, err := decodeRecordedFrame(b)
			if err != nil {
				return
			}
			select {
			case c.frames <- f:
			default: // a full buffer is a harness bug, not an assertion; the wait will time out
			}
		}
	}()

	if h.conns == nil {
		h.conns = map[id.ID]*deviceConn{}
	}
	h.conns[device] = c
	return c
}

func (h *dsHarness) connOf(t *testing.T, device id.ID) *deviceConn {
	t.Helper()
	c := h.conns[device]
	if c == nil {
		t.Fatalf("device %s holds no connection", device.String()[:8])
	}
	return c
}

// expectDeviceFrames waits for each named frame on the device's connection, in order, skipping
// frames it was not asked about — mls.handshake reaches every online member of the group by design
// (R30), so a strict "the next frame is" assertion would be asserting the fan-out, not the
// election.
func (h *dsHarness) expectDeviceFrames(t *testing.T, device id.ID, names ...string) {
	t.Helper()
	for _, name := range names {
		h.waitDeviceFrame(t, device, name)
	}
}

// expectNoFrames asserts that none of these devices was elected: no mls.commit_needed reached
// them. It is deliberately about that one frame. Every online member of the group receives the
// mls.handshake the proposal fans out, so "no frames at all" is not a property invariant 7 has.
func (h *dsHarness) expectNoFrames(t *testing.T, devices ...id.ID) {
	t.Helper()
	// One settle: the elected device's frame has already arrived by the time this runs, and the
	// instance writes to every one of these connections from the same call, so a frame on its way
	// to a device that should not have been elected is already on the wire.
	time.Sleep(100 * time.Millisecond)
	for _, device := range devices {
		c := h.connOf(t, device)
		for {
			select {
			case f := <-c.frames:
				if f.name == "mls.commit_needed" {
					t.Fatalf("device %s was elected too: it received mls.commit_needed",
						device.String()[:8])
				}
			default:
				return
			}
		}
	}
}

// waitDeviceFrame consumes frames until one is named `name`, and returns it.
func (h *dsHarness) waitDeviceFrame(t *testing.T, device id.ID, name string) recordedFrame {
	t.Helper()
	c := h.connOf(t, device)
	deadline := time.After(20 * time.Second)
	for {
		select {
		case f := <-c.frames:
			if f.name == name {
				return f
			}
		case <-c.closed:
			t.Fatalf("the connection of device %s closed before %s arrived",
				device.String()[:8], name)
		case <-deadline:
			t.Fatalf("device %s never received %s", device.String()[:8], name)
		}
	}
}

// deadlineMSOf is element 2 of an mls.commit_needed payload: [epoch, refs, deadline_ms, round].
func (h *dsHarness) deadlineMSOf(t *testing.T, f recordedFrame) uint64 {
	t.Helper()
	if len(f.payload) != 4 {
		t.Fatalf("the %s payload has %d elements, want 4", f.name, len(f.payload))
	}
	var deadline uint64
	if err := cborx.Unmarshal(f.payload[2], &deadline); err != nil {
		t.Fatalf("decode deadline_ms: %v", err)
	}
	return deadline
}

// sendFiller puts one well-formed frame on the device's connection. Its only job is to be the
// frame the writer is inside sink.write for while the next one waits in the queue.
func (h *dsHarness) sendFiller(t *testing.T, device id.ID) {
	t.Helper()
	session := h.sessions[device]
	payload, err := gateway.PresencePayload(session.UserID, 1, uint64(h.clk.Now().Unix()), "")
	if err != nil {
		t.Fatalf("PresencePayload: %v", err)
	}
	h.gw.DeliverDevice(device, gateway.Frame{Op: gateway.OpPresence, Payload: payload})
}

// stallWriter parks the next write to this device's socket inside net.Conn.Write, where the
// instance's writer goroutine sits until releaseWriter lets it go. Everything enqueued meanwhile
// waits in the writer's queue, which is the state D11's re-base is defined over.
func (h *dsHarness) stallWriter(device id.ID) { h.connOf(h.t, device).stall.stall() }

func (h *dsHarness) releaseWriter(device id.ID) { h.connOf(h.t, device).stall.release() }

// waitWriterBlocked waits until the instance's writer is actually inside that stalled write.
// Without it the clock could advance before the filler frame ever reached the socket, and the
// commit_needed frame would never have waited in the queue at all.
func (h *dsHarness) waitWriterBlocked(t *testing.T, device id.ID) {
	t.Helper()
	c := h.connOf(t, device)
	deadline := time.Now().Add(20 * time.Second)
	for !c.stall.blocked() {
		if time.Now().After(deadline) {
			t.Fatalf("the writer of device %s never blocked", device.String()[:8])
		}
		time.Sleep(time.Millisecond)
	}
}

func readRecordedFrame(t *testing.T, ctx context.Context, ws *websocket.Conn) recordedFrame {
	t.Helper()
	typ, b, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != websocket.MessageBinary {
		t.Fatalf("message type %v, want binary: dilla's wire is CBOR, never text", typ)
	}
	f, err := decodeRecordedFrame(b)
	if err != nil {
		t.Fatalf("decode the frame: %v", err)
	}
	return f
}

func decodeRecordedFrame(b []byte) (recordedFrame, error) {
	var frame []cbor.RawMessage
	if err := cborx.Unmarshal(b, &frame); err != nil {
		return recordedFrame{}, err
	}
	if len(frame) != 4 {
		return recordedFrame{}, fmt.Errorf("the frame has %d elements, want 4", len(frame))
	}
	var op uint64
	if err := cborx.Unmarshal(frame[0], &op); err != nil {
		return recordedFrame{}, err
	}
	var payload []cbor.RawMessage
	if err := cborx.Unmarshal(frame[3], &payload); err != nil {
		return recordedFrame{}, err
	}
	return recordedFrame{
		op: gateway.Op(op), name: dsFrameName(gateway.Op(op)), payload: payload,
	}, nil
}

// stallListener hands the harness the server side of the one connection its test server accepts,
// wrapped so that writes to it can be parked on demand.
type stallListener struct {
	net.Listener

	mu   sync.Mutex
	conn *stallConn
}

func (l *stallListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	wrapped := &stallConn{Conn: c}
	l.mu.Lock()
	l.conn = wrapped
	l.mu.Unlock()
	return wrapped, nil
}

func (l *stallListener) accepted(t *testing.T) *stallConn {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		l.mu.Lock()
		c := l.conn
		l.mu.Unlock()
		if c != nil {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatal("the test server accepted no connection")
		}
		time.Sleep(time.Millisecond)
	}
}

// stallConn is the server side of one socket, with a gate in front of Write. It is not a double:
// every byte still goes to the real connection, and the gate is open unless a test closed it.
type stallConn struct {
	net.Conn

	mu       sync.Mutex
	gate     chan struct{}
	inflight int
}

func (c *stallConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	gate := c.gate
	if gate != nil {
		c.inflight++
	}
	c.mu.Unlock()
	if gate != nil {
		<-gate
		c.mu.Lock()
		c.inflight--
		c.mu.Unlock()
	}
	return c.Conn.Write(b)
}

func (c *stallConn) stall() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gate == nil {
		c.gate = make(chan struct{})
	}
}

func (c *stallConn) release() {
	c.mu.Lock()
	gate := c.gate
	c.gate = nil
	c.mu.Unlock()
	if gate != nil {
		close(gate)
	}
}

func (c *stallConn) blocked() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inflight > 0
}
