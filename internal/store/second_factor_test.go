package store_test

// second_factor_test.go pins web3.6 S1's store half: users.factor_owed, its backfill in migration
// 00017, the boot reconcile (MarkFactorOwedWithoutFactor sets the flag, ClearFactorOwedWithFactor clears it),
// the flag riding along on GetSessionByHash, and the factor deletes S2 and S3 build on.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
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

// factorUser creates a user through the repository with the given kind and owed flag.
func factorUser(ctx context.Context, t *testing.T, repo store.Repository, kind uint8, owed bool) id.ID {
	t.Helper()
	u := store.UserRow{
		ID: id.New(), Username: "u" + id.New().String()[:12], Display: "U", Kind: kind,
		UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64),
		Created: 1_700_000_000, FactorOwed: owed,
	}
	if err := repo.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return u.ID
}

// userBefore17 writes a users row by hand into a database migrated to before 00017, whose users
// table has no factor_owed column for the repository's CreateUser to write. pg selects Postgres's
// placeholders.
func userBefore17(ctx context.Context, t *testing.T, db *sql.DB, name string, kind int) id.ID {
	return userBefore17On(ctx, t, db, false, name, kind)
}

func userBefore17On(ctx context.Context, t *testing.T, db *sql.DB, pg bool, name string, kind int) id.ID {
	t.Helper()
	uid := id.New()
	q := `INSERT INTO users (id, username, display, kind, umk_pub, ssk_pub, sig_umk_ssk, created)
		VALUES (?, ?, ?, ?, ?, ?, ?, 1)`
	if pg {
		q = `INSERT INTO users (id, username, display, kind, umk_pub, ssk_pub, sig_umk_ssk, created)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 1)`
	}
	if _, err := db.ExecContext(ctx, q, uid, name, name, kind, make([]byte, 32), make([]byte, 32), make([]byte, 64)); err != nil {
		t.Fatalf("insert %s: %v", name, err)
	}
	return uid
}

func putTOTP(ctx context.Context, t *testing.T, repo store.Repository, user id.ID, confirmed bool) {
	t.Helper()
	row := store.TOTPRow{UserID: user, Secret: []byte("0123456789abcdef0123"), Digits: 6, Period: 30,
		Algorithm: "SHA1", Created: 1}
	if confirmed {
		at := int64(2)
		row.ConfirmedAt = &at
	}
	if err := repo.PutTOTP(ctx, row); err != nil {
		t.Fatalf("PutTOTP: %v", err)
	}
}

func putPasskey(ctx context.Context, t *testing.T, repo store.Repository, user id.ID, rpID string, credID []byte) {
	t.Helper()
	if err := repo.PutWebauthnCredential(ctx, store.WebauthnCredentialRow{
		CredID: credID, RPID: rpID, UserID: user, PublicKey: []byte{4, 4, 4}, Flags: []byte{1},
		ExtensionsJSON: "{}", Created: 3,
	}); err != nil {
		t.Fatalf("PutWebauthnCredential: %v", err)
	}
}

func owed(ctx context.Context, t *testing.T, repo store.Repository, user id.ID) bool {
	t.Helper()
	u, err := repo.GetUser(ctx, user)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	return u.FactorOwed
}

// The backfill of migration 00017: a live human account without a confirmed TOTP secret or a
// passkey owes a second factor; every other account does not. On both engines.
func TestTheMigrationBackfillsTheOwedFlag(t *testing.T) {
	ctx := context.Background()
	t.Run("sqlite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "before-00017.db")
		write, err := sqlite.OpenWrite(path)
		if err != nil {
			t.Fatalf("OpenWrite: %v", err)
		}
		p, err := goose.NewProvider(goose.DialectSQLite3, write, sqlitemigrations.FS)
		if err != nil {
			t.Fatalf("provider: %v", err)
		}
		read, err := sqlite.OpenRead(path)
		if err != nil {
			t.Fatalf("OpenRead: %v", err)
		}
		repo := sqlite.New(write, read)
		t.Cleanup(func() { repo.Close() })
		checkBackfill(ctx, t, p, repo, write, false)
	})
	t.Run("postgres", func(t *testing.T) {
		dsn := os.Getenv("DILLA_TEST_PG")
		if dsn == "" {
			t.Skip("DILLA_TEST_PG is unset: no local Postgres server on this box; CI's postgres service container runs the Postgres leg")
		}
		db, err := postgres.Open(freshPostgresDSN(t, dsn), 4, time.Hour)
		if err != nil {
			t.Fatalf("postgres Open: %v", err)
		}
		p, err := goose.NewProvider(goose.DialectPostgres, db, pgmigrations.FS)
		if err != nil {
			t.Fatalf("provider: %v", err)
		}
		repo := postgres.New(db)
		t.Cleanup(func() { repo.Close() })
		checkBackfill(ctx, t, p, repo, db, true)
	})
}

// The rollback of docs/deploy §3 (web3.6 review I1): `dillad migrate down --to=16` drops only
// users.factor_owed. The accounts, TOTP secrets, passkeys and backup codes are kept, so web3.5
// runs on the data as before; a second upgrade's backfill marks the account without a factor again.
func TestMigratingDownTo16KeepsTheFactorsAndDropsOnlyTheMark(t *testing.T) {
	ctx := context.Background()
	t.Run("sqlite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "down-to-16.db")
		write, err := sqlite.OpenWrite(path)
		if err != nil {
			t.Fatalf("OpenWrite: %v", err)
		}
		p, err := goose.NewProvider(goose.DialectSQLite3, write, sqlitemigrations.FS)
		if err != nil {
			t.Fatalf("provider: %v", err)
		}
		read, err := sqlite.OpenRead(path)
		if err != nil {
			t.Fatalf("OpenRead: %v", err)
		}
		repo := sqlite.New(write, read)
		t.Cleanup(func() { repo.Close() })
		checkDownTo16(ctx, t, p, repo, write, false)
	})
	t.Run("postgres", func(t *testing.T) {
		dsn := os.Getenv("DILLA_TEST_PG")
		if dsn == "" {
			t.Skip("DILLA_TEST_PG is unset: no local Postgres server on this box; CI's postgres service container runs the Postgres leg")
		}
		db, err := postgres.Open(freshPostgresDSN(t, dsn), 4, time.Hour)
		if err != nil {
			t.Fatalf("postgres Open: %v", err)
		}
		p, err := goose.NewProvider(goose.DialectPostgres, db, pgmigrations.FS)
		if err != nil {
			t.Fatalf("provider: %v", err)
		}
		repo := postgres.New(db)
		t.Cleanup(func() { repo.Close() })
		checkDownTo16(ctx, t, p, repo, db, true)
	})
}

func checkDownTo16(ctx context.Context, t *testing.T, p *goose.Provider, repo store.Repository, db *sql.DB, pg bool) {
	t.Helper()
	if _, err := p.UpTo(ctx, 17); err != nil {
		t.Fatalf("up to 17: %v", err)
	}
	withTOTP := factorUser(ctx, t, repo, 0, false)
	putTOTP(ctx, t, repo, withTOTP, true)
	if err := repo.PutRecoveryCodes(ctx, withTOTP, [][]byte{bytes.Repeat([]byte{0x51}, 32)}, 1); err != nil {
		t.Fatalf("PutRecoveryCodes: %v", err)
	}
	withPasskey := factorUser(ctx, t, repo, 0, false)
	putPasskey(ctx, t, repo, withPasskey, "dilla.example", bytes.Repeat([]byte{0x52}, 32))
	bare := factorUser(ctx, t, repo, 0, true)

	if _, err := p.DownTo(ctx, 16); err != nil {
		t.Fatalf("down to 16: %v", err)
	}
	count := func(q string) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	for q, want := range map[string]int{
		"SELECT COUNT(*) FROM users":                                       3,
		"SELECT COUNT(*) FROM totp_secrets WHERE confirmed_at IS NOT NULL": 1,
		"SELECT COUNT(*) FROM webauthn_credentials":                        1,
		"SELECT COUNT(*) FROM recovery_codes":                              1,
	} {
		if got := count(q); got != want {
			t.Errorf("after the down migration, %s = %d, want %d", q, got, want)
		}
	}
	column := "SELECT COUNT(*) FROM pragma_table_info('users') WHERE name = 'factor_owed'"
	if pg {
		column = "SELECT COUNT(*) FROM information_schema.columns WHERE table_name = 'users' AND column_name = 'factor_owed'"
	}
	if got := count(column); got != 0 {
		t.Fatalf("users.factor_owed survived the down migration")
	}

	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("the second upgrade: %v", err)
	}
	for _, c := range []struct {
		name string
		user id.ID
		want bool
	}{
		{"a confirmed TOTP", withTOTP, false},
		{"a passkey", withPasskey, false},
		{"no factor", bare, true},
	} {
		if got := owed(ctx, t, repo, c.user); got != c.want {
			t.Errorf("%s: factor_owed = %v after the second upgrade, want %v", c.name, got, c.want)
		}
	}
}

func checkBackfill(ctx context.Context, t *testing.T, p *goose.Provider, repo store.Repository, db *sql.DB, pg bool) {
	t.Helper()
	if _, err := p.UpTo(ctx, 16); err != nil {
		t.Fatalf("up to 16: %v", err)
	}
	// users has no factor_owed column yet, so the rows are written by hand.
	user := func(name string, kind int) id.ID { return userBefore17On(ctx, t, db, pg, name, kind) }
	password := user("password", 0)
	totp := user("totp", 0)
	putTOTP(ctx, t, repo, totp, true)
	unconfirmed := user("unconfirmed", 0)
	putTOTP(ctx, t, repo, unconfirmed, false)
	passkey := user("passkey", 0)
	putPasskey(ctx, t, repo, passkey, "dilla.example", bytes.Repeat([]byte{0x11}, 32))
	bot := user("bot", 1)
	deleted := user("deleted", 0)
	// By hand too: TombstoneUser also clears status_msg and bio (00022), which this schema lacks.
	tombstone := "UPDATE users SET deleted_at = 5 WHERE id = ?"
	if pg {
		tombstone = "UPDATE users SET deleted_at = 5 WHERE id = $1"
	}
	if _, err := db.ExecContext(ctx, tombstone, deleted); err != nil {
		t.Fatalf("tombstone: %v", err)
	}

	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}
	for _, c := range []struct {
		name string
		user id.ID
		want bool
	}{
		{"a password-only user", password, true},
		{"a TOTP-confirmed user", totp, false},
		{"an unconfirmed-TOTP user", unconfirmed, true},
		{"a passkey user", passkey, false},
		{"a bot", bot, false},
		{"a deleted user", deleted, false},
	} {
		if got := owed(ctx, t, repo, c.user); got != c.want {
			t.Errorf("%s: factor_owed = %v after 00017, want %v", c.name, got, c.want)
		}
	}
}

// The reconcile's second half (web3.6 review I2) clears the mark from an owed account that holds a
// usable factor, and from no other. The revert row: an rp_id changed (the mark is set) and then
// put back (the mark is cleared).
func TestClearFactorOwedWithFactorClearsOnlyAHolder(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			reverted := factorUser(ctx, t, repo, 0, false)
			putPasskey(ctx, t, repo, reverted, "dilla.example", bytes.Repeat([]byte{0x41}, 32))
			if n, err := repo.MarkFactorOwedWithoutFactor(ctx, "mistyped.example"); err != nil || n != 1 {
				t.Fatalf("the reconcile under the mistyped rp_id = %d, %v; want 1, nil", n, err)
			}
			if n, err := repo.ClearFactorOwedWithFactor(ctx, "mistyped.example"); err != nil || n != 0 {
				t.Fatalf("the clear under the mistyped rp_id = %d, %v; want 0, nil", n, err)
			}
			if !owed(ctx, t, repo, reverted) {
				t.Fatal("the mistyped rp_id did not mark the passkey-only account")
			}

			withTOTP := factorUser(ctx, t, repo, 0, true)
			putTOTP(ctx, t, repo, withTOTP, true)
			otherRP := factorUser(ctx, t, repo, 0, true)
			putPasskey(ctx, t, repo, otherRP, "old.example", bytes.Repeat([]byte{0x42}, 32))
			unconfirmed := factorUser(ctx, t, repo, 0, true)
			putTOTP(ctx, t, repo, unconfirmed, false)
			bare := factorUser(ctx, t, repo, 0, true)

			// The rp_id is put back.
			if _, err := repo.MarkFactorOwedWithoutFactor(ctx, "dilla.example"); err != nil {
				t.Fatalf("MarkFactorOwedWithoutFactor: %v", err)
			}
			n, err := repo.ClearFactorOwedWithFactor(ctx, "dilla.example")
			if err != nil {
				t.Fatalf("ClearFactorOwedWithFactor: %v", err)
			}
			if n != 2 {
				t.Errorf("ClearFactorOwedWithFactor cleared %d accounts, want 2 (the reverted passkey and the TOTP)", n)
			}
			for _, c := range []struct {
				name string
				user id.ID
				want bool
			}{
				{"a passkey under dilla.example, after the revert", reverted, false},
				{"an owed user holding a confirmed TOTP", withTOTP, false},
				{"a passkey only under old.example", otherRP, true},
				{"an unconfirmed TOTP", unconfirmed, true},
				{"no factor", bare, true},
			} {
				if got := owed(ctx, t, repo, c.user); got != c.want {
					t.Errorf("%s: factor_owed = %v after the reconcile, want %v", c.name, got, c.want)
				}
			}
			if n, err := repo.ClearFactorOwedWithFactor(ctx, "dilla.example"); err != nil || n != 0 {
				t.Fatalf("a second clear = %d, %v; want 0, nil", n, err)
			}
		})
	}
}

// The boot reconcile's first half marks an account whose only passkey is under another rp_id, and
// only ever sets the flag (the second half, ClearFactorOwedWithFactor, clears it).
func TestMarkFactorOwedWithoutFactorOnlySetsTheFlag(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			oldRP := factorUser(ctx, t, repo, 0, false)
			putPasskey(ctx, t, repo, oldRP, "old.example", bytes.Repeat([]byte{0x21}, 32))
			// The control: a passkey under the instance's own rp_id is a usable factor.
			currentRP := factorUser(ctx, t, repo, 0, false)
			putPasskey(ctx, t, repo, currentRP, "dilla.example", bytes.Repeat([]byte{0x22}, 32))
			totp := factorUser(ctx, t, repo, 0, false)
			putTOTP(ctx, t, repo, totp, true)
			// Owed and holding a factor: this half never clears.
			owedWithTOTP := factorUser(ctx, t, repo, 0, true)
			putTOTP(ctx, t, repo, owedWithTOTP, true)
			unconfirmed := factorUser(ctx, t, repo, 0, false)
			putTOTP(ctx, t, repo, unconfirmed, false)
			bot := factorUser(ctx, t, repo, 1, false)
			deleted := factorUser(ctx, t, repo, 0, false)
			if err := repo.TombstoneUser(ctx, deleted, 5); err != nil {
				t.Fatalf("TombstoneUser: %v", err)
			}

			n, err := repo.MarkFactorOwedWithoutFactor(ctx, "dilla.example")
			if err != nil {
				t.Fatalf("MarkFactorOwedWithoutFactor: %v", err)
			}
			if n != 2 {
				t.Errorf("MarkFactorOwedWithoutFactor marked %d accounts, want 2 (the old rp_id and the unconfirmed TOTP)", n)
			}
			for _, c := range []struct {
				name string
				user id.ID
				want bool
			}{
				{"a passkey only under old.example", oldRP, true},
				{"a passkey under dilla.example", currentRP, false},
				{"a TOTP-confirmed user", totp, false},
				{"an owed user holding a TOTP", owedWithTOTP, true},
				{"an unconfirmed-TOTP user", unconfirmed, true},
				{"a bot", bot, false},
				{"a deleted user", deleted, false},
			} {
				if got := owed(ctx, t, repo, c.user); got != c.want {
					t.Errorf("%s: factor_owed = %v after the reconcile, want %v", c.name, got, c.want)
				}
			}
			if n, err := repo.MarkFactorOwedWithoutFactor(ctx, "dilla.example"); err != nil || n != 0 {
				t.Fatalf("a second reconcile = %d, %v; want 0, nil", n, err)
			}
		})
	}
}

// SetFactorOwed round-trips through GetUser and through GetSessionByHash's join, and refuses an
// unknown user.
func TestTheOwedFlagReachesTheSessionRow(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			user := factorUser(ctx, t, repo, 0, true)
			device := seedDevice(ctx, t, repo, user)
			hash := bytes.Repeat([]byte{0x31}, 32)
			if err := repo.CreateSession(ctx, store.SessionRow{
				TokenHash: hash, DeviceID: device, UserID: user, Created: 1, Expires: 1000, IdleExpires: 1000,
			}); err != nil {
				t.Fatalf("CreateSession: %v", err)
			}
			sess, err := repo.GetSessionByHash(ctx, hash, 10)
			if err != nil || !sess.FactorOwed {
				t.Fatalf("GetSessionByHash of an owed account = %+v, %v; want FactorOwed", sess, err)
			}
			if err := repo.SetFactorOwed(ctx, user, false); err != nil {
				t.Fatalf("SetFactorOwed(false): %v", err)
			}
			if owed(ctx, t, repo, user) {
				t.Fatal("GetUser still reads factor_owed after SetFactorOwed(false)")
			}
			sess, err = repo.GetSessionByHash(ctx, hash, 10)
			if err != nil || sess.FactorOwed {
				t.Fatalf("GetSessionByHash after SetFactorOwed(false) = %+v, %v; want not owed", sess, err)
			}
			// The expiry predicates survive the join.
			if _, err := repo.GetSessionByHash(ctx, hash, 1000); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("an expired session resolved through the join: %v", err)
			}
			if err := repo.SetFactorOwed(ctx, id.New(), true); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("SetFactorOwed of an unknown user = %v, want ErrNotFound", err)
			}
		})
	}
}

// The factor deletes: a credential id goes only from its own account, and the per-user deletes
// leave other accounts alone.
func TestTheFactorDeletesTouchOnlyTheirOwnAccount(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			alice := factorUser(ctx, t, repo, 0, false)
			mallory := factorUser(ctx, t, repo, 0, false)
			aliceCred := bytes.Repeat([]byte{0x41}, 32)
			aliceOld := bytes.Repeat([]byte{0x42}, 32)
			malloryCred := bytes.Repeat([]byte{0x43}, 32)
			putPasskey(ctx, t, repo, alice, "dilla.example", aliceCred)
			putPasskey(ctx, t, repo, alice, "old.example", aliceOld)
			putPasskey(ctx, t, repo, mallory, "dilla.example", malloryCred)
			putTOTP(ctx, t, repo, alice, true)
			putTOTP(ctx, t, repo, mallory, true)
			for _, u := range []id.ID{alice, mallory} {
				if err := repo.PutRecoveryCodes(ctx, u, [][]byte{bytes.Repeat([]byte{0x51}, 32)}, 1); err != nil {
					t.Fatalf("PutRecoveryCodes: %v", err)
				}
			}

			if n, err := repo.DeleteWebauthnCredential(ctx, mallory, aliceCred); err != nil || n != 0 {
				t.Fatalf("mallory deleting alice's credential = %d, %v; want 0 rows", n, err)
			}
			if n, err := repo.DeleteWebauthnCredential(ctx, alice, aliceCred); err != nil || n != 1 {
				t.Fatalf("alice deleting her credential = %d, %v; want 1 row", n, err)
			}
			if _, err := repo.GetWebauthnCredential(ctx, aliceCred); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("the deleted credential is still there: %v", err)
			}

			if err := repo.DeleteWebauthnCredentialsByUser(ctx, alice); err != nil {
				t.Fatalf("DeleteWebauthnCredentialsByUser: %v", err)
			}
			if _, err := repo.GetWebauthnCredential(ctx, aliceOld); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("alice's credential under another rp_id survived: %v", err)
			}
			if err := repo.DeleteTOTP(ctx, alice); err != nil {
				t.Fatalf("DeleteTOTP: %v", err)
			}
			if _, err := repo.GetTOTP(ctx, alice); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("alice's TOTP survived DeleteTOTP: %v", err)
			}
			if err := repo.DeleteTOTP(ctx, alice); err != nil {
				t.Fatalf("DeleteTOTP with no row: %v", err)
			}
			if err := repo.DeleteRecoveryCodes(ctx, alice); err != nil {
				t.Fatalf("DeleteRecoveryCodes: %v", err)
			}
			if n, err := repo.CountRecoveryCodes(ctx, alice); err != nil || n != 0 {
				t.Fatalf("alice's recovery codes after the delete = %d, %v", n, err)
			}

			// Mallory's factors are untouched.
			if _, err := repo.GetWebauthnCredential(ctx, malloryCred); err != nil {
				t.Fatalf("mallory's credential went: %v", err)
			}
			if _, err := repo.GetTOTP(ctx, mallory); err != nil {
				t.Fatalf("mallory's TOTP went: %v", err)
			}
			if n, err := repo.CountRecoveryCodes(ctx, mallory); err != nil || n != 1 {
				t.Fatalf("mallory's recovery codes = %d, %v; want 1", n, err)
			}
		})
	}
}
