package api_test

import (
	"sync"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// AW2/AW3 fix round 2: the re-review's R1..R5.

// within runs f and fails the test when it does not return within d.
func returnsWithin(t *testing.T, d time.Duration, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %s", what, d)
	}
}

// R1: while one clear's op 55 audience read is held open, a second clear and many lookups of users
// who are not away (typing) all return: nothing waits on the send or on awayMu behind it.
func TestAFKAwaySendsNeverBlockLookupsOrOtherClears(t *testing.T) {
	a := newAwayEnv(t)
	a.markAway(t, a.cid, a.member, 100)
	a.markAway(t, a.cid, a.owner, 100)
	probe := newAwayProbe(a.e.Repo)
	c := probeCalls(t, a, probe, a.e.Clk)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	probe.onMembers = func() {
		once.Do(func() {
			close(entered)
			<-release
		})
	}
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	go c.AFKActiveInChannel(t.Context(), a.member, a.ch) // clear A: its audience read blocks
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("clear A never resolved its audience")
	}
	returnsWithin(t, 2*time.Second, "a second clear", func() { c.AFKActiveInChannel(t.Context(), a.owner, a.ch) })
	returnsWithin(t, 2*time.Second, "typing of users who are not away", func() {
		var wg sync.WaitGroup
		for range 64 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c.AFKActiveInChannel(t.Context(), id.New(), a.ch)
			}()
		}
		wg.Wait()
	})
	close(release)
	for _, u := range []id.ID{a.member, a.owner} {
		if a.isAway(t, a.cid, u) {
			t.Fatalf("%s is still away", u)
		}
		if sent := a.away.sent(a.cid, u); len(sent) == 0 || sent[len(sent)-1].since != 0 {
			t.Fatalf("op 55 for %s = %v, want since 0", u, sent)
		}
	}
}

// R1: the drainer stops on StopAFK, with a send in flight, and StopAFK returns.
func TestAFKStopEndsTheDrainer(t *testing.T) {
	a := newAwayEnv(t)
	a.markAway(t, a.cid, a.member, 100)
	probe := newAwayProbe(a.e.Repo)
	c := probeCalls(t, a, probe, a.e.Clk)
	entered := make(chan struct{})
	var once sync.Once
	probe.onMembers = func() { once.Do(func() { close(entered) }) }
	c.AFKActiveInChannel(t.Context(), a.member, a.ch)
	<-entered
	returnsWithin(t, 5*time.Second, "StopAFK", c.StopAFK)
}

// R2: a leave's announcement racing a rejoin and a mark: the membership read and the index drop
// happen under awayMu, so the mark that follows is never undone; the row, the index and the last
// op 55 agree.
func TestAFKMemberRemovedRacingARejoinAndAMark(t *testing.T) {
	a := newAwayEnv(t)
	a.markAway(t, a.cid, a.member, 100)
	probe := newAwayProbe(a.e.Repo)
	c := probeCalls(t, a, probe, a.e.Clk)
	if err := a.e.Repo.DeleteMember(t.Context(), a.cid, a.member); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	probe.onGetMember = func() {
		once.Do(func() {
			done := make(chan struct{})
			go func() {
				defer close(done)
				if err := a.e.Repo.PutMember(t.Context(), store.MemberOfCommunityRow{CommunityID: a.cid, UserID: a.member, Joined: 2}); err != nil {
					t.Error(err)
				}
				c.MarkAwayForTest(t.Context(), a.cid, a.member)
			}()
			select {
			case <-done:
			case <-time.After(200 * time.Millisecond):
			}
		})
	}
	c.AFKMemberRemoved(t.Context(), a.cid, a.member)
	c.FlushAFKAwayForTest()
	// Let a mark that waited on the lock land.
	deadline := time.Now().Add(5 * time.Second)
	for !a.isAway(t, a.cid, a.member) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !a.isAway(t, a.cid, a.member) {
		t.Fatal("the mark after the rejoin left no row")
	}
	sent := a.away.sent(a.cid, a.member)
	if len(sent) == 0 || sent[len(sent)-1].since == 0 {
		t.Fatalf("op 55 sequence %v: the last frame says cleared while the row exists", sent)
	}
	// The index knows the row: the member's typing clears it.
	c.AFKActiveInChannel(t.Context(), a.member, a.ch)
	if a.isAway(t, a.cid, a.member) {
		t.Fatal("the index lost the row: typing did not clear it")
	}
}

// R3: a Shutdown during the grace pass clears no rows: StopAFK cancels the pass and waits for it.
func TestAFKShutdownDuringTheGracePassClearsNoRows(t *testing.T) {
	a := newAwayEnv(t)
	a.markAway(t, a.cid, a.member, 100)
	probe := newAwayProbe(a.e.Repo)
	c := probeCalls(t, a, probe, a.e.Clk)
	if _, err := c.EndAFKCalls(t.Context()); err != nil {
		t.Fatal(err)
	}
	a.setOffline(a.member, true)
	a.e.Clk.Advance(61 * time.Second)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	probe.onList = func() {
		once.Do(func() {
			close(entered)
			<-release
		})
	}
	c.GoAFKMaintenance()
	<-entered
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		c.StopAFK()
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("StopAFK did not return")
	}
	if !a.isAway(t, a.cid, a.member) {
		t.Fatal("a pass cut short by shutdown cleared a row")
	}
	c.GoAFKMaintenance() // after stop: starts nothing
	if !a.isAway(t, a.cid, a.member) {
		t.Fatal("a pass ran after StopAFK")
	}
}

// R3: while the gateway drains, every user looks disconnected; the grace pass clears nothing.
func TestAFKGracePassSkipsWhileTheGatewayDrains(t *testing.T) {
	a := newAwayEnv(t)
	a.markAway(t, a.cid, a.member, 100)
	c := probeCalls(t, a, a.e.Repo, a.e.Clk)
	c.WithAFKDraining(func() bool { return true })
	if _, err := c.EndAFKCalls(t.Context()); err != nil {
		t.Fatal(err)
	}
	a.setOffline(a.member, true)
	a.e.Clk.Advance(61 * time.Second)
	c.AFKMaintenance(t.Context())
	if !a.isAway(t, a.cid, a.member) {
		t.Fatal("the grace pass cleared a row while the gateway drains")
	}
}

// retriedAFKCall seeds a call left live in the AFK channel with the member's device and a Calls
// whose first end of it fails.
func retriedAFKCall(t *testing.T, a *awayEnv) (*api.Calls, id.ID) {
	t.Helper()
	group := seedCallGroup(t, a.e, a.afk, a.cid, callGroupEpoch)
	seedLeaf(t, a.e, group, a.memberDev, 3, nil)
	call, _, _ := cutRoom(legacyAFKCall(t, a, group))
	probe := newAwayProbe(a.e.Repo)
	probe.failGetVS = 1
	c := probeCalls(t, a, probe, a.e.Clk)
	if n, _ := c.EndAFKCalls(t.Context()); n != 0 {
		t.Fatalf("EndAFKCalls ended %d calls with the failure injected", n)
	}
	return c, call
}

// R4: a retried end marks like the sweep: never a user with no gateway connection...
func TestAFKRetriedEndDoesNotMarkAnOfflineUser(t *testing.T) {
	a := newAwayEnv(t)
	a.stub.setPresent(a.room, &livekit.ParticipantInfo{Identity: deviceOf(t, a.e, a.ownerTok).String()})
	c, call := retriedAFKCall(t, a)
	a.setOffline(a.member, true)
	c.AFKMaintenance(t.Context())
	if row, err := a.e.Repo.GetVoiceSession(t.Context(), call); err != nil || row.Ended == nil {
		t.Fatalf("the retry did not end the call: %+v, %v", row, err)
	}
	if a.isAway(t, a.cid, a.member) {
		t.Fatal("a retried end marked an offline user away")
	}
}

// R4: ...and never a user with a device in another call (R-AW2).
func TestAFKRetriedEndDoesNotMarkAUserInACall(t *testing.T) {
	a := newAwayEnv(t)
	// The member's device is in the voice call in a.room (membershipCallEnv's presence).
	c, call := retriedAFKCall(t, a)
	c.AFKMaintenance(t.Context())
	if row, err := a.e.Repo.GetVoiceSession(t.Context(), call); err != nil || row.Ended == nil {
		t.Fatalf("the retry did not end the call: %+v, %v", row, err)
	}
	if a.isAway(t, a.cid, a.member) {
		t.Fatal("a retried end marked a user who is in a call")
	}
}

// R5: a failed seed is retried with backoff; until it lands every user may be away (the store
// path), and a removal of a user the seed never loaded sends no op 55.
func TestAFKFailedSeedIsRetriedWithBackoff(t *testing.T) {
	a := newAwayEnv(t)
	probe := newAwayProbe(a.e.Repo)
	probe.failList = 1
	c := unseededCalls(t, a, probe)
	if _, err := c.EndAFKCalls(t.Context()); err != nil {
		t.Fatal(err)
	}
	if c.AwaySeededForTest() {
		t.Fatal("seeded although the roster read failed")
	}
	stranger := id.New()
	c.AFKMemberRemoved(t.Context(), a.cid, stranger)
	if n := len(a.away.sent(a.cid, stranger)); n != 0 {
		t.Fatalf("an unseeded removal of a user never loaded sent %d op 55", n)
	}
	calls := probe.listCalls
	c.AFKMaintenance(t.Context()) // inside the backoff: no retry yet
	if probe.listCalls != calls || c.AwaySeededForTest() {
		t.Fatal("the seed was retried inside its backoff")
	}
	a.e.Clk.Advance(time.Minute)
	c.AFKMaintenance(t.Context())
	if !c.AwaySeededForTest() {
		t.Fatal("the seed was not retried after its backoff")
	}
}
