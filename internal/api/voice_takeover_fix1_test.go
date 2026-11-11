package api_test

import (
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
)

// Fix round 1 (I1): a device the SFU dropped (a wifi loss) is listed in no room when the user joins
// on another device. It still gets pending 3, so its lost-connection recovery reads 3 and does not
// rejoin and take voice back.
func TestTakeoverMarksADroppedDevice(t *testing.T) {
	k := newTakeoverEnv(t)
	owner := deviceOf(t, k.e, k.ownerTok)
	k.calls.Admitted(t.Context(), k.room, k.memberDev, false)
	k.stub.setPresent(k.room, part(owner)) // the laptop dropped off the SFU
	k.connect(t, k.room, k.phone, part(owner), part(k.phone))
	if n := len(deviceRemovals(k.stub)); n != 0 {
		t.Fatalf("removals %v with the laptop in no room", deviceRemovals(k.stub))
	}
	if got := k.pending(t, k.memberTok); got != 3 {
		t.Fatalf("the dropped laptop's pending = %d, want 3 (its recovery must not rejoin)", got)
	}
	if got := k.pending(t, "member-phone"); got != 0 {
		t.Fatalf("the phone's pending = %d, want 0", got)
	}
}

// An idle device never admitted to a call (no remembered admission, sequence 0) gets pending 3 as
// well; another user's idle device is untouched.
func TestTakeoverMarksIdleDevicesOfTheUserOnly(t *testing.T) {
	k := newTakeoverEnv(t)
	owner := deviceOf(t, k.e, k.ownerTok)
	k.stub.setPresent(k.room)
	k.connect(t, k.room, k.phone, part(k.phone))
	if got := k.pending(t, k.memberTok); got != 3 {
		t.Fatalf("the member's idle laptop's pending = %d, want 3", got)
	}
	if got := k.pending(t, k.ownerTok); got != 0 {
		t.Fatalf("another user's idle device's pending = %d, want 0", got)
	}
	if n := removalsOf(k.stub, owner); n != 0 {
		t.Fatalf("another user's device was cut %d times", n)
	}
}

// A device the sweep moved (pending 1) and not admitted since keeps its reason: it is already
// pending, so its recovery does not rejoin, and it left for the sweep's reason.
func TestTakeoverKeepsAnEarlierReasonOfAnUnlistedDevice(t *testing.T) {
	k := newTakeoverEnv(t)
	k.idleMove(t)
	owner := deviceOf(t, k.e, k.ownerTok)
	k.stub.setPresent(k.room, part(owner))
	k.connect(t, k.room, k.phone, part(owner), part(k.phone))
	if got := k.pending(t, k.memberTok); got != 1 {
		t.Fatalf("the moved laptop's pending = %d, want 1 (kept)", got)
	}
}

// Fix round 1 (m5): a takeover retry whose room is no longer a live call's is dropped, not retried.
func TestTakeoverRetryDroppedWhenTheCallEnded(t *testing.T) {
	k := newTakeoverEnv(t)
	owner := deviceOf(t, k.e, k.ownerTok)
	k.calls.Admitted(t.Context(), k.room, k.memberDev, false)
	k.stub.mu.Lock()
	k.stub.failRemovals = 1
	k.stub.mu.Unlock()
	k.connect(t, k.room, k.phone, part(owner), part(k.memberDev), part(k.phone))
	callHex, _, _ := strings.Cut(k.room, "-")
	callID, err := id.Parse(callHex)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.e.Repo.EndVoiceSession(t.Context(), callID, k.e.Clk.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	k.calls.AFKMaintenance(t.Context())
	if n := removalsOf(k.stub, k.memberDev); n != 1 {
		t.Fatalf("a retry in an ended call's room was tried (%d tries, want 1)", n)
	}
}
