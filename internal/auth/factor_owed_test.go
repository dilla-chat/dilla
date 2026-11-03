package auth_test

// factor_owed_test.go pins web3.6 S1's middleware rule (protocol/02 § Device sessions item 8): an
// enrolled session whose account owes a second factor reaches only the `F` routes (mounted
// through MiddlewareOwedOK) and the `P` routes (want = ScopePending); every other route, the
// "E or V" ones (want = ScopeProvisional) included, answers 403 E_FACTOR_REQUIRED before the
// handler runs. Each refusal row has a control that differs in the one input: the flag.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/id"
)

type owedRoute struct {
	name   string
	owedOK bool
	want   auth.Scope
}

// owedRoutes is every way a route is mounted, with whether an owed enrolled session gets through.
var owedRoutes = []struct {
	owedRoute
	admitted bool
}{
	{owedRoute{"E (Middleware, enrolled)", false, auth.ScopeEnrolled}, false},
	{owedRoute{"E or V (Middleware, provisional)", false, auth.ScopeProvisional}, false},
	{owedRoute{"E or P (Middleware, pending)", false, auth.ScopePending}, true},
	{owedRoute{"E, F (MiddlewareOwedOK, enrolled)", true, auth.ScopeEnrolled}, true},
}

// serveOwed sends one bearer through the route and reports the status, the body and whether the
// handler ran.
func serveOwed(t *testing.T, s *auth.Sessions, r owedRoute, token string) (int, []byte, bool) {
	t.Helper()
	ran := false
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ran = true
		w.WriteHeader(http.StatusNoContent)
	})
	var guarded http.Handler
	if r.owedOK {
		guarded = s.MiddlewareOwedOK(h, r.want)
	} else {
		guarded = s.Middleware(h, r.want)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/anything", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	guarded.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes(), ran
}

func TestAnOwedEnrolledSessionReachesOnlyTheAllowList(t *testing.T) {
	s, repo, _ := newSessions(t)
	ctx := context.Background()
	user, device, priv := seedDevice(t, repo)
	if err := repo.SetFactorOwed(ctx, user, true); err != nil {
		t.Fatalf("SetFactorOwed: %v", err)
	}
	tok, err := establishOnce(t, s, device, priv, auth.PurposeSession)
	if err != nil || tok.Scope != auth.ScopeEnrolled {
		t.Fatalf("establish: scope %d, err %v; want enrolled", tok.Scope, err)
	}
	if sess, err := s.Resolve(ctx, tok.Token); err != nil || !sess.FactorOwed {
		t.Fatalf("Resolve of an owed account = %+v, %v; want FactorOwed", sess, err)
	}
	for _, r := range owedRoutes {
		code, body, ran := serveOwed(t, s, r.owedRoute, tok.Token)
		if r.admitted {
			if code != http.StatusNoContent || !ran {
				t.Errorf("%s: an owed enrolled session = %d (handler ran %v), want it admitted", r.name, code, ran)
			}
			continue
		}
		if code != http.StatusForbidden || !bytes.Contains(body, []byte("E_FACTOR_REQUIRED")) || ran {
			t.Errorf("%s: an owed enrolled session = %d %x (handler ran %v), want 403 E_FACTOR_REQUIRED before the handler",
				r.name, code, body, ran)
		}
	}

	// The control: the same session once the account holds a factor reaches every route.
	if err := repo.SetFactorOwed(ctx, user, false); err != nil {
		t.Fatalf("SetFactorOwed(false): %v", err)
	}
	for _, r := range owedRoutes {
		if code, body, ran := serveOwed(t, s, r.owedRoute, tok.Token); code != http.StatusNoContent || !ran {
			t.Errorf("%s: the control (owed cleared) = %d %x, want admitted", r.name, code, body)
		}
	}
}

// MiddlewareOwedOK lifts the owed refusal and nothing else: a pending session of an owed account
// is still refused an enrolled route, and still reaches a pending one.
func TestTheOwedAllowListDoesNotWidenTheScope(t *testing.T) {
	s, repo, clk := newSessions(t)
	ctx := context.Background()
	lister := listerOf(t, s)
	user, device, priv := seedDevice(t, repo)
	if err := repo.SetFactorOwed(ctx, user, true); err != nil {
		t.Fatalf("SetFactorOwed: %v", err)
	}
	makeDeviceYoung(t, repo, device, clk.Now().Unix())
	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	lister.list(user, entry(id.New(), pubOf(otherPriv)))
	pending, err := establishOnce(t, s, device, priv, auth.PurposeSession)
	if err != nil || pending.Scope != auth.ScopePending {
		t.Fatalf("establish: scope %d, err %v; want pending", pending.Scope, err)
	}
	code, body, ran := serveOwed(t, s, owedRoute{"F", true, auth.ScopeEnrolled}, pending.Token)
	if code != http.StatusForbidden || !bytes.Contains(body, []byte("E_FORBIDDEN")) || ran {
		t.Fatalf("a pending session on an `E, F` route = %d %x (ran %v), want 403 E_FORBIDDEN", code, body, ran)
	}
	if code, _, ran := serveOwed(t, s, owedRoute{"P", false, auth.ScopePending}, pending.Token); code != http.StatusNoContent || !ran {
		t.Fatalf("a pending session of an owed account on a `P` route = %d, want admitted", code)
	}
}

// S1 review minor 2: the owed check is on every session of an owed account, not only an enrolled
// one. A PROVISIONAL session of an owed account is refused the "E or V" routes it would otherwise
// reach (today the delivery service refuses it anyway, having no pairing group; once pairing sets
// one it must still find nothing). The control clears the flag.
func TestAnOwedProvisionalSessionIsRefusedTheEOrVRoutes(t *testing.T) {
	s, repo, _ := newSessions(t)
	ctx := context.Background()
	user, device, priv := seedDevice(t, repo)
	if err := repo.SetFactorOwed(ctx, user, true); err != nil {
		t.Fatalf("SetFactorOwed: %v", err)
	}
	tok, err := establishOnce(t, s, device, priv, auth.PurposeProvisional)
	if err != nil || tok.Scope != auth.ScopeProvisional {
		t.Fatalf("establish: scope %d, err %v; want provisional", tok.Scope, err)
	}
	eOrV := owedRoute{"E or V", false, auth.ScopeProvisional}
	code, body, ran := serveOwed(t, s, eOrV, tok.Token)
	if code != http.StatusForbidden || !bytes.Contains(body, []byte("E_FACTOR_REQUIRED")) || ran {
		t.Fatalf("an owed provisional session on an `E or V` route = %d %x (ran %v), want 403 E_FACTOR_REQUIRED", code, body, ran)
	}

	if err := repo.SetFactorOwed(ctx, user, false); err != nil {
		t.Fatalf("SetFactorOwed(false): %v", err)
	}
	if code, body, ran := serveOwed(t, s, eOrV, tok.Token); code != http.StatusNoContent || !ran {
		t.Fatalf("the control (owed cleared) = %d %x, want admitted", code, body)
	}
}
