package auth_test

// registration_lockout_test.go pins the replacing rule of the server-block security review's F1:
// a registration is never refused while an unlisted live row of the user exists. The cap and the
// hourly rate decide WHICH row a registration replaces (the oldest unlisted, whatever its age),
// never WHETHER; only a cap of entirely listed rows refuses. At registration the instance cannot
// tell the owner from a holder of the password alone (what tells them apart is the recovery key,
// used after registration), so any per-user refusal a password holder can keep saturated is a
// lockout of the owner's recovery.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// seedListedRows writes n more live devices for user, created at created, and returns their keys
// with the ids, for the caller to put in the lister.
func seedListedRows(t *testing.T, repo store.Repository, user id.ID, n int, created int64) []listedRow {
	t.Helper()
	out := make([]listedRow, 0, n)
	for i := 0; i < n; i++ {
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("GenerateKey: %v", err)
		}
		row := store.DeviceRow{ID: id.New(), UserID: user, DSKPub: pub, CredentialBlob: []byte{1}, Created: created, LastSeen: created}
		if err := repo.CreateDevice(context.Background(), row); err != nil {
			t.Fatalf("CreateDevice: %v", err)
		}
		out = append(out, listedRow{id: row.ID, pub: pub})
	}
	return out
}

type listedRow struct {
	id  id.ID
	pub []byte
}

func wantLive(t *testing.T, repo store.Repository, what string, device id.ID, live bool) {
	t.Helper()
	row, err := repo.GetDevice(context.Background(), device)
	if err != nil {
		t.Fatalf("%s: GetDevice: %v", what, err)
	}
	if (row.RevokedAt == nil) != live {
		t.Fatalf("%s: revoked_at %v, want live %t", what, row.RevokedAt, live)
	}
}

// The owner registers while three fresh unlisted rows of a password holder exist: the registration
// is admitted and replaces the oldest of them; the other two stay.
func TestTheOwnerRegistersPastThreeFreshAttackerRows(t *testing.T) {
	s, repo, clk := newSessions(t)
	a := newFakeAssertions()
	s.Assertions = a
	user, owner, priv := seedDevice(t, repo)
	listerOf(t, s).list(user, entry(owner, pubOf(priv)))
	var attacker []id.ID
	for i := 0; i < 3; i++ {
		reg, key := newRegistration(t)
		if _, err := registerWith(t, s, reg, key, a.issue(user)); err != nil {
			t.Fatalf("attacker row %d: %v", i+1, err)
		}
		attacker = append(attacker, reg.DeviceID)
		clk.Advance(time.Second)
	}
	recovering, recoveringKey := newRegistration(t)
	tok, err := registerWith(t, s, recovering, recoveringKey, a.issue(user))
	if err != nil {
		t.Fatalf("the owner's registration past three fresh unlisted rows: %v, want it admitted", err)
	}
	if tok.Scope != auth.ScopePending {
		t.Fatalf("the owner's registration: scope %d, want pending", tok.Scope)
	}
	wantLive(t, repo, "the oldest attacker row", attacker[0], false)
	wantLive(t, repo, "the second attacker row", attacker[1], true)
	wantLive(t, repo, "the third attacker row", attacker[2], true)
	wantLive(t, repo, "the owner's new row", recovering.DeviceID, true)
}

// Eight listed live devices: the one refusal left. A registration is 403 "device cap reached" and
// writes nothing.
func TestEightListedDevicesRefuseARegistration(t *testing.T) {
	s, repo, clk := newSessions(t)
	a := newFakeAssertions()
	s.Assertions = a
	user, owner, priv := seedDevice(t, repo)
	keys := []auth.ListedDevice{entry(owner, pubOf(priv))}
	for _, r := range seedListedRows(t, repo, user, 7, clk.Now().Unix()-60) {
		keys = append(keys, entry(r.id, r.pub))
	}
	listerOf(t, s).list(user, keys...)
	reg, key := newRegistration(t)
	_, err := registerWith(t, s, reg, key, a.issue(user))
	if e := refusal(t, err); e.Code != server.CodeForbidden || e.Status() != http.StatusForbidden || e.Detail != "device cap reached" {
		t.Fatalf("a registration at eight listed devices: %s %d %q, want 403 E_FORBIDDEN \"device cap reached\"", e.Code, e.Status(), e.Detail)
	}
	noDeviceRow(t, repo, reg.DeviceID)
}

// Probe A of the review: a holder of the password alone registers three unlisted rows at the top of
// every hour. Before the fix the owner's registration in hour 2 answered 429 (the shared hourly
// rate), every hour, for ever. The owner now registers, its list names the new row, and its next
// establish is enrolled.
func TestProbeAThreeAttackerRowsAnHourDoNotKeepTheOwnerOut(t *testing.T) {
	s, repo, clk := newSessions(t)
	a := newFakeAssertions()
	s.Assertions = a
	lister := listerOf(t, s)
	user, owner, priv := seedDevice(t, repo)
	lister.list(user, entry(owner, pubOf(priv)))
	start := clk.Now()
	for hour := 0; hour < 3; hour++ {
		clk.Advance(start.Add(time.Duration(hour) * time.Hour).Sub(clk.Now()))
		for i := 0; i < 3; i++ {
			reg, key := newRegistration(t)
			if _, err := registerWith(t, s, reg, key, a.issue(user)); err != nil {
				t.Fatalf("hour %d: attacker registration %d: %v", hour, i+1, err)
			}
			clk.Advance(time.Second)
		}
	}
	recovering, recoveringKey := newRegistration(t)
	if _, err := registerWith(t, s, recovering, recoveringKey, a.issue(user)); err != nil {
		t.Fatalf("hour 2: the owner's registration: %v, want it admitted", err)
	}
	// The owner's recovery key publishes the list that names the new device; its next establish is enrolled.
	lister.list(user, entry(owner, pubOf(priv)), entry(recovering.DeviceID, recovering.DSKPub))
	tok, err := establishOnce(t, s, recovering.DeviceID, recoveringKey, auth.PurposeSession)
	if err != nil || tok.Scope != auth.ScopeEnrolled {
		t.Fatalf("the owner's device once listed: scope %d, err %v; want enrolled", tok.Scope, err)
	}
}

// Probe B of the review: five listed devices leave three unlisted slots under the cap of eight; a
// holder of the password alone refreshes all three at the top of every hour, so every unlisted row
// is always younger than an hour. With the first round's guard (no eviction of a row younger than
// the rate window) the owner's registration answered 403 all hour, every hour. It is now admitted,
// replacing the oldest unlisted row, and the owner ends enrolled.
func TestProbeBRefreshedYoungSlotsDoNotKeepTheOwnerOut(t *testing.T) {
	s, repo, clk := newSessions(t)
	a := newFakeAssertions()
	s.Assertions = a
	lister := listerOf(t, s)
	user, owner, priv := seedDevice(t, repo)
	keys := []auth.ListedDevice{entry(owner, pubOf(priv))}
	for _, r := range seedListedRows(t, repo, user, 4, 1) {
		keys = append(keys, entry(r.id, r.pub))
	}
	lister.list(user, keys...)
	start := clk.Now()
	var latest []id.ID
	for hour := 0; hour < 3; hour++ {
		clk.Advance(start.Add(time.Duration(hour) * time.Hour).Sub(clk.Now()))
		latest = latest[:0]
		for i := 0; i < 3; i++ {
			reg, key := newRegistration(t)
			if _, err := registerWith(t, s, reg, key, a.issue(user)); err != nil {
				t.Fatalf("hour %d: attacker registration %d: %v", hour, i+1, err)
			}
			latest = append(latest, reg.DeviceID)
			clk.Advance(time.Second)
		}
	}
	live, err := repo.CountLiveDevicesByUser(context.Background(), user)
	if err != nil || live != 8 {
		t.Fatalf("before the owner: %d live rows (err %v), want the cap of 8", live, err)
	}
	recovering, recoveringKey := newRegistration(t)
	if _, err := registerWith(t, s, recovering, recoveringKey, a.issue(user)); err != nil {
		t.Fatalf("hour 2: the owner's registration at the cap: %v, want it admitted", err)
	}
	wantLive(t, repo, "the oldest refreshed attacker row", latest[0], false)
	lister.list(user, append(keys, entry(recovering.DeviceID, recovering.DSKPub))...)
	tok, err := establishOnce(t, s, recovering.DeviceID, recoveringKey, auth.PurposeSession)
	if err != nil || tok.Scope != auth.ScopeEnrolled {
		t.Fatalf("the owner's device once listed: scope %d, err %v; want enrolled", tok.Scope, err)
	}
}

// F2: listed is the (device_id, dsk_pub) pair. A list naming this device's key under another id
// does not list this device: its establish is pending, not enrolled.
func TestAListedKeyUnderAnotherIDDoesNotListTheDevice(t *testing.T) {
	s, repo, clk := newSessions(t)
	lister := listerOf(t, s)
	user, device, priv := seedDevice(t, repo)
	makeDeviceYoung(t, repo, device, clk.Now().Unix())
	lister.list(user, entry(id.New(), pubOf(priv)))
	tok, err := establishOnce(t, s, device, priv, auth.PurposeSession)
	if err != nil || tok.Scope != auth.ScopePending {
		t.Fatalf("a device whose key the list names under another id: scope %d, err %v; want pending", tok.Scope, err)
	}
	lister.list(user, entry(device, pubOf(priv)))
	tok, err = establishOnce(t, s, device, priv, auth.PurposeSession)
	if err != nil || tok.Scope != auth.ScopeEnrolled {
		t.Fatalf("a device the list names by id and key: scope %d, err %v; want enrolled", tok.Scope, err)
	}
}

// F2: one live row per key on the assertion path too. A registration under a key a live row of the
// user holds is 409 and writes nothing, even signed by that key.
func TestAssertionRegistrationRefusesAKeyALiveRowHolds(t *testing.T) {
	s, repo, _ := newSessions(t)
	a := newFakeAssertions()
	s.Assertions = a
	user, owner, priv := seedDevice(t, repo)
	listerOf(t, s).list(user, entry(owner, pubOf(priv)))
	reg, _ := newRegistration(t)
	reg.DSKPub = pubOf(priv)
	_, err := registerWith(t, s, reg, priv, a.issue(user))
	if e := refusal(t, err); e.Code != server.CodeInvalidRequest || e.Status() != http.StatusConflict ||
		e.Detail != "dsk_pub is already registered to a live device" {
		t.Fatalf("a registration under a live row's key: %s %d %q, want 409 E_INVALID_REQUEST", e.Code, e.Status(), e.Detail)
	}
	noDeviceRow(t, repo, reg.DeviceID)
}

// Security review F8 (m14): Resolve re-checks a pending session's device. A pending browser session
// kept alive past the device's 24 hours (each Resolve slides its 12-hour idle window) stops
// resolving when its unlisted row turns 24 hours old, and the row is revoked, without any
// establish or registration running the sweep.
func TestResolveExpiresAPendingSessionWhoseRowTurned24HoursOld(t *testing.T) {
	s, repo, clk := newSessions(t)
	a := newFakeAssertions()
	s.Assertions = a
	user, owner, priv := seedDevice(t, repo)
	listerOf(t, s).list(user, entry(owner, pubOf(priv)))
	reg, key := newRegistration(t)
	tok, err := registerWith(t, s, reg, key, a.issue(user))
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []time.Duration{11 * time.Hour, 11 * time.Hour} {
		clk.Advance(step)
		if _, err := s.Resolve(t.Context(), tok.Token); err != nil {
			t.Fatalf("the pending session inside the row's 24 hours: %v", err)
		}
	}
	clk.Advance(2 * time.Hour)
	if _, err := s.Resolve(t.Context(), tok.Token); refusal(t, err).Code != server.CodeUnauthenticated {
		t.Fatalf("the pending session of a 24-hour-old unlisted row: %v, want 401", err)
	}
	wantLive(t, repo, "the 24-hour-old unlisted row after Resolve", reg.DeviceID, false)
}
