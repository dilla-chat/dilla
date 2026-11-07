package dillad_test

// bot_runtime_socket_test.go: Bot v2 task 14. The Bot runtime's routes under /v1/bot-runtime/ are socket-only, apart
// from the Bot's enrollment, which runs before the Bot has a gateway connection (botEnrollmentRoutes).

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	botclient "github.com/dilla-chat/dilla/internal/bot/client"
)

// botEnrollmentRoutes are the /v1/bot-runtime/ routes that stay on HTTP: the runtime credential's own routes (the Bot
// lists, claims and enrolls its installations, and reports its status, before and outside any device session) and the
// credential finalize, which the device session sends while the installation is enrolling and the gateway does not
// admit it yet. On a supervised instance they are reachable only on the Bot's Unix socket and the public listener's
// HTTP; nothing else of /v1/bot-runtime/ is served over HTTP.
var botEnrollmentRoutes = []string{
	"GET /v1/bot-runtime/installations",
	"POST /v1/bot-runtime/installations/{installation}/claim",
	"POST /v1/bot-runtime/installations/{installation}/enroll",
	"PUT /v1/bot-runtime/installations/{installation}/credential",
	"PUT /v1/bot-runtime/installations/{installation}/status",
}

// concretePath fills every {name} segment of a pattern's path with an id.
func concretePath(pattern string) (method, path string) {
	method, path, _ = strings.Cut(pattern, " ")
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if strings.HasPrefix(p, "{") {
			parts[i] = anID
		}
	}
	return method, strings.Join(parts, "/")
}

// TestBotRuntimeRoutesAreSocketOnly: every /v1/bot-runtime/ route but the enrollment routes is on the socket mux and
// not the HTTP mux, answers 404 or 405 over HTTP and 401 as a request frame without a session; and every runtime route
// the Bot's client sends over its socket (client.SocketRoutes) is one of them.
func TestBotRuntimeRoutesAreSocketOnly(t *testing.T) {
	srv, ts := newServer(t)
	socket := srv.Mux().SocketPatterns()
	httpPatterns := srv.Mux().Patterns()
	disp := socketHandler(srv)
	var runtime []string
	for _, p := range socket {
		if _, path, _ := strings.Cut(p, " "); strings.HasPrefix(path, "/v1/bot-runtime/") {
			runtime = append(runtime, p)
		}
	}
	for _, p := range httpPatterns {
		_, path, _ := strings.Cut(p, " ")
		if strings.HasPrefix(path, "/v1/bot-runtime/") && !slices.Contains(botEnrollmentRoutes, p) {
			t.Errorf("%s is served on HTTP; only the Bot's enrollment routes stay there", p)
		}
	}
	for _, p := range botEnrollmentRoutes {
		if !slices.Contains(httpPatterns, p) || slices.Contains(socket, p) {
			t.Errorf("enrollment route %s: on HTTP %v, on the socket %v; want HTTP only", p, slices.Contains(httpPatterns, p), slices.Contains(socket, p))
		}
	}
	if len(runtime) < 20 {
		t.Fatalf("only %d /v1/bot-runtime/ routes are on the socket mux: %v", len(runtime), runtime)
	}
	for _, p := range runtime {
		t.Run(p, func(t *testing.T) {
			method, path := concretePath(p)
			req, err := http.NewRequestWithContext(t.Context(), method, ts.URL+path, strings.NewReader(""))
			if err != nil {
				t.Fatal(err)
			}
			res, err := ts.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != http.StatusNotFound && res.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("over HTTP = %d, want 404 or 405", res.StatusCode)
			}
			if rec := call(t, disp, method, path, "", nil); rec.Code != http.StatusUnauthorized {
				t.Errorf("over the socket without a session = %d, want 401", rec.Code)
			}
		})
	}
	for _, p := range botclient.SocketRoutes {
		if _, path, _ := strings.Cut(p, " "); strings.HasPrefix(path, "/v1/bot-runtime/") && !slices.Contains(runtime, p) {
			t.Errorf("the Bot sends %s over its socket, but dillad serves no such socket route", p)
		}
	}
	for _, p := range runtime {
		if !slices.Contains(botclient.SocketRoutes, p) {
			t.Errorf("%s is socket-only, but the Bot's client.SocketRoutes does not send it over the socket", p)
		}
	}
}
