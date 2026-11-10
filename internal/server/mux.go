package server

import (
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
)

// Mux is the stdlib ServeMux with Go 1.22 method-and-wildcard patterns. There
// is no router library: the patterns dillad needs are exactly what the stdlib
// parses, and the stdlib's conflict detection panics at registration, which a
// test relies on.
type Mux struct {
	mux *http.ServeMux
	// wrap, when set, is applied to every handler registered through this Mux
	// before it reaches mux (Wrapped).
	wrap func(pattern string, h http.Handler) http.Handler
	// seen is every pattern registered through this Mux and its Wrapped views, in order. It is
	// shared by the views, so it lists the whole ServeMux's Handle calls (Patterns).
	seen *patterns
	// socket is the socket-only ServeMux (Bot v2 task 2): the routes a gateway `request` frame
	// reaches and no HTTP request does. It is shared by the Wrapped views, as mux is.
	socket *socketRoutes
}

// socketRoutes is the socket-only ServeMux, the patterns registered on it in order, and each
// pattern's body cap.
type socketRoutes struct {
	mux   *http.ServeMux
	mu    sync.Mutex
	list  []string
	caps  map[string]int64
	guard atomic.Pointer[SocketGuard]
}

// SocketGuard is the socket routes' innermost leaf: it runs after the route's wrap (the session
// middleware and rate meter), immediately before the handler, and decides whether next runs. The
// socket request dispatcher installs it (it checks that the session middleware passed and serves
// the replay record); internal/server cannot import internal/auth, hence the hook.
type SocketGuard func(w http.ResponseWriter, r *http.Request, next http.Handler)

// SetSocketGuard installs the guard every socket route runs innermost. Until one is installed,
// every socket request is refused (fail closed).
func (m *Mux) SetSocketGuard(g SocketGuard) { m.socket.guard.Store(&g) }

// DefaultSocketBodyCap bounds a socket route's response body (and, through the frame reader, its
// request body) when the route was registered with Socket rather than SocketWithCap. It is the
// largest HTTP body cap of any authenticated route (the commit's 16 MiB); a handler still applies
// its own, smaller, request cap exactly as it does over HTTP.
const DefaultSocketBodyCap int64 = 16 << 20

type patterns struct {
	mu   sync.Mutex
	list []string
}

func NewMux() *Mux {
	return &Mux{mux: http.NewServeMux(), seen: &patterns{},
		socket: &socketRoutes{mux: http.NewServeMux(), caps: map[string]int64{}}}
}

// Patterns is every pattern registered through Handle on this Mux or a Wrapped view of it. A
// route registered on Mux() directly is not listed. The route sweep of web3.6 reads it, so a
// mounted route that no protocol document lists fails a test instead of escaping it.
func (m *Mux) Patterns() []string {
	m.seen.mu.Lock()
	defer m.seen.mu.Unlock()
	return append([]string(nil), m.seen.list...)
}

// Handle registers a pattern such as "POST /v1/groups/{id}/commit". A pattern
// that conflicts with one already registered panics, at start-up, on purpose.
func (m *Mux) Handle(pattern string, h http.Handler) {
	if m.wrap != nil {
		h = m.wrap(pattern, h)
	}
	m.mux.Handle(pattern, h)
	m.seen.mu.Lock()
	m.seen.list = append(m.seen.list, pattern)
	m.seen.mu.Unlock()
}

func (m *Mux) HandleFunc(pattern string, h http.HandlerFunc) { m.Handle(pattern, h) }

// Wrapped returns a view of m that registers on the same ServeMux, with every
// handler passed through wrap first; wrap sees the pattern, so it can choose
// per route. It is how the composition root mounts a handler group whose
// Register mounts its handlers bare (Plan 2's): the view puts the session
// middleware and the rate meter in front of each route the group registers,
// and a route registered on m itself is untouched.
func (m *Mux) Wrapped(wrap func(pattern string, h http.Handler) http.Handler) *Mux {
	return &Mux{mux: m.mux, wrap: wrap, seen: m.seen, socket: m.socket}
}

// Socket registers a pattern on the socket-only ServeMux, through this view's wrap exactly as
// Handle would (so a Wrapped view's session middleware and rate meter sit in front of it). The
// route is reachable only through a gateway `request` frame: the HTTP mux never serves it. A
// conflicting socket pattern panics at start-up, as Handle does.
func (m *Mux) Socket(pattern string, h http.Handler) {
	m.SocketWithCap(pattern, DefaultSocketBodyCap, h)
}

// SocketWithCap is Socket for a route whose response body is bounded by maxBody rather than
// DefaultSocketBodyCap: the route's own HTTP body cap, so a response over it is refused as the
// request over it is.
func (m *Mux) SocketWithCap(pattern string, maxBody int64, h http.Handler) {
	m.socketRoute(pattern, maxBody, nil, h)
}

// SocketAuthed registers a socket route whose middleware is built inline rather than by a Wrapped
// view: the enrolled/scoped/dsMeter chains of api.go, groups.go, messages.go, keypackages.go,
// heal.go and the bot_*.go helpers. mw is that chain and h the bare handler; the socket guard runs
// between them, so the chain's session middleware has marked the call passed before the guard
// looks, its rate meter is spent once per request (a replay hit included) and stays outside the
// replay record, and the session is resolved once. The composition is cap setter → this view's
// wrap → mw → guard → h. Tasks 4–5 move inline-wrapped routes this way: pass the route's existing
// chain as mw and its handler as h. A chain that does not run the session middleware fails closed
// (401) at the guard.
func (m *Mux) SocketAuthed(pattern string, mw func(http.Handler) http.Handler, h http.Handler) {
	m.SocketAuthedWithCap(pattern, DefaultSocketBodyCap, mw, h)
}

// SocketAuthedWithCap is SocketAuthed with the route's own response cap, as SocketWithCap is to
// Socket.
func (m *Mux) SocketAuthedWithCap(pattern string, maxBody int64, mw func(http.Handler) http.Handler, h http.Handler) {
	if mw == nil {
		panic("server: SocketAuthed " + pattern + " without its middleware chain")
	}
	m.socketRoute(pattern, maxBody, mw, h)
}

// socketRoute composes cap setter → view wrap → mw (when given) → guard → h and registers it.
func (m *Mux) socketRoute(pattern string, maxBody int64, mw func(http.Handler) http.Handler, h http.Handler) {
	handler := h
	h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g := m.socket.guard.Load()
		if g == nil || *g == nil {
			slog.Default().Error("socket route served with no socket guard installed; refused", "route", r.Pattern)
			WriteError(w, Errorf(CodeUnauthenticated, ""))
			return
		}
		(*g)(w, r, handler)
	})
	if mw != nil {
		h = mw(h)
	}
	if m.wrap != nil {
		h = m.wrap(pattern, h)
	}
	inner := h
	h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s, ok := w.(SocketCapSetter); ok {
			s.SetSocketCap(maxBody)
		}
		inner.ServeHTTP(w, r)
	})
	m.socket.mux.Handle(pattern, h)
	m.socket.mu.Lock()
	m.socket.list = append(m.socket.list, pattern)
	m.socket.caps[pattern] = maxBody
	m.socket.mu.Unlock()
}

// SocketCapSetter is implemented by the socket dispatcher's response recorder: the matched
// route tells it its cap before any middleware or handler writes, so the recorder bounds the
// response by the route without a second pattern match.
type SocketCapSetter interface{ SetSocketCap(maxBody int64) }

// SocketMux is the socket-only ServeMux the request dispatcher serves into.
func (m *Mux) SocketMux() *http.ServeMux { return m.socket.mux }

// SocketPatterns is every pattern registered through Socket on this Mux or a Wrapped view of it.
// Patterns does not list them: an HTTP route sweep must not find a route HTTP cannot reach.
func (m *Mux) SocketPatterns() []string {
	m.socket.mu.Lock()
	defer m.socket.mu.Unlock()
	return append([]string(nil), m.socket.list...)
}

// SocketCap is the body cap a socket pattern was registered with; an unknown pattern has the
// default.
func (m *Mux) SocketCap(pattern string) int64 {
	m.socket.mu.Lock()
	defer m.socket.mu.Unlock()
	if c, ok := m.socket.caps[pattern]; ok {
		return c
	}
	return DefaultSocketBodyCap
}

// Mux exposes the underlying ServeMux. It is the extension point parts 1b and 2
// register their handler groups through: without it a *server.Mux cannot be
// passed to a `Register(mux *http.ServeMux)` method and their routes are never
// mounted (deviation ID14).
func (m *Mux) Mux() *http.ServeMux { return m.mux }

func (m *Mux) ServeHTTP(w http.ResponseWriter, r *http.Request) { m.mux.ServeHTTP(w, r) }
