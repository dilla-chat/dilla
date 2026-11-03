package service

import (
	"context"
	"errors"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/bot/announcements"
	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/id"
)

type announcementPermsFake struct {
	bits    uint64
	err     error
	channel id.ID
}

func (f *announcementPermsFake) ChannelPermissions(_ context.Context, ch id.ID) (uint64, error) {
	f.channel = ch
	return f.bits, f.err
}

func TestAnnouncementAuthorizationUsesEffectiveSendPermission(t *testing.T) {
	channel := testID(2)
	for _, tc := range []struct {
		name string
		bits uint64
		err  error
		want bool
	}{
		{name: "allowed", bits: uint64(api.PermSendMessages), want: true},
		{name: "denied", bits: uint64(api.PermViewChannel), want: false},
		{name: "permission error", err: errors.New("permissions unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &announcementPermsFake{bits: tc.bits, err: tc.err}
			got, err := (AnnouncementAuthorization{Session: f}).CanAnnounce(context.Background(), channel)
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("error = %v", err)
				}
				return
			}
			if err != nil || got != tc.want || f.channel != channel {
				t.Fatalf("got (%v, %v), channel %v", got, err, f.channel)
			}
		})
	}
}

type announcementAuthorityFake struct {
	generation uint64
	err        error
}

func (f announcementAuthorityFake) DiscoverObserved(context.Context) (client.Discovery, client.ResponseObservation, error) {
	return client.Discovery{Generation: f.generation}, client.ResponseObservation{}, f.err
}

type announcementCoordinatorFake struct {
	calls int
	input SendInput
}

func (f *announcementCoordinatorFake) Send(_ context.Context, _ *client.Session, in SendInput) (protocol.SendReceipt, error) {
	f.calls++
	f.input = in
	return protocol.SendReceipt{}, nil
}

func TestAnnouncementSenderChecksGateOnceAndUsesResolvedGroup(t *testing.T) {
	coordinator := &announcementCoordinatorFake{}
	sender := AnnouncementSender{Coordinator: coordinator, Authority: announcementAuthorityFake{generation: 12}, Session: &client.Session{}, CommunityID: testID(1), InstallationID: testID(2)}
	predecessor := testID(6)
	req := announcements.SendRequest{OperationID: testID(3), AttemptID: testID(4), PredecessorAttemptID: &predecessor, CommunityID: testID(1), InstallationID: testID(2), GroupID: testID(5), Text: "private announcement", OriginalNow: 99}
	gateCalls := 0
	req.BeforeSend = func() bool { gateCalls++; return false }
	if _, err := sender.SendAnnouncement(context.Background(), req); err == nil {
		t.Fatal("refused gate did not fail")
	}
	if gateCalls != 1 || coordinator.calls != 0 {
		t.Fatalf("gate calls=%d sends=%d", gateCalls, coordinator.calls)
	}
	req.BeforeSend = func() bool { gateCalls++; return true }
	if _, err := sender.SendAnnouncement(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if gateCalls != 2 || coordinator.calls != 1 || coordinator.input.GroupID != req.GroupID || coordinator.input.ExpectedGeneration != 12 || coordinator.input.OriginalNow != 99 || coordinator.input.PredecessorAttemptID == nil || *coordinator.input.PredecessorAttemptID != predecessor {
		t.Fatalf("unexpected send: gate calls=%d input=%+v", gateCalls, coordinator.input)
	}
	if coordinator.input.RequestBody == nil {
		t.Fatal("missing encoded announcement body")
	}
}

func TestAnnouncementSenderRejectsWrongBinding(t *testing.T) {
	coordinator := &announcementCoordinatorFake{}
	sender := AnnouncementSender{Coordinator: coordinator, Authority: announcementAuthorityFake{generation: 1}, Session: &client.Session{}, CommunityID: testID(1), InstallationID: testID(2)}
	req := announcements.SendRequest{OperationID: testID(3), AttemptID: testID(4), CommunityID: testID(8), InstallationID: testID(2), GroupID: testID(5), Text: "secret", BeforeSend: func() bool { return true }}
	if _, err := sender.SendAnnouncement(context.Background(), req); err == nil {
		t.Fatal("wrong community binding accepted")
	}
	if coordinator.calls != 0 {
		t.Fatalf("sent %d times", coordinator.calls)
	}
}

type announcementReceiptFake struct {
	receipt                              protocol.SendReceipt
	err                                  error
	community, operation, attempt, group id.ID
}

func (f *announcementReceiptFake) BotMessageReceipt(_ context.Context, c, o, a, g id.ID) (protocol.SendReceipt, error) {
	f.community, f.operation, f.attempt, f.group = c, o, a, g
	return f.receipt, f.err
}

type announcementConfirmFake struct {
	calls     int
	operation id.ID
	body      []byte
	err       error
}

func (f *announcementConfirmFake) ConfirmSendReceipt(_ context.Context, op id.ID, body []byte) error {
	f.calls++
	f.operation = op
	f.body = append([]byte(nil), body...)
	return f.err
}

func TestAnnouncementReceiptLookupRecoversAndConfirms(t *testing.T) {
	community, operation, attempt, group := testID(1), testID(3), testID(4), testID(5)
	receipt := protocol.SendReceipt{CommunityID: community, OperationID: operation, AttemptID: attempt, GroupID: group, ExpectedGeneration: 7, CiphertextHash: make([]byte, 32), Outcome: protocol.ReceiptClosedNotAccepted}
	session, native := &announcementReceiptFake{receipt: receipt}, &announcementConfirmFake{}
	req := announcements.SendRequest{CommunityID: community, OperationID: operation, AttemptID: attempt, GroupID: group}
	got, found, err := (AnnouncementReceiptLookup{Session: session, Native: native}).LookupAnnouncement(context.Background(), req)
	if err != nil || !found || got.AttemptID != attempt || native.calls != 1 || native.operation != operation || len(native.body) == 0 {
		t.Fatalf("receipt recovery: found=%v err=%v confirms=%d", found, err, native.calls)
	}
	if session.community != community || session.operation != operation || session.attempt != attempt || session.group != group {
		t.Fatal("receipt lookup used the wrong binding")
	}
}

var _ announcements.Authorization = AnnouncementAuthorization{}
var _ announcements.Sender = AnnouncementSender{}
var _ announcements.ReceiptLookup = AnnouncementReceiptLookup{}
