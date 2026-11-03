package capability

import (
	"context"
	"errors"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

type sourceFunc func(context.Context, id.ID) (store.BotInstallationRow, error)

func (f sourceFunc) GetActiveBotInstallationByCommunity(ctx context.Context, community id.ID) (store.BotInstallationRow, error) {
	return f(ctx, community)
}

func TestEnabledMatchesLiveInstallationPredicate(t *testing.T) {
	community := id.New()
	for _, tc := range []struct {
		name    string
		state   uint8
		removed bool
		want    bool
	}{
		{"pending", store.BotInstallationPending, false, true},
		{"enrolling", store.BotInstallationEnrolling, false, true},
		{"joining", store.BotInstallationJoining, false, true},
		{"ready", store.BotInstallationReady, false, true},
		{"error", store.BotInstallationError, false, true},
		{"disabled", store.BotInstallationDisabled, false, false},
		{"removed", store.BotInstallationReady, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := New(sourceFunc(func(context.Context, id.ID) (store.BotInstallationRow, error) {
				row := store.BotInstallationRow{CommunityID: community, State: tc.state}
				if tc.removed {
					removedAt := int64(10)
					row.RemovedAt = &removedAt
				}
				return row, nil
			}))
			got, err := policy.Enabled(context.Background(), community)
			if err != nil || got != tc.want {
				t.Fatalf("Enabled() = %v, %v; want %v, nil", got, err, tc.want)
			}
		})
	}
}

func TestRequireCommunityBoundaryAndFailClosedErrors(t *testing.T) {
	community := id.New()
	state := store.BotInstallationPending
	policy := New(sourceFunc(func(context.Context, id.ID) (store.BotInstallationRow, error) {
		return store.BotInstallationRow{CommunityID: community, State: state}, nil
	}))
	if err := policy.RequireCommunity(context.Background(), 0, &community); err == nil {
		t.Fatal("unnegotiated wire version passed for an enabled community")
	} else {
		var upgrade *UpgradeRequired
		if !errors.As(err, &upgrade) || err.Error() != "wire version 3 is required" {
			t.Fatalf("refusal = %v, want typed upgrade refusal without resource identifiers", err)
		}
	}
	if err := policy.RequireCommunity(context.Background(), 3, &community); err != nil {
		t.Fatalf("wire 3 refused: %v", err)
	}
	state = store.BotInstallationDisabled
	if err := policy.RequireCommunity(context.Background(), 2, &community); err != nil {
		t.Fatalf("wire 2 stayed refused after committed disable: %v", err)
	}
	absent := New(sourceFunc(func(context.Context, id.ID) (store.BotInstallationRow, error) {
		return store.BotInstallationRow{}, store.ErrNotFound
	}))
	if err := absent.RequireCommunity(context.Background(), 2, &community); err != nil {
		t.Fatalf("wire 2 refused for a community without an installation: %v", err)
	}
	if err := policy.RequireCommunity(context.Background(), 0, nil); err != nil {
		t.Fatalf("unscoped request refused: %v", err)
	}
	missing := New(nil)
	if err := missing.RequireCommunity(context.Background(), 3, &community); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("missing source error = %v, want ErrConfiguration", err)
	}
	failure := errors.New("database unavailable")
	broken := New(sourceFunc(func(context.Context, id.ID) (store.BotInstallationRow, error) {
		return store.BotInstallationRow{}, failure
	}))
	if err := broken.RequireCommunity(context.Background(), 2, &community); !errors.Is(err, failure) {
		t.Fatalf("source failure = %v, want wrapped operational error", err)
	}
}

func TestInteractionGroupsRequireWireThreeWithoutCommunity(t *testing.T) {
	policy := New(nil)
	if err := policy.RequireGroup(context.Background(), 2, 3, nil); err == nil {
		t.Fatal("wire 2 passed for an unscoped interaction group")
	}
	if err := policy.RequireGroup(context.Background(), 3, 3, nil); err != nil {
		t.Fatalf("wire 3 refused for an interaction group: %v", err)
	}
	if err := policy.RequireGroup(context.Background(), 2, 2, nil); err != nil {
		t.Fatalf("wire 2 refused for a pairing group: %v", err)
	}
}
