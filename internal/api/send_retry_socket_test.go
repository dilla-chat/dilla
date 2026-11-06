package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/api/sockettest"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
)

// messageIdem is the client's idempotency key for a message send (packages/client-core/src/http/routes.ts messageIdem):
// the first 16 bytes of SHA-256("dilla/message-idem/v1\0" || msgId || body).
func messageIdem(msgID, body []byte) []byte {
	sum := sha256.Sum256(append(append([]byte("dilla/message-idem/v1\x00"), msgID...), body...))
	return sum[:16]
}

// TestMessageIdemMatchesTheClientVector pins the derivation against the vector routes.test.ts checks too.
func TestMessageIdemMatchesTheClientVector(t *testing.T) {
	msgID := bytes.Repeat([]byte{0x31}, 16)
	body := bytes.Repeat([]byte{0x44}, 40)
	if got := hex.EncodeToString(messageIdem(msgID, body)); got != "0cf6fce80c1e9d827699c5296e842471" {
		t.Fatalf("message idem = %s, want the shared vector", got)
	}
}

// TestSendRetryOverSocketIsAnsweredFromTheReplayRecord: the server knows no msg_id on this route; what dedupes a send
// written again on a new connection is the (device, idem, body) replay record, for ReplayWindow. A retry past the window
// runs again and stores a second copy: that is why the client stops resending a written mutation after 4 minutes
// (client-core RESEND_WINDOW_MS) and hands the lost answer to the send lane's catch-up.
func TestSendRetryOverSocketIsAnsweredFromTheReplayRecord(t *testing.T) {
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
	idem := messageIdem([]byte("msg-0123456789ab"), body)
	send := func(addr string) (uint64, int) {
		resp := sockettest.DispatchFrom(context.Background(), disp, sess, addr, http.MethodPost, route, body, idem)
		if resp.Status != http.StatusOK {
			t.Fatalf("status = %d, want 200: %x", resp.Status, resp.Body)
		}
		var out struct {
			_           struct{} `cbor:",toarray"`
			Seq         uint64
			FrankingTag []byte
			RecvTS      uint64
		}
		if err := cborx.Unmarshal(resp.Body, &out); err != nil {
			t.Fatal(err)
		}
		rows, err := h.deps.Repo.ListAppMessages(t.Context(), h.groupID, 1, 100)
		if err != nil {
			t.Fatalf("ListAppMessages: %v", err)
		}
		return out.Seq, len(rows)
	}
	first, stored := send("127.0.0.1:40001")
	// The connection dropped; the retry arrives on a new one (another client port) with the same idem.
	again, stored2 := send("127.0.0.1:40002")
	if first != again || stored != 1 || stored2 != 1 {
		t.Fatalf("retry within the window: seq %d then %d, %d and %d stored; want one stored message", first, again, stored, stored2)
	}
	h.deps.Clock.(*clock.Fake).Advance(6 * time.Minute)
	late, stored3 := send("127.0.0.1:40003")
	if late == first || stored3 != 2 {
		t.Fatalf("retry past the window: seq %d, %d stored; want a second stored copy (the record is gone)", late, stored3)
	}
}
