package service

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/community/welcome"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/id"
)

type missingInspector struct{}

func (missingInspector) InspectGroup(context.Context, id.ID) (native.GroupInfo, error) {
	return native.GroupInfo{}, errors.New("group not found")
}

// The welcome destination defers (welcome.ErrRetryLater) while the Bot has not joined the channel's group, and while
// that group has pending proposals or a pending commit: a post then would be refused and its attempt closed. Other
// features keep today's behaviour.
func TestWelcomeDestinationDefersWhileGroupIsBusy(t *testing.T) {
	community, channel, group := sendID(2), sendID(5), sendID(4)
	tg := group
	lister := fakeChannelLister{channels: []client.Channel{{ID: channel, CommunityID: community, Kind: 0, TextGroupID: &tg, Name: "secret-channel-name"}}}
	settled := native.GroupInfo{ID: group, Kind: 0, Community: &community, Target: channel, State: 2}
	gateFor(t, community, &marks{set: map[id.ID]bool{channel: true}, known: true})

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	proposals := settled
	proposals.ProposalsPending = 2
	commit := settled
	commit.PendingCommit = true
	joining := settled
	joining.State = 1
	for name, inspector := range map[string]interface {
		InspectGroup(context.Context, id.ID) (native.GroupInfo, error)
	}{
		"proposals pending": fakeInspector{group: proposals},
		"commit pending":    fakeInspector{group: commit},
		"not in group":      missingInspector{},
		"group joining":     fakeInspector{group: joining},
	} {
		welcomeResolver := WelcomeDestinationResolver{Session: lister, Native: inspector, Feature: "welcome", DeferWhileBusy: true}
		got, err := welcomeResolver.ResolveTextGroup(context.Background(), community, channel)
		if !errors.Is(err, welcome.ErrRetryLater) || !got.IsZero() {
			t.Fatalf("%s: group=%v err=%v, want ErrRetryLater", name, got, err)
		}
		other := WelcomeDestinationResolver{Session: lister, Native: inspector, Feature: "announcements"}
		if _, err := other.ResolveTextGroup(context.Background(), community, channel); errors.Is(err, welcome.ErrRetryLater) {
			t.Fatalf("%s: another feature's resolver deferred: %v", name, err)
		}
	}
	resolver := WelcomeDestinationResolver{Session: lister, Native: fakeInspector{group: settled}, Feature: "welcome", DeferWhileBusy: true}
	if got, err := resolver.ResolveTextGroup(context.Background(), community, channel); err != nil || got != group {
		t.Fatalf("settled group: group=%v err=%v", got, err)
	}
	out := logs.String()
	if !strings.Contains(out, "reason=group_busy") || strings.Contains(out, "secret-channel-name") {
		t.Fatalf("refusal log %q: want reason=group_busy, ids only", out)
	}
}
