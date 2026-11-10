package api_test

// The four application-ciphertext routes of interfaces.md §5.1 — rows 7 (upload), 12 (catch-up),
// 17 (delete) and 18 (cursor) — end to end over the same mux, sessions, delivery service and real
// wasm core groups_test.go stands up. Nothing between the request and the guest is a double.

import (
	"bytes"
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/api/sockettest"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// Endpoint 7's response is the three-element array [seq, franking_tag, recv_ts] — the epoch is NOT
// on the wire (deviation B14), so a fourth element would be a contract break.
func TestUploadAnswersSeqTheFrankingTagAndRecvTS(t *testing.T) {
	h := newGroupsAPI(t)
	h.mustCreate(t)
	member := h.memberToken(t)

	res := h.do(t, http.MethodPost, "/v1/groups/"+h.groupID.String()+"/message", member,
		h.uploadBody(t, 64))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", res.Code, res.Body.String())
	}
	var raw []any
	if err := cborx.Unmarshal(res.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode the response: %v", err)
	}
	if len(raw) != 3 {
		t.Fatalf("the upload response has %d elements, want 3: [seq, franking_tag, recv_ts]", len(raw))
	}
	var out struct {
		_           struct{} `cbor:",toarray"`
		Seq         uint64
		FrankingTag []byte
		RecvTS      uint64
	}
	if err := cborx.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode the response: %v", err)
	}
	if out.Seq != 1 {
		t.Errorf("seq = %d, want 1 — the first allocation of the group's one seq space", out.Seq)
	}
	if len(out.FrankingTag) != 32 {
		t.Errorf("franking_tag is %d bytes, want 32", len(out.FrankingTag))
	}
	if out.RecvTS == 0 {
		t.Error("recv_ts is 0")
	}
}

// A body over the ciphertext cap plus its CBOR framing is 413 E_TOO_LARGE, with protocol/02's
// error array in the body. DecodeBody RETURNS its refusal and writes nothing, so a handler that
// dropped the error would answer 200 with an empty body instead (deviation D5).
func TestAnOversizeUploadIsFourOneThreeAndCarriesTheErrorBody(t *testing.T) {
	h := newGroupsAPI(t)
	h.mustCreate(t)
	member := h.memberToken(t)

	body := mustCBOR(t, []any{h.groupEpoch(t), bytes.Repeat([]byte{7}, 140000)})
	res := h.do(t, http.MethodPost, "/v1/groups/"+h.groupID.String()+"/message", member, body)
	if res.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: %s", res.Code, res.Body.String())
	}
	if got := errorCode(t, res); got != string(server.CodeTooLarge) {
		t.Fatalf("code = %s, want %s", got, server.CodeTooLarge)
	}
}

// Endpoint 12's row is the eight-element [seq, epoch, uploader_device, blob, commitment,
// franking_tag, recv_ts, deleted]. A tombstone keeps its place in the stream with a null blob and
// deleted = 1; everything else survives, which is what makes the seq stream dense after a delete.
func TestTheCatchUpCarriesTheRowShapeAndTheTombstone(t *testing.T) {
	h := newGroupsAPI(t)
	h.mustCreate(t)
	member := h.memberToken(t)

	h.mustUpload(t, member, 64)
	second := h.mustUpload(t, member, 96)

	res := h.do(t, http.MethodDelete,
		"/v1/groups/"+h.groupID.String()+"/messages/1", member, nil)
	if res.Code != http.StatusNoContent {
		t.Fatalf("delete: status = %d, want 204: %s", res.Code, res.Body.String())
	}

	res = h.do(t, http.MethodGet, "/v1/groups/"+h.groupID.String()+"/messages?from=0", member, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", res.Code, res.Body.String())
	}
	var raw []any
	if err := cborx.Unmarshal(res.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode the array: %v", err)
	}
	if len(raw) != 2 {
		t.Fatalf("got %d rows, want the tombstone and the surviving message", len(raw))
	}
	row0, ok := raw[0].([]any)
	if !ok || len(row0) != 9 {
		t.Fatalf("row 0 is %v, want a nine-element array", raw[0])
	}
	if row0[3] != nil {
		t.Errorf("the tombstone's blob is %T, want null", row0[3])
	}

	var rows []struct {
		_              struct{} `cbor:",toarray"`
		Seq            uint64
		Epoch          uint64
		UploaderDevice id.ID
		Blob           []byte
		Commitment     []byte
		FrankingTag    []byte
		RecvTS         uint64
		Deleted        uint8
		Class          *uint64
	}
	if err := cborx.Unmarshal(res.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode the rows: %v", err)
	}
	if rows[0].Seq != 1 || rows[0].Deleted != 1 {
		t.Errorf("row 0 = seq %d deleted %d, want seq 1 deleted 1", rows[0].Seq, rows[0].Deleted)
	}
	if rows[0].Blob != nil {
		t.Error("a tombstone keeps no ciphertext")
	}
	if len(rows[0].Commitment) != 32 || len(rows[0].FrankingTag) != 32 {
		t.Errorf("the tombstone lost its commitment (%d bytes) or its tag (%d bytes)",
			len(rows[0].Commitment), len(rows[0].FrankingTag))
	}
	if rows[0].Epoch == 0 || rows[0].RecvTS == 0 {
		t.Error("the tombstone lost its epoch or its recv_ts")
	}
	if rows[1].Seq != 2 || rows[1].Deleted != 0 {
		t.Errorf("row 1 = seq %d deleted %d, want seq 2 deleted 0", rows[1].Seq, rows[1].Deleted)
	}
	if !bytes.Equal(rows[1].Blob, second.pm) {
		t.Error("row 1 served a ciphertext the uploader did not send")
	}
	if rows[1].UploaderDevice == (id.ID{}) {
		t.Error("row 1 has no uploader_device")
	}
}

// L-HTTP-80 over the wire: a session whose device is not a member of the group is 404 E_NOT_FOUND —
// what every member-only group route answers, so group existence is not probeable — before the
// uploader rule is read.
func TestADeleteByANonMemberIsFourZeroFourNotFound(t *testing.T) {
	h := newGroupsAPI(t)
	h.mustCreate(t)
	member := h.memberToken(t)
	h.mustUpload(t, member, 64)

	// h.session is an enrolled session of a DIFFERENT user, and not a leaf of the group.
	res := h.do(t, http.MethodDelete,
		"/v1/groups/"+h.groupID.String()+"/messages/1", h.session, nil)
	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", res.Code, res.Body.String())
	}
	if got := errorCode(t, res); got != string(server.CodeNotFound) {
		t.Fatalf("code = %s, want %s", got, server.CodeNotFound)
	}
}

// R33: a malformed path value is E_INVALID_REQUEST at the edge and never reaches the database.
func TestADeleteWithANonNumericSeqIsFourHundred(t *testing.T) {
	h := newGroupsAPI(t)
	h.mustCreate(t)
	member := h.memberToken(t)

	res := h.do(t, http.MethodDelete,
		"/v1/groups/"+h.groupID.String()+"/messages/nineteen", member, nil)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", res.Code, res.Body.String())
	}
	if got := errorCode(t, res); got != string(server.CodeInvalidRequest) {
		t.Fatalf("code = %s, want %s", got, server.CodeInvalidRequest)
	}
}

// Endpoint 18 is [last_seq, last_epoch] -> 204 with no body, and the row it writes is the one
// MinCursor reads.
func TestTheCursorRouteAnswersTwoZeroFourAndWritesTheRow(t *testing.T) {
	h := newGroupsAPI(t)
	h.mustCreate(t)
	member := h.memberToken(t)
	h.mustUpload(t, member, 64)

	body := mustCBOR(t, []any{uint64(1), h.groupEpoch(t)})
	res := h.do(t, http.MethodPost, "/v1/groups/"+h.groupID.String()+"/cursor", member, body)
	if res.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", res.Code, res.Body.String())
	}
	if res.Body.Len() != 0 {
		t.Errorf("a 204 carried %d bytes of body", res.Body.Len())
	}
	device := h.memberDevice(t)
	cur, err := h.deps.Repo.GetCursor(context.Background(), device, h.groupID)
	if err != nil {
		t.Fatalf("GetCursor: %v", err)
	}
	if cur.LastSeq != 1 {
		t.Errorf("last_seq = %d, want 1", cur.LastSeq)
	}
}

// A zero-valued Messages must still serve: both caps reach http.MaxBytesReader, where a limit of 0
// refuses every body including an empty one, so the zero value has to mean "the default"
// (deviation D6). Nothing in the struct is set here on purpose.
func TestAZeroValuedMessagesFallsBackToTheDefaultCaps(t *testing.T) {
	h := newGroupsAPI(t)
	h.mustCreate(t)
	member := h.memberToken(t)

	bare := server.NewMux()
	(&api.Messages{DS: h.ds}).Register(bare, h.deps.Sessions)
	// The routes are socket routes: this mux's requests go through the socket dispatcher.
	mux := sockettest.NewBridge(bare, bare, api.NewSocketDispatcher(bare, api.SocketDispatchOptions{Clock: h.deps.Clock}),
		h.deps.Repo, h.deps.Clock.Now)

	res := doOn(t, mux, http.MethodPost, "/v1/groups/"+h.groupID.String()+"/message", member,
		h.uploadBody(t, 64))
	if res.Code != http.StatusOK {
		t.Fatalf("upload: status = %d, want 200 — a zero MaxCiphertextBytes refused the body: %s",
			res.Code, res.Body.String())
	}
	body := mustCBOR(t, []any{uint64(1), h.groupEpoch(t)})
	res = doOn(t, mux, http.MethodPost, "/v1/groups/"+h.groupID.String()+"/cursor", member, body)
	if res.Code != http.StatusNoContent {
		t.Fatalf("cursor: status = %d, want 204 — a zero MaxBody refused the body: %s",
			res.Code, res.Body.String())
	}
}

// ------------------------------------------------------------------ harness

// uploaded is what mustUpload hands back: the seq the instance allocated and the exact ciphertext
// it was given, so the catch-up can be compared against the bytes rather than against a length.
type uploaded struct {
	seq uint64
	pm  []byte
}

func (h *groupsAPI) groupEpoch(t *testing.T) uint64 {
	t.Helper()
	row, err := h.deps.Repo.GetGroup(context.Background(), h.groupID)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	return row.Epoch
}

// memberDevice is the device behind memberToken: leaf 0 of the committed fixture.
func (h *groupsAPI) memberDevice(t *testing.T) id.ID {
	t.Helper()
	members, err := h.deps.Repo.ListMembers(context.Background(), h.groupID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	if len(members) == 0 {
		t.Fatal("the registered group has no member rows")
	}
	return members[0].DeviceID
}

// uploadBody is endpoint 7's [epoch, private_message, class 0] for a well-formed post at the group's own
// epoch.
func (h *groupsAPI) uploadBody(t *testing.T, ctLen int) []byte {
	t.Helper()
	epoch := h.groupEpoch(t)
	return mustCBOR(t, []any{epoch, apiPrivateMessage(t, epoch, ctLen), uint64(0)})
}

func (h *groupsAPI) mustUpload(t *testing.T, token string, ctLen int) uploaded {
	t.Helper()
	epoch := h.groupEpoch(t)
	pm := apiPrivateMessage(t, epoch, ctLen)
	res := h.do(t, http.MethodPost, "/v1/groups/"+h.groupID.String()+"/message", token,
		mustCBOR(t, []any{epoch, pm, uint64(0)}))
	if res.Code != http.StatusOK {
		t.Fatalf("upload: status = %d, want 200: %s", res.Code, res.Body.String())
	}
	var out struct {
		_           struct{} `cbor:",toarray"`
		Seq         uint64
		FrankingTag []byte
		RecvTS      uint64
	}
	if err := cborx.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatalf("upload response: %v", err)
	}
	return uploaded{seq: out.Seq, pm: pm}
}

func doOn(t *testing.T, mux http.Handler, method, path, token string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == nil {
		req = httptest.NewRequestWithContext(t.Context(), method, path, nil)
	} else {
		req = httptest.NewRequestWithContext(t.Context(), method, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/cbor")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// apiPrivateMessage frames RFC 9420 §6.3.2's MLSMessage/PrivateMessage by hand, as
// core/dilla-core-wasi/src/private_message.rs's own tests_support::message does. The guest parses
// the header without decrypting — the delivery service holds no group secrets — so a real
// ciphertext is neither needed nor possible here. It is the same framing internal/ds's harness
// uses; the two packages cannot share an unexported test helper.
func apiPrivateMessage(t *testing.T, epoch uint64, ctLen int) []byte {
	t.Helper()
	var out []byte
	out = binary.BigEndian.AppendUint16(out, 1) // protocol_version: MLS 1.0
	out = binary.BigEndian.AppendUint16(out, 2) // wire_format: PrivateMessage
	out = appendVL(t, out, []byte("group-id"))
	out = binary.BigEndian.AppendUint64(out, epoch)
	out = append(out, 1) // content_type: application
	out = appendVL(t, out, bytes.Repeat([]byte{5}, 32))
	out = appendVL(t, out, bytes.Repeat([]byte{7}, 16))
	out = appendVL(t, out, bytes.Repeat([]byte{9}, ctLen))
	return out
}

// appendVL writes RFC 9420 §2.1.3's variable-length header — RFC 9000's varint, whose two top bits
// give the width, in the shortest form that carries the value — and then the payload.
func appendVL(t *testing.T, dst, v []byte) []byte {
	t.Helper()
	switch {
	case len(v) < 64:
		dst = append(dst, byte(len(v)))
	case len(v) < 16384:
		dst = binary.BigEndian.AppendUint16(dst, uint16(len(v))|0x4000)
	default:
		t.Fatalf("appendVL: %d bytes is past the two-byte form", len(v))
	}
	return append(dst, v...)
}

// ------------------------------------------------------------------ the upload class (web3.7 SB1)

// fakePostPolicy is the delivery service's post policy seam as the route sees it: the classes it
// was asked about, and one answer for all of them.
type fakePostPolicy struct {
	mu      sync.Mutex
	classes []uint8
	err     error
}

func (p *fakePostPolicy) Check(_ context.Context, _ store.GroupRow, _ id.ID, class uint8, _ int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.classes = append(p.classes, class)
	return p.err
}

func (p *fakePostPolicy) answer(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = err
}

func (p *fakePostPolicy) take() []uint8 {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.classes
	p.classes = nil
	return out
}

func newClassAPI(t *testing.T) (*groupsAPI, *fakePostPolicy) {
	t.Helper()
	policy := &fakePostPolicy{}
	h := newGroupsAPIWith(t, func(store.Repository) ds.Channels { return openChannels{} }, policy)
	h.mustCreate(t)
	return h, policy
}

// apiListenStore is apiGatewayStore for a device that comes online: ready reads a cursor for each
// of the device's groups, and a device that never acknowledged one starts at zero.
type apiListenStore struct{ apiGatewayStore }

func (apiListenStore) GetCursor(_ context.Context, _, _ id.ID) (store.CursorRow, error) {
	return store.CursorRow{}, nil
}

// listen puts the device behind token online on the harness's gateway and returns a function that
// waits for the next op 19 (message.ct) frame and hands back its payload's elements.
func (h *groupsAPI) listen(t *testing.T, token string) func() []cbor.RawMessage {
	t.Helper()
	srv := httptest.NewServer(h.gw.Handler())
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), &websocket.DialOptions{ //nolint:bodyclose // websocket.Dial documents that the handshake response body never needs closing
		HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + token}},
		Subprotocols: []string{"dilla.v1"},
	})
	if err != nil {
		t.Fatalf("dial the gateway: %v", err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	read := func() (gateway.Op, []cbor.RawMessage) {
		t.Helper()
		_, b, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read a frame: %v", err)
		}
		var frame []cbor.RawMessage
		if err := cborx.Unmarshal(b, &frame); err != nil || len(frame) != 4 {
			t.Fatalf("a frame of %d elements: %v", len(frame), err)
		}
		var op uint64
		var payload []cbor.RawMessage
		if err := cborx.Unmarshal(frame[0], &op); err != nil {
			t.Fatalf("decode the opcode: %v", err)
		}
		if err := cborx.Unmarshal(frame[3], &payload); err != nil {
			t.Fatalf("decode the payload: %v", err)
		}
		return gateway.Op(op), payload
	}
	if op, _ := read(); op != gateway.OpHello {
		t.Fatalf("first frame op %d, want hello", op)
	}
	identify, err := gateway.Encode(gateway.Frame{Op: gateway.OpIdentify,
		Payload: cborx.Raw(mustCBOR(t, []any{"", uint64(6), uint64(1), uint64(1), uint64(0)}))}, 1)
	if err != nil {
		t.Fatalf("encode identify: %v", err)
	}
	if err := c.Write(ctx, websocket.MessageBinary, identify); err != nil {
		t.Fatalf("write identify: %v", err)
	}
	for {
		if op, _ := read(); op == gateway.OpReady {
			break
		}
	}
	return func() []cbor.RawMessage {
		t.Helper()
		for {
			if op, payload := read(); op == gateway.OpMessageCT {
				return payload
			}
		}
	}
}

func (h *groupsAPI) post(t *testing.T, token string, body []any) *httptest.ResponseRecorder {
	t.Helper()
	return h.do(t, http.MethodPost, "/v1/groups/"+h.groupID.String()+"/message", token, mustCBOR(t, body))
}

// catchUpRow is the raw elements of the one catch-up row at seq.
func (h *groupsAPI) catchUpRow(t *testing.T, token string, seq uint64) []cbor.RawMessage {
	t.Helper()
	res := h.do(t, http.MethodGet, "/v1/groups/"+h.groupID.String()+"/messages?from=0", token, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("catch-up: status = %d: %s", res.Code, res.Body.String())
	}
	var rows [][]cbor.RawMessage
	if err := cborx.Unmarshal(res.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode the rows: %v", err)
	}
	for _, row := range rows {
		var got uint64
		if len(row) > 0 && cborx.Unmarshal(row[0], &got) == nil && got == seq {
			return row
		}
	}
	t.Fatalf("no catch-up row at seq %d among %d", seq, len(rows))
	return nil
}

func uploadSeq(t *testing.T, res *httptest.ResponseRecorder) uint64 {
	t.Helper()
	var out []cbor.RawMessage
	var seq uint64
	if err := cborx.Unmarshal(res.Body.Bytes(), &out); err != nil || len(out) != 3 || cborx.Unmarshal(out[0], &seq) != nil {
		t.Fatalf("the upload response %x: %v", res.Body.Bytes(), err)
	}
	return seq
}

// classElement is the CBOR of the class a row or frame should carry: the uint, or null.
func classElement(class *uint64) []byte {
	if class == nil {
		return []byte{0xf6}
	}
	return []byte{byte(*class)}
}

func TestAnUploadCarriesItsClassToTheRowAndTheFrame(t *testing.T) {
	h, policy := newClassAPI(t)
	member := h.memberToken(t)
	next := h.listen(t, member)
	for _, class := range []uint64{0, 1} {
		epoch := h.groupEpoch(t)
		res := h.post(t, member, []any{epoch, apiPrivateMessage(t, epoch, 64), class})
		if res.Code != http.StatusOK {
			t.Fatalf("class %d: status = %d, want 200: %s", class, res.Code, res.Body.String())
		}
		seq := uploadSeq(t, res)
		if got := policy.take(); len(got) != 1 || uint64(got[0]) != class {
			t.Fatalf("class %d: the policy was asked about %v", class, got)
		}
		row := h.catchUpRow(t, member, seq)
		if len(row) != 9 || !bytes.Equal(row[8], classElement(&class)) {
			t.Fatalf("class %d: the catch-up row has %d elements, the 9th %x", class, len(row), row[len(row)-1])
		}
		frame := next()
		if len(frame) != 7 || !bytes.Equal(frame[6], classElement(&class)) {
			t.Fatalf("class %d: message.ct has %d elements, the 7th %x", class, len(frame), frame[len(frame)-1])
		}
	}
}

// web3.8 S1: a two-element body (the legacy upload web3.7 accepted, R28 there) is refused with
// 400 E_INVALID_REQUEST before the policy is asked, and nothing is stored. The control is the same
// body plus class 0 from the same member, which is accepted.
func TestALegacyUploadIsRefused(t *testing.T) {
	h, policy := newClassAPI(t)
	member := h.memberToken(t)
	epoch := h.groupEpoch(t)
	pm := apiPrivateMessage(t, epoch, 64)
	before, err := h.deps.Repo.GetGroup(context.Background(), h.groupID)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	res := h.post(t, member, []any{epoch, pm})
	if res.Code != http.StatusBadRequest || errorCode(t, res) != "E_INVALID_REQUEST" {
		t.Fatalf("a two-element upload: %d %s, want 400 E_INVALID_REQUEST", res.Code, res.Body.String())
	}
	var out []any
	if err := cborx.Unmarshal(res.Body.Bytes(), &out); err != nil || len(out) < 2 ||
		out[1] != "body must be [epoch, private_message, class], got 2 elements" {
		t.Fatalf("the error body %v (%v), want the three-element detail", out, err)
	}
	if got := policy.take(); len(got) != 0 {
		t.Fatalf("a refused two-element upload reached the policy %d times", len(got))
	}
	if row, err := h.deps.Repo.GetGroup(context.Background(), h.groupID); err != nil || row.Seq != before.Seq {
		t.Fatalf("after the refusal the group's seq is %d (%v), want %d: nothing stored", row.Seq, err, before.Seq)
	}
	// Control: the same body plus class 0.
	if res := h.post(t, member, []any{epoch, pm, uint64(0)}); res.Code != http.StatusOK {
		t.Fatalf("control: the same body plus class 0: status = %d, want 200: %s", res.Code, res.Body.String())
	}
}

// S1 fix round 1 (I1): a row a web3.7 legacy upload stored has a NULL class, and the catch-up
// still serves it as null (0xf6), never as 0: a receiver drops a row whose class disagrees with
// its decrypted type, so an old edit served as class 0 would be lost. The control is a class-0
// row in the same response, served as 0x00.
func TestALegacyRowStillReadsBackNull(t *testing.T) {
	h, _ := newClassAPI(t)
	member := h.memberToken(t)
	epoch := h.groupEpoch(t)
	res := h.post(t, member, []any{epoch, apiPrivateMessage(t, epoch, 64), uint64(0)})
	if res.Code != http.StatusOK {
		t.Fatalf("the class-0 upload: status = %d, want 200: %s", res.Code, res.Body.String())
	}
	posted := uploadSeq(t, res)
	legacy := posted + 1
	if err := h.deps.Repo.PutAppMessage(context.Background(), store.AppMessageRow{
		GroupID: h.groupID, Seq: legacy, Epoch: epoch, UploaderDevice: id.New(), Blob: []byte("ct"),
		FrankingTag: make([]byte, 32), Size: 2, Created: 1, Class: nil,
	}); err != nil {
		t.Fatalf("seed the legacy row: %v", err)
	}
	if row := h.catchUpRow(t, member, legacy); len(row) != 9 || !bytes.Equal(row[8], []byte{0xf6}) {
		t.Fatalf("the legacy row has %d elements, the 9th %x; want 9, null (f6)", len(row), row[len(row)-1])
	}
	// Control: the class-0 row in the same catch-up.
	if row := h.catchUpRow(t, member, posted); len(row) != 9 || !bytes.Equal(row[8], []byte{0x00}) {
		t.Fatalf("the class-0 row has %d elements, the 9th %x; want 9, 00", len(row), row[len(row)-1])
	}
}

func TestAnUploadOfAnyOtherShapeIsRefused(t *testing.T) {
	h, policy := newClassAPI(t)
	member := h.memberToken(t)
	epoch := h.groupEpoch(t)
	pm := apiPrivateMessage(t, epoch, 64)
	for _, c := range []struct {
		name   string
		body   []any
		detail string
	}{
		{"one element", []any{epoch}, ""},
		{"four elements", []any{epoch, pm, uint64(0), uint64(0)}, ""},
		{"class 2", []any{epoch, pm, uint64(2)}, "class must be 0 or 1"},
		{"a text class", []any{epoch, pm, "0"}, ""},
		// SB1 fix round 1: the wire is uint 0 or 1; a null third element is not a legacy upload.
		{"a null class", []any{epoch, pm, nil}, ""},
	} {
		res := h.post(t, member, c.body)
		if res.Code != http.StatusBadRequest || errorCode(t, res) != "E_INVALID_REQUEST" {
			t.Fatalf("%s: %d %s, want 400 E_INVALID_REQUEST", c.name, res.Code, res.Body.String())
		}
		if c.detail != "" {
			var out []any
			if err := cborx.Unmarshal(res.Body.Bytes(), &out); err != nil || len(out) < 2 || out[1] != c.detail {
				t.Fatalf("%s: the error body %v, want detail %q", c.name, out, c.detail)
			}
		}
	}
	if got := policy.take(); len(got) != 0 {
		t.Fatalf("a refused shape reached the policy %d times", len(got))
	}
	// Control: the well-formed three-element body is accepted.
	if res := h.post(t, member, []any{epoch, pm, uint64(0)}); res.Code != http.StatusOK {
		t.Fatalf("control: status = %d, want 200: %s", res.Code, res.Body.String())
	}
}

func TestThePolicysRefusalIsTheAnswer(t *testing.T) {
	h, policy := newClassAPI(t)
	member := h.memberToken(t)
	epoch := h.groupEpoch(t)
	slow := server.RateLimited(5000)
	slow.Detail = "slowmode: 5 seconds left"
	for _, c := range []struct {
		err    error
		status int
		code   string
		retry  *uint64
	}{
		{server.Errorf(server.CodeForbidden, "missing permission"), http.StatusForbidden, "E_FORBIDDEN", nil},
		{slow, http.StatusTooManyRequests, "E_RATE_LIMITED", slow.RetryAfterMS},
	} {
		policy.answer(c.err)
		res := h.post(t, member, []any{epoch, apiPrivateMessage(t, epoch, 64), uint64(0)})
		if res.Code != c.status || errorCode(t, res) != c.code {
			t.Fatalf("the policy answering %v: %d %s, want %d %s", c.err, res.Code, res.Body.String(), c.status, c.code)
		}
		var out []cbor.RawMessage
		if err := cborx.Unmarshal(res.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode the error body: %v", err)
		}
		if c.retry != nil {
			var retry uint64
			if len(out) < 3 || cborx.Unmarshal(out[2], &retry) != nil || retry != *c.retry {
				t.Fatalf("retry_after_ms of %x, want %d", res.Body.Bytes(), *c.retry)
			}
		}
	}
	// Control: the policy relents and the same body is accepted.
	policy.answer(nil)
	if res := h.post(t, member, []any{epoch, apiPrivateMessage(t, epoch, 64), uint64(0)}); res.Code != http.StatusOK {
		t.Fatalf("control: status = %d, want 200: %s", res.Code, res.Body.String())
	}
}

func TestTimedOutMemberCannotUploadClassOneButCanDeleteOwnMessage(t *testing.T) {
	handler, deps := newTestAPI(t)
	cid, owner := id.New(), id.New()
	now := deps.Clock.Now().Unix()
	if err := deps.Repo.CreateUser(t.Context(), newAPITestUser(owner, "timeout-owner", now)); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.CreateCommunity(t.Context(), store.CommunityRow{ID: cid, Owner: owner, Name: "timeout-community", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: now}); err != nil {
		t.Fatal(err)
	}
	channelID := id.New()
	if err := deps.Repo.CreateChannel(t.Context(), store.ChannelRow{ID: channelID, CommunityID: &cid, Kind: api.ChannelText, Mode: api.ModeE2EE, Name: "general", SettingsJSON: []byte(`{}`), HostPolicyVersion: 1, Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.PutRole(t.Context(), store.RoleRow{ID: id.New(), CommunityID: cid, Name: "@everyone", Allow: uint64(api.PermViewChannel | api.PermSendMessages), Created: now}); err != nil {
		t.Fatal(err)
	}

	h := newGroupsAPIWithHandler(t, handler, deps, func(repo store.Repository) ds.Channels { return api.StructureChannels{Repo: repo} }, api.PostPolicy{Repo: deps.Repo})
	var binding ds.Binding
	if err := cborx.Unmarshal(h.fixture.binding, &binding); err != nil {
		t.Fatal(err)
	}
	binding.CommunityID, binding.TargetID = &cid, channelID
	h.fixture.binding, _ = cborx.Marshal(binding)
	h.mustCreate(t)
	token := h.memberToken(t)
	members, err := deps.Repo.ListMembers(t.Context(), h.groupID)
	if err != nil || len(members) == 0 {
		t.Fatalf("group members = %d, %v", len(members), err)
	}
	user := members[0].UserID
	if err := deps.Repo.PutMember(t.Context(), store.MemberOfCommunityRow{CommunityID: cid, UserID: user, Joined: now}); err != nil {
		t.Fatal(err)
	}
	seq := h.mustUpload(t, token, 64).seq
	until := h.clk.Now().Unix() + 600
	if err := deps.Repo.PutMemberTimeout(t.Context(), store.MemberTimeoutRow{CommunityID: cid, UserID: user, ByUser: owner, Created: now, Expires: until}); err != nil {
		t.Fatal(err)
	}

	epoch := h.groupEpoch(t)
	edit := h.do(t, http.MethodPost, "/v1/groups/"+h.groupID.String()+"/message", token, mustCBOR(t, []any{epoch, apiPrivateMessage(t, epoch, 64), uint64(1)}))
	if edit.Code != http.StatusForbidden {
		t.Fatalf("timed out class-1 upload = %d, want 403", edit.Code)
	}
	deleted := h.do(t, http.MethodDelete, "/v1/groups/"+h.groupID.String()+"/messages/"+strconv.FormatUint(seq, 10), token, nil)
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("own DELETE route during timeout = %d, want 204: %s", deleted.Code, deleted.Body.String())
	}
}
