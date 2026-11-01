package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/id"
)

// perUserListSession answers each user's current signed device list head on its own.
type perUserListSession struct {
	heads map[id.ID][]byte
	errs  map[id.ID]error
}

func (s *perUserListSession) OwnDeviceList(_ context.Context, user id.ID) ([]byte, error) {
	if err := s.errs[user]; err != nil {
		return nil, err
	}
	return s.heads[user], nil
}
func (*perUserListSession) DeviceListHistory(context.Context, id.ID, uint64) ([][]any, error) {
	return nil, errors.New("no history needed")
}

type isolatingTrustNative struct {
	binding     native.Binding
	leaves      []native.GroupPendingLeaf
	leavesErr   error
	trustErr    error
	trusted     []id.ID
	revalidated []id.ID
}

func (n *isolatingTrustNative) Binding() native.Binding { return n.binding }
func (n *isolatingTrustNative) PendingLeaves(context.Context) ([]native.GroupPendingLeaf, error) {
	return append([]native.GroupPendingLeaf(nil), n.leaves...), n.leavesErr
}
func (*isolatingTrustNative) ProposedAdds(context.Context, id.ID, []byte) ([]native.GroupProposedAdd, error) {
	return nil, nil
}
func (n *isolatingTrustNative) TrustSender(_ context.Context, _ id.ID, _ uint64, user id.ID, _, _ []byte, _ uint64) (native.GroupTrustVerdict, error) {
	n.trusted = append(n.trusted, user)
	return native.GroupTrustVerdict{}, n.trustErr
}
func (*isolatingTrustNative) TrustProposedAdd(context.Context, id.ID, []byte, uint64, id.ID, []byte, []byte, uint64) (native.GroupTrustVerdict, error) {
	return native.GroupTrustVerdict{}, nil
}
func (n *isolatingTrustNative) RevalidateGroup(_ context.Context, group id.ID, _ uint64, _ uint64) (uint32, error) {
	n.revalidated = append(n.revalidated, group)
	return 1, nil
}

func isolationFixture(t *testing.T) (*SourceTrust, *isolatingTrustNative, *perUserListSession, id.ID, id.ID) {
	t.Helper()
	a, b := sid(41), sid(42)
	n := &isolatingTrustNative{binding: native.Binding{Instance: sid(1), Community: sid(2), Installation: sid(3)},
		leaves: []native.GroupPendingLeaf{
			{Group: sid(51), Leaf: 1, User: b, CachedVersion: 5}, // B first: its failure must not stop A
			{Group: sid(52), Leaf: 2, User: a, CachedVersion: 1},
			{Group: sid(53), Leaf: 3, User: b, CachedVersion: 5},
		}}
	trust, err := newSourceTrust(n)
	if err != nil {
		t.Fatal(err)
	}
	session := &perUserListSession{
		// A's current head is its cached version: trusted. B's head (v1) is older than B's cached v5:
		// its signed-list history is stale, a failure scoped to B alone.
		heads: map[id.ID][]byte{a: outerDeviceList(t, 1, []byte("a-list")), b: outerDeviceList(t, 1, []byte("b-list"))},
		errs:  map[id.ID]error{},
	}
	return trust, n, session, a, b
}

// One participant's stale sender history must not stop the trust pass for the others: A is
// refreshed and its group revalidated; B is reported unrefreshed and nothing is applied for B.
func TestRefreshPendingLeavesIsolatesOneUsersStaleHistory(t *testing.T) {
	trust, n, session, a, b := isolationFixture(t)
	refresh, err := trust.refreshPendingLeaves(context.Background(), session)
	if err != nil {
		t.Fatalf("a user-scoped failure failed the pass: %v", err)
	}
	if len(n.trusted) != 1 || n.trusted[0] != a {
		t.Fatalf("trusted senders %v, want only A", n.trusted)
	}
	if len(n.revalidated) != 1 || n.revalidated[0] != sid(52) {
		t.Fatalf("revalidated groups %v, want only A's", n.revalidated)
	}
	if !refresh.Unrefreshed[b] || refresh.Unrefreshed[a] || len(refresh.Unrefreshed) != 1 {
		t.Fatalf("unrefreshed %v, want exactly B", refresh.Unrefreshed)
	}
	if refresh.Recovered != 1 {
		t.Fatalf("recovered %d, want A's one event", refresh.Recovered)
	}
}

// Failures that are not scoped to one sender stay fatal to the whole pass, exactly as before: a
// general API failure while reading a list, and the pending-leaf read itself.
func TestRefreshPendingLeavesKeepsGeneralFailuresFatal(t *testing.T) {
	trust, _, session, _, b := isolationFixture(t)
	unavailable := errors.New("server unavailable")
	session.errs[b] = unavailable
	if _, err := trust.refreshPendingLeaves(context.Background(), session); !errors.Is(err, unavailable) {
		t.Fatalf("an API failure returned %v, want it fatal", err)
	}
	trust, n, session, _, _ := isolationFixture(t)
	n.leavesErr = errors.New("pending leaves unreadable")
	if _, err := trust.refreshPendingLeaves(context.Background(), session); err == nil {
		t.Fatal("a PendingLeaves failure was not fatal")
	}
}

// Only a stale history READ for one sender is sender-scoped. The same ErrSenderHistoryStale from
// the clock check (no trust phase) or from the native core's verdict (sender-trust phase) is not
// about one sender's published data and stays fatal.
func TestRefreshPendingLeavesKeepsClockAndCoreStalenessFatal(t *testing.T) {
	t.Run("clock", func(t *testing.T) {
		trust, n, session, _, _ := isolationFixture(t)
		// Only A's leaf, whose history reads fine: the failure can only come from the clock.
		n.leaves = n.leaves[1:2]
		trust.now = func() time.Time { return time.Unix(0, 0) }
		if _, err := trust.refreshPendingLeaves(context.Background(), session); !errors.Is(err, ErrSenderHistoryStale) {
			t.Fatalf("a clock failure returned %v, want it fatal", err)
		}
	})
	t.Run("native core", func(t *testing.T) {
		trust, n, session, _, _ := isolationFixture(t)
		n.leaves = n.leaves[1:2]
		n.trustErr = ErrSenderHistoryStale // the core refuses the verdict
		if _, err := trust.refreshPendingLeaves(context.Background(), session); !errors.Is(err, ErrSenderHistoryStale) {
			t.Fatalf("a core-side staleness returned %v, want it fatal", err)
		}
	})
}

// Drain dispatches only events from senders whose trust is current. An event from an unrefreshed
// sender is held: not handled, not acknowledged, and nothing after it is acknowledged past it; the
// event before it from another sender is dispatched.
func TestHeldSenderEventIsNeitherDispatchedNorAcknowledged(t *testing.T) {
	community, a, b := id.New(), id.New(), id.New()
	f := &fakeGroupEventNative{binding: native.Binding{Instance: id.New(), Community: community, Installation: id.New()}, page: native.GroupEventPage{
		Events: []native.GroupEvent{
			{DeliveryID: 1, Community: community, SenderUser: a},
			{DeliveryID: 2, Community: community, SenderUser: b},
			{DeliveryID: 3, Community: community, SenderUser: a},
		}, Next: 3, HighWater: 3,
	}}
	p, err := newGroupEventPoller(f)
	if err != nil {
		t.Fatal(err)
	}
	var dispatched []uint64
	handler := HoldUnrefreshedSenders(groupEventHandlerFunc(func(_ context.Context, event native.GroupEvent) error {
		dispatched = append(dispatched, event.DeliveryID)
		return nil
	}), map[id.ID]bool{b: true})
	_, err = p.PollOnce(context.Background(), handler)
	if !errors.Is(err, ErrGroupEventHeld) {
		t.Fatalf("PollOnce error %v, want the held sender reported", err)
	}
	if len(dispatched) != 1 || dispatched[0] != 1 {
		t.Fatalf("dispatched %v, want only A's event before B's", dispatched)
	}
	if len(f.acked) != 1 || f.acked[0] != 1 {
		t.Fatalf("acknowledged %v, want only through A's event: B's must stay pending", f.acked)
	}
}

// A revoked device whose revocation could not be processed (its user's trust refresh failed) has
// nothing dispatched, whichever of the user's devices sent it.
func TestRevokedDeviceOfAnUnrefreshedUserHasNothingDispatched(t *testing.T) {
	community, b, revoked := id.New(), id.New(), id.New()
	f := &fakeGroupEventNative{binding: native.Binding{Instance: id.New(), Community: community, Installation: id.New()}, page: native.GroupEventPage{
		Events: []native.GroupEvent{{DeliveryID: 7, Community: community, SenderUser: b, SenderDevice: revoked}}, Next: 7, HighWater: 7,
	}}
	p, _ := newGroupEventPoller(f)
	dispatched := false
	handler := HoldUnrefreshedSenders(groupEventHandlerFunc(func(context.Context, native.GroupEvent) error {
		dispatched = true
		return nil
	}), map[id.ID]bool{b: true})
	if _, err := p.PollOnce(context.Background(), handler); !errors.Is(err, ErrGroupEventHeld) {
		t.Fatalf("PollOnce error %v, want held", err)
	}
	if dispatched || len(f.acked) != 0 {
		t.Fatalf("revoked device's event dispatched=%v acked=%v", dispatched, f.acked)
	}
}

// With nobody unrefreshed the hold is transparent.
func TestHoldWithNoUnrefreshedSendersDispatchesEverything(t *testing.T) {
	community := id.New()
	f := &fakeGroupEventNative{binding: native.Binding{Instance: id.New(), Community: community, Installation: id.New()}, page: native.GroupEventPage{
		Events: []native.GroupEvent{{DeliveryID: 1, Community: community, SenderUser: id.New()}}, Next: 1, HighWater: 1,
	}}
	p, _ := newGroupEventPoller(f)
	handled := 0
	handler := HoldUnrefreshedSenders(groupEventHandlerFunc(func(context.Context, native.GroupEvent) error { handled++; return nil }), nil)
	if result, err := p.PollOnce(context.Background(), handler); err != nil || handled != 1 || result.Handled != 1 {
		t.Fatalf("PollOnce = (%+v, %v), handled %d", result, err, handled)
	}
}
