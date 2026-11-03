package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/moderation"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

type manualRemovalLookupFake struct {
	receipt               AcceptedManualRemovalReceipt
	err                   error
	request, installation id.ID
	calls                 int
}

func (f *manualRemovalLookupFake) AcceptedManualRemoval(_ context.Context, installation, request id.ID) (AcceptedManualRemovalReceipt, error) {
	f.calls++
	f.installation, f.request = installation, request
	return f.receipt, f.err
}

type manualEvidenceWriterFake struct {
	input moderation.EvidenceInput
	calls int
	err   error
	bound bool
}

type manualWarningLookupFake struct {
	receipt               AcceptedManualWarningReceipt
	err                   error
	request, installation id.ID
	calls                 int
}

func (f *manualWarningLookupFake) AcceptedManualWarning(_ context.Context, installation, request id.ID) (AcceptedManualWarningReceipt, error) {
	f.calls++
	f.installation, f.request = installation, request
	return f.receipt, f.err
}

func (f *manualEvidenceWriterFake) BoundTo(id.ID, id.ID) bool { return f.bound }

func (f *manualEvidenceWriterFake) Save(_ context.Context, input moderation.EvidenceInput) error {
	f.calls++
	f.input = input
	return f.err
}

func TestManualModerationEvidenceRequiresAcceptedActionAndBindsActorCommunity(t *testing.T) {
	community, installation, request, actor, device, group := interactionID(2), interactionID(3), interactionID(4), interactionID(5), interactionID(6), interactionID(7)
	lookup := &manualRemovalLookupFake{receipt: AcceptedManualRemovalReceipt{
		ActionID: interactionID(9), CommunityID: community, GroupID: group, ActorUserID: actor, ActorDeviceID: device,
		TargetSequence: 44, Revision: 12, AcceptedAt: time.Now().Unix(),
	}}
	writer := &manualEvidenceWriterFake{bound: true}
	h := ModerationInteractions{CommunityID: community, InstallationID: installation, ManualRemovalLookup: lookup, EvidenceWriter: writer}
	handler, ok := h.Handlers()[protocol.OpModerationEvidence]
	if !ok {
		t.Fatal("manual evidence operation is not registered")
	}
	body, _ := cborx.Marshal([]any{request, "reviewed by moderator", "quoted excerpt"})
	result, err := handler.Handle(context.Background(), InteractionActor{User: actor, Device: device}, protocol.Request{CommunityID: community, Args: []any{request, "reviewed by moderator", "quoted excerpt"}})
	if err != nil || result.Status != 0 || lookup.calls != 1 || lookup.installation != installation || lookup.request != request {
		t.Fatalf("result=%+v lookup=%+v err=%v body=%x", result, lookup, err, body)
	}
	if writer.calls != 1 || writer.input.OperationID != request || !writer.input.MessageID.IsZero() || writer.input.Action != moderation.ActionRemove || writer.input.Reason != "reviewed by moderator" || writer.input.Excerpt != "quoted excerpt" || writer.input.AcceptedActionID != lookup.receipt.ActionID || writer.input.AcceptedRevision != lookup.receipt.Revision || writer.input.GroupID != group || writer.input.TargetSequence != 44 {
		t.Fatalf("saved evidence=%+v calls=%d", writer.input, writer.calls)
	}
}

func TestManualWarningEvidenceRequiresMatchingAcceptedWarningReceipt(t *testing.T) {
	community, installation, request, actor, device, target := interactionID(2), interactionID(3), interactionID(4), interactionID(5), interactionID(6), interactionID(7)
	lookup := &manualWarningLookupFake{receipt: AcceptedManualWarningReceipt{RequestID: request, ActionID: interactionID(8),
		CommunityID: community, ActorUserID: actor, ActorDeviceID: device, TargetUserID: target, Revision: 12, AcceptedAt: time.Now().Unix()}}
	writer := &manualEvidenceWriterFake{bound: true}
	h := ModerationInteractions{CommunityID: community, InstallationID: installation, ManualWarningLookup: lookup, EvidenceWriter: writer}
	handler := h.Handlers()[protocol.OpModerationEvidence]
	result, err := handler.Handle(context.Background(), InteractionActor{User: actor, Device: device},
		protocol.Request{CommunityID: community, Args: []any{request, "reason encrypted in Bot state", ""}})
	if err != nil || result.Status != 0 || lookup.calls != 1 || lookup.installation != installation || lookup.request != request {
		t.Fatalf("result=%+v lookup=%+v err=%v", result, lookup, err)
	}
	if writer.calls != 1 || writer.input.OperationID != request || writer.input.Action != moderation.ActionWarn ||
		writer.input.Rule != moderation.RuleManual || writer.input.Reason != "reason encrypted in Bot state" || writer.input.AcceptedActionID != lookup.receipt.ActionID ||
		writer.input.AcceptedRevision != lookup.receipt.Revision || writer.input.TargetUserID != target || writer.input.ActorUserID != actor ||
		writer.input.ActorDeviceID != device || !writer.input.MessageID.IsZero() || !writer.input.GroupID.IsZero() || writer.input.TargetSequence != 0 {
		t.Fatalf("saved warning evidence=%+v calls=%d", writer.input, writer.calls)
	}
}

func TestManualWarningEvidenceFailsClosedOnMissingOrMismatchedReceipt(t *testing.T) {
	community, installation, request, actor, device, target := interactionID(2), interactionID(3), interactionID(4), interactionID(5), interactionID(6), interactionID(7)
	cases := []struct {
		name    string
		receipt AcceptedManualWarningReceipt
		err     error
	}{
		{name: "lookup unavailable", err: errors.New("offline")},
		{name: "wrong request", receipt: AcceptedManualWarningReceipt{RequestID: interactionID(8), ActionID: interactionID(9), CommunityID: community, ActorUserID: actor, ActorDeviceID: device, TargetUserID: target, Revision: 1, AcceptedAt: time.Now().Unix()}},
		{name: "wrong community", receipt: AcceptedManualWarningReceipt{RequestID: request, ActionID: interactionID(9), CommunityID: interactionID(10), ActorUserID: actor, ActorDeviceID: device, TargetUserID: target, Revision: 1, AcceptedAt: time.Now().Unix()}},
		{name: "wrong actor", receipt: AcceptedManualWarningReceipt{RequestID: request, ActionID: interactionID(9), CommunityID: community, ActorUserID: interactionID(11), ActorDeviceID: device, TargetUserID: target, Revision: 1, AcceptedAt: time.Now().Unix()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lookup := &manualWarningLookupFake{receipt: tc.receipt, err: tc.err}
			writer := &manualEvidenceWriterFake{bound: true}
			handler := (ModerationInteractions{CommunityID: community, InstallationID: installation, ManualWarningLookup: lookup, EvidenceWriter: writer}).Handlers()[protocol.OpModerationEvidence]
			result, err := handler.Handle(context.Background(), InteractionActor{User: actor, Device: device},
				protocol.Request{CommunityID: community, Args: []any{request, "accepted warning reason", ""}})
			if err != nil || result.Status == 0 || lookup.calls != 1 || writer.calls != 0 {
				t.Fatalf("result=%+v lookup=%+v err=%v writer calls=%d; rejected evidence must not be stored", result, lookup, err, writer.calls)
			}
		})
	}
}

func TestManualEvidenceRejectsAmbiguousAcceptedRemovalAndWarningReceipts(t *testing.T) {
	community, installation, request, actor, device := interactionID(2), interactionID(3), interactionID(4), interactionID(5), interactionID(6)
	now := time.Now().Unix()
	removal := &manualRemovalLookupFake{receipt: AcceptedManualRemovalReceipt{ActionID: interactionID(7), CommunityID: community,
		GroupID: interactionID(8), ActorUserID: actor, ActorDeviceID: device, TargetSequence: 9, Revision: 10, AcceptedAt: now}}
	warning := &manualWarningLookupFake{receipt: AcceptedManualWarningReceipt{RequestID: request, ActionID: interactionID(11),
		CommunityID: community, ActorUserID: actor, ActorDeviceID: device, TargetUserID: interactionID(12), Revision: 13, AcceptedAt: now}}
	writer := &manualEvidenceWriterFake{bound: true}
	handler := (ModerationInteractions{CommunityID: community, InstallationID: installation, ManualRemovalLookup: removal,
		ManualWarningLookup: warning, EvidenceWriter: writer}).Handlers()[protocol.OpModerationEvidence]
	result, err := handler.Handle(context.Background(), InteractionActor{User: actor, Device: device},
		protocol.Request{CommunityID: community, Args: []any{request, "reason", ""}})
	if err != nil || result.Status == 0 || removal.calls != 1 || warning.calls != 1 || writer.calls != 0 {
		t.Fatalf("ambiguous evidence result=%+v removal=%+v warning=%+v writer calls=%d err=%v", result, removal, warning, writer.calls, err)
	}
}

func TestManualModerationEvidenceFailsClosedOnMissingOrMismatchedReceipt(t *testing.T) {
	community, request, actor, device := interactionID(2), interactionID(4), interactionID(5), interactionID(6)
	cases := []struct {
		name    string
		receipt AcceptedManualRemovalReceipt
		err     error
	}{
		{name: "lookup unavailable", err: errors.New("offline")},
		{name: "wrong community", receipt: AcceptedManualRemovalReceipt{ActionID: interactionID(9), CommunityID: interactionID(10), GroupID: interactionID(7), ActorUserID: actor, ActorDeviceID: device, TargetSequence: 1, Revision: 1, AcceptedAt: time.Now().Unix()}},
		{name: "wrong actor", receipt: AcceptedManualRemovalReceipt{ActionID: interactionID(9), CommunityID: community, GroupID: interactionID(7), ActorUserID: interactionID(11), ActorDeviceID: device, TargetSequence: 1, Revision: 1, AcceptedAt: time.Now().Unix()}},
		{name: "wrong device", receipt: AcceptedManualRemovalReceipt{ActionID: interactionID(9), CommunityID: community, GroupID: interactionID(7), ActorUserID: actor, ActorDeviceID: interactionID(12), TargetSequence: 1, Revision: 1, AcceptedAt: time.Now().Unix()}},
		{name: "incomplete receipt", receipt: AcceptedManualRemovalReceipt{CommunityID: community, GroupID: interactionID(7), ActorUserID: actor, ActorDeviceID: device, TargetSequence: 1, Revision: 1, AcceptedAt: time.Now().Unix()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lookup := &manualRemovalLookupFake{receipt: tc.receipt, err: tc.err}
			writer := &manualEvidenceWriterFake{bound: true}
			handler := (ModerationInteractions{CommunityID: community, InstallationID: interactionID(3), ManualRemovalLookup: lookup, EvidenceWriter: writer}).Handlers()[protocol.OpModerationEvidence]
			result, err := handler.Handle(context.Background(), InteractionActor{User: actor, Device: device}, protocol.Request{CommunityID: community, Args: []any{request, "reason", "excerpt"}})
			if err != nil || result.Status == 0 || writer.calls != 0 {
				t.Fatalf("result=%+v err=%v writer calls=%d; rejected evidence must never be stored", result, err, writer.calls)
			}
		})
	}
}

func TestManualModerationEvidenceRejectsCrossCommunityRequestAndInvalidFieldsBeforeLookup(t *testing.T) {
	community, request, actor, device := interactionID(2), interactionID(4), interactionID(5), interactionID(6)
	lookup := &manualRemovalLookupFake{}
	writer := &manualEvidenceWriterFake{bound: true}
	handler := (ModerationInteractions{CommunityID: community, InstallationID: interactionID(3), ManualRemovalLookup: lookup, EvidenceWriter: writer}).Handlers()[protocol.OpModerationEvidence]
	for _, req := range []protocol.Request{
		{CommunityID: interactionID(10), Args: []any{request, "reason", "excerpt"}},
		{CommunityID: community, Args: []any{request, "  ", "excerpt"}},
		{CommunityID: community, Args: []any{request, "reason", string(make([]byte, moderation.ExcerptMaxBytes+1))}},
		{CommunityID: community, Revision: func() *uint64 { value := uint64(1); return &value }(), Args: []any{request, "reason", "excerpt"}},
	} {
		_, _ = handler.Handle(context.Background(), InteractionActor{User: actor, Device: device}, req)
	}
	if lookup.calls != 0 || writer.calls != 0 {
		t.Fatalf("invalid request reached lookup/storage: lookup=%d writer=%d", lookup.calls, writer.calls)
	}
}

func TestManualModerationEvidenceRejectsMisboundEncryptedWriterBeforeLookup(t *testing.T) {
	community, request := interactionID(2), interactionID(4)
	lookup := &manualRemovalLookupFake{}
	writer := &manualEvidenceWriterFake{bound: false}
	handler := (ModerationInteractions{CommunityID: community, InstallationID: interactionID(3), ManualRemovalLookup: lookup, EvidenceWriter: writer}).Handlers()[protocol.OpModerationEvidence]
	result, err := handler.Handle(context.Background(), InteractionActor{User: interactionID(5), Device: interactionID(6)}, protocol.Request{CommunityID: community, Args: []any{request, "reason", "excerpt"}})
	if err != nil || result.Status == 0 || lookup.calls != 0 || writer.calls != 0 {
		t.Fatalf("result=%+v err=%v lookup=%d writer=%d", result, err, lookup.calls, writer.calls)
	}
}
