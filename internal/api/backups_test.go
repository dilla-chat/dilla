package api_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/blob"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// newBackupAPI is newTestAPIWithConfig with the device buckets opened wide, so a test that makes
// many requests from one device measures the routes and not the meter; the meter test narrows them.
func newBackupAPI(t *testing.T, tune func(*config.Config)) (http.Handler, api.Deps) {
	t.Helper()
	return newTestAPIWithConfig(t, func(c *config.Config) {
		c.Limits.Rate.ReadBurst, c.Limits.Rate.WriteBurst = 1000, 1000
		if tune != nil {
			tune(c)
		}
	})
}

// backupTokens are three live sessions of one device of one user, each established through the
// real challenge-and-signature path: enrolled, pending (a host-login assertion spent on an
// existing device, protocol/02 item 2) and provisional (purpose 2).
type backupTokens struct {
	user                           store.UserRow
	enrolled, pending, provisional string
}

func backupSessions(t *testing.T, d api.Deps) backupTokens {
	t.Helper()
	ctx := t.Context()
	u, dev, priv := seedAPIDevice(t, d)
	establish := func(p auth.Purpose, login []byte, want auth.Scope) string {
		t.Helper()
		nonce, _, err := d.Sessions.Challenge(ctx, dev.ID)
		if err != nil {
			t.Fatalf("Challenge: %v", err)
		}
		pre := auth.SessionPreimage(d.Sessions.InstanceID(), dev.ID, nonce, p)
		tok, err := d.Sessions.Establish(ctx, auth.EstablishRequest{
			DeviceID: dev.ID, Nonce: nonce, Purpose: p, Sig: ed25519.Sign(priv, pre), Login: login,
		})
		if err != nil {
			t.Fatalf("Establish(purpose %d): %v", p, err)
		}
		if tok.Scope != want {
			t.Fatalf("Establish(purpose %d) scope = %d, want %d", p, tok.Scope, want)
		}
		return tok.Token
	}
	return backupTokens{
		user:        u,
		enrolled:    establish(auth.PurposeSession, nil, auth.ScopeEnrolled),
		pending:     establish(auth.PurposeSession, []byte(d.Assertions.Issue(u.ID, 0)), auth.ScopePending),
		provisional: establish(auth.PurposeProvisional, nil, auth.ScopeProvisional),
	}
}

// sealedObject is a stored header object of protocol/06 ("Header"): [1, nonce(12), ciphertext].
// The bytes are not a real AEAD output: the instance cannot open one and never tries.
func sealedObject(t *testing.T, ctLen int, fill byte) []byte {
	t.Helper()
	return mustCBOR(t, []any{uint64(1), bytes.Repeat([]byte{fill}, 12), bytes.Repeat([]byte{fill ^ 0xff}, ctLen)})
}

// rootObject is a 103-byte root object: the 70-byte plaintext [1, umk_priv, ssk_priv] sealed
// under AES-256-GCM is 86 bytes of ciphertext.
func rootObject(t *testing.T, fill byte) []byte {
	t.Helper()
	b := sealedObject(t, 86, fill)
	if len(b) != 103 {
		t.Fatalf("root fixture is %d bytes, want 103", len(b))
	}
	return b
}

func objectID(object []byte) []byte {
	sum := sha256.Sum256(object)
	return sum[:]
}

func backupReq(t *testing.T, h http.Handler, method, path, token string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, bytes.NewReader(body))
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

func putObject(t *testing.T, h http.Handler, kind, token string, object []byte) *httptest.ResponseRecorder {
	t.Helper()
	return backupReq(t, h, http.MethodPut, "/v1/backups/"+kind+"/0", token, mustCBOR(t, []any{object}))
}

func cborArray(t *testing.T, rec *httptest.ResponseRecorder) []any {
	t.Helper()
	var out []any
	if err := cborx.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %d body: %v", rec.Code, err)
	}
	return out
}

// wantRefusal asserts the status and protocol/02's error code.
func wantRefusal(t *testing.T, what string, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("%s = %d, want %d; body %x", what, rec.Code, status, rec.Body.Bytes())
	}
	if got := errorCode(t, rec); got != code {
		t.Fatalf("%s code = %s, want %s", what, got, code)
	}
}

// wantStored asserts a PUT's answer: the status and [SHA-256(object), len(object)].
func wantStored(t *testing.T, what string, rec *httptest.ResponseRecorder, status int, object []byte) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("%s = %d, want %d; body %x", what, rec.Code, status, rec.Body.Bytes())
	}
	if out := cborArray(t, rec); !reflect.DeepEqual(out, []any{objectID(object), uint64(len(object))}) {
		t.Fatalf("%s body = %v, want [blob_id, %d]", what, out, len(object))
	}
}

// L-HTTP-50, F3, Q27: the root object is written once. A repeat of the same bytes is 200 and
// changes nothing; different bytes are 409 and the first root stays. Attacker: only the
// account's own enrolled session reaches this route and it names no user. A stolen enrolled
// session can upload a junk root before the owner's first upload; the owner's first PUT gets 409,
// then the client fetches and compares the stored root and raises E_ROOT_MISMATCH.
func TestTheRootObjectIsWrittenOnce(t *testing.T) {
	h, d := newBackupAPI(t, nil)
	s := backupSessions(t, d)
	ctx := t.Context()
	now := d.Clock.Now().Unix()
	a, b := rootObject(t, 0x11), rootObject(t, 0x22)

	wantStored(t, "PUT of the first root", putObject(t, h, "0", s.enrolled, a), http.StatusCreated, a)
	wantStored(t, "PUT of the same root again", putObject(t, h, "0", s.enrolled, a), http.StatusOK, a)
	wantRefusal(t, "PUT of a different root", putObject(t, h, "0", s.enrolled, b), http.StatusConflict, "E_INVALID_REQUEST")

	rec := backupReq(t, h, http.MethodGet, "/v1/backups/0/0", s.enrolled, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET root = %d", rec.Code)
	}
	if out := cborArray(t, rec); !reflect.DeepEqual(out, []any{a, uint64(now)}) {
		t.Fatalf("GET root = %v, want the first root and its creation time", out)
	}
	rows, err := d.Repo.ListBackups(ctx, s.user.ID, 0)
	if err != nil || len(rows) != 1 || !bytes.Equal(rows[0].BlobID, objectID(a)) {
		t.Fatalf("root rows = %+v, %v; want one row naming the first root", rows, err)
	}
	if row, err := d.Repo.GetBlob(ctx, objectID(a)); err != nil || row.UnrefSince != nil || row.Size != 103 {
		t.Fatalf("the root's blobs row = %+v, %v; want 103 bytes, not marked", row, err)
	}
	// The refused bytes were written before the refusal: they are an orphan, marked so the sweeper
	// collects them after blobs.gc_grace, and kept on disk until then.
	orphan, err := d.Repo.GetBlob(ctx, objectID(b))
	if err != nil || orphan.UnrefSince == nil || *orphan.UnrefSince != now {
		t.Fatalf("the refused root's blobs row = %+v, %v; want it marked unreferenced at %d", orphan, err, now)
	}
	if _, err := d.Blobs.Stat(objectID(b)); err != nil {
		t.Fatalf("the orphan file: %v", err)
	}
	// Another user's root is that user's own.
	other := backupSessions(t, d)
	c := rootObject(t, 0x33)
	wantStored(t, "another user's first root", putObject(t, h, "0", other.enrolled, c), http.StatusCreated, c)
}

// L-HTTP-50 kind 1 and architect ruling 2: the state object is replaced by every PUT. Changed by
// BACKUPS-RECOVERY-01 as amended (it asserted the replaced bytes were marked unreferenced and kept
// for blobs.gc_grace): the bytes it replaced are deleted at once, row and file, when nothing else
// names them, so replacements cannot pile up on disk; bytes stored again are simply stored anew.
// Attacker (L-HTTP-50, ruling 28): kind 1 is replaceable by any enrolled session of the user, a
// stolen one included. It cannot brick recovery: the recovering device treats a state object it
// cannot open as pins = [] and rewrites it with its own re-seal (L-CORE-26 step 3, task 3), so the
// worst a stolen session does is lose pins — none exist on browsers in web-2a. It cannot replace
// the root (409) and gains nothing from reading either object (AEAD under K_header and K_backup).
func TestTheStateObjectIsReplacedAndTheOldBytesDeleted(t *testing.T) {
	h, d := newBackupAPI(t, nil)
	s := backupSessions(t, d)
	ctx := t.Context()
	clk := d.Clock.(*clock.Fake)
	s1, s2 := sealedObject(t, 400, 0x31), sealedObject(t, 500, 0x32)

	wantStored(t, "PUT of the first state", putObject(t, h, "1", s.enrolled, s1), http.StatusCreated, s1)
	wantStored(t, "PUT of the same state again", putObject(t, h, "1", s.enrolled, s1), http.StatusOK, s1)
	if row, err := d.Repo.GetBlob(ctx, objectID(s1)); err != nil || row.UnrefSince != nil {
		t.Fatalf("s1 after a repeat = %+v, %v; want it not marked", row, err)
	}

	gone := func(what string, object []byte) {
		t.Helper()
		if _, err := d.Repo.GetBlob(ctx, objectID(object)); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%s: blobs row err %v, want ErrNotFound", what, err)
		}
		if _, err := d.Blobs.Stat(objectID(object)); !errors.Is(err, blob.ErrNotFound) {
			t.Fatalf("%s: file err %v, want blob.ErrNotFound", what, err)
		}
	}
	clk.Advance(time.Minute)
	replacedAt := clk.Now().Unix()
	wantStored(t, "PUT of a second state", putObject(t, h, "1", s.enrolled, s2), http.StatusOK, s2)
	gone("the replaced state", s1)
	if row, err := d.Repo.GetBlob(ctx, objectID(s2)); err != nil || row.UnrefSince != nil {
		t.Fatalf("the new state's blobs row = %+v, %v; want it not marked", row, err)
	}
	rec := backupReq(t, h, http.MethodGet, "/v1/backups/1/0", s.enrolled, nil)
	if out := cborArray(t, rec); rec.Code != http.StatusOK || !reflect.DeepEqual(out, []any{s2, uint64(replacedAt)}) {
		t.Fatalf("GET state = %d %v, want the second state stored at %d", rec.Code, out, replacedAt)
	}

	clk.Advance(time.Hour)
	wantStored(t, "PUT of the first state once more", putObject(t, h, "1", s.enrolled, s1), http.StatusOK, s1)
	if row, err := d.Repo.GetBlob(ctx, objectID(s1)); err != nil || row.UnrefSince != nil {
		t.Fatalf("s1 stored again = %+v, %v; want it stored and not marked", row, err)
	}
	if _, err := d.Blobs.Stat(objectID(s1)); err != nil {
		t.Fatalf("s1 stored again: file %v", err)
	}
	gone("s2 replaced", s2)
	if rows, err := d.Repo.ListBackups(ctx, s.user.ID, 1); err != nil || len(rows) != 1 || !bytes.Equal(rows[0].BlobID, objectID(s1)) {
		t.Fatalf("state rows = %+v, %v; want one row naming s1", rows, err)
	}
}

// L-HTTP-51, -52, -57 and protocol/02 item 4: a pending session reaches the two reads and nothing
// that writes. Attacker: a stolen host password yields at most a pending session, which reads
// two objects sealed under keys derived from a 260-bit recovery key and can write neither.
func TestThePendingSessionReachesTheTwoReadsOnly(t *testing.T) {
	h, d := newBackupAPI(t, nil)
	s := backupSessions(t, d)
	now := uint64(d.Clock.Now().Unix())
	root, state := rootObject(t, 0x41), sealedObject(t, 300, 0x42)
	wantStored(t, "PUT root", putObject(t, h, "0", s.enrolled, root), http.StatusCreated, root)
	wantStored(t, "PUT state", putObject(t, h, "1", s.enrolled, state), http.StatusCreated, state)

	rec := backupReq(t, h, http.MethodGet, "/v1/backups", s.pending, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("pending GET /v1/backups = %d, want 200", rec.Code)
	}
	want := []any{
		[]any{uint64(0), uint64(0), uint64(103), now},
		[]any{uint64(1), uint64(0), uint64(len(state)), now},
	}
	if out := cborArray(t, rec); !reflect.DeepEqual(out, want) {
		t.Fatalf("GET /v1/backups = %v, want %v", out, want)
	}
	for path, object := range map[string][]byte{"/v1/backups/0/0": root, "/v1/backups/1/0": state} {
		rec := backupReq(t, h, http.MethodGet, path, s.pending, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("pending GET %s = %d, want 200", path, rec.Code)
		}
		if out := cborArray(t, rec); !reflect.DeepEqual(out, []any{object, now}) {
			t.Fatalf("pending GET %s = %v", path, out)
		}
	}

	other := sealedObject(t, 64, 0x43)
	wantRefusal(t, "pending PUT state", putObject(t, h, "1", s.pending, other), http.StatusForbidden, "E_FORBIDDEN")
	wantRefusal(t, "pending DELETE", backupReq(t, h, http.MethodDelete, "/v1/backups/1/0", s.pending, nil), http.StatusForbidden, "E_FORBIDDEN")
	rec = backupReq(t, h, http.MethodGet, "/v1/backups/1/0", s.enrolled, nil)
	if out := cborArray(t, rec); !reflect.DeepEqual(out, []any{state, now}) {
		t.Fatalf("the state after a refused pending PUT = %v, want it unchanged", out)
	}
	for _, path := range []string{"/v1/backups", "/v1/backups/0/0"} {
		wantRefusal(t, "provisional GET "+path, backupReq(t, h, http.MethodGet, path, s.provisional, nil),
			http.StatusForbidden, "E_PROVISIONAL_OUTSIDE_PAIRING")
		wantRefusal(t, "GET "+path+" without a token", backupReq(t, h, http.MethodGet, path, "", nil),
			http.StatusUnauthorized, "E_UNAUTHENTICATED")
	}

	// Self-scoped: another user's sessions see only their own (nothing).
	stranger := backupSessions(t, d)
	rec = backupReq(t, h, http.MethodGet, "/v1/backups", stranger.pending, nil)
	if out := cborArray(t, rec); rec.Code != http.StatusOK || len(out) != 0 {
		t.Fatalf("a stranger's GET /v1/backups = %d %v, want 200 []", rec.Code, out)
	}
	wantRefusal(t, "a stranger's GET root", backupReq(t, h, http.MethodGet, "/v1/backups/0/0", stranger.enrolled, nil),
		http.StatusNotFound, "E_NOT_FOUND")
}

// L-HTTP-50, -52, Q27: only kinds 0 and 1 at chunk 0 exist at wire 1; kind 2 is not served.
func TestOnlyKindsZeroAndOneAtChunkZeroExist(t *testing.T) {
	h, d := newBackupAPI(t, nil)
	s := backupSessions(t, d)
	wantRefusal(t, "GET root before any PUT", backupReq(t, h, http.MethodGet, "/v1/backups/0/0", s.enrolled, nil),
		http.StatusNotFound, "E_NOT_FOUND")
	rec := backupReq(t, h, http.MethodGet, "/v1/backups", s.enrolled, nil)
	if out := cborArray(t, rec); rec.Code != http.StatusOK || out == nil || len(out) != 0 {
		t.Fatalf("GET /v1/backups with none = %d %v, want 200 []", rec.Code, out)
	}
	body := mustCBOR(t, []any{rootObject(t, 0x51)})
	for _, path := range []string{
		"/v1/backups/2/0", "/v1/backups/0/1", "/v1/backups/1/7", "/v1/backups/00/0",
		"/v1/backups/x/0", "/v1/backups/0/-1", "/v1/backups/1/00",
	} {
		wantRefusal(t, "PUT "+path, backupReq(t, h, http.MethodPut, path, s.enrolled, body), http.StatusNotFound, "E_NOT_FOUND")
		wantRefusal(t, "GET "+path, backupReq(t, h, http.MethodGet, path, s.enrolled, nil), http.StatusNotFound, "E_NOT_FOUND")
	}
	for kind := range int32(3) {
		if rows, err := d.Repo.ListBackups(t.Context(), s.user.ID, kind); err != nil || len(rows) != 0 {
			t.Fatalf("kind %d rows = %+v, %v; want none", kind, rows, err)
		}
	}
}

// L-HTTP-53, Q27: DELETE is mounted and answers 501 E_INTERNAL with an empty detail.
func TestDeletingABackupIsNotServed(t *testing.T) {
	h, d := newBackupAPI(t, nil)
	s := backupSessions(t, d)
	now := uint64(d.Clock.Now().Unix())
	root := rootObject(t, 0x61)
	wantStored(t, "PUT root", putObject(t, h, "0", s.enrolled, root), http.StatusCreated, root)
	for _, path := range []string{"/v1/backups/0/0", "/v1/backups/1/0", "/v1/backups/2/0"} {
		rec := backupReq(t, h, http.MethodDelete, path, s.enrolled, nil)
		wantRefusal(t, "DELETE "+path, rec, http.StatusNotImplemented, "E_INTERNAL")
		if out := cborArray(t, rec); len(out) < 2 || out[1] != "" {
			t.Fatalf("DELETE %s detail = %v, want empty", path, out)
		}
	}
	rec := backupReq(t, h, http.MethodGet, "/v1/backups/0/0", s.enrolled, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET root after DELETE = %d, want 200", rec.Code)
	}
	if out := cborArray(t, rec); !reflect.DeepEqual(out, []any{root, now}) {
		t.Fatalf("the root after DELETE = %v, want it unchanged", out)
	}
}

// L-HTTP-50: the body caps and the stored-object shape. Each refusal stores nothing.
func TestTheBodyCapsAndTheObjectShape(t *testing.T) {
	h, d := newBackupAPI(t, nil)
	s := backupSessions(t, d)
	ctx := t.Context()

	// 257-byte object, 261-byte body: over the root cap of 256.
	wantRefusal(t, "a root body over 256 bytes", putObject(t, h, "0", s.enrolled, sealedObject(t, 240, 0x71)),
		http.StatusRequestEntityTooLarge, "E_TOO_LARGE")
	// 1,048,620-byte object, 1,048,626-byte body: under the state cap of 1,048,640.
	big := sealedObject(t, 1_048_600, 0x72)
	wantStored(t, "a state body under the cap", putObject(t, h, "1", s.enrolled, big), http.StatusCreated, big)
	// 1,048,660-byte object, 1,048,666-byte body: over it.
	wantRefusal(t, "a state body over 1,048,640 bytes", putObject(t, h, "1", s.enrolled, sealedObject(t, 1_048_640, 0x73)),
		http.StatusRequestEntityTooLarge, "E_TOO_LARGE")

	nonce, ct := bytes.Repeat([]byte{1}, 12), bytes.Repeat([]byte{2}, 32)
	for name, c := range map[string]struct {
		kind string
		body []byte
	}{
		"a root of 102 bytes":                 {"0", mustCBOR(t, []any{sealedObject(t, 85, 0x74)})},
		"a root of 104 bytes":                 {"0", mustCBOR(t, []any{sealedObject(t, 87, 0x75)})},
		"version 2":                           {"1", mustCBOR(t, []any{mustCBOR(t, []any{uint64(2), nonce, ct})})},
		"an 11-byte nonce":                    {"1", mustCBOR(t, []any{mustCBOR(t, []any{uint64(1), nonce[:11], ct})})},
		"a 13-byte nonce":                     {"1", mustCBOR(t, []any{mustCBOR(t, []any{uint64(1), append(bytes.Clone(nonce), 1), ct})})},
		"a 15-byte ciphertext":                {"1", mustCBOR(t, []any{mustCBOR(t, []any{uint64(1), nonce, ct[:15]})})},
		"four elements":                       {"1", mustCBOR(t, []any{mustCBOR(t, []any{uint64(1), nonce, ct, uint64(0)})})},
		"an object that is not an array":      {"1", mustCBOR(t, []any{bytes.Repeat([]byte{0x01}, 40)})},
		"an object with a trailing byte":      {"1", mustCBOR(t, []any{append(sealedObject(t, 32, 0x76), 0x00)})},
		"the bare object as the body":         {"1", sealedObject(t, 32, 0x77)},
		"a body element that is not a bstr":   {"1", mustCBOR(t, []any{[]any{uint64(1)}})},
		"two body elements":                   {"1", mustCBOR(t, []any{sealedObject(t, 32, 0x78), sealedObject(t, 32, 0x79)})},
		"a body that is a bstr, not an array": {"1", mustCBOR(t, sealedObject(t, 32, 0x7a))},
	} {
		t.Run(name, func(t *testing.T) {
			wantRefusal(t, name, backupReq(t, h, http.MethodPut, "/v1/backups/"+c.kind+"/0", s.enrolled, c.body),
				http.StatusBadRequest, "E_INVALID_REQUEST")
		})
	}

	req := httptest.NewRequestWithContext(ctx, http.MethodPut, "/v1/backups/0/0", bytes.NewReader(mustCBOR(t, []any{rootObject(t, 0x7b)})))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.enrolled)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	wantRefusal(t, "a JSON Content-Type", rec, http.StatusUnsupportedMediaType, "E_INVALID_REQUEST")

	if rows, err := d.Repo.ListBackups(ctx, s.user.ID, 0); err != nil || len(rows) != 0 {
		t.Fatalf("root rows after refusals = %+v, %v; want none", rows, err)
	}
	if rows, err := d.Repo.ListBackups(ctx, s.user.ID, 1); err != nil || len(rows) != 1 || !bytes.Equal(rows[0].BlobID, objectID(big)) {
		t.Fatalf("state rows = %+v, %v; want only the accepted one", rows, err)
	}
}

// L-HTTP-50: backup bytes count against blobs.store_max_bytes (and, by boundary ruling 2, against
// the uploader's quota: TestBackupsCountTowardTheUploadersQuota). Attacker: another user can fill
// the instance store with attachments (each bounded by quota_bytes_per_user) and so fail an honest
// user's upload; the client keeps its objects and retries.
func TestBackupsCountAgainstTheInstanceStore(t *testing.T) {
	h, d := newBackupAPI(t, func(c *config.Config) {
		c.Blobs.StoreMaxBytes = 200
	})
	s := backupSessions(t, d)
	ctx := t.Context()
	root := rootObject(t, 0x81)
	wantStored(t, "a 103-byte root under a 200-byte store", putObject(t, h, "0", s.enrolled, root), http.StatusCreated, root)

	state := sealedObject(t, 120, 0x82) // 137 bytes: 103 + 137 > 200
	wantRefusal(t, "a state that overflows the store", putObject(t, h, "1", s.enrolled, state), http.StatusInsufficientStorage, "E_STORAGE_FULL")
	if _, err := d.Repo.GetBlob(ctx, objectID(state)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the refused state's blobs row: %v, want ErrNotFound", err)
	}
	if _, err := d.Blobs.Stat(objectID(state)); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("the refused state's file: %v, want blob.ErrNotFound", err)
	}
	wantRefusal(t, "GET the refused state", backupReq(t, h, http.MethodGet, "/v1/backups/1/0", s.enrolled, nil), http.StatusNotFound, "E_NOT_FOUND")

	wantStored(t, "the stored root again (no new bytes)", putObject(t, h, "0", s.enrolled, root), http.StatusOK, root)
	small := sealedObject(t, 60, 0x83) // 77 bytes: 103 + 77 <= 200
	wantStored(t, "a state that fits", putObject(t, h, "1", s.enrolled, small), http.StatusCreated, small)
}

// Boundary ruling 2 (task 6 scan): backup objects count toward the uploader's
// blobs.quota_bytes_per_user with their attachments, each distinct blob once, and a refusal is the
// blob route's 507 E_STORAGE_FULL. A replaced state object leaves the count with its row, so
// replacing it at the quota nets out. Attacker: before this, backups counted only against the
// instance-wide store_max_bytes, so one user could fill the instance through state replacements
// alone; now each user's own quota bounds what their sessions store. Only the user's own enrolled
// sessions reach the route, so no one can spend another user's quota.
func TestBackupsCountTowardTheUploadersQuota(t *testing.T) {
	h, d := newBackupAPI(t, func(c *config.Config) {
		c.Blobs.QuotaBytesPerUser = 240
	})
	s := backupSessions(t, d)
	ctx := t.Context()
	used := func(what string, want int64) {
		t.Helper()
		if n, err := d.Repo.UserBlobBytes(ctx, s.user.ID); err != nil || n != want {
			t.Fatalf("%s: UserBlobBytes = %d, %v; want %d", what, n, err, want)
		}
	}
	root := rootObject(t, 0x91)
	wantStored(t, "a root under the quota", putObject(t, h, "0", s.enrolled, root), http.StatusCreated, root)
	used("after the root", 103)
	s1 := sealedObject(t, 120, 0x92) // 137 bytes: 103 + 137 = 240, exactly the quota
	wantStored(t, "a state that reaches the quota", putObject(t, h, "1", s.enrolled, s1), http.StatusCreated, s1)
	used("at the quota", 240)

	s2 := sealedObject(t, 120, 0x93) // the same size: the replacement nets out
	wantStored(t, "a same-size replacement at the quota", putObject(t, h, "1", s.enrolled, s2), http.StatusOK, s2)
	used("after a replacement", 240)

	big := sealedObject(t, 121, 0x94) // 138 bytes: 103 + 138 > 240
	wantRefusal(t, "a replacement past the quota", putObject(t, h, "1", s.enrolled, big), http.StatusInsufficientStorage, "E_STORAGE_FULL")
	used("after the refusal", 240)
	rec := backupReq(t, h, http.MethodGet, "/v1/backups/1/0", s.enrolled, nil)
	if out := cborArray(t, rec); rec.Code != http.StatusOK || len(out) != 2 || !bytes.Equal(out[0].([]byte), s2) {
		t.Fatalf("the state after a refused replacement = %d %v, want s2 unchanged", rec.Code, out)
	}
	// The refused bytes were written before the exact check: an orphan, marked for the sweeper and
	// counted toward nobody's quota.
	if row, err := d.Repo.GetBlob(ctx, objectID(big)); err != nil || row.UnrefSince == nil {
		t.Fatalf("the refused state's blobs row = %+v, %v; want it marked unreferenced", row, err)
	}

	// Another user at zero is under the quota; the first user's objects are not theirs.
	other := backupSessions(t, d)
	wantStored(t, "another user's root", putObject(t, h, "0", other.enrolled, rootObject(t, 0x95)), http.StatusCreated, rootObject(t, 0x95))
	if n, err := d.Repo.UserBlobBytes(ctx, other.user.ID); err != nil || n != 103 {
		t.Fatalf("the other user's count = %d, %v; want 103", n, err)
	}

	// A user whose quota is already spent is refused even a root.
	full, fd := newBackupAPI(t, func(c *config.Config) {
		c.Blobs.QuotaBytesPerUser = 102
	})
	fs := backupSessions(t, fd)
	wantRefusal(t, "a root over the quota", putObject(t, full, "0", fs.enrolled, rootObject(t, 0x96)), http.StatusInsufficientStorage, "E_STORAGE_FULL")
	if rows, err := fd.Repo.ListBackups(ctx, fs.user.ID, 0); err != nil || len(rows) != 0 {
		t.Fatalf("root rows after a quota refusal = %+v, %v; want none", rows, err)
	}
}

// BACKUPS-RECOVERY-01, the quota half: the quota check for a state replacement is the delta over
// the object it replaces, so a replacement no larger than its predecessor never fails it, even for
// a user already at or past the quota (an operator lowered it, or attachments filled it). Before the
// fix the whole user count was checked after the upsert, so such a user could not re-seal the state
// object at all — and revoking another device starts with that PUT. A larger replacement is still
// held to the quota.
func TestAStateReplacementNoLargerThanItsPredecessorNeverFailsTheQuota(t *testing.T) {
	h, d := newBackupAPI(t, func(c *config.Config) {
		c.Blobs.QuotaBytesPerUser = 240
	})
	s := backupSessions(t, d)
	root := rootObject(t, 0xd1)
	wantStored(t, "the root", putObject(t, h, "0", s.enrolled, root), http.StatusCreated, root)
	s1 := sealedObject(t, 120, 0xd2) // 137 bytes: 103 + 137 = 240, exactly the quota
	wantStored(t, "a state at the quota", putObject(t, h, "1", s.enrolled, s1), http.StatusCreated, s1)

	d.Config.Blobs.QuotaBytesPerUser = 200 // the user is now 40 bytes past it
	s2 := sealedObject(t, 120, 0xd3)       // the same size
	wantStored(t, "an equal-size replacement past the quota", putObject(t, h, "1", s.enrolled, s2), http.StatusOK, s2)
	s3 := sealedObject(t, 100, 0xd4) // smaller
	wantStored(t, "a smaller replacement past the quota", putObject(t, h, "1", s.enrolled, s3), http.StatusOK, s3)
	s4 := sealedObject(t, 101, 0xd5) // one byte larger than its predecessor, and 103 + 118 > 200
	wantRefusal(t, "a larger replacement past the quota", putObject(t, h, "1", s.enrolled, s4), http.StatusInsufficientStorage, "E_STORAGE_FULL")
	rec := backupReq(t, h, http.MethodGet, "/v1/backups/1/0", s.enrolled, nil)
	if out := cborArray(t, rec); rec.Code != http.StatusOK || len(out) != 2 || !bytes.Equal(out[0].([]byte), s3) {
		t.Fatalf("the state after a refused larger replacement = %d %v, want s3 unchanged", rec.Code, out)
	}

	// A larger replacement that fits is admitted as before.
	d.Config.Blobs.QuotaBytesPerUser = 240
	wantStored(t, "a larger replacement under the quota", putObject(t, h, "1", s.enrolled, s4), http.StatusOK, s4)
}

// wantNothingStored asserts that a refused PUT left no row, no blobs row and no file for object.
func wantNothingStored(t *testing.T, d api.Deps, user store.UserRow, kind int32, object []byte) {
	t.Helper()
	ctx := t.Context()
	if rows, err := d.Repo.ListBackups(ctx, user.ID, kind); err != nil || len(rows) != 0 {
		t.Fatalf("kind %d rows = %+v, %v; want none", kind, rows, err)
	}
	if _, err := d.Repo.GetBlob(ctx, objectID(object)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the refused object's blobs row: %v, want ErrNotFound", err)
	}
	if _, err := d.Blobs.Stat(objectID(object)); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("the refused object's file: %v, want blob.ErrNotFound", err)
	}
}

// Boundary ruling 1 (task 6 scan): an administrator's purge tombstones a blob id, and every route
// that stores bytes under their hash refuses those bytes again with the blob route's 410 E_PRUNED;
// without it, content addressing would let anyone holding the purged ciphertext re-upload it as a
// backup object and get the same name back. Attacker: only an enrolled session of the uploader
// reaches the route, and the refusal is about the bytes, not the user: no honest client's backup
// object is the hash of content an administrator removed.
func TestAPurgedBlobCannotBeStoredAsABackup(t *testing.T) {
	h, d := newBackupAPI(t, nil)
	s := backupSessions(t, d)
	root, state := rootObject(t, 0xc1), sealedObject(t, 200, 0xc2)
	for _, object := range [][]byte{root, state} {
		if err := d.Repo.PutBlobTombstone(t.Context(), objectID(object), "takedown", s.user.ID, d.Clock.Now().Unix()); err != nil {
			t.Fatalf("PutBlobTombstone: %v", err)
		}
	}
	wantRefusal(t, "PUT of a purged root", putObject(t, h, "0", s.enrolled, root), http.StatusGone, "E_PRUNED")
	wantNothingStored(t, d, s.user, 0, root)
	wantRefusal(t, "PUT of a purged state", putObject(t, h, "1", s.enrolled, state), http.StatusGone, "E_PRUNED")
	wantNothingStored(t, d, s.user, 1, state)
}

// purgeAfterFirstCheck is the repository the backup handler reads through, with an administrator's
// purge committing right after the handler's first tombstone read: the bytes were not purged when
// the request arrived and are by the time its row would be written.
type purgeAfterFirstCheck struct {
	store.Repository
	by   store.UserRow
	at   int64
	done bool
}

func (p *purgeAfterFirstCheck) GetBlobTombstone(ctx context.Context, blobID []byte) (bool, error) {
	tomb, err := p.Repository.GetBlobTombstone(ctx, blobID)
	if !p.done {
		p.done = true
		if perr := p.PutBlobTombstone(ctx, blobID, "takedown", p.by.ID, p.at); perr != nil {
			return false, perr
		}
	}
	return tomb, err
}

// Boundary ruling 1, the transaction half: a purge that commits between the first check and the
// transaction that records the object is honoured there too, as the blob route's fix wave I9 does,
// and the bytes the request wrote are removed.
func TestABackupRacingAPurgeOfTheSameBytesLeavesNothing(t *testing.T) {
	_, d := newBackupAPI(t, nil)
	s := backupSessions(t, d)
	racing := &purgeAfterFirstCheck{Repository: d.Repo, by: s.user, at: d.Clock.Now().Unix()}
	d.Repo = racing
	m := server.NewMux()
	api.Register(m, d)
	state := sealedObject(t, 200, 0xc3)
	wantRefusal(t, "PUT racing a purge", putObject(t, m, "1", s.enrolled, state), http.StatusGone, "E_PRUNED")
	if !racing.done {
		t.Fatal("the handler never read the tombstone before the transaction; the race was not run")
	}
	wantNothingStored(t, d, s.user, 1, state)
}

// L-HTTP common rule: the four routes spend the device session's read and write buckets.
func TestTheBackupRoutesSpendTheDeviceBuckets(t *testing.T) {
	h, d := newTestAPIWithConfig(t, func(c *config.Config) {
		c.Limits.Rate.WriteBurst, c.Limits.Rate.ReadBurst = 1, 1
	})
	s := backupSessions(t, d)
	root := rootObject(t, 0x91)
	wantStored(t, "the first PUT", putObject(t, h, "0", s.enrolled, root), http.StatusCreated, root)
	wantRefusal(t, "a second PUT", putObject(t, h, "1", s.enrolled, sealedObject(t, 32, 0x92)), http.StatusTooManyRequests, "E_RATE_LIMITED")
	wantRefusal(t, "a DELETE", backupReq(t, h, http.MethodDelete, "/v1/backups/0/0", s.enrolled, nil), http.StatusTooManyRequests, "E_RATE_LIMITED")
	if rec := backupReq(t, h, http.MethodGet, "/v1/backups", s.pending, nil); rec.Code != http.StatusOK {
		t.Fatalf("the first GET = %d, want 200", rec.Code)
	}
	wantRefusal(t, "a second GET", backupReq(t, h, http.MethodGet, "/v1/backups/0/0", s.enrolled, nil), http.StatusTooManyRequests, "E_RATE_LIMITED")
}

// Requirement 10: a composition root that forgot the blob store refuses rather than half-writes.
func TestAnUnwiredBackupStoreFailsClosed(t *testing.T) {
	_, d := newBackupAPI(t, nil)
	d.Blobs = nil
	m := server.NewMux()
	api.Register(m, d)
	s := backupSessions(t, d)
	wantRefusal(t, "PUT without a store", putObject(t, m, "0", s.enrolled, rootObject(t, 0xa1)), http.StatusInternalServerError, "E_INTERNAL")
	if rows, err := d.Repo.ListBackups(t.Context(), s.user.ID, 0); err != nil || len(rows) != 0 {
		t.Fatalf("rows after an unwired PUT = %+v, %v; want none", rows, err)
	}
	wantRefusal(t, "GET root without a store", backupReq(t, m, http.MethodGet, "/v1/backups/0/0", s.enrolled, nil), http.StatusInternalServerError, "E_INTERNAL")
	rec := backupReq(t, m, http.MethodGet, "/v1/backups", s.enrolled, nil)
	if out := cborArray(t, rec); rec.Code != http.StatusOK || len(out) != 0 {
		t.Fatalf("GET /v1/backups without a store = %d %v, want 200 []", rec.Code, out)
	}
}

// L-HTTP-52: a row whose bytes are gone is a server fault, never a 404 that would tell the client
// to upload a new root.
func TestAStoredObjectThatCannotBeReadIsAServerFault(t *testing.T) {
	h, d := newBackupAPI(t, nil)
	s := backupSessions(t, d)
	root := rootObject(t, 0xb1)
	wantStored(t, "PUT root", putObject(t, h, "0", s.enrolled, root), http.StatusCreated, root)
	if err := d.Blobs.Delete(objectID(root)); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	wantRefusal(t, "GET a root whose file is gone", backupReq(t, h, http.MethodGet, "/v1/backups/0/0", s.enrolled, nil),
		http.StatusInternalServerError, "E_INTERNAL")
}
