package desktopdownload_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/desktopdownload"
)

var now = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

const version = "0.1.0-beta.2"

func allAssets(v string) []map[string]any {
	var out []map[string]any
	for _, suffix := range []string{"linux-x64.AppImage", "linux-x64.deb", "linux-x64.rpm", "linux-x64.aur.tar.gz", "mac-arm64.dmg", "mac-arm64.zip", "win-x64.exe", "linux-x64.sandbox"} {
		out = append(out, map[string]any{"name": "dilla-" + v + "-" + suffix, "size": 1, "sha512": "x"})
	}
	return out
}

func feedDoc(mutate func(map[string]any)) []byte {
	doc := map[string]any{
		"format": 1, "version": version, "channel": "beta",
		"publishedAt": now.Add(-time.Hour).Format(time.RFC3339), "expiresAt": now.Add(89 * 24 * time.Hour).Format(time.RFC3339),
		"assets": allAssets(version),
		// The artifacts' URLs are never used: the redirect is built from the version and the asset name.
		"artifacts": []map[string]any{{"platform": "linux", "arch": "x64", "url": "https://evil.example/dilla.AppImage"}},
	}
	if mutate != nil {
		mutate(doc)
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		panic(err)
	}
	return append(b, '\n')
}

func sign(key ed25519.PrivateKey, feed []byte) []byte {
	return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(key, feed)) + "\n")
}

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

type served struct {
	feed, sig []byte
	status    int
	hits      atomic.Int32
}

func feedServer(t *testing.T, s *served) (*httptest.Server, *time.Time) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		if s.status != 0 {
			w.WriteHeader(s.status)
			return
		}
		switch r.URL.Path {
		case "/dilla/beta/desktop-beta.json":
			_, _ = w.Write(s.feed)
		case "/dilla/beta/desktop-beta.json.sig":
			_, _ = w.Write(s.sig)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	clock := now
	return srv, &clock
}

func handler(t *testing.T, s *served, keys []ed25519.PublicKey) (http.Handler, *time.Time) {
	t.Helper()
	srv, clock := feedServer(t, s)
	h := desktopdownload.New(desktopdownload.Options{FeedURL: srv.URL + "/dilla/beta/desktop-beta.json", Client: srv.Client(), Keys: keys, Now: func() time.Time { return *clock }})
	mux := http.NewServeMux()
	mux.Handle("GET /download/{platform}", h)
	return mux, clock
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil))
	return rec
}

func TestSignedFeedRedirectsEveryPlatformToItsVersionedAsset(t *testing.T) {
	pub, priv := newKey(t)
	feed := feedDoc(nil)
	s := &served{feed: feed, sig: sign(priv, feed)}
	h, _ := handler(t, s, []ed25519.PublicKey{pub})
	want := map[string]string{
		"linux-appimage": "dilla-0.1.0-beta.2-linux-x64.AppImage",
		"linux-deb":      "dilla-0.1.0-beta.2-linux-x64.deb",
		"linux-rpm":      "dilla-0.1.0-beta.2-linux-x64.rpm",
		"linux-aur":      "dilla-0.1.0-beta.2-linux-x64.aur.tar.gz",
		"mac-arm64":      "dilla-0.1.0-beta.2-mac-arm64.dmg",
		"win-x64":        "dilla-0.1.0-beta.2-win-x64.exe",
	}
	for platform, name := range want {
		rec := get(h, "/download/"+platform)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "https://github.com/dilla-chat/dilla/releases/download/desktop-v0.1.0-beta.2/"+name || rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: %d %q", platform, rec.Code, rec.Header().Get("Location"))
		}
	}
	// Six redirects, one verified fetch (feed and signature): the feed is cached.
	if hits := s.hits.Load(); hits != 2 {
		t.Fatalf("feed fetched %d times, want 2 (cached)", hits)
	}
}

func TestUnknownPlatformIsNotFound(t *testing.T) {
	pub, priv := newKey(t)
	feed := feedDoc(nil)
	h, _ := handler(t, &served{feed: feed, sig: sign(priv, feed)}, []ed25519.PublicKey{pub})
	for _, p := range []string{"mac-x64", "linux", "win-arm64", "linux-appimage.exe", "LINUX-DEB"} {
		if rec := get(h, "/download/"+p); rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d", p, rec.Code)
		}
	}
}

func refused(t *testing.T, h http.Handler) {
	t.Helper()
	rec := get(h, "/download/linux-deb")
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Location") != "" || !strings.Contains(rec.Body.String(), `href="https://github.com/dilla-chat/dilla/releases"`) {
		t.Fatalf("not refused: %d %q %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
}

func TestBadSignatureIsRefused(t *testing.T) {
	pub, _ := newKey(t)
	_, other := newKey(t)
	feed := feedDoc(nil)
	refused(t, func() http.Handler {
		h, _ := handler(t, &served{feed: feed, sig: sign(other, feed)}, []ed25519.PublicKey{pub})
		return h
	}())
	// A signature over other bytes, a malformed one and an empty one.
	_, priv := newKey(t)
	for _, sig := range [][]byte{sign(priv, append([]byte(nil), feed[1:]...)), []byte("not base64\n"), nil} {
		h, _ := handler(t, &served{feed: feed, sig: sig}, []ed25519.PublicKey{pub})
		refused(t, h)
	}
}

func TestExpiredFeedIsRefused(t *testing.T) {
	pub, priv := newKey(t)
	feed := feedDoc(func(d map[string]any) {
		d["publishedAt"] = now.Add(-80 * 24 * time.Hour).Format(time.RFC3339)
		d["expiresAt"] = now.Add(-time.Minute).Format(time.RFC3339)
	})
	h, _ := handler(t, &served{feed: feed, sig: sign(priv, feed)}, []ed25519.PublicKey{pub})
	refused(t, h)
}

func TestCachedFeedIsNotUsedPastItsExpiry(t *testing.T) {
	pub, priv := newKey(t)
	feed := feedDoc(func(d map[string]any) { d["expiresAt"] = now.Add(5 * time.Minute).Format(time.RFC3339) })
	s := &served{feed: feed, sig: sign(priv, feed)}
	h, clock := handler(t, s, []ed25519.PublicKey{pub})
	if rec := get(h, "/download/win-x64"); rec.Code != http.StatusFound {
		t.Fatalf("fresh feed = %d", rec.Code)
	}
	*clock = now.Add(6 * time.Minute)
	refused(t, h)
}

func TestForeignOrUnlistedAssetsAreNeverRedirectedTo(t *testing.T) {
	pub, priv := newKey(t)
	for name, mutate := range map[string]func(map[string]any){
		"asset missing": func(d map[string]any) {
			d["assets"] = slices.DeleteFunc(allAssets(version), func(a map[string]any) bool { return strings.HasSuffix(a["name"].(string), ".deb") })
		},
		"asset listed twice": func(d map[string]any) {
			d["assets"] = append(allAssets(version), map[string]any{"name": "dilla-" + version + "-linux-x64.deb"})
		},
		"foreign asset name": func(d map[string]any) {
			d["assets"] = []map[string]any{{"name": "https://evil.example/dilla-" + version + "-linux-x64.deb"}, {"name": "../dilla-" + version + "-linux-x64.deb"}}
		},
		"version with a path": func(d map[string]any) {
			v := "0.1.0-beta.2/../../evil"
			d["version"], d["assets"] = v, allAssets(v)
		},
		"stable version on the beta feed": func(d map[string]any) { d["version"], d["assets"] = "0.1.0", allAssets("0.1.0") },
		"stable channel":                  func(d map[string]any) { d["channel"] = "stable" },
		"format 2":                        func(d map[string]any) { d["format"] = 2 },
		"published in the future":         func(d map[string]any) { d["publishedAt"] = now.Add(time.Hour).Format(time.RFC3339) },
		"lifetime past 93 days":           func(d map[string]any) { d["expiresAt"] = now.Add(93 * 24 * time.Hour).Format(time.RFC3339) },
		"non-UTC date":                    func(d map[string]any) { d["expiresAt"] = now.Add(time.Hour).Format("2006-01-02T15:04:05+01:00") },
	} {
		t.Run(name, func(t *testing.T) {
			feed := feedDoc(mutate)
			h, _ := handler(t, &served{feed: feed, sig: sign(priv, feed)}, []ed25519.PublicKey{pub})
			refused(t, h)
		})
	}
}

func TestUnreachableFeedIsRefusedAndBrieflyNegativelyCached(t *testing.T) {
	pub, priv := newKey(t)
	feed := feedDoc(nil)
	s := &served{feed: feed, sig: sign(priv, feed), status: http.StatusBadGateway}
	h, clock := handler(t, s, []ed25519.PublicKey{pub})
	refused(t, h)
	refused(t, h)
	if hits := s.hits.Load(); hits != 1 {
		t.Fatalf("failed feed fetched %d times inside the negative cache", hits)
	}
	s.status = 0
	*clock = now.Add(desktopdownload.NegativeTTL + time.Second)
	if rec := get(h, "/download/mac-arm64"); rec.Code != http.StatusFound {
		t.Fatalf("recovered feed = %d", rec.Code)
	}
}

func TestPlainHTTPFeedIsRefused(t *testing.T) {
	pub, priv := newKey(t)
	feed := feedDoc(nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sig") {
			_, _ = w.Write(sign(priv, feed))
			return
		}
		_, _ = w.Write(feed)
	}))
	defer srv.Close()
	h := desktopdownload.New(desktopdownload.Options{FeedURL: srv.URL + "/f.json", Keys: []ed25519.PublicKey{pub}, Now: func() time.Time { return now }})
	mux := http.NewServeMux()
	mux.Handle("GET /download/{platform}", h)
	refused(t, mux)
}

// The pins must be the keys the desktop app verifies its own updates with.
func TestPinnedKeysMatchTheDesktopApp(t *testing.T) {
	var trust struct {
		MetadataPublicKeys []string `json:"metadataPublicKeys"`
	}
	raw, err := os.ReadFile("../../packages/desktop/release-trust.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &trust); err != nil {
		t.Fatal(err)
	}
	ts, err := os.ReadFile("../../packages/desktop/src/update-keys.ts")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)RELEASE_PUBLIC_KEYS: readonly string\[\] = Object\.freeze\((\[.*?\])\);`).FindSubmatch(ts)
	if m == nil {
		t.Fatal("update-keys.ts no longer declares RELEASE_PUBLIC_KEYS")
	}
	var app []string
	if err := json.Unmarshal(m[1], &app); err != nil {
		t.Fatal(err)
	}
	pinned, err := desktopdownload.ParseKeys(trust.MetadataPublicKeys)
	if err != nil || len(pinned) == 0 {
		t.Fatalf("release-trust.json keys: %v", err)
	}
	if !slices.Equal(app, trust.MetadataPublicKeys) {
		t.Fatal("update-keys.ts and release-trust.json pin different keys")
	}
	got := desktopdownload.PinnedKeys()
	if len(got) != len(pinned) {
		t.Fatalf("server pins %d keys, the desktop app %d", len(got), len(pinned))
	}
	for i := range got {
		if !got[i].Equal(pinned[i]) {
			t.Fatalf("pinned key %d differs from packages/desktop/release-trust.json", i)
		}
	}
}
