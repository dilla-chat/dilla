package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/dilla-chat/dilla/internal/dillad/dilladtest"
	"github.com/dilla-chat/dilla/internal/web"
)

// Task 12: dillad serves no browser client, but -web-root stays a test-only seam. The GPUI tests serve
// harness and strings trees through it (and a one-page tree to turn on the LiveKit URL rule), so a
// host built from the flags serves the given tree, with nothing embedded.
func TestTheWebRootFlagStillServesAGivenTree(t *testing.T) {
	const index = "<!doctype html><title>strings</title>\n"
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(index), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(index))
	manifest := `{"v":1,"files":[{"path":"index.html","sha256":"` + hex.EncodeToString(sum[:]) + `","size":` +
		strconv.Itoa(len(index)) + `}]}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, web.ManifestName), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	o, err := parseFlags([]string{"-web-root", dir})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	core, err := filepath.Abs(filepath.Join("..", "..", "internal", "mlswasi", "testdata", "dilla_core_wasi.wasm"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := dilladtest.NewHost(context.Background(), o.hostOptions(t.TempDir(), core, ""))
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	t.Cleanup(func() { _ = h.Close(context.Background()) })
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != index {
		t.Fatalf("GET / = %d %q, want the -web-root tree", rec.Code, rec.Body.String())
	}
}
