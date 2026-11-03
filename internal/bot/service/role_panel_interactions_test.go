package service

import (
	"context"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

type rolePanelAuthorityFake struct {
	allowed bool
	calls   int
}

func (f *rolePanelAuthorityFake) BotActorAuthority(context.Context, id.ID, id.ID) (client.ActorAuthority, bool, error) {
	f.calls++
	return client.ActorAuthority{EffectiveBits: 1 << 15}, f.allowed, nil
}

type rolePanelDestinationFake struct {
	group id.ID
	calls int
}

func (f *rolePanelDestinationFake) ResolveTextGroup(_ context.Context, _, _ id.ID) (id.ID, error) {
	f.calls++
	return f.group, nil
}

type rolePanelSendFake struct {
	input   SendInput
	receipt protocol.SendReceipt
	calls   int
	err     error
}

func (f *rolePanelSendFake) Send(_ context.Context, _ *client.Session, input SendInput) (protocol.SendReceipt, error) {
	f.calls++
	f.input = input
	return f.receipt, f.err
}

type rolePanelDiscoveryFake struct {
	generation uint64
	calls      int
}

func (f *rolePanelDiscoveryFake) DiscoverObserved(context.Context) (client.Discovery, client.ResponseObservation, error) {
	f.calls++
	return client.Discovery{InstanceID: id.New(), Generation: f.generation}, client.ResponseObservation{}, nil
}

func TestRolePanelPublishRequiresManagerAndReturnsAcceptedBotMessageIdentity(t *testing.T) {
	community, actor, channel, role, group, requestID := id.New(), id.New(), id.New(), id.New(), id.New(), id.New()
	payload, err := cborx.Marshal([]any{[]any{"🎮", role, "Gamers"}})
	if err != nil {
		t.Fatal(err)
	}
	authority := &rolePanelAuthorityFake{allowed: true}
	destination := &rolePanelDestinationFake{group: group}
	discovery := &rolePanelDiscoveryFake{generation: 7}
	sender := &rolePanelSendFake{receipt: protocol.SendReceipt{OperationID: requestID, AttemptID: rolePanelAttemptID(requestID), CommunityID: community,
		GroupID: group, ExpectedGeneration: 7, Outcome: protocol.ReceiptAccepted, SendResponse: &protocol.OriginalSendResponse{Sequence: 42}}}
	handler := RolePanelInteractions{Authority: authority, Destination: destination, Sender: sender, Discovery: discovery,
		Session: &client.Session{}, CommunityID: community, Now: func() time.Time { return time.Unix(100, 0) }}.Handlers()[protocol.OpRolePanelPublish]
	request := protocol.Request{RequestID: requestID, CommunityID: community, Operation: protocol.OpRolePanelPublish,
		Revision: rolePanelU64Ptr(0), Expires: rolePanelU64Ptr(130), Args: []any{channel, payload}}
	result, err := handler.Handle(context.Background(), InteractionActor{User: actor, Kind: 0}, request)
	if err != nil || result.Status != 0 {
		t.Fatalf("publish result=%+v err=%v", result, err)
	}
	var identity []any
	if err := cborx.Unmarshal(result.Data, &identity); err != nil || len(identity) != 2 {
		t.Fatalf("published identity=%v err=%v", identity, err)
	}
	if got, ok := identity[0].([]byte); !ok || len(got) != 16 {
		t.Fatalf("published channel=%v", identity[0])
	}
	if seq, ok := identity[1].(uint64); !ok || seq != 42 {
		t.Fatalf("published sequence=%v", identity[1])
	}
	if authority.calls != 1 || destination.calls != 1 || discovery.calls != 1 || sender.calls != 1 {
		t.Fatalf("calls authority=%d destination=%d discovery=%d sender=%d", authority.calls, destination.calls, discovery.calls, sender.calls)
	}
	if sender.input.OperationID != requestID || sender.input.AttemptID != rolePanelAttemptID(requestID) || sender.input.GroupID != group || sender.input.ExpectedGeneration != 7 {
		t.Fatalf("send input=%+v", sender.input)
	}
	if got := string(sender.input.RequestBody); got == "" {
		t.Fatal("bot message body is empty")
	}
}

func TestRolePanelPublishRefusesUnauthorizedOrUnacceptedMessage(t *testing.T) {
	community, actor, channel, group := id.New(), id.New(), id.New(), id.New()
	payload, _ := cborx.Marshal([]any{[]any{"✅", id.New(), "Readers"}})
	for _, tc := range []struct {
		name     string
		allowed  bool
		outcome  protocol.SendOutcome
		response *protocol.OriginalSendResponse
	}{
		{name: "non manager", allowed: false, outcome: protocol.ReceiptAccepted, response: &protocol.OriginalSendResponse{Sequence: 4}},
		{name: "no accepted receipt", allowed: true, outcome: protocol.ReceiptClosedNotAccepted},
		{name: "missing original sequence", allowed: true, outcome: protocol.ReceiptAccepted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reqID := id.New()
			authority := &rolePanelAuthorityFake{allowed: tc.allowed}
			destination := &rolePanelDestinationFake{group: group}
			discovery := &rolePanelDiscoveryFake{generation: 2}
			sender := &rolePanelSendFake{receipt: protocol.SendReceipt{OperationID: reqID, AttemptID: rolePanelAttemptID(reqID), CommunityID: community,
				GroupID: group, ExpectedGeneration: 2, Outcome: tc.outcome, SendResponse: tc.response}}
			handler := RolePanelInteractions{Authority: authority, Destination: destination, Sender: sender, Discovery: discovery,
				Session: &client.Session{}, CommunityID: community, Now: func() time.Time { return time.Unix(100, 0) }}.Handlers()[protocol.OpRolePanelPublish]
			result, err := handler.Handle(context.Background(), InteractionActor{User: actor, Kind: 0}, protocol.Request{RequestID: reqID, CommunityID: community,
				Operation: protocol.OpRolePanelPublish, Revision: rolePanelU64Ptr(0), Expires: rolePanelU64Ptr(130), Args: []any{channel, payload}})
			if err != nil || result.Status == 0 {
				t.Fatalf("unsafe role panel publish result=%+v err=%v", result, err)
			}
			if !tc.allowed && sender.calls != 0 {
				t.Fatal("unauthorized actor reached the Bot sender")
			}
		})
	}
}

func TestRolePanelPublishRetryAfterLostSendResponseReusesStableOperationAndAttempt(t *testing.T) {
	community, actor, channel, role, group, requestID := id.New(), id.New(), id.New(), id.New(), id.New(), id.New()
	payload, _ := cborx.Marshal([]any{[]any{"✅", role, "Readers"}})
	authority := &rolePanelAuthorityFake{allowed: true}
	destination := &rolePanelDestinationFake{group: group}
	discovery := &rolePanelDiscoveryFake{generation: 2}
	sender := &rolePanelSendFake{err: context.DeadlineExceeded}
	handler := RolePanelInteractions{Authority: authority, Destination: destination, Sender: sender, Discovery: discovery,
		Session: &client.Session{}, CommunityID: community, Now: func() time.Time { return time.Unix(100, 0) }}.Handlers()[protocol.OpRolePanelPublish]
	request := protocol.Request{RequestID: requestID, CommunityID: community, Operation: protocol.OpRolePanelPublish,
		Revision: rolePanelU64Ptr(0), Expires: rolePanelU64Ptr(130), Args: []any{channel, payload}}
	if _, err := handler.Handle(context.Background(), InteractionActor{User: actor, Kind: 0}, request); err == nil {
		t.Fatal("lost send response unexpectedly succeeded")
	}
	first := sender.input
	sender.err = nil
	sender.receipt = protocol.SendReceipt{OperationID: requestID, AttemptID: rolePanelAttemptID(requestID), CommunityID: community,
		GroupID: group, ExpectedGeneration: 2, Outcome: protocol.ReceiptAccepted, SendResponse: &protocol.OriginalSendResponse{Sequence: 12}}
	if result, err := handler.Handle(context.Background(), InteractionActor{User: actor, Kind: 0}, request); err != nil || result.Status != 0 {
		t.Fatalf("retry result=%+v err=%v", result, err)
	}
	if sender.calls != 2 || sender.input.OperationID != first.OperationID || sender.input.AttemptID != first.AttemptID ||
		sender.input.GroupID != first.GroupID || sender.input.ExpectedGeneration != first.ExpectedGeneration || string(sender.input.RequestBody) != string(first.RequestBody) {
		t.Fatal("retry did not reuse the exact encrypted message operation and attempt")
	}
}

func rolePanelU64Ptr(v uint64) *uint64 { return &v }
