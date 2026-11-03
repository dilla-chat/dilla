package api_test

// web-3 task 3a: GET /v1/channels/{id}/call (L-HTTP-100) and GET /v1/communities/{id}/calls
// (L-HTTP-101), the voice snapshot a client reads before it applies voice_state frames, and the
// self-mute and self-deafen bits the snapshot carries per (call, device).

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/livekit/protocol/webhook"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/id"
)

// snapshotEntry is one participants element: [user_id, device_id, flags].
type snapshotEntry struct {
	User, Device id.ID
	Flags        uint64
}

// callSnapshot is L-HTTP-100's body decoded; Group and Call are nil for CBOR null.
type callSnapshot struct {
	Group, Call  *id.ID
	Participants []snapshotEntry
}

func decodeEntries(t *testing.T, raw cbor.RawMessage) []snapshotEntry {
	t.Helper()
	var rows [][]cbor.RawMessage
	mustUnmarshal(t, raw, &rows)
	if rows == nil {
		t.Fatalf("participants %x is not an array", raw)
	}
	out := make([]snapshotEntry, 0, len(rows))
	for _, r := range rows {
		if len(r) != 3 {
			t.Fatalf("a participant has %d elements, want 3", len(r))
		}
		var e snapshotEntry
		mustUnmarshal(t, r[0], &e.User)
		mustUnmarshal(t, r[1], &e.Device)
		mustUnmarshal(t, r[2], &e.Flags)
		out = append(out, e)
	}
	return out
}

func optionalID(t *testing.T, raw cbor.RawMessage) *id.ID {
	t.Helper()
	if bytes.Equal(raw, []byte{0xf6}) {
		return nil
	}
	var v id.ID
	mustUnmarshal(t, raw, &v)
	return &v
}

// getChannelCall is GET /v1/channels/{id}/call as tok: the status, the raw body and, on 200, the
// decoded snapshot.
func getChannelCall(t *testing.T, e *env, ch id.ID, tok string) (int, []byte, callSnapshot) {
	t.Helper()
	status, body := e.Do(http.MethodGet, "/v1/channels/"+ch.String()+"/call", tok, nil)
	if status != http.StatusOK {
		return status, body, callSnapshot{}
	}
	var out []cbor.RawMessage
	mustUnmarshalBody(t, body, &out)
	if len(out) != 3 {
		t.Fatalf("the call snapshot has %d elements, want 3: %x", len(out), body)
	}
	return status, body, callSnapshot{Group: optionalID(t, out[0]), Call: optionalID(t, out[1]),
		Participants: decodeEntries(t, out[2])}
}

func TestChannelCallNamesTheGroupAndWhoIsIn(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	_, _, bobTok := joinedMember(t, f.e, f.ch, f.group, "bob")
	f.send(t, webhook.EventParticipantJoined, f.dev.String(), nil)
	// voice_self flags 1: its bit 0 is self-mute, which the instance maps to voice_state bit 1.
	f.events.SetSelf(f.callID, f.dev, 1)

	status, body, snap := getChannelCall(t, f.e, f.ch, bobTok)
	if status != http.StatusOK {
		t.Fatalf("GET call as bob = %d %s", status, f.e.ErrCode(body))
	}
	if snap.Group == nil || *snap.Group != f.group || snap.Call == nil || *snap.Call != f.callID {
		t.Fatalf("snapshot names group %v call %v, want %s and %s", snap.Group, snap.Call, f.group, f.callID)
	}
	want := []snapshotEntry{{User: f.owner, Device: f.dev, Flags: api.VoiceInCall | api.VoiceSelfMute}}
	if len(snap.Participants) != 1 || snap.Participants[0] != want[0] {
		t.Fatalf("participants = %+v, want %+v", snap.Participants, want)
	}
}

func TestChannelCallBeforeAnyGroupIsAllNull(t *testing.T) {
	e, ch, tok, _, _, _ := callEnvCalls(t, api.CallsConfig{LiveKitURL: testLiveKitURL})
	fresh, _, st := newChannel(t, e, ownerCommunityOf(t, e, ch), tok, 1, 1, 2, "fresh")
	if st != http.StatusCreated {
		t.Fatalf("voice channel = %d", st)
	}
	status, body, _ := getChannelCall(t, e, fresh, tok)
	if status != http.StatusOK {
		t.Fatalf("GET call = %d %s", status, e.ErrCode(body))
	}
	// [null, null, []]
	if want := []byte{0x83, 0xf6, 0xf6, 0x80}; !bytes.Equal(body, want) {
		t.Fatalf("GET call of a fresh channel = %x, want %x", body, want)
	}
}

// Attacker: a member without view_channel probing a hidden channel gets the unknown-channel answer.
// It learns neither that the channel exists nor whether a call is live in it.
func TestChannelCallIsNotFoundWithoutViewChannel(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	mallory, _, malTok := joinedMember(t, f.e, f.ch, f.group, "mallory")
	denyInChannel(t, f.e, f.ch, mallory, api.PermViewChannel)

	status, hidden, _ := getChannelCall(t, f.e, f.ch, malTok)
	if status != http.StatusNotFound || f.e.ErrCode(hidden) != "E_NOT_FOUND" {
		t.Fatalf("GET call of a hidden channel = %d %x, want 404 E_NOT_FOUND", status, hidden)
	}
	status, unknown, _ := getChannelCall(t, f.e, id.New(), malTok)
	if status != http.StatusNotFound {
		t.Fatalf("GET call of a random id = %d", status)
	}
	if !bytes.Equal(hidden, unknown) {
		t.Fatalf("the hidden channel's answer %x differs from an unknown channel's %x", hidden, unknown)
	}
}

func TestChannelCallRefusesATextChannel(t *testing.T) {
	e, ch, tok, _, _, _ := callEnvCalls(t, api.CallsConfig{LiveKitURL: testLiveKitURL})
	cid := ownerCommunityOf(t, e, ch)
	text, _, st := newChannel(t, e, cid, tok, 0, 1, 0, "general")
	if st != http.StatusCreated {
		t.Fatalf("text channel = %d", st)
	}
	cat, _, st := newChannel(t, e, cid, tok, 2, 1, 0, "cat")
	if st != http.StatusCreated {
		t.Fatalf("category = %d", st)
	}
	for name, target := range map[string]id.ID{"kind 0": text, "kind 2": cat} {
		status, body, _ := getChannelCall(t, e, target, tok)
		if status != http.StatusBadRequest || e.ErrCode(body) != "E_INVALID_REQUEST" {
			t.Fatalf("GET call of a %s channel = %d %x, want 400 E_INVALID_REQUEST", name, status, body)
		}
	}
}

func TestChannelCallWithoutConnectStillReads(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	bob, _, bobTok := joinedMember(t, f.e, f.ch, f.group, "bob")
	denyInChannel(t, f.e, f.ch, bob, api.PermConnect)
	status, body, snap := getChannelCall(t, f.e, f.ch, bobTok)
	if status != http.StatusOK {
		t.Fatalf("GET call without connect = %d %s, want 200", status, f.e.ErrCode(body))
	}
	if snap.Call == nil || *snap.Call != f.callID {
		t.Fatalf("snapshot call = %v, want %s", snap.Call, f.callID)
	}
}

func TestCommunityCallsListsOnlyViewableLiveCalls(t *testing.T) {
	e, ch1, tok, group1, _, _ := callEnvCalls(t, api.CallsConfig{LiveKitURL: testLiveKitURL})
	cid := ownerCommunityOf(t, e, ch1)
	ch2, _, st := newChannel(t, e, cid, tok, 1, 1, 2, "voice-2")
	if st != http.StatusCreated {
		t.Fatalf("voice-2 = %d", st)
	}
	ch3, _, st := newChannel(t, e, cid, tok, 1, 1, 2, "voice-3")
	if st != http.StatusCreated {
		t.Fatalf("voice-3 = %d", st)
	}
	group2 := seedCallGroup(t, e, ch2, cid, callGroupEpoch)
	seedCallGroup(t, e, ch3, cid, callGroupEpoch) // a group, no call
	dev := deviceOf(t, e, tok)
	seedLeaf(t, e, group1, dev, 3, nil)
	seedLeaf(t, e, group2, dev, 3, nil)
	var call1 id.ID
	for _, ch := range []id.ID{ch1, ch2} {
		status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
		if status != http.StatusCreated {
			t.Fatalf("start in %s = %d %s", ch, status, e.ErrCode(body))
		}
		if ch == ch1 {
			call1 = decodeCall(t, body).CallID
		}
	}
	bob, bobTok := e.NewUser("bob")
	joinCommunity(t, e, cid, bobTok)
	denyInChannel(t, e, ch2, bob, api.PermViewChannel)

	status, body := e.Do(http.MethodGet, "/v1/communities/"+cid.String()+"/calls", bobTok, nil)
	if status != http.StatusOK {
		t.Fatalf("GET calls as bob = %d %s", status, e.ErrCode(body))
	}
	var rows [][]cbor.RawMessage
	mustUnmarshalBody(t, body, &rows)
	if len(rows) != 1 || len(rows[0]) != 3 {
		t.Fatalf("bob's live calls = %x, want exactly one row of three", body)
	}
	var gotCh, gotCall id.ID
	mustUnmarshal(t, rows[0][0], &gotCh)
	mustUnmarshal(t, rows[0][1], &gotCall)
	if gotCh != ch1 || gotCall != call1 || len(decodeEntries(t, rows[0][2])) != 0 {
		t.Fatalf("bob's row = %s %s %x, want %s %s []", gotCh, gotCall, rows[0][2], ch1, call1)
	}

	_, carolTok := e.NewUser("carol")
	status, body = e.Do(http.MethodGet, "/v1/communities/"+cid.String()+"/calls", carolTok, nil)
	if status != http.StatusNotFound || e.ErrCode(body) != "E_NOT_FOUND" {
		t.Fatalf("GET calls as a non-member = %d %x, want 404", status, body)
	}
}

func TestSelfFlagsClearOnLeave(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	f.send(t, webhook.EventParticipantJoined, f.dev.String(), nil)
	f.events.SetSelf(f.callID, f.dev, 3)
	if _, _, snap := getChannelCall(t, f.e, f.ch, f.tok); len(snap.Participants) != 1 || snap.Participants[0].Flags != 7 {
		t.Fatalf("participants after voice_self 3 = %+v, want flags 7", snap.Participants)
	}

	f.send(t, webhook.EventParticipantLeft, f.dev.String(), nil)
	if _, _, snap := getChannelCall(t, f.e, f.ch, f.tok); len(snap.Participants) != 0 {
		t.Fatalf("participants after the leave = %+v, want none", snap.Participants)
	}
	// Back in the room: the flags of the earlier session are gone with its leave.
	f.send(t, webhook.EventParticipantJoined, f.dev.String(), nil)
	if _, _, snap := getChannelCall(t, f.e, f.ch, f.tok); len(snap.Participants) != 1 || snap.Participants[0].Flags != api.VoiceInCall {
		t.Fatalf("participants after a rejoin = %+v, want flags 1", snap.Participants)
	}

	// The call ends with the flags still set: a restart of the call (the same call id, R9) starts clean.
	f.events.SetSelf(f.callID, f.dev, 1)
	if status, body := f.e.Do(http.MethodDelete, "/v1/calls/"+f.callID.String(), f.tok, nil); status != http.StatusNoContent {
		t.Fatalf("DELETE call = %d %s", status, f.e.ErrCode(body))
	}
	group := seedCallGroup(t, f.e, f.ch, ownerCommunityOf(t, f.e, f.ch), callGroupEpoch)
	seedLeaf(t, f.e, group, f.dev, 3, nil)
	status, body := f.e.Do(http.MethodPost, "/v1/channels/"+f.ch.String()+"/calls", f.tok, []any{})
	if status != http.StatusCreated {
		t.Fatalf("restart = %d %s", status, f.e.ErrCode(body))
	}
	if decodeCall(t, body).CallID != f.callID {
		t.Fatal("the restarted call has another id")
	}
	f.room = f.stub.lastMint().Room
	f.send(t, webhook.EventParticipantJoined, f.dev.String(), nil)
	if _, _, snap := getChannelCall(t, f.e, f.ch, f.tok); len(snap.Participants) != 1 || snap.Participants[0].Flags != api.VoiceInCall {
		t.Fatalf("participants of the restarted call = %+v, want flags 1 (the ended call's self flags cleared)", snap.Participants)
	}
}

// Review M3: a voice_self the gateway admitted just before the device's leave was processed must not
// come back as a stale mute when the device rejoins: the store keeps self flags only for a device
// the call events hold in the call's room.
func TestSelfFlagsAfterALeaveAreNotStored(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	f.send(t, webhook.EventParticipantJoined, f.dev.String(), nil)
	f.send(t, webhook.EventParticipantLeft, f.dev.String(), nil)
	f.events.SetSelf(f.callID, f.dev, 1) // admitted by the SFU check before the leave landed
	f.send(t, webhook.EventParticipantJoined, f.dev.String(), nil)
	if _, _, snap := getChannelCall(t, f.e, f.ch, f.tok); len(snap.Participants) != 1 || snap.Participants[0].Flags != api.VoiceInCall {
		t.Fatalf("participants after a rejoin = %+v, want flags 1 (no stale self-mute)", snap.Participants)
	}
}
