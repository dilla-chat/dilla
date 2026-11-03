package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strconv"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/blob"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/obs"
	"github.com/dilla-chat/dilla/internal/ops"
	"github.com/dilla-chat/dilla/internal/store"
)

// NewInstanceAdmin creates a user whose users.flags has bit 0 set (NV8,
// protocol/09 § Flags) and returns its bearer token.
func (e *env) NewInstanceAdmin(username string) string {
	e.t.Helper()
	uid, did := id.New(), id.New()
	now := e.Clk.Now().Unix()
	u := newAPITestUser(uid, username, now)
	u.Flags = store.UserFlagInstanceAdmin
	if err := e.Repo.CreateUser(context.Background(), u); err != nil {
		e.t.Fatalf("CreateUser: %v", err)
	}
	if err := e.Repo.CreateDevice(context.Background(), newAPITestDevice(did, uid, now)); err != nil {
		e.t.Fatalf("CreateDevice: %v", err)
	}
	tok := uid.String()
	e.sess[tok] = sessionFor(uid, did)
	return tok
}

// joinChannel makes tok a member of ch's community and adds its
// channel_members row.
func joinChannel(t *testing.T, e *env, ch id.ID, tok string) {
	t.Helper()
	row := mustChannel(t, e, ch)
	joinCommunity(t, e, *row.CommunityID, tok)
	if err := e.Repo.PutChannelMember(t.Context(), ch, userOf(t, e, tok), e.Clk.Now().Unix()); err != nil {
		t.Fatalf("PutChannelMember: %v", err)
	}
}

// newModerator creates a member of ch's community who holds
// PermManageMessages in ch through a role, and returns its token. The role is
// written through the store because blobEnv mounts no role routes; the bit is
// asserted through the one resolver, so a test that relies on "a moderator"
// really has one.
func newModerator(t *testing.T, e *env, ch id.ID) string {
	t.Helper()
	row := mustChannel(t, e, ch)
	cid := *row.CommunityID
	modID, modTok := e.NewUser("moderator")
	joinChannel(t, e, ch, modTok)
	role := id.New()
	if err := e.Repo.PutRole(t.Context(), store.RoleRow{
		ID: role, CommunityID: cid, Name: "mods", Position: 10,
		Allow: uint64(api.PermManageMessages), Created: e.Clk.Now().Unix(),
	}); err != nil {
		t.Fatalf("PutRole: %v", err)
	}
	if err := e.Repo.PutMemberRole(t.Context(), cid, modID, role); err != nil {
		t.Fatalf("PutMemberRole: %v", err)
	}
	bits, err := api.NewResolver(e.Repo).Resolve(t.Context(), modID, row)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !bits.Has(api.PermManageMessages) {
		t.Fatalf("the moderator's bits %b lack manage_messages", bits)
	}
	return modTok
}

func TestDeleteRemovesOnlyThisChannelsReference(t *testing.T) {
	e, ch, tok := blobEnv(t)
	other := secondChannel(t, e, ch, tok)
	payload := []byte("shared")
	sum := sha256.Sum256(payload)
	for _, c := range []id.ID{ch, other} {
		if status, _ := e.DoRaw(http.MethodPut,
			"/v1/channels/"+c.String()+"/blobs/"+hex.EncodeToString(sum[:]), tok,
			"application/octet-stream", payload); status >= 300 {
			t.Fatalf("PUT into %x = %d", c, status)
		}
	}
	if status, _ := e.Do(http.MethodDelete,
		"/v1/channels/"+ch.String()+"/blobs/"+hex.EncodeToString(sum[:]), tok, nil); status != http.StatusNoContent {
		t.Fatal("DELETE failed")
	}
	n, err := e.Repo.CountBlobRefs(t.Context(), sum[:])
	if err != nil || n != 1 {
		t.Fatalf("references = %d (%v), want 1", n, err)
	}
	// The other channel can still fetch it.
	if status, _ := e.Do(http.MethodGet,
		"/v1/channels/"+other.String()+"/blobs/"+hex.EncodeToString(sum[:]), tok, nil); status != http.StatusOK {
		t.Fatal("the surviving reference could not fetch the blob")
	}
	// DELETE is idempotent.
	if status, _ := e.Do(http.MethodDelete,
		"/v1/channels/"+ch.String()+"/blobs/"+hex.EncodeToString(sum[:]), tok, nil); status != http.StatusNoContent {
		t.Fatal("a repeated DELETE was not 204")
	}
	// The channel that dropped its reference no longer reaches the bytes.
	if status, _ := e.Do(http.MethodGet,
		"/v1/channels/"+ch.String()+"/blobs/"+hex.EncodeToString(sum[:]), tok, nil); status != http.StatusNotFound {
		t.Fatalf("GET after the DELETE = %d, want 404", status)
	}
}

// Deleting the last reference marks the blob for the sweeper; the file stays on
// disk until the grace window has passed (gap-47 INV-B3).
func TestDeletingTheLastReferenceMarksTheBlobAndKeepsTheFile(t *testing.T) {
	e, ch, tok := blobEnv(t)
	payload := []byte("last one")
	sum := sha256.Sum256(payload)
	if status, _ := e.DoRaw(http.MethodPut, blobURL(ch, sum[:]), tok, "application/octet-stream", payload); status != http.StatusCreated {
		t.Fatal("PUT failed")
	}
	if status, _ := e.Do(http.MethodDelete, blobURL(ch, sum[:]), tok, nil); status != http.StatusNoContent {
		t.Fatal("DELETE failed")
	}
	row, err := e.Repo.GetBlob(t.Context(), sum[:])
	if err != nil || row.UnrefSince == nil || *row.UnrefSince != e.Clk.Now().Unix() {
		t.Fatalf("blobs row after the last DELETE = %+v (%v), want unref_since = now", row, err)
	}
	if _, err := e.Blobs.Stat(sum[:]); err != nil {
		t.Fatalf("the DELETE unlinked the file itself: %v", err)
	}
}

// A moderator is a non-uploader too. Without this case the rule above is
// untested, because the non-uploader below holds only @everyone.
func TestDeletionByAModeratorIsAlsoRefused(t *testing.T) {
	e, ch, tok := blobEnv(t)
	payload := []byte("mine")
	sum := sha256.Sum256(payload)
	if status, _ := e.DoRaw(http.MethodPut,
		"/v1/channels/"+ch.String()+"/blobs/"+hex.EncodeToString(sum[:]), tok,
		"application/octet-stream", payload); status != http.StatusCreated {
		t.Fatal("PUT failed")
	}
	modTok := newModerator(t, e, ch) // holds PermManageMessages in this channel
	status, body := e.Do(http.MethodDelete,
		"/v1/channels/"+ch.String()+"/blobs/"+hex.EncodeToString(sum[:]), modTok, nil)
	if status != http.StatusForbidden {
		t.Fatalf("DELETE by a moderator = %d, want 403", status)
	}
	if code := e.ErrCode(body); code != "E_NOT_UPLOADER" {
		t.Fatalf("code = %s, want E_NOT_UPLOADER", code)
	}
	if n, _ := e.Repo.CountBlobRefs(t.Context(), sum[:]); n != 1 {
		t.Fatalf("the reference count moved to %d", n)
	}
}

func TestDeletionByANonUploaderIsRefused(t *testing.T) {
	e, ch, tok := blobEnv(t)
	payload := []byte("mine")
	sum := sha256.Sum256(payload)
	if status, _ := e.DoRaw(http.MethodPut,
		"/v1/channels/"+ch.String()+"/blobs/"+hex.EncodeToString(sum[:]), tok,
		"application/octet-stream", payload); status != http.StatusCreated {
		t.Fatal("PUT failed")
	}
	_, otherTok := e.NewUser("other")
	joinChannel(t, e, ch, otherTok)
	status, body := e.Do(http.MethodDelete,
		"/v1/channels/"+ch.String()+"/blobs/"+hex.EncodeToString(sum[:]), otherTok, nil)
	if status != http.StatusForbidden {
		t.Fatalf("DELETE by a non-uploader = %d", status)
	}
	if code := e.ErrCode(body); code != "E_NOT_UPLOADER" {
		t.Fatalf("code = %s, want E_NOT_UPLOADER", code)
	}
}

// The uploader is a user, not a device: another of the uploader's devices may
// delete (R34, "from any of their devices").
func TestTheUploadersOtherDeviceMayDelete(t *testing.T) {
	e, ch, tok := blobEnv(t)
	payload := []byte("from my laptop")
	sum := sha256.Sum256(payload)
	if status, _ := e.DoRaw(http.MethodPut, blobURL(ch, sum[:]), tok, "application/octet-stream", payload); status != http.StatusCreated {
		t.Fatal("PUT failed")
	}
	user := userOf(t, e, tok)
	phone := seedDevices(t, e, user, 1)[0]
	phoneTok := "phone-" + user.String()
	e.sess[phoneTok] = sessionFor(user, phone)
	if status, body := e.Do(http.MethodDelete, blobURL(ch, sum[:]), phoneTok, nil); status != http.StatusNoContent {
		t.Fatalf("DELETE from the uploader's other device = %d (%x)", status, body)
	}
	if n, _ := e.Repo.CountBlobRefs(t.Context(), sum[:]); n != 0 {
		t.Fatalf("references = %d, want 0", n)
	}
}

// A caller who cannot view the channel gets 404 on DELETE as on every verb.
func TestADeleteByANonMemberIs404(t *testing.T) {
	e, ch, tok := blobEnv(t)
	payload := []byte("hidden")
	sum := sha256.Sum256(payload)
	if status, _ := e.DoRaw(http.MethodPut, blobURL(ch, sum[:]), tok, "application/octet-stream", payload); status != http.StatusCreated {
		t.Fatal("PUT failed")
	}
	_, stranger := e.NewUser("stranger")
	if status, _ := e.Do(http.MethodDelete, blobURL(ch, sum[:]), stranger, nil); status != http.StatusNotFound {
		t.Fatalf("DELETE by a non-member = %d, want 404", status)
	}
	if n, _ := e.Repo.CountBlobRefs(t.Context(), sum[:]); n != 1 {
		t.Fatalf("references = %d, want 1", n)
	}
}

func TestATombstonedBlobCannotBeReuploaded(t *testing.T) {
	e, ch, tok := blobEnv(t)
	adminTok := e.NewInstanceAdmin("root")
	payload := []byte("takedown")
	sum := sha256.Sum256(payload)
	hexID := hex.EncodeToString(sum[:])
	if status, _ := e.DoRaw(http.MethodPut, "/v1/channels/"+ch.String()+"/blobs/"+hexID, tok,
		"application/octet-stream", payload); status != http.StatusCreated {
		t.Fatal("PUT failed")
	}
	if status, _ := e.Do(http.MethodDelete, "/v1/admin/blobs/"+hexID, adminTok,
		[]any{"court order 42"}); status != http.StatusNoContent {
		t.Fatal("admin purge failed")
	}
	if n, _ := e.Repo.CountBlobRefs(t.Context(), sum[:]); n != 0 {
		t.Fatal("the purge left a reference")
	}
	if _, err := e.Blobs.Stat(sum[:]); err == nil {
		t.Fatal("the purge did not unlink the file")
	}
	status, body := e.DoRaw(http.MethodPut, "/v1/channels/"+ch.String()+"/blobs/"+hexID, tok,
		"application/octet-stream", payload)
	if status != http.StatusGone {
		t.Fatalf("re-PUT of purged bytes = %d, want 410", status)
	}
	if code := e.ErrCode(body); code != "E_PRUNED" {
		t.Fatalf("code = %s", code)
	}
	if status, _ := e.Do(http.MethodGet, "/v1/channels/"+ch.String()+"/blobs/"+hexID, tok, nil); status != http.StatusGone {
		t.Fatal("GET of purged bytes was not 410")
	}
	// The purge is audited, naming the admin and the reason.
	rows, _ := e.Repo.ListAudit(t.Context(), 0, 50)
	var found bool
	for _, r := range rows {
		if r.Action == "blob.purge" && r.Target == hexID && r.Detail == "court order 42" &&
			r.Actor != nil && *r.Actor == userOf(t, e, adminTok) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no audit row for the purge: %+v", rows)
	}
	// A second purge of the same bytes is harmless.
	if status, _ := e.Do(http.MethodDelete, "/v1/admin/blobs/"+hexID, adminTok,
		[]any{"again"}); status != http.StatusNoContent {
		t.Fatal("a repeated purge was not 204")
	}
}

// A purge of bytes the instance does not hold yet still writes the tombstone:
// the operator's hash blocklist (gap-47 §7.3).
func TestAPurgeOfUnknownBytesBlocksTheirUpload(t *testing.T) {
	e, ch, tok := blobEnv(t)
	adminTok := e.NewInstanceAdmin("root")
	payload := []byte("never uploaded")
	sum := sha256.Sum256(payload)
	if status, body := e.Do(http.MethodDelete, "/v1/admin/blobs/"+hex.EncodeToString(sum[:]), adminTok,
		[]any{"blocklist"}); status != http.StatusNoContent {
		t.Fatalf("purge of unknown bytes = %d (%x)", status, body)
	}
	if status, _ := e.DoRaw(http.MethodPut, blobURL(ch, sum[:]), tok, "application/octet-stream", payload); status != http.StatusGone {
		t.Fatalf("PUT of blocklisted bytes = %d, want 410", status)
	}
}

func TestAPurgeByANonAdminIsRefused(t *testing.T) {
	e, ch, tok := blobEnv(t)
	payload := []byte("not yours")
	sum := sha256.Sum256(payload)
	hexID := hex.EncodeToString(sum[:])
	if status, _ := e.DoRaw(http.MethodPut, "/v1/channels/"+ch.String()+"/blobs/"+hexID, tok,
		"application/octet-stream", payload); status != http.StatusCreated {
		t.Fatal("PUT failed")
	}
	status, body := e.Do(http.MethodDelete, "/v1/admin/blobs/"+hexID, tok, []any{"because"})
	if status != http.StatusForbidden {
		t.Fatalf("purge by a non-admin = %d", status)
	}
	if code := e.ErrCode(body); code != "E_FORBIDDEN" {
		t.Fatalf("code = %s", code)
	}
	if n, _ := e.Repo.CountBlobRefs(t.Context(), sum[:]); n != 1 {
		t.Fatal("a refused purge dropped a reference")
	}
	if tomb, _ := e.Repo.GetBlobTombstone(t.Context(), sum[:]); tomb {
		t.Fatal("a refused purge wrote a tombstone")
	}
}

// Every admin route refuses a caller with no session, and a purge needs a
// reason an auditor can read.
func TestAdminRouteGates(t *testing.T) {
	e, _, _ := blobEnv(t)
	adminTok := e.NewInstanceAdmin("root")
	hexID := hex.EncodeToString(make([]byte, 32))
	if status, body := e.Do(http.MethodDelete, "/v1/admin/blobs/"+hexID, "", []any{"x"}); status != http.StatusUnauthorized ||
		e.ErrCode(body) != "E_UNAUTHENTICATED" {
		t.Fatalf("purge without a session = %d", status)
	}
	if status, _ := e.Do(http.MethodGet, "/v1/admin/audit", "", nil); status != http.StatusUnauthorized {
		t.Fatalf("audit without a session = %d", status)
	}
	if status, body := e.Do(http.MethodDelete, "/v1/admin/blobs/"+hexID, adminTok, []any{""}); status != http.StatusBadRequest ||
		e.ErrCode(body) != "E_INVALID_REQUEST" {
		t.Fatalf("purge with an empty reason = %d", status)
	}
	long := make([]byte, 1025)
	for i := range long {
		long[i] = 'a'
	}
	if status, _ := e.Do(http.MethodDelete, "/v1/admin/blobs/"+hexID, adminTok, []any{string(long)}); status != http.StatusBadRequest {
		t.Fatalf("purge with a 1025-byte reason = %d", status)
	}
	if status, _ := e.Do(http.MethodDelete, "/v1/admin/blobs/zz", adminTok, []any{"x"}); status != http.StatusBadRequest {
		t.Fatalf("purge of a malformed id = %d", status)
	}
}

type auditItem struct {
	_      struct{} `cbor:",toarray"`
	Actor  *id.ID
	Action string
	Target string
	Detail string
	At     uint64
}

func TestAuditIsAdminOnlyAndListsNewestFirst(t *testing.T) {
	e, _, tok := blobEnv(t)
	adminTok := e.NewInstanceAdmin("root")
	admin := userOf(t, e, adminTok)
	now := e.Clk.Now().Unix()
	for i, action := range []string{"first", "second", "third"} {
		if err := e.Repo.Audit(t.Context(), store.AuditRow{
			Actor: &admin, Action: action, Target: "t", Detail: "d", At: now + int64(i),
		}); err != nil {
			t.Fatalf("Audit: %v", err)
		}
	}
	if status, body := e.Do(http.MethodGet, "/v1/admin/audit", tok, nil); status != http.StatusForbidden ||
		e.ErrCode(body) != "E_FORBIDDEN" {
		t.Fatalf("audit by a non-admin = %d", status)
	}
	status, body := e.Do(http.MethodGet, "/v1/admin/audit?limit=2", adminTok, nil)
	if status != http.StatusOK {
		t.Fatalf("audit = %d (%x)", status, body)
	}
	var items []auditItem
	if err := cborx.Unmarshal(body, &items); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(items) != 2 || items[0].Action != "third" || items[1].Action != "second" ||
		items[0].Actor == nil || *items[0].Actor != admin || items[0].At != uint64(now+2) {
		t.Fatalf("audit page = %+v", items)
	}
	status, body = e.Do(http.MethodGet, "/v1/admin/audit?since="+strconv.FormatInt(now+2, 10), adminTok, nil)
	if status != http.StatusOK {
		t.Fatalf("audit since = %d", status)
	}
	items = nil
	if err := cborx.Unmarshal(body, &items); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(items) != 1 || items[0].Action != "third" {
		t.Fatalf("audit since now+2 = %+v", items)
	}
	// An empty log is an empty array, never null.
	_, body = e.Do(http.MethodGet, "/v1/admin/audit?since="+strconv.FormatInt(now+100, 10), adminTok, nil)
	var raw []cbor.RawMessage
	if err := cborx.Unmarshal(body, &raw); err != nil || raw == nil || len(raw) != 0 {
		t.Fatalf("an empty audit page = %x (%v), want an empty array", body, err)
	}
}

// protocol §2.2 point 6: disabling a user ends every session of every device of
// theirs, in the same transaction that sets users.disabled_at.
func TestDisablingAUserEndsTheirSessions(t *testing.T) {
	e, _, tok := blobEnv(t)
	adminTok := e.NewInstanceAdmin("root")
	victim, victimTok := e.NewUser("victim")
	now := e.Clk.Now().Unix()
	dev := e.sess[victimTok].DeviceID
	for i := range 2 {
		if err := e.Repo.CreateSession(t.Context(), store.SessionRow{
			TokenHash: sha256Of("victim-" + strconv.Itoa(i)), UserID: victim, DeviceID: dev,
			Scope: 1, Created: now, Expires: now + 3600, IdleExpires: now + 3600,
		}); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
	}
	path := "/v1/admin/users/" + victim.String() + "/disable"
	if status, body := e.Do(http.MethodPost, path, tok, []any{uint64(1)}); status != http.StatusForbidden ||
		e.ErrCode(body) != "E_FORBIDDEN" {
		t.Fatalf("disable by a non-admin = %d", status)
	}
	if status, body := e.Do(http.MethodPost, path, adminTok, []any{uint64(1)}); status != http.StatusNoContent {
		t.Fatalf("disable = %d (%x)", status, body)
	}
	u, err := e.Repo.GetUser(t.Context(), victim)
	if err != nil || u.DisabledAt == nil || *u.DisabledAt != now {
		t.Fatalf("disabled_at = %v (%v), want %d", u.DisabledAt, err, now)
	}
	if n, _ := e.Repo.CountSessionsByDevice(t.Context(), dev); n != 0 {
		t.Fatalf("%d sessions survived the disable", n)
	}
	rows, _ := e.Repo.ListAudit(t.Context(), 0, 50)
	var audited bool
	for _, r := range rows {
		if r.Action == "user.disable" && r.Target == victim.String() {
			audited = true
		}
	}
	if !audited {
		t.Fatalf("no audit row for the disable: %+v", rows)
	}
	// [0] enables the account again.
	if status, _ := e.Do(http.MethodPost, path, adminTok, []any{uint64(0)}); status != http.StatusNoContent {
		t.Fatalf("enable = %d", status)
	}
	if u, _ := e.Repo.GetUser(t.Context(), victim); u.DisabledAt != nil {
		t.Fatalf("disabled_at after enable = %d", *u.DisabledAt)
	}
	if status, _ := e.Do(http.MethodPost, path, adminTok, []any{uint64(2)}); status != http.StatusBadRequest {
		t.Fatalf("disable with [2] = %d, want 400", status)
	}
	if status, _ := e.Do(http.MethodPost, "/v1/admin/users/"+id.New().String()+"/disable", adminTok,
		[]any{uint64(1)}); status != http.StatusNotFound {
		t.Fatalf("disable of an unknown user = %d, want 404", status)
	}
}

// sessionFor is the enrolled session a bearer token of the test middleware maps
// to.
func sessionFor(user, device id.ID) auth.Session {
	return auth.Session{UserID: user, DeviceID: device, Scope: auth.ScopeEnrolled}
}

func sha256Of(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// The purge is counted in dilla_blob_purges_total when the admin routes carry
// metrics; a refused purge is not.
func TestAPurgeIsCounted(t *testing.T) {
	e, _, tok := channelEnv(t)
	bs, err := blob.Open(t.TempDir(), "fs")
	if err != nil {
		t.Fatalf("blob.Open: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })
	reg := prometheus.NewRegistry()
	m := obs.NewMetrics(reg, reg)
	api.NewAdmin(e.Repo, bs, e.Clk, slog.New(slog.DiscardHandler)).WithMetrics(m).Register(e.Mux)
	adminTok := e.NewInstanceAdmin("root")
	path := "/v1/admin/blobs/" + hex.EncodeToString(sha256Of("counted"))
	if status, _ := e.Do(http.MethodDelete, path, tok, []any{"no"}); status != http.StatusForbidden {
		t.Fatalf("purge by a non-admin = %d", status)
	}
	if status, _ := e.Do(http.MethodDelete, path, adminTok, []any{"yes"}); status != http.StatusNoContent {
		t.Fatalf("purge = %d", status)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var got float64
	for _, f := range families {
		if f.GetName() == "dilla_blob_purges_total" {
			got = f.GetMetric()[0].GetCounter().GetValue()
		}
	}
	if got != 1 {
		t.Fatalf("dilla_blob_purges_total = %v, want 1", got)
	}
}

type diagnosticsLeg struct {
	_      struct{} `cbor:",toarray"`
	Name   string
	Status uint64
	Detail string
	Fix    string
}

// GET /v1/admin/diagnostics answers the doctor report the composition root runs (P2-5):
// [[name, status, detail, fix]] in leg order, status 0 OK, 1 WARN, 2 FAIL. It is instance-admin
// only, and an Admin built without a report answers 501 rather than an empty report that would
// read as "nothing checked, nothing wrong".
func TestDiagnosticsAnswersTheDoctorReportToAnInstanceAdmin(t *testing.T) {
	e, _, tok := channelEnv(t)
	bs, err := blob.Open(t.TempDir(), "fs")
	if err != nil {
		t.Fatalf("blob.Open: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })
	calls := 0
	api.NewAdmin(e.Repo, bs, e.Clk, slog.New(slog.DiscardHandler)).
		WithDiagnostics(func(context.Context) ops.Report {
			calls++
			return ops.Report{Legs: []ops.Leg{
				{Name: "database", Status: ops.Green, Detail: "schema version 12"},
				{Name: "blobs", Status: ops.Red, Detail: "1 missing file", Fix: "dillad admin blob purge"},
			}}
		}).Register(e.Mux)
	adminTok := e.NewInstanceAdmin("root")

	if status, body := e.Do(http.MethodGet, "/v1/admin/diagnostics", tok, nil); status != http.StatusForbidden ||
		e.ErrCode(body) != "E_FORBIDDEN" || calls != 0 {
		t.Fatalf("diagnostics for a non-admin = %d (%d runs), want 403 before the report runs", status, calls)
	}
	status, body := e.Do(http.MethodGet, "/v1/admin/diagnostics", adminTok, nil)
	if status != http.StatusOK {
		t.Fatalf("diagnostics = %d (%x)", status, body)
	}
	var legs []diagnosticsLeg
	if err := cborx.Unmarshal(body, &legs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(legs) != 2 || legs[0].Name != "database" || legs[0].Status != 0 || legs[0].Detail != "schema version 12" ||
		legs[1].Name != "blobs" || legs[1].Status != 2 || legs[1].Fix != "dillad admin blob purge" {
		t.Fatalf("diagnostics = %+v", legs)
	}

	bare := newEnv(t)
	api.NewAdmin(bare.Repo, bs, bare.Clk, slog.New(slog.DiscardHandler)).Register(bare.Mux)
	if status, _ := bare.Do(http.MethodGet, "/v1/admin/diagnostics", bare.NewInstanceAdmin("root"), nil); status != http.StatusNotImplemented {
		t.Fatalf("diagnostics with no report wired = %d, want 501", status)
	}
}
