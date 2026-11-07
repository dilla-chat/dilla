package youtube

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// shipped makes the test run as a shipped build does: an owner-less context is refused.
func shipped(t *testing.T) {
	t.Helper()
	if !shippedRequireOwner {
		t.Fatal("the shipped default must require an owner")
	}
	requireOwner = true
	t.Cleanup(func() { requireOwner = false })
}

func TestAnEmptyOwnerIsRefusedOutsideATestHostBuild(t *testing.T) {
	shipped(t)
	w := newFakeLookup()
	r, _, _, _ := testWarm(t, w)
	if _, err := r.Resolve(context.Background(), warmVideo); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("owner-less Resolve = %v, want ErrInvalidInput", err)
	}
	if w.calls.Load() != 0 {
		t.Fatal("an owner-less lookup reached the worker")
	}
	dir := t.TempDir()
	starter, _ := streamStarterForTest(t, dir, writeScript(t, dir, "ffmpeg", `exit 0`), &fakeResolver{})
	if _, err := starter.Start(context.Background(), warmVideo); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("owner-less Start = %v", err)
	}
	if err := starter.Prefetch(context.Background(), warmVideo); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("owner-less Prefetch = %v", err)
	}
	if _, err := (&Backend{streams: starter, tracks: warmTracks{warm: r}}).For(""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("For(\"\") = %v", err)
	}
	// And a named owner works.
	if _, err := r.Resolve(WithOwner(context.Background(), "community-a"), warmVideo); err != nil {
		t.Fatal(err)
	}
}

func TestOwnersAreValidated(t *testing.T) {
	shipped(t)
	r, _, _, _ := testWarm(t, newFakeLookup())
	backend := &Backend{streams: &StreamStarter{}, tracks: warmTracks{warm: r}}
	for _, owner := range []string{"", "a\x00b", "a\nb", "tab\there", "\x7f", strings.Repeat("x", maxOwnerBytes+1), "bad\xffutf8"} {
		if _, err := r.Resolve(WithOwner(context.Background(), owner), warmVideo); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("Resolve with owner %q = %v", owner, err)
		}
		if _, err := backend.For(owner); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("For(%q) = %v", owner, err)
		}
	}
	for _, owner := range []string{"community-a", "0190c2f4-7a6e-7000-8000-000000000001", "Gemenskap åäö"} {
		if _, err := backend.For(owner); err != nil {
			t.Fatalf("For(%q) = %v", owner, err)
		}
	}
}

func TestOwnersThatWouldCollideWhenJoinedStayApart(t *testing.T) {
	r, _, _, _ := testWarm(t, newFakeLookup())
	first, second := Resolved{StreamURL: "https://rr1---sn-abc.googlevideo.com/first"}, Resolved{StreamURL: "https://rr1---sn-abc.googlevideo.com/second"}
	first.Track.CanonicalID, second.Track.CanonicalID = "aaaaaaaaaaa", "bbbbbbbbbbb"
	first.Expires, second.Expires = warmBase.Add(time.Hour), warmBase.Add(time.Hour)
	// "a"+NUL+"b"+NUL+"c" is both ("a", "b\x00c") and ("a\x00b", "c") if keys are joined strings.
	r.mu.Lock()
	r.storeLocked("a", ownedKey{"a", "b\x00c"}, first)
	r.storeLocked("a\x00b", ownedKey{"a\x00b", "c"}, second)
	got1, got2 := r.cache[ownedKey{"a", "b\x00c"}], r.cache[ownedKey{"a\x00b", "c"}]
	r.mu.Unlock()
	if got1 == nil || got2 == nil || got1.resolved.StreamURL != first.StreamURL || got2.resolved.StreamURL != second.StreamURL {
		t.Fatalf("keys collided: %+v %+v", got1, got2)
	}
	if prefetchKey("a", "b\x00c") == prefetchKey("a\x00b", "c") {
		t.Fatal("pre-buffer keys collided")
	}
}

func TestEachOwnerHasItsOwnCacheCapWithLRUEviction(t *testing.T) {
	w := newFakeLookup()
	r, _, _, _ := testWarm(t, w)
	a, b := WithOwner(context.Background(), "a"), WithOwner(context.Background(), "b")
	if _, err := r.Resolve(b, "b's only song"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(a, "first song"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxOwnerCacheEntries-20; i++ {
		if _, err := r.Resolve(a, "song n"+strings.Repeat("x", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Resolve(a, "first song"); err != nil { // touched: now the most recently used
		t.Fatal(err)
	}
	for i := 0; i < 60; i++ { // far past A's cap
		if _, err := r.Resolve(a, "later song n"+strings.Repeat("y", i)); err != nil {
			t.Fatal(err)
		}
	}
	r.mu.Lock()
	countA, countB := r.countLocked("a"), r.countLocked("b")
	_, firstKept := r.cache[ownedKey{"a", "search:first song"}]
	_, secondEvicted := r.cache[ownedKey{"a", "search:song n"}]
	r.mu.Unlock()
	if countA > maxOwnerCacheEntries {
		t.Fatalf("owner A holds %d entries, cap %d", countA, maxOwnerCacheEntries)
	}
	if countB == 0 {
		t.Fatal("owner A's searches evicted owner B's entry")
	}
	if !firstKept {
		t.Fatal("the recently used entry was evicted: eviction is not LRU")
	}
	if secondEvicted {
		t.Fatal("the least recently used entry was kept")
	}
}

func TestAFailureRetirementBacksOffInsteadOfRestartingEveryThreeLookups(t *testing.T) {
	failing := func() *funcWorker {
		return newFuncWorker(func(context.Context, []string) ([]byte, error) {
			return nil, &workerFailure{Code: 1, Stderr: "ERROR: Unable to download webpage: connection reset"}
		})
	}
	w1, w2, w3 := failing(), failing(), failing()
	r, _, sleeps, starts := testWarm(t, w1, w2, w3)
	for i := 0; i < 3*workerMaxFailures; i++ {
		_, _ = r.Resolve(context.Background(), "outage "+string(rune('a'+i)))
	}
	if *starts != 3 {
		t.Fatalf("%d workers started for %d failed lookups, want 3", *starts, 3*workerMaxFailures)
	}
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond}
	if len(*sleeps) != len(want) || (*sleeps)[0] != want[0] || (*sleeps)[1] != want[1] {
		t.Fatalf("restart waits = %v, want the backoff %v", *sleeps, want)
	}
	deadline := time.After(2 * time.Second)
	for !w1.closed.Load() || !w2.closed.Load() {
		select {
		case <-deadline:
			t.Fatal("a failure-retired worker was left running")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestACancelledMetadataLookupStillCountsTowardRecycling(t *testing.T) {
	w := newFuncWorker(func(ctx context.Context, _ []string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	r, _, _, _ := testWarm(t, w)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	_, _ = r.MetadataResolver().Resolve(ctx, "https://www.youtube.com/playlist?list=PLabc")
	r.mu.Lock()
	served := r.served
	r.mu.Unlock()
	if served != 1 {
		t.Fatalf("served = %d after a cancelled lookup, want 1", served)
	}
	if w.closed.Load() {
		t.Fatal("a caller cancel killed the worker")
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
func (discardWriter) Close() error                { return nil }

func TestWedgeIsJudgedOnTheMonotonicClock(t *testing.T) {
	w := &lookupProcess{process: &isolatedPCM{stdin: discardWriter{}}, pending: map[uint64]chan workerReply{}, base: time.Now(), grace: 20 * time.Millisecond}
	cause := context.DeadlineExceeded
	sent := int64(time.Since(w.base))
	w.lastReply.Store(sent - 1) // nothing answered since the request was sent
	if err := w.cancel(1, make(chan workerReply, 1), sent, cause); !errors.Is(err, errWorkerWedged) {
		t.Fatalf("silent worker = %v, want wedged", err)
	}
	w.lastReply.Store(sent + 1) // another request was answered: alive
	if err := w.cancel(2, make(chan workerReply, 1), sent, cause); errors.Is(err, errWorkerWedged) || !errors.Is(err, cause) {
		t.Fatalf("answering worker = %v, want the plain cause", err)
	}
}
