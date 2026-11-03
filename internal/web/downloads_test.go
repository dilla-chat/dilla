package web_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/server"
)

func TestDownloadsServeNavigationWithoutStartingTheClient(t *testing.T) {
	client, _ := newSite(t)
	mux := server.NewMux()
	h := client.DownloadsFallback(mux)
	for _, route := range []string{"/", "/welcome?invite=CODE", "/dm", "/settings/devices", "/index.html", "/download"} {
		rec := do(t, h, http.MethodGet, route, nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Download Dilla") {
			t.Fatalf("%s = %d %s", route, rec.Code, rec.Body.String())
		}
		for _, forbidden := range []string{"<script", "<iframe", "<form", "localStorage", "indexedDB", "Worker(", "serviceWorker"} {
			if strings.Contains(rec.Body.String(), forbidden) {
				t.Errorf("%s contains %q", route, forbidden)
			}
		}
		if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Vary") != "Cookie" {
			t.Errorf("%s cache headers = %v", route, rec.Header())
		}
		if rec.Header().Get("Content-Security-Policy") != do(t, client, "GET", "/", nil).Header().Get("Content-Security-Policy") || rec.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("headers = %v", rec.Header())
		}
		for _, artifact := range []string{"linux-x64.AppImage", "linux-x64.deb", "linux-x64.rpm", "linux-x64.aur.tar.gz", "win-x64.exe", "mac-x64.dmg", "mac-arm64.dmg"} {
			if !strings.Contains(rec.Body.String(), `href="https://github.com/dilla-chat/dilla/releases/download/desktop-beta/dilla-`+artifact+`"`) {
				t.Errorf("missing fixed %s link", artifact)
			}
		}
	}
	head := do(t, h, http.MethodHead, "/", nil)
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD = %d %q", head.Code, head.Body.String())
	}
	hostile := do(t, h, http.MethodGet, "/", map[string]string{"Host": "attacker.test; script-src *"})
	if strings.Contains(hostile.Header().Get("Content-Security-Policy"), "attacker") || strings.Contains(hostile.Body.String(), "attacker") {
		t.Fatal("request Host injected into download response")
	}
}

func TestDownloadsPreserveVerifiedAssetsAndMuxRoutes(t *testing.T) {
	client, _ := newSite(t)
	mux := server.NewMux()
	for _, pattern := range []string{"GET /v1/probe", "POST /v1/probe", "GET /desktop-auth/{path...}", "POST /desktop-auth/{path...}", "GET /i/{code}", "/rtc", "/gateway"} {
		mux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	}
	h := client.DownloadsFallback(mux)
	for _, tc := range []struct {
		method, route string
		status        int
	}{
		{"GET", "/v1/probe", 418}, {"POST", "/v1/probe", 418}, {"GET", "/v1/missing", 404},
		{"GET", "/desktop-auth/browser.js", 418}, {"POST", "/desktop-auth/complete", 418},
		{"DELETE", "/desktop-auth/complete", 405}, {"GET", "/i/CODE", 418}, {"POST", "/rtc", 418}, {"GET", "/gateway", 418},
		{"GET", "/assets/missing.js", 404}, {"GET", "/assets/../index.html", 404}, {"POST", "/", 405},
	} {
		if rec := do(t, h, tc.method, tc.route, nil); rec.Code != tc.status {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.route, rec.Code, tc.status)
		}
	}
	for _, route := range []string{"/assets/app-1a2b.js", "/assets/core_bg-5e6f.wasm", "/config.json", "/app-version.json", "/dilla-manifest.json"} {
		before, after := do(t, client.Fallback(mux), "GET", route, nil), do(t, h, "GET", route, nil)
		if before.Code != after.Code || before.Body.String() != after.Body.String() || before.Header().Get("ETag") != after.Header().Get("ETag") {
			t.Errorf("asset changed: %s", route)
		}
	}
	css := do(t, h, "GET", "/desktop-download.css", nil)
	if css.Code != 200 || !strings.HasPrefix(css.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("download CSS = %d %v", css.Code, css.Header())
	}
}

func TestMigrationAccessKeepsOriginalOriginAndURLsThenExits(t *testing.T) {
	client, _ := newSite(t)
	h := client.DownloadsFallback(server.NewMux())
	entry := do(t, h, "GET", "/browser-migration?next=https://evil.test", nil)
	if entry.Code != 303 || entry.Header().Get("Location") != "/" || entry.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("entry = %d %v", entry.Code, entry.Header())
	}
	cookies := entry.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %v", cookies)
	}
	cookie := cookies[0]
	if cookie.Path != "/" || cookie.Domain != "" || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode || cookie.MaxAge != 3600 || time.Until(cookie.Expires) > time.Hour+time.Minute {
		t.Fatalf("migration cookie = %+v", cookie)
	}
	headers := map[string]string{"Cookie": cookie.Name + "=" + cookie.Value, "If-None-Match": "*"}
	for _, route := range []string{"/", "/welcome", "/dm", "/settings/devices", "/index.html"} {
		rec := do(t, h, "GET", route, headers)
		if rec.Code != 200 || rec.Body.String() != site["index.html"] || rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("legacy %s = %d %q %v", route, rec.Code, rec.Body.String(), rec.Header())
		}
	}
	if rec := do(t, h, "GET", "/download", headers); !strings.Contains(rec.Body.String(), "Download Dilla") {
		t.Fatal("explicit download route hidden by migration preference")
	}
	exit := do(t, h, "GET", "/browser-migration/exit", headers)
	if exit.Code != 303 || exit.Header().Get("Location") != "/download" || exit.Result().Cookies()[0].MaxAge != -1 {
		t.Fatalf("exit = %d %v", exit.Code, exit.Header())
	}
	for _, route := range []string{"/browser-migration", "/browser-migration/exit"} {
		for _, method := range []string{"HEAD", "POST", "DELETE"} {
			rec := do(t, h, method, route, nil)
			if rec.Code != 405 || rec.Header().Get("Set-Cookie") != "" || rec.Header().Get("Allow") != "GET" {
				t.Errorf("%s %s = %d %v", method, route, rec.Code, rec.Header())
			}
		}
	}
	for _, route := range []string{"/browser-migration/../", "/browser-migration%2f..%2f", "/browser-migration/unknown", "/assets/../browser-migration", "/browser-migration/"} {
		rec := do(t, h, "GET", route, nil)
		if rec.Code != 404 || rec.Header().Get("Set-Cookie") != "" {
			t.Errorf("unsafe migration path %s = %d %v", route, rec.Code, rec.Header())
		}
	}
	invalid := do(t, h, "GET", "/", map[string]string{"Cookie": cookie.Name + "=other"})
	if !strings.Contains(invalid.Body.String(), "Download Dilla") {
		t.Fatal("unknown preference value selected legacy client")
	}
}

// Exercise browser cookie scoping and redirects over a real HTTPS listener, not
// just by hand-inserting a Cookie header into a recorder request.
func TestMigrationHTTPSRoundTripRetainsOriginalClientAtSameOrigin(t *testing.T) {
	client, _ := newSite(t)
	srv := httptest.NewTLSServer(client.DownloadsFallback(server.NewMux()))
	defer srv.Close()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	browser := srv.Client()
	browser.Jar = jar
	read := func(route string) (string, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), "GET", srv.URL+route, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := browser.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.Request.URL.String(), body
	}
	_, initial := read("/")
	if !bytes.Contains(initial, []byte("Download Dilla")) {
		t.Fatal("initial visit did not show downloads")
	}
	resp, legacy := read("/browser-migration")
	if resp != srv.URL+"/" || string(legacy) != site["index.html"] {
		t.Fatalf("migration changed origin or index: %s %s", resp, legacy)
	}
	_, asset := read("/assets/app-1a2b.js")
	if string(asset) != site["assets/app-1a2b.js"] {
		t.Fatal("original asset URL changed")
	}
	origin, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(jar.Cookies(origin)) != 1 {
		t.Fatal("HTTPS migration did not retain preference cookie")
	}
	resp, downloads := read("/browser-migration/exit")
	if resp != srv.URL+"/download" || !bytes.Contains(downloads, []byte("Download Dilla")) || len(jar.Cookies(origin)) != 0 {
		t.Fatalf("exit retained cookie or wrong destination: %s", resp)
	}
	_, after := read("/")
	if !bytes.Contains(after, []byte("Download Dilla")) {
		t.Fatal("home did not return to downloads")
	}
}

func TestDownloadBrandReusesTheDesktopAsset(t *testing.T) {
	client, _ := newSite(t)
	rec := do(t, client.DownloadsFallback(server.NewMux()), "GET", "/desktop-download-brand.png", nil)
	source, err := os.ReadFile("../../packages/desktop/assets/tray.png")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/png" || !bytes.Equal(rec.Body.Bytes(), source) {
		t.Fatal("download branding differs from the existing desktop asset")
	}
}
