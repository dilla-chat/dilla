package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/dilla-chat/dilla/internal/cborx"
)

func TestReplyAndStateHintUseTheBotRuntimeRoutes(t *testing.T) {
	if got, ok := expectedStatus(http.MethodPost, "bot-runtime/replies"); !ok || got != 204 {
		t.Fatalf("replies route status = %d,%v", got, ok)
	}
	if got, ok := expectedStatus(http.MethodPost, "bot-runtime/state-hints"); !ok || got != 200 {
		t.Fatalf("state-hints route status = %d,%v", got, ok)
	}
	device, request, scope := testID(31), testID(32), testID(33)
	sealed := bytes.Repeat([]byte{0x42}, 90)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, http.StatusOK, discovery(testID(99)))
			return
		}
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer bot-session" {
			t.Errorf("request = %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/v1/bot-runtime/replies":
			want, _ := cborx.Marshal([]any{device[:], request[:], uint64(2), sealed, uint64(1)})
			if !bytes.Equal(body, want) {
				t.Errorf("reply body = %x, want %x", body, want)
			}
			w.WriteHeader(http.StatusNoContent)
		case "/v1/bot-runtime/state-hints":
			want, _ := cborx.Marshal([]any{uint64(StateScopeChannel), scope[:]})
			if !bytes.Equal(body, want) {
				t.Errorf("hint body = %x, want %x", body, want)
			}
			reply(w, http.StatusOK, []uint64{4})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}, 0)
	s, _ := c.Session("bot-session")
	if err := s.Reply(context.Background(), device, request, 2, sealed, true); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	rev, err := s.StateHint(context.Background(), StateScopeChannel, scope)
	if err != nil || rev != 4 {
		t.Fatalf("StateHint = %d, %v; want 4", rev, err)
	}
	if err := s.Reply(context.Background(), device, request, 0, make([]byte, maxSealedReply+1), false); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("an oversized reply = %v, want ErrInvalidConfig", err)
	}
	if _, err := s.StateHint(context.Background(), 3, scope); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("the catalog kind = %v, want ErrInvalidConfig", err)
	}
}
