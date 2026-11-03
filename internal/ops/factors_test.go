package ops_test

import (
	"bytes"
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/ops"
	"github.com/dilla-chat/dilla/internal/store"
)

// seedFactorUser is a person holding a confirmed TOTP, a passkey and backup codes, owing nothing,
// with two devices that each hold a session.
func seedFactorUser(t *testing.T, h *opsHarness) (uid id.ID, devices []id.ID) {
	t.Helper()
	return seedFactorUserNamed(t, h, "factored")
}

// seedFactorUserNamed is seedFactorUser under another username, for a second account in one test.
func seedFactorUserNamed(t *testing.T, h *opsHarness, name string) (uid id.ID, devices []id.ID) {
	t.Helper()
	ctx := t.Context()
	now := h.Clock.Now().Unix()
	uid = id.New()
	if err := h.Repo.CreateUser(ctx, store.UserRow{
		ID: uid, Username: name, Display: name,
		UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: now,
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	for range 2 {
		d := id.New()
		devices = append(devices, d)
		if err := h.Repo.CreateDevice(ctx, store.DeviceRow{
			ID: d, UserID: uid, DSKPub: make([]byte, 32), CredentialBlob: []byte{1}, LastSeen: now, Created: now,
		}); err != nil {
			t.Fatalf("CreateDevice: %v", err)
		}
		if err := h.Repo.CreateSession(ctx, store.SessionRow{
			TokenHash: []byte(d.String()), DeviceID: d, UserID: uid, Scope: 1, Created: now, Expires: now + 3600, IdleExpires: now + 3600,
		}); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
	}
	confirmed := now
	if err := h.Repo.PutTOTP(ctx, store.TOTPRow{UserID: uid, Secret: make([]byte, 20), Digits: 6, Period: 30,
		Algorithm: "SHA1", ConfirmedAt: &confirmed, Created: now}); err != nil {
		t.Fatalf("PutTOTP: %v", err)
	}
	if err := h.Repo.PutRecoveryCodes(ctx, uid, [][]byte{make([]byte, 32), append(make([]byte, 31), 1)}, now); err != nil {
		t.Fatalf("PutRecoveryCodes: %v", err)
	}
	if err := h.Repo.PutWebauthnUser(ctx, uid, "dilla.test", append([]byte(name), make([]byte, 64-len(name))...), now); err != nil {
		t.Fatalf("PutWebauthnUser: %v", err)
	}
	if err := h.Repo.PutWebauthnCredential(ctx, store.WebauthnCredentialRow{
		CredID: []byte("cred-" + name), RPID: "dilla.test", UserID: uid, PublicKey: []byte{1}, Flags: []byte{0}, Name: "key", Created: now,
	}); err != nil {
		t.Fatalf("PutWebauthnCredential: %v", err)
	}
	return uid, devices
}

func assertNoFactors(t *testing.T, h *opsHarness, uid id.ID) {
	t.Helper()
	ctx := t.Context()
	if _, err := h.Repo.GetTOTP(ctx, uid); err == nil {
		t.Error("a TOTP row survived the reset")
	}
	if n, _ := h.Repo.CountRecoveryCodes(ctx, uid); n != 0 {
		t.Errorf("%d backup codes survived the reset", n)
	}
	if creds, _ := h.Repo.ListWebauthnCredentials(ctx, uid, "dilla.test"); len(creds) != 0 {
		t.Errorf("%d passkeys survived the reset", len(creds))
	}
	u, err := h.Repo.GetUser(ctx, uid)
	if err != nil || !u.FactorOwed {
		t.Errorf("factor_owed = %v (%v), want true", u.FactorOwed, err)
	}
}

func TestResetFactorsRemovesEveryFactorAndKeepsTheSessions(t *testing.T) {
	h := newOpsHarness(t)
	uid, devices := seedFactorUser(t, h)
	var closed []id.ID
	now := h.Clock.Now().Unix()
	if err := ops.ResetFactors(t.Context(), h.Repo, func(d id.ID) { closed = append(closed, d) }, uid, now); err != nil {
		t.Fatalf("ResetFactors: %v", err)
	}
	assertNoFactors(t, h, uid)
	for _, d := range devices {
		if n, err := h.Repo.CountSessionsByDevice(t.Context(), d); err != nil || n != 1 {
			t.Errorf("device %s keeps %d sessions (%v), want 1: the reset must not sign anyone out", d, n, err)
		}
		if n := countOf(closed, d); n != 1 {
			t.Errorf("the closer ran %d times for device %s, want 1", n, d)
		}
	}
	audit, err := h.Repo.ListAudit(t.Context(), 0, 100)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if !slices.ContainsFunc(audit, func(a store.AuditRow) bool {
		return a.Action == "user.factors_reset" && a.Target == uid.String() && a.At == now
	}) {
		t.Errorf("no user.factors_reset audit row: %+v", audit)
	}
}

func countOf(ids []id.ID, want id.ID) int {
	n := 0
	for _, d := range ids {
		if d == want {
			n++
		}
	}
	return n
}

func TestResetFactorsForAnUnknownUserChangesNothing(t *testing.T) {
	h := newOpsHarness(t)
	called := false
	if err := ops.ResetFactors(t.Context(), h.Repo, func(id.ID) { called = true }, id.New(), 1); err == nil {
		t.Fatal("an unknown user was reset")
	}
	if called {
		t.Error("the closer ran for a user that does not exist")
	}
}

// hookedRepo runs hook just before the reset's transaction opens: the state changes between the
// caller's read and the transaction. With it, TestALateFactorIsRemovedToo shows that a factor
// confirmed just before the reset goes with the rest; the lock itself is pinned by
// TestAFactorWriteDuringTheResetWaitsForIt.
type hookedRepo struct {
	store.Repository
	hook func(ctx context.Context)
}

func (r hookedRepo) Tx(ctx context.Context, fn func(store.Repository) error) error {
	if r.hook != nil {
		r.hook(ctx)
	}
	return r.Repository.Tx(ctx, fn)
}

func TestALateFactorIsRemovedToo(t *testing.T) {
	for _, tc := range []struct {
		name string
		hook bool
	}{{"control: nothing happens in between", false}, {"a passkey lands before the transaction", true}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newOpsHarness(t)
			uid, _ := seedFactorUser(t, h)
			repo := hookedRepo{Repository: h.Repo}
			if tc.hook {
				repo.hook = func(ctx context.Context) {
					if err := h.Repo.PutWebauthnCredential(ctx, store.WebauthnCredentialRow{
						CredID: []byte("cred-late"), RPID: "dilla.test", UserID: uid, PublicKey: []byte{1}, Flags: []byte{0}, Name: "late",
						Created: h.Clock.Now().Unix(),
					}); err != nil {
						t.Errorf("hook: %v", err)
					}
					if err := h.Repo.SetFactorOwed(ctx, uid, false); err != nil {
						t.Errorf("hook: %v", err)
					}
				}
			}
			if err := ops.ResetFactors(t.Context(), repo, nil, uid, h.Clock.Now().Unix()); err != nil {
				t.Fatalf("ResetFactors: %v", err)
			}
			assertNoFactors(t, h, uid)
		})
	}
}

// lockHookTx is the reset's transaction with a hook that runs once, right after the reset has
// taken the user lock: inside the transaction, before any delete.
type lockHookTx struct {
	store.Repository
	once *sync.Once
	hook func()
}

func (tx lockHookTx) LockUserForDeviceRegistration(ctx context.Context, uid id.ID) error {
	if err := tx.Repository.LockUserForDeviceRegistration(ctx, uid); err != nil {
		return err
	}
	tx.once.Do(tx.hook)
	return nil
}

// inTxRepo hands the reset's fn a lockHookTx, and after the reset's transaction returns it waits
// for whatever the hook started, so the test sees the concurrent write settle before the reset
// goes on to anything else it does.
type inTxRepo struct {
	store.Repository
	hook func()
	wait func()
}

func (r inTxRepo) Tx(ctx context.Context, fn func(store.Repository) error) error {
	once := &sync.Once{}
	err := r.Repository.Tx(ctx, func(tx store.Repository) error {
		return fn(lockHookTx{Repository: tx, once: once, hook: r.hook})
	})
	r.wait()
	return err
}

// S3 review minor 1: another device lands a passkey while the reset holds the user lock. The
// passkey's write (the finish leg's: the lock, the credential, factor_owed cleared, in one
// transaction) cannot commit inside the reset; it waits for the reset to commit and then lands on
// the reset account. So the account ends either owed with no factor or, as here, not owed with
// exactly the new passkey: never owed with a factor, never unowed with none. The control is the
// reset with nothing landing.
func TestAFactorWriteDuringTheResetWaitsForIt(t *testing.T) {
	h := newOpsHarness(t)
	uid, _ := seedFactorUser(t, h)
	late := []byte("cred-during")
	done := make(chan error, 1)
	blocked := false
	var waited sync.Once
	repo := inTxRepo{Repository: h.Repo,
		hook: func() {
			go func() {
				done <- h.Repo.Tx(context.Background(), func(tx store.Repository) error {
					if err := tx.LockUserForDeviceRegistration(context.Background(), uid); err != nil {
						return err
					}
					if err := tx.PutWebauthnCredential(context.Background(), store.WebauthnCredentialRow{
						CredID: late, RPID: "dilla.test", UserID: uid, PublicKey: []byte{1}, Flags: []byte{0},
						Name: "during", Created: h.Clock.Now().Unix(),
					}); err != nil {
						return err
					}
					return tx.SetFactorOwed(context.Background(), uid, false)
				})
			}()
			select {
			case err := <-done:
				done <- err
			case <-time.After(300 * time.Millisecond):
				blocked = true
			}
		},
		wait: func() {
			// Once: only the transaction whose lock ran the hook has a write to wait for.
			waited.Do(func() {
				if err := <-done; err != nil {
					t.Errorf("the concurrent passkey write: %v", err)
				}
			})
		},
	}
	if err := ops.ResetFactors(t.Context(), repo, nil, uid, h.Clock.Now().Unix()); err != nil {
		t.Fatalf("ResetFactors: %v", err)
	}
	if !blocked {
		t.Fatal("a passkey write committed while the reset held the user lock")
	}
	u, err := h.Repo.GetUser(t.Context(), uid)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	creds, _ := h.Repo.ListWebauthnCredentials(t.Context(), uid, "dilla.test")
	if u.FactorOwed || len(creds) != 1 || !bytes.Equal(creds[0].CredID, late) {
		t.Fatalf("after the reset and the late passkey: owed %v, passkeys %d; want not owed with exactly the new passkey",
			u.FactorOwed, len(creds))
	}
	if _, err := h.Repo.GetTOTP(t.Context(), uid); err == nil {
		t.Error("the TOTP survived the reset")
	}

	// The control: the same reset with nothing landing ends owed with no factor.
	v, _ := seedFactorUserNamed(t, h, "control")
	if err := ops.ResetFactors(t.Context(), h.Repo, nil, v, h.Clock.Now().Unix()); err != nil {
		t.Fatalf("ResetFactors (control): %v", err)
	}
	assertNoFactors(t, h, v)
}

// S3 review minor 2: the reset deletes the passkeys under every rp_id, not only the instance's
// current one, so a passkey left under an old rp_id cannot come back if the rp_id is moved back.
func TestTheResetDeletesPasskeysUnderEveryRelyingParty(t *testing.T) {
	h := newOpsHarness(t)
	uid, _ := seedFactorUser(t, h)
	if err := h.Repo.PutWebauthnCredential(t.Context(), store.WebauthnCredentialRow{
		CredID: []byte("cred-old"), RPID: "old.example", UserID: uid, PublicKey: []byte{1}, Flags: []byte{0},
		Name: "old", Created: h.Clock.Now().Unix(),
	}); err != nil {
		t.Fatalf("PutWebauthnCredential: %v", err)
	}
	for _, rp := range []string{"dilla.test", "old.example"} {
		if creds, _ := h.Repo.ListWebauthnCredentials(t.Context(), uid, rp); len(creds) != 1 {
			t.Fatalf("before the reset: %d passkeys under %s, want 1", len(creds), rp)
		}
	}
	if err := ops.ResetFactors(t.Context(), h.Repo, nil, uid, h.Clock.Now().Unix()); err != nil {
		t.Fatalf("ResetFactors: %v", err)
	}
	for _, rp := range []string{"dilla.test", "old.example"} {
		if creds, _ := h.Repo.ListWebauthnCredentials(t.Context(), uid, rp); len(creds) != 0 {
			t.Errorf("after the reset: %d passkeys under %s, want 0", len(creds), rp)
		}
	}
}
