package dillad_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/dillad"
	"github.com/dilla-chat/dilla/internal/gateway"
)

// The test host stops and starts its Bot through /debug/bot/… on its private control listener (dilladtest). dillad's
// own surface has no such route, on the public listener or on the Bot's socket; TestTheReleaseBinaryDoesNotLinkTheTestHelpers
// keeps the control handler itself out of the release binary.
func TestBotControlEndpointIsNotServedByDillad(t *testing.T) {
	srv, err := dillad.New(context.Background(), dillad.Options{Config: testConfig(t), Clock: clock.System(), Wasm: sharedRuntime(t)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = srv.Shutdown(context.Background()) }()
	for _, path := range []string{"/debug/bot/stop", "/debug/bot/start", "/debug/bot/restart", "/debug/bot/status"} {
		for _, method := range []string{http.MethodPost, http.MethodGet} {
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, path, nil))
			if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s answered %d on dillad's handler", method, path, rec.Code)
			}
		}
	}
}

// Server.ServeBot serves a Unix listener only.
func TestServeBotRefusesATCPListener(t *testing.T) {
	srv, err := dillad.New(context.Background(), dillad.Options{Config: testConfig(t), Clock: clock.System(), Wasm: sharedRuntime(t)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = srv.Shutdown(context.Background()) }()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	if err := srv.ServeBot(l); !errors.Is(err, gateway.ErrBotListenerNotUnix) {
		t.Fatalf("ServeBot(tcp) = %v, want gateway.ErrBotListenerNotUnix", err)
	}
}
