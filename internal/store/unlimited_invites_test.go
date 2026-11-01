package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

// Task 10c (migration 00049): an unlimited invite redeems past max_uses, a finite one stops at it,
// and a never-expiring one (expires_at = InviteNeverExpires) survives any clock. Existing rows read
// back as finite.
func TestUnlimitedInvitesRedeemOnEveryEngine(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			cid := seedCommunity(ctx, t, repo)
			mk := func(hashByte byte, unlimited uint8, maxUses, used uint64, expires int64) []byte {
				t.Helper()
				hash := make([]byte, 32)
				hash[0] = hashByte
				copy(hash[1:], id.New().String())
				if err := repo.CreateInvite(ctx, store.InviteRow{ID: id.New(), CodeHash: hash, CommunityID: &cid,
					MaxUses: maxUses, UsedCount: used, Created: 1, ExpiresAt: expires, UnlimitedUses: unlimited}); err != nil {
					t.Fatalf("CreateInvite: %v", err)
				}
				return hash
			}
			unlimited := mk(1, 1, 1000, 1000, store.InviteNeverExpires)
			finite := mk(2, 0, 3, 3, 1<<40)
			got, err := repo.RedeemInvite(ctx, unlimited, 1<<62)
			if err != nil || got.UsedCount != 1001 || got.UnlimitedUses != 1 || !got.NeverExpires() || got.Exhausted() {
				t.Fatalf("redeem unlimited = %+v, %v", got, err)
			}
			if _, err := repo.RedeemInvite(ctx, finite, 2); !errors.Is(err, store.ErrExhausted) {
				t.Fatalf("redeem a spent finite invite = %v, want ErrExhausted", err)
			}
			row, err := repo.GetInviteByHash(ctx, finite)
			if err != nil || row.UnlimitedUses != 0 || !row.Exhausted() || row.NeverExpires() {
				t.Fatalf("finite row = %+v, %v", row, err)
			}
		})
	}
}
