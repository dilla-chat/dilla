package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func TestBotKeyPackageReceiptIsCompactAndInstallationScoped(t *testing.T) {
	ctx := context.Background()
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			community := seedCommunity(ctx, t, repo)
			approver := seedUser(ctx, t, repo)
			user := seedUser(ctx, t, repo)
			device := store.DeviceRow{ID: id.New(), UserID: user.ID, DSKPub: make([]byte, 32), CredentialBlob: []byte{1}, LastSeen: 1, Created: 1}
			if err := repo.CreateDevice(ctx, device); err != nil {
				t.Fatal(err)
			}
			installation, operation := id.New(), id.New()
			if err := repo.CreateBotInstallation(ctx, store.BotInstallationRow{ID: installation, CommunityID: community, UserID: &user.ID, DeviceID: &device.ID, State: store.BotInstallationJoining, Revision: 1, ApprovedBy: approver.ID, ApprovedAt: 1, Created: 1, Updated: 1}); err != nil {
				t.Fatal(err)
			}
			receipt := store.KeyPackagePublicationReceipt{
				InstallationID: installation, UserID: user.ID, DeviceID: device.ID, OperationID: operation,
				Generation: 4, UploadSHA256: make([]byte, 32), Count: 2, AcceptedAt: 100,
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				if err := tx.CreateBotKeyPackagePublicationReceipt(ctx, receipt); err != nil {
					return err
				}
				got, err := tx.GetBotKeyPackagePublicationReceipt(ctx, installation, operation)
				if err != nil {
					return err
				}
				if got.UserID != user.ID || got.DeviceID != device.ID || got.Generation != 4 || got.Count != 2 || got.AcceptedAt != 100 {
					t.Fatalf("receipt = %+v", got)
				}
				return nil
			}); err != nil {
				t.Fatalf("persist receipt: %v", err)
			}
			if _, err := repo.GetBotKeyPackagePublicationReceipt(ctx, id.New(), operation); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("receipt from another installation = %v", err)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				return tx.DeleteBotKeyPackagePublicationReceipts(ctx, installation)
			}); err != nil {
				t.Fatalf("delete on removal: %v", err)
			}
			if _, err := repo.GetBotKeyPackagePublicationReceipt(ctx, installation, operation); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("receipt after removal = %v", err)
			}
		})
	}
}
