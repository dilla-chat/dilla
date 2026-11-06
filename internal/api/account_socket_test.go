package api_test

// account_socket_test.go: Bot v2 task 4. The account, profile, presence, device and sign-in-settings routes that
// need a session already on the gateway are served on the socket only; the routes an account uses before its
// socket exists (sign-up, the factor gate of an owed account, device lists, KeyPackage top-up, session ceremonies,
// backups) stay on HTTP.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/api/sockettest"
	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// accountSocketEnv is the API harness with presence wired, its session resolved to an auth.Session and the
// socket dispatcher installed on its mux.
type accountSocketEnv struct {
	mux  *server.Mux
	deps api.Deps
	disp *api.SocketDispatcher
	user store.UserRow
	dev  store.DeviceRow
	sess auth.Session
	http *httptest.Server
	tok  string
}

func newAccountSocketEnv(t *testing.T) *accountSocketEnv {
	t.Helper()
	gw := &presenceGatewayFake{live: map[id.ID]bool{}}
	var mux *server.Mux
	_, deps := newTestAPIFullWithRepoAndMount(t, newAPIRepo(t), nil, func(d *api.Deps) { d.Presence = api.NewPresenceService(d.Repo, gw, d.Clock, d.Log) },
		func(m *server.Mux, _ api.Deps) { mux = m })
	user, dev, tok := seedAPISession(t, deps)
	sess, err := deps.Sessions.Resolve(t.Context(), tok)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &accountSocketEnv{mux: mux, deps: deps, disp: api.NewSocketDispatcher(mux, api.SocketDispatchOptions{Clock: deps.Clock}),
		user: user, dev: dev, sess: sess, http: srv, tok: tok}
}

func cborBody(t *testing.T, v any) []byte {
	t.Helper()
	if v == nil {
		return nil
	}
	b, err := cborx.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// httpStatus sends the REST form of a request to the HTTP mux.
func (e *accountSocketEnv) httpStatus(t *testing.T, method, route string, body []byte) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, e.http.URL+route, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/cbor")
	req.Header.Set("Authorization", "Bearer "+e.tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestAccountAreaIsSocketOnly: every moved route answers 404 or 405 over HTTP, is registered on the socket mux, and
// over the socket reaches its handler with the session's authority.
func TestAccountAreaIsSocketOnly(t *testing.T) {
	e := newAccountSocketEnv(t)
	own := "/v1/devices/" + e.dev.ID.String()
	rows := []struct {
		method, route string
		body          any
		want          int // the handler's own answer; a refusal of an empty fixture where success needs more
	}{
		{http.MethodPatch, "/v1/accounts/me", []any{nil, "writing code", nil}, http.StatusNoContent},
		{http.MethodPatch, "/v1/accounts/me/presence", []any{uint64(2)}, http.StatusNoContent},
		{http.MethodPost, "/v1/accounts/me/presence/activity", nil, http.StatusNoContent},
		{http.MethodGet, "/v1/devices", nil, http.StatusOK},
		{http.MethodPost, "/v1/devices", []any{}, http.StatusBadRequest},
		{http.MethodPatch, own, []any{"laptop"}, http.StatusNoContent},
		{http.MethodDelete, "/v1/devices/" + id.New().String(), nil, http.StatusNotFound},
		{http.MethodPost, "/v1/auth/password", []any{nil, "a long new password", nil}, http.StatusNoContent},
		{http.MethodPost, "/v1/auth/stepup", []any{uint64(0), "wrong"}, http.StatusForbidden},
		{http.MethodPost, "/v1/auth/stepup/passkey/begin", []any{}, http.StatusNotImplemented},
		{http.MethodPost, "/v1/auth/stepup/passkey/finish", []any{"00", "{}"}, http.StatusNotImplemented},
		{http.MethodPost, "/v1/auth/factors/remove", []any{"token", uint64(0), nil}, http.StatusForbidden},
		{http.MethodPost, "/v1/auth/recovery/codes", []any{"token"}, http.StatusForbidden},
		// Last: a deleted account ends every session after it.
		{http.MethodDelete, "/v1/accounts/me", nil, http.StatusNoContent},
	}
	socketPatterns := map[string]bool{}
	for _, p := range e.mux.SocketPatterns() {
		socketPatterns[p] = true
	}
	httpPatterns := map[string]bool{}
	for _, p := range e.mux.Patterns() {
		httpPatterns[p] = true
	}
	for _, r := range rows {
		t.Run(r.method+" "+r.route, func(t *testing.T) {
			// The registered pattern, with the device id wildcard in place of the id.
			pattern := r.method + " " + strings.NewReplacer(e.dev.ID.String(), "{device_id}").Replace(r.route)
			if strings.HasPrefix(r.route, "/v1/devices/") {
				pattern = r.method + " /v1/devices/{device_id}"
			}
			if !socketPatterns[pattern] || httpPatterns[pattern] {
				t.Fatalf("%s: on the socket mux %v, on the HTTP mux %v; want socket only", pattern, socketPatterns[pattern], httpPatterns[pattern])
			}
			if status := e.httpStatus(t, r.method, r.route, cborBody(t, r.body)); status != http.StatusNotFound && status != http.StatusMethodNotAllowed {
				t.Fatalf("over HTTP = %d, want 404 or 405", status)
			}
			status, body := sockettest.Do(t, e.disp, e.sess, r.method, r.route, cborBody(t, r.body))
			if status != r.want {
				t.Fatalf("over the socket = %d %x, want %d", status, body, r.want)
			}
		})
	}
}

// A session that owes a second factor is refused E_FACTOR_REQUIRED on the moved enrolled routes, as over HTTP; a
// device of another account is 404, never 403.
func TestSocketRoutesRefuseWeakerSessionsAsHTTPDid(t *testing.T) {
	e := newAccountSocketEnv(t)
	other, otherDev, _ := seedAPISession(t, e.deps)
	_ = other
	status, body := sockettest.Do(t, e.disp, e.sess, http.MethodDelete, "/v1/devices/"+otherDev.ID.String(), nil)
	if code, _ := sockErrorCode(t, body); status != http.StatusNotFound || code != string(server.CodeNotFound) {
		t.Fatalf("deleting another user's device = %d %s, want 404 E_NOT_FOUND", status, code)
	}
	if err := e.deps.Repo.SetFactorOwed(t.Context(), e.user.ID, true); err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct {
		method, route string
		body          any
	}{
		{http.MethodPatch, "/v1/accounts/me", []any{nil, "x", nil}},
		{http.MethodPost, "/v1/auth/stepup", []any{uint64(0), "pw"}},
		{http.MethodGet, "/v1/devices", nil},
	} {
		status, body := sockettest.Do(t, e.disp, e.sess, r.method, r.route, cborBody(t, r.body))
		if code, _ := sockErrorCode(t, body); status != http.StatusForbidden || code != "E_FACTOR_REQUIRED" {
			t.Errorf("%s %s for an owed session = %d %s, want 403 E_FACTOR_REQUIRED", r.method, r.route, status, code)
		}
	}
}

// TestOwedAccountFactorRoutesStayOnHTTP: an account that owes a second factor has no socket, so the routes it uses
// to set one up are served over HTTP for its session and are not socket routes.
func TestOwedAccountFactorRoutesStayOnHTTP(t *testing.T) {
	e := newAccountSocketEnv(t)
	if err := e.deps.Repo.SetFactorOwed(t.Context(), e.user.ID, true); err != nil {
		t.Fatal(err)
	}
	for _, p := range e.mux.SocketPatterns() {
		switch p {
		case "GET /v1/auth/factors", "POST /v1/auth/totp/enroll", "POST /v1/auth/totp/confirm",
			"POST /v1/auth/passkey/register/begin", "POST /v1/auth/passkey/register/finish", "GET /v1/accounts/me",
			"DELETE /v1/devices/{device_id}/sessions":
			t.Errorf("%s is an F route and must not be a socket route", p)
		}
	}
	for _, r := range []struct{ method, route string }{
		{http.MethodGet, "/v1/auth/factors"},
		{http.MethodGet, "/v1/accounts/me"},
		{http.MethodPost, "/v1/auth/totp/enroll"},
	} {
		status := e.httpStatus(t, r.method, r.route, cborBody(t, []any{nil}))
		if status == http.StatusNotFound || status == http.StatusMethodNotAllowed || status == http.StatusForbidden || status == http.StatusUnauthorized {
			t.Errorf("%s %s over HTTP for an owed session = %d, want the route's own answer", r.method, r.route, status)
		}
	}
}
