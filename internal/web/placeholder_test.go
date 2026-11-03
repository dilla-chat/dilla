//go:build !webdist

package web_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/web"
)

// The committed placeholder is a tree its own manifest describes, and it is what a binary built
// without the client serves at /. Not built under -tags webdist: CI's go-release job runs the
// webdist test with the real build downloaded into internal/web/dist, where these placeholder
// assertions would be false.
func TestTheEmbeddedPlaceholderIsAVerifiedTree(t *testing.T) {
	h, err := web.New(web.Embedded())
	if err != nil {
		t.Fatalf("New(Embedded()): %v", err)
	}
	rec := do(t, h, http.MethodGet, "/", nil)
	body := rec.Body.String()
	if rec.Code != http.StatusOK ||
		!strings.Contains(body, `<meta name="dilla-web" content="placeholder">`) ||
		!strings.Contains(body, "<p>dilla: this build does not include the web client.</p>") ||
		strings.Contains(body, "<script") || strings.Contains(body, "<style") {
		t.Fatalf("GET / = %d %q", rec.Code, body)
	}
	if rec.Body.Len() != 288 {
		t.Fatalf("the placeholder is %d bytes, want the committed 288", rec.Body.Len())
	}
	if got := rec.Header().Get("ETag"); got != `"53dbf07c74d975d3d41cd602d34cf60f1ca4c76dbdec248fbe75977c7b9112cc"` {
		t.Fatalf("the placeholder's ETag is %s: its bytes are not the committed 288", got)
	}
}
