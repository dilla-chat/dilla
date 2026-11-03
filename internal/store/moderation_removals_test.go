package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func TestModerationRemovalReceiptAndFeedAreDurableAndAtomic(t *testing.T) {
	ctx := context.Background()
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			community := seedCommunity(ctx, t, repo)
			user := seedUser(ctx, t, repo)
			device := store.DeviceRow{ID: id.New(), UserID: user.ID, DSKPub: make([]byte, 32), CredentialBlob: []byte{1}, Tier: 0, SignerTier: 0, Created: 100, LastSeen: 100}
			if err := repo.CreateDevice(ctx, device); err != nil {
				t.Fatal(err)
			}
			group := id.New()
			if err := repo.CreateGroup(ctx, store.GroupRow{GroupID: group, Binding: []byte{0x80}, Kind: 0, CommunityID: &community, TargetID: id.New(), Ciphersuite: 1, Epoch: 1, ExternalSenderKeyID: id.New(), E2EEVersion: 1, MediaVersion: 1, PolicyVersion: 1, Created: 100}); err != nil {
				t.Fatal(err)
			}
			message := store.AppMessageRow{GroupID: group, Seq: 1, UploaderDevice: device.ID, Blob: []byte("ciphertext"), FrankingTag: make([]byte, 32), Created: 101, FrankingKeyID: id.New()}
			if err := repo.PutAppMessage(ctx, message); err != nil {
				t.Fatal(err)
			}
			message.Seq = 2
			if err := repo.PutAppMessage(ctx, message); err != nil {
				t.Fatal(err)
			}
			v := store.ModerationRemovalRow{RequestID: id.New(), ActionID: id.New(), CommunityID: community, GroupID: group, TargetSequence: 1, Proof: []byte{1, 2}, Signature: make([]byte, 64), AcceptedAt: 102}
			var got store.ModerationRemovalRow
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				var err error
				got, err = tx.AcceptModerationRemoval(ctx, v)
				return err
			}); err != nil || got.Revision != 1 {
				t.Fatalf("accept = %+v, %v", got, err)
			}
			row, err := repo.GetAppMessage(ctx, group, 1)
			if err != nil || row.DeletedAt == nil || len(row.Blob) != 0 {
				t.Fatalf("target after accept = %+v, %v", row, err)
			}
			page, err := repo.ListModerationRemovals(ctx, group, 0, 10)
			if err != nil || len(page.Rows) != 1 || page.Rows[0].Revision != 1 || page.NextRevision != 1 {
				t.Fatalf("feed = %+v, %v", page, err)
			}
			byRequest, err := repo.GetModerationRemovalByRequest(ctx, v.RequestID)
			if err != nil || byRequest.ActionID != got.ActionID || byRequest.GroupID != group || byRequest.Revision != got.Revision {
				t.Fatalf("lookup by accepted request = %+v, %v", byRequest, err)
			}
			if _, err := repo.GetModerationRemovalByRequest(ctx, id.New()); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown removal request = %v", err)
			}
			if _, err := repo.AcceptModerationRemoval(ctx, v); err == nil {
				t.Fatal("accept outside transaction succeeded")
			}
			abort := errors.New("rollback")
			v.RequestID = id.New()
			v.TargetSequence = 2
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				if _, err := tx.AcceptModerationRemoval(ctx, v); err != nil {
					return err
				}
				return abort
			}); !errors.Is(err, abort) {
				t.Fatalf("rollback = %v", err)
			}
			page, err = repo.ListModerationRemovals(ctx, group, 0, 10)
			if err != nil || len(page.Rows) != 1 || page.NextRevision != 1 {
				t.Fatalf("rolled back feed = %+v, %v", page, err)
			}
		})
	}
}
