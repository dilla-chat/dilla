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
	listedIn := map[string]string{}
	for _, name := range []string{"SOCKET_ROUTES", "HTTP_ONLY_ROUTES", "BOT_RUNTIME_ROUTES", "PRE_GATEWAY_ROUTES"} {
		m := regexp.MustCompile(`export const ` + name + `: readonly SocketRoute\[\] = \[([\s\S]*?)\n\];`).FindSubmatch(raw)
		if m == nil {
			t.Fatalf("%s not found in the manifest", name)
		}
		lists[name] = map[string]bool{}
		for _, e := range entry.FindAllSubmatch(m[1], -1) {
			key := string(e[1]) + " " + string(e[2])
			// PRE_GATEWAY_ROUTES annotates HTTP_ONLY_ROUTES entries; the other three lists are disjoint.
			if other, ok := listedIn[key]; ok && name != "PRE_GATEWAY_ROUTES" {
				t.Errorf("%s is listed more than once (%s and %s)", key, other, name)
			} else if name != "PRE_GATEWAY_ROUTES" {
				listedIn[key] = name
			}
			lists[name][key] = true
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
	served := map[string]bool{}
	for _, pattern := range srv.Mux().Patterns() {
		served[pattern] = true
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
	for _, name := range []string{"HTTP_ONLY_ROUTES", "BOT_RUNTIME_ROUTES", "PRE_GATEWAY_ROUTES"} {
		for pattern := range lists[name] {
			if !served[pattern] {
				t.Errorf("%s is in %s but the instance does not serve it on HTTP", pattern, name)
			}
		}
	}
	// Ruling C1: a route called before the gateway exists stays on HTTP.
	for pattern := range lists["PRE_GATEWAY_ROUTES"] {
		if lists["SOCKET_ROUTES"][pattern] || socket[pattern] {
			t.Errorf("%s is called before the gateway exists (PRE_GATEWAY_ROUTES) but is on the socket", pattern)
		}
	}
	for _, pattern := range botclient.SocketRoutes {
		if !socket[pattern] {
			t.Errorf("the Bot sends %s over the socket but the instance does not serve it there", pattern)
		}
	}
}
