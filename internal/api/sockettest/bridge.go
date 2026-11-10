package sockettest

import (
	"io"
	"net/http"
	"time"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// Bridge is an HTTP handler that sends the routes which moved to the gateway socket through the socket
// request dispatcher. A request for a route registered on the socket mux and not on the HTTP mux is
// dispatched as a request frame of the session its Authorization bearer names (the session the gateway
// connection would have identified with), and answered with the response frame's status and body, which
// carry no headers. Everything else goes to next untouched. A test about a moved route builds its harness as
// before and wraps the handler, so its fixtures and assertions keep their shape.
type Bridge struct {
	next http.Handler
	mux  *server.Mux
	disp *api.SocketDispatcher
	repo store.Repository
	now  func() time.Time
}

// NewBridge wraps next, the HTTP handler served from mux. disp is the dispatcher installed on mux, repo
// the store the sessions live in and now the clock the sessions are read at.
func NewBridge(next http.Handler, mux *server.Mux, disp *api.SocketDispatcher, repo store.Repository, now func() time.Time) *Bridge {
	return &Bridge{next: next, mux: mux, disp: disp, repo: repo, now: now}
}

// Mux is the mux behind the bridge.
func (b *Bridge) Mux() *server.Mux { return b.mux }

func (b *Bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, pattern := b.mux.Mux().Handler(r); pattern == "" {
		if _, socketPattern := b.mux.SocketMux().Handler(r); socketPattern != "" {
			b.serveSocket(w, r)
			return
		}
	}
	b.next.ServeHTTP(w, r)
}

func (b *Bridge) serveSocket(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var sess auth.Session
	if bearer, ok := bearerOf(r); ok {
		sess.TokenHash = auth.TokenHash(bearer)
		// The connection's device, read without Resolve's side effects: a token that names no live session
		// leaves it zero, and the middleware refuses the request.
		if row, err := b.repo.GetSessionByHash(r.Context(), sess.TokenHash, b.now().Unix()); err == nil {
			sess.DeviceID = row.DeviceID
		}
	}
	resp := DispatchFrom(r.Context(), b.disp, sess, r.RemoteAddr, r.Method, r.URL.RequestURI(), body, nil)
	if len(resp.Body) > 0 {
		w.Header().Set("Content-Type", "application/cbor")
	}
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}

func bearerOf(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	v := r.Header.Get("Authorization")
	if len(v) <= len(prefix) || v[:len(prefix)] != prefix {
		return "", false
	}
	return v[len(prefix):], true
}
