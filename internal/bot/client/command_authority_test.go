package client

import (
	"context"
	"net/http"
	"testing"

	"github.com/jonasthim/dilla/internal/cborx"
)

func TestBotActorChannelAuthorityParsesBoundCurrentPermissions(t *testing.T) {
	community, actor, channel := testID(37), testID(38), testID(39)
	var gotPath string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, http.StatusOK, discovery(testID(99)))
			return
		}
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/cbor")
		body, err := cborx.Marshal([]any{community, actor, channel, uint64(1<<6 | 1<<2)})
		if err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}, 0)
	s, err := c.Session("authority-token")
	if err != nil {
		t.Fatal(err)
	}
	bits, present, err := s.BotActorChannelAuthority(context.Background(), community, actor, channel)
	if err != nil || !present || bits != (1<<6|1<<2) {
		t.Fatalf("channel authority = (%d, %v, %v)", bits, present, err)
	}
	wantPath := "/v1/communities/" + community.String() + "/bot/actors/" + actor.String() + "/channels/" + channel.String() + "/authority"
	if gotPath != wantPath {
		t.Fatalf("request path = %q, want %q", gotPath, wantPath)
	}
}

func TestBotActorChannelAuthorityRejectsMalformedBindingAndHides404(t *testing.T) {
	community, actor, channel := testID(37), testID(38), testID(39)
	for _, tc := range []struct {
		name   string
		status int
		body   any
	}{
		{name: "foreign community", status: http.StatusOK, body: []any{testID(40), actor, channel, uint64(4)}},
		{name: "foreign actor", status: http.StatusOK, body: []any{community, testID(40), channel, uint64(4)}},
		{name: "foreign channel", status: http.StatusOK, body: []any{community, actor, testID(40), uint64(4)}},
		{name: "unknown permission", status: http.StatusOK, body: []any{community, actor, channel, uint64(1 << 30)}},
		{name: "extra field", status: http.StatusOK, body: []any{community, actor, channel, uint64(4), uint64(1)}},
		{name: "absent", status: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/instance" {
					reply(w, http.StatusOK, discovery(testID(99)))
					return
				}
				if tc.status == http.StatusNotFound {
					reply(w, tc.status, []any{"E_NOT_FOUND", "no such object", nil})
					return
				}
				body, err := cborx.Marshal(tc.body)
				if err != nil {
					t.Fatal(err)
				}
				w.Header().Set("Content-Type", "application/cbor")
				w.WriteHeader(tc.status)
				_, _ = w.Write(body)
			}, 0)
			s, err := c.Session("authority-token")
			if err != nil {
				t.Fatal(err)
			}
			bits, present, err := s.BotActorChannelAuthority(context.Background(), community, actor, channel)
			if tc.status == http.StatusNotFound {
				if err != nil || present || bits != 0 {
					t.Fatalf("404 = (%d, %v, %v), want absent", bits, present, err)
				}
			} else if err == nil || present || bits != 0 {
				t.Fatalf("malformed authority = (%d, %v, %v), want protocol error", bits, present, err)
			}
		})
	}
}

func TestExpectedStatusAllowsOnlyTheChannelAuthorityRouteShape(t *testing.T) {
	if status, ok := expectedStatus(http.MethodGet, "communities/c/bot/actors/a/channels/ch/authority"); !ok || status != http.StatusOK {
		t.Fatalf("channel authority expected status = (%d, %v), want (200, true)", status, ok)
	}
	for _, path := range []string{
		"communities/c/bot/actors/a/channels/ch/other",
		"communities/c/bot/actors/a/channels/ch/authority/extra",
		"communities/c/bot/actors/a/channels/ch/authority",
	} {
		method := http.MethodGet
		if path == "communities/c/bot/actors/a/channels/ch/authority" {
			method = http.MethodPost
		}
		if _, ok := expectedStatus(method, path); ok {
			t.Fatalf("unexpectedly allowed %s %s", method, path)
		}
	}
}
