//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/community/welcome"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/id"
)

// scriptedWelcome answers each welcome run with the next error in its script (the last one repeats).
type scriptedWelcome struct {
	mu       sync.Mutex
	script   []error
	runs     []protocol.Event
	episodes map[id.ID]id.ID
}

func (s *scriptedWelcome) welcome(_ context.Context, ev protocol.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs = append(s.runs, ev)
	return s.script[min(len(s.runs)-1, len(s.script)-1)]
}

func (s *scriptedWelcome) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runs)
}

func (s *scriptedWelcome) ResolveCurrentEpisode(_ context.Context, user id.ID) (id.ID, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.episodes[user]
	return e, ok, nil
}

func (s *scriptedWelcome) setEpisode(user, episode id.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.episodes[user] = episode
}

// welcomeRetryFeed is a feed past its first snapshot whose clock the test moves, and whose log it reads.
func welcomeRetryFeed(t *testing.T, script ...error) (*installationFeed, *scriptedWelcome, *time.Time, *lockedLog, id.ID, id.ID) {
	t.Helper()
	community, bot, bea := feedID(1), feedID(2), feedID(4)
	s := &scriptedWelcome{script: script, episodes: map[id.ID]id.ID{bea: feedID(0x32)}}
	logs := &lockedLog{}
	f := newInstallationFeed(feedID(9), community, bot, slog.New(slog.NewTextHandler(logs, nil)), func() {})
	now := time.Unix(1_800_000_000, 0)
	f.now = func() time.Time { return now }
	f.setDeps(feedDeps{episodes: s, welcome: s.welcome})
	f.SnapshotBegin(t.Context())
	f.SnapshotDone(t.Context())
	return f, s, &now, logs, community, bea
}

// A join whose welcome is deferred (welcome.ErrRetryLater) is run again on a later tick once its backoff passed, with
// the same event; it stops once the welcome succeeds.
func TestDeferredWelcomeIsRetriedOnTheTickAfterItsBackoff(t *testing.T) {
	retry := fmt.Errorf("bot has not joined the channel's group yet: %w", welcome.ErrRetryLater)
	f, s, now, _, community, bea := welcomeRetryFeed(t, retry, retry, nil)
	ctx := t.Context()
	f.Member(ctx, MemberEvent{Community: community, User: bea, Joined: true})
	if s.count() != 1 || f.pendingWelcomes() != 1 {
		t.Fatalf("after the join: runs=%d pending=%d, want 1 and 1", s.count(), f.pendingWelcomes())
	}
	*now = now.Add(time.Second)
	f.retryWelcomes(ctx)
	if s.count() != 1 {
		t.Fatalf("a retry ran %v after the join, before the %v backoff", time.Second, welcomeRetryMin)
	}
	*now = now.Add(time.Second)
	f.retryWelcomes(ctx)
	if s.count() != 2 || f.pendingWelcomes() != 1 {
		t.Fatalf("at the backoff: runs=%d pending=%d, want 2 and 1", s.count(), f.pendingWelcomes())
	}
	if s.runs[1].ID != s.runs[0].ID || s.runs[1].Timestamp != s.runs[0].Timestamp {
		t.Fatal("the retry presented a different event id or time")
	}
	// The backoff grows: 2 s after the second run is too soon.
	*now = now.Add(welcomeRetryMin)
	f.retryWelcomes(ctx)
	if s.count() != 2 {
		t.Fatalf("runs=%d, the backoff did not grow", s.count())
	}
	*now = now.Add(time.Minute)
	f.retryWelcomes(ctx)
	if s.count() != 3 || f.pendingWelcomes() != 0 {
		t.Fatalf("after success: runs=%d pending=%d, want 3 and 0", s.count(), f.pendingWelcomes())
	}
	*now = now.Add(time.Hour)
	f.retryWelcomes(ctx)
	if s.count() != 3 {
		t.Fatalf("a welcomed episode ran again: runs=%d", s.count())
	}
}

// A welcome that gives up (the attempt bound) or fails for good is not retried, and the give-up is one warning with
// ids and counts only.
func TestGivenUpWelcomeStopsWithOneWarning(t *testing.T) {
	retry := fmt.Errorf("closed: %w", welcome.ErrRetryLater)
	f, s, now, logs, community, bea := welcomeRetryFeed(t, retry, fmt.Errorf("8 attempts: %w", welcome.ErrGaveUp))
	ctx := t.Context()
	f.Member(ctx, MemberEvent{Community: community, User: bea, Joined: true})
	for range 5 {
		*now = now.Add(10 * time.Minute)
		f.retryWelcomes(ctx)
	}
	if s.count() != 2 || f.pendingWelcomes() != 0 {
		t.Fatalf("runs=%d pending=%d, want 2 and 0", s.count(), f.pendingWelcomes())
	}
	out := logs.String()
	if strings.Count(out, "welcome given up") != 1 || !strings.Contains(out, "episode_id="+feedID(0x32).String()) {
		t.Fatalf("give-up log %q, want one warning naming the episode", out)
	}
	// The deferral and the give-up log ids and counts only: no error text (which could grow content), no name.
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if (strings.Contains(line, "welcome deferred") || strings.Contains(line, "welcome given up")) &&
			(strings.Contains(line, "error=") || strings.Contains(line, "closed:") || strings.Contains(line, "attempts:")) {
			t.Fatalf("log line carries more than ids and counts: %q", line)
		}
	}
	if !strings.Contains(out, "welcome deferred") {
		t.Fatalf("log %q, want the deferral", out)
	}

	other := errors.New("welcome configuration tombstoned")
	f2, s2, now2, _, community2, bea2 := welcomeRetryFeed(t, other)
	f2.Member(ctx, MemberEvent{Community: community2, User: bea2, Joined: true})
	*now2 = now2.Add(time.Hour)
	f2.retryWelcomes(ctx)
	if s2.count() != 1 || f2.pendingWelcomes() != 0 {
		t.Fatalf("a non-retryable error was retried: runs=%d pending=%d", s2.count(), f2.pendingWelcomes())
	}
}

// A member who left, or whose episode changed, before the retry is not welcomed for the old episode.
func TestDeferredWelcomeDroppedWhenTheMemberLeft(t *testing.T) {
	retry := fmt.Errorf("busy: %w", welcome.ErrRetryLater)
	f, s, now, _, community, bea := welcomeRetryFeed(t, retry, nil)
	ctx := t.Context()
	f.Member(ctx, MemberEvent{Community: community, User: bea, Joined: true})
	f.Member(ctx, MemberEvent{Community: community, User: bea, Joined: false})
	*now = now.Add(time.Minute)
	f.retryWelcomes(ctx)
	if s.count() != 1 || f.pendingWelcomes() != 0 {
		t.Fatalf("left member: runs=%d pending=%d", s.count(), f.pendingWelcomes())
	}

	f2, s2, now2, _, community2, bea2 := welcomeRetryFeed(t, retry, nil)
	f2.Member(ctx, MemberEvent{Community: community2, User: bea2, Joined: true})
	s2.setEpisode(bea2, feedID(0x40))
	*now2 = now2.Add(time.Minute)
	f2.retryWelcomes(ctx)
	if s2.count() != 1 || f2.pendingWelcomes() != 0 {
		t.Fatalf("changed episode: runs=%d pending=%d", s2.count(), f2.pendingWelcomes())
	}
}

// A live channel-group epoch change (a commit) kicks the deferred welcomes without waiting for the due tick, still no
// sooner than the minimum backoff.
func TestChannelGroupCommitKicksDeferredWelcome(t *testing.T) {
	retry := fmt.Errorf("busy: %w", welcome.ErrRetryLater)
	f, s, now, _, community, bea := welcomeRetryFeed(t, retry, retry, nil)
	ctx := t.Context()
	f.Member(ctx, MemberEvent{Community: community, User: bea, Joined: true})
	*now = now.Add(welcomeRetryMin)
	f.retryWelcomes(ctx) // second run: the backoff is now 4 s
	*now = now.Add(welcomeRetryMin)
	f.ChannelGroup(ctx, ChannelGroupEvent{Community: community, Channel: feedID(6), Group: feedID(7), Epoch: 3})
	waitFor(t, "the kicked retry", 5*time.Second, func() bool { return s.count() == 3 })
	waitFor(t, "the retry set to empty", 5*time.Second, func() bool { return f.pendingWelcomes() == 0 })
}
