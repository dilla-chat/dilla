package api_test

// backup_meter_test.go pins the security review's F3 for the root object and the branch review's
// BACKUPS-RECOVERY-01 for the state object. PUT /v1/backups/0/0 (the root, written once) spends the
// blob upload meter (blobs.uploads_per_minute and blobs.upload_bytes_per_day) exactly as a blob PUT
// does. PUT /v1/backups/1/0 (the state object) does not: it spends the device session's own write
// bucket and the device's own daily byte budget (StateMeter). Before the branch review it also drew on the user's shared upload meter, so a stolen
// session of the same user, uploading tiny objects at its own device rate, kept the shared bucket
// empty and the owner's state PUT, which revoking another device starts with, answered 429 for as
// long as the thief kept at it.

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/blob"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// The 21st root PUT in a minute is the blob route's 429 E_RATE_LIMITED, before the body is read;
// a minute later it passes. Changed by BACKUPS-RECOVERY-01: this drove state PUTs, which no longer
// spend the upload meter; the root, the kind that still does, drives it now.
func TestTheRootPutSpendsTheUploadsPerMinute(t *testing.T) {
	h, d := newBackupAPI(t, nil) // blobs.uploads_per_minute = 20 by default
	if d.Config.Blobs.UploadsPerMinute != 20 {
		t.Fatalf("uploads_per_minute = %d, want the default 20", d.Config.Blobs.UploadsPerMinute)
	}
	s := backupSessions(t, d)
	root := rootObject(t, 0x31)
	wantStored(t, "the root", putObject(t, h, "0", s.enrolled, root), http.StatusCreated, root)
	for i := 1; i < 20; i++ {
		if rec := putObject(t, h, "0", s.enrolled, root); rec.Code != http.StatusOK {
			t.Fatalf("root PUT %d of 20 = %d %x", i+1, rec.Code, rec.Body.Bytes())
		}
	}
	rec := putObject(t, h, "0", s.enrolled, root)
	wantRefusal(t, "the 21st root PUT in a minute", rec, http.StatusTooManyRequests, "E_RATE_LIMITED")
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("the refusal carries no Retry-After")
	}
	d.Clock.(*clock.Fake).Advance(time.Minute)
	if rec := putObject(t, h, "0", s.enrolled, root); rec.Code != http.StatusOK {
		t.Fatalf("a root PUT a minute later = %d %x, want 200", rec.Code, rec.Body.Bytes())
	}
}

// Past blobs.upload_bytes_per_day the root PUT is the blob route's 429 E_RATE_LIMITED, and every
// byte read spends the budget whatever happens to the object (a repeat of the stored root included).
// Changed by BACKUPS-RECOVERY-01 like the test above: state PUTs drove it.
func TestTheRootPutSpendsTheDailyUploadBytes(t *testing.T) {
	h, d := newBackupAPI(t, func(c *config.Config) { c.Blobs.UploadBytesPerDay = 250 }) // a root body is 106 bytes
	s := backupSessions(t, d)
	root := rootObject(t, 0x41)
	for i := 0; i < 2; i++ {
		if rec := putObject(t, h, "0", s.enrolled, root); rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("root PUT %d = %d %x", i+1, rec.Code, rec.Body.Bytes())
		}
	}
	wantRefusal(t, "a third root body on a 250-byte daily budget",
		putObject(t, h, "0", s.enrolled, root), http.StatusTooManyRequests, "E_RATE_LIMITED")
	// The budget refills continuously: a day later the same object passes.
	d.Clock.(*clock.Fake).Advance(24 * time.Hour)
	if rec := putObject(t, h, "0", s.enrolled, root); rec.Code != http.StatusOK {
		t.Fatalf("the root PUT a day later = %d %x, want 200", rec.Code, rec.Body.Bytes())
	}
}

// secondEnrolledSession adds a device to user (who has published no list, so the device is
// enrolled) and establishes a session for it through the real challenge-and-signature path.
func secondEnrolledSession(t *testing.T, d api.Deps, user id.ID) string {
	t.Helper()
	ctx := t.Context()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	now := d.Clock.Now().Unix()
	dev := store.DeviceRow{ID: id.New(), UserID: user, DSKPub: pub, CredentialBlob: []byte{1}, LastSeen: now, Created: now}
	if err := d.Repo.CreateDevice(ctx, dev); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	nonce, _, err := d.Sessions.Challenge(ctx, dev.ID)
	if err != nil {
		t.Fatalf("Challenge: %v", err)
	}
	tok, err := d.Sessions.Establish(ctx, auth.EstablishRequest{DeviceID: dev.ID, Nonce: nonce, Purpose: auth.PurposeSession,
		Sig: ed25519.Sign(priv, auth.SessionPreimage(d.Sessions.InstanceID(), dev.ID, nonce, auth.PurposeSession))})
	if err != nil || tok.Scope != auth.ScopeEnrolled {
		t.Fatalf("establish the second device: %v (scope %d)", err, tok.Scope)
	}
	return tok.Token
}

// BACKUPS-RECOVERY-01, the meter half. Attacker statement: a thief holds an enrolled session of
// another device of the same user and spends the user's whole upload meter (here one upload a
// minute and a 200-byte day). The owner's state PUT still lands, a state object larger than the
// day's byte budget included, because the state object draws only on the owner's own device
// bucket; the root, written once, still draws on the shared meter.
func TestAThiefExhaustingTheUploadMeterDoesNotBlockTheOwnersStatePut(t *testing.T) {
	h, d := newBackupAPI(t, func(c *config.Config) {
		c.Blobs.UploadsPerMinute, c.Blobs.UploadBytesPerDay = 1, 200
	})
	thief := backupSessions(t, d)
	owner := secondEnrolledSession(t, d, thief.user.ID)
	root := rootObject(t, 0x51)
	wantStored(t, "the thief's root PUT", putObject(t, h, "0", thief.enrolled, root), http.StatusCreated, root)
	wantRefusal(t, "a root PUT once the meter is spent", putObject(t, h, "0", owner, root),
		http.StatusTooManyRequests, "E_RATE_LIMITED")

	state := sealedObject(t, 300, 0x52) // 317 bytes: more than the whole day's byte budget
	wantStored(t, "the owner's state PUT with the meter spent", putObject(t, h, "1", owner, state), http.StatusCreated, state)
	again := sealedObject(t, 300, 0x53)
	wantStored(t, "the owner's next state PUT", putObject(t, h, "1", owner, again), http.StatusOK, again)
	junk := sealedObject(t, 16, 0x54)
	wantStored(t, "the thief's state PUT", putObject(t, h, "1", thief.enrolled, junk), http.StatusOK, junk)
	wantStored(t, "the owner's state PUT after the thief's", putObject(t, h, "1", owner, again), http.StatusOK, again)
}

// BACKUPS-RECOVERY-01 as amended (the disk half). Attacker statement: off the shared upload meter
// and with a delta quota, each distinct state PUT used to leave its predecessor on disk for
// blobs.gc_grace, so one enrolled session at its 2/s write bucket stored about 7 GiB an hour that
// no quota counted (security review F3 again). A replacement now deletes the bytes it replaced at
// once: a thousand replacements leave exactly one state blob, in the blob table and on disk.
func TestAThousandStateReplacementsLeaveOneStateBlob(t *testing.T) {
	h, d := newBackupAPI(t, func(c *config.Config) { c.Limits.Rate.WriteBurst = 2000 })
	s := backupSessions(t, d)
	ctx := t.Context()
	objects := make([][]byte, 0, 1000)
	for i := 0; i < 1000; i++ {
		nonce := make([]byte, 12)
		nonce[0], nonce[1] = byte(i), byte(i>>8)
		object := mustCBOR(t, []any{uint64(1), nonce, make([]byte, 32)})
		if rec := putObject(t, h, "1", s.enrolled, object); rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("state PUT %d = %d %x", i+1, rec.Code, rec.Body.Bytes())
		}
		objects = append(objects, object)
	}
	last := objects[len(objects)-1]
	for i, object := range objects[:len(objects)-1] {
		if _, err := d.Repo.GetBlob(ctx, objectID(object)); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("replaced state %d: blobs row err %v, want ErrNotFound", i, err)
		}
		if _, err := d.Blobs.Stat(objectID(object)); !errors.Is(err, blob.ErrNotFound) {
			t.Fatalf("replaced state %d: file err %v, want blob.ErrNotFound", i, err)
		}
	}
	if n, err := d.Repo.InstanceBlobBytes(ctx); err != nil || n != int64(len(last)) {
		t.Fatalf("instance blob bytes = %d, %v; want %d, the one live state object", n, err, len(last))
	}
	if _, err := d.Blobs.Stat(objectID(last)); err != nil {
		t.Fatalf("the live state's file: %v", err)
	}
}

// BACKUPS-RECOVERY-01 as amended (the budget half): a state PUT also spends a per-DEVICE daily
// byte budget (StateBytesPerDevicePerDay; narrowed here to 1000 bytes). A device past it gets 429;
// another device of the same user still PUTs, so a thief spending its own budget cannot block the
// owner. The budget refills over the day.
func TestADevicePastItsDailyStateBudgetIsRefusedWhileAnotherDeviceIsNot(t *testing.T) {
	h, d := newTestAPIFull(t, func(c *config.Config) {
		c.Limits.Rate.ReadBurst, c.Limits.Rate.WriteBurst = 1000, 1000
	}, func(d *api.Deps) { d.StateMeter = api.NewStateMeter(d.Clock, 1000) })
	thief := backupSessions(t, d)
	owner := secondEnrolledSession(t, d, thief.user.ID)
	for i := 0; i < 3; i++ { // 317-byte objects in 322-byte bodies: three fit in 1000 bytes
		object := sealedObject(t, 300, byte(0x60+i))
		if rec := putObject(t, h, "1", thief.enrolled, object); rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("thief state PUT %d = %d %x", i+1, rec.Code, rec.Body.Bytes())
		}
	}
	over := sealedObject(t, 300, 0x6f)
	wantRefusal(t, "a state PUT past the device's daily budget", putObject(t, h, "1", thief.enrolled, over),
		http.StatusTooManyRequests, "E_RATE_LIMITED")
	if _, err := d.Repo.GetBlob(t.Context(), objectID(over)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the refused PUT stored its bytes: %v", err)
	}
	mine := sealedObject(t, 300, 0x70)
	wantStored(t, "another device's state PUT", putObject(t, h, "1", owner, mine), http.StatusOK, mine)
	d.Clock.(*clock.Fake).Advance(24 * time.Hour)
	wantStored(t, "the thief's state PUT a day later", putObject(t, h, "1", thief.enrolled, over), http.StatusOK, over)
}
