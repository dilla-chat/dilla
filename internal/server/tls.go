package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/pelletier/go-toml/v2"

	"github.com/jonasthim/dilla/internal/config"
)

// TLSMode is config.TLSMode: dilla.toml's one mode switch. The four spellings
// are config's (acme_tls_alpn, acme_dns, acme_ip, behind_proxy); this package
// names them again only so a reader of the listener code does not have to
// cross to internal/config to see them.
type TLSMode = config.TLSMode

const (
	ModeALPN   = config.TLSModeACMETLSALPN
	ModeDNS    = config.TLSModeACMEDNS
	ModeACMEIP = config.TLSModeACMEIP
	ModeProxy  = config.TLSModeBehindProxy
)

// DNSProvider is what a DNS-01 solver needs from a DNS host: libdns's
// RecordAppender and RecordDeleter. Each provider is a separate
// github.com/libdns/<provider> module.
type DNSProvider = certmagic.DNSProvider

// DNSProviderFactory builds a provider from the operator's credentials file.
type DNSProviderFactory func(credentials map[string]string) (DNSProvider, error)

var (
	dnsProvidersMu sync.RWMutex
	dnsProviders   = map[string]DNSProviderFactory{}
)

// RegisterDNSProvider makes a provider available to tls.dns.provider. This
// build registers none: every libdns provider is a Go module of its own, and
// Plan 2 adds no module beyond the ones Plan 1 pins, so acme_dns refuses to
// start until a build registers the provider it names. There is no
// certmagic-side plugin machinery; this map is the whole registry.
func RegisterDNSProvider(name string, f DNSProviderFactory) {
	dnsProvidersMu.Lock()
	defer dnsProvidersMu.Unlock()
	dnsProviders[name] = f
}

func dnsProvider(name string) (DNSProviderFactory, bool) {
	dnsProvidersMu.RLock()
	defer dnsProvidersMu.RUnlock()
	f, ok := dnsProviders[name]
	return f, ok
}

// acmeSettings is what each mode fixes about issuance.
type acmeSettings struct {
	Profile              string
	RenewalWindowRatio   float64 // 0 is certmagic's default, 1/3
	DisableHTTPChallenge bool
	DNS01                bool
}

// settingsFor derives the issuance settings of an acme_* mode.
//
//   - DisableHTTPChallenge is set in every mode: dillad binds no port 80, and
//     acmez shuffles the challenge order, so an enabled HTTP-01 would be tried
//     first about half the time and fail.
//   - acme_dns makes DNS-01 exclusive (a non-nil DNS01Solver does).
//   - acme_ip forces the "shortlived" profile, which Let's Encrypt requires for
//     an IP address certificate, and renews at half the lifetime: certmagic's
//     1/3 default renews a 160-hour certificate with 53 h left, 0.5 with 80 h,
//     which a homelab box that sleeps can meet. It validates with TLS-ALPN-01:
//     RFC 8738 §7 rules dns-01 out for an IP identifier.
//
// An operator's tls.renewal_window_ratio overrides the mode's default.
func settingsFor(c config.TLS) acmeSettings {
	s := acmeSettings{Profile: c.Profile, DisableHTTPChallenge: true}
	switch c.Mode {
	case ModeDNS:
		s.DNS01 = true
	case ModeACMEIP:
		s.Profile = "shortlived"
		s.RenewalWindowRatio = 0.5
	}
	if c.RenewalWindowRatio > 0 {
		s.RenewalWindowRatio = c.RenewalWindowRatio
	}
	return s
}

// TLS is one certmagic configuration and the certificate cache it owns.
type TLS struct {
	magic *certmagic.Config
	cache *certmagic.Cache

	mu         sync.Mutex
	lastErr    string
	onFailure  func(msg string)
	onObtained func()

	closeOnce sync.Once
}

// BuildTLS constructs the certmagic configuration for an acme_* mode and
// returns the *tls.Config the 443 listener serves. It never contacts an ACME
// server: issuance is (*TLS).Manage. behind_proxy is refused, because in that
// mode certmagic is not constructed at all — behind_proxy is dilla's own mode,
// and certmagic v0.25.4 has no such thing.
//
// The cache is dillad's own, not certmagic.NewDefault's package-global one:
// the default cache's GetConfigForCert answers certmagic.NewDefault(), which is
// built from the package Default and would renew with none of this
// configuration — no email, no CA, no DNS solver and no OnEvent hook. The cache
// stops when ctx ends or on Close.
func BuildTLS(ctx context.Context, c config.TLS, domains []string) (*TLS, *tls.Config, error) {
	switch c.Mode {
	case ModeALPN, ModeDNS, ModeACMEIP:
	case ModeProxy:
		return nil, nil, errors.New("tls: behind_proxy terminates TLS at the proxy; certmagic is not constructed")
	default:
		return nil, nil, fmt.Errorf("tls: tls.mode %q is not one of acme_tls_alpn, acme_dns, acme_ip, behind_proxy", c.Mode)
	}
	if len(domains) == 0 || domains[0] == "" {
		return nil, nil, errors.New("tls: no name to obtain a certificate for")
	}
	s := settingsFor(c)
	issuer := certmagic.ACMEIssuer{
		CA:                   c.CA,
		TestCA:               c.TestCA,
		Email:                c.Email,
		Agreed:               c.Agreed,
		Profile:              s.Profile,
		DisableHTTPChallenge: s.DisableHTTPChallenge,
	}
	if s.DNS01 {
		solver, err := dnsSolver(c.DNS)
		if err != nil {
			return nil, nil, err
		}
		issuer.DNS01Solver = solver
	}

	t := &TLS{}
	t.cache = certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) { return t.magic, nil },
	})
	magic := certmagic.New(t.cache, certmagic.Config{
		RenewalWindowRatio: s.RenewalWindowRatio,
		Storage:            &certmagic.FileStorage{Path: c.StorageDir},
		// A client that sends no SNI — one dialling the instance by IP in
		// acme_ip mode — gets the instance's certificate.
		DefaultServerName: domains[0],
		OnEvent:           t.onEvent,
	})
	magic.Issuers = []certmagic.Issuer{certmagic.NewACMEIssuer(magic, issuer)}
	t.magic = magic
	context.AfterFunc(ctx, t.Close)

	tc := magic.TLSConfig()
	// certmagic's TLSConfig sets NextProtos to exactly ["acme-tls/1"]. dillad
	// appends and keeps acme-tls/1 first, or TLS-ALPN-01 renewals break.
	tc.NextProtos = append(tc.NextProtos, "h2", "http/1.1")
	tc.MinVersion = tls.VersionTLS12 // TURN/TLS clients; LiveKit and pion use the same floor
	return t, tc, nil
}

// dnsSolver builds the DNS-01 solver [tls.dns] describes.
func dnsSolver(d config.TLSDNS) (*certmagic.DNS01Solver, error) {
	if d.Provider == "" {
		return nil, errors.New("tls: acme_dns needs tls.dns.provider")
	}
	factory, ok := dnsProvider(d.Provider)
	if !ok {
		return nil, fmt.Errorf("tls: tls.dns.provider %q is not built into this dillad", d.Provider)
	}
	creds := map[string]string{}
	if d.CredentialsFile != "" {
		body, err := os.ReadFile(d.CredentialsFile)
		if err != nil {
			return nil, fmt.Errorf("tls: tls.dns.credentials_file: %w", err)
		}
		if err := toml.Unmarshal(body, &creds); err != nil {
			return nil, fmt.Errorf("tls: tls.dns.credentials_file %s is not a table of strings: %w", d.CredentialsFile, err)
		}
	}
	provider, err := factory(creds)
	if err != nil {
		return nil, fmt.Errorf("tls: dns provider %s: %w", d.Provider, err)
	}
	return &certmagic.DNS01Solver{DNSManager: certmagic.DNSManager{
		DNSProvider:        provider,
		TTL:                d.TTL.Value(),
		PropagationDelay:   d.PropagationDelay.Value(),
		PropagationTimeout: d.PropagationTimeout.Value(),
		Resolvers:          d.Resolvers,
		OverrideDomain:     d.OverrideDomain,
	}}, nil
}

// Manage is the ACME round trip: it loads the certificates for domains from
// storage or obtains them (certmagic's ManageSync), and from then on the cache
// renews them in the background. The 443 listener must already be serving the
// *tls.Config BuildTLS returned, because TLS-ALPN-01 is answered on it.
func (t *TLS) Manage(ctx context.Context, domains []string) error {
	return t.magic.ManageSync(ctx, domains)
}

// SetHooks installs what runs when an issuance or renewal fails (with the
// error's text) and when one succeeds. Either may be nil.
func (t *TLS) SetHooks(onFailure func(msg string), onObtained func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onFailure, t.onObtained = onFailure, onObtained
}

// LastRenewalError is "" when the most recent issuance or renewal succeeded
// (or none has run), and the failure's text otherwise.
func (t *TLS) LastRenewalError() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastErr
}

// onEvent is certmagic's OnEvent hook. certmagic keeps serving the existing
// certificate when a renewal fails — that part is certmagic's own behaviour and
// needs no code — but it says nothing, so the operator would find out when the
// certificate expires. This is the alert: `dillad doctor`'s certificate leg
// and dilla_cert_renewal_failures_total read what it records. It always
// returns nil: an error from a "cert_obtaining" hook would abort the issuance.
func (t *TLS) onEvent(_ context.Context, event string, data map[string]any) error {
	var hook func()
	t.mu.Lock()
	switch event {
	case "cert_failed":
		msg := fmt.Sprint(data["error"])
		t.lastErr = msg
		if f := t.onFailure; f != nil {
			hook = func() { f(msg) }
		}
	case "cert_obtained":
		t.lastErr = ""
		hook = t.onObtained
	}
	t.mu.Unlock()
	if hook != nil {
		hook()
	}
	return nil
}

// Close stops the certificate cache's maintenance goroutine. It is idempotent.
func (t *TLS) Close() {
	t.closeOnce.Do(func() {
		if t.cache != nil {
			t.cache.Stop()
		}
	})
}

// NewHTTPServer is dillad's one http.Server, for whichever listener tls.mode
// chooses. ReadHeaderTimeout is never zero (net/http reads zero as "no
// limit"); ReadTimeout and WriteTimeout stay unset because the gateway's
// WebSocket shares the listener and keeps its own deadlines.
//
// The protocols follow the listener. Behind a proxy the listener is plain, so
// HTTP/1.1 plus unencrypted HTTP/2 (stdlib h2c in Go 1.27): a front proxy
// configured for h2c reaches the API, while /gateway stays HTTP/1.1 because a
// WebSocket upgrade is an HTTP/1.1 mechanism. On the direct TLS listener it is
// HTTP/1.1 plus h2 over TLS, and never unencrypted HTTP/2. In both, Go does not
// advertise RFC 8441 extended CONNECT unless GODEBUG=http2xconnect=1, which
// dillad must never set: /gateway needs http.Hijacker, which an HTTP/2 stream
// cannot give, so browsers open a separate HTTP/1.1 connection for it.
func NewHTTPServer(h http.Handler, c *config.Config) *http.Server {
	rht := c.Server.ReadHeaderTimeout.Value()
	if rht <= 0 {
		rht = 10 * time.Second
	}
	budget := c.Server.ReadBodyTimeout.Value()
	if budget <= 0 {
		budget = 30 * time.Second
	}
	p := new(http.Protocols)
	p.SetHTTP1(true)
	if c.BehindProxy() {
		p.SetUnencryptedHTTP2(true)
	} else {
		p.SetHTTP2(true)
	}
	return &http.Server{
		Handler:           withBodyReadDeadline(h, budget),
		ReadHeaderTimeout: rht,
		IdleTimeout:       c.Server.IdleTimeout.Value(),
		Protocols:         p,
	}
}

// Ordinary requests have a body budget as well as a header budget. Installing
// it outside the router covers authentication and peeks too; commit and blob
// handlers can replace it with their larger route budget before reading.
// GET upgrades have no body and retain the gateway's own connection deadlines.
func withBodyReadDeadline(next http.Handler, budget time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Body != http.NoBody {
			controller := http.NewResponseController(w)
			if err := controller.SetReadDeadline(time.Now().Add(budget)); err != nil {
				slog.Error("request body deadline unavailable", "err", err)
				w.Header().Set("Connection", "close")
				WriteError(w, Errorf(CodeUnavailable, "request body deadline unavailable"))
				return
			}
			body := r.Body
			defer func() {
				// net/http may drain an unread HTTP/1 body while flushing its
				// buffered response. Close under the budget before clearing it,
				// so early refusals cannot turn that drain into an unbounded read.
				_ = body.Close()
				_ = controller.SetReadDeadline(time.Time{})
			}()
		}
		if next == nil {
			http.DefaultServeMux.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}
