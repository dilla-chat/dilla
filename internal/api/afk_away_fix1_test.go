package api_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// AW2/AW3 fix round 1: the review's I1 and M1..M8.

// awayProbeRepo counts the store calls of the away clears and runs hooks inside the reads the
// lock tests care about.
type awayProbeRepo struct {
	store.Repository
	mu        sync.Mutex
	counts    map[string]int
	onMembers func() // inside ListMembersOfCommunity (EligibleUsers: the op 55 audience)
	onList    func() // inside ListAFKAway (the post-boot walk)
	failGetVS int    // GetVoiceSession failures left (an AFK call's end at boot)
	failList  int    // ListAFKAway failures left (the seed)
	listCalls int    // ListAFKAway calls
	// onGetMember runs inside GetMember after the read, before it answers (fix round 2, R2).
	onGetMember func()
	// onClear runs inside ClearAFKAway, before the delete (fix round 3, N-B).
	onClear func(user id.ID)
}

func (p *awayProbeRepo) GetMember(ctx context.Context, cid, uid id.ID) (store.MemberOfCommunityRow, error) {
	row, err := p.Repository.GetMember(ctx, cid, uid)
	if f := p.onGetMember; f != nil {
		f()
	}
	return row, err
}

func newAwayProbe(r store.Repository) *awayProbeRepo {
	return &awayProbeRepo{Repository: r, counts: map[string]int{}}
}

func (p *awayProbeRepo) count(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.counts[name]++
}

func (p *awayProbeRepo) total() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.counts {
		n += c
	}
	return n
}

func (p *awayProbeRepo) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.counts = map[string]int{}
}

func (p *awayProbeRepo) GetChannel(ctx context.Context, ch id.ID) (store.ChannelRow, error) {
	p.count("GetChannel")
	return p.Repository.GetChannel(ctx, ch)
}

func (p *awayProbeRepo) GetGroup(ctx context.Context, g id.ID) (store.GroupRow, error) {
	p.count("GetGroup")
	return p.Repository.GetGroup(ctx, g)
}

func (p *awayProbeRepo) ClearAFKAway(ctx context.Context, cid, uid id.ID) (bool, error) {
	p.count("ClearAFKAway")
	if f := p.onClear; f != nil {
		f(uid)
	}
	return p.Repository.ClearAFKAway(ctx, cid, uid)
}

func (p *awayProbeRepo) clearsOf(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.counts[name]
}

func (p *awayProbeRepo) ClearAFKAwayUser(ctx context.Context, uid id.ID) ([]id.ID, error) {
	p.count("ClearAFKAwayUser")
	return p.Repository.ClearAFKAwayUser(ctx, uid)
}

func (p *awayProbeRepo) ListMembersOfCommunity(ctx context.Context, cid, after id.ID, limit int32) ([]store.MemberOfCommunityRow, error) {
	if f := p.onMembers; f != nil {
		f()
	}
	return p.Repository.ListMembersOfCommunity(ctx, cid, after, limit)
}

func (p *awayProbeRepo) ListAFKAway(ctx context.Context, cid id.ID) ([]store.AFKAway, error) {
	if f := p.onList; f != nil {
		f()
	}
	p.mu.Lock()
	p.listCalls++
	fail := p.failList > 0
	if fail {
		p.failList--
	}
	p.mu.Unlock()
	if fail {
		return nil, errors.New("injected ListAFKAway failure")
	}
	return p.Repository.ListAFKAway(ctx, cid)
}

func (p *awayProbeRepo) GetVoiceSession(ctx context.Context, call id.ID) (store.VoiceSessionRow, error) {
	p.mu.Lock()
	fail := p.failGetVS > 0
	if fail {
		p.failGetVS--
	}
	p.mu.Unlock()
	if fail {
		return store.VoiceSessionRow{}, errors.New("injected GetVoiceSession failure")
	}
	return p.Repository.GetVoiceSession(ctx, call)
}

// probeCalls is a second call route group over probe, sharing a's database, SFU and op 55 recorder.
func probeCalls(t *testing.T, a *awayEnv, probe store.Repository, clk clock.Clock) *api.Calls {
	t.Helper()
	c := api.NewCalls(probe, api.NewResolver(a.e.Repo), a.stub, api.CallsConfig{LiveKitURL: testLiveKitURL}, clk,
		slog.New(slog.DiscardHandler)).WithAFKAway(a.away, a.connected)
	t.Cleanup(c.StopAFK)
	a.away.addFlush(c.FlushAFKAwayForTest)
	if err := c.SeedAFKAway(t.Context()); err != nil {
		t.Fatal(err)
	}
	return c
}

// unseededCalls is probeCalls without the seed.
func unseededCalls(t *testing.T, a *awayEnv, probe store.Repository) *api.Calls {
	t.Helper()
	c := api.NewCalls(probe, api.NewResolver(a.e.Repo), a.stub, api.CallsConfig{LiveKitURL: testLiveKitURL}, a.e.Clk,
		slog.New(slog.DiscardHandler)).WithAFKAway(a.away, a.connected)
	t.Cleanup(c.StopAFK)
	a.away.addFlush(c.FlushAFKAwayForTest)
	return c
}

// I1: a typing op, an upload or an admission by a user who is not away is a map lookup, never a
// store call.
func TestAFKActivityOfAUserWhoIsNotAwayTouchesNoStore(t *testing.T) {
	a := newAwayEnv(t)
	probe := newAwayProbe(a.e.Repo)
	c := probeCalls(t, a, probe, a.e.Clk)
	probe.reset()
	c.AFKActiveInChannel(t.Context(), a.member, a.ch)
	c.AFKActiveInGroup(t.Context(), a.member, a.group)
	c.AFKOffline(t.Context(), a.member)
	if n := probe.total(); n != 0 {
		t.Fatalf("activity of a user who is not away made %d store calls (%v), want none", n, probe.counts)
	}
	// The control: an away user's typing clears the row through the store.
	a.markAway(t, a.cid, a.member, 100)
	if err := c.SeedAFKAway(t.Context()); err != nil {
		t.Fatal(err)
	}
	c.AFKActiveInChannel(t.Context(), a.member, a.ch)
	if a.isAway(t, a.cid, a.member) {
		t.Fatal("the away member's typing did not clear the row (control)")
	}
}

// I1: a clear resolves its op 55 audience with awayMu free, so other users' typing, uploads and
// admissions never wait on an O(members) read.
func TestAFKClearResolvesTheAudienceOutsideTheLock(t *testing.T) {
	a := newAwayEnv(t)
	probe := newAwayProbe(a.e.Repo)
	c := probeCalls(t, a, probe, a.e.Clk)
	a.markAway(t, a.cid, a.member, 100)
	if err := c.SeedAFKAway(t.Context()); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var free []bool
	probe.onMembers = func() {
		mu.Lock()
		defer mu.Unlock()
		free = append(free, c.AwayLockFreeForTest())
	}
	c.AFKActiveInChannel(t.Context(), a.member, a.ch)
	c.FlushAFKAwayForTest() // op 55 goes out on the drainer (fix round 2)
	mu.Lock()
	defer mu.Unlock()
	if len(free) == 0 {
		t.Fatal("the clear resolved no audience")
	}
	for _, f := range free {
		if !f {
			t.Fatal("the clear resolved the op 55 audience while holding awayMu")
		}
	}
	if len(a.away.sent(a.cid, a.member)) == 0 {
		t.Fatal("the clear sent no op 55")
	}
}

// M1: a member whose snapshot names no AFK channel is sent no away list, as op 55 reaches nobody.
func TestAFKSnapshotWithANullChannelHasNoAwayList(t *testing.T) {
	a := newAwayEnv(t)
	a.markAway(t, a.cid, a.member, 100)
	// The pointer names a channel that is no voice channel: afkDestination answers null.
	text, _, status := newChannel(t, a.e, a.cid, a.ownerTok, uint64(api.ChannelText), 0, 0, "text")
	if status != http.StatusCreated {
		t.Fatalf("text channel = %d", status)
	}
	if err := a.e.Repo.SetCommunityAFK(t.Context(), a.cid, store.CommunityAFK{ChannelID: &text, TimeoutSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	if ch, _, _, _ := afkSnapshotOf(t, a.e, a.cid, a.memberTok); ch != nil {
		t.Fatalf("snapshot channel = %v, want null", ch)
	}
	if got := afkAwayOf(t, a.e, a.cid, a.memberTok); len(got) != 0 {
		t.Fatalf("snapshot away with a null channel = %+v, want []", got)
	}
}

// tickClock advances one second on every read: a path that reads the clock twice sees two values.
type tickClock struct{ *clock.Fake }

func (c tickClock) Now() time.Time {
	now := c.Fake.Now()
	c.Advance(time.Second)
	return now
}

func (c tickClock) Since(t time.Time) time.Duration { return c.Fake.Now().Sub(t) }

// M2: op 55's since_ts is the stored since, read once.
func TestAFKAwaySinceIsReadOnce(t *testing.T) {
	a := newAwayEnv(t)
	group := seedCallGroup(t, a.e, a.afk, a.cid, callGroupEpoch)
	seedLeaf(t, a.e, group, a.memberDev, 3, nil)
	legacyAFKCall(t, a, group)
	c := probeCalls(t, a, a.e.Repo, tickClock{clock.NewFake(time.Unix(1_800_000_000, 0))})
	if _, err := c.EndAFKCalls(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows := a.roster(t, a.cid)
	sent := a.away.sent(a.cid, a.member)
	if len(rows) != 1 || len(sent) == 0 {
		t.Fatalf("roster %v, op 55 %v", rows, sent)
	}
	for _, f := range sent {
		if f.since != uint64(rows[0].Since) {
			t.Fatalf("op 55 since %d, stored since %d", f.since, rows[0].Since)
		}
	}
}

// M4: an AFK call whose end fails at boot is retried on the next maintenance tick, and its users are
// marked then.
func TestAFKCallEndFailingAtBootIsRetried(t *testing.T) {
	a := newAwayEnv(t)
	group := seedCallGroup(t, a.e, a.afk, a.cid, callGroupEpoch)
	seedLeaf(t, a.e, group, a.memberDev, 3, nil)
	room := legacyAFKCall(t, a, group)
	call, _, _ := cutRoom(room)
	// The member's device left the voice call: a retried mark checks for another call (R4).
	a.stub.setPresent(a.room, &livekit.ParticipantInfo{Identity: deviceOf(t, a.e, a.ownerTok).String()})
	probe := newAwayProbe(a.e.Repo)
	probe.failGetVS = 1
	c := probeCalls(t, a, probe, a.e.Clk)
	if n, _ := c.EndAFKCalls(t.Context()); n != 0 {
		t.Fatalf("EndAFKCalls ended %d calls with the failure injected, want 0", n)
	}
	if row, _ := a.e.Repo.GetVoiceSession(t.Context(), call); row.Ended != nil {
		t.Fatal("the call ended despite the injected failure")
	}
	c.AFKMaintenance(t.Context())
	if row, err := a.e.Repo.GetVoiceSession(t.Context(), call); err != nil || row.Ended == nil {
		t.Fatalf("the AFK call is still live after the retry tick: %+v, %v", row, err)
	}
	if !a.isAway(t, a.cid, a.member) {
		t.Fatal("the retried call's user was not marked away")
	}
}

// M5: the once-per-boot clear walks the communities without holding awayMu across the walk.
func TestAFKBootClearDoesNotHoldTheLockAcrossTheWalk(t *testing.T) {
	a := newAwayEnv(t)
	a.markAway(t, a.cid, a.member, 100)
	probe := newAwayProbe(a.e.Repo)
	c := probeCalls(t, a, probe, a.e.Clk)
	if _, err := c.EndAFKCalls(t.Context()); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var free []bool
	probe.onList = func() {
		mu.Lock()
		defer mu.Unlock()
		free = append(free, c.AwayLockFreeForTest())
	}
	a.setOffline(a.member, true)
	a.e.Clk.Advance(61 * time.Second)
	c.AFKMaintenance(t.Context())
	if a.isAway(t, a.cid, a.member) {
		t.Fatal("the disconnected member's row survived the grace")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(free) == 0 {
		t.Fatal("the walk read no roster")
	}
	for _, f := range free {
		if !f {
			t.Fatal("the walk read a roster while holding awayMu")
		}
	}
}

// M6: a kick, a leave or a ban drops the away row by cascade and tells the remaining audience with
// op 55 since 0.
func TestAFKRemovalOfAnAwayMemberIsAnnounced(t *testing.T) {
	for _, tc := range []struct {
		name string
		act  func(a *awayEnv) (int, []byte)
	}{
		{"kick", func(a *awayEnv) (int, []byte) {
			return a.e.Do(http.MethodDelete, "/v1/communities/"+a.cid.String()+"/members/"+a.member.String(), a.ownerTok, nil)
		}},
		{"leave", func(a *awayEnv) (int, []byte) {
			return a.e.Do(http.MethodPost, "/v1/communities/"+a.cid.String()+"/leave", a.memberTok, nil)
		}},
		{"ban", func(a *awayEnv) (int, []byte) {
			return a.e.Do(http.MethodPut, "/v1/communities/"+a.cid.String()+"/bans/"+a.member.String(), a.ownerTok, []any{"spam", nil})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAwayEnv(t)
			a.markAway(t, a.cid, a.member, 100)
			if status, body := tc.act(a); status != http.StatusNoContent && status != http.StatusOK {
				t.Fatalf("%s = %d (%x)", tc.name, status, body)
			}
			if a.isAway(t, a.cid, a.member) {
				t.Fatal("the row survived the removal")
			}
			toEveryone(t, a.away.sent(a.cid, a.member), []id.ID{a.owner}, 0)
		})
	}
}

// M7: the latest admission of any other device within the join window keeps the user busy, even
// when the swept device itself was admitted (a resume) after it.
func TestAFKSweepDoesNotMarkWhenAnEarlierPhoneIsStillJoining(t *testing.T) {
	a := newAwayEnv(t)
	phone := secondDevice(t, a.e, a.member, "member-phone")
	seedLeaf(t, a.e, a.group, phone, 3, nil)
	a.calls.Admitted(t.Context(), a.room, a.memberDev, false)
	a.e.Clk.Advance(50 * time.Second)
	a.calls.Admitted(t.Context(), a.room, phone, false) // not yet in the SFU's list
	a.e.Clk.Advance(5 * time.Second)
	a.calls.Admitted(t.Context(), a.room, a.memberDev, true) // a resume: the timer stays
	a.e.Clk.Advance(6 * time.Second)
	a.calls.SweepAFK(t.Context())
	if !removedDevice(a.stub, a.room, a.memberDev) {
		t.Fatal("the idle device was not cut")
	}
	if a.isAway(t, a.cid, a.member) {
		t.Fatal("the member was marked away while their phone is joining a call")
	}
}

// M8: op 55 reaches only who may view the AFK channel: a user of another community hears nothing.
func TestAFKAwayIsNotSentToAnOutsider(t *testing.T) {
	a := newAwayEnv(t)
	_, outsiderTok := a.e.NewUser("outsider")
	outsider := a.e.sess[outsiderTok].UserID
	createCommunity(t, a.e, outsiderTok)
	a.idleMove(t)
	frames := a.away.sent(a.cid, a.member)
	if len(frames) == 0 {
		t.Fatal("no op 55 at all")
	}
	for _, f := range frames {
		if f.to == outsider {
			t.Fatal("op 55 reached a user of another community")
		}
	}
	toEveryone(t, frames, []id.ID{a.owner, a.member}, frames[0].since)
}

// M8: a non-member's PUT …/afk/away is 404 and marks nothing.
func TestAFKPutAwayByANonMemberIsNotFound(t *testing.T) {
	a := newAwayEnv(t)
	_, strangerTok := a.e.NewUser("stranger")
	if status, _ := a.e.Do(http.MethodPut, "/v1/communities/"+a.cid.String()+"/afk/away", strangerTok, []any{}); status != http.StatusNotFound {
		t.Fatalf("PUT as a non-member = %d, want 404", status)
	}
	if len(a.roster(t, a.cid)) != 0 || a.away.count() != 0 {
		t.Fatal("a non-member's PUT changed the roster")
	}
}

// M8, the reverse Q2 race: the last connection closes after the mark checked it and before the write.
// The offline clear waits for the mark and then clears, so the user is not left away.
func TestAFKLastCloseRacingTheMarkStillClears(t *testing.T) {
	a := newAwayEnv(t)
	var wg sync.WaitGroup
	a.calls.SetAFKHookForTest(func(stage string) {
		if stage == "mark-checked" {
			a.setOffline(a.member, true)
			wg.Add(1)
			go func() {
				defer wg.Done()
				a.calls.AFKOffline(context.Background(), a.member)
			}()
			time.Sleep(20 * time.Millisecond) // let the offline clear reach the lock
		}
	})
	a.idleMove(t)
	wg.Wait()
	a.e.Clk.Advance(31 * time.Second) // Q2 as revised: the clear waits out a 30 s grace
	waitOffline(t, a.calls)
	if a.isAway(t, a.cid, a.member) {
		t.Fatal("the member is away after their last connection closed")
	}
	sent := a.away.sent(a.cid, a.member)
	if len(sent) < 2 || sent[len(sent)-1].since != 0 {
		t.Fatalf("op 55 sequence %v, want the mark then since 0 last", sent)
	}
}
