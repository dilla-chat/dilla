package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/dilla-chat/dilla/internal/store"
)

func TestMemberTimeoutRoundTripAndExpiry(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			cid := seedCommunity(ctx, t, repo)
			user, mod := seedUser(ctx, t, repo).ID, seedUser(ctx, t, repo).ID
			if err := repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: user, Joined: 10}); err != nil {
				t.Fatal(err)
			}
			if _, err := repo.GetMemberTimeout(ctx, cid, user, 100); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("missing timeout = %v, want ErrNotFound", err)
			}
			expires := int64(200)
			row := store.MemberTimeoutRow{CommunityID: cid, UserID: user, ByUser: mod, Created: 100, Expires: expires}
			if err := repo.PutMemberTimeout(ctx, row); err != nil {
				t.Fatal(err)
			}
			got, err := repo.GetMemberTimeout(ctx, cid, user, 199)
			if err != nil || got != row {
				t.Fatalf("active timeout = %+v, %v", got, err)
			}
			if _, err := repo.GetMemberTimeout(ctx, cid, user, expires); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("expired timeout = %v", err)
			}
			if err := repo.DeleteMemberTimeout(ctx, cid, user); err != nil {
				t.Fatal(err)
			}
			if err := repo.DeleteMemberTimeout(ctx, cid, user); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("second delete = %v", err)
			}
		})
	}
}
