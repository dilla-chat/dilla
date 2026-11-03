package api_test

import (
	"context"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func TestPostPolicyCheckTxReadsPermissionChangesInItsTransaction(t *testing.T) {
	for engine, repo := range policyEngines(t) {
		t.Run(engine, func(t *testing.T) {
			f := newPolicyFixture(t, repo, api.PermViewChannel, 0)
			policy := api.PostPolicy{Repo: repo, Resolver: api.NewResolver(repo)}
			err := repo.Tx(context.Background(), func(tx store.Repository) error {
				role := id.New()
				if err := tx.PutRole(context.Background(), store.RoleRow{
					ID: role, CommunityID: f.cid, Name: "temporary-send", Position: 1,
					Allow: uint64(api.PermSendMessages), Created: policyT0,
				}); err != nil {
					return err
				}
				if err := tx.PutMemberRole(context.Background(), f.cid, f.user, role); err != nil {
					return err
				}
				return policy.CheckTx(context.Background(), tx, f.group, f.user, 0, policyT0+1)
			})
			if err != nil {
				t.Fatalf("CheckTx did not observe transaction-local send permission: %v", err)
			}
		})
	}
}
