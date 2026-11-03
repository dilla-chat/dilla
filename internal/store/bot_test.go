package store_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func TestBotRuntimeRotationAndRevocationAreDurable(t *testing.T) {
	ctx := context.Background()
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			runtime := store.BotRuntimeRow{ID: id.New(), TokenHash: make([]byte, 32), Created: 10, Rotated: 10}
			runtime.TokenHash[0] = 1
			if err := repo.CreateBotRuntime(ctx, runtime); err != nil {
				t.Fatalf("CreateBotRuntime: %v", err)
			}
			if _, err := repo.GetBotRuntimeByHash(ctx, runtime.TokenHash); err != nil {
				t.Fatalf("GetBotRuntimeByHash: %v", err)
			}
			nextHash := make([]byte, 32)
			nextHash[0] = 2
			if err := repo.RotateBotRuntime(ctx, runtime.ID, runtime.TokenHash, nextHash, 20); err != nil {
				t.Fatalf("RotateBotRuntime: %v", err)
			}
			if _, err := repo.GetBotRuntimeByHash(ctx, runtime.TokenHash); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("old hash lookup = %v, want not found", err)
			}
			got, err := repo.GetBotRuntimeByHash(ctx, nextHash)
			if err != nil || got.ID != runtime.ID || got.Rotated != 20 {
				t.Fatalf("new hash lookup = %+v, %v", got, err)
			}
			if err := repo.RevokeBotRuntime(ctx, runtime.ID, 30); err != nil {
				t.Fatalf("RevokeBotRuntime: %v", err)
			}
			if err := repo.RotateBotRuntime(ctx, runtime.ID, nextHash, runtime.TokenHash, 40); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("rotate revoked runtime = %v, want conflict", err)
			}
		})
	}
}

func TestAutoModerationGrantIsOneUseAndExactRetryKeepsReceipt(t *testing.T) {
	ctx := context.Background()
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			community := seedCommunity(ctx, t, repo)
			approver, bot := seedUser(ctx, t, repo), seedUser(ctx, t, repo)
			device := store.DeviceRow{ID: id.New(), UserID: bot.ID, DSKPub: make([]byte, 32), CredentialBlob: []byte{1}, Created: 10}
			if err := repo.CreateDevice(ctx, device); err != nil {
				t.Fatal(err)
			}
			runtime := store.BotRuntimeRow{ID: id.New(), TokenHash: bytes.Repeat([]byte{1}, 32), Created: 10, Rotated: 10}
			if err := repo.CreateBotRuntime(ctx, runtime); err != nil {
				t.Fatal(err)
			}
			uid, did, rid := bot.ID, device.ID, runtime.ID
			inst := store.BotInstallationRow{ID: id.New(), CommunityID: community, RuntimeID: &rid, UserID: &uid, DeviceID: &did, State: store.BotInstallationReady, Revision: 1, ApprovedBy: approver.ID, ApprovedAt: 10, Created: 10, Updated: 10}
			if err := repo.CreateBotInstallation(ctx, inst); err != nil {
				t.Fatal(err)
			}
			row := store.BotAutoModerationGrantRow{TokenHash: bytes.Repeat([]byte{2}, 32), CommunityID: community, InstallationID: inst.ID, GroupID: id.New(), RequestID: id.New(), ActorUser: approver.ID, ActorDevice: id.New(), ExpectedRevision: 0, RequestExpires: 90, ConfigHash: bytes.Repeat([]byte{3}, 32), Expires: 80}
			if err := repo.CreateBotAutoModerationGrant(ctx, row); err != nil {
				t.Fatal(err)
			}
			got, err := repo.GetBotAutoModerationGrant(ctx, row.TokenHash)
			if err != nil || got.ExpectedRevision != 0 || !bytes.Equal(got.ConfigHash, row.ConfigHash) {
				t.Fatalf("grant read=%+v err=%v", got, err)
			}
			accepted := row
			accepted.ConsumedAt = 40
			accepted.RequestHash = bytes.Repeat([]byte{4}, 32)
			accepted.Receipt = bytes.Repeat([]byte{5}, 16)
			first, retry, err := repo.AcceptBotAutoModerationGrant(ctx, accepted)
			if err != nil || retry || !bytes.Equal(first.Receipt, accepted.Receipt) {
				t.Fatalf("first accept=%+v retry=%v err=%v", first, retry, err)
			}
			accepted.Receipt = bytes.Repeat([]byte{6}, 16)
			second, retry, err := repo.AcceptBotAutoModerationGrant(ctx, accepted)
			if err != nil || !retry || !bytes.Equal(second.Receipt, first.Receipt) {
				t.Fatalf("exact retry=%+v retry=%v err=%v", second, retry, err)
			}
			accepted.RequestHash = bytes.Repeat([]byte{7}, 32)
			if _, _, err = repo.AcceptBotAutoModerationGrant(ctx, accepted); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("changed request reuse=%v, want conflict", err)
			}
			accepted.RequestHash = first.RequestHash
			accepted.InstallationID = id.New()
			if _, _, err = repo.AcceptBotAutoModerationGrant(ctx, accepted); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("cross-installation reuse=%v, want conflict", err)
			}
		})
	}
}

func TestBotCASAllowsOnlyOneConcurrentRevision(t *testing.T) {
	ctx := context.Background()
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			community := seedCommunity(ctx, t, repo)
			approver := seedUser(ctx, t, repo).ID
			row := store.BotInstallationRow{ID: id.New(), CommunityID: community, State: store.BotInstallationPending, Revision: 1, ApprovedBy: approver, ApprovedAt: 1, Created: 1, Updated: 1}
			if err := repo.CreateBotInstallation(ctx, row); err != nil {
				t.Fatal(err)
			}
			start := make(chan struct{})
			results := make(chan error, 2)
			var wg sync.WaitGroup
			for state := store.BotInstallationEnrolling; state <= store.BotInstallationJoining; state++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					next := row
					next.State = state
					next.Revision = 2
					next.Updated = int64(state)
					results <- repo.UpdateBotInstallation(ctx, row.ID, 1, next)
				}()
			}
			close(start)
			wg.Wait()
			close(results)
			committed, conflicted := 0, 0
			for err := range results {
				if err == nil {
					committed++
				} else if errors.Is(err, store.ErrConflict) {
					conflicted++
				} else {
					t.Fatalf("CAS: %v", err)
				}
			}
			if committed != 1 || conflicted != 1 {
				t.Fatalf("committed=%d conflicted=%d", committed, conflicted)
			}
		})
	}
}

func TestBotInstallationRemovalRequiresDisabledState(t *testing.T) {
	ctx := context.Background()
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			community := seedCommunity(ctx, t, repo)
			approver := seedUser(ctx, t, repo).ID
			removedAt := int64(20)
			row := store.BotInstallationRow{ID: id.New(), CommunityID: community, State: store.BotInstallationReady, Revision: 1, ApprovedBy: approver, ApprovedAt: 1, Created: 1, Updated: 1, RemovedAt: &removedAt}
			if err := repo.CreateBotInstallation(ctx, row); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("create ready tombstone = %v, want conflict", err)
			}

			row.State = store.BotInstallationReady
			row.RemovedAt = nil
			if err := repo.CreateBotInstallation(ctx, row); err != nil {
				t.Fatalf("create active installation: %v", err)
			}
			next := row
			next.Revision = 2
			next.Updated = 20
			next.RemovedAt = &removedAt
			if err := repo.UpdateBotInstallation(ctx, row.ID, 1, next); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("CAS to ready tombstone = %v, want conflict", err)
			}

			query := `UPDATE bot_installations SET removed_at = ? WHERE id = ?`
			if engine == "postgres" {
				query = `UPDATE bot_installations SET removed_at = $1 WHERE id = $2`
			}
			if _, err := rawDB(t, repo).ExecContext(ctx, query, removedAt, row.ID); err == nil {
				t.Fatal("database accepted ready installation with removed_at")
			}
		})
	}
}

func TestBotClaimPutReplacesAtomicallyAndRollsBack(t *testing.T) {
	ctx := context.Background()
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			community := seedCommunity(ctx, t, repo)
			approver := seedUser(ctx, t, repo).ID
			firstRuntime := store.BotRuntimeRow{ID: id.New(), TokenHash: make([]byte, 32), Created: 1, Rotated: 1}
			firstRuntime.TokenHash[0] = 1
			if err := repo.CreateBotRuntime(ctx, firstRuntime); err != nil {
				t.Fatal(err)
			}
			firstID := firstRuntime.ID
			installation := store.BotInstallationRow{ID: id.New(), CommunityID: community, RuntimeID: &firstID, State: store.BotInstallationEnrolling, Revision: 1, ApprovedBy: approver, ApprovedAt: 1, Created: 1, Updated: 1}
			if err := repo.CreateBotInstallation(ctx, installation); err != nil {
				t.Fatal(err)
			}
			original := store.BotClaimRow{InstallationID: installation.ID, RuntimeID: firstRuntime.ID, ClaimID: id.New(), RequestID: id.New(), Expires: 100}
			if err := repo.PutBotClaim(ctx, original); err != nil {
				t.Fatal(err)
			}
			replacement := store.BotClaimRow{InstallationID: installation.ID, RuntimeID: firstRuntime.ID, ClaimID: id.New(), RequestID: id.New(), Expires: 200}
			if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.PutBotClaim(ctx, replacement) }); err != nil {
				t.Fatalf("replace claim: %v", err)
			}
			// Read through the outer repository after commit to cover persistence across caller restarts.
			if got, err := repo.GetBotClaim(ctx, installation.ID); err != nil || got.RuntimeID != firstID || got.ClaimID != replacement.ClaimID || got.RequestID != replacement.RequestID || got.Expires != replacement.Expires {
				t.Fatalf("replacement claim = %+v, %v", got, err)
			}
			otherCommunity := seedCommunity(ctx, t, repo)
			otherInstallation := store.BotInstallationRow{ID: id.New(), CommunityID: otherCommunity, RuntimeID: &firstID, State: store.BotInstallationEnrolling, Revision: 1, ApprovedBy: approver, ApprovedAt: 1, Created: 1, Updated: 1}
			if err := repo.CreateBotInstallation(ctx, otherInstallation); err != nil {
				t.Fatal(err)
			}
			duplicateGlobalClaim := store.BotClaimRow{InstallationID: otherInstallation.ID, RuntimeID: firstRuntime.ID, ClaimID: replacement.ClaimID, RequestID: id.New(), Expires: 250}
			if err := repo.PutBotClaim(ctx, duplicateGlobalClaim); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("duplicate global claim ID = %v, want conflict", err)
			}
			rollback := store.BotClaimRow{InstallationID: installation.ID, RuntimeID: firstRuntime.ID, ClaimID: id.New(), RequestID: id.New(), Expires: 300}
			txErr := errors.New("rollback claim replacement")
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				if err := tx.PutBotClaim(ctx, rollback); err != nil {
					return err
				}
				return txErr
			}); !errors.Is(err, txErr) {
				t.Fatalf("claim replacement rollback = %v", err)
			}
			if got, err := repo.GetBotClaim(ctx, installation.ID); err != nil || got.ClaimID != replacement.ClaimID || got.RuntimeID != firstRuntime.ID {
				t.Fatalf("claim after rollback = %+v, %v", got, err)
			}
		})
	}
}

func TestBotCommunityAndInstallationLocksRequireTransactions(t *testing.T) {
	ctx := context.Background()
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			community := seedCommunity(ctx, t, repo)
			approver := seedUser(ctx, t, repo).ID
			installation := store.BotInstallationRow{ID: id.New(), CommunityID: community, State: store.BotInstallationPending, Revision: 1, ApprovedBy: approver, ApprovedAt: 1, Created: 1, Updated: 1}
			if err := repo.CreateBotInstallation(ctx, installation); err != nil {
				t.Fatal(err)
			}
			runtime := store.BotRuntimeRow{ID: id.New(), TokenHash: make([]byte, 32), Created: 1, Rotated: 1}
			runtime.TokenHash[0] = 1
			if err := repo.CreateBotRuntime(ctx, runtime); err != nil {
				t.Fatal(err)
			}
			if err := repo.LockBotCommunity(ctx, community); err == nil {
				t.Fatal("community lock outside Tx succeeded")
			}
			if err := repo.LockBotInstallation(ctx, installation.ID); err == nil {
				t.Fatal("installation lock outside Tx succeeded")
			}
			if err := repo.LockBotRuntime(ctx, runtime.ID); err == nil {
				t.Fatal("runtime lock outside Tx succeeded")
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				if err := tx.LockBotRuntime(ctx, runtime.ID); err != nil {
					return err
				}
				if err := tx.LockBotCommunity(ctx, community); err != nil {
					return err
				}
				return tx.LockBotInstallation(ctx, installation.ID)
			}); err != nil {
				t.Fatalf("transaction locks: %v", err)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.LockBotInstallation(ctx, id.New()) }); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("missing row lock = %v", err)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.LockBotRuntime(ctx, id.New()) }); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("missing runtime lock = %v", err)
			}
		})
	}
}

func TestBotClaimRotationReceiptAndSessionBindingPersistence(t *testing.T) {
	ctx := context.Background()
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			community := seedCommunity(ctx, t, repo)
			approver := seedUser(ctx, t, repo).ID
			user := seedUser(ctx, t, repo)
			device := store.DeviceRow{ID: id.New(), UserID: user.ID, DSKPub: make([]byte, 32), CredentialBlob: []byte{1}, Created: 1, LastSeen: 1}
			if err := repo.CreateDevice(ctx, device); err != nil {
				t.Fatal(err)
			}
			runtime := store.BotRuntimeRow{ID: id.New(), TokenHash: make([]byte, 32), Created: 1, Rotated: 1}
			runtime.TokenHash[0] = 1
			if err := repo.CreateBotRuntime(ctx, runtime); err != nil {
				t.Fatal(err)
			}
			uid, did, rid := user.ID, device.ID, runtime.ID
			installation := store.BotInstallationRow{ID: id.New(), CommunityID: community, RuntimeID: &rid, UserID: &uid, DeviceID: &did, State: store.BotInstallationEnrolling, Revision: 1, ApprovedBy: approver, ApprovedAt: 1, Created: 1, Updated: 1}
			if err := repo.CreateBotInstallation(ctx, installation); err != nil {
				t.Fatalf("installation: %v", err)
			}
			claim := store.BotClaimRow{InstallationID: installation.ID, RuntimeID: runtime.ID, ClaimID: id.New(), RequestID: id.New(), Expires: 100}
			if err := repo.PutBotClaim(ctx, claim); err != nil {
				t.Fatalf("claim: %v", err)
			}
			operation := store.BotOperationRow{CommunityID: community, RequestID: id.New(), InstallationID: installation.ID, ActorKind: 1, ActorID: runtime.ID, Operation: 5, PayloadHash: make([]byte, 32), ResultCBOR: []byte{0x80}, Created: 2}
			if err := repo.CreateBotOperation(ctx, operation); err != nil {
				t.Fatalf("receipt: %v", err)
			}
			nextHash := make([]byte, 32)
			nextHash[0] = 2
			if err := repo.RotateBotRuntime(ctx, runtime.ID, runtime.TokenHash, nextHash, 3); err != nil {
				t.Fatalf("rotate: %v", err)
			}
			if _, err := repo.GetBotClaim(ctx, installation.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("claim after rotate: %v", err)
			}
			if got, err := repo.GetBotOperation(ctx, community, operation.RequestID); err != nil || got.ActorID != runtime.ID {
				t.Fatalf("receipt after rotate: %+v %v", got, err)
			}
			if err := repo.PutBotClaim(ctx, claim); err != nil {
				t.Fatalf("claim after rotation: %v", err)
			}
			if err := repo.RevokeBotRuntime(ctx, runtime.ID, 4); err != nil {
				t.Fatalf("revoke: %v", err)
			}
			if _, err := repo.GetBotClaim(ctx, installation.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("claim after revoke: %v", err)
			}
			sessionHash := make([]byte, 32)
			sessionHash[0] = 8
			if err := repo.CreateSession(ctx, store.SessionRow{TokenHash: sessionHash, DeviceID: device.ID, UserID: user.ID, Scope: 0, Tier: 0, Created: 1, Expires: 100, IdleExpires: 100}); err != nil {
				t.Fatalf("session: %v", err)
			}
			binding := store.BotSessionBindingRow{TokenHash: sessionHash, InstallationID: installation.ID, UserID: user.ID, DeviceID: device.ID, Created: 2}
			if err := repo.CreateBotSessionBinding(ctx, binding); err != nil {
				t.Fatalf("binding: %v", err)
			}
			other := store.BotInstallationRow{ID: id.New(), CommunityID: community, State: store.BotInstallationPending, Revision: 1, ApprovedBy: approver, ApprovedAt: 1, Created: 3, Updated: 3}
			// The active-community uniqueness means this second installation cannot coexist in the same community.
			if err := repo.CreateBotInstallation(ctx, other); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("second active installation = %v", err)
			}
			otherCommunity := seedCommunity(ctx, t, repo)
			otherUser := seedUser(ctx, t, repo)
			otherDevice := store.DeviceRow{ID: id.New(), UserID: otherUser.ID, DSKPub: make([]byte, 32), CredentialBlob: []byte{2}, Created: 3, LastSeen: 3}
			if err := repo.CreateDevice(ctx, otherDevice); err != nil {
				t.Fatal(err)
			}
			ouid, odid := otherUser.ID, otherDevice.ID
			other.ID = id.New()
			other.CommunityID = otherCommunity
			other.UserID = &ouid
			other.DeviceID = &odid
			if err := repo.CreateBotInstallation(ctx, other); err != nil {
				t.Fatalf("other installation: %v", err)
			}
			otherHash := make([]byte, 32)
			otherHash[0] = 9
			if err := repo.CreateSession(ctx, store.SessionRow{TokenHash: otherHash, DeviceID: otherDevice.ID, UserID: otherUser.ID, Scope: 0, Tier: 0, Created: 3, Expires: 100, IdleExpires: 100}); err != nil {
				t.Fatalf("other session: %v", err)
			}
			if err := repo.CreateBotSessionBinding(ctx, store.BotSessionBindingRow{TokenHash: otherHash, InstallationID: installation.ID, UserID: otherUser.ID, DeviceID: otherDevice.ID, Created: 4}); err == nil {
				t.Fatal("cross-installation binding was accepted")
			}
			if err := repo.DeleteSession(ctx, sessionHash); err != nil {
				t.Fatalf("delete session: %v", err)
			}
			if _, err := repo.GetBotSessionBinding(ctx, sessionHash); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("binding after session deletion: %v", err)
			}
		})
	}
}

func TestBotInstallationCASAndOperationReceiptAreAtomic(t *testing.T) {
	ctx := context.Background()
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			community := seedCommunity(ctx, t, repo)
			approver := seedUser(ctx, t, repo).ID
			row := store.BotInstallationRow{ID: id.New(), CommunityID: community, State: 0, Revision: 1, ApprovedBy: approver, ApprovedAt: 10, Created: 10, Updated: 10}
			if err := repo.CreateBotInstallation(ctx, row); err != nil {
				t.Fatalf("CreateBotInstallation: %v", err)
			}
			if err := repo.UpdateBotInstallation(ctx, row.ID, 1, func() store.BotInstallationRow { r := row; r.State = 1; r.Revision = 2; r.Updated = 20; return r }()); err != nil {
				t.Fatalf("UpdateBotInstallation: %v", err)
			}
			if err := repo.UpdateBotInstallation(ctx, row.ID, 1, row); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("stale CAS = %v, want conflict", err)
			}
			operation := store.BotOperationRow{CommunityID: community, RequestID: id.New(), InstallationID: row.ID, ActorKind: 0, ActorID: approver, Operation: 1, PayloadHash: make([]byte, 32), ResultCBOR: []byte{0x80}, Created: 20}
			if err := repo.CreateBotOperation(ctx, operation); err != nil {
				t.Fatalf("CreateBotOperation: %v", err)
			}
			if err := repo.CreateBotOperation(ctx, operation); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("duplicate operation = %v, want conflict", err)
			}
			got, err := repo.GetBotOperation(ctx, community, operation.RequestID)
			if err != nil || string(got.ResultCBOR) != string(operation.ResultCBOR) {
				t.Fatalf("GetBotOperation = %+v, %v", got, err)
			}
		})
	}
}
