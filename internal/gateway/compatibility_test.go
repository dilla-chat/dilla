package gateway

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/id"
)

func TestWireTwoFanoutFiltersOnlyEnabledCommunityAndInteractionGroups(t *testing.T) {
	h := newHarness(t)
	h.gw.opts.WireVersions = []uint64{2}
	device := id.New()
	community, otherCommunity := id.New(), id.New()
	blocked, ordinary, dm, pairing, interaction := id.New(), id.New(), id.New(), id.New(), id.New()
	h.setGroupResource(blocked, GroupResource{Kind: 0, CommunityID: &community})
	h.setGroupResource(ordinary, GroupResource{Kind: 0, CommunityID: &otherCommunity})
	h.setGroupResource(dm, GroupResource{Kind: 0})
	h.setGroupResource(pairing, GroupResource{Kind: 2})
	h.setGroupResource(interaction, GroupResource{Kind: 3})
	h.setCommunityEnabled(community, true)
	h.setCommunityEnabled(otherCommunity, false)
	c := h.connect(t, device)
	h.drainReady(t, c)
	for _, group := range []id.ID{blocked, ordinary, dm, pairing, interaction} {
		h.gw.SetGroupMembers(group, []id.ID{device})
	}
	p, err := EpochChangedPayload(1, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range []id.ID{blocked, ordinary, dm, pairing, interaction} {
		h.gw.DeliverGroup(group, Frame{Op: OpMLSEpochChanged, GroupID: &group, Payload: p, Replay: true})
	}
	c.mu.Lock()
	gotN := c.n
	c.mu.Unlock()
	if gotN != 4 { // ready plus ordinary community, true DM, and pairing frames
		t.Fatalf("wire-2 replay sequence n = %d, want ready plus three unaffected frames", gotN)
	}

	h.setCommunityEnabled(otherCommunity, true)
	h.gw.DeliverGroup(ordinary, Frame{Op: OpMLSEpochChanged, GroupID: &ordinary, Payload: p, Replay: true})
	c.mu.Lock()
	gotN = c.n
	c.mu.Unlock()
	if gotN != 4 {
		t.Fatalf("enabling a community while connected admitted a new scoped frame; n = %d", gotN)
	}
	h.setCommunityEnabled(otherCommunity, false)
	h.gw.DeliverGroup(ordinary, Frame{Op: OpMLSEpochChanged, GroupID: &ordinary, Payload: p, Replay: true})
	c.mu.Lock()
	gotN = c.n
	c.mu.Unlock()
	if gotN != 5 {
		t.Fatalf("disabling a community did not restore wire-2 delivery; n = %d, want 5", gotN)
	}
}

func TestWireTwoChannelFanoutAndCurrentCallScope(t *testing.T) {
	h := newHarness(t)
	h.gw.opts.WireVersions = []uint64{2}
	device := id.New()
	community := id.New()
	communityChannel, dmChannel := id.New(), id.New()
	h.setChannelCommunity(communityChannel, &community)
	h.setCommunityEnabled(community, true)
	c := h.connect(t, device)
	h.drainReady(t, c)
	h.gw.SetChannelMembers(communityChannel, []id.ID{c.userID})
	h.gw.SetChannelMembers(dmChannel, []id.ID{c.userID})
	p, err := payload("event")
	if err != nil {
		t.Fatal(err)
	}
	h.gw.DeliverChannel(communityChannel, Frame{Op: OpMessagePlain, Payload: p, Replay: true})
	h.gw.DeliverChannel(dmChannel, Frame{Op: OpMessagePlain, Payload: p, Replay: true})
	c.mu.Lock()
	gotN := c.n
	c.mu.Unlock()
	if gotN != 2 { // ready plus the DM channel event
		t.Fatalf("wire-2 readable/call fanout sequence n = %d, want 2", gotN)
	}

	// User fan-out producers such as voice_state and call_ring carry their resolved channel scope.
	h.gw.DeliverUser(c.userID, Frame{Op: OpVoiceState, Payload: p, Scope: FrameScope{Channels: []id.ID{communityChannel}}, Replay: true})
	c.mu.Lock()
	gotN = c.n
	c.mu.Unlock()
	if gotN != 2 {
		t.Fatalf("a scoped user fan-out entered the wire-2 ring; n = %d", gotN)
	}
}

func TestAddressedWelcomeKeepsItsGroupScopeThroughDeviceFanout(t *testing.T) {
	h := newHarness(t)
	h.gw.opts.WireVersions = []uint64{2}
	device := id.New()
	community, group := id.New(), id.New()
	h.setGroupResource(group, GroupResource{Kind: 0, CommunityID: &community})
	h.setCommunityEnabled(community, true)
	c := h.connect(t, device)
	h.drainReady(t, c)
	p, err := payload(uint64(1), uint64(1), uint64(1), []byte("welcome"), []byte("tree"), []byte("hash"))
	if err != nil {
		t.Fatal(err)
	}
	h.gw.DeliverDevice(device, Frame{Op: OpMLSWelcome, GroupID: &group, Payload: p, Replay: true})
	h.gw.DeliverUser(c.userID, Frame{Op: OpMLSWelcome, GroupID: &group, Payload: p, Replay: true})
	c.mu.Lock()
	n := c.n
	c.mu.Unlock()
	if n != 1 {
		t.Fatalf("wire-2 connection received an enabled community's addressed Welcome; n = %d", n)
	}

	// The same device-targeted path still delivers true DM and pairing group Welcomes.
	dm := id.New()
	h.setGroupResource(dm, GroupResource{Kind: 0})
	h.gw.DeliverDevice(device, Frame{Op: OpMLSWelcome, GroupID: &dm, Payload: p, Replay: true})
	pairing := id.New()
	h.setGroupResource(pairing, GroupResource{Kind: 2})
	h.gw.DeliverDevice(device, Frame{Op: OpMLSWelcome, GroupID: &pairing, Payload: p, Replay: true})
	c.mu.Lock()
	n = c.n
	c.mu.Unlock()
	if n != 3 {
		t.Fatalf("unaffected addressed Welcome was filtered; n = %d, want 3", n)
	}
	entries := c.ring.entriesSince(0)
	if len(entries) != 3 || !slices.Equal(entries[1].scope.Groups, []id.ID{dm}) || !slices.Equal(entries[2].scope.Groups, []id.ID{pairing}) {
		t.Fatalf("Welcome replay scope = %#v, want its addressed group", entries)
	}
}

func TestSubscribeAndCommitAckRequireEligibleGroupAfterMembership(t *testing.T) {
	h := newHarness(t)
	h.gw.opts.WireVersions = []uint64{2}
	device := id.New()
	community, enabledGroup, ordinaryGroup := id.New(), id.New(), id.New()
	h.setGroupResource(enabledGroup, GroupResource{Kind: 0, CommunityID: &community})
	h.setGroupResource(ordinaryGroup, GroupResource{Kind: 2})
	h.setCommunityEnabled(community, true)
	h.setGroupsForDevice(device, enabledGroup, ordinaryGroup)
	c := h.connect(t, device)
	h.drainReady(t, c)

	groupsPayload, err := payload([]id.ID{enabledGroup, ordinaryGroup})
	if err != nil {
		t.Fatal(err)
	}
	wire, err := Encode(Frame{Op: OpSubscribe, Payload: groupsPayload}, 0)
	if err != nil {
		t.Fatal(err)
	}
	in, err := Decode(wire, 16384)
	if err != nil {
		t.Fatal(err)
	}
	in.CID = 17
	h.gw.handle(context.Background(), c, in)
	c.mu.Lock()
	_, blockedSubscribed := c.groups[enabledGroup]
	_, dmSubscribed := c.groups[ordinaryGroup]
	c.mu.Unlock()
	if blockedSubscribed || !dmSubscribed {
		t.Fatalf("subscribe result blocked=%v unaffected=%v; want blocked out and unaffected in", blockedSubscribed, dmSubscribed)
	}
	frame := h.waitFrame(t, device)
	if frame.Op != OpError {
		t.Fatalf("subscribe refusal op = %d, want error", frame.Op)
	}
	code, err := rawText(frame.Payload[1])
	if err != nil || code != "E_VERSION" {
		t.Fatalf("subscribe refusal code = %q, %v; want E_VERSION", code, err)
	}

	acked := 0
	h.gw.opts.CommitAck = func(context.Context, id.ID, id.ID, uint64) { acked++ }
	ackPayload, err := payload(uint64(3))
	if err != nil {
		t.Fatal(err)
	}
	ackWire, err := Encode(Frame{Op: OpCommitAck, GroupID: &enabledGroup, Payload: ackPayload}, 0)
	if err != nil {
		t.Fatal(err)
	}
	ack, err := Decode(ackWire, 16384)
	if err != nil {
		t.Fatal(err)
	}
	h.gw.handle(context.Background(), c, ack)
	if acked != 0 {
		t.Fatal("wire-2 commit_ack reached the group handler after compatibility refusal")
	}
	c.mu.Lock()
	c.wire = 3
	c.mu.Unlock()
	h.gw.handle(context.Background(), c, ack)
	if acked != 1 {
		t.Fatal("wire-3 commit_ack did not reach the group handler")
	}
}

func TestWireTwoReadyIsFilteredAndResumeRefusesNewlyBlockedReplay(t *testing.T) {
	h := newHarness(t)
	h.gw.opts.WireVersions = []uint64{2}
	device := id.New()
	community := id.New()
	communityGroup, dmGroup, pairingGroup := id.New(), id.New(), id.New()
	h.setGroupResource(communityGroup, GroupResource{Kind: 0, CommunityID: &community})
	h.setGroupResource(dmGroup, GroupResource{Kind: 0})
	h.setGroupResource(pairingGroup, GroupResource{Kind: 2})
	h.setGroupsForDevice(device, communityGroup, dmGroup, pairingGroup)
	h.setCommunityEnabled(community, false)
	c := h.connect(t, device)
	h.drainReady(t, c)
	entries := c.ring.entriesSince(0)
	if len(entries) != 1 || !slices.Equal(entries[0].scope.Groups, []id.ID{communityGroup, dmGroup, pairingGroup}) {
		t.Fatalf("ready replay scope = %#v, want all included groups in its digest", entries)
	}

	h.gw.suspend(c)
	h.setCommunityEnabled(community, true)
	resumePayload, err := payload(h.tokenFor(device), c.resumeToken(), h.generation, uint64(0))
	if err != nil {
		t.Fatal(err)
	}
	resumeWire, err := Encode(Frame{Op: OpResume, Payload: resumePayload}, 0)
	if err != nil {
		t.Fatal(err)
	}
	resume, err := Decode(resumeWire, 16384)
	if err != nil {
		t.Fatal(err)
	}
	resumeSink := newRecordingSink(8, false)
	if _, ok := h.gw.resumeConnection(context.Background(), resumeSink, resume); ok {
		t.Fatal("resume replayed a ready digest that now contains a wire-3-only community")
	}
	if _, ok := h.gw.suspended.Load(string(c.resumeToken())); ok {
		t.Fatal("a nonresumable scope refusal kept the old resume token alive")
	}
	if invalid := h.readFrom(t, resumeSink); invalid.Op != OpInvalidSession {
		t.Fatalf("refused resume frame op = %d, want invalid_session", invalid.Op)
	}

	identifyPayload, err := payload(h.tokenFor(device), uint64(2), uint64(1), uint64(1), uint64(0))
	if err != nil {
		t.Fatal(err)
	}
	identifyWire, err := Encode(Frame{Op: OpIdentify, Payload: identifyPayload}, 0)
	if err != nil {
		t.Fatal(err)
	}
	identify, err := Decode(identifyWire, 16384)
	if err != nil {
		t.Fatal(err)
	}
	if fresh, ok := h.gw.identifyConnection(context.Background(), newRecordingSink(8, false), identify); !ok || fresh == nil {
		t.Fatal("fresh wire-2 identify should succeed after the old replay is refused")
	} else if current := fresh.ring.entriesSince(0); len(current) != 1 || !slices.Equal(current[0].scope.Groups, []id.ID{dmGroup, pairingGroup}) {
		t.Fatalf("fresh filtered ready scope = %#v, want only the DM and pairing groups", current)
	}
}

func TestAllowedReplayRemainsContiguousAcrossScopedFrames(t *testing.T) {
	h := newHarness(t)
	h.gw.opts.WireVersions = []uint64{2}
	device := id.New()
	community, group := id.New(), id.New()
	h.setGroupResource(group, GroupResource{Kind: 0, CommunityID: &community})
	h.setGroupsForDevice(device, group)
	h.setCommunityEnabled(community, false)
	c := h.connect(t, device)
	h.drainReady(t, c)
	h.gw.SetGroupMembers(group, []id.ID{device})
	p, err := EpochChangedPayload(4, 5)
	if err != nil {
		t.Fatal(err)
	}
	h.gw.DeliverGroup(group, Frame{Op: OpMLSEpochChanged, GroupID: &group, Payload: p, Replay: true})
	h.gw.suspend(c)

	resumePayload, err := payload(h.tokenFor(device), c.resumeToken(), h.generation, uint64(0))
	if err != nil {
		t.Fatal(err)
	}
	resumeWire, err := Encode(Frame{Op: OpResume, Payload: resumePayload}, 0)
	if err != nil {
		t.Fatal(err)
	}
	resume, err := Decode(resumeWire, 16384)
	if err != nil {
		t.Fatal(err)
	}
	sink := newRecordingSink(8, false)
	if resumed, ok := h.gw.resumeConnection(context.Background(), sink, resume); !ok || resumed != c {
		t.Fatal("an allowed scoped replay should resume the original connection")
	}
	for _, want := range []Op{OpResumed, OpReady, OpMLSEpochChanged} {
		if got := h.readFrom(t, sink).Op; got != want {
			t.Fatalf("replay op = %d, want contiguous op %d", got, want)
		}
	}
}

func TestOnlineElectionExcludesWireTwoOnlyWhenGroupRequiresUpgrade(t *testing.T) {
	h := newHarness(t)
	h.gw.opts.Auth = &capabilityRecordingAuth{Authenticator: h}
	community := id.New()
	group := id.New()
	h.setGroupResource(group, GroupResource{Kind: 0, CommunityID: &community})
	h.setCommunityEnabled(community, true)
	oldDevice, dualDevice, dualUser := id.New(), id.New(), id.New()
	old := registerAtWire(t, h, oldDevice, id.New(), 2)
	dual2 := registerAtWire(t, h, dualDevice, dualUser, 2)
	h.clk.Advance(1)
	dual3 := registerAtWire(t, h, dualDevice, dualUser, 3)
	h.gw.SetGroupMembers(group, []id.ID{oldDevice, dualDevice})
	candidates := h.gw.OnlineIn(group)
	if len(candidates) != 1 || candidates[0].DeviceID != dualDevice || !candidates[0].ReadyAt.Equal(dual3.readyAt()) {
		t.Fatalf("eligible committer candidates = %#v, want the dual-session device at its wire-3 connection", candidates)
	}
	h.setCommunityEnabled(community, false)
	candidates = h.gw.OnlineIn(group)
	if len(candidates) != 2 {
		t.Fatalf("unaffected group candidates = %d, want both connected devices", len(candidates))
	}
	var dualCandidate *OnlineDevice
	for i := range candidates {
		if candidates[i].DeviceID == dualDevice {
			dualCandidate = &candidates[i]
		}
	}
	if dualCandidate == nil || !dualCandidate.ReadyAt.Equal(dual2.readyAt()) {
		t.Fatalf("unaffected dual-session candidate = %#v, want earliest connection wire 2", dualCandidate)
	}
	_ = old
}

func registerAtWire(t *testing.T, h *harness, device, user id.ID, wire uint64) *conn {
	t.Helper()
	hash := make([]byte, 32)
	copy(hash, device[:])
	hash[31] = byte(wire)
	session := auth.Session{DeviceID: device, UserID: user, Scope: auth.ScopeEnrolled, TokenHash: hash}
	sink := newRecordingSink(1024, false)
	c, err := h.gw.registerVersions(context.Background(), session, sink, [3]uint64{wire, 1, 1})
	if err != nil {
		t.Fatalf("register wire %d: %v", wire, err)
	}
	h.mu.Lock()
	h.sinks[c] = sink
	h.mu.Unlock()
	return c
}

func TestCompatibilityLookupFailureFailsClosedAndSharedGateIsFreshAcrossGateways(t *testing.T) {
	h := newHarness(t)
	h.gw.opts.WireVersions = []uint64{2}
	device := id.New()
	community, group := id.New(), id.New()
	h.setGroupResource(group, GroupResource{Kind: 0, CommunityID: &community})
	h.setCommunityEnabled(community, false)
	c := h.connect(t, device)
	h.drainReady(t, c)
	h.gw.SetGroupMembers(group, []id.ID{device})
	p, _ := EpochChangedPayload(1, 2)
	h.gw.DeliverGroup(group, Frame{Op: OpMLSEpochChanged, GroupID: &group, Payload: p, Replay: true})
	c.mu.Lock()
	before := c.n
	c.mu.Unlock()

	h.mu.Lock()
	h.policyErr = errors.New("temporary repository error")
	h.mu.Unlock()
	h.gw.DeliverGroup(group, Frame{Op: OpMLSEpochChanged, GroupID: &group, Payload: p, Replay: true})
	c.mu.Lock()
	after := c.n
	c.mu.Unlock()
	if after != before {
		t.Fatalf("storage error admitted scoped frame: n moved from %d to %d", before, after)
	}
	h.mu.Lock()
	h.policyErr = nil
	h.enabled[community] = true
	h.mu.Unlock()

	// A second gateway observes the same authoritative scope/policy source. It must see a state
	// change made after the first gateway was constructed, with no per-process enablement cache.
	second := New(Options{
		Clock: h.clk, Store: h, Auth: h, WireVersions: []uint64{2},
		ScopeResolver: h, CommunityCompatibility: h,
	})
	t.Cleanup(func() { _ = second.Shutdown(context.Background()) })
	second.SetGroupMembers(group, []id.ID{device})
	secondSink := newRecordingSink(8, false)
	secondSession := auth.Session{DeviceID: device, UserID: c.userID, Scope: auth.ScopeEnrolled}
	secondConn, err := second.registerVersions(context.Background(), secondSession, secondSink, [3]uint64{2, 1, 1})
	if err != nil {
		t.Fatal(err)
	}
	second.DeliverGroup(group, Frame{Op: OpMLSEpochChanged, GroupID: &group, Payload: p, Replay: true})
	secondConn.mu.Lock()
	secondN := secondConn.n
	secondConn.mu.Unlock()
	if secondN != 1 { // only the ready frame
		t.Fatalf("second gateway admitted a stale wire-2 frame after enablement; n = %d", secondN)
	}
}

func TestBearerRenegotiationDoesNotUpgradeExistingWireTwoConnectionOrResume(t *testing.T) {
	h := newHarness(t)
	h.gw.opts.WireVersions = []uint64{2}
	device, community, group := id.New(), id.New(), id.New()
	h.setGroupResource(group, GroupResource{Kind: 0, CommunityID: &community})
	h.setCommunityEnabled(community, false)
	h.setGroupsForDevice(device, id.New()) // an unaffected binding allows ready to replay
	c := h.connect(t, device)
	h.drainReady(t, c)
	oldWire := c.wire

	// A later HTTP negotiation updates the current bearer capability, while the open connection
	// retains the wire version selected when it identified.
	h.mu.Lock()
	session := h.tokens[h.tokenFor(device)]
	session.WireVersion = 3
	h.tokens[h.tokenFor(device)] = session
	h.mu.Unlock()
	h.setCommunityEnabled(community, true)
	h.gw.SetGroupMembers(group, []id.ID{device})
	p, _ := EpochChangedPayload(2, 3)
	h.gw.DeliverGroup(group, Frame{Op: OpMLSEpochChanged, GroupID: &group, Payload: p, Replay: true})
	c.mu.Lock()
	n := c.n
	c.mu.Unlock()
	if n != 1 {
		t.Fatalf("current bearer wire 3 upgraded a live wire-%d connection; n = %d", oldWire, n)
	}

	h.gw.suspend(c)
	resumePayload, err := payload(h.tokenFor(device), c.resumeToken(), h.generation, uint64(0))
	if err != nil {
		t.Fatal(err)
	}
	resumeWire, err := Encode(Frame{Op: OpResume, Payload: resumePayload}, 0)
	if err != nil {
		t.Fatal(err)
	}
	resume, err := Decode(resumeWire, 16384)
	if err != nil {
		t.Fatal(err)
	}
	resumeSink := newRecordingSink(8, false)
	resumed, ok := h.gw.resumeConnection(context.Background(), resumeSink, resume)
	if !ok || resumed != c || c.wire != oldWire {
		t.Fatalf("resume changed original connection wire version: conn=%p ok=%v wire=%d, want %d", resumed, ok, c.wire, oldWire)
	}
	h.gw.DeliverGroup(group, Frame{Op: OpMLSEpochChanged, GroupID: &group, Payload: p, Replay: true})
	c.mu.Lock()
	n = c.n
	c.mu.Unlock()
	if n != 2 { // ready plus resumed; scoped delivery remains blocked
		t.Fatalf("resumed wire-%d connection admitted newly enabled traffic; n = %d", oldWire, n)
	}
}
