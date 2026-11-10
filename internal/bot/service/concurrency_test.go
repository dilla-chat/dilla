package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/id"
)

func receiveConcurrentResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("independent work waited for blocked network I/O")
		return nil
	}
}

type concurrentSourceNative struct {
	fakeSourceSyncNative
	groups map[id.ID]native.GroupInfo
	claims testOperationClaims
}

type testOperationClaims struct {
	mu     sync.Mutex
	active map[id.ID]bool
}

func (c *testOperationClaims) claim(key id.ID) (func(), bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active[key] {
		return nil, false
	}
	if c.active == nil {
		c.active = make(map[id.ID]bool)
	}
	c.active[key] = true
	return func() { c.mu.Lock(); delete(c.active, key); c.mu.Unlock() }, true
}

func (n *concurrentSourceNative) TryClaimSource(group id.ID) (func(), bool) {
	return n.claims.claim(group)
}

func (n *concurrentSourceNative) InspectGroup(_ context.Context, group id.ID) (native.GroupInfo, error) {
	return n.groups[group], nil
}

type blockedSourceSession struct {
	*fakeSourceSyncSession
	group   id.ID
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (s *blockedSourceSession) GroupInfo(ctx context.Context, group id.ID) (client.GroupInfo, error) {
	if group == s.group {
		s.once.Do(func() { close(s.entered); <-s.release })
	}
	return s.fakeSourceSyncSession.GroupInfo(ctx, group)
}

func TestSourceSyncDefersSameGroupWithoutBlockingIndependentGroup(t *testing.T) {
	binding := native.Binding{Instance: sid(1), Community: sid(2), Installation: sid(3)}
	first, second, target := sid(4), sid(5), sid(6)
	n := &concurrentSourceNative{fakeSourceSyncNative: fakeSourceSyncNative{binding: binding}, groups: map[id.ID]native.GroupInfo{
		first: sourceGroupInfo(binding, first, target, 1), second: sourceGroupInfo(binding, second, target, 1),
	}}
	source, err := newSourceSync(n, sourceSyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := newSourceSync(n, sourceSyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s := &blockedSourceSession{fakeSourceSyncSession: &fakeSourceSyncSession{info: client.GroupInfo{NextSequence: 1}}, group: first, entered: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(s.release) }) })
	background := make(chan error, 1)
	go func() {
		_, err := rebuilt.sync(t.Context(), s, sourceExpected(first, binding.Community, target))
		background <- err
	}()
	<-s.entered
	interactive := make(chan error, 1)
	go func() {
		_, err := source.sync(t.Context(), s, sourceExpected(first, binding.Community, target))
		if !errors.Is(err, ErrSourceSyncBusy) {
			interactive <- errors.New("overlapping source pass did not report busy")
			return
		}
		_, err = rebuilt.sync(t.Context(), s, sourceExpected(second, binding.Community, target))
		interactive <- err
	}()
	if err := receiveConcurrentResult(t, interactive); err != nil {
		t.Fatal(err)
	}
	release.Do(func() { close(s.release) })
	if err := receiveConcurrentResult(t, background); err != nil {
		t.Fatal(err)
	}
	if _, err := source.sync(t.Context(), s, sourceExpected(first, binding.Community, target)); err != nil {
		t.Fatalf("source claim leaked after completion: %v", err)
	}
}

func TestGroupEventPollerRejectsOverlappingCumulativeCursorDrain(t *testing.T) {
	q := ageTestQueue()
	p, err := newGroupEventPoller(q)
	if err != nil {
		t.Fatal(err)
	}
	entered, unblock := make(chan struct{}), make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(unblock) }) })
	background := make(chan error, 1)
	go func() {
		_, err := p.PollOnce(t.Context(), groupEventHandlerFunc(func(_ context.Context, e native.GroupEvent) error {
			if e.DeliveryID == 1 {
				close(entered)
				<-unblock
			}
			return nil
		}))
		background <- err
	}()
	<-entered
	result, err := p.PollOnce(t.Context(), groupEventHandlerFunc(func(context.Context, native.GroupEvent) error {
		t.Error("overlapping drain dispatched an event")
		return nil
	}))
	if !errors.Is(err, ErrGroupEventPollBusy) || !result.More || q.acked != 0 {
		t.Fatalf("overlapping drain: result=%+v err=%v ack=%d", result, err, q.acked)
	}
	release.Do(func() { close(unblock) })
	if err := receiveConcurrentResult(t, background); err != nil {
		t.Fatal(err)
	}
	if q.acked != 2 {
		t.Fatalf("owning drain did not acknowledge in order: %d", q.acked)
	}
}
