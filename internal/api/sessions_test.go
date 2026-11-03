package api_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// The challenge answer is identical for an unknown device: the route is
// unauthenticated, so a different answer would make it a device-enumeration
// oracle (protocol/02 § Device sessions item 1).
func TestTheChallengeRouteIsNotAnEnumerationOracle(t *testing.T) {
	h, deps := newTestAPI(t)
	_, device, _ := seedAPIDevice(t, deps)

	known := postCBOR(h, "/v1/devices/"+device.ID.String()+"/sessions/challenge", nil)
	unknown := postCBOR(h, "/v1/devices/"+id.New().String()+"/sessions/challenge", nil)
	if known.Code != http.StatusCreated || unknown.Code != http.StatusCreated {
		t.Fatalf("challenge answered %d for a known device and %d for an unknown one",
			known.Code, unknown.Code)
	}
	var a, b []any
	if err := cborx.Unmarshal(known.Body.Bytes(), &a); err != nil {
		t.Fatalf("decode known: %v", err)
	}
	if err := cborx.Unmarshal(unknown.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode unknown: %v", err)
	}
	if len(a) != 2 || len(b) != 2 {
		t.Fatalf("challenge response has %d/%d elements, want [nonce, expires]", len(a), len(b))
	}
	if n, _ := a[0].([]byte); len(n) != 32 {
		t.Fatalf("nonce is %d bytes, want 32", len(n))
	}
	if a[1] != b[1] {
		t.Fatal("the expiry differs between a known and an unknown device")
	}
}

// The establish body is the five-element array of protocol/02 § Device sessions
// item 2, and the answer is the seven-element array of the same item.
func TestEstablishTakesFiveElementsAndAnswersSeven(t *testing.T) {
	h, deps := newTestAPI(t)
	_, device, priv := seedAPIDevice(t, deps)

	rec := postCBOR(h, "/v1/devices/"+device.ID.String()+"/sessions/challenge", nil)
	var challenge []any
	if err := cborx.Unmarshal(rec.Body.Bytes(), &challenge); err != nil {
		t.Fatalf("decode challenge: %v", err)
	}
	nonce, _ := challenge[0].([]byte)
	pre := auth.SessionPreimage(deps.Instance.InstanceID, device.ID, nonce, auth.PurposeSession)
	body, _ := cborx.Marshal([]any{nonce, uint64(auth.PurposeSession), ed25519.Sign(priv, pre), nil, nil})

	rec = postCBOR(h, "/v1/devices/"+device.ID.String()+"/sessions", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("establish = %d, want 201; body %x", rec.Code, rec.Body.Bytes())
	}
	var out []any
	if err := cborx.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 7 {
		t.Fatalf("establish response has %d elements, want 7", len(out))
	}
	if out[1] != uint64(auth.ScopeEnrolled) {
		t.Fatalf("scope = %v, want 0 (enrolled)", out[1])
	}
	if out[6] != deps.Instance.Generation {
		t.Fatalf("generation = %v, want the instance's %d", out[6], deps.Instance.Generation)
	}
	// The header and element 6 are the SAME number, not merely both present:
	// a client that sees 0 in one and 3 in the other cannot tell a restore from
	// a bug (R29, D12, and auth.NewSessions' own comment).
	gen, _ := out[6].(uint64)
	if got := rec.Header().Get("X-Dilla-Generation"); got != strconv.FormatUint(gen, 10) {
		t.Fatalf("X-Dilla-Generation = %q, want %d — the header and element 6 have drifted",
			got, gen)
	}
}

// A body of the wrong length is E_INVALID_REQUEST, never a partial read.
func TestEstablishRefusesAWrongLengthBody(t *testing.T) {
	h, deps := newTestAPI(t)
	_, device, _ := seedAPIDevice(t, deps)
	body, _ := cborx.Marshal([]any{make([]byte, 32), uint64(0), make([]byte, 64)})
	rec := postCBOR(h, "/v1/devices/"+device.ID.String()+"/sessions", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a three-element establish body gave %d, want 400", rec.Code)
	}
}

// The ticket route exists and is honest about not being wired until part 1b.
func TestTheGatewayTicketRouteIsReservedUntilPartOneB(t *testing.T) {
	h, deps := newTestAPI(t)
	_, _, token := seedAPISession(t, deps)
	rec := postCBORAuth(h, "/v1/gateway/ticket", nil, token)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("POST /v1/gateway/ticket = %d, want 501 while Deps.Tickets is nil", rec.Code)
	}
}

// doAuth sends a bodyless request with an optional bearer token. The session
// routes' refusals are about the caller's identity, so the body is never the
// interesting half.
func doAuth(h http.Handler, method, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// postFrom is postCBOR from a named client address, so a test can tell the
// per-address bucket from the per-device_id one: vary the address and only the
// device key can fill, vary the device and only the address key can.
func postFrom(h http.Handler, path, addr string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/cbor")
	req.RemoteAddr = addr + ":40000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// DELETE /v1/devices/{device_id}/sessions is "log this device out": it drops
// every session row of exactly one device, and it is refused for a device the
// caller does not own.
func TestDeletingADeviceSessionDropsOnlyThatDevice(t *testing.T) {
	h, deps := newTestAPI(t)
	_, deviceA, tokenA := seedAPISession(t, deps)
	_, deviceB, tokenB := seedAPISession(t, deps)
	pathA := "/v1/devices/" + deviceA.ID.String() + "/sessions"
	pathB := "/v1/devices/" + deviceB.ID.String() + "/sessions"

	// Another user's device and an identifier that does not exist answer the
	// SAME status, exactly as DELETE /v1/devices/{device_id} does: a route that
	// separated the two would tell any authenticated caller whether an
	// arbitrary 16-byte device id is real.
	other := doAuth(h, http.MethodDelete, pathB, tokenA)
	unknown := doAuth(h, http.MethodDelete, "/v1/devices/"+id.New().String()+"/sessions", tokenA)
	if other.Code != http.StatusNotFound || unknown.Code != http.StatusNotFound {
		t.Fatalf("another user's device answered %d and an unknown id %d, want 404 for both",
			other.Code, unknown.Code)
	}
	if a, b := refusalCode(t, other), refusalCode(t, unknown); a != "E_NOT_FOUND" || b != "E_NOT_FOUND" {
		t.Fatalf("refusal codes %q and %q, want E_NOT_FOUND for both", a, b)
	}
	if rec := doAuth(h, http.MethodGet, "/v1/accounts/me", tokenB); rec.Code != http.StatusOK {
		t.Fatalf("B's session answered %d after A tried to drop it, want 200", rec.Code)
	}

	// No bearer token at all is 401, not 204.
	if rec := doAuth(h, http.MethodDelete, pathA, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated delete = %d, want 401", rec.Code)
	}

	// The caller's own device: 204, and the token it authenticated with is gone.
	if rec := doAuth(h, http.MethodDelete, pathA, tokenA); rec.Code != http.StatusNoContent {
		t.Fatalf("deleting one's own device sessions = %d, want 204; body %x", rec.Code, rec.Body.Bytes())
	}
	if rec := doAuth(h, http.MethodGet, "/v1/accounts/me", tokenA); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the deleted token still answered %d, want 401: the rows are still there", rec.Code)
	}
	if rec := doAuth(h, http.MethodGet, "/v1/accounts/me", tokenB); rec.Code != http.StatusOK {
		t.Fatalf("B's session answered %d after A logged itself out, want 200", rec.Code)
	}
}

// Dropping a device's sessions must close its gateway connections in the same
// operation (protocol/02 § Device sessions item 6). Without this, "log out this
// device" removes the HTTP credential and leaves the socket that was already
// authenticated with it.
func TestDeletingADeviceSessionClosesItsGatewayConnections(t *testing.T) {
	h, deps := newTestAPI(t)
	var closed []id.ID
	deps.Sessions.OnRevoke = func(deviceID id.ID) { closed = append(closed, deviceID) }
	_, device, token := seedAPISession(t, deps)

	if rec := doAuth(h, http.MethodDelete, "/v1/devices/"+device.ID.String()+"/sessions", token); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", rec.Code)
	}
	if len(closed) != 1 || closed[0] != device.ID {
		t.Fatalf("the gateway was told to close %v, want exactly [%v]", closed, device.ID)
	}
}

// protocol/02 § Device sessions: `429 E_RATE_LIMITED per source address and per
// device_id` — for the section, which is both unauthenticated routes, and on
// both keys, because the address bucket alone is defeated by a distributed
// caller and the device bucket alone by an attacker-chosen id.
func TestTheSessionRoutesAreMeteredPerAddressAndPerDevice(t *testing.T) {
	const burst = 10

	t.Run("the challenge route meters the source address", func(t *testing.T) {
		h, _ := newTestAPI(t)
		const addr = "198.51.100.7"
		// A fresh device id every time, so nothing but the address key can fill.
		for i := 0; i < burst; i++ {
			if rec := postFrom(h, "/v1/devices/"+id.New().String()+"/sessions/challenge", addr, nil); rec.Code != http.StatusCreated {
				t.Fatalf("challenge %d of %d = %d, want 201", i+1, burst, rec.Code)
			}
		}
		rec := postFrom(h, "/v1/devices/"+id.New().String()+"/sessions/challenge", addr, nil)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("challenge %d = %d, want 429: a fresh device id buys a free burst from one address", burst+1, rec.Code)
		}
		if got := refusalCode(t, rec); got != "E_RATE_LIMITED" {
			t.Fatalf("refusal code = %q, want E_RATE_LIMITED", got)
		}
		if rec.Header().Get("Retry-After") == "" {
			t.Fatal("a rate refusal carries no Retry-After header")
		}
	})

	t.Run("the challenge route meters the device_id", func(t *testing.T) {
		h, _ := newTestAPI(t)
		path := "/v1/devices/" + id.New().String() + "/sessions/challenge"
		// A fresh address every time, so nothing but the device key can fill.
		for i := 0; i < burst; i++ {
			if rec := postFrom(h, path, fmt.Sprintf("198.51.100.%d", i+1), nil); rec.Code != http.StatusCreated {
				t.Fatalf("challenge %d of %d = %d, want 201", i+1, burst, rec.Code)
			}
		}
		rec := postFrom(h, path, "203.0.113.9", nil)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("challenge %d = %d, want 429: a fresh address buys a free burst for one device id", burst+1, rec.Code)
		}
		if got := refusalCode(t, rec); got != "E_RATE_LIMITED" {
			t.Fatalf("refusal code = %q, want E_RATE_LIMITED", got)
		}
	})

	t.Run("the establish route carries the same two buckets", func(t *testing.T) {
		h, _ := newTestAPI(t)
		const addr = "192.0.2.77"
		// An empty array is refused as E_INVALID_REQUEST, so a 429 here also
		// proves the meter runs BEFORE the body is decoded — the point of
		// metering an unauthenticated route at all.
		body := []byte{0x80}
		for i := 0; i < burst; i++ {
			if rec := postFrom(h, "/v1/devices/"+id.New().String()+"/sessions", addr, body); rec.Code == http.StatusTooManyRequests {
				t.Fatalf("establish %d of %d was refused inside the burst", i+1, burst)
			}
		}
		rec := postFrom(h, "/v1/devices/"+id.New().String()+"/sessions", addr, body)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("establish %d = %d, want 429: the establish route is unmetered", burst+1, rec.Code)
		}
		if got := refusalCode(t, rec); got != "E_RATE_LIMITED" {
			t.Fatalf("refusal code = %q, want E_RATE_LIMITED", got)
		}
	})

	t.Run("the challenge bucket is separate from the establish bucket", func(t *testing.T) {
		h, _ := newTestAPI(t)
		const addr = "192.0.2.90"
		device := "/v1/devices/" + id.New().String()
		for i := 0; i < burst; i++ {
			if rec := postFrom(h, device+"/sessions/challenge", addr, nil); rec.Code != http.StatusCreated {
				t.Fatalf("challenge %d of %d = %d, want 201", i+1, burst, rec.Code)
			}
		}
		// The challenge bucket is now empty on both of its keys; a client that
		// legitimately minted a nonce must still be able to spend it.
		if rec := postFrom(h, device+"/sessions", addr, []byte{0x80}); rec.Code == http.StatusTooManyRequests {
			t.Fatal("an emptied challenge bucket refused the establish route: the two share a key")
		}
	})
}

// A Deps built without a limiter must refuse the unauthenticated session routes
// rather than serve them unmetered, exactly as the registration routes do.
func TestAnUnwiredLimiterRefusesTheSessionRoutes(t *testing.T) {
	_, deps := newTestAPI(t)
	unwired := deps
	unwired.Limiter = nil
	m := server.NewMux()
	api.Register(m, unwired)

	for _, path := range []string{"/sessions/challenge", "/sessions"} {
		rec := postCBOR(m, "/v1/devices/"+id.New().String()+path, []byte{0x80})
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("%s = %d, want 500: a nil limiter must fail closed, not open", path, rec.Code)
		}
		if got := refusalCode(t, rec); got != "E_INTERNAL" {
			t.Fatalf("%s refusal code = %q, want E_INTERNAL", path, got)
		}
	}
}

// establishBody is the five-element establish body for device, signed by priv over a fresh
// challenge, with element 3 and element 4 exactly as given (nil encodes as CBOR null).
func establishBody(t *testing.T, h http.Handler, d api.Deps, device id.ID, priv ed25519.PrivateKey, element3, login any) []byte {
	t.Helper()
	// The unauthenticated challenge meter allows one request per second per address.
	if fake, ok := d.Clock.(*clock.Fake); ok {
		fake.Advance(time.Second)
	}
	rec := postCBOR(h, "/v1/devices/"+device.String()+"/sessions/challenge", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("challenge = %d", rec.Code)
	}
	var challenge []any
	if err := cborx.Unmarshal(rec.Body.Bytes(), &challenge); err != nil {
		t.Fatalf("decode challenge: %v", err)
	}
	nonce, _ := challenge[0].([]byte)
	pre := auth.SessionPreimage(d.Instance.InstanceID, device, nonce, auth.PurposeSession)
	body, err := cborx.Marshal([]any{nonce, uint64(auth.PurposeSession), ed25519.Sign(priv, pre), element3, login})
	if err != nil {
		t.Fatalf("marshal establish: %v", err)
	}
	return body
}

// L-HTTP-54: element 3 is a registration array only when login is present; without login it is
// ignored whatever its type. Attacker statement for the 400s: only the registering client's own
// malformed body triggers them, and nothing is spent or written.
func TestElementThreeIsARegistrationArrayOnlyWhenLoginIsPresent(t *testing.T) {
	h, deps := newTestAPI(t)
	ctx := context.Background()
	lister := listerOf(t, deps)
	u, dev, priv := seedAPIDevice(t, deps)
	lister.list(u.ID, dev)
	path := func(d id.ID) string { return "/v1/devices/" + d.String() + "/sessions" }
	assertion := func() []byte { return []byte(deps.Assertions.Issue(u.ID, 0)) }

	// No login: the testkit's bstr credential is ignored.
	rec := postCBOR(h, path(dev.ID), establishBody(t, h, deps, dev.ID, priv, []byte{0xa1, 0x01}, nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("a bstr element 3 without login = %d %x, want 201", rec.Code, rec.Body.Bytes())
	}
	var out []any
	if err := cborx.Unmarshal(rec.Body.Bytes(), &out); err != nil || out[1] != uint64(auth.ScopeEnrolled) {
		t.Fatalf("scope %v (err %v), want 0", out, err)
	}

	newPub, newPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	newID := id.New()
	for name, element3 := range map[string]any{
		"a bstr":                   []byte{1},
		"null":                     nil,
		"a four-element array":     []any{newID, []byte(newPub), uint64(1), uint64(1)},
		"a 31-byte dsk_pub":        []any{newID, make([]byte, 31), uint64(1), uint64(1), []byte{0}},
		"a tier of 2":              []any{newID, []byte(newPub), uint64(2), uint64(1), []byte{0}},
		"an empty credential":      []any{newID, []byte(newPub), uint64(1), uint64(1), []byte{}},
		"another device than path": []any{id.New(), []byte(newPub), uint64(1), uint64(1), []byte{0}},
	} {
		rec := postCBOR(h, path(newID), establishBody(t, h, deps, newID, newPriv, element3, assertion()))
		if rec.Code != http.StatusBadRequest || refusalCode(t, rec) != "E_INVALID_REQUEST" {
			t.Fatalf("%s with login = %d %q, want 400 E_INVALID_REQUEST", name, rec.Code, refusalCode(t, rec))
		}
	}
	if _, err := deps.Repo.GetDevice(ctx, newID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a refused registration wrote a row: %v", err)
	}
	// Head ruling 31. Attacker statement: only the registering client's own body; a password alone
	// can no longer mint a `native` row with 30-day sessions and the `app` label in Settings;
	// protocol/03's pairing ceremony is the native path.
	cred := bytes.Repeat([]byte{0}, 10)
	for name, element3 := range map[string]any{
		"a native tier":        []any{newID, []byte(newPub), uint64(0), uint64(0), cred},
		"a native signer tier": []any{newID, []byte(newPub), uint64(1), uint64(0), cred},
	} {
		rec := postCBOR(h, path(newID), establishBody(t, h, deps, newID, newPriv, element3, assertion()))
		var refusalBody []any
		if err := cborx.Unmarshal(rec.Body.Bytes(), &refusalBody); err != nil || len(refusalBody) < 2 {
			t.Fatalf("%s: undecodable answer %d %x (err %v)", name, rec.Code, rec.Body.Bytes(), err)
		}
		if rec.Code != http.StatusBadRequest || refusalBody[0] != "E_INVALID_REQUEST" ||
			refusalBody[1] != "assertion registration is for browser devices" {
			t.Fatalf("%s with login = %d %v %q, want 400 E_INVALID_REQUEST \"assertion registration is for browser devices\"",
				name, rec.Code, refusalBody[0], refusalBody[1])
		}
		if _, err := deps.Repo.GetDevice(ctx, newID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%s: a refused registration wrote a row: %v", name, err)
		}
	}

	reg := []any{newID, []byte(newPub), uint64(1), uint64(1), cred}
	tooLong := []any{newID, []byte(newPub), uint64(1), uint64(1), bytes.Repeat([]byte{0}, 2049)}
	rec = postCBOR(h, path(newID), establishBody(t, h, deps, newID, newPriv, tooLong, assertion()))
	if rec.Code != http.StatusBadRequest || refusalCode(t, rec) != "E_INVALID_REQUEST" {
		t.Fatalf("2049-byte credential = %d %q, want 400 E_INVALID_REQUEST", rec.Code, refusalCode(t, rec))
	}
	rec = postCBOR(h, path(newID), bytes.Repeat([]byte{0}, 8193))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("8193-byte establish body = %d, want 413", rec.Code)
	}
	rec = postCBOR(h, path(newID), establishBody(t, h, deps, newID, newPriv, reg, assertion()))
	if rec.Code != http.StatusCreated {
		t.Fatalf("registration = %d %x, want 201", rec.Code, rec.Body.Bytes())
	}
	out = nil
	if err := cborx.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	userID, _ := out[2].([]byte)
	deviceID, _ := out[3].([]byte)
	if out[1] != uint64(auth.ScopePending) || !bytes.Equal(userID, u.ID[:]) || !bytes.Equal(deviceID, newID[:]) {
		t.Fatalf("registration answered %v, want scope 1 for %s's device %s", out, u.ID, newID)
	}
	row, err := deps.Repo.GetDevice(ctx, newID)
	if err != nil || row.UserID != u.ID || row.VerifiedAt != nil || row.Tier != 1 || row.SignerTier != 1 {
		t.Fatalf("the registered row = %+v (err %v), want an unverified browser row", row, err)
	}
}

// Q26 through the handler. Attacker statement: a stolen password for an account whose owner never
// published a list registers nothing.
func TestAStolenPasswordCannotRegisterADeviceForAUserWithNoList(t *testing.T) {
	h, deps := newTestAPI(t)
	u, _, _ := seedAPIDevice(t, deps) // never listed: the harness lister answers ErrNoDeviceList
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	newID := id.New()
	reg := []any{newID, []byte(pub), uint64(1), uint64(1), []byte{0}}
	rec := postCBOR(h, "/v1/devices/"+newID.String()+"/sessions",
		establishBody(t, h, deps, newID, priv, reg, []byte(deps.Assertions.Issue(u.ID, 0))))
	if rec.Code != http.StatusUnauthorized || refusalCode(t, rec) != "E_UNAUTHENTICATED" {
		t.Fatalf("registration for a no-list user = %d %q, want 401 E_UNAUTHENTICATED", rec.Code, refusalCode(t, rec))
	}
	if _, err := deps.Repo.GetDevice(context.Background(), newID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a stolen password created a device row: %v", err)
	}
}
