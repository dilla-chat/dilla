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

// /download/{platform} is a 404 without --desktop-downloads (never the browser client's
// navigation fallback), and the desktop download routes with it.
func TestDesktopDownloadRoutesFollowTheServerOption(t *testing.T) {
	const page = `<!doctype html><script type="module" src="/assets/app-1a2b.js"></script>`
	routes := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://github.com/dilla-chat/dilla/releases/download/desktop-v0.1.0-beta.2/dilla-0.1.0-beta.2-"+r.PathValue("platform"))
		w.WriteHeader(http.StatusFound)
	})
	for _, selected := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "downloads"}[selected], func(t *testing.T) {
			cfg, _ := testConfigInvite(t)
			srv, err := dillad.New(t.Context(), dillad.Options{Config: cfg, Clock: clock.System(), Wasm: sharedRuntime(t), Web: webTree(t, map[string]string{"index.html": page, "assets/app-1a2b.js": "/* original client */"}), DesktopDownloads: selected, DesktopDownloadRoutes: routes})
			if err != nil {
				t.Fatal(err)
			}
			defer srv.Shutdown(context.Background())
			rec := serveOne(t, srv.Handler(), "GET", "/download/linux-deb", "")
			if selected {
				if rec.Code != http.StatusFound || !strings.HasSuffix(rec.Header().Get("Location"), "-linux-deb") {
					t.Fatalf("downloads route = %d %v", rec.Code, rec.Header())
				}
			} else if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "<script") {
				t.Fatalf("flag off = %d %q", rec.Code, rec.Body.String())
			}
			if root := serveOne(t, srv.Handler(), "GET", "/download", ""); selected && !strings.Contains(root.Body.String(), "Download Dilla") {
				t.Fatalf("download page = %q", root.Body.String())
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
			srv, err := dillad.New(t.Context(), dillad.Options{Config: cfg, Clock: clock.System(), Wasm: sharedRuntime(t), Web: webTree(t, map[string]string{"index.html": `<!doctype html>`}), DesktopDownloads: true, DesktopDownloadChannel: tc.ch})
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
