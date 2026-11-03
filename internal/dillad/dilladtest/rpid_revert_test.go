package dilladtest

import (
	"context"
	"net/http"
	"testing"

	"github.com/dilla-chat/dilla/internal/cborx"
)

// restartWithRPID stops the instance and starts it again over the same data directory with
// auth.webauthn.rp_id set to rp, as an operator who edits dilla.toml and restarts does. The new
// server runs the boot reconcile.
func restartWithRPID(t *testing.T, h *Host, rp string) {
	t.Helper()
	ctx := context.Background()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.current.Store(nil)
	if err := h.server.Shutdown(ctx); err != nil {
		t.Fatalf("stop the instance: %v", err)
	}
	h.cfg.Auth.WebAuthn.RPID = rp
	server, err := h.newServer(ctx)
	if err != nil {
		t.Fatalf("restart with rp_id %q: %v", rp, err)
	}
	h.server = server
	h.current.Store(server)
}

// I2 of the web3.6 whole-branch review: an operator mistypes auth.webauthn.rp_id, restarts, and
// the boot reconcile marks every passkey-only account as owing a factor. They put the rp_id back
// and restart: the passkeys count again, so the reconcile clears the mark, and the person signs in
// with the same passkey and reaches the instance. Before the fix nothing cleared it, and the forced
// set-up screen could not finish (its enrolment needed a step-up the owed account cannot get).
func TestAnRPIDChangeThatIsRevertedLeavesAPasskeyOnlyAccountOwingNothing(t *testing.T) {
	const origin = "http://localhost:9999"
	h, err := NewHost(t.Context(), HostOptions{DataDir: t.TempDir(), CorePath: corePath(t), WebAuthnOrigin: origin})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	t.Cleanup(func() { _ = h.Close(t.Context()) })
	user, device, token := seedOwedPerson(t, h, "alice")

	// The forced set-up: the first factor is a passkey under rp_id localhost.
	key := newSoftKey(t, "localhost", origin)
	begin := postCBOR(h.Handler(), "/v1/auth/passkey/register/begin", token, []any{nil})
	if begin.Code != http.StatusOK {
		t.Fatalf("register/begin = %d: %q", begin.Code, begin.Body.String())
	}
	ceremony, options := ceremonyAndOptions(t, begin.Body.Bytes())
	if rec := postCBOR(h.Handler(), "/v1/auth/passkey/register/finish", token, []any{ceremony, key.register(t, options), "Key"}); rec.Code != http.StatusOK {
		t.Fatalf("register/finish = %d: %q", rec.Code, rec.Body.String())
	}
	owed := func(when string) bool {
		t.Helper()
		u, err := h.Server().Repo().GetUser(t.Context(), user)
		if err != nil {
			t.Fatalf("GetUser %s: %v", when, err)
		}
		return u.FactorOwed
	}
	if owed("after the passkey") {
		t.Fatal("the account still owes a factor after registering its passkey")
	}

	restartWithRPID(t, h, "mistyped.example")
	if !owed("after the rp_id change") {
		t.Fatal("the boot reconcile did not mark the account whose only passkey is under the old rp_id")
	}

	restartWithRPID(t, h, "localhost")
	if owed("after the rp_id was put back") {
		t.Fatal("the account still owes a factor after the rp_id was put back and the instance restarted: " +
			"its passkey is usable again, and nothing clears the mark")
	}

	// It signs in with the same passkey: a user-verified passkey is both factors.
	lb := postCBOR(h.Handler(), "/v1/auth/passkey/login/begin", "", []any{})
	if lb.Code != http.StatusOK {
		t.Fatalf("login/begin = %d: %q", lb.Code, lb.Body.String())
	}
	ceremony, options = ceremonyAndOptions(t, lb.Body.Bytes())
	fin := postCBOR(h.Handler(), "/v1/auth/passkey/login/finish", "", []any{ceremony, key.login(t, options)})
	if fin.Code != http.StatusOK {
		t.Fatalf("login/finish = %d: %q", fin.Code, fin.Body.String())
	}
	var out []any
	if err := cborx.Unmarshal(fin.Body.Bytes(), &out); err != nil || len(out) != 2 {
		t.Fatalf("login/finish body %x: %v", fin.Body.Bytes(), err)
	}
	if needs, _ := out[1].(uint64); needs != 0 {
		t.Fatalf("needs = %d, want 0", needs)
	}
	// And its device reaches a route an owed account is refused.
	status, body, err := h.AsDevice(t.Context(), device, http.MethodGet, "/v1/devices", nil)
	if err != nil {
		t.Fatalf("AsDevice: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("GET /v1/devices after the revert = %d %x, want 200", status, body)
	}
}
