package web_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/web"
)

// The download page's own policy: no script, no connection, nothing but its stylesheet, its picture and its font.
const pageCSP = "default-src 'none'; style-src 'self'; img-src 'self'; font-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
const pagePermissions = "camera=(), microphone=(), display-capture=(), geolocation=()"

// fontSHA256 is @fontsource-variable/jetbrains-mono 5.3.0's files/jetbrains-mono-latin-wght-normal.woff2, the file the
// browser client bundled (Task 12, Jonas 2026-11-26: "Embed the font").
const fontSHA256 = "18be452724bfdc236c074ca94a249a7f41a86752c7d04ab258ce9ed5651f6a7e"

const hex32 = "0123456789abcdef0123456789abcdef"

// Jonas 2026-11-26 ("Send to the download page"): every former browser-client path is a 302 to /download, with a
// fixed origin-relative Location that carries nothing of the request (an invite code in ?invite= included).
func TestOldClientPathsRedirectToTheDownloadPage(t *testing.T) {
	h := web.Fallback(server.NewMux(), nil)
	for _, route := range []string{
		"/", "/index.html", "/welcome", "/welcome?invite=CODE123", "/c/" + hex32, "/c/" + hex32 + "/" + hex32 + "/",
		"/dm", "/dm/" + hex32, "/settings/devices", "/browser-migration", "/browser-migration/exit",
		"/gatewayx", "/rtcx/y",
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			rec := do(t, h, method, route, map[string]string{"Host": "attacker.test"})
			if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/download" {
				t.Errorf("%s %s = %d Location %q, want 302 to /download", method, route, rec.Code, rec.Header().Get("Location"))
			}
			if rec.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("%s %s: Cache-Control %q, want no-store", method, route, rec.Header().Get("Cache-Control"))
			}
			if strings.Contains(rec.Body.String(), "CODE123") || strings.Contains(rec.Body.String(), "attacker") ||
				rec.Header().Get("Set-Cookie") != "" {
				t.Errorf("%s %s echoed the request or set a cookie: %v %q", method, route, rec.Header(), rec.Body.String())
			}
		}
	}
}

func TestTheDownloadPageIsTheOnePageDilladServes(t *testing.T) {
	h := web.Fallback(server.NewMux(), nil)
	rec := do(t, h, http.MethodGet, "/download", map[string]string{"Host": "attacker.test; script-src *"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Download Dilla") {
		t.Fatalf("/download = %d %s", rec.Code, rec.Body.String())
	}
	for _, forbidden := range []string{"<script", "<iframe", "<form", "localStorage", "indexedDB", "Worker(", "serviceWorker",
		"browser-migration", "Moving from this browser", "attacker", "github.com", "mac-x64"} {
		if strings.Contains(rec.Body.String(), forbidden) {
			t.Errorf("the download page contains %q", forbidden)
		}
	}
	// Each button asks this server, which redirects to the verified release asset (internal/desktopdownload).
	for _, platform := range []string{"linux-appimage", "linux-deb", "linux-rpm", "linux-aur", "win-x64", "mac-arm64"} {
		if !strings.Contains(rec.Body.String(), `href="/download/`+platform+`"`) {
			t.Errorf("missing %s link", platform)
		}
	}
	want := map[string]string{
		"Content-Type": "text/html; charset=utf-8", "Cache-Control": "no-store",
		"Content-Security-Policy": pageCSP, "Permissions-Policy": pagePermissions,
		"X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer", "X-Frame-Options": "DENY",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("/download: %s = %q, want %q", k, got, v)
		}
	}
	if rec.Header().Get("Vary") != "" || rec.Header().Get("Set-Cookie") != "" {
		t.Errorf("/download varies on or sets a cookie: %v", rec.Header())
	}
	head := do(t, h, http.MethodHead, "/download", nil)
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD /download = %d %q", head.Code, head.Body.String())
	}
}

// Nothing of the browser client is left: its files, its updater's /app-version.json and its manifest are plain 404s,
// unsafe paths too, and no method but GET and HEAD reaches a page.
func TestNothingOfTheBrowserClientIsServed(t *testing.T) {
	h := web.Fallback(server.NewMux(), nil)
	for _, route := range []string{
		"/assets/app-1a2b.js", "/assets/core_bg-5e6f.wasm", "/app-version.json", "/dilla-manifest.json", "/config.json",
		"/favicon.ico", "/robots.txt", "/assets/../index.html", "/browser-migration%2f..%2f", "/a%5cb", "/a%00b",
		"/download/../welcome",
	} {
		rec := do(t, h, http.MethodGet, route, nil)
		if rec.Code != http.StatusNotFound || rec.Body.String() != notFound {
			t.Errorf("GET %s = %d %q, want a plain 404", route, rec.Code, rec.Body.String())
		}
	}
	for _, route := range []string{"/", "/download", "/welcome", "/desktop-download.css"} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions} {
			rec := do(t, h, method, route, nil)
			if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" {
				t.Errorf("%s %s = %d Allow %q, want 405 GET, HEAD", method, route, rec.Code, rec.Header().Get("Allow"))
			}
		}
	}
}

// Every route of the mux and every reserved prefix keeps the mux's own answer, its 404s and its 405s; a data path
// below the root is the API's CBOR refusal, never a redirect.
func TestTheFallbackLeavesTheServerRoutesAlone(t *testing.T) {
	mux := server.NewMux()
	teapot := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	for _, pattern := range []string{"GET /v1/probe", "POST /v1/probe", "GET /desktop-auth/{path...}", "POST /desktop-auth/{path...}",
		"GET /i/{code}", "GET /download/{platform}", "/rtc", "/gateway", "POST /hooks/{id}"} {
		mux.Handle(pattern, teapot)
	}
	h := web.Fallback(mux, nil)
	mux.Handle("GET /v1/late", teapot) // registered after Fallback was built, as Options.Extra is
	for _, tc := range []struct {
		method, route string
		status        int
		body          string
	}{
		{"GET", "/v1/probe", 418, ""}, {"POST", "/v1/probe", 418, ""}, {"GET", "/v1/late", 418, ""},
		{"GET", "/v1/x", 404, notFound}, {"POST", "/v1/x", 404, notFound}, {"GET", "/v1", 404, notFound},
		{"GET", "/desktop-auth/browser.js", 418, ""}, {"POST", "/desktop-auth/complete", 418, ""},
		{"DELETE", "/desktop-auth/complete", 405, ""},
		{"GET", "/i/CODE", 418, ""}, {"GET", "/i", 404, notFound}, {"GET", "/download/linux-deb", 418, ""},
		{"POST", "/rtc", 418, ""}, {"GET", "/gateway", 418, ""}, {"POST", "/hooks/abc", 418, ""}, {"GET", "/hooks/abc", 405, ""},
		{"GET", "/healthz", 404, notFound}, {"GET", "/readyz", 404, notFound}, {"GET", "/metrics", 404, notFound},
		{"POST", "/debug/sfu/token", 404, notFound},
	} {
		rec := do(t, h, tc.method, tc.route, nil)
		if rec.Code != tc.status || (tc.body != "" && rec.Body.String() != tc.body) {
			t.Errorf("%s %s = %d %q, want %d %q", tc.method, tc.route, rec.Code, rec.Body.String(), tc.status, tc.body)
		}
	}
	want := pathRefusal(t)
	for _, target := range []string{"/welcome/v1/instance", "/c/" + hex32 + "/v1/instance", "/welcome/gateway", "/welcome/rtc/validate", "/welcome/i/code"} {
		rec := do(t, h, http.MethodGet, target, nil)
		if rec.Code != 404 || rec.Header().Get("Content-Type") != "application/cbor" || rec.Body.String() != want {
			t.Errorf("GET %s = %d %q %q, want the CBOR 404", target, rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
		}
	}
}

// pathRefusal is the API's refusal for an instance address with a path.
func pathRefusal(t *testing.T) string {
	t.Helper()
	rec := do(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		server.WriteError(w, server.Errorf(server.CodeNotFound, "the instance address must not have a path"))
	}), http.MethodGet, "/", nil)
	return rec.Body.String()
}

// Jonas 2026-11-26 ("Embed the font"): the download page and the invite page name JetBrains Mono at web.FontPath, and
// dillad serves it from its own binary, with its licence beside it in the tree.
func TestTheDownloadPageServesTheEmbeddedFont(t *testing.T) {
	h := web.Fallback(server.NewMux(), nil)
	css := do(t, h, http.MethodGet, "/desktop-download.css", nil)
	if css.Code != 200 || css.Header().Get("Content-Type") != "text/css; charset=utf-8" {
		t.Fatalf("download CSS = %d %v", css.Code, css.Header())
	}
	if !strings.Contains(css.Body.String(), `@font-face{font-family:"JetBrains Mono";`) || !strings.Contains(css.Body.String(), `url("`+web.FontPath+`") format("woff2")`) {
		t.Fatalf("the download CSS does not name the embedded font at %s:\n%s", web.FontPath, css.Body.String())
	}
	font := do(t, h, http.MethodGet, web.FontPath, nil)
	sum := sha256.Sum256(font.Body.Bytes())
	if font.Code != 200 || font.Header().Get("Content-Type") != "font/woff2" || hex.EncodeToString(sum[:]) != fontSHA256 ||
		font.Body.Len() != 40404 || !bytes.HasPrefix(font.Body.Bytes(), []byte("wOF2")) {
		t.Fatalf("GET %s = %d %q, %d bytes, sha256 %x", web.FontPath, font.Code, font.Header().Get("Content-Type"), font.Body.Len(), sum)
	}
	if font.Header().Get("Content-Security-Policy") != pageCSP || font.Header().Get("X-Content-Type-Options") != "nosniff" ||
		font.Header().Get("ETag") != `"`+fontSHA256+`"` {
		t.Fatalf("GET %s headers %v", web.FontPath, font.Header())
	}
	if rec := do(t, h, http.MethodGet, web.FontPath, map[string]string{"If-None-Match": `"` + fontSHA256 + `"`}); rec.Code != http.StatusNotModified {
		t.Fatalf("a revalidated font = %d, want 304", rec.Code)
	}
	licence, err := os.ReadFile("downloads/OFL-JetBrainsMono.txt")
	if err != nil || !bytes.Contains(licence, []byte("SIL Open Font License, Version 1.1")) {
		t.Fatalf("the font's licence is not beside it: %v", err)
	}
}

func TestDownloadBrandReusesTheDesktopAsset(t *testing.T) {
	rec := do(t, web.Fallback(server.NewMux(), nil), "GET", "/desktop-download-brand.png", nil)
	source, err := os.ReadFile("../../clients/desktop/assets/brand/tray.png")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/png" || !bytes.Equal(rec.Body.Bytes(), source) {
		t.Fatal("download branding differs from the existing desktop asset")
	}
}

// The test-only seam (dilladtest WebRoot, dilla-testhost -web-root): a verified tree answers what the download page
// does not, with its own policy, and the download page, its assets and its font stay where they are.
func TestAServedTreeAnswersBehindTheDownloadPage(t *testing.T) {
	tree, _ := newSite(t)
	h := web.Fallback(server.NewMux(), tree)
	for _, tc := range []struct{ route, body string }{
		{"/", site["index.html"]}, {"/c/" + hex32, site["index.html"]}, {"/welcome", site["index.html"]},
		{"/assets/app-1a2b.js", site["assets/app-1a2b.js"]},
	} {
		rec := do(t, h, http.MethodGet, tc.route, map[string]string{"Host": "example.org"})
		if rec.Code != 200 || rec.Body.String() != tc.body || rec.Header().Get("Content-Security-Policy") != cspExampleOrg {
			t.Errorf("GET %s = %d %q CSP %q, want the tree's file", tc.route, rec.Code, rec.Body.String(), rec.Header().Get("Content-Security-Policy"))
		}
	}
	if rec := do(t, h, http.MethodGet, "/download", nil); !strings.Contains(rec.Body.String(), "Download Dilla") || rec.Header().Get("Content-Security-Policy") != pageCSP {
		t.Errorf("GET /download beside a tree = %d %q", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, http.MethodGet, web.FontPath, nil); rec.Code != 200 || rec.Header().Get("Content-Type") != "font/woff2" {
		t.Errorf("GET %s beside a tree = %d", web.FontPath, rec.Code)
	}
	for _, route := range []string{"/app-version.json", "/dilla-manifest.json", "/assets/missing-0000.js"} {
		if rec := do(t, h, http.MethodGet, route, nil); rec.Code != 404 {
			t.Errorf("GET %s beside a tree = %d, want 404", route, rec.Code)
		}
	}
}
