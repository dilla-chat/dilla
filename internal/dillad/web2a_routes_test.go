package dillad_test

// web2a_routes_test.go is web-2a's wiring through the composition root (task 8, lesson b): every
// route the plan mounts or changes — the four backup routes, the device-list read, history and
// publish, POST /v1/devices — is mounted behind the session middleware and the per-device meter,
// a pending session reaches exactly the surface protocol/02 item 4 names, and the chain from two
// host-login registrations to a revoking list that ends a device runs through dillad.New with the
// real verifier, assertion store, gateway and delivery service.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/dillad"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// recordLog is a slog.Handler that keeps every record, so a test can see what a hook that runs
// after its response logged.
type recordLog struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (l *recordLog) Enabled(context.Context, slog.Level) bool { return true }

func (l *recordLog) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recs = append(l.recs, r.Clone())
	return nil
}

func (l *recordLog) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *recordLog) WithGroup(string) slog.Handler      { return l }

// has reports whether a record at level, whose message contains msgPart, carries every attribute
// of want with exactly that string value.
func (l *recordLog) has(level slog.Level, msgPart string, want map[string]string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.recs {
		if r.Level != level || !strings.Contains(r.Message, msgPart) {
			continue
		}
		matched := 0
		r.Attrs(func(a slog.Attr) bool {
			if v, ok := want[a.Key]; ok && a.Value.String() == v {
				matched++
			}
			return true
		})
		if matched == len(want) {
			return true
		}
	}
	return false
}

// messages lists every captured record's level and message, for a failure report.
func (l *recordLog) messages() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.recs))
	for _, r := range l.recs {
		out = append(out, r.Level.String()+" "+r.Message)
	}
	return out
}

// newLoggedInstance is an instance on a fake clock (no bucket refills between requests) whose log
// is captured, served over a real listener for the gateway upgrade.
func newLoggedInstance(t *testing.T, logs *recordLog) (*dillad.Server, http.Handler, *httptest.Server, string) {
	t.Helper()
	cfg, code := testConfigInvite(t)
	srv, err := dillad.New(context.Background(), dillad.Options{
		Config: cfg, Clock: clock.NewFake(time.Now().Truncate(time.Second)), Wasm: sharedRuntime(t), Log: slog.New(logs),
	})
	if err != nil {
		t.Fatalf("dillad.New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, srv.Handler(), ts, code
}

// Every route web-2a mounts or changes answers 401 from the middleware without a token, never the
// mux's 404.
func TestEveryWebTwoARouteIsMountedBehindTheSessionMiddleware(t *testing.T) {
	srv, h, _ := newInstance(t)
	defer srv.Shutdown(context.Background())
	const x = "0102030405060708090a0b0c0d0e0f10"
	for _, r := range []struct{ method, path string }{
		{http.MethodPut, "/v1/backups/0/0"},
		{http.MethodGet, "/v1/backups"},
		{http.MethodGet, "/v1/backups/0/0"},
		{http.MethodDelete, "/v1/backups/0/0"},
		{http.MethodPut, "/v1/users/" + x + "/device-list"},
		{http.MethodGet, "/v1/users/" + x + "/device-list"},
		{http.MethodGet, "/v1/users/" + x + "/device-list?after=0"},
		{http.MethodPost, "/v1/devices"},
	} {
		if rec := call(t, h, r.method, r.path, "", nil); rec.Code != http.StatusUnauthorized || errorCode(rec) != "E_UNAUTHENTICATED" {
			t.Errorf("%s %s = %d %q, want 401 E_UNAUTHENTICATED", r.method, r.path, rec.Code, errorCode(rec))
		}
	}
}

// The whole of piece A's server side through dillad.New: two browsers join an account by host
// login and register as pending devices, reach only the pending surface, publish the lists that
// add them, become enrolled; a revoking list then closes one's socket with 4004, makes its token
// and its key worthless, and has the delivery service asked to remove its leaf from every group
// it holds. A stolen password for an account without a list registers nothing.
func TestTwoBrowsersEnrolByAssertionAndARevokingListEndsOne(t *testing.T) {
	logs := &recordLog{}
	srv, h, ts, code := newLoggedInstance(t, logs)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const password = "correct horse battery staple"
	ssk := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x5a}, 32))
	d0 := newTestDevice(t)
	alice, tok0 := accountWithKeys(t, h, code, "alice", password, d0, ssk)
	bob, bobTok := accountWithKeys(t, h, mintInvite(t, srv), "bob", password, newTestDevice(t),
		ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x6b}, 32)))
	satisfyFactor(t, srv, alice)
	satisfyFactor(t, srv, bob)
	own := "/v1/users/" + alice.String() + "/device-list"
	bobs := "/v1/users/" + bob.String() + "/device-list"
	must := func(rec *httptest.ResponseRecorder, want int, what string) *httptest.ResponseRecorder {
		t.Helper()
		if rec.Code != want {
			t.Fatalf("%s = %d %q (%x), want %d", what, rec.Code, errorCode(rec), rec.Body.Bytes(), want)
		}
		return rec
	}
	refused := func(rec *httptest.ResponseRecorder, want int, code, what string) {
		t.Helper()
		if rec.Code != want || errorCode(rec) != code {
			t.Errorf("%s = %d %q, want %d %s", what, rec.Code, errorCode(rec), want, code)
		}
	}

	body1, blob1 := signedDeviceList(t, ssk, alice, 1, nil, []gateEntry{entryFor(d0, 0, nil)})
	must(call(t, h, http.MethodPut, own, tok0, body1), http.StatusNoContent, "PUT v1")
	refused(call(t, h, http.MethodDelete, "/v1/backups/0/0", tok0, nil), http.StatusNotImplemented, "E_INTERNAL", "DELETE /v1/backups")

	// Browser 1: host login, registration, pending.
	d1 := newTestDevice(t)
	reg1 := []any{d1.ID, pubOf(d1), uint64(1), uint64(1), bytes.Repeat([]byte{0}, 10)}
	scope, p1 := scopeOf(t, establishAs(t, h, srv, d1, reg1, []byte(loginAssertion(t, h, "alice", password))))
	if scope != 1 {
		t.Fatalf("browser 1 registration: scope %d, want 1", scope)
	}
	// The pending surface: the two own-backup reads and the own list, read and history.
	must(call(t, h, http.MethodGet, "/v1/backups", p1, nil), http.StatusOK, "pending GET /v1/backups")
	refused(call(t, h, http.MethodGet, "/v1/backups/0/0", p1, nil), http.StatusNotFound, "E_NOT_FOUND", "pending GET root (none stored)")
	must(call(t, h, http.MethodGet, own, p1, nil), http.StatusOK, "pending GET own list")
	if rows := decodeArray(t, must(call(t, h, http.MethodGet, own+"?after=0", p1, nil), http.StatusOK, "pending history")); len(rows) != 1 {
		t.Fatalf("pending history = %v, want the one version", rows)
	}
	// And nothing else.
	for _, r := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, bobs, nil},
		{http.MethodGet, bobs + "?after=0", nil},
		{http.MethodPut, bobs, []any{}},
		{http.MethodPut, "/v1/backups/1/0", []any{[]byte{1}}},
		{http.MethodDelete, "/v1/backups/0/0", nil},
		{http.MethodPost, "/v1/devices", []any{}},
		{http.MethodGet, "/v1/devices", nil},
		{http.MethodPost, "/v1/keypackages", []any{}},
		{http.MethodGet, "/v1/welcomes", nil},
		{http.MethodGet, "/v1/accounts/me", nil},
		{http.MethodGet, "/v1/communities", nil},
		{http.MethodPost, "/v1/gateway/ticket", []any{}},
	} {
		refused(call(t, h, r.method, r.path, p1, r.body), http.StatusForbidden, "E_FORBIDDEN", "pending "+r.method+" "+r.path)
	}
	// The pending browser publishes the version that adds itself, then establishes enrolled.
	body2, blob2 := signedDeviceList(t, ssk, alice, 2, blob1, []gateEntry{entryFor(d0, 0, nil), entryFor(d1, 1, nil)})
	must(call(t, h, http.MethodPut, own, p1, body2), http.StatusNoContent, "pending PUT v2")
	scope, tok1 := scopeOf(t, establishAs(t, h, srv, d1, nil, nil))
	if scope != 0 {
		t.Fatalf("browser 1 after v2: scope %d, want 0", scope)
	}

	// Browser 2, the same way.
	d2 := newTestDevice(t)
	reg2 := []any{d2.ID, pubOf(d2), uint64(1), uint64(1), bytes.Repeat([]byte{0}, 10)}
	scope, p2 := scopeOf(t, establishAs(t, h, srv, d2, reg2, []byte(loginAssertion(t, h, "alice", password))))
	if scope != 1 {
		t.Fatalf("browser 2 registration: scope %d, want 1", scope)
	}
	body3, blob3 := signedDeviceList(t, ssk, alice, 3, blob2,
		[]gateEntry{entryFor(d0, 0, nil), entryFor(d1, 1, nil), entryFor(d2, 1, nil)})
	must(call(t, h, http.MethodPut, own, p2, body3), http.StatusNoContent, "pending PUT v3")
	scope, tok2 := scopeOf(t, establishAs(t, h, srv, d2, nil, nil))
	if scope != 0 {
		t.Fatalf("browser 2 after v3: scope %d, want 0", scope)
	}

	// Browser 2 holds a leaf in one group and an open gateway connection.
	cid, group := id.New(), id.New()
	if err := srv.Repo().CreateGroup(ctx, store.GroupRow{GroupID: group, Binding: []byte{0x80}, Kind: 0,
		CommunityID: &cid, TargetID: id.New(), Ciphersuite: 1, ExternalSenderKeyID: id.New(),
		E2EEVersion: 1, MediaVersion: 1, PolicyVersion: 1, Created: srv.Now().Unix()}); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	if err := srv.Repo().ReplaceMembers(ctx, group, 0, []store.MemberRow{{GroupID: group, LeafIndex: 0,
		UserID: alice, DeviceID: d2.ID, SignatureKey: pubOf(d2)}}); err != nil {
		t.Fatalf("ReplaceMembers: %v", err)
	}
	c := dialReady(t, ctx, ts, tok2)
	ended := make(chan error, 1)
	go func() {
		for {
			if _, _, err := c.Read(ctx); err != nil {
				ended <- err
				return
			}
		}
	}()

	// Browser 1 publishes v4, which revokes browser 2.
	now := uint64(srv.Now().Unix())
	body4, _ := signedDeviceList(t, ssk, alice, 4, blob3,
		[]gateEntry{entryFor(d0, 0, nil), entryFor(d1, 1, nil), entryFor(d2, 1, &now)})
	must(call(t, h, http.MethodPut, own, tok1, body4), http.StatusNoContent, "PUT v4 (revokes browser 2)")
	if got := websocket.CloseStatus(<-ended); got != 4004 {
		t.Fatalf("browser 2's socket ended with status %d, want 4004 session revoked", got)
	}
	refused(call(t, h, http.MethodGet, "/v1/accounts/me", tok2, nil), http.StatusUnauthorized, "E_UNAUTHENTICATED", "the revoked token")
	refused(establishAs(t, h, srv, d2, nil, nil), http.StatusUnauthorized, "E_UNAUTHENTICATED", "the revoked device's establish")
	devices := decodeArray(t, must(call(t, h, http.MethodGet, "/v1/devices", tok0, nil), http.StatusOK, "GET /v1/devices"))
	revokedListed := false
	for _, d := range devices {
		row, _ := d.([]any)
		if len(row) == 7 && bytes.Equal(row[0].([]byte), d2.ID[:]) {
			revokedListed = row[4] != nil
		}
	}
	if !revokedListed {
		t.Fatalf("GET /v1/devices = %v, want browser 2 with a revoked_at", devices)
	}
	if rows := decodeArray(t, must(call(t, h, http.MethodGet, own+"?after=0", tok0, nil), http.StatusOK, "history")); len(rows) != 4 {
		t.Fatalf("history after 0 = %d rows, want versions 1 to 4", len(rows))
	}
	// The hook asked the delivery service to remove browser 2's leaf from the seeded group; the
	// group has no guest state here, so the attempt is refused and logged as one record. The proof
	// is split (head L-HTTP-55): the internal/api unit test with recordingDS proves one
	// ProposeRemoveDevice per (group, revoked device); this record proves the composition root's
	// hook made the attempt; the real Remove of a real leaf is task 9's HARNESS `revoke` and task
	// 21's e2e. The hook runs inside the handler after the 204 is flushed, so it has finished when
	// `call` (h.ServeHTTP on a recorder) returns.
	const (
		removeLevel = slog.LevelError
		removeMsg   = "revoked device"
	)
	removeAttrs := map[string]string{"group": group.String(), "device": d2.ID.String()}
	if !logs.has(slog.LevelInfo, "removing revoked devices", map[string]string{"user": alice.String()}) {
		t.Fatalf("the composition root did not enter the prompt Remove path; records: %v", logs.messages())
	}
	if !logs.has(removeLevel, removeMsg, removeAttrs) {
		t.Fatalf("no Remove was attempted for browser 2 in group %s: no %s record containing %q with %v; records: %v",
			group, removeLevel, removeMsg, removeAttrs, logs.messages())
	}

	// A stolen password for an account that never published a list registers nothing.
	d3 := newTestDevice(t)
	reg3 := []any{d3.ID, pubOf(d3), uint64(1), uint64(1), bytes.Repeat([]byte{0}, 10)}
	refused(establishAs(t, h, srv, d3, reg3, []byte(loginAssertion(t, h, "bob", password))),
		http.StatusUnauthorized, "E_UNAUTHENTICATED", "registration for a no-list account")
	if _, err := srv.Repo().GetDevice(ctx, d3.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a stolen password created a device row: %v", err)
	}
	_ = bobTok
}

// The device-list and backup routes are metered per device session on the delivery service's read
// and write buckets, through the composition root's own limiter (fake clock: no refill).
func TestTheWebTwoARoutesAreMeteredPerDevice(t *testing.T) {
	srv, h, _, code := newLoggedInstance(t, &recordLog{})
	rate := config.Default().Limits.Rate
	d0 := newTestDevice(t)
	alice, tok0 := accountWithKeys(t, h, code, "alice", "correct horse battery staple", d0,
		ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x5a}, 32)))
	satisfyFactor(t, srv, alice)
	own := "/v1/users/" + alice.String() + "/device-list"
	for i := 0; i < rate.ReadBurst; i++ {
		path := own
		if i%2 == 1 {
			path = "/v1/backups"
		}
		if rec := call(t, h, http.MethodGet, path, tok0, nil); rec.Code == http.StatusTooManyRequests {
			t.Fatalf("read %d of a burst of %d was refused", i+1, rate.ReadBurst)
		}
	}
	// GET /v1/devices included (REGISTRATION-DEVICES-03: it was unmetered, so a stolen session
	// polled it for the recovering owner's new row).
	for _, p := range []string{own, own + "?after=0", "/v1/backups", "/v1/backups/1/0", "/v1/devices"} {
		if rec := call(t, h, http.MethodGet, p, tok0, nil); rec.Code != http.StatusTooManyRequests || errorCode(rec) != "E_RATE_LIMITED" {
			t.Errorf("GET %s past the read burst = %d %q, want 429 E_RATE_LIMITED", p, rec.Code, errorCode(rec))
		}
	}
	for i := 0; i < rate.WriteBurst; i++ {
		if rec := call(t, h, http.MethodPut, own, tok0, []any{}); rec.Code == http.StatusTooManyRequests {
			t.Fatalf("write %d of a burst of %d was refused", i+1, rate.WriteBurst)
		}
	}
	for _, r := range []struct {
		path string
		body any
	}{{own, []any{}}, {"/v1/backups/1/0", []any{[]byte{1}}}} {
		if rec := call(t, h, http.MethodPut, r.path, tok0, r.body); rec.Code != http.StatusTooManyRequests || errorCode(rec) != "E_RATE_LIMITED" {
			t.Errorf("PUT %s past the write burst = %d %q, want 429 E_RATE_LIMITED", r.path, rec.Code, errorCode(rec))
		}
	}
	// POST and DELETE /v1/devices spend the same write bucket (REGISTRATION-DEVICES-03).
	if rec := call(t, h, http.MethodPost, "/v1/devices", tok0, []any{}); rec.Code != http.StatusTooManyRequests || errorCode(rec) != "E_RATE_LIMITED" {
		t.Errorf("POST /v1/devices past the write burst = %d %q, want 429 E_RATE_LIMITED", rec.Code, errorCode(rec))
	}
	if rec := call(t, h, http.MethodDelete, "/v1/devices/"+d0.ID.String(), tok0, nil); rec.Code != http.StatusTooManyRequests || errorCode(rec) != "E_RATE_LIMITED" {
		t.Errorf("DELETE /v1/devices/{id} past the write burst = %d %q, want 429 E_RATE_LIMITED", rec.Code, errorCode(rec))
	}
	_ = srv
}
