package store_test

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/store/postgres"
	pgmigrations "github.com/dilla-chat/dilla/internal/store/postgres/migrations"
	"github.com/dilla-chat/dilla/internal/store/sqlite"
	sqlitemigrations "github.com/dilla-chat/dilla/internal/store/sqlite/migrations"
)

// engines returns one opened, migrated repository per engine available here, each
// in storage of the calling test's own: a fresh SQLite file, and a fresh Postgres
// database (freshPostgresDSN) when DILLA_TEST_PG names a server. rawDBs lets a conformance assertion read a column the Repository interface
// does not expose. There is exactly one such column — instance_settings.updated
// — and it is worth asserting because a repository that wrote 0 there would
// pass every round-trip test (deviation ID11).
var (
	rawMu       sync.Mutex
	rawDBs      = map[store.Repository]*sql.DB{}
	rawWriteDBs = map[store.Repository]*sql.DB{}
)

func rawDB(t *testing.T, repo store.Repository) *sql.DB {
	t.Helper()
	rawMu.Lock()
	defer rawMu.Unlock()
	db, ok := rawDBs[repo]
	if !ok {
		t.Fatal("no raw handle registered for this repository")
	}
	return db
}

// freshPostgresDSN creates a database named for this test alone on the server dsn names and
// returns a DSN for it. The database is dropped WITH (FORCE) at cleanup, after the
// repository's own Cleanup has closed its pool (cleanups run last-registered first).
//
// Every Postgres leg in this package runs in such a database. Sharing the one CI database
// broke three ways: a test that asserts on an instance-wide listing (ListCommunities,
// ListPendingJoinGroups, ListBlobRetentionPolicies, ListBlobRefsOfDeletedChannels, an empty
// report queue, the single instance row) saw every other test's rows; a fixed key (an invite
// code hash, a blob digest) collided on a second run; and two packages migrating the same
// empty database at once failed with "relation instances already exists".
func freshPostgresDSN(t *testing.T, dsn string) string {
	t.Helper()
	admin, err := postgres.Open(dsn, 1, time.Hour)
	if err != nil {
		t.Fatalf("postgres Open: %v", err)
	}
	name := "dilla_t_" + id.New().String()[:16]
	if _, err := admin.ExecContext(context.Background(), `CREATE DATABASE `+name); err != nil {
		_ = admin.Close()
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
		_ = admin.Close()
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("DILLA_TEST_PG is not a URL: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

func engines(t *testing.T) map[string]store.Repository {
	t.Helper()
	out := map[string]store.Repository{}

	path := filepath.Join(t.TempDir(), "conformance.db")
	write, err := sqlite.OpenWrite(path)
	if err != nil {
		t.Fatalf("sqlite OpenWrite: %v", err)
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, write, sqlitemigrations.FS)
	if err != nil {
		t.Fatalf("sqlite provider: %v", err)
	}
	if _, err := p.Up(context.Background()); err != nil {
		t.Fatalf("sqlite up: %v", err)
	}
	read, err := sqlite.OpenRead(path)
	if err != nil {
		t.Fatalf("sqlite OpenRead: %v", err)
	}
	repo := sqlite.New(write, read)
	t.Cleanup(func() { repo.Close() })
	out["sqlite"] = repo
	rawMu.Lock()
	rawDBs[repo] = read
	rawWriteDBs[repo] = write
	rawMu.Unlock()

	dsn := os.Getenv("DILLA_TEST_PG")
	if dsn == "" {
		t.Log("DILLA_TEST_PG is unset: no local Postgres server on this box; CI's postgres service container runs the Postgres leg")
		return out
	}
	db, err := postgres.Open(freshPostgresDSN(t, dsn), 8, time.Hour)
	if err != nil {
		t.Fatalf("postgres Open: %v", err)
	}
	pp, err := goose.NewProvider(goose.DialectPostgres, db, pgmigrations.FS)
	if err != nil {
		t.Fatalf("postgres provider: %v", err)
	}
	if _, err := pp.Up(context.Background()); err != nil {
		t.Fatalf("postgres up: %v", err)
	}
	pgrepo := postgres.New(db)
	t.Cleanup(func() { pgrepo.Close() })
	out["postgres"] = pgrepo
	rawMu.Lock()
	rawDBs[pgrepo] = db
	rawWriteDBs[pgrepo] = db
	rawMu.Unlock()
	return out
}

func seedUser(ctx context.Context, t *testing.T, repo store.Repository) store.UserRow {
	t.Helper()
	u := store.UserRow{
		ID: id.New(), Username: "jonas" + id.New().String()[:8], Display: "Jonas",
		Kind: 0, UMKPub: make([]byte, 32), SSKPub: make([]byte, 32),
		SigUMKSSK: make([]byte, 64), Created: 1_700_000_000,
	}
	if err := repo.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return u
}

func TestRepositoryConformance(t *testing.T) {
	ctx := context.Background()
	for name, repo := range engines(t) {
		t.Run(name, func(t *testing.T) {
			t.Run("instance", func(t *testing.T) {
				in := store.InstanceRow{
					InstanceID: id.New(), ExternalSenderKeyID: id.New(), KeyHistory: []byte{1},
					FrankingKeyID: id.New(), Generation: 1, PolicyVersion: 1, Created: 1,
				}
				if err := repo.CreateInstance(ctx, in); err != nil {
					t.Fatalf("CreateInstance: %v", err)
				}
				got, err := repo.GetInstance(ctx)
				if err != nil {
					t.Fatalf("GetInstance: %v", err)
				}
				if got.InstanceID != in.InstanceID {
					t.Fatalf("instance id %s != %s", got.InstanceID, in.InstanceID)
				}
				gen, err := repo.BumpGeneration(ctx)
				if err != nil {
					t.Fatalf("BumpGeneration: %v", err)
				}
				if gen != 2 {
					t.Fatalf("generation = %d, want 2", gen)
				}
				if err := repo.PutSetting(ctx, "k", []byte("v"), 4242); err != nil {
					t.Fatalf("PutSetting: %v", err)
				}
				v, err := repo.GetSetting(ctx, "k")
				if err != nil || string(v) != "v" {
					t.Fatalf("GetSetting = %q, %v", v, err)
				}
				if _, err := repo.GetSetting(ctx, "absent"); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("GetSetting(absent) = %v, want ErrNotFound", err)
				}
				// instance_settings.updated is NOT NULL and must carry the
				// caller's timestamp, not 0 (deviation ID11).
				var updated int64
				if err := rawDB(t, repo).QueryRowContext(ctx,
					`SELECT updated FROM instance_settings WHERE key = 'k'`).Scan(&updated); err != nil {
					t.Fatalf("read updated: %v", err)
				}
				if updated != 4242 {
					t.Fatalf("instance_settings.updated = %d, want 4242", updated)
				}
				if err := repo.PutSetting(ctx, "k", []byte("v2"), 9999); err != nil {
					t.Fatalf("second PutSetting: %v", err)
				}
				if err := rawDB(t, repo).QueryRowContext(ctx,
					`SELECT updated FROM instance_settings WHERE key = 'k'`).Scan(&updated); err != nil {
					t.Fatalf("read updated again: %v", err)
				}
				if updated != 9999 {
					t.Fatalf("a second PutSetting left updated at %d", updated)
				}
				// E8: a server's Giphy setting is a JSON value under a per-server key, written and replaced in a
				// transaction on both engines (the value is bytes, not parsed by the store).
				gifKey := "community_gifs:" + id.New().String()
				for _, value := range []string{`{"key":"abcdEFGH1234","rating":"pg-13"}`, `{"key":"","rating":"g"}`, `{}`} {
					if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.PutSetting(ctx, gifKey, []byte(value), 5) }); err != nil {
						t.Fatalf("PutSetting %s in a transaction: %v", value, err)
					}
					got, err := repo.GetSetting(ctx, gifKey)
					if err != nil || string(got) != value {
						t.Fatalf("GetSetting(%s) = %q, %v; want %q", gifKey, got, err, value)
					}
				}
			})

			t.Run("accounts and devices", func(t *testing.T) {
				u := seedUser(ctx, t, repo)
				got, err := repo.GetUser(ctx, u.ID)
				if err != nil {
					t.Fatalf("GetUser: %v", err)
				}
				if got.Username != u.Username {
					t.Fatalf("username %q != %q", got.Username, u.Username)
				}
				if _, err := repo.GetUser(ctx, id.New()); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("GetUser(absent) = %v, want ErrNotFound", err)
				}
				dup := u
				dup.ID = id.New()
				if err := repo.CreateUser(ctx, dup); !errors.Is(err, store.ErrConflict) {
					t.Fatalf("duplicate username = %v, want ErrConflict", err)
				}
				d := store.DeviceRow{
					ID: id.New(), UserID: u.ID, DSKPub: make([]byte, 32), Tier: 0, SignerTier: 0,
					CredentialBlob: []byte{9}, LastSeen: 5, Created: 5,
				}
				if err := repo.CreateDevice(ctx, d); err != nil {
					t.Fatalf("CreateDevice: %v", err)
				}
				list, err := repo.ListDevicesByUser(ctx, u.ID)
				if err != nil || len(list) != 1 {
					t.Fatalf("ListDevicesByUser = %d, %v", len(list), err)
				}
				if err := repo.TouchDevice(ctx, d.ID, 99); err != nil {
					t.Fatalf("TouchDevice: %v", err)
				}
				again, _ := repo.GetDevice(ctx, d.ID)
				if again.LastSeen != 99 {
					t.Fatalf("last_seen = %d, want 99", again.LastSeen)
				}
			})

			t.Run("sessions die with their device", func(t *testing.T) {
				u := seedUser(ctx, t, repo)
				d := store.DeviceRow{ID: id.New(), UserID: u.ID, DSKPub: make([]byte, 32),
					CredentialBlob: []byte{1}, LastSeen: 1, Created: 1}
				if err := repo.CreateDevice(ctx, d); err != nil {
					t.Fatalf("CreateDevice: %v", err)
				}
				hash := make([]byte, 32)
				hash[0] = 7
				s := store.SessionRow{TokenHash: hash, DeviceID: d.ID, UserID: u.ID, Scope: 0, Tier: 0,
					Created: 10, Expires: 1_000_000, IdleExpires: 1_000_000}
				if err := repo.CreateSession(ctx, s); err != nil {
					t.Fatalf("CreateSession: %v", err)
				}
				if _, err := repo.GetSessionByHash(ctx, hash, 20); err != nil {
					t.Fatalf("GetSessionByHash: %v", err)
				}
				if err := repo.SetSessionVersions(ctx, hash, 3, 1, 1); err != nil {
					t.Fatalf("SetSessionVersions: %v", err)
				}
				selected, err := repo.GetSessionByHash(ctx, hash, 20)
				if err != nil || selected.WireVersion != 3 || selected.E2EEVersion != 1 || selected.MediaVersion != 1 {
					t.Fatalf("session versions = [%d %d %d], %v; want [3 1 1]", selected.WireVersion, selected.E2EEVersion, selected.MediaVersion, err)
				}
				missing := make([]byte, 32)
				missing[0] = 8
				if err := repo.SetSessionVersions(ctx, missing, 2, 1, 1); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("SetSessionVersions for missing bearer = %v, want ErrNotFound", err)
				}
				n, err := repo.DeleteSessionsByDevice(ctx, d.ID)
				if err != nil || n != 1 {
					t.Fatalf("DeleteSessionsByDevice = %d, %v", n, err)
				}
				if _, err := repo.GetSessionByHash(ctx, hash, 20); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("session survived revocation: %v", err)
				}
			})

			t.Run("invite redemption is exhaustible", func(t *testing.T) {
				hash := make([]byte, 32)
				hash[1] = 3
				inv := store.InviteRow{ID: id.New(), CodeHash: hash, GrantsAdmin: 1,
					MaxUses: 1, Created: 1, ExpiresAt: 1_000_000}
				if err := repo.CreateInvite(ctx, inv); err != nil {
					t.Fatalf("CreateInvite: %v", err)
				}
				if _, err := repo.RedeemInvite(ctx, hash, 20); err != nil {
					t.Fatalf("first redeem: %v", err)
				}
				if _, err := repo.RedeemInvite(ctx, hash, 20); !errors.Is(err, store.ErrExhausted) {
					t.Fatalf("second redeem = %v, want ErrExhausted", err)
				}
			})

			t.Run("Tx rolls back and refuses nesting", func(t *testing.T) {
				u := store.UserRow{ID: id.New(), Username: "rollback" + id.New().String()[:8],
					Display: "x", UMKPub: make([]byte, 32), SSKPub: make([]byte, 32),
					SigUMKSSK: make([]byte, 64), Created: 1}
				boom := errors.New("boom")
				err := repo.Tx(ctx, func(tx store.Repository) error {
					if err := tx.CreateUser(ctx, u); err != nil {
						return err
					}
					return boom
				})
				if !errors.Is(err, boom) {
					t.Fatalf("Tx returned %v, want boom", err)
				}
				if _, err := repo.GetUser(ctx, u.ID); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("rolled-back user survived: %v", err)
				}
				err = repo.Tx(ctx, func(tx store.Repository) error {
					return tx.Tx(ctx, func(store.Repository) error { return nil })
				})
				if err == nil {
					t.Fatal("a nested Tx must be an error")
				}
				// A multi-statement METHOD, however, must join the open
				// transaction rather than refuse it: PutRecoveryCodes and
				// TakeCeremony open their own Tx only when !inTx (task 3 step 6).
				u2 := seedUser(ctx, t, repo)
				if err := repo.Tx(ctx, func(tx store.Repository) error {
					return tx.PutRecoveryCodes(ctx, u2.ID, [][]byte{make([]byte, 32)}, 1)
				}); err != nil {
					t.Fatalf("PutRecoveryCodes inside a Tx: %v", err)
				}
				n, err := repo.CountRecoveryCodes(ctx, u2.ID)
				if err != nil || n != 1 {
					t.Fatalf("CountRecoveryCodes = %d, %v", n, err)
				}
				cid := id.New()
				if err := repo.PutCeremony(ctx, store.CeremonyRow{ID: cid, Kind: 0,
					SessionJSON: `{"challenge":"x"}`, Created: 1, Expires: 1_000_000}); err != nil {
					t.Fatalf("PutCeremony: %v", err)
				}
				if err := repo.Tx(ctx, func(tx store.Repository) error {
					_, err := tx.TakeCeremony(ctx, cid, 10)
					return err
				}); err != nil {
					t.Fatalf("TakeCeremony inside a Tx: %v", err)
				}
			})

			t.Run("audit and schema version", func(t *testing.T) {
				actor := id.New()
				if err := repo.Audit(ctx, store.AuditRow{Actor: &actor, Action: "test",
					Target: "none", Detail: "", At: 42}); err != nil {
					t.Fatalf("Audit: %v", err)
				}
				rows, err := repo.ListAudit(ctx, 0, 10)
				if err != nil || len(rows) == 0 {
					t.Fatalf("ListAudit = %d, %v", len(rows), err)
				}
				v, err := repo.SchemaVersion(ctx)
				if err != nil {
					t.Fatalf("SchemaVersion: %v", err)
				}
				if want := wantSchemaVersion(t); v != want {
					t.Fatalf("schema version = %d, want %d", v, want)
				}
			})
		})
	}
}

func TestManualWarningActionReceiptRoundTrip(t *testing.T) {
	ctx := context.Background()
	for name, repo := range engines(t) {
		t.Run(name, func(t *testing.T) {
			actor := seedUser(ctx, t, repo)
			target := seedUser(ctx, t, repo)
			community := store.CommunityRow{ID: id.New(), Owner: actor.ID, Name: "warning receipt", PolicyJSON: []byte("{}"), PolicyVersion: 1, Created: 1}
			if err := repo.CreateCommunity(ctx, community); err != nil {
				t.Fatal(err)
			}
			device := store.DeviceRow{ID: id.New(), UserID: actor.ID, DSKPub: make([]byte, 32), CredentialBlob: []byte{1}, Created: 1}
			if err := repo.CreateDevice(ctx, device); err != nil {
				t.Fatal(err)
			}
			row := store.ManualWarningActionRow{CommunityID: community.ID, RequestID: id.New(), ActionID: id.New(),
				ActorUserID: actor.ID, ActorDeviceID: device.ID, TargetUserID: target.ID, Revision: 4, AcceptedAt: 2}
			if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.PutManualWarningAction(ctx, row) }); err != nil {
				t.Fatalf("PutManualWarningAction: %v", err)
			}
			got, err := repo.GetManualWarningAction(ctx, community.ID, row.RequestID)
			if err != nil || got != row {
				t.Fatalf("GetManualWarningAction = %+v, %v; want %+v", got, err, row)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.PutManualWarningAction(ctx, row) }); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("duplicate PutManualWarningAction = %v, want conflict", err)
			}
		})
	}
}

func TestOneInviteWinsUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	for name, repo := range engines(t) {
		t.Run(name, func(t *testing.T) {
			hash := make([]byte, 32)
			hash[2] = 11
			if err := repo.CreateInvite(ctx, store.InviteRow{ID: id.New(), CodeHash: hash,
				MaxUses: 1, Created: 1, ExpiresAt: 1_000_000}); err != nil {
				t.Fatalf("CreateInvite: %v", err)
			}
			var wg sync.WaitGroup
			results := make(chan error, 64)
			for i := 0; i < 64; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, err := repo.RedeemInvite(ctx, hash, 20)
					results <- err
				}()
			}
			wg.Wait()
			close(results)
			won := 0
			for err := range results {
				switch {
				case err == nil:
					won++
				case errors.Is(err, store.ErrExhausted):
				default:
					t.Fatalf("unexpected redemption error: %v", err)
				}
			}
			if won != 1 {
				t.Fatalf("%d redemptions won, want exactly 1", won)
			}
		})
	}
}

// A backup code is consumed once under concurrency (web3.7 3f, R20): N goroutines released together
// spend the SAME code; exactly one succeeds and every other one is ErrNotFound (used). The
// `used_at IS NULL` guard in the UPDATE is what decides it on both engines.
func TestABackupCodeIsConsumedOnceUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	for name, repo := range engines(t) {
		t.Run(name, func(t *testing.T) {
			u := seedUser(ctx, t, repo)
			hash := make([]byte, 32)
			for i := range hash {
				hash[i] = 0x5c
			}
			if err := repo.PutRecoveryCodes(ctx, u.ID, [][]byte{hash}, 1); err != nil {
				t.Fatalf("PutRecoveryCodes: %v", err)
			}
			const n = 16
			var wg sync.WaitGroup
			start := make(chan struct{})
			results := make(chan error, n)
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					results <- repo.ConsumeRecoveryCode(ctx, u.ID, hash, int64(10+i))
				}()
			}
			close(start)
			wg.Wait()
			close(results)
			won := 0
			for err := range results {
				switch {
				case err == nil:
					won++
				case errors.Is(err, store.ErrNotFound):
				default:
					t.Fatalf("unexpected consume error: %v", err)
				}
			}
			if won != 1 {
				t.Fatalf("%d of %d concurrent consumes of one backup code succeeded, want exactly 1", won, n)
			}
		})
	}
}

// TestTxRollsBackOnPanic pins the one thing a deferred rollback buys that an
// explicit one does not: a panic inside fn must still close the transaction.
// On SQLite the write pool is a single connection, so an abandoned *sql.Tx
// holds it forever and every later write in the process blocks on the pool
// rather than on SQLite's busy_timeout. database/sql's awaitDone goroutine
// rescues only a caller that passed a cancellable context; this test passes
// context.Background(), which is what every CLI verb does.
func TestTxRollsBackOnPanic(t *testing.T) {
	ctx := context.Background()
	for name, repo := range engines(t) {
		t.Run(name, func(t *testing.T) {
			u := store.UserRow{ID: id.New(), Username: "panic" + id.New().String()[:8],
				Display: "x", UMKPub: make([]byte, 32), SSKPub: make([]byte, 32),
				SigUMKSSK: make([]byte, 64), Created: 1}

			var recovered any
			func() {
				defer func() { recovered = recover() }()
				_ = repo.Tx(ctx, func(tx store.Repository) error {
					if err := tx.CreateUser(ctx, u); err != nil {
						t.Errorf("CreateUser inside Tx: %v", err)
					}
					panic("boom")
				})
			}()
			if recovered != "boom" {
				t.Fatalf("the panic did not propagate out of Tx: %v", recovered)
			}

			if _, err := repo.GetUser(ctx, u.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("the panicking transaction was not rolled back: GetUser = %v", err)
			}

			// The pool must still take a write. Without the deferred rollback
			// this blocks forever on SQLite.
			done := make(chan error, 1)
			go func() {
				done <- repo.CreateUser(context.Background(), store.UserRow{
					ID: id.New(), Username: "after" + id.New().String()[:8],
					Display: "x", UMKPub: make([]byte, 32), SSKPub: make([]byte, 32),
					SigUMKSSK: make([]byte, 64), Created: 2})
			}()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("a write after a panicking Tx: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the write pool is still held by the panicking transaction")
			}
		})
	}
}
