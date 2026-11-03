package store_test

import (
	"context"
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func TestReactionRoleGrantSourcesAreIndependentAndRetainEffectiveRole(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			cid := seedCommunity(ctx, t, repo)
			user := seedUser(ctx, t, repo).ID
			if err := repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: user, Joined: 1}); err != nil {
				t.Fatal(err)
			}
			role := store.RoleRow{ID: id.New(), CommunityID: cid, Name: "panel role", Created: 1}
			if err := repo.PutRole(ctx, role); err != nil {
				t.Fatal(err)
			}
			bot := seedUser(ctx, t, repo).ID
			installation := id.New()
			if err := repo.CreateBotInstallation(ctx, store.BotInstallationRow{ID: installation, CommunityID: cid, State: store.BotInstallationPending, Revision: 1, ApprovedBy: bot, ApprovedAt: 1, Created: 1, Updated: 1}); err != nil {
				t.Fatal(err)
			}
			channel := channelIn(cid, 0, 1, 2, "panels", 1)
			if err := repo.CreateChannel(ctx, channel); err != nil {
				t.Fatal(err)
			}
			panelGrant := func(seq uint64) store.ReactionRoleGrant {
				panel := store.RolePanelRow{CommunityID: cid, ChannelID: channel.ID, MessageSeq: seq, AuthorID: bot, InstallationID: installation}
				if err := repo.Tx(ctx, func(tx store.Repository) error {
					return tx.SetRolePanel(ctx, panel, []store.RolePanelOptionRow{{Emoji: "🎨", RoleID: role.ID}})
				}); err != nil {
					t.Fatalf("register test panel: %v", err)
				}
				return store.ReactionRoleGrant{CommunityID: cid, UserID: user, RoleID: role.ID, ChannelID: channel.ID, MessageSeq: seq, Reaction: "🎨"}
			}
			first := panelGrant(17)
			second := first
			second.MessageSeq = 18
			second = panelGrant(second.MessageSeq)
			for _, grant := range []store.ReactionRoleGrant{first, second} {
				if err := repo.AddReactionRoleGrant(ctx, grant); err != nil {
					t.Fatalf("add reaction grant: %v", err)
				}
			}
			if err := repo.AddReactionRoleGrant(ctx, first); err != nil {
				t.Fatalf("repeat add: %v", err)
			}
			tooLarge := first
			tooLarge.MessageSeq = uint64(math.MaxInt64) + 1
			if err := repo.AddReactionRoleGrant(ctx, tooLarge); err == nil {
				t.Fatal("reaction grant above MaxInt64 was accepted")
			}
			if err := repo.RemoveReactionRoleGrant(ctx, tooLarge); err == nil {
				t.Fatal("reaction grant removal above MaxInt64 was accepted")
			}
			maxGrant := panelGrant(uint64(math.MaxInt64))
			if err := repo.AddReactionRoleGrant(ctx, maxGrant); err != nil {
				t.Fatalf("add reaction grant at MaxInt64: %v", err)
			}
			if err := repo.RemoveReactionRoleGrant(ctx, maxGrant); err != nil {
				t.Fatalf("remove reaction grant at MaxInt64: %v", err)
			}
			if err := repo.RemoveReactionRoleGrant(ctx, first); err != nil {
				t.Fatalf("remove first grant: %v", err)
			}
			held, err := repo.ListMemberRoles(ctx, cid, user)
			if err != nil || !slices.Contains(held, role.ID) {
				t.Fatalf("role after removing one panel = %v, %v; want retained", held, err)
			}
			if err := repo.PutMemberRole(ctx, cid, user, role.ID); err != nil {
				t.Fatalf("add manual grant: %v", err)
			}
			if err := repo.RemoveReactionRoleGrant(ctx, second); err != nil {
				t.Fatalf("remove second grant: %v", err)
			}
			held, err = repo.ListMemberRoles(ctx, cid, user)
			if err != nil || !slices.Contains(held, role.ID) {
				t.Fatalf("role after reaction grants removed = %v, %v; want manual role retained", held, err)
			}
			if err := repo.DeleteMemberRole(ctx, cid, user, role.ID); err != nil {
				t.Fatal(err)
			}
			held, err = repo.ListMemberRoles(ctx, cid, user)
			if err != nil || slices.Contains(held, role.ID) {
				t.Fatalf("role after final source removed = %v, %v; want absent", held, err)
			}
			third := panelGrant(19)
			if err := repo.AddReactionRoleGrant(ctx, third); err != nil {
				t.Fatalf("add reaction grant alongside manual: %v", err)
			}
			// Replacing the panel option revokes its source atomically and must
			// retain the effective role only when another grant source remains.
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				return tx.SetRolePanel(ctx, store.RolePanelRow{CommunityID: cid, ChannelID: channel.ID, MessageSeq: 19, AuthorID: bot, InstallationID: installation}, []store.RolePanelOptionRow{{Emoji: "🎨", RoleID: role.ID}})
			}); err != nil {
				t.Fatal(err)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				return tx.SetRolePanel(ctx, store.RolePanelRow{CommunityID: cid, ChannelID: channel.ID, MessageSeq: 19, AuthorID: bot, InstallationID: installation}, []store.RolePanelOptionRow{{Emoji: "🧩", RoleID: role.ID}})
			}); err != nil {
				t.Fatal(err)
			}
			if err := repo.RemoveReactionRoleGrant(ctx, third); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("replaced option source = %v, want ErrNotFound", err)
			}
			held, err = repo.ListMemberRoles(ctx, cid, user)
			if err != nil || slices.Contains(held, role.ID) {
				t.Fatalf("role after replacing final source = %v, %v; want absent", held, err)
			}
			third = panelGrant(19)
			if err := repo.AddReactionRoleGrant(ctx, third); err != nil {
				t.Fatal(err)
			}
			if err := repo.PutMemberRole(ctx, cid, user, role.ID); err != nil {
				t.Fatalf("add manual grant alongside reaction: %v", err)
			}
			if err := repo.DeleteMemberRole(ctx, cid, user, role.ID); err != nil {
				t.Fatalf("remove manual grant: %v", err)
			}
			held, err = repo.ListMemberRoles(ctx, cid, user)
			if err != nil || !slices.Contains(held, role.ID) {
				t.Fatalf("role after manual removal = %v, %v; want reaction role retained", held, err)
			}
			if err := repo.RemoveReactionRoleGrant(ctx, third); err != nil {
				t.Fatalf("remove last reaction grant: %v", err)
			}
			if err := repo.RemoveReactionRoleGrant(ctx, second); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("remove absent source = %v, want ErrNotFound", err)
			}
			fourth := panelGrant(20)
			if err := repo.AddReactionRoleGrant(ctx, fourth); err != nil {
				t.Fatal(err)
			}
			affected, err := repo.(store.ReactionRoleGrantSources).RemoveReactionRolePanelGrants(ctx, cid, channel.ID, fourth.MessageSeq)
			if err != nil || !slices.Contains(affected, user) {
				t.Fatalf("remove panel sources = %v, %v; want affected member", affected, err)
			}
			held, err = repo.ListMemberRoles(ctx, cid, user)
			if err != nil || slices.Contains(held, role.ID) {
				t.Fatalf("role after panel removal = %v, %v; want absent", held, err)
			}
		})
	}
}
