package api_test

// device_pairs_test.go pins the security review's F2: POST /v1/devices proves possession of the
// dsk_pub it registers, one live row per key, and "listed" is the (device_id, dsk_pub) pair. Before
// the fix a stolen enrolled session planted rows that copied a listed key; judged by key they passed
// for listed, so they never expired, were never evicted, refused the key-less DELETE (409) and could
// not be revoked by the owner's client, and eight of them were a permanent 403 for every
// registration, the owner's recovery included.

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// A stolen enrolled session cannot plant a row under a listed key it does not hold: the proof is
// the establish signature by the registered key over a challenge nonce for the new id. Attacker
// statement: the holder of the session reads the user's list (every enrolled session may), so it
// knows every listed dsk_pub; it holds none of their private halves.
func TestPostDevicesRequiresProofOfPossession(t *testing.T) {
	h, deps := newTestAPI(t)
	user, first, token := seedAPISession(t, deps)
	listerOf(t, deps).list(user.ID, first)
	_, attackerKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	copied := id.New()
	rec := cborCall(t, h, http.MethodPost, "/v1/devices", token, devicePostBody(t, deps, copied, first.DSKPub, attackerKey))
	wantListRefusal(t, rec, http.StatusForbidden, "E_FORBIDDEN", "possession of dsk_pub is not proven")
	noAPIDeviceRow(t, deps, copied)

	// The old five-element body, without a proof, is malformed.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bare := id.New()
	rec = cborCall(t, h, http.MethodPost, "/v1/devices", token, []any{bare, []byte(pub), uint64(1), uint64(1), []byte{1}})
	if rec.Code != http.StatusBadRequest || refusalCode(t, rec) != "E_INVALID_REQUEST" {
		t.Fatalf("POST /v1/devices without a proof = %d %q, want 400 E_INVALID_REQUEST", rec.Code, refusalCode(t, rec))
	}
	noAPIDeviceRow(t, deps, bare)

	// A nonce minted for another device_id proves nothing about this one.
	elsewhere := devicePostBody(t, deps, id.New(), pub, priv)
	misbound := id.New()
	elsewhere[0] = misbound
	rec = cborCall(t, h, http.MethodPost, "/v1/devices", token, elsewhere)
	wantListRefusal(t, rec, http.StatusForbidden, "E_FORBIDDEN", "possession of dsk_pub is not proven")
	noAPIDeviceRow(t, deps, misbound)

	// The holder of the key registers.
	honest := id.New()
	body := devicePostBody(t, deps, honest, pub, priv)
	if rec := cborCall(t, h, http.MethodPost, "/v1/devices", token, body); rec.Code != http.StatusOK {
		t.Fatalf("POST /v1/devices with a proof = %d %x, want 200", rec.Code, rec.Body.Bytes())
	}
	// Its proof is spent: replaying the same body is refused before anything else.
	wantListRefusal(t, cborCall(t, h, http.MethodPost, "/v1/devices", token, body),
		http.StatusForbidden, "E_FORBIDDEN", "possession of dsk_pub is not proven")

	// One live row per key: even the key's holder cannot register it under a second id.
	second := id.New()
	rec = cborCall(t, h, http.MethodPost, "/v1/devices", token, devicePostBody(t, deps, second, pub, priv))
	wantListRefusal(t, rec, http.StatusConflict, "E_INVALID_REQUEST", "dsk_pub is already registered to a live device")
	noAPIDeviceRow(t, deps, second)
}

// A browser assertion cannot reuse another user's device id. The global devices primary key
// protects storage; this route must also report the collision in its public 409 vocabulary.
func TestAssertionRegistrationRejectsAnotherUsersDeviceID(t *testing.T) {
	h, deps := newTestAPI(t)
	_, existing, _ := seedAPIDevice(t, deps)
	owner, listed, _ := seedAPIDevice(t, deps)
	listerOf(t, deps).list(owner.ID, listed)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	registration := []any{existing.ID, []byte(pub), uint64(1), uint64(1), []byte{1}}
	rec := postCBOR(h, "/v1/devices/"+existing.ID.String()+"/sessions",
		establishBody(t, h, deps, existing.ID, priv, registration, []byte(deps.Assertions.Issue(owner.ID, 0))))
	wantListRefusal(t, rec, http.StatusConflict, "E_INVALID_REQUEST", "device_id already exists")
	row, err := deps.Repo.GetDevice(t.Context(), existing.ID)
	if err != nil || row.UserID == owner.ID {
		t.Fatalf("existing device owner changed: row=%+v err=%v", row, err)
	}
}

func TestAssertionRegistrationRejectsCrossUserIDEvenWithTheExistingKey(t *testing.T) {
	h, deps := newTestAPI(t)
	_, existing, existingKey := seedAPIDevice(t, deps)
	owner, listed, _ := seedAPIDevice(t, deps)
	listerOf(t, deps).list(owner.ID, listed)
	registration := []any{existing.ID, existing.DSKPub, uint64(1), uint64(1), []byte{1}}
	rec := postCBOR(h, "/v1/devices/"+existing.ID.String()+"/sessions",
		establishBody(t, h, deps, existing.ID, existingKey, registration, []byte(deps.Assertions.Issue(owner.ID, 0))))
	wantListRefusal(t, rec, http.StatusConflict, "E_INVALID_REQUEST", "device_id already exists")
}

// A row that copies a listed key under another id — what a stolen session could plant before the
// fix, still in a database written then — is not the listed device: the owner removes it with the
// key-less DELETE once it is ten minutes old, an assertion registration at the cap evicts it, and it
// expires after 24 hours. REGISTRATION-DEVICES-03 changed two steps: the deleted copy is planted ten
// minutes ago (a younger one is too new for another device to remove), and the eviction and the
// expiry sweep are driven by assertion registration, since POST /v1/devices no longer evicts.
func TestARowCopyingAListedKeyIsUnlisted(t *testing.T) {
	h, deps := newTestAPI(t)
	ctx := t.Context()
	user, first, token := seedAPISession(t, deps)
	now := deps.Clock.Now().Unix()
	plant := func(created int64) id.ID {
		t.Helper()
		row := store.DeviceRow{ID: id.New(), UserID: user.ID, DSKPub: first.DSKPub, Tier: 1, SignerTier: 1,
			CredentialBlob: []byte{1}, Created: created, LastSeen: created}
		if err := deps.Repo.CreateDevice(ctx, row); err != nil {
			t.Fatalf("plant a copy of the listed key: %v", err)
		}
		return row.ID
	}
	listed := []store.DeviceRow{first}
	for i := 0; i < 6; i++ {
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		row := store.DeviceRow{ID: id.New(), UserID: user.ID, DSKPub: pub, CredentialBlob: []byte{1}, Created: 1, LastSeen: 1}
		if err := deps.Repo.CreateDevice(ctx, row); err != nil {
			t.Fatal(err)
		}
		listed = append(listed, row)
	}
	listerOf(t, deps).list(user.ID, listed...)

	deletable := plant(now - 600)
	if rec := cborCall(t, h, http.MethodDelete, "/v1/devices/"+deletable.String(), token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("key-less DELETE of a row copying a listed key = %d %x, want 204", rec.Code, rec.Body.Bytes())
	}
	wantAPIDeviceLive(t, deps, "the deleted copy", deletable, false)
	wantAPIDeviceLive(t, deps, "the listed device whose key was copied", first.ID, true)

	// Seven listed rows and one copy: the cap. The enrolled route refuses; a registration replaces
	// the copy.
	evictable := plant(now)
	if _, rec := postFreshDevice(t, h, deps, token); rec.Code != http.StatusForbidden {
		t.Fatalf("POST /v1/devices at a cap holding a copy = %d %x, want 403", rec.Code, rec.Body.Bytes())
	}
	wantAPIDeviceLive(t, deps, "the copy after a refused POST", evictable, true)
	pendingSession(t, h, deps, user.ID)
	wantAPIDeviceLive(t, deps, "the evicted copy", evictable, false)

	// A copy older than 24 hours is swept as an expired unlisted row.
	expired := plant(now - 86400)
	pendingSession(t, h, deps, user.ID)
	wantAPIDeviceLive(t, deps, "the expired copy", expired, false)
	for _, row := range listed {
		wantAPIDeviceLive(t, deps, "a listed device", row.ID, true)
	}
}

// A planted row under a fresh key (the most a stolen enrolled session can now plant) is unlisted:
// the owner deletes it key-lessly once it is ten minutes old and an assertion registration at the
// cap evicts it. REGISTRATION-DEVICES-03 changed both steps: the DELETE waits out the ten-minute
// grace, and POST /v1/devices at the cap is refused (403) rather than evicting.
func TestAPlantedRowWithAFreshKeyIsEvictableAndDeletable(t *testing.T) {
	h, deps := newTestAPI(t)
	ctx := t.Context()
	user, first, token := seedAPISession(t, deps)
	listed := []store.DeviceRow{first}
	for i := 0; i < 6; i++ {
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		row := store.DeviceRow{ID: id.New(), UserID: user.ID, DSKPub: pub, CredentialBlob: []byte{1}, Created: 1, LastSeen: 1}
		if err := deps.Repo.CreateDevice(ctx, row); err != nil {
			t.Fatal(err)
		}
		listed = append(listed, row)
	}
	listerOf(t, deps).list(user.ID, listed...)

	planted, rec := postFreshDevice(t, h, deps, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("plant = %d %x", rec.Code, rec.Body.Bytes())
	}
	deps.Clock.(*clock.Fake).Advance(10 * time.Minute)
	if rec := cborCall(t, h, http.MethodDelete, "/v1/devices/"+planted.ID.String(), token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("key-less DELETE of a planted row = %d %x, want 204", rec.Code, rec.Body.Bytes())
	}
	wantAPIDeviceLive(t, deps, "the deleted planted row", planted.ID, false)

	planted, rec = postFreshDevice(t, h, deps, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("plant at seven listed = %d %x", rec.Code, rec.Body.Bytes())
	}
	if _, rec := postFreshDevice(t, h, deps, token); rec.Code != http.StatusForbidden {
		t.Fatalf("POST /v1/devices at the cap = %d %x, want 403", rec.Code, rec.Body.Bytes())
	}
	wantAPIDeviceLive(t, deps, "the planted row after a refused POST", planted.ID, true)
	pendingSession(t, h, deps, user.ID)
	wantAPIDeviceLive(t, deps, "the evicted planted row", planted.ID, false)
}

// noAPIDeviceRow fails unless the refused request wrote no device row.
func noAPIDeviceRow(t *testing.T, d api.Deps, device id.ID) {
	t.Helper()
	if _, err := d.Repo.GetDevice(t.Context(), device); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("device %s has a row (GetDevice err %v); a refused registration must write nothing", device, err)
	}
}

func wantAPIDeviceLive(t *testing.T, d api.Deps, what string, device id.ID, live bool) {
	t.Helper()
	row, err := d.Repo.GetDevice(t.Context(), device)
	if err != nil {
		t.Fatalf("%s: GetDevice: %v", what, err)
	}
	if (row.RevokedAt == nil) != live {
		t.Fatalf("%s: revoked_at %v, want live %t", what, row.RevokedAt, live)
	}
}
