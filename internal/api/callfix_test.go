package api_test

// The server-half fix wave, lens calls: CALLS-1…6, the parked item (queued grant syncs), DS-7,
// CALLS-M1…M4.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// errSFUDown is a stub SFU refusal.
var errSFUDown = errors.New("stub SFU: down")

// waitFor polls cond every 5 ms for up to d.
func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return true
}

// callEnded reports whether call's voice session is ended.
func callEnded(t *testing.T, e *env, call id.ID) bool {
	t.Helper()
	row, err := e.Repo.GetVoiceSession(t.Context(), call)
	if err != nil {
		t.Fatalf("GetVoiceSession: %v", err)
	}
	return row.Ended != nil
}

// roomHeld reports whether the stub SFU still holds room.
func roomHeld(stub *stubSFU, room string) bool {
	return slices.Contains(stub.heldRooms(), room)
}

// callIDOfRoom is the call id a room name carries.
func callIDOfRoom(t *testing.T, room string) id.ID {
	t.Helper()
	call, err := id.Parse(room[:32])
	if err != nil {
		t.Fatalf("room %q: %v", room, err)
	}
	return call
}

// CALLS-1: deleting a channel ends its live call — the voice session ended at once (every gate
// refuses its room from then on), the room deleted and its pending state dropped right after on the
// retry loop — and deleting a community does the same for every channel of it.
func TestDeletingAChannelOrACommunityEndsItsLiveCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		del  func(m *membershipCallEnv) (int, []byte)
	}{
		{"channel", func(m *membershipCallEnv) (int, []byte) {
			return m.e.Do(http.MethodDelete, "/v1/channels/"+m.ch.String(), m.ownerTok, nil)
		}},
		{"community", func(m *membershipCallEnv) (int, []byte) {
			return m.e.Do(http.MethodDelete, "/v1/communities/"+m.cid.String(), m.ownerTok, nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMembershipCallEnv(t)
			call := callIDOfRoom(t, m.room)
			m.calls.MarkPending(call, m.memberDev, m.room)
			if status, body := tc.del(m); status != http.StatusNoContent {
				t.Fatalf("DELETE %s = %d (%x)", tc.name, status, body)
			}
			if !callEnded(t, m.e, call) {
				t.Fatalf("the %s delete left its call live", tc.name)
			}
			if _, code, err := admitted(t, m.calls, m.room, deviceOf(t, m.e, m.ownerTok)); code == "" || err != nil {
				t.Fatalf("the gate after the %s delete = %q %v, want a refusal", tc.name, code, err)
			}
			if !waitFor(2*time.Second, func() bool { return !roomHeld(m.stub, m.room) }) {
				t.Fatalf("the %s delete left the call's room open: %v", tc.name, m.stub.heldRooms())
			}
			if !waitFor(2*time.Second, func() bool { return m.calls.PendingRepairs() == 0 }) {
				t.Fatalf("the %s delete left %d pending repairs", tc.name, m.calls.PendingRepairs())
			}
		})
	}
}

// CALLS-1: the delete's own teardown can fail half way (the SFU refuses the room's close). A cut
// after the delete, and the room sweep, still reach the room: a room of an ended call is deleted by
// both, never skipped.
func TestACutOrTheSweepAfterAChannelDeleteStillClosesTheRoom(t *testing.T) {
	for _, via := range []string{"cut", "sweep"} {
		t.Run(via, func(t *testing.T) {
			m := newMembershipCallEnv(t)
			m.stub.mu.Lock()
			m.stub.deleteFail = errSFUDown
			m.stub.mu.Unlock()
			if status, _ := m.e.Do(http.MethodDelete, "/v1/channels/"+m.ch.String(), m.ownerTok, nil); status != http.StatusNoContent {
				t.Fatalf("DELETE channel = %d", status)
			}
			if !waitFor(2*time.Second, func() bool { return slices.Contains(m.stub.deletedRooms(), m.room) }) {
				t.Fatal("the delete's teardown never tried to close the room")
			}
			if !roomHeld(m.stub, m.room) {
				t.Fatal("the failing SFU closed the room anyway")
			}
			m.stub.mu.Lock()
			m.stub.deleteFail = nil
			m.stub.mu.Unlock()
			if via == "cut" {
				if err := m.e.Repo.RevokeDevice(t.Context(), m.memberDev, m.e.Clk.Now().Unix()); err != nil {
					t.Fatalf("RevokeDevice: %v", err)
				}
				m.calls.CutDevice(t.Context(), m.memberDev)
			} else {
				m.calls.SweepRooms(t.Context())
			}
			if !waitFor(2*time.Second, func() bool { return !roomHeld(m.stub, m.room) }) {
				t.Fatalf("the %s after the delete left the room open: %v", via, m.stub.heldRooms())
			}
		})
	}
}

// CALLS-1: a live call whose channel is gone without the delete's end — a crash between the commit
// and the end — is ended by the room sweep, which deletes its room, never skipped as an error.
func TestTheSweepEndsALiveCallWhoseChannelIsGone(t *testing.T) {
	m := newMembershipCallEnv(t)
	call := callIDOfRoom(t, m.room)
	if err := m.e.Repo.DeleteChannel(t.Context(), m.ch, m.e.Clk.Now().Unix()); err != nil {
		t.Fatalf("DeleteChannel: %v", err)
	}
	m.calls.SweepRooms(t.Context())
	if !callEnded(t, m.e, call) {
		t.Fatal("the sweep left a call whose channel is gone live")
	}
	if roomHeld(m.stub, m.room) {
		t.Fatal("the sweep left the room of a call whose channel is gone open")
	}
}

// CALLS-1: a grant sync scoped to a channel that is gone ends its call rather than failing on it.
func TestASyncOfAChannelThatIsGoneEndsItsCall(t *testing.T) {
	m := newMembershipCallEnv(t)
	call := callIDOfRoom(t, m.room)
	if err := m.e.Repo.DeleteChannel(t.Context(), m.ch, m.e.Clk.Now().Unix()); err != nil {
		t.Fatalf("DeleteChannel: %v", err)
	}
	if err := api.SyncCallGrants(t.Context(), m.e.Repo, api.NewResolver(m.e.Repo), m.stub, m.calls, m.cid, nil, &m.ch); err != nil {
		t.Fatalf("SyncCallGrants over a deleted channel: %v", err)
	}
	if !callEnded(t, m.e, call) {
		t.Fatal("a sync of a deleted channel left its call live")
	}
	if !waitFor(2*time.Second, func() bool { return !roomHeld(m.stub, m.room) }) {
		t.Fatal("a sync of a deleted channel left its room open")
	}
}

// CALLS-2 (a): a closed call group holds no leaf, for the /rtc gate and for the room's reconcile;
// the sweep ends a live call on a closed group.
func TestAClosedCallGroupHoldsNoLeaf(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	if _, code, err := admitted(t, f.calls, f.room, f.dev); code != "" || err != nil {
		t.Fatalf("before the close the gate = %q %v", code, err)
	}
	if err := f.e.Repo.CloseGroup(t.Context(), f.group, f.e.Clk.Now().Unix()); err != nil {
		t.Fatalf("CloseGroup: %v", err)
	}
	if perm, code, err := admitted(t, f.calls, f.room, f.dev); perm != nil || code != "E_LEAF_NOT_CURRENT" || err != nil {
		t.Fatalf("the gate on a closed group = %v %q %v, want E_LEAF_NOT_CURRENT", perm, code, err)
	}
	f.stub.setPresent(f.room, &livekit.ParticipantInfo{Identity: f.dev.String()})
	f.calls.SweepRooms(t.Context())
	if !callEnded(t, f.e, f.callID) || roomHeld(f.stub, f.room) {
		t.Fatal("the sweep left a live call on a closed group running")
	}
}

// CALLS-2 (b): a start that read the call group open, parked, and writes the row after a DELETE
// ended the call and closed its group reopens the row on a closed group. The end closes the group
// before it ends the row, so the start's re-read finds it closed: the call it reopened is ended at
// once and the start answers 503 E_UNAVAILABLE with no token minted.
func TestAStartThatReopensACallOnAGroupAnEndClosedIsEnded(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	parked, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.calls.SetStartHookForTest(func(stage string) {
		if stage == "put" {
			once.Do(func() {
				close(parked)
				<-resume
			})
		}
	})
	type answer struct {
		status int
		body   []byte
	}
	done := make(chan answer, 1)
	go func() {
		status, body := f.e.Do(http.MethodPost, "/v1/channels/"+f.ch.String()+"/calls", f.tok, []any{})
		done <- answer{status, body}
	}()
	select {
	case <-parked:
	case <-time.After(5 * time.Second):
		t.Fatal("the start never reached its write")
	}
	if status, _ := f.e.Do(http.MethodDelete, "/v1/calls/"+f.callID.String(), f.tok, nil); status != http.StatusNoContent {
		t.Fatalf("DELETE = %d", status)
	}
	minted := len(f.stub.minted())
	close(resume)
	a := <-done
	if code := codeOfBody(f.e, a.status, a.body); a.status != http.StatusServiceUnavailable || code != "E_UNAVAILABLE" {
		t.Fatalf("the start that reopened the call on a closed group = %d %s, want 503 E_UNAVAILABLE",
			a.status, code)
	}
	if len(f.stub.minted()) != minted {
		t.Fatal("a token was minted for a call on a closed group")
	}
	if !callEnded(t, f.e, f.callID) {
		t.Fatal("the call reopened on a closed group was left live")
	}
}

// CALLS-2: an end parked after it closed the group and before it ended the row, with a start in the
// gap that opens the next call on a fresh call group: the end does not end the new call.
func TestAnEndNeverEndsTheCallAStartReopenedInItsGap(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	parked, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.calls.SetEndHookForTest(func(stage string) {
		if stage == "closed" {
			once.Do(func() {
				close(parked)
				<-resume
			})
		}
	})
	done := make(chan int, 1)
	go func() {
		status, _ := f.e.Do(http.MethodDelete, "/v1/calls/"+f.callID.String(), f.tok, nil)
		done <- status
	}()
	var unpark sync.Once
	release := func() { unpark.Do(func() { close(resume) }) }
	t.Cleanup(release) // a failure below must not leave the DELETE parked
	select {
	case <-parked:
	case <-time.After(10 * time.Second):
		t.Fatal("the DELETE never reached the point between closing the group and ending the row")
	}
	f.calls.SetEndHookForTest(nil)
	fresh := seedCallGroup(t, f.e, f.ch, ownerCommunityOf(t, f.e, f.ch), callGroupEpoch)
	seedLeaf(t, f.e, fresh, f.dev, 3, nil)
	type answer struct {
		status int
		body   []byte
	}
	started := make(chan answer, 1)
	go func() {
		status, body := f.e.Do(http.MethodPost, "/v1/channels/"+f.ch.String()+"/calls", f.tok, []any{})
		started <- answer{status, body}
	}()
	var a answer
	select {
	case a = <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the start in the end's gap did not answer within 10 s: it waits on the parked end")
	}
	if a.status != http.StatusCreated {
		t.Fatalf("the start in the end's gap = %d %s", a.status, codeOfBody(f.e, a.status, a.body))
	}
	next := decodeCall(t, a.body)
	release()
	if s := <-done; s != http.StatusNoContent {
		t.Fatalf("DELETE = %d", s)
	}
	row, err := f.e.Repo.GetVoiceSession(t.Context(), f.callID)
	if err != nil || row.Ended != nil || row.GroupID == nil || *row.GroupID != next.GroupID {
		t.Fatalf("the next call after the parked end = %+v (%v), want it live on the fresh group", row, err)
	}
}

// CALLS-3: a room_finished for a room LiveKit reaped, processed after a start re-created the room
// under the same name, names the earlier room sid: the re-created call is left as it is. One whose
// sid is the room the SFU holds now ends the call.
func TestALateRoomFinishedForAReapedRoomLeavesTheReopenedCall(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	f.stub.setSID(f.room, "RM_new")
	finished := func(sid string) {
		f.events.Handle(t.Context(), &livekit.WebhookEvent{Event: webhook.EventRoomFinished, Id: "EV_" + sid,
			Room: &livekit.Room{Name: f.room, Sid: sid}})
	}
	finished("RM_old")
	if callEnded(t, f.e, f.callID) || len(f.stub.deletedRooms()) != 0 || len(f.ds.closedGroups()) != 0 {
		t.Fatal("a room_finished for an earlier incarnation of the room ended the re-created call")
	}
	finished("RM_new")
	if !callEnded(t, f.e, f.callID) {
		t.Fatal("a room_finished for the room the SFU holds did not end the call")
	}
}

// CALLS-4 / CALLS-M1: DELETE /v1/calls/{call_id} refuses a barred device (403 E_FORBIDDEN), as start,
// share and the gate do, and a device penalised in the call's room for media that is not dilla's;
// the call goes on.
func TestABarredOrPenalisedDeviceCannotEndTheCall(t *testing.T) {
	for _, st := range barredStates() {
		t.Run(st.name, func(t *testing.T) {
			b := newBarredEnv(t)
			st.bar(t, b.e, b.member, b.memberDev)
			status, body := b.e.Do(http.MethodDelete, "/v1/calls/"+b.callID.String(), b.memberTok, nil)
			if status != http.StatusForbidden || codeOfBody(b.e, status, body) != "E_FORBIDDEN" {
				t.Fatalf("a DELETE from a %s device = %d %s, want 403 E_FORBIDDEN", st.name, status, codeOfBody(b.e, status, body))
			}
			if callEnded(t, b.e, b.callID) || len(b.stub.deletedRooms()) != 0 {
				t.Fatalf("a %s device ended the call", st.name)
			}
		})
	}
	t.Run("penalised", func(t *testing.T) {
		f := newEventsFixture(t, api.CallsConfig{})
		f.send(t, webhook.EventParticipantJoined, f.dev.String(), nil)
		f.send(t, webhook.EventTrackPublished, f.dev.String(), &livekit.TrackInfo{Sid: "TR_none",
			Type: livekit.TrackType_AUDIO, Source: livekit.TrackSource_MICROPHONE, Encryption: livekit.Encryption_NONE})
		status, body := f.e.Do(http.MethodDelete, "/v1/calls/"+f.callID.String(), f.tok, nil)
		if status != http.StatusForbidden || f.e.ErrCode(body) != "E_FORBIDDEN" {
			t.Fatalf("a DELETE from a penalised device = %d %s, want 403 E_FORBIDDEN", status, f.e.ErrCode(body))
		}
		if callEnded(t, f.e, f.callID) {
			t.Fatal("a penalised device ended the call (and with it its penalty)")
		}
	})
}

// CALLS-5: a sharer that left without the webhook's participant_left reaching the instance (lost,
// shed) keeps its slot no longer than the room sweep: the sweep frees the slot of a holder the room
// no longer lists, within its period, and the next device can share.
func TestTheSweepFreesTheSlotOfADeviceThatLeftWithoutAWebhook(t *testing.T) {
	l := newLeaseEnv(t, 1, 2)
	if status := l.share(0); status != http.StatusNoContent {
		t.Fatalf("dev0 share = %d", status)
	}
	// dev0 is gone from the room; its participant_left never arrives.
	l.stub.setPresent(l.room, &livekit.ParticipantInfo{Identity: l.devs[1].String()})
	l.calls.SetSweepEvery(20 * time.Millisecond)
	stop := l.calls.StartRetries(20 * time.Millisecond)
	defer stop()
	if !waitFor(2*time.Second, func() bool { return len(l.calls.SharersOf(l.callID)) == 0 }) {
		t.Fatalf("sharers after the sweep = %v, want the departed device's slot freed", l.calls.SharersOf(l.callID))
	}
	if status := l.share(1); status != http.StatusNoContent {
		t.Fatalf("dev1 share after the sweep = %d, want 204", status)
	}
}

// countingRepo counts the per-member role reads of an audience resolution (EligibleUsers).
type countingRepo struct {
	store.Repository
	memberRoles atomic.Int64
}

func (r *countingRepo) ListMemberRoles(ctx context.Context, communityID, userID id.ID) ([]id.ID, error) {
	r.memberRoles.Add(1)
	return r.Repository.ListMemberRoles(ctx, communityID, userID)
}

// CALLS-5: a member who publishes and unpublishes a microphone in a loop costs no audience resolution
// per event: voice_state is coalesced per device and the audience resolved once per delivery, off the
// webhook's goroutine; the last state still reaches everyone.
func TestATrackFloodDoesNotResolveTheAudiencePerEvent(t *testing.T) {
	e, ch, tok, group, stub, calls := callEnvCalls(t, api.CallsConfig{LiveKitURL: testLiveKitURL})
	dev := deviceOf(t, e, tok)
	seedLeaf(t, e, group, dev, 3, nil)
	for _, name := range []string{"m1", "m2", "m3"} {
		joinedMember(t, e, ch, group, name)
	}
	repo := &countingRepo{Repository: e.Repo}
	gw := &voiceGW{}
	events := api.NewCallEvents(repo, api.NewResolver(e.Repo), &callDS{repo: e.Repo}, stub, calls, gw, e.Clk, slog.New(slog.DiscardHandler))
	if status, _ := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{}); status != http.StatusCreated {
		t.Fatalf("start = %d", status)
	}
	room := stub.minted()[0][0]
	handle := func(name string, track *livekit.TrackInfo) {
		events.Handle(t.Context(), &livekit.WebhookEvent{Event: name, Id: "EV_" + id.New().String(), Room: &livekit.Room{Name: room},
			Participant: &livekit.ParticipantInfo{Identity: dev.String()}, Track: track})
	}
	handle(webhook.EventParticipantJoined, nil)
	const flood = 100
	start := time.Now()
	for i := range flood {
		mic := &livekit.TrackInfo{Sid: "TR_mic", Type: livekit.TrackType_AUDIO, Source: livekit.TrackSource_MICROPHONE,
			Encryption: livekit.Encryption_CUSTOM}
		if i%2 == 0 {
			handle(webhook.EventTrackPublished, mic)
		} else {
			handle(webhook.EventTrackUnpublished, mic)
		}
	}
	took := time.Since(start)
	if f, ok := any(events).(interface{ FlushAnnouncements() }); ok {
		f.FlushAnnouncements()
	}
	members := int64(4) // the owner and three members
	if n := repo.memberRoles.Load(); n > 10*members {
		t.Fatalf("%d events made %d per-member role reads (%s on the webhook's goroutine), want the audience resolved per delivery, not per event",
			flood, n, took)
	}
	if fr := gw.last(); fr.Device != dev || fr.Flags != api.VoiceInCall {
		t.Fatalf("the last voice_state = %+v, want the device's final in_call state", fr)
	}
}

// CALLS-6 / DS-4: the delivery service's evictor returns at once with a hung SFU — it runs on the
// committer's request goroutine — and the eviction still lands, on the retry loop.
func TestEvictReturnsAtOnceWithAHungSFUAndStillEvicts(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	f.send(t, webhook.EventParticipantJoined, f.dev.String(), nil)
	stop := f.calls.StartRetries(20 * time.Millisecond)
	defer stop()
	f.stub.mu.Lock()
	f.stub.hang = true
	f.stub.mu.Unlock()
	start := time.Now()
	f.events.Evict(t.Context(), f.group, []id.ID{f.dev})
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Fatalf("Evict took %s with a hung SFU, want it to return at once", took)
	}
	time.Sleep(50 * time.Millisecond)
	f.stub.mu.Lock()
	f.stub.hang = false
	f.stub.mu.Unlock()
	if !waitFor(10*time.Second, func() bool { return removedDevice(f.stub, f.room, f.dev) }) {
		t.Fatal("the queued eviction never removed the device once the SFU answered")
	}
}

// The parked item: a kick, a ban and an overwrite change queue their call grant sync instead of
// running it on the request — with a hung SFU each answers at once — and the cut still lands once the
// SFU answers.
func TestMembershipAndPermissionChangesNeverWaitOnTheSFU(t *testing.T) {
	for _, tc := range []struct {
		name string
		act  func(m *membershipCallEnv) (int, []byte)
	}{
		{"kick", func(m *membershipCallEnv) (int, []byte) {
			return m.e.Do(http.MethodDelete, "/v1/communities/"+m.cid.String()+"/members/"+m.member.String(), m.ownerTok, nil)
		}},
		{"ban", func(m *membershipCallEnv) (int, []byte) {
			return m.e.Do(http.MethodPut, "/v1/communities/"+m.cid.String()+"/bans/"+m.member.String(), m.ownerTok, []any{"spam", nil})
		}},
		{"overwrite", func(m *membershipCallEnv) (int, []byte) {
			return m.e.Do(http.MethodPut, "/v1/channels/"+m.ch.String()+"/overwrites/1/"+m.member.String(), m.ownerTok,
				[]any{uint64(0), uint64(api.PermConnect)})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMembershipCallEnv(t)
			api.NewRoles(m.e.Repo, m.e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).WithDS(m.e.DS).
				WithCalls(m.stub, m.calls).Register(m.e.Mux)
			m.stub.mu.Lock()
			m.stub.hang = true
			m.stub.mu.Unlock()
			start := time.Now()
			if status, body := tc.act(m); status != http.StatusNoContent {
				t.Fatalf("%s = %d (%x)", tc.name, status, body)
			}
			if took := time.Since(start); took > 500*time.Millisecond {
				t.Fatalf("the %s took %s with a hung SFU, want it to answer without waiting on it", tc.name, took)
			}
			m.stub.mu.Lock()
			m.stub.hang = false
			m.stub.mu.Unlock()
			if !waitFor(15*time.Second, func() bool {
				m.calls.RetryPending(t.Context())
				return removedDevice(m.stub, m.room, m.memberDev)
			}) {
				t.Fatalf("the %s's queued cut never landed", tc.name)
			}
		})
	}
}

// DS-7: a participant_left of an earlier session, processed after the device's leaf was removed and
// the device rejoined the call group — at the same leaf index, in a later epoch — proposes no Remove:
// the leave is bound to the membership its session joined with.
func TestALateLeaveAfterARejoinOfTheGroupProposesNothing(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	f.sendAs(t, webhook.EventParticipantJoined, f.dev.String(), "PA_1", nil)
	// The device's Remove is committed; it rejoins the call group by external join and is re-added
	// at the blank leaf, in a later epoch; it has not reconnected to the room yet.
	dropLeaf(t, f.e, f.group, f.dev)
	seedLeaf(t, f.e, f.group, f.dev, callGroupEpoch, nil)
	f.sendAs(t, webhook.EventParticipantLeft, f.dev.String(), "PA_1", nil)
	if got := f.ds.removeDevices(); len(got) != 0 {
		t.Fatalf("a late leave of an earlier membership proposed %v", got)
	}
}

// CALLS-M4: a cut that does not fit the full work queue is not left to the next periodic sweep: the
// loop sweeps every room at once.
func TestACutThatDoesNotFitTheQueueIsSweptAtOnce(t *testing.T) {
	b := newBarredEnv(t)
	for range 1100 {
		b.calls.CutDevice(t.Context(), id.New())
	}
	if err := b.e.Repo.RevokeDevice(t.Context(), b.memberDev, b.e.Clk.Now().Unix()); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	b.calls.CutDevice(t.Context(), b.memberDev)
	stop := b.calls.StartRetries(time.Hour)
	defer stop()
	if !waitRemoved(b.stub, b.room, b.memberDev) {
		t.Fatal("a cut the full queue could not hold waited for the hourly sweep")
	}
}

// CALLS-M2: a retry pass that meets busy calls serves a cut queued meanwhile between two of them,
// rather than after the pass.
func TestARetryPassServesAQueuedCutBetweenCalls(t *testing.T) {
	b := newBarredEnv(t)
	for range 3 {
		call := id.New()
		b.calls.MarkPending(call, id.New(), call.String()+"-1")
		release, err := b.calls.LockCallForTest(t.Context(), call)
		if err != nil {
			t.Fatalf("LockCallForTest: %v", err)
		}
		defer release()
	}
	if err := b.e.Repo.RevokeDevice(t.Context(), b.memberDev, b.e.Clk.Now().Unix()); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.calls.RetryPending(context.WithoutCancel(t.Context()))
	}()
	time.Sleep(100 * time.Millisecond) // the pass is waiting on the first busy call
	b.calls.CutDevice(t.Context(), b.memberDev)
	cut := waitFor(3500*time.Millisecond, func() bool { return removedDevice(b.stub, b.room, b.memberDev) })
	<-done
	if !cut {
		t.Fatal("a cut queued during a retry pass waited for the whole pass")
	}
}

// CALLS-M3: a device switching from camera to screen unpublishes the camera before it publishes the
// screen. When the SFU already shows the screen track as the camera's unpublish is processed, the
// slot stays and no demotion is pushed.
func TestACameraToScreenSwitchKeepsTheSlot(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{MaxPublishers: 1})
	if status, _ := f.e.Do(http.MethodPost, "/v1/calls/"+f.callID.String()+"/share", f.tok, []any{}); status != http.StatusNoContent {
		t.Fatalf("share = %d", status)
	}
	f.send(t, webhook.EventParticipantJoined, f.dev.String(), nil)
	cam := &livekit.TrackInfo{Sid: "TR_cam", Type: livekit.TrackType_VIDEO, Source: livekit.TrackSource_CAMERA,
		Encryption: livekit.Encryption_CUSTOM}
	f.send(t, webhook.EventTrackPublished, f.dev.String(), cam)
	f.stub.setPresent(f.room, &livekit.ParticipantInfo{Identity: f.dev.String(), Tracks: []*livekit.TrackInfo{{
		Sid: "TR_scr", Type: livekit.TrackType_VIDEO, Source: livekit.TrackSource_SCREEN_SHARE, Encryption: livekit.Encryption_CUSTOM}}})
	pushes := len(f.stub.permUpdates())
	f.send(t, webhook.EventTrackUnpublished, f.dev.String(), cam)
	if got := f.calls.SharersOf(f.callID); len(got) != 1 || got[0] != f.dev {
		t.Fatalf("sharers after the camera's unpublish = %v, want the switching device", got)
	}
	if got := f.stub.permUpdates(); len(got) != pushes {
		t.Fatalf("the switch was demoted: %+v", got[pushes:])
	}
}
