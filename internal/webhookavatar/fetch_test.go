package webhookavatar

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(1, 1, color.RGBA{R: 200, A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func jpegOf(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewGray(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// fakeResolver answers from a table; an unknown host is an error.
type fakeResolver map[string][]string

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	answers, ok := f[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	out := make([]netip.Addr, 0, len(answers))
	for _, a := range answers {
		out = append(out, netip.MustParseAddr(a))
	}
	return out, nil
}

// avatarServer is a TLS server whose certificate names example.com, reached by dialling any vetted address: the
// vetting under test is real, only the last hop is redirected to the local listener.
type avatarServer struct {
	srv    *httptest.Server
	mu     sync.Mutex
	dialed []netip.AddrPort
	heads  []http.Header
}

func newAvatarServer(t *testing.T, routes map[string]func(w http.ResponseWriter, r *http.Request)) *avatarServer {
	t.Helper()
	a := &avatarServer{}
	mux := http.NewServeMux()
	for path, h := range routes {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			a.mu.Lock()
			a.heads = append(a.heads, r.Header.Clone())
			a.mu.Unlock()
			h(w, r)
		})
	}
	a.srv = httptest.NewTLSServer(mux)
	t.Cleanup(a.srv.Close)
	return a
}

func (a *avatarServer) fetcher(t *testing.T, self ...string) *Fetcher {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(a.srv.Certificate())
	var selfAddrs []netip.Addr
	for _, s := range self {
		selfAddrs = append(selfAddrs, netip.MustParseAddr(s))
	}
	return NewFetcher(FetchOptions{
		Resolver: fakeResolver{
			"example.com":     {"93.184.216.34"},
			"cdn.test":        {"93.184.216.35"},
			"internal.test":   {"10.0.0.5"},
			"meta.test":       {"169.254.169.254"},
			"loop.test":       {"127.0.0.1"},
			"mixed.test":      {"93.184.216.34", "10.0.0.1"},
			"mapped.test":     {"::ffff:127.0.0.1"},
			"ula.test":        {"fd00::1"},
			"cgnat.test":      {"100.64.1.1"},
			"nat64local.test": {"64:ff9b:1:7f00:1:0:808:808"},
			"siit.test":       {"::ffff:0:7f00:1"},
			"self.test":       {"93.184.216.36"},
		},
		Self:    selfAddrs,
		RootCAs: pool,
		Dial: func(ctx context.Context, network string, addr netip.AddrPort) (net.Conn, error) {
			a.mu.Lock()
			a.dialed = append(a.dialed, addr)
			a.mu.Unlock()
			return (&net.Dialer{}).DialContext(ctx, network, a.srv.Listener.Addr().String())
		},
	})
}

func (a *avatarServer) dials() []netip.AddrPort {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]netip.AddrPort(nil), a.dialed...)
}

func serve(body []byte, contentType string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(body)
	}
}

func redirect(to string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, to, http.StatusFound) }
}

func TestFetchReencodesAValidImageAndSendsNothingIdentifying(t *testing.T) {
	jpg := jpegOf(t, 32, 16)
	a := newAvatarServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"/a.jpg":     serve(jpg, "image/png"), // a lying header: the bytes decide
		"/hop1":      redirect("https://example.com/hop2"),
		"/hop2":      redirect("https://example.com/a.jpg"),
		"/redirect3": redirect("https://example.com/hop1"),
	})
	f := a.fetcher(t)
	for _, raw := range []string{"https://example.com/a.jpg", "https://example.com:443/hop1"} {
		got, err := f.Fetch(context.Background(), raw)
		if err != nil {
			t.Fatalf("Fetch(%s): %v", raw, err)
		}
		if http.DetectContentType(got) != "image/png" || bytes.Equal(got, jpg) {
			t.Fatalf("Fetch(%s) did not re-encode to PNG", raw)
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(got))
		if err != nil || cfg.Width != 32 || cfg.Height != 16 {
			t.Fatalf("re-encoded %+v, %v", cfg, err)
		}
	}
	// Three redirects is one too many.
	if _, err := f.Fetch(context.Background(), "https://example.com/redirect3"); !errors.Is(err, ErrResponse) {
		t.Fatalf("three redirects = %v, want ErrResponse", err)
	}
	for _, d := range a.dials() {
		if d != netip.MustParseAddrPort("93.184.216.34:443") {
			t.Fatalf("dialled %s, want only the vetted 93.184.216.34:443", d)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, h := range a.heads {
		if h.Get("User-Agent") != UserAgent || h.Get("Cookie") != "" || h.Get("Authorization") != "" || h.Get("X-Forwarded-For") != "" {
			t.Fatalf("request headers %v", h)
		}
	}
}

func TestFetchRefusesPrivateTargets(t *testing.T) {
	a := newAvatarServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"/ok.png":       serve(pngOf(t, 8, 8), "image/png"),
		"/to-internal":  redirect("https://internal.test/x.png"),
		"/to-meta":      redirect("https://169.254.169.254/latest/meta-data"),
		"/to-http":      redirect("http://example.com/ok.png"),
		"/to-port":      redirect("https://example.com:8443/ok.png"),
		"/to-loopback6": redirect("https://[::1]/ok.png"),
	})
	f := a.fetcher(t, "93.184.216.35")
	for raw, want := range map[string]error{
		"http://example.com/ok.png":                               ErrURL,
		"https://example.com:8443/ok.png":                         ErrURL,
		"https://user:pw@example.com/ok.png":                      ErrURL,
		"https://example.com/" + strings.Repeat("a", MaxURLBytes): ErrURL,
		"https://127.0.0.1/ok.png":                                ErrAddress,
		"https://[::1]/ok.png":                                    ErrAddress,
		"https://[::ffff:127.0.0.1]/ok.png":                       ErrAddress,
		"https://[64:ff9b::a00:1]/ok.png":                         ErrAddress, // NAT64 of 10.0.0.1
		"https://[2002:a00:1::]/ok.png":                           ErrAddress, // 6to4 of 10.0.0.1
		"https://169.254.169.254/ok.png":                          ErrAddress,
		"https://internal.test/ok.png":                            ErrAddress,
		"https://meta.test/ok.png":                                ErrAddress,
		"https://loop.test/ok.png":                                ErrAddress,
		"https://mixed.test/ok.png":                               ErrAddress, // one bad answer refuses the host
		"https://mapped.test/ok.png":                              ErrAddress,
		"https://ula.test/ok.png":                                 ErrAddress,
		"https://cgnat.test/ok.png":                               ErrAddress,
		"https://cdn.test/ok.png":                                 ErrAddress, // the instance's own address
		"https://unknown.test/ok.png":                             ErrAddress,
		"https://example.com/to-internal":                         ErrAddress,
		"https://example.com/to-meta":                             ErrAddress,
		"https://example.com/to-http":                             ErrURL,
		"https://example.com/to-port":                             ErrURL,
		"https://example.com/to-loopback6":                        ErrAddress,
		// Fix round 2 S2/N-1: the local-use NAT64 /48 (127.0.0.1 in its /48 layout) and SIIT ::ffff:0:0/96.
		"https://[64:ff9b:1:7f00:1:0:808:808]/ok.png": ErrAddress,
		"https://[::ffff:0:7f00:1]/ok.png":            ErrAddress,
		"https://nat64local.test/ok.png":              ErrAddress,
		"https://siit.test/ok.png":                    ErrAddress,
	} {
		if _, err := f.Fetch(context.Background(), raw); !errors.Is(err, want) {
			t.Errorf("Fetch(%.60s) = %v, want %v", raw, err, want)
		}
	}
	for _, d := range a.dials() {
		if Blocked(d.Addr(), nil) {
			t.Fatalf("a blocked address was dialled: %s", d)
		}
	}
}

func TestFetchRefusesWhatIsNotASmallImage(t *testing.T) {
	big := append(pngOf(t, 8, 8), bytes.Repeat([]byte{0}, MaxBodyBytes)...)
	webp := append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), bytes.Repeat([]byte{0}, 32)...)
	a := newAvatarServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"/big.png":  serve(big, "image/png"),
		"/page":     serve([]byte("<!doctype html><title>x</title>"), "image/png"),
		"/huge.png": serve(pngOf(t, MaxSide+1, 4), "image/png"),
		"/a.webp":   serve(webp, "image/webp"),
		"/404":      func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", http.StatusNotFound) },
	})
	f := a.fetcher(t)
	for path, want := range map[string]error{
		"/big.png": ErrTooLarge, "/page": ErrNotImage, "/huge.png": ErrDimensions, "/a.webp": ErrNotImage, "/404": ErrResponse,
	} {
		if _, err := f.Fetch(context.Background(), "https://example.com"+path); !errors.Is(err, want) {
			t.Errorf("Fetch(%s) = %v, want %v", path, err, want)
		}
	}
}

// Fix round 2 N-4: the addresses instance.domain resolves to are refused, so an avatar URL naming the operator's own
// public address cannot hairpin through their proxy.
// switchResolver answers instance.domain lookups from a fakeResolver, or fails while down is set; every other host
// goes to the fake table.
type switchResolver struct {
	fakeResolver
	down atomic.Bool
}

func (r *switchResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	if host == "self.test" && r.down.Load() {
		return nil, errors.New("SERVFAIL")
	}
	return r.fakeResolver.LookupNetIP(ctx, network, host)
}

// Fix round 2 N-4 and round 3 S3: the addresses instance.domain resolves to are refused; the domain is resolved by the
// background refresh only; before its first success nothing is fetched; a failed refresh keeps the last good set.
func TestFetchRefusesTheInstanceDomainsAddresses(t *testing.T) {
	a := newAvatarServer(t, map[string]func(http.ResponseWriter, *http.Request){"/ok.png": serve(pngOf(t, 8, 8), "image/png")})
	f := a.fetcher(t)
	base, ok := f.o.Resolver.(fakeResolver)
	if !ok {
		t.Fatal("the test fetcher's resolver is not the fake table")
	}
	r := &switchResolver{fakeResolver: base}
	r.down.Store(true)
	f.o.Resolver = r
	f.o.SelfDomain = "self.test:443"

	// Before the first successful lookup: fail closed, and nothing is dialled.
	if err := f.RefreshSelf(context.Background()); err == nil {
		t.Fatal("RefreshSelf succeeded while the resolver is down")
	}
	if _, err := f.Fetch(context.Background(), "https://example.com/ok.png"); !errors.Is(err, ErrSelfUnknown) {
		t.Fatalf("fetch before the first lookup = %v, want ErrSelfUnknown", err)
	}
	if n := len(a.dials()); n != 0 {
		t.Fatalf("%d dials before the instance's addresses were known", n)
	}

	r.down.Store(false)
	if err := f.RefreshSelf(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Fetch(context.Background(), "https://[::ffff:93.184.216.36]/ok.png"); !errors.Is(err, ErrAddress) {
		t.Fatalf("the instance domain's address = %v, want ErrAddress", err)
	}
	if _, err := f.Fetch(context.Background(), "https://example.com/ok.png"); err != nil {
		t.Fatalf("another address = %v", err)
	}

	// A failed refresh keeps the last good set: the domain's address stays refused, fetching goes on.
	r.down.Store(true)
	if err := f.RefreshSelf(context.Background()); err == nil {
		t.Fatal("RefreshSelf succeeded while the resolver is down")
	}
	if _, err := f.Fetch(context.Background(), "https://93.184.216.36/ok.png"); !errors.Is(err, ErrAddress) {
		t.Fatalf("after a failed refresh the domain's address = %v, want still ErrAddress", err)
	}
	if _, err := f.Fetch(context.Background(), "https://example.com/ok.png"); err != nil {
		t.Fatalf("after a failed refresh another address = %v", err)
	}
}

func TestBlocked(t *testing.T) {
	for _, s := range []string{
		"0.0.0.0", "127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "100.127.255.255",
		"224.0.0.1", "255.255.255.255", "198.18.0.1", "::", "::1", "fe80::1", "fc00::1", "fd12::1", "ff02::1",
		"::ffff:10.0.0.1", "64:ff9b::7f00:1", "2002:c0a8:101::1", "::127.0.0.1", "2001::1",
		// Fix round 2: everything outside 2000::/3 that is not a decoded translation form.
		"64:ff9b:1:7f00:1:0:808:808", "64:ff9b:1:a00:1:0:808:808", "64:ff9b:1::808:808", "::ffff:0:7f00:1", "::ffff:0:808:808",
		"100::1", "fec0::1", "4000::1", "2001:2::1", "2001:10::1", "3fff::1",
	} {
		if !Blocked(netip.MustParseAddr(s), nil) {
			t.Errorf("%s is not blocked", s)
		}
	}
	for _, s := range []string{"93.184.216.34", "1.1.1.1", "2606:4700:4700::1111", "64:ff9b::5db8:d822", "2002:5db8:d822::1"} {
		if Blocked(netip.MustParseAddr(s), nil) {
			t.Errorf("%s is blocked", s)
		}
	}
	if !Blocked(netip.MustParseAddr("93.184.216.34"), []netip.Addr{netip.MustParseAddr("93.184.216.34")}) {
		t.Error("the instance's own address is not blocked")
	}
}
