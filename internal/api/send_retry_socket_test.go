package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/api/sockettest"
	"github.com/dilla-chat/dilla/internal/cborx"
)

// TestSendRetryOverSocketDedupesByMsgID is the server half of Review Focus 1. A message send carries an idem derived
// from the message's msg_id; a client that lost its connection after the send and retries it on the next connection
// sends the same idem. The device's replay record answers the retry with the first answer: one stored message, the
// same seq. (The client half, a stable idem per message, is client-core's `send idem is stable per message`.)
func TestSendRetryOverSocketDedupesByMsgID(t *testing.T) {
	h := newGroupsAPI(t)
	h.mustCreate(t)
	token := h.memberToken(t)
	sess, err := h.deps.Sessions.Resolve(t.Context(), token)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	disp := api.NewSocketDispatcher(h.mux, api.SocketDispatchOptions{Clock: h.deps.Clock})
	route := "/v1/groups/" + h.groupID.String() + "/message"
	body := h.uploadBody(t, 64)
	msgID := []byte("msg-0123456789ab") // the idem is the message id's bytes
	r1 := sockettest.DispatchFrom(context.Background(), disp, sess, "127.0.0.1:40001", http.MethodPost, route, body, msgID)
	// The connection dropped; the retry arrives on a new one (another client port) with the same idem.
	r2 := sockettest.DispatchFrom(context.Background(), disp, sess, "127.0.0.1:40002", http.MethodPost, route, body, msgID)
	if r1.Status != http.StatusOK || r2.Status != http.StatusOK {
		t.Fatalf("statuses = %d, %d, want 200 and 200: %x %x", r1.Status, r2.Status, r1.Body, r2.Body)
	}
	var a, b struct {
		_           struct{} `cbor:",toarray"`
		Seq         uint64
		FrankingTag []byte
		RecvTS      uint64
	}
	if err := cborx.Unmarshal(r1.Body, &a); err != nil {
		t.Fatal(err)
	}
	if err := cborx.Unmarshal(r2.Body, &b); err != nil {
		t.Fatal(err)
	}
	if a.Seq != b.Seq {
		t.Fatalf("the retry got seq %d, the first send %d: the same msg_id stored two messages", b.Seq, a.Seq)
	}
	rows, err := h.deps.Repo.ListAppMessages(t.Context(), h.groupID, 1, 100)
	if err != nil {
		t.Fatalf("ListAppMessages: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d stored messages after a retry with the same msg_id, want 1", len(rows))
	}
}
