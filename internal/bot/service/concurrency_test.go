package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/id"
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

type blockedMembershipSession struct {
	*membershipFakeSession
	entered chan struct{}
	release chan struct{}
}

func (s *blockedMembershipSession) Community(ctx context.Context, community id.ID) (client.Community, error) {
	close(s.entered)
	select {
	case <-ctx.Done():
		return client.Community{}, ctx.Err()
	case <-s.release:
		return s.membershipFakeSession.Community(ctx, community)
	}
}

func TestMembershipInteractionProgressesWhileBackgroundHTTPIsBlocked(t *testing.T) {
	r, n, base, community, _, _ := membershipFixture(t)
	group, participant := membershipID(21), membershipID(22)
	binding := native.InteractionGroupBinding{Instance: n.binding.Instance, Installation: n.binding.Installation, Community: community,
		BotUser: membershipID(23), ParticipantUser: participant, ParticipantDevice: membershipID(24), PolicyVersion: 3, E2EEVersion: 1}
	botDevice := membershipID(25)
	n.interactionGroups = []native.InteractionGroupDescriptor{{Group: group, Binding: binding, BotDevice: botDevice, Epoch: 1, State: 2}}
	n.interactionInspected = native.InteractionGroupInspectResult{Binding: binding, BotDevice: botDevice, Epoch: 1, State: 2, NextSequence: 1}
	base.interactionBinding, base.interactionBotDevice, base.interactionGroup = binding, botDevice, group
	s := &blockedMembershipSession{membershipFakeSession: base, entered: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(s.release) }) })
	background := make(chan error, 1)
	go func() { _, err := r.reconcileOnce(t.Context(), s); background <- err }()
	<-s.entered
	interactive := make(chan error, 1)
	go func() {
		result, err := r.reconcileInteractionGroupOnce(t.Context(), s, group)
		if err == nil && result.GroupsSynced != 1 {
			err = errors.New("interaction group did not synchronize")
		}
		interactive <- err
	}()
	if err := receiveConcurrentResult(t, interactive); err != nil {
		t.Fatal(err)
	}
	release.Do(func() { close(s.release) })
	if err := receiveConcurrentResult(t, background); err != nil {
		t.Fatal(err)
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

type blockedTrustSession struct {
	user    id.ID
	entered chan struct{}
	release chan struct{}
	err     error
}

type concurrentTrustNative struct {
	duplicatePendingTrustNative
	claims testOperationClaims
}

func (n *concurrentTrustNative) TryClaimTrust(user id.ID) (func(), bool) {
	return n.claims.claim(user)
}

func (s *blockedTrustSession) OwnDeviceList(_ context.Context, user id.ID) ([]byte, error) {
	if user == s.user {
		close(s.entered)
		<-s.release
	}
	return nil, s.err
}

func (s *blockedTrustSession) DeviceListHistory(context.Context, id.ID, uint64) ([][]any, error) {
	return nil, s.err
}

func TestSourceTrustClaimsSharedUserWithoutBlockingOtherSenders(t *testing.T) {
	group, user, other := sid(4), sid(5), sid(6)
	n := &concurrentTrustNative{duplicatePendingTrustNative: duplicatePendingTrustNative{binding: native.Binding{Instance: sid(1), Community: sid(2), Installation: sid(3)}}}
	trust, err := newSourceTrust(n)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := newSourceTrust(n)
	if err != nil {
		t.Fatal(err)
	}
	refusal := errors.New("history temporarily unavailable")
	s := &blockedTrustSession{user: user, entered: make(chan struct{}), release: make(chan struct{}), err: refusal}
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(s.release) }) })
	background := make(chan error, 1)
	go func() { _, err := trust.trustSender(t.Context(), s, group, 0, user, 1); background <- err }()
	<-s.entered
	interactive := make(chan error, 1)
	go func() {
		_, err := rebuilt.trustSender(t.Context(), s, group, 0, user, 1)
		if !errors.Is(err, ErrSourceTrustBusy) {
			interactive <- errors.New("same sender did not retain exclusive trust claim")
			return
		}
		if err = rebuilt.trustProposedAdd(t.Context(), s, group, []byte{1}, native.GroupProposedAdd{User: user}); !errors.Is(err, ErrSourceTrustBusy) {
			interactive <- errors.New("proposed add bypassed shared user trust claim")
			return
		}
		_, err = rebuilt.trustSender(t.Context(), s, group, 0, other, 1)
		if errors.Is(err, refusal) {
			err = nil
		}
		interactive <- err
	}()
	if err := receiveConcurrentResult(t, interactive); err != nil {
		t.Fatal(err)
	}
	release.Do(func() { close(s.release) })
	if err := receiveConcurrentResult(t, background); !errors.Is(err, refusal) {
		t.Fatalf("background error=%v", err)
	}
}

func TestPendingTrustHoldsBusySenderAndRefreshesOtherSenders(t *testing.T) {
	trust, n, session, available, busy := isolationFixture(t)
	release, ok := trust.claimUser(busy)
	if !ok {
		t.Fatal("could not reserve background sender refresh")
	}
	defer release()
	result, err := trust.refreshPendingLeaves(t.Context(), session)
	if err != nil || !result.Busy[busy] || result.Unrefreshed[busy] || result.Busy[available] || result.Unrefreshed[available] || result.Recovered != 1 {
		t.Fatalf("busy sender isolation: result=%+v err=%v", result, err)
	}
	if len(n.trusted) != 1 || n.trusted[0] != available {
		t.Fatalf("trusted users=%v", n.trusted)
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
