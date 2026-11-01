package web_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/web"
)

type manifestEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int    `json:"size"`
}

type manifestDoc struct {
	V     int             `json:"v"`
	Files []manifestEntry `json:"files"`
}

// site is a small built client: the page, one hashed asset of each content type under /assets/,
// and root files outside it.
var site = map[string]string{
	"index.html":                      "<!doctype html><title>dilla</title>\n",
	"assets/app-1a2b.js":              "export const a = 1;\n",
	"assets/noise-model-deadbeef.bin": "model bytes",
	"assets/core_bg-5e6f.wasm":        "\x00asm\x01\x00\x00\x00",
	"assets/icon-3a4b.png":            "\x89PNG",
	"assets/logo-1e2f.svg":            "<svg xmlns=\"http://www.w3.org/2000/svg\"/>",
	"assets/mono-7a8b.woff2":          "wOF2fake",
	"assets/mono-9c0d.woff":           "wOFFfake",
	"assets/style-3c4d.css":           "body{}\n",
	"config.json":                     "{}\n",
	"favicon.ico":                     "ico",
	"robots.txt":                      "User-agent: *\n",
}

const notFound = "404 page not found\n"

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// tree builds the file system of files plus the manifest the writer (task 22) would emit for it.
func tree(t *testing.T, files map[string]string) (fstest.MapFS, manifestDoc) {
	t.Helper()
	fsys := fstest.MapFS{}
	doc := manifestDoc{V: 1, Files: []manifestEntry{}}
	for _, p := range slices.Sorted(maps.Keys(files)) {
		doc.Files = append(doc.Files, manifestEntry{Path: p, SHA256: sha(files[p]), Size: len(files[p])})
		fsys[p] = &fstest.MapFile{Data: []byte(files[p])}
	}
	writeManifest(t, fsys, doc)
	return fsys, doc
}

func writeManifest(t *testing.T, fsys fstest.MapFS, doc manifestDoc) {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal the manifest: %v", err)
	}
	fsys[web.ManifestName] = &fstest.MapFile{Data: append(raw, '\n')}
}

func newSite(t *testing.T) (*web.Handler, fstest.MapFS) {
	t.Helper()
	fsys, _ := tree(t, site)
	h, err := web.New(fsys)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h, fsys
}

// do sends one request; a "Host" entry in header sets r.Host.
func do(t *testing.T, h http.Handler, method, target string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, target, nil)
	for k, v := range header {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestNewAcceptsATreeItsManifestDescribes(t *testing.T) {
	newSite(t)
}

func TestBuildVersionEndpointTracksTheEntryAsset(t *testing.T) {
	files := maps.Clone(site)
	files["index.html"] = `<!doctype html><script type="module" crossorigin src="/assets/app-1a2b.js"></script>`
	fsys, _ := tree(t, files)
	h, err := web.New(fsys)
	if err != nil {
		t.Fatal(err)
	}
	if h.BuildEntry() != "/assets/app-1a2b.js" {
		t.Fatalf("served build=%q", h.BuildEntry())
	}
	got := do(t, h, http.MethodGet, "/app-version.json", nil)
	if got.Code != http.StatusOK || got.Body.String() != `{"entry":"/assets/app-1a2b.js"}` {
		t.Fatalf("version = %d %q", got.Code, got.Body.String())
	}
	if got.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("version cache = %q", got.Header().Get("Cache-Control"))
	}
	if got.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("version content type = %q", got.Header().Get("Content-Type"))
	}
}

// Fail-closed content: New refuses every tree its manifest does not describe exactly.
func TestNewRefusesATreeItsManifestDoesNotDescribe(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(f fstest.MapFS, d *manifestDoc) (rewrite bool)
		want string
	}{
		{"a file whose bytes changed at the same size", func(f fstest.MapFS, _ *manifestDoc) bool {
			f["index.html"] = &fstest.MapFile{Data: []byte(strings.ToUpper(site["index.html"]))}
			return false
		}, "web: index.html does not match its manifest sha256"},
		{"a file whose size changed", func(f fstest.MapFS, _ *manifestDoc) bool {
			f["assets/app-1a2b.js"] = &fstest.MapFile{Data: []byte(site["assets/app-1a2b.js"] + "//")}
			return false
		}, "web: assets/app-1a2b.js is 22 bytes, the manifest says 20"},
		{"a listed file that is missing", func(f fstest.MapFS, _ *manifestDoc) bool {
			delete(f, "favicon.ico")
			return false
		}, "web: favicon.ico is listed in the manifest but missing"},
		{"a file the manifest does not list", func(f fstest.MapFS, _ *manifestDoc) bool {
			f["assets/extra-0000.js"] = &fstest.MapFile{Data: []byte("alert(1)\n")}
			return false
		}, "web: assets/extra-0000.js is not listed in the manifest"},
		{"a symbolic link", func(f fstest.MapFS, _ *manifestDoc) bool {
			f["assets/link-0000.js"] = &fstest.MapFile{Data: []byte("app-1a2b.js"), Mode: fs.ModeSymlink}
			return false
		}, "web: assets/link-0000.js is not a regular file"},
		{"no index.html", func(f fstest.MapFS, d *manifestDoc) bool {
			delete(f, "index.html")
			d.Files = slices.DeleteFunc(d.Files, func(e manifestEntry) bool { return e.Path == "index.html" })
			return true
		}, "web: the manifest does not list index.html"},
		{"version 2", func(_ fstest.MapFS, d *manifestDoc) bool {
			d.V = 2
			return true
		}, "web: manifest version 2, want 1"},
		{"an extension with no content type", func(f fstest.MapFS, d *manifestDoc) bool {
			const p, body = "assets/app-1a2b.js.map", "{}"
			f[p] = &fstest.MapFile{Data: []byte(body)}
			d.Files = append(d.Files, manifestEntry{Path: p, SHA256: sha(body), Size: len(body)})
			slices.SortFunc(d.Files, func(a, b manifestEntry) int { return strings.Compare(a.Path, b.Path) })
			return true
		}, `web: assets/app-1a2b.js.map has no content type (extension ".map")`},
		{"unsorted entries", func(_ fstest.MapFS, d *manifestDoc) bool {
			d.Files[0], d.Files[1] = d.Files[1], d.Files[0]
			return true
		}, "web: manifest entry 1: paths are not in strictly ascending byte order"},
		{"a duplicated entry", func(_ fstest.MapFS, d *manifestDoc) bool {
			d.Files = append(d.Files, d.Files[len(d.Files)-1])
			return true
		}, "paths are not in strictly ascending byte order"},
		{"an upper-case digest", func(_ fstest.MapFS, d *manifestDoc) bool {
			d.Files[0].SHA256 = strings.ToUpper(d.Files[0].SHA256)
			return true
		}, "web: manifest entry 0: sha256 is not 64 lowercase hex digits"},
		{"a path that leaves the tree", func(_ fstest.MapFS, d *manifestDoc) bool {
			d.Files[0].Path = "../app.js"
			return true
		}, `web: manifest entry 0: path "../app.js" is not a valid relative path`},
		{"a negative size", func(_ fstest.MapFS, d *manifestDoc) bool {
			d.Files[0].Size = -1
			return true
		}, "web: manifest entry 0: negative size"},
		{"no manifest", func(f fstest.MapFS, _ *manifestDoc) bool {
			delete(f, web.ManifestName)
			return false
		}, "web: dilla-manifest.json: "},
		{"an unknown manifest field", func(f fstest.MapFS, _ *manifestDoc) bool {
			f[web.ManifestName] = &fstest.MapFile{Data: []byte(`{"v":1,"files":[],"extra":true}` + "\n")}
			return false
		}, "web: dilla-manifest.json: "},
		{"trailing data after the manifest", func(f fstest.MapFS, _ *manifestDoc) bool {
			f[web.ManifestName] = &fstest.MapFile{Data: []byte(`{"v":1,"files":[]} {}` + "\n")}
			return false
		}, "web: dilla-manifest.json: trailing data"},
	} {
		t.Run(c.name, func(t *testing.T) {
			fsys, doc := tree(t, site)
			if c.edit(fsys, &doc) {
				writeManifest(t, fsys, doc)
			}
			_, err := web.New(fsys)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("New = %v, want an error containing %q", err, c.want)
			}
		})
	}
	if _, err := web.New(nil); err == nil || err.Error() != "web: no client tree" {
		t.Fatalf("New(nil) = %v, want web: no client tree", err)
	}
}

// L-HTTP-10's five rules, in order, over the handler alone.
func TestTheHandlerRoutesByItsFiveRules(t *testing.T) {
	h, _ := newSite(t)
	index := site["index.html"]
	const hex32 = "0123456789abcdef0123456789abcdef"
	for _, c := range []struct {
		method, target string
		status         int
		body           string
	}{
		{http.MethodGet, "/", 200, index},
		{http.MethodGet, "/index.html", 200, index},
		{http.MethodGet, "/assets/app-1a2b.js", 200, site["assets/app-1a2b.js"]},
		{http.MethodGet, "/favicon.ico", 200, site["favicon.ico"]},
		{http.MethodGet, "/welcome", 200, index},
		{http.MethodGet, "/welcome?invite=abc", 200, index},
		{http.MethodGet, "/c/" + hex32, 200, index},
		{http.MethodGet, "/c/" + hex32 + "/" + hex32 + "/", 200, index},
		{http.MethodGet, "/gatewayx", 200, index},
		{http.MethodGet, "/rtcx/y", 200, index},
		{http.MethodGet, "/assets/missing-0000.js", 404, notFound},
		{http.MethodGet, "/missing.png", 404, notFound},
		{http.MethodGet, "/dilla-manifest.json", 404, notFound},
		{http.MethodGet, "/v1", 404, notFound},
		{http.MethodGet, "/v1/", 404, notFound},
		{http.MethodGet, "/v1/anything", 404, notFound},
		{http.MethodGet, "/v1/communities/" + hex32, 404, notFound},
		{http.MethodGet, "//v1/anything", 404, notFound},
		{http.MethodGet, "/gateway", 404, notFound},
		{http.MethodGet, "/gateway/x", 404, notFound},
		{http.MethodGet, "/rtc", 404, notFound},
		{http.MethodGet, "/rtc/validate", 404, notFound},
		{http.MethodGet, "/i", 404, notFound},
		{http.MethodGet, "/i/abcd", 404, notFound},
		{http.MethodGet, "/healthz", 404, notFound},
		{http.MethodGet, "/readyz", 404, notFound},
		{http.MethodGet, "/metrics", 404, notFound},
		{http.MethodGet, "/debug/sfu", 404, notFound},
		{http.MethodGet, "/a/../index.html", 404, notFound},
		{http.MethodGet, "/assets/..%2findex.html", 404, notFound},
		{http.MethodGet, "/a%00b", 404, notFound},
		{http.MethodPost, "/", 405, "method not allowed\n"},
		{http.MethodPut, "/assets/app-1a2b.js", 405, "method not allowed\n"},
		{http.MethodDelete, "/c/" + hex32, 405, "method not allowed\n"},
		{http.MethodOptions, "/", 405, "method not allowed\n"},
	} {
		rec := do(t, h, c.method, c.target, nil)
		if rec.Code != c.status || rec.Body.String() != c.body {
			t.Errorf("%s %s = %d %q, want %d %q", c.method, c.target, rec.Code, rec.Body.String(), c.status, c.body)
		}
		if c.status == 405 && rec.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("%s %s: Allow %q, want GET, HEAD", c.method, c.target, rec.Header().Get("Allow"))
		}
		// The handler's own refusals are not documents: they carry no policy and no ETag.
		if c.status >= 400 && (rec.Header().Get("Content-Security-Policy") != "" || rec.Header().Get("ETag") != "") {
			t.Errorf("%s %s = %d carries CSP %q, ETag %q; want neither", c.method, c.target, c.status,
				rec.Header().Get("Content-Security-Policy"), rec.Header().Get("ETag"))
		}
	}
	rec := do(t, h, http.MethodHead, "/", nil)
	if rec.Code != 200 || rec.Body.Len() != 0 || rec.Header().Get("Content-Length") != strconv.Itoa(len(index)) {
		t.Fatalf("HEAD / = %d, %d body bytes, Content-Length %q", rec.Code, rec.Body.Len(), rec.Header().Get("Content-Length"))
	}
}

const permissions = "camera=(self), microphone=(self), display-capture=(self), geolocation=()"

// cspExampleOrg is L-HTTP-10's policy, written out, for a request whose Host is example.org.
const cspExampleOrg = "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self'; " +
	"img-src 'self' blob:; font-src 'self'; connect-src 'self' ws://example.org wss://example.org; " +
	"worker-src 'self'; media-src blob:; object-src 'none'; base-uri 'none'; form-action 'none'; " +
	"frame-ancestors 'none'"

// Every file, whatever its extension, carries the same CSP: a dedicated worker takes its policy
// from its own script response (ruling 31), so the .js and .wasm rows matter as much as the page.
func TestEveryServedFileCarriesItsTypeCacheAndSecurityHeaders(t *testing.T) {
	h, _ := newSite(t)
	const immutable, revalidate = "public, max-age=31536000, immutable", "no-cache"
	for _, c := range []struct {
		target, file, contentType, cache string
	}{
		{"/", "index.html", "text/html; charset=utf-8", revalidate},
		{"/c/0123456789abcdef0123456789abcdef", "index.html", "text/html; charset=utf-8", revalidate},
		{"/assets/app-1a2b.js", "assets/app-1a2b.js", "text/javascript; charset=utf-8", immutable},
		{"/assets/style-3c4d.css", "assets/style-3c4d.css", "text/css; charset=utf-8", immutable},
		{"/assets/noise-model-deadbeef.bin", "assets/noise-model-deadbeef.bin", "application/octet-stream", immutable},
		{"/assets/core_bg-5e6f.wasm", "assets/core_bg-5e6f.wasm", "application/wasm", immutable},
		{"/assets/mono-7a8b.woff2", "assets/mono-7a8b.woff2", "font/woff2", immutable},
		{"/assets/mono-9c0d.woff", "assets/mono-9c0d.woff", "font/woff", immutable},
		{"/assets/logo-1e2f.svg", "assets/logo-1e2f.svg", "image/svg+xml", immutable},
		{"/assets/icon-3a4b.png", "assets/icon-3a4b.png", "image/png", immutable},
		{"/favicon.ico", "favicon.ico", "image/x-icon", revalidate},
		{"/robots.txt", "robots.txt", "text/plain; charset=utf-8", revalidate},
		{"/config.json", "config.json", "application/json", revalidate},
	} {
		rec := do(t, h, http.MethodGet, c.target, map[string]string{"Host": "example.org"})
		hd := rec.Header()
		want := map[string]string{
			"Content-Type":            c.contentType,
			"Content-Length":          strconv.Itoa(len(site[c.file])),
			"Cache-Control":           c.cache,
			"ETag":                    `"` + sha(site[c.file]) + `"`,
			"X-Content-Type-Options":  "nosniff",
			"Referrer-Policy":         "no-referrer",
			"X-Frame-Options":         "DENY",
			"Permissions-Policy":      permissions,
			"Content-Security-Policy": cspExampleOrg,
		}
		for k, v := range want {
			if hd.Get(k) != v {
				t.Errorf("GET %s: %s = %q, want %q", c.target, k, hd.Get(k), v)
			}
		}
		for _, absent := range []string{"Strict-Transport-Security", "Cross-Origin-Opener-Policy",
			"Cross-Origin-Embedder-Policy", "Access-Control-Allow-Origin"} {
			if hd.Get(absent) != "" {
				t.Errorf("GET %s sets %s", c.target, absent)
			}
		}
		if rec.Body.String() != site[c.file] {
			t.Errorf("GET %s served %q, want the bytes of %s", c.target, rec.Body.String(), c.file)
		}
	}
}

func TestTheCSPNamesTheWebSocketOriginOnlyForAWellFormedHost(t *testing.T) {
	h, _ := newSite(t)
	const head = "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self'; " +
		"img-src 'self' blob:; font-src 'self'; connect-src 'self'"
	const tail = "; worker-src 'self'; media-src blob:; object-src 'none'; base-uri 'none'; " +
		"form-action 'none'; frame-ancestors 'none'"
	for _, c := range []struct{ host, sources string }{
		{"dilla.test", " ws://dilla.test wss://dilla.test"},
		{"dilla.test:8443", " ws://dilla.test:8443 wss://dilla.test:8443"},
		{"127.0.0.1:8463", " ws://127.0.0.1:8463 wss://127.0.0.1:8463"},
		{"[::1]:8463", " ws://[::1]:8463 wss://[::1]:8463"},
		{"[::1]", " ws://[::1] wss://[::1]"},
		{strings.Repeat("a", 253), " ws://" + strings.Repeat("a", 253) + " wss://" + strings.Repeat("a", 253)},
		{strings.Repeat("a", 254), ""},
		{`dilla.test"`, ""},
		{"dilla.test; script-src *", ""},
		{"user@dilla.test", ""},
		{"dilla.test:123456", ""},
		{"", ""},
	} {
		for _, target := range []string{"/", "/assets/app-1a2b.js"} {
			rec := do(t, h, http.MethodGet, target, map[string]string{"Host": c.host})
			if got, want := rec.Header().Get("Content-Security-Policy"), head+c.sources+tail; got != want {
				t.Errorf("GET %s, Host %q: CSP\n %q\nwant\n %q", target, c.host, got, want)
			}
		}
	}
	if head+" ws://example.org wss://example.org"+tail != cspExampleOrg {
		t.Fatal("the two spellings of the policy in this file disagree")
	}
}

func TestAMatchingValidatorIsAnsweredNotModified(t *testing.T) {
	h, _ := newSite(t)
	etag := `"` + sha(site["assets/app-1a2b.js"]) + `"`
	for _, c := range []struct {
		inm    string
		status int
	}{
		{etag, 304},
		{`"0000", ` + etag, 304},
		{"W/" + etag, 304},
		{"*", 304},
		{`"0000"`, 200},
		{sha(site["assets/app-1a2b.js"]), 200},
	} {
		rec := do(t, h, http.MethodGet, "/assets/app-1a2b.js",
			map[string]string{"If-None-Match": c.inm, "Host": "example.org"})
		if rec.Code != c.status {
			t.Errorf("If-None-Match %q = %d, want %d", c.inm, rec.Code, c.status)
			continue
		}
		hd := rec.Header()
		// The script is the worker's policy source: a revalidated .js keeps the exact CSP.
		if got := hd.Get("Content-Security-Policy"); got != cspExampleOrg {
			t.Errorf("If-None-Match %q: %d with CSP %q, want %q", c.inm, rec.Code, got, cspExampleOrg)
		}
		if c.status == 304 {
			if rec.Body.Len() != 0 || hd.Get("ETag") != etag || hd.Get("Content-Type") != "" ||
				hd.Get("Content-Length") != "" || hd.Get("Cache-Control") != "public, max-age=31536000, immutable" ||
				hd.Get("X-Content-Type-Options") != "nosniff" || hd.Get("Permissions-Policy") != permissions {
				t.Errorf("If-None-Match %q: 304 with %d body bytes and headers %v", c.inm, rec.Body.Len(), hd)
			}
		}
	}
	page := do(t, h, http.MethodGet, "/",
		map[string]string{"If-None-Match": `"` + sha(site["index.html"]) + `"`, "Host": "example.org"})
	if page.Code != 304 || page.Header().Get("Content-Security-Policy") != cspExampleOrg {
		t.Fatalf("a revalidated page = %d with CSP %q, want 304 carrying %q", page.Code,
			page.Header().Get("Content-Security-Policy"), cspExampleOrg)
	}
}

// What New verified is what is served: the tree changing underneath changes nothing.
func TestTheHandlerServesTheBytesItVerified(t *testing.T) {
	h, fsys := newSite(t)
	fsys["index.html"] = &fstest.MapFile{Data: []byte("<!doctype html><title>changed</title>\n")}
	delete(fsys, "assets/app-1a2b.js")
	if rec := do(t, h, http.MethodGet, "/", nil); rec.Body.String() != site["index.html"] {
		t.Fatalf("GET / served %q after the tree changed", rec.Body.String())
	}
	if rec := do(t, h, http.MethodGet, "/assets/app-1a2b.js", nil); rec.Code != 200 || rec.Body.String() != site["assets/app-1a2b.js"] {
		t.Fatalf("GET /assets/app-1a2b.js = %d %q after the file was removed", rec.Code, rec.Body.String())
	}
}

// Fallback serves the client only where the mux has no route and the path is not reserved; every
// routed request keeps the mux's answer, its 404s and its 405s with their Allow lists. The
// method-less /rtc patterns are the ones a "GET /" pattern would collide with (ruling 32).
func TestFallbackLeavesEveryRouteOfTheMuxAlone(t *testing.T) {
	h, _ := newSite(t)
	teapot := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	mux := server.NewMux()
	mux.Handle("POST /v1/things", teapot)
	mux.Handle("GET /v1/things/{id}", teapot)
	mux.Handle("GET /healthz", teapot)
	mux.Handle("/rtc", teapot)
	mux.Handle("/rtc/", teapot)
	f := h.Fallback(mux)
	mux.Handle("GET /v1/late", teapot) // registered after Fallback was built, as Options.Extra is
	for _, c := range []struct {
		method, target string
		status         int
		allow, body    string
	}{
		{http.MethodPost, "/v1/things", 418, "", ""},
		{http.MethodGet, "/v1/things/x", 418, "", ""},
		{http.MethodGet, "/v1/late", 418, "", ""},
		{http.MethodGet, "/v1/things", 405, "POST", ""},
		{http.MethodDelete, "/v1/things/x", 405, "GET, HEAD", ""},
		{http.MethodPost, "/v1/nope", 404, "", notFound},
		{http.MethodGet, "/v1/nope", 404, "", notFound},
		{http.MethodGet, "/healthz", 418, "", ""},
		{http.MethodPost, "/rtc", 418, "", ""},
		{http.MethodGet, "/rtc/validate", 418, "", ""},
		{http.MethodGet, "/gateway", 404, "", notFound},
		{http.MethodPost, "/debug/sfu/token", 404, "", notFound},
		{http.MethodGet, "//v1/nope", http.StatusTemporaryRedirect, "", ""},
		{http.MethodGet, "/", 200, "", site["index.html"]},
		{http.MethodGet, "/c/x", 200, "", site["index.html"]},
		{http.MethodPost, "/", 405, "GET, HEAD", "method not allowed\n"},
	} {
		rec := do(t, f, c.method, c.target, nil)
		if rec.Code != c.status {
			t.Errorf("%s %s = %d, want %d", c.method, c.target, rec.Code, c.status)
		}
		if c.allow != "" && rec.Header().Get("Allow") != c.allow {
			t.Errorf("%s %s: Allow %q, want %q", c.method, c.target, rec.Header().Get("Allow"), c.allow)
		}
		if c.body != "" && rec.Body.String() != c.body {
			t.Errorf("%s %s: body %q, want %q", c.method, c.target, rec.Body.String(), c.body)
		}
	}
}

// L-HTTP-102 (web-3): the page may ask for the camera, the microphone and a screen, on its own
// origin only; nothing else is granted, and geolocation stays denied.
func TestPermissionsPolicyGrantsMediaToSelf(t *testing.T) {
	h, _ := newSite(t)
	for _, target := range []string{"/", "/assets/app-1a2b.js", "/assets/core_bg-5e6f.wasm"} {
		got := do(t, h, http.MethodGet, target, map[string]string{"Host": "example.org"}).Header().Get("Permissions-Policy")
		if got != "camera=(self), microphone=(self), display-capture=(self), geolocation=()" {
			t.Fatalf("GET %s Permissions-Policy = %q", target, got)
		}
	}
}

// L-HTTP-102, RV-5: media-src admits the page-minted blob: URLs of the attachment players and
// nothing the instance serves; the rest of the policy is unchanged.
func TestCSPMediaSrcIsBlobOnly(t *testing.T) {
	h, _ := newSite(t)
	csp := do(t, h, http.MethodGet, "/", map[string]string{"Host": "example.org"}).Header().Get("Content-Security-Policy")
	for _, want := range []string{"; media-src blob:;", "connect-src 'self'", "worker-src 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("the CSP lacks %q: %s", want, csp)
		}
	}
	for _, never := range []string{"media-src 'self'", "media-src 'none'", "media-src blob: "} {
		if strings.Contains(csp, never) {
			t.Errorf("the CSP contains %q: %s", never, csp)
		}
	}
}
