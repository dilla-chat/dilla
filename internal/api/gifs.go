package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// Gifs is the GIF picker's server side (E8, ruling R3 as amended by Jonas on 2026-10-21: the Giphy key is a
// per-server setting, entered by a server manager in that server's settings). Routes, all behind the enrolled
// session:
//
//	GET    /v1/communities/{id}/gifs                    member: [gifs(0|1), rating]; never the key
//	PUT    /v1/communities/{id}/gifs/key        [key]   manage community: set or replace; audited
//	DELETE /v1/communities/{id}/gifs/key                manage community: remove; audited
//	PUT    /v1/communities/{id}/gifs/rating     [rating] manage community; audited
//	GET    /v1/communities/{id}/gifs/search?q=&offset=  member; this server's key; 404 without one
//	GET    /v1/communities/{id}/gifs/media/{gif_id}?r=preview|full   member; 404 without a key
//
// The key lives in instance_settings under "community_gifs:{id}" (no migration), is written and never read
// back out through the API, and is in no reply, header, log line, audit detail or doctor line.
//
// The privacy claims it keeps, each pinned by a test:
//   - Giphy sees dillad, never a person: the key and every upstream request live here.
//   - Readers' browsers never contact giphy.com: the picker downloads the GIF through the media route and
//     sends it as an ordinary attachment.
//   - No user-supplied URL reaches a fetch. The media route takes a Giphy id (letters and digits, checked),
//     builds the URL itself from a host on the allow-list, and refuses a redirect, in the fetch or the dial,
//     that leaves it.
type Gifs struct {
	repo    store.Repository
	limiter *server.RateLimiter
	clk     clock.Clock
	log     *slog.Logger

	client  *http.Client
	apiBase string // the scheme and host of Giphy's API, no trailing slash
	// mediaBase is the scheme and host the media URL is built on; its host is on hosts.
	mediaBase string
	scheme    string   // the one scheme the allow-list admits
	hosts     []string // every host (with its port, if not the default) a fetch or redirect may reach

	timeout      time.Duration
	writeTimeout time.Duration
	slotWait     time.Duration
	maxPreview   int64
	maxFull      int64

	// A fetch holds a slot from before the upstream request until the reply is written (or the client is
	// gone), so the bytes it buffers are bounded by slots x cap. perUser is how many one member may hold.
	slots   chan struct{}
	perUser int
	mu      sync.Mutex
	held    map[id.ID]int
	// perServer is how many slots one server's members may hold together; heldServer counts them.
	perServer  int
	heldServer map[id.ID]int
}

// The production allow-list: Giphy's API host and its media hosts. Nothing else is ever dialled.
var giphyHosts = []string{
	"api.giphy.com",
	"media.giphy.com", "media0.giphy.com", "media1.giphy.com", "media2.giphy.com", "media3.giphy.com", "media4.giphy.com",
}

const (
	giphyAPIBase   = "https://api.giphy.com"
	giphyMediaBase = "https://media.giphy.com"

	gifPageSize      = 24
	gifMaxOffset     = 4999 // Giphy refuses an offset past this
	gifMaxQueryBytes = 100
	gifMaxIDBytes    = 64
	gifMaxRedirects  = 3
	gifMaxJSONBytes  = 1 << 20

	// gifMaxFull is the size cap of the full GIF: the 25 MB the plan names, well under
	// limits.max_attachment_bytes, so the attachment path never refuses what this route served.
	gifMaxFull    = 25 << 20
	gifMaxPreview = 5 << 20

	// gifSlots bounds the fetches in flight on the instance, each held until its reply is written:
	// 8 slots of at most 25 MiB. gifPerUser bounds one member's share so a single member cannot starve the rest.
	gifSlots   = 8
	gifPerUser = 3
	// gifPerServer keeps one server from taking the whole instance's capacity.
	gifPerServer = 4
	gifTimeout   = 15 * time.Second
	gifWriteWait = 30 * time.Second
	// gifSlotWait is how long a request waits for a free slot before it is told to retry.
	gifSlotWait = 2 * time.Second

	gifDefaultRating = "pg-13"
	gifSettingPrefix = "community_gifs:"
	gifKeyMinBytes   = 8
	gifKeyMaxBytes   = 64
)

// The two per-user buckets. A search is one upstream call against the server's Giphy quota; a preview grid
// is up to 24 media fetches, so the media bucket is far wider.
var (
	gifSearchClass = server.Class{Name: "gif_search", PerSecond: 1, Burst: 10}
	gifMediaClass  = server.Class{Name: "gif_media", PerSecond: 20, Burst: 60}
)

// NewGifs builds the routes. limiter is the instance's rate limiter (nil meters nothing, for the handler
// tests that do not care).
func NewGifs(repo store.Repository, limiter *server.RateLimiter, clk clock.Clock, log *slog.Logger) *Gifs {
	g := &Gifs{
		repo: repo, limiter: limiter, clk: clk, log: log,
		apiBase: giphyAPIBase, mediaBase: giphyMediaBase, scheme: "https", hosts: giphyHosts,
		timeout: gifTimeout, writeTimeout: gifWriteWait, slotWait: gifSlotWait,
		maxPreview: gifMaxPreview, maxFull: gifMaxFull,
		slots: make(chan struct{}, gifSlots), perUser: gifPerUser, held: map[id.ID]int{},
		perServer: gifPerServer, heldServer: map[id.ID]int{},
	}
	g.client = &http.Client{
		Transport: &http.Transport{
			// No proxy, no environment: dilla.toml is the whole configuration.
			Proxy: nil,
			// A name on the allow-list that resolves to a non-public address (a poisoned resolver, a
			// rebinding record) is refused at connect time.
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, Control: dialGuard}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			MaxIdleConns:          4,
			IdleConnTimeout:       30 * time.Second,
			ForceAttemptHTTP2:     true,
		},
	}
	g.client.CheckRedirect = g.checkRedirect
	return g
}

// Register mounts the routes.
func (g *Gifs) Register(mux *server.Mux) {
	mux.Socket("GET /v1/communities/{id}/gifs", http.HandlerFunc(g.status))
	mux.Socket("PUT /v1/communities/{id}/gifs/key", http.HandlerFunc(g.putKey))
	mux.Socket("DELETE /v1/communities/{id}/gifs/key", http.HandlerFunc(g.deleteKey))
	mux.Socket("PUT /v1/communities/{id}/gifs/rating", http.HandlerFunc(g.putRating))
	mux.Socket("GET /v1/communities/{id}/gifs/search", http.HandlerFunc(g.search))
	mux.HandleFunc("GET /v1/communities/{id}/gifs/media/{gif_id}", g.media)
}

// ---- the per-server setting ----

// gifSettings is what a server stores: the key (write-only through the API) and the rating.
type gifSettings struct {
	Key    string `json:"key"`
	Rating string `json:"rating"`
}

func (s gifSettings) on() bool { return s.Key != "" }

func gifSettingKey(cid id.ID) string { return gifSettingPrefix + cid.String() }

func validGifRating(r string) bool { return r == "g" || r == "pg" || r == gifDefaultRating || r == "r" }

// validGifKey is the shape of a Giphy API key: letters and digits. It keeps the key from steering the
// upstream URL or a log line and from carrying whitespace pasted with it.
func validGifKey(k string) bool {
	if len(k) < gifKeyMinBytes || len(k) > gifKeyMaxBytes {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func loadGifSettings(ctx context.Context, repo store.Repository, cid id.ID) (gifSettings, error) {
	def := gifSettings{Rating: gifDefaultRating}
	raw, err := repo.GetSetting(ctx, gifSettingKey(cid))
	if errors.Is(err, store.ErrNotFound) {
		return def, nil
	}
	if err != nil {
		return def, err
	}
	s, ok := parseGifSettings(raw)
	if !ok {
		return def, nil // a malformed value is as good as none; the next save replaces it
	}
	return s, nil
}

func parseGifSettings(raw []byte) (gifSettings, bool) {
	var s gifSettings
	if err := json.Unmarshal(raw, &s); err != nil {
		return gifSettings{}, false
	}
	if !validGifRating(s.Rating) {
		s.Rating = gifDefaultRating
	}
	return s, true
}

func (g *Gifs) saveSettings(ctx context.Context, tx store.Repository, cid id.ID, s gifSettings) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return tx.PutSetting(ctx, gifSettingKey(cid), raw, g.clk.Now().Unix())
}

// member is the session and community of a member-only route; a non-member learns nothing (404).
func (g *Gifs) member(r *http.Request) (auth.Session, id.ID, error) {
	s, err := enrolledSession(r)
	if err != nil {
		return s, id.ID{}, err
	}
	cid, err := server.PathID(r, "id")
	if err != nil {
		return s, cid, err
	}
	if _, err := g.repo.GetCommunity(r.Context(), cid); err != nil {
		return s, cid, notFound(err)
	}
	if _, err := g.repo.GetMember(r.Context(), cid, s.UserID); err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return s, cid, err
		}
		return s, cid, server.Errorf(server.CodeNotFound, "no such object")
	}
	return s, cid, nil
}

// manager is member plus Manage Server, which the owner always holds.
func (g *Gifs) manager(r *http.Request) (auth.Session, id.ID, error) {
	s, cid, err := g.member(r)
	if err != nil {
		return s, cid, err
	}
	snap, err := LoadSnapshot(r.Context(), g.repo, cid, s.UserID, nil)
	if err != nil {
		return s, cid, notFound(err)
	}
	if !snap.Resolve(s.UserID).Has(PermManageCommunity) {
		return s, cid, server.Errorf(server.CodeForbidden, "manage community")
	}
	return s, cid, nil
}

func (g *Gifs) fail(w http.ResponseWriter, r *http.Request, what string, err error) {
	var se *server.Error
	if !errors.As(err, &se) {
		g.log.ErrorContext(r.Context(), what, "err", g.describe(err, ""))
	}
	server.WriteError(w, err)
}

// status is GET /v1/communities/{id}/gifs: whether this server has a key, and the rating. Never the key.
func (g *Gifs) status(w http.ResponseWriter, r *http.Request) {
	_, cid, err := g.member(r)
	if err != nil {
		g.fail(w, r, "gifs status", err)
		return
	}
	s, err := loadGifSettings(r.Context(), g.repo, cid)
	if err != nil {
		g.fail(w, r, "gifs status", err)
		return
	}
	on := uint64(0)
	if s.on() {
		on = 1
	}
	if err := server.EncodeBody(w, http.StatusOK, []any{on, s.Rating}); err != nil {
		g.log.Warn("gifs: encode status", "err", err)
	}
}

func decodeOne(w http.ResponseWriter, r *http.Request, v any) error {
	var body []cbor.RawMessage
	if err := server.DecodeBody(w, r, maxCBORBody, &body); err != nil {
		return err
	}
	if len(body) != 1 {
		return server.Errorf(server.CodeInvalidRequest, "the body is a one-element array")
	}
	if err := cborx.Unmarshal(body[0], v); err != nil {
		return server.Errorf(server.CodeInvalidRequest, "the element has the wrong type")
	}
	return nil
}

// change runs a manager's edit of the settings under the community lock and audits it. The audit detail
// never carries the key.
func (g *Gifs) change(w http.ResponseWriter, r *http.Request, action string, edit func(*gifSettings)) {
	s, cid, err := g.manager(r)
	if err != nil {
		g.fail(w, r, action, err)
		return
	}
	if err := g.repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := tx.LockCommunity(r.Context(), cid); err != nil {
			return notFound(err)
		}
		cur, err := loadGifSettings(r.Context(), tx, cid)
		if err != nil {
			return err
		}
		edit(&cur)
		if err := g.saveSettings(r.Context(), tx, cid, cur); err != nil {
			return err
		}
		return tx.Audit(r.Context(), store.AuditRow{
			Actor: &s.UserID, Action: action, Target: cid.String(), Detail: "", At: g.clk.Now().Unix(),
		})
	}); err != nil {
		g.fail(w, r, action, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (g *Gifs) putKey(w http.ResponseWriter, r *http.Request) {
	if _, _, err := g.manager(r); err != nil {
		g.fail(w, r, "gifs key", err)
		return
	}
	var key string
	if err := decodeOne(w, r, &key); err != nil {
		server.WriteError(w, err)
		return
	}
	if !validGifKey(key) {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "a Giphy key is %d to %d letters and digits", gifKeyMinBytes, gifKeyMaxBytes))
		return
	}
	g.change(w, r, "community.gifs.key_set", func(s *gifSettings) { s.Key = key })
}

func (g *Gifs) deleteKey(w http.ResponseWriter, r *http.Request) {
	g.change(w, r, "community.gifs.key_remove", func(s *gifSettings) { s.Key = "" })
}

func (g *Gifs) putRating(w http.ResponseWriter, r *http.Request) {
	if _, _, err := g.manager(r); err != nil {
		g.fail(w, r, "gifs rating", err)
		return
	}
	var rating string
	if err := decodeOne(w, r, &rating); err != nil {
		server.WriteError(w, err)
		return
	}
	if !validGifRating(rating) {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "rating is g, pg, pg-13 or r"))
		return
	}
	g.change(w, r, "community.gifs.rating", func(s *gifSettings) { s.Rating = rating })
}

// ---- the allow-list and the dial guard ----

// allowed is the allow-list: the one scheme, a host on the list, no userinfo. The host is compared whole
// (with its port), so "media.giphy.com:8443" and "media.giphy.com.evil.test" are both out.
func (g *Gifs) allowed(u *url.URL) bool {
	if u == nil || u.Scheme != g.scheme || u.User != nil {
		return false
	}
	host := strings.ToLower(u.Host)
	for _, h := range g.hosts {
		if host == h {
			return true
		}
	}
	return false
}

var errRedirectOff = errors.New("redirect to a host that is not on the allow-list")
var errTooManyRedirects = errors.New("too many redirects")

func (g *Gifs) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > gifMaxRedirects {
		return errTooManyRedirects
	}
	if !g.allowed(req.URL) {
		return errRedirectOff
	}
	return nil
}

// dialGuard refuses a connection to an address that is not a public unicast one.
func dialGuard(_, address string, _ syscall.RawConn) error { return guardAddress(address) }

var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved
	netip.MustParsePrefix("64:ff9b::/96"),    // NAT64: embeds an IPv4 address this check cannot see
	netip.MustParsePrefix("64:ff9b:1::/48"),  // local-use NAT64
	netip.MustParsePrefix("100::/64"),        // discard
	netip.MustParsePrefix("2001::/32"),       // Teredo
	netip.MustParsePrefix("2002::/16"),       // 6to4: embeds an IPv4 address
	netip.MustParsePrefix("fec0::/10"),       // site-local
}

func guardAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("gifs: dial target is not host:port")
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return errors.New("gifs: dial target is not an IP address")
	}
	ip = ip.Unmap()
	refused := errors.New("gifs: refusing to connect to a non-public address")
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
		return refused
	}
	for _, p := range nonPublicPrefixes {
		if p.Contains(ip) {
			return refused
		}
	}
	return nil
}

// ---- fetching ----

// upstream failures, mapped to one reply each by fail503.
var (
	errGifNotFound    = errors.New("giphy has no such gif")
	errGifUnavailable = errors.New("giphy is unavailable")
	errGifBadBody     = errors.New("giphy sent something that is not a gif")
	errGifBusy        = errors.New("too many gif fetches in flight")
)

// describe is an error's text with the request URL (which carries api_key) left out: net/http wraps every
// transport error in a *url.Error whose text begins with the full URL. The key, if it somehow remains, is
// replaced.
func (g *Gifs) describe(err error, key string) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	msg := err.Error()
	if key != "" {
		msg = strings.ReplaceAll(msg, key, "[key]")
	}
	return msg
}

// acquire takes one of the instance's slots and one of the member's shares, waiting briefly for the slot.
// The returned release must run once the reply is written or the client is gone.
func (g *Gifs) acquire(ctx context.Context, user, server id.ID) (func(), error) {
	g.mu.Lock()
	if g.held[user] >= g.perUser || g.heldServer[server] >= g.perServer {
		g.mu.Unlock()
		return nil, errGifBusy
	}
	g.held[user]++
	g.heldServer[server]++
	g.mu.Unlock()
	giveBack := func() {
		g.mu.Lock()
		if g.held[user]--; g.held[user] <= 0 {
			delete(g.held, user)
		}
		if g.heldServer[server]--; g.heldServer[server] <= 0 {
			delete(g.heldServer, server)
		}
		g.mu.Unlock()
	}
	select {
	case g.slots <- struct{}{}:
	default:
		t := time.NewTimer(g.slotWait)
		defer t.Stop()
		select {
		case g.slots <- struct{}{}:
		case <-t.C:
			giveBack()
			return nil, errGifBusy
		case <-ctx.Done():
			giveBack()
			return nil, errGifBusy
		}
	}
	var once sync.Once
	return func() { once.Do(func() { <-g.slots; giveBack() }) }, nil
}

// get fetches rawURL, which the caller built from allow-listed parts, and returns at most limit bytes of its
// body. The caller holds a slot.
func (g *Gifs) get(ctx context.Context, rawURL, key string, limit int64, contentType func(string) bool) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	u, err := url.Parse(rawURL)
	if err != nil || !g.allowed(u) {
		// A bug, not a request: every URL here is built from the allow-list.
		g.log.Error("gifs: refusing a fetch off the allow-list")
		return nil, errGifUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, errGifUnavailable
	}
	req.Header.Set("User-Agent", "dilla-gifs")
	resp, err := g.client.Do(req)
	if err != nil {
		g.log.Warn("gifs: upstream request failed", "err", g.describe(err, key))
		return nil, errGifUnavailable
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, errGifNotFound
	default:
		g.log.Warn("gifs: upstream refused", "status", resp.StatusCode)
		return nil, errGifUnavailable
	}
	if resp.ContentLength > limit {
		g.log.Warn("gifs: upstream body over the cap", "declared", resp.ContentLength, "cap", limit)
		return nil, errGifBadBody
	}
	if contentType != nil && !contentType(resp.Header.Get("Content-Type")) {
		g.log.Warn("gifs: upstream content type refused", "type", resp.Header.Get("Content-Type"))
		return nil, errGifBadBody
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		g.log.Warn("gifs: upstream body failed", "err", g.describe(err, key))
		return nil, errGifUnavailable
	}
	if int64(len(body)) > limit {
		g.log.Warn("gifs: upstream body over the cap", "cap", limit)
		return nil, errGifBadBody
	}
	return body, nil
}

func gifIsGIF(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && mt == "image/gif"
}

// fail503 writes the refusal for an upstream or capacity failure; every 503 carries Retry-After.
func (g *Gifs) fail503(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errGifNotFound):
		server.WriteError(w, server.Errorf(server.CodeNotFound, "no such gif"))
	case errors.Is(err, errGifBusy):
		w.Header().Set("Retry-After", "1")
		server.WriteError(w, server.Unavailable(1000, "gif fetches are busy; try again"))
	case errors.Is(err, errGifBadBody):
		w.Header().Set("Retry-After", "5")
		server.WriteError(w, server.Unavailable(5000, "the gif service sent something unusable"))
	default:
		w.Header().Set("Retry-After", "5")
		server.WriteError(w, server.Unavailable(5000, "the gif service is unavailable"))
	}
}

// gate is what search and media start with: a member of the community, the community's key (404 without
// one), then the caller's bucket.
func (g *Gifs) gate(r *http.Request, class server.Class) (auth.Session, id.ID, gifSettings, error) {
	s, cid, err := g.member(r)
	if err != nil {
		return s, cid, gifSettings{}, err
	}
	cfg, err := loadGifSettings(r.Context(), g.repo, cid)
	if err != nil {
		return s, cid, cfg, err
	}
	if !cfg.on() {
		return s, cid, cfg, server.Errorf(server.CodeNotFound, "no such object")
	}
	if g.limiter != nil {
		// One bucket per member, not per server and member: the rate must not grow with the servers someone is in.
		if ok, wait := g.limiter.Allow(class, s.UserID.String()); !ok {
			return s, cid, cfg, server.RateLimitedAfter(wait)
		}
	}
	return s, cid, cfg, nil
}

func cleanTitle(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	if utf8.RuneCountInString(s) > 120 {
		s = string([]rune(s)[:120])
	}
	return strings.TrimSpace(s)
}

// validGifID is a Giphy id: letters and digits. Anything else (a dot, a slash, a colon, an @) could steer the
// URL built from it, so it is refused before a URL exists.
func validGifID(id string) bool {
	if id == "" || len(id) > gifMaxIDBytes {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

type giphyList struct {
	Data []struct {
		ID     string `json:"id"`
		Title  string `json:"title"`
		Images struct {
			FixedWidth struct {
				Width  json.RawMessage `json:"width"`
				Height json.RawMessage `json:"height"`
			} `json:"fixed_width"`
		} `json:"images"`
	} `json:"data"`
	Pagination struct {
		TotalCount int `json:"total_count"`
		Count      int `json:"count"`
		Offset     int `json:"offset"`
	} `json:"pagination"`
}

// gifMaxDim is the largest preview side the instance passes on, in pixels.
const gifMaxDim = 4000

// dim reads a Giphy dimension, a number or a numeric string: a whole number of 1 to gifMaxDim, else 0 (unknown).
func dim(raw json.RawMessage) uint64 {
	n, err := strconv.Atoi(strings.Trim(string(raw), `" `))
	if err != nil || n < 1 || n > gifMaxDim {
		return 0
	}
	return uint64(n)
}

// dims reads a preview's width and height together: both are small positive integers or both are 0, so a client sizing
// a tile from them never sees half a ratio.
func dims(w, h json.RawMessage) (uint64, uint64) {
	cw, ch := dim(w), dim(h)
	if cw == 0 || ch == 0 {
		return 0, 0
	}
	return cw, ch
}

func (g *Gifs) search(w http.ResponseWriter, r *http.Request) {
	s, cid, cfg, err := g.gate(r, gifSearchClass)
	if err != nil {
		g.fail(w, r, "gifs search", err)
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) > gifMaxQueryBytes || !utf8.ValidString(q) || strings.IndexFunc(q, unicode.IsControl) >= 0 {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "q is at most %d bytes of text", gifMaxQueryBytes))
		return
	}
	offset := uint64(0)
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil || n > gifMaxOffset {
			server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "offset is 0..%d", gifMaxOffset))
			return
		}
		offset = n
	}
	params := url.Values{}
	params.Set("api_key", cfg.Key)
	params.Set("limit", strconv.Itoa(gifPageSize))
	params.Set("offset", strconv.FormatUint(offset, 10))
	params.Set("rating", cfg.Rating)
	params.Set("bundle", "messaging_non_clips")
	path := "/v1/gifs/trending"
	if q != "" {
		path = "/v1/gifs/search"
		params.Set("q", q)
		params.Set("lang", "en")
	}
	release, err := g.acquire(r.Context(), s.UserID, cid)
	if err != nil {
		g.fail503(w, err)
		return
	}
	defer release()
	body, err := g.get(r.Context(), g.apiBase+path+"?"+params.Encode(), cfg.Key, gifMaxJSONBytes, nil)
	if err != nil {
		g.fail503(w, err)
		return
	}
	var list giphyList
	if err := json.Unmarshal(body, &list); err != nil {
		g.log.Warn("gifs: upstream search body is not json", "err", g.describe(err, cfg.Key))
		g.fail503(w, errGifBadBody)
		return
	}
	items := make([]any, 0, len(list.Data))
	for _, d := range list.Data {
		if !validGifID(d.ID) {
			continue // never echo an id a client would put in a path
		}
		cw, ch := dims(d.Images.FixedWidth.Width, d.Images.FixedWidth.Height)
		items = append(items, []any{
			d.ID, cleanTitle(d.Title), cw, ch,
			"/v1/communities/" + cid.String() + "/gifs/media/" + d.ID + "?r=preview",
		})
	}
	var next *uint64
	if end := offset + uint64(max(len(list.Data), 0)); len(list.Data) > 0 && end < uint64(max(list.Pagination.TotalCount, 0)) && end <= gifMaxOffset {
		next = &end
	}
	g.setWriteDeadline(w)
	if err := server.EncodeBody(w, http.StatusOK, []any{items, next}); err != nil {
		g.log.Debug("gifs: encode search", "err", err)
	}
}

// setWriteDeadline bounds how long a reply may take to reach a slow client. The server sets no WriteTimeout of
// its own (a long upload would die), so this route sets one for itself.
func (g *Gifs) setWriteDeadline(w http.ResponseWriter) {
	if g.writeTimeout > 0 {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(g.writeTimeout))
	}
}

func (g *Gifs) media(w http.ResponseWriter, r *http.Request) {
	s, cid, cfg, err := g.gate(r, gifMediaClass)
	if err != nil {
		g.fail(w, r, "gifs media", err)
		return
	}
	gifID := r.PathValue("gif_id")
	variants := r.URL.Query()["r"]
	if !validGifID(gifID) || len(variants) != 1 {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "a gif id and r=preview|full are required"))
		return
	}
	var file string
	var limit int64
	switch variants[0] {
	case "preview":
		file, limit = "200w.gif", g.maxPreview
	case "full":
		file, limit = "giphy.gif", g.maxFull
	default:
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "r is preview or full"))
		return
	}
	// The slot is held until the reply is written: the buffer below is alive that long.
	release, err := g.acquire(r.Context(), s.UserID, cid)
	if err != nil {
		g.fail503(w, err)
		return
	}
	defer release()
	// The URL is built here, from the allow-listed media host and the checked id. Nothing the caller sent,
	// but the id and the variant, is in it.
	body, err := g.get(r.Context(), g.mediaBase+"/media/"+gifID+"/"+file, cfg.Key, limit, gifIsGIF)
	if err != nil {
		g.fail503(w, err)
		return
	}
	if !bytes.HasPrefix(body, []byte("GIF87a")) && !bytes.HasPrefix(body, []byte("GIF89a")) {
		g.log.Warn("gifs: upstream bytes are not a gif")
		g.fail503(w, errGifBadBody)
		return
	}
	g.setWriteDeadline(w)
	h := w.Header()
	h.Set("Content-Type", "image/gif")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'")
	h.Set("Cache-Control", "private, max-age=86400")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		g.log.Debug("gifs: write media", "err", g.describe(err, cfg.Key))
	}
}
