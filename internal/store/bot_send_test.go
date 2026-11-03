package store_test

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sync"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

type sendFixture struct {
	installation store.BotInstallationRow
	user         store.UserRow
	device       store.DeviceRow
	community    id.ID
}

func newSendFixture(ctx context.Context, t *testing.T, repo store.Repository) sendFixture {
	t.Helper()
	community := seedCommunity(ctx, t, repo)
	approver := seedUser(ctx, t, repo)
	user := seedUser(ctx, t, repo)
	device := store.DeviceRow{ID: id.New(), UserID: user.ID, DSKPub: make([]byte, 32), CredentialBlob: []byte{1}, Created: 1, LastSeen: 1}
	if err := repo.CreateDevice(ctx, device); err != nil {
		t.Fatal(err)
	}
	installation := store.BotInstallationRow{ID: id.New(), CommunityID: community, UserID: &user.ID, DeviceID: &device.ID, State: store.BotInstallationJoining, Revision: 1, ApprovedBy: approver.ID, ApprovedAt: 1, Created: 1, Updated: 1}
	if err := repo.CreateBotInstallation(ctx, installation); err != nil {
		t.Fatal(err)
	}
	return sendFixture{installation: installation, user: user, device: device, community: community}
}

func sendOperation(f sendFixture, operation, attempt, group id.ID) store.BotSendOperationRow {
	return store.BotSendOperationRow{InstallationID: f.installation.ID, OperationID: operation, UserID: f.user.ID, DeviceID: f.device.ID, CommunityID: f.community, GroupID: group, CurrentAttempt: attempt, State: uint8(store.BotSendOperationActive)}
}

func sendAttempt(f sendFixture, operation, attempt id.ID, predecessor *id.ID, outcome store.BotSendAttemptOutcome) store.BotSendAttemptRow {
	return store.BotSendAttemptRow{InstallationID: f.installation.ID, OperationID: operation, AttemptID: attempt, PredecessorID: predecessor, Generation: 2, Epoch: math.MaxInt64, Class: 1, CiphertextSHA256: make([]byte, 32), FingerprintSHA256: make([]byte, 32), Outcome: uint8(outcome), Receipt: []byte{0x81}}
}

func TestBotSendImmutableChainAndSingleSuccessor(t *testing.T) {
	ctx := context.Background()
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			f := newSendFixture(ctx, t, repo)
			op, first, group := id.New(), id.New(), id.New()
			created := sendOperation(f, op, first, group)
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				got, inserted, err := tx.CreateBotSendOperationIfAbsent(ctx, created)
				if err != nil {
					return err
				}
				if !inserted || got != created {
					t.Fatalf("create got=%+v inserted=%v", got, inserted)
				}
				if err := tx.InsertBotSendAttempt(ctx, sendAttempt(f, op, first, nil, store.BotSendAttemptClosed)); err != nil {
					return err
				}
				return nil
			}); err != nil {
				t.Fatalf("create first attempt: %v", err)
			}
			if got, err := repo.GetBotSendAttempt(ctx, f.installation.ID, op, first); err != nil || got.AttemptID != first || !reflect.DeepEqual(got.Receipt, []byte{0x81}) {
				t.Fatalf("get immutable attempt: %+v, %v", got, err)
			}
			changed := sendAttempt(f, op, first, nil, store.BotSendAttemptClosed)
			changed.Generation++
			if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.InsertBotSendAttempt(ctx, changed) }); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("changed duplicate attempt=%v", err)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				return tx.InsertBotSendAttempt(ctx, sendAttempt(f, op, id.New(), nil, store.BotSendAttemptClosed))
			}); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("second first attempt=%v", err)
			}

			// A conflicting create must return the original immutable scope unchanged.
			conflicting := created
			conflicting.GroupID = id.New()
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				got, inserted, err := tx.CreateBotSendOperationIfAbsent(ctx, conflicting)
				if err != nil {
					return err
				}
				if inserted || got.GroupID != group {
					t.Fatalf("conflict rewrote operation: %+v inserted=%v", got, inserted)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			// A pointer CAS without a matching successor row would strand
			// recovery. The repository must refuse it even though the current
			// predecessor is closed.
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				return tx.AdvanceBotSendAttempt(ctx, f.installation.ID, op, first, id.New())
			}); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("advance to missing successor=%v", err)
			}

			branches := []id.ID{id.New(), id.New()}
			start := make(chan struct{})
			results := make(chan error, 2)
			var wg sync.WaitGroup
			for _, branch := range branches {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					results <- repo.Tx(ctx, func(tx store.Repository) error {
						if err := tx.InsertBotSendAttempt(ctx, sendAttempt(f, op, branch, &first, store.BotSendAttemptClosed)); err != nil {
							return err
						}
						return tx.AdvanceBotSendAttempt(ctx, f.installation.ID, op, first, branch)
					})
				}()
			}
			close(start)
			wg.Wait()
			close(results)
			won, lost := 0, 0
			for err := range results {
				if err == nil {
					won++
				} else if errors.Is(err, store.ErrConflict) {
					lost++
				} else {
					t.Fatalf("successor race: %v", err)
				}
			}
			if won != 1 || lost != 1 {
				t.Fatalf("successors won=%d lost=%d", won, lost)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				row, err := tx.LockBotSendOperation(ctx, f.installation.ID, op)
				if err != nil {
					return err
				}
				if row.CurrentAttempt == first {
					t.Fatalf("current pointer did not advance")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			// Exact duplicate keys, invalid digest widths, and uint64 wrap attempts refuse.
			badOp, badAttempt := id.New(), id.New()
			bad := sendAttempt(f, badOp, badAttempt, nil, store.BotSendAttemptClosed)
			bad.Epoch = math.MaxUint64
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				if _, _, err := tx.CreateBotSendOperationIfAbsent(ctx, sendOperation(f, badOp, badAttempt, group)); err != nil {
					return err
				}
				return tx.InsertBotSendAttempt(ctx, bad)
			}); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("out-of-range epoch insert=%v", err)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				_, err := tx.LockBotSendOperation(ctx, f.installation.ID, badOp)
				return err
			}); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("failed attempt did not roll back its logical operation: %v", err)
			}
		})
	}
}

func TestBotSendSealAndRemovalCleanupAreAtomic(t *testing.T) {
	ctx := context.Background()
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			f := newSendFixture(ctx, t, repo)
			op, attempt := id.New(), id.New()
			row := sendOperation(f, op, attempt, id.New())
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				if _, _, err := tx.CreateBotSendOperationIfAbsent(ctx, row); err != nil {
					return err
				}
				if err := tx.InsertBotSendAttempt(ctx, sendAttempt(f, op, attempt, nil, store.BotSendAttemptAccepted)); err != nil {
					return err
				}
				return tx.SealBotSendOperation(ctx, f.installation.ID, op, attempt, store.BotSendOperationAccepted)
			}); err != nil {
				t.Fatal(err)
			}
			var got store.BotSendOperationRow
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				var err error
				got, err = tx.LockBotSendOperation(ctx, f.installation.ID, op)
				return err
			}); err != nil || got.State != uint8(store.BotSendOperationAccepted) || got.AcceptedAttempt == nil || *got.AcceptedAttempt != attempt {
				t.Fatalf("accepted seal=%+v, %v", got, err)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				return tx.SealBotSendOperation(ctx, f.installation.ID, op, attempt, store.BotSendOperationUncertain)
			}); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("second seal=%v", err)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				return tx.AdvanceBotSendAttempt(ctx, f.installation.ID, op, attempt, id.New())
			}); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("accepted operation advanced=%v", err)
			}

			removed := int64(9)
			next := f.installation
			next.State = store.BotInstallationDisabled
			next.Revision = 2
			next.Updated = 9
			next.RemovedAt = &removed
			if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.UpdateBotInstallation(ctx, next.ID, 1, next) }); err != nil {
				t.Fatalf("remove installation: %v", err)
			}
			if _, err := repo.GetBotSendAttempt(ctx, f.installation.ID, op, attempt); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("receipt after removal=%v", err)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				_, err := tx.LockBotSendOperation(ctx, f.installation.ID, op)
				return err
			}); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("operation after removal=%v", err)
			}
		})
	}
}

func TestBotSendBoundsRollbackAndRetention(t *testing.T) {
	ctx := context.Background()
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			f := newSendFixture(ctx, t, repo)
			group := seedGroup(ctx, t, repo, 1)
			operation, attempt := id.New(), id.New()
			row := sendOperation(f, operation, attempt, group.GroupID)
			// A failed transaction after all three writes must leave no logical
			// row or attempt behind. This models acceptance's atomic seal boundary.
			rollback := errors.New("force rollback")
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				if _, _, err := tx.CreateBotSendOperationIfAbsent(ctx, row); err != nil {
					return err
				}
				accepted := sendAttempt(f, operation, attempt, nil, store.BotSendAttemptAccepted)
				accepted.Generation = math.MaxInt64
				accepted.CiphertextSHA256 = make([]byte, 32) // all-zero hashes are opaque and valid
				if err := tx.InsertBotSendAttempt(ctx, accepted); err != nil {
					return err
				}
				if err := tx.SealBotSendOperation(ctx, f.installation.ID, operation, attempt, store.BotSendOperationAccepted); err != nil {
					return err
				}
				return rollback
			}); !errors.Is(err, rollback) {
				t.Fatalf("injected rollback=%v", err)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				_, err := tx.LockBotSendOperation(ctx, f.installation.ID, operation)
				return err
			}); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("rollback kept operation: %v", err)
			}
			if _, err := repo.GetBotSendAttempt(ctx, f.installation.ID, operation, attempt); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("rollback kept attempt: %v", err)
			}

			// Invalid state must not be used to manufacture a terminal operation
			// without an attempt; creation is only the active starting state.
			invalid := row
			invalid.State = uint8(store.BotSendOperationAccepted)
			invalid.AcceptedAttempt = &attempt
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				_, _, err := tx.CreateBotSendOperationIfAbsent(ctx, invalid)
				return err
			}); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("terminal operation creation=%v", err)
			}

			// A refused attempt can be closed only with a signed-int64 epoch and
			// generation. The max signed value itself is valid.
			closedOp, closedAttempt := id.New(), id.New()
			closed := sendAttempt(f, closedOp, closedAttempt, nil, store.BotSendAttemptClosed)
			closed.Generation, closed.Epoch = math.MaxInt64, math.MaxInt64
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				if _, _, err := tx.CreateBotSendOperationIfAbsent(ctx, sendOperation(f, closedOp, closedAttempt, group.GroupID)); err != nil {
					return err
				}
				return tx.InsertBotSendAttempt(ctx, closed)
			}); err != nil {
				t.Fatalf("signed-int64 max bounds: %v", err)
			}
			invalidAttempts := []struct {
				name   string
				change func(*store.BotSendAttemptRow)
			}{
				{"generation overflow", func(v *store.BotSendAttemptRow) { v.Generation = math.MaxUint64 }},
				{"epoch overflow", func(v *store.BotSendAttemptRow) { v.Epoch = math.MaxUint64 }},
				{"short ciphertext digest", func(v *store.BotSendAttemptRow) { v.CiphertextSHA256 = make([]byte, 31) }},
				{"long fingerprint digest", func(v *store.BotSendAttemptRow) { v.FingerprintSHA256 = make([]byte, 33) }},
				{"class overflow", func(v *store.BotSendAttemptRow) { v.Class = 2 }},
				{"receipt overflow", func(v *store.BotSendAttemptRow) { v.Receipt = make([]byte, 513) }},
				{"zero attempt id", func(v *store.BotSendAttemptRow) { v.AttemptID = id.ID{} }},
			}
			for _, tc := range invalidAttempts {
				t.Run(tc.name, func(t *testing.T) {
					invalid := sendAttempt(f, id.New(), id.New(), nil, store.BotSendAttemptClosed)
					tc.change(&invalid)
					if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.InsertBotSendAttempt(ctx, invalid) }); !errors.Is(err, store.ErrConflict) {
						t.Fatalf("invalid attempt=%v", err)
					}
				})
			}

			if err := repo.PutAppMessage(ctx, store.AppMessageRow{GroupID: group.GroupID, Seq: 1, Epoch: 1, UploaderDevice: f.device.ID, Blob: []byte("ciphertext"), FrankingTag: make([]byte, 32), Size: 10, Created: 1}); err != nil {
				t.Fatalf("seed prunable message: %v", err)
			}
			if n, err := repo.PruneAppMessages(ctx, group.GroupID, 0, 2, 2); err != nil || n != 1 {
				t.Fatalf("prune content: deleted=%d err=%v", n, err)
			}
			if got, err := repo.GetBotSendAttempt(ctx, f.installation.ID, closedOp, closedAttempt); err != nil || !reflect.DeepEqual(got.Receipt, closed.Receipt) {
				t.Fatalf("content prune removed receipt: %+v %v", got, err)
			}
			expires := int64(3)
			if err := repo.PutAppMessage(ctx, store.AppMessageRow{GroupID: group.GroupID, Seq: 2, Epoch: 1, UploaderDevice: f.device.ID, Blob: []byte("expired ciphertext"), FrankingTag: make([]byte, 32), Size: 19, Created: 2, Expires: &expires}); err != nil {
				t.Fatalf("seed expiring message: %v", err)
			}
			if n, err := repo.PruneAppMessages(ctx, group.GroupID, 0, 0, 4); err != nil || n != 1 {
				t.Fatalf("expire content: deleted=%d err=%v", n, err)
			}
			if got, err := repo.GetBotSendAttempt(ctx, f.installation.ID, closedOp, closedAttempt); err != nil || !reflect.DeepEqual(got.Receipt, closed.Receipt) {
				t.Fatalf("message expiry removed receipt: %+v %v", got, err)
			}
		})
	}
}

func TestBotSendUnknownRestoreAndDisableRetainReceipt(t *testing.T) {
	ctx := context.Background()
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			f := newSendFixture(ctx, t, repo)
			operation, attempt, lostPredecessor := id.New(), id.New(), id.New()
			group := id.New()
			unknown := sendAttempt(f, operation, attempt, &lostPredecessor, store.BotSendAttemptUnknown)
			unknown.Receipt = []byte{0x81, 0x02}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				if _, inserted, err := tx.CreateBotSendOperationIfAbsent(ctx, sendOperation(f, operation, attempt, group)); err != nil || !inserted {
					t.Fatalf("create restore operation: inserted=%v err=%v", inserted, err)
				}
				if err := tx.InsertBotSendAttempt(ctx, unknown); err != nil {
					return err
				}
				return tx.SealBotSendOperation(ctx, f.installation.ID, operation, attempt, store.BotSendOperationUncertain)
			}); err != nil {
				t.Fatalf("seal restored unknown outcome: %v", err)
			}
			if got, err := repo.GetBotSendAttempt(ctx, f.installation.ID, operation, attempt); err != nil || got.PredecessorID == nil || *got.PredecessorID != lostPredecessor || !reflect.DeepEqual(got.Receipt, unknown.Receipt) {
				t.Fatalf("unknown receipt lost predecessor or exact body: %+v %v", got, err)
			}
			if _, err := repo.GetBotSendAttempt(ctx, id.New(), operation, attempt); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("foreign installation read: %v", err)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				return tx.AdvanceBotSendAttempt(ctx, f.installation.ID, operation, attempt, id.New())
			}); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("uncertain operation advanced: %v", err)
			}

			disabled := f.installation
			disabled.State = store.BotInstallationDisabled
			disabled.Revision = 2
			disabled.Updated = 2
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				return tx.UpdateBotInstallation(ctx, disabled.ID, 1, disabled)
			}); err != nil {
				t.Fatalf("disable installation: %v", err)
			}
			if got, err := repo.GetBotSendAttempt(ctx, f.installation.ID, operation, attempt); err != nil || !reflect.DeepEqual(got.Receipt, unknown.Receipt) {
				t.Fatalf("disable erased recovery receipt: %+v %v", got, err)
			}
		})
	}
}
