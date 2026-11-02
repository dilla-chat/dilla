package web

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jonasthim/dilla/internal/server"
)

// These are separately embedded public documents, never executable client bytes.
// downloads/brand.png is the existing packages/desktop/assets/tray.png.
//
//go:embed downloads/index.html
var downloadHTML []byte

//go:embed downloads/style.css
var downloadCSS []byte

//go:embed downloads/brand.png
var downloadBrand []byte

var safeFont = regexp.MustCompile(`^assets/jetbrains-mono-latin-wght-normal-[A-Za-z0-9_-]+\.woff2$`)

const migrationCookie = "__Host-dilla-browser-migration"
const migrationLifetime = time.Hour

// DownloadsFallback explicitly selects desktop downloads for client navigation.
// The zero-value server option continues using Fallback; the original verified
// tree is retained for assets and temporary same-origin migration access.
func (h *Handler) DownloadsFallback(mux *server.Mux) http.Handler {
	css := bytes.Clone(downloadCSS)
	var fonts []string
	for name := range h.files {
		if match, _ := path.Match("assets/jetbrains-mono-latin-wght-normal-*.woff2", name); match {
			fonts = append(fonts, name)
		}
	}
	slices.Sort(fonts)
	if len(fonts) != 0 {
		// Only manifest-verified font names with a CSS-safe basename are referenced.
		font := fonts[0]
		if safeFont.MatchString(font) {
			css = append(css, []byte(`@font-face{font-family:"JetBrains Mono";font-style:normal;font-weight:100 800;font-display:swap;src:url("/`+font+`") format("woff2")}`)...)
		}
	}
	d := &downloadsHandler{client: h, page: downloadFile(downloadHTML, "text/html; charset=utf-8"), css: downloadFile(css, "text/css; charset=utf-8"), brand: downloadFile(downloadBrand, "image/png")}
	return h.fallback(mux, d)
}

type downloadsHandler struct {
	client           *Handler
	page, css, brand file
}

func downloadFile(data []byte, contentType string) file {
	return file{data: data, contentType: contentType, etag: fmt.Sprintf(`"%x"`, sha256.Sum256(data)), noStore: true}
}

func (d *downloadsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := clean(r.URL.Path)
	escaped := strings.ToLower(r.URL.EscapedPath())
	if strings.Contains(r.URL.Path, "..") || strings.ContainsAny(r.URL.Path, "\\\x00") || strings.Contains(escaped, "%2f") || strings.Contains(escaped, "%5c") {
		http.NotFound(w, r)
		return
	}
	if p == "/browser-migration" || p == "/browser-migration/exit" {
		// No query value is read or echoed, and redirects are always origin-relative.
		if r.URL.Path != p {
			http.NotFound(w, r)
			return
		}
		d.client.securityHeaders(w, r)
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		c := &http.Cookie{Name: migrationCookie, Value: "1", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: int(migrationLifetime / time.Second), Expires: time.Now().Add(migrationLifetime).UTC()}
		destination := "/"
		if p == "/browser-migration/exit" {
			c.Value = ""
			c.MaxAge = -1
			c.Expires = time.Unix(1, 0).UTC()
			destination = "/download"
		}
		http.SetCookie(w, c)
		// A fixed Location avoids Host-based absolute redirects, including behind a proxy.
		w.Header().Set("Location", destination)
		w.WriteHeader(http.StatusSeeOther)
		return
	}
	if strings.HasPrefix(p, "/browser-migration/") {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch p {
	case "/desktop-download.css":
		d.client.serve(w, r, d.css)
		return
	case "/desktop-download-brand.png":
		d.client.serve(w, r, d.brand)
		return
	}
	if p != "/" && p != "/index.html" && p != "/download" {
		if _, ok := d.client.files[strings.TrimPrefix(p, "/")]; ok || p == "/app-version.json" || strings.Contains(path.Base(p), ".") {
			d.client.ServeHTTP(w, r)
			return
		}
	}
	w.Header().Set("Vary", "Cookie")
	selected := d.page
	if cookie, err := r.Cookie(migrationCookie); p != "/download" && err == nil && cookie.Value == "1" {
		// UI preference only. Sessions, device membership and worker key ownership
		// still have their original checks; no browser storage is read or changed.
		selected = d.client.files["index.html"]
		selected.noStore = true
	}
	d.client.serve(w, r, selected)
}
