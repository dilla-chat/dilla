package client

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
)

func TestRedeemAutoModerationGrantSendsOnlyConfigDigestAndAcceptsReceipt(t *testing.T) {
	if got, ok := expectedStatus(http.MethodPost, "bot-runtime/installations/x/automod-grants/redeem"); !ok || got != 200 {
		t.Fatalf("route status = %d,%v", got, ok)
	}
	installation, community, group, request := testID(11), testID(12), testID(13), testID(14)
	actor, device := testID(15), testID(16)
	hash := [32]byte{1, 2, 3}
	receipt := testID(17)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, http.StatusOK, discovery(testID(99)))
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/bot-runtime/installations/"+installation.String()+"/automod-grants/redeem" || r.Header.Get("Authorization") != "Bearer bot-session" {
			t.Errorf("request = %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		fields, err := decode(body, 9)
		if err != nil {
			t.Errorf("body: %v", err)
			return
		}
		if got, ok := fields[8].([]byte); !ok || len(got) != 32 || string(got) != string(hash[:]) {
			t.Errorf("config hash field = %#v", fields[8])
		}
		reply(w, http.StatusOK, []any{uint64(1), receipt})
	}, 0)
	s, _ := c.Session("bot-session")
	accepted, got, err := s.RedeemAutoModerationGrant(context.Background(), installation, "opaque-grant", community, group, request, 0, 1_900_000_000, actor, device, hash)
	if err != nil || !accepted || got != receipt {
		t.Fatalf("accepted=%v receipt=%v err=%v", accepted, got, err)
	}
}

func TestRedeemAutoModerationGrantRejectsMalformedResponse(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, http.StatusOK, discovery(testID(99)))
			return
		}
		reply(w, http.StatusOK, []any{false, id.Zero})
	}, 0)
	s, _ := c.Session("bot-session")
	if _, _, err := s.RedeemAutoModerationGrant(context.Background(), testID(1), "token", testID(2), testID(3), testID(4), 0, 10, testID(5), testID(6), [32]byte{}); err == nil {
		t.Fatal("malformed grant response accepted")
	}
}
