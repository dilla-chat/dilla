package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/moderation/operationid"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

func TestAcceptBotModerationActionUsesBoundRouteAndReceipt(t *testing.T) {
	installation, group := testID(7), testID(8)
	request := operationid.Automatic(group, 12)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, http.StatusOK, discovery(testID(99)))
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/bot-runtime/installations/"+installation.String()+"/moderation/actions" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		want, _ := cborx.Marshal([]any{request, group, uint64(12), "timeout", []any{"message_rate"}, uint64(600000), uint64(1800000020)})
		if got := readBody(t, r); !bytes.Equal(got, want) {
			t.Errorf("request body = %x, want %x", got, want)
		}
		reply(w, http.StatusOK, []any{testID(22), uint64(45), uint64(1800000000)})
	}, 0)
	s, _ := c.Session("device-token")
	receipt, err := s.AcceptBotModerationAction(context.Background(), installation, request, group, 12, "timeout", []string{"message_rate"}, 600000, 1800000020)
	if err != nil || receipt.ActionID != testID(22) || receipt.Revision != 45 || receipt.Expires == nil || *receipt.Expires != 1800000000 {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
}

func TestAcceptBotModerationActionRejectsInvalidAuthorityInputs(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, http.StatusOK, discovery(testID(99)))
			return
		}
		t.Errorf("invalid input reached server")
	}, 0)
	s, _ := c.Session("device-token")
	group := testID(2)
	if _, err := s.AcceptBotModerationAction(context.Background(), id.ID{}, operationid.Automatic(group, 1), group, 1, "warn", []string{"message_rate"}, 0, 1800000020); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("zero installation error=%v", err)
	}
	if _, err := s.AcceptBotModerationAction(context.Background(), testID(3), testID(4), group, 1, "warn", []string{"message_rate"}, 0, 1800000020); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("forged operation error=%v", err)
	}
	if _, err := s.AcceptBotModerationAction(context.Background(), testID(3), operationid.Automatic(group, 1), group, 1, "timeout", []string{"message_rate"}, 0, 1800000020); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("zero timeout error=%v", err)
	}
}

func readBody(t *testing.T, r *http.Request) []byte {
	t.Helper()
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
