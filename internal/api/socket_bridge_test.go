package api_test

import (
	"net/http"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/api/sockettest"
	"github.com/dilla-chat/dilla/internal/server"
)

// viaSocket is h with the routes that moved to the gateway socket sent as request frames
// (sockettest.Bridge): a test about a moved route builds its harness as usual and wraps the handler, so its
// fixtures and its assertions stay what they were; the answer it reads has no headers, as a socket response
// has none.
func viaSocket(t testing.TB, h http.Handler, d api.Deps) http.Handler {
	t.Helper()
	mux, ok := h.(*server.Mux)
	if !ok {
		t.Fatalf("viaSocket: the harness handler is %T, want *server.Mux", h)
	}
	disp := api.NewSocketDispatcher(mux, api.SocketDispatchOptions{Clock: d.Clock})
	return sockettest.NewBridge(mux, mux, disp, d.Repo, d.Clock.Now)
}
