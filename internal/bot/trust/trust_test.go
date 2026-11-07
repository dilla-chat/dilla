package trust

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
)

func uid(b byte) id.ID {
	var v id.ID
	v[0], v[15] = b, 1
	return v
}

// leafKey is one current leaf of the fake core: a group and a leaf index.
type leafKey struct {
	group id.ID
	leaf  uint32
}

// fakeCore models the helper: per user, the pushed list (its version and revoked devices), each leaf's device and
// whether the leaf was judged under the current version. PendingLeaves reports a leaf only when its user's list
// changed since it was judged (core/dilla-core pending_leaves for a Bot); RevalidateGroup judges one leaf.
type fakeCore struct {
	mu        sync.Mutex
	version   map[id.ID]uint64
	revoked   map[id.ID]bool // device → revoked by its user's list
	leaves    map[leafKey]id.ID
	devices   map[leafKey]id.ID
	judged    map[leafKey]uint64
	refused   map[leafKey]bool
	failIn    map[id.ID]error // user → op 24 failure
	failCount map[id.ID]int   // user → remaining failures (0 = always while failIn is set)
	ingests   map[id.ID]int
	pending   int
	revalid   map[id.ID]int // user → RevalidateGroup calls
	state     map[id.ID]native.DeviceListState
	last      map[id.ID]string // user → the last applied push: the same push again changes nothing
	pendFail  int              // PendingLeaves failures to answer before succeeding
}

func newFakeCore() *fakeCore {
	return &fakeCore{version: map[id.ID]uint64{}, revoked: map[id.ID]bool{}, leaves: map[leafKey]id.ID{}, devices: map[leafKey]id.ID{},
		judged: map[leafKey]uint64{}, refused: map[leafKey]bool{}, failIn: map[id.ID]error{}, failCount: map[id.ID]int{},
		ingests: map[id.ID]int{}, revalid: map[id.ID]int{}, state: map[id.ID]native.DeviceListState{}, last: map[id.ID]string{}}
}

// member adds user's device as leaf in group, judged under the user's current list.
func (c *fakeCore) member(group id.ID, leaf uint32, user, device id.ID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := leafKey{group, leaf}
	c.leaves[k], c.devices[k] = user, device
	if c.version[user] == 0 {
		c.version[user] = 1
	}
	c.judged[k] = c.version[user]
}

func (c *fakeCore) fail(user id.ID, err error, times int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failIn[user], c.failCount[user] = err, times
}

// push is the fake's op 24 body: the revoked devices.
func push(revoked ...id.ID) []byte {
	var b []byte
	for _, d := range revoked {
		b = append(b, d[:]...)
	}
	if b == nil {
		b = []byte{0}
	}
	return b
}

func (c *fakeCore) IngestDeviceList(_ context.Context, user id.ID, body []byte) (native.DeviceListIngest, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ingests[user]++
	if err := c.failIn[user]; err != nil {
		if c.failCount[user] == 0 {
			return native.DeviceListIngest{}, err
		}
		c.failCount[user]--
		if c.failCount[user] == 0 {
			delete(c.failIn, user)
		}
		return native.DeviceListIngest{}, err
	}
	for i := 0; i+id.Size <= len(body); i += id.Size {
		var d id.ID
		copy(d[:], body[i:i+id.Size])
		c.revoked[d] = true
	}
	st := c.state[user]
	if c.last[user] == string(body) {
		return native.DeviceListIngest{Version: c.version[user], State: st}, nil
	}
	c.last[user] = string(body)
	c.version[user]++
	return native.DeviceListIngest{Changed: true, Version: c.version[user], State: st}, nil
}

func (c *fakeCore) PendingLeaves(context.Context) ([]native.GroupPendingLeaf, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending++
	if c.pendFail > 0 {
		c.pendFail--
		return nil, errors.New("pending leaves unavailable")
	}
	var out []native.GroupPendingLeaf
	for k, u := range c.leaves {
		if c.judged[k] != c.version[u] {
			out = append(out, native.GroupPendingLeaf{Group: k.group, Leaf: k.leaf, User: u, CachedVersion: c.version[u], Reason: "changed"})
		}
	}
	return out, nil
}

func (c *fakeCore) RevalidateGroup(_ context.Context, group id.ID, leaf, _ uint64) (uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := leafKey{group, uint32(leaf)}
	u := c.leaves[k]
	c.revalid[u]++
	c.judged[k] = c.version[u]
	c.refused[k] = c.revoked[c.devices[k]]
	return 0, nil
}

func (c *fakeCore) calls() (pending int, revalid map[id.ID]int, ingests map[id.ID]int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, i := map[id.ID]int{}, map[id.ID]int{}
	for k, v := range c.revalid {
		r[k] = v
	}
	for k, v := range c.ingests {
		i[k] = v
	}
	return c.pending, r, i
}

func (c *fakeCore) isRefused(k leafKey) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refused[k]
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// settled reports whether user's attempt number n has settled: rescheduled as attempt n, or given up after it.
func settled(tr *Trust, user id.ID, n int) bool {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if r := tr.retries[user]; r != nil {
		return r.attempts == n
	}
	return tr.givenUp[user] && n == tr.policy.Attempts
}

var testPolicy = RetryPolicy{First: time.Second, Max: 5 * time.Minute, Attempts: 8}

func TestDefaultRetryIsTheBriefsPolicy(t *testing.T) {
	if DefaultRetry != (RetryPolicy{First: 1 * time.Second, Max: 5 * time.Minute, Attempts: 8}) {
		t.Fatalf("DefaultRetry = %+v", DefaultRetry)
	}
}

// TestChangedRevalidatesOnlyThatUser: a pushed list of one user re-validates that user's leaves, one helper call per
// leaf, in every group the Bot is in; no other user's leaf is touched.
func TestChangedRevalidatesOnlyThatUser(t *testing.T) {
	core := newFakeCore()
	g1, g2, g3 := uid(0x91), uid(0x92), uid(0x93)
	ada := uid(1)
	core.member(g1, 1, ada, uid(0xa1))
	core.member(g2, 4, ada, uid(0xa1))
	core.member(g3, 2, ada, uid(0xa2))
	for i := byte(0); i < 50; i++ {
		core.member(g1, uint32(10+i), uid(0x20+i), uid(0x60+i))
		core.member(g2, uint32(10+i), uid(0x20+i), uid(0x60+i))
	}
	tr := New(core, testPolicy)
	defer tr.Close()
	if _, err := tr.Pushed(t.Context(), ada, push()); err != nil {
		t.Fatal(err)
	}
	pending, revalid, _ := core.calls()
	if revalid[ada] != 3 || len(revalid) != 1 {
		t.Fatalf("revalidations %v, want 3 for the pushed user's 3 leaves and none for anyone else", revalid)
	}
	if pending != 1 {
		t.Fatalf("pending-leaf reads %d, want 1 per push", pending)
	}
	// Changed alone, with nothing changed: no leaf to judge.
	if err := tr.Changed(t.Context(), ada); err != nil {
		t.Fatal(err)
	}
	if _, revalid, _ = core.calls(); revalid[ada] != 3 {
		t.Fatalf("a Changed with no change judged %d leaves again", revalid[ada]-3)
	}
}

// TestRevokedDeviceRefusedOtherDevicesWork (Review Focus 4): a revocation refuses the revoked device's leaves only. The
// user's other device stays valid, the user is not held, and no other user's leaves are re-checked or held.
func TestRevokedDeviceRefusedOtherDevicesWork(t *testing.T) {
	core := newFakeCore()
	g1, g2 := uid(0x91), uid(0x92)
	ada, bea := uid(1), uid(2)
	phone, laptop := uid(0xa1), uid(0xa2)
	core.member(g1, 1, ada, phone)
	core.member(g1, 2, ada, laptop)
	core.member(g2, 1, ada, phone)
	core.member(g1, 3, bea, uid(0xb1))
	tr := New(core, testPolicy)
	defer tr.Close()
	if _, err := tr.Pushed(t.Context(), ada, push(phone)); err != nil {
		t.Fatal(err)
	}
	if !core.isRefused(leafKey{g1, 1}) || !core.isRefused(leafKey{g2, 1}) {
		t.Fatal("the revoked device's leaves were not refused")
	}
	if core.isRefused(leafKey{g1, 2}) || core.isRefused(leafKey{g1, 3}) {
		t.Fatal("another device was refused")
	}
	if tr.Held(ada) || tr.Held(bea) {
		t.Fatalf("held: ada %v bea %v; a revocation holds no one", tr.Held(ada), tr.Held(bea))
	}
	if _, revalid, _ := core.calls(); revalid[bea] != 0 {
		t.Fatalf("bea's leaves were re-checked %d times", revalid[bea])
	}
}

// TestOneUsersFailedListDoesNotHoldOthers (fault): user A's op 24 fails. B's list proceeds at once. A is held and
// retried on the policy (1 s doubling, capped, 8 attempts); a later success releases A. When the attempts run out A
// stays held: refused, fail closed.
func TestOneUsersFailedListDoesNotHoldOthers(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	core := newFakeCore()
	ada, bea := uid(1), uid(2)
	core.member(uid(0x91), 1, ada, uid(0xa1))
	core.member(uid(0x91), 2, bea, uid(0xb1))
	core.fail(ada, &helper.StatusError{Status: 6}, 0) // busy, always
	tr := New(core, testPolicy).WithClock(clk)
	defer tr.Close()
	if _, err := tr.Pushed(t.Context(), ada, push()); err == nil {
		t.Fatal("a transient failure reported success")
	}
	if !tr.Held(ada) {
		t.Fatal("A is not held after a transient failure")
	}
	if _, err := tr.Pushed(t.Context(), bea, push()); err != nil || tr.Held(bea) {
		t.Fatalf("B behind A's failure: err %v held %v", err, tr.Held(bea))
	}
	attempts := func() int { _, _, i := core.calls(); return i[ada] }
	// Retries at 1, 2, 4, 8, 16, 32, 64 s after each failure: 8 attempts in all.
	for i, secs := range []int{1, 2, 4, 8, 16, 32, 64} {
		wait := time.Duration(secs) * time.Second
		before := attempts()
		clk.Advance(wait - time.Millisecond)
		time.Sleep(5 * time.Millisecond)
		if attempts() != before {
			t.Fatalf("retry %d ran before its %v", i+1, wait)
		}
		clk.Advance(time.Millisecond)
		waitFor(t, fmt.Sprintf("retry %d settled", i+1), func() bool { return settled(tr, ada, before+1) })
	}
	if got := attempts(); got != 8 {
		t.Fatalf("attempts %d, want 8", got)
	}
	clk.Advance(time.Hour)
	time.Sleep(5 * time.Millisecond)
	if got := attempts(); got != 8 {
		t.Fatalf("attempts after giving up %d, want 8", got)
	}
	if !tr.Held(ada) {
		t.Fatal("A released after the attempts ran out; want held (fail closed)")
	}
	// A's next push succeeds: released.
	core.fail(ada, nil, 0)
	if _, err := tr.Pushed(t.Context(), ada, push()); err != nil || tr.Held(ada) {
		t.Fatalf("A after a successful push: err %v held %v", err, tr.Held(ada))
	}
}

// TestARetrySucceedsAndReleases: a retry that succeeds applies the push and releases the user.
func TestARetrySucceedsAndReleases(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	core := newFakeCore()
	ada := uid(1)
	core.member(uid(0x91), 1, ada, uid(0xa1))
	core.fail(ada, fmt.Errorf("ingest: %w", helper.ErrClosed), 2)
	tr := New(core, testPolicy).WithClock(clk)
	defer tr.Close()
	_, _ = tr.Pushed(t.Context(), ada, push(uid(0xa1)))
	clk.Advance(time.Second)
	waitFor(t, "second attempt", func() bool { return settled(tr, ada, 2) })
	if !tr.Held(ada) {
		t.Fatal("released after a failed retry")
	}
	clk.Advance(2 * time.Second)
	waitFor(t, "released", func() bool { return !tr.Held(ada) })
	if !core.isRefused(leafKey{uid(0x91), 1}) {
		t.Fatal("the retried revocation was not applied to the leaf")
	}
}

// TestARevocationSurvivesATransientStatus (R2-4): busy, unavailable and internal are transient: the user stays held
// and the push is retried until it applies, never released with the revocation unapplied. A definitive refusal
// (invalid) is final for that push and holds no one.
func TestARevocationSurvivesATransientStatus(t *testing.T) {
	for _, status := range []uint64{5, 6, 9} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			clk := clock.NewFake(time.Unix(1_800_000_000, 0))
			core := newFakeCore()
			ada, phone := uid(1), uid(0xa1)
			core.member(uid(0x91), 1, ada, phone)
			core.fail(ada, &helper.StatusError{Status: status}, 1)
			tr := New(core, testPolicy).WithClock(clk)
			defer tr.Close()
			_, _ = tr.Pushed(t.Context(), ada, push(phone))
			if !tr.Held(ada) {
				t.Fatalf("status %d released the user with the revocation unapplied", status)
			}
			clk.Advance(time.Second)
			waitFor(t, "the revocation applied", func() bool { return core.isRefused(leafKey{uid(0x91), 1}) && !tr.Held(ada) })
		})
	}
	core := newFakeCore()
	ada := uid(1)
	core.member(uid(0x91), 1, ada, uid(0xa1))
	core.fail(ada, &helper.StatusError{Status: 1}, 1)
	tr := New(core, testPolicy)
	defer tr.Close()
	if _, err := tr.Pushed(t.Context(), ada, push()); err == nil || tr.Held(ada) {
		t.Fatalf("an invalid push: err %v held %v; want refused and not held", err, tr.Held(ada))
	}
}

// TestHeldAndBehindAnswersRefuseTheUser: a key change held from a push, or a list behind its signed head, refuses the
// user (fail closed) until a later push answers current.
func TestHeldAndBehindAnswersRefuseTheUser(t *testing.T) {
	for _, st := range []native.DeviceListState{native.DeviceListHeld, native.DeviceListBehind} {
		core := newFakeCore()
		ada := uid(1)
		core.member(uid(0x91), 1, ada, uid(0xa1))
		core.state[ada] = st
		tr := New(core, testPolicy)
		if _, err := tr.Pushed(t.Context(), ada, push()); !errors.Is(err, ErrHeld) || !tr.Held(ada) {
			t.Fatalf("state %d: err %v held %v; want held", st, err, tr.Held(ada))
		}
		core.mu.Lock()
		core.state[ada] = native.DeviceListCurrent
		core.mu.Unlock()
		if _, err := tr.Pushed(t.Context(), ada, push()); err != nil || tr.Held(ada) {
			t.Fatalf("state %d then current: err %v held %v; want released", st, err, tr.Held(ada))
		}
		tr.Close()
	}
}

// TestANewerPushReplacesARetry: a failing user's next push supersedes the retried one; a stale retry applies nothing.
func TestANewerPushReplacesARetry(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	core := newFakeCore()
	ada := uid(1)
	core.member(uid(0x91), 1, ada, uid(0xa1))
	core.fail(ada, errors.New("timeout"), 1)
	tr := New(core, testPolicy).WithClock(clk)
	defer tr.Close()
	_, _ = tr.Pushed(t.Context(), ada, push())
	if _, err := tr.Pushed(t.Context(), ada, push()); err != nil || tr.Held(ada) {
		t.Fatalf("the newer push: err %v held %v", err, tr.Held(ada))
	}
	clk.Advance(time.Hour)
	time.Sleep(5 * time.Millisecond)
	if _, _, i := core.calls(); i[ada] != 2 {
		t.Fatalf("ingests %d, want 2 (the stale retry ran)", i[ada])
	}
}

// TestKickRetriesHeldUsersNow: a fresh helper bound after a closed one retries every held user at once.
func TestKickRetriesHeldUsersNow(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	core := newFakeCore()
	ada := uid(1)
	core.member(uid(0x91), 1, ada, uid(0xa1))
	core.fail(ada, helper.ErrClosed, 1)
	tr := New(core, testPolicy).WithClock(clk)
	defer tr.Close()
	_, _ = tr.Pushed(t.Context(), ada, push())
	tr.Kick()
	waitFor(t, "released by the kick", func() bool { return !tr.Held(ada) })
}

// TestNoTrustWorkOnTimer: 10 minutes with no feed events cost no trust call at all.
func TestNoTrustWorkOnTimer(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	core := newFakeCore()
	for i := byte(0); i < 50; i++ {
		core.member(uid(0x91), uint32(i), uid(0x20+i), uid(0x60+i))
	}
	tr := New(core, testPolicy).WithClock(clk)
	defer tr.Close()
	for i := 0; i < 600; i++ {
		clk.Advance(time.Second)
	}
	time.Sleep(5 * time.Millisecond)
	pending, revalid, ingests := core.calls()
	if pending != 0 || len(revalid) != 0 || len(ingests) != 0 {
		t.Fatalf("trust calls on a clock: pending %d, revalidations %v, ingests %v", pending, revalid, ingests)
	}
}

// TestAnUnchangedPushCostsNoLeafWork: a push that changes nothing (a snapshot re-sending a known list) costs op 24
// alone: no pending-leaf read and no revalidation, so a cold snapshot of many members stays O(members) helper calls.
func TestAnUnchangedPushCostsNoLeafWork(t *testing.T) {
	core := newFakeCore()
	ada := uid(1)
	core.member(uid(0x91), 1, ada, uid(0xa1))
	tr := New(core, testPolicy)
	defer tr.Close()
	if _, err := tr.Pushed(t.Context(), ada, push()); err != nil {
		t.Fatal(err)
	}
	pending, _, _ := core.calls()
	if _, err := tr.Pushed(t.Context(), ada, push()); err != nil || tr.Held(ada) {
		t.Fatalf("the same push again: err %v held %v", err, tr.Held(ada))
	}
	if again, _, _ := core.calls(); again != pending {
		t.Fatalf("an unchanged push read pending leaves %d times", again-pending)
	}
}

// TestAFailedRejudgingIsRetriedAfterTheListIsCached: op 24 applies but re-judging the user's leaves fails. The user is
// held and retried; the retry's op 24 changes nothing (the list is cached already), and the leaves are re-judged anyway
// before the user is released.
func TestAFailedRejudgingIsRetriedAfterTheListIsCached(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	core := newFakeCore()
	ada, phone := uid(1), uid(0xa1)
	core.member(uid(0x91), 1, ada, phone)
	core.pendFail = 1
	tr := New(core, testPolicy).WithClock(clk)
	defer tr.Close()
	if _, err := tr.Pushed(t.Context(), ada, push(phone)); err == nil || !tr.Held(ada) {
		t.Fatalf("a failed re-judging: err %v held %v; want held", err, tr.Held(ada))
	}
	clk.Advance(time.Second)
	waitFor(t, "the retry re-judged the leaf", func() bool { return core.isRefused(leafKey{uid(0x91), 1}) && !tr.Held(ada) })
}

// TestSweepJudgesLeavesLeftUnjudged: leaves whose list changed while their re-judging never ran (a restart between
// op 24 and op 13) are judged by the snapshot's one Sweep; a current group costs one read and no revalidation.
func TestSweepJudgesLeavesLeftUnjudged(t *testing.T) {
	core := newFakeCore()
	ada, bea, phone := uid(1), uid(2), uid(0xa1)
	core.member(uid(0x91), 1, ada, phone)
	core.member(uid(0x91), 2, bea, uid(0xb1))
	core.mu.Lock()
	core.version[ada]++ // ada's list changed; her leaf was not judged under it
	core.revoked[phone] = true
	core.mu.Unlock()
	tr := New(core, testPolicy)
	defer tr.Close()
	if err := tr.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	pending, revalid, _ := core.calls()
	if pending != 1 || revalid[ada] != 1 || revalid[bea] != 0 || !core.isRefused(leafKey{uid(0x91), 1}) {
		t.Fatalf("pending reads %d, revalidations %v, refused %v", pending, revalid, core.isRefused(leafKey{uid(0x91), 1}))
	}
	if err := tr.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, revalid, _ = core.calls(); revalid[ada] != 1 {
		t.Fatalf("a current group was revalidated again: %v", revalid)
	}
}
