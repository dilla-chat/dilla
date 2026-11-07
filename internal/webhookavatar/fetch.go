// Package webhookavatar fetches, vets, re-encodes and caches the avatar a webhook sender names in avatar_url, so
// members are shown the image from the instance and no viewer's address ever reaches the sender's server (Bot v2
// task 13, fix round 1: Jonas's decision of 2026-11-07).
//
// It makes outbound requests on attacker input, so every step is closed by default:
//   - the URL is https, port 443 only, at most 2048 bytes, with no credentials (CheckURL);
//   - the host is resolved here, and every answer must be a public unicast address that is not the instance's own
//     (Blocked); the vetted address is dialled directly, so a DNS rebind between the check and the dial changes
//     nothing; a literal IP in the URL is vetted the same way;
//   - at most 2 redirects, each re-checked as a new URL and re-vetted at its dial;
//   - no proxy from the environment, TLS verified, no cookies, no credentials, no instance identifier, a fixed
//     User-Agent; 3 s in all;
//   - at most 512 KiB of body, read through a limited reader;
//   - the type is sniffed from the bytes (the header is ignored): PNG, JPEG or GIF; the dimensions are at most
//     512×512 before anything is decoded; the image is decoded and re-encoded as PNG, so the instance never serves
//     the sender's bytes. WebP is refused: the standard library has no decoder for it, and serving it unchanged is
//     what re-encoding exists to prevent.
package webhookavatar

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // registers the GIF decoder; only the first frame is decoded
	_ "image/jpeg"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"time"
)

const (
	// MaxURLBytes bounds avatar_url.
	MaxURLBytes = 2048
	// MaxBodyBytes bounds the response body read.
	MaxBodyBytes = 512 << 10
	// MaxSide bounds each dimension of the image.
	MaxSide = 512
	// MaxProcessedBytes bounds the re-encoded PNG.
	MaxProcessedBytes = 1 << 20
	// FetchTimeout bounds one fetch, redirects and body included.
	FetchTimeout = 3 * time.Second
	// MaxRedirects is the most redirects one fetch follows.
	MaxRedirects = 2
	// UserAgent is the only identifying header a fetch sends.
	UserAgent = "dilla-webhook-avatar"
)

var (
	// ErrURL: not an https URL on port 443 with a host and no credentials, or too long.
	ErrURL = errors.New("webhookavatar: the avatar URL is not an allowed https URL")
	// ErrAddress: the host resolved to no address, or to one that may not be fetched.
	ErrAddress = errors.New("webhookavatar: the avatar host resolves to an address that may not be fetched")
	// ErrResponse: the server did not answer 200, or redirected too often.
	ErrResponse = errors.New("webhookavatar: the avatar server did not answer with an image")
	// ErrTooLarge: the body or the re-encoded image is over its bound.
	ErrTooLarge = errors.New("webhookavatar: the avatar is too large")
	// ErrNotImage: the bytes are not a PNG, JPEG or GIF that decodes.
	ErrNotImage = errors.New("webhookavatar: the avatar is not a PNG, JPEG or GIF image")
	// ErrDimensions: a side is 0 or over MaxSide.
	ErrDimensions = errors.New("webhookavatar: the avatar is larger than 512x512")
)

// CheckURL parses raw and refuses anything but an absolute https URL on port 443 with a host and no credentials, at
// most MaxURLBytes long, with no control or space characters.
func CheckURL(raw string) (*url.URL, error) {
	if raw == "" || len(raw) > MaxURLBytes {
		return nil, ErrURL
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] <= ' ' || raw[i] == 0x7f {
			return nil, ErrURL
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.Hostname() == "" {
		return nil, ErrURL
	}
	if p := u.Port(); p != "" && p != "443" {
		return nil, ErrURL
	}
	return u, nil
}

// blocked4 are the IPv4 special-purpose ranges netip's predicates do not name: this network, CGNAT, IETF protocol
// assignments, documentation, benchmarking, and reserved with broadcast.
var blocked4 = prefixes("0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24",
	"203.0.113.0/24", "240.0.0.0/4")

// blocked6 are the special-purpose ranges inside 2000::/3: the IETF protocol assignments (2001::/23, which holds
// Teredo 2001::/32, benchmarking and ORCHID) and both documentation ranges. Teredo is refused outright rather than
// decoded: its embedded client address is obfuscated and its server address is a relay's, so neither names the host
// that would answer.
var blocked6 = prefixes("2001::/23", "2001:db8::/32", "3fff::/20")

var (
	// global6 is the IPv6 global unicast space, the only IPv6 an avatar may come from (fix round 2 S2/N-1):
	// anything outside it (::/96, ::ffff:0:0/96 SIIT, 64:ff9b:1::/48 local-use NAT64, 100::/64, fc00::/7, fe80::/10,
	// fec0::/10, ff00::/8, and every future special range) is refused by default.
	global6 = netip.MustParsePrefix("2000::/3")
	// The two translation forms whose IPv4 position is fixed are decoded and the embedded IPv4 vetted instead.
	sixToFour = netip.MustParsePrefix("2002::/16")    // RFC 3056: bytes 2-5
	nat64     = netip.MustParsePrefix("64:ff9b::/96") // RFC 6052 well-known prefix: bytes 12-15
)

func prefixes(list ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(list))
	for _, s := range list {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}

// Blocked reports whether a may not be fetched from. IPv4 (an IPv4-mapped address is read as its IPv4) must be
// public unicast: not loopback, RFC 1918, link-local, multicast, unspecified, nor in blocked4. IPv6 must be in
// 2000::/3 and not in blocked6, except that 6to4 and the NAT64 well-known /96 are decoded and their embedded IPv4
// vetted as IPv4. Neither may be one of self.
func Blocked(a netip.Addr, self []netip.Addr) bool {
	a = a.WithZone("")
	if a.Is4In6() {
		a = a.Unmap()
	}
	if !a.IsValid() {
		return true
	}
	for _, s := range self {
		if s.WithZone("").Unmap() == a {
			return true
		}
	}
	if a.Is4() {
		if a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsMulticast() || a.IsUnspecified() {
			return true
		}
		for _, p := range blocked4 {
			if p.Contains(a) {
				return true
			}
		}
		return false
	}
	b := a.As16()
	switch {
	case sixToFour.Contains(a):
		return Blocked(netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}), self)
	case nat64.Contains(a):
		return Blocked(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), self)
	case !global6.Contains(a):
		return true
	}
	for _, p := range blocked6 {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Resolver is the DNS lookup the fetcher vets; *net.Resolver is one.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// FetchOptions configures a Fetcher. Only Resolver, Self and RootCAs matter in production; Dial is a test seam that
// receives the address already vetted.
type FetchOptions struct {
	Resolver Resolver
	// Self is the instance's own addresses (its public IP and its interfaces), refused like private ones.
	Self []netip.Addr
	// SelfDomain is instance.domain: its resolved addresses are refused too (fix round 2 N-4), so an avatar URL that
	// names the operator's own public address cannot hairpin through their reverse proxy from inside the network.
	// It is resolved by RunSelfRefresh in the background (fix round 3 S3), never on the fetch path; until its first
	// lookup succeeds every fetch is refused (fail closed).
	SelfDomain string
	// RootCAs, nil for the system pool.
	RootCAs *x509.CertPool
	// Dial connects to a vetted address; nil dials it with a plain net.Dialer.
	Dial func(ctx context.Context, network string, addr netip.AddrPort) (net.Conn, error)
}

const (
	// SelfRefresh is how often instance.domain is resolved again for the refused set.
	SelfRefresh = 10 * time.Minute
	// SelfLookupTimeout bounds one lookup of instance.domain.
	SelfLookupTimeout = 5 * time.Second
)

// ErrSelfUnknown is returned while instance.domain has not resolved yet: the instance cannot tell its own addresses apart and fetches
// nothing (fail closed).
var ErrSelfUnknown = errors.New("webhookavatar: the instance's own addresses are not known yet")

// Fetcher fetches and processes one avatar at a time; it is safe for concurrent use.
type Fetcher struct {
	o      FetchOptions
	client *http.Client

	selfMu     sync.RWMutex
	selfDomain []netip.Addr
	selfReady  bool
}

// self is the refused set: Self and the last addresses instance.domain resolved to. It reports false while a
// configured domain has never resolved. It only reads what RefreshSelf stored.
func (f *Fetcher) self() ([]netip.Addr, bool) {
	if f.o.SelfDomain == "" {
		return f.o.Self, true
	}
	f.selfMu.RLock()
	defer f.selfMu.RUnlock()
	if !f.selfReady {
		return nil, false
	}
	return append(append([]netip.Addr(nil), f.o.Self...), f.selfDomain...), true
}

// RefreshSelf resolves instance.domain once, within SelfLookupTimeout. A success replaces the stored addresses; a
// failure keeps the last good ones (and, before any success, fetching stays refused).
func (f *Fetcher) RefreshSelf(ctx context.Context) error {
	if f.o.SelfDomain == "" {
		return nil
	}
	host := f.o.SelfDomain
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	ctx, cancel := context.WithTimeout(ctx, SelfLookupTimeout)
	defer cancel()
	addrs, err := f.o.Resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return err
	}
	if len(addrs) == 0 {
		return ErrAddress
	}
	f.selfMu.Lock()
	f.selfDomain, f.selfReady = addrs, true
	f.selfMu.Unlock()
	return nil
}

// RunSelfRefresh resolves instance.domain now and then every SelfRefresh, until ctx ends.
func (f *Fetcher) RunSelfRefresh(ctx context.Context) {
	if f.o.SelfDomain == "" {
		return
	}
	_ = f.RefreshSelf(ctx)
	t := time.NewTicker(SelfRefresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = f.RefreshSelf(ctx)
		}
	}
}

// NewFetcher builds a Fetcher.
func NewFetcher(o FetchOptions) *Fetcher {
	if o.Resolver == nil {
		o.Resolver = net.DefaultResolver
	}
	if o.Dial == nil {
		d := &net.Dialer{Timeout: FetchTimeout}
		o.Dial = func(ctx context.Context, network string, addr netip.AddrPort) (net.Conn, error) {
			return d.DialContext(ctx, network, addr.String())
		}
	}
	f := &Fetcher{o: o}
	f.client = &http.Client{
		Transport: &http.Transport{
			Proxy:                  nil, // never the environment's proxy
			DialContext:            f.dial,
			TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: o.RootCAs},
			TLSHandshakeTimeout:    FetchTimeout,
			ResponseHeaderTimeout:  FetchTimeout,
			MaxResponseHeaderBytes: 16 << 10,
			DisableKeepAlives:      true,
			DisableCompression:     true,
			ForceAttemptHTTP2:      true,
		},
		CheckRedirect: checkRedirect,
		Timeout:       FetchTimeout,
	}
	return f
}

// dial resolves the host itself, refuses the connection unless every answer may be fetched, and dials the vetted
// address: the address checked is the address connected to.
func (f *Fetcher) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port != "443" {
		return nil, ErrURL
	}
	var addrs []netip.Addr
	if literal, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{literal}
	} else if addrs, err = f.o.Resolver.LookupNetIP(ctx, "ip", host); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrAddress, err)
	}
	if len(addrs) == 0 {
		return nil, ErrAddress
	}
	self, known := f.self()
	if !known {
		return nil, ErrSelfUnknown
	}
	for _, a := range addrs {
		if Blocked(a, self) {
			return nil, ErrAddress
		}
	}
	return f.o.Dial(ctx, network, netip.AddrPortFrom(addrs[0].WithZone("").Unmap(), 443))
}

func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > MaxRedirects {
		return ErrResponse
	}
	if _, err := CheckURL(req.URL.String()); err != nil {
		return err
	}
	return nil
}

// Fetch fetches raw and returns the image re-encoded as PNG.
func (f *Fetcher) Fetch(ctx context.Context, raw string) ([]byte, error) {
	u, err := CheckURL(raw)
	if err != nil {
		return nil, err
	}
	if _, known := f.self(); !known {
		return nil, ErrSelfUnknown
	}
	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, ErrURL
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "image/png, image/jpeg, image/gif")
	res, err := f.client.Do(req)
	if err != nil {
		// The sentinel the dial or redirect check returned, when there is one; never the URL in an error string.
		for _, s := range []error{ErrURL, ErrAddress, ErrResponse, ErrSelfUnknown} {
			if errors.Is(err, s) {
				return nil, s
			}
		}
		return nil, ErrResponse
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, ErrResponse
	}
	if res.ContentLength > MaxBodyBytes {
		return nil, ErrTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, MaxBodyBytes+1))
	if err != nil {
		return nil, ErrResponse
	}
	if len(body) > MaxBodyBytes {
		return nil, ErrTooLarge
	}
	return Process(body)
}

// Process sniffs body, checks the dimensions before decoding, decodes it and re-encodes it as PNG.
func Process(body []byte) ([]byte, error) {
	if len(body) > MaxBodyBytes {
		return nil, ErrTooLarge
	}
	var want string
	switch http.DetectContentType(body) {
	case "image/png":
		want = "png"
	case "image/jpeg":
		want = "jpeg"
	case "image/gif":
		want = "gif"
	default:
		return nil, ErrNotImage
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(body))
	if err != nil || format != want {
		return nil, ErrNotImage
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > MaxSide || cfg.Height > MaxSide {
		return nil, ErrDimensions
	}
	img, _, err := image.Decode(bytes.NewReader(body))
	if err != nil {
		return nil, ErrNotImage
	}
	if b := img.Bounds(); b.Dx() < 1 || b.Dy() < 1 || b.Dx() > MaxSide || b.Dy() > MaxSide {
		return nil, ErrDimensions
	}
	var out bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&out, img); err != nil {
		return nil, ErrNotImage
	}
	if out.Len() > MaxProcessedBytes {
		return nil, ErrTooLarge
	}
	return out.Bytes(), nil
}

// LocalAddrs is the addresses of this host's interfaces, for FetchOptions.Self.
func LocalAddrs() []netip.Addr {
	var out []netip.Addr
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		if p, err := netip.ParsePrefix(a.String()); err == nil {
			out = append(out, p.Addr().Unmap())
		}
	}
	return out
}
