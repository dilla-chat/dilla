//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/polls"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/bot/reminders"
	"github.com/dilla-chat/dilla/internal/bot/service"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

type utilityStateFake struct {
	mu                sync.Mutex
	binding           native.Binding
	values            map[string]native.StateValue
	otherAreas        map[string]native.StateValue
	scanCalls         int
	failScanAt        int
	failMarkPublished bool
}

func newUtilityStateFake() *utilityStateFake {
	return &utilityStateFake{binding: native.Binding{Instance: utilityID(1), Community: utilityID(2), Installation: utilityID(3)}, values: map[string]native.StateValue{}, otherAreas: map[string]native.StateValue{}}
}
func (s *utilityStateFake) Binding() native.Binding { return s.binding }
func (s *utilityStateFake) StateGet(_ context.Context, area uint64, key []byte) (native.StateValue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if area != 8 {
		if v, ok := s.otherAreas[fmt.Sprintf("%d:%x", area, key)]; ok {
			return v, nil
		}
		return native.StateValue{}, nil
	}
	return s.values[string(key)], nil
}
func (s *utilityStateFake) StateTransaction(_ context.Context, _ id.ID, changes []native.StateChange) ([]uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]uint64, len(changes))
	for i, ch := range changes {
		if ch.Area != 8 {
			stateKey := fmt.Sprintf("%d:%x", ch.Area, ch.Key)
			v := s.otherAreas[stateKey]
			if v.Revision != ch.ExpectedRevision {
				return nil, errors.New("state conflict")
			}
			v.Revision++
			v.Value = append([]byte(nil), ch.Value...)
			v.Tombstone = ch.Value == nil
			s.otherAreas[stateKey] = v
			out[i] = v.Revision
			continue
		}
		v := s.values[string(ch.Key)]
		if ch.Area != 8 || v.Revision != ch.ExpectedRevision {
			return nil, errors.New("state conflict")
		}
		v.Revision++
		v.Value = append([]byte(nil), ch.Value...)
		v.Tombstone = ch.Value == nil
		if s.failMarkPublished && strings.HasPrefix(string(ch.Key), "dilla.bot.poll.v1/") && len(ch.Value) > 0 {
			var row []any
			if cborx.Unmarshal(ch.Value, &row) == nil && len(row) == 15 && row[14] == uint64(1) {
				s.failMarkPublished = false
				return nil, errors.New("injected published marker failure")
			}
		}
		s.values[string(ch.Key)] = v
		out[i] = v.Revision
	}
	return out, nil
}
func (s *utilityStateFake) ConfirmSendReceipt(context.Context, id.ID, []byte) error { return nil }
func (s *utilityStateFake) StateScan(_ context.Context, area uint64, after []byte, limit uint64) (native.StatePage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scanCalls++
	if s.failScanAt > 0 && s.scanCalls == s.failScanAt {
		return native.StatePage{}, errors.New("injected scan failure")
	}
	rows := s.values
	if area != 8 {
		return native.StatePage{}, nil
	}
	keys := make([]string, 0, len(rows))
	for key := range rows {
		if len(after) == 0 || key > string(after) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) > int(limit) {
		keys = keys[:limit]
	}
	page := native.StatePage{}
	for _, key := range keys {
		v := rows[key]
		page.Entries = append(page.Entries, native.StateEntry{Key: []byte(key), Revision: v.Revision, Value: append([]byte(nil), v.Value...), Tombstone: v.Tombstone})
	}
	if len(keys) > 0 {
		page.Next = []byte(keys[len(keys)-1])
	}
	return page, nil
}
func utilityID(n byte) id.ID { var v id.ID; v[15] = n; return v }

type utilityDiscoveryFake struct {
	instance   id.ID
	generation uint64
}

func (a utilityDiscoveryFake) DiscoverObserved(context.Context) (client.Discovery, client.ResponseObservation, error) {
	return client.Discovery{InstanceID: a.instance, Generation: a.generation}, client.ResponseObservation{}, nil
}

type utilityDestinationFake struct{ group id.ID }

func (d utilityDestinationFake) ResolveTextGroup(context.Context, id.ID, id.ID) (id.ID, error) {
	return d.group, nil
}

type utilityChannelAuthorityFake struct {
	member bool
	bits   uint64
	err    error
}

func (a *utilityChannelAuthorityFake) BotActorAuthority(context.Context, id.ID, id.ID) (client.ActorAuthority, bool, error) {
	return client.ActorAuthority{}, a.member, a.err
}
func (a *utilityChannelAuthorityFake) BotActorChannelAuthority(context.Context, id.ID, id.ID, id.ID) (uint64, bool, error) {
	return a.bits, a.member, a.err
}

type utilityCoordinatorFake struct {
	mu           sync.Mutex
	outcomes     []protocol.SendOutcome
	inputs       []service.SendInput
	preflightErr error
}

func (c *utilityCoordinatorFake) SendAuthorized(ctx context.Context, _ service.SendSession, in service.SendInput, preflight func(context.Context) error) (protocol.SendReceipt, error) {
	if err := preflight(ctx); err != nil {
		return protocol.SendReceipt{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inputs = append(c.inputs, in)
	outcome := protocol.ReceiptAccepted
	if len(c.outcomes) > 0 {
		outcome = c.outcomes[0]
		c.outcomes = c.outcomes[1:]
	}
	receipt := protocol.SendReceipt{OperationID: in.OperationID, AttemptID: in.AttemptID, CommunityID: utilityID(2), GroupID: in.GroupID, CiphertextHash: make([]byte, 32), ExpectedGeneration: in.ExpectedGeneration, Outcome: outcome}
	if outcome == protocol.ReceiptAccepted {
		response, _ := cborx.Marshal([]any{uint64(1), make([]byte, 32), uint64(1234)})
		receipt.SendResponse = &protocol.OriginalSendResponse{Sequence: 1, FrankingTag: make([]byte, 32), ReceiveTimestamp: 1234, Body: response}
	}
	if c.preflightErr != nil {
		return protocol.SendReceipt{}, c.preflightErr
	}
	if outcome == protocol.ReceiptUnknownAfterRestore {
		return receipt, service.ErrSendTerminal
	}
	return receipt, nil
}

type utilityReceiptLookupFake struct{}

func (utilityReceiptLookupFake) BotMessageReceipt(context.Context, id.ID, id.ID, id.ID, id.ID) (protocol.SendReceipt, error) {
	return protocol.SendReceipt{}, client.ErrBotSendReceiptNotFound
}
func utilityPoll() polls.Poll {
	return polls.Poll{ID: utilityID(4), CommunityID: utilityID(2), ChannelID: utilityID(5), CreatorID: utilityID(6), Question: "Where?", Answers: []string{"Here", "There"}, DueAt: uint64(time.Now().Add(time.Hour).Unix())}
}
func newPublisherTest(state *utilityStateFake, c *utilityCoordinatorFake, auth *utilityChannelAuthorityFake) *durablePollPublisher {
	return &durablePollPublisher{state: state, coordinator: c, destination: utilityDestinationFake{group: utilityID(7)}, authority: utilityDiscoveryFake{instance: state.binding.Instance, generation: 9}, session: &client.Session{}, receipts: utilityReceiptLookupFake{}, actorAuthority: auth, community: state.binding.Community, installation: state.binding.Installation, now: func() time.Time { return time.Unix(1234, 0).UTC() }}
}
func TestUtilityRuntimeRegistersPollAndReminderOperations(t *testing.T) {
	u := &utilityRuntime{polls: service.PollInteractions{Store: polls.NewStore(newUtilityStateFake(), utilityID(2)), Authority: utilityActorAuthority{session: &client.Session{}, community: utilityID(2)}, CommunityID: utilityID(2)}}
	got := u.handlers()
	// The due reminders are the sealed read 93's handler (Bot v2 Task 23 removed the v1 operation 84).
	want := []protocol.Operation{protocol.OpPollCreate, protocol.OpPollGet, protocol.OpPollList, protocol.OpPollVote, protocol.OpPollClose, protocol.OpPollVoters, protocol.OpReminderCreate, protocol.OpReminderList, protocol.OpReminderCancel, protocol.OpReminderDismiss, protocol.OpReadRemindersDue}
	for _, op := range want {
		if _, ok := got[op]; !ok {
			t.Fatalf("runtime forgot utility operation %d", op)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("unexpected registered handlers: %v", len(got))
	}
}

func TestUtilityMaintenanceIsolatesReminderTickFailureFromPollReconcile(t *testing.T) {
	state := newUtilityStateFake()
	state.failScanAt = 1
	now := time.Unix(2000, 0).UTC()
	u := &utilityRuntime{
		reminders: service.ReminderInteractions{Store: reminders.NewStore(state, state.binding, func() time.Time { return now })},
		polls:     service.PollInteractions{Store: polls.NewStore(state, state.binding.Community), Publisher: utilityPublisherNoop{}},
	}
	err := u.maintain(context.Background())
	if err == nil || !strings.Contains(err.Error(), "reminder tick") {
		t.Fatalf("expected isolated reminder error, got %v", err)
	}
	state.mu.Lock()
	calls := state.scanCalls
	state.mu.Unlock()
	if calls != 2 {
		t.Fatalf("poll reconciliation did not run after reminder failure; scans=%d", calls)
	}
}

type utilityPublisherNoop struct{}

func (utilityPublisherNoop) PublishPoll(context.Context, polls.Poll) error { return nil }
func TestPollPublisherPersistsAcceptedReceiptAndRestartDoesNotSendAgain(t *testing.T) {
	state := newUtilityStateFake()
	coord := &utilityCoordinatorFake{}
	auth := &utilityChannelAuthorityFake{member: true, bits: uint64(api.PermViewChannel | api.PermSendMessages | api.PermReadHistory)}
	p := newPublisherTest(state, coord, auth)
	poll := utilityPoll()
	if err := p.PublishPoll(context.Background(), poll); err != nil {
		t.Fatal(err)
	}
	recordRaw := state.values[string(publicationKey(poll.ID))]
	if recordRaw.Revision != 2 {
		t.Fatalf("want prepared and receipt revisions, got %d", recordRaw.Revision)
	}
	// A new process reconstructs the adapter from the same encrypted native state.
	restarted := newPublisherTest(state, coord, auth)
	if err := restarted.PublishPoll(context.Background(), poll); err != nil {
		t.Fatal(err)
	}
	if len(coord.inputs) != 1 {
		t.Fatalf("restart sent again: %d sends", len(coord.inputs))
	}
	var record utilityPublisherState
	var err error
	record, err = decodePublication(recordRaw.Value)
	if err != nil {
		t.Fatal(err)
	}
	if record.operation != poll.ID || record.phase != 1 || len(record.receipt) == 0 || record.generation != 9 || record.originalNow != 1234 {
		t.Fatalf("incomplete durable receipt record: %+v", record)
	}
}
func TestPollPublisherClosedReceiptAllowsSuccessorButUnknownIsTerminal(t *testing.T) {
	state := newUtilityStateFake()
	coord := &utilityCoordinatorFake{outcomes: []protocol.SendOutcome{protocol.ReceiptClosedNotAccepted, protocol.ReceiptAccepted}}
	auth := &utilityChannelAuthorityFake{member: true, bits: utilityPollSendPermission}
	poll := utilityPoll()
	if err := newPublisherTest(state, coord, auth).PublishPoll(context.Background(), poll); err == nil {
		t.Fatal("closed nonaccepted publication was reported successful")
	}
	if err := newPublisherTest(state, coord, auth).PublishPoll(context.Background(), poll); err != nil {
		t.Fatal(err)
	}
	if len(coord.inputs) != 2 || coord.inputs[0].AttemptID == coord.inputs[1].AttemptID || coord.inputs[1].PredecessorAttemptID == nil || *coord.inputs[1].PredecessorAttemptID != coord.inputs[0].AttemptID {
		t.Fatalf("successor did not follow recorded closed receipt: %+v", coord.inputs)
	}
	state2 := newUtilityStateFake()
	unknown := &utilityCoordinatorFake{outcomes: []protocol.SendOutcome{protocol.ReceiptUnknownAfterRestore}}
	p := newPublisherTest(state2, unknown, auth)
	if err := p.PublishPoll(context.Background(), poll); !errors.Is(err, service.ErrSendTerminal) {
		t.Fatalf("unknown result error=%v", err)
	}
	if err := newPublisherTest(state2, unknown, auth).PublishPoll(context.Background(), poll); !errors.Is(err, service.ErrSendTerminal) {
		t.Fatalf("unknown restart error=%v", err)
	}
	if len(unknown.inputs) != 1 {
		t.Fatalf("unknown send was retried: %d calls", len(unknown.inputs))
	}
}
func TestPollPublisherRechecksCreatorCurrentAccessBeforeRetry(t *testing.T) {
	state := newUtilityStateFake()
	coord := &utilityCoordinatorFake{preflightErr: errors.New("transient send failure")}
	auth := &utilityChannelAuthorityFake{member: true, bits: utilityPollSendPermission}
	poll := utilityPoll()
	if err := newPublisherTest(state, coord, auth).PublishPoll(context.Background(), poll); err == nil {
		t.Fatal("expected first send to fail")
	}
	auth.member = false
	auth.bits = 0
	coord.preflightErr = nil
	if err := newPublisherTest(state, coord, auth).PublishPoll(context.Background(), poll); err == nil {
		t.Fatal("revoked creator was allowed to resume publication")
	}
	if len(coord.inputs) != 1 {
		t.Fatalf("revoked creator reached send coordinator: %d", len(coord.inputs))
	}
}

func TestPollPublisherKeepsOriginalGroupForPreparedAttemptAcrossDiscoveryChange(t *testing.T) {
	state := newUtilityStateFake()
	coord := &utilityCoordinatorFake{preflightErr: errors.New("pause after durable preparation")}
	auth := &utilityChannelAuthorityFake{member: true, bits: utilityPollSendPermission}
	poll := utilityPoll()
	p := newPublisherTest(state, coord, auth)
	if err := p.PublishPoll(context.Background(), poll); err == nil {
		t.Fatal("expected first attempt to pause")
	}
	var saved utilityPublisherState
	var err error
	saved, err = decodePublication(state.values[string(publicationKey(poll.ID))].Value)
	if err != nil || saved.group != utilityID(7) {
		t.Fatalf("original group was not durably recorded: %+v err=%v", saved, err)
	}
	p.destination = utilityDestinationFake{group: utilityID(99)}
	p.authority = utilityDiscoveryFake{instance: state.binding.Instance, generation: 10}
	coord.preflightErr = nil
	if err := p.PublishPoll(context.Background(), poll); err != nil {
		t.Fatal(err)
	}
	if len(coord.inputs) != 2 || coord.inputs[1].AttemptID != coord.inputs[0].AttemptID || coord.inputs[1].GroupID != utilityID(7) || coord.inputs[1].ExpectedGeneration != 9 {
		t.Fatalf("retry changed immutable group or generation: %+v", coord.inputs)
	}
}
func TestPollPublicationBodyMarkerAndThreadAreBound(t *testing.T) {
	// Exercise the body emitted to the coordinator and verify its readable fallback
	// ends with the exact stable poll marker and preserves the thread root.
	state := newUtilityStateFake()
	coord := &utilityCoordinatorFake{}
	auth := &utilityChannelAuthorityFake{member: true, bits: utilityPollSendPermission}
	poll := utilityPoll()
	thread := utilityID(10)
	poll.ThreadID = &thread
	p := newPublisherTest(state, coord, auth)
	if err := p.PublishPoll(context.Background(), poll); err != nil {
		t.Fatal(err)
	}
	if len(coord.inputs) != 1 {
		t.Fatalf("got %d sends", len(coord.inputs))
	}
	var request []any
	if err := cborx.Unmarshal(coord.inputs[0].RequestBody, &request); err != nil || len(request) != 5 {
		t.Fatalf("request body is not a send payload: %v", err)
	}
	text, ok := request[2].(string)
	if !ok {
		t.Fatal("poll fallback text missing")
	}
	marker := fmt.Sprintf("[dilla-poll:v1:%x]", poll.ID[:])
	if !strings.HasSuffix(text, "\n"+marker) || strings.Contains(text, "vote") && strings.Contains(text, "voter") {
		t.Fatalf("poll marker/body is malformed: %q", text)
	}
	threadValue, ok := request[4].(id.ID)
	if raw, isBytes := request[4].([]byte); isBytes && len(raw) == 16 {
		var decoded id.ID
		copy(decoded[:], raw)
		threadValue, ok = decoded, true
	}
	if !ok || threadValue != *poll.ThreadID {
		t.Fatalf("thread root was not bound in the send: %#v", request[4])
	}
}

type reviewUnavailableDestination struct{}

func (reviewUnavailableDestination) ResolveTextGroup(context.Context, id.ID, id.ID) (id.ID, error) {
	return id.ID{}, errors.New("channel no longer has an active group")
}

func TestPollPublisherRecoversAcceptedEvidenceAfterDestinationLoss(t *testing.T) {
	state := newUtilityStateFake()
	coord := &utilityCoordinatorFake{}
	auth := &utilityChannelAuthorityFake{member: true, bits: utilityPollSendPermission}
	poll := utilityPoll()
	if err := newPublisherTest(state, coord, auth).PublishPoll(context.Background(), poll); err != nil {
		t.Fatal(err)
	}
	restarted := newPublisherTest(state, coord, auth)
	restarted.destination = reviewUnavailableDestination{}
	if err := restarted.PublishPoll(context.Background(), poll); err != nil {
		t.Fatalf("accepted publication depended on current destination: %v", err)
	}
}
