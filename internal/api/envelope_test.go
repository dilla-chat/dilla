package api_test

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

// env9 builds an envelope of the given type with the given body and lists.
func env9(typ uint8, body string, atts []api.Attachment, prevs []api.Preview) []byte {
	e := []any{
		uint64(1), id.New(), uint64(typ), nil, nil, body,
		attsToAny(atts), prevsToAny(prevs), make([]byte, 32),
	}
	b, err := cborx.Marshal(e)
	if err != nil {
		panic(err)
	}
	return b
}

// envRef is env9 with reply_to naming a readable channel's seq (P2-D15): a
// big-endian uint64 in the low eight bytes, the high eight zero.
func envRef(typ uint8, body string, seq uint64) []byte {
	b, err := cborx.Marshal([]any{
		uint64(1), id.New(), uint64(typ), nil, seqRef(seq), body, []any{}, []any{}, bytes.Repeat([]byte{0x0b}, 32),
	})
	if err != nil {
		panic(err)
	}
	return b
}

func seqRef(seq uint64) id.ID {
	var ref id.ID
	binary.BigEndian.PutUint64(ref[8:], seq)
	return ref
}

func TestEveryTightenedLimitIsEnforced(t *testing.T) {
	big := func(n int) string { return strings.Repeat("a", n) }
	att := func(mime string, thumb int) api.Attachment {
		return api.Attachment{
			BlobID: make([]byte, 32), Key: make([]byte, 32), Nonce: make([]byte, 12),
			Size: 1, Mime: mime, Thumb: make([]byte, thumb),
		}
	}
	named := func(n int) api.Attachment { a := att("image/png", 0); a.Name = big(n); return a }
	prev := func(url, title, desc string, img int) api.Preview {
		return api.Preview{URL: url, Title: title, Description: desc, Image: make([]byte, img)}
	}
	ok := att("image/png", 8192)
	okPrev := prev("https://x", "t", "d", 16384)

	cases := []struct {
		name string
		b    []byte
		code string
	}{
		{"name 255 is allowed", env9(0, "hi", []api.Attachment{named(255)}, nil), ""},
		{"name 256 is refused", env9(0, "hi", []api.Attachment{named(256)}, nil), "E_ENVELOPE_LIMIT"},
		{"mime 255 is allowed", env9(0, "hi", []api.Attachment{att(big(255), 0)}, nil), ""},
		{"mime 256 is refused", env9(0, "hi", []api.Attachment{att(big(256), 0)}, nil), "E_ENVELOPE_LIMIT"},
		{"thumb 8192 is allowed", env9(0, "hi", []api.Attachment{ok}, nil), ""},
		{"thumb 8193 is refused", env9(0, "hi", []api.Attachment{att("image/png", 8193)}, nil), "E_ENVELOPE_LIMIT"},
		{"4 attachments are allowed", env9(0, "hi", []api.Attachment{ok, ok, ok, ok}, nil), ""},
		{"5 attachments are refused", env9(0, "hi", []api.Attachment{ok, ok, ok, ok, ok}, nil), "E_ENVELOPE_LIMIT"},
		{"2 previews are allowed", env9(0, "hi", nil, []api.Preview{okPrev, okPrev}), ""},
		{"3 previews are refused", env9(0, "hi", nil, []api.Preview{okPrev, okPrev, okPrev}), "E_ENVELOPE_LIMIT"},
		{"url 2048 is allowed", env9(0, "hi", nil, []api.Preview{prev(big(2048), "t", "d", 0)}), ""},
		{"url 2049 is refused", env9(0, "hi", nil, []api.Preview{prev(big(2049), "t", "d", 0)}), "E_ENVELOPE_LIMIT"},
		{"title 257 is refused", env9(0, "hi", nil, []api.Preview{prev("u", big(257), "d", 0)}), "E_ENVELOPE_LIMIT"},
		{"description 1025 is refused", env9(0, "hi", nil, []api.Preview{prev("u", "t", big(1025), 0)}), "E_ENVELOPE_LIMIT"},
		{"preview image 16385 is refused", env9(0, "hi", nil, []api.Preview{prev("u", "t", "d", 16385)}), "E_ENVELOPE_LIMIT"},
		{"body 4000 for type 0 is allowed", env9(0, big(4000), nil, nil), ""},
		{"body 4001 for type 0 is refused", env9(0, big(4001), nil, nil), "E_ENVELOPE_LIMIT"},
		{"body 32 for a reaction is allowed", envRef(3, big(32), 1), ""},
		{"body 33 for a reaction is refused", env9(3, big(33), nil, nil), "E_ENVELOPE_LIMIT"},
		{"a delete with a body is refused", env9(2, "x", nil, nil), "E_ENVELOPE_LIMIT"},
		{"a pin with a body is refused", env9(5, "x", nil, nil), "E_ENVELOPE_LIMIT"},
		{"an unpin with a body is refused", env9(6, "x", nil, nil), "E_ENVELOPE_LIMIT"},
		{"an unknown type is refused", env9(7, "", nil, nil), "E_ENVELOPE_TYPE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := api.ParseEnvelope(tc.b)
			switch {
			case tc.code == "" && err != nil:
				t.Fatalf("ParseEnvelope: %v", err)
			case tc.code != "" && err == nil:
				t.Fatalf("ParseEnvelope accepted it; want %s", tc.code)
			case tc.code != "":
				if got := codeOf(err); got != tc.code {
					t.Fatalf("code = %s, want %s", got, tc.code)
				}
			}
		})
	}
}

func TestEnvelopeShapeIsRejected(t *testing.T) {
	short, _ := cborx.Marshal([]any{uint64(1), id.New(), uint64(0)})
	if _, err := api.ParseEnvelope(short); codeOf(err) != "E_ENVELOPE_SHAPE" {
		t.Fatalf("a 3-element envelope = %v", err)
	}
	long, _ := cborx.Marshal([]any{
		uint64(1), id.New(), uint64(0), nil, nil, "x", []any{}, []any{}, make([]byte, 32), "extra",
	})
	if _, err := api.ParseEnvelope(long); codeOf(err) != "E_ENVELOPE_SHAPE" {
		t.Fatalf("a 10-element envelope = %v", err)
	}
	badKF, _ := cborx.Marshal([]any{
		uint64(1), id.New(), uint64(0), nil, nil, "x", []any{}, []any{}, make([]byte, 31),
	})
	if _, err := api.ParseEnvelope(badKF); codeOf(err) != "E_ENVELOPE_SHAPE" {
		t.Fatalf("a 31-byte k_f = %v", err)
	}
}

// fxamacker/cbor decodes a CBOR array of small uints straight into a Go []byte,
// and a toarray struct decodes a SHORT array by zero-filling its tail. Neither
// may reach a stored envelope: every field's major type and every member's
// element count is checked.
func TestEnvelopeFieldShapesAreStrict(t *testing.T) {
	kf := make([]byte, 32)
	okAtt := func() []any {
		return []any{make([]byte, 32), make([]byte, 32), make([]byte, 12), uint64(1), "image/png", nil, nil, nil, ""}
	}
	arrayOf := func(n int) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = uint64(0)
		}
		return out
	}
	withAtt := func(mutate func([]any)) []byte {
		a := okAtt()
		mutate(a)
		return mustMarshalEnv(t, []any{uint64(1), id.New(), uint64(0), nil, nil, "x", []any{a}, []any{}, kf})
	}
	cases := map[string][]byte{
		"a v of 2":      mustMarshalEnv(t, []any{uint64(2), id.New(), uint64(0), nil, nil, "x", []any{}, []any{}, kf}),
		"a null msg_id": mustMarshalEnv(t, []any{uint64(1), nil, uint64(0), nil, nil, "x", []any{}, []any{}, kf}),
		"a 15-byte thread_id": mustMarshalEnv(t, []any{uint64(1), id.New(), uint64(0), make([]byte, 15), nil, "x",
			[]any{}, []any{}, kf}),
		"a text reply_to": mustMarshalEnv(t, []any{uint64(1), id.New(), uint64(0), nil, "0123456789abcdef", "x",
			[]any{}, []any{}, kf}),
		"a k_f spelled as an array": mustMarshalEnv(t, []any{uint64(1), id.New(), uint64(0), nil, nil, "x",
			[]any{}, []any{}, arrayOf(32)}),
		"attachments that are not an array": mustMarshalEnv(t, []any{uint64(1), id.New(), uint64(0), nil, nil, "x",
			"none", []any{}, kf}),
		"a blob_id spelled as an array": withAtt(func(a []any) { a[0] = arrayOf(32) }),
		"a 31-byte blob_id":             withAtt(func(a []any) { a[0] = make([]byte, 31) }),
		"an 11-byte nonce":              withAtt(func(a []any) { a[2] = make([]byte, 11) }),
		"a bstr name":                   withAtt(func(a []any) { a[8] = []byte("x") }),
		"a bstr mime":                   withAtt(func(a []any) { a[4] = []byte("image/png") }),
		"a text w":                      withAtt(func(a []any) { a[5] = "1" }),
		"a thumb spelled as an array":   withAtt(func(a []any) { a[7] = arrayOf(4) }),
		"a three-element preview": mustMarshalEnv(t, []any{uint64(1), id.New(), uint64(0), nil, nil, "x",
			[]any{}, []any{[]any{"u", "t", "d"}}, kf}),
		"a bstr preview title": mustMarshalEnv(t, []any{uint64(1), id.New(), uint64(0), nil, nil, "x",
			[]any{}, []any{[]any{"u", []byte("t"), "d", nil}}, kf}),
	}
	cases["an eight-element attachment"] = mustMarshalEnv(t, []any{uint64(1), id.New(), uint64(0), nil, nil, "x",
		[]any{okAtt()[:8]}, []any{}, kf})
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := api.ParseEnvelope(b); codeOf(err) != "E_ENVELOPE_SHAPE" {
				t.Fatalf("ParseEnvelope = %v, want E_ENVELOPE_SHAPE", err)
			}
		})
	}
	// Null w, h, thumb and image are legal (protocol/04: uint|null, bstr|null).
	legal := mustMarshalEnv(t, []any{uint64(1), id.New(), uint64(0), id.New(), id.New(), "x",
		[]any{okAtt()}, []any{[]any{"u", "t", "d", nil}}, kf})
	e, err := api.ParseEnvelope(legal)
	if err != nil {
		t.Fatalf("a legal envelope with null optionals: %v", err)
	}
	if len(e.Attachments) != 1 || e.Attachments[0].W != nil || e.Attachments[0].Thumb != nil ||
		e.Attachments[0].Mime != "image/png" || len(e.Previews) != 1 || e.Previews[0].Image != nil ||
		e.ThreadID == nil || e.ReplyTo == nil {
		t.Fatalf("parsed = %+v", e)
	}
}

func mustMarshalEnv(t *testing.T, v []any) []byte {
	t.Helper()
	b, err := cborx.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// The rejects protocol/04 lists are replayed against the parser AND the POST
// handler, so the Go validator and the vector corpus cannot drift.
func TestTheRejectVectorsAreRefused(t *testing.T) {
	path := filepath.Join(repoRoot(t), "protocol", "vectors", "envelope.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc struct {
		Rejects []struct {
			Name  string `json:"name"`
			CBOR  string `json:"cbor"`
			Error string `json:"error"`
		} `json:"rejects"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	// protocol/04 § Vectors: the delete tombstone Plan 1 shipped, the eight
	// interfaces.md §2.8 added and web-2b's four — thirteen cases.
	if len(doc.Rejects) != 13 {
		t.Fatalf("rejects = %d, want 13 (protocol/04 § Vectors)", len(doc.Rejects))
	}
	e, cid, ownerTok := readableEnv(t)
	ch := readableChannel(t, e, cid, ownerTok)
	for _, rj := range doc.Rejects {
		t.Run(rj.Name, func(t *testing.T) {
			b, err := hex.DecodeString(rj.CBOR)
			if err != nil {
				t.Fatalf("hex: %v", err)
			}
			_, err = api.ParseEnvelope(b)
			if err == nil {
				t.Fatalf("accepted a reject vector; want %s", rj.Error)
			}
			if got := codeOf(err); got != rj.Error {
				t.Fatalf("code = %s, want %s", got, rj.Error)
			}
			status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/messages", ownerTok, []any{b})
			if status != http.StatusBadRequest || e.ErrCode(body) != rj.Error {
				t.Fatalf("POST = %d %s, want 400 %s", status, e.ErrCode(body), rj.Error)
			}
		})
	}
	if rows, _ := e.Repo.ListReadableMessages(t.Context(), ch, 0, 100); len(rows) != 0 {
		t.Fatalf("%d reject vectors were stored", len(rows))
	}
}

func TestEditFromAnotherUserIsRefused(t *testing.T) {
	e, cid, ownerTok := readableEnv(t)
	ch := readableChannel(t, e, cid, ownerTok)
	seq := postReadable(t, e, ch, ownerTok, env9(0, "hello", nil, nil))

	_, otherTok := e.NewUser("other")
	joinCommunity(t, e, cid, otherTok)
	status, body := e.Do(http.MethodPatch,
		fmt.Sprintf("/v1/channels/%s/messages/%d", ch, seq), otherTok,
		[]any{envRef(1, "edited", seq)})
	if status != http.StatusForbidden {
		t.Fatalf("edit by another user = %d", status)
	}
	if code := e.ErrCode(body); code != "E_FORBIDDEN" {
		t.Fatalf("code = %s", code)
	}
	// The author's own edit succeeds and stamps edited.
	if status, _ := e.Do(http.MethodPatch,
		fmt.Sprintf("/v1/channels/%s/messages/%d", ch, seq), ownerTok,
		[]any{envRef(1, "edited", seq)}); status != http.StatusNoContent {
		t.Fatal("the author could not edit")
	}
}

func TestPinRequiresThePermission(t *testing.T) {
	e, cid, ownerTok := readableEnv(t)
	ch := readableChannel(t, e, cid, ownerTok)
	seq := postReadable(t, e, ch, ownerTok, env9(0, "hello", nil, nil))
	member, memberTok := e.NewUser("member")
	joinCommunity(t, e, cid, memberTok)
	_ = member

	pin := envRef(5, "", seq)
	status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/messages", memberTok, []any{pin})
	if status != http.StatusForbidden {
		t.Fatalf("pin without the permission = %d", status)
	}
	if code := e.ErrCode(body); code != "E_FORBIDDEN" {
		t.Fatalf("code = %s", code)
	}
	if status, _ := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/messages", ownerTok, []any{pin}); status != http.StatusOK {
		t.Fatal("the owner could not pin")
	}
}

func TestMentionCountIsComputedAtWriteTime(t *testing.T) {
	e, cid, ownerTok := readableEnv(t)
	ch := readableChannel(t, e, cid, ownerTok)
	a, _ := e.NewUser("mentioned")
	body := "hej <@" + a.String() + "> och <@" + a.String() + "> igen och <@everyone>"
	seq := postReadable(t, e, ch, ownerTok, env9(0, body, nil, nil))
	rows, err := e.Repo.ListReadableMessages(t.Context(), ch, seq, 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListReadableMessages: %d rows, %v", len(rows), err)
	}
	// Two distinct targets: the user (twice, counted once) and @everyone.
	if rows[0].MentionCount != 2 {
		t.Fatalf("mention_count = %d, want 2", rows[0].MentionCount)
	}
}

func TestReadStateRoundTrips(t *testing.T) {
	e, cid, ownerTok := readableEnv(t)
	ch := readableChannel(t, e, cid, ownerTok)
	seq := postReadable(t, e, ch, ownerTok, env9(0, "hello", nil, nil))
	if status, _ := e.Do(http.MethodPut, "/v1/channels/"+ch.String()+"/read-state", ownerTok,
		[]any{seq}); status != http.StatusNoContent {
		t.Fatal("PUT read-state failed")
	}
	got, err := e.Repo.GetReadState(t.Context(), userOf(t, e, ownerTok), ch)
	if err != nil {
		t.Fatalf("GetReadState: %v", err)
	}
	if got != seq {
		t.Fatalf("read state = %d, want %d", got, seq)
	}
	// Moving it backwards is a no-op, not an error: a client with a stale tab
	// must not un-read a channel.
	if status, _ := e.Do(http.MethodPut, "/v1/channels/"+ch.String()+"/read-state", ownerTok,
		[]any{uint64(0)}); status != http.StatusNoContent {
		t.Fatal("PUT read-state backwards failed")
	}
	if got, _ := e.Repo.GetReadState(t.Context(), userOf(t, e, ownerTok), ch); got != seq {
		t.Fatalf("read state moved backwards to %d", got)
	}
}

// An edit re-franks: the tag is protocol/04's T over the NEW envelope's C, the
// editing device and the edit's time, under the current key, and the key id moves
// with it — a tag over bytes the instance no longer stores could never verify.
func TestAnEditRecomputesTheFrankingTag(t *testing.T) {
	f := newReadableFixture(t)
	e := f.e
	ch := readableChannel(t, e, f.cid, f.ownerTok)
	seq := postReadable(t, e, ch, f.ownerTok, env9(0, "first words", nil, nil))
	before, _ := e.Repo.ListReadableMessages(t.Context(), ch, seq, 1)

	e.Clk.Advance(90 * time.Second)
	edit := envWith(1, "second words", seqRef(seq), nil, nil)
	if status, body := e.Do(http.MethodPatch, fmt.Sprintf("/v1/channels/%s/messages/%d", ch, seq), f.ownerTok,
		[]any{edit}); status != http.StatusNoContent {
		t.Fatalf("PATCH = %d (%x)", status, body)
	}
	rows, _ := e.Repo.ListReadableMessages(t.Context(), ch, seq, 1)
	now := e.Clk.Now().Unix()
	c, err := api.Commitment(edit, make([]byte, 32))
	if err != nil {
		t.Fatalf("Commitment: %v", err)
	}
	keyID, key := f.keys.Current()
	want := api.Tag(key, ch, 0, seq, sessionDevice(t, e, f.ownerTok), c, now)
	r := rows[0]
	if !bytes.Equal(r.FrankingTag, want) || r.FrankingKeyID != keyID || !bytes.Equal(r.Envelope, edit) ||
		r.Body != "second words" || r.Edited == nil || *r.Edited != now || r.Created != before[0].Created {
		t.Fatalf("after edit = %+v; want tag %x at %d", r, want, now)
	}
	if bytes.Equal(r.FrankingTag, before[0].FrankingTag) {
		t.Fatal("the edit kept the original upload's tag")
	}
	frames := f.fan.delivered()
	payload := frames[len(frames)-1].frame.Payload
	if !bytes.Contains(payload, want) {
		t.Fatal("the edit's message.plain does not carry the new tag")
	}
}

// P2-D15's alias: a type-1 or type-2 envelope on POST names its target's seq in
// reply_to and lands in the same edit and delete as PATCH and DELETE.
func TestEditAndDeleteThroughTheEnvelopeAlias(t *testing.T) {
	e, cid, ownerTok := readableEnv(t)
	ch := readableChannel(t, e, cid, ownerTok)
	_, memberTok := e.NewUser("member")
	joinCommunity(t, e, cid, memberTok)
	seq := postReadable(t, e, ch, memberTok, env9(0, "original text", nil, nil))
	post := func(tok string, b []byte) (int, []byte) {
		return e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/messages", tok, []any{b})
	}

	// Without a target, an edit or a delete is malformed.
	for _, typ := range []uint8{1, 2} {
		body := ""
		if typ == 1 {
			body = "x"
		}
		if status, b := post(memberTok, env9(typ, body, nil, nil)); status != http.StatusBadRequest || e.ErrCode(b) != "E_ENVELOPE_SHAPE" {
			t.Fatalf("a type-%d envelope with no reply_to = %d %s, want 400 E_ENVELOPE_SHAPE", typ, status, e.ErrCode(b))
		}
	}
	// A reply_to that is a msg_id, not a seq reference, is malformed on a readable channel.
	msgRef, _ := cborx.Marshal([]any{uint64(1), id.New(), uint64(1), nil, id.New(), "x", []any{}, []any{}, make([]byte, 32)})
	if status, b := post(memberTok, msgRef); status != http.StatusBadRequest || e.ErrCode(b) != "E_ENVELOPE_SHAPE" {
		t.Fatalf("an edit naming a msg_id = %d %s, want 400 E_ENVELOPE_SHAPE", status, e.ErrCode(b))
	}
	if status, b := post(ownerTok, envRef(1, "hijacked", seq)); status != http.StatusForbidden || e.ErrCode(b) != "E_FORBIDDEN" {
		t.Fatalf("an alias edit by another user = %d %s, want 403 E_FORBIDDEN", status, e.ErrCode(b))
	}
	if status, _ := post(memberTok, envRef(1, "x", seq+50)); status != http.StatusNotFound {
		t.Fatalf("an alias edit of an unknown seq = %d, want 404", status)
	}
	if status, b := post(memberTok, envRef(1, "corrected text", seq)); status != http.StatusNoContent {
		t.Fatalf("the author's alias edit = %d (%x)", status, b)
	}
	rows, _ := e.Repo.ListReadableMessages(t.Context(), ch, seq, 10)
	if len(rows) != 1 || rows[0].Body != "corrected text" || rows[0].Edited == nil {
		t.Fatalf("after the alias edit = %d rows, %+v (an edit is applied in place, not appended)", len(rows), rows)
	}

	if status, b := post(ownerTok, envRef(2, "", seq)); status != http.StatusForbidden || e.ErrCode(b) != "E_NOT_UPLOADER" {
		t.Fatalf("an alias delete by the owner = %d %s, want 403 E_NOT_UPLOADER", status, e.ErrCode(b))
	}
	if status, b := post(memberTok, envRef(2, "", seq)); status != http.StatusNoContent {
		t.Fatalf("the author's alias delete = %d (%x)", status, b)
	}
	rows, _ = e.Repo.ListReadableMessages(t.Context(), ch, seq, 10)
	if len(rows) != 1 || rows[0].Deleted == nil || len(rows[0].Envelope) != 0 {
		t.Fatalf("after the alias delete = %+v", rows)
	}
	if status, _ := post(memberTok, envRef(1, "revived", seq)); status != http.StatusNotFound {
		t.Fatalf("an alias edit of a deleted message = %d, want 404", status)
	}
}

// R29: delete-for-everyone is the uploader's alone. Holding manage_messages — or
// owning the community — does not let anyone else delete a readable message;
// moderator deletion needs a signed moderation event (follow-up card 2).
func TestOnlyTheUploaderDeletes(t *testing.T) {
	e, cid, ownerTok := readableEnv(t)
	ch := readableChannel(t, e, cid, ownerTok)
	_, memberTok := e.NewUser("member")
	joinCommunity(t, e, cid, memberTok)
	seq := postReadable(t, e, ch, memberTok, env9(0, "mine", nil, nil))
	path := fmt.Sprintf("/v1/channels/%s/messages/%d", ch, seq)
	if status, b := e.Do(http.MethodDelete, path, ownerTok, nil); status != http.StatusForbidden || e.ErrCode(b) != "E_NOT_UPLOADER" {
		t.Fatalf("the owner's delete of a member's message = %d %s, want 403 E_NOT_UPLOADER", status, e.ErrCode(b))
	}
	if status, _ := e.Do(http.MethodDelete, path, memberTok, nil); status != http.StatusNoContent {
		t.Fatalf("the uploader's delete = %d", status)
	}
}

// Reactions and pins are appended like messages, but neither a reaction's emoji
// nor a pin is search content and neither carries a mention count.
func TestReactionsArePostedAndNotIndexed(t *testing.T) {
	e, cid, ownerTok := readableEnv(t)
	ch := readableChannel(t, e, cid, ownerTok)
	seq := postReadable(t, e, ch, ownerTok, env9(0, "hello", nil, nil))
	rseq := postReadable(t, e, ch, ownerTok, envRef(3, "<@everyone>", seq))
	rows, _ := e.Repo.ListReadableMessages(t.Context(), ch, rseq, 1)
	if len(rows) != 1 || rows[0].Body != "" || rows[0].MentionCount != 0 {
		t.Fatalf("reaction row = %+v", rows)
	}
}

// envWith is env9 with reply_to set.
func envWith(typ uint8, body string, replyTo id.ID, atts []api.Attachment, prevs []api.Preview) []byte {
	b, err := cborx.Marshal([]any{
		uint64(1), id.New(), uint64(typ), nil, replyTo, body, attsToAny(atts), prevsToAny(prevs), make([]byte, 32),
	})
	if err != nil {
		panic(err)
	}
	return b
}

// L-CORE-30 (web-2b task 1): a type 1..6 envelope names its target and carries no files, as
// protocol/04 requires of every receiver. Attacker statement: only the envelope's own sender can
// build one that fails, so the refusal is never something one member can do to another.
func TestFoldTypesNameATargetAndCarryNoFiles(t *testing.T) {
	att := api.Attachment{BlobID: make([]byte, 32), Key: make([]byte, 32), Nonce: make([]byte, 12), Size: 1, Mime: "image/png", Name: "map.png"}
	prev := api.Preview{URL: "https://x", Title: "t", Description: "d"}
	bodies := map[uint8]string{1: "fixed", 2: "", 3: "👍", 4: "👍", 5: "", 6: ""}
	for typ := uint8(1); typ <= 6; typ++ {
		body := bodies[typ]
		cases := []struct {
			name string
			b    []byte
			code string
		}{
			{"no reply_to", env9(typ, body, nil, nil), "E_ENVELOPE_SHAPE"},
			{"with a target", envWith(typ, body, seqRef(1), nil, nil), ""},
			{"with an attachment", envWith(typ, body, seqRef(1), []api.Attachment{att}, nil), "E_ENVELOPE_SHAPE"},
			{"with a preview", envWith(typ, body, seqRef(1), nil, []api.Preview{prev}), "E_ENVELOPE_SHAPE"},
		}
		for _, tc := range cases {
			t.Run(fmt.Sprintf("type %d %s", typ, tc.name), func(t *testing.T) {
				_, err := api.ParseEnvelope(tc.b)
				if tc.code == "" {
					if err != nil {
						t.Fatalf("ParseEnvelope: %v", err)
					}
					return
				}
				if got := codeOf(err); got != tc.code {
					t.Fatalf("code = %s (%v), want %s", got, err, tc.code)
				}
			})
		}
	}
	if _, err := api.ParseEnvelope(env9(0, "hi", []api.Attachment{att}, nil)); err != nil {
		t.Fatalf("a type-0 message with a file: %v", err)
	}
	if _, err := api.ParseEnvelope(env9(2, "x", nil, nil)); codeOf(err) != "E_ENVELOPE_LIMIT" {
		t.Fatalf("a type-2 envelope with a body = %v, want E_ENVELOPE_LIMIT (the body limit is checked first)", err)
	}
}
