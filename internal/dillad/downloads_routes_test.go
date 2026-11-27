package dillad_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/desktopdownload"
	"github.com/dilla-chat/dilla/internal/dillad"
)

// /download/{platform} is always the desktop download routes (Task 12: no option turns them on), and /download is
// always the download page, with or without a served tree behind it (the test-only seam).
func TestDesktopDownloadRoutesAreAlwaysMounted(t *testing.T) {
	routes := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://github.com/dilla-chat/dilla/releases/download/desktop-v0.1.0-beta.2/dilla-0.1.0-beta.2-"+r.PathValue("platform"))
		w.WriteHeader(http.StatusFound)
	})
	for name, tree := range map[string]bool{"no tree": false, "a served tree": true} {
		t.Run(name, func(t *testing.T) {
			cfg, _ := testConfigInvite(t)
			o := dillad.Options{Config: cfg, Clock: clock.System(), Wasm: sharedRuntime(t), DesktopDownloadRoutes: routes}
			if tree {
				o.Web = webTree(t, map[string]string{"index.html": `<!doctype html><title>tree</title>`})
			}
			srv, err := dillad.New(t.Context(), o)
			if err != nil {
				t.Fatal(err)
			}
			defer srv.Shutdown(context.Background())
			rec := serveOne(t, srv.Handler(), "GET", "/download/linux-deb", "")
			if rec.Code != http.StatusFound || !strings.HasSuffix(rec.Header().Get("Location"), "-linux-deb") {
				t.Fatalf("downloads route = %d %v", rec.Code, rec.Header())
			}
			if page := serveOne(t, srv.Handler(), "GET", "/download", ""); !strings.Contains(page.Body.String(), "Download Dilla") {
				t.Fatalf("download page = %q", page.Body.String())
			}
		})
	}
}

// The zero DesktopDownloadChannel is beta.5's table (no linux-tar), and GPUIAlpha reaches the
// default handler: its feed is fetched (unreachable here, so a 503) for the tarball route.
func TestDesktopDownloadChannelReachesTheDefaultRoutes(t *testing.T) {
	unreachable := desktopdownload.GPUIAlpha
	unreachable.FeedURL = "https://127.0.0.1:1/dilla/gpui-alpha/desktop-gpui-alpha.json"
	for name, tc := range map[string]struct {
		ch   desktopdownload.Channel
		want int
	}{
		"zero value is beta": {desktopdownload.Channel{}, http.StatusNotFound},
		"beta":               {desktopdownload.Beta, http.StatusNotFound},
		"gpui-alpha":         {unreachable, http.StatusServiceUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, _ := testConfigInvite(t)
			srv, err := dillad.New(t.Context(), dillad.Options{Config: cfg, Clock: clock.System(), Wasm: sharedRuntime(t), DesktopDownloadChannel: tc.ch})
			if err != nil {
				t.Fatal(err)
			}
			defer srv.Shutdown(context.Background())
			if rec := serveOne(t, srv.Handler(), "GET", "/download/linux-tar", ""); rec.Code != tc.want {
				t.Fatalf("/download/linux-tar = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}
