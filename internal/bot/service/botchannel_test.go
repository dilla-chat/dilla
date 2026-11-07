package service

import (
	"context"
	"errors"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/id"
)

// marks is a fake feed: the community's Bot channels and whether a snapshot was applied.
type marks struct {
	set   map[id.ID]bool
	known bool
}

func (m *marks) Marked() (map[id.ID]bool, bool) { return m.set, m.known }

func gateFor(t *testing.T, community id.ID, m *marks) {
	t.Helper()
	SetPostGate(community, m)
	t.Cleanup(func() { SetPostGate(community, nil) })
}

// Task 17, carry 2: the gate is at the send layer. A post to an unmarked channel's group is refused before anything is
// prepared or uploaded, whichever feature (or webhook) asked.
func TestSendToUnmarkedChannelGroupIsRefusedAtTheSendLayer(t *testing.T) {
	c, n, s, in := sendFixture()
	gateFor(t, sendBinding().Community, &marks{set: map[id.ID]bool{sendID(99): true}, known: true})
	_, err := c.send(context.Background(), s, in)
	if !errors.Is(err, ErrNeedsBotChannel) {
		t.Fatalf("send to an unmarked channel's group: %v, want needs_bot_channel", err)
	}
	if n.prepareCalls != 0 || s.posts != 0 {
		t.Fatalf("an unmarked channel must prepare and post nothing: prepare=%d posts=%d", n.prepareCalls, s.posts)
	}
}

func TestSendToMarkedBotChannelGroupPostsAndMarksNotYetKnownRefuses(t *testing.T) {
	c, n, s, in := sendFixture()
	// The group's channel is sendID(5).
	m := &marks{set: map[id.ID]bool{sendID(5): true}, known: true}
	gateFor(t, sendBinding().Community, m)
	if _, err := c.send(context.Background(), s, in); errors.Is(err, ErrNeedsBotChannel) || errors.Is(err, ErrBotChannelsUnknown) {
		t.Fatalf("a marked Bot channel must pass the gate: %v", err)
	}
	if n.prepareCalls != 1 {
		t.Fatalf("marked channel prepared %d times, want 1", n.prepareCalls)
	}
	// Before the feed's first snapshot nothing is known, so nothing is posted.
	c2, n2, s2, in2 := sendFixture()
	m.known = false
	_, err := c2.send(context.Background(), s2, in2)
	if !errors.Is(err, ErrBotChannelsUnknown) || n2.prepareCalls != 0 {
		t.Fatalf("marks unknown: err=%v prepare=%d", err, n2.prepareCalls)
	}
}

// A resolve-only settlement of an old attempt posts nothing new, so it is not gated.
func TestResolvePreparedIsNotGated(t *testing.T) {
	c, _, s, in := sendFixture()
	gateFor(t, sendBinding().Community, &marks{known: true})
	_, err := c.ResolvePrepared(context.Background(), s, in)
	if errors.Is(err, ErrNeedsBotChannel) {
		t.Fatalf("resolve-only must not be gated: %v", err)
	}
}

type fakeChannelLister struct{ channels []client.Channel }

func (f fakeChannelLister) Channels(context.Context, id.ID) ([]client.Channel, error) {
	return f.channels, nil
}

type fakeInspector struct{ group native.GroupInfo }

func (f fakeInspector) InspectGroup(context.Context, id.ID) (native.GroupInfo, error) {
	return f.group, nil
}

// Review Focus 5: a feature whose channel is not (or no longer) a Bot channel posts nothing and flags
// needs_bot_channel, for welcome, announcements, polls and role panels alike (one destination resolver).
func TestFeatureWithUnmarkedChannelPostsNothingAndFlags(t *testing.T) {
	community, channel, group := sendID(2), sendID(5), sendID(4)
	tg := group
	lister := fakeChannelLister{channels: []client.Channel{{ID: channel, CommunityID: community, Kind: 0, TextGroupID: &tg}}}
	inspector := fakeInspector{group: native.GroupInfo{ID: group, Kind: 0, Community: &community, Target: channel, State: 2}}
	resolver := WelcomeDestinationResolver{Session: lister, Native: inspector, Feature: "welcome"}
	m := &marks{set: map[id.ID]bool{}, known: true}
	gateFor(t, community, m)
	t.Cleanup(func() { ClearNeedsBotChannel(community, "welcome") })

	got, err := resolver.ResolveTextGroup(context.Background(), community, channel)
	if !errors.Is(err, ErrNeedsBotChannel) || !got.IsZero() {
		t.Fatalf("unmarked channel: group=%v err=%v, want needs_bot_channel and no group", got, err)
	}
	flags := NeedsBotChannelFlags(community)
	if len(flags) != 1 || flags[0].Feature != "welcome" || flags[0].Channel != channel {
		t.Fatalf("flags %+v, want welcome flagged for the channel", flags)
	}

	// Marking the channel lets it post, and clears the flag.
	m.set[channel] = true
	got, err = resolver.ResolveTextGroup(context.Background(), community, channel)
	if err != nil || got != group {
		t.Fatalf("marked channel: group=%v err=%v", got, err)
	}
	if flags := NeedsBotChannelFlags(community); len(flags) != 0 {
		t.Fatalf("flags %+v after the channel was marked, want none", flags)
	}
}
