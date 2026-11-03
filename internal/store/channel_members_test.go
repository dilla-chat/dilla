package store_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// dmChannel is a DM (kind 3) or group DM (kind 4) row: no community, e2ee,
// private.
func dmChannel(kind uint8, created int64) store.ChannelRow {
	return store.ChannelRow{
		ID: id.New(), Kind: kind, Mode: 0, Visibility: 0, SettingsJSON: []byte(`{}`),
		HostPolicyVersion: 1, Created: created,
	}
}

// Plan 2 task 6: the channel_members table (00008_channel_members.sql) and
// P2-D11's ListChannelsForUser, on both engines.
func TestChannelMembersRoundTrip(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			a, b, c := seedUser(ctx, t, repo).ID, seedUser(ctx, t, repo).ID, seedUser(ctx, t, repo).ID

			dm := dmChannel(3, 100)
			group := dmChannel(4, 200)
			for _, ch := range []store.ChannelRow{dm, group} {
				if err := repo.CreateChannel(ctx, ch); err != nil {
					t.Fatalf("CreateChannel: %v", err)
				}
			}
			for _, m := range []struct{ ch, u id.ID }{
				{dm.ID, a}, {dm.ID, b}, {group.ID, a}, {group.ID, b}, {group.ID, c},
			} {
				if err := repo.PutChannelMember(ctx, m.ch, m.u, 100); err != nil {
					t.Fatalf("PutChannelMember: %v", err)
				}
			}
			// A second put is a no-op, not a conflict.
			if err := repo.PutChannelMember(ctx, dm.ID, a, 999); err != nil {
				t.Fatalf("PutChannelMember (again): %v", err)
			}

			got, err := repo.ListChannelMembers(ctx, group.ID)
			if err != nil {
				t.Fatalf("ListChannelMembers: %v", err)
			}
			want := []id.ID{a, b, c}
			slices.SortFunc(want, func(x, y id.ID) int { return bytes.Compare(x[:], y[:]) })
			if !slices.Equal(got, want) {
				t.Fatalf("ListChannelMembers = %x, want %x (ordered by user id)", got, want)
			}

			// Newest first.
			mine, err := repo.ListChannelsForUser(ctx, a)
			if err != nil {
				t.Fatalf("ListChannelsForUser: %v", err)
			}
			if len(mine) != 2 || mine[0].ID != group.ID || mine[1].ID != dm.ID ||
				mine[0].CommunityID != nil || mine[0].Kind != 4 || mine[1].Kind != 3 {
				t.Fatalf("ListChannelsForUser(a) = %+v", mine)
			}
			if only, err := repo.ListChannelsForUser(ctx, c); err != nil || len(only) != 1 || only[0].ID != group.ID {
				t.Fatalf("ListChannelsForUser(c) = %+v, %v", only, err)
			}

			if err := repo.DeleteChannelMember(ctx, group.ID, c); err != nil {
				t.Fatalf("DeleteChannelMember: %v", err)
			}
			if err := repo.DeleteChannelMember(ctx, group.ID, c); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("second DeleteChannelMember = %v, want ErrNotFound", err)
			}
			if only, err := repo.ListChannelsForUser(ctx, c); err != nil || len(only) != 0 {
				t.Fatalf("ListChannelsForUser(c) after removal = %+v, %v", only, err)
			}

			// A deleted DM leaves the listing; its members stay with the tombstone.
			if err := repo.DeleteChannel(ctx, dm.ID, 300); err != nil {
				t.Fatalf("DeleteChannel: %v", err)
			}
			if mine, err := repo.ListChannelsForUser(ctx, a); err != nil || len(mine) != 1 || mine[0].ID != group.ID {
				t.Fatalf("ListChannelsForUser(a) after the DM is deleted = %+v, %v", mine, err)
			}

			// A community channel's materialised members are never listed as a DM.
			cid := seedCommunity(ctx, t, repo)
			text := channelIn(cid, 0, 0, 0, "general", 0)
			if err := repo.CreateChannel(ctx, text); err != nil {
				t.Fatalf("CreateChannel(text): %v", err)
			}
			if err := repo.PutChannelMember(ctx, text.ID, b, 400); err != nil {
				t.Fatalf("PutChannelMember(text): %v", err)
			}
			if mine, err := repo.ListChannelsForUser(ctx, b); err != nil || len(mine) != 1 || mine[0].ID != group.ID {
				t.Fatalf("ListChannelsForUser(b) = %+v, %v; a community channel is not a DM", mine, err)
			}

			// Empty is a slice, not an error.
			if got, err := repo.ListChannelMembers(ctx, id.New()); err != nil || len(got) != 0 {
				t.Fatalf("ListChannelMembers(unknown) = %v, %v", got, err)
			}
		})
	}
}

// C3 (fix wave): a kick, ban or leave removes the user's channel_members rows for every channel
// of that community in the same transaction as the membership; other communities' channels, DMs
// and other users are untouched.
func TestDeleteCommunityChannelMembersIsScopedToOneUserAndCommunity(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			u, v := seedUser(ctx, t, repo).ID, seedUser(ctx, t, repo).ID
			a, b := seedCommunity(ctx, t, repo), seedCommunity(ctx, t, repo)
			a1, a2, b1 := channelIn(a, 0, 0, 0, "a1", 0), channelIn(a, 1, 0, 0, "a2", 1), channelIn(b, 0, 0, 0, "b1", 0)
			dm := dmChannel(3, 100)
			for _, ch := range []store.ChannelRow{a1, a2, b1, dm} {
				if err := repo.CreateChannel(ctx, ch); err != nil {
					t.Fatalf("CreateChannel: %v", err)
				}
			}
			for _, m := range []struct{ ch, u id.ID }{
				{a1.ID, u}, {a2.ID, u}, {b1.ID, u}, {dm.ID, u}, {a1.ID, v},
			} {
				if err := repo.PutChannelMember(ctx, m.ch, m.u, 100); err != nil {
					t.Fatalf("PutChannelMember: %v", err)
				}
			}
			n, err := repo.DeleteCommunityChannelMembers(ctx, a, u)
			if err != nil || n != 2 {
				t.Fatalf("DeleteCommunityChannelMembers = %d, %v; want 2", n, err)
			}
			for _, c := range []struct {
				ch   id.ID
				want []id.ID
			}{{a1.ID, []id.ID{v}}, {a2.ID, []id.ID{}}, {b1.ID, []id.ID{u}}, {dm.ID, []id.ID{u}}} {
				got, err := repo.ListChannelMembers(ctx, c.ch)
				if err != nil || len(got) != len(c.want) || (len(got) > 0 && !slices.Equal(got, c.want)) {
					t.Fatalf("ListChannelMembers(%s) = %x, %v; want %x", c.ch, got, err, c.want)
				}
			}
			if n, err := repo.DeleteCommunityChannelMembers(ctx, a, u); err != nil || n != 0 {
				t.Fatalf("a second DeleteCommunityChannelMembers = %d, %v; want 0", n, err)
			}
		})
	}
}
