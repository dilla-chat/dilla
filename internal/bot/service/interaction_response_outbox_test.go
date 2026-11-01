package service

import (
	"context"
	"errors"
	"testing"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

type responseOutboxState struct {
	binding native.Binding
	value   native.StateValue
	failCAS bool
}

func (s *responseOutboxState) Binding() native.Binding { return s.binding }
func (s *responseOutboxState) StateGet(_ context.Context, area uint64, key []byte) (native.StateValue, error) {
	if area != interactionResponseOutboxArea || string(key) != string(interactionResponseOutboxKey(s.binding.Installation, 9)) {
		return native.StateValue{}, errors.New("unexpected state key")
	}
	return native.StateValue{Revision: s.value.Revision, Value: append([]byte(nil), s.value.Value...), Tombstone: s.value.Tombstone}, nil
}
func (s *responseOutboxState) StateTransaction(_ context.Context, _ id.ID, changes []native.StateChange) ([]uint64, error) {
	if s.failCAS {
		return nil, errors.New("injected CAS failure")
	}
	ch := changes[0]
	if ch.ExpectedRevision != s.value.Revision {
		return nil, errors.New("CAS revision conflict")
	}
	s.value = native.StateValue{Revision: s.value.Revision + 1, Value: append([]byte(nil), ch.Value...)}
	return []uint64{s.value.Revision}, nil
}

type responseOutboxSender struct {
	inputs []SendInput
	err    error
	before func(SendInput) error
}

func (s *responseOutboxSender) send(_ context.Context, in SendInput) (protocol.SendReceipt, error) {
	if s.before != nil {
		if err := s.before(in); err != nil {
			return protocol.SendReceipt{}, err
		}
	}
	s.inputs = append(s.inputs, SendInput{OperationID: in.OperationID, AttemptID: in.AttemptID, GroupID: in.GroupID,
		RequestBody: append([]byte(nil), in.RequestBody...), OriginalNow: in.OriginalNow, ExpectedGeneration: in.ExpectedGeneration})
	if s.err != nil {
		return protocol.SendReceipt{}, s.err
	}
	return protocol.SendReceipt{OperationID: in.OperationID, AttemptID: in.AttemptID}, nil
}

func responseOutboxFixture(t *testing.T) (*responseOutboxState, *responseOutboxSender, native.GroupEvent) {
	t.Helper()
	b := native.Binding{Instance: interactionID(21), Community: interactionID(22), Installation: interactionID(23)}
	body, err := protocol.EncodeRequest(protocol.Request{RequestID: interactionID(24), CommunityID: b.Community, Operation: protocol.OpOverviewGet, Args: []any{}})
	if err != nil {
		t.Fatal(err)
	}
	event := native.GroupEvent{DeliveryID: 9, Group: interactionID(25), Community: b.Community, Channel: interactionID(26), SourceSequence: 4, Epoch: 7,
		SenderUser: interactionID(27), SenderDevice: interactionID(28), SenderKind: 0, SenderTier: 0, Message: interactionID(29), EnvelopeType: 0, Body: &body}
	state, sender := &responseOutboxState{binding: b}, &responseOutboxSender{}
	return state, sender, event
}

func newResponseOutbox(t *testing.T, state *responseOutboxState, sender *responseOutboxSender) *InteractionResponseOutbox {
	t.Helper()
	o, err := newInteractionResponseOutbox(state, sender.send)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestInteractionResponseOutboxRestartReusesExactPersistedResponseAndSendIdentity(t *testing.T) {
	ctx := context.Background()
	state, firstSender, event := responseOutboxFixture(t)
	firstSender.before = func(in SendInput) error {
		if state.value.Revision != 1 || len(state.value.Value) == 0 {
			return errors.New("send started before response was durably persisted")
		}
		saved, err := decodeInteractionResponseRecord(state.value.Value)
		request, decodeErr := decodeInteractionSendRequest(in.RequestBody)
		if err != nil || decodeErr != nil || string(saved.ResponseBody) != "response-one" || request != "response-one" || saved.OperationID != in.OperationID || saved.AttemptID != in.AttemptID {
			return errors.New("persisted response does not match send input")
		}
		return nil
	}
	first := newResponseOutbox(t, state, firstSender)
	if _, err := first.Send(ctx, event, []byte("response-one"), interactionID(30), interactionID(31), 123, 8); err != nil {
		t.Fatal(err)
	}
	secondSender := &responseOutboxSender{}
	second := newResponseOutbox(t, state, secondSender) // simulate process restart
	saved, found, err := second.Lookup(ctx, event)
	if err != nil || !found || string(saved.Body) != "response-one" || saved.OperationID != interactionID(30) || saved.AttemptID != interactionID(31) {
		t.Fatalf("lookup found=%v saved=%#v err=%v", found, saved, err)
	}
	if _, found, err := second.Resume(ctx, event); err != nil || !found {
		t.Fatal(err)
	}
	if len(secondSender.inputs) != 1 {
		t.Fatalf("restart sends=%d, want 1", len(secondSender.inputs))
	}
	got := secondSender.inputs[0]
	request, decodeErr := decodeInteractionSendRequest(got.RequestBody)
	if decodeErr != nil || request != "response-one" || got.OperationID != interactionID(30) || got.AttemptID != interactionID(31) || got.OriginalNow != 123 || got.ExpectedGeneration != 8 {
		t.Fatalf("restart changed persisted send: %#v", got)
	}
	if string(got.RequestBody) != string(encodeInteractionSendRequestForTest(t, "response-one")) {
		t.Fatal("restart changed deterministic send request encoding")
	}
}

func decodeInteractionSendRequest(body []byte) (string, error) {
	var request []any
	if err := cborx.Unmarshal(body, &request); err != nil {
		return "", err
	}
	if len(request) != 4 || request[0] != uint64(0) || request[1] != nil || request[3] == nil {
		return "", errors.New("unexpected send request envelope")
	}
	text, ok := request[2].(string)
	if !ok || len(request[3].([]any)) != 0 {
		return "", errors.New("unexpected send request fields")
	}
	return text, nil
}

func encodeInteractionSendRequestForTest(t *testing.T, text string) []byte {
	t.Helper()
	body, err := cborx.Marshal([]any{uint64(0), nil, text, []any{}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestInteractionResponseOutboxKeepsRevokedPendingResponseUnsent(t *testing.T) {
	ctx := context.Background()
	state, firstSender, event := responseOutboxFixture(t)
	first := newResponseOutbox(t, state, firstSender)
	if _, err := first.Send(ctx, event, []byte("private response"), interactionID(30), interactionID(31), 123, 8); err != nil {
		t.Fatal(err)
	}
	savedValue := append([]byte(nil), state.value.Value...)
	retrySender := &responseOutboxSender{}
	retry := newResponseOutbox(t, state, retrySender)
	revoked := errors.New("read permission revoked")
	preflightCalls := 0
	_, found, err := retry.ResumeWithPreflight(ctx, event, func(context.Context) error {
		preflightCalls++
		return revoked
	})
	if !found || !errors.Is(err, revoked) || preflightCalls != 1 || len(retrySender.inputs) != 0 {
		t.Fatalf("found=%v err=%v checks=%d sends=%d", found, err, preflightCalls, len(retrySender.inputs))
	}
	if string(state.value.Value) != string(savedValue) {
		t.Fatal("denied replay changed the immutable outbox record")
	}
}

func TestInteractionResponseOutboxRejectsConflictingEventRequestIdentity(t *testing.T) {
	ctx := context.Background()
	state, sender, event := responseOutboxFixture(t)
	o := newResponseOutbox(t, state, sender)
	if _, err := o.Send(ctx, event, []byte("response"), interactionID(30), interactionID(31), 123, 8); err != nil {
		t.Fatal(err)
	}
	changed := event
	otherBody, err := protocol.EncodeRequest(protocol.Request{RequestID: interactionID(35), CommunityID: state.binding.Community, Operation: protocol.OpOverviewGet, Args: []any{}})
	if err != nil {
		t.Fatal(err)
	}
	changed.Body = &otherBody
	if _, err := o.Send(ctx, changed, []byte("response-two"), interactionID(36), interactionID(37), 123, 8); !errors.Is(err, ErrInteractionResponseConflict) {
		t.Fatalf("conflicting request error=%v", err)
	}
	if len(sender.inputs) != 1 {
		t.Fatalf("conflict triggered send, count=%d", len(sender.inputs))
	}
}

func TestInteractionResponseOutboxCASFailureNeverSends(t *testing.T) {
	ctx := context.Background()
	state, sender, event := responseOutboxFixture(t)
	state.failCAS = true
	o := newResponseOutbox(t, state, sender)
	if _, err := o.Send(ctx, event, []byte("response"), interactionID(30), interactionID(31), 123, 8); err == nil {
		t.Fatal("expected CAS failure")
	}
	if len(sender.inputs) != 0 {
		t.Fatalf("CAS failure triggered %d sends", len(sender.inputs))
	}
}

func TestInteractionResponseOutboxRejectsOversizedResponseBeforePersisting(t *testing.T) {
	ctx := context.Background()
	state, sender, event := responseOutboxFixture(t)
	o := newResponseOutbox(t, state, sender)
	response := make([]byte, maxInteractionResponseBody+1)
	for i := range response {
		response[i] = 'x'
	}
	if _, err := o.Send(ctx, event, response, interactionID(30), interactionID(31), 123, 8); err == nil {
		t.Fatal("oversized response was accepted")
	}
	if state.value.Revision != 0 || len(sender.inputs) != 0 {
		t.Fatalf("oversized response persisted revision=%d and sent=%d times", state.value.Revision, len(sender.inputs))
	}
}

func TestInteractionResponseOutboxLostSendResponseRetriesSameAttempt(t *testing.T) {
	ctx := context.Background()
	state, sender, event := responseOutboxFixture(t)
	sender.err = errors.New("response lost after remote acceptance")
	o := newResponseOutbox(t, state, sender)
	if _, err := o.Send(ctx, event, []byte("stable-response"), interactionID(30), interactionID(31), 123, 8); err == nil {
		t.Fatal("expected lost response error")
	}
	sender.err = nil
	if _, err := newResponseOutbox(t, state, sender).Send(ctx, event, []byte("stable-response"), interactionID(32), interactionID(33), 999, 11); err != nil {
		t.Fatal(err)
	}
	if len(sender.inputs) != 2 {
		t.Fatalf("send calls=%d, want 2", len(sender.inputs))
	}
	first, second := sender.inputs[0], sender.inputs[1]
	if first.OperationID != second.OperationID || first.AttemptID != second.AttemptID || first.OriginalNow != second.OriginalNow || first.ExpectedGeneration != second.ExpectedGeneration || string(first.RequestBody) != string(second.RequestBody) {
		t.Fatalf("retry changed attempt: first=%#v second=%#v", first, second)
	}
}

func TestInteractionResponseOutboxRejectsSendCoordinatorForDifferentInstallation(t *testing.T) {
	state, _, _ := responseOutboxFixture(t)
	otherBinding := state.binding
	otherBinding.Installation = interactionID(44)
	if otherBinding.Community != state.binding.Community {
		t.Fatal("fixture must share community")
	}
	n := &sendNativeFake{binding: otherBinding}
	coordinator, err := newSendCoordinator(n, sendAuthorityFake{discovery: client.Discovery{InstanceID: otherBinding.Instance, Generation: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newInteractionResponseOutboxWithCoordinator(state, coordinator, &client.Session{}); err == nil {
		t.Fatal("outbox accepted a coordinator bound to a different installation in the same community")
	}
}

type closedResponseSession struct {
	*sendSessionFake
	state       *responseOutboxState
	closeUpload bool
	t           *testing.T
}

func (s *closedResponseSession) PostBotMessage(_ context.Context, community, group id.ID, upload client.BotMessageUpload) (protocol.SendReceipt, error) {
	s.t.Helper()
	saved, err := decodeInteractionResponseRecord(s.state.value.Value)
	if err != nil || saved.AttemptID != upload.AttemptID || saved.OperationID != upload.OperationID {
		s.t.Fatal("successor upload preceded durable identity")
	}
	s.uploads = append(s.uploads, upload)
	receipt := sendReceipt(upload.OperationID, upload.AttemptID, group, upload.ExpectedGeneration, upload.Epoch, protocol.ReceiptAccepted)
	receipt.CommunityID = community
	receipt.PredecessorAttemptID = upload.PredecessorAttemptID
	if s.closeUpload {
		receipt.Outcome = protocol.ReceiptClosedNotAccepted
		receipt.SendResponse = nil
		s.receipts[upload.AttemptID] = receipt
		return receipt, nil
	}
	s.receipts[upload.AttemptID] = receipt
	return protocol.SendReceipt{}, errors.New("lost accepted upload reply")
}
func closedRecoveryFixture(t *testing.T) (*responseOutboxState, *sendNativeFake, *closedResponseSession, *SendCoordinator, *InteractionResponseOutbox, native.GroupEvent) {
	t.Helper()
	ctx := context.Background()
	state, initial, event := responseOutboxFixture(t)
	op, attempt := interactionID(30), interactionID(31)
	first := newResponseOutbox(t, state, initial)
	if _, err := first.Send(ctx, event, []byte("immutable private response"), op, attempt, 123, 8); err != nil {
		t.Fatal(err)
	}
	closed := sendReceipt(op, attempt, event.Group, 8, 7, protocol.ReceiptClosedNotAccepted)
	closed.CommunityID = state.binding.Community
	n := &sendNativeFake{binding: state.binding, group: native.GroupInfo{ID: event.Group, Kind: 0, Community: &state.binding.Community, State: 2, Epoch: 8}, prepared: native.PreparedSend{Epoch: 8, Ciphertext: []byte("ciphertext")}}
	coordinator, err := NewSendCoordinatorWithAdapters(n, sendAuthorityFake{discovery: client.Discovery{InstanceID: state.binding.Instance, Generation: 9}})
	if err != nil {
		t.Fatal(err)
	}
	session := &closedResponseSession{sendSessionFake: &sendSessionFake{receipts: map[id.ID]protocol.SendReceipt{attempt: closed}}, state: state, t: t}
	restart, err := NewInteractionResponseOutboxWithAdapters(state, coordinator, session)
	if err != nil {
		t.Fatal(err)
	}
	return state, n, session, coordinator, restart, event
}

func TestInteractionResponseOutboxRecoversClosedAttemptWithoutRedispatch(t *testing.T) {
	ctx := context.Background()
	state, _, session, coordinator, restart, event := closedRecoveryFixture(t)
	op, attempt := interactionID(30), interactionID(31)
	checks := 0
	receipt, found, err := restart.ResumeWithPreflight(ctx, event, func(context.Context) error { checks++; return nil })
	if err != nil || !found || receipt.Outcome != protocol.ReceiptAccepted {
		t.Fatalf("closed response blocked queue: found=%v outcome=%v err=%v", found, receipt.Outcome, err)
	}
	if len(session.uploads) != 1 || checks != 3 {
		t.Fatalf("uploads=%d authorization checks=%d", len(session.uploads), checks)
	}
	upload := session.uploads[0]
	if upload.OperationID != op || upload.AttemptID == attempt || upload.PredecessorAttemptID == nil || *upload.PredecessorAttemptID != attempt || upload.ExpectedGeneration != 9 {
		t.Fatalf("invalid successor coordinates: %+v", upload)
	}
	persisted, ok, err := restart.Lookup(ctx, event)
	if err != nil || !ok || string(persisted.Body) != "immutable private response" || persisted.OperationID != op {
		t.Fatal("successor changed saved response")
	}
	again, err := NewInteractionResponseOutboxWithAdapters(state, coordinator, session)
	if err != nil {
		t.Fatal(err)
	}
	receipt, found, err = again.ResumeWithPreflight(ctx, event, func(context.Context) error { t.Fatal("accepted response was reauthorized or sent again"); return nil })
	if err != nil || !found || receipt.Outcome != protocol.ReceiptAccepted || len(session.uploads) != 1 {
		t.Fatal("restart did not resume exact accepted successor")
	}
}

func TestInteractionResponseOutboxDeniedClosedReplayDoesNotPromote(t *testing.T) {
	state, n, session, _, restart, event := closedRecoveryFixture(t)
	before := append([]byte(nil), state.value.Value...)
	denied := errors.New("permission revoked")
	_, found, err := restart.ResumeWithPreflight(context.Background(), event, func(context.Context) error { return denied })
	if !found || !errors.Is(err, denied) || len(session.uploads) != 0 || n.prepareCalls != 0 || string(state.value.Value) != string(before) {
		t.Fatalf("denied replay promoted or sent: found=%v err=%v revisions=%d", found, err, state.value.Revision)
	}
}

type lostSuccessorState struct {
	*responseOutboxState
	lost bool
}

func (s *lostSuccessorState) StateTransaction(ctx context.Context, tx id.ID, changes []native.StateChange) ([]uint64, error) {
	result, err := s.responseOutboxState.StateTransaction(ctx, tx, changes)
	if err == nil && !s.lost {
		s.lost = true
		return nil, errors.New("lost committed CAS reply")
	}
	return result, err
}
func TestInteractionResponseOutboxLostSuccessorCommitReusesWinningAttempt(t *testing.T) {
	state, _, session, coordinator, _, event := closedRecoveryFixture(t)
	uncertain := &lostSuccessorState{responseOutboxState: state}
	outbox, err := NewInteractionResponseOutboxWithAdapters(uncertain, coordinator, session)
	if err != nil {
		t.Fatal(err)
	}
	receipt, found, err := outbox.Resume(context.Background(), event)
	if err != nil || !found || receipt.Outcome != protocol.ReceiptAccepted || state.value.Revision != 2 || len(session.uploads) != 1 {
		t.Fatalf("lost CAS did not reuse durable successor: found=%v err=%v revision=%d uploads=%d", found, err, state.value.Revision, len(session.uploads))
	}
}
func TestInteractionResponseOutboxDoesNotPromoteUncertainOutcomes(t *testing.T) {
	for _, outcome := range []protocol.SendOutcome{protocol.ReceiptAccepted, protocol.ReceiptUnknownAfterRestore} {
		t.Run(string(rune('0'+outcome)), func(t *testing.T) {
			state, n, session, _, outbox, event := closedRecoveryFixture(t)
			old := session.receipts[interactionID(31)]
			old.Outcome = outcome
			if outcome == protocol.ReceiptAccepted {
				accepted := sendReceipt(old.OperationID, old.AttemptID, old.GroupID, old.ExpectedGeneration, old.Epoch, outcome)
				accepted.CommunityID = old.CommunityID
				old = accepted
			}
			session.receipts[old.AttemptID] = old
			before := append([]byte(nil), state.value.Value...)
			_, _, _ = outbox.Resume(context.Background(), event)
			if len(session.uploads) != 0 || n.prepareCalls != 0 || string(state.value.Value) != string(before) {
				t.Fatal("terminal outcome created successor")
			}
		})
	}
}

func TestInteractionResponseOutboxBoundsSuccessorsAndLinksRepeatedClosedAttempts(t *testing.T) {
	state, _, session, _, outbox, event := closedRecoveryFixture(t)
	session.closeUpload = true
	receipt, found, err := outbox.Resume(context.Background(), event)
	if err != nil || !found || receipt.Outcome != protocol.ReceiptClosedNotAccepted || state.value.Revision != 2 || len(session.uploads) != 1 {
		t.Fatal("one call created unbounded successors")
	}
	prior := session.uploads[0].AttemptID
	session.closeUpload = false
	receipt, found, err = outbox.Resume(context.Background(), event)
	if err != nil || !found || receipt.Outcome != protocol.ReceiptAccepted || state.value.Revision != 3 || len(session.uploads) != 2 || session.uploads[1].PredecessorAttemptID == nil || *session.uploads[1].PredecessorAttemptID != prior {
		t.Fatalf("repeated closed recovery lost chain: err=%v", err)
	}
}
func TestInteractionResponseOutboxRestartAfterPromotionPreservesAttemptOnAuthorizationFailure(t *testing.T) {
	for _, denyAt := range []int{2, 3} {
		t.Run(string(rune('0'+denyAt)), func(t *testing.T) {
			state, _, session, coordinator, outbox, event := closedRecoveryFixture(t)
			denied := errors.New("authorization changed")
			checks := 0
			_, found, err := outbox.ResumeWithPreflight(context.Background(), event, func(context.Context) error {
				checks++
				if checks == denyAt {
					return denied
				}
				return nil
			})
			if !found || !errors.Is(err, denied) || state.value.Revision != 2 || len(session.uploads) != 0 {
				t.Fatalf("authorization failure uploaded or discarded durable promotion: err=%v checks=%d", err, checks)
			}
			saved, _, err := outbox.Lookup(context.Background(), event)
			if err != nil {
				t.Fatal(err)
			}
			restarted, err := NewInteractionResponseOutboxWithAdapters(state, coordinator, session)
			if err != nil {
				t.Fatal(err)
			}
			receipt, found, err := restarted.ResumeWithPreflight(context.Background(), event, func(context.Context) error { return nil })
			if err != nil || !found || receipt.Outcome != protocol.ReceiptAccepted || len(session.uploads) != 1 || session.uploads[0].AttemptID != saved.AttemptID || state.value.Revision != 2 {
				t.Fatal("restart did not reuse exact promoted identity")
			}
		})
	}
}
