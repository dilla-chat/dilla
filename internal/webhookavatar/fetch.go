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

// blockedPrefixes are the special-purpose ranges netip's predicates do not name: this network, CGNAT, IETF protocol
// assignments, documentation, benchmarking, reserved and broadcast, the old site-local and the discard-only ranges.
var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24",
		"203.0.113.0/24", "240.0.0.0/4",
		"100::/64", "2001:db8::/32", "fec0::/10", "2001::/32", // the last is Teredo: its embedded address is the relay's
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

var (
	sixToFour = netip.MustParsePrefix("2002::/16")
	nat64     = netip.MustParsePrefix("64:ff9b::/96")
	nat64Ext  = netip.MustParsePrefix("64:ff9b:1::/48")
	v4compat  = netip.MustParsePrefix("::/96")
)

// Blocked reports whether a may not be fetched from: it is not valid, is loopback, private (RFC 1918 or ULA
// fc00::/7), link-local, multicast, unspecified, in a special-purpose range, one of self, or an IPv4-mapped,
// IPv4-compatible, 6to4 or NAT64 form of any of those.
func Blocked(a netip.Addr, self []netip.Addr) bool {
	a = a.WithZone("").Unmap()
	if !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	if a.Is6() {
		b := a.As16()
		switch {
		case sixToFour.Contains(a):
			return Blocked(netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}), self)
		case nat64.Contains(a), nat64Ext.Contains(a), v4compat.Contains(a):
			return Blocked(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), self)
		}
	}
	for _, s := range self {
		if s.WithZone("").Unmap() == a {
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
	// RootCAs, nil for the system pool.
	RootCAs *x509.CertPool
	// Dial connects to a vetted address; nil dials it with a plain net.Dialer.
	Dial func(ctx context.Context, network string, addr netip.AddrPort) (net.Conn, error)
}

// Fetcher fetches and processes one avatar at a time; it is safe for concurrent use.
type Fetcher struct {
	o      FetchOptions
	client *http.Client
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
	for _, a := range addrs {
		if Blocked(a, f.o.Self) {
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
		for _, s := range []error{ErrURL, ErrAddress, ErrResponse} {
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
