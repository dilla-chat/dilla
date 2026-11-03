package api_test

// The four directory routes of interfaces.md §5.1 — rows 10 (publish KeyPackages), 11 (take one),
// 15 (list Welcomes) and 16 (acknowledge one) — end to end over the same mux, sessions, delivery
// service and real wasm core groups_test.go stands up. Nothing between the request and the guest
// is a double.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// Row 10 answers `201 [count]`, and row 11 answers the three-element
// `[blob, last_resort(uint), kp_ref(bstr)]` — last_resort is a UINT on the wire, not a CBOR bool.
func TestPublishAndTakeAKeyPackageOverTheMux(t *testing.T) {
	h := newGroupsAPI(t)
	token, device := h.keyPackageOwnerToken(t)
	blob := apiKeyPackageFixture(t)

	res := h.do(t, http.MethodPost, "/v1/keypackages", token,
		mustCBOR(t, []any{[][]byte{blob}, nil}))
	if res.Code != http.StatusCreated {
		t.Fatalf("publish: status = %d, want 201: %s", res.Code, res.Body.String())
	}
	var published []any
	if err := cborx.Unmarshal(res.Body.Bytes(), &published); err != nil {
		t.Fatalf("decode the publish response: %v", err)
	}
	if len(published) != 1 {
		t.Fatalf("the publish response has %d elements, want 1: [count]", len(published))
	}
	var count struct {
		_     struct{} `cbor:",toarray"`
		Count uint64
	}
	if err := cborx.Unmarshal(res.Body.Bytes(), &count); err != nil {
		t.Fatalf("decode count: %v", err)
	}
	if count.Count != 1 {
		t.Fatalf("count = %d, want 1", count.Count)
	}

	res = h.do(t, http.MethodGet, "/v1/devices/"+device.String()+"/keypackage", h.session, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("take: status = %d, want 200: %s", res.Code, res.Body.String())
	}
	var raw []any
	if err := cborx.Unmarshal(res.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode the take response: %v", err)
	}
	if len(raw) != 3 {
		t.Fatalf("the take response has %d elements, want 3: [blob, last_resort, kp_ref]", len(raw))
	}
	if _, isUint := raw[1].(uint64); !isUint {
		t.Fatalf("last_resort is %T, want a uint", raw[1])
	}
	var out struct {
		_          struct{} `cbor:",toarray"`
		Blob       []byte
		LastResort uint8
		KPRef      []byte
	}
	if err := cborx.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode the take response: %v", err)
	}
	if !bytes.Equal(out.Blob, blob) {
		t.Fatal("the served blob is not the published one")
	}
	if len(out.KPRef) != 32 {
		t.Fatalf("kp_ref is %d bytes, want 32", len(out.KPRef))
	}

	// The directory is empty now, and an empty directory is 404 E_NOT_FOUND.
	res = h.do(t, http.MethodGet, "/v1/devices/"+device.String()+"/keypackage", h.session, nil)
	if res.Code != http.StatusNotFound {
		t.Fatalf("second take: status = %d, want 404: %s", res.Code, res.Body.String())
	}
	if got := errorCode(t, res); got != string(server.CodeNotFound) {
		t.Fatalf("code = %s, want %s", got, server.CodeNotFound)
	}
}

// An accepted publish hands (user, device) to AfterKeyPackages once the 201 is written, so the
// composition root can propose the device into the DMs it could not be Added to while it held no
// KeyPackage (Plan 2 task 6); Drain waits for the run, and a refused publish reaches no hook.
func TestAnAcceptedPublishIsHandedToAfterKeyPackages(t *testing.T) {
	h := newGroupsAPI(t)
	token, device := h.keyPackageOwnerToken(t)
	type seen struct{ user, device id.ID }
	got := make(chan seen, 4)
	h.groups.AfterKeyPackages = func(ctx context.Context, user, dev id.ID) {
		if ctx.Err() != nil {
			t.Errorf("the hook's context has already ended: %v", ctx.Err())
		}
		got <- seen{user, dev}
	}
	if res := h.do(t, http.MethodPost, "/v1/keypackages", token, mustCBOR(t, []any{[][]byte{{0x00}}, nil})); res.Code == http.StatusCreated {
		t.Fatal("a malformed KeyPackage was accepted")
	}
	res := h.do(t, http.MethodPost, "/v1/keypackages", token, mustCBOR(t, []any{[][]byte{apiKeyPackageFixture(t)}, nil}))
	if res.Code != http.StatusCreated {
		t.Fatalf("publish: status = %d, want 201: %s", res.Code, res.Body.String())
	}
	if err := h.groups.Drain(t.Context()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	close(got)
	var all []seen
	for s := range got {
		all = append(all, s)
	}
	_, user := apiKeyPackageIdentity(t)
	if len(all) != 1 || all[0].user != user || all[0].device != device {
		t.Fatalf("AfterKeyPackages saw %v, want exactly one run for (%s, %s)", all, user, device)
	}
}

// Hardening C, point (a), on the wire: the device the committed KeyPackage names is registered with
// a key of its own — a real one, whose session is established through the challenge — that is not
// the key the package's leaf carries. Row 10 answers 422 E_COMMIT_INVALID, the code it gives every
// KeyPackage it refuses as invalid, and the device's directory stays empty.
func TestAKeyPackageNotKeyedByTheDevicesRegisteredKeyIsRefusedOverTheMux(t *testing.T) {
	h := newGroupsAPI(t)
	device, user := apiKeyPackageIdentity(t)
	now := h.deps.Clock.Now().Unix()
	if err := h.deps.Repo.CreateUser(context.Background(), store.UserRow{
		ID: user, Username: "kp" + user.String()[:8], Display: "KeyPackage owner",
		UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64),
		Created: now,
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	token := h.tokenForNewDevice(t, user, device, auth.PurposeSession) // a fresh key, not the leaf's

	res := h.do(t, http.MethodPost, "/v1/keypackages", token,
		mustCBOR(t, []any{[][]byte{apiKeyPackageFixture(t)}, nil}))
	if res.Code != http.StatusUnprocessableEntity {
		t.Fatalf("publish: status = %d, want 422: %s", res.Code, res.Body.String())
	}
	if got := errorCode(t, res); got != "E_COMMIT_INVALID" {
		t.Fatalf("code = %s, want E_COMMIT_INVALID", got)
	}
	res = h.do(t, http.MethodGet, "/v1/devices/"+device.String()+"/keypackage", h.session, nil)
	if res.Code != http.StatusNotFound {
		t.Fatalf("take after a refused publish: status = %d, want 404", res.Code)
	}
}

// Row 15's items are the seven-element [welcome_id, group_id, epoch, commit_seq, blob,
// ratchet_tree, tree_hash]; row 16 is 204 and is what marks one delivered — the GET does not.
func TestTheWelcomeQueueIsServedAndOnlyTheDeleteMarksItDelivered(t *testing.T) {
	h := newGroupsAPI(t)
	joiner, token := h.seedJoiner(t)
	groupID := id.New()
	h.seedWelcome(t, joiner, groupID, 3, bytes.Repeat([]byte{0x51}, 96))

	res := h.do(t, http.MethodGet, "/v1/welcomes", token, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", res.Code, res.Body.String())
	}
	var rows []any
	if err := cborx.Unmarshal(res.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode the array: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d welcomes, want 1", len(rows))
	}
	row0, ok := rows[0].([]any)
	if !ok || len(row0) != 7 {
		t.Fatalf("row 0 is %v, want a seven-element array", rows[0])
	}
	var items []struct {
		_           struct{} `cbor:",toarray"`
		WelcomeID   uint64
		GroupID     id.ID
		Epoch       uint64
		CommitSeq   uint64
		Blob        []byte
		RatchetTree []byte
		TreeHash    []byte
	}
	if err := cborx.Unmarshal(res.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode the rows: %v", err)
	}
	if items[0].GroupID != groupID || items[0].Epoch != 3 {
		t.Fatalf("row = group %s epoch %d, want %s epoch 3", items[0].GroupID, items[0].Epoch, groupID)
	}
	if len(items[0].TreeHash) != 32 {
		t.Fatalf("tree_hash is %d bytes, want 32", len(items[0].TreeHash))
	}
	if len(items[0].RatchetTree) == 0 {
		t.Fatal("the row carries no ratchet tree; row 15 serves the welcoming epoch's")
	}

	// The GET did not consume it.
	if again := h.do(t, http.MethodGet, "/v1/welcomes", token, nil); again.Code != http.StatusOK {
		t.Fatalf("second GET: %d %s", again.Code, again.Body.String())
	}

	path := "/v1/welcomes/" + strconv.FormatUint(items[0].WelcomeID, 10)
	if res := h.do(t, http.MethodDelete, path, token, nil); res.Code != http.StatusNoContent {
		t.Fatalf("delete: status = %d, want 204: %s", res.Code, res.Body.String())
	}
	res = h.do(t, http.MethodGet, "/v1/welcomes", token, nil)
	if err := cborx.Unmarshal(res.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode after the delete: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("%d welcomes survive the delete, want 0", len(rows))
	}
	// A second delete of the same id is 404: the row is marked, so it is no longer the caller's to
	// acknowledge.
	if res := h.do(t, http.MethodDelete, path, token, nil); res.Code != http.StatusNotFound {
		t.Fatalf("second delete: status = %d, want 404: %s", res.Code, res.Body.String())
	}
}

// A non-numeric welcome_id never reaches the delivery service: the surrogate key is a decimal
// integer, not the 32 hex characters every other id in a path is.
func TestAMalformedWelcomeIDIsRefusedBeforeTheDeliveryService(t *testing.T) {
	h := newGroupsAPI(t)
	_, token := h.seedJoiner(t)
	res := h.do(t, http.MethodDelete, "/v1/welcomes/not-a-number", token, nil)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", res.Code, res.Body.String())
	}
	if got := errorCode(t, res); got != string(server.CodeInvalidRequest) {
		t.Fatalf("code = %s, want %s", got, server.CodeInvalidRequest)
	}
}

// Rows 10, 15 and 16 are "E or V": the middleware admits a PROVISIONAL session, and the delivery
// service is what keeps it inside its one pairing group. Row 11 is "E" only and the middleware
// turns it away.
//
// Plan 1 mints no provisional session that names a pairing group — the `sessions.pairing_group`
// column does not exist yet (see auth.Session.PairingGroup) — so every provisional session today
// is bound to no group and every one of the three routes refuses it. That is the fail-closed end
// of the gap, and it is what this test pins: the routes are REACHABLE by the scope and REFUSED by
// the rule, rather than reachable and unguarded.
func TestTheDirectoryRoutesAdmitAProvisionalSessionAndTheRuleRefusesIt(t *testing.T) {
	h := newGroupsAPI(t)
	device, token := h.seedProvisional(t)

	for _, c := range []struct {
		name, method, path string
		body               []byte
	}{
		{"publish", http.MethodPost, "/v1/keypackages", mustCBOR(t, []any{[][]byte{{0x00}}, nil})},
		{"welcomes", http.MethodGet, "/v1/welcomes", nil},
		{"ack", http.MethodDelete, "/v1/welcomes/1", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := h.do(t, c.method, c.path, token, c.body)
			if res.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403: %s", res.Code, res.Body.String())
			}
			if got := errorCode(t, res); got != string(server.CodeProvisionalOutsidePairing) {
				t.Fatalf("code = %s, want %s", got, server.CodeProvisionalOutsidePairing)
			}
		})
	}

	// Row 11 is enrolled-only, and the refusal comes from the middleware with the same code.
	res := h.do(t, http.MethodGet, "/v1/devices/"+device.String()+"/keypackage", token, nil)
	if res.Code != http.StatusForbidden {
		t.Fatalf("take: status = %d, want 403: %s", res.Code, res.Body.String())
	}
	if got := errorCode(t, res); got != string(server.CodeProvisionalOutsidePairing) {
		t.Fatalf("take: code = %s, want %s", got, server.CodeProvisionalOutsidePairing)
	}
}

// ------------------------------------------------------------------ harness additions

// keyPackageOwnerToken seeds the account and device the committed KeyPackage was BUILT for, and
// mints its session. A publish for any other device is E_FORBIDDEN, and one whose leaf key is not
// the device's registered key is E_COMMIT_INVALID (hardening C), so this is the only identity that
// can put the fixture package in the directory: the device is registered under the package's own
// leaf key.
//
// The fixture does not publish that key's private half, so the session cannot come from the
// challenge ceremony. It is minted the way POST /v1/accounts mints a new device's first session
// (auth.Sessions.NewDeviceSession, in the transaction that writes the user and the device), which
// is exactly the session a freshly signed-up browser publishes its first KeyPackages under.
func (h *groupsAPI) keyPackageOwnerToken(t *testing.T) (string, id.ID) {
	t.Helper()
	ctx := context.Background()
	info := apiKeyPackageInfo(t)
	var device, user id.ID
	copy(device[:], info.DeviceID)
	copy(user[:], info.UserID)
	now := h.deps.Clock.Now().Unix()
	var token string
	if err := h.deps.Repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.CreateUser(ctx, store.UserRow{
			ID: user, Username: "kp" + user.String()[:8], Display: "KeyPackage owner",
			UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64),
			Created: now,
		}); err != nil {
			return err
		}
		if err := tx.CreateDevice(ctx, store.DeviceRow{
			ID: device, UserID: user, DSKPub: info.SignatureKey, CredentialBlob: []byte{1},
			LastSeen: now, Created: now,
		}); err != nil {
			return err
		}
		tok, err := h.deps.Sessions.NewDeviceSession(ctx, tx, user, device, 0)
		token = tok.Token
		return err
	}); err != nil {
		t.Fatalf("register the KeyPackage's device: %v", err)
	}
	return token, device
}

// seedJoiner is one enrolled account and device with a session: the device a Welcome is addressed
// to.
func (h *groupsAPI) seedJoiner(t *testing.T) (id.ID, string) {
	t.Helper()
	_, dev, token := seedAPISession(t, h.deps)
	return dev.ID, token
}

// seedProvisional is a device holding a PROVISIONAL session, established through the real
// challenge-and-signature path with `PurposeProvisional`.
func (h *groupsAPI) seedProvisional(t *testing.T) (id.ID, string) {
	t.Helper()
	ctx := context.Background()
	_, dev, priv := seedAPIDevice(t, h.deps)
	nonce, _, err := h.deps.Sessions.Challenge(ctx, dev.ID)
	if err != nil {
		t.Fatalf("Challenge: %v", err)
	}
	pre := auth.SessionPreimage(h.deps.Sessions.InstanceID(), dev.ID, nonce, auth.PurposeProvisional)
	tok, err := h.deps.Sessions.Establish(ctx, auth.EstablishRequest{
		DeviceID: dev.ID, Nonce: nonce, Purpose: auth.PurposeProvisional,
		Sig: ed25519.Sign(priv, pre),
	})
	if err != nil {
		t.Fatalf("Establish: %v", err)
	}
	return dev.ID, tok.Token
}

// tokenForNewDevice writes one device of an existing user with a fresh signing key and establishes
// a session for it through the real ceremony.
func (h *groupsAPI) tokenForNewDevice(t *testing.T, user, device id.ID, purpose auth.Purpose) string {
	t.Helper()
	ctx := context.Background()
	now := h.deps.Clock.Now().Unix()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	verified := now
	if err := h.deps.Repo.CreateDevice(ctx, store.DeviceRow{
		ID: device, UserID: user, DSKPub: pub, CredentialBlob: []byte{1},
		VerifiedAt: &verified, LastSeen: now, Created: now,
	}); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	nonce, _, err := h.deps.Sessions.Challenge(ctx, device)
	if err != nil {
		t.Fatalf("Challenge: %v", err)
	}
	pre := auth.SessionPreimage(h.deps.Sessions.InstanceID(), device, nonce, purpose)
	tok, err := h.deps.Sessions.Establish(ctx, auth.EstablishRequest{
		DeviceID: device, Nonce: nonce, Purpose: purpose, Sig: ed25519.Sign(priv, pre),
	})
	if err != nil {
		t.Fatalf("Establish: %v", err)
	}
	return tok.Token
}

// seedWelcome queues one Welcome for a device, with a real DM-shaped text-group row and the epoch
// tree row 15 serves beside it. The committed fixture's tree is 620 KiB and nothing here is
// asserting its size.
func (h *groupsAPI) seedWelcome(t *testing.T, device, groupID id.ID, epoch uint64, blob []byte) {
	t.Helper()
	ctx := context.Background()
	sum := sha256.Sum256(blob)
	now := h.clk.Now().Unix()
	targetID := id.New()
	binding, err := cborx.Marshal(ds.Binding{
		V: 1, InstanceID: h.keys.InstanceID, TargetID: targetID,
		Kind: 0, PolicyVersion: 1, E2EEVersion: 1,
	})
	if err != nil {
		t.Fatalf("encode DM group binding: %v", err)
	}
	if err := h.deps.Repo.CreateGroup(ctx, store.GroupRow{
		GroupID: groupID, Binding: binding, Kind: 0, TargetID: targetID,
		Ciphersuite: 1, Epoch: epoch, ExternalSenderKeyID: id.New(), E2EEVersion: 1,
		PolicyVersion: 1, Created: now,
	}); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	if err := h.deps.Repo.PutWelcomePayload(ctx, store.WelcomePayloadRow{
		BlobSHA256: sum[:], GroupID: groupID, Epoch: epoch, Blob: blob, Created: now,
	}); err != nil {
		t.Fatalf("PutWelcomePayload: %v", err)
	}
	if err := h.deps.Repo.PutEpochTree(ctx, store.EpochTreeRow{
		GroupID: groupID, Epoch: epoch, RatchetTree: bytes.Repeat([]byte{0xEE}, 64),
		TreeHash: bytes.Repeat([]byte{0xAB}, 32), Created: now,
	}); err != nil {
		t.Fatalf("PutEpochTree: %v", err)
	}
	if err := h.deps.Repo.PutWelcomes(ctx, []store.WelcomeRow{{
		DeviceID: device, GroupID: groupID, Epoch: epoch, CommitSeq: 1,
		BlobSHA256: sum[:], Created: now, Expires: now + 30*24*60*60,
	}}); err != nil {
		t.Fatalf("PutWelcomes: %v", err)
	}
}

// apiKeyPackageFixture is the committed `key_package.mls`.
func apiKeyPackageFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(apiFixtureDir, "key_package.mls"))
	if err != nil {
		t.Fatalf("read the committed KeyPackage: %v", err)
	}
	return raw
}

// apiKeyPackageIdentity is the device and user the committed KeyPackage names, read out of the
// GUEST's own `validate_key_package` rather than out of the manifest — which records the package's
// ref but not its identity — and certainly not invented here: `PublishKeyPackages` compares the
// publishing session's device against exactly this value, so a test that supplied it would be
// asserting its own arithmetic.
func apiKeyPackageIdentity(t *testing.T) (device, user id.ID) {
	t.Helper()
	info := apiKeyPackageInfo(t)
	copy(device[:], info.DeviceID)
	copy(user[:], info.UserID)
	return device, user
}

// apiKeyPackageInfo is everything the guest says about the committed KeyPackage, its leaf's
// signature key included (ABI v4): the key its device must be registered under for the publish to
// be accepted.
func apiKeyPackageInfo(t *testing.T) mlswasi.KeyPackageInfo {
	t.Helper()
	ctx := context.Background()
	f := apiFixtureData(t)
	// A runtime of its own, over the SAME compilation cache the harness uses, so this costs a
	// module instantiation rather than a compile.
	rt, err := mlswasi.New(ctx, f.wasm, mlswasi.Options{PoolSize: 1, CacheDir: apiWasmCacheDir(t)})
	if err != nil {
		t.Fatalf("mlswasi.New: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	inst, err := rt.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer inst.Release()
	info, err := inst.ValidateKeyPackage(ctx, apiKeyPackageFixture(t))
	if err != nil {
		t.Fatalf("ValidateKeyPackage on the committed fixture: %v", err)
	}
	if len(info.DeviceID) != id.Size || len(info.UserID) != id.Size {
		t.Fatalf("the fixture KeyPackage names a %d-byte device and a %d-byte user, want 16 each",
			len(info.DeviceID), len(info.UserID))
	}
	if len(info.SignatureKey) != 32 {
		t.Fatalf("the fixture KeyPackage's leaf key is %d bytes, want 32", len(info.SignatureKey))
	}
	return info
}
