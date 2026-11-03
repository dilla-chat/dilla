package dilladtest_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/dillad/dilladtest"
	"github.com/dilla-chat/dilla/internal/web"
)

// webRootDir writes a built client and its manifest to a directory, as `npm run build -w
// @dilla/web` leaves packages/web/dist.
func webRootDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	type entry struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
		Size   int    `json:"size"`
	}
	doc := struct {
		V     int     `json:"v"`
		Files []entry `json:"files"`
	}{V: 1, Files: []entry{}}
	for _, p := range slices.Sorted(maps.Keys(files)) {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(files[p]), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		sum := sha256.Sum256([]byte(files[p]))
		doc.Files = append(doc.Files, entry{Path: p, SHA256: hex.EncodeToString(sum[:]), Size: len(files[p])})
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, web.ManifestName), append(raw, '\n'), 0o600); err != nil {
		t.Fatalf("write the manifest: %v", err)
	}
	return dir
}

func hostWithWebRoot(t *testing.T, webRoot string) (*dilladtest.Host, error) {
	t.Helper()
	core, err := filepath.Abs(filepath.Join("..", "..", "mlswasi", "testdata", "dilla_core_wasi.wasm"))
	if err != nil {
		t.Fatalf("core path: %v", err)
	}
	if _, err := os.Stat(core); err != nil {
		t.Fatalf("%s is missing: build it with\n"+
			"  cargo build -p dilla-core-wasi --target wasm32-wasip1 --release --locked\n"+
			"and copy it there (CI downloads the rust-wasi job's artifact): %v", core, err)
	}
	h, err := dilladtest.NewHost(context.Background(), dilladtest.HostOptions{
		DataDir: t.TempDir(), CorePath: core, WebRoot: webRoot,
	})
	if err == nil {
		t.Cleanup(func() { _ = h.Close(context.Background()) })
	}
	return h, err
}

func pageOf(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
	return rec
}

// C18: the test host serves a built client from its own origin, the same handler production runs.
func TestTheHostServesItsWebRootFromItsOwnOrigin(t *testing.T) {
	const index, script = "<!doctype html><title>harness</title>\n", "export {};\n"
	root := webRootDir(t, map[string]string{"index.html": index, "assets/main-1a2b.js": script})
	h, err := hostWithWebRoot(t, root)
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	if rec := pageOf(t, h.Handler(), "/"); rec.Code != http.StatusOK || rec.Body.String() != index ||
		rec.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("GET / = %d %q (%s)", rec.Code, rec.Body.String(), rec.Header().Get("Content-Type"))
	}
	if rec := pageOf(t, h.Handler(), "/assets/main-1a2b.js"); rec.Body.String() != script ||
		rec.Header().Get("Content-Type") != "text/javascript; charset=utf-8" {
		t.Fatalf("GET /assets/main-1a2b.js = %d %q (%s)", rec.Code, rec.Body.String(), rec.Header().Get("Content-Type"))
	}
}

func TestTheHostRefusesAWebRootItsManifestDoesNotDescribe(t *testing.T) {
	const index = "<!doctype html><title>harness</title>\n"
	root := webRootDir(t, map[string]string{"index.html": index})
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte(strings.ToUpper(index)), 0o600); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if _, err := hostWithWebRoot(t, root); err == nil || !strings.HasPrefix(err.Error(), "dillad: web client: ") {
		t.Fatalf("NewHost = %v, want dillad's web client refusal", err)
	}
}

func TestWithoutAWebRootTheHostServesThePlaceholder(t *testing.T) {
	h := newHost(t)
	if rec := pageOf(t, h.Handler(), "/"); !strings.Contains(rec.Body.String(), `<meta name="dilla-web" content="placeholder">`) {
		t.Fatalf("GET / = %d %q, want the embedded placeholder", rec.Code, rec.Body.String())
	}
}
