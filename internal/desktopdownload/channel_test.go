package desktopdownload_test

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/desktopdownload"
)

const alphaVersion = "0.2.0-alpha.1"

// alphaDoc is a GPUI feed document as scripts/desktop-gpui-feed.mjs writes it: beta.5 format 1,
// channel "alpha", JSON.stringify bytes plus a newline.
func alphaDoc(mutate func(map[string]any)) []byte {
	var assets []map[string]any
	for _, suffix := range gpuiFeedSuffixes {
		assets = append(assets, map[string]any{"name": "dilla-" + alphaVersion + "-" + suffix, "size": 1, "sha512": "x"})
	}
	doc := map[string]any{
		"format": 1, "channel": "alpha", "version": alphaVersion,
		"publishedAt": now.Add(-time.Hour).Format(time.RFC3339), "expiresAt": now.Add(89 * 24 * time.Hour).Format(time.RFC3339),
		"assets": assets,
	}
	if mutate != nil {
		mutate(doc)
	}
	b, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return append(b, '\n')
}

func channelHandler(t *testing.T, ch desktopdownload.Channel, s *served, keys []ed25519.PublicKey) http.Handler {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		if strings.HasSuffix(r.URL.Path, ".sig") {
			_, _ = w.Write(s.sig)
			return
		}
		_, _ = w.Write(s.feed)
	}))
	t.Cleanup(srv.Close)
	ch.FeedURL = srv.URL + "/dilla/feed.json"
	h := desktopdownload.New(desktopdownload.Options{Channel: ch, Client: srv.Client(), Keys: keys, Now: func() time.Time { return now }})
	mux := http.NewServeMux()
	mux.Handle("GET /download/{platform}", h)
	return mux
}

func TestGPUIAlphaRedirectsStayUnderItsOwnTag(t *testing.T) {
	pub, priv := newKey(t)
	feed := alphaDoc(nil)
	h := channelHandler(t, desktopdownload.GPUIAlpha, &served{feed: feed, sig: sign(priv, feed)}, []ed25519.PublicKey{pub})
	want := map[string]string{
		"linux-appimage": "linux-x64.AppImage",
		"linux-deb":      "linux-x64.deb",
		"linux-rpm":      "linux-x64.rpm",
		"linux-aur":      "linux-x64.aur.tar.gz",
		"linux-tar":      "linux-x64.tar.gz",
		"mac-arm64":      "mac-arm64.dmg",
		"win-x64":        "win-x64-setup.exe",
	}
	if len(desktopdownload.GPUIAlpha.Platforms) != len(want) {
		t.Fatalf("GPUIAlpha serves %d platforms, want %d", len(desktopdownload.GPUIAlpha.Platforms), len(want))
	}
	const base = "https://github.com/dilla-chat/dilla/releases/download/desktop-gpui-v" + alphaVersion + "/"
	for platform, suffix := range want {
		rec := get(h, "/download/"+platform)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != base+"dilla-"+alphaVersion+"-"+suffix {
			t.Errorf("%s: %d %q", platform, rec.Code, rec.Header().Get("Location"))
		}
	}
}

// A document signed for one channel is never accepted by the other, in either direction, at
// the Verify boundary and through the handler.
func TestCrossChannelReplayIsRefused(t *testing.T) {
	pub, priv := newKey(t)
	keys := []ed25519.PublicKey{pub}
	betaFeed, alphaFeed := feedDoc(nil), alphaDoc(nil)
	for name, tc := range map[string]struct {
		ch   desktopdownload.Channel
		feed []byte
	}{
		"beta document on the gpui-alpha channel": {desktopdownload.GPUIAlpha, betaFeed},
		"gpui document on the beta channel":       {desktopdownload.Beta, alphaFeed},
		// Only the channel field differs: the version alone must not let it through.
		"alpha version labelled beta, on gpui-alpha": {desktopdownload.GPUIAlpha, alphaDoc(func(d map[string]any) { d["channel"] = "beta" })},
		"beta version labelled alpha, on beta":       {desktopdownload.Beta, feedDoc(func(d map[string]any) { d["channel"] = "alpha" })},
		// Only the version differs: the channel field alone must not let it through.
		"beta version labelled alpha, on gpui-alpha": {desktopdownload.GPUIAlpha, feedDoc(func(d map[string]any) { d["channel"] = "alpha" })},
		"alpha version labelled beta, on beta":       {desktopdownload.Beta, alphaDoc(func(d map[string]any) { d["channel"] = "beta" })},
		"gpui-alpha as the channel field":            {desktopdownload.GPUIAlpha, alphaDoc(func(d map[string]any) { d["channel"] = "gpui-alpha" })},
	} {
		t.Run(name, func(t *testing.T) {
			sig := sign(priv, tc.feed)
			if _, err := desktopdownload.Verify(tc.ch, tc.feed, sig, keys, now); err == nil {
				t.Fatal("Verify accepted a document from another channel")
			}
			refused(t, channelHandler(t, tc.ch, &served{feed: tc.feed, sig: sig}, keys))
		})
	}
	// Each document verifies on its own channel, so the refusals above are about the channel.
	if _, err := desktopdownload.Verify(desktopdownload.Beta, betaFeed, sign(priv, betaFeed), keys, now); err != nil {
		t.Fatalf("beta on beta: %v", err)
	}
	if _, err := desktopdownload.Verify(desktopdownload.GPUIAlpha, alphaFeed, sign(priv, alphaFeed), keys, now); err != nil {
		t.Fatalf("alpha on gpui-alpha: %v", err)
	}
}

// A verified Feed builds its redirect from its own channel: the beta tag and table never
// serve a GPUI version, and the GPUI tag never a beta one.
func TestGPUIAlphaRefusesForeignVersions(t *testing.T) {
	pub, priv := newKey(t)
	for name, mutate := range map[string]func(map[string]any){
		"stable version": func(d map[string]any) { d["version"] = "0.2.0" },
		"leading zero":   func(d map[string]any) { d["version"] = "0.2.0-alpha.01" },
		"setup.exe not listed": func(d map[string]any) {
			d["assets"] = []map[string]any{{"name": "dilla-" + alphaVersion + "-linux-x64.deb"}}
		},
		"version with a path":     func(d map[string]any) { d["version"] = "0.2.0-alpha.1/../../evil" },
		"lifetime past 93 days":   func(d map[string]any) { d["expiresAt"] = now.Add(93 * 24 * time.Hour).Format(time.RFC3339) },
		"published in the future": func(d map[string]any) { d["publishedAt"] = now.Add(time.Hour).Format(time.RFC3339) },
		"format 2":                func(d map[string]any) { d["format"] = 2 },
		"empty channel":           func(d map[string]any) { d["channel"] = "" },
		"setup.exe listed twice": func(d map[string]any) {
			d["assets"] = append(d["assets"].([]map[string]any), map[string]any{"name": "dilla-" + alphaVersion + "-win-x64-setup.exe"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			feed := alphaDoc(mutate)
			h := channelHandler(t, desktopdownload.GPUIAlpha, &served{feed: feed, sig: sign(priv, feed)}, []ed25519.PublicKey{pub})
			rec := get(h, "/download/win-x64")
			if rec.Code == http.StatusFound {
				t.Fatalf("redirected to %q", rec.Header().Get("Location"))
			}
		})
	}
}

func TestEachChannelServesOnlyItsOwnPlatforms(t *testing.T) {
	pub, priv := newKey(t)
	feed := feedDoc(nil)
	beta := channelHandler(t, desktopdownload.Beta, &served{feed: feed, sig: sign(priv, feed)}, []ed25519.PublicKey{pub})
	if rec := get(beta, "/download/linux-tar"); rec.Code != http.StatusNotFound {
		t.Fatalf("beta /download/linux-tar = %d, want 404", rec.Code)
	}
	if rec := get(beta, "/download/win-x64"); rec.Header().Get("Location") != "https://github.com/dilla-chat/dilla/releases/download/desktop-v"+version+"/dilla-"+version+"-win-x64.exe" {
		t.Fatalf("beta win-x64 = %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

type recordingTransport struct {
	mu   sync.Mutex
	urls []string
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.urls = append(r.urls, req.URL.String())
	r.mu.Unlock()
	return nil, errors.New("offline")
}

// The zero Options read beta.5's feed exactly as before the channel flag; GPUIAlpha reads the
// GPUI feed Task 9 signs.
func TestEachChannelReadsItsPinnedFeed(t *testing.T) {
	for name, tc := range map[string]struct {
		opts desktopdownload.Options
		want string
	}{
		"zero value": {desktopdownload.Options{}, "https://dilla-chat.github.io/dilla/beta/desktop-beta.json"},
		"beta":       {desktopdownload.Options{Channel: desktopdownload.Beta}, "https://dilla-chat.github.io/dilla/beta/desktop-beta.json"},
		"gpui-alpha": {desktopdownload.Options{Channel: desktopdownload.GPUIAlpha}, "https://dilla-chat.github.io/dilla/gpui-alpha/desktop-gpui-alpha.json"},
	} {
		t.Run(name, func(t *testing.T) {
			rt := &recordingTransport{}
			tc.opts.Client = &http.Client{Transport: rt}
			mux := http.NewServeMux()
			mux.Handle("GET /download/{platform}", desktopdownload.New(tc.opts))
			refused(t, mux)
			if len(rt.urls) != 1 || rt.urls[0] != tc.want {
				t.Fatalf("fetched %v, want [%s]", rt.urls, tc.want)
			}
		})
	}
	if desktopdownload.Beta.Name != "beta" || desktopdownload.GPUIAlpha.Name != "alpha" {
		t.Fatalf("channel names %q %q must equal the feeds' channel fields", desktopdownload.Beta.Name, desktopdownload.GPUIAlpha.Name)
	}
}

func TestChannelFlagNames(t *testing.T) {
	for flag, want := range map[string]string{"beta": "beta", "gpui-alpha": "alpha"} {
		ch, err := desktopdownload.ChannelNamed(flag)
		if err != nil || ch.Name != want {
			t.Fatalf("ChannelNamed(%q) = %q, %v", flag, ch.Name, err)
		}
	}
	for _, flag := range []string{"", "alpha", "Beta", "gpui", "stable", "gpui-alpha "} {
		if _, err := desktopdownload.ChannelNamed(flag); err == nil {
			t.Fatalf("ChannelNamed(%q) accepted", flag)
		}
	}
}
