package api_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/jonasthim/dilla/internal/api"
	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/ds"
	"github.com/jonasthim/dilla/internal/gateway"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/sfu"
	"github.com/jonasthim/dilla/internal/sfu/sfutest"
	"github.com/jonasthim/dilla/internal/store"
)

// callDS records the two delivery-service calls the call events make. Close closes the group on the
// store, as the real one does, so the routes see it closed.
type callDS struct {
	repo    store.Repository
	mu      sync.Mutex
	removes [][2]id.ID // (group, device)
	closed  []id.ID
	// memberRemoves is every membership-bound Remove issued (ProposeRemoveOfMember).
	memberRemoves []memberRemove
	// closeBlocks makes Close wait until its context ends: a delivery service stuck on the group.
	closeBlocks bool
}

var _ api.DS = (*callDS)(nil)

func (d *callDS) ProposeAdd(context.Context, id.ID, id.ID, id.ID) error     { return nil }
func (d *callDS) ProposeRemove(context.Context, id.ID, uint32, id.ID) error { return nil }
func (d *callDS) ProposeAddBatch(context.Context, id.ID, []id.ID) error     { return nil }
func (d *callDS) VoidIneligibleAdds(context.Context, id.ID) error           { return nil }

// ProposeRemoveOf is recorded with the device-addressed Removes: a leave bound to the membership its
// session joined with (DS-7) issues it.
func (d *callDS) ProposeRemoveOf(_ context.Context, g id.ID, _ uint32, dev, _ id.ID) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.removes = append(d.removes, [2]id.ID{g, dev})
	return nil
}

// ProposeRemoveOfMember checks the binding the way the delivery service does — the live member row at
// leaf must be dev, added in addedEpoch — refusing with ds.ErrRemoveTargetGone otherwise, and records
// an issued Remove with the device-addressed ones and in memberRemoves.
func (d *callDS) ProposeRemoveOfMember(ctx context.Context, g id.ID, leaf uint32, dev id.ID, added uint64, _ id.ID) error {
	members, err := d.repo.ListMembers(ctx, g)
	if err != nil {
		return err
	}
	bound := false
	for _, m := range members {
		if m.LeafIndex == leaf && m.RemovedEpoch == nil && m.DeviceID == dev && m.AddedEpoch == added {
			bound = true
		}
	}
	if !bound {
		return fmt.Errorf("callDS: %w", ds.ErrRemoveTargetGone)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.removes = append(d.removes, [2]id.ID{g, dev})
	d.memberRemoves = append(d.memberRemoves, memberRemove{Group: g, Leaf: leaf, Device: dev, Added: added})
	return nil
}

// memberRemove is one membership-bound Remove callDS issued.
type memberRemove struct {
	Group, Device id.ID
	Leaf          uint32
	Added         uint64
}

func (d *callDS) memberRemovals() []memberRemove {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.memberRemoves)
}

func (d *callDS) ProposeRemoveDevice(_ context.Context, g, dev, _ id.ID) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.removes = append(d.removes, [2]id.ID{g, dev})
	return nil
}

// CloseIfAlone checks the members the way the delivery service does (ds.SoleLeaf), then closes.
func (d *callDS) CloseIfAlone(ctx context.Context, g, dev id.ID) error {
	row, err := d.repo.GetGroup(ctx, g)
	if err != nil {
		return err
	}
	if row.ClosedAt != nil {
		return nil
	}
	members, err := d.repo.ListMembers(ctx, g)
	if err != nil {
		return err
	}
	sole, err := ds.SoleLeaf(ctx, d.repo, members, row.Epoch, dev)
	if err != nil {
		return err
	}
	if !sole {
		return fmt.Errorf("callDS: %w", ds.ErrNotAlone)
	}
	return d.Close(ctx, g)
}

func (d *callDS) Close(ctx context.Context, g id.ID) error {
	d.mu.Lock()
	d.closed = append(d.closed, g)
	blocks := d.closeBlocks
	d.mu.Unlock()
	if blocks {
		<-ctx.Done()
		return ctx.Err()
	}
	return d.repo.CloseGroup(ctx, g, time.Now().Unix())
}

func (d *callDS) removeDevices() [][2]id.ID {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.removes)
}

func (d *callDS) closedGroups() []id.ID {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.closed)
}

// voiceFrame is one voice_state the gateway was asked to deliver, decoded.
type voiceFrame struct {
	To, User, Device, Call id.ID
	Op                     gateway.Op
	Flags                  uint64
}

type voiceGW struct {
	mu     sync.Mutex
	frames []voiceFrame
}

func (g *voiceGW) DeliverUser(to id.ID, f gateway.Frame) {
	vf := voiceFrame{To: to, Op: f.Op}
	var raw []cbor.RawMessage
	if err := cborx.Unmarshal(f.Payload, &raw); err == nil && len(raw) == 4 {
		_ = cborx.Unmarshal(raw[0], &vf.User)
		_ = cborx.Unmarshal(raw[1], &vf.Device)
		_ = cborx.Unmarshal(raw[2], &vf.Call)
		_ = cborx.Unmarshal(raw[3], &vf.Flags)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.frames = append(g.frames, vf)
}

func (g *voiceGW) all() []voiceFrame {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.frames)
}

func (g *voiceGW) last() voiceFrame {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.frames) == 0 {
		return voiceFrame{}
	}
	return g.frames[len(g.frames)-1]
}

// eventsFixture is an open call — the owner's device a leaf, its room the stub's — with the call
// events built over the same Calls.
type eventsFixture struct {
	e                     *env
	ch, group, dev, owner id.ID
	callID                id.ID
	tok, room             string
	stub                  *stubSFU
	calls                 *api.Calls
	ds                    *callDS
	gw                    *voiceGW
	events                *api.CallEvents
}

func newEventsFixture(t *testing.T, cfg api.CallsConfig) *eventsFixture {
	t.Helper()
	cfg.LiveKitURL = testLiveKitURL
	e, ch, tok, group, stub, calls := callEnvCalls(t, cfg)
	f := &eventsFixture{e: e, ch: ch, group: group, dev: deviceOf(t, e, tok), owner: userOf(t, e, tok),
		tok: tok, stub: stub, calls: calls, ds: &callDS{repo: e.Repo}, gw: &voiceGW{}}
	seedLeaf(t, e, group, f.dev, 3, nil)
	f.events = api.NewCallEvents(e.Repo, api.NewResolver(e.Repo), f.ds, stub, calls, f.gw, e.Clk, slog.New(slog.DiscardHandler))
	status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	if status != http.StatusCreated {
		t.Fatalf("opening the call = %d %s", status, e.ErrCode(body))
	}
	f.callID = decodeCall(t, body).CallID
	f.room = stub.minted()[0][0]
	return f
}

func (f *eventsFixture) send(t *testing.T, name, identity string, track *livekit.TrackInfo) {
	t.Helper()
	f.sendAs(t, name, identity, "", track)
}

// sendAs is send with the participant's session id (LiveKit's ParticipantInfo.Sid).
func (f *eventsFixture) sendAs(t *testing.T, name, identity, sid string, track *livekit.TrackInfo) {
	t.Helper()
	ev := &livekit.WebhookEvent{Event: name, Id: "EV_" + id.New().String(), Room: &livekit.Room{Name: f.room}, Track: track}
	if identity != "" {
		ev.Participant = &livekit.ParticipantInfo{Identity: identity, Sid: sid}
	}
	f.events.Handle(t.Context(), ev)
	f.events.FlushAnnouncements() // voice_state is delivered off the webhook's goroutine (CALLS-5)
}

func deviceRemovals(stub *stubSFU) [][2]string {
	devices, _ := stub.removals()
	return devices
}

// MD-11 / DEV-60: a join announces in_call to every view_channel holder and nobody else, and pushes
// the device's current grant (a token minted before a role change joins with a stale one).
func TestAJoinAnnouncesInCallToEveryViewerAndNobodyElse(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	member, _, _ := joinedMember(t, f.e, f.ch, f.group, "member")
	f.e.NewUser("stranger")
	f.send(t, webhook.EventParticipantJoined, f.dev.String(), nil)
	to := map[id.ID]bool{}
	for _, fr := range f.gw.all() {
		to[fr.To] = true
		if fr.Op != gateway.OpVoiceState || fr.User != f.owner || fr.Device != f.dev || fr.Call != f.callID || fr.Flags != api.VoiceInCall {
			t.Fatalf("frame = %+v", fr)
		}
	}
	if len(to) != 2 || !to[f.owner] || !to[member] {
		t.Fatalf("voice_state reached %v, want the owner and the member only", to)
	}
	u := f.stub.permUpdates()
	if len(u) != 1 || u[0].Identity != f.dev.String() ||
		!slices.Equal(u[0].Perm.GetCanPublishSources(), []livekit.TrackSource{livekit.TrackSource_MICROPHONE}) {
		t.Fatalf("the join pushed %+v, want the owner's microphone grant", u)
	}
}

// DEV-45 / DEV-43: a mid-call leave proposes one Remove of the device through the delivery service
// (a duplicate within the proposal TTL proposes nothing more) and announces flags 0; a "#" shadow's
// leave says nothing about the device.
func TestALeaveMidCallProposesOneRemoveAndAnnouncesZero(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	f.send(t, webhook.EventParticipantJoined, f.dev.String(), nil)
	f.send(t, webhook.EventParticipantLeft, f.dev.String(), nil)
	f.send(t, webhook.EventParticipantConnectionAborted, f.dev.String(), nil)
	f.send(t, webhook.EventParticipantLeft, f.dev.String()+"#x", nil)
	if got := f.ds.removeDevices(); len(got) != 1 || got[0] != [2]id.ID{f.group, f.dev} {
		t.Fatalf("Remove proposals = %v, want one for (%s, %s)", got, f.group, f.dev)
	}
	if fr := f.gw.last(); fr.Device != f.dev || fr.Flags != 0 {
		t.Fatalf("the last voice_state = %+v, want flags 0 for the device", fr)
	}
}

// SP-21 row 8: a webhook can arrive ~45 s late. A participant_left for a session the device has
// already replaced — it rejoined and took a sharing slot again — changes nothing: no Remove is
// proposed, the slot stays held, no demotion is pushed and nobody is told the device left.
func TestAStaleLeaveAfterARejoinAndAShareChangesNothing(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{MaxPublishers: 1})
	_, _, memberTok := joinedMember(t, f.e, f.ch, f.group, "member")
	f.sendAs(t, webhook.EventParticipantJoined, f.dev.String(), "PA_old", nil)
	// The device rejoins under a new session and shares before the old session's leave arrives.
	f.stub.setPresent(f.room, &livekit.ParticipantInfo{Identity: f.dev.String(), Sid: "PA_new"})
	share := "/v1/calls/" + f.callID.String() + "/share"
	if status, _ := f.e.Do(http.MethodPost, share, f.tok, []any{}); status != http.StatusNoContent {
		t.Fatalf("share after the rejoin = %d", status)
	}
	pushes := len(f.stub.permUpdates())
	frames := len(f.gw.all())
	f.sendAs(t, webhook.EventParticipantLeft, f.dev.String(), "PA_old", nil)
	f.sendAs(t, webhook.EventParticipantConnectionAborted, f.dev.String(), "PA_old", nil)
	if got := f.ds.removeDevices(); len(got) != 0 {
		t.Fatalf("a stale leave proposed %v", got)
	}
	if got := f.calls.SharersOf(f.callID); len(got) != 1 || got[0] != f.dev {
		t.Fatalf("sharers after a stale leave = %v, want the device", got)
	}
	if got := f.stub.permUpdates(); len(got) != pushes {
		t.Fatalf("a stale leave pushed %+v", got[pushes:])
	}
	if got := f.gw.all(); len(got) != frames {
		t.Fatalf("a stale leave announced %+v", got[frames:])
	}
	if status, _ := f.e.Do(http.MethodPost, share, memberTok, []any{}); status != http.StatusConflict {
		t.Fatalf("the member's share = %d, want 409: the rejoined device still holds the one slot", status)
	}
}

// A real leave frees the departed device's slot through the call's lock (no slot outlives its
// sharer) and the next device can share.
func TestALeaveFreesTheSharingSlot(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{MaxPublishers: 1})
	_, _, memberTok := joinedMember(t, f.e, f.ch, f.group, "member")
	share := "/v1/calls/" + f.callID.String() + "/share"
	f.sendAs(t, webhook.EventParticipantJoined, f.dev.String(), "PA_1", nil)
	if status, _ := f.e.Do(http.MethodPost, share, f.tok, []any{}); status != http.StatusNoContent {
		t.Fatalf("share = %d", status)
	}
	f.sendAs(t, webhook.EventParticipantLeft, f.dev.String(), "PA_1", nil)
	if got := f.calls.SharersOf(f.callID); len(got) != 0 {
		t.Fatalf("sharers after the leave = %v, want none", got)
	}
	if status, _ := f.e.Do(http.MethodPost, share, memberTok, []any{}); status != http.StatusNoContent {
		t.Fatalf("the member's share after the sharer left = %d, want 204", status)
	}
}

// DEV-46 / G30: room_finished ends the call — session ended, room deleted, group closed through the
// delivery service, flags 0 for the device seen in it — and a participant_left after it proposes
// nothing; a second room_finished changes nothing; the next start needs a fresh call group.
func TestARoomFinishedEndsTheCall(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	f.send(t, webhook.EventParticipantJoined, f.dev.String(), nil)
	f.send(t, webhook.EventRoomFinished, "", nil)
	row, err := f.e.Repo.GetVoiceSession(t.Context(), f.callID)
	if err != nil || row.Ended == nil {
		t.Fatalf("the voice session after room_finished = %+v (%v), want it ended", row, err)
	}
	if got := f.stub.deletedRooms(); len(got) != 1 || got[0] != f.room {
		t.Fatalf("rooms deleted = %v", got)
	}
	if got := f.ds.closedGroups(); len(got) != 1 || got[0] != f.group {
		t.Fatalf("groups closed = %v, want the call group once", got)
	}
	if fr := f.gw.last(); fr.Device != f.dev || fr.Flags != 0 {
		t.Fatalf("the last voice_state = %+v, want flags 0", fr)
	}
	f.send(t, webhook.EventParticipantLeft, f.dev.String(), nil)
	if got := f.ds.removeDevices(); len(got) != 0 {
		t.Fatalf("a leave after the call ended proposed %v", got)
	}
	f.send(t, webhook.EventRoomFinished, "", nil)
	if len(f.stub.deletedRooms()) != 1 || len(f.ds.closedGroups()) != 1 {
		t.Fatal("a second room_finished ended the call again")
	}
	if status, _ := f.e.Do(http.MethodPost, "/v1/channels/"+f.ch.String()+"/calls", f.tok, []any{}); status != http.StatusNotFound {
		t.Fatalf("a start after the call ended = %d, want 404 (no open call group)", status)
	}
}

// DELETE ends the call through the same path, closing the group through the delivery service.
func TestDeletingACallEndsItThroughTheCallEvents(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	f.send(t, webhook.EventParticipantJoined, f.dev.String(), nil)
	if status, _ := f.e.Do(http.MethodDelete, "/v1/calls/"+f.callID.String(), f.tok, nil); status != http.StatusNoContent {
		t.Fatalf("DELETE = %d", status)
	}
	f.events.FlushAnnouncements()
	if got := f.ds.closedGroups(); len(got) != 1 || got[0] != f.group {
		t.Fatalf("groups closed = %v", got)
	}
	if fr := f.gw.last(); fr.Device != f.dev || fr.Flags != 0 {
		t.Fatalf("the last voice_state = %+v, want flags 0", fr)
	}
}

// #37 CI (the AFK preview's ghost tile): LiveKit's participant_joined for a page's session is read
// while the call is live, and its work — the reconcile under the call's lock, then the room's state —
// runs after the call ended: a reloaded page's boot sweep ended the call it left behind (DELETE
// ?if_alone=1) in between. The late join must not put the device back in the ended call: the call
// events list nobody in it, and no voice_state after the end says the device is in the call, or every
// viewer keeps its tile until a reload (nothing announces it out again: the room is gone, and its
// leave and room_finished are dropped for an ended call). A late track of that session neither.
func TestAJoinOrTrackReadLiveAndHandledAfterTheEndLeavesNoGhost(t *testing.T) {
	for _, event := range []string{webhook.EventParticipantJoined, webhook.EventTrackPublished} {
		t.Run(event, func(t *testing.T) {
			f := newEventsFixture(t, api.CallsConfig{})
			live, err := f.e.Repo.GetVoiceSession(t.Context(), f.callID)
			if err != nil || live.Ended != nil {
				t.Fatalf("the voice session before the end = %+v (%v), want it live", live, err)
			}
			if status, body := f.e.Do(http.MethodDelete, "/v1/calls/"+f.callID.String()+"?if_alone=1", f.tok, nil); status != http.StatusNoContent {
				t.Fatalf("DELETE ?if_alone=1 = %d %s", status, f.e.ErrCode(body))
			}
			f.events.FlushAnnouncements()
			before := len(f.gw.all())
			ev := &livekit.WebhookEvent{Event: event, Id: "EV_" + id.New().String(), Room: &livekit.Room{Name: f.room},
				Participant: &livekit.ParticipantInfo{Identity: f.dev.String(), Sid: "PA_late"}}
			if event == webhook.EventTrackPublished {
				ev.Track = &livekit.TrackInfo{Sid: "TR_late", Source: livekit.TrackSource_MICROPHONE, Type: livekit.TrackType_AUDIO,
					Encryption: livekit.Encryption_GCM}
			}
			f.events.HandleReadLiveForTest(t.Context(), ev, live)
			f.events.FlushAnnouncements()
			if got := f.events.Participants(f.callID); len(got) != 0 {
				t.Fatalf("the ended call lists %+v, want nobody", got)
			}
			for _, fr := range f.gw.all()[before:] {
				if fr.Device == f.dev && fr.Flags != 0 {
					t.Fatalf("a voice_state after the end says the device is in the call: %+v", fr)
				}
			}
		})
	}
}

// An event naming another room of the call (an older one) or no call at all changes nothing.
func TestAnEventForAnotherRoomIsIgnored(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	for _, room := range []string{f.callID.String() + "-1", "not-a-call"} {
		f.events.Handle(t.Context(), &livekit.WebhookEvent{Event: webhook.EventRoomFinished, Id: "EV_" + room,
			Room: &livekit.Room{Name: room}})
		f.events.Handle(t.Context(), &livekit.WebhookEvent{Event: webhook.EventParticipantLeft, Id: "EV_l" + room,
			Room: &livekit.Room{Name: room}, Participant: &livekit.ParticipantInfo{Identity: f.dev.String()}})
	}
	if row, _ := f.e.Repo.GetVoiceSession(t.Context(), f.callID); row.Ended != nil {
		t.Fatal("an event for another room ended the call")
	}
	if len(f.ds.removeDevices()) != 0 || len(f.stub.deletedRooms()) != 0 {
		t.Fatal("an event for another room reached the delivery service or the SFU")
	}
}

// F11: a published track that is not dilla-sframe/1 (Encryption NONE) or not of its declared kind
// demotes the device to listen-only; a repeat removes it; a rejoin stays demoted.
func TestANonDillaTrackDemotesThenRemoves(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	f.send(t, webhook.EventParticipantJoined, f.dev.String(), nil)
	f.send(t, webhook.EventTrackPublished, f.dev.String(), &livekit.TrackInfo{Sid: "TR_none",
		Type: livekit.TrackType_AUDIO, Source: livekit.TrackSource_MICROPHONE, Encryption: livekit.Encryption_NONE})
	u := f.stub.permUpdates()
	if last := u[len(u)-1]; last.Identity != f.dev.String() || last.Perm.GetCanPublish() || !last.Perm.GetCanSubscribe() {
		t.Fatalf("after a NONE track the last push = %+v, want a complete listen-only permission", last)
	}
	if len(deviceRemovals(f.stub)) != 0 {
		t.Fatal("a first violation removed the device")
	}
	f.send(t, webhook.EventTrackPublished, f.dev.String(), &livekit.TrackInfo{Sid: "TR_mislabelled",
		Type: livekit.TrackType_VIDEO, Source: livekit.TrackSource_MICROPHONE, Encryption: livekit.Encryption_CUSTOM})
	if got := deviceRemovals(f.stub); len(got) != 1 || got[0] != [2]string{f.room, f.dev.String()} {
		t.Fatalf("removals after a repeat = %v", got)
	}
	f.send(t, webhook.EventParticipantLeft, f.dev.String(), nil)
	f.send(t, webhook.EventParticipantJoined, f.dev.String(), nil)
	u = f.stub.permUpdates()
	if last := u[len(u)-1]; last.Perm.GetCanPublish() {
		t.Fatalf("a rejoin after two violations was re-granted %+v", last.Perm)
	}
}

// F11 with task 10's reconciliation: the demotion is per-call state every path honours. The room
// sweep and a grant sync push it again rather than the device's entitlement, a fresh start mints a
// listen-only token, the /rtc gate admits only that, and a share is refused.
func TestTheF11DemotionSurvivesTheSweepTheSyncAStartAndTheGate(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	f.stub.setPresent(f.room, &livekit.ParticipantInfo{Identity: f.dev.String(), Sid: "PA_1"})
	f.sendAs(t, webhook.EventParticipantJoined, f.dev.String(), "PA_1", nil)
	f.sendAs(t, webhook.EventTrackPublished, f.dev.String(), "PA_1", &livekit.TrackInfo{Sid: "TR_none",
		Type: livekit.TrackType_AUDIO, Source: livekit.TrackSource_MICROPHONE, Encryption: livekit.Encryption_NONE})
	lastPush := func(what string) {
		t.Helper()
		u := f.stub.permUpdates()
		if last := u[len(u)-1]; last.Identity != f.dev.String() || last.Perm.GetCanPublish() {
			t.Fatalf("after %s the last push = %+v, want listen-only", what, last)
		}
	}
	lastPush("the violation")
	f.calls.SweepRooms(t.Context())
	lastPush("the room sweep")
	if err := api.SyncCallGrants(t.Context(), f.e.Repo, api.NewResolver(f.e.Repo), f.stub, f.calls,
		ownerCommunityOf(t, f.e, f.ch), nil, nil); err != nil {
		t.Fatalf("SyncCallGrants: %v", err)
	}
	lastPush("a grant sync")
	perm, err := f.calls.AdmitRoom(t.Context(), f.room, f.dev)
	if err != nil || perm.GetCanPublish() {
		t.Fatalf("the gate = %+v, %v; want admission with a listen-only grant", perm, err)
	}
	if status, _ := f.e.Do(http.MethodPost, "/v1/channels/"+f.ch.String()+"/calls", f.tok, []any{}); status != http.StatusOK {
		t.Fatalf("a rejoin start = %d", status)
	}
	if got := f.stub.lastMint().Perm; got.GetCanPublish() {
		t.Fatalf("the demoted device's new token = %+v, want listen-only", got)
	}
	status, body := f.e.Do(http.MethodPost, "/v1/calls/"+f.callID.String()+"/share", f.tok, []any{})
	if status != http.StatusForbidden || f.e.ErrCode(body) != "E_FORBIDDEN" {
		t.Fatalf("a demoted device's share = %d %s, want 403 E_FORBIDDEN", status, f.e.ErrCode(body))
	}
}

// DEV-60 bits 3 and 4 and F1: an encrypted camera sets the video bit (GCM counts as encrypted like
// CUSTOM: never test == GCM); unpublishing the last video track clears it and frees the slot after
// the demotion.
func TestAnEncryptedCameraSetsTheVideoBitAndStoppingItFreesTheSlot(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{MaxPublishers: 1})
	_, _, memberTok := joinedMember(t, f.e, f.ch, f.group, "member")
	share := "/v1/calls/" + f.callID.String() + "/share"
	if status, _ := f.e.Do(http.MethodPost, share, f.tok, []any{}); status != http.StatusNoContent {
		t.Fatalf("share = %d", status)
	}
	f.send(t, webhook.EventParticipantJoined, f.dev.String(), nil)
	f.send(t, webhook.EventTrackPublished, f.dev.String(), &livekit.TrackInfo{Sid: "TR_cam",
		Type: livekit.TrackType_VIDEO, Source: livekit.TrackSource_CAMERA, Encryption: livekit.Encryption_CUSTOM})
	f.send(t, webhook.EventTrackPublished, f.dev.String(), &livekit.TrackInfo{Sid: "TR_mic",
		Type: livekit.TrackType_AUDIO, Source: livekit.TrackSource_MICROPHONE, Encryption: livekit.Encryption_GCM})
	if fr := f.gw.last(); fr.Flags != api.VoiceInCall|api.VoiceVideo {
		t.Fatalf("flags with a camera = %b, want in_call|video", fr.Flags)
	}
	if len(deviceRemovals(f.stub)) != 0 {
		t.Fatal("a GCM-flagged track was treated as unencrypted")
	}
	f.send(t, webhook.EventTrackUnpublished, f.dev.String(), &livekit.TrackInfo{Sid: "TR_cam",
		Type: livekit.TrackType_VIDEO, Source: livekit.TrackSource_CAMERA, Encryption: livekit.Encryption_CUSTOM})
	if fr := f.gw.last(); fr.Flags != api.VoiceInCall {
		t.Fatalf("flags after the camera stopped = %b, want in_call", fr.Flags)
	}
	u := f.stub.permUpdates()
	if last := u[len(u)-1]; last.Identity != f.dev.String() || hasCamera(last.Perm) {
		t.Fatalf("the last push = %+v, want the owner demoted", last)
	}
	if status, _ := f.e.Do(http.MethodPost, share, memberTok, []any{}); status != http.StatusNoContent {
		t.Fatalf("the member's share after the owner's camera stopped = %d, want 204", status)
	}
}

// dropLeaf takes dev's leaf out of group's member rows, as a committed Remove does.
func dropLeaf(t *testing.T, e *env, group, dev id.ID) {
	t.Helper()
	members, err := e.Repo.ListMembers(t.Context(), group)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	kept := slices.DeleteFunc(members, func(m store.MemberRow) bool { return m.DeviceID == dev })
	if err := e.Repo.ReplaceMembers(t.Context(), group, callGroupEpoch, kept); err != nil {
		t.Fatalf("ReplaceMembers: %v", err)
	}
}

// G29 / SP-22 (c): the evictor takes a device the call group removed out of the room at once; once
// its leaf is gone the /rtc gate refuses its rejoin.
func TestEvictTakesTheDeviceOutOfTheRoom(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	f.send(t, webhook.EventParticipantJoined, f.dev.String(), nil)
	f.events.Evict(t.Context(), id.New(), []id.ID{f.dev}) // a group that is not the call's
	if len(deviceRemovals(f.stub)) != 0 {
		t.Fatal("an eviction from another group touched the room")
	}
	f.events.Evict(t.Context(), f.group, []id.ID{f.dev})
	f.calls.ProcessQueueForTest(t.Context()) // the eviction is queued to the retry loop (CALLS-6)
	f.events.FlushAnnouncements()
	if got := deviceRemovals(f.stub); len(got) != 1 || got[0] != [2]string{f.room, f.dev.String()} {
		t.Fatalf("removals = %v", got)
	}
	if fr := f.gw.last(); fr.Device != f.dev || fr.Flags != 0 {
		t.Fatalf("the last voice_state = %+v, want flags 0", fr)
	}
	dropLeaf(t, f.e, f.group, f.dev)
	_, err := f.calls.AdmitRoom(t.Context(), f.room, f.dev)
	var se *server.Error
	if !errors.As(err, &se) || se.Code != server.CodeLeafNotCurrent {
		t.Fatalf("AdmitRoom after the eviction = %v; want E_LEAF_NOT_CURRENT: the gate must refuse the rejoin", err)
	}
}

// An eviction the SFU refuses is a pending cut: the gate refuses the device meanwhile and the retry
// loop's pass removes it, because a device that is no current leaf of the call group is cut by the
// same reconciliation the sweep runs.
func TestAnEvictionTheSFURefusesIsRetriedUntilItLands(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	f.send(t, webhook.EventParticipantJoined, f.dev.String(), nil)
	f.stub.mu.Lock()
	f.stub.failRemovals = 1
	f.stub.mu.Unlock()
	dropLeaf(t, f.e, f.group, f.dev)
	f.events.Evict(t.Context(), f.group, []id.ID{f.dev})
	f.calls.ProcessQueueForTest(t.Context()) // the eviction is queued to the retry loop (CALLS-6)
	if f.calls.PendingRepairs() != 1 {
		t.Fatalf("pending repairs = %d, want the refused eviction", f.calls.PendingRepairs())
	}
	f.calls.RetryPending(t.Context())
	if got := deviceRemovals(f.stub); len(got) != 2 {
		t.Fatalf("removals = %v, want the refused one and its retry", got)
	}
	if f.calls.PendingRepairs() != 0 {
		t.Fatalf("pending repairs after the retry = %d", f.calls.PendingRepairs())
	}
}

// A participant that is no current leaf of the call group — a Remove committed while the evictor's
// call was lost — is cut by the room sweep like a device that lost access.
func TestTheRoomSweepCutsADeviceThatIsNoLongerALeaf(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	_, memberDev, _ := joinedMember(t, f.e, f.ch, f.group, "member")
	f.stub.setPresent(f.room, &livekit.ParticipantInfo{Identity: f.dev.String()},
		&livekit.ParticipantInfo{Identity: memberDev.String()})
	dropLeaf(t, f.e, f.group, memberDev)
	f.calls.SweepRooms(t.Context())
	if !removedDevice(f.stub, f.room, memberDev) {
		t.Fatal("the sweep did not cut the device whose leaf is gone")
	}
	if removedDevice(f.stub, f.room, f.dev) {
		t.Fatal("the sweep cut the owner, a current leaf")
	}
}

func publishOpus(t *testing.T, room *lksdk.Room, enc livekit.Encryption_Type) *lksdk.LocalTrackPublication {
	t.Helper()
	track, err := lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2})
	if err != nil {
		t.Fatalf("track: %v", err)
	}
	pub, err := room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Name: "mic", Source: livekit.TrackSource_MICROPHONE, Encryption: enc,
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				_ = track.WriteSample(media.Sample{Data: []byte{0xf8, 0xff, 0xfe}, Duration: 20 * time.Millisecond}, nil)
			}
		}
	}()
	return pub
}

// SP-14 against the real SFU: a Go publisher with Encryption NONE is demoted (its track is
// unpublished and its own permission says canPublish false); a CUSTOM publisher is left alone.
func TestANoneFlaggedGoPublisherIsDemotedAndACustomOneIsLeftAlone(t *testing.T) {
	e, cid, tok := channelEnv(t)
	ch, _, _ := newChannel(t, e, cid, tok, 1 /* voice */, 1, 2, "voice")
	group := seedCallGroup(t, e, ch, cid, callGroupEpoch)
	owner := userOf(t, e, tok)
	seedLeaf(t, e, group, deviceOf(t, e, tok), 3, nil)
	phone := seedDevices(t, e, owner, 1)[0]
	e.sess["phone"] = auth.Session{UserID: owner, DeviceID: phone, Scope: auth.ScopeEnrolled}
	seedLeaf(t, e, group, phone, 3, nil)

	log := slog.New(slog.DiscardHandler)
	var events atomic.Pointer[api.CallEvents]
	var mu sync.Mutex
	var seen []*livekit.WebhookEvent
	c := sfu.DefaultConfig()
	c.APISecret = "dilla-call-events-secret-0123456789"
	c.Port, c.UDPPort = sfutest.FreePorts(t) // outside internal/sfu, never a fixed port (task 7's sfutest)
	hook := sfu.NewWebhookHandler(c.APIKey, c.APISecret, func(ctx context.Context, ev *livekit.WebhookEvent) {
		mu.Lock()
		seen = append(seen, ev)
		mu.Unlock()
		if ce := events.Load(); ce != nil {
			ce.Handle(ctx, ev)
		}
	}, log)
	defer hook.(io.Closer).Close()
	mux := http.NewServeMux()
	mux.Handle("POST "+sfu.WebhookPath, hook)
	ts := httptest.NewServer(mux)
	defer ts.Close()
	c.WebhookURL = ts.URL + sfu.WebhookPath
	srv, err := sfu.Start(t.Context(), c)
	if err != nil {
		t.Fatalf("sfu.Start: %v", err)
	}
	defer func() { _ = srv.Stop(context.WithoutCancel(t.Context())) }()
	calls := api.NewCalls(e.Repo, api.NewResolver(e.Repo), srv, api.CallsConfig{LiveKitURL: srv.URL()}, e.Clk, log)
	calls.Register(e.Mux)
	events.Store(api.NewCallEvents(e.Repo, api.NewResolver(e.Repo), &callDS{repo: e.Repo}, srv, calls, &voiceGW{}, e.Clk, log))

	path := "/v1/channels/" + ch.String() + "/calls"
	_, b1 := e.Do(http.MethodPost, path, tok, []any{})
	_, b2 := e.Do(http.MethodPost, path, "phone", []any{})
	alice, err := lksdk.ConnectToRoomWithToken(srv.URL(), decodeCall(t, b1).Token, &lksdk.RoomCallback{})
	if err != nil {
		t.Fatalf("alice join: %v", err)
	}
	defer alice.Disconnect()
	carol, err := lksdk.ConnectToRoomWithToken(srv.URL(), decodeCall(t, b2).Token, &lksdk.RoomCallback{})
	if err != nil {
		t.Fatalf("carol join: %v", err)
	}
	defer carol.Disconnect()
	nonePub := publishOpus(t, alice, livekit.Encryption_NONE)
	customPub := publishOpus(t, carol, livekit.Encryption_CUSTOM)

	unpublished := func(sid string) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, ev := range seen {
			if ev.GetEvent() == webhook.EventTrackUnpublished && ev.GetTrack().GetSid() == sid {
				return true
			}
		}
		return false
	}
	deadline := time.Now().Add(15 * time.Second)
	for !unpublished(nonePub.SID()) {
		if time.Now().After(deadline) {
			t.Fatal("the NONE-flagged track was never unpublished")
		}
		time.Sleep(50 * time.Millisecond)
	}
	deadline = time.Now().Add(5 * time.Second)
	for alice.LocalParticipant.Permissions().GetCanPublish() {
		if time.Now().After(deadline) {
			t.Fatal("the NONE publisher still holds canPublish")
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(3 * time.Second)
	if unpublished(customPub.SID()) || !carol.LocalParticipant.Permissions().GetCanPublish() {
		t.Fatal("the CUSTOM publisher was touched")
	}
}
