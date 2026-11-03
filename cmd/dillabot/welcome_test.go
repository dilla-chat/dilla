//go:build linux

package main

import (
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/id"
)

func TestConfiguredWelcomeChannelRequiresCurrentTextGroup(t *testing.T) {
	community, channel, group := id.New(), id.New(), id.New()
	rows := []client.Channel{{ID: channel, CommunityID: community, Kind: 0, Name: "general", TextGroupID: &group}}
	got, err := configuredWelcomeChannel(rows, community, channel)
	if err != nil || got.Name != "general" || *got.TextGroupID != group {
		t.Fatalf("valid destination = %+v, %v", got, err)
	}
	for _, bad := range []client.Channel{
		{ID: channel, CommunityID: id.New(), Kind: 0, Name: "foreign", TextGroupID: &group},
		{ID: channel, CommunityID: community, Kind: 1, Name: "voice", TextGroupID: &group},
		{ID: channel, CommunityID: community, Kind: 0, Name: "missing"},
	} {
		if _, err := configuredWelcomeChannel([]client.Channel{bad}, community, channel); err == nil {
			t.Fatalf("accepted invalid destination %+v", bad)
		}
	}
	if _, err := configuredWelcomeChannel(nil, community, channel); err == nil {
		t.Fatal("accepted missing destination")
	}
}
