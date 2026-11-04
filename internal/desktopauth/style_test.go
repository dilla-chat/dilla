package desktopauth

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestHTTPPageLinksSameOriginStylesheetUnderStrictCSP(t *testing.T) {
	b := New(time.Now)
	h := Handler(b, "https://chat.test", nil)
	serve := func(method, path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), method, "https://chat.test/desktop-auth/"+path, nil))
		return w
	}
	css := serve("GET", "app.css")
	if css.Code != 200 || css.Header().Get("Content-Type") != "text/css; charset=utf-8" || css.Header().Get("X-Content-Type-Options") != "nosniff" || css.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(css.Code, css.Header())
	}
	const csp = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
	if css.Header().Get("Content-Security-Policy") != csp {
		t.Fatal(css.Header().Get("Content-Security-Policy"))
	}
	if !strings.Contains(css.Body.String(), "color-scheme: dark") || serve("POST", "app.css").Code == 200 {
		t.Fatal("stylesheet content or method")
	}
	if strings.Contains(page, "style=") || strings.Contains(page, "<style") || !strings.Contains(page, `<link rel="stylesheet" href="../app.css">`) {
		t.Fatal("the page must link the same-origin stylesheet and carry no inline style")
	}
	for _, kept := range []string{"<h1>Dilla desktop sign-in</h1>", `id="continue"`, ">Continue</button>", `id="status"`, "Continue only if you started this request in your Dilla desktop app."} {
		if !strings.Contains(page, kept) {
			t.Fatal("missing", kept)
		}
	}
}

// The stylesheet copies the design tokens it uses; this keeps the copy from drifting.
func TestAppCSSTokensMatchDesignTokens(t *testing.T) {
	tokens, err := os.ReadFile("../../packages/design-tokens/dist/tokens.css")
	if err != nil {
		t.Skip("design tokens not built:", err)
	}
	root := tokens[:bytes.Index(tokens, []byte("[data-theme"))]
	decl := regexp.MustCompile(`(--[a-z0-9-]+):\s*([^;]+);`)
	want := map[string]string{}
	for _, m := range decl.FindAllSubmatch(root, -1) {
		want[string(m[1])] = string(m[2])
	}
	block := appCSS[:bytes.Index(appCSS, []byte("}"))]
	checked := 0
	for _, m := range decl.FindAllSubmatch(block, -1) {
		if want[string(m[1])] != string(m[2]) {
			t.Errorf("%s: app.css %q, tokens %q", m[1], m[2], want[string(m[1])])
		}
		checked++
	}
	if checked < 15 {
		t.Fatal("too few tokens checked:", checked)
	}
}
