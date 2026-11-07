package dillad_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/dillad"
	"github.com/dilla-chat/dilla/internal/id"
)

// Bot v2 task 13: incoming webhooks on the composed server.

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// The secret travels in the path. dillad's request log (server.RequestLog, the one access log the composed handler
// writes) records the route pattern, never the URL, so no line holds the secret.
func TestWebhookSecretIsNotInTheAccessLog(t *testing.T) {
	logs := &syncBuffer{}
	srv, err := dillad.New(context.Background(), dillad.Options{Config: testConfig(t), Clock: clock.NewFake(time.Unix(1_700_000_000, 0)),
		Wasm: sharedRuntime(t), Log: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = srv.Shutdown(context.Background()) }()
	secret := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x5c}, 32))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/hooks/"+id.New().String()+"/"+secret, strings.NewReader(`{"text":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST = %d %s, want 401", rec.Code, rec.Body.String())
	}
	out := logs.String()
	if strings.Contains(out, secret) {
		t.Fatalf("the log holds the webhook secret:\n%s", out)
	}
	if !strings.Contains(out, `route="POST /hooks/{webhook_id}/{secret}"`) {
		t.Fatalf("the access log line does not name the route pattern:\n%s", out)
	}
	// A GET of a /hooks path is not the browser client's: /hooks/ is a reserved prefix.
	get := httptest.NewRecorder()
	srv.Handler().ServeHTTP(get, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/hooks/"+id.New().String()+"/"+secret, nil))
	if get.Code != http.StatusMethodNotAllowed && get.Code != http.StatusNotFound {
		t.Fatalf("GET /hooks/… = %d, want 404 or 405", get.Code)
	}
}

// admitAll is a BotLaunch that admits this test process as the Bot child.
type admitAll struct{}

func (admitAll) Peer(int32, uint32) bool  { return true }
func (admitAll) Spend(int32, []byte) bool { return true }
func (admitAll) Authenticated(int32) bool { return true }

// On the Bot's Unix socket every peer would share one address bucket, so /hooks/ is not served there at all: the
// rate limits cannot be walked around through it. The rest of dillad's HTTP surface still is.
func TestBotSocketDoesNotServeWebhooks(t *testing.T) {
	cfg := testConfig(t)
	srv, err := dillad.New(context.Background(), dillad.Options{Config: cfg, Clock: clock.System(), Wasm: sharedRuntime(t), BotLaunch: admitAll{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = srv.Shutdown(context.Background()) }()
	sock := filepath.Join(t.TempDir(), "g.sock")
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.ServeBot(l) }()
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}, Timeout: 10 * time.Second}
	do := func(method, path string) int {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, "http://bot"+path, strings.NewReader(`{"text":"x"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		_ = res.Body.Close()
		return res.StatusCode
	}
	if got := do(http.MethodPost, "/hooks/"+id.New().String()+"/secret"); got != http.StatusNotFound {
		t.Fatalf("POST /hooks/… on the Bot socket = %d, want 404", got)
	}
	if got := do(http.MethodGet, "/v1/instance"); got != http.StatusOK {
		t.Fatalf("GET /v1/instance on the Bot socket = %d, want 200 (the socket still serves dillad's HTTP routes)", got)
	}
}
