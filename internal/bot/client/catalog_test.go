package client

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
)

func TestPublishCatalogAndCommandKeyUseTheBotRuntimeRoutes(t *testing.T) {
	if got, ok := expectedStatus(http.MethodPut, "bot-runtime/communities/x/catalog"); !ok || got != 200 {
		t.Fatalf("catalog route status = %d,%v", got, ok)
	}
	if got, ok := expectedStatus(http.MethodPut, "bot-runtime/communities/x/command-key"); !ok || got != 204 {
		t.Fatalf("command-key route status = %d,%v", got, ok)
	}
	community := testID(21)
	commands := []byte{0x80}
	publication := bytes.Repeat([]byte{0x87}, 180)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, http.StatusOK, discovery(testID(99)))
			return
		}
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPut || r.Header.Get("Authorization") != "Bearer bot-session" {
			t.Errorf("request = %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/v1/bot-runtime/communities/" + community.String() + "/catalog":
			if !bytes.Equal(body, commands) {
				t.Errorf("catalog body = %x", body)
			}
			reply(w, http.StatusOK, []uint64{3})
		case "/v1/bot-runtime/communities/" + community.String() + "/command-key":
			if !bytes.Equal(body, publication) {
				t.Errorf("publication body = %x", body)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}, 0)
	s, _ := c.Session("bot-session")
	rev, err := s.PublishCatalog(context.Background(), community, commands)
	if err != nil || rev != 3 {
		t.Fatalf("PublishCatalog = %d, %v; want 3", rev, err)
	}
	if err := s.PublishCommandKey(context.Background(), community, publication); err != nil {
		t.Fatalf("PublishCommandKey: %v", err)
	}
}
