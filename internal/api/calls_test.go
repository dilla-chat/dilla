package api_test

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/livekit/protocol/livekit"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// callResponse is POST /v1/channels/{id}/calls's six elements, and the seventh, rung, that a start
// opening a DM or group-DM call carries (WB product walk I6; HasRung is false without it).
type callResponse struct {
	CallID     id.ID
	GroupID    id.ID
	LiveKitURL string
	Token      string
	ICE        [][]cbor.RawMessage
	Caps       []uint64
	Rung       []id.ID
	HasRung    bool
}

func decodeCall(t *testing.T, body []byte) callResponse {
	t.Helper()
	var out []cbor.RawMessage
	mustUnmarshalBody(t, body, &out)
	if len(out) != 6 && len(out) != 7 {
		t.Fatalf("call response has %d elements, want 6 or 7: %x", len(out), body)
	}
	var r callResponse
	mustUnmarshal(t, out[0], &r.CallID)
	mustUnmarshal(t, out[1], &r.GroupID)
	mustUnmarshal(t, out[2], &r.LiveKitURL)
	mustUnmarshal(t, out[3], &r.Token)
	mustUnmarshal(t, out[4], &r.ICE)
	mustUnmarshal(t, out[5], &r.Caps)
	if len(out) == 7 {
		r.HasRung = true
		mustUnmarshal(t, out[6], &r.Rung)
	}
	return r
}

func TestACurrentLeafGetsALiveKitToken(t *testing.T) {
	e, ch, tok, group, sfu := callEnvWith(t, api.CallsConfig{
		LiveKitURL: testLiveKitURL, TURNSecret: testTURNSecret,
		TURNURLs:      []string{"turns:chat.example.test:443?transport=tcp"},
		CredentialTTL: time.Hour,
	})
	dev := deviceOf(t, e, tok)
	seedLeaf(t, e, group, dev, 3 /* addedEpoch */, nil /* removedEpoch */)
	status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	if status != http.StatusCreated {
		t.Fatalf("POST calls = %d (%x)", status, body)
	}
	out := decodeCall(t, body)
	if out.Token != "jwt-for-"+dev.String() || out.LiveKitURL != testLiveKitURL || out.GroupID != group {
		t.Fatalf("response = %+v", out)
	}
	// R9: the call is keyed by the call group's call id.
	if out.CallID != ch {
		t.Fatalf("call_id = %s, want the call group's call id %s", out.CallID, ch)
	}
	minted := sfu.minted()
	if len(minted) != 1 || minted[0][1] != dev.String() {
		t.Fatalf("the SFU minted %v, want one token for device %s", minted, dev)
	}
	// The call is recorded, which is what DELETE /v1/calls/{call_id} and the
	// restore path both read, and its room is the one the token names.
	row, err := e.Repo.GetVoiceSession(t.Context(), out.CallID)
	if err != nil || row.Ended != nil || row.LivekitRoom != minted[0][0] || row.LivekitRoom == "" ||
		row.ChannelID != ch || row.GroupID == nil || *row.GroupID != group {
		t.Fatalf("voice_sessions row = %+v (%v)", row, err)
	}

	// One relay entry, carrying the REST credential pion validates:
	// "<expiry>:<device_id>:<issued>" and base64(HMAC-SHA1(secret, username)).
	if len(out.ICE) != 1 || len(out.ICE[0]) != 3 {
		t.Fatalf("ice_servers = %v", out.ICE)
	}
	var urls []string
	var user, cred string
	mustUnmarshal(t, out.ICE[0][0], &urls)
	mustUnmarshal(t, out.ICE[0][1], &user)
	mustUnmarshal(t, out.ICE[0][2], &cred)
	if len(urls) != 1 || urls[0] != "turns:chat.example.test:443?transport=tcp" {
		t.Fatalf("urls = %v", urls)
	}
	if want := "1790003600:" + dev.String() + ":1790000000000"; user != want {
		t.Fatalf("username = %q, want <now+1h, s>:<device>:<now, ms> %q", user, want)
	}
	mac := hmac.New(sha1.New, []byte(testTURNSecret))
	mac.Write([]byte(user))
	if cred != base64.StdEncoding.EncodeToString(mac.Sum(nil)) {
		t.Fatal("the TURN credential is not HMAC-SHA1 over the username")
	}
}

func TestARemovedDeviceIsRefused(t *testing.T) {
	e, ch, tok, group := callEnv(t)
	dev := deviceOf(t, e, tok)
	removed := uint64(5)
	seedLeaf(t, e, group, dev, 3, &removed)
	status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	if status != http.StatusForbidden {
		t.Fatalf("removed leaf = %d, want 403", status)
	}
	if code := e.ErrCode(body); code != "E_LEAF_NOT_CURRENT" {
		t.Fatalf("code = %s", code)
	}
}

func TestALeafAddedInAFutureEpochIsRefused(t *testing.T) {
	e, ch, tok, group := callEnv(t) // group epoch is 7
	dev := deviceOf(t, e, tok)
	seedLeaf(t, e, group, dev, 9 /* addedEpoch > epoch */, nil)
	status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	if status != http.StatusForbidden {
		t.Fatalf("future leaf = %d, want 403", status)
	}
	if code := e.ErrCode(body); code != "E_LEAF_NOT_CURRENT" {
		t.Fatalf("code = %s", code)
	}
}

func TestADeviceThatIsNoLeafIsRefused(t *testing.T) {
	e, ch, tok, group := callEnv(t)
	// Another device of the same user is a leaf; this one is not.
	other := seedDevices(t, e, userOf(t, e, tok), 1)[0]
	seedLeaf(t, e, group, other, 3, nil)
	status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	if status != http.StatusForbidden || e.ErrCode(body) != "E_LEAF_NOT_CURRENT" {
		t.Fatalf("no leaf = %d %s, want 403 E_LEAF_NOT_CURRENT", status, e.ErrCode(body))
	}
}

func TestEndingACallMarksTheVoiceSession(t *testing.T) {
	e, ch, tok, group := callEnv(t)
	seedLeaf(t, e, group, deviceOf(t, e, tok), 3, nil)
	_, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	callID := decodeCall(t, body).CallID
	e.Clk.Advance(time.Minute)
	if status, _ := e.Do(http.MethodDelete, "/v1/calls/"+callID.String(), tok, nil); status != http.StatusNoContent {
		t.Fatal("DELETE call failed")
	}
	row, _ := e.Repo.GetVoiceSession(t.Context(), callID)
	if row.Ended == nil || *row.Ended != e.Clk.Now().Unix() {
		t.Fatalf("the voice session was not ended: %+v", row)
	}
	// Ending it again is not an error: the call is over either way.
	if status, _ := e.Do(http.MethodDelete, "/v1/calls/"+callID.String(), tok, nil); status != http.StatusNoContent {
		t.Fatal("a second DELETE of an ended call failed")
	}
	// DEV-46: ending a call closed its call group, so the next call needs a fresh one; once it is
	// registered the call reopens the row in a fresh room.
	status, resp := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	if status != http.StatusNotFound || e.ErrCode(resp) != "E_NOT_FOUND" {
		t.Fatalf("a start after the call ended = %d %s, want 404: its call group is closed", status, e.ErrCode(resp))
	}
	if g, err := e.Repo.GetGroup(t.Context(), group); err != nil || g.ClosedAt == nil {
		t.Fatalf("the ended call's group = %+v (%v), want it closed", g, err)
	}
	next := seedCallGroup(t, e, ch, ownerCommunityOf(t, e, ch), callGroupEpoch)
	seedLeaf(t, e, next, deviceOf(t, e, tok), 3, nil)
	status, body = e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	if status != http.StatusCreated {
		t.Fatalf("the next call = %d", status)
	}
	again, _ := e.Repo.GetVoiceSession(t.Context(), decodeCall(t, body).CallID)
	if again.Ended != nil || again.LivekitRoom == row.LivekitRoom || again.GroupID == nil || *again.GroupID != next {
		t.Fatalf("the next call = %+v, after %+v", again, row)
	}
}

// I11 (fix wave): protocol/09 says DELETE ends the call for everyone, so the LiveKit room the call
// was handed out in is closed, not merely marked ended: a participant still connected to it cannot
// stay in a room the next call no longer shares. An already-ended call closes nothing again.
func TestEndingACallClosesItsLiveKitRoom(t *testing.T) {
	e, ch, tok, group, sfu := callEnvWith(t, api.CallsConfig{LiveKitURL: testLiveKitURL})
	seedLeaf(t, e, group, deviceOf(t, e, tok), 3, nil)
	_, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	callID := decodeCall(t, body).CallID
	row, _ := e.Repo.GetVoiceSession(t.Context(), callID)
	if status, _ := e.Do(http.MethodDelete, "/v1/calls/"+callID.String(), tok, nil); status != http.StatusNoContent {
		t.Fatalf("DELETE call = %d, want 204", status)
	}
	if got := sfu.deletedRooms(); len(got) != 1 || got[0] != row.LivekitRoom {
		t.Fatalf("rooms closed = %v, want the call's room %q", got, row.LivekitRoom)
	}
	if status, _ := e.Do(http.MethodDelete, "/v1/calls/"+callID.String(), tok, nil); status != http.StatusNoContent {
		t.Fatalf("a second DELETE = %d, want 204", status)
	}
	if got := sfu.deletedRooms(); len(got) != 1 {
		t.Fatalf("rooms closed after a repeat DELETE = %v, want still only the one", got)
	}
}

// The call is over in dilla's record whatever the SFU answers: a room that will not close is logged,
// and the DELETE is still 204 with the voice session ended.
func TestARoomThatWillNotCloseStillEndsTheCall(t *testing.T) {
	e, ch, tok, group, sfu := callEnvWith(t, api.CallsConfig{LiveKitURL: testLiveKitURL})
	seedLeaf(t, e, group, deviceOf(t, e, tok), 3, nil)
	_, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	callID := decodeCall(t, body).CallID
	sfu.mu.Lock()
	sfu.deleteFail = errors.New("sfu down")
	sfu.mu.Unlock()
	if status, _ := e.Do(http.MethodDelete, "/v1/calls/"+callID.String(), tok, nil); status != http.StatusNoContent {
		t.Fatalf("DELETE with a failing room close = %d, want 204", status)
	}
	if row, _ := e.Repo.GetVoiceSession(t.Context(), callID); row.Ended == nil {
		t.Fatal("the voice session was not ended")
	}
	if got := sfu.deletedRooms(); len(got) != 1 {
		t.Fatalf("rooms asked to close = %v, want one attempt", got)
	}
}

// A second device starting the call that is already live joins it: the same
// call id and the same room, answered 200 rather than 201.
func TestASecondDeviceJoinsTheLiveCall(t *testing.T) {
	e, ch, tok, group, sfu := callEnvWith(t, api.CallsConfig{LiveKitURL: testLiveKitURL})
	seedLeaf(t, e, group, deviceOf(t, e, tok), 3, nil)
	_, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	first := decodeCall(t, body)

	user := userOf(t, e, tok)
	phone := seedDevices(t, e, user, 1)[0]
	e.sess["phone"] = auth.Session{UserID: user, DeviceID: phone, Scope: auth.ScopeEnrolled}
	seedLeaf(t, e, group, phone, 6, nil)
	status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", "phone", []any{})
	if status != http.StatusOK {
		t.Fatalf("joining a live call = %d, want 200", status)
	}
	second := decodeCall(t, body)
	if second.CallID != first.CallID {
		t.Fatalf("second call id %s, want %s", second.CallID, first.CallID)
	}
	minted := sfu.minted()
	if len(minted) != 2 || minted[0][0] != minted[1][0] || minted[1][1] != phone.String() {
		t.Fatalf("tokens minted %v: both devices must be in one room", minted)
	}
	// TURN is off here, so there is no relay to offer: an empty list, never null.
	if second.ICE == nil || len(second.ICE) != 0 {
		t.Fatalf("ice_servers without TURN = %v, want []", second.ICE)
	}
}

// C1 (fix wave): while a call is live, its token gate is the call group the call was opened on
// (voice_sessions.group_id), never merely the newest open call group of the channel. A second
// call group registered during the call (a restore's re-creation, or a registration from before
// the delivery service refused it) neither locks the live call's leaves out nor lets its own
// leaves into the live room.
func TestALiveCallIsGatedOnTheGroupItWasOpenedOn(t *testing.T) {
	e, ch, tok, group, sfu := callEnvWith(t, api.CallsConfig{LiveKitURL: testLiveKitURL})
	seedLeaf(t, e, group, deviceOf(t, e, tok), 3, nil)
	status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	if status != http.StatusCreated {
		t.Fatalf("opening the call = %d (%x)", status, body)
	}
	first := decodeCall(t, body)

	// A member's device is a leaf of a NEWER call group of the same channel only.
	cid := ownerCommunityOf(t, e, ch)
	_, memberTok := e.NewUser("member")
	joinCommunity(t, e, cid, memberTok)
	e.Clk.Advance(time.Minute)
	rival := seedCallGroup(t, e, ch, cid, callGroupEpoch)
	seedLeaf(t, e, rival, deviceOf(t, e, memberTok), 3, nil)

	status, body = e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	if status != http.StatusOK {
		t.Fatalf("a leaf of the live call's group, after a newer group appeared = %d %s, want 200",
			status, e.ErrCode(body))
	}
	if again := decodeCall(t, body); again.GroupID != group || again.CallID != first.CallID {
		t.Fatalf("rejoining = %+v, want group %s and call %s", again, group, first.CallID)
	}
	status, body = e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", memberTok, []any{})
	if status != http.StatusForbidden || e.ErrCode(body) != "E_LEAF_NOT_CURRENT" {
		t.Fatalf("a leaf of only the newer group = %d %s, want 403 E_LEAF_NOT_CURRENT", status, e.ErrCode(body))
	}
	if minted := sfu.minted(); len(minted) != 2 {
		t.Fatalf("tokens minted %v, want the owner's two and none for the newer group's leaf", minted)
	}
	row, err := e.Repo.GetVoiceSession(t.Context(), first.CallID)
	if err != nil || row.GroupID == nil || *row.GroupID != group {
		t.Fatalf("voice_sessions row = %+v (%v), want it still on the first group", row, err)
	}
}

// A live call whose group has been closed (a failed heal's re-creation) cannot go on: the next
// start from a leaf of the channel's current call group ends it and opens a fresh call there.
func TestACallWhoseGroupClosedIsReplacedOnTheCurrentGroup(t *testing.T) {
	e, ch, tok, group, sfu := callEnvWith(t, api.CallsConfig{LiveKitURL: testLiveKitURL})
	dev := deviceOf(t, e, tok)
	seedLeaf(t, e, group, dev, 3, nil)
	_, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	first := decodeCall(t, body)
	before, _ := e.Repo.GetVoiceSession(t.Context(), first.CallID)

	e.Clk.Advance(time.Minute)
	if err := e.Repo.CloseGroup(t.Context(), group, e.Clk.Now().Unix()); err != nil {
		t.Fatalf("CloseGroup: %v", err)
	}
	next := seedCallGroup(t, e, ch, ownerCommunityOf(t, e, ch), callGroupEpoch)
	seedLeaf(t, e, next, dev, 3, nil)
	status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	if status != http.StatusCreated {
		t.Fatalf("starting after the live call's group closed = %d %s, want 201", status, e.ErrCode(body))
	}
	if out := decodeCall(t, body); out.GroupID != next {
		t.Fatalf("the new call is on group %s, want the current group %s", out.GroupID, next)
	}
	after, _ := e.Repo.GetVoiceSession(t.Context(), first.CallID)
	if after.Ended != nil || after.GroupID == nil || *after.GroupID != next || after.LivekitRoom == before.LivekitRoom {
		t.Fatalf("voice_sessions row = %+v after %+v, want a fresh live room on the current group", after, before)
	}
	// The stale call is ended, so its room is closed like any ended call's.
	if got := sfu.deletedRooms(); len(got) != 1 || got[0] != before.LivekitRoom {
		t.Fatalf("rooms closed = %v, want the stale call's room %q", got, before.LivekitRoom)
	}
}

func TestCallsAreRefusedWhereThereIsNoCall(t *testing.T) {
	e, ch, tok, _ := callEnv(t)
	// A stranger learns nothing about the channel.
	_, stranger := e.NewUser("stranger")
	if status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", stranger, []any{}); status != http.StatusNotFound {
		t.Fatalf("a non-member = %d %s, want 404", status, e.ErrCode(body))
	}
	// A text channel carries no calls.
	cid := ownerCommunityOf(t, e, ch)
	text, _, _ := newChannel(t, e, cid, tok, 0, 1, 2, "text")
	if status, body := e.Do(http.MethodPost, "/v1/channels/"+text.String()+"/calls", tok, []any{}); status != http.StatusBadRequest {
		t.Fatalf("a text channel = %d %s, want 400", status, e.ErrCode(body))
	}
	// A voice channel with no call group registered yet.
	bare, _, _ := newChannel(t, e, cid, tok, 1, 1, 2, "bare")
	if status, body := e.Do(http.MethodPost, "/v1/channels/"+bare.String()+"/calls", tok, []any{}); status != http.StatusNotFound {
		t.Fatalf("no call group = %d %s, want 404", status, e.ErrCode(body))
	}
	// An unknown call.
	if status, _ := e.Do(http.MethodDelete, "/v1/calls/"+id.New().String(), tok, nil); status != http.StatusNotFound {
		t.Fatalf("DELETE of an unknown call = %d, want 404", status)
	}
	// A body that is not the empty array.
	if status, _ := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{"x"}); status != http.StatusBadRequest {
		t.Fatalf("a non-empty body = %d, want 400", status)
	}
}

// Only a current leaf of the call's group may end it for everyone.
func TestOnlyALeafEndsACall(t *testing.T) {
	e, ch, tok, group := callEnv(t)
	seedLeaf(t, e, group, deviceOf(t, e, tok), 3, nil)
	_, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	callID := decodeCall(t, body).CallID

	user := userOf(t, e, tok)
	other := seedDevices(t, e, user, 1)[0]
	e.sess["other"] = auth.Session{UserID: user, DeviceID: other, Scope: auth.ScopeEnrolled}
	status, resp := e.Do(http.MethodDelete, "/v1/calls/"+callID.String(), "other", nil)
	if status != http.StatusForbidden || e.ErrCode(resp) != "E_LEAF_NOT_CURRENT" {
		t.Fatalf("a non-leaf DELETE = %d %s, want 403 E_LEAF_NOT_CURRENT", status, e.ErrCode(resp))
	}
	if row, _ := e.Repo.GetVoiceSession(t.Context(), callID); row.Ended != nil {
		t.Fatal("a non-leaf ended the call")
	}
}

// A leaf whose user lost connect — a kicked device whose Remove is not committed yet — cannot end the
// call for everyone.
func TestALeafWithoutConnectCannotEndTheCall(t *testing.T) {
	e, ch, tok, group, _ := callEnvWith(t, api.CallsConfig{LiveKitURL: testLiveKitURL})
	seedLeaf(t, e, group, deviceOf(t, e, tok), 3, nil)
	_, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	callID := decodeCall(t, body).CallID
	member, _, memberTok := joinedMember(t, e, ch, group, "member")
	denyInChannel(t, e, ch, member, api.PermConnect)
	status, resp := e.Do(http.MethodDelete, "/v1/calls/"+callID.String(), memberTok, nil)
	if status != http.StatusForbidden || e.ErrCode(resp) != "E_FORBIDDEN" {
		t.Fatalf("a DELETE from a leaf without connect = %d %s, want 403 E_FORBIDDEN", status, e.ErrCode(resp))
	}
	if row, _ := e.Repo.GetVoiceSession(t.Context(), callID); row.Ended != nil {
		t.Fatal("a leaf without connect ended the call")
	}
}

func TestASFUFailureIsAnInternalError(t *testing.T) {
	e, ch, tok, group, sfu := callEnvWith(t, api.CallsConfig{LiveKitURL: testLiveKitURL})
	seedLeaf(t, e, group, deviceOf(t, e, tok), 3, nil)
	sfu.fail = errors.New("sfu down")
	if status, _ := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{}); status != http.StatusInternalServerError {
		t.Fatalf("an SFU failure = %d, want 500", status)
	}
}

// An instance with no SFU (livekit.enabled = false) still mounts the call routes, because the
// composition root mounts every route protocol/09 lists: a start that passes every gate is then 501
// and records no voice session, since there is no room to hand out; a request refused by an earlier
// gate keeps its own answer.
func TestAnInstanceWithoutAnSFUAnswersNotImplemented(t *testing.T) {
	e, cid, tok := channelEnv(t)
	ch, _, status := newChannel(t, e, cid, tok, 1 /* voice */, 1, 2, "voice")
	if status != http.StatusCreated {
		t.Fatalf("voice channel = %d", status)
	}
	api.NewCalls(e.Repo, api.NewResolver(e.Repo), nil, api.CallsConfig{}, e.Clk, slog.New(slog.DiscardHandler)).Register(e.Mux)
	group := seedCallGroup(t, e, ch, cid, callGroupEpoch)
	if status, _ := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{}); status != http.StatusForbidden {
		t.Fatalf("a device that is no leaf = %d, want the leaf gate's 403", status)
	}
	seedLeaf(t, e, group, deviceOf(t, e, tok), 3, nil)
	status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	if status != http.StatusNotImplemented {
		t.Fatalf("a call on an instance with no SFU = %d (%x), want 501", status, body)
	}
	if _, err := e.Repo.GetVoiceSession(t.Context(), ch); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a refused start recorded a voice session: %v", err)
	}
}

func ownerCommunityOf(t *testing.T, e *env, ch id.ID) id.ID {
	t.Helper()
	row, err := e.Repo.GetChannel(t.Context(), ch)
	if err != nil || row.CommunityID == nil {
		t.Fatalf("GetChannel: %v", err)
	}
	return *row.CommunityID
}

// joinedMember is a second user of the voice channel's community, with one device that is a leaf of
// group, and its bearer token.
func joinedMember(t *testing.T, e *env, ch, group id.ID, name string) (id.ID, id.ID, string) {
	t.Helper()
	user, tok := e.NewUser(name)
	joinCommunity(t, e, ownerCommunityOf(t, e, ch), tok)
	dev := deviceOf(t, e, tok)
	seedLeaf(t, e, group, dev, 3, nil)
	return user, dev, tok
}

func denyInChannel(t *testing.T, e *env, ch, user id.ID, deny api.Bits) {
	t.Helper()
	if err := e.Repo.PutOverwrite(t.Context(), store.OverwriteRow{
		ChannelID: ch, TargetKind: 1 /* user */, TargetID: user, Deny: uint64(deny),
	}); err != nil {
		t.Fatalf("PutOverwrite: %v", err)
	}
}

// G26: the token mirrors speak/video/screen_share per source; video sources only with a slot.
func TestTheTokenMirrorsTheMembersPermissions(t *testing.T) {
	e, ch, tok, group, stub := callEnvWith(t, api.CallsConfig{LiveKitURL: testLiveKitURL})
	seedLeaf(t, e, group, deviceOf(t, e, tok), 3, nil)
	if status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{}); status != http.StatusCreated {
		t.Fatalf("owner start = %d %s", status, e.ErrCode(body))
	}
	if got := stub.lastMint().Perm; !slices.Equal(got.GetCanPublishSources(), []livekit.TrackSource{livekit.TrackSource_MICROPHONE}) ||
		!got.GetCanPublish() || got.GetCanPublishData() || !got.GetCanSubscribe() {
		t.Fatalf("the owner's token = %+v, want the microphone only until a slot is held", got)
	}
	member, _, memberTok := joinedMember(t, e, ch, group, "member")
	denyInChannel(t, e, ch, member, api.PermSpeak|api.PermScreenShare)
	if status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", memberTok, []any{}); status != http.StatusOK {
		t.Fatalf("member start = %d %s", status, e.ErrCode(body))
	}
	if got := stub.lastMint().Perm; got.GetCanPublish() || got.GetCanPublishSources() != nil || !got.GetCanSubscribe() {
		t.Fatalf("a member without speak and no slot = %+v, want listen-only with no source list", got)
	}
}

// DEV-07 / MD-10: the decode list reaches the token as the dilla.vdec attribute.
func TestTheDecodeListReachesTheTokenAsAnAttribute(t *testing.T) {
	e, ch, tok, group, stub := callEnvWith(t, api.CallsConfig{LiveKitURL: testLiveKitURL})
	seedLeaf(t, e, group, deviceOf(t, e, tok), 3, nil)
	path := "/v1/channels/" + ch.String() + "/calls"
	if status, _ := e.Do(http.MethodPost, path, tok, []any{"vp8,h264"}); status != http.StatusCreated {
		t.Fatalf("start with vdec = %d", status)
	}
	if got := stub.lastMint().Attrs["dilla.vdec"]; got != "vp8,h264" {
		t.Fatalf("dilla.vdec = %q", got)
	}
	if status, _ := e.Do(http.MethodPost, path, tok, []any{}); status != http.StatusOK || stub.lastMint().Attrs != nil {
		t.Fatalf("start without vdec = %d, attributes %v; want 200 and none", status, stub.lastMint().Attrs)
	}
	for _, bad := range []any{"vp8,av1", "VP8", "vp8,vp8", "", uint64(1)} {
		if status, body := e.Do(http.MethodPost, path, tok, []any{bad}); status != http.StatusBadRequest || e.ErrCode(body) != "E_INVALID_REQUEST" {
			t.Errorf("vdec %v = %d %s, want 400 E_INVALID_REQUEST", bad, status, e.ErrCode(body))
		}
	}
	if status, _ := e.Do(http.MethodPost, path, tok, []any{"vp8", "h264"}); status != http.StatusBadRequest {
		t.Errorf("a two-element body = %d, want 400", status)
	}
}

// DEV-26 / MD-10: the caps element carries the bitrate ceilings in bits per second and the VP9 flag.
func TestTheCallResponseCarriesTheCaps(t *testing.T) {
	for _, vp9 := range []bool{false, true} {
		e, ch, tok, group, _ := callEnvWith(t, api.CallsConfig{
			LiveKitURL: testLiveKitURL, MaxAudioBitrateKbps: 64, MaxShareBitrateKbps: 2500, VP9: vp9,
		})
		seedLeaf(t, e, group, deviceOf(t, e, tok), 3, nil)
		_, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
		want := []uint64{64_000, 2_500_000, 0}
		if vp9 {
			want[2] = 1
		}
		if got := decodeCall(t, body).Caps; !slices.Equal(got, want) {
			t.Errorf("caps with vp9=%v = %v, want %v", vp9, got, want)
		}
	}
}

// DEV-44: every start opens the room in the SFU before minting, on 201 and 200 alike; a room that
// will not open mints nothing.
func TestEveryStartCreatesTheRoomBeforeMinting(t *testing.T) {
	e, ch, tok, group, stub := callEnvWith(t, api.CallsConfig{LiveKitURL: testLiveKitURL})
	seedLeaf(t, e, group, deviceOf(t, e, tok), 3, nil)
	path := "/v1/channels/" + ch.String() + "/calls"
	e.Do(http.MethodPost, path, tok, []any{})
	e.Do(http.MethodPost, path, tok, []any{})
	minted := stub.minted()
	if got := stub.createdRooms(); len(got) != 2 || got[0] != minted[0][0] || got[1] != minted[0][0] {
		t.Fatalf("rooms created = %v, want the call's room twice (%v)", got, minted)
	}
	stub.mu.Lock()
	stub.createFail = errors.New("sfu down")
	stub.mu.Unlock()
	if status, _ := e.Do(http.MethodPost, path, tok, []any{}); status != http.StatusInternalServerError {
		t.Fatalf("a room that will not open = %d, want 500", status)
	}
	if len(stub.minted()) != 2 {
		t.Fatal("a token was minted for a room the SFU did not open")
	}
}

// DEV-01: the advisory count refuses 409 E_CALL_FULL; it skips the caller's own device and its "#"
// shadows, disconnected participants, agents and egress, and fails open when the SFU cannot answer.
func TestAFullCallIsRefusedWithECallFull(t *testing.T) {
	e, ch, tok, group, stub := callEnvWith(t, api.CallsConfig{LiveKitURL: testLiveKitURL, MaxVoiceParticipants: 2})
	dev := deviceOf(t, e, tok)
	seedLeaf(t, e, group, dev, 3, nil)
	path := "/v1/channels/" + ch.String() + "/calls"
	_, body := e.Do(http.MethodPost, path, tok, []any{})
	room := stub.minted()[0][0]
	other := func(identity string) *livekit.ParticipantInfo { return &livekit.ParticipantInfo{Identity: identity} }
	stub.setPresent(room,
		other(dev.String()), other(dev.String()+"#x"), other(id.New().String()),
		&livekit.ParticipantInfo{Identity: id.New().String(), State: livekit.ParticipantInfo_DISCONNECTED},
		&livekit.ParticipantInfo{Identity: "agent", Kind: livekit.ParticipantInfo_AGENT},
		&livekit.ParticipantInfo{Identity: "egress", Kind: livekit.ParticipantInfo_EGRESS},
	)
	if status, _ := e.Do(http.MethodPost, path, tok, []any{}); status != http.StatusOK {
		t.Fatalf("a rejoin counting one other = %d, want 200", status)
	}
	user := userOf(t, e, tok)
	phone := seedDevices(t, e, user, 1)[0]
	e.sess["phone"] = auth.Session{UserID: user, DeviceID: phone, Scope: auth.ScopeEnrolled}
	seedLeaf(t, e, group, phone, 3, nil)
	stub.setPresent(room, other(dev.String()), other(id.New().String()))
	status, resp := e.Do(http.MethodPost, path, "phone", []any{})
	if status != http.StatusConflict || e.ErrCode(resp) != "E_CALL_FULL" {
		t.Fatalf("a third device in a call of two = %d %s, want 409 E_CALL_FULL (%x)", status, e.ErrCode(resp), body)
	}
	stub.mu.Lock()
	stub.listFail = errors.New("sfu down")
	stub.mu.Unlock()
	if status, _ := e.Do(http.MethodPost, path, "phone", []any{}); status != http.StatusOK {
		t.Fatalf("a count that fails = %d, want the fail-open 200", status)
	}
}

// admitted is AdmitRoom as (permission, refusal code, store error).
func admitted(t *testing.T, calls *api.Calls, room string, dev id.ID) (*livekit.ParticipantPermission, string, error) {
	t.Helper()
	perm, err := calls.AdmitRoom(t.Context(), room, dev)
	var se *server.Error
	if errors.As(err, &se) {
		return nil, string(se.Code), nil
	}
	return perm, "", err
}

// A kicked device is still a leaf until another member commits its Remove, but it lost view_channel
// or connect at once: the gate refuses it meanwhile.
func TestTheGateRefusesALeafThatLostViewOrConnect(t *testing.T) {
	e, ch, tok, group, stub, calls := callEnvCalls(t, api.CallsConfig{LiveKitURL: testLiveKitURL})
	seedLeaf(t, e, group, deviceOf(t, e, tok), 3, nil)
	e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	room := stub.minted()[0][0]
	member, memberDev, _ := joinedMember(t, e, ch, group, "member")
	for _, lost := range []api.Bits{api.PermConnect, api.PermViewChannel} {
		denyInChannel(t, e, ch, member, lost)
		if perm, code, err := admitted(t, calls, room, memberDev); perm != nil || code != "E_FORBIDDEN" || err != nil {
			t.Errorf("lost %#x: the gate = %v %q %v; want E_FORBIDDEN", lost, perm, code, err)
		}
	}
}

// The gate answers the device's current base grant: the microphone while it holds speak, nothing to
// publish once speak is revoked, and never a camera or screen source even while it holds a slot.
func TestTheGateAnswersTheCurrentBaseGrant(t *testing.T) {
	e, ch, tok, group, stub, calls := callEnvCalls(t, api.CallsConfig{LiveKitURL: testLiveKitURL})
	seedLeaf(t, e, group, deviceOf(t, e, tok), 3, nil)
	_, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	room := stub.minted()[0][0]
	member, memberDev, memberTok := joinedMember(t, e, ch, group, "member")
	if status, _ := e.Do(http.MethodPost, "/v1/calls/"+decodeCall(t, body).CallID.String()+"/share", memberTok, []any{}); status != http.StatusNoContent {
		t.Fatalf("member share = %d", status)
	}
	perm, code, err := admitted(t, calls, room, memberDev)
	if err != nil || code != "" || !slices.Equal(perm.GetCanPublishSources(), []livekit.TrackSource{livekit.TrackSource_MICROPHONE}) {
		t.Fatalf("a sharer's admission = %+v %q %v, want the microphone only", perm, code, err)
	}
	denyInChannel(t, e, ch, member, api.PermSpeak)
	if perm, _, _ := admitted(t, calls, room, memberDev); perm.GetCanPublish() || len(perm.GetCanPublishSources()) != 0 {
		t.Fatalf("after speak was revoked the gate answers %+v, want nothing to publish", perm)
	}
}

func TestAdmitRoomIsTheGateTheProxyReads(t *testing.T) {
	e, ch, tok, group, stub, calls := callEnvCalls(t, api.CallsConfig{LiveKitURL: testLiveKitURL})
	dev := deviceOf(t, e, tok)
	seedLeaf(t, e, group, dev, 3, nil)
	_, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	callID := decodeCall(t, body).CallID
	room := stub.minted()[0][0]
	for _, tc := range []struct {
		name string
		room string
		dev  id.ID
		code string
	}{
		{"the call's room and a leaf", room, dev, ""},
		{"another device", room, id.New(), "E_LEAF_NOT_CURRENT"},
		{"an older room of the call", callID.String() + "-1", dev, "E_LEAF_NOT_CURRENT"},
		{"a room that is no call", "not-a-call", dev, "E_LEAF_NOT_CURRENT"},
	} {
		if _, code, err := admitted(t, calls, tc.room, tc.dev); err != nil || code != tc.code {
			t.Errorf("%s: AdmitRoom = %q, %v; want %q", tc.name, code, err, tc.code)
		}
	}
	if err := e.Repo.EndVoiceSession(t.Context(), callID, e.Clk.Now().Unix()); err != nil {
		t.Fatalf("EndVoiceSession: %v", err)
	}
	if _, code, _ := admitted(t, calls, room, dev); code != "E_LEAF_NOT_CURRENT" {
		t.Error("an ended call's room still admits its leaves")
	}
}
