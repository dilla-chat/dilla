package api_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
)

// protocol/01 § Joining batches at most 256 Adds into one commit, and row 5 addresses the commit's
// Welcome to each added device separately: `welcomes([[device_id, blob]])`. One Welcome for 256
// joiners is about 30 KiB (measured by join_storm_256_batched), so a full batch is about 256 copies
// of it — some 8 MiB, four times the 2 MiB the other delivery-service routes accept. The commit
// route's cap must admit the batch the protocol itself mandates, or no join storm can complete.
func TestAFullBatchOfWelcomesIsNotRefusedAsTooLarge(t *testing.T) {
	h := newGroupsAPI(t)
	h.mustCreate(t)
	member := h.memberToken(t)

	welcome := bytes.Repeat([]byte{0x5a}, 32<<10)
	welcomes := make([]any, 0, 256)
	for range 256 {
		device := id.New()
		welcomes = append(welcomes, []any{device[:], welcome})
	}
	body := mustCBOR(t, []any{h.groupEpoch(t), []byte{0x00}, []byte{0x00}, welcomes, nil})
	res := h.do(t, http.MethodPost, "/v1/groups/"+h.groupID.String()+"/commit", member, body)
	if res.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("a %d-byte commit carrying 256 Welcomes was refused as too large", len(body))
	}
	// The commit itself is not a commit, so the delivery service refuses it on its own terms.
	if res.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want the delivery service's 422: %s", res.Code, res.Body.String())
	}
}

// …but the headroom above 2 MiB is the Welcomes' alone (deviation B37). A commit, GroupInfo and
// ratchet tree that together exceed the delivery service's own 2 MiB cap are refused as they are
// on every other delivery-service route, however few Welcomes ride along: the 16 MiB exists for a
// full batch of Welcomes, not for any enrolled device to upload 16 MiB of anything.
func TestTheWelcomeHeadroomDoesNotWidenTheRestOfTheCommit(t *testing.T) {
	h := newGroupsAPI(t)
	h.mustCreate(t)
	member := h.memberToken(t)

	for name, body := range map[string][]byte{
		"a 3 MiB commit": mustCBOR(t, []any{h.groupEpoch(t), bytes.Repeat([]byte{0x00}, 3<<20),
			[]byte{0x00}, []any{}, nil}),
		"a 3 MiB ratchet tree": mustCBOR(t, []any{h.groupEpoch(t), []byte{0x00}, []byte{0x00},
			[]any{}, bytes.Repeat([]byte{0x00}, 3<<20)}),
		"a commit and a GroupInfo of 1.5 MiB each": mustCBOR(t, []any{h.groupEpoch(t),
			bytes.Repeat([]byte{0x00}, 3<<19), bytes.Repeat([]byte{0x00}, 3<<19), []any{}, nil}),
	} {
		res := h.do(t, http.MethodPost, "/v1/groups/"+h.groupID.String()+"/commit", member, body)
		if res.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s: status = %d, want 413: %s", name, res.Code, res.Body.String())
		}
	}
}

// A device that holds no leaf in the group is refused BEFORE its body is read: the stream here is a
// commit head followed by bytes that are not CBOR at all, and the answer is the delivery service's
// E_LEAF_NOT_CURRENT, not the decoder's E_INVALID_REQUEST.
func TestACommitFromANonMemberIsRefusedBeforeItsBodyIsRead(t *testing.T) {
	h := newGroupsAPI(t)
	h.mustCreate(t)
	body := append([]byte{0x85, 0x00}, bytes.Repeat([]byte{0xff}, 64)...)
	res := h.do(t, http.MethodPost, "/v1/groups/"+h.groupID.String()+"/commit", h.session, body)
	if got := errorCode(t, res); got != "E_LEAF_NOT_CURRENT" {
		t.Fatalf("code = %s (status %d), want E_LEAF_NOT_CURRENT before the body is decoded", got, res.Code)
	}
}

// A member's commit for an epoch the instance has not reached is refused from the epoch at the head
// of the stream, before the rest is read.
func TestACommitForTheWrongEpochIsRefusedBeforeItsBodyIsRead(t *testing.T) {
	h := newGroupsAPI(t)
	h.mustCreate(t)
	member := h.memberToken(t)
	body := append([]byte{0x85, 0x19, 0x01, 0x00}, bytes.Repeat([]byte{0xff}, 64)...) // epoch 256
	res := h.do(t, http.MethodPost, "/v1/groups/"+h.groupID.String()+"/commit", member, body)
	if res.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want the delivery service's 422 epoch_ahead: %s", res.Code, res.Body.String())
	}
	if got := errorCode(t, res); got != "E_COMMIT_INVALID" {
		t.Fatalf("code = %s, want E_COMMIT_INVALID", got)
	}
}

// protocol/01 § Joining's MAX_ADDS: a commit addresses at most 256 Welcomes.
func TestACommitWithMoreThanMaxAddsWelcomesIsRefused(t *testing.T) {
	h := newGroupsAPI(t)
	h.mustCreate(t)
	member := h.memberToken(t)
	welcomes := make([]any, 0, 257)
	for range 257 {
		device := id.New()
		welcomes = append(welcomes, []any{device[:], []byte{0x01}})
	}
	body := mustCBOR(t, []any{h.groupEpoch(t), []byte{0x00}, []byte{0x00}, welcomes, nil})
	res := h.do(t, http.MethodPost, "/v1/groups/"+h.groupID.String()+"/commit", member, body)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", res.Code, res.Body.String())
	}
	if got := errorCode(t, res); got != "E_INVALID_REQUEST" {
		t.Fatalf("code = %s, want E_INVALID_REQUEST", got)
	}
}

// budgetReader serves a fixed head and fails the test if the handler reads a byte past it: a body
// whose head is not `0x85` and a uint epoch is refused from the head alone.
type budgetReader struct {
	t    *testing.T
	head []byte
	off  int
}

func (b *budgetReader) Read(p []byte) (int, error) {
	if b.off >= len(b.head) {
		b.t.Errorf("the handler read past the %d-byte head of a body that cannot be a commit", len(b.head))
		return 0, io.EOF
	}
	n := copy(p, b.head[b.off:])
	b.off += n
	return n, nil
}

// Every valid commit body starts with 0x85 and a uint epoch. A body that does not is refused as
// E_INVALID_REQUEST before DecodeBody reads up to 16 MiB of it, whether or not the device is a member.
func TestACommitWhoseHeadDoesNotParseIsRefusedWithoutReadingTheRest(t *testing.T) {
	h := newGroupsAPI(t)
	h.mustCreate(t)
	head := append([]byte{0x86, 0x00}, bytes.Repeat([]byte{0xff}, 30)...)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/v1/groups/"+h.groupID.String()+"/commit", &budgetReader{t: t, head: head})
	req.Header.Set("Content-Type", "application/cbor")
	req.Header.Set("Authorization", "Bearer "+h.session)
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	if got := errorCode(t, rec); got != "E_INVALID_REQUEST" {
		t.Fatalf("code = %s (status %d), want E_INVALID_REQUEST", got, rec.Code)
	}
}

// …and the raised cap is still a cap.
func TestACommitBodyAboveTheBatchCapIsTooLarge(t *testing.T) {
	h := newGroupsAPI(t)
	h.mustCreate(t)
	member := h.memberToken(t)

	body := mustCBOR(t, []any{h.groupEpoch(t), bytes.Repeat([]byte{0x00}, 17<<20), []byte{0x00},
		[]any{}, nil})
	res := h.do(t, http.MethodPost, "/v1/groups/"+h.groupID.String()+"/commit", member, body)
	if res.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 for a %d-byte body", res.Code, len(body))
	}
}
