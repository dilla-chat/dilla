package api_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/livekit/protocol/livekit"

	"github.com/dilla-chat/dilla/internal/id"
)

// countRemoved is how many RemoveParticipants calls the stub SFU took for dev in room.
func countRemoved(s *stubSFU, room string, dev id.ID) int {
	devices, _ := s.removals()
	n := 0
	for _, r := range devices {
		if r[0] == room && r[1] == dev.String() {
			n++
		}
	}
	return n
}

// Final review I3: a cut by PUT …/afk/away that fails (the SFU refuses the removal once) is retried by
// the next AFK maintenance pass while the user is still away, and only until it went through.
func TestAFKPutAwayRetriesAFailedCutOnTheNextTick(t *testing.T) {
	a := newAwayEnv(t)
	path := "/v1/communities/" + a.cid.String() + "/afk/away"
	phone := secondDevice(t, a.e, a.member, "member-phone")
	a.stub.setPresent(a.room, &livekit.ParticipantInfo{Identity: phone.String()})
	a.stub.mu.Lock()
	a.stub.failRemovals = 1
	a.stub.mu.Unlock()
	if status, body := a.e.Do(http.MethodPut, path, a.memberTok, []any{}); status != http.StatusNoContent {
		t.Fatalf("PUT [] = %d (%x), want 204", status, body)
	}
	if !a.isAway(t, a.cid, a.member) {
		t.Fatal("PUT did not mark the member away")
	}
	if n := countRemoved(a.stub, a.room, phone); n != 1 {
		t.Fatalf("removals of the phone after the PUT = %d, want 1 (the failed one)", n)
	}
	a.calls.AFKMaintenance(t.Context())
	if n := countRemoved(a.stub, a.room, phone); n != 2 {
		t.Fatalf("removals of the phone after the tick = %d, want 2: the failed cut is retried", n)
	}
	// The retry went through, so the next tick asks for nothing more.
	a.calls.AFKMaintenance(t.Context())
	if n := countRemoved(a.stub, a.room, phone); n != 2 {
		t.Fatalf("removals of the phone after a second tick = %d, want 2: a finished cut is not retried", n)
	}
}

// Final review I3: a failed SFU listing during the PUT cuts nothing then; the next tick lists again
// and cuts the device.
func TestAFKPutAwayRetriesAFailedListingOnTheNextTick(t *testing.T) {
	a := newAwayEnv(t)
	path := "/v1/communities/" + a.cid.String() + "/afk/away"
	phone := secondDevice(t, a.e, a.member, "member-phone")
	a.stub.setPresent(a.room, &livekit.ParticipantInfo{Identity: phone.String()})
	a.stub.mu.Lock()
	a.stub.listFail = errors.New("stub SFU: listing failed")
	a.stub.mu.Unlock()
	if status, body := a.e.Do(http.MethodPut, path, a.memberTok, []any{}); status != http.StatusNoContent {
		t.Fatalf("PUT [] = %d (%x), want 204", status, body)
	}
	if n := countRemoved(a.stub, a.room, phone); n != 0 {
		t.Fatalf("removals after a failed listing = %d, want 0", n)
	}
	a.stub.mu.Lock()
	a.stub.listFail = nil
	a.stub.mu.Unlock()
	a.calls.AFKMaintenance(t.Context())
	if n := countRemoved(a.stub, a.room, phone); n != 1 {
		t.Fatalf("removals of the phone after the tick = %d, want 1: the cut is retried once the listing answers", n)
	}
}

// Final review I3: a retry stops once the user is no longer away (here: a message cleared it), without
// cutting anything.
func TestAFKCutRetryStopsWhenTheUserIsBack(t *testing.T) {
	a := newAwayEnv(t)
	path := "/v1/communities/" + a.cid.String() + "/afk/away"
	phone := secondDevice(t, a.e, a.member, "member-phone")
	a.stub.setPresent(a.room, &livekit.ParticipantInfo{Identity: phone.String()})
	a.stub.mu.Lock()
	a.stub.failRemovals = 1
	a.stub.mu.Unlock()
	if status, _ := a.e.Do(http.MethodPut, path, a.memberTok, []any{}); status != http.StatusNoContent {
		t.Fatalf("PUT [] = %d, want 204", status)
	}
	a.calls.AFKActiveInChannel(t.Context(), a.member, a.ch)
	if a.isAway(t, a.cid, a.member) {
		t.Fatal("activity did not clear away")
	}
	a.calls.AFKMaintenance(t.Context())
	if n := countRemoved(a.stub, a.room, phone); n != 1 {
		t.Fatalf("removals after the user came back = %d, want 1: no retry once not away", n)
	}
}

// Final review M1: a PUT whose mark is skipped (the user holds no gateway connection) leaves the user
// not away, so it cuts nothing.
func TestAFKPutAwayWithASkippedMarkCutsNothing(t *testing.T) {
	a := newAwayEnv(t)
	path := "/v1/communities/" + a.cid.String() + "/afk/away"
	phone := secondDevice(t, a.e, a.member, "member-phone")
	a.stub.setPresent(a.room, &livekit.ParticipantInfo{Identity: phone.String()})
	a.setOffline(a.member, true)
	if status, _ := a.e.Do(http.MethodPut, path, a.memberTok, []any{}); status != http.StatusNoContent {
		t.Fatalf("PUT [] = %d, want 204", status)
	}
	if a.isAway(t, a.cid, a.member) {
		t.Fatal("an unconnected user was marked away")
	}
	if n := countRemoved(a.stub, a.room, phone); n != 0 {
		t.Fatalf("removals of the phone = %d, want 0: a skipped mark cuts nothing", n)
	}
}

// Final review I3: the retries are bounded: a removal that keeps failing is tried on five maintenance
// passes after the PUT's own attempt, and then no more.
func TestAFKCutRetryGivesUpAfterFivePasses(t *testing.T) {
	a := newAwayEnv(t)
	path := "/v1/communities/" + a.cid.String() + "/afk/away"
	phone := secondDevice(t, a.e, a.member, "member-phone")
	a.stub.setPresent(a.room, &livekit.ParticipantInfo{Identity: phone.String()})
	a.stub.mu.Lock()
	a.stub.failRemovals = 100
	a.stub.mu.Unlock()
	if status, _ := a.e.Do(http.MethodPut, path, a.memberTok, []any{}); status != http.StatusNoContent {
		t.Fatalf("PUT [] = %d, want 204", status)
	}
	for range 8 {
		a.calls.AFKMaintenance(t.Context())
	}
	if n := countRemoved(a.stub, a.room, phone); n != 6 {
		t.Fatalf("removals of the phone = %d, want 6: the PUT's and five retries", n)
	}
}
