package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func interactionEligibilityRevision(t *testing.T, repo store.Repository, community, user id.ID) uint64 {
	t.Helper()
	var revision uint64
	err := repo.Tx(context.Background(), func(tx store.Repository) error {
		var err error
		revision, err = tx.(store.BotInteractionEligibility).GetBotInteractionEligibilityRevision(context.Background(), community, user)
		return err
	})
	if err != nil {
		t.Fatalf("read interaction eligibility revision: %v", err)
	}
	return revision
}

func TestBotInteractionEligibilityRevisionFencesMembershipBanAndDeviceChanges(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			community := seedCommunity(ctx, t, repo)
			user := seedUser(ctx, t, repo).ID
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				_, err := tx.(store.BotInteractionEligibility).GetBotInteractionEligibilityRevision(ctx, community, user)
				return err
			}); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("revision before membership = %v, want not found", err)
			}

			member := store.MemberOfCommunityRow{CommunityID: community, UserID: user, Joined: 10}
			if err := repo.PutMember(ctx, member); err != nil {
				t.Fatal(err)
			}
			if got := interactionEligibilityRevision(t, repo, community, user); got != 1 {
				t.Fatalf("first membership revision = %d, want 1", got)
			}
			member.Nick = "unchanged-membership"
			if err := repo.PutMember(ctx, member); err != nil {
				t.Fatal(err)
			}
			if got := interactionEligibilityRevision(t, repo, community, user); got != 1 {
				t.Fatalf("membership metadata update revision = %d, want unchanged 1", got)
			}

			owner := seedUser(ctx, t, repo).ID
			ban := store.BanRow{CommunityID: community, UserID: user, ByUser: owner, Created: 11}
			if err := repo.PutBan(ctx, ban); err != nil {
				t.Fatal(err)
			}
			if got := interactionEligibilityRevision(t, repo, community, user); got != 2 {
				t.Fatalf("ban insertion revision = %d, want 2", got)
			}
			ban.Reason = "updated"
			if err := repo.PutBan(ctx, ban); err != nil {
				t.Fatal(err)
			}
			if got := interactionEligibilityRevision(t, repo, community, user); got != 3 {
				t.Fatalf("ban update revision = %d, want 3", got)
			}
			if err := repo.DeleteBan(ctx, community, user); err != nil {
				t.Fatal(err)
			}
			if got := interactionEligibilityRevision(t, repo, community, user); got != 4 {
				t.Fatalf("ban removal revision = %d, want 4", got)
			}

			device := store.DeviceRow{ID: id.New(), UserID: user, DSKPub: make([]byte, 32), CredentialBlob: []byte{0}, Created: 12}
			if err := repo.CreateDevice(ctx, device); err != nil {
				t.Fatal(err)
			}
			if got := interactionEligibilityRevision(t, repo, community, user); got != 5 {
				t.Fatalf("device insertion revision = %d, want 5", got)
			}
			if err := repo.TouchDevice(ctx, device.ID, 99); err != nil {
				t.Fatal(err)
			}
			if got := interactionEligibilityRevision(t, repo, community, user); got != 5 {
				t.Fatalf("last-seen update revision = %d, want unchanged 5", got)
			}
			if err := repo.UpdateDeviceCredential(ctx, device.ID, []byte{1, 2, 3}); err != nil {
				t.Fatal(err)
			}
			if got := interactionEligibilityRevision(t, repo, community, user); got != 6 {
				t.Fatalf("credential update revision = %d, want 6", got)
			}
			if err := repo.RevokeDevice(ctx, device.ID, 13); err != nil {
				t.Fatal(err)
			}
			if got := interactionEligibilityRevision(t, repo, community, user); got != 7 {
				t.Fatalf("device revocation revision = %d, want 7", got)
			}

			list := store.DeviceListRow{UserID: user, Version: 1, Blob: []byte{0x80}, SSKSignature: make([]byte, 64), PrevHash: make([]byte, 32), Created: 14}
			if err := repo.PutDeviceList(ctx, list); err != nil {
				t.Fatal(err)
			}
			if got := interactionEligibilityRevision(t, repo, community, user); got != 8 {
				t.Fatalf("device-list insertion revision = %d, want 8", got)
			}

			rollback := errors.New("rollback eligibility update")
			list.Version = 2
			err := repo.Tx(ctx, func(tx store.Repository) error {
				if err := tx.PutDeviceList(ctx, list); err != nil {
					return err
				}
				return rollback
			})
			if !errors.Is(err, rollback) {
				t.Fatalf("rollback device-list update = %v, want sentinel", err)
			}
			if got := interactionEligibilityRevision(t, repo, community, user); got != 8 {
				t.Fatalf("rolled-back device-list revision = %d, want 8", got)
			}

			if err := repo.DeleteMember(ctx, community, user); err != nil {
				t.Fatal(err)
			}
			if got := interactionEligibilityRevision(t, repo, community, user); got != 9 {
				t.Fatalf("membership removal revision = %d, want 9", got)
			}
			if err := repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: community, UserID: user, Joined: 15}); err != nil {
				t.Fatal(err)
			}
			if got := interactionEligibilityRevision(t, repo, community, user); got != 10 {
				t.Fatalf("rejoin revision = %d, want 10", got)
			}
			at := int64(16)
			if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.SetUserDisabled(ctx, user, &at) }); err != nil {
				t.Fatalf("disable account: %v", err)
			}
			if got := interactionEligibilityRevision(t, repo, community, user); got != 11 {
				t.Fatalf("account disable revision = %d, want 11", got)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.SetUserDisabled(ctx, user, nil) }); err != nil {
				t.Fatalf("enable account: %v", err)
			}
			if got := interactionEligibilityRevision(t, repo, community, user); got != 12 {
				t.Fatalf("account enable revision = %d, want 12", got)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.TombstoneUser(ctx, user, 17) }); err != nil {
				t.Fatalf("tombstone account: %v", err)
			}
			if got := interactionEligibilityRevision(t, repo, community, user); got != 13 {
				t.Fatalf("account tombstone revision = %d, want 13", got)
			}
		})
	}
}

func TestBotInteractionEligibilityAccountDisableSerializesWithAdmissionFence(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			community, user := seedCommunity(ctx, t, repo), seedUser(ctx, t, repo).ID
			if err := repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: community, UserID: user, Joined: 30}); err != nil {
				t.Fatal(err)
			}
			locked, release := make(chan struct{}), make(chan struct{})
			admissionDone := make(chan error, 1)
			go func() {
				admissionDone <- repo.Tx(ctx, func(tx store.Repository) error {
					if err := tx.LockUserForDeviceRegistration(ctx, user); err != nil {
						return err
					}
					if _, err := tx.(store.BotInteractionEligibility).GetBotInteractionEligibilityRevision(ctx, community, user); err != nil {
						return err
					}
					close(locked)
					<-release
					return nil
				})
			}()
			<-locked
			disabled := make(chan error, 1)
			go func() {
				at := int64(31)
				disabled <- repo.Tx(ctx, func(tx store.Repository) error { return tx.SetUserDisabled(ctx, user, &at) })
			}()
			select {
			case err := <-disabled:
				t.Fatalf("disable passed the admission's locked user row: %v", err)
			case <-time.After(75 * time.Millisecond):
			}
			close(release)
			if err := <-admissionDone; err != nil {
				t.Fatalf("admission fence transaction: %v", err)
			}
			if err := <-disabled; err != nil {
				t.Fatalf("disable after admission fence: %v", err)
			}
			if got := interactionEligibilityRevision(t, repo, community, user); got != 2 {
				t.Fatalf("revision after concurrent disable = %d, want 2", got)
			}
		})
	}
}

func TestBotInteractionEligibilityRevisionReassignmentFencesOldAndNewPrincipals(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			oldCommunity := seedCommunity(ctx, t, repo)
			otherOldCommunity := seedCommunity(ctx, t, repo)
			newCommunity := seedCommunity(ctx, t, repo)
			oldUser := seedUser(ctx, t, repo).ID
			newUser := seedUser(ctx, t, repo).ID
			for _, community := range []id.ID{oldCommunity, otherOldCommunity} {
				if err := repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: community, UserID: oldUser, Joined: 20}); err != nil {
					t.Fatal(err)
				}
			}
			if err := repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: newCommunity, UserID: newUser, Joined: 21}); err != nil {
				t.Fatal(err)
			}
			device := store.DeviceRow{ID: id.New(), UserID: oldUser, DSKPub: make([]byte, 32), CredentialBlob: []byte{0}, Created: 22}
			if err := repo.CreateDevice(ctx, device); err != nil {
				t.Fatal(err)
			}
			if err := repo.PutDeviceList(ctx, store.DeviceListRow{UserID: oldUser, Version: 1, Blob: []byte{0x80}, SSKSignature: make([]byte, 64), PrevHash: make([]byte, 32), Created: 23}); err != nil {
				t.Fatal(err)
			}
			if err := repo.PutBan(ctx, store.BanRow{CommunityID: oldCommunity, UserID: oldUser, ByUser: newUser, Created: 24}); err != nil {
				t.Fatal(err)
			}

			updateDevice := `UPDATE devices SET user_id=? WHERE id=?`
			updateDeviceList := `UPDATE device_lists SET user_id=? WHERE user_id=?`
			updateBan := `UPDATE bans SET community_id=?,user_id=? WHERE community_id=? AND user_id=?`
			if engine == "postgres" {
				updateDevice = `UPDATE devices SET user_id=$1 WHERE id=$2`
				updateDeviceList = `UPDATE device_lists SET user_id=$1 WHERE user_id=$2`
				updateBan = `UPDATE bans SET community_id=$1,user_id=$2 WHERE community_id=$3 AND user_id=$4`
			}
			raw := rawWriteDB(t, repo)
			tx, err := raw.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(ctx, updateDevice, newUser, device.ID); err != nil {
				_ = tx.Rollback()
				t.Fatalf("reassign device: %v", err)
			}
			if _, err := tx.ExecContext(ctx, updateDeviceList, newUser, oldUser); err != nil {
				_ = tx.Rollback()
				t.Fatalf("reassign device list: %v", err)
			}
			if _, err := tx.ExecContext(ctx, updateBan, newCommunity, newUser, oldCommunity, oldUser); err != nil {
				_ = tx.Rollback()
				t.Fatalf("reassign ban: %v", err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if got := interactionEligibilityRevision(t, repo, oldCommunity, oldUser); got != 7 {
				t.Fatalf("old community/user after reassignment = %d, want 7", got)
			}
			if got := interactionEligibilityRevision(t, repo, otherOldCommunity, oldUser); got != 5 {
				t.Fatalf("other old community/user after reassignment = %d, want 5", got)
			}
			if got := interactionEligibilityRevision(t, repo, newCommunity, newUser); got != 4 {
				t.Fatalf("new community/user after reassignment = %d, want 4", got)
			}

			rollbackTx, err := raw.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := rollbackTx.ExecContext(ctx, updateDevice, oldUser, device.ID); err != nil {
				_ = rollbackTx.Rollback()
				t.Fatalf("reverse device reassignment: %v", err)
			}
			if _, err := rollbackTx.ExecContext(ctx, updateDeviceList, oldUser, newUser); err != nil {
				_ = rollbackTx.Rollback()
				t.Fatalf("reverse device-list reassignment: %v", err)
			}
			if _, err := rollbackTx.ExecContext(ctx, updateBan, oldCommunity, oldUser, newCommunity, newUser); err != nil {
				_ = rollbackTx.Rollback()
				t.Fatalf("reverse ban reassignment: %v", err)
			}
			if err := rollbackTx.Rollback(); err != nil {
				t.Fatalf("rollback reassignment transaction: %v", err)
			}
			if got := interactionEligibilityRevision(t, repo, oldCommunity, oldUser); got != 7 {
				t.Fatalf("old community/user after rollback = %d, want 7", got)
			}
			if got := interactionEligibilityRevision(t, repo, otherOldCommunity, oldUser); got != 5 {
				t.Fatalf("other old community/user after rollback = %d, want 5", got)
			}
			if got := interactionEligibilityRevision(t, repo, newCommunity, newUser); got != 4 {
				t.Fatalf("new community/user after rollback = %d, want 4", got)
			}
		})
	}
}
