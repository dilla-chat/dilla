package api_test

// web-3 task 3b: the server-side ring of a DM or group-DM call (L-GW-33, op 53 call_ring), its
// decline route (L-HTTP-104) and the (caller, recipient) pair rate rule, on the fake clock.

import (
	"bytes"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// ringFrame is one op 53 a device received, decoded: [call_id, channel_id, user_id, state, expires_ts].
type ringFrame struct {
	Call, Channel, User id.ID
	State, Expires      uint64
}

// deviceFrame is one frame one device received.
type deviceFrame struct {
	Op   gateway.Op
	Ring ringFrame
}

// ringGW is the gateway the ring and the call events deliver through: DeliverUser fans a frame out to
// every device registered for the user, as the real gateway does to the user's live connections.
type ringGW struct {
	mu      sync.Mutex
	devices map[id.ID][]id.ID // user → its online devices
	frames  map[id.ID][]deviceFrame
}

func (g *ringGW) DeliverUser(user id.ID, f gateway.Frame) {
	df := deviceFrame{Op: f.Op}
	if f.Op == gateway.OpCallRing {
		var raw []cbor.RawMessage
		if err := cborx.Unmarshal(f.Payload, &raw); err == nil && len(raw) == 5 {
			_ = cborx.Unmarshal(raw[0], &df.Ring.Call)
			_ = cborx.Unmarshal(raw[1], &df.Ring.Channel)
			_ = cborx.Unmarshal(raw[2], &df.Ring.User)
			_ = cborx.Unmarshal(raw[3], &df.Ring.State)
			_ = cborx.Unmarshal(raw[4], &df.Ring.Expires)
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, dev := range g.devices[user] {
		g.frames[dev] = append(g.frames[dev], df)
	}
}

// rings is every op 53 dev received, in order.
func (g *ringGW) rings(dev id.ID) []ringFrame {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []ringFrame
	for _, f := range g.frames[dev] {
		if f.Op == gateway.OpCallRing {
			out = append(out, f.Ring)
		}
	}
	return out
}

// ringUser is one account with its bearer token (its first device) and every device it has online.
type ringUser struct {
	user    id.ID
	tok     string
	devices []id.ID
}

// dmCallHost is the call routes, the call events and the ring over one env, with DMs.
type dmCallHost struct {
	e      *env
	stub   *stubSFU
	calls  *api.Calls
	events *api.CallEvents
	gw     *ringGW
	alice  ringUser
	bob    ringUser
	carol  ringUser
	dm     id.ID // the 1:1 DM between alice and bob
	room   string
}

func (h *dmCallHost) newUser(t *testing.T, name string, devices int) ringUser {
	t.Helper()
	u, tok := h.e.NewUser(name)
	ru := ringUser{user: u, tok: tok, devices: []id.ID{deviceOf(t, h.e, tok)}}
	for range devices - 1 {
		dev := id.New()
		if err := h.e.Repo.CreateDevice(t.Context(), newAPITestDevice(dev, u, h.e.Clk.Now().Unix())); err != nil {
			t.Fatalf("CreateDevice: %v", err)
		}
		ru.devices = append(ru.devices, dev)
	}
	h.gw.mu.Lock()
	h.gw.devices[u] = ru.devices
	h.gw.mu.Unlock()
	return ru
}

// newDMCallHost is a 1:1 DM between alice and bob, where bob has two online devices; carol is a
// third account of the instance with no part in it.
func newDMCallHost(t *testing.T) *dmCallHost {
	t.Helper()
	e, _ := dmEnv(t)
	h := &dmCallHost{e: e, stub: &stubSFU{}, gw: &ringGW{devices: map[id.ID][]id.ID{}, frames: map[id.ID][]deviceFrame{}}}
	log := slog.New(slog.DiscardHandler)
	h.calls = api.NewCalls(e.Repo, api.NewResolver(e.Repo), h.stub, api.CallsConfig{LiveKitURL: testLiveKitURL,
		MaxVoiceParticipants: 10}, e.Clk, log).WithRinger(api.NewRinger(h.gw, e.Clk, log).WithMaxUsers(10))
	h.calls.Register(e.Mux)
	h.events = api.NewCallEvents(e.Repo, api.NewResolver(e.Repo), &callDS{repo: e.Repo}, h.stub, h.calls, h.gw, e.Clk, log)
	h.alice = h.newUser(t, "alice", 1)
	h.bob = h.newUser(t, "bob", 2)
	h.carol = h.newUser(t, "carol", 1)
	h.dm = h.openDMWith(t, h.alice, h.bob)
	return h
}

// openDMWith opens a DM (or a group DM) from a with the others and registers its call group, every
// device of every participant a leaf of it.
func (h *dmCallHost) openDMWith(t *testing.T, a ringUser, others ...ringUser) id.ID {
	t.Helper()
	ids := make([]id.ID, 0, len(others))
	for _, o := range others {
		ids = append(ids, o.user)
	}
	dm, status := openDM(t, h.e, a.tok, ids...)
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("open DM = %d", status)
	}
	h.register(t, dm, append([]ringUser{a}, others...)...)
	return dm
}

// register writes a fresh open call group for dm, every device of users a leaf of its current
// epoch: what the participants' clients do after an end closed the previous one.
func (h *dmCallHost) register(t *testing.T, dm id.ID, users ...ringUser) {
	t.Helper()
	call := dm
	g := store.GroupRow{
		GroupID: id.New(), Binding: []byte{0x80}, Kind: api.GroupCall, TargetID: dm, CallID: &call,
		Ciphersuite: 1, Epoch: callGroupEpoch, ExternalSenderKeyID: id.New(), E2EEVersion: 1,
		MediaVersion: 1, PolicyVersion: 1, Created: h.e.Clk.Now().Unix(),
	}
	if err := h.e.Repo.CreateGroup(t.Context(), g); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	for _, u := range users {
		for _, dev := range u.devices {
			seedLeaf(t, h.e, g.GroupID, dev, 3, nil)
		}
	}
}

// startIn is POST /v1/channels/{ch}/calls as u; it asserts want and answers the call id.
func (h *dmCallHost) startIn(t *testing.T, ch id.ID, u ringUser, want int) id.ID {
	t.Helper()
	status, body := h.e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", u.tok, []any{})
	if status != want {
		t.Fatalf("start in %s = %d %x, want %d", ch, status, body, want)
	}
	h.room = h.stub.lastMint().Room
	return decodeCall(t, body).CallID
}

func (h *dmCallHost) startCall(t *testing.T, u ringUser) id.ID {
	t.Helper()
	return h.startIn(t, h.dm, u, http.StatusCreated)
}

// end is DELETE /v1/calls/{call} as u.
func (h *dmCallHost) end(t *testing.T, call id.ID, u ringUser) {
	t.Helper()
	if status, body := h.e.Do(http.MethodDelete, "/v1/calls/"+call.String(), u.tok, nil); status != http.StatusNoContent {
		t.Fatalf("end = %d %x", status, body)
	}
}

// joined is the SFU's participant_joined for dev in the live room.
func (h *dmCallHost) joined(t *testing.T, dev id.ID) {
	t.Helper()
	h.events.Handle(t.Context(), &livekit.WebhookEvent{Event: webhook.EventParticipantJoined, Id: "EV_" + id.New().String(),
		Room: &livekit.Room{Name: h.room}, Participant: &livekit.ParticipantInfo{Identity: dev.String()}})
	h.events.FlushAnnouncements()
}

// decline is POST /v1/calls/{call}/decline [expires] as u.
func (h *dmCallHost) decline(t *testing.T, call id.ID, u ringUser, expires uint64) (int, []byte) {
	t.Helper()
	return h.e.Do(http.MethodPost, "/v1/calls/"+call.String()+"/decline", u.tok, []any{expires})
}

// lastFrame is the last op 53 dev received.
func (h *dmCallHost) lastFrame(t *testing.T, dev id.ID) ringFrame {
	t.Helper()
	rings := h.gw.rings(dev)
	if len(rings) == 0 {
		t.Fatalf("device %s received no call_ring", dev)
	}
	return rings[len(rings)-1]
}

func (h *dmCallHost) requireNoFrame(t *testing.T, dev id.ID) {
	t.Helper()
	if rings := h.gw.rings(dev); len(rings) != 0 {
		t.Fatalf("device %s received call_ring %+v, want none", dev, rings)
	}
}

// waitState waits for dev's last op 53 to be state (the timeout fires on the ring's own goroutine).
func (h *dmCallHost) waitState(t *testing.T, dev id.ID, state uint64) ringFrame {
	t.Helper()
	var last ringFrame
	if !waitFor(5*time.Second, func() bool {
		rings := h.gw.rings(dev)
		if len(rings) == 0 {
			return false
		}
		last = rings[len(rings)-1]
		return last.State == state
	}) {
		t.Fatalf("device %s: last call_ring %+v, want state %d", dev, last, state)
	}
	return last
}

// counts is how many op 53 every device of users received.
func (h *dmCallHost) counts(users ...ringUser) map[id.ID]int {
	out := map[id.ID]int{}
	for _, u := range users {
		for _, dev := range u.devices {
			out[dev] = len(h.gw.rings(dev))
		}
	}
	return out
}

func (h *dmCallHost) everyone() []ringUser { return []ringUser{h.alice, h.bob, h.carol} }

func requireRing(t *testing.T, f ringFrame, call, channel, user id.ID, state uint64, expires int64) {
	t.Helper()
	if f.Call != call || f.Channel != channel || f.User != user || f.State != state || f.Expires != uint64(expires) {
		t.Fatalf("call_ring = %+v, want [%s %s %s %d %d]", f, call, channel, user, state, expires)
	}
}

func TestOpeningADMCallRingsEveryOtherParticipantDevice(t *testing.T) {
	h := newDMCallHost(t)
	call := h.startCall(t, h.alice)
	for _, dev := range h.bob.devices {
		requireRing(t, h.lastFrame(t, dev), call, h.dm, h.alice.user, 1, h.e.Clk.Now().Unix()+30)
	}
	h.requireNoFrame(t, h.alice.devices[0])
	h.requireNoFrame(t, h.carol.devices[0])
}

func TestASecondStartOfALiveCallNeverRings(t *testing.T) {
	h := newDMCallHost(t)
	h.startCall(t, h.alice)
	// bob joined the call group with his clients (register made every device a leaf); his start of
	// the live call is a 200 and rings no one, the caller alice included.
	before := h.counts(h.everyone()...)
	h.startIn(t, h.dm, h.bob, http.StatusOK)
	if after := h.counts(h.everyone()...); !mapsEqual(before, after) {
		t.Fatalf("op 53 counts before a 200 start %v, after %v: a live call's start rang", before, after)
	}
}

func mapsEqual(a, b map[id.ID]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestRingStopsOnEveryDeviceWhenOneJoins(t *testing.T) {
	h := newDMCallHost(t)
	call := h.startCall(t, h.alice)
	expires := h.lastFrame(t, h.bob.devices[0]).Expires
	h.joined(t, h.bob.devices[0])
	for _, dev := range h.bob.devices {
		requireRing(t, h.lastFrame(t, dev), call, h.dm, h.alice.user, 0, int64(expires))
	}
	h.requireNoFrame(t, h.alice.devices[0])
}

func TestRingTimesOutAfter30Seconds(t *testing.T) {
	h := newDMCallHost(t)
	call := h.startCall(t, h.alice)
	expires := h.lastFrame(t, h.bob.devices[0]).Expires
	h.e.Clk.Advance(29 * time.Second)
	time.Sleep(20 * time.Millisecond) // a wrongly early timeout would have delivered by now
	for _, dev := range h.bob.devices {
		if f := h.lastFrame(t, dev); f.State != 1 {
			t.Fatalf("after 29 s device %s got %+v, want still ringing", dev, f)
		}
	}
	h.e.Clk.Advance(time.Second)
	for _, dev := range h.bob.devices {
		f := h.waitState(t, dev, 0)
		requireRing(t, f, call, h.dm, h.alice.user, 0, int64(expires))
	}
	h.requireNoFrame(t, h.alice.devices[0])
}

func TestRingStopsWhenTheCallEnds(t *testing.T) {
	h := newDMCallHost(t)
	call := h.startCall(t, h.alice)
	expires := h.lastFrame(t, h.bob.devices[0]).Expires
	h.end(t, call, h.alice)
	for _, dev := range h.bob.devices {
		requireRing(t, h.lastFrame(t, dev), call, h.dm, h.alice.user, 0, int64(expires))
	}
}

func TestDeclineStopsTheRingAndTellsTheCaller(t *testing.T) {
	h := newDMCallHost(t)
	call := h.startCall(t, h.alice)
	expires := h.lastFrame(t, h.bob.devices[0]).Expires
	if status, body := h.decline(t, call, h.bob, expires); status != http.StatusNoContent {
		t.Fatalf("decline = %d %x", status, body)
	}
	for _, dev := range h.bob.devices {
		requireRing(t, h.lastFrame(t, dev), call, h.dm, h.alice.user, 0, int64(expires))
	}
	requireRing(t, h.lastFrame(t, h.alice.devices[0]), call, h.dm, h.bob.user, 2, int64(expires))

	before := h.counts(h.everyone()...)
	if status, body := h.decline(t, call, h.bob, expires); status != http.StatusNoContent {
		t.Fatalf("a repeated decline = %d %x", status, body)
	}
	if after := h.counts(h.everyone()...); !mapsEqual(before, after) {
		t.Fatalf("a repeated decline sent frames: %v → %v", before, after)
	}
}

// Attacker: bob's late decline of an earlier ring of the same DM (the call id is the channel id).
// It must not stop the next ring or tell its caller "declined".
func TestADeclineOfAnEarlierRingDoesNothing(t *testing.T) {
	h := newDMCallHost(t)
	call := h.startCall(t, h.alice)
	ring1 := h.lastFrame(t, h.bob.devices[0]).Expires
	h.e.Clk.Advance(30 * time.Second)
	h.waitState(t, h.bob.devices[0], 0)
	h.end(t, call, h.alice)
	h.register(t, h.dm, h.alice, h.bob)
	if again := h.startCall(t, h.alice); again != call {
		t.Fatalf("the restarted DM call has id %s, want the channel's %s", again, call)
	}
	ring2 := h.lastFrame(t, h.bob.devices[0])
	if ring2.State != 1 || ring2.Expires == ring1 {
		t.Fatalf("ring 2 = %+v, want a new ring (ring 1 expired at %d)", ring2, ring1)
	}

	before := h.counts(h.everyone()...)
	if status, body := h.decline(t, call, h.bob, ring1); status != http.StatusNoContent {
		t.Fatalf("a decline of ring 1 = %d %x, want 204", status, body)
	}
	if after := h.counts(h.everyone()...); !mapsEqual(before, after) {
		t.Fatalf("a decline of an earlier ring sent op 53: %v → %v", before, after)
	}
	// bob is still ringing: the decline that names ring 2 stops it and tells alice.
	if status, _ := h.decline(t, call, h.bob, ring2.Expires); status != http.StatusNoContent {
		t.Fatalf("a decline of ring 2 = %d", status)
	}
	requireRing(t, h.lastFrame(t, h.alice.devices[0]), call, h.dm, h.bob.user, 2, int64(ring2.Expires))
}

// Attacker: a non-participant who learnt a call id. They can neither stop a ring nor confirm the
// call exists.
func TestDeclineByANonParticipantIsNotFound(t *testing.T) {
	h := newDMCallHost(t)
	call := h.startCall(t, h.alice)
	expires := h.lastFrame(t, h.bob.devices[0]).Expires
	status, known := h.decline(t, call, h.carol, expires)
	if status != http.StatusNotFound || h.e.ErrCode(known) != "E_NOT_FOUND" {
		t.Fatalf("carol's decline = %d %x, want 404 E_NOT_FOUND", status, known)
	}
	status, unknown := h.decline(t, id.New(), h.carol, expires)
	if status != http.StatusNotFound || !bytes.Equal(known, unknown) {
		t.Fatalf("carol's decline answers %x, a random call %d %x: they must be identical", known, status, unknown)
	}
	for _, dev := range h.bob.devices {
		if f := h.lastFrame(t, dev); f.State != 1 {
			t.Fatalf("bob's device %s got %+v after carol's decline, want still ringing", dev, f)
		}
	}
	h.requireNoFrame(t, h.alice.devices[0])
}

// WB security m2: a decline on a deleted channel answers like an unknown call id, as the read routes
// do. Deleting a channel ends its calls today; this pins the route's own check in case that ever
// stops being synchronous (the channel row is marked deleted here without ending the call).
func TestDeclineOnADeletedChannelIsNotFound(t *testing.T) {
	h := newDMCallHost(t)
	call := h.startCall(t, h.alice)
	expires := h.lastFrame(t, h.bob.devices[0]).Expires
	if err := h.e.Repo.DeleteChannel(t.Context(), h.dm, h.e.Clk.Now().Unix()); err != nil {
		t.Fatalf("DeleteChannel: %v", err)
	}
	status, known := h.decline(t, call, h.bob, expires)
	if status != http.StatusNotFound || h.e.ErrCode(known) != "E_NOT_FOUND" {
		t.Fatalf("a decline on a deleted DM = %d %x, want 404 E_NOT_FOUND", status, known)
	}
	if _, unknown := h.decline(t, id.New(), h.bob, expires); !bytes.Equal(known, unknown) {
		t.Fatalf("a decline on a deleted DM answers %x, a random call %x: they must be identical", known, unknown)
	}
	h.requireNoFrame(t, h.alice.devices[0])

	// A community voice channel deleted under its call: 404 like an unknown id, not the 400 that
	// would confirm a call of a voice channel exists there.
	e, ch, tok, group, _, calls := callEnvCalls(t, api.CallsConfig{LiveKitURL: testLiveKitURL})
	calls.WithRinger(api.NewRinger(&ringGW{devices: map[id.ID][]id.ID{}, frames: map[id.ID][]deviceFrame{}}, e.Clk, slog.New(slog.DiscardHandler)))
	_, _, bobTok := joinedMember(t, e, ch, group, "bob")
	seedLeaf(t, e, group, deviceOf(t, e, tok), 3, nil)
	status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	if status != http.StatusCreated {
		t.Fatalf("start = %d %s", status, e.ErrCode(body))
	}
	voice := decodeCall(t, body).CallID
	if err := e.Repo.DeleteChannel(t.Context(), ch, e.Clk.Now().Unix()); err != nil {
		t.Fatalf("DeleteChannel: %v", err)
	}
	status, deleted := e.Do(http.MethodPost, "/v1/calls/"+voice.String()+"/decline", bobTok, []any{uint64(1)})
	_, random := e.Do(http.MethodPost, "/v1/calls/"+id.New().String()+"/decline", bobTok, []any{uint64(1)})
	if status != http.StatusNotFound || !bytes.Equal(deleted, random) {
		t.Fatalf("a decline in a deleted voice channel = %d %x, want the unknown id's answer %x", status, deleted, random)
	}
}

func TestACommunityVoiceCallNeverRings(t *testing.T) {
	e, ch, tok, group, _, calls := callEnvCalls(t, api.CallsConfig{LiveKitURL: testLiveKitURL})
	gw := &ringGW{devices: map[id.ID][]id.ID{}, frames: map[id.ID][]deviceFrame{}}
	calls.WithRinger(api.NewRinger(gw, e.Clk, slog.New(slog.DiscardHandler)))
	owner := userOf(t, e, tok)
	bob, bobDev, bobTok := joinedMember(t, e, ch, group, "bob")
	gw.devices[owner] = []id.ID{deviceOf(t, e, tok)}
	gw.devices[bob] = []id.ID{bobDev}
	seedLeaf(t, e, group, deviceOf(t, e, tok), 3, nil)
	status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	if status != http.StatusCreated {
		t.Fatalf("start = %d %s", status, e.ErrCode(body))
	}
	started := decodeCall(t, body)
	if started.HasRung {
		t.Fatalf("a community voice start carries rung %v; want the six elements", started.Rung)
	}
	call := started.CallID
	for _, dev := range []id.ID{deviceOf(t, e, tok), bobDev} {
		if rings := gw.rings(dev); len(rings) != 0 {
			t.Fatalf("a community voice call rang %s: %+v", dev, rings)
		}
	}
	status, body = e.Do(http.MethodPost, "/v1/calls/"+call.String()+"/decline", bobTok, []any{uint64(1)})
	if status != http.StatusBadRequest || e.ErrCode(body) != "E_INVALID_REQUEST" {
		t.Fatalf("a decline in a voice channel = %d %x, want 400 E_INVALID_REQUEST", status, body)
	}
}

func TestDeclineRefusesAMalformedBody(t *testing.T) {
	h := newDMCallHost(t)
	call := h.startCall(t, h.alice)
	for _, body := range []any{[]any{}, []any{"x"}, []any{uint64(1), uint64(2)}, map[string]any{}} {
		status, raw := h.e.Do(http.MethodPost, "/v1/calls/"+call.String()+"/decline", h.bob.tok, body)
		if status != http.StatusBadRequest || h.e.ErrCode(raw) != "E_INVALID_REQUEST" {
			t.Fatalf("decline body %v = %d %x, want 400", body, status, raw)
		}
	}
}

func TestGroupDMRingsEachOtherParticipantUntilTheyAnswer(t *testing.T) {
	h := newDMCallHost(t)
	gdm := h.openDMWith(t, h.alice, h.bob, h.carol)
	call := h.startIn(t, gdm, h.alice, http.StatusCreated)
	expires := h.e.Clk.Now().Unix() + 30
	for _, dev := range append(slices.Clone(h.bob.devices), h.carol.devices...) {
		requireRing(t, h.lastFrame(t, dev), call, gdm, h.alice.user, 1, expires)
	}
	h.joined(t, h.bob.devices[0])
	for _, dev := range h.bob.devices {
		requireRing(t, h.lastFrame(t, dev), call, gdm, h.alice.user, 0, expires)
	}
	if f := h.lastFrame(t, h.carol.devices[0]); f.State != 1 {
		t.Fatalf("carol's ring after bob answered = %+v, want still ringing", f)
	}
	h.e.Clk.Advance(30 * time.Second)
	requireRing(t, h.waitState(t, h.carol.devices[0], 0), call, gdm, h.alice.user, 0, expires)
}

// ringBurst starts and ends caller's call in dm n times, 15 s apart on the fake clock, and answers
// how many of the starts rang dev.
func (h *dmCallHost) ringBurst(t *testing.T, dm id.ID, caller ringUser, others []ringUser, dev id.ID, n int) int {
	t.Helper()
	rang := 0
	for range n {
		h.register(t, dm, append([]ringUser{caller}, others...)...)
		before := len(h.gw.rings(dev))
		call := h.startIn(t, dm, caller, http.StatusCreated)
		if len(h.gw.rings(dev)) > before {
			rang++
		}
		h.end(t, call, caller)
		h.e.Clk.Advance(15 * time.Second)
	}
	return rang
}

// rang reports whether one start by caller in dm rings dev (the call is ended afterwards).
func (h *dmCallHost) rang(t *testing.T, dm id.ID, caller ringUser, others []ringUser, dev id.ID) bool {
	t.Helper()
	h.register(t, dm, append([]ringUser{caller}, others...)...)
	before := len(h.gw.rings(dev))
	call := h.startIn(t, dm, caller, http.StatusCreated)
	got := false
	for _, f := range h.gw.rings(dev)[before:] {
		got = got || f.State == 1
	}
	h.end(t, call, caller)
	return got
}

// Attacker: a DM participant (or anyone who opens DMs with the victim) ringing without end.
func TestRingsArePairRateLimited(t *testing.T) {
	h := newDMCallHost(t)
	bob := []ringUser{h.bob}
	bobDev := h.bob.devices[0]
	call := h.startCall(t, h.alice)
	h.end(t, call, h.alice)
	h.e.Clk.Advance(10 * time.Second)
	if h.rang(t, h.dm, h.alice, bob, bobDev) {
		t.Fatal("a second ring 10 s after the first rang bob: the 15 s pair gap is not kept")
	}
	h.e.Clk.Advance(5 * time.Second)
	if !h.rang(t, h.dm, h.alice, bob, bobDev) {
		t.Fatal("a ring 15 s after the last one did not ring bob")
	}
	h.e.Clk.Advance(15 * time.Second)

	// Two rings so far this hour; 18 more fill the rolling hour's 20, and the 21st rings nobody.
	if got := h.ringBurst(t, h.dm, h.alice, bob, bobDev, 18); got != 18 {
		t.Fatalf("%d of 18 rings 15 s apart rang bob, want all", got)
	}
	if h.rang(t, h.dm, h.alice, bob, bobDev) {
		t.Fatal("the 21st ring within an hour rang bob")
	}
	// The same 21st start rings carol in a separate DM with alice: the bound is per pair.
	carolDM := h.openDMWith(t, h.alice, h.carol)
	if !h.rang(t, carolDM, h.alice, []ringUser{h.carol}, h.carol.devices[0]) {
		t.Fatal("alice's first ring of carol was suppressed by alice's rings of bob")
	}
	// Past the hour from the first ring, the oldest falls out of the window and bob rings again.
	h.e.Clk.Advance(time.Hour)
	if !h.rang(t, h.dm, h.alice, bob, bobDev) {
		t.Fatal("a ring past the rolling hour did not ring bob")
	}
}

// WB product walk I6: the caller learns whom the start rang. A start that opens a DM or group-DM call
// carries a seventh element, the users it rang; a recipient the pair rule held back is not in it, so
// the caller's page never says "ringing" for a ring nobody got. A 200 start and a community voice
// start keep the six elements.
func TestAStartTellsTheCallerWhomItRang(t *testing.T) {
	h := newDMCallHost(t)
	status, body := h.e.Do(http.MethodPost, "/v1/channels/"+h.dm.String()+"/calls", h.alice.tok, []any{})
	if status != http.StatusCreated {
		t.Fatalf("start = %d %x", status, body)
	}
	first := decodeCall(t, body)
	if !first.HasRung || !slices.Equal(first.Rung, []id.ID{h.bob.user}) {
		t.Fatalf("the first start's rung = %v (present %v), want [bob]", first.Rung, first.HasRung)
	}
	// bob's own start of the live call is a 200: nothing rang, and the answer keeps six elements.
	status, body = h.e.Do(http.MethodPost, "/v1/channels/"+h.dm.String()+"/calls", h.bob.tok, []any{})
	if status != http.StatusOK || decodeCall(t, body).HasRung {
		t.Fatalf("a 200 start = %d with rung present %v, want 200 and six elements", status, decodeCall(t, body).HasRung)
	}
	h.end(t, first.CallID, h.alice)
	// A call-back within the 15 s pair gap opens the call and rings nobody: rung is empty.
	h.e.Clk.Advance(5 * time.Second)
	h.register(t, h.dm, h.alice, h.bob)
	status, body = h.e.Do(http.MethodPost, "/v1/channels/"+h.dm.String()+"/calls", h.alice.tok, []any{})
	again := decodeCall(t, body)
	if status != http.StatusCreated || !again.HasRung || len(again.Rung) != 0 {
		t.Fatalf("a call-back within the pair gap = %d rung %v (present %v), want 201 and an empty rung", status, again.Rung, again.HasRung)
	}
	h.end(t, again.CallID, h.alice)
	// A group DM where bob's pair is still held and carol's is not: only carol is rung.
	gdm := h.openDMWith(t, h.alice, h.bob, h.carol)
	status, body = h.e.Do(http.MethodPost, "/v1/channels/"+gdm.String()+"/calls", h.alice.tok, []any{})
	group := decodeCall(t, body)
	if status != http.StatusCreated || !slices.Equal(group.Rung, []id.ID{h.carol.user}) {
		t.Fatalf("the group DM start = %d rung %v, want 201 and [carol]", status, group.Rung)
	}
}

// Attacker: mallory rings bob without end to exhaust a budget and silence alice's calls to bob. The
// bound is per pair, so she cannot.
func TestAnotherCallersRingsNeverSilenceAlice(t *testing.T) {
	h := newDMCallHost(t)
	mallory := h.newUser(t, "mallory", 1)
	malDM := h.openDMWith(t, mallory, h.bob)
	if got := h.ringBurst(t, malDM, mallory, []ringUser{h.bob}, h.bob.devices[0], 21); got != 20 {
		t.Fatalf("mallory's 21 rings rang bob %d times, want 20 (rate-limited after 20)", got)
	}
	if !h.rang(t, h.dm, h.alice, []ringUser{h.bob}, h.bob.devices[0]) {
		t.Fatal("mallory's rings silenced alice's call to bob")
	}
}
