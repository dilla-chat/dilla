package store_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// Plan 2 task 4: the bans table (00007_bans.sql) and P2-D10's listing, on both
// engines.
func TestBanRoundTrip(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			cid := seedCommunity(ctx, t, repo)
			mod := seedUser(ctx, t, repo).ID
			a, b := seedUser(ctx, t, repo).ID, seedUser(ctx, t, repo).ID

			if _, err := repo.GetBan(ctx, cid, a); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetBan before any ban = %v, want ErrNotFound", err)
			}
			until := int64(1_800_000_000)
			for _, ban := range []store.BanRow{
				{CommunityID: cid, UserID: a, Reason: "spam", ByUser: mod, Created: 100},
				{CommunityID: cid, UserID: b, Reason: "", ByUser: mod, Created: 200, Expires: &until},
			} {
				if err := repo.PutBan(ctx, ban); err != nil {
					t.Fatalf("PutBan: %v", err)
				}
			}
			got, err := repo.GetBan(ctx, cid, b)
			if err != nil {
				t.Fatalf("GetBan: %v", err)
			}
			if got.CommunityID != cid || got.UserID != b || got.ByUser != mod || got.Created != 200 ||
				got.Expires == nil || *got.Expires != until || got.Reason != "" {
				t.Fatalf("GetBan = %+v", got)
			}

			// Newest first.
			list, err := repo.ListBans(ctx, cid)
			if err != nil {
				t.Fatalf("ListBans: %v", err)
			}
			if len(list) != 2 || list[0].UserID != b || list[1].UserID != a || list[1].Expires != nil ||
				list[1].Reason != "spam" {
				t.Fatalf("ListBans = %+v", list)
			}

			// A second ban of the same user replaces the first: one row per (community, user).
			if err := repo.PutBan(ctx, store.BanRow{
				CommunityID: cid, UserID: a, Reason: "again", ByUser: mod, Created: 300,
			}); err != nil {
				t.Fatalf("PutBan (upsert): %v", err)
			}
			list, _ = repo.ListBans(ctx, cid)
			if len(list) != 2 || list[0].UserID != a || list[0].Reason != "again" || list[0].Created != 300 {
				t.Fatalf("after upsert = %+v", list)
			}

			if err := repo.DeleteBan(ctx, cid, a); err != nil {
				t.Fatalf("DeleteBan: %v", err)
			}
			if err := repo.DeleteBan(ctx, cid, a); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("second DeleteBan = %v, want ErrNotFound", err)
			}
			if _, err := repo.GetBan(ctx, cid, a); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetBan after delete = %v, want ErrNotFound", err)
			}

			// Another community's list is its own, and empty is a slice, not an error.
			other := seedCommunity(ctx, t, repo)
			if got, err := repo.ListBans(ctx, other); err != nil || len(got) != 0 {
				t.Fatalf("ListBans(other) = %+v, %v", got, err)
			}
		})
	}
}

func TestTheBansTableRefusesIllegalRows(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			cid := seedCommunity(ctx, t, repo)
			user := seedUser(ctx, t, repo).ID
			for name, b := range map[string]store.BanRow{
				"an unknown community": {CommunityID: id.New(), UserID: user, ByUser: user, Created: 1},
				"an unknown user":      {CommunityID: cid, UserID: id.New(), ByUser: user, Created: 1},
			} {
				if err := repo.PutBan(ctx, b); err == nil {
					t.Errorf("%s was accepted", name)
				}
			}
		})
	}
}

// P2-D3: the open groups bound to one target, of one kind, oldest first.
func TestGroupsForTarget(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			target := id.New()
			mk := func(kind uint8, created int64, tgt id.ID) store.GroupRow {
				g := store.GroupRow{
					GroupID: id.New(), Binding: []byte{0x80}, Kind: kind, TargetID: tgt, Ciphersuite: 1,
					ExternalSenderKeyID: id.New(), E2EEVersion: 1, MediaVersion: 1, PolicyVersion: 1,
					Created: created,
				}
				if err := repo.CreateGroup(ctx, g); err != nil {
					t.Fatalf("CreateGroup: %v", err)
				}
				return g
			}
			later := mk(0, 20, target)
			earlier := mk(0, 10, target)
			call := mk(1, 5, target)
			mk(0, 1, id.New()) // another target
			closed := mk(0, 15, target)
			if err := repo.CloseGroup(ctx, closed.GroupID, 30); err != nil {
				t.Fatalf("CloseGroup: %v", err)
			}

			text, err := repo.GroupsForTarget(ctx, target, 0)
			if err != nil {
				t.Fatalf("GroupsForTarget(text): %v", err)
			}
			if len(text) != 2 || text[0].GroupID != earlier.GroupID || text[1].GroupID != later.GroupID ||
				text[0].TargetID != target || text[0].Kind != 0 {
				t.Fatalf("GroupsForTarget(text) = %+v", text)
			}
			calls, err := repo.GroupsForTarget(ctx, target, 1)
			if err != nil || len(calls) != 1 || calls[0].GroupID != call.GroupID {
				t.Fatalf("GroupsForTarget(call) = %+v, %v", calls, err)
			}
			if none, err := repo.GroupsForTarget(ctx, id.New(), 0); err != nil || len(none) != 0 {
				t.Fatalf("GroupsForTarget(unknown) = %+v, %v", none, err)
			}
		})
	}
}

// LockCommunity (task 4 fix round 1) is the lock a join and a ban both take on
// the community row, so a join's ban check and its membership write cannot
// interleave with a ban. It works only inside a transaction, answers
// ErrNotFound for an unknown or deleted community, and a second transaction's
// lock waits for the first transaction to end.
func TestLockCommunity(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			cid := seedCommunity(ctx, t, repo)
			gone := seedCommunity(ctx, t, repo)
			if err := repo.SoftDeleteCommunity(ctx, gone, 5); err != nil {
				t.Fatalf("SoftDeleteCommunity: %v", err)
			}

			if err := repo.LockCommunity(ctx, cid); err == nil {
				t.Fatal("LockCommunity outside a transaction succeeded; the lock would end with the statement")
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				if err := tx.LockCommunity(ctx, cid); err != nil {
					t.Errorf("LockCommunity(live) = %v", err)
				}
				if err := tx.LockCommunity(ctx, id.New()); !errors.Is(err, store.ErrNotFound) {
					t.Errorf("LockCommunity(unknown) = %v, want ErrNotFound", err)
				}
				if err := tx.LockCommunity(ctx, gone); !errors.Is(err, store.ErrNotFound) {
					t.Errorf("LockCommunity(deleted) = %v, want ErrNotFound", err)
				}
				return nil
			}); err != nil {
				t.Fatalf("Tx: %v", err)
			}

			// A second locker waits for the first transaction to commit.
			held := make(chan struct{})
			release := make(chan struct{})
			first := make(chan error, 1)
			go func() {
				first <- repo.Tx(ctx, func(tx store.Repository) error {
					if err := tx.LockCommunity(ctx, cid); err != nil {
						close(held)
						return err
					}
					close(held)
					<-release
					return nil
				})
			}()
			<-held
			var locked atomic.Bool
			second := make(chan error, 1)
			go func() {
				second <- repo.Tx(ctx, func(tx store.Repository) error {
					if err := tx.LockCommunity(ctx, cid); err != nil {
						return err
					}
					locked.Store(true)
					return nil
				})
			}()
			time.Sleep(100 * time.Millisecond)
			if locked.Load() {
				t.Error("a second transaction took the community lock while the first held it")
			}
			close(release)
			if err := <-first; err != nil {
				t.Fatalf("first Tx: %v", err)
			}
			if err := <-second; err != nil || !locked.Load() {
				t.Fatalf("second Tx = %v, locked = %v", err, locked.Load())
			}
		})
	}
}
