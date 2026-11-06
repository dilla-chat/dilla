package ds_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	"github.com/dilla-chat/dilla/internal/clock"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/store/postgres"
	pgmigrations "github.com/dilla-chat/dilla/internal/store/postgres/migrations"
)

// This runs the real WASI KeyPackage validator against native Bot KeyPackages on SQLite and
// Postgres. A separate human fixture is deliberately rejected for the Bot installation.
func TestManagedBotKeyPackageReceiptSurvivesConsumptionAndGenerationChange(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	fixture := loadNativeBotKeyPackageFixture(t)
	device, user, credential, batches := fixture.rows(t)
	if err := h.repo.CreateUser(ctx, store.UserRow{ID: user, Username: "bot" + user.String()[:10], Display: "Native Bot", Kind: 1, UMKPub: fixture.decode(t, fixture.UMKPublicHex), SSKPub: fixture.decode(t, fixture.SSKPublicHex), SigUMKSSK: fixture.decode(t, fixture.SigUMKSSKHex), Created: fixture.GeneratedAt}); err != nil {
		t.Fatal(err)
	}
	verifiedAt := fixture.GeneratedAt
	if err := h.repo.CreateDevice(ctx, store.DeviceRow{ID: device, UserID: user, DSKPub: fixture.decode(t, fixture.DSKPublicHex), Tier: 0, SignerTier: 0, CredentialBlob: credential, VerifiedAt: &verifiedAt, LastSeen: fixture.GeneratedAt, Created: fixture.GeneratedAt}); err != nil {
		t.Fatal(err)
	}
	info := validateNativePackage(t, h.wasm, batches[0].Ordinary[0])
	runManagedKeyPackageFlow(t, h.repo, h.ds, h.wasm, h.clk, device, user,
		batches[0].Ordinary[0].blob(t), credential, info,
		batches[1].Ordinary[0], batches[0].Fallback, batches[1].Fallback,
		batches[0].Ordinary[1], batches[0].Ordinary[2], nil, nil)

	// A genuine human/browser package with the same numeric account/device identifiers is not a
	// managed Bot package. The ABI8 credential comparison must reject it before acceptance.

	if dsn := os.Getenv("DILLA_TEST_PG"); dsn != "" {
		repo, restoreRepo, monitorDB, delivery, wasm, pgClock := newManagedPostgresDS(t, dsn)
		defer wasm.Close(context.Background())
		pgDevice, pgUser := device, user
		// The cryptographic fixture names stable identifiers, so create precisely those rows in the
		// independent Postgres database before exercising the real validator there.
		if err := repo.CreateUser(ctx, store.UserRow{ID: pgUser, Username: "bot" + pgUser.String()[:10], Display: "Bot", Kind: 1, UMKPub: fixture.decode(t, fixture.UMKPublicHex), SSKPub: fixture.decode(t, fixture.SSKPublicHex), SigUMKSSK: fixture.decode(t, fixture.SigUMKSSKHex), Created: pgClock.Now().Unix()}); err != nil {
			t.Fatal(err)
		}
		if err := repo.CreateDevice(ctx, store.DeviceRow{ID: pgDevice, UserID: pgUser, DSKPub: fixture.decode(t, fixture.DSKPublicHex), Tier: 0, SignerTier: 0, CredentialBlob: credential, VerifiedAt: &verifiedAt, LastSeen: pgClock.Now().Unix(), Created: pgClock.Now().Unix()}); err != nil {
			t.Fatal(err)
		}
		pgInfo := validateNativePackage(t, wasm, batches[0].Ordinary[0])
		runManagedKeyPackageFlow(t, repo, delivery, wasm, pgClock, pgDevice, pgUser,
			batches[0].Ordinary[0].blob(t), credential, pgInfo,
			batches[1].Ordinary[0], batches[0].Fallback, batches[1].Fallback,
			batches[0].Ordinary[1], batches[0].Ordinary[2], restoreRepo, monitorDB)
	} else {
		t.Log("DILLA_TEST_PG unset: skipped managed KeyPackage WASI/Postgres leg")
	}
}

func TestManagedBotPublicationRejectsHumanCredentialWithMatchingRegisteredIDs(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	device, humanSession, humanPackage, _ := h.keyPackageOwner(t)
	fixture := loadNativeBotKeyPackageFixture(t)
	_, _, managedCredential, _ := fixture.rows(t)
	db, err := sql.Open("sqlite", h.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `UPDATE users SET kind=1 WHERE id=?`, humanSession.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE devices SET tier=0, signer_tier=0, credential_blob=? WHERE id=?`, managedCredential, device); err != nil {
		t.Fatal(err)
	}
	community := id.New()
	now := h.clk.Now().Unix()
	if err := h.repo.CreateCommunity(ctx, store.CommunityRow{ID: community, Owner: humanSession.UserID, Name: "credential-mismatch", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: community, UserID: humanSession.UserID, Joined: now}); err != nil {
		t.Fatal(err)
	}
	runtime := store.BotRuntimeRow{ID: id.New(), TokenHash: make([]byte, 32), Created: now, Rotated: now}
	runtime.TokenHash[0] = 1
	if err := h.repo.CreateBotRuntime(ctx, runtime); err != nil {
		t.Fatal(err)
	}
	verified := now
	installation := store.BotInstallationRow{ID: id.New(), CommunityID: community, RuntimeID: &runtime.ID, UserID: &humanSession.UserID, DeviceID: &device, State: store.BotInstallationJoining, Revision: 1, ApprovedBy: humanSession.UserID, ApprovedAt: now, CredentialVerifiedAt: &verified, Created: now, Updated: now}
	if err := h.repo.CreateBotInstallation(ctx, installation); err != nil {
		t.Fatal(err)
	}
	session := humanSession
	session.WireVersion = 6
	session.Bot = &auth.BotSessionBinding{InstallationID: installation.ID, CommunityID: community, State: store.BotInstallationJoining}
	upload, err := cborx.Marshal([]any{[]any{humanPackage}, nil})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.ds.PublishBotKeyPackages(ctx, session, installation.ID, id.New(), 1, upload); err == nil {
		t.Fatal("human/browser credential was accepted as the installed Bot credential")
	} else {
		var refusal *ds.Error
		if !errors.As(err, &refusal) || refusal.Code != "E_FORBIDDEN" || !strings.Contains(refusal.Detail, "verified final Bot credential") {
			t.Fatalf("human credential mismatch refusal=%v, want E_FORBIDDEN from exact credential equality", err)
		}
	}
	if got, err := h.repo.CountKeyPackages(ctx, device, now); err != nil || got != 0 {
		t.Fatalf("mismatched package rows count=%d err=%v, want no accepted packages", got, err)
	}
}

func TestPostgresDeviceQuarantineSerializesWithManagedPublication(t *testing.T) {
	dsn := os.Getenv("DILLA_TEST_PG")
	if dsn == "" {
		t.Skip("DILLA_TEST_PG unset")
	}
	fixture := loadNativeBotKeyPackageFixture(t)
	device, user, credential, batches := fixture.rows(t)
	for _, tc := range []struct {
		name             string
		publicationFirst bool
	}{
		{name: "publication holds row first", publicationFirst: true},
		{name: "quarantine commits first", publicationFirst: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, secondRepo, monitorDB, delivery, wasm, clk := newManagedPostgresDS(t, dsn)
			defer wasm.Close(context.Background())
			ctx := context.Background()
			seedNativeManagedBotRows(t, repo, fixture, device, user, credential, clk)
			community := id.New()
			if err := repo.CreateCommunity(ctx, store.CommunityRow{ID: community, Owner: user, Name: "kp-quarantine", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: clk.Now().Unix()}); err != nil {
				t.Fatal(err)
			}
			if err := repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: community, UserID: user, Joined: clk.Now().Unix()}); err != nil {
				t.Fatal(err)
			}
			runtime := store.BotRuntimeRow{ID: id.New(), TokenHash: make([]byte, 32), Created: clk.Now().Unix(), Rotated: clk.Now().Unix()}
			runtime.TokenHash[0] = 1
			if err := repo.CreateBotRuntime(ctx, runtime); err != nil {
				t.Fatal(err)
			}
			verified := clk.Now().Unix()
			installation := store.BotInstallationRow{ID: id.New(), CommunityID: community, RuntimeID: &runtime.ID, UserID: &user, DeviceID: &device, State: store.BotInstallationJoining, Revision: 1, ApprovedBy: user, ApprovedAt: verified, CredentialVerifiedAt: &verified, Created: verified, Updated: verified}
			if err := repo.CreateBotInstallation(ctx, installation); err != nil {
				t.Fatal(err)
			}
			session := auth.Session{UserID: user, DeviceID: device, Scope: auth.ScopeEnrolled, WireVersion: 6, Bot: &auth.BotSessionBinding{InstallationID: installation.ID, CommunityID: community, State: store.BotInstallationJoining}}
			blob, err := cborx.Marshal([]any{[]any{batches[0].Ordinary[0].blob(t)}, nil})
			if err != nil {
				t.Fatal(err)
			}
			operation := id.New()
			if tc.publicationFirst {
				locked, release := make(chan struct{}), make(chan struct{})
				var deviceLocks atomic.Int32
				pausedRepo := &keyPackageHookRepo{Repository: repo, deviceLockHook: func() {
					if deviceLocks.Add(1) == 2 { // pause after WASI, during final locked admission
						close(locked)
						<-release
					}
				}}
				pausedDS, err := ds.New(ds.Options{Store: pausedRepo, Wasm: wasm, Clock: clk, Keys: testInstanceKeys(t), Policy: ds.DefaultPolicy()})
				if err != nil {
					t.Fatal(err)
				}
				defer pausedDS.Shutdown(context.Background())
				publishDone := make(chan error, 1)
				go func() {
					_, _, err := pausedDS.PublishBotKeyPackages(ctx, session, installation.ID, operation, 1, blob)
					publishDone <- err
				}()
				<-locked
				quarantineDone := make(chan error, 1)
				go func() {
					quarantineDone <- secondRepo.QuarantineDevice(ctx, device, clk.Now().Unix(), "publication race")
				}()
				waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				waitForPostgresLockWait(t, waitCtx, monitorDB)
				cancel()
				close(release)
				if err := <-publishDone; err != nil {
					t.Fatalf("publication holding device row failed before quarantine: %v", err)
				}
				if err := <-quarantineDone; err != nil {
					t.Fatalf("quarantine after accepted publication: %v", err)
				}
			} else {
				locked, release := make(chan struct{}), make(chan struct{})
				quarantineDone := make(chan error, 1)
				go func() {
					quarantineDone <- secondRepo.Tx(ctx, func(tx store.Repository) error {
						if err := tx.QuarantineDevice(ctx, device, clk.Now().Unix(), "publication race"); err != nil {
							return err
						}
						close(locked)
						<-release
						return nil
					})
				}()
				<-locked
				publishDone := make(chan error, 1)
				go func() {
					_, _, err := delivery.PublishBotKeyPackages(ctx, session, installation.ID, operation, 1, blob)
					publishDone <- err
				}()
				waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				waitForPostgresLockWait(t, waitCtx, monitorDB)
				cancel()
				close(release)
				if err := <-quarantineDone; err != nil {
					t.Fatal(err)
				}
				if err := <-publishDone; err == nil {
					t.Fatal("publication succeeded after the quarantine transaction committed")
				}
				if _, err := repo.GetBotKeyPackagePublicationReceipt(ctx, installation.ID, operation); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("quarantine-first race created a receipt: %v", err)
				}
			}
		})
	}
}

func seedNativeManagedBotRows(t *testing.T, repo store.Repository, fixture nativeBotKeyPackageFixture, device, user id.ID, credential []byte, clk *clock.Fake) {
	t.Helper()
	ctx := context.Background()
	verified := clk.Now().Unix()
	if err := repo.CreateUser(ctx, store.UserRow{ID: user, Username: "bot" + user.String()[:10], Display: "Bot", Kind: 1, UMKPub: fixture.decode(t, fixture.UMKPublicHex), SSKPub: fixture.decode(t, fixture.SSKPublicHex), SigUMKSSK: fixture.decode(t, fixture.SigUMKSSKHex), Created: verified}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateDevice(ctx, store.DeviceRow{ID: device, UserID: user, DSKPub: fixture.decode(t, fixture.DSKPublicHex), Tier: 0, SignerTier: 0, CredentialBlob: credential, VerifiedAt: &verified, LastSeen: verified, Created: verified}); err != nil {
		t.Fatal(err)
	}
}

func runManagedKeyPackageFlow(t *testing.T, repo store.Repository, delivery *ds.DS, wasm *mlswasi.Runtime, clk *clock.Fake, device, user id.ID, blob, credential []byte, info mlswasi.KeyPackageInfo, alternate, fallbackA, fallbackB, raceA, raceB nativeBotKeyPackage, restoreRepo store.Repository, monitorDB *sql.DB) {
	t.Helper()
	ctx := context.Background()
	now := clk.Now().Unix()
	registered, err := repo.GetDevice(ctx, device)
	if err != nil || !bytes.Equal(registered.CredentialBlob, credential) || !bytes.Equal(info.Credential, credential) {
		t.Fatalf("native Bot credential agreement: device=%v WASI=%v err=%v", bytes.Equal(registered.CredentialBlob, credential), bytes.Equal(info.Credential, credential), err)
	}
	community := id.New()
	if err := repo.CreateCommunity(ctx, store.CommunityRow{ID: community, Owner: user, Name: "bot-kp", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: community, UserID: user, Joined: now}); err != nil {
		t.Fatal(err)
	}
	runtime := store.BotRuntimeRow{ID: id.New(), Created: now, Rotated: now}
	// crypto hash is fixed-width in storage; use a deterministic nonzero 32-byte test hash.
	runtime.TokenHash = make([]byte, 32)
	runtime.TokenHash[0] = 1
	if err := repo.CreateBotRuntime(ctx, runtime); err != nil {
		t.Fatal(err)
	}
	verified := now
	installation := store.BotInstallationRow{ID: id.New(), CommunityID: community, RuntimeID: &runtime.ID, UserID: &user, DeviceID: &device, State: store.BotInstallationJoining, Revision: 1, ApprovedBy: user, ApprovedAt: now, CredentialVerifiedAt: &verified, Created: now, Updated: now}
	if err := repo.CreateBotInstallation(ctx, installation); err != nil {
		t.Fatal(err)
	}
	session := auth.Session{UserID: user, DeviceID: device, Scope: auth.ScopeEnrolled}
	session.WireVersion = 6
	session.Bot = &auth.BotSessionBinding{InstallationID: installation.ID, CommunityID: community, State: store.BotInstallationJoining}
	empty, err := delivery.ManagedBotKeyPackageInventory(ctx, session, installation.ID)
	if err != nil || empty.Generation != 1 || empty.ServerTime != uint64(clk.Now().Unix()) || empty.OrdinaryActiveCount != 0 || empty.HasCurrentFallback != 0 {
		t.Fatalf("fresh managed KeyPackage inventory=%+v err=%v", empty, err)
	}
	if _, err := delivery.ManagedBotKeyPackageInventory(ctx, session, id.New()); err == nil {
		t.Fatal("managed Bot session read a foreign installation inventory")
	}
	humanSession := session
	humanSession.Bot = nil
	if _, err := delivery.ManagedBotKeyPackageInventory(ctx, humanSession, installation.ID); err == nil {
		t.Fatal("human session read managed Bot KeyPackage inventory")
	}
	// Repeated refs within a batch are deduped. Distinct native Bot batches also cover the
	// A/B/A retry rule: retrying A must return its receipt without restoring fallback A.
	upload, err := cborx.Marshal([]any{[]any{blob, blob}, fallbackA.blob(t)})
	if err != nil {
		t.Fatal(err)
	}
	operation := id.New()
	rollbackOperation := id.New()
	if monitorDB != nil {
		if _, err := monitorDB.ExecContext(ctx, `CREATE FUNCTION dilla_test_reject_bot_kp_receipt() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected receipt failure'; END $$`); err != nil {
			t.Fatal(err)
		}
		if _, err := monitorDB.ExecContext(ctx, `CREATE TRIGGER dilla_test_reject_bot_kp_receipt BEFORE INSERT ON bot_keypackage_publication_receipts FOR EACH ROW EXECUTE FUNCTION dilla_test_reject_bot_kp_receipt()`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = monitorDB.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS dilla_test_reject_bot_kp_receipt ON bot_keypackage_publication_receipts`)
			_, _ = monitorDB.ExecContext(context.Background(), `DROP FUNCTION IF EXISTS dilla_test_reject_bot_kp_receipt()`)
		})
	} else if sqliteRepo, ok := repo.(*failingRepo); ok {
		sqliteRepo.failNext("CreateBotKeyPackagePublicationReceipt")
	}
	if _, _, err := delivery.PublishBotKeyPackages(ctx, session, installation.ID, rollbackOperation, 1, upload); err == nil {
		t.Fatal("injected receipt insert failure was accepted")
	}
	if _, err := repo.GetKeyPackage(ctx, device, info.KPRef); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("package survived receipt transaction rollback: %v", err)
	}
	if _, err := repo.GetBotKeyPackagePublicationReceipt(ctx, installation.ID, rollbackOperation); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("receipt survived failed publication: %v", err)
	}
	if monitorDB != nil {
		if _, err := monitorDB.ExecContext(ctx, `DROP TRIGGER dilla_test_reject_bot_kp_receipt ON bot_keypackage_publication_receipts`); err != nil {
			t.Fatal(err)
		}
		if _, err := monitorDB.ExecContext(ctx, `DROP FUNCTION dilla_test_reject_bot_kp_receipt()`); err != nil {
			t.Fatal(err)
		}
	}
	count, accepted, err := delivery.PublishBotKeyPackages(ctx, session, installation.ID, operation, 1, upload)
	if err != nil || !accepted || count != 2 {
		t.Fatalf("publish count=%d accepted=%v err=%v", count, accepted, err)
	}
	operationB := id.New()
	uploadB, err := cborx.Marshal([]any{[]any{alternate.blob(t)}, fallbackB.blob(t)})
	if err != nil {
		t.Fatal(err)
	}
	if count, accepted, err := delivery.PublishBotKeyPackages(ctx, session, installation.ID, operationB, 1, uploadB); err != nil || !accepted || count != 2 {
		t.Fatalf("publish B count=%d accepted=%v err=%v", count, accepted, err)
	}
	fallbackBInfo := validateNativePackage(t, wasm, fallbackB)
	if count, accepted, err := delivery.PublishBotKeyPackages(ctx, session, installation.ID, operation, 1, upload); err != nil || accepted || count != 2 {
		t.Fatalf("A/B/A lost-reply replay count=%d accepted=%v err=%v", count, accepted, err)
	}
	if row, err := repo.GetKeyPackage(ctx, device, fallbackBInfo.KPRef); err != nil || !bytes.Equal(row.Blob, fallbackB.blob(t)) || row.LastResort != 1 {
		t.Fatalf("historical A replay replaced fallback B: row=%+v err=%v", row, err)
	}
	before, err := repo.CountKeyPackages(ctx, device, clk.Now().Unix())
	if err != nil || before != 2 {
		t.Fatalf("ordinary count before inventory=%d err=%v", before, err)
	}
	receiptBefore, err := repo.GetBotKeyPackagePublicationReceipt(ctx, installation.ID, operation)
	if err != nil {
		t.Fatalf("read publication receipt before inventory: %v", err)
	}
	for i := 0; i < 2; i++ {
		inventory, err := delivery.ManagedBotKeyPackageInventory(ctx, session, installation.ID)
		if err != nil || inventory.Generation != 1 || inventory.ServerTime != uint64(clk.Now().Unix()) || inventory.OrdinaryActiveCount != 2 || inventory.HasCurrentFallback != 1 {
			t.Fatalf("published inventory pass %d = %+v err=%v", i, inventory, err)
		}
	}
	if after, err := repo.CountKeyPackages(ctx, device, clk.Now().Unix()); err != nil || after != before {
		t.Fatalf("inventory changed ordinary inventory count %d -> %d (err=%v)", before, after, err)
	}
	receiptAfter, err := repo.GetBotKeyPackagePublicationReceipt(ctx, installation.ID, operation)
	if err != nil || receiptAfter.Generation != receiptBefore.Generation || receiptAfter.Count != receiptBefore.Count || !bytes.Equal(receiptAfter.UploadSHA256, receiptBefore.UploadSHA256) || receiptAfter.AcceptedAt != receiptBefore.AcceptedAt {
		t.Fatalf("inventory changed publication receipt: before=%+v after=%+v err=%v", receiptBefore, receiptAfter, err)
	}
	// Fill the ordinary directory to its cap with distinct stored refs, then publish the same real
	// package under a new operation. Its existing ref consumes no capacity and remains a no-op.
	limit := ds.DefaultPolicy().MaxKeyPackagesPerDevice
	var synthetic [][]byte
	for i := 0; i < limit-2; i++ {
		ref := make([]byte, 32)
		ref[0], ref[1], ref[2] = 0xff, byte(i>>8), byte(i+1)
		if string(ref) == string(info.KPRef) {
			t.Fatal("synthetic ref collided with fixture ref")
		}
		synthetic = append(synthetic, ref)
	}
	rows := make([]store.KeyPackageRow, len(synthetic))
	for i, ref := range synthetic {
		rows[i] = store.KeyPackageRow{DeviceID: device, KPRef: ref, Blob: []byte{1}, Expires: clk.Now().Add(time.Hour).Unix(), Created: clk.Now().Unix()}
	}
	if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.PutKeyPackages(ctx, device, rows) }); err != nil {
		t.Fatal(err)
	}
	capUpload, err := cborx.Marshal([]any{[]any{blob}, nil})
	if err != nil {
		t.Fatal(err)
	}
	capOperation := id.New()
	if count, accepted, err := delivery.PublishBotKeyPackages(ctx, session, installation.ID, capOperation, 1, capUpload); err != nil || !accepted || count != 1 {
		t.Fatalf("duplicate-at-cap publish count=%d accepted=%v err=%v", count, accepted, err)
	}
	if got, err := repo.CountKeyPackages(ctx, device, clk.Now().Unix()); err != nil || got != int64(limit) {
		t.Fatalf("full directory count=%d err=%v", got, err)
	}
	if count, accepted, err := delivery.PublishBotKeyPackages(ctx, session, installation.ID, capOperation, 1, capUpload); err != nil || accepted || count != 1 {
		t.Fatalf("lost-reply replay at capacity count=%d accepted=%v err=%v", count, accepted, err)
	}
	// Two genuine new references race for the final ordinary slot. The per-device lock must let
	// exactly one batch commit and make the other observe a full directory.
	if len(synthetic) == 0 {
		t.Fatal("cap fixture has no synthetic row to remove")
	}
	if err := repo.DeleteKeyPackage(ctx, device, synthetic[len(synthetic)-1]); err != nil {
		t.Fatal(err)
	}
	synthetic = synthetic[:len(synthetic)-1]
	type publishResult struct {
		accepted bool
		err      error
	}
	start := make(chan struct{})
	results := make(chan publishResult, 2)
	var publishers sync.WaitGroup
	for _, pkg := range []nativeBotKeyPackage{raceA, raceB} {
		body, err := cborx.Marshal([]any{[]any{pkg.blob(t)}, nil})
		if err != nil {
			t.Fatal(err)
		}
		publishers.Add(1)
		go func() {
			defer publishers.Done()
			<-start
			_, accepted, err := delivery.PublishBotKeyPackages(ctx, session, installation.ID, id.New(), 1, body)
			results <- publishResult{accepted: accepted, err: err}
		}()
	}
	close(start)
	publishers.Wait()
	close(results)
	wins := 0
	for result := range results {
		if result.err == nil && result.accepted {
			wins++
			continue
		}
		var refusal *ds.Error
		if !errors.As(result.err, &refusal) || refusal.Code != "E_TOO_LARGE" {
			t.Fatalf("concurrent capacity loser = %v, want E_TOO_LARGE", result.err)
		}
	}
	if wins != 1 {
		t.Fatalf("concurrent final-slot publishers accepted %d, want exactly one", wins)
	}
	for _, ref := range synthetic {
		if err := repo.DeleteKeyPackage(ctx, device, ref); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := delivery.TakeKeyPackage(ctx, session, device); err != nil {
		t.Fatalf("consume package: %v", err)
	}
	if _, err := delivery.TakeKeyPackage(ctx, session, device); err != nil {
		t.Fatalf("consume second package: %v", err)
	}
	if _, err := delivery.TakeKeyPackage(ctx, session, device); err != nil {
		t.Fatalf("consume concurrent winner: %v", err)
	}
	if got, err := repo.CountKeyPackages(ctx, device, clk.Now().Unix()); err != nil || got != 0 {
		t.Fatalf("active ordinary count=%d, err=%v, want 0", got, err)
	}
	postConsume, err := delivery.ManagedBotKeyPackageInventory(ctx, session, installation.ID)
	if err != nil || postConsume.OrdinaryActiveCount != 0 || postConsume.HasCurrentFallback != 1 {
		t.Fatalf("post-consumption inventory=%+v err=%v", postConsume, err)
	}
	if fallback, err := delivery.TakeKeyPackage(ctx, session, device); err != nil || !fallback.LastResort || !bytes.Equal(fallback.Blob, fallbackB.blob(t)) {
		t.Fatalf("inventory consumed or changed the current fallback: %+v err=%v", fallback, err)
	}
	{
		// Hold the restore fence through generation bump and purge. A concurrent inventory must
		// wait and then report one coherent post-restore generation/directory snapshot.
		fenceRepo := restoreRepo
		if fenceRepo == nil {
			fenceRepo = repo
		}
		locked := make(chan struct{})
		release := make(chan struct{})
		restoreDone := make(chan error, 1)
		go func() {
			restoreDone <- fenceRepo.Tx(ctx, func(tx store.Repository) error {
				if err := tx.LockInstanceGeneration(ctx); err != nil {
					return err
				}
				close(locked)
				<-release
				if err := tx.SetGeneration(ctx, 2); err != nil {
					return err
				}
				_, err := tx.PurgeKeyPackages(ctx, true)
				return err
			})
		}()
		<-locked
		staleDone := make(chan error, 1)
		go func() {
			_, _, err := delivery.PublishBotKeyPackages(ctx, session, installation.ID, id.New(), 1, upload)
			staleDone <- err
		}()
		inventoryDone := make(chan ds.BotKeyPackageInventory, 1)
		inventoryErr := make(chan error, 1)
		go func() {
			inventory, err := delivery.ManagedBotKeyPackageInventory(ctx, session, installation.ID)
			inventoryDone <- inventory
			inventoryErr <- err
		}()
		if monitorDB != nil {
			waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			waitForPostgresLockWait(t, waitCtx, monitorDB)
			cancel()
		} else {
			select {
			case err := <-staleDone:
				t.Fatalf("stale publication escaped held restore fence: %v", err)
			case inventory := <-inventoryDone:
				t.Fatalf("inventory escaped held restore fence: %+v", inventory)
			case <-time.After(75 * time.Millisecond):
			}
		}
		close(release)
		if err := <-restoreDone; err != nil {
			t.Fatal(err)
		}
		if err := <-staleDone; err == nil {
			t.Fatal("publication with stale generation was accepted after restore")
		}
		inventory := <-inventoryDone
		if err := <-inventoryErr; err != nil || inventory.Generation != 2 {
			t.Fatalf("inventory after restore = %+v err=%v, want generation 2", inventory, err)
		}
		if inventory.ServerTime != uint64(clk.Now().Unix()) {
			t.Fatalf("post-restore inventory server time=%d now=%d", inventory.ServerTime, clk.Now().Unix())
		}
	}
	count, accepted, err = delivery.PublishBotKeyPackages(ctx, session, installation.ID, operation, 1, upload)
	if err != nil || accepted || count != 2 {
		t.Fatalf("replay after consumption/restore count=%d accepted=%v err=%v", count, accepted, err)
	}
	if got, err := repo.CountKeyPackages(ctx, device, clk.Now().Unix()); err != nil || got != 0 {
		t.Fatalf("receipt replay revived consumed package; active count=%d, err=%v", got, err)
	}
	receipt, err := delivery.BotKeyPackagePublicationReceipt(ctx, session, installation.ID, operation)
	if err != nil || receipt.Generation != 1 || receipt.Count != 2 {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	other, _ := cborx.Marshal([]any{[]any{blob}, []byte("different-fallback")})
	if _, _, err := delivery.PublishBotKeyPackages(ctx, session, installation.ID, operation, 1, other); err == nil {
		t.Fatal("same operation with changed upload was accepted")
	}
	if _, _, err := delivery.PublishBotKeyPackages(ctx, session, installation.ID, id.New(), 1, upload); err == nil {
		t.Fatal("missing receipt admitted old generation")
	}
	if _, err := delivery.PublishKeyPackages(ctx, session, [][]byte{blob}, nil); err == nil {
		t.Fatal("generic publication accepted managed Bot")
	}
	if _, err := repo.GetBotKeyPackagePublicationReceipt(ctx, id.New(), operation); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-installation lookup = %v", err)
	}

	// A ready transition and receipt read share runtime -> community -> installation locks. The
	// same bearer is re-resolved to Ready after the transition, as auth.currentBotBinding does.
	readyLocked, finishReady := make(chan struct{}), make(chan struct{})
	readyDone := make(chan error, 1)
	go func() {
		readyDone <- repo.Tx(ctx, func(tx store.Repository) error {
			if err := tx.LockBotRuntime(ctx, *installation.RuntimeID); err != nil {
				return err
			}
			if err := tx.LockBotCommunity(ctx, community); err != nil {
				return err
			}
			if err := tx.LockBotInstallation(ctx, installation.ID); err != nil {
				return err
			}
			close(readyLocked)
			<-finishReady
			row, err := tx.GetBotInstallation(ctx, installation.ID)
			if err != nil {
				return err
			}
			row.State = store.BotInstallationReady
			row.Revision++
			row.Updated = clk.Now().Unix()
			return tx.UpdateBotInstallation(ctx, row.ID, row.Revision-1, row)
		})
	}()
	<-readyLocked
	receiptDone := make(chan error, 1)
	go func() {
		_, err := delivery.BotKeyPackagePublicationReceipt(ctx, session, installation.ID, operation)
		receiptDone <- err
	}()
	select {
	case err := <-receiptDone:
		t.Fatalf("receipt escaped the held lifecycle locks: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(finishReady)
	if err := <-readyDone; err != nil {
		t.Fatal(err)
	}
	if err := <-receiptDone; err == nil {
		t.Fatal("stale joining binding unexpectedly survived ready transition")
	}
	session.Bot.State = store.BotInstallationReady
	if _, err := delivery.BotKeyPackagePublicationReceipt(ctx, session, installation.ID, operation); err != nil {
		t.Fatalf("same bearer after current binding refresh could not read receipt: %v", err)
	}
	wrongScope := session
	wrongScope.Scope = auth.ScopeProvisional
	if _, _, err := delivery.PublishBotKeyPackages(ctx, wrongScope, installation.ID, id.New(), 2, upload); err == nil {
		t.Fatal("provisional session published a managed Bot KeyPackage")
	}
	if _, err := delivery.ManagedBotKeyPackageInventory(ctx, wrongScope, installation.ID); err == nil {
		t.Fatal("provisional session read managed Bot KeyPackage inventory")
	}
	disabledAt := clk.Now().Unix()
	if err := repo.SetUserDisabled(ctx, user, &disabledAt); err != nil {
		t.Fatal(err)
	}
	if _, err := delivery.BotKeyPackagePublicationReceipt(ctx, session, installation.ID, operation); err == nil {
		t.Fatal("disabled Bot retained receipt authority")
	}
	if _, err := delivery.ManagedBotKeyPackageInventory(ctx, session, installation.ID); err == nil {
		t.Fatal("disabled Bot retained inventory authority")
	}
	if err := repo.SetUserDisabled(ctx, user, nil); err != nil {
		t.Fatal(err)
	}

	// Advance the clock only after the publication has validated and acquired all lifecycle and
	// device locks. The final locked expiry check must refuse it even though WASI saw it as live.
	advanceBy := time.Duration(int64(info.NotAfter)+1-clk.Now().Unix()) * time.Second
	if advanceBy <= 0 {
		t.Fatal("native fixture package was already expired before the delayed-publication test")
	}
	var deviceLocks atomic.Int32
	hooked := &keyPackageHookRepo{Repository: repo, deviceLockHook: func() {
		if deviceLocks.Add(1) == 2 { // the first lock is preflight; the second is final admission
			clk.Advance(advanceBy)
		}
	}}
	expiringDS, err := ds.New(ds.Options{Store: hooked, Wasm: wasm, Clock: clk, Keys: testInstanceKeys(t), Policy: ds.DefaultPolicy()})
	if err != nil {
		t.Fatal(err)
	}
	defer expiringDS.Shutdown(context.Background())
	delayedOperation := id.New()
	if _, _, err := expiringDS.PublishBotKeyPackages(ctx, session, installation.ID, delayedOperation, 2, upload); err == nil || !strings.Contains(err.Error(), "expired while publication was waiting") {
		t.Fatalf("delayed publication error=%v, want locked expiry refusal", err)
	}
	expiredInventory, err := delivery.ManagedBotKeyPackageInventory(ctx, session, installation.ID)
	if err != nil || expiredInventory.OrdinaryActiveCount != 0 || expiredInventory.HasCurrentFallback != 0 {
		t.Fatalf("expired package inventory=%+v err=%v", expiredInventory, err)
	}
	if _, err := repo.GetBotKeyPackagePublicationReceipt(ctx, installation.ID, delayedOperation); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired delayed publication left receipt: %v", err)
	}
	if err := repo.RevokeDevice(ctx, device, clk.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := delivery.BotKeyPackagePublicationReceipt(ctx, session, installation.ID, operation); err == nil {
		t.Fatal("revoked device retained historical receipt authority")
	}
	if _, err := delivery.ManagedBotKeyPackageInventory(ctx, session, installation.ID); err == nil {
		t.Fatal("revoked device retained inventory authority")
	}
}

// keyPackageHookRepo allows a focused test to move the fake clock at the exact store boundary
// between WASI validation and the final in-transaction expiry check.
type keyPackageHookRepo struct {
	store.Repository
	deviceLockHook func()
}

func (r *keyPackageHookRepo) Tx(ctx context.Context, fn func(store.Repository) error) error {
	return r.Repository.Tx(ctx, func(tx store.Repository) error {
		return fn(&keyPackageHookRepo{Repository: tx, deviceLockHook: r.deviceLockHook})
	})
}

func (r *keyPackageHookRepo) LockKeyPackageDevice(ctx context.Context, device id.ID) error {
	if err := r.Repository.LockKeyPackageDevice(ctx, device); err != nil {
		return err
	}
	if r.deviceLockHook != nil {
		r.deviceLockHook()
	}
	return nil
}

func newManagedPostgresDS(t *testing.T, dsn string) (store.Repository, store.Repository, *sql.DB, *ds.DS, *mlswasi.Runtime, *clock.Fake) {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	adminURL := *u
	adminURL.Path = "/postgres"
	admin, err := postgres.Open(adminURL.String(), 2, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	name := "dilla_kp_" + id.New().String()[:12]
	if _, err := admin.ExecContext(context.Background(), "CREATE DATABASE "+name); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = admin.Close()
	})
	u.Path = "/" + name
	db, err := postgres.Open(u.String(), 4, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, pgmigrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	repo := postgres.New(db)
	t.Cleanup(func() { _ = repo.Close() })
	clk := clock.NewFake(time.Unix(time.Now().Unix(), 0))
	keys := testInstanceKeys(t)
	wasm, err := mlswasi.New(context.Background(), loadWasmForDS(t), mlswasi.Options{PoolSize: 1, CacheDir: sharedWasmCacheDir(t), Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	service, err := ds.New(ds.Options{Store: repo, Wasm: wasm, Clock: clk, Keys: keys, Policy: ds.DefaultPolicy()})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateInstance(context.Background(), store.InstanceRow{InstanceID: keys.InstanceID, ExternalSenderKeyID: keys.ExternalSenderKeyID, KeyHistory: []byte{0x80}, FrankingKeyID: keys.FrankingKeyID, Generation: 1, PolicyVersion: 1, Created: clk.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	secondDB, err := postgres.Open(u.String(), 4, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	secondRepo := postgres.New(secondDB)
	t.Cleanup(func() { _ = secondRepo.Close() })
	return repo, secondRepo, secondDB, service, wasm, clk
}

func waitForPostgresLockWait(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND state='active' AND wait_event_type='Lock'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("publisher did not block behind restore fence: %v", ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

type nativeBotKeyPackageFixture struct {
	GeneratedAt      int64                      `json:"generated_at"`
	UserIDHex        string                     `json:"user_id_hex"`
	DeviceIDHex      string                     `json:"device_id_hex"`
	UMKPublicHex     string                     `json:"umk_public_hex"`
	SSKPublicHex     string                     `json:"ssk_public_hex"`
	SigUMKSSKHex     string                     `json:"sig_umk_ssk_hex"`
	DSKPublicHex     string                     `json:"dsk_public_hex"`
	CredentialHex    string                     `json:"credential_hex"`
	SignedDeviceList string                     `json:"signed_device_list_hex"`
	Batches          []nativeBotKeyPackageBatch `json:"batches"`
}

type nativeBotKeyPackageBatch struct {
	Ordinary []nativeBotKeyPackage `json:"ordinary"`
	Fallback nativeBotKeyPackage   `json:"fallback"`
}

type nativeBotKeyPackage struct {
	KeyPackageBase64 string `json:"key_package_base64"`
	KeyPackageRefHex string `json:"key_package_ref_hex"`
	LastResort       bool   `json:"last_resort"`
}

func loadNativeBotKeyPackageFixture(t *testing.T) nativeBotKeyPackageFixture {
	t.Helper()
	raw, err := os.ReadFile("../mlswasi/testdata/native_bot_keypackages.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture nativeBotKeyPackageFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Batches) != 2 {
		t.Fatalf("native Bot fixture batch count = %d", len(fixture.Batches))
	}
	if len(fixture.Batches[0].Ordinary) != 32 || len(fixture.Batches[1].Ordinary) != 32 {
		t.Fatalf("native Bot fixture ordinary batch lengths = %d / %d", len(fixture.Batches[0].Ordinary), len(fixture.Batches[1].Ordinary))
	}
	return fixture
}

func (f nativeBotKeyPackageFixture) decode(t *testing.T, value string) []byte {
	t.Helper()
	b, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (f nativeBotKeyPackageFixture) rows(t *testing.T) (id.ID, id.ID, []byte, []nativeBotKeyPackageBatch) {
	t.Helper()
	var user, device id.ID
	userBytes := f.decode(t, f.UserIDHex)
	deviceBytes := f.decode(t, f.DeviceIDHex)
	if len(userBytes) != id.Size || len(deviceBytes) != id.Size {
		t.Fatalf("native fixture identity lengths user=%d device=%d", len(userBytes), len(deviceBytes))
	}
	copy(user[:], userBytes)
	copy(device[:], deviceBytes)
	return device, user, f.decode(t, f.CredentialHex), f.Batches
}

func (p nativeBotKeyPackage) blob(t *testing.T) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(p.KeyPackageBase64)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func validateNativePackage(t *testing.T, wasm *mlswasi.Runtime, pkg nativeBotKeyPackage) mlswasi.KeyPackageInfo {
	t.Helper()
	inst, err := wasm.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Release()
	info, err := inst.ValidateKeyPackage(context.Background(), pkg.blob(t))
	if err != nil {
		t.Fatalf("validate native Bot KeyPackage: %v", err)
	}
	if hex.EncodeToString(info.KPRef) != pkg.KeyPackageRefHex {
		t.Fatalf("WASI KeyPackage reference disagrees with native fixture")
	}
	return info
}
