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

func TestBotInteractionAdmissionReceiptPersistsPerInstallationRequest(t *testing.T) {
	ctx := context.Background()
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			f := newSendFixture(ctx, t, repo)
			r := store.BotInteractionAdmissionReceipt{InstallationID: f.installation.ID, RequestID: id.New(), RequestHash: bytes.Repeat([]byte{0x41}, 32), Payload: []byte{0x81, 0x01}, Signature: bytes.Repeat([]byte{0x52}, 64), NotAfter: 100}
			if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.CreateBotInteractionAdmissionReceipt(ctx, r) }); err != nil {
				t.Fatalf("create: %v", err)
			}
			got, err := repo.GetBotInteractionAdmissionReceipt(ctx, r.InstallationID, r.RequestID)
			if err != nil || !reflect.DeepEqual(got, r) {
				t.Fatalf("get receipt: %+v, %v", got, err)
			}
			changed := r
			changed.RequestHash = bytes.Repeat([]byte{0x61}, 32)
			if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.CreateBotInteractionAdmissionReceipt(ctx, changed) }); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("duplicate request insert=%v, want conflict", err)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				return tx.CompactExpiredBotInteractionAdmissionReceipts(ctx, r.InstallationID, r.NotAfter+30*24*60*60+1)
			}); err != nil {
				t.Fatalf("compact after retention: %v", err)
			}
			if _, err := repo.GetBotInteractionAdmissionReceipt(ctx, r.InstallationID, r.RequestID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("receipt after retention=%v, want forgotten", err)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.CreateBotInteractionAdmissionReceipt(ctx, changed) }); err != nil {
				t.Fatalf("same request ID after retention: %v", err)
			}
			deleteInstallation := `DELETE FROM bot_installations WHERE id=?`
			if engine == "postgres" {
				deleteInstallation = `DELETE FROM bot_installations WHERE id=$1`
			}
			if _, err := rawWriteDB(t, repo).ExecContext(ctx, deleteInstallation, r.InstallationID); err != nil {
				t.Fatalf("delete installation: %v", err)
			}
			if _, err := repo.GetBotInteractionAdmissionReceipt(ctx, r.InstallationID, r.RequestID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("receipt after installation deletion=%v, want cascade not found", err)
			}
		})
	}
}
