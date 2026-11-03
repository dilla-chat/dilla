package server_test

import (
	"context"
	"crypto/tls"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/libdns/libdns"

	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/server"
)

func TestBuildTLSKeepsACMETLS1FirstAndAppendsH2(t *testing.T) {
	// BuildTLS constructs; it does not issue. Manage is what would contact
	// Let's Encrypt, and this test never calls it.
	_, cfg, err := server.BuildTLS(t.Context(), config.TLS{
		Mode: config.TLSModeACMETLSALPN, Email: "ops@example.test", Agreed: true, StorageDir: t.TempDir(),
	}, []string{"chat.example.test"})
	if err != nil {
		t.Fatalf("BuildTLS: %v", err)
	}
	// certmagic's TLSConfig() sets NextProtos to exactly ["acme-tls/1"]; dillad
	// must append and must keep acme-tls/1 first, or TLS-ALPN-01 renewals break.
	if len(cfg.NextProtos) != 3 || cfg.NextProtos[0] != "acme-tls/1" ||
		cfg.NextProtos[1] != "h2" || cfg.NextProtos[2] != "http/1.1" {
		t.Fatalf("NextProtos = %v", cfg.NextProtos)
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %x; TURN/TLS clients and pion use a TLS 1.2 floor", cfg.MinVersion)
	}
	if cfg.GetCertificate == nil {
		t.Fatal("GetCertificate is not wired to certmagic")
	}
}

func TestTheACMEIPModeForcesShortlivedAndHalfTheWindow(t *testing.T) {
	got := server.ACMEIPSettingsForTest()
	if got.Profile != "shortlived" {
		t.Fatalf("profile = %q, want shortlived", got.Profile)
	}
	// certmagic's default renewal window ratio is 1/3, which for a 160-hour
	// certificate renews with 53.3 h left. 0.5 renews at 80 h, which a homelab
	// box that sleeps can actually meet.
	if got.RenewalWindowRatio != 0.5 {
		t.Fatalf("renewal window ratio = %v, want 0.5", got.RenewalWindowRatio)
	}
	if !got.DisableHTTPChallenge {
		t.Fatal("DisableHTTPChallenge must be set: acmez shuffles challenge order otherwise")
	}
	// RFC 8738 §7: dns-01 cannot validate an IP identifier, so the IP mode is
	// TLS-ALPN-01 and never DNS-01.
	if got.DNS01 {
		t.Fatal("acme_ip must not use DNS-01: RFC 8738 forbids it for IP identifiers")
	}
}

func TestBehindProxyBindsOnlyThePlainListenerAndLeavesTURNOff(t *testing.T) {
	c := config.TLS{Mode: config.TLSModeBehindProxy}
	if _, _, err := server.BuildTLS(t.Context(), c, []string{"chat.example.test"}); err == nil {
		t.Fatal("behind_proxy built a TLS config; certmagic must not be constructed at all")
	}
	full := config.Default()
	full.Instance.Domain = "chat.example.test"
	full.TLS.Mode = config.TLSModeBehindProxy
	full.Server.TrustedProxyCIDRs = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	full.LiveKit.Enabled = false
	full.Derive()
	if err := full.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if full.TURN.Enabled {
		t.Fatal("turn.enabled must default to false under behind_proxy")
	}
	full.TURN.Enabled = true
	err := full.Validate()
	if err == nil {
		t.Fatal("turn.enabled without turn.listen was accepted")
	}
	if !strings.Contains(err.Error(), "turn.listen is required") {
		t.Fatalf("Validate = %v, want the turn.listen refusal among its findings", err)
	}
}

func TestTheGatewayIsHTTP11Only(t *testing.T) {
	srv := server.NewHTTPServer(nil, config.Default())
	if srv.Protocols == nil {
		t.Fatal("Protocols is unset")
	}
	// The direct-TLS listener: h2 over TLS and HTTP/1.1, never unencrypted
	// HTTP/2 — and GODEBUG http2xconnect stays unset, because /gateway needs
	// Hijacker, which an HTTP/2 stream cannot give.
	if srv.Protocols.UnencryptedHTTP2() {
		t.Fatal("unencrypted HTTP/2 on the TLS listener")
	}
	if !srv.Protocols.HTTP1() || !srv.Protocols.HTTP2() {
		t.Fatal("the TLS listener must speak HTTP/1.1 (the /gateway upgrade) and h2")
	}
	if srv.ReadHeaderTimeout <= 0 {
		t.Fatalf("ReadHeaderTimeout = %v; zero is no limit to net/http", srv.ReadHeaderTimeout)
	}
}

// behind_proxy's plain listener is where a front proxy configured for h2c
// reaches the API; the gateway route stays HTTP/1.1 through the same server.
func TestBehindProxyServesUnencryptedHTTP2(t *testing.T) {
	c := config.Default()
	c.TLS.Mode = config.TLSModeBehindProxy
	srv := server.NewHTTPServer(nil, c)
	if !srv.Protocols.UnencryptedHTTP2() || !srv.Protocols.HTTP1() {
		t.Fatal("behind_proxy must serve HTTP/1.1 and unencrypted HTTP/2")
	}
}

func TestACMEDNSNeedsAKnownProvider(t *testing.T) {
	base := config.TLS{
		Mode: config.TLSModeACMEDNS, Email: "ops@example.test", Agreed: true, StorageDir: t.TempDir(),
	}
	if _, _, err := server.BuildTLS(t.Context(), base, []string{"chat.example.test"}); err == nil ||
		!strings.Contains(err.Error(), "tls.dns.provider") {
		t.Fatalf("acme_dns with no provider = %v, want a tls.dns.provider refusal", err)
	}
	base.DNS.Provider = "no-such-provider"
	if _, _, err := server.BuildTLS(t.Context(), base, []string{"chat.example.test"}); err == nil ||
		!strings.Contains(err.Error(), "no-such-provider") {
		t.Fatalf("acme_dns with an unknown provider = %v, want it named", err)
	}

	var got map[string]string
	server.RegisterDNSProvider("test-dns-provider", func(creds map[string]string) (server.DNSProvider, error) {
		got = creds
		return fakeDNS{}, nil
	})
	base.DNS.Provider = "test-dns-provider"
	base.DNS.CredentialsFile = writeFile(t, "creds.toml", "api_token = \"t0ken\"\n")
	_, cfg, err := server.BuildTLS(t.Context(), base, []string{"chat.example.test"})
	if err != nil {
		t.Fatalf("BuildTLS acme_dns: %v", err)
	}
	if got["api_token"] != "t0ken" {
		t.Fatalf("the provider saw credentials %v", got)
	}
	if cfg.NextProtos[0] != "acme-tls/1" {
		t.Fatalf("NextProtos = %v", cfg.NextProtos)
	}
}

type fakeDNS struct{}

func (fakeDNS) AppendRecords(context.Context, string, []libdns.Record) ([]libdns.Record, error) {
	return nil, errors.New("fake")
}

func (fakeDNS) DeleteRecords(context.Context, string, []libdns.Record) ([]libdns.Record, error) {
	return nil, errors.New("fake")
}

// certmagic keeps serving the existing certificate when a renewal fails and
// says nothing; the OnEvent hook is the alert. A failure is recorded, counted
// through the hook and cleared by the next success.
func TestTheRenewalHookRecordsFailuresAndClearsThem(t *testing.T) {
	tl, _, err := server.BuildTLS(t.Context(), config.TLS{
		Mode: config.TLSModeACMETLSALPN, Email: "ops@example.test", Agreed: true, StorageDir: t.TempDir(),
	}, []string{"chat.example.test"})
	if err != nil {
		t.Fatalf("BuildTLS: %v", err)
	}
	var mu sync.Mutex
	var failures []string
	obtained := 0
	tl.SetHooks(func(msg string) {
		mu.Lock()
		defer mu.Unlock()
		failures = append(failures, msg)
	}, func() {
		mu.Lock()
		defer mu.Unlock()
		obtained++
	})
	if err := tl.EmitForTest(t.Context(), "cert_failed", map[string]any{
		"renewal": true, "error": errors.New("acme: directory answered 500"),
	}); err != nil {
		t.Fatalf("emit cert_failed: %v", err)
	}
	if got := tl.LastRenewalError(); !strings.Contains(got, "500") {
		t.Fatalf("LastRenewalError = %q", got)
	}
	mu.Lock()
	if len(failures) != 1 || !strings.Contains(failures[0], "500") {
		t.Fatalf("failure hook saw %v", failures)
	}
	mu.Unlock()
	if err := tl.EmitForTest(t.Context(), "cert_obtained", map[string]any{"renewal": true}); err != nil {
		t.Fatalf("emit cert_obtained: %v", err)
	}
	if got := tl.LastRenewalError(); got != "" {
		t.Fatalf("LastRenewalError after a success = %q", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if obtained != 1 {
		t.Fatalf("obtained hook ran %d times", obtained)
	}
}
