package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	botprotocol "github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

func TestBotMessageRejectsMalformedAndOversizeBodiesBeforeSessionOrDS(t *testing.T) {
	for _, tc := range []struct {
		name string
		max  int64
		body []byte
		code string
		want int
	}{
		{name: "noncanonical body", max: 128, body: []byte{0x9f, 0xff}, code: "E_INVALID_REQUEST", want: http.StatusBadRequest},
		{name: "over body cap", max: 8, body: bytes.Repeat([]byte{0}, 9), code: "E_TOO_LARGE", want: http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Groups{MaxBody: tc.max}
			r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/groups/00000000000000000000000000000001/bot/messages", bytes.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/cbor")
			r.SetPathValue("group", "00000000000000000000000000000001")
			w := httptest.NewRecorder()
			h.postBotMessage(w, r)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d: %x", w.Code, tc.want, w.Body.Bytes())
			}
			if got := botRefusalCode(t, w); got != tc.code {
				t.Fatalf("error code = %q, want %q", got, tc.code)
			}
		})
	}
}

func TestBotMessageReceiptWritesDirectTwelveFieldCBOR(t *testing.T) {
	community, group, operation, attempt := id.New(), id.New(), id.New(), id.New()
	receipt, err := botprotocol.EncodeReceipt(botprotocol.SendReceipt{
		OperationID: operation, AttemptID: attempt, CommunityID: community, GroupID: group,
		CiphertextHash: bytes.Repeat([]byte{0x5a}, 32), Class: 1,
		Outcome: botprotocol.ReceiptClosedNotAccepted, ExpectedGeneration: 9, Epoch: 12,
	})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	writeBotReceipt(w, receipt, operation, attempt, community, group)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %x", w.Code, w.Body.Bytes())
	}
	if !bytes.Equal(w.Body.Bytes(), receipt) {
		t.Fatalf("receipt body = %x, want exact original twelve-field receipt %x", w.Body.Bytes(), receipt)
	}
	if got := w.Header().Get("Content-Type"); got != "application/cbor" {
		t.Fatalf("content type = %q, want application/cbor", got)
	}
}

func botRefusalCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var fields []any
	if err := cborx.Unmarshal(w.Body.Bytes(), &fields); err != nil || len(fields) == 0 {
		t.Fatalf("decode refusal: %v; body=%x", err, w.Body.Bytes())
	}
	code, _ := fields[0].(string)
	return code
}
