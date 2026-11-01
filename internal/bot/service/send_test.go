package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"reflect"
	"testing"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/helper"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

func sendID(n byte) id.ID { var v id.ID; v[15] = n; return v }

func sendBinding() native.Binding {
	return native.Binding{Instance: sendID(1), Community: sendID(2), Installation: sendID(3)}
}

type sendNativeFake struct {
	binding                    native.Binding
	group                      native.GroupInfo
	inspectErr                 error
	interactionGroups          []native.InteractionGroupDescriptor
	interactionPrepareCalls    int
	prepared                   native.PreparedSend
	prepareCalls, confirmCalls int
	confirmed                  [][]byte
}

func (f *sendNativeFake) Binding() native.Binding { return f.binding }
func (f *sendNativeFake) InspectGroup(context.Context, id.ID) (native.GroupInfo, error) {
	return f.group, f.inspectErr
}
func (f *sendNativeFake) InteractionGroups(context.Context) ([]native.InteractionGroupDescriptor, error) {
	return f.interactionGroups, nil
}
func (f *sendNativeFake) PrepareSend(context.Context, id.ID, id.ID, id.ID, []byte, uint64, uint64) (native.PreparedSend, error) {
	f.prepareCalls++
	if f.inspectErr != nil {
		return native.PreparedSend{}, errors.New("generic send prepare cannot encrypt kind 3")
	}
	return f.prepared, nil
}
func (f *sendNativeFake) PrepareInteractionSend(_ context.Context, expected native.InteractionGroupExpected, snapshot []byte, _, _ id.ID, _ []byte, _, _ uint64) (native.PreparedSend, error) {
	f.interactionPrepareCalls++
	if expected.Group != f.interactionGroups[0].Group || expected.ParticipantDevice != f.interactionGroups[0].Binding.ParticipantDevice || len(snapshot) == 0 {
		return native.PreparedSend{}, errors.New("wrong private send proof")
	}
	return f.prepared, nil
}
func (f *sendNativeFake) ConfirmSendReceipt(_ context.Context, _ id.ID, b []byte) error {
	f.confirmCalls++
	f.confirmed = append(f.confirmed, append([]byte(nil), b...))
	return nil
}

type sendAuthorityFake struct{ discovery client.Discovery }

func (f sendAuthorityFake) DiscoverObserved(context.Context) (client.Discovery, client.ResponseObservation, error) {
	return f.discovery, client.ResponseObservation{}, nil
}

type sendSessionFake struct {
	receipts                 map[id.ID]protocol.SendReceipt
	postReceipt              *protocol.SendReceipt
	postErr                  error
	resolveReceipt           *protocol.SendReceipt
	resolveErr               error
	posts, lookups, resolves int
	uploads                  []client.BotMessageUpload
	resolutions              []client.BotMessageResolution
}

func (f *sendSessionFake) IssueBotInteractionAdmission(context.Context, id.ID, id.ID, id.ID, id.ID, id.ID) ([]byte, error) {
	return []byte{1}, nil
}
func (f *sendSessionFake) BotInteractionContextSnapshot(_ context.Context, _, request, group, participant, participantDevice id.ID) ([]byte, error) {
	return makeSignedSnapshotFixtureFor(request, []any{sendID(1), sendID(3), sendID(2), sendID(11), sendID(10), participant, participantDevice, group}, 100)
}

func (f *sendSessionFake) BotMessageReceipt(_ context.Context, _, operation, attempt, group id.ID) (protocol.SendReceipt, error) {
	f.lookups++
	if r, ok := f.receipts[attempt]; ok {
		return r, nil
	}
	return protocol.SendReceipt{}, client.ErrBotSendReceiptNotFound
}
func (f *sendSessionFake) PostBotMessage(_ context.Context, _, _ id.ID, upload client.BotMessageUpload) (protocol.SendReceipt, error) {
	f.posts++
	f.uploads = append(f.uploads, upload)
	if f.postReceipt != nil {
		f.receipts[upload.AttemptID] = *f.postReceipt
	}
	return protocol.SendReceipt{}, f.postErr
}
func (f *sendSessionFake) ResolveBotMessage(_ context.Context, _, _ id.ID, resolution client.BotMessageResolution) (protocol.SendReceipt, error) {
	f.resolves++
	f.resolutions = append(f.resolutions, resolution)
	if f.resolveErr != nil {
		return protocol.SendReceipt{}, f.resolveErr
	}
	if f.resolveReceipt == nil {
		return protocol.SendReceipt{}, errors.New("missing configured resolution")
	}
	f.receipts[resolution.AttemptID] = *f.resolveReceipt
	return *f.resolveReceipt, nil
}

func sendReceipt(op, attempt, group id.ID, generation, epoch uint64, outcome protocol.SendOutcome) protocol.SendReceipt {
	hash := sha256.Sum256([]byte("ciphertext"))
	r := protocol.SendReceipt{OperationID: op, AttemptID: attempt, CommunityID: sendID(2), GroupID: group, ExpectedGeneration: generation, CiphertextHash: hash[:], Epoch: epoch, Outcome: outcome}
	if outcome == protocol.ReceiptAccepted {
		body, _ := cborx.Marshal([]any{uint64(1), make([]byte, 32), uint64(100)})
		r.SendResponse = &protocol.OriginalSendResponse{Sequence: 1, FrankingTag: make([]byte, 32), ReceiveTimestamp: 100, Body: body}
	}
	return r
}

func sendFixture() (*SendCoordinator, *sendNativeFake, *sendSessionFake, SendInput) {
	b := sendBinding()
	group := sendID(4)
	g := native.GroupInfo{ID: group, Kind: 0, Community: &b.Community, Target: sendID(5), State: 2, Epoch: 9, NextSequence: 1}
	n := &sendNativeFake{binding: b, group: g, prepared: native.PreparedSend{MessageID: sendID(8), Epoch: 9, Class: 0, Ciphertext: []byte("ciphertext"), UploadBody: []byte("exact-canonical-upload")}}
	s := &sendSessionFake{receipts: map[id.ID]protocol.SendReceipt{}}
	a := sendAuthorityFake{discovery: client.Discovery{InstanceID: b.Instance, Generation: 12}}
	c, _ := newSendCoordinator(n, a)
	return c, n, s, SendInput{OperationID: sendID(6), AttemptID: sendID(7), GroupID: group, RequestBody: []byte{0x84, 0, 0xf6, 0x60, 0x80}, OriginalNow: 100, ExpectedGeneration: 12}
}

func TestSendAcceptsActiveInstallationBoundInteractionGroup(t *testing.T) {
	c, n, s, in := sendFixture()
	n.inspectErr = &helper.StatusError{Status: 2} // Generic helper inspection refuses kind 3.
	n.interactionGroups = []native.InteractionGroupDescriptor{{
		Group: in.GroupID, BotDevice: sendID(10), Epoch: 9, State: 2,
		Binding: native.InteractionGroupBinding{Instance: n.binding.Instance, Installation: n.binding.Installation,
			Community: n.binding.Community, BotUser: sendID(11), ParticipantUser: sendID(12), ParticipantDevice: sendID(13),
			PolicyVersion: 3, E2EEVersion: 1, MediaVersion: 0},
	}}
	accepted := sendReceipt(in.OperationID, in.AttemptID, in.GroupID, in.ExpectedGeneration, 9, protocol.ReceiptAccepted)
	s.postErr = errors.New("lost HTTP response")
	s.resolveReceipt = &accepted
	got, err := c.send(context.Background(), s, in)
	if err != nil || got.Outcome != protocol.ReceiptAccepted || s.posts != 1 || n.prepareCalls != 0 || n.interactionPrepareCalls != 1 {
		t.Fatalf("interaction send outcome=%v err=%v posts=%d generic prepares=%d private prepares=%d", got.Outcome, err, s.posts, n.prepareCalls, n.interactionPrepareCalls)
	}
}

func TestSendRejectsInteractionDescriptorWithOverlappingIdentities(t *testing.T) {
	c, n, s, in := sendFixture()
	n.inspectErr = &helper.StatusError{Status: 2}
	n.interactionGroups = []native.InteractionGroupDescriptor{{
		Group: in.GroupID, BotDevice: sendID(10), Epoch: 9, State: 2,
		Binding: native.InteractionGroupBinding{Instance: n.binding.Instance, Installation: n.binding.Installation,
			Community: n.binding.Community, BotUser: sendID(12), ParticipantUser: sendID(12), ParticipantDevice: sendID(13),
			PolicyVersion: 3, E2EEVersion: 1, MediaVersion: 0},
	}}
	if _, err := c.send(context.Background(), s, in); err == nil || n.prepareCalls != 0 || s.posts != 0 {
		t.Fatalf("overlapping interaction identities reached send: err=%v prepares=%d posts=%d", err, n.prepareCalls, s.posts)
	}
}

func TestSendRecoversLostAcceptedResponseAndRestartDoesNotPostAgain(t *testing.T) {
	c, n, s, in := sendFixture()
	accepted := sendReceipt(in.OperationID, in.AttemptID, in.GroupID, in.ExpectedGeneration, 9, protocol.ReceiptAccepted)
	s.postErr = errors.New("connection lost after server commit")
	s.postReceipt = &accepted
	// The first recovery lookup sees the receipt committed by the lost response.
	got, err := c.send(context.Background(), s, in)
	if err != nil || got.Outcome != protocol.ReceiptAccepted {
		t.Fatalf("first Send=%+v, %v", got, err)
	}
	if s.posts != 1 || s.resolves != 0 || n.confirmCalls != 1 {
		t.Fatalf("calls post=%d resolve=%d confirm=%d", s.posts, s.resolves, n.confirmCalls)
	}
	// Reopening the coordinator with the same immutable inputs first consults the server receipt.
	c2, _, _, _ := sendFixture()
	got, err = c2.send(context.Background(), s, in)
	if err != nil || got.Outcome != protocol.ReceiptAccepted {
		t.Fatalf("restart Send=%+v, %v", got, err)
	}
	if s.posts != 1 {
		t.Fatalf("restart retransmitted POST, posts=%d", s.posts)
	}
}

func TestSendResolvesLateReceipt404WithoutReencrypting(t *testing.T) {
	c, n, s, in := sendFixture()
	closed := sendReceipt(in.OperationID, in.AttemptID, in.GroupID, in.ExpectedGeneration, 9, protocol.ReceiptClosedNotAccepted)
	s.postErr = errors.New("HTTP result unknown")
	s.resolveReceipt = &closed
	got, err := c.send(context.Background(), s, in)
	if err != nil || got.Outcome != protocol.ReceiptClosedNotAccepted {
		t.Fatalf("Send=%+v, %v", got, err)
	}
	if s.posts != 1 || s.resolves != 1 || n.prepareCalls != 1 || n.confirmCalls != 1 {
		t.Fatalf("post=%d resolve=%d prepare=%d confirm=%d", s.posts, s.resolves, n.prepareCalls, n.confirmCalls)
	}
	if s.resolutions[0].CiphertextSHA256 == ([32]byte{}) {
		t.Fatal("resolution omitted exact ciphertext hash")
	}
}

func TestSendUnknownReceiptIsTerminal(t *testing.T) {
	c, _, s, in := sendFixture()
	unknown := sendReceipt(in.OperationID, in.AttemptID, in.GroupID, in.ExpectedGeneration, 9, protocol.ReceiptUnknownAfterRestore)
	s.postErr = errors.New("HTTP result unknown")
	s.resolveReceipt = &unknown
	got, err := c.send(context.Background(), s, in)
	if !errors.Is(err, ErrSendTerminal) || got.Outcome != protocol.ReceiptUnknownAfterRestore {
		t.Fatalf("Send=%+v, %v", got, err)
	}
	if s.posts != 1 || s.resolves != 1 {
		t.Fatalf("post=%d resolve=%d", s.posts, s.resolves)
	}
}

func TestSendWithPreflightChecksOnlyUnacceptedAttempts(t *testing.T) {
	c, n, s, in := sendFixture()
	preflightCalls := 0
	blocked := errors.New("read authorization revoked")
	preflight := func(context.Context) error {
		preflightCalls++
		return blocked
	}

	// A durable accepted receipt is authoritative and can be confirmed without
	// reauthorizing or changing the immutable attempt.
	accepted := sendReceipt(in.OperationID, in.AttemptID, in.GroupID, in.ExpectedGeneration, 9, protocol.ReceiptAccepted)
	s.receipts[in.AttemptID] = accepted
	got, err := c.sendWithPreflight(context.Background(), s, in, preflight)
	if err != nil || got.Outcome != protocol.ReceiptAccepted || preflightCalls != 0 || n.prepareCalls != 0 {
		t.Fatalf("accepted replay receipt=%+v err=%v preflight=%d prepares=%d", got, err, preflightCalls, n.prepareCalls)
	}

	// With no server receipt, current authorization is required before preparing
	// or posting the saved response.
	delete(s.receipts, in.AttemptID)
	got, err = c.sendWithPreflight(context.Background(), s, in, preflight)
	if !errors.Is(err, blocked) || preflightCalls != 1 || n.prepareCalls != 0 || s.posts != 0 {
		t.Fatalf("blocked replay receipt=%+v err=%v preflight=%d prepares=%d posts=%d", got, err, preflightCalls, n.prepareCalls, s.posts)
	}
}

type reviewRevokeDuringPrepare struct {
	*sendNativeFake
	allowed *bool
}

func (n reviewRevokeDuringPrepare) PrepareSend(ctx context.Context, operation, attempt, group id.ID, body []byte, now, generation uint64) (native.PreparedSend, error) {
	*n.allowed = false
	return n.sendNativeFake.PrepareSend(ctx, operation, attempt, group, body, now, generation)
}

func TestSendRechecksAuthorizationAtUploadBoundary(t *testing.T) {
	c, n, s, in := sendFixture()
	allowed := true
	c.native = reviewRevokeDuringPrepare{sendNativeFake: n, allowed: &allowed}
	blocked := errors.New("creator revoked during native preparation")
	preflight := func(context.Context) error {
		if !allowed {
			return blocked
		}
		return nil
	}
	_, err := c.sendWithPreflight(context.Background(), s, in, preflight)
	if !errors.Is(err, blocked) || s.posts != 0 || n.prepareCalls != 1 {
		t.Fatalf("post-prepare revoke result err=%v posts=%d prepares=%d", err, s.posts, n.prepareCalls)
	}
}

func TestSendReauthorizesPreparedAttemptOnResume(t *testing.T) {
	c, n, s, in := sendFixture()
	prepared := false
	blocked := errors.New("creator revoked before prepared retry upload")
	preflight := func(context.Context) error {
		if prepared && s.posts > 0 {
			return blocked
		}
		return nil
	}
	s.postErr = errors.New("temporary upload failure")
	s.resolveErr = errors.New("receipt lookup unavailable")
	if _, err := c.sendWithPreflight(context.Background(), s, in, func(ctx context.Context) error {
		err := preflight(ctx)
		return err
	}); err == nil {
		t.Fatal("first uncertain attempt unexpectedly succeeded")
	}
	prepared = true
	// Model current authorization being revoked after the uncertain upload; a second
	// invocation must stop before posting the already-prepared immutable ciphertext.
	if _, err := c.sendWithPreflight(context.Background(), s, in, preflight); !errors.Is(err, blocked) {
		t.Fatalf("prepared retry authorization error=%v", err)
	}
	if s.posts != 1 || n.prepareCalls != 1 {
		t.Fatalf("prepared retry was reuploaded or reencrypted: posts=%d prepares=%d", s.posts, n.prepareCalls)
	}
}

func TestSendAllowsSuccessorOnlyAfterClosedAttemptAndNewEpoch(t *testing.T) {
	c, n, s, in := sendFixture()
	prev := sendID(10)
	in.AttemptID = sendID(11)
	in.PredecessorAttemptID = &prev
	prior := sendReceipt(in.OperationID, prev, in.GroupID, in.ExpectedGeneration, 8, protocol.ReceiptClosedNotAccepted)
	s.receipts[prev] = prior
	n.group.Epoch = 9
	closed := sendReceipt(in.OperationID, in.AttemptID, in.GroupID, in.ExpectedGeneration, 9, protocol.ReceiptClosedNotAccepted)
	closed.PredecessorAttemptID = &prev
	s.resolveReceipt = &closed
	s.postErr = errors.New("network unavailable")
	if _, err := c.send(context.Background(), s, in); err != nil {
		t.Fatal(err)
	}
	if len(s.uploads) != 1 || s.uploads[0].PredecessorAttemptID == nil || *s.uploads[0].PredecessorAttemptID != prev {
		t.Fatalf("upload predecessor=%v", s.uploads)
	}
}

func TestSendAllowsSuccessorAfterClosedAttemptAtSameEpoch(t *testing.T) {
	c, n, s, in := sendFixture()
	prev := sendID(10)
	in.AttemptID = sendID(11)
	in.PredecessorAttemptID = &prev
	prior := sendReceipt(in.OperationID, prev, in.GroupID, in.ExpectedGeneration, n.group.Epoch, protocol.ReceiptClosedNotAccepted)
	s.receipts[prev] = prior
	closed := sendReceipt(in.OperationID, in.AttemptID, in.GroupID, in.ExpectedGeneration, n.group.Epoch, protocol.ReceiptClosedNotAccepted)
	closed.PredecessorAttemptID = &prev
	s.resolveReceipt = &closed
	s.postErr = errors.New("network unavailable")
	if _, err := c.send(context.Background(), s, in); err != nil {
		t.Fatalf("same-epoch successor was refused: %v", err)
	}
	if len(s.uploads) != 1 || s.uploads[0].PredecessorAttemptID == nil || *s.uploads[0].PredecessorAttemptID != prev {
		t.Fatalf("upload predecessor=%v", s.uploads)
	}
}

func TestSendRejectsSuccessorFromUnknownAttempt(t *testing.T) {
	c, _, s, in := sendFixture()
	prev := sendID(10)
	in.PredecessorAttemptID = &prev
	s.receipts[prev] = sendReceipt(in.OperationID, prev, in.GroupID, in.ExpectedGeneration, 8, protocol.ReceiptUnknownAfterRestore)
	if _, err := c.send(context.Background(), s, in); err == nil {
		t.Fatal("unknown predecessor accepted")
	}
	if s.posts != 0 {
		t.Fatalf("posted successor after unknown predecessor: %d", s.posts)
	}
}

func TestSendGenerationChangeResolvesOldAttemptWithoutPosting(t *testing.T) {
	c, _, s, in := sendFixture()
	c.authority = sendAuthorityFake{discovery: client.Discovery{InstanceID: sendBinding().Instance, Generation: 13}}
	closed := sendReceipt(in.OperationID, in.AttemptID, in.GroupID, in.ExpectedGeneration, 9, protocol.ReceiptUnknownAfterRestore)
	s.resolveReceipt = &closed
	got, err := c.send(context.Background(), s, in)
	if !errors.Is(err, ErrSendTerminal) || got.Outcome != protocol.ReceiptUnknownAfterRestore {
		t.Fatalf("Send=%+v, %v", got, err)
	}
	if s.posts != 0 || s.resolves != 1 {
		t.Fatalf("generation drift posted=%d resolved=%d", s.posts, s.resolves)
	}
}

func TestSendEpochChangeResolvesCachedAttemptWithoutPosting(t *testing.T) {
	c, n, s, in := sendFixture()
	n.prepared.Epoch = 8
	closed := sendReceipt(in.OperationID, in.AttemptID, in.GroupID, in.ExpectedGeneration, 8, protocol.ReceiptClosedNotAccepted)
	s.resolveReceipt = &closed
	got, err := c.send(context.Background(), s, in)
	if err != nil || got.Outcome != protocol.ReceiptClosedNotAccepted {
		t.Fatalf("Send=%+v, %v", got, err)
	}
	if s.posts != 0 || s.resolves != 1 || n.prepareCalls != 1 {
		t.Fatalf("epoch drift post=%d resolve=%d prepare=%d", s.posts, s.resolves, n.prepareCalls)
	}
}

func TestSendRejectsForeignInstanceBeforeUsingSession(t *testing.T) {
	c, _, s, in := sendFixture()
	c.authority = sendAuthorityFake{discovery: client.Discovery{InstanceID: sendID(99), Generation: 12}}
	if _, err := c.send(context.Background(), s, in); !errors.Is(err, client.ErrInstanceChanged) {
		t.Fatalf("foreign instance error=%v", err)
	}
	if s.posts != 0 || s.lookups != 0 {
		t.Fatalf("foreign instance network use: post=%d lookup=%d", s.posts, s.lookups)
	}
}

func TestSendRateLimitLeavesPreparedAttemptPending(t *testing.T) {
	coordinator, n, session, input := sendFixture()
	refusal := &client.APIError{Status: 429, Code: "E_RATE_LIMITED", Outcome: client.OutcomeDefinitive}
	session.postErr = refusal
	if _, err := coordinator.send(t.Context(), session, input); !errors.Is(err, refusal) {
		t.Fatalf("send error=%v", err)
	}
	if session.lookups != 1 || session.resolves != 0 || n.confirmCalls != 0 {
		t.Fatalf("refused upload was resolved/closed: lookups=%d resolves=%d confirmations=%d", session.lookups, session.resolves, n.confirmCalls)
	}
	accepted := sendReceipt(input.OperationID, input.AttemptID, input.GroupID, 12, 9, protocol.ReceiptAccepted)
	session.postReceipt = &accepted
	session.postErr = errors.New("lost upload response")
	receipt, err := coordinator.send(t.Context(), session, input)
	if err != nil || receipt.Outcome != protocol.ReceiptAccepted || session.resolves != 0 {
		t.Fatalf("next send receipt=%+v error=%v resolves=%d", receipt, err, session.resolves)
	}
}

type sendPreparedResolver interface {
	ResolvePrepared(context.Context, SendSession, SendInput) (protocol.SendReceipt, error)
}
type immutableResolutionNative struct {
	*sendNativeFake
	expected SendInput
	inputs   []SendInput
}

func (n *immutableResolutionNative) PrepareSend(ctx context.Context, operation, attempt, group id.ID, body []byte, now, generation uint64) (native.PreparedSend, error) {
	in := SendInput{OperationID: operation, AttemptID: attempt, GroupID: group, RequestBody: append([]byte(nil), body...), OriginalNow: now, ExpectedGeneration: generation}
	n.inputs = append(n.inputs, in)
	if operation != n.expected.OperationID || attempt != n.expected.AttemptID || group != n.expected.GroupID || !bytes.Equal(body, n.expected.RequestBody) || now != n.expected.OriginalNow || generation != n.expected.ExpectedGeneration {
		return native.PreparedSend{}, errors.New("resolution changed immutable input")
	}
	return n.sendNativeFake.PrepareSend(ctx, operation, attempt, group, body, now, generation)
}
func TestResolvePreparedClosesSameAttemptWithoutUploading(t *testing.T) {
	coordinator, helper, session, in := sendFixture()
	resolver, ok := any(coordinator).(sendPreparedResolver)
	if !ok {
		t.Fatal("coordinator has no resolve-only path for revoked prepared attempts")
	}
	helper.group.State = 4
	replay := &immutableResolutionNative{sendNativeFake: helper, expected: in}
	coordinator.native = replay
	closed := sendReceipt(in.OperationID, in.AttemptID, in.GroupID, in.ExpectedGeneration, helper.prepared.Epoch, protocol.ReceiptClosedNotAccepted)
	session.resolveReceipt = &closed
	session.resolveErr = context.DeadlineExceeded
	if _, err := resolver.ResolvePrepared(context.Background(), session, in); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lost resolution response=%v", err)
	}
	session.resolveErr = nil
	got, err := resolver.ResolvePrepared(context.Background(), session, in)
	if err != nil || got.Outcome != protocol.ReceiptClosedNotAccepted || session.posts != 0 || session.resolves != 2 || helper.confirmCalls != 1 || len(replay.inputs) != 2 {
		t.Fatalf("resolve result=%+v err=%v uploads=%d resolutions=%d confirmations=%d prepares=%d", got, err, session.posts, session.resolves, helper.confirmCalls, len(replay.inputs))
	}
	if !reflect.DeepEqual(session.resolutions[0], session.resolutions[1]) {
		t.Fatal("retry replaced original ciphertext resolution")
	}
}
func TestResolvePreparedRecoversLostAcceptedReceiptBeforeClosing(t *testing.T) {
	coordinator, helper, session, in := sendFixture()
	resolver, ok := any(coordinator).(sendPreparedResolver)
	if !ok {
		t.Fatal("coordinator has no resolve-only receipt recovery")
	}
	accepted := sendReceipt(in.OperationID, in.AttemptID, in.GroupID, in.ExpectedGeneration, helper.prepared.Epoch, protocol.ReceiptAccepted)
	session.receipts[in.AttemptID] = accepted
	got, err := resolver.ResolvePrepared(context.Background(), session, in)
	if err != nil || got.Outcome != protocol.ReceiptAccepted || helper.prepareCalls != 0 || session.resolves != 0 || session.posts != 0 || helper.confirmCalls != 1 {
		t.Fatalf("accepted recovery result=%+v error=%v prepares=%d resolutions=%d uploads=%d confirmations=%d", got, err, helper.prepareCalls, session.resolves, session.posts, helper.confirmCalls)
	}
}

type unavailablePreparedNative struct {
	*sendNativeFake
	failure error
	blocked map[id.ID]bool
	failed  chan id.ID
}

func (n unavailablePreparedNative) PrepareSend(ctx context.Context, operation, attempt, group id.ID, body []byte, now, generation uint64) (native.PreparedSend, error) {
	if n.blocked == nil || n.blocked[operation] {
		n.prepareCalls++
		if n.failed != nil {
			n.failed <- operation
		}
		return native.PreparedSend{}, n.failure
	}
	return n.sendNativeFake.PrepareSend(ctx, operation, attempt, group, body, now, generation)
}
func TestResolvePreparedDistinguishesMissingCiphertextFromRetryableFailure(t *testing.T) {
	for _, test := range []struct {
		name     string
		failure  error
		terminal bool
	}{
		{"conflict", &helper.StatusError{Status: 4}, true},
		{"not found", &helper.StatusError{Status: 7}, true},
		{"unknown", &helper.StatusError{Status: 5}, false},
		{"deadline", context.DeadlineExceeded, false},
		{"transport", errors.New("private native transport details"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			coordinator, native, session, in := sendFixture()
			coordinator.native = unavailablePreparedNative{sendNativeFake: native, failure: test.failure}
			_, err := coordinator.ResolvePrepared(context.Background(), session, in)
			var classified interface{ FailureClass() string }
			terminal := errors.As(err, &classified) && classified.FailureClass() == "send_not_prepared"
			if terminal != test.terminal || !errors.Is(err, test.failure) || session.posts != 0 || session.resolves != 0 || native.confirmCalls != 0 {
				t.Fatalf("missing-ciphertext classification: terminal=%v error=%v posts=%d resolves=%d confirms=%d", terminal, err, session.posts, session.resolves, native.confirmCalls)
			}
			if test.terminal && err.Error() != "bot send has no recoverable prepared ciphertext" {
				t.Fatalf("unbounded terminal error: %q", err)
			}
		})
	}
}

func TestResolvePreparedDefinitiveRefusalIsTerminal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		code     string
		outcome  client.Outcome
		terminal bool
	}{
		{"closed group", 404, "E_NOT_FOUND", client.OutcomeDefinitive, true},
		{"forbidden", 403, "E_FORBIDDEN", client.OutcomeDefinitive, true},
		{"leaf revoked", 403, "E_LEAF_NOT_CURRENT", client.OutcomeDefinitive, true},
		{"timeout", 504, "unavailable", client.OutcomeDefinitive, false},
		{"unknown outcome", 403, "E_FORBIDDEN", client.OutcomeUnknown, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			coordinator, native, session, in := sendFixture()
			cause := &client.APIError{Status: tc.status, Code: tc.code, Outcome: tc.outcome}
			session.resolveErr = cause
			_, err := coordinator.ResolvePrepared(t.Context(), session, in)
			if errors.Is(err, ErrSendTerminal) != tc.terminal || !errors.Is(err, cause) || session.posts != 0 || session.resolves != 1 || native.confirmCalls != 0 {
				t.Fatalf("terminal=%v want=%v error=%v posts=%d resolves=%d confirms=%d", errors.Is(err, ErrSendTerminal), tc.terminal, err, session.posts, session.resolves, native.confirmCalls)
			}
		})
	}
}
