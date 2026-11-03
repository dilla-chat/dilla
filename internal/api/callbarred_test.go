package api_test

import (
	"net/http"
	"testing"

	"github.com/livekit/protocol/livekit"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/id"
)

// barredState is one of the four states that bar a device from every call: its own row revoked or
// quarantined, or its user disabled or deleted.
type barredState struct {
	name string
	bar  func(t *testing.T, e *env, user, dev id.ID)
}

func barredStates() []barredState {
	return []barredState{
		{"revoked", func(t *testing.T, e *env, _, dev id.ID) {
			if err := e.Repo.RevokeDevice(t.Context(), dev, e.Clk.Now().Unix()); err != nil {
				t.Fatalf("RevokeDevice: %v", err)
			}
		}},
		{"quarantined", func(t *testing.T, e *env, _, dev id.ID) {
			if err := e.Repo.QuarantineDevice(t.Context(), dev, e.Clk.Now().Unix(), "fork quorum"); err != nil {
				t.Fatalf("QuarantineDevice: %v", err)
			}
		}},
		{"disabled", func(t *testing.T, e *env, user, _ id.ID) {
			if err := e.Repo.SetUserDisabled(t.Context(), user, ptr(e.Clk.Now().Unix())); err != nil {
				t.Fatalf("SetUserDisabled: %v", err)
			}
		}},
		{"deleted", func(t *testing.T, e *env, user, _ id.ID) {
			if err := e.Repo.TombstoneUser(t.Context(), user, e.Clk.Now().Unix()); err != nil {
				t.Fatalf("TombstoneUser: %v", err)
			}
		}},
	}
}

// barredEnv is an open call (the owner's), a member whose device is a leaf of it and present in its
// room, and the paths a test drives.
type barredEnv struct {
	e                 *env
	stub              *stubSFU
	calls             *api.Calls
	ch, callID        id.ID
	room              string
	member, memberDev id.ID
	memberTok         string
	ownerDev          id.ID
}

func newBarredEnv(t *testing.T) *barredEnv {
	t.Helper()
	e, ch, tok, group, stub, calls := callEnvCalls(t, api.CallsConfig{LiveKitURL: testLiveKitURL, MaxPublishers: 2})
	seedLeaf(t, e, group, deviceOf(t, e, tok), 3, nil)
	_, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	member, memberDev, memberTok := joinedMember(t, e, ch, group, "member")
	room := stub.minted()[0][0]
	stub.setPresent(room, &livekit.ParticipantInfo{Identity: deviceOf(t, e, tok).String()},
		&livekit.ParticipantInfo{Identity: memberDev.String()})
	return &barredEnv{e: e, stub: stub, calls: calls, ch: ch, callID: decodeCall(t, body).CallID, room: room,
		member: member, memberDev: memberDev, memberTok: memberTok, ownerDev: deviceOf(t, e, tok)}
}

func (b *barredEnv) sharePath() string { return "/v1/calls/" + b.callID.String() + "/share" }

// codeOfBody is the E_* code of an error answer, or "" for a success, whose body is no error array.
func codeOfBody(e *env, status int, body []byte) string {
	if status < 400 {
		return ""
	}
	return e.ErrCode(body)
}

// Critical finding of the task 10 review: a revoked or quarantined device, or a device of a disabled
// or deleted user, is refused at the /rtc gate, at start and at share (403 E_FORBIDDEN), however
// current its leaf and its user's bits are.
func TestABarredDeviceIsRefusedAtTheGateAtStartAndAtShare(t *testing.T) {
	for _, st := range barredStates() {
		t.Run(st.name, func(t *testing.T) {
			b := newBarredEnv(t)
			// Unbarred, the member is admitted, minted and may share: the refusals below are the bar's.
			if _, code, err := admitted(t, b.calls, b.room, b.memberDev); code != "" || err != nil {
				t.Fatalf("before the bar the gate = %q %v", code, err)
			}
			st.bar(t, b.e, b.member, b.memberDev)
			if perm, code, err := admitted(t, b.calls, b.room, b.memberDev); perm != nil || code != "E_FORBIDDEN" || err != nil {
				t.Errorf("the gate for a %s device = %v %q %v; want E_FORBIDDEN", st.name, perm, code, err)
			}
			minted := len(b.stub.minted())
			status, body := b.e.Do(http.MethodPost, "/v1/channels/"+b.ch.String()+"/calls", b.memberTok, []any{})
			if status != http.StatusForbidden || codeOfBody(b.e, status, body) != "E_FORBIDDEN" {
				t.Errorf("a start from a %s device = %d %s; want 403 E_FORBIDDEN", st.name, status, codeOfBody(b.e, status, body))
			}
			if len(b.stub.minted()) != minted {
				t.Errorf("a token was minted for a %s device", st.name)
			}
			status, body = b.e.Do(http.MethodPost, b.sharePath(), b.memberTok, []any{})
			if status != http.StatusForbidden || codeOfBody(b.e, status, body) != "E_FORBIDDEN" {
				t.Errorf("a share from a %s device = %d %s; want 403 E_FORBIDDEN", st.name, status, codeOfBody(b.e, status, body))
			}
			if hasCamera(b.stub.lastPerms()[b.memberDev.String()]) || len(b.calls.SharersOf(b.callID)) != 0 {
				t.Errorf("a %s device was promoted", st.name)
			}
		})
	}
}

// Minor m3 of the task 10 review: a sharing slot belongs to the room it was taken in. A slot left
// behind when a call's room is replaced (the DELETE's slot drop could not take the lock, or the
// room was reaped and the call re-opened) never carries into the next call of the same call id: it
// neither takes a slot of the new call nor lets a sync push the camera to a device that did not
// share in it.
func TestASlotNeverCarriesIntoTheNextCallOfTheSameCallID(t *testing.T) {
	l := newLeaseEnv(t, 1, 2)
	if status := l.share(0); status != http.StatusNoContent {
		t.Fatalf("dev0 share = %d", status)
	}
	// The call ends and the next one opens in a fresh room, without the DELETE's slot drop.
	row, err := l.e.Repo.GetVoiceSession(t.Context(), l.callID)
	if err != nil {
		t.Fatalf("GetVoiceSession: %v", err)
	}
	if err := l.e.Repo.EndVoiceSession(t.Context(), l.callID, l.e.Clk.Now().Unix()); err != nil {
		t.Fatalf("EndVoiceSession: %v", err)
	}
	next := row
	next.LivekitRoom, next.Started, next.Ended = l.callID.String()+"-1790009999", row.Started+1, nil
	if err := l.e.Repo.PutVoiceSession(t.Context(), next); err != nil {
		t.Fatalf("PutVoiceSession: %v", err)
	}
	present := []*livekit.ParticipantInfo{{Identity: l.devs[0].String()}, {Identity: l.devs[1].String()}}
	l.stub.setPresent(next.LivekitRoom, present...)

	if err := l.sync(t); err != nil {
		t.Fatalf("SyncCallGrants: %v", err)
	}
	if hasCamera(l.stub.lastPerms()[l.devs[0].String()]) {
		t.Fatal("a sync in the next call pushed the camera to a device that did not share in it")
	}
	if status := l.share(1); status != http.StatusNoContent {
		t.Fatalf("the one slot of the next call = %d, want 204: the old call's slot carried over", status)
	}
	l.calls.RetryPending(t.Context())
	if got := l.calls.SharersOf(l.callID); len(got) != 1 || got[0] != l.devs[1] {
		t.Fatalf("slot holders of the next call = %v, want only dev1", got)
	}
}
