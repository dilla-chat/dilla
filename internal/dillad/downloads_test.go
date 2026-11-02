package dillad_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jonasthim/dilla/internal/clock"
	"github.com/jonasthim/dilla/internal/dillad"
)

func TestDesktopDownloadsAreAnExplicitServerOption(t *testing.T) {
	const page = `<!doctype html><script type="module" src="/assets/app-1a2b.js"></script>`
	for _, selected := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "downloads"}[selected], func(t *testing.T) {
			cfg, code := testConfigInvite(t)
			srv, err := dillad.New(t.Context(), dillad.Options{Config: cfg, Clock: clock.System(), Wasm: sharedRuntime(t), Web: webTree(t, map[string]string{"index.html": page, "assets/app-1a2b.js": "/* original client */"}), DesktopDownloads: selected})
			if err != nil {
				t.Fatal(err)
			}
			defer srv.Shutdown(context.Background())
			h := srv.Handler()
			root := serveOne(t, h, "GET", "/", "")
			if selected {
				if !strings.Contains(root.Body.String(), "Download Dilla") || strings.Contains(root.Body.String(), "<script") {
					t.Fatalf("downloads root = %q", root.Body.String())
				}
			} else if root.Body.String() != page {
				t.Fatalf("default root changed: %q", root.Body.String())
			}
			for _, tc := range []struct {
				method, path string
				status       int
			}{
				{"GET", "/assets/app-1a2b.js", 200}, {"GET", "/v1/instance", 200}, {"GET", "/v1/not-a-route", 404},
				{"GET", "/desktop-auth/app.js", 200}, {"POST", "/desktop-auth/not-a-route", 404}, {"GET", "/desktop-auth/not-a-route", 404},
				{"GET", "/v1/auth/oidc/callback", 501}, {"POST", "/", 405},
			} {
				if rec := serveOne(t, h, tc.method, tc.path, ""); rec.Code != tc.status {
					t.Errorf("%s %s = %d want %d", tc.method, tc.path, rec.Code, tc.status)
				}
			}
			invite := serveOne(t, h, http.MethodGet, "/i/"+code, "")
			if invite.Code != 200 {
				t.Fatalf("invite=%d %s", invite.Code, invite.Body.String())
			}
			if selected {
				if !strings.Contains(invite.Body.String(), `href="dilla://invite/dilla.test/`+code+`"`) || !strings.Contains(invite.Body.String(), `href="/download"`) || strings.Contains(invite.Body.String(), "Open dilla in this browser") {
					t.Fatalf("desktop invite = %s", invite.Body.String())
				}
			} else if !strings.Contains(invite.Body.String(), `href="/welcome?invite=`+code+`"`) {
				t.Fatalf("default invite changed: %s", invite.Body.String())
			}
		})
	}
}
