//go:build linux

package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/bot/service"
)

// threadRootCoordinator answers helper not_found (the thread root not held) for its first notHeld sends, then sends.
type threadRootCoordinator struct {
	inner   *utilityCoordinatorFake
	mu      sync.Mutex
	notHeld int
	tries   []service.SendInput
}

func (c *threadRootCoordinator) SendAuthorized(ctx context.Context, s service.SendSession, in service.SendInput, preflight func(context.Context) error) (protocol.SendReceipt, error) {
	c.mu.Lock()
	c.tries = append(c.tries, in)
	held := c.notHeld <= 0
	c.notHeld--
	c.mu.Unlock()
	if !held {
		return protocol.SendReceipt{}, &helper.StatusError{Status: 7}
	}
	return c.inner.SendAuthorized(ctx, s, in, preflight)
}

type threadSyncFake struct {
	mu       sync.Mutex
	expected []native.GroupExpected
	busy     int
	coord    *threadRootCoordinator
	holdAt   int // the sync pass after which the root is held (0: never)
}

func (f *threadSyncFake) sync(_ context.Context, expected native.GroupExpected) (service.SourceSyncResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.busy > 0 {
		f.busy--
		return service.SourceSyncResult{More: true}, service.ErrSourceSyncBusy
	}
	f.expected = append(f.expected, expected)
	if f.holdAt > 0 && len(f.expected) >= f.holdAt {
		f.coord.mu.Lock()
		f.coord.notHeld = 0
		f.coord.mu.Unlock()
	}
	return service.SourceSyncResult{}, nil
}

func shortThreadRootWait(t *testing.T, wait time.Duration) {
	t.Helper()
	oldWait, oldRetry := pollThreadRootWait, pollThreadRootRetry
	pollThreadRootWait, pollThreadRootRetry = wait, 5*time.Millisecond
	t.Cleanup(func() { pollThreadRootWait, pollThreadRootRetry = oldWait, oldRetry })
}

func threadRootPublisher(notHeld int) (*durablePollPublisher, *threadRootCoordinator, *utilityStateFake) {
	state := newUtilityStateFake()
	coord := &threadRootCoordinator{inner: &utilityCoordinatorFake{}, notHeld: notHeld}
	auth := &utilityChannelAuthorityFake{member: true, bits: uint64(api.PermViewChannel | api.PermSendMessages | api.PermReadHistory)}
	p := newPublisherTest(state, &utilityCoordinatorFake{}, auth)
	p.coordinator = coord
	return p, coord, state
}

// A member starts a thread and runs /poll in it before the Bot's group sync reaches the root: the publisher catches the
// text group up and sends the same native attempt again, then publishes (Task 21b fix round 2, slash-commands :424).
func TestThreadPollCatchesTheGroupUpWhenTheRootIsNotHeld(t *testing.T) {
	shortThreadRootWait(t, 2*time.Second)
	p, coord, state := threadRootPublisher(1 << 20)
	syncer := &threadSyncFake{busy: 1, coord: coord, holdAt: 2}
	p.threadSync = syncer.sync
	poll := utilityPoll()
	root := utilityID(9)
	poll.ThreadID = &root
	if err := p.PublishPoll(context.Background(), poll); err != nil {
		t.Fatalf("thread poll not published after the catch-up: %v", err)
	}
	if len(coord.inner.inputs) != 1 {
		t.Fatalf("want one accepted send, got %d", len(coord.inner.inputs))
	}
	if len(coord.tries) < 2 {
		t.Fatalf("want a retry after the catch-up, got %d tries", len(coord.tries))
	}
	for _, in := range coord.tries[1:] {
		if in.AttemptID != coord.tries[0].AttemptID || in.OperationID != poll.ID || in.PredecessorAttemptID != nil {
			t.Fatal("a retry changed the native attempt")
		}
	}
	want := native.GroupExpected{Group: utilityID(7), Community: state.binding.Community, Channel: poll.ChannelID, Kind: 0}
	for _, got := range syncer.expected {
		if got != want {
			t.Fatalf("caught up %+v, want the poll's text group %+v", got, want)
		}
	}
	if raw := state.values[string(publicationKey(poll.ID))]; raw.Revision != 2 {
		t.Fatalf("want the receipt recorded, revision %d", raw.Revision)
	}
}

// The wait is bounded: a root that never arrives fails the publication with the helper's not_found after the bound.
func TestThreadPollRootWaitIsBounded(t *testing.T) {
	shortThreadRootWait(t, 60*time.Millisecond)
	p, coord, _ := threadRootPublisher(1 << 20)
	syncer := &threadSyncFake{coord: coord}
	p.threadSync = syncer.sync
	poll := utilityPoll()
	root := utilityID(9)
	poll.ThreadID = &root
	started := time.Now()
	err := p.PublishPoll(context.Background(), poll)
	var status *helper.StatusError
	if !errors.As(err, &status) || status.Status != 7 || !strings.Contains(err.Error(), "thread root not held") {
		t.Fatalf("want a bounded not_found, got %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("the wait ran %s past its bound", elapsed)
	}
	if len(coord.inner.inputs) != 0 || len(syncer.expected) == 0 {
		t.Fatalf("sent %d, synced %d", len(coord.inner.inputs), len(syncer.expected))
	}
}

// Only a thread poll waits: a not_found for a channel poll (its group gone) answers at once, without a catch-up.
func TestChannelPollNotFoundDoesNotWait(t *testing.T) {
	shortThreadRootWait(t, 2*time.Second)
	p, coord, _ := threadRootPublisher(1 << 20)
	syncer := &threadSyncFake{coord: coord}
	p.threadSync = syncer.sync
	err := p.PublishPoll(context.Background(), utilityPoll())
	var status *helper.StatusError
	if !errors.As(err, &status) || status.Status != 7 {
		t.Fatalf("want the helper's not_found, got %v", err)
	}
	if len(coord.tries) != 1 || len(syncer.expected) != 0 {
		t.Fatalf("a channel poll retried: %d tries, %d syncs", len(coord.tries), len(syncer.expected))
	}
}
