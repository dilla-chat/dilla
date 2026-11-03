package store_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func TestLockGroupRequiresTransactionAndFindsExistingGroup(t *testing.T) {
	ctx := context.Background()
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			group := seedGroup(ctx, t, repo, 1)
			if err := repo.LockGroup(ctx, group.GroupID); !errors.Is(err, sql.ErrTxDone) {
				t.Fatalf("LockGroup outside transaction = %v, want ErrTxDone", err)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				if err := tx.LockGroup(ctx, group.GroupID); err != nil {
					return err
				}
				return tx.LockGroup(ctx, id.New())
			}); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("LockGroup missing group = %v, want not found", err)
			}
		})
	}
}

func TestLockGroupSerializesTwoTransactions(t *testing.T) {
	ctx := context.Background()
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			group := seedGroup(ctx, t, repo, 1)
			locked := make(chan struct{})
			release := make(chan struct{})
			firstDone := make(chan error, 1)
			go func() {
				firstDone <- repo.Tx(ctx, func(tx store.Repository) error {
					if err := tx.LockGroup(ctx, group.GroupID); err != nil {
						return err
					}
					close(locked)
					<-release
					return nil
				})
			}()
			<-locked

			secondLocked := make(chan struct{})
			secondDone := make(chan error, 1)
			go func() {
				secondDone <- repo.Tx(ctx, func(tx store.Repository) error {
					if err := tx.LockGroup(ctx, group.GroupID); err != nil {
						return err
					}
					close(secondLocked)
					return nil
				})
			}()
			select {
			case <-secondLocked:
				t.Fatal("second transaction acquired the group before the first released it")
			case <-time.After(50 * time.Millisecond):
			}
			close(release)
			if err := <-firstDone; err != nil {
				t.Fatalf("first transaction: %v", err)
			}
			select {
			case <-secondLocked:
			case <-time.After(2 * time.Second):
				t.Fatal("second transaction stayed blocked after the group lock was released")
			}
			if err := <-secondDone; err != nil {
				t.Fatalf("second transaction: %v", err)
			}
		})
	}
}
