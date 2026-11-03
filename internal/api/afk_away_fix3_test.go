package api_test

import (
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/id"
)

// AW2/AW3 fix round 3: the second re-review's N-A, N-B and N-C.

// N-A: a seed that merges while a mark that will be skipped holds its index entry must not lose the
// entry of a user whose row exists: the merge runs under awayMu, so it lands after the mark's undo.
func TestAFKSeedMergeSurvivesASkippedMarksUndo(t *testing.T) {
	a := newAwayEnv(t)
	a.markAway(t, a.cid, a.member, 100) // the stored row; c's index is not seeded
	probe := newAwayProbe(a.e.Repo)
	c := unseededCalls(t, a, probe)
	a.setOffline(a.member, true) // the mark will be skipped
	seeded := make(chan struct{})
	var once sync.Once
	c.SetAFKHookForTest(func(stage string) {
		if stage != "mark-noted" {
			return
		}
		once.Do(func() {
			go func() {
				defer close(seeded)
				if err := c.SeedAFKAway(t.Context()); err != nil {
					t.Error(err)
				}
			}()
			select {
			case <-seeded:
			case <-time.After(200 * time.Millisecond):
			}
		})
	})
	c.MarkAwayForTest(t.Context(), a.cid, a.member)
	select {
	case <-seeded:
	case <-time.After(5 * time.Second):
		t.Fatal("the seed never finished")
	}
	if !c.AwaySeededForTest() {
		t.Fatal("not seeded")
	}
	a.setOffline(a.member, false)
	c.AFKActiveInChannel(t.Context(), a.member, a.ch)
	if a.isAway(t, a.cid, a.member) {
		t.Fatal("the seeded index lost the member's entry: typing did not clear the stored row")
	}
}

// N-B: until the seed lands, a user with no row is looked up outside awayMu: their typing neither
// queues behind another user's clear nor runs a DELETE.
func TestAFKUnseededTypingOfAUserWithNoRowNeverQueues(t *testing.T) {
	a := newAwayEnv(t)
	a.markAway(t, a.cid, a.owner, 100)
	probe := newAwayProbe(a.e.Repo)
	c := unseededCalls(t, a, probe)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	probe.onClear = func(u id.ID) {
		if u == a.owner {
			once.Do(func() {
				close(entered)
				<-release
			})
		}
	}
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	ownerDone := make(chan struct{})
	go func() {
		defer close(ownerDone)
		c.AFKActiveInChannel(t.Context(), a.owner, a.ch) // a real clear, held inside its DELETE
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the owner's clear never reached its DELETE")
	}
	before := probe.clearsOf("ClearAFKAway")
	returnsWithin(t, 2*time.Second, "typing of a user with no row", func() {
		c.AFKActiveInChannel(t.Context(), a.member, a.ch)
	})
	if n := probe.clearsOf("ClearAFKAway") - before; n != 0 {
		t.Fatalf("typing of a user with no row ran %d DELETEs, want none", n)
	}
	close(release)
	<-ownerDone
	c.FlushAFKAwayForTest()
	if a.isAway(t, a.cid, a.owner) {
		t.Fatal("the owner's clear did not land (control)")
	}
}

// N-C: one drain batch resolves each community's audience once, and the notes of one (community,
// user) in it collapse to the last: a member cleared, marked and cleared again while the drainer was
// busy is told "cleared" once per recipient.
func TestAFKDrainBatchResolvesOnceAndSendsTheFinalState(t *testing.T) {
	a := newAwayEnv(t)
	_, otherTok := a.e.NewUser("third")
	joinCommunity(t, a.e, a.cid, otherTok)
	third := a.e.sess[otherTok].UserID
	a.markAway(t, a.cid, a.owner, 100)
	a.markAway(t, a.cid, a.member, 100)
	probe := newAwayProbe(a.e.Repo)
	c := probeCalls(t, a, probe, a.e.Clk)
	entered, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	reads := 0
	var once sync.Once
	probe.onMembers = func() {
		mu.Lock()
		reads++
		mu.Unlock()
		once.Do(func() {
			close(entered)
			<-release
		})
	}
	c.AFKActiveInChannel(t.Context(), a.owner, a.ch) // the drainer takes this note and blocks on its audience
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the drainer never resolved an audience")
	}
	c.AFKActiveInChannel(t.Context(), a.member, a.ch) // member: since 0
	c.MarkAwayForTest(t.Context(), a.cid, a.member)   // member: since now
	c.AFKActiveInChannel(t.Context(), a.member, a.ch) // member: since 0 again
	c.MarkAwayForTest(t.Context(), a.cid, third)      // third: since now
	mu.Lock()
	readsBefore := reads
	mu.Unlock()
	close(release)
	c.FlushAFKAwayForTest()
	mu.Lock()
	extra := reads - readsBefore
	mu.Unlock()
	// EligibleUsers pages the members; one resolution here is one page read.
	if extra != 1 {
		t.Fatalf("the second batch resolved the community's audience %d times, want once", extra)
	}
	sent := a.away.sent(a.cid, a.member)
	if len(sent) != 3 {
		t.Fatalf("member op 55 = %v, want one frame per recipient (owner, member, third)", sent)
	}
	for _, f := range sent {
		if f.since != 0 {
			t.Fatalf("member op 55 %v: want only the final state, since 0", sent)
		}
	}
	if len(a.away.sent(a.cid, third)) != 3 {
		t.Fatalf("third's mark: op 55 = %v, want one per recipient", a.away.sent(a.cid, third))
	}
}
