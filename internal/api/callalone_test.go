package api_test

import (
	"net/http"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
)

// DELETE /v1/calls/{call_id}?if_alone=1 (web-3 task 10 fix, f10app I1): a leave by a person who
// believes they are alone ends the call only while their device is the call group's only current
// leaf, decided by the delivery service under the group lock. A device that has just joined (its
// external commit landed, so it holds a leaf) keeps the call: the answer is 409 E_CALL_NOT_ALONE and
// nothing is ended or closed.
func TestAnEndIfAloneLeavesACallAnotherDeviceHasJoined(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	joinedMember(t, f.e, f.ch, f.group, "member")
	status, body := f.e.Do(http.MethodDelete, "/v1/calls/"+f.callID.String()+"?if_alone=1", f.tok, nil)
	if status != http.StatusConflict || f.e.ErrCode(body) != "E_CALL_NOT_ALONE" {
		t.Fatalf("DELETE ?if_alone=1 beside another leaf = %d %s, want 409 E_CALL_NOT_ALONE", status, f.e.ErrCode(body))
	}
	if row, _ := f.e.Repo.GetVoiceSession(t.Context(), f.callID); row.Ended != nil {
		t.Fatal("a conditional end beside another leaf ended the call")
	}
	if g, _ := f.e.Repo.GetGroup(t.Context(), f.group); g.ClosedAt != nil {
		t.Fatal("a conditional end beside another leaf closed the call group")
	}
	if got := f.stub.deletedRooms(); len(got) != 0 {
		t.Fatalf("a conditional end beside another leaf closed rooms %v", got)
	}
}

// …and ends it, exactly as the unconditional DELETE does, when the caller is alone.
func TestAnEndIfAloneEndsTheCallOfItsOnlyLeaf(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	if status, body := f.e.Do(http.MethodDelete, "/v1/calls/"+f.callID.String()+"?if_alone=1", f.tok, nil); status != http.StatusNoContent {
		t.Fatalf("DELETE ?if_alone=1 alone = %d %s, want 204", status, f.e.ErrCode(body))
	}
	if row, _ := f.e.Repo.GetVoiceSession(t.Context(), f.callID); row.Ended == nil {
		t.Fatal("the only leaf's conditional end left the call live")
	}
	if got := f.ds.closedGroups(); len(got) != 1 || got[0] != f.group {
		t.Fatalf("groups closed = %v, want the call group once", got)
	}
	if got := f.stub.deletedRooms(); len(got) != 1 || got[0] != f.room {
		t.Fatalf("rooms deleted = %v", got)
	}
	// A repeat finds the call over: 204, as the unconditional form answers.
	if status, _ := f.e.Do(http.MethodDelete, "/v1/calls/"+f.callID.String()+"?if_alone=1", f.tok, nil); status != http.StatusNoContent {
		t.Fatalf("a repeated conditional end = %d, want 204", status)
	}
}

// The unconditional DELETE keeps its semantics: any current leaf ends the call for everyone.
func TestAnUnconditionalEndStillEndsACallWithOthersInIt(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	joinedMember(t, f.e, f.ch, f.group, "member")
	if status, body := f.e.Do(http.MethodDelete, "/v1/calls/"+f.callID.String(), f.tok, nil); status != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s, want 204", status, f.e.ErrCode(body))
	}
	if row, _ := f.e.Repo.GetVoiceSession(t.Context(), f.callID); row.Ended == nil {
		t.Fatal("the unconditional end left the call live")
	}
}

// if_alone takes the one value 1; anything else is a malformed request and ends nothing.
func TestAnEndWithAMalformedIfAloneIsRefused(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	for _, q := range []string{"?if_alone=0", "?if_alone=", "?if_alone=true", "?if_alone=1&if_alone=1"} {
		status, body := f.e.Do(http.MethodDelete, "/v1/calls/"+f.callID.String()+q, f.tok, nil)
		if status != http.StatusBadRequest || f.e.ErrCode(body) != "E_INVALID_REQUEST" {
			t.Fatalf("DELETE %s = %d %s, want 400 E_INVALID_REQUEST", q, status, f.e.ErrCode(body))
		}
	}
	if row, _ := f.e.Repo.GetVoiceSession(t.Context(), f.callID); row.Ended != nil {
		t.Fatal("a malformed conditional end ended the call")
	}
}
