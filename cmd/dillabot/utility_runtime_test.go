//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/api"
	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/helper"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/polls"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/bot/reminders"
	"github.com/jonasthim/dilla/internal/bot/service"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
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
func TestUtilityRuntimeRegistersPollReminderAndCapabilityOperations(t *testing.T) {
	u := &utilityRuntime{polls: service.PollInteractions{Store: polls.NewStore(newUtilityStateFake(), utilityID(2)), Authority: utilityActorAuthority{session: &client.Session{}, community: utilityID(2)}, CommunityID: utilityID(2)}}
	got := u.handlers()
	want := []protocol.Operation{protocol.OpPollCreate, protocol.OpPollGet, protocol.OpPollList, protocol.OpPollVote, protocol.OpPollClose, protocol.OpPollVoters, protocol.OpReminderCreate, protocol.OpReminderList, protocol.OpReminderCancel, protocol.OpReminderDismiss, protocol.OpReminderDue}
	for _, op := range want {
		if _, ok := got[op]; !ok {
			t.Fatalf("runtime forgot utility operation %d", op)
		}
	}
	addCapabilityDiscoveryHandler(got, &client.Session{}, utilityID(2), utilityID(3), utilityID(8), 0)
	if _, ok := got[protocol.OpCapabilities]; !ok {
		t.Fatal("runtime did not register op3")
	}
	if len(got) != len(want)+1 {
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

type utilityIntegrationNative struct {
	*utilityStateFake
	groups                  map[id.ID]native.GroupInfo
	interaction             native.InteractionGroupDescriptor
	requests                [][]byte
	prepared                map[string]native.PreparedSend
	interactionListCalls    int
	interactionInspectCalls int
	interactionPrepareCalls int
}

func (n *utilityIntegrationNative) InspectGroup(_ context.Context, group id.ID) (native.GroupInfo, error) {
	if group == n.interaction.Group {
		return native.GroupInfo{}, &helper.StatusError{Status: 2}
	}
	g, ok := n.groups[group]
	if !ok {
		return native.GroupInfo{}, errors.New("unknown test group")
	}
	return g, nil
}
func (n *utilityIntegrationNative) InteractionGroups(context.Context) ([]native.InteractionGroupDescriptor, error) {
	n.interactionListCalls++
	return []native.InteractionGroupDescriptor{n.interaction}, nil
}
func (n *utilityIntegrationNative) InspectInteractionGroup(_ context.Context, expected native.InteractionGroupExpected, snapshot []byte) (native.InteractionGroupInspectResult, error) {
	n.interactionInspectCalls++
	b := n.interaction.Binding
	if len(snapshot) == 0 || expected.Group != n.interaction.Group || expected.Instance != b.Instance || expected.Installation != b.Installation || expected.Community != b.Community || expected.BotUser != b.BotUser || expected.BotDevice != n.interaction.BotDevice || expected.ParticipantUser != b.ParticipantUser || expected.ParticipantDevice != b.ParticipantDevice || expected.PolicyVersion != b.PolicyVersion {
		return native.InteractionGroupInspectResult{}, errors.New("interaction inspection binding mismatch")
	}
	return native.InteractionGroupInspectResult{Binding: b, BotDevice: n.interaction.BotDevice, Epoch: n.interaction.Epoch, State: n.interaction.State, NextSequence: 1}, nil
}
func (n *utilityIntegrationNative) PrepareSend(_ context.Context, operation, attempt, group id.ID, request []byte, _, _ uint64) (native.PreparedSend, error) {
	n.requests = append(n.requests, append([]byte(nil), request...))
	key := fmt.Sprintf("%x/%x", operation[:], attempt[:])
	if prior, ok := n.prepared[key]; ok {
		return prior, nil
	}
	g := n.groups[group]
	prepared := native.PreparedSend{MessageID: utilityID(byte(len(n.requests) + 20)), Epoch: g.Epoch, Class: 0, Ciphertext: []byte(fmt.Sprintf("cipher-%d", len(n.requests)))}
	n.prepared[key] = prepared
	return prepared, nil
}
func (n *utilityIntegrationNative) PrepareInteractionSend(_ context.Context, expected native.InteractionGroupExpected, snapshot []byte, operation, attempt id.ID, request []byte, originalNow, generation uint64) (native.PreparedSend, error) {
	n.interactionPrepareCalls++
	b := n.interaction.Binding
	if len(snapshot) == 0 || expected.Group != n.interaction.Group || expected.ParticipantUser != b.ParticipantUser || expected.ParticipantDevice != b.ParticipantDevice || operation.IsZero() || attempt.IsZero() || len(request) == 0 || originalNow == 0 || generation == 0 {
		return native.PreparedSend{}, errors.New("private send binding mismatch")
	}
	return n.prepare(operation, attempt, expected.Group)
}

func (n *utilityIntegrationNative) prepare(operation, attempt, group id.ID) (native.PreparedSend, error) {
	key := fmt.Sprintf("%x/%x", operation[:], attempt[:])
	if prior, ok := n.prepared[key]; ok {
		return prior, nil
	}
	g, ok := n.groups[group]
	if !ok && group == n.interaction.Group {
		g = native.GroupInfo{ID: group, Epoch: n.interaction.Epoch}
		ok = true
	}
	if !ok {
		return native.PreparedSend{}, errors.New("unknown prepared group")
	}
	prepared := native.PreparedSend{MessageID: utilityID(byte(len(n.requests) + 20)), Epoch: g.Epoch, Class: 0, Ciphertext: []byte(fmt.Sprintf("cipher-%d", len(n.requests)))}
	n.prepared[key] = prepared
	return prepared, nil
}

type utilityIntegrationSession struct {
	mu                    sync.Mutex
	community             id.ID
	member, active, owner bool
	bits, channelBits     uint64
	receipts              map[id.ID]protocol.SendReceipt
	uploads               []client.BotMessageUpload
	uploadGroups          []id.ID
	admissionCalls        int
	snapshotCalls         int
	closedUploads         int
}

func (s *utilityIntegrationSession) BotActorAuthority(_ context.Context, community, actor id.ID) (client.ActorAuthority, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active || community != s.community || !s.member {
		return client.ActorAuthority{}, false, nil
	}
	return client.ActorAuthority{EffectiveBits: s.bits, Owner: s.owner}, true, nil
}
func (s *utilityIntegrationSession) BotActorChannelAuthority(_ context.Context, community, actor, channel id.ID) (uint64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.channelBits, s.active && s.member && community == s.community, nil
}
func (s *utilityIntegrationSession) BotMessageReceipt(_ context.Context, community, operation, attempt, group id.ID) (protocol.SendReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return protocol.SendReceipt{}, errors.New("installation disabled")
	}
	if r, ok := s.receipts[attempt]; ok {
		if r.OperationID == operation && r.GroupID == group && r.CommunityID == community {
			return r, nil
		}
		return protocol.SendReceipt{}, client.ErrProtocol
	}
	return protocol.SendReceipt{}, client.ErrBotSendReceiptNotFound
}
func (s *utilityIntegrationSession) PostBotMessage(_ context.Context, community, group id.ID, upload client.BotMessageUpload) (protocol.SendReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return protocol.SendReceipt{}, errors.New("installation disabled")
	}
	hash := sha256.Sum256(upload.PrivateMessage)
	reply, _ := cborx.Marshal([]any{uint64(len(s.uploads) + 1), make([]byte, 32), uint64(2000)})
	receipt := protocol.SendReceipt{OperationID: upload.OperationID, AttemptID: upload.AttemptID, PredecessorAttemptID: upload.PredecessorAttemptID, CommunityID: community, GroupID: group, ExpectedGeneration: upload.ExpectedGeneration, CiphertextHash: hash[:], Class: upload.Class, Outcome: protocol.ReceiptAccepted, Epoch: upload.Epoch, SendResponse: &protocol.OriginalSendResponse{Sequence: uint64(len(s.uploads) + 1), FrankingTag: make([]byte, 32), ReceiveTimestamp: 2000, Body: reply}}
	closed := s.closedUploads > 0
	if closed {
		s.closedUploads--
		receipt.Outcome = protocol.ReceiptClosedNotAccepted
		receipt.SendResponse = nil
	}
	s.receipts[upload.AttemptID] = receipt
	s.uploads = append(s.uploads, upload)
	s.uploadGroups = append(s.uploadGroups, group)
	if closed {
		return protocol.SendReceipt{}, errors.New("upload failed; durable receipt closes this attempt")
	}
	return receipt, nil
}
func (s *utilityIntegrationSession) IssueBotInteractionAdmission(_ context.Context, installation, request, group, participant, device id.ID) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active || installation != utilityID(3) || request.IsZero() || group != utilityID(10) || participant != utilityID(6) || device != utilityID(9) {
		return nil, errors.New("invalid private admission request")
	}
	s.admissionCalls++
	return []byte{1}, nil
}
func (s *utilityIntegrationSession) BotInteractionContextSnapshot(_ context.Context, installation, request, group, participant, device id.ID) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active || installation != utilityID(3) || request.IsZero() || group != utilityID(10) || participant != utilityID(6) || device != utilityID(9) {
		return nil, errors.New("invalid private snapshot request")
	}
	s.snapshotCalls++
	payload, err := cborx.Marshal([]any{uint64(1), request, uint64(2000), uint64(0), uint64(0), uint64(2000), uint64(1), uint64(1), uint64(1), uint64(0), uint64(0), uint64(0), uint64(0), []any{utilityID(1), utilityID(3), utilityID(2), utilityID(8), utilityID(14), utilityID(6), utilityID(9), utilityID(10)}, []any{}, uint64(0)})
	if err != nil {
		return nil, err
	}
	return cborx.Marshal([]any{payload, make([]byte, 64)})
}
func (s *utilityIntegrationSession) ResolveBotMessage(ctx context.Context, community, operation id.ID, resolution client.BotMessageResolution) (protocol.SendReceipt, error) {
	return protocol.SendReceipt{}, errors.New("unexpected resolve")
}

type utilityIntegrationSnapshot struct {
	expected native.InteractionGroupExpected
	calls    int
}

func (s *utilityIntegrationSnapshot) InteractionSnapshot(_ context.Context, event native.GroupEvent) (native.InteractionGroupExpected, []byte, error) {
	s.calls++
	if event.Group != s.expected.Group || event.Channel != s.expected.ParticipantUser || event.SenderUser != s.expected.ParticipantUser || event.SenderDevice != s.expected.ParticipantDevice {
		return native.InteractionGroupExpected{}, nil, errors.New("interaction event does not match participant binding")
	}
	return s.expected, []byte{1}, nil
}

func integrationEvent(t *testing.T, request protocol.Request, delivery uint64, community, group, channel, actor, device id.ID) native.GroupEvent {
	t.Helper()
	body, err := protocol.EncodeRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	return native.GroupEvent{DeliveryID: delivery, Group: group, Community: community, Channel: channel, SourceSequence: delivery, Epoch: 1, ReceivedAt: 1, SenderUser: actor, SenderDevice: device, SenderKind: 0, EnvelopeType: 0, Body: &body}
}
func integrationReply(t *testing.T, dispatcher *service.InteractionDispatcher, outbox *service.InteractionResponseOutbox, authority utilityDiscoveryFake, event native.GroupEvent, now time.Time) protocol.Response {
	t.Helper()
	body, err := dispatcher.Dispatch(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	discovery, _, err := authority.DiscoverObserved(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = outbox.Send(context.Background(), event, []byte(body), id.New(), id.New(), uint64(now.Unix()), discovery.Generation); err != nil {
		t.Fatal(err)
	}
	response, err := protocol.DecodeResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := outbox.Lookup(context.Background(), event); err != nil || !found {
		t.Fatalf("private reply was not persisted: found=%v err=%v", found, err)
	}
	return response
}

func TestRuntimeAssemblyDispatcherOutboxAuthorityAndRecovery(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1000, 0).UTC()
	community, installation, bot, actor, device := utilityID(2), utilityID(3), utilityID(8), utilityID(6), utilityID(9)
	dmGroup, pollGroup, pollChannel := utilityID(10), utilityID(12), utilityID(13)
	dmChannel := actor
	state := newUtilityStateFake()
	session := &utilityIntegrationSession{community: community, member: true, active: true, channelBits: utilityPollSendPermission, receipts: map[id.ID]protocol.SendReceipt{}}
	com := community
	privateBinding := native.InteractionGroupBinding{Instance: state.binding.Instance, Installation: installation, Community: community, BotUser: bot, ParticipantUser: actor, ParticipantDevice: device, PolicyVersion: 1, E2EEVersion: 1}
	privateDescriptor := native.InteractionGroupDescriptor{Group: dmGroup, BotDevice: utilityID(14), Binding: privateBinding, Epoch: 1, State: 2}
	nativeBoundary := &utilityIntegrationNative{utilityStateFake: state, groups: map[id.ID]native.GroupInfo{pollGroup: {ID: pollGroup, Kind: 0, Community: &com, Target: pollChannel, State: 2, Epoch: 1, NextSequence: 1}}, interaction: privateDescriptor, prepared: map[string]native.PreparedSend{}}
	discovery := utilityDiscoveryFake{instance: state.binding.Instance, generation: 9}
	coordinator, err := service.NewSendCoordinatorWithAdapters(nativeBoundary, discovery)
	if err != nil {
		t.Fatal(err)
	}
	actorAuthority := utilityActorAuthority{session: session, community: community}
	publisher := &durablePollPublisher{state: nativeBoundary, coordinator: coordinator, destination: utilityDestinationFake{group: pollGroup}, authority: discovery, session: session, receipts: session, actorAuthority: session, community: community, installation: installation, now: func() time.Time { return now }}
	pollStore := polls.NewStore(nativeBoundary, community)
	reminderStore := reminders.NewStore(nativeBoundary, state.binding, func() time.Time { return now })
	utilities := &utilityRuntime{polls: service.PollInteractions{Store: pollStore, Authority: actorAuthority, CommunityID: community, Now: func() time.Time { return now }, Publisher: publisher}, reminders: service.ReminderInteractions{Store: reminderStore, Authority: actorAuthority, CommunityID: community, Now: func() time.Time { return now }}}
	base := service.OverviewInteractions{Authority: session, CommunityID: community}.Handlers()
	base[protocol.OpMusicProvidersStatus] = service.InteractionHandlerSpec{Handle: func(context.Context, service.InteractionActor, protocol.Request) (service.InteractionResult, error) {
		return service.InteractionResult{}, nil
	}}
	expected := native.InteractionGroupExpected{Instance: state.binding.Instance, Installation: installation, Community: community, BotUser: bot, BotDevice: utilityID(14), ParticipantUser: actor, ParticipantDevice: device, Group: dmGroup, PolicyVersion: 1}
	snapshotProvider := &utilityIntegrationSnapshot{expected: expected}
	// Music plus Polls and Reminders (Task 10c bits 8 and 9).
	dispatcher, err := newRuntimeUtilityDispatcher(nativeBoundary, base, utilities, session, community, installation, bot, 1|1<<8|1<<9, func() uint64 { return uint64(now.Unix()) }, snapshotProvider)
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := service.NewInteractionResponseOutboxWithAdapters(nativeBoundary, coordinator, session)
	if err != nil {
		t.Fatal(err)
	}
	capEvent := integrationEvent(t, protocol.Request{RequestID: utilityID(20), CommunityID: community, Operation: protocol.OpCapabilities, Args: []any{}}, 1, community, dmGroup, dmChannel, actor, device)
	capResponse := integrationReply(t, dispatcher, outbox, discovery, capEvent, now)
	var capabilityData []any
	if err := cborx.Unmarshal(capResponse.Data, &capabilityData); err != nil {
		t.Fatal(err)
	}
	operations, ok := capabilityData[4].([]any)
	if !ok || !hasOperation(operations, protocol.OpCapabilities) || !hasOperation(operations, protocol.OpPollCreate) || !hasOperation(operations, protocol.OpReminderCreate) || hasOperation(operations, protocol.OpMusicProvidersStatus) {
		t.Fatalf("ordinary member op3 operations=%v", capabilityData)
	}
	session.mu.Lock()
	session.owner = true
	session.mu.Unlock()
	managerCap := integrationReply(t, dispatcher, outbox, discovery, integrationEvent(t, protocol.Request{RequestID: utilityID(21), CommunityID: community, Operation: protocol.OpCapabilities, Args: []any{}}, 2, community, dmGroup, dmChannel, actor, device), now)
	if err := cborx.Unmarshal(managerCap.Data, &capabilityData); err != nil {
		t.Fatal(err)
	}
	operations = capabilityData[4].([]any)
	if !hasOperation(operations, protocol.OpMusicProvidersStatus) {
		t.Fatalf("registered op24 omitted for manager: %v", operations)
	}
	session.mu.Lock()
	session.owner = false
	session.mu.Unlock()
	createPayload, _ := cborx.Marshal([]any{uint64(1), pollChannel, nil, "Lunch?", []any{"Soup", "Salad"}, uint64(0), uint64(3600)})
	createReq := protocol.Request{RequestID: utilityID(22), CommunityID: community, Operation: protocol.OpPollCreate, Revision: u64ptr(0), Expires: u64ptr(uint64(now.Unix() + 30)), Args: []any{createPayload}}
	createEvent := integrationEvent(t, createReq, 3, community, dmGroup, dmChannel, actor, device)
	createResponse := integrationReply(t, dispatcher, outbox, discovery, createEvent, now)
	if createResponse.Status != 0 {
		t.Fatalf("poll create status=%d", createResponse.Status)
	}
	if len(session.uploads) != 4 {
		t.Fatalf("expected poll publication plus three private replies, uploads=%d", len(session.uploads))
	}
	if session.uploadGroups[0] != dmGroup || session.uploadGroups[1] != dmGroup || session.uploadGroups[2] != pollGroup || session.uploadGroups[3] != dmGroup {
		t.Fatalf("private/public uploads used wrong groups: %v", session.uploadGroups)
	}
	if nativeBoundary.interactionInspectCalls < 3 || snapshotProvider.calls < 3 || nativeBoundary.interactionListCalls < 3 || session.admissionCalls != 3 || session.snapshotCalls != 3 || nativeBoundary.interactionPrepareCalls != 3 {
		t.Fatalf("private branches were not all exercised: inspect=%d dispatch snapshots=%d descriptors=%d admissions=%d send snapshots=%d private prepares=%d", nativeBoundary.interactionInspectCalls, snapshotProvider.calls, nativeBoundary.interactionListCalls, session.admissionCalls, session.snapshotCalls, nativeBoundary.interactionPrepareCalls)
	}
	var publicSend []any
	if err := cborx.Unmarshal(nativeBoundary.requests[0], &publicSend); err != nil {
		t.Fatal(err)
	}
	if publicSend[2].(string) == "" || publicSend[4] != nil {
		t.Fatalf("poll publication payload=%v", publicSend)
	}
	privateRecord, found, err := outbox.Lookup(ctx, createEvent)
	if err != nil || !found {
		t.Fatalf("private response outbox missing: found=%v err=%v", found, err)
	}
	if _, err := protocol.DecodeResponse(string(privateRecord.Body)); err != nil {
		t.Fatalf("private reply was not protocol response: %v", err)
	}
	reminderPayload, _ := cborx.Marshal([]any{uint64(1), uint64(0), uint64(60), "stretch", "UTC", nil, nil})
	reminderReq := protocol.Request{RequestID: utilityID(23), CommunityID: community, Operation: protocol.OpReminderCreate, Revision: u64ptr(0), Expires: u64ptr(uint64(now.Unix() + 30)), Args: []any{reminderPayload}}
	reminderEvent := integrationEvent(t, reminderReq, 4, community, dmGroup, dmChannel, actor, device)
	if result := integrationReply(t, dispatcher, outbox, discovery, reminderEvent, now); result.Status != 0 {
		t.Fatalf("reminder create status=%d", result.Status)
	}
	now = now.Add(120 * time.Second)
	dueReq := protocol.Request{RequestID: utilityID(24), CommunityID: community, Operation: protocol.OpReminderDue, Args: []any{nil}}
	if err := utilities.maintain(ctx); err != nil {
		t.Fatal(err)
	}
	dueResponse := integrationReply(t, dispatcher, outbox, discovery, integrationEvent(t, dueReq, 5, community, dmGroup, dmChannel, actor, device), now)
	var dueData []any
	if err := cborx.Unmarshal(dueResponse.Data, &dueData); err != nil {
		t.Fatal(err)
	}
	records, ok := dueData[1].([]any)
	if !ok || len(records) != 1 {
		t.Fatalf("reminder catch-up page=%v", dueData)
	}
	// Simulate a crash after the public poll was accepted and before Published was
	// marked. Reconciliation after creator revocation and destination loss must
	// consume the recorded receipt without another POST.
	secondPollID := utilityID(25)
	state.failMarkPublished = true
	// Use the shared registered handler so poll state, public send, and private
	// response all traverse the production assembly, dispatcher, and outbox.
	secondPayload, _ := cborx.Marshal([]any{uint64(1), pollChannel, nil, "Tea?", []any{"Yes", "No"}, uint64(0), uint64(3600)})
	secondReq := protocol.Request{RequestID: secondPollID, CommunityID: community, Operation: protocol.OpPollCreate, Revision: u64ptr(0), Expires: u64ptr(uint64(now.Unix() + 30)), Args: []any{secondPayload}}
	secondEvent := integrationEvent(t, secondReq, 6, community, dmGroup, dmChannel, actor, device)
	secondResult := integrationReply(t, dispatcher, outbox, discovery, secondEvent, now)
	if secondResult.Status == 0 {
		t.Fatal("injected mark-published failure was hidden")
	}
	publicPostsBefore := len(session.uploads)
	publisher.destination = reviewUnavailableDestination{}
	session.mu.Lock()
	session.member = false
	session.channelBits = 0
	session.mu.Unlock()
	if err := utilities.maintain(ctx); err != nil {
		t.Fatalf("accepted recovery after creator revoke: %v", err)
	}
	recovered, err := pollStore.Get(ctx, secondPollID)
	if err != nil || !recovered.Published {
		t.Fatalf("accepted poll not marked published after recovery: %+v err=%v", recovered, err)
	}
	if len(session.uploads) != publicPostsBefore {
		t.Fatalf("accepted recovery duplicated sends: before=%d after=%d", publicPostsBefore, len(session.uploads))
	}
	revokedCap := integrationReply(t, dispatcher, outbox, discovery, integrationEvent(t, protocol.Request{RequestID: utilityID(26), CommunityID: community, Operation: protocol.OpCapabilities, Args: []any{}}, 7, community, dmGroup, dmChannel, actor, device), now)
	if revokedCap.Status != 2 {
		t.Fatalf("revoked member op3 status=%d", revokedCap.Status)
	}
	session.mu.Lock()
	session.active = false
	session.mu.Unlock()
	disabledEvent := integrationEvent(t, protocol.Request{RequestID: utilityID(27), CommunityID: community, Operation: protocol.OpCapabilities, Args: []any{}}, 8, community, dmGroup, dmChannel, actor, device)
	disabledBody, err := dispatcher.Dispatch(ctx, disabledEvent)
	if err != nil {
		t.Fatal(err)
	}
	disabledCap, err := protocol.DecodeResponse(disabledBody)
	if err != nil || disabledCap.Status != 2 {
		t.Fatalf("disabled installation op3 status=%d err=%v", disabledCap.Status, err)
	}
	if _, err := outbox.Send(ctx, disabledEvent, []byte(disabledBody), id.New(), id.New(), uint64(now.Unix()), discovery.generation); err == nil || !strings.Contains(err.Error(), "installation disabled") {
		t.Fatalf("disabled installation private send error=%v", err)
	}
	privatePosts, publicPosts := 0, 0
	for _, group := range session.uploadGroups {
		switch group {
		case dmGroup:
			privatePosts++
		case pollGroup:
			publicPosts++
		default:
			t.Fatalf("response uploaded outside its private interaction group and poll channel: %s", group)
		}
	}
	if privatePosts != 7 || publicPosts != 2 || len(session.uploads) != 9 {
		t.Fatalf("unexpected final upload routing: private=%d public=%d total=%d groups=%v", privatePosts, publicPosts, len(session.uploads), session.uploadGroups)
	}
}

func u64ptr(v uint64) *uint64 { return &v }
func hasOperation(operations []any, want protocol.Operation) bool {
	for _, v := range operations {
		if x, ok := v.(uint64); ok && x == uint64(want) {
			return true
		}
	}
	return false
}

func TestRuntimeUtilityDispatcherRejectsOverflowCapabilities(t *testing.T) {
	if _, err := newRuntimeUtilityDispatcher(nil, nil, nil, nil, utilityID(1), utilityID(2), utilityID(3), 1024, nil, nil); err == nil {
		t.Fatal("oversized capabilities were truncated")
	}
}

func TestPrivateCapabilityRecoveryResumesClosedResponseThroughProductionAssembly(t *testing.T) {
	ctx := context.Background()
	state := newUtilityStateFake()
	community, installation, bot, actor, device, group := utilityID(2), utilityID(3), utilityID(8), utilityID(6), utilityID(9), utilityID(10)
	binding := native.InteractionGroupBinding{Instance: state.binding.Instance, Installation: installation, Community: community, BotUser: bot, ParticipantUser: actor, ParticipantDevice: device, PolicyVersion: 1, E2EEVersion: 1}
	n := &utilityIntegrationNative{utilityStateFake: state, groups: map[id.ID]native.GroupInfo{}, interaction: native.InteractionGroupDescriptor{Group: group, BotDevice: utilityID(14), Binding: binding, Epoch: 1, State: 2}, prepared: map[string]native.PreparedSend{}}
	session := &utilityIntegrationSession{community: community, member: true, active: true, closedUploads: 2, receipts: map[id.ID]protocol.SendReceipt{}}
	discovery := utilityDiscoveryFake{instance: state.binding.Instance, generation: 9}
	coordinator, err := service.NewSendCoordinatorWithAdapters(n, discovery)
	if err != nil {
		t.Fatal(err)
	}
	expected := native.InteractionGroupExpected{Instance: state.binding.Instance, Installation: installation, Community: community, BotUser: bot, BotDevice: utilityID(14), ParticipantUser: actor, ParticipantDevice: device, Group: group, PolicyVersion: 1}
	snapshots := &utilityIntegrationSnapshot{expected: expected}
	dispatcher, err := newRuntimeUtilityDispatcher(n, service.OverviewInteractions{Authority: session, CommunityID: community}.Handlers(), nil, session, community, installation, bot, 0, func() uint64 { return 1000 }, snapshots)
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := service.NewInteractionResponseOutboxWithAdapters(n, coordinator, session)
	if err != nil {
		t.Fatal(err)
	}
	event := integrationEvent(t, protocol.Request{RequestID: utilityID(20), CommunityID: community, Operation: protocol.OpCapabilities, Args: []any{}}, 1, community, group, actor, actor, device)
	body, err := dispatcher.Dispatch(ctx, event)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := outbox.Send(ctx, event, []byte(body), id.New(), id.New(), 1000, 9)
	if err != nil || initial.Outcome != protocol.ReceiptClosedNotAccepted || len(session.uploads) != 2 {
		t.Fatalf("expected bounded closed attempts: outcome=%v err=%v uploads=%d", initial.Outcome, err, len(session.uploads))
	}
	saved, found, err := outbox.Lookup(ctx, event)
	if err != nil || !found {
		t.Fatal("missing original response")
	}
	// Both original attempts were closed from the outset; neither was accepted.
	restarted, err := service.NewInteractionResponseOutboxWithAdapters(n, coordinator, session)
	if err != nil {
		t.Fatal(err)
	}
	receipt, found, err := restarted.ResumeWithPreflight(ctx, event, func(c context.Context) error { return dispatcher.ReauthorizeReadReplay(c, event) })
	if err != nil || !found || receipt.Outcome != protocol.ReceiptAccepted {
		t.Fatalf("private command remains blocked: found=%v outcome=%v err=%v", found, receipt.Outcome, err)
	}
	after, found, err := restarted.Lookup(ctx, event)
	if err != nil || !found || string(after.Body) != body || after.OperationID != saved.OperationID || after.AttemptID == saved.AttemptID || after.PredecessorAttemptID == nil || *after.PredecessorAttemptID != saved.AttemptID {
		t.Fatal("recovery changed response or failed to link successor")
	}
	if len(session.uploads) != 3 || session.admissionCalls != 3 || n.interactionPrepareCalls != 3 {
		t.Fatal("private successor bypassed proof-bound encryption or duplicated upload")
	}
}
