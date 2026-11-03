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

func groupRegistrationReceipt(f sendFixture, operation, group id.ID) store.BotGroupRegistrationReceipt {
	return store.BotGroupRegistrationReceipt{
		InstallationID: f.installation.ID,
		OperationID:    operation,
		UserID:         f.user.ID,
		DeviceID:       f.device.ID,
		CommunityID:    f.community,
		GroupID:        group,
		BodySHA256:     bytes.Repeat([]byte{0xa7}, 32),
		Generation:     4,
		Epoch:          0,
		NextSeq:        1,
		AcceptedAt:     42,
	}
}

func TestBotGroupRegistrationReceiptIsImmutableScopedAndTransactional(t *testing.T) {
	ctx := context.Background()
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			f := newSendFixture(ctx, t, repo)
			op, group := id.New(), id.New()
			receipt := groupRegistrationReceipt(f, op, group)
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				if err := tx.CreateBotGroupRegistrationReceipt(ctx, receipt); err != nil {
					return err
				}
				got, err := tx.GetBotGroupRegistrationReceipt(ctx, f.installation.ID, op)
				if err != nil || !reflect.DeepEqual(got, receipt) {
					t.Fatalf("transaction read-your-write: %+v, %v", got, err)
				}
				return nil
			}); err != nil {
				t.Fatalf("create receipt: %v", err)
			}
			got, err := repo.GetBotGroupRegistrationReceipt(ctx, f.installation.ID, op)
			if err != nil || !reflect.DeepEqual(got, receipt) {
				t.Fatalf("get receipt: %+v, %v", got, err)
			}
			got, err = repo.GetBotGroupRegistrationReceiptByGroup(ctx, group)
			if err != nil || !reflect.DeepEqual(got, receipt) {
				t.Fatalf("get receipt by accepted group: %+v, %v", got, err)
			}
			if _, err := repo.GetBotGroupRegistrationReceiptByGroup(ctx, id.New()); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown group receipt lookup: %v", err)
			}
			if _, err := repo.GetBotGroupRegistrationReceipt(ctx, id.New(), op); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("foreign installation lookup: %v", err)
			}
			otherInstallation := newSendFixture(ctx, t, repo)
			otherReceipt := groupRegistrationReceipt(otherInstallation, op, id.New())
			otherReceipt.BodySHA256 = bytes.Repeat([]byte{0x19}, 32)
			if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.CreateBotGroupRegistrationReceipt(ctx, otherReceipt) }); err != nil {
				t.Fatalf("same operation ID under another installation: %v", err)
			}
			if got, err := repo.GetBotGroupRegistrationReceipt(ctx, otherInstallation.installation.ID, op); err != nil || !reflect.DeepEqual(got, otherReceipt) {
				t.Fatalf("other installation receipt: %+v, %v", got, err)
			}

			changed := receipt
			changed.BodySHA256 = bytes.Repeat([]byte{0xb8}, 32)
			if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.CreateBotGroupRegistrationReceipt(ctx, receipt) }); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("exact duplicate insert=%v", err)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.CreateBotGroupRegistrationReceipt(ctx, changed) }); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("duplicate operation insert=%v", err)
			}
			otherOperation := groupRegistrationReceipt(f, id.New(), group)
			if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.CreateBotGroupRegistrationReceipt(ctx, otherOperation) }); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("second receipt for group=%v", err)
			}
			if got, err := repo.GetBotGroupRegistrationReceipt(ctx, f.installation.ID, op); err != nil || !reflect.DeepEqual(got, receipt) {
				t.Fatalf("conflict rewrote receipt: %+v, %v", got, err)
			}

			rollback := groupRegistrationReceipt(f, id.New(), id.New())
			abort := errors.New("abort group write")
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				if err := tx.CreateBotGroupRegistrationReceipt(ctx, rollback); err != nil {
					return err
				}
				return abort
			}); !errors.Is(err, abort) {
				t.Fatalf("rollback transaction=%v", err)
			}
			if _, err := repo.GetBotGroupRegistrationReceipt(ctx, f.installation.ID, rollback.OperationID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("rollback left receipt: %v", err)
			}

			disabled := f.installation
			disabled.State = store.BotInstallationDisabled
			disabled.Revision++
			disabled.Updated++
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				return tx.UpdateBotInstallation(ctx, disabled.ID, f.installation.Revision, disabled)
			}); err != nil {
				t.Fatalf("disable installation: %v", err)
			}
			if got, err := repo.GetBotGroupRegistrationReceipt(ctx, f.installation.ID, op); err != nil || !reflect.DeepEqual(got, receipt) {
				t.Fatalf("disable removed receipt: %+v, %v", got, err)
			}

			removed := disabled
			removed.Revision++
			removed.Updated++
			removedAt := int64(3)
			removed.RemovedAt = &removedAt
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				return tx.UpdateBotInstallation(ctx, disabled.ID, disabled.Revision, removed)
			}); err != nil {
				t.Fatalf("remove installation: %v", err)
			}
			if _, err := repo.GetBotGroupRegistrationReceipt(ctx, f.installation.ID, op); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("removed installation receipt remains: %v", err)
			}
		})
	}
}
