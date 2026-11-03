package main

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// seedEveryFactor gives the user the three things reset-factors removes: a confirmed TOTP secret,
// backup codes and a passkey.
func (h *cliHarness) seedEveryFactor(t *testing.T, uid id.ID) {
	t.Helper()
	ctx := t.Context()
	h.SeedFactor(t, uid)
	if err := h.Repo(t).PutRecoveryCodes(ctx, uid, [][]byte{make([]byte, 32)}, h.Now()); err != nil {
		t.Fatalf("PutRecoveryCodes: %v", err)
	}
	handle := make([]byte, 64)
	copy(handle, uid[:])
	if err := h.Repo(t).PutWebauthnUser(ctx, uid, "dilla.test", handle, h.Now()); err != nil {
		t.Fatalf("PutWebauthnUser: %v", err)
	}
	if err := h.Repo(t).PutWebauthnCredential(ctx, store.WebauthnCredentialRow{
		CredID: append([]byte("cred-"), uid[:]...), RPID: "dilla.test", UserID: uid, PublicKey: []byte{1}, Flags: []byte{0},
		Name: "key", Created: h.Now(),
	}); err != nil {
		t.Fatalf("PutWebauthnCredential: %v", err)
	}
}

func TestResetFactorsRemovesTheFactorsAndKeepsTheSessions(t *testing.T) {
	h := newCLIHarness(t)
	alice := h.SeedUser(t, "alice")
	bob := h.SeedUser(t, "bob")
	h.seedEveryFactor(t, alice)
	h.seedEveryFactor(t, bob)
	tok := h.SeedSession(t, alice)

	code, out := h.Run(t, "admin", "user", "reset-factors", "--username=alice", "--yes", "--config="+h.ConfigPath)
	if code != 0 || !strings.Contains(out, "alice") {
		t.Fatalf("reset-factors = %d: %s", code, out)
	}
	repo := h.Repo(t)
	if _, err := repo.GetTOTP(t.Context(), alice); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("alice's TOTP survived: %v", err)
	}
	if n, _ := repo.CountRecoveryCodes(t.Context(), alice); n != 0 {
		t.Errorf("%d backup codes survived", n)
	}
	if creds, _ := repo.ListWebauthnCredentials(t.Context(), alice, "dilla.test"); len(creds) != 0 {
		t.Errorf("%d passkeys survived", len(creds))
	}
	if u, _ := repo.GetUser(t.Context(), alice); !u.FactorOwed {
		t.Error("factor_owed was not set")
	}
	if _, err := repo.GetSessionByHash(t.Context(), auth.TokenHash(tok), h.Now()); err != nil {
		t.Errorf("the session did not survive (D5): %v", err)
	}
	audit, err := repo.ListAudit(t.Context(), 0, 100)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if !slices.ContainsFunc(audit, func(a store.AuditRow) bool {
		return a.Action == "user.factors_reset" && a.Target == alice.String()
	}) {
		t.Errorf("no user.factors_reset audit row: %+v", audit)
	}
	// Control: bob, not named, keeps everything.
	if _, err := repo.GetTOTP(t.Context(), bob); err != nil {
		t.Errorf("bob's TOTP was removed: %v", err)
	}
	if u, _ := repo.GetUser(t.Context(), bob); u.FactorOwed {
		t.Error("bob's factor_owed was set")
	}
}

// seedFactors gives the user a TOTP secret (confirmed or not), n backup codes, and a passkey under each
// of the rp ids named.
func (h *cliHarness) seedFactors(t *testing.T, uid id.ID, totp, confirmed bool, codes int, passkeyRPs ...string) {
	t.Helper()
	ctx, repo := t.Context(), h.Repo(t)
	if totp {
		row := store.TOTPRow{UserID: uid, Secret: make([]byte, 20), Digits: 6, Period: 30, Algorithm: "SHA1", Created: h.Now()}
		if confirmed {
			at := h.Now()
			row.ConfirmedAt = &at
		}
		if err := repo.PutTOTP(ctx, row); err != nil {
			t.Fatalf("PutTOTP: %v", err)
		}
	}
	if codes > 0 {
		hashes := make([][]byte, codes)
		for i := range hashes {
			hashes[i] = make([]byte, 32)
			hashes[i][0] = byte(i + 1)
		}
		if err := repo.PutRecoveryCodes(ctx, uid, hashes, h.Now()); err != nil {
			t.Fatalf("PutRecoveryCodes: %v", err)
		}
	}
	for i, rp := range passkeyRPs {
		handle := make([]byte, 64)
		copy(handle, uid[:])
		if err := repo.PutWebauthnUser(ctx, uid, rp, handle, h.Now()); err != nil {
			t.Fatalf("PutWebauthnUser: %v", err)
		}
		if err := repo.PutWebauthnCredential(ctx, store.WebauthnCredentialRow{
			CredID: append([]byte{byte(i)}, uid[:]...), RPID: rp, UserID: uid, PublicKey: []byte{1}, Flags: []byte{0},
			Name: "key", Created: h.Now(),
		}); err != nil {
			t.Fatalf("PutWebauthnCredential: %v", err)
		}
	}
}

func TestUserFactorsSummarisesWhatAnAccountHolds(t *testing.T) {
	h := newCLIHarness(t)
	const rp, other = "dilla.test", "old.example"
	h.Repo(t) // opened under the parent test, so a subtest's end does not close it
	for i, tc := range []struct {
		name      string
		totp      bool
		confirmed bool
		codes     int
		passkeys  []string
		owed      bool
		want      string
	}{
		{"a confirmed TOTP and 10 codes", true, true, 10, nil, false, "totp,codes:10"},
		{"one passkey under rp_id and one under another", false, false, 0, []string{rp, other}, false, "passkey\u00d71"},
		{"an unconfirmed TOTP only, owed", true, false, 0, nil, true, "owed"},
		{"nothing, and not owed", false, false, 0, nil, false, "-"},
		{"TOTP, 2 passkeys, 3 codes", true, true, 3, []string{rp, rp}, false, "totp,passkey\u00d72,codes:3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uid := h.SeedUser(t, "user"+string(rune('a'+i)))
			h.seedFactors(t, uid, tc.totp, tc.confirmed, tc.codes, tc.passkeys...)
			if tc.owed {
				if err := h.Repo(t).SetFactorOwed(t.Context(), uid, true); err != nil {
					t.Fatalf("SetFactorOwed: %v", err)
				}
			}
			u, err := h.Repo(t).GetUser(t.Context(), uid)
			if err != nil {
				t.Fatalf("GetUser: %v", err)
			}
			f, err := userFactors(t.Context(), h.Repo(t), u, rp)
			if err != nil {
				t.Fatalf("userFactors: %v", err)
			}
			if got := f.String(); got != tc.want {
				t.Fatalf("factors = %q, want %q (%+v)", got, tc.want, f)
			}
		})
	}
}

func TestAdminUserListAndShowCarryTheFactors(t *testing.T) {
	h := newCLIHarness(t)
	rp := h.cfg.Auth.WebAuthn.RPID
	alice := h.SeedUser(t, "alice")
	h.seedFactors(t, alice, true, true, 10, rp)
	bob := h.SeedUser(t, "bob")
	if err := h.Repo(t).SetFactorOwed(t.Context(), bob, true); err != nil {
		t.Fatalf("SetFactorOwed: %v", err)
	}

	code, out := h.Run(t, "admin", "user", "list", "--config="+h.ConfigPath)
	if code != 0 {
		t.Fatalf("list = %d: %s", code, out)
	}
	if !regexp.MustCompile(`id\s+username\s+display\s+kind\s+flags\s+factors\s+status\s+created`).MatchString(out) {
		t.Errorf("the header lacks the factors column after flags:\n%s", out)
	}
	if !regexp.MustCompile(`alice\s+Alice\s+0\s+-\s+totp,passkey\x{00d7}1,codes:10\s+active`).MatchString(out) {
		t.Errorf("alice's row lacks her factors:\n%s", out)
	}
	if !regexp.MustCompile(`bob\s+Bob\s+0\s+-\s+owed\s+active`).MatchString(out) {
		t.Errorf("bob's row lacks owed:\n%s", out)
	}

	code, out = h.Run(t, "admin", "user", "show", "--username=alice", "--config="+h.ConfigPath)
	if code != 0 {
		t.Fatalf("show alice = %d: %s", code, out)
	}
	for _, re := range []string{`(?m)^flags +-\n`, `(?m)^totp +confirmed \d{4}-\d\d-\d\dT[\d:]+Z\n`, `(?m)^passkeys +1\n`,
		`(?m)^backup_codes +10\n`, `(?m)^factor_owed +no\n`} {
		if !regexp.MustCompile(re).MatchString(out) {
			t.Errorf("show alice lacks %s:\n%s", re, out)
		}
	}
	code, out = h.Run(t, "admin", "user", "show", "--username=bob", "--config="+h.ConfigPath)
	if code != 0 {
		t.Fatalf("show bob = %d: %s", code, out)
	}
	for _, re := range []string{`(?m)^totp +-\n`, `(?m)^passkeys +0\n`, `(?m)^backup_codes +0\n`, `(?m)^factor_owed +yes\n`} {
		if !regexp.MustCompile(re).MatchString(out) {
			t.Errorf("show bob lacks %s:\n%s", re, out)
		}
	}
}

func TestResetFactorsNeedsYesAndAnAccount(t *testing.T) {
	h := newCLIHarness(t)
	alice := h.SeedUser(t, "alice")
	h.seedEveryFactor(t, alice)
	for _, args := range [][]string{
		{"--username=alice"},
		{"--yes"},
		{"--id=" + alice.String(), "--username=alice", "--yes"},
	} {
		code, _, stderr := h.Run3(t, append([]string{"admin", "user", "reset-factors"}, append(args, "--config="+h.ConfigPath)...)...)
		if code != 2 {
			t.Errorf("%v: exit %d, want 2: %s", args, code, stderr)
		}
	}
	if code, _ := h.Run(t, "admin", "user", "reset-factors", "--username=nobody", "--yes", "--config="+h.ConfigPath); code == 0 {
		t.Error("an unknown username was reset")
	}
	if _, err := h.Repo(t).GetTOTP(t.Context(), alice); err != nil {
		t.Errorf("a refused reset removed the TOTP: %v", err)
	}
}

// The CLI is another process and holds no gateway: the serving process's liveness tick finds the
// session row's account owing a factor and closes the socket 4003, so the client's reconnect meets
// E_FACTOR_REQUIRED. Control: the same tick before the reset closes nothing.
func TestResetFactorsClosesALiveSocketAtTheNextHeartbeat(t *testing.T) {
	h := newCLIHarness(t)
	uid := h.SeedUser(t, "alice")
	h.SeedFactor(t, uid)
	h.SeedSession(t, uid)
	conn := h.OpenGatewayConn(t, uid)

	h.clk.Advance(h.HeartbeatInterval() + time.Second)
	if code, err := conn.WaitClosed(t, 300*time.Millisecond); err == nil {
		t.Fatalf("a connection with a factor was closed %d by the heartbeat", code)
	}

	if code, _ := h.Run(t, "admin", "user", "reset-factors", "--id="+uid.String(), "--yes", "--config="+h.ConfigPath); code != 0 {
		t.Fatal("reset-factors failed")
	}
	h.clk.Advance(h.HeartbeatInterval() + time.Second)
	code, err := conn.WaitClosed(t, 10*time.Second)
	if err != nil {
		t.Fatalf("the socket was still open a heartbeat after the reset: %v", err)
	}
	if code != 4003 {
		t.Fatalf("close code = %d, want 4003", code)
	}
}
