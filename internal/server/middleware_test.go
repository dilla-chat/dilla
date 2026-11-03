package server_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/server"
)

// RequestLog wraps the whole mux, so its ResponseWriter is what a WebSocket
// upgrade sees. coder/websocket reaches the connection through
// http.NewResponseController(w).Hijack(), which walks Unwrap() chains; without
// Unwrap on statusWriter every upgrade in part 1b fails at run time and nothing
// in 1a notices. This test is that notice.
func TestTheLoggedResponseWriterStillHijacks(t *testing.T) {
	var hijacked bool
	var hijackErr error
	// A hijacked connection dies without a response, so http.Get returns an
	// error rather than a synchronisation edge. done is that edge: without it
	// the assertions below read hijacked/hijackErr while the server goroutine
	// is still writing them, and the pinned -race suite reports the race.
	done := make(chan struct{})
	h := server.RequestLog(slog.New(slog.DiscardHandler), clock.System(), nil)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(done)
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				hijackErr = err
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			hijacked = true
			conn.Close()
		}))
	srv := httptest.NewServer(h)
	defer srv.Close()
	res, err := httpGet(t, srv.URL+"/anything")
	if err == nil {
		res.Body.Close()
	}
	<-done
	if hijackErr != nil {
		t.Fatalf("Hijack through the log middleware: %v", hijackErr)
	}
	if !hijacked {
		t.Fatal("the handler could not hijack the connection through RequestLog")
	}
}
