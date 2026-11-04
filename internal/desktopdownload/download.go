// Package desktopdownload answers /download/<platform> on a desktop-only instance
// (`dillad serve --desktop-downloads`): it reads the desktop beta channel's signed
// update feed from GitHub Pages, verifies it under the metadata keys the desktop app
// pins (packages/desktop/release-trust.json), and redirects to that release's asset
// for the platform. The redirect URL is built from the verified version and the
// expected asset name, never taken from the feed's text; anything that does not
// verify gets a 503 page that links to the releases page instead.
package desktopdownload

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// FeedURL is the beta channel's signed metadata, as packages/desktop/src/updater.ts reads it.
	FeedURL = "https://dilla-chat.github.io/dilla/beta/desktop-beta.json"
	// ReleasesPage is where a person goes when no verified download can be offered.
	ReleasesPage = "https://github.com/dilla-chat/dilla/releases"
	// downloadBase is the only host and path a redirect may point at.
	downloadBase = "https://github.com/dilla-chat/dilla/releases/download/desktop-v"

	maxFeedBytes      = 1 << 20
	maxSignatureBytes = 128
	// CacheTTL keeps a verified feed; NegativeTTL keeps a failure, so an outage is not hammered.
	CacheTTL    = 10 * time.Minute
	NegativeTTL = 30 * time.Second
)

// Platforms maps each /download/<platform> to the asset suffix the release workflow publishes
// (packages/desktop/scripts/release-feed.mjs): dilla-<version>-<suffix>. There is no macOS Intel build.
var Platforms = map[string]string{
	"linux-appimage": "linux-x64.AppImage",
	"linux-deb":      "linux-x64.deb",
	"linux-rpm":      "linux-x64.rpm",
	"linux-aur":      "linux-x64.aur.tar.gz",
	"mac-arm64":      "mac-arm64.dmg",
	"win-x64":        "win-x64.exe",
}

var (
	versionPattern   = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)-beta\.(0|[1-9][0-9]*)$`)
	signaturePattern = regexp.MustCompile(`^[A-Za-z0-9+/]{86}==$`)
	timePattern      = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?Z$`)
)

// Feed is the part of a verified release document this package uses.
type Feed struct {
	Version   string
	ExpiresAt time.Time
	assets    map[string]int
}

type document struct {
	Format      int    `json:"format"`
	Version     string `json:"version"`
	Channel     string `json:"channel"`
	PublishedAt string `json:"publishedAt"`
	ExpiresAt   string `json:"expiresAt"`
	Assets      []struct {
		Name string `json:"name"`
	} `json:"assets"`
}

// Verify checks the detached Ed25519 signature (base64, one optional trailing newline) over the
// exact feed bytes under any of keys, then the document: format 1, the beta channel, a beta
// version, a publication not in the future and an expiry after now, at most 93 days after it.
func Verify(feed, signature []byte, keys []ed25519.PublicKey, now time.Time) (Feed, error) {
	if len(keys) == 0 {
		return Feed{}, errors.New("no pinned desktop metadata key")
	}
	if len(feed) == 0 || len(feed) > maxFeedBytes {
		return Feed{}, errors.New("feed size out of bounds")
	}
	sig := strings.TrimSuffix(string(signature), "\n")
	if !signaturePattern.MatchString(sig) {
		return Feed{}, errors.New("malformed feed signature")
	}
	raw, err := base64.StdEncoding.DecodeString(sig)
	if err != nil || len(raw) != ed25519.SignatureSize {
		return Feed{}, errors.New("malformed feed signature")
	}
	valid := false
	for _, key := range keys {
		if len(key) == ed25519.PublicKeySize && ed25519.Verify(key, feed, raw) {
			valid = true
			break
		}
	}
	if !valid {
		return Feed{}, errors.New("feed signature does not verify under the pinned keys")
	}
	var doc document
	if err := json.Unmarshal(feed, &doc); err != nil {
		return Feed{}, fmt.Errorf("feed document: %w", err)
	}
	if doc.Format != 1 || doc.Channel != "beta" || !versionPattern.MatchString(doc.Version) {
		return Feed{}, errors.New("feed format, channel or version is not a beta release")
	}
	published, err := parseUTC(doc.PublishedAt)
	if err != nil {
		return Feed{}, err
	}
	expires, err := parseUTC(doc.ExpiresAt)
	if err != nil {
		return Feed{}, err
	}
	if published.After(now.Add(5*time.Minute)) || !expires.After(now) || !expires.After(published) || expires.Sub(published) > 93*24*time.Hour {
		return Feed{}, errors.New("feed is expired or its dates are invalid")
	}
	f := Feed{Version: doc.Version, ExpiresAt: expires, assets: map[string]int{}}
	for _, a := range doc.Assets {
		f.assets[a.Name]++
	}
	return f, nil
}

func parseUTC(value string) (time.Time, error) {
	if !timePattern.MatchString(value) {
		return time.Time{}, errors.New("feed date is not a UTC timestamp")
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("feed date: %w", err)
	}
	return t, nil
}

// AssetURL is the release download for platform: dilla-<version>-<suffix> under
// desktop-v<version>, when the verified feed lists exactly that asset once.
func (f Feed) AssetURL(platform string) (string, error) {
	suffix, ok := Platforms[platform]
	if !ok {
		return "", errors.New("unknown platform")
	}
	if !versionPattern.MatchString(f.Version) {
		return "", errors.New("unverified version")
	}
	name := "dilla-" + f.Version + "-" + suffix
	if f.assets[name] != 1 {
		return "", fmt.Errorf("the feed does not list %s exactly once", name)
	}
	return downloadBase + f.Version + "/" + name, nil
}

// Options configure a Handler; the zero value reads FeedURL under the pinned keys.
type Options struct {
	FeedURL string
	Client  *http.Client
	Keys    []ed25519.PublicKey
	Now     func() time.Time
}

// Handler serves GET /download/{platform}.
type Handler struct {
	feedURL string
	client  *http.Client
	keys    []ed25519.PublicKey
	now     func() time.Time

	mu        sync.Mutex
	feed      *Feed
	feedUntil time.Time
	failUntil time.Time
}

// New returns a Handler over the channel feed.
func New(o Options) *Handler {
	h := &Handler{feedURL: o.FeedURL, client: o.Client, keys: o.Keys, now: o.Now}
	if h.feedURL == "" {
		h.feedURL = FeedURL
	}
	if h.client == nil {
		h.client = &http.Client{Timeout: 10 * time.Second}
	}
	// No redirect is followed: GitHub Pages serves the feed itself.
	client := *h.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	h.client = &client
	if h.keys == nil {
		h.keys = PinnedKeys()
	}
	if h.now == nil {
		h.now = time.Now
	}
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	platform := r.PathValue("platform")
	if _, ok := Platforms[platform]; !ok {
		http.NotFound(w, r)
		return
	}
	feed, err := h.current(r.Context())
	if err == nil {
		var location string
		if location, err = feed.AssetURL(platform); err == nil {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Location", location)
			w.WriteHeader(http.StatusFound)
			return
		}
	}
	unavailable(w)
}

// current returns the cached verified feed, or fetches and verifies it again. A failure is kept
// for NegativeTTL; a cached feed is used only while it has not expired.
func (h *Handler) current(ctx context.Context) (Feed, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	if h.feed != nil && now.Before(h.feedUntil) && now.Before(h.feed.ExpiresAt) {
		return *h.feed, nil
	}
	if now.Before(h.failUntil) {
		return Feed{}, errors.New("desktop feed recently unavailable")
	}
	feed, err := h.fetch(ctx, now)
	if err != nil {
		h.feed = nil
		h.failUntil = now.Add(NegativeTTL)
		return Feed{}, err
	}
	h.feed, h.feedUntil = &feed, now.Add(CacheTTL)
	return feed, nil
}

func (h *Handler) fetch(ctx context.Context, now time.Time) (Feed, error) {
	// Detached from the request: one person closing the tab does not fail the shared fetch.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	feed, err := h.get(ctx, h.feedURL, maxFeedBytes)
	if err != nil {
		return Feed{}, err
	}
	sig, err := h.get(ctx, h.feedURL+".sig", maxSignatureBytes)
	if err != nil {
		return Feed{}, err
	}
	return Verify(feed, sig, h.keys, now)
}

func (h *Handler) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	if !strings.HasPrefix(url, "https://") {
		return nil, errors.New("the desktop feed must be fetched over HTTPS")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("desktop feed answered %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errors.New("desktop feed response too large")
	}
	return body, nil
}

var unavailablePage = []byte(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Download unavailable</title>
<link rel="stylesheet" href="/desktop-download.css">
</head>
<body>
<main>
<h1>Download unavailable</h1>
<p>This server cannot confirm the current desktop release right now. Try again in a minute, or download Dilla from the <a href="` + html.EscapeString(ReleasesPage) + `" rel="noreferrer">releases page</a>.</p>
<p><a href="/download">Back to downloads</a></p>
</main>
</body>
</html>
`)

func unavailable(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", "60")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.Copy(w, bytes.NewReader(unavailablePage))
}

// NotFound answers /download/{platform} on an instance without --desktop-downloads,
// so the browser client's navigation fallback never serves it.
func NotFound() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
}
