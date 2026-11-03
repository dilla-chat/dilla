package api_test

import (
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/id"
)

// AW2 final round: Q2 as revised by Jonas (a reload or a short blip keeps a person away; only 30
// continuous seconds with no gateway connection clear it), and the re-review's R3-M1, R3-n1, R3-n2
// and R3-n3.

// waitOffline waits until no goroutine of c waits out an offline grace, then flushes op 55.
func waitOffline(t *testing.T, c *api.Calls) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for c.AFKOfflineWaitersForTest() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d offline waits never ended", c.AFKOfflineWaitersForTest())
		}
		time.Sleep(time.Millisecond)
	}
	c.FlushAFKAwayForTest()
}

// A reload: the last connection closes and a new one opens 10 s later. The grace ends with the user
// connected, so they stay away and nobody is told otherwise.
func TestAFKReconnectWithinTheGraceKeepsAway(t *testing.T) {
	a := newAwayEnv(t)
	a.markAway(t, a.cid, a.member, 100)
	a.setOffline(a.member, true)
	a.calls.AFKOffline(t.Context(), a.member)
	a.e.Clk.Advance(10 * time.Second)
	a.setOffline(a.member, false) // the reloaded page connects again
	a.e.Clk.Advance(21 * time.Second)
	waitOffline(t, a.calls)
	if !a.isAway(t, a.cid, a.member) {
		t.Fatal("a reconnect 10 s after the last close cleared away")
	}
	if sent := a.away.sent(a.cid, a.member); len(sent) != 0 {
		t.Fatalf("op 55 sent for a reload: %v", sent)
	}
}

// 31 s with no connection clears every community's row and announces each; 29 s does not.
func TestAFKOfflineForTheGraceClearsAway(t *testing.T) {
	a := newAwayEnv(t)
	other := createCommunity(t, a.e, a.ownerTok)
	joinCommunity(t, a.e, other, a.memberTok)
	a.markAway(t, a.cid, a.member, 100)
	a.markAway(t, other, a.member, 100)
	a.setOffline(a.member, true)
	a.calls.AFKOffline(t.Context(), a.member)
	a.e.Clk.Advance(29 * time.Second)
	time.Sleep(20 * time.Millisecond)
	if !a.isAway(t, a.cid, a.member) || !a.isAway(t, other, a.member) {
		t.Fatal("away cleared before the 30 s grace ended")
	}
	a.e.Clk.Advance(2 * time.Second)
	waitOffline(t, a.calls)
	for _, cid := range []id.ID{a.cid, other} {
		if a.isAway(t, cid, a.member) {
			t.Fatalf("still away in %s after 31 s offline", cid)
		}
		toEveryone(t, a.away.sent(cid, a.member), []id.ID{a.owner, a.member}, 0)
	}
}

// Two tabs: the gateway runs the hook only for the user's last connection, so a closing tab while
// another stays open never starts a grace; and should one start (a close racing a second tab's
// connect), the grace ends with the user connected and clears nothing.
func TestAFKOneTabClosingWhileAnotherStaysKeepsAway(t *testing.T) {
	a := newAwayEnv(t)
	a.markAway(t, a.cid, a.member, 100)
	a.calls.AFKOffline(t.Context(), a.member) // the other tab is open: the user stays connected
	a.e.Clk.Advance(31 * time.Second)
	waitOffline(t, a.calls)
	if !a.isAway(t, a.cid, a.member) {
		t.Fatal("a close with another tab open cleared away")
	}
	// The second tab closes too: 30 s later away clears.
	a.setOffline(a.member, true)
	a.calls.AFKOffline(t.Context(), a.member)
	a.e.Clk.Advance(31 * time.Second)
	waitOffline(t, a.calls)
	if a.isAway(t, a.cid, a.member) {
		t.Fatal("still away 31 s after the last tab closed")
	}
}

// A flapping connection: many short drops, none 30 s long, never clear away; one timer per user at
// most, whatever the number of closes, and none left once the user is back.
func TestAFKFlappingConnectionKeepsAwayAndLeaksNoTimers(t *testing.T) {
	a := newAwayEnv(t)
	a.markAway(t, a.cid, a.member, 100)
	for i := range 60 {
		a.setOffline(a.member, true)
		a.calls.AFKOffline(t.Context(), a.member)
		if n := a.calls.AFKOfflineWaitersForTest(); n != 1 {
			t.Fatalf("close %d: %d offline waits, want 1", i, n)
		}
		if i%3 == 2 {
			// A drop of 20 s, then 15 s back: the grace ends while connected and the wait ends.
			a.e.Clk.Advance(20 * time.Second)
			a.setOffline(a.member, false)
			a.e.Clk.Advance(15 * time.Second)
			waitOffline(t, a.calls)
			continue
		}
		// A drop of 25 s, then 1 s back: the next close restarts the grace.
		a.e.Clk.Advance(25 * time.Second)
		a.setOffline(a.member, false)
		a.e.Clk.Advance(time.Second)
	}
	a.e.Clk.Advance(31 * time.Second)
	waitOffline(t, a.calls)
	if n := a.calls.AFKOfflineWaitersForTest(); n != 0 {
		t.Fatalf("%d offline waits left after the user came back", n)
	}
	if !a.isAway(t, a.cid, a.member) {
		t.Fatal("a flapping connection cleared away")
	}
	if sent := a.away.sent(a.cid, a.member); len(sent) != 0 {
		t.Fatalf("op 55 sent while flapping: %v", sent)
	}
	// It still works afterwards.
	a.setOffline(a.member, true)
	a.calls.AFKOffline(t.Context(), a.member)
	a.e.Clk.Advance(31 * time.Second)
	waitOffline(t, a.calls)
	if a.isAway(t, a.cid, a.member) {
		t.Fatal("still away 31 s after the last close that followed the flapping")
	}
}

// A shutdown during the grace clears nothing: StopAFK ends the wait, and a grace that ends while the
// gateway drains (every user looks disconnected) clears nothing either.
func TestAFKShutdownDuringTheOfflineGraceClearsNothing(t *testing.T) {
	t.Run("stop", func(t *testing.T) {
		a := newAwayEnv(t)
		a.markAway(t, a.cid, a.member, 100)
		a.setOffline(a.member, true)
		a.calls.AFKOffline(t.Context(), a.member)
		a.e.Clk.Advance(10 * time.Second)
		a.calls.StopAFK()
		a.e.Clk.Advance(30 * time.Second)
		if n := a.calls.AFKOfflineWaitersForTest(); n != 0 {
			t.Fatalf("%d offline waits outlived StopAFK", n)
		}
		if !a.isAway(t, a.cid, a.member) {
			t.Fatal("a grace cut short by shutdown cleared a row")
		}
		a.calls.AFKOffline(t.Context(), a.member) // after the stop: starts nothing
		if n := a.calls.AFKOfflineWaitersForTest(); n != 0 {
			t.Fatal("an offline wait started after StopAFK")
		}
	})
	t.Run("draining", func(t *testing.T) {
		a := newAwayEnv(t)
		var draining atomic.Bool
		a.calls.WithAFKDraining(draining.Load)
		a.markAway(t, a.cid, a.member, 100)
		a.setOffline(a.member, true)
		a.calls.AFKOffline(t.Context(), a.member)
		draining.Store(true)
		a.e.Clk.Advance(31 * time.Second)
		waitOffline(t, a.calls)
		if !a.isAway(t, a.cid, a.member) {
			t.Fatal("a grace that ended while the gateway drains cleared a row")
		}
	})
}

// R3-M1: PUT …/afk/away also gives pending 2 to a device of the user that was admitted to a call in
// the last two minutes and that no room lists now (its call is recovering after the SFU dropped
// it), so its recovery reads 2 and does not rejoin. An older admission, and the device that sent
// the PUT, get nothing.
func TestAFKPutAwayRecordsChosenPendingForARecoveringDevice(t *testing.T) {
	a := newAwayEnv(t)
	phone := secondDevice(t, a.e, a.member, "member-phone")
	tablet := secondDevice(t, a.e, a.member, "member-tablet")
	a.calls.Admitted(t.Context(), a.room, tablet, false) // three minutes ago
	a.e.Clk.Advance(2 * time.Minute)
	a.calls.Admitted(t.Context(), a.room, phone, false)       // a minute ago
	a.calls.Admitted(t.Context(), a.room, a.memberDev, false) // the laptop, which then left
	a.e.Clk.Advance(time.Minute)
	ownerDev := deviceOf(t, a.e, a.ownerTok)
	a.stub.setPresent(a.room, &livekit.ParticipantInfo{Identity: ownerDev.String()}) // the SFU dropped the phone
	if status, body := a.e.Do(http.MethodPut, "/v1/communities/"+a.cid.String()+"/afk/away", a.memberTok, []any{}); status != http.StatusNoContent {
		t.Fatalf("PUT …/afk/away = %d (%x)", status, body)
	}
	if got := a.pending(t, "member-phone"); got != 2 {
		t.Fatalf("the recovering phone's pending = %d, want 2 (chose AFK)", got)
	}
	if got := a.pending(t, "member-tablet"); got != 0 {
		t.Fatalf("a device admitted three minutes ago got pending %d, want 0", got)
	}
	if got := a.pending(t, a.memberTok); got != 0 {
		t.Fatalf("the device that chose AFK got pending %d, want 0", got)
	}
	if !a.isAway(t, a.cid, a.member) {
		t.Fatal("PUT did not mark the member away")
	}
}

// R3-n1: the PUT and the sweep record pending BEFORE they ask the SFU to cut, so a client whose
// read reaches the instance while its removal is still under way already sees the reason. A sweep
// whose removal fails leaves no pending behind.
func TestAFKPendingIsRecordedBeforeTheCut(t *testing.T) {
	t.Run("put", func(t *testing.T) {
		a := newAwayEnv(t)
		a.stub.setPresent(a.room, &livekit.ParticipantInfo{Identity: a.memberDev.String()})
		var during []uint64
		a.stub.beforeRemove = func(_ string, dev id.ID) {
			if dev == a.memberDev {
				during = append(during, a.pending(t, a.memberTok))
			}
		}
		if status, _ := a.e.Do(http.MethodPut, "/v1/communities/"+a.cid.String()+"/afk/away", a.memberTok, []any{}); status != http.StatusNoContent {
			t.Fatalf("PUT …/afk/away = %d", status)
		}
		if len(during) != 1 || during[0] != 2 {
			t.Fatalf("pending read during the cut = %v, want [2]", during)
		}
	})
	t.Run("sweep", func(t *testing.T) {
		a := newAwayEnv(t)
		var during []uint64
		a.stub.beforeRemove = func(_ string, dev id.ID) {
			if dev == a.memberDev {
				during = append(during, a.pending(t, a.memberTok))
			}
		}
		a.idleMove(t)
		if len(during) != 1 || during[0] != 1 {
			t.Fatalf("pending read during the sweep's cut = %v, want [1]", during)
		}
	})
	t.Run("sweep cut fails", func(t *testing.T) {
		p := newPendingEnv(t)
		p.calls.Admitted(t.Context(), p.room, p.memberDev, false)
		p.e.Clk.Advance(61 * time.Second)
		p.stub.mu.Lock()
		p.stub.failRemovals = 1
		p.stub.mu.Unlock()
		p.calls.SweepAFK(t.Context())
		if got := p.pending(t, p.memberTok); got != 0 {
			t.Fatalf("pending after a failed cut = %d, want 0", got)
		}
	})
}

// R3-n2: the PUT marks away first and then cuts; a cut that fails is logged, the device keeps
// pending 2, the other devices are cut and the request answers 204.
func TestAFKPutAwayWithAFailedCutStillMarksAndAnswers204(t *testing.T) {
	a := newAwayEnv(t)
	phone := secondDevice(t, a.e, a.member, "member-phone")
	otherRoom := id.New().String() + "-1-0ther001"
	a.stub.setPresent(a.room, &livekit.ParticipantInfo{Identity: a.memberDev.String()})
	a.stub.setPresent(otherRoom, &livekit.ParticipantInfo{Identity: phone.String()})
	awayAtCut := map[id.ID]bool{}
	var mu sync.Mutex
	a.stub.beforeRemove = func(_ string, dev id.ID) {
		mu.Lock()
		defer mu.Unlock()
		awayAtCut[dev] = a.isAway(t, a.cid, a.member)
	}
	a.stub.mu.Lock()
	a.stub.failRemovals = 1 // the first cut fails
	a.stub.mu.Unlock()
	if status, body := a.e.Do(http.MethodPut, "/v1/communities/"+a.cid.String()+"/afk/away", a.memberTok, []any{}); status != http.StatusNoContent {
		t.Fatalf("PUT …/afk/away with a failed cut = %d (%x), want 204", status, body)
	}
	if !removedDevice(a.stub, a.room, a.memberDev) || !removedDevice(a.stub, otherRoom, phone) {
		t.Fatal("a failed cut stopped the other device's cut")
	}
	for dev, away := range awayAtCut {
		if !away {
			t.Fatalf("device %s was cut before the user was marked away", dev)
		}
	}
	if !a.isAway(t, a.cid, a.member) {
		t.Fatal("the member is not away after a partly failed PUT")
	}
	for _, tok := range []string{a.memberTok, "member-phone"} {
		if got := a.pending(t, tok); got != 2 {
			t.Fatalf("%s's pending = %d, want 2", tok, got)
		}
	}
}

// R3-n3: an admission clears the device's pending in the same afkMu section that records it, so no
// read sees the old reason once the admission is recorded. A reader polls both together while the
// admission lands.
func TestAFKAdmissionClearsPendingAtomically(t *testing.T) {
	a := newAwayEnv(t)
	a.idleMove(t)
	if got := a.pending(t, a.memberTok); got != 1 {
		t.Fatalf("pending after the sweep = %d, want 1", got)
	}
	_, before := a.calls.AFKAdmissionStateForTest(a.member, a.memberDev)
	stop := make(chan struct{})
	var bad atomic.Value
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			pending, seq := a.calls.AFKAdmissionStateForTest(a.member, a.memberDev)
			if seq > before && pending != 0 {
				bad.Store([2]uint64{pending, seq})
			}
		}
	}()
	a.calls.Admitted(t.Context(), a.room, a.memberDev, false)
	close(stop)
	<-done
	if v := bad.Load(); v != nil {
		t.Fatalf("a read saw pending and the new admission together: %v", v)
	}
	if got := a.pending(t, a.memberTok); got != 0 {
		t.Fatalf("pending after the admission = %d, want 0", got)
	}
}
