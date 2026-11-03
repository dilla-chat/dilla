package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func TestBotJoinRoleGrantIsEpisodeBoundIdempotentAndSourceSeparated(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			community := seedCommunity(ctx, t, repo)
			owner := seedUser(ctx, t, repo).ID
			member := seedUser(ctx, t, repo).ID
			role := store.RoleRow{ID: id.New(), CommunityID: community, Name: "welcome", Created: 1}
			if err := repo.PutRole(ctx, role); err != nil {
				t.Fatal(err)
			}
			installation := store.BotInstallationRow{ID: id.New(), CommunityID: community, State: store.BotInstallationReady, Revision: 1, ApprovedBy: owner, ApprovedAt: 1, Created: 1, Updated: 1}
			if err := repo.CreateBotInstallation(ctx, installation); err != nil {
				t.Fatal(err)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				if err := tx.LockCommunity(ctx, community); err != nil {
					return err
				}
				return tx.(store.BotJoinRolePolicy).SetBotJoinRolePolicy(ctx, installation.ID, role.ID, owner, 2)
			}); err != nil {
				t.Fatal(err)
			}
			if err := repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: community, UserID: member, Joined: 3}); err != nil {
				t.Fatal(err)
			}
			current, err := repo.GetMember(ctx, community, member)
			if err != nil {
				t.Fatal(err)
			}
			request := store.BotJoinRoleGrantRequest{RequestID: id.New(), InstallationID: installation.ID, CommunityID: community, UserID: member, EpisodeID: current.EpisodeID}
			grants := repo.(store.BotJoinRoleGrants)
			if _, _, err := grants.GrantBotJoinRoleForEpisode(ctx, request); err == nil {
				t.Fatal("grant outside caller transaction succeeded")
			}
			rollback := errors.New("rollback bot join grant")
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				if _, _, err := tx.(store.BotJoinRoleGrants).GrantBotJoinRoleForEpisode(ctx, request); err != nil {
					return err
				}
				return rollback
			}); !errors.Is(err, rollback) {
				t.Fatalf("rollback transaction = %v, want sentinel", err)
			}
			if held, err := repo.ListMemberRoles(ctx, community, member); err != nil || slices.Contains(held, role.ID) {
				t.Fatalf("role after rollback = %v, %v; want absent", held, err)
			}
			var first store.BotJoinRoleGrantReceipt
			var created bool
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				var e error
				first, created, e = tx.(store.BotJoinRoleGrants).GrantBotJoinRoleForEpisode(ctx, request)
				return e
			}); err != nil || !created || first.RoleID != role.ID {
				t.Fatalf("first grant = %+v, created %v, err %v", first, created, err)
			}
			if err := repo.PutMemberRole(ctx, community, member, role.ID); err != nil {
				t.Fatal(err)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				var e error
				got, inserted, e := tx.(store.BotJoinRoleGrants).GrantBotJoinRoleForEpisode(ctx, request)
				if e == nil && (inserted || got != first) {
					t.Errorf("replay = %+v, created %v; want original receipt", got, inserted)
				}
				return e
			}); err != nil {
				t.Fatalf("same tuple replay: %v", err)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				changed := request
				changed.RequestID = id.New()
				_, _, err := tx.(store.BotJoinRoleGrants).GrantBotJoinRoleForEpisode(ctx, changed)
				return err
			}); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("second request for same episode = %v, want conflict", err)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				changed := request
				changed.UserID = owner
				_, _, err := tx.(store.BotJoinRoleGrants).GrantBotJoinRoleForEpisode(ctx, changed)
				return err
			}); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("request id reused for another member = %v, want conflict", err)
			}
			if err := repo.DeleteMemberRole(ctx, community, member, role.ID); err != nil {
				t.Fatal(err)
			}
			held, err := repo.ListMemberRoles(ctx, community, member)
			if err != nil || !slices.Contains(held, role.ID) {
				t.Fatalf("bot grant after manual source removal = %v, %v", held, err)
			}
			if err := repo.DeleteMember(ctx, community, member); err != nil {
				t.Fatal(err)
			}
			if err := repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: community, UserID: member, Joined: 4}); err != nil {
				t.Fatal(err)
			}
			rejoined, err := repo.GetMember(ctx, community, member)
			if err != nil || rejoined.EpisodeID == request.EpisodeID {
				t.Fatalf("rejoin episode = %+v, %v", rejoined, err)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				got, inserted, e := tx.(store.BotJoinRoleGrants).GrantBotJoinRoleForEpisode(ctx, request)
				if e == nil && (inserted || got != first) {
					t.Errorf("post-leave retry = %+v, created %v; want retained receipt", got, inserted)
				}
				return e
			}); err != nil {
				t.Fatalf("post-leave receipt replay: %v", err)
			}
			stale := request
			stale.RequestID = id.New()
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				_, _, e := tx.(store.BotJoinRoleGrants).GrantBotJoinRoleForEpisode(ctx, stale)
				return e
			}); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("second operation for old episode = %v, want conflict", err)
			}
		})
	}
}
