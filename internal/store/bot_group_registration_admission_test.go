package store_test

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func TestBotGroupRegistrationAdmissionIsAtomicAndIdempotent(t *testing.T) {
	ctx := context.Background()
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			f := newSendFixture(ctx, t, repo)
			requestID, groupID := id.New(), id.New()
			receipt := groupRegistrationReceipt(f, id.New(), groupID)
			admission := store.BotInteractionAdmissionReceipt{InstallationID: f.installation.ID, RequestID: requestID, RequestHash: bytes.Repeat([]byte{0x22}, 32), Payload: []byte{1}, Signature: bytes.Repeat([]byte{0x33}, 64), NotAfter: 10}
			if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.CreateBotInteractionAdmissionReceipt(ctx, admission) }); err != nil {
				t.Fatalf("create admission receipt: %v", err)
			}
			var replay bool
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				var err error
				_, replay, err = tx.AcceptBotGroupRegistrationAdmission(ctx, requestID, receipt)
				return err
			}); err != nil || replay {
				t.Fatalf("first acceptance: replay=%v err=%v", replay, err)
			}
			var got store.BotGroupRegistrationReceipt
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				var err error
				got, replay, err = tx.AcceptBotGroupRegistrationAdmission(ctx, requestID, receipt)
				return err
			}); err != nil || !replay || !reflect.DeepEqual(got, receipt) {
				t.Fatalf("exact retry: got=%+v replay=%v err=%v", got, replay, err)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				var err error
				got, err = tx.GetBotGroupRegistrationAdmission(ctx, f.installation.ID, requestID)
				return err
			}); err != nil || !reflect.DeepEqual(got, receipt) {
				t.Fatalf("read consumed admission: got=%+v err=%v", got, err)
			}
			if _, err := repo.GetBotGroupRegistrationAdmission(ctx, f.installation.ID, id.New()); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown consumed admission: %v", err)
			}

			conflicts := []store.BotGroupRegistrationReceipt{receipt, receipt, receipt}
			conflicts[0].GroupID = id.New()
			conflicts[1].BodySHA256 = bytes.Repeat([]byte{0x41}, 32)
			conflicts[2].OperationID = id.New()
			for i, conflicting := range conflicts {
				if err := repo.Tx(ctx, func(tx store.Repository) error {
					_, _, err := tx.AcceptBotGroupRegistrationAdmission(ctx, requestID, conflicting)
					return err
				}); !errors.Is(err, store.ErrConflict) {
					t.Fatalf("conflicting reuse %d: %v", i, err)
				}
			}
			if got, err := repo.GetBotGroupRegistrationReceipt(ctx, f.installation.ID, receipt.OperationID); err != nil || !reflect.DeepEqual(got, receipt) {
				t.Fatalf("conflict changed accepted receipt: %+v, %v", got, err)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				return tx.CompactExpiredBotInteractionAdmissionReceipts(ctx, f.installation.ID, 10+30*24*60*60+1)
			}); err != nil {
				t.Fatalf("compact consumed admission payload: %v", err)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				var err error
				got, err = tx.GetBotGroupRegistrationAdmission(ctx, f.installation.ID, requestID)
				return err
			}); err != nil || !reflect.DeepEqual(got, receipt) {
				t.Fatalf("consumption survives admission payload compaction: got=%+v err=%v", got, err)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				var err error
				got, replay, err = tx.AcceptBotGroupRegistrationAdmission(ctx, requestID, receipt)
				return err
			}); err != nil || !replay || !reflect.DeepEqual(got, receipt) {
				t.Fatalf("consumption tombstone after compaction: got=%+v replay=%v err=%v", got, replay, err)
			}

			rollbackRequest := id.New()
			rollbackReceipt := groupRegistrationReceipt(f, id.New(), id.New())
			abort := errors.New("abort accepted registration")
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				if _, _, err := tx.AcceptBotGroupRegistrationAdmission(ctx, rollbackRequest, rollbackReceipt); err != nil {
					return err
				}
				return abort
			}); !errors.Is(err, abort) {
				t.Fatalf("rollback transaction: %v", err)
			}
			if _, err := repo.GetBotGroupRegistrationReceipt(ctx, f.installation.ID, rollbackReceipt.OperationID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("rollback left registration: %v", err)
			}
		})
	}
}
