package api_test

// devices_test.go covers the device and device-list routes as web-2a reshapes them: what a
// pending session (a host login without the recovery key, L-HTTP-57) reaches, and — task 8 — the
// verified publication, the revocation action and the history read.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/api"
	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/clock"
	"github.com/jonasthim/dilla/internal/config"
	"github.com/jonasthim/dilla/internal/ds"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/mlswasi"
	"github.com/jonasthim/dilla/internal/store"
)

// cborCall sends one request through the real mux; body is CBOR-encoded when it is not nil.
func cborCall(t *testing.T, h http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		if raw, err = cborx.Marshal(body); err != nil {
			t.Fatalf("marshal %s %s: %v", method, path, err)
		}
	}
	req := httptest.NewRequestWithContext(context.Background(), method, path, bytes.NewReader(raw))
	if body != nil {
		req.Header.Set("Content-Type", "application/cbor")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// pendingSession registers a fresh browser device for user through the assertion path and
// returns its id and its pending token. The user must be listed in the harness lister.
func pendingSession(t *testing.T, h http.Handler, d api.Deps, user id.ID) (id.ID, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	device := id.New()
	reg := []any{device, []byte(pub), uint64(1), uint64(1), bytes.Repeat([]byte{0}, 10)}
	rec := postCBOR(h, "/v1/devices/"+device.String()+"/sessions",
		establishBody(t, h, d, device, priv, reg, []byte(d.Assertions.Issue(user, 0))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("pending registration = %d %x", rec.Code, rec.Body.Bytes())
	}
	var out []any
	if err := cborx.Unmarshal(rec.Body.Bytes(), &out); err != nil || out[1] != uint64(1) {
		t.Fatalf("pending registration answered %v (err %v), want scope 1", out, err)
	}
	token, _ := out[0].(string)
	return device, token
}

// devicePostBody is POST /v1/devices' seven-element body for device under pub, with the proof of
// possession of security review F2: a challenge nonce for device signed by signer over the
// establish preimage (purpose 0). An honest client signs with pub's own private half.
func devicePostBody(t *testing.T, d api.Deps, device id.ID, pub []byte, signer ed25519.PrivateKey) []any {
	t.Helper()
	nonce, _, err := d.Sessions.Challenge(context.Background(), device)
	if err != nil {
		t.Fatalf("Challenge: %v", err)
	}
	sig := ed25519.Sign(signer, auth.SessionPreimage(d.Sessions.InstanceID(), device, nonce, auth.PurposeSession))
	return []any{device, pub, uint64(1), uint64(1), testDeviceCredential(t, id.ID{}, device, pub, 0), nonce, sig}
}

// testUMK and testSSK are the identity keys seedAPIDevice gives every seeded account (users.umk_pub,
// ssk_pub, sig_umk_ssk), so a credential testDeviceCredential builds is one POST /v1/devices binds
// to the session's account (Task S1). testIdentityOf is a fresh identity for the refusal cases.
var (
	testUMK = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0xa1}, ed25519.SeedSize))
	testSSK = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0xa2}, ed25519.SeedSize))
)

// testAccountKeys is (umk_pub, ssk_pub, sig_umk_ssk) of the shared test identity.
func testAccountKeys() (umkPub, sskPub, sigUMKSSK []byte) {
	umkPub = testUMK.Public().(ed25519.PublicKey)
	sskPub = testSSK.Public().(ed25519.PublicKey)
	return umkPub, sskPub, ed25519.Sign(testUMK, append([]byte("dilla ssk v1"), sskPub...))
}

// testDeviceCredential is a CredentialIdentity the WASI verifier accepts for a browser device (tier 1, signer tier 1)
// under the shared test identity: the SSK signed by the UMK, the device's dsk_pub signed by the SSK. Task 8b security
// review M1.
func testDeviceCredential(t *testing.T, user, device id.ID, dskPub []byte, kind byte) []byte {
	t.Helper()
	return testDeviceCredentialUnder(t, testUMK, testSSK, user, device, dskPub, kind)
}

// testDeviceCredentialUnder is testDeviceCredential under the given UMK and SSK.
func testDeviceCredentialUnder(t *testing.T, umk, ssk ed25519.PrivateKey, user, device id.ID, dskPub []byte, kind byte) []byte {
	t.Helper()
	sskPub := ssk.Public().(ed25519.PublicKey)
	umkPub := umk.Public().(ed25519.PublicKey)
	sigUMKSSK := ed25519.Sign(umk, append([]byte("dilla ssk v1"), sskPub...))
	message := append(append(append([]byte("dilla dsk v1"), device[:]...), dskPub...), kind, 1, 1)
	blob, err := cborx.Marshal([]any{uint64(1), []byte(umkPub), user[:], device[:], uint64(kind), uint64(1), uint64(1), []byte(sskPub), sigUMKSSK, ed25519.Sign(ssk, message)})
	if err != nil {
		t.Fatal(err)
	}
	return blob
}

// postFreshDevice registers a fresh browser device under a fresh key through POST /v1/devices.
func postFreshDevice(t *testing.T, h http.Handler, d api.Deps, token string) (store.DeviceRow, *httptest.ResponseRecorder) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	row := store.DeviceRow{ID: id.New(), DSKPub: pub}
	return row, cborCall(t, h, http.MethodPost, "/v1/devices", token, devicePostBody(t, d, row.ID, pub, priv))
}

// L-HTTP-57. Attacker statement for the 403s: a pending session is a host login without the
// recovery key; it reads and publishes only its own list (what its own enrolment needs) and no
// other route, so a stolen password reaches nothing of another user and nothing of the groups.
func TestAPendingSessionReachesItsOwnDeviceListAndNothingElse(t *testing.T) {
	h, deps := newTestAPI(t)
	ctx := context.Background()
	owner, dev, _ := seedAPIDevice(t, deps)
	listerOf(t, deps).list(owner.ID, dev)
	other, _, otherToken := seedAPISession(t, deps)
	stored := store.DeviceListRow{UserID: owner.ID, Version: 1, Blob: []byte{0x80},
		SSKSignature: bytes.Repeat([]byte{7}, 64), PrevHash: make([]byte, 32), Created: deps.Clock.Now().Unix()}
	if err := deps.Repo.PutDeviceList(ctx, stored); err != nil {
		t.Fatalf("PutDeviceList: %v", err)
	}
	_, pending := pendingSession(t, h, deps, owner.ID)
	own := "/v1/users/" + owner.ID.String() + "/device-list"
	theirs := "/v1/users/" + other.ID.String() + "/device-list"

	rec := cborCall(t, h, http.MethodGet, own, pending, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("a pending session reading its own list = %d %x, want 200", rec.Code, rec.Body.Bytes())
	}
	var got []any
	if err := cborx.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got) != 4 || got[0] != uint64(1) {
		t.Fatalf("own list = %v (err %v), want [1, blob, sig, prev]", got, err)
	}
	// The publish route is reached (the handler's own 400 for an empty body, never the scope's 403).
	if rec := cborCall(t, h, http.MethodPut, own, pending, []any{}); rec.Code != http.StatusBadRequest {
		t.Fatalf("a pending session publishing its own list = %d %q, want the handler's 400", rec.Code, refusalCode(t, rec))
	}
	for _, r := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, theirs, nil},
		{http.MethodPut, theirs, []any{}},
		{http.MethodPost, "/v1/devices", []any{}},
		{http.MethodGet, "/v1/devices", nil},
		{http.MethodDelete, "/v1/devices/" + dev.ID.String(), nil},
		{http.MethodDelete, "/v1/devices/" + dev.ID.String() + "/sessions", nil},
		{http.MethodGet, "/v1/accounts/me", nil},
		{http.MethodPatch, "/v1/accounts/me", []any{nil, "x", nil}},
		{http.MethodPost, "/v1/gateway/ticket", []any{}},
	} {
		rec := cborCall(t, h, r.method, r.path, pending, r.body)
		if rec.Code != http.StatusForbidden || refusalCode(t, rec) != "E_FORBIDDEN" {
			t.Errorf("pending %s %s = %d %q, want 403 E_FORBIDDEN", r.method, r.path, rec.Code, refusalCode(t, rec))
		}
	}
	// An enrolled session still reads any user's list (DEV-W2-34 stays open for web-2c).
	if rec := cborCall(t, h, http.MethodGet, own, otherToken, nil); rec.Code != http.StatusOK {
		t.Fatalf("an enrolled session reading another user's list = %d, want 200", rec.Code)
	}
}

// An enrolled token alone must not bypass the same device cap and hourly rate as
// assertion registration. A stolen enrolled token cannot fill unlimited rows: a cap of listed rows
// refuses, and past the rate the oldest unlisted row is replaced (F1).
func TestPostDevicesAppliesTheCapAndRate(t *testing.T) {
	h, deps := newTestAPI(t)
	user, first, token := seedAPISession(t, deps)
	listed := []store.DeviceRow{first}
	for i := 0; i < 7; i++ {
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		row := store.DeviceRow{ID: id.New(), UserID: user.ID,
			DSKPub: pub, CredentialBlob: []byte{1}, Created: 1, LastSeen: 1}
		listed = append(listed, row)
		if err := deps.Repo.CreateDevice(t.Context(), row); err != nil {
			t.Fatal(err)
		}
	}
	listerOf(t, deps).list(user.ID, listed...)
	create := func() *httptest.ResponseRecorder {
		_, rec := postFreshDevice(t, h, deps, token)
		return rec
	}
	if rec := create(); rec.Code != http.StatusForbidden || refusalCode(t, rec) != "E_FORBIDDEN" {
		t.Fatalf("POST /v1/devices at cap = %d %q, want 403 E_FORBIDDEN", rec.Code, refusalCode(t, rec))
	}
	// Free the cap, then consume the hourly rate with three live unlisted rows.
	rows, err := deps.Repo.ListDevicesByUser(t.Context(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows[1:] {
		if err := deps.Repo.RevokeDevice(t.Context(), row.ID, deps.Clock.Now().Unix()); err != nil {
			t.Fatal(err)
		}
	}
	var created []id.ID
	for i := 0; i < 3; i++ {
		rec := create()
		if rec.Code != http.StatusOK {
			t.Fatalf("POST /v1/devices #%d = %d", i+1, rec.Code)
		}
		var out []id.ID
		if err := cborx.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out) != 1 {
			t.Fatalf("POST /v1/devices #%d answered %x (err %v)", i+1, rec.Body.Bytes(), err)
		}
		created = append(created, out[0])
		deps.Clock.(*clock.Fake).Advance(time.Second)
	}
	// Changed by REGISTRATION-DEVICES-03 (it was F1's replacement of the oldest unlisted row, 200):
	// the enrolled route never evicts. Past the hourly rate the fourth is 429 and every row stays.
	if rec := create(); rec.Code != http.StatusTooManyRequests || refusalCode(t, rec) != "E_RATE_LIMITED" {
		t.Fatalf("POST /v1/devices past the rate = %d %x, want 429 E_RATE_LIMITED", rec.Code, rec.Body.Bytes())
	}
	for i := range created {
		row, err := deps.Repo.GetDevice(t.Context(), created[i])
		if err != nil || row.RevokedAt != nil {
			t.Fatalf("row %d after the refused fourth = %+v, err %v; want live", i, row, err)
		}
	}
}

// Changed by REGISTRATION-DEVICES-03 (it asserted that the enrolled route evicts the oldest
// unlisted row at the cap, as assertion registration does): at the cap POST /v1/devices answers
// 403 "device cap reached" and both unlisted rows stay; the caller frees a slot itself.
func TestPostDevicesRefusesAtCapInsteadOfEvicting(t *testing.T) {
	h, deps := newTestAPI(t)
	user, first, token := seedAPISession(t, deps)
	listed := []store.DeviceRow{first}
	for i := 0; i < 5; i++ {
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		row := store.DeviceRow{ID: id.New(), UserID: user.ID,
			DSKPub: pub, CredentialBlob: []byte{1}, Created: 1, LastSeen: 1}
		listed = append(listed, row)
		if err := deps.Repo.CreateDevice(t.Context(), row); err != nil {
			t.Fatal(err)
		}
	}
	listerOf(t, deps).list(user.ID, listed...)
	now := deps.Clock.Now().Unix()
	var unlisted [2]id.ID
	for i := range unlisted {
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		unlisted[i] = id.New()
		if err := deps.Repo.CreateDevice(t.Context(), store.DeviceRow{ID: unlisted[i], UserID: user.ID,
			DSKPub: pub, CredentialBlob: []byte{1}, Created: now - int64(2-i), LastSeen: now}); err != nil {
			t.Fatal(err)
		}
	}
	fresh, rec := postFreshDevice(t, h, deps, token)
	wantListRefusal(t, rec, http.StatusForbidden, "E_FORBIDDEN", "device cap reached")
	noAPIDeviceRow(t, deps, fresh.ID)
	for i := range unlisted {
		wantAPIDeviceLive(t, deps, "an unlisted row at the cap", unlisted[i], true)
	}
}

func TestPostDevicesSweepsExpiredUnlistedRow(t *testing.T) {
	h, deps := newTestAPI(t)
	user, first, token := seedAPISession(t, deps)
	listerOf(t, deps).list(user.ID, first)
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	old := id.New()
	if err := deps.Repo.CreateDevice(t.Context(), store.DeviceRow{ID: old, UserID: user.ID,
		DSKPub: pub, CredentialBlob: []byte{1}, Created: deps.Clock.Now().Unix() - 86400, LastSeen: 1}); err != nil {
		t.Fatal(err)
	}
	_, rec := postFreshDevice(t, h, deps, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST after expiry = %d %x, want 200", rec.Code, rec.Body.Bytes())
	}
	row, err := deps.Repo.GetDevice(t.Context(), old)
	if err != nil || row.RevokedAt == nil {
		t.Fatalf("expired unlisted row = %+v, err %v; want revoked", row, err)
	}
}

// The no-list clause keeps a web-1 account's first device live while its owner
// uses the enrolled route; an absent list is not an omitted entry in a list.
func TestPostDevicesDoesNotExpireANoListAccount(t *testing.T) {
	h, deps := newTestAPI(t)
	user, first, token := seedAPISession(t, deps)
	deps.Clock.(*clock.Fake).Advance(24 * time.Hour)
	_, rec := postFreshDevice(t, h, deps, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("no-list device creation = %d", rec.Code)
	}
	row, err := deps.Repo.GetDevice(t.Context(), first.ID)
	if err != nil || row.RevokedAt != nil || row.UserID != user.ID {
		t.Fatalf("first device after POST = %+v, err %v; want live", row, err)
	}
}

// Branch review REGISTRATION-DEVICES-03. Attacker statement: the attacker holds an enrolled session
// of the user (a stolen browser token, or a stolen device) and polls GET /v1/devices; the owner,
// with no other enrolled device, starts recovery on a new browser, whose row O is unlisted until
// the list PUT that would revoke the attacker. Before the fix one key-less DELETE of O, or three
// POST /v1/devices past the hourly rate (each evicting the oldest unlisted row), cut the owner's
// pending session at no login cost, on every attempt. Now the DELETE of an unlisted row younger than
// ten minutes is 409 unless it is the caller's own row, and POST /v1/devices refuses past the rate
// (429) or the cap (403) instead of evicting; eviction is left to assertion registration, which
// costs a host login.
func TestAnEnrolledSessionCannotRemoveOrEvictARecoveringOwnersNewRow(t *testing.T) {
	h, deps := newTestAPI(t)
	clk := deps.Clock.(*clock.Fake)
	user, thief, thiefToken := seedAPISession(t, deps)
	listerOf(t, deps).list(user.ID, thief)
	owner, ownerToken := pendingSession(t, h, deps, user.ID)
	clk.Advance(time.Minute)

	path := "/v1/devices/" + owner.String()
	wantListRefusal(t, cborCall(t, h, http.MethodDelete, path, thiefToken, nil), http.StatusConflict, "E_INVALID_REQUEST",
		"the device registered less than 10 minutes ago and is too new to remove; its own session may remove it, or it expires unlisted after 24 hours")
	wantAPIDeviceLive(t, deps, "the owner's 1-minute-old row after a DELETE by another device", owner, true)

	// The thief's device and the owner's row are two live creations of the hour; one more is
	// admitted, and every POST after it is past the rate and refused with the wait until the oldest
	// creation leaves the window.
	if _, rec := postFreshDevice(t, h, deps, thiefToken); rec.Code != http.StatusOK {
		t.Fatalf("POST /v1/devices under the rate = %d %x, want 200", rec.Code, rec.Body.Bytes())
	}
	for i := 0; i < 3; i++ {
		row, rec := postFreshDevice(t, h, deps, thiefToken)
		if rec.Code != http.StatusTooManyRequests || refusalCode(t, rec) != "E_RATE_LIMITED" {
			t.Fatalf("POST /v1/devices past the rate = %d %x, want 429 E_RATE_LIMITED", rec.Code, rec.Body.Bytes())
		}
		var body []any
		if err := cborx.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body) < 3 {
			t.Fatalf("429 body %x (err %v), want [code, detail, retry_after_ms]", rec.Body.Bytes(), err)
		}
		// The oldest live creation (the thief's device) leaves the hour at its creation + 3600 s.
		want := uint64(thief.Created+3600-clk.Now().Unix()) * 1000
		if ms, ok := body[2].(uint64); !ok || ms != want {
			t.Fatalf("retry_after_ms = %v, want %d (when the oldest creation leaves the hour)", body[2], want)
		}
		noAPIDeviceRow(t, deps, row.ID)
	}
	wantAPIDeviceLive(t, deps, "the owner's row after POSTs past the rate", owner, true)
	if rec := cborCall(t, h, http.MethodGet, "/v1/users/"+user.ID.String()+"/device-list", ownerToken, nil); rec.Code == http.StatusUnauthorized {
		t.Fatal("the owner's pending session was cut")
	}

	// The grace is ten minutes: after it the owner (or anyone enrolled) clears a stray row.
	clk.Advance(9 * time.Minute)
	if rec := cborCall(t, h, http.MethodDelete, path, thiefToken, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE of a 10-minute-old unlisted row = %d %x, want 204", rec.Code, rec.Body.Bytes())
	}
	wantAPIDeviceLive(t, deps, "the 10-minute-old row after DELETE", owner, false)
}

// The grace does not stop a device removing its own new row: a no-list account's young device
// (unlisted, enrolled) deletes itself, while another device of the account may not.
func TestADeviceMayRemoveItsOwnNewRow(t *testing.T) {
	h, deps := newTestAPI(t)
	user, first, firstToken := seedAPISession(t, deps)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	second := store.DeviceRow{ID: id.New(), UserID: user.ID, DSKPub: pub, CredentialBlob: []byte{1},
		Created: deps.Clock.Now().Unix(), LastSeen: deps.Clock.Now().Unix()}
	if err := deps.Repo.CreateDevice(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	nonce, _, err := deps.Sessions.Challenge(t.Context(), second.ID)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := deps.Sessions.Establish(t.Context(), auth.EstablishRequest{DeviceID: second.ID, Nonce: nonce,
		Purpose: auth.PurposeSession, Sig: ed25519.Sign(priv, auth.SessionPreimage(deps.Sessions.InstanceID(), second.ID, nonce, auth.PurposeSession))})
	if err != nil || tok.Scope != auth.ScopeEnrolled {
		t.Fatalf("establish the second device: %v (scope %d)", err, tok.Scope)
	}
	if rec := cborCall(t, h, http.MethodDelete, "/v1/devices/"+second.ID.String(), firstToken, nil); rec.Code != http.StatusConflict {
		t.Fatalf("another device's DELETE of a new row = %d %x, want 409", rec.Code, rec.Body.Bytes())
	}
	if rec := cborCall(t, h, http.MethodDelete, "/v1/devices/"+second.ID.String(), tok.Token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("a device's DELETE of its own new row = %d %x, want 204", rec.Code, rec.Body.Bytes())
	}
	wantAPIDeviceLive(t, deps, "the self-removed row", second.ID, false)
	wantAPIDeviceLive(t, deps, "the other device", first.ID, true)
}

// Fix-wave review NEW-1. Attacker statement: as above, the attacker holds an enrolled session of
// the user and polls GET /v1/devices; the sibling route DELETE /v1/devices/{id}/sessions cut the
// recovering owner's pending session (200 DELETEs, all 204, then the owner's token answered 401)
// with no meter and no grace. Now it spends the write bucket and refuses another device's
// unlisted row younger than ten minutes with 409, so 200 DELETEs inside the grace leave the
// owner's pending token valid; after the grace a stray row's sessions are removable again.
func TestAnEnrolledSessionCannotCutARecoveringOwnersPendingSession(t *testing.T) {
	h, deps := newTestAPI(t)
	clk := deps.Clock.(*clock.Fake)
	user, thief, thiefToken := seedAPISession(t, deps)
	listerOf(t, deps).list(user.ID, thief)
	owner, ownerToken := pendingSession(t, h, deps, user.ID)
	clk.Advance(time.Minute)

	path := "/v1/devices/" + owner.String() + "/sessions"
	ownList := "/v1/users/" + user.ID.String() + "/device-list"
	const detail = "the device registered less than 10 minutes ago and is too new to remove its sessions; its own session may remove them, or it expires unlisted after 24 hours"
	refused, limited := 0, 0
	for i := 0; i < 200; i++ {
		rec := cborCall(t, h, http.MethodDelete, path, thiefToken, nil)
		switch {
		case rec.Code == http.StatusConflict:
			wantListRefusal(t, rec, http.StatusConflict, "E_INVALID_REQUEST", detail)
			refused++
		case rec.Code == http.StatusTooManyRequests && refusalCode(t, rec) == "E_RATE_LIMITED":
			limited++
		default:
			t.Fatalf("DELETE #%d of the owner's sessions = %d %x, want 409 or 429", i+1, rec.Code, rec.Body.Bytes())
		}
	}
	if refused == 0 || limited == 0 {
		t.Fatalf("200 DELETEs: %d refused by the grace, %d by the write bucket; want both", refused, limited)
	}
	if rec := cborCall(t, h, http.MethodGet, ownList, ownerToken, nil); rec.Code == http.StatusUnauthorized {
		t.Fatal("the owner's pending session was cut inside the grace")
	}

	// After the grace (and a refilled bucket) the young-row rule no longer applies.
	clk.Advance(10 * time.Minute)
	if rec := cborCall(t, h, http.MethodDelete, path, thiefToken, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE of a 11-minute-old unlisted row's sessions = %d %x, want 204", rec.Code, rec.Body.Bytes())
	}
	if rec := cborCall(t, h, http.MethodGet, ownList, ownerToken, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the stray row's token after the DELETE = %d, want 401", rec.Code)
	}
}

// The sessions DELETE's grace covers unlisted rows only, and never the caller's own device: a
// listed device's sessions are removable at any age, and a young unlisted device logs itself out.
func TestTheSessionsDeleteGraceSparesListedRowsAndTheCallersOwnDevice(t *testing.T) {
	h, deps := newTestAPI(t)
	user, first, firstToken := seedAPISession(t, deps)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := deps.Clock.Now().Unix()
	second := store.DeviceRow{ID: id.New(), UserID: user.ID, DSKPub: pub, CredentialBlob: []byte{1}, Created: now, LastSeen: now}
	if err := deps.Repo.CreateDevice(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	establish := func() string {
		nonce, _, err := deps.Sessions.Challenge(t.Context(), second.ID)
		if err != nil {
			t.Fatal(err)
		}
		tok, err := deps.Sessions.Establish(t.Context(), auth.EstablishRequest{DeviceID: second.ID, Nonce: nonce,
			Purpose: auth.PurposeSession, Sig: ed25519.Sign(priv, auth.SessionPreimage(deps.Sessions.InstanceID(), second.ID, nonce, auth.PurposeSession))})
		if err != nil || tok.Scope != auth.ScopeEnrolled {
			t.Fatalf("establish the second device: %v (scope %d)", err, tok.Scope)
		}
		return tok.Token
	}
	path := "/v1/devices/" + second.ID.String() + "/sessions"
	// No list yet (a web-1 account): the young second row is unlisted, so another device is refused
	// and the row's own session is not.
	secondToken := establish()
	if rec := cborCall(t, h, http.MethodDelete, path, firstToken, nil); rec.Code != http.StatusConflict {
		t.Fatalf("another device's sessions DELETE of a new unlisted row = %d %x, want 409", rec.Code, rec.Body.Bytes())
	}
	if rec := cborCall(t, h, http.MethodDelete, path, secondToken, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("a device's sessions DELETE of its own new row = %d %x, want 204", rec.Code, rec.Body.Bytes())
	}
	// Listed, the same young row's sessions are removable by another device.
	listerOf(t, deps).list(user.ID, first, second)
	secondToken = establish()
	deps.Clock.(*clock.Fake).Advance(time.Minute)
	if rec := cborCall(t, h, http.MethodDelete, path, firstToken, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("another device's sessions DELETE of a listed new row = %d %x, want 204", rec.Code, rec.Body.Bytes())
	}
	if rec := cborCall(t, h, http.MethodGet, "/v1/accounts/me", secondToken, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the listed row's token after the DELETE = %d, want 401", rec.Code)
	}
}

// REGISTRATION-DEVICES-03 (c): the three /v1/devices routes spend the device session's buckets,
// GET the read bucket and POST and DELETE the write bucket, as the other web-2a routes do.
func TestTheDeviceRoutesSpendTheDeviceBuckets(t *testing.T) {
	h, deps := newTestAPIWithConfig(t, func(c *config.Config) {
		c.Limits.Rate.WriteBurst, c.Limits.Rate.ReadBurst = 1, 1
	})
	_, first, token := seedAPISession(t, deps)
	if rec := cborCall(t, h, http.MethodGet, "/v1/devices", token, nil); rec.Code != http.StatusOK {
		t.Fatalf("the first GET = %d, want 200", rec.Code)
	}
	if rec := cborCall(t, h, http.MethodGet, "/v1/devices", token, nil); rec.Code != http.StatusTooManyRequests || refusalCode(t, rec) != "E_RATE_LIMITED" {
		t.Fatalf("a second GET = %d %q, want 429 E_RATE_LIMITED", rec.Code, refusalCode(t, rec))
	}
	if _, rec := postFreshDevice(t, h, deps, token); rec.Code != http.StatusOK {
		t.Fatalf("the first POST = %d %x, want 200", rec.Code, rec.Body.Bytes())
	}
	if _, rec := postFreshDevice(t, h, deps, token); rec.Code != http.StatusTooManyRequests || refusalCode(t, rec) != "E_RATE_LIMITED" {
		t.Fatalf("a second POST = %d %q, want 429 E_RATE_LIMITED", rec.Code, refusalCode(t, rec))
	}
	if rec := cborCall(t, h, http.MethodDelete, "/v1/devices/"+first.ID.String(), token, nil); rec.Code != http.StatusTooManyRequests || refusalCode(t, rec) != "E_RATE_LIMITED" {
		t.Fatalf("a DELETE past the write burst = %d %q, want 429 E_RATE_LIMITED", rec.Code, refusalCode(t, rec))
	}
	wantAPIDeviceLive(t, deps, "the device a metered DELETE named", first.ID, true)
	// NEW-1: the sessions DELETE spends the same write bucket.
	if rec := cborCall(t, h, http.MethodDelete, "/v1/devices/"+first.ID.String()+"/sessions", token, nil); rec.Code != http.StatusTooManyRequests || refusalCode(t, rec) != "E_RATE_LIMITED" {
		t.Fatalf("a sessions DELETE past the write burst = %d %q, want 429 E_RATE_LIMITED", rec.Code, refusalCode(t, rec))
	}
	if rec := cborCall(t, h, http.MethodGet, "/v1/accounts/me", token, nil); rec.Code != http.StatusOK {
		t.Fatalf("the session a metered sessions DELETE named = %d, want 200", rec.Code)
	}
}

// signedListPut is version `version` of user's list chained to prevBlob (nil: 32 zero bytes),
// signed by ssk as a client signs it; it returns the PUT body and the blob.
func signedListPut(t *testing.T, ssk ed25519.PrivateKey, user id.ID, version uint64, prevBlob []byte, entries []apiListEntry) ([]any, []byte) {
	t.Helper()
	prev := make([]byte, 32)
	if prevBlob != nil {
		sum := sha256.Sum256(prevBlob)
		prev = sum[:]
	}
	unsigned, err := cborx.Marshal(apiListUnsigned{V: 1, UserID: user[:], Version: version, PrevHash: prev, Entries: entries})
	if err != nil {
		t.Fatalf("encode the unsigned list: %v", err)
	}
	sig := ed25519.Sign(ssk, append([]byte("dilla devices v1"), unsigned...))
	blob, err := cborx.Marshal(apiListSigned{V: 1, UserID: user[:], Version: version, PrevHash: prev, Entries: entries, SigSSK: sig})
	if err != nil {
		t.Fatalf("encode the signed list: %v", err)
	}
	return []any{version, blob, sig, prev}, blob
}

// listedUser is a user whose users.ssk_pub is SSK's public half, with enrolled devices.
type listedUser struct {
	User   store.UserRow
	SSK    ed25519.PrivateKey
	Devs   []store.DeviceRow
	Tokens []string
}

func entryOf(u listedUser, i int, revokedAt *uint64) apiListEntry {
	return apiListEntry{DeviceID: u.Devs[i].ID[:], DSKPub: u.Devs[i].DSKPub, Tier: uint64(u.Devs[i].Tier), AddedAt: 1, RevokedAt: revokedAt}
}

// seedListedUser writes the user and n native devices and establishes each through the real
// session path (the harness lister answers no list, so each is enrolled).
func seedListedUser(t *testing.T, d api.Deps, seed byte, n int) listedUser {
	t.Helper()
	ctx := context.Background()
	ssk := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, 32))
	now := d.Clock.Now().Unix()
	u := store.UserRow{ID: id.New(), Username: "lister" + id.New().String()[:8], Display: "Lister",
		UMKPub: make([]byte, 32), SSKPub: ssk.Public().(ed25519.PublicKey), SigUMKSSK: make([]byte, 64), Created: now}
	if err := d.Repo.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	out := listedUser{User: u, SSK: ssk}
	for i := 0; i < n; i++ {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("GenerateKey: %v", err)
		}
		dev := store.DeviceRow{ID: id.New(), UserID: u.ID, DSKPub: pub, CredentialBlob: []byte{1}, LastSeen: now, Created: now - 7200}
		if err := d.Repo.CreateDevice(ctx, dev); err != nil {
			t.Fatalf("CreateDevice: %v", err)
		}
		nonce, _, err := d.Sessions.Challenge(ctx, dev.ID)
		if err != nil {
			t.Fatalf("Challenge: %v", err)
		}
		pre := auth.SessionPreimage(d.Sessions.InstanceID(), dev.ID, nonce, auth.PurposeSession)
		tok, err := d.Sessions.Establish(ctx, auth.EstablishRequest{DeviceID: dev.ID, Nonce: nonce,
			Purpose: auth.PurposeSession, Sig: ed25519.Sign(priv, pre)})
		if err != nil {
			t.Fatalf("Establish: %v", err)
		}
		out.Devs = append(out.Devs, dev)
		out.Tokens = append(out.Tokens, tok.Token)
	}
	return out
}

// listAPI is the api harness with the real device-list verifier — the committed wasi core's
// device_list_entries — and an optional AfterDeviceList hook.
func listAPI(t *testing.T, hook func(ctx context.Context, user id.ID, revoked []id.ID)) (http.Handler, api.Deps) {
	t.Helper()
	wasm, err := os.ReadFile(filepath.Join("..", "mlswasi", "testdata", "dilla_core_wasi.wasm"))
	if err != nil {
		t.Fatalf("read the wasi core: %v", err)
	}
	rt, err := mlswasi.New(context.Background(), wasm, mlswasi.Options{PoolSize: 1, CacheDir: apiWasmCacheDir(t)})
	if err != nil {
		t.Fatalf("mlswasi.New: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	return newTestAPIWired(t, func(d *api.Deps) {
		d.DeviceLists = ds.NewDeviceLists(d.Repo, rt)
		d.AfterDeviceList = hook
	})
}

// wantListRefusal requires status and the CBOR error [code, detail, …].
func wantListRefusal(t *testing.T, rec *httptest.ResponseRecorder, status int, code, detail string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status %d (%x), want %d %s %q", rec.Code, rec.Body.Bytes(), status, code, detail)
	}
	var body []any
	if err := cborx.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body) < 2 {
		t.Fatalf("error body %x (err %v)", rec.Body.Bytes(), err)
	}
	if body[0] != code || body[1] != detail {
		t.Fatalf("refusal %v %q, want %s %q", body[0], body[1], code, detail)
	}
}

// L-HTTP-55. Attacker statement: only the user's own sessions reach the route; the verification
// keeps a stolen session (no SSK_priv) from storing an unverifiable list that would lock every
// device of the user out (DEV-W2-24); the 409 is a race between the user's own devices, cured by
// a refetch.
func TestAPublishedDeviceListIsVerified(t *testing.T) {
	h, deps := listAPI(t, nil)
	lu := seedListedUser(t, deps, 0x5a, 1)
	path := "/v1/users/" + lu.User.ID.String() + "/device-list"
	put := func(body []any) *httptest.ResponseRecorder {
		return cborCall(t, h, http.MethodPut, path, lu.Tokens[0], body)
	}
	entries := []apiListEntry{entryOf(lu, 0, nil)}

	genesis2, _ := signedListPut(t, lu.SSK, lu.User.ID, 2, nil, entries)
	wantListRefusal(t, put(genesis2), http.StatusConflict, "E_INVALID_REQUEST", "device-list version 2 is not the next version")
	// The genesis pin (security review F8, m3b): version 1 chains from 32 zero bytes. A signed v1
	// with any other prev_hash is refused and nothing is stored; every client's accept would refuse
	// it, so storing it would lock the SSK holder's own devices out.
	orphan, _ := signedListPut(t, lu.SSK, lu.User.ID, 1, []byte("no parent"), entries)
	wantListRefusal(t, put(orphan), http.StatusConflict, "E_INVALID_REQUEST", "device-list version 1 is not the next version")
	if _, err := deps.Repo.GetDeviceList(context.Background(), lu.User.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a v1 with a non-zero prev_hash was stored (GetDeviceList err %v)", err)
	}
	body1, blob1 := signedListPut(t, lu.SSK, lu.User.ID, 1, nil, entries)
	if rec := put(body1); rec.Code != http.StatusNoContent {
		t.Fatalf("v1 = %d %x, want 204", rec.Code, rec.Body.Bytes())
	}

	body2, blob2 := signedListPut(t, lu.SSK, lu.User.ID, 2, blob1, entries)
	for i, v := range map[int]any{0: uint64(3), 2: bytes.Repeat([]byte{9}, 64), 3: bytes.Repeat([]byte{9}, 32)} {
		lying := slices.Clone(body2)
		lying[i] = v
		wantListRefusal(t, put(lying), http.StatusBadRequest, "E_INVALID_REQUEST", "device list elements disagree")
	}
	wantListRefusal(t, put([]any{uint64(2), []byte{0x80}, body2[2], body2[3]}), http.StatusBadRequest,
		"E_INVALID_REQUEST", "device list elements disagree")

	forged, _ := signedListPut(t, ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x77}, 32)), lu.User.ID, 2, blob1, entries)
	wantListRefusal(t, put(forged), http.StatusBadRequest, "E_INVALID_REQUEST", "device list does not verify")
	elsewhere, _ := signedListPut(t, lu.SSK, id.New(), 2, blob1, entries)
	wantListRefusal(t, put(elsewhere), http.StatusBadRequest, "E_INVALID_REQUEST", "device list does not verify")

	skip, _ := signedListPut(t, lu.SSK, lu.User.ID, 3, blob1, entries)
	wantListRefusal(t, put(skip), http.StatusConflict, "E_INVALID_REQUEST", "device-list version 3 is not the next version")
	// A list chaining from the wrong parent is refused.
	wrongParent, _ := signedListPut(t, lu.SSK, lu.User.ID, 2, []byte("not the newest list"), entries)
	wantListRefusal(t, put(wrongParent), http.StatusConflict, "E_INVALID_REQUEST", "device-list version 2 is not the next version")

	newest, err := deps.Repo.GetDeviceList(context.Background(), lu.User.ID)
	if err != nil || newest.Version != 1 || !bytes.Equal(newest.Blob, blob1) {
		t.Fatalf("after the refusals the newest is v%d (err %v), want v1 unchanged", newest.Version, err)
	}
	if rec := put(body2); rec.Code != http.StatusNoContent {
		t.Fatalf("v2 = %d %x, want 204", rec.Code, rec.Body.Bytes())
	}
	wantListRefusal(t, put(body2), http.StatusConflict, "E_INVALID_REQUEST", "device-list version 2 is not the next version")
	got := cborCall(t, h, http.MethodGet, path, lu.Tokens[0], nil)
	var row []any
	if err := cborx.Unmarshal(got.Body.Bytes(), &row); err != nil || row[0] != uint64(2) || !bytes.Equal(row[1].([]byte), blob2) {
		t.Fatalf("GET after v2 = %v (err %v), want v2", row, err)
	}
}

func TestPublishedDeviceListAppendsOneEventPerCurrentCommunity(t *testing.T) {
	h, deps := listAPI(t, nil)
	lu := seedListedUser(t, deps, 0x5a, 1)
	ctx := context.Background()
	owner, _, _ := seedAPIDevice(t, deps)
	communities := make([]id.ID, 257)
	if err := deps.Repo.Tx(ctx, func(tx store.Repository) error {
		for i := range communities {
			community := id.New()
			communities[i] = community
			if err := tx.CreateCommunity(ctx, store.CommunityRow{ID: community, Owner: owner.ID,
				Name: "device-list-event-" + community.String()[:8], PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: deps.Clock.Now().Unix()}); err != nil {
				return err
			}
			if err := tx.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: community, UserID: lu.User.ID, Joined: deps.Clock.Now().Unix() + int64(i)}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	body, _ := signedListPut(t, lu.SSK, lu.User.ID, 1, nil, []apiListEntry{entryOf(lu, 0, nil)})
	path := "/v1/users/" + lu.User.ID.String() + "/device-list"
	if rec := cborCall(t, h, http.MethodPut, path, lu.Tokens[0], body); rec.Code != http.StatusNoContent {
		t.Fatalf("publish v1 = %d %x, want 204", rec.Code, rec.Body.Bytes())
	}
	for _, community := range communities {
		page, err := deps.Repo.ListBotStructuralEvents(ctx, community, 0, 10)
		if err != nil || len(page.Events) != 1 {
			t.Fatalf("community %s event page=%+v err=%v, want one device-list event", community, page, err)
		}
		event := page.Events[0]
		if event.ID.IsZero() || event.Sequence != 1 || event.Kind != protocol.EventDeviceList || len(event.Data) != 2 || event.Data[0] != lu.User.ID || event.Data[1] != uint64(1) {
			t.Fatalf("community %s event=%+v, want device-list [user, version]", community, event)
		}
	}
	if rec := cborCall(t, h, http.MethodPut, path, lu.Tokens[0], body); rec.Code != http.StatusConflict {
		t.Fatalf("replay v1 = %d, want conflict", rec.Code)
	}
	for _, community := range communities {
		page, err := deps.Repo.ListBotStructuralEvents(ctx, community, 0, 10)
		if err != nil || len(page.Events) != 1 {
			t.Fatalf("replayed list changed event stream for %s: page=%+v err=%v", community, page, err)
		}
	}
}

// protocol/02 item 6, Q13. Attacker statement: the action touches only devices of this user that
// the user's SSK signed as revoked; another user's device named in a list is ignored; the
// publisher may revoke itself.
func TestARevokingListRevokesTheDeviceItsSessionsAndItsSockets(t *testing.T) {
	var closed []id.ID
	var hooked [][]id.ID
	h, deps := listAPI(t, func(_ context.Context, _ id.ID, revoked []id.ID) { hooked = append(hooked, revoked) })
	deps.Sessions.OnRevoke = func(dev id.ID) { closed = append(closed, dev) }
	ctx := context.Background()
	lu := seedListedUser(t, deps, 0x5a, 2)
	stranger := seedListedUser(t, deps, 0x6b, 1)
	path := "/v1/users/" + lu.User.ID.String() + "/device-list"
	put := func(token string, body []any) {
		t.Helper()
		if rec := cborCall(t, h, http.MethodPut, path, token, body); rec.Code != http.StatusNoContent {
			t.Fatalf("PUT = %d %x, want 204", rec.Code, rec.Body.Bytes())
		}
	}
	me := func(token string) int { return doAuth(h, http.MethodGet, "/v1/accounts/me", token).Code }

	body1, blob1 := signedListPut(t, lu.SSK, lu.User.ID, 1, nil, []apiListEntry{entryOf(lu, 0, nil), entryOf(lu, 1, nil)})
	put(lu.Tokens[0], body1)
	if len(closed) != 0 || len(hooked) != 1 || len(hooked[0]) != 0 {
		t.Fatalf("a list that revokes nothing: closed %v, hook %v", closed, hooked)
	}

	now := uint64(deps.Clock.Now().Unix())
	strangerEntry := apiListEntry{DeviceID: stranger.Devs[0].ID[:], DSKPub: stranger.Devs[0].DSKPub, AddedAt: 1, RevokedAt: &now}
	revoking := []apiListEntry{entryOf(lu, 0, nil), entryOf(lu, 1, &now), strangerEntry}
	body2, blob2 := signedListPut(t, lu.SSK, lu.User.ID, 2, blob1, revoking)
	put(lu.Tokens[0], body2)
	row, err := deps.Repo.GetDevice(ctx, lu.Devs[1].ID)
	if err != nil || row.RevokedAt == nil || *row.RevokedAt != int64(now) {
		t.Fatalf("the revoked device's row = %+v (err %v), want revoked_at %d", row, err, now)
	}
	if _, err := deps.Sessions.Resolve(ctx, lu.Tokens[1]); err == nil {
		t.Fatal("a revoked device's token still resolves")
	}
	if got := me(lu.Tokens[1]); got != http.StatusUnauthorized {
		t.Fatalf("the revoked device's token answered %d, want 401", got)
	}
	if len(closed) != 1 || closed[0] != lu.Devs[1].ID {
		t.Fatalf("OnRevoke saw %v, want exactly [%s]", closed, lu.Devs[1].ID)
	}
	if len(hooked) != 2 || len(hooked[1]) != 1 || hooked[1][0] != lu.Devs[1].ID {
		t.Fatalf("AfterDeviceList saw %v, want the second run to carry [%s]", hooked, lu.Devs[1].ID)
	}
	if srow, err := deps.Repo.GetDevice(ctx, stranger.Devs[0].ID); err != nil || srow.RevokedAt != nil {
		t.Fatalf("another user's device was revoked by this user's list: %+v (err %v)", srow, err)
	}
	if got := me(stranger.Tokens[0]); got != http.StatusOK {
		t.Fatalf("another user's session answered %d, want 200", got)
	}
	if got := me(lu.Tokens[0]); got != http.StatusOK {
		t.Fatalf("the publisher's session answered %d, want 200", got)
	}

	body3, blob3 := signedListPut(t, lu.SSK, lu.User.ID, 3, blob2, revoking)
	put(lu.Tokens[0], body3)
	if len(closed) != 1 || len(hooked[2]) != 0 {
		t.Fatalf("a list that keeps a revocation acted again: closed %v, hook %v", closed, hooked[2])
	}

	body4, _ := signedListPut(t, lu.SSK, lu.User.ID, 4, blob3, []apiListEntry{entryOf(lu, 0, &now), entryOf(lu, 1, &now)})
	put(lu.Tokens[0], body4)
	if got := me(lu.Tokens[0]); got != http.StatusUnauthorized {
		t.Fatalf("the self-revoking publisher's token answered %d, want 401", got)
	}
	if len(closed) != 2 || closed[1] != lu.Devs[0].ID {
		t.Fatalf("OnRevoke saw %v, want the publisher second", closed)
	}
}

// A Deps without a verifier refuses every publish rather than store an unverified list.
func TestAnUnwiredDeviceListVerifierFailsClosed(t *testing.T) {
	h, deps := newTestAPI(t)
	lu := seedListedUser(t, deps, 0x5a, 1)
	body1, _ := signedListPut(t, lu.SSK, lu.User.ID, 1, nil, []apiListEntry{entryOf(lu, 0, nil)})
	wantListRefusal(t, cborCall(t, h, http.MethodPut, "/v1/users/"+lu.User.ID.String()+"/device-list", lu.Tokens[0], body1),
		http.StatusInternalServerError, "E_INTERNAL", "")
	if _, err := deps.Repo.GetDeviceList(context.Background(), lu.User.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an unverified list was stored: %v", err)
	}
}

// failingRemoveDS refuses the Remove of one group, as the guest refuses an instance Remove in a
// pairing group, and records every other call.
type failingRemoveDS struct {
	*recordingDS
	fail id.ID
}

func (f *failingRemoveDS) ProposeRemoveDevice(ctx context.Context, g, dev, a id.ID) error {
	if g == f.fail {
		return errors.New("the guest refused the instance Remove")
	}
	return f.recordingDS.ProposeRemoveDevice(ctx, g, dev, a)
}

// The revoked device's live leaves each get an instance Remove; a leaf it already left gets none;
// one refusal does not stop the rest and is reported with its group and device.
func TestRemoveRevokedDevicesProposesARemoveInEveryGroupTheDeviceHolds(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	_, tok := e.NewUser("revoked")
	dev := deviceOf(t, e, tok)
	cid := id.New()
	g1 := seedGroupOfKind(t, e, id.New(), cid, 0)
	g2 := seedGroupOfKind(t, e, id.New(), cid, 0)
	left := seedGroupOfKind(t, e, id.New(), cid, 0)
	seedLeaf(t, e, g1, dev, 0, nil)
	seedLeaf(t, e, g2, dev, 0, nil)
	gone := uint64(3)
	seedLeaf(t, e, left, dev, 0, &gone)

	if err := api.RemoveRevokedDevices(ctx, e.Repo, e.DS, []id.ID{dev}); err != nil {
		t.Fatalf("RemoveRevokedDevices: %v", err)
	}
	got := map[id.ID]id.ID{}
	for _, r := range e.DS.deviceRemoves() {
		got[r.Group] = r.Device
	}
	if len(got) != 2 || got[g1] != dev || got[g2] != dev {
		t.Fatalf("Removes = %v, want one per live leaf (%s, %s) and none for %s", got, g1, g2, left)
	}

	failing := &failingRemoveDS{recordingDS: &recordingDS{}, fail: g1}
	err := api.RemoveRevokedDevices(ctx, e.Repo, failing, []id.ID{dev})
	if err == nil || !strings.Contains(err.Error(), g1.String()) || !strings.Contains(err.Error(), dev.String()) {
		t.Fatalf("a refused Remove: err %v, want one naming group %s and device %s", err, g1, dev)
	}
	var re *api.RevokedRemoveError
	if !errors.As(err, &re) || re.Group != g1 || re.Device != dev || re.Err == nil {
		t.Fatalf("a refused Remove: %v, want an *api.RevokedRemoveError for group %s and device %s", err, g1, dev)
	}
	if rs := failing.deviceRemoves(); len(rs) != 1 || rs[0].Group != g2 {
		t.Fatalf("after the refusal the other group got %v, want its Remove", rs)
	}
	if err := api.RemoveRevokedDevices(ctx, e.Repo, nil, []id.ID{dev}); err == nil {
		t.Fatal("no delivery service was a silent no-op")
	}
}

// L-HTTP-56, DEV-W2-32. Attacker statement: the history refuses nothing new and exposes only what
// the user published; a pending session reads only its own.
func TestTheDeviceListHistoryIsServedAfterAVersion(t *testing.T) {
	h, deps := listAPI(t, nil)
	ctx := context.Background()
	lu := seedListedUser(t, deps, 0x5a, 1)
	stranger := seedListedUser(t, deps, 0x6b, 1)
	path := "/v1/users/" + lu.User.ID.String() + "/device-list"
	strangerPath := "/v1/users/" + stranger.User.ID.String() + "/device-list"
	entries := []apiListEntry{entryOf(lu, 0, nil)}
	var bodies [][]any
	var blobs [][]byte
	var prev []byte
	for v := uint64(1); v <= 3; v++ {
		body, blob := signedListPut(t, lu.SSK, lu.User.ID, v, prev, entries)
		if rec := cborCall(t, h, http.MethodPut, path, lu.Tokens[0], body); rec.Code != http.StatusNoContent {
			t.Fatalf("PUT v%d = %d", v, rec.Code)
		}
		bodies, blobs, prev = append(bodies, body), append(blobs, blob), blob
	}
	history := func(token, p, after string) []any {
		t.Helper()
		rec := cborCall(t, h, http.MethodGet, p+"?after="+after, token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s?after=%s = %d %x, want 200", p, after, rec.Code, rec.Body.Bytes())
		}
		var rows []any
		if err := cborx.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
			t.Fatalf("decode history: %v", err)
		}
		return rows
	}

	rows := history(lu.Tokens[0], path, "0")
	if len(rows) != 3 {
		t.Fatalf("history after 0 has %d rows, want 3", len(rows))
	}
	for i, r := range rows {
		row, _ := r.([]any)
		if len(row) != 4 || row[0] != uint64(i+1) || !bytes.Equal(row[1].([]byte), blobs[i]) ||
			!bytes.Equal(row[2].([]byte), bodies[i][2].([]byte)) || !bytes.Equal(row[3].([]byte), bodies[i][3].([]byte)) {
			t.Fatalf("history row %d = %v, want version %d as published", i, row, i+1)
		}
	}
	if rows := history(lu.Tokens[0], path, "1"); len(rows) != 2 || rows[0].([]any)[0] != uint64(2) || rows[1].([]any)[0] != uint64(3) {
		t.Fatalf("history after 1 = %v, want versions 2 and 3", rows)
	}
	if rec := cborCall(t, h, http.MethodGet, path+"?after=3", lu.Tokens[0], nil); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), []byte{0x80}) {
		t.Fatalf("history after the newest = %d %x, want 200 and the empty array 80", rec.Code, rec.Body.Bytes())
	}
	var newest []any
	if err := cborx.Unmarshal(cborCall(t, h, http.MethodGet, path, lu.Tokens[0], nil).Body.Bytes(), &newest); err != nil || len(newest) != 4 || newest[0] != uint64(3) {
		t.Fatalf("GET without after = %v (err %v), want the newest row [3, …]", newest, err)
	}
	for _, bad := range []string{"abc", "-1", "", "1.5", "9223372036854775808", "18446744073709551616"} {
		if rec := cborCall(t, h, http.MethodGet, path+"?after="+bad, lu.Tokens[0], nil); rec.Code != http.StatusBadRequest || refusalCode(t, rec) != "E_INVALID_REQUEST" {
			t.Errorf("after=%q = %d, want 400 E_INVALID_REQUEST", bad, rec.Code)
		}
	}
	if rows := history(lu.Tokens[0], strangerPath, "0"); len(rows) != 0 {
		t.Fatalf("a user with no list: history %v, want []", rows)
	}
	if rec := cborCall(t, h, http.MethodGet, strangerPath, lu.Tokens[0], nil); rec.Code != http.StatusNotFound {
		t.Fatalf("a user with no list: GET without after = %d, want 404", rec.Code)
	}

	for v := uint64(4); v <= 70; v++ {
		if err := deps.Repo.PutDeviceList(ctx, store.DeviceListRow{UserID: lu.User.ID, Version: v, Blob: []byte{byte(v)},
			SSKSignature: make([]byte, 64), PrevHash: make([]byte, 32), Created: 1}); err != nil {
			t.Fatalf("PutDeviceList v%d: %v", v, err)
		}
	}
	rows = history(lu.Tokens[0], path, "0")
	if len(rows) != 64 || rows[0].([]any)[0] != uint64(1) || rows[63].([]any)[0] != uint64(64) {
		t.Fatalf("history after 0 of 70 versions: %d rows, want 64 from 1 to 64", len(rows))
	}
	if rows := history(lu.Tokens[0], path, "64"); len(rows) != 6 || rows[5].([]any)[0] != uint64(70) {
		t.Fatalf("history after 64: %d rows, want 6 ending at 70", len(rows))
	}

	listerOf(t, deps).list(lu.User.ID, lu.Devs[0])
	_, pending := pendingSession(t, h, deps, lu.User.ID)
	if rows := history(pending, path, "0"); len(rows) != 64 {
		t.Fatalf("a pending session's own history: %d rows, want 64", len(rows))
	}
	if rec := cborCall(t, h, http.MethodGet, strangerPath+"?after=0", pending, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("a pending session reading another user's history = %d, want 403", rec.Code)
	}
}

// A session without the SSK may clear an unlisted registration row, but the owner's signed list
// is the authority for revoking a listed device. Only this user's own sessions reach the route.
func TestKeylessDeleteRefusesAListedDevice(t *testing.T) {
	h, deps := listAPI(t, nil)
	lu := seedListedUser(t, deps, 0x5a, 2)
	body, _ := signedListPut(t, lu.SSK, lu.User.ID, 1, nil, []apiListEntry{entryOf(lu, 0, nil)})
	path := "/v1/users/" + lu.User.ID.String() + "/device-list"
	if rec := cborCall(t, h, http.MethodPut, path, lu.Tokens[0], body); rec.Code != http.StatusNoContent {
		t.Fatalf("publish = %d %x, want 204", rec.Code, rec.Body.Bytes())
	}
	listerOf(t, deps).list(lu.User.ID, lu.Devs[0])
	listed := "/v1/devices/" + lu.Devs[0].ID.String()
	wantListRefusal(t, cborCall(t, h, http.MethodDelete, listed, lu.Tokens[0], nil),
		http.StatusConflict, "E_INVALID_REQUEST", "a listed device is revoked by a signed device list")
	if row, err := deps.Repo.GetDevice(t.Context(), lu.Devs[0].ID); err != nil || row.RevokedAt != nil {
		t.Fatalf("listed device after refused DELETE = %+v, %v", row, err)
	}
	unlisted := "/v1/devices/" + lu.Devs[1].ID.String()
	if rec := cborCall(t, h, http.MethodDelete, unlisted, lu.Tokens[0], nil); rec.Code != http.StatusNoContent {
		t.Fatalf("unlisted DELETE = %d %x, want 204", rec.Code, rec.Body.Bytes())
	}
	// Another user's device (security review F8, m24): 404, as for an unknown id, and nothing is
	// revoked. Device ids travel in every device list an enrolled session may read, so without
	// this an enrolled session revokes any user's unlisted device.
	stranger := seedListedUser(t, deps, 0x6b, 1)
	theirs := "/v1/devices/" + stranger.Devs[0].ID.String()
	wantListRefusal(t, cborCall(t, h, http.MethodDelete, theirs, lu.Tokens[0], nil),
		http.StatusNotFound, "E_NOT_FOUND", "not found")
	if row, err := deps.Repo.GetDevice(t.Context(), stranger.Devs[0].ID); err != nil || row.RevokedAt != nil {
		t.Fatalf("another user's device after a cross-user DELETE = %+v, %v; want live", row, err)
	}
	if got := doAuth(h, http.MethodGet, "/v1/accounts/me", stranger.Tokens[0]).Code; got != http.StatusOK {
		t.Fatalf("another user's session after a cross-user DELETE answered %d, want 200", got)
	}
}

// Task 8b security review M1 (fail closed): POST /v1/devices verifies the credential with the WASI core and binds it to
// the account's kind, the session's user (or the unbound zero id) and this device. Anything else is refused.
func TestPostDevicesBindsTheCredentialToTheAccountAndDevice(t *testing.T) {
	h, deps := newTestAPI(t)
	user, _, token := seedAPISession(t, deps)
	post := func(credential func(device id.ID, pub []byte) []byte) int {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		device := id.New()
		body := devicePostBody(t, deps, device, pub, priv)
		body[4] = credential(device, pub)
		return cborCall(t, h, http.MethodPost, "/v1/devices", token, body).Code
	}
	cases := []struct {
		name       string
		credential func(device id.ID, pub []byte) []byte
		want       int
	}{
		{"malformed", func(id.ID, []byte) []byte { return []byte{1} }, http.StatusBadRequest},
		{"ten fields but not a credential", func(device id.ID, _ []byte) []byte {
			blob, _ := cborx.Marshal([]any{uint64(1), make([]byte, 32), user.ID[:], device[:], uint64(0), uint64(1), uint64(1), make([]byte, 32), make([]byte, 64), make([]byte, 64)})
			return blob
		}, http.StatusBadRequest},
		{"bot kind on a human account", func(device id.ID, pub []byte) []byte { return testDeviceCredential(t, user.ID, device, pub, 1) }, http.StatusBadRequest},
		{"another user", func(device id.ID, pub []byte) []byte { return testDeviceCredential(t, id.New(), device, pub, 0) }, http.StatusBadRequest},
		{"another device", func(_ id.ID, pub []byte) []byte { return testDeviceCredential(t, user.ID, id.New(), pub, 0) }, http.StatusBadRequest},
		{"another dsk", func(device id.ID, _ []byte) []byte {
			other, _, _ := ed25519.GenerateKey(rand.Reader)
			return testDeviceCredential(t, user.ID, device, other, 0)
		}, http.StatusBadRequest},
		// Task S1: the account's own UMK, SSK and UMK-over-SSK signature, as signup binds them.
		{"another identity (UMK and SSK)", func(device id.ID, pub []byte) []byte {
			_, umk, _ := ed25519.GenerateKey(rand.Reader)
			_, ssk, _ := ed25519.GenerateKey(rand.Reader)
			return testDeviceCredentialUnder(t, umk, ssk, user.ID, device, pub, 0)
		}, http.StatusBadRequest},
		{"the account's UMK over another SSK", func(device id.ID, pub []byte) []byte {
			_, ssk, _ := ed25519.GenerateKey(rand.Reader)
			return testDeviceCredentialUnder(t, testUMK, ssk, user.ID, device, pub, 0)
		}, http.StatusBadRequest},
		{"another UMK over the account's SSK", func(device id.ID, pub []byte) []byte {
			_, umk, _ := ed25519.GenerateKey(rand.Reader)
			return testDeviceCredentialUnder(t, umk, testSSK, user.ID, device, pub, 0)
		}, http.StatusBadRequest},
		{"valid human, this user", func(device id.ID, pub []byte) []byte { return testDeviceCredential(t, user.ID, device, pub, 0) }, http.StatusOK},
		{"valid human, unbound user id", func(device id.ID, pub []byte) []byte { return testDeviceCredential(t, id.ID{}, device, pub, 0) }, http.StatusOK},
	}
	for _, tc := range cases {
		if got := post(tc.credential); got != tc.want {
			t.Fatalf("%s: status %d, want %d", tc.name, got, tc.want)
		}
	}
}
