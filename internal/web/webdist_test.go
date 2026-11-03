//go:build webdist

// Package web_test checks the real web build the way dillad serves it. Run with -tags webdist
// after the build; CI's go-release job sets DILLA_WEB_DIST to the downloaded internal/web/dist.
// The module-wide untagged run never compiles this file, so it never depends on a Node build.
package web_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/web"
)

// builtDist is the directory under test: DILLA_WEB_DIST when set (go-release: the bytes it is
// about to embed), else the build output beside this module (web-build and the local loop).
func builtDist() string {
	if d := os.Getenv("DILLA_WEB_DIST"); d != "" {
		return d
	}
	return filepath.FromSlash("../../packages/web/dist")
}

const wantCSP = "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self'; img-src 'self' blob:; " +
	"font-src 'self'; connect-src 'self' ws://127.0.0.1:8463 wss://127.0.0.1:8463; worker-src 'self'; " +
	"media-src blob:; object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

const wantPermissions = "camera=(self), microphone=(self), display-capture=(self), geolocation=()"

type manifest struct {
	V     int `json:"v"`
	Files []struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
		Size   int    `json:"size"`
	} `json:"files"`
}

func TestBuiltClientIsServed(t *testing.T) {
	dir := builtDist()
	h, err := web.New(os.DirFS(dir))
	if err != nil {
		t.Fatalf("web.New(%s): %v (set DILLA_WEB_DIST or run npm run build -w @dilla/web)", dir, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, web.ManifestName))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if m.V != 1 {
		t.Fatalf("manifest v = %d, want 1", m.V)
	}

	get := func(p string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, p, nil)
		req.Host = "127.0.0.1:8463"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	root := get("/")
	if root.Code != http.StatusOK {
		t.Fatalf("GET / = %d", root.Code)
	}
	if got := root.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("GET / Content-Type = %q", got)
	}
	if got := root.Header().Get("Content-Security-Policy"); got != wantCSP {
		t.Errorf("GET / CSP = %q\nwant %q", got, wantCSP)
	}
	if got := root.Header().Get("Permissions-Policy"); got != wantPermissions {
		t.Errorf("GET / Permissions-Policy = %q\nwant %q", got, wantPermissions)
	}
	body := root.Body.String()
	for _, want := range []string{`<div id="root" class="d-root">`, `<script type="module" crossorigin src="/assets/`} {
		if !strings.Contains(body, want) {
			t.Errorf("index.html lacks %q", want)
		}
	}
	for _, bad := range []string{`content="placeholder"`, "<style"} {
		if strings.Contains(body, bad) {
			t.Errorf("index.html contains %q", bad)
		}
	}

	wasm, scripts := 0, 0
	for _, f := range m.Files {
		rec := get("/" + f.Path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET /%s = %d", f.Path, rec.Code)
			continue
		}
		if rec.Body.Len() != f.Size {
			t.Errorf("GET /%s: %d bytes, manifest says %d", f.Path, rec.Body.Len(), f.Size)
		}
		if got := rec.Header().Get("ETag"); got != `"`+f.SHA256+`"` {
			t.Errorf("GET /%s ETag = %q", f.Path, got)
		}
		wantCache := "no-cache"
		if strings.HasPrefix(f.Path, "assets/") {
			wantCache = "public, max-age=31536000, immutable"
		}
		if got := rec.Header().Get("Cache-Control"); got != wantCache {
			t.Errorf("GET /%s Cache-Control = %q, want %q", f.Path, got, wantCache)
		}
		switch path.Ext(f.Path) {
		case ".wasm":
			wasm++
			if got := rec.Header().Get("Content-Type"); got != "application/wasm" {
				t.Errorf("GET /%s Content-Type = %q", f.Path, got)
			}
			if got := rec.Header().Get("Content-Security-Policy"); got != wantCSP {
				t.Errorf("GET /%s CSP = %q\nwant %q", f.Path, got, wantCSP)
			}
		case ".js":
			scripts++
			if got := rec.Header().Get("Content-Type"); got != "text/javascript; charset=utf-8" {
				t.Errorf("GET /%s Content-Type = %q", f.Path, got)
			}
			// The entry and the core worker: a dedicated worker takes its policy from its own
			// script response (ruling 31), so every script must carry it.
			if strings.HasPrefix(f.Path, "assets/") {
				if got := rec.Header().Get("Content-Security-Policy"); got != wantCSP {
					t.Errorf("GET /%s CSP = %q\nwant %q", f.Path, got, wantCSP)
				}
			}
		}
	}
	// web-3 task 8: one core wasm (the core and media workers share it) and RNNoise's, as check-entry-chunk.mjs.
	wantWasm := 2
	if raw, err := os.ReadFile(filepath.Join(dir, "noise-build.json")); err == nil {
		var build struct {
			Mode string `json:"mode"`
		}
		if err := json.Unmarshal(raw, &build); err != nil || (build.Mode != "evaluation" && build.Mode != "beta" && build.Mode != "qualified") {
			t.Fatal("invalid enhanced build identity")
		}
		wantWasm = 3
	}
	if wasm != wantWasm {
		t.Errorf("the build has %d wasm files, want %d for this build profile", wasm, wantWasm)
	}
	for _, glob := range []string{"assets/dilla_core_wasm_bg-*.wasm", "assets/rnnoise-*.wasm"} {
		n := 0
		for _, f := range m.Files {
			if ok, _ := path.Match(glob, f.Path); ok {
				n++
			}
		}
		if n != 1 {
			t.Errorf("the build has %d %s, want 1", n, glob)
		}
	}
	if scripts < 2 {
		t.Errorf("the build has %d scripts, want the entry and the core worker at least", scripts)
	}

	spa := get("/c/" + strings.Repeat("ab", 16) + "/" + strings.Repeat("cd", 16))
	if spa.Code != http.StatusOK || !strings.Contains(spa.Body.String(), `<div id="root" class="d-root">`) {
		t.Errorf("a client route is not answered with index.html: %d", spa.Code)
	}
}
