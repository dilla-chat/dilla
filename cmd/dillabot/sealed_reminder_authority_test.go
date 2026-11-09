package main

import (
	"context"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/intake"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/bot/service"
	"github.com/dilla-chat/dilla/internal/id"
)

type reminderSourceFake struct{ channel id.ID }

func (f reminderSourceFake) ReminderMember(context.Context, id.ID) (bool, error) { return true, nil }
func (f reminderSourceFake) ReminderSource(_ context.Context, _ id.ID, channel id.ID) (bool, error) {
	return channel == f.channel, nil
}

// Task 21b fix round 1, review I1: the sealed reminder handlers keep the source-channel check of the pass's authority
// (membership from the feed, the source channel's view/read-history as before), so op 180 with a source works and 181/93
// keep "#channel" and jump-to-source. Without it every source would be refused.
func TestSealedRemindersKeepTheSourceChannelCheck(t *testing.T) {
	source := id.New()
	authz := intake.NewAuthz(id.New())
	rem := sealedReminders(service.ReminderInteractions{Authority: reminderSourceFake{channel: source}}, authz)
	checker, ok := rem.Authority.(interface {
		ReminderSource(context.Context, id.ID, id.ID) (bool, error)
	})
	if !ok {
		t.Fatal("the sealed reminder authority has no source check")
	}
	if allowed, err := checker.ReminderSource(context.Background(), id.New(), source); err != nil || !allowed {
		t.Fatalf("a visible source = %v %v", allowed, err)
	}
	if allowed, _ := checker.ReminderSource(context.Background(), id.New(), id.New()); allowed {
		t.Fatal("another channel passed the source check")
	}
	// Membership still comes from the feed, not from the pass's authority.
	if member, _ := rem.Authority.ReminderMember(context.Background(), id.New()); member {
		t.Fatal("membership did not come from the feed")
	}
	// A pass whose authority has no source check fails closed.
	bare := sealedReminders(service.ReminderInteractions{}, authz)
	if allowed, _ := bare.Authority.(interface {
		ReminderSource(context.Context, id.ID, id.ID) (bool, error)
	}).ReminderSource(context.Background(), id.New(), source); allowed {
		t.Fatal("no source authority, yet a source passed")
	}
	_ = protocol.OpReminderCreate
}
