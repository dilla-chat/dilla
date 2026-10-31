// Package web serves the browser client from dillad's own origin (C9): an embedded,
// manifest-verified tree behind the routing, header and CSP rules of protocol/09.
package web

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/jonasthim/dilla/internal/server"
)

const ManifestName = "dilla-manifest.json"
const maxManifestBytes = 1 << 20
const maxFileBytes = 64 << 20
const permissionsPolicy = "camera=(self), microphone=(self), display-capture=(self), geolocation=()"
const cspHead = "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self'; img-src 'self' blob:; font-src 'self'; connect-src 'self'"
const cspTail = "; worker-src 'self'; media-src blob:; object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

//go:embed all:dist
var dist embed.FS

var contentTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".wasm":  "application/wasm",
	".bin":   "application/octet-stream",
	".json":  "application/json",
	".woff2": "font/woff2",
	".woff":  "font/woff",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".ico":   "image/x-icon",
	".txt":   "text/plain; charset=utf-8",
}

var reservedPrefixes = []string{"/v1/", "/gateway", "/rtc", "/i/", "/healthz", "/readyz", "/metrics", "/debug/"}
var dnsHost = regexp.MustCompile(`^[A-Za-z0-9.-]{1,253}(:[0-9]{1,5})?$`)
var ipv6Host = regexp.MustCompile(`^\[[0-9A-Fa-f:.]{2,45}\](:[0-9]{1,5})?$`)
var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)
var entryScript = regexp.MustCompile(`<script\s+type="module"[^>]*\ssrc="(/assets/[A-Za-z0-9_./-]+\.js)"`)

// Embedded is the committed placeholder, or the real build when CI copied it in before go build.
func Embedded() fs.FS {
	tree, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(fmt.Sprintf("web: the embedded tree has no dist directory: %v", err))
	}
	return tree
}

type file struct {
	data              []byte
	contentType, etag string
	immutable         bool
}

// Handler serves one verified client tree; it is safe for concurrent use and immutable after New.
type Handler struct {
	files   map[string]file
	version []byte
	entry   string
}

type manifestEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type manifest struct {
	V     int             `json:"v"`
	Files []manifestEntry `json:"files"`
}

// New verifies fsys against its dilla-manifest.json and keeps the verified bytes.
func New(fsys fs.FS) (*Handler, error) {
	if fsys == nil {
		return nil, fmt.Errorf("web: no client tree")
	}
	info, err := fs.Stat(fsys, ManifestName)
	if err != nil {
		return nil, fmt.Errorf("web: %s: %w", ManifestName, err)
	}
	if info.Size() > maxManifestBytes {
		return nil, fmt.Errorf("web: %s is larger than 1048576 bytes", ManifestName)
	}
	raw, err := fs.ReadFile(fsys, ManifestName)
	if err != nil {
		return nil, fmt.Errorf("web: %s: %w", ManifestName, err)
	}
	var doc manifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&doc); err != nil {
		return nil, fmt.Errorf("web: %s: %w", ManifestName, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("web: %s: trailing data", ManifestName)
	}
	if doc.V != 1 {
		return nil, fmt.Errorf("web: manifest version %d, want 1", doc.V)
	}
	listed := make(map[string]manifestEntry, len(doc.Files))
	previous := ""
	for i, entry := range doc.Files {
		if !fs.ValidPath(entry.Path) || entry.Path == "." || entry.Path == ManifestName || strings.Contains(entry.Path, `\`) {
			return nil, fmt.Errorf("web: manifest entry %d: path %q is not a valid relative path", i, entry.Path)
		}
		if !sha256Hex.MatchString(entry.SHA256) {
			return nil, fmt.Errorf("web: manifest entry %d: sha256 is not 64 lowercase hex digits", i)
		}
		if entry.Size < 0 {
			return nil, fmt.Errorf("web: manifest entry %d: negative size", i)
		}
		if i > 0 && entry.Path <= previous {
			return nil, fmt.Errorf("web: manifest entry %d: paths are not in strictly ascending byte order", i)
		}
		if _, ok := contentTypes[path.Ext(entry.Path)]; !ok {
			return nil, fmt.Errorf("web: %s has no content type (extension %q)", entry.Path, path.Ext(entry.Path))
		}
		listed[entry.Path] = entry
		previous = entry.Path
	}
	if _, ok := listed["index.html"]; !ok {
		return nil, fmt.Errorf("web: the manifest does not list index.html")
	}
	h := &Handler{files: make(map[string]file, len(listed))}
	err = fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || name == ManifestName {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("web: %s is not a regular file", name)
		}
		entry, ok := listed[name]
		if !ok {
			return fmt.Errorf("web: %s is not listed in the manifest", name)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > maxFileBytes {
			return fmt.Errorf("web: %s is larger than 67108864 bytes", name)
		}
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		if int64(len(data)) != entry.Size {
			return fmt.Errorf("web: %s is %d bytes, the manifest says %d", name, len(data), entry.Size)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != entry.SHA256 {
			return fmt.Errorf("web: %s does not match its manifest sha256", name)
		}
		h.files[name] = file{data: data, contentType: contentTypes[path.Ext(name)], etag: `"` + entry.SHA256 + `"`, immutable: strings.HasPrefix(name, "assets/")}
		return nil
	})
	if err != nil {
		if strings.HasPrefix(err.Error(), "web: ") {
			return nil, err
		}
		return nil, fmt.Errorf("web: %w", err)
	}
	for _, entry := range doc.Files {
		if _, ok := h.files[entry.Path]; !ok {
			return nil, fmt.Errorf("web: %s is listed in the manifest but missing", entry.Path)
		}
	}
	entry := ""
	if match := entryScript.FindSubmatch(h.files["index.html"].data); match != nil {
		entry = string(match[1])
		if _, ok := h.files[strings.TrimPrefix(entry, "/")]; !ok {
			return nil, fmt.Errorf("web: index.html references missing entry %s", entry)
		}
	}
	h.entry = entry
	h.version, _ = json.Marshal(struct {
		Entry string `json:"entry"`
	}{entry})
	return h, nil
}

func clean(p string) string { return path.Clean("/" + p) }

func reserved(p string) bool {
	for _, prefix := range reservedPrefixes {
		root := strings.TrimSuffix(prefix, "/")
		if p == root || strings.HasPrefix(p, root+"/") {
			return true
		}
	}
	return false
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := clean(r.URL.Path)
	if reserved(p) || strings.Contains(r.URL.Path, "..") || strings.ContainsRune(r.URL.Path, 0) {
		http.NotFound(w, r)
		return
	}
	if p == "/app-version.json" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Length", strconv.Itoa(len(h.version)))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(h.version)
		}
		return
	}
	name := strings.TrimPrefix(p, "/")
	if p == "/" {
		name = "index.html"
	}
	if f, ok := h.files[name]; ok {
		h.serve(w, r, f)
		return
	}
	if !strings.Contains(path.Base(p), ".") {
		h.serve(w, r, h.files["index.html"])
		return
	}
	http.NotFound(w, r)
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, f file) {
	headers := w.Header()
	headers.Set("X-Content-Type-Options", "nosniff")
	headers.Set("Referrer-Policy", "no-referrer")
	headers.Set("X-Frame-Options", "DENY")
	headers.Set("Permissions-Policy", permissionsPolicy)
	headers.Set("ETag", f.etag)
	if f.immutable {
		headers.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		headers.Set("Cache-Control", "no-cache")
	}
	csp := cspHead
	if dnsHost.MatchString(r.Host) || ipv6Host.MatchString(r.Host) {
		csp += " ws://" + r.Host + " wss://" + r.Host
	}
	headers.Set("Content-Security-Policy", csp+cspTail)
	for _, validator := range strings.Split(r.Header.Get("If-None-Match"), ",") {
		validator = strings.TrimSpace(validator)
		validator = strings.TrimPrefix(validator, "W/")
		if validator == "*" || validator == f.etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	headers.Set("Content-Type", f.contentType)
	headers.Set("Content-Length", strconv.Itoa(len(f.data)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(f.data)
	}
}

// Fallback sends paths routed for their method, or under a reserved prefix, to the mux.
// It registers nothing and checks the mux on each request, so later registrations work.
func (h *Handler) Fallback(mux *server.Mux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pattern := mux.Mux().Handler(r)
		if pattern != "" || reserved(clean(r.URL.Path)) {
			mux.ServeHTTP(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// BuildEntry identifies the verified bundle actually served by this immutable handler.
func (h *Handler) BuildEntry() string { return h.entry }
