package store_test

import (
	"context"
	"slices"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// Bot v2 Task 12 fix round 1 (M-3): the Bot feed reads every member's roles in one query.
func TestStoreListCommunityMemberRolesIsOneMapOfTheCommunity(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			cid, other := seedCommunity(ctx, t, repo), seedCommunity(ctx, t, repo)
			a, b, c := seedUser(ctx, t, repo), seedUser(ctx, t, repo), seedUser(ctx, t, repo)
			r1, r2, r3 := id.New(), id.New(), id.New()
			for _, r := range []store.RoleRow{{ID: r1, CommunityID: cid, Name: "one", Position: 1}, {ID: r2, CommunityID: cid, Name: "two", Position: 2}, {ID: r3, CommunityID: other, Name: "x", Position: 1}} {
				if err := repo.PutRole(ctx, r); err != nil {
					t.Fatalf("PutRole: %v", err)
				}
			}
			for _, m := range []store.MemberOfCommunityRow{{CommunityID: cid, UserID: a.ID}, {CommunityID: cid, UserID: b.ID}, {CommunityID: cid, UserID: c.ID}, {CommunityID: other, UserID: a.ID}} {
				m.EpisodeID = id.New()
				if err := repo.PutMember(ctx, m); err != nil {
					t.Fatalf("PutMember: %v", err)
				}
			}
			for _, g := range []struct{ cid, user, role id.ID }{{cid, a.ID, r2}, {cid, a.ID, r1}, {cid, b.ID, r1}, {other, a.ID, r3}} {
				if err := repo.PutMemberRole(ctx, g.cid, g.user, g.role); err != nil {
					t.Fatalf("PutMemberRole: %v", err)
				}
			}
			got, err := repo.ListCommunityMemberRoles(ctx, cid)
			if err != nil {
				t.Fatalf("ListCommunityMemberRoles: %v", err)
			}
			for user := range got {
				single, err := repo.ListMemberRoles(ctx, cid, user)
				if err != nil || !slices.Equal(single, got[user]) {
					t.Fatalf("bulk roles of %s = %v, per-member read %v (%v)", user, got[user], single, err)
				}
			}
			if len(got) != 2 || len(got[a.ID]) != 2 || len(got[b.ID]) != 1 || got[c.ID] != nil {
				t.Fatalf("ListCommunityMemberRoles = %v, want a with two roles, b with one, c absent, nothing from the other community", got)
			}
		})
	}
}
