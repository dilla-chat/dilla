package dillad_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	botclient "github.com/dilla-chat/dilla/internal/bot/client"
)

// Bot v2: the app's socket route manifest (packages/client-core/src/http/socket-routes.ts) against the composed mux.
// scripts/check-socket-routes.mjs checks the same rules from source on the Node lane; this one reads the patterns the
// composition root really registered, so a registration that script cannot read still fails here.
func TestSocketRouteManifestMatchesTheMux(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "packages", "client-core", "src", "http", "socket-routes.ts"))
	if err != nil {
		t.Fatal(err)
	}
	entry := regexp.MustCompile(`\{\s*method:\s*'([A-Z]+)',\s*pattern:\s*'([^']+)'\s*\}`)
	lists := map[string]map[string]bool{}
	for _, name := range []string{"SOCKET_ROUTES", "HTTP_ONLY_ROUTES", "BOT_RUNTIME_ROUTES"} {
		m := regexp.MustCompile(`export const ` + name + `: readonly SocketRoute\[\] = \[([\s\S]*?)\n\];`).FindSubmatch(raw)
		if m == nil {
			t.Fatalf("%s not found in the manifest", name)
		}
		lists[name] = map[string]bool{}
		for _, e := range entry.FindAllSubmatch(m[1], -1) {
			lists[name][string(e[1])+" "+string(e[2])] = true
		}
	}
	srv, _, _ := newInstance(t)
	defer srv.Shutdown(context.Background())
	socket := map[string]bool{}
	for _, pattern := range srv.Mux().SocketPatterns() {
		socket[pattern] = true
		if !lists["SOCKET_ROUTES"][pattern] {
			t.Errorf("%s is served on the socket but is not in SOCKET_ROUTES", pattern)
		}
	}
	for pattern := range lists["SOCKET_ROUTES"] {
		if !socket[pattern] {
			t.Errorf("%s is in SOCKET_ROUTES but the instance does not serve it on the socket", pattern)
		}
	}
	mounted := 0
	for _, pattern := range srv.Mux().Patterns() {
		if _, path, _ := strings.Cut(pattern, " "); !strings.HasPrefix(path, "/v1/") {
			continue
		}
		mounted++
		if !lists["HTTP_ONLY_ROUTES"][pattern] && !lists["SOCKET_ROUTES"][pattern] && !lists["BOT_RUNTIME_ROUTES"][pattern] {
			t.Errorf("%s is served on HTTP but is in none of HTTP_ONLY_ROUTES, SOCKET_ROUTES and BOT_RUNTIME_ROUTES", pattern)
		}
	}
	if mounted < 50 {
		t.Fatalf("only %d /v1 HTTP patterns were recorded", mounted)
	}
	for _, pattern := range botclient.SocketRoutes {
		if !socket[pattern] {
			t.Errorf("the Bot sends %s over the socket but the instance does not serve it there", pattern)
		}
	}
}
