package web

import (
	"crypto/sha256"
	_ "embed"
	"fmt"
	"net/http"
	"path"
	"strings"
)

// These are separately embedded public documents, never executable client bytes.
// downloads/brand.png is the existing clients/desktop/assets/brand/tray.png.
//
//go:embed downloads/index.html
var downloadHTML []byte

//go:embed downloads/style.css
var downloadCSS []byte

//go:embed downloads/brand.png
var downloadBrand []byte

// downloads/jetbrains-mono.woff2 is @fontsource-variable/jetbrains-mono 5.3.0's
// files/jetbrains-mono-latin-wght-normal.woff2 (40404 bytes, SHA-256
// 18be452724bfdc236c074ca94a249a7f41a86752c7d04ab258ce9ed5651f6a7e), the file the browser client
// bundled; downloads/OFL-JetBrainsMono.txt is its SIL Open Font License 1.1.
//
//go:embed downloads/jetbrains-mono.woff2
var downloadFont []byte

// FontPath is where the embedded JetBrains Mono is served. The download page's stylesheet and the
// invite page (api.Deps.LandingFont) name it; their CSPs allow font-src 'self'.
const FontPath = "/jetbrains-mono.woff2"

// The download page's own policy: no script, no connection, no frame, nothing but its stylesheet,
// its picture and its font from this origin, and no powerful feature.
const pageCSP = "default-src 'none'; style-src 'self'; img-src 'self'; font-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
const pagePermissions = "camera=(), microphone=(), display-capture=(), geolocation=()"

// pages answers every path the mux leaves to Fallback: /download and its three assets, then the
// served tree when there is one (the test-only seam), else a 302 to /download for any extensionless
// path (the former browser-client routes, / included) and a 404 for the rest.
type pages struct {
	tree                   *Handler
	page, css, brand, font file
}

func newPages(tree *Handler) *pages {
	return &pages{
		tree:  tree,
		page:  pageFile(downloadHTML, "text/html; charset=utf-8", true),
		css:   pageFile(downloadCSS, "text/css; charset=utf-8", false),
		brand: pageFile(downloadBrand, "image/png", false),
		font:  pageFile(downloadFont, "font/woff2", false),
	}
}

func pageFile(data []byte, contentType string, noStore bool) file {
	return file{data: data, contentType: contentType, etag: fmt.Sprintf(`"%x"`, sha256.Sum256(data)), noStore: noStore}
}

func (d *pages) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := clean(r.URL.Path)
	escaped := strings.ToLower(r.URL.EscapedPath())
	if strings.Contains(r.URL.Path, "..") || strings.ContainsAny(r.URL.Path, "\\\x00") || strings.Contains(escaped, "%2f") || strings.Contains(escaped, "%5c") {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var f *file
	switch p {
	case "/download":
		f = &d.page
	case "/desktop-download.css":
		f = &d.css
	case "/desktop-download-brand.png":
		f = &d.brand
	case FontPath:
		f = &d.font
	}
	if f != nil {
		headers := w.Header()
		headers.Set("X-Content-Type-Options", "nosniff")
		headers.Set("Referrer-Policy", "no-referrer")
		headers.Set("X-Frame-Options", "DENY")
		headers.Set("Permissions-Policy", pagePermissions)
		headers.Set("Content-Security-Policy", pageCSP)
		serveFile(w, r, *f)
		return
	}
	if d.tree != nil {
		d.tree.ServeHTTP(w, r)
		return
	}
	if p == "/index.html" || !strings.Contains(path.Base(p), ".") {
		// A fixed, origin-relative Location: nothing of the request (its Host, its query, an invite
		// code in ?invite=) reaches the answer, and no body is written.
		w.Header().Set("Location", "/download")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusFound)
		return
	}
	http.NotFound(w, r)
}
