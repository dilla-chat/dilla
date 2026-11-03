package client

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/cborx"
)

func TestBotInteractionGroupReadsUseInstallationScopedRoutes(t *testing.T) {
	const token = "interaction-read-token"
	installation, group := testID(7), testID(8)
	wantBase := "/v1/bot-runtime/installations/" + installation.String() + "/interaction-groups/" + group.String()
	var seen int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, http.StatusOK, discovery(testID(99)))
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("missing session bearer on %s", r.URL.Path)
		}
		seen++
		switch {
		case r.Method == http.MethodGet && r.URL.Path == wantBase+"/info":
			payload := []any{uint64(2), []byte{1, 2}, make([]byte, 32), uint64(4)}
			reply(w, http.StatusOK, payload)
		case r.Method == http.MethodGet && (r.URL.Path == wantBase+"/messages" || r.URL.Path == wantBase+"/handshakes"):
			if r.URL.Query().Get("from") != "1" || r.URL.Query().Get("limit") != "8" {
				t.Errorf("page query = %s", r.URL.RawQuery)
			}
			reply(w, http.StatusOK, []any{})
		case r.Method == http.MethodPost && r.URL.Path == wantBase+"/cursor":
			var body []any
			if err := cborx.Unmarshal(readRequest(t, r), &body); err != nil || len(body) != 2 || body[0] != uint64(3) || body[1] != uint64(2) {
				t.Errorf("cursor body %v err=%v", body, err)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			reply(w, http.StatusNotFound, []any{"E_NOT_FOUND", "", nil})
		}
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c, err := New(Config{BaseURL: "https://example.invalid", HTTPClient: &http.Client{Transport: rewriteTransport{base: srv.Client().Transport, host: u.Host}}, InstanceID: testID(99), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, err := c.Session(token)
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.BotInteractionGroupInfo(context.Background(), installation, group)
	if err != nil || info.Epoch != 2 || info.NextSequence != 4 {
		t.Fatalf("info=%+v err=%v", info, err)
	}
	for _, kind := range []string{"messages", "handshakes"} {
		if page, e := s.BotInteractionGroupPage(context.Background(), installation, group, kind, 1, 8); e != nil || len(page.Rows) != 0 {
			t.Fatalf("%s page=%+v err=%v", kind, page, e)
		}
	}
	if err := s.AdvanceBotInteractionGroupCursor(context.Background(), installation, group, 3, 2); err != nil {
		t.Fatal(err)
	}
	if seen != 4 {
		t.Fatalf("scoped route calls = %d, want 4", seen)
	}
}

func TestBotInteractionHealUsesInstallationScopedRoutes(t *testing.T) {
	installation, group := testID(7), testID(8)
	base := "/v1/bot-runtime/installations/" + installation.String() + "/interaction-groups/" + group.String() + "/heal"
	var posted bool
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, http.StatusOK, discovery(testID(99)))
			return
		}
		if r.URL.Path != base {
			t.Errorf("unexpected route %s %s", r.Method, r.URL.Path)
			reply(w, http.StatusNotFound, []any{"E_NOT_FOUND", "", nil})
			return
		}
		switch r.Method {
		case http.MethodGet:
			reply(w, http.StatusOK, []any{uint64(2), uint64(9), uint64(3), uint64(8), uint64(1)})
		case http.MethodPost:
			posted = true
			var body []any
			if err := cborx.Unmarshal(readRequest(t, r), &body); err != nil || len(body) != 3 ||
				!bytes.Equal(body[0].([]byte), []byte{1, 2}) || len(body[1].([]any)) != 0 || body[2] != nil {
				t.Errorf("heal request = %v, %v", body, err)
			}
			reply(w, http.StatusOK, []any{uint64(2), uint64(10)})
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}, time.Second)
	s, err := c.Session("bot-device")
	if err != nil {
		t.Fatal(err)
	}
	status, err := s.BotInteractionHealStatus(context.Background(), installation, group)
	if err != nil || !status.NeedsHeal || status.Generation != 3 || status.NeedFromSequence != 8 {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	result, err := s.BotInteractionHeal(context.Background(), installation, group, []byte{1, 2}, nil, nil)
	if err != nil || !posted || result.Epoch != 2 || result.NextSequence != 10 {
		t.Fatalf("result=%+v posted=%v err=%v", result, posted, err)
	}
}

func readRequest(t *testing.T, r *http.Request) []byte {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
