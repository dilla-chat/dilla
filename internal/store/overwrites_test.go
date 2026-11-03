package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// I2 (fix wave): a user's channel overwrites end with their membership. DeleteUserOverwrites
// removes the user's kind-1 overwrites in every channel of one community and nothing else: not a
// role overwrite with the same id, not another user's, not another community's.
func TestDeleteUserOverwritesIsScopedToOneUserAndCommunity(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			a, b := seedCommunity(ctx, t, repo), seedCommunity(ctx, t, repo)
			a1, a2, b1 := channelIn(a, 0, 0, 0, "a1", 0), channelIn(a, 0, 0, 0, "a2", 1), channelIn(b, 0, 0, 0, "b1", 0)
			for _, ch := range []store.ChannelRow{a1, a2, b1} {
				if err := repo.CreateChannel(ctx, ch); err != nil {
					t.Fatalf("CreateChannel: %v", err)
				}
			}
			u, v := id.New(), id.New()
			for _, o := range []store.OverwriteRow{
				{ChannelID: a1.ID, TargetKind: 1, TargetID: u, Allow: 1},
				{ChannelID: a2.ID, TargetKind: 1, TargetID: u, Deny: 2},
				{ChannelID: a1.ID, TargetKind: 0, TargetID: u, Allow: 4}, // a role row with the same id
				{ChannelID: a1.ID, TargetKind: 1, TargetID: v, Allow: 1},
				{ChannelID: b1.ID, TargetKind: 1, TargetID: u, Allow: 1},
			} {
				if err := repo.PutOverwrite(ctx, o); err != nil {
					t.Fatalf("PutOverwrite: %v", err)
				}
			}
			n, err := repo.DeleteUserOverwrites(ctx, a, u)
			if err != nil || n != 2 {
				t.Fatalf("DeleteUserOverwrites = %d, %v; want 2", n, err)
			}
			for _, c := range []struct {
				ch   id.ID
				want int
			}{{a1.ID, 2}, {a2.ID, 0}, {b1.ID, 1}} {
				if got, err := repo.ListOverwrites(ctx, c.ch); err != nil || len(got) != c.want {
					t.Fatalf("ListOverwrites(%s) = %+v, %v; want %d rows", c.ch, got, err, c.want)
				}
			}
		})
	}
}

// Plan 2 task 3: channel_overwrites and the role delete, on both engines.
func TestOverwriteRoundTrip(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			cid := seedCommunity(ctx, t, repo)
			ch := channelIn(cid, 0, 0, 0, "general", 0)
			if err := repo.CreateChannel(ctx, ch); err != nil {
				t.Fatalf("CreateChannel: %v", err)
			}
			role, user := id.New(), id.New()
			// Bit 61 is the highest a signed BIGINT carries without the sign bit;
			// the permission vocabulary stops well below it, but the column must
			// carry any value the resolver could mask off.
			const high = uint64(1) << 61
			for _, o := range []store.OverwriteRow{
				{ChannelID: ch.ID, TargetKind: 1, TargetID: user, Allow: 0, Deny: 2},
				{ChannelID: ch.ID, TargetKind: 0, TargetID: role, Allow: 1 | high, Deny: 0},
			} {
				if err := repo.PutOverwrite(ctx, o); err != nil {
					t.Fatalf("PutOverwrite: %v", err)
				}
			}
			got, err := repo.ListOverwrites(ctx, ch.ID)
			if err != nil {
				t.Fatalf("ListOverwrites: %v", err)
			}
			// Ordered by (target_kind, target_id): the role row first.
			if len(got) != 2 || got[0].TargetKind != 0 || got[0].TargetID != role ||
				got[0].Allow != 1|high || got[0].ChannelID != ch.ID ||
				got[1].TargetKind != 1 || got[1].TargetID != user || got[1].Deny != 2 {
				t.Fatalf("ListOverwrites = %+v", got)
			}

			// A second put on the same key replaces allow and deny.
			if err := repo.PutOverwrite(ctx, store.OverwriteRow{
				ChannelID: ch.ID, TargetKind: 1, TargetID: user, Allow: 8, Deny: 0,
			}); err != nil {
				t.Fatalf("PutOverwrite (upsert): %v", err)
			}
			got, _ = repo.ListOverwrites(ctx, ch.ID)
			if len(got) != 2 || got[1].Allow != 8 || got[1].Deny != 0 {
				t.Fatalf("after upsert = %+v", got)
			}

			// The same target id under the other kind is a different row.
			if err := repo.DeleteOverwrite(ctx, ch.ID, 0, user); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("DeleteOverwrite(wrong kind) = %v, want ErrNotFound", err)
			}
			if err := repo.DeleteOverwrite(ctx, ch.ID, 1, user); err != nil {
				t.Fatalf("DeleteOverwrite: %v", err)
			}
			if err := repo.DeleteOverwrite(ctx, ch.ID, 1, user); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("second DeleteOverwrite = %v, want ErrNotFound", err)
			}
			got, _ = repo.ListOverwrites(ctx, ch.ID)
			if len(got) != 1 || got[0].TargetID != role {
				t.Fatalf("after delete = %+v", got)
			}

			// A channel with no overwrites lists an empty slice, not an error.
			other := channelIn(cid, 0, 0, 0, "other", 1)
			if err := repo.CreateChannel(ctx, other); err != nil {
				t.Fatalf("CreateChannel: %v", err)
			}
			if got, err := repo.ListOverwrites(ctx, other.ID); err != nil || len(got) != 0 {
				t.Fatalf("ListOverwrites(empty) = %+v, %v", got, err)
			}
		})
	}
}

func TestTheOverwritesTableRefusesIllegalRows(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			cid := seedCommunity(ctx, t, repo)
			ch := channelIn(cid, 0, 0, 0, "general", 0)
			if err := repo.CreateChannel(ctx, ch); err != nil {
				t.Fatalf("CreateChannel: %v", err)
			}
			for name, o := range map[string]store.OverwriteRow{
				"a target kind outside 0..1": {ChannelID: ch.ID, TargetKind: 2, TargetID: id.New()},
				"a channel that does not exist": {
					ChannelID: id.New(), TargetKind: 1, TargetID: id.New(),
				},
			} {
				if err := repo.PutOverwrite(ctx, o); err == nil {
					t.Errorf("%s was accepted", name)
				}
			}
		})
	}
}

func TestDeleteRoleDropsItsGrants(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			cid := seedCommunity(ctx, t, repo)
			user := seedUser(ctx, t, repo).ID
			if err := repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: user, Joined: 1}); err != nil {
				t.Fatalf("PutMember: %v", err)
			}
			role := store.RoleRow{ID: id.New(), CommunityID: cid, Name: "mod", Position: 5, Created: 1}
			if err := repo.PutRole(ctx, role); err != nil {
				t.Fatalf("PutRole: %v", err)
			}
			if err := repo.PutMemberRole(ctx, cid, user, role.ID); err != nil {
				t.Fatalf("PutMemberRole: %v", err)
			}

			// The community is part of the key: a role id named under another
			// community is not found and not deleted.
			if err := repo.DeleteRole(ctx, seedCommunity(ctx, t, repo), role.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("DeleteRole(other community) = %v, want ErrNotFound", err)
			}
			if _, err := repo.GetRole(ctx, role.ID); err != nil {
				t.Fatalf("the role did not survive a delete under another community: %v", err)
			}

			if err := repo.DeleteRole(ctx, cid, role.ID); err != nil {
				t.Fatalf("DeleteRole: %v", err)
			}
			if _, err := repo.GetRole(ctx, role.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetRole after delete = %v, want ErrNotFound", err)
			}
			held, err := repo.ListMemberRoles(ctx, cid, user)
			if err != nil {
				t.Fatalf("ListMemberRoles: %v", err)
			}
			if slices.Contains(held, role.ID) {
				t.Fatal("the grant survived the role's deletion")
			}
			if err := repo.DeleteRole(ctx, cid, role.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("second DeleteRole = %v, want ErrNotFound", err)
			}
		})
	}
}
