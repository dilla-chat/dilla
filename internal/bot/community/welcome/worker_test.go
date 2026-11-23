package welcome

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

type testNative struct {
	binding      native.Binding
	values       map[string]native.StateValue
	transactions map[id.ID][]native.StateChange
}

type receiptFirstNative struct {
	*testNative
	receipts map[id.ID][]uint64
}

func (n *receiptFirstNative) StateTransaction(ctx context.Context, tx id.ID, changes []native.StateChange) ([]uint64, error) {
	if prior, ok := n.receipts[tx]; ok {
		return prior, nil
	}
	revs, err := n.testNative.StateTransaction(ctx, tx, changes)
	if err == nil {
		n.receipts[tx] = append([]uint64(nil), revs...)
	}
	return revs, err
}

func (n *testNative) Binding() native.Binding { return n.binding }
func (n *testNative) StateGet(_ context.Context, area uint64, key []byte) (native.StateValue, error) {
	return n.values[stateMapKey(area, key)], nil
}
func (n *testNative) StateTransaction(_ context.Context, tx id.ID, changes []native.StateChange) ([]uint64, error) {
	if old, ok := n.transactions[tx]; ok {
		if len(old) != len(changes) {
			return nil, errors.New("transaction conflict")
		}
		for i := range old {
			if old[i].Area != changes[i].Area || string(old[i].Key) != string(changes[i].Key) || string(old[i].Value) != string(changes[i].Value) {
				return nil, errors.New("transaction conflict")
			}
		}
	} else {
		n.transactions[tx] = changes
	}
	revs := make([]uint64, len(changes))
	for i, ch := range changes {
		k := stateMapKey(ch.Area, ch.Key)
		current := n.values[k]
		if current.Revision != ch.ExpectedRevision {
			return nil, errors.New("revision conflict")
		}
		current.Revision++
		current.Value = append([]byte(nil), ch.Value...)
		n.values[k] = current
		revs[i] = current.Revision
	}
	return revs, nil
}
func stateMapKey(area uint64, key []byte) string { return string(rune(area)) + ":" + string(key) }

func TestWelcomeStateStaleReplayCannotHideLaterRevision(t *testing.T) {
	n := &receiptFirstNative{testNative: &testNative{values: map[string]native.StateValue{}, transactions: map[id.ID][]native.StateChange{}}, receipts: map[id.ID][]uint64{}}
	p := &Processor{native: n}
	key := []byte("episode")
	first := episode{Status: StatusPending, EventID: testID(1), Timestamp: 100, OperationID: testID(2)}
	second := episode{Status: StatusPosted, EventID: testID(1), Timestamp: 100, OperationID: testID(2)}
	if err := p.save(context.Background(), key, 0, first); err != nil {
		t.Fatal(err)
	}
	if err := p.save(context.Background(), key, 1, second); err != nil {
		t.Fatal(err)
	}
	if err := p.save(context.Background(), key, 0, first); err == nil {
		t.Fatal("stale save replayed an old native transaction receipt")
	}
}

type testSender struct {
	calls        int
	err          error
	receipt      protocol.SendReceipt
	resolveFound bool
	resolveCalls int
	operations   []id.ID
	groups       []id.ID
	text         []string
}

func (s *testSender) ResolveWelcome(_ context.Context, _, _ id.ID) (protocol.SendReceipt, bool, error) {
	s.resolveCalls++
	return s.receipt, s.resolveFound, nil
}

func (s *testSender) SendWelcome(_ context.Context, req SendRequest) (protocol.SendReceipt, error) {
	s.calls++
	s.operations = append(s.operations, req.OperationID)
	s.groups = append(s.groups, req.GroupID)
	s.text = append(s.text, req.Text)
	s.receipt.OperationID = req.OperationID
	s.receipt.AttemptID = req.AttemptID
	s.receipt.CommunityID = req.CommunityID
	s.receipt.GroupID = req.GroupID
	s.receipt.CiphertextHash = make([]byte, 32)
	return s.receipt, s.err
}

type testMembers struct {
	present bool
	name    string
	err     error
	calls   int
}

type testRoleGranter struct {
	calls   int
	err     error
	user    id.ID
	episode id.ID
}

func (g *testRoleGranter) GrantJoinRole(_ context.Context, user, episode id.ID) error {
	g.calls++
	g.user, g.episode = user, episode
	return g.err
}

func (m *testMembers) ResolveCurrentMember(context.Context, id.ID) (bool, string, error) {
	m.calls++
	return m.present, m.name, m.err
}

type testDestination struct {
	group id.ID
	err   error
}

func (d testDestination) ResolveTextGroup(context.Context, id.ID, id.ID) (id.ID, error) {
	return d.group, d.err
}

func fixture() (*testNative, *testSender, *Processor, id.ID, id.ID, id.ID, id.ID) {
	community, install, user, episode := testID(1), testID(2), testID(3), testID(4)
	n := &testNative{binding: native.Binding{Instance: testID(5), Community: community, Installation: install}, values: map[string]native.StateValue{}, transactions: map[id.ID][]native.StateChange{}}
	channel, group := testID(6), testID(12)
	s := &testSender{receipt: protocol.SendReceipt{OperationID: testID(9), AttemptID: testID(10), CommunityID: community, GroupID: group, Outcome: protocol.ReceiptAccepted, SendResponse: &protocol.OriginalSendResponse{Sequence: 1, FrankingTag: make([]byte, 32), ReceiveTimestamp: 1, Body: mustCBOR([]any{uint64(1), make([]byte, 32), uint64(1)})}}}
	members := &testMembers{present: true, name: "Ada"}
	p, _ := New(n, Options{ChannelID: channel, ExpectedGroupID: group, Destination: testDestination{group: group}, Template: "Welcome {member} to {server} in {channel}", ServerName: "Dilla", ChannelName: "general", Members: members, Sender: s, Now: func() time.Time { return time.Unix(2_000_000, 0) }})
	return n, s, p, community, user, episode, install
}
func testID(v byte) id.ID   { var out id.ID; out[15] = v; return out }
func mustCBOR(v any) []byte { b, _ := cborx.Marshal(v); return b }
func joinEvent(community, user, episode id.ID, ts uint64) protocol.Event {
	return protocol.Event{ID: testID(7), Sequence: 1, CommunityID: community, Timestamp: ts, Kind: protocol.EventMemberJoin, Data: []any{user, episode}}
}

func TestDuplicateJoinSendsOneEpisode(t *testing.T) {
	n, s, p, c, u, e, _ := fixture()
	ev := joinEvent(c, u, e, 1_999_999)
	if err := p.Process(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if err := p.Process(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if s.calls != 1 {
		t.Fatalf("send calls = %d", s.calls)
	}
	if s.text[0] != "Welcome Ada to Dilla in general" {
		t.Fatalf("rendered %q", s.text[0])
	}
	if s.groups[0] == testID(6) || s.groups[0] != testID(12) || s.receipt.GroupID != s.groups[0] {
		t.Fatalf("send group=%s receipt group=%s; want resolved group %s, distinct from channel %s", s.groups[0], s.receipt.GroupID, testID(12), testID(6))
	}
	_ = n
}

func TestConfiguredEventHandlerDistinguishesProvenDisabledFromPending(t *testing.T) {
	n, _, _, community, user, episode, _ := fixture()
	event := joinEvent(community, user, episode, 1_999_999)
	handler := ConfiguredEventHandler(n, nil)
	if err := handler(context.Background(), event); err != nil {
		t.Fatalf("missing config should be a proven no-op: %v", err)
	}
	change, err := ConfigStateChange(Config{ChannelID: testID(6), Template: "hello {member}", ServerName: "Dilla"})
	if err != nil {
		t.Fatal(err)
	}
	change.ExpectedRevision = 0
	if _, err = n.StateTransaction(context.Background(), testID(20), []native.StateChange{change}); err != nil {
		t.Fatal(err)
	}
	if err = handler(context.Background(), event); err == nil || err.Error() != "configured welcome is pending: authenticated current-membership and member-profile APIs are unavailable" {
		t.Fatalf("configured join should remain pending, got %v", err)
	}
}

func TestJoinRoleRunsWithoutWelcomeConfigurationAndFailsClosed(t *testing.T) {
	n, _, _, community, user, episode, _ := fixture()
	event := joinEvent(community, user, episode, 1_999_999)
	granter := &testRoleGranter{}
	handler := ConfiguredEventHandlerWithJoinRole(n, granter, nil)
	if err := handler(context.Background(), event); err != nil {
		t.Fatalf("unconfigured welcome with role grant: %v", err)
	}
	if granter.calls != 1 || granter.user != user || granter.episode != episode {
		t.Fatalf("role grant calls=%d user=%s episode=%s", granter.calls, granter.user, granter.episode)
	}
	granter.err = errors.New("role grant refused")
	if err := handler(context.Background(), event); err == nil || err.Error() != "role grant refused" {
		t.Fatalf("refusal was not propagated: %v", err)
	}
}

func TestDisabledWelcomeCapabilitySkipsConfiguredTextAfterValidatingEvent(t *testing.T) {
	n, _, _, community, user, episode, _ := fixture()
	change, err := ConfigStateChange(Config{ChannelID: testID(6), Template: "hello {member}", ServerName: "Dilla"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = n.StateTransaction(context.Background(), testID(22), []native.StateChange{change}); err != nil {
		t.Fatal(err)
	}
	event := joinEvent(community, user, episode, 1_999_999)
	handler := ConfiguredEventHandlerWithCapabilities(n, nil, false, nil)
	if err := handler(context.Background(), event); err != nil {
		t.Fatalf("disabled welcome should not invoke configured processor: %v", err)
	}
	event.CommunityID = testID(23)
	if err := handler(context.Background(), event); !errors.Is(err, protocol.ErrContext) {
		t.Fatalf("disabled welcome accepted foreign event: %v", err)
	}
}

func TestConfiguredLeaveAdvancesWithoutWelcomeDestination(t *testing.T) {
	n, _, _, community, user, episode, _ := fixture()
	change, err := ConfigStateChange(Config{ChannelID: testID(6), Template: "hello {member}", ServerName: "Dilla"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = n.StateTransaction(context.Background(), testID(21), []native.StateChange{change}); err != nil {
		t.Fatal(err)
	}
	event := joinEvent(community, user, episode, 1_999_999)
	event.Kind = protocol.EventMemberLeave
	if err := ConfiguredEventHandler(n, nil)(context.Background(), event); err != nil {
		t.Fatalf("leave blocked by unavailable configured channel: %v", err)
	}
	p := &Processor{native: n, binding: n.Binding()}
	if status, err := p.Status(context.Background(), user, episode); err != nil || status != StatusLeft {
		t.Fatalf("leave status = %d, %v", status, err)
	}
}

func TestConfiguredEventHandlerAllowsReconciledStructuralEventsBeforeJoin(t *testing.T) {
	n, _, _, community, user, episode, _ := fixture()
	handler := ConfiguredEventHandler(n, nil)
	for _, event := range []protocol.Event{
		{ID: testID(41), Sequence: 1, CommunityID: community, Timestamp: 100, Kind: protocol.EventInstallation,
			Data: []any{testID(42), uint64(1), uint64(1)}},
		{ID: testID(44), Sequence: 2, CommunityID: community, Timestamp: 100, Kind: protocol.EventPermission,
			Data: []any{uint64(1), testID(45), uint64(1)}},
		{ID: testID(47), Sequence: 3, CommunityID: community, Timestamp: 100, Kind: protocol.EventChannel,
			Data: []any{testID(48), uint64(1), uint64(1)}},
	} {
		if err := handler(context.Background(), event); err != nil {
			t.Fatalf("reconciled event kind %d blocks later joins: %v", event.Kind, err)
		}
	}
	if err := handler(context.Background(), joinEvent(community, user, episode, 1_999_999)); err != nil {
		t.Fatalf("unconfigured join after structural events: %v", err)
	}
}

func TestConfiguredEventHandlerValidatesThenIgnoresOtherKnownStructuralEvents(t *testing.T) {
	n, _, _, community, _, _, _ := fixture()
	handler := ConfiguredEventHandler(n, nil)
	for _, event := range []protocol.Event{
		{ID: testID(21), Sequence: 1, CommunityID: community, Timestamp: 100, Kind: protocol.EventModeration,
			Data: []any{testID(22), testID(23), testID(24), testID(25), testID(26), nil, nil, uint64(0), nil}},
		{ID: testID(31), Sequence: 2, CommunityID: community, Timestamp: 101, Kind: protocol.EventRolePanel,
			Data: []any{testID(32), uint64(1), uint64(0)}},
		{ID: testID(33), Sequence: 3, CommunityID: community, Timestamp: 102, Kind: protocol.EventDeviceList,
			Data: []any{testID(34), uint64(2)}},
	} {
		if err := handler(context.Background(), event); err != nil {
			t.Fatalf("known unrelated event kind %d blocked dispatch: %v", event.Kind, err)
		}
		foreign := event
		foreign.CommunityID = testID(99)
		if err := handler(context.Background(), foreign); !errors.Is(err, protocol.ErrContext) {
			t.Fatalf("foreign event kind %d was ignored before context validation: %v", event.Kind, err)
		}
		malformed := event
		malformed.Data = nil
		if err := handler(context.Background(), malformed); !errors.Is(err, protocol.ErrInvalid) {
			t.Fatalf("malformed event kind %d was ignored before codec validation: %v", event.Kind, err)
		}
	}
}

func TestMalformedWelcomeConfigFailsClosed(t *testing.T) {
	n, _, _, community, user, episode, _ := fixture()
	n.values[stateMapKey(stateArea, []byte(configKey))] = native.StateValue{Revision: 1, Value: []byte{0xff}}
	err := ConfiguredEventHandler(n, nil)(context.Background(), joinEvent(community, user, episode, 1_999_999))
	if err == nil || err.Error() != "malformed welcome configuration" {
		t.Fatalf("malformed state error=%v", err)
	}
}

func TestChangedOrMissingDestinationFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		to   testDestination
	}{
		{name: "changed group", to: testDestination{group: testID(13)}},
		{name: "missing group", to: testDestination{}},
		{name: "resolver error", to: testDestination{err: errors.New("channel is no longer authorized")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, s, p, c, u, e, _ := fixture()
			p.destination = tc.to
			if err := p.Process(context.Background(), joinEvent(c, u, e, 1_999_999)); err == nil {
				t.Fatal("expected destination resolution failure")
			}
			if s.calls != 0 {
				t.Fatalf("sent %d messages to invalid destination", s.calls)
			}
		})
	}
}

func TestRejoinEpisodeHasDifferentOperation(t *testing.T) {
	_, s, p, c, u, e, _ := fixture()
	first := joinEvent(c, u, e, 1_999_999)
	if err := p.Process(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	next := testID(8)
	if err := p.Process(context.Background(), joinEvent(c, u, next, 1_999_999)); err != nil {
		t.Fatal(err)
	}
	if s.calls != 2 || s.operations[0] == s.operations[1] {
		t.Fatalf("rejoin not distinct: calls=%d", s.calls)
	}
}

func TestOldJoinIsMarkedMissedWithoutSend(t *testing.T) {
	_, s, p, c, u, e, _ := fixture()
	ev := joinEvent(c, u, e, 1_000_000)
	if err := p.Process(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if s.calls != 0 {
		t.Fatalf("old event sent %d messages", s.calls)
	}
}

func TestLeftMemberIsNotWelcomed(t *testing.T) {
	_, s, p, c, u, e, _ := fixture()
	p.members = &testMembers{present: false}
	if err := p.Process(context.Background(), joinEvent(c, u, e, 1_999_999)); err != nil {
		t.Fatal(err)
	}
	if s.calls != 0 {
		t.Fatal("sent to a member who already left")
	}
	if state, err := p.Status(context.Background(), u, e); err != nil || state != StatusLeft {
		t.Fatalf("status=%v err=%v, want left", state, err)
	}
}

func TestMemberProfileFailureLeavesWelcomePendingAndRetries(t *testing.T) {
	_, s, p, c, u, e, _ := fixture()
	members := &testMembers{present: true, name: "Ada", err: errors.New("profile API unavailable")}
	p.members = members
	ev := joinEvent(c, u, e, 1_999_999)
	if err := p.Process(context.Background(), ev); err == nil {
		t.Fatal("profile lookup failure should be retryable")
	}
	if members.calls != 1 || s.calls != 0 {
		t.Fatalf("profile calls=%d sends=%d after failure", members.calls, s.calls)
	}
	if state, err := p.Status(context.Background(), u, e); err != nil || state != StatusPending {
		t.Fatalf("status=%v err=%v, want pending", state, err)
	}
	members.err = nil
	if err := p.Process(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if members.calls != 2 || s.calls != 1 || s.text[0] != "Welcome Ada to Dilla in general" {
		t.Fatalf("profile calls=%d sends=%d text=%v", members.calls, s.calls, s.text)
	}
}

func TestAmbiguousSendRemainsPendingForSameOperation(t *testing.T) {
	_, s, p, c, u, e, _ := fixture()
	s.err = errors.New("response lost")
	ev := joinEvent(c, u, e, 1_999_999)
	if err := p.Process(context.Background(), ev); err == nil {
		t.Fatal("expected ambiguous send error")
	}
	first := s.operations[0]
	s.err = nil
	if err := p.Process(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if s.operations[1] != first {
		t.Fatal("retry changed operation id")
	}
	state, err := p.Status(context.Background(), u, e)
	if err != nil || state != StatusPosted {
		t.Fatalf("state=%v err=%v", state, err)
	}
}

func TestExpiredPendingJoinResolvesReceiptWithoutLateSend(t *testing.T) {
	_, s, p, c, u, e, _ := fixture()
	s.err = errors.New("response lost")
	ev := joinEvent(c, u, e, 1_999_999)
	if err := p.Process(context.Background(), ev); err == nil {
		t.Fatal("expected ambiguous send")
	}
	p.now = func() time.Time { return time.Unix(2_000_000, 0).Add(25 * time.Hour) }
	s.err = nil
	if err := p.Process(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if s.calls != 1 || s.resolveCalls != 1 {
		t.Fatalf("late sends=%d receipt lookups=%d", s.calls, s.resolveCalls)
	}
	if status, err := p.Status(context.Background(), u, e); err != nil || status != StatusMissed {
		t.Fatalf("status=%v err=%v", status, err)
	}
}

func TestExpiredPendingAcceptedReceiptIsPostedWithoutResend(t *testing.T) {
	_, s, p, c, u, e, _ := fixture()
	s.err = errors.New("response lost")
	ev := joinEvent(c, u, e, 1_999_999)
	if err := p.Process(context.Background(), ev); err == nil {
		t.Fatal("expected ambiguous send")
	}
	p.now = func() time.Time { return time.Unix(2_000_000, 0).Add(25 * time.Hour) }
	s.resolveFound = true
	s.err = nil
	if err := p.Process(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if s.calls != 1 || s.resolveCalls != 1 {
		t.Fatalf("late sends=%d receipt lookups=%d", s.calls, s.resolveCalls)
	}
	if status, err := p.Status(context.Background(), u, e); err != nil || status != StatusPosted {
		t.Fatalf("status=%v err=%v", status, err)
	}
}

func TestExpiredPendingResolvesReceiptAfterDestinationLoss(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		_, s, p, c, u, e, _ := fixture()
		s.err = errors.New("response lost")
		ev := joinEvent(c, u, e, 1_999_999)
		if err := p.Process(context.Background(), ev); err == nil {
			t.Fatal("expected ambiguous send")
		}
		p.now = func() time.Time { return time.Unix(2_000_000, 0).Add(25 * time.Hour) }
		p.destination = testDestination{err: errors.New("channel deleted")}
		s.resolveFound = accepted
		s.err = nil
		if err := p.Process(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
		want := StatusMissed
		if accepted {
			want = StatusPosted
		}
		if status, err := p.Status(context.Background(), u, e); err != nil || status != want {
			t.Fatalf("accepted=%v status=%v err=%v", accepted, status, err)
		}
		if s.calls != 1 || s.resolveCalls != 1 {
			t.Fatalf("accepted=%v sends=%d lookups=%d", accepted, s.calls, s.resolveCalls)
		}
	}
}

func TestCrossCommunityEventRejected(t *testing.T) {
	_, s, p, _, u, e, _ := fixture()
	if err := p.Process(context.Background(), joinEvent(testID(99), u, e, 1_999_999)); err == nil {
		t.Fatal("accepted foreign event")
	}
	if s.calls != 0 {
		t.Fatal("foreign event triggered a send")
	}
}

func TestLeaveEventRefusesExistingJoinEpisode(t *testing.T) {
	_, s, p, c, u, e, _ := fixture()
	leave := protocol.Event{ID: testID(11), Sequence: 2, CommunityID: c, Timestamp: 1_999_999, Kind: protocol.EventMemberLeave, Data: []any{u, e}}
	if err := p.Process(context.Background(), leave); err != nil {
		t.Fatal(err)
	}
	if err := p.Process(context.Background(), joinEvent(c, u, e, 1_999_999)); err != nil {
		t.Fatal(err)
	}
	if s.calls != 0 {
		t.Fatal("sent after leave event")
	}
}

func TestUnacceptedReceiptDoesNotMarkPosted(t *testing.T) {
	_, s, p, c, u, e, _ := fixture()
	s.receipt.Outcome = protocol.ReceiptUnknownAfterRestore
	s.receipt.SendResponse = nil
	if err := p.Process(context.Background(), joinEvent(c, u, e, 1_999_999)); err == nil {
		t.Fatal("expected unknown receipt to remain unresolved")
	}
	state, err := p.Status(context.Background(), u, e)
	if err != nil || state != StatusPending {
		t.Fatalf("state=%v err=%v", state, err)
	}
}

func TestConcurrentDuplicateJoinUsesOneSend(t *testing.T) {
	_, s, p, c, u, e, _ := fixture()
	ev := joinEvent(c, u, e, 1_999_999)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.Process(context.Background(), ev); err != nil {
				t.Errorf("process: %v", err)
			}
		}()
	}
	wg.Wait()
	if s.calls != 1 {
		t.Fatalf("send calls=%d", s.calls)
	}
}

func TestRoleGrantIsExplicitlyUnavailable(t *testing.T) {
	_, _, p, _, u, e, _ := fixture()
	if err := p.GrantRole(context.Background(), u, e); !errors.Is(err, ErrRoleGrantUnavailable) {
		t.Fatalf("grant result=%v", err)
	}
}

// scriptedSender answers each send with the next outcome in its script (the last one repeats) and records every request.
type scriptedSender struct {
	outcomes []protocol.SendOutcome
	requests []SendRequest
	resolved []id.ID
}

func (s *scriptedSender) SendWelcome(_ context.Context, req SendRequest) (protocol.SendReceipt, error) {
	s.requests = append(s.requests, req)
	outcome := s.outcomes[min(len(s.requests)-1, len(s.outcomes)-1)]
	r := protocol.SendReceipt{OperationID: req.OperationID, AttemptID: req.AttemptID, PredecessorAttemptID: req.PredecessorAttemptID,
		CommunityID: req.CommunityID, GroupID: req.GroupID, CiphertextHash: make([]byte, 32), Outcome: outcome}
	if outcome == protocol.ReceiptAccepted {
		r.SendResponse = &protocol.OriginalSendResponse{Sequence: 1, FrankingTag: make([]byte, 32), ReceiveTimestamp: 1, Body: mustCBOR([]any{uint64(1), make([]byte, 32), uint64(1)})}
	}
	return r, nil
}

func (s *scriptedSender) ResolveWelcome(_ context.Context, _, attempt id.ID) (protocol.SendReceipt, bool, error) {
	s.resolved = append(s.resolved, attempt)
	return protocol.SendReceipt{}, false, nil
}

func scriptedFixture(outcomes ...protocol.SendOutcome) (*testNative, *scriptedSender, *Processor, *testMembers, id.ID, id.ID, id.ID) {
	n, _, p, c, u, e, _ := fixture()
	s := &scriptedSender{outcomes: outcomes}
	p.sender = s
	members := p.members.(*testMembers)
	return n, s, p, members, c, u, e
}

// storedEpisode reads the episode back from the native state store.
func storedEpisode(t *testing.T, p *Processor, user, episodeID id.ID) episode {
	t.Helper()
	v, _, err := p.load(context.Background(), p.stateKey(user, episodeID))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// The server closed attempt 0 (the group was busy): the episode stays pending with a successor attempt, and the next run
// sends attempt 1 chained to the closed one. An accepted successor posts the welcome.
func TestClosedReceiptRetriesWithSuccessor(t *testing.T) {
	_, s, p, _, c, u, e := scriptedFixture(protocol.ReceiptClosedNotAccepted, protocol.ReceiptAccepted)
	ev := joinEvent(c, u, e, 1_999_999)
	if err := p.Process(context.Background(), ev); !errors.Is(err, ErrRetryLater) {
		t.Fatalf("closed receipt: err=%v, want ErrRetryLater", err)
	}
	op := operationID(p.binding.Installation, u, e)
	if got := storedEpisode(t, p, u, e); got.Status != StatusPending || got.Attempt != 1 {
		t.Fatalf("after a closed receipt: status=%v attempt=%d, want pending 1", got.Status, got.Attempt)
	}
	if first := s.requests[0]; first.AttemptID != attemptID(op) || first.PredecessorAttemptID != nil {
		t.Fatalf("attempt 0 = %s (predecessor %v), want the v1 id and no predecessor", first.AttemptID, first.PredecessorAttemptID)
	}
	if err := p.Process(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if len(s.requests) != 2 {
		t.Fatalf("sends=%d, want 2", len(s.requests))
	}
	second := s.requests[1]
	if second.OperationID != op || second.AttemptID != attemptID(op, 1) || second.AttemptID == attemptID(op) ||
		second.PredecessorAttemptID == nil || *second.PredecessorAttemptID != attemptID(op) {
		t.Fatalf("successor attempt=%s predecessor=%v", second.AttemptID, second.PredecessorAttemptID)
	}
	if got := storedEpisode(t, p, u, e); got.Status != StatusPosted {
		t.Fatalf("status=%v, want posted", got.Status)
	}
	if err := p.Process(context.Background(), ev); err != nil || len(s.requests) != 2 {
		t.Fatalf("a posted episode was processed again: err=%v sends=%d", err, len(s.requests))
	}
}

// Attempt n's id is the v2 domain plus the operation plus n as a big-endian uint32; attempt 0 keeps the v1 id.
func TestAttemptIDDerivation(t *testing.T) {
	op := testID(42)
	want := sha256.Sum256(append(append([]byte("dilla.bot.welcome.attempt.v2\x00"), op[:]...), 0, 0, 0, 3))
	var w id.ID
	copy(w[:], want[:16])
	if attemptID(op, 3) != w {
		t.Fatalf("attempt 3 = %s, want %s", attemptID(op, 3), w)
	}
	v1 := sha256.Sum256(append([]byte("dilla.bot.welcome.attempt.v1\x00"), op[:]...))
	var w0 id.ID
	copy(w0[:], v1[:16])
	if attemptID(op) != w0 || attemptID(op, 0) != w0 {
		t.Fatal("attempt 0 changed its id")
	}
}

// A join seen before the Bot joined the channel's group defers: nothing is sent, the episode stays pending at attempt 0.
func TestNotJoinedIsRetryable(t *testing.T) {
	_, s, p, _, c, u, e := scriptedFixture(protocol.ReceiptAccepted)
	p.destination = testDestination{err: fmt.Errorf("bot has not joined the channel's group yet: %w", ErrRetryLater)}
	ev := joinEvent(c, u, e, 1_999_999)
	if err := p.Process(context.Background(), ev); !errors.Is(err, ErrRetryLater) {
		t.Fatalf("not joined: err=%v, want ErrRetryLater", err)
	}
	if len(s.requests) != 0 {
		t.Fatalf("sent %d while not joined", len(s.requests))
	}
	if got := storedEpisode(t, p, u, e); got.Status != StatusPending || got.Attempt != 0 {
		t.Fatalf("status=%v attempt=%d", got.Status, got.Attempt)
	}
	p.destination = testDestination{group: testID(12)}
	if err := p.Process(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if len(s.requests) != 1 || s.requests[0].PredecessorAttemptID != nil {
		t.Fatalf("after joining: sends=%d", len(s.requests))
	}
}

// While the destination group has pending proposals or a pending commit, nothing is sent and no attempt is used up.
func TestPendingGroupDefers(t *testing.T) {
	_, s, p, _, c, u, e := scriptedFixture(protocol.ReceiptAccepted)
	p.destination = testDestination{err: fmt.Errorf("group busy: %w", ErrRetryLater)}
	ev := joinEvent(c, u, e, 1_999_999)
	for range 3 {
		if err := p.Process(context.Background(), ev); !errors.Is(err, ErrRetryLater) {
			t.Fatalf("busy group: err=%v", err)
		}
	}
	if len(s.requests) != 0 {
		t.Fatalf("sent %d into a busy group", len(s.requests))
	}
	if got := storedEpisode(t, p, u, e); got.Status != StatusPending || got.Attempt != 0 {
		t.Fatalf("status=%v attempt=%d, want pending 0", got.Status, got.Attempt)
	}
}

// A server that keeps refusing: the 8th closed receipt ends the episode as failed, and nothing is sent after that.
func TestRetryBoundGivesUp(t *testing.T) {
	_, s, p, _, c, u, e := scriptedFixture(protocol.ReceiptClosedNotAccepted)
	ev := joinEvent(c, u, e, 1_999_999)
	for i := 1; i < maxAttempts; i++ {
		if err := p.Process(context.Background(), ev); !errors.Is(err, ErrRetryLater) {
			t.Fatalf("closed receipt %d: err=%v, want ErrRetryLater", i, err)
		}
	}
	if err := p.Process(context.Background(), ev); !errors.Is(err, ErrGaveUp) || errors.Is(err, ErrRetryLater) {
		t.Fatalf("closed receipt %d: err=%v, want ErrGaveUp", maxAttempts, err)
	}
	if len(s.requests) != 8 || maxAttempts != 8 {
		t.Fatalf("sends=%d (max %d), want 8", len(s.requests), maxAttempts)
	}
	if got := storedEpisode(t, p, u, e); got.Status != StatusFailed {
		t.Fatalf("status=%v, want failed", got.Status)
	}
	if err := p.Process(context.Background(), ev); err != nil || len(s.requests) != 8 {
		t.Fatalf("a failed episode was retried: err=%v sends=%d", err, len(s.requests))
	}
	op := operationID(p.binding.Installation, u, e)
	for i, r := range s.requests {
		if r.AttemptID != attemptID(op, uint32(i)) {
			t.Fatalf("send %d used attempt %s", i, r.AttemptID)
		}
		if i > 0 && (r.PredecessorAttemptID == nil || *r.PredecessorAttemptID != s.requests[i-1].AttemptID) {
			t.Fatalf("send %d is not chained to send %d", i, i-1)
		}
	}
}

// A member who left before the retry runs gets no welcome.
func TestLeftMemberNotWelcomedOnRetry(t *testing.T) {
	_, s, p, members, c, u, e := scriptedFixture(protocol.ReceiptClosedNotAccepted, protocol.ReceiptAccepted)
	ev := joinEvent(c, u, e, 1_999_999)
	if err := p.Process(context.Background(), ev); !errors.Is(err, ErrRetryLater) {
		t.Fatalf("err=%v", err)
	}
	members.present = false
	if err := p.Process(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if len(s.requests) != 1 {
		t.Fatalf("sends=%d, want only the first", len(s.requests))
	}
	if got := storedEpisode(t, p, u, e); got.Status != StatusLeft {
		t.Fatalf("status=%v, want left", got.Status)
	}
}

// After a restart (a fresh processor over the same state), a posted episode is not sent again, and a pending one's
// closed attempt is not reused: the stored attempt decides.
func TestPostedEpisodeNeverResent(t *testing.T) {
	n, s, p, _, c, u, e := scriptedFixture(protocol.ReceiptClosedNotAccepted, protocol.ReceiptAccepted)
	ev := joinEvent(c, u, e, 1_999_999)
	if err := p.Process(context.Background(), ev); !errors.Is(err, ErrRetryLater) {
		t.Fatalf("err=%v", err)
	}
	restart := func() *Processor {
		q, err := New(n, Options{ChannelID: p.channel, ExpectedGroupID: p.expectedGroup, Destination: p.destination, Template: p.template, ServerName: p.serverName, ChannelName: p.channelName, Members: p.members, Sender: s, Now: p.now})
		if err != nil {
			t.Fatal(err)
		}
		return q
	}
	if err := restart().Process(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	op := operationID(p.binding.Installation, u, e)
	if len(s.requests) != 2 || s.requests[1].AttemptID != attemptID(op, 1) {
		t.Fatalf("after restart: sends=%d", len(s.requests))
	}
	if err := restart().Process(context.Background(), ev); err != nil || len(s.requests) != 2 {
		t.Fatalf("a posted episode was resent after restart: err=%v sends=%d", err, len(s.requests))
	}
}

// A pending episode past the catch-up cutoff resolves its current (successor) attempt, not attempt 0.
func TestExpiredPendingResolvesCurrentAttempt(t *testing.T) {
	_, s, p, _, c, u, e := scriptedFixture(protocol.ReceiptClosedNotAccepted)
	ev := joinEvent(c, u, e, 1_999_999)
	for range 2 {
		if err := p.Process(context.Background(), ev); !errors.Is(err, ErrRetryLater) {
			t.Fatalf("err=%v", err)
		}
	}
	p.now = func() time.Time { return time.Unix(2_000_000, 0).Add(25 * time.Hour) }
	if err := p.Process(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	op := operationID(p.binding.Installation, u, e)
	if len(s.resolved) != 1 || s.resolved[0] != attemptID(op, 2) || len(s.requests) != 2 {
		t.Fatalf("resolved=%v sends=%d, want attempt 2 resolved and no late send", s.resolved, len(s.requests))
	}
	if got := storedEpisode(t, p, u, e); got.Status != StatusMissed {
		t.Fatalf("status=%v, want missed", got.Status)
	}
}

// Episodes stored before attempts existed (v1 rows, five fields) read back as attempt 0; a row with an attempt or the
// failed status round-trips.
func TestEpisodeStateIsBackwardCompatible(t *testing.T) {
	n, _, p, _, _, u, e := scriptedFixture(protocol.ReceiptAccepted)
	key := p.stateKey(u, e)
	v1 := mustCBOR([]any{uint64(1), uint64(StatusPending), testID(7), uint64(1_999_999), testID(8)})
	n.values[stateMapKey(stateArea, key)] = native.StateValue{Revision: 1, Value: v1}
	got, rev, err := p.load(context.Background(), key)
	if err != nil || rev != 1 || got.Status != StatusPending || got.Attempt != 0 || got.OperationID != testID(8) {
		t.Fatalf("v1 row: %+v rev=%d err=%v", got, rev, err)
	}
	for i, want := range []episode{
		{Status: StatusPending, EventID: testID(7), Timestamp: 5, OperationID: testID(8), Attempt: 3},
		{Status: StatusFailed, EventID: testID(7), Timestamp: 5, OperationID: testID(8), Attempt: 7},
	} {
		if err := p.save(context.Background(), key, uint64(i+1), want); err != nil {
			t.Fatal(err)
		}
		if got, _, err := p.load(context.Background(), key); err != nil || got != want {
			t.Fatalf("round trip %d: %+v err=%v, want %+v", i, got, err, want)
		}
	}
}
