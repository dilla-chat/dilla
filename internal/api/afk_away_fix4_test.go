package api_test

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/id"
)

// AW2 fix round 4: the final re-review's A-N1, and a pending move that outlives a long sleep.

// A-N1: while the index is unseeded, a clear whose store lookup runs while a mark is in flight
// (noted and checked, its row not yet written) must not skip: the mark's index entry makes it take
// awayMu, so it clears after the mark commits. An admission, a last disconnect and a typing op each
// leave no row behind.
func TestAFKUnseededClearHonoursAnInFlightMark(t *testing.T) {
	clears := map[string]func(a *awayEnv, c *api.Calls){
		"admission": func(a *awayEnv, c *api.Calls) { c.Admitted(t.Context(), a.room, a.memberDev, false) },
		// Q2 as revised: the last connection closes during the mark, and the grace ends while the
		// mark still holds awayMu; the clear runs after the mark's write.
		"offline": func(a *awayEnv, c *api.Calls) {
			a.setOffline(a.member, true)
			c.AFKOffline(t.Context(), a.member)
			a.e.Clk.Advance(31 * time.Second)
			waitOffline(t, c)
		},
		"typing": func(a *awayEnv, c *api.Calls) { c.AFKActiveInChannel(t.Context(), a.member, a.ch) },
	}
	for name, clear := range clears {
		t.Run(name, func(t *testing.T) {
			a := newAwayEnv(t)
			probe := newAwayProbe(a.e.Repo)
			c := unseededCalls(t, a, probe)
			if c.AwaySeededForTest() {
				t.Fatal("the index is seeded; the test needs it unseeded")
			}
			cleared := make(chan struct{})
			var once sync.Once
			c.SetAFKHookForTest(func(stage string) {
				if stage != "mark-checked" {
					return
				}
				once.Do(func() {
					go func() {
						defer close(cleared)
						clear(a, c)
					}()
					// The clear either returns at once (its lookup missed the in-flight mark) or waits
					// for the mark on awayMu; give it the time to do the former.
					select {
					case <-cleared:
					case <-time.After(200 * time.Millisecond):
					}
				})
			})
			c.MarkAwayForTest(t.Context(), a.cid, a.member)
			select {
			case <-cleared:
			case <-time.After(5 * time.Second):
				t.Fatal("the clear never finished")
			}
			c.FlushAFKAwayForTest()
			if a.isAway(t, a.cid, a.member) {
				t.Fatalf("the %s's clear missed the in-flight mark: the member is listed as away", name)
			}
		})
	}
}

// A sweep's pending move (reason 1) survives a sleep longer than the old ten minutes and a gateway
// disconnect, and a read does not consume it: a client that reads twice on wake (a reload and a
// fresh read) sees it both times.
func TestAFKPendingSurvivesALongSleepAndRepeatedReads(t *testing.T) {
	a := newAwayEnv(t)
	a.idleMove(t)
	a.setOffline(a.member, true)
	a.calls.AFKOffline(t.Context(), a.member) // the laptop sleeps: its last connection closes
	a.e.Clk.Advance(11 * time.Minute)
	a.calls.SweepAFK(t.Context())
	a.setOffline(a.member, false)
	if got := a.pending(t, a.memberTok); got != 1 {
		t.Fatalf("pending after an 11-minute sleep = %d, want 1 (moved by the sweep)", got)
	}
	if got := a.pending(t, a.memberTok); got != 1 {
		t.Fatalf("pending on a second read = %d, want 1 (a read does not consume it)", got)
	}
}

// PUT …/afk/away records pending 2 (chose AFK) for every device it cuts, wherever it was: a call in
// this community, a call in another community, a DM call. Each device's read of its own community
// reports 2, twice; an admission of one device clears its pending alone.
func TestAFKPutAwayRecordsChosenPendingForEveryCutDevice(t *testing.T) {
	a := newAwayEnv(t)
	other := createCommunity(t, a.e, a.ownerTok)
	joinCommunity(t, a.e, other, a.memberTok)
	phone := secondDevice(t, a.e, a.member, "member-phone")
	tablet := secondDevice(t, a.e, a.member, "member-tablet")
	otherRoom := id.New().String() + "-1-0ther001" // a call in the other community
	dmRoom := id.New().String() + "-1-d0000001"    // a DM call
	a.stub.setPresent(a.room, &livekit.ParticipantInfo{Identity: a.memberDev.String()})
	a.stub.setPresent(otherRoom, &livekit.ParticipantInfo{Identity: phone.String()})
	a.stub.setPresent(dmRoom, &livekit.ParticipantInfo{Identity: tablet.String() + "#screen"})
	if status, body := a.e.Do(http.MethodPut, "/v1/communities/"+a.cid.String()+"/afk/away", a.memberTok, []any{}); status != http.StatusNoContent {
		t.Fatalf("PUT …/afk/away = %d (%x)", status, body)
	}
	for _, c := range []struct {
		room string
		dev  id.ID
	}{{a.room, a.memberDev}, {otherRoom, phone}, {dmRoom, tablet}} {
		if !removedDevice(a.stub, c.room, c.dev) {
			t.Fatalf("device %s was not cut from %s", c.dev, c.room)
		}
	}
	reads := []struct {
		name string
		cid  id.ID
		tok  string
	}{{"laptop", a.cid, a.memberTok}, {"phone (other community)", other, "member-phone"}, {"tablet (DM call)", a.cid, "member-tablet"}}
	for range 2 {
		for _, r := range reads {
			if _, _, _, got := afkSnapshotOf(t, a.e, r.cid, r.tok); got != 2 {
				t.Fatalf("the %s's pending = %d, want 2 (chose AFK)", r.name, got)
			}
		}
	}
	a.calls.Admitted(t.Context(), a.room, phone, false)
	if _, _, _, got := afkSnapshotOf(t, a.e, other, "member-phone"); got != 0 {
		t.Fatalf("the phone's pending after its admission = %d, want 0", got)
	}
	if got := a.pending(t, a.memberTok); got != 2 {
		t.Fatalf("the laptop's pending after the phone's admission = %d, want 2", got)
	}
}

// A device not in any call is not cut and gets no pending.
func TestAFKPutAwayLeavesAnUncutDeviceWithoutPending(t *testing.T) {
	a := newAwayEnv(t)
	secondDevice(t, a.e, a.member, "member-phone")
	if status, _ := a.e.Do(http.MethodPut, "/v1/communities/"+a.cid.String()+"/afk/away", a.memberTok, []any{}); status != http.StatusNoContent {
		t.Fatalf("PUT …/afk/away = %d", status)
	}
	if got := a.pending(t, "member-phone"); got != 0 {
		t.Fatalf("an uncut device's pending = %d, want 0", got)
	}
}

// The upper bound: a move nobody answered is dropped after 24 hours.
func TestAFKPendingExpiresAfterADay(t *testing.T) {
	p := newPendingEnv(t)
	p.idleMove(t)
	p.e.Clk.Advance(24*time.Hour + time.Second)
	p.calls.SweepAFK(t.Context())
	if got := p.pending(t, p.memberTok); got != 0 {
		t.Fatalf("pending after a day = %d, want 0", got)
	}
}
