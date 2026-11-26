package dillad_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/dillad"
	"github.com/dilla-chat/dilla/internal/web"
)

// Task 12 (Jonas 2026-11-26): dillad serves no browser client. Through the whole handler chain, the former client
// paths are a 302 to the download page, the server's own routes keep their answers and their 404s, the download page
// carries the embedded font, and the invite page points only to Dilla desktop. No option selects any of it.
func TestDilladServesTheDownloadPageInsteadOfTheBrowserClient(t *testing.T) {
	cfg, code := testConfigInvite(t)
	srv, err := dillad.New(t.Context(), dillad.Options{Config: cfg, Clock: clock.System(), Wasm: sharedRuntime(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(context.Background())
	h := srv.Handler()
	for _, path := range []string{"/", "/index.html", "/welcome", "/welcome?invite=" + code, "/c/0123456789abcdef0123456789abcdef", "/dm", "/settings/devices", "/browser-migration"} {
		rec := serveOne(t, h, http.MethodGet, path, "dilla.test")
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/download" {
			t.Errorf("GET %s = %d Location %q, want 302 to /download", path, rec.Code, rec.Header().Get("Location"))
		}
		if rec.Header().Get("X-Dilla-Generation") == "" {
			t.Errorf("GET %s carries no X-Dilla-Generation", path)
		}
	}
	page := serveOne(t, h, http.MethodGet, "/download", "")
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Download Dilla") || strings.Contains(page.Body.String(), "<script") {
		t.Fatalf("GET /download = %d %q", page.Code, page.Body.String())
	}
	if font := serveOne(t, h, http.MethodGet, web.FontPath, ""); font.Code != http.StatusOK || font.Header().Get("Content-Type") != "font/woff2" || font.Body.Len() != 40404 {
		t.Fatalf("GET %s = %d %q, %d bytes", web.FontPath, font.Code, font.Header().Get("Content-Type"), font.Body.Len())
	}
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/v1/instance", 200}, {"GET", "/v1/x", 404}, {"GET", "/v1/not-a-route", 404}, {"POST", "/v1/x", 404},
		{"GET", "/desktop-auth/app.js", 200}, {"POST", "/desktop-auth/not-a-route", 404}, {"GET", "/desktop-auth/not-a-route", 404},
		{"GET", "/v1/auth/oidc/callback", 501}, {"GET", "/healthz", 200}, {"GET", "/i", 404},
		{"GET", "/assets/app-1a2b.js", 404}, {"GET", "/app-version.json", 404}, {"GET", "/dilla-manifest.json", 404},
		{"POST", "/", 405}, {"POST", "/download", 405},
	} {
		if rec := serveOne(t, h, tc.method, tc.path, ""); rec.Code != tc.status {
			t.Errorf("%s %s = %d want %d", tc.method, tc.path, rec.Code, tc.status)
		}
	}
	invite := serveOne(t, h, http.MethodGet, "/i/"+code, "")
	if invite.Code != 200 {
		t.Fatalf("invite=%d %s", invite.Code, invite.Body.String())
	}
	body := invite.Body.String()
	if !strings.Contains(body, `href="dilla://invite/dilla.test/`+code+`"`) || !strings.Contains(body, `href="/download"`) ||
		strings.Contains(body, "/welcome") || strings.Contains(body, "this browser") {
		t.Fatalf("invite page = %s", body)
	}
	// The invite page names the font dillad embeds, at the path the download page serves it from.
	if !strings.Contains(body, `url("`+web.FontPath+`") format("woff2")`) {
		t.Fatalf("the invite page does not name the embedded font:\n%s", body)
	}
}
