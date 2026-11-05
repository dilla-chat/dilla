package api_test

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// rotatingKeys is a test api.FrankingKeys whose key set a test can rotate and
// shrink: the current key first, then the retained ones, as StaticFrankingKeys
// orders them.
type rotatingKeys struct {
	mu   sync.Mutex
	keys []api.FrankingKey
}

func newRotatingKeys() *rotatingKeys {
	k := &rotatingKeys{}
	k.Rotate()
	return k
}

func (k *rotatingKeys) Current() (id.ID, []byte) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.keys[0].ID, k.keys[0].Key
}

func (k *rotatingKeys) ByID(keyID id.ID) ([]byte, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, key := range k.keys {
		if key.ID == keyID {
			return key.Key, true
		}
	}
	return nil, false
}

func (k *rotatingKeys) All() []api.FrankingKey {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.keys)
}

// Rotate makes a fresh key current and keeps every earlier one.
func (k *rotatingKeys) Rotate() {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	k.mu.Lock()
	defer k.mu.Unlock()
	k.keys = append([]api.FrankingKey{{ID: id.New(), Key: key}}, k.keys...)
}

func (k *rotatingKeys) CurrentID() id.ID {
	keyID, _ := k.Current()
	return keyID
}

// Forget drops a retained key, as an operator who deleted it from the key
// history would.
func (k *rotatingKeys) Forget(keyID id.ID) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.keys = slices.DeleteFunc(k.keys, func(key api.FrankingKey) bool { return key.ID == keyID })
}

// reportEnv is the harness with the report routes mounted over a rotating key set.
type reportEnv struct {
	*env
	Keys *rotatingKeys
}

func newReportEnv(t *testing.T) *reportEnv {
	t.Helper()
	e := &reportEnv{env: newEnv(t), Keys: newRotatingKeys()}
	api.NewReports(e.Repo, e.Keys, e.Clk, slog.New(slog.DiscardHandler)).Register(e.Mux)
	return e
}

// frankedEnvelope is a type-0 envelope with the given body and k_f.
func frankedEnvelope(t *testing.T, body string, kf []byte) []byte {
	t.Helper()
	b, err := cborx.Marshal([]any{uint64(1), id.New(), uint64(0), nil, nil, body, []any{}, []any{}, kf})
	if err != nil {
		t.Fatalf("cborx.Marshal: %v", err)
	}
	return b
}

// seedFrankedMessage writes one mls_app_messages row exactly as the delivery
// service's upload path does (internal/ds/message.go): C from the envelope and
// k_f, T from the current instance key over (group, epoch, seq, uploader, C,
// recv_ts), and the id of that key beside it. It returns (group_id, seq,
// envelope, k_f).
func seedFrankedMessage(t *testing.T) (*reportEnv, id.ID, uint64, []byte, []byte) {
	t.Helper()
	e := newReportEnv(t)
	group, seq := seedGroupRow(t, e.env), uint64(7)
	kf := make([]byte, 32)
	_, _ = rand.Read(kf)
	envelope := frankedEnvelope(t, "the message someone will report", kf)
	putFranked(t, e, group, seq, 3, id.New(), envelope, kf)
	return e, group, seq, envelope, kf
}

// seedGroupRow writes one mls_groups row with no community: the franking tuple
// needs a group to hang off, nothing more.
func seedGroupRow(t *testing.T, e *env) id.ID {
	t.Helper()
	g := store.GroupRow{
		GroupID: id.New(), Binding: []byte{0x80}, Kind: 0, TargetID: id.New(),
		Ciphersuite: 1, ExternalSenderKeyID: id.New(), E2EEVersion: 1, MediaVersion: 1,
		PolicyVersion: 1, Created: e.Clk.Now().Unix(),
	}
	if err := e.Repo.CreateGroup(t.Context(), g); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	return g.GroupID
}

// putFranked stores the franked row for envelope under the current key.
func putFranked(t *testing.T, e *reportEnv, group id.ID, seq, epoch uint64, uploader id.ID, envelope, kf []byte) {
	t.Helper()
	c, err := api.Commitment(envelope, kf)
	if err != nil {
		t.Fatalf("Commitment: %v", err)
	}
	keyID, key := e.Keys.Current()
	recv := e.Clk.Now().Unix()
	if err := e.Repo.PutAppMessage(t.Context(), store.AppMessageRow{
		GroupID: group, Seq: seq, Epoch: epoch, UploaderDevice: uploader,
		Blob: []byte("ciphertext"), CommitmentC: c,
		FrankingTag: api.Tag(key, group, epoch, seq, uploader, c, recv),
		Size:        10, Created: recv, FrankingKeyID: keyID,
	}); err != nil {
		t.Fatalf("PutAppMessage: %v", err)
	}
}

// forgeBody re-encodes envelope with element 5, the body, replaced.
func forgeBody(t *testing.T, envelope []byte, body string) []byte {
	t.Helper()
	var raw []cbor.RawMessage
	mustUnmarshal(t, envelope, &raw)
	b, err := cborx.Marshal(body)
	if err != nil {
		t.Fatalf("cborx.Marshal: %v", err)
	}
	raw[5] = b
	out, err := cborx.Marshal(raw)
	if err != nil {
		t.Fatalf("cborx.Marshal: %v", err)
	}
	return out
}

// report files one report and returns the stored row, failing on anything but 201.
func (e *reportEnv) report(t *testing.T, tok string, group id.ID, seq uint64, envelope, kf []byte) store.ReportRow {
	t.Helper()
	status, body := e.Do(http.MethodPost, "/v1/reports", tok, []any{group, seq, envelope, kf})
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/reports = %d (%x)", status, body)
	}
	var out []cbor.RawMessage
	mustUnmarshalBody(t, body, &out)
	if len(out) != 2 {
		t.Fatalf("POST /v1/reports answered %d elements, want [report_id, verification_result]", len(out))
	}
	var reportID id.ID
	mustUnmarshal(t, out[0], &reportID)
	var result string
	mustUnmarshal(t, out[1], &result)
	row, err := e.Repo.GetReport(t.Context(), reportID)
	if err != nil {
		t.Fatalf("GetReport: %v", err)
	}
	if row.VerificationResult != result {
		t.Fatalf("answered %q but stored %q", result, row.VerificationResult)
	}
	return row
}

func mustID(t *testing.T, s string) id.ID {
	t.Helper()
	v, err := id.Parse(s)
	if err != nil {
		t.Fatalf("id %q: %v", s, err)
	}
	return v
}

func TestAReportVerifiesOnlyWhenBothEquationsHold(t *testing.T) {
	e, group, seq, envelope, kf := seedFrankedMessage(t)
	reporter, reporterTok := e.NewUser("reporter")

	status, body := e.Do(http.MethodPost, "/v1/reports", reporterTok,
		[]any{group, seq, envelope, kf})
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/reports = %d (%x)", status, body)
	}
	var out []cbor.RawMessage
	mustUnmarshalBody(t, body, &out)
	var reportID id.ID
	mustUnmarshal(t, out[0], &reportID)
	row, err := e.Repo.GetReport(t.Context(), reportID)
	if err != nil {
		t.Fatalf("GetReport: %v", err)
	}
	if row.VerificationResult != "verified" {
		t.Fatalf("verification_result = %q", row.VerificationResult)
	}
	if row.Reporter != reporter || row.GroupID != group || row.Seq != seq || row.Status != 0 ||
		!bytes.Equal(row.RevealedEnvelope, envelope) || !bytes.Equal(row.KF, kf) ||
		row.Created != e.Clk.Now().Unix() {
		t.Fatalf("stored report = %+v", row)
	}

	// A forged envelope fails the commitment equation.
	forged := forgeBody(t, envelope, "something else entirely")
	status, body = e.Do(http.MethodPost, "/v1/reports", reporterTok, []any{group, seq, forged, kf})
	if status != http.StatusCreated {
		t.Fatalf("a forged report must still be filed, got %d", status)
	}
	mustUnmarshalBody(t, body, &out)
	mustUnmarshal(t, out[0], &reportID)
	row, _ = e.Repo.GetReport(t.Context(), reportID)
	if row.VerificationResult != "commitment_mismatch" {
		t.Fatalf("a forged envelope was reported as %q", row.VerificationResult)
	}
	if !bytes.Equal(row.RevealedEnvelope, forged) {
		t.Fatal("a forged report does not keep exactly the envelope the reporter submitted")
	}

	// A wrong k_f fails the same equation, and a report naming a message the
	// instance never stored is 404.
	wrongKF := make([]byte, 32)
	status, body = e.Do(http.MethodPost, "/v1/reports", reporterTok, []any{group, seq, envelope, wrongKF})
	if status != http.StatusCreated {
		t.Fatalf("a wrong k_f must still be filed, got %d", status)
	}
	mustUnmarshalBody(t, body, &out)
	mustUnmarshal(t, out[0], &reportID)
	row, _ = e.Repo.GetReport(t.Context(), reportID)
	if row.VerificationResult != "commitment_mismatch" {
		t.Fatalf("a wrong k_f was reported as %q", row.VerificationResult)
	}
	if status, _ := e.Do(http.MethodPost, "/v1/reports", reporterTok,
		[]any{group, seq + 9999, envelope, kf}); status != http.StatusNotFound {
		t.Fatal("a report against an unknown seq was not 404")
	}
	if status, _ := e.Do(http.MethodPost, "/v1/reports", reporterTok,
		[]any{id.New(), seq, envelope, kf}); status != http.StatusNotFound {
		t.Fatal("a report against an unknown group was not 404")
	}
}

// The tag equation is checked on its own: a stored tag that does not match the
// recomputed one is tag_mismatch even when the commitment matches.
func TestATagThatDoesNotMatchIsATagMismatch(t *testing.T) {
	e := newReportEnv(t)
	group := seedGroupRow(t, e.env)
	kf := bytes.Repeat([]byte{0x41}, 32)
	envelope := frankedEnvelope(t, "tag", kf)
	c, err := api.Commitment(envelope, kf)
	if err != nil {
		t.Fatalf("Commitment: %v", err)
	}
	keyID, key := e.Keys.Current()
	uploader := id.New()
	// The tag was made over another uploader: the instance's stored tuple
	// disagrees with the tag, which no honest upload produces.
	if err := e.Repo.PutAppMessage(t.Context(), store.AppMessageRow{
		GroupID: group, Seq: 1, Epoch: 1, UploaderDevice: uploader, Blob: []byte("x"), CommitmentC: c,
		FrankingTag: api.Tag(key, group, 1, 1, id.New(), c, e.Clk.Now().Unix()),
		Size:        1, Created: e.Clk.Now().Unix(), FrankingKeyID: keyID,
	}); err != nil {
		t.Fatalf("PutAppMessage: %v", err)
	}
	_, tok := e.NewUser("reporter")
	if got := e.report(t, tok, group, 1, envelope, kf).VerificationResult; got != "tag_mismatch" {
		t.Fatalf("verification_result = %q, want tag_mismatch", got)
	}
}

// A message with no stored commitment and an envelope that is not an envelope
// are both filed, each with its own honest result.
func TestAMalformedEnvelopeAndAMissingCommitmentAreRecordedNotRefused(t *testing.T) {
	e, group, seq, _, kf := seedFrankedMessage(t)
	_, tok := e.NewUser("reporter")
	if got := e.report(t, tok, group, seq, []byte{0x01}, kf).VerificationResult; got != "envelope_malformed" {
		t.Fatalf("a non-envelope verified as %q", got)
	}
	if got := e.report(t, tok, group, seq, []byte{}, kf).VerificationResult; got != "envelope_malformed" {
		t.Fatalf("an empty envelope verified as %q", got)
	}

	keyID, key := e.Keys.Current()
	if err := e.Repo.PutAppMessage(t.Context(), store.AppMessageRow{
		GroupID: group, Seq: 8, Epoch: 1, UploaderDevice: id.New(), Blob: []byte("x"),
		FrankingTag: api.Tag(key, group, 1, 8, id.New(), nil, 1), Size: 1, Created: 1, FrankingKeyID: keyID,
	}); err != nil {
		t.Fatalf("PutAppMessage: %v", err)
	}
	envelope := frankedEnvelope(t, "x", kf)
	if got := e.report(t, tok, group, 8, envelope, kf).VerificationResult; got != "no_commitment_stored" {
		t.Fatalf("a message with no stored commitment verified as %q", got)
	}
}

func TestAReportAgainstATombstonedMessageStillVerifies(t *testing.T) {
	e, group, seq, envelope, kf := seedFrankedMessage(t)
	if err := e.Repo.TombstoneAppMessage(t.Context(), group, seq, e.Clk.Now().Unix()); err != nil {
		t.Fatalf("TombstoneAppMessage: %v", err)
	}
	// The ciphertext is gone; the franking tuple is not, which is the whole
	// point of storing (seq, epoch, uploader_device, C, T, recv_ts) beside it.
	_, reporterTok := e.NewUser("reporter")
	status, body := e.Do(http.MethodPost, "/v1/reports", reporterTok, []any{group, seq, envelope, kf})
	if status != http.StatusCreated {
		t.Fatalf("report against a tombstone = %d", status)
	}
	var out []cbor.RawMessage
	mustUnmarshalBody(t, body, &out)
	var reportID id.ID
	mustUnmarshal(t, out[0], &reportID)
	row, _ := e.Repo.GetReport(t.Context(), reportID)
	if row.VerificationResult != "verified" {
		t.Fatalf("verification_result = %q", row.VerificationResult)
	}
}

func TestARotatedFrankingKeyIsSelectedByTheStoredID(t *testing.T) {
	e, group, seq, envelope, kf := seedFrankedMessage(t) // franked under key A
	keyA := e.Keys.CurrentID()
	e.Keys.Rotate() // key B becomes current; A is kept
	_, reporterTok := e.NewUser("reporter")
	status, body := e.Do(http.MethodPost, "/v1/reports", reporterTok, []any{group, seq, envelope, kf})
	if status != http.StatusCreated {
		t.Fatalf("report after rotation = %d", status)
	}
	var out []cbor.RawMessage
	mustUnmarshalBody(t, body, &out)
	var reportID id.ID
	mustUnmarshal(t, out[0], &reportID)
	row, _ := e.Repo.GetReport(t.Context(), reportID)
	if row.VerificationResult != "verified" {
		t.Fatalf("a message franked under a rotated key verified as %q", row.VerificationResult)
	}
	if row.FrankingKeyID == e.Keys.CurrentID() {
		t.Fatal("the report recorded the current key id rather than the one the message was franked with")
	}
	if row.FrankingKeyID != keyA {
		t.Fatalf("the report recorded key %v, want the message's %v", row.FrankingKeyID, keyA)
	}
	// A key the instance no longer holds is a distinct, honest outcome.
	e.Keys.Forget(row.FrankingKeyID)
	status, body = e.Do(http.MethodPost, "/v1/reports", reporterTok, []any{group, seq, envelope, kf})
	if status != http.StatusCreated {
		t.Fatalf("report after the key was forgotten = %d", status)
	}
	mustUnmarshalBody(t, body, &out)
	mustUnmarshal(t, out[0], &reportID)
	row, _ = e.Repo.GetReport(t.Context(), reportID)
	if row.VerificationResult != "franking_key_unavailable" {
		t.Fatalf("verification_result = %q", row.VerificationResult)
	}
}

// P2-D21: a message stored before franking_key_id existed carries the all-zero
// id. The verifier tries every retained key, current first, and says
// franking_key_unknown only when none of them made the tag.
func TestAMessageFrankedBeforeTheKeyIDExistedTriesEveryRetainedKey(t *testing.T) {
	e := newReportEnv(t)
	group := seedGroupRow(t, e.env)
	kf := bytes.Repeat([]byte{0x52}, 32)
	envelope := frankedEnvelope(t, "from before the upgrade", kf)
	c, err := api.Commitment(envelope, kf)
	if err != nil {
		t.Fatalf("Commitment: %v", err)
	}
	_, oldKey := e.Keys.Current()
	uploader, now := id.New(), e.Clk.Now().Unix()
	if err := e.Repo.PutAppMessage(t.Context(), store.AppMessageRow{
		GroupID: group, Seq: 1, Epoch: 2, UploaderDevice: uploader, Blob: []byte("x"), CommitmentC: c,
		FrankingTag: api.Tag(oldKey, group, 2, 1, uploader, c, now), Size: 1, Created: now,
		// FrankingKeyID left zero: what the migration's default writes.
	}); err != nil {
		t.Fatalf("PutAppMessage: %v", err)
	}
	e.Keys.Rotate()
	_, tok := e.NewUser("reporter")
	row := e.report(t, tok, group, 1, envelope, kf)
	if row.VerificationResult != "verified" {
		t.Fatalf("a pre-migration message franked under a retained key verified as %q", row.VerificationResult)
	}
	if row.FrankingKeyID != (id.ID{}) {
		t.Fatalf("the report recorded key %v; the message names none", row.FrankingKeyID)
	}
	for _, k := range e.Keys.All() {
		e.Keys.Forget(k.ID)
	}
	e.Keys.Rotate()
	if got := e.report(t, tok, group, 1, envelope, kf).VerificationResult; got != "franking_key_unknown" {
		t.Fatalf("with no retained key that made the tag, verification_result = %q", got)
	}
}

func TestTheReportViewShowsExactlyTheSubmittedEnvelope(t *testing.T) {
	e, group, seq, envelope, kf := seedFrankedMessage(t)
	reporter, reporterTok := e.NewUser("reporter")
	if status, _ := e.Do(http.MethodPost, "/v1/reports", reporterTok, []any{group, seq, envelope, kf}); status != http.StatusCreated {
		t.Fatal("POST /v1/reports failed")
	}
	adminTok := e.NewInstanceAdmin("root")
	status, body := e.Do(http.MethodGet, "/v1/reports", adminTok, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/reports = %d", status)
	}
	var rows [][]cbor.RawMessage
	mustUnmarshalBody(t, body, &rows)
	if len(rows) != 1 {
		t.Fatalf("reports = %d", len(rows))
	}
	var shown []byte
	mustUnmarshal(t, rows[0][4], &shown)
	if !bytes.Equal(shown, envelope) {
		t.Fatal("the report view does not show exactly the submitted envelope")
	}
	// It shows nothing else about the group: no other message, no member list.
	if len(rows[0]) != 8 {
		t.Fatalf("a report row has %d elements; the shape is fixed at 8", len(rows[0]))
	}
	var gotReporter, gotGroup id.ID
	var gotSeq, gotStatus, gotCreated uint64
	var gotResult string
	mustUnmarshal(t, rows[0][1], &gotReporter)
	mustUnmarshal(t, rows[0][2], &gotGroup)
	mustUnmarshal(t, rows[0][3], &gotSeq)
	mustUnmarshal(t, rows[0][5], &gotResult)
	mustUnmarshal(t, rows[0][6], &gotStatus)
	mustUnmarshal(t, rows[0][7], &gotCreated)
	if gotReporter != reporter || gotGroup != group || gotSeq != seq || gotResult != "verified" ||
		gotStatus != 0 || gotCreated != uint64(e.Clk.Now().Unix()) {
		t.Fatalf("report row = %v %v %d %q %d %d", gotReporter, gotGroup, gotSeq, gotResult, gotStatus, gotCreated)
	}
	// A non-admin cannot read the queue at all.
	if status, _ := e.Do(http.MethodGet, "/v1/reports", reporterTok, nil); status != http.StatusForbidden {
		t.Fatal("a non-admin read the report queue")
	}
}

// The queue is newest first and bounded by limit.
func TestTheReportQueueIsNewestFirstAndBounded(t *testing.T) {
	e, group, seq, envelope, kf := seedFrankedMessage(t)
	_, tok := e.NewUser("reporter")
	var ids []id.ID
	for range 3 {
		ids = append(ids, e.report(t, tok, group, seq, envelope, kf).ID)
		e.Clk.Advance(time.Second)
	}
	adminTok := e.NewInstanceAdmin("root")
	status, body := e.Do(http.MethodGet, "/v1/reports?limit=2", adminTok, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/reports = %d", status)
	}
	var rows [][]cbor.RawMessage
	mustUnmarshalBody(t, body, &rows)
	if len(rows) != 2 {
		t.Fatalf("limit=2 answered %d rows", len(rows))
	}
	for i, want := range []id.ID{ids[2], ids[1]} {
		var got id.ID
		mustUnmarshal(t, rows[i][0], &got)
		if got != want {
			t.Fatalf("row %d is %v, want %v (newest first)", i, got, want)
		}
	}
}

// PATCH moves the status, keeps the instance's own verification result (a
// moderator cannot relabel a forgery "verified") and writes an audit row.
func TestPatchingAReportMovesItsStatusAndIsAudited(t *testing.T) {
	e, group, seq, envelope, kf := seedFrankedMessage(t)
	_, tok := e.NewUser("reporter")
	forged := e.report(t, tok, group, seq, forgeBody(t, envelope, "forged"), kf)
	adminTok := e.NewInstanceAdmin("root")
	path := "/v1/reports/" + forged.ID.String()

	if status, _ := e.Do(http.MethodPatch, path, tok, []any{uint64(1), "resolved"}); status != http.StatusForbidden {
		t.Fatalf("a non-admin PATCH = %d, want 403", status)
	}
	if status, body := e.Do(http.MethodPatch, path, adminTok, []any{uint64(2), "not what it claims"}); status != http.StatusNoContent {
		t.Fatalf("PATCH = %d (%x)", status, body)
	}
	row, err := e.Repo.GetReport(t.Context(), forged.ID)
	if err != nil {
		t.Fatalf("GetReport: %v", err)
	}
	if row.Status != 2 {
		t.Fatalf("status = %d, want 2", row.Status)
	}
	if row.VerificationResult != "commitment_mismatch" {
		t.Fatalf("PATCH rewrote the verification result to %q", row.VerificationResult)
	}
	audit, err := e.Repo.ListAudit(t.Context(), 0, 10)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(audit) != 1 || audit[0].Action != "report.dismiss" || audit[0].Target != forged.ID.String() ||
		audit[0].Detail != "not what it claims" || audit[0].Actor == nil {
		t.Fatalf("audit = %+v", audit)
	}

	for name, body := range map[string][]any{
		"status out of range": {uint64(3), ""},
		"NUL in result":       {uint64(1), "a\x00b"},
		"result too long":     {uint64(1), string(bytes.Repeat([]byte{'r'}, 1025))},
	} {
		if status, _ := e.Do(http.MethodPatch, path, adminTok, body); status != http.StatusBadRequest {
			t.Errorf("%s: PATCH = %d, want 400", name, status)
		}
	}
	if status, _ := e.Do(http.MethodPatch, "/v1/reports/"+id.New().String(), adminTok, []any{uint64(1), ""}); status != http.StatusNotFound {
		t.Fatalf("PATCH of an unknown report = %d, want 404", status)
	}
	if status, _ := e.Do(http.MethodPatch, "/v1/reports/nope", adminTok, []any{uint64(1), ""}); status != http.StatusBadRequest {
		t.Fatalf("PATCH of a malformed id = %d, want 400", status)
	}
}

func TestAReportIsRefusedOnlyForAMalformedRequest(t *testing.T) {
	e, group, seq, envelope, kf := seedFrankedMessage(t)
	_, tok := e.NewUser("reporter")
	if status, _ := e.Do(http.MethodPost, "/v1/reports", "", []any{group, seq, envelope, kf}); status != http.StatusUnauthorized {
		t.Fatalf("an anonymous report = %d, want 401", status)
	}
	for name, body := range map[string][]any{
		"k_f of 31 bytes":  {group, seq, envelope, kf[:31]},
		"k_f of 33 bytes":  {group, seq, envelope, append(slices.Clone(kf), 0)},
		"three elements":   {group, seq, envelope},
		"short group id":   {group[:15], seq, envelope, kf},
		"envelope as text": {group, seq, "envelope", kf},
	} {
		if status, _ := e.Do(http.MethodPost, "/v1/reports", tok, body); status != http.StatusBadRequest {
			t.Errorf("%s: POST = %d, want 400", name, status)
		}
	}
	if status, _ := e.Do(http.MethodGet, "/v1/reports", "", nil); status != http.StatusUnauthorized {
		t.Fatalf("an anonymous GET = %d, want 401", status)
	}
}

// A server-readable channel's message is reportable by its channel id: the
// franking tuple lives in readable_messages, with channel_id in the group_id
// slot and epoch 0 (protocol/09 § Readable channels). An edit re-franks, so the
// edited envelope verifies and the original no longer does; a delete keeps the
// tuple.
func TestAReadableChannelMessageVerifies(t *testing.T) {
	f := newReadableFixture(t)
	e := f.e
	api.NewReports(e.Repo, f.keys, e.Clk, slog.New(slog.DiscardHandler)).Register(e.Mux)
	ch := readableChannel(t, e, f.cid, f.ownerTok)
	kf := bytes.Repeat([]byte{0x06}, 32)
	original := envelope0(t, "a readable message")
	seq := postReadable(t, e, ch, f.ownerTok, original)

	re := &reportEnv{env: e}
	_, tok := e.NewUser("reporter")
	row := re.report(t, tok, ch, seq, original, kf)
	if row.VerificationResult != "verified" {
		t.Fatalf("a readable message verified as %q", row.VerificationResult)
	}
	keyID, _ := f.keys.Current()
	if row.FrankingKeyID != keyID || row.GroupID != ch {
		t.Fatalf("report = %+v, want key %v and the channel id in group_id", row, keyID)
	}
	if got := re.report(t, tok, ch, seq, forgeBody(t, original, "forged"), kf).VerificationResult; got != "commitment_mismatch" {
		t.Fatalf("a forged readable report verified as %q", got)
	}

	// The edit re-franks at the edit's time, by the editing device.
	e.Clk.Advance(time.Minute)
	edited := envelopeOf(t, 1, "a readable message, edited")
	if status, body := e.Do(http.MethodPatch, "/v1/channels/"+ch.String()+"/messages/1", f.ownerTok, []any{edited}); status != http.StatusNoContent {
		t.Fatalf("PATCH = %d (%x)", status, body)
	}
	if got := re.report(t, tok, ch, seq, edited, kf).VerificationResult; got != "verified" {
		t.Fatalf("an edited readable message verified as %q", got)
	}
	if got := re.report(t, tok, ch, seq, original, kf).VerificationResult; got != "commitment_mismatch" {
		t.Fatalf("the replaced envelope verified as %q", got)
	}

	// A delete empties the envelope; the franking tuple survives.
	if status, body := e.Do(http.MethodDelete, "/v1/channels/"+ch.String()+"/messages/1", f.ownerTok, nil); status != http.StatusNoContent {
		t.Fatalf("DELETE = %d (%x)", status, body)
	}
	if got := re.report(t, tok, ch, seq, edited, kf).VerificationResult; got != "verified" {
		t.Fatalf("a deleted readable message verified as %q", got)
	}
	if status, _ := e.Do(http.MethodPost, "/v1/reports", tok, []any{ch, seq + 1, edited, kf}); status != http.StatusNotFound {
		t.Fatalf("a report against an unknown readable seq = %d, want 404", status)
	}
}

// The franking equations are a cross-implementation contract (protocol/04):
// the committed corpus that the TypeScript reference generator writes, and the
// Rust core and the delivery service replay, must come out of the verifier's
// Commitment and Tag byte for byte.
func TestTheFrankingVectorsMatch(t *testing.T) {
	var env struct {
		Cases []struct {
			Name       string `json:"name"`
			CBOR       string `json:"cbor"`
			Commitment string `json:"commitment"`
			Envelope   struct {
				KF string `json:"kf"`
			} `json:"envelope"`
		} `json:"cases"`
	}
	readVectors(t, "envelope.json", &env)
	var doc frankingVectors
	readVectors(t, "franking.json", &doc)
	if len(env.Cases) == 0 || len(doc.Cases) == 0 {
		t.Fatal("the franking vector corpus is empty; the protocol-vectors generator has not run")
	}
	for _, v := range env.Cases {
		t.Run("C/"+v.Name, func(t *testing.T) {
			got, err := api.Commitment(mustHex(t, v.CBOR), mustHex(t, v.Envelope.KF))
			if err != nil {
				t.Fatalf("Commitment: %v", err)
			}
			if want := mustHex(t, v.Commitment); !bytes.Equal(got, want) {
				t.Fatalf("C = %x, want %x", got, want)
			}
		})
	}
	for _, v := range doc.Cases {
		t.Run(fmt.Sprintf("T/seq %d", v.Seq), func(t *testing.T) {
			got := api.Tag(mustHex(t, doc.InstanceFrankingKey), mustID(t, v.GroupID), v.Epoch, v.Seq,
				mustID(t, v.UploaderDevice), mustHex(t, v.Commitment), v.RecvTS)
			if want := mustHex(t, v.Tag); !bytes.Equal(got, want) {
				t.Fatalf("T = %x, want %x", got, want)
			}
		})
	}
}

// The corpus end to end: every franking.json case stored as the delivery
// service would store it, under the corpus's instance key, and reported with
// the corpus's envelope and k_f (0x06 x 32, protocol/vectors/README.md). The
// report path must say "verified" for each, which pins the verifier's choice of
// stored fields (recv_ts is created, the uploader is uploader_device) to the
// corpus rather than to the formula the handler happens to share with the tests.
func TestTheFrankingCorpusVerifiesThroughTheReportRoute(t *testing.T) {
	var doc frankingVectors
	readVectors(t, "franking.json", &doc)
	e := &reportEnv{env: newEnv(t)}
	keyID := id.New()
	keys := api.NewStaticFrankingKeys(api.FrankingKey{ID: keyID, Key: mustHex(t, doc.InstanceFrankingKey)})
	api.NewReports(e.Repo, keys, e.Clk, slog.New(slog.DiscardHandler)).Register(e.Mux)
	_, tok := e.NewUser("reporter")
	envelope := mustHex(t, doc.EnvelopeCBOR)
	kf := bytes.Repeat([]byte{0x06}, 32)
	created := map[id.ID]bool{}
	for _, v := range doc.Cases {
		group := mustID(t, v.GroupID)
		if !created[group] {
			g := store.GroupRow{
				GroupID: group, Binding: []byte{0x80}, TargetID: id.New(), Ciphersuite: 1,
				ExternalSenderKeyID: id.New(), E2EEVersion: 1, MediaVersion: 1, PolicyVersion: 1, Created: 1,
			}
			if err := e.Repo.CreateGroup(t.Context(), g); err != nil {
				t.Fatalf("CreateGroup: %v", err)
			}
			created[group] = true
		}
		if err := e.Repo.PutAppMessage(t.Context(), store.AppMessageRow{
			GroupID: group, Seq: v.Seq, Epoch: v.Epoch, UploaderDevice: mustID(t, v.UploaderDevice),
			Blob: []byte("x"), CommitmentC: mustHex(t, v.Commitment), FrankingTag: mustHex(t, v.Tag),
			Size: 1, Created: v.RecvTS, FrankingKeyID: keyID,
		}); err != nil {
			t.Fatalf("PutAppMessage: %v", err)
		}
		if got := e.report(t, tok, group, v.Seq, envelope, kf).VerificationResult; got != "verified" {
			t.Fatalf("seq %d: the corpus case verified as %q", v.Seq, got)
		}
	}
}

// A readable envelope that fits the cap must be reportable: the report wraps it in about 65 bytes
// of ids and heads, which the report body cap allows for.
func TestACapSizedReadableMessageCanBeReported(t *testing.T) {
	env := maximalTenFileEnvelope(t)
	capBytes := int64(len(env)) + 8
	f := newReadableFixtureCap(t, capBytes)
	e := f.e
	api.NewReports(e.Repo, f.keys, e.Clk, slog.New(slog.DiscardHandler)).WithMaxCiphertextBytes(capBytes).Register(e.Mux)
	ch := readableChannel(t, e, f.cid, f.ownerTok)
	seq := postReadable(t, e, ch, f.ownerTok, env)
	re := &reportEnv{env: e}
	_, tok := e.NewUser("reporter")
	if row := re.report(t, tok, ch, seq, env, bytes.Repeat([]byte{0x06}, 32)); row.VerificationResult != "verified" {
		t.Fatalf("a cap-sized readable message verified as %q", row.VerificationResult)
	}
}
