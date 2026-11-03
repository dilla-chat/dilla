package client

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
)

func TestAcceptedModerationRemovalUsesBoundReadAndRejectsMalformedReceipt(t *testing.T) {
	installation, request := testID(1), testID(2)
	path := "/v1/bot-runtime/installations/" + installation.String() + "/moderation/removals/" + request.String()
	for _, tc := range []struct {
		name  string
		wire  []any
		valid bool
	}{
		{"accepted", []any{testID(3), testID(4), testID(5), testID(6), testID(7), uint64(8), uint64(9), uint64(10)}, true},
		{"missing actor", []any{testID(3), testID(4), testID(5), id.ID{}, testID(7), uint64(8), uint64(9), uint64(10)}, false},
		{"zero revision", []any{testID(3), testID(4), testID(5), testID(6), testID(7), uint64(8), uint64(0), uint64(10)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/instance" {
					reply(w, http.StatusOK, discovery(testID(99)))
					return
				}
				if r.Method != http.MethodGet || r.URL.Path != path || r.Header.Get("Authorization") != "Bearer bot-session" {
					t.Errorf("request = %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
				}
				reply(w, http.StatusOK, tc.wire)
			}, 0)
			s, _ := c.Session("bot-session")
			got, err := s.AcceptedModerationRemoval(context.Background(), installation, request)
			if tc.valid && (err != nil || got.ActorUserID != testID(6) || got.Revision != 9) {
				t.Fatalf("receipt=%+v err=%v", got, err)
			}
			if !tc.valid && !errors.Is(err, ErrProtocol) {
				t.Fatalf("invalid response err=%v", err)
			}
		})
	}
}
