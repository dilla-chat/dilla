package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func TestBotJoinRolePolicyIsOneInstallationScopedApproval(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			cid := seedCommunity(ctx, t, repo)
			owner := seedUser(ctx, t, repo).ID
			bot := seedUser(ctx, t, repo).ID
			role := store.RoleRow{ID: id.New(), CommunityID: cid, Name: "newcomer", Position: 2, Created: 1}
			if err := repo.PutRole(ctx, role); err != nil {
				t.Fatal(err)
			}
			installation := store.BotInstallationRow{ID: id.New(), CommunityID: cid, State: store.BotInstallationReady, Revision: 1, ApprovedBy: owner, ApprovedAt: 1, Created: 1, Updated: 1}
			if err := repo.CreateBotInstallation(ctx, installation); err != nil {
				t.Fatal(err)
			}
			policy := repo.(store.BotJoinRolePolicy)
			if err := policy.SetBotJoinRolePolicy(ctx, installation.ID, role.ID, owner, 2); err == nil {
				t.Fatal("write outside transaction succeeded")
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				if err := tx.LockCommunity(ctx, cid); err != nil {
					return err
				}
				return tx.(store.BotJoinRolePolicy).SetBotJoinRolePolicy(ctx, installation.ID, role.ID, owner, 2)
			}); err != nil {
				t.Fatal(err)
			}
			got, err := policy.GetBotJoinRolePolicy(ctx, installation.ID)
			if err != nil || got.CommunityID != cid || got.RoleID != role.ID || got.ApprovedBy != owner || got.ApprovedAt != 2 {
				t.Fatalf("approved policy = %+v, %v", got, err)
			}
			other := store.RoleRow{ID: id.New(), CommunityID: cid, Name: "other", Position: 1, Created: 1}
			if err := repo.PutRole(ctx, other); err != nil {
				t.Fatal(err)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				return tx.(store.BotJoinRolePolicy).SetBotJoinRolePolicy(ctx, installation.ID, other.ID, bot, 3)
			}); err != nil {
				t.Fatal(err)
			}
			got, err = policy.GetBotJoinRolePolicy(ctx, installation.ID)
			if err != nil || got.RoleID != other.ID || got.ApprovedBy != bot {
				t.Fatalf("replacement = %+v, %v", got, err)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				return tx.(store.BotJoinRolePolicy).SetBotJoinRolePolicy(ctx, installation.ID, id.ID{}, owner, 4)
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := policy.GetBotJoinRolePolicy(ctx, installation.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("cleared policy = %v, want ErrNotFound", err)
			}
		})
	}
}
