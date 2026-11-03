package ops_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/turn/v5"

	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/ops"
)

func TestClockSkewParsesTheDateHeader(t *testing.T) {
	// RFC 9110 §6.6.1: an origin server MUST generate Date in all responses
	// except 1xx, 204 and 304. So any HTTPS origin works, not only the ACME one.
	skewed := time.Now().Add(-10 * time.Minute)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", skewed.UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	got, err := ops.ClockSkew(t.Context(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("ClockSkew: %v", err)
	}
	// HTTP-date has one-second granularity, so the floor is 1s + rtt/2.
	if got < 9*time.Minute || got > 11*time.Minute {
		t.Fatalf("skew = %v, want about 10m", got)
	}
}

func TestClockLegFailsPast60Seconds(t *testing.T) {
	ok1, ok2, ok3 := mk60(t, 0), mk60(t, 0), mk60(t, 0)
	leg := ops.ClockLeg(t.Context(), ok1.Client(), []string{ok1.URL, ok2.URL, ok3.URL}, 60*time.Second)
	if leg.Status != ops.Green {
		t.Fatalf("a synchronised clock = %v (%s)", leg.Status, leg.Detail)
	}

	bad1, bad2, bad3 := mk60(t, -5*time.Minute), mk60(t, -5*time.Minute), mk60(t, -5*time.Minute)
	leg = ops.ClockLeg(t.Context(), bad1.Client(), []string{bad1.URL, bad2.URL, bad3.URL}, 60*time.Second)
	if leg.Status != ops.Red {
		t.Fatalf("a 5-minute skew = %v", leg.Status)
	}
	for _, want := range []string{"ahead", "ACME", "KeyPackage"} {
		if !strings.Contains(leg.Detail, want) {
			t.Fatalf("detail %q does not say what breaks (%q)", leg.Detail, want)
		}
	}
	if leg.Fix == "" {
		t.Fatal("a red clock leg must name the fix")
	}
}

// mk60 returns an httptest server whose Date header is offset by off,
// registered for cleanup with t.Cleanup.
func mk60(t *testing.T, off time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", time.Now().Add(off).UTC().Format(http.TimeFormat))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOneMisconfiguredPeerDoesNotFailTheLeg(t *testing.T) {
	good1, good2 := mk60(t, 0), mk60(t, 0)
	bad := mk60(t, -30*time.Minute)
	leg := ops.ClockLeg(t.Context(), good1.Client(), []string{good1.URL, good2.URL, bad.URL}, 60*time.Second)
	if leg.Status != ops.Green {
		t.Fatalf("the median of three should be green, got %v (%s)", leg.Status, leg.Detail)
	}
}

func TestAClockWithNoAnsweringPeerIsYellowNotRed(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	leg := ops.ClockLeg(t.Context(), http.DefaultClient, []string{dead.URL}, 60*time.Second)
	if leg.Status != ops.Yellow || leg.Fix == "" {
		t.Fatalf("an unreachable peer set = %v fix=%q; a booting host with no network must not fail doctor", leg.Status, leg.Fix)
	}
}

func TestASlowPeerIsDiscarded(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2100 * time.Millisecond)
		w.Header().Set("Date", time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat))
	}))
	t.Cleanup(slow.Close)
	leg := ops.ClockLeg(t.Context(), slow.Client(), []string{slow.URL}, 60*time.Second)
	if leg.Status != ops.Yellow || !strings.Contains(leg.Detail, "too slow") {
		t.Fatalf("a 2.1s round trip was judged: %v %q", leg.Status, leg.Detail)
	}
}

func TestWatchClockPublishesTheMedianSkewInSeconds(t *testing.T) {
	a, b, c := mk60(t, -5*time.Minute), mk60(t, -5*time.Minute), mk60(t, 0)
	got := make(chan float64, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go ops.WatchClock(ctx, a.Client(), []string{a.URL, b.URL, c.URL}, time.Hour, func(s float64) {
		select {
		case got <- s:
		default:
		}
	})
	select {
	case s := <-got:
		if s < 290 || s > 310 {
			t.Fatalf("gauge = %vs, want about +300 (local clock ahead of the median peer)", s)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("WatchClock never published a sample")
	}
}

func TestTheReportIsStableDiffableText(t *testing.T) {
	r := ops.Report{Legs: []ops.Leg{
		{Name: "config", Status: ops.Green, Detail: "parsed, 128 keys"},
		{Name: "database", Status: ops.Green, Detail: "goose 9, target 9"},
		{Name: "certificate", Status: ops.Yellow, Detail: "expires in 12d, issuer Let's Encrypt"},
		{Name: "turn", Status: ops.Red, Detail: "no allocation", Fix: "open tcp/5349 to dillad"},
	}}
	want := strings.Join([]string{
		"OK    config       parsed, 128 keys",
		"OK    database     goose 9, target 9",
		"WARN  certificate  expires in 12d, issuer Let's Encrypt",
		"FAIL  turn         no allocation",
		"      fix: open tcp/5349 to dillad",
		"",
	}, "\n")
	if got := r.Text(); got != want {
		t.Fatalf("Text() =\n%s\nwant\n%s", got, want)
	}
	if r.Worst() != ops.Red {
		t.Fatalf("Worst() = %v", r.Worst())
	}
	// Two runs of the same report are byte-identical: a runbook diffs them.
	first, second := r.Text(), r.Text()
	if first != second {
		t.Fatal("Text() is not stable")
	}
	if (ops.Report{}).Worst() != ops.Green {
		t.Fatal("an empty report is not green")
	}
}

func configWithUDPProbe() config.Config {
	c := config.Default()
	c.Instance.Domain = "dilla.test"
	c.Doctor.UDPProbe = true
	c.Derive()
	return *c
}

func TestTheUDPLegPrintsAnInstructionAndContactsNothing(t *testing.T) {
	var contacted atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { contacted.Add(1) }))
	defer srv.Close()
	leg := ops.UDPLeg(configWithUDPProbe())
	if contacted.Load() != 0 {
		t.Fatal("the UDP leg contacted a network service")
	}
	if leg.Status != ops.Yellow {
		t.Fatalf("status = %v, want Yellow (unknown until the operator checks)", leg.Status)
	}
	if !strings.Contains(leg.Fix, "7882/udp") || !strings.Contains(leg.Fix, "behind_proxy") {
		t.Fatalf("the leg does not tell the operator what to open: %q", leg.Fix)
	}
}

func TestTheUDPLegIsQuietWhenThereIsNoMediaPort(t *testing.T) {
	c := configWithUDPProbe()
	c.LiveKit.Enabled = false
	if leg := ops.UDPLeg(c); leg.Status != ops.Green {
		t.Fatalf("livekit disabled = %v", leg.Status)
	}
}

func TestBlobConsistencyReportsBothDirections(t *testing.T) {
	h := newOpsHarness(t)
	leg := ops.BlobConsistencyLeg(t.Context(), h.Repo, h.Blobs)
	if leg.Status != ops.Green {
		t.Fatalf("a consistent instance = %v (%s)", leg.Status, leg.Detail)
	}

	// A row with no file, and a file with no row.
	h.LoseOneBlob()
	writeStray(t, h, []byte("stray"))
	leg = ops.BlobConsistencyLeg(t.Context(), h.Repo, h.Blobs)
	if leg.Status != ops.Red {
		t.Fatalf("status = %v", leg.Status)
	}
	if !strings.Contains(leg.Detail, "1 referenced blob missing") ||
		!strings.Contains(leg.Detail, "1 file with no row") {
		t.Fatalf("detail = %q", leg.Detail)
	}
	if leg.Fix == "" {
		t.Fatal("a red blob leg must say how to recover")
	}
}

func TestAStrayFileAloneIsYellowAndHarmless(t *testing.T) {
	h := newOpsHarness(t)
	writeStray(t, h, []byte("older blob tree"))
	leg := ops.BlobConsistencyLeg(t.Context(), h.Repo, h.Blobs)
	if leg.Status != ops.Yellow || !strings.Contains(leg.Detail, "1 file with no row") {
		t.Fatalf("a file with no row = %v (%s)", leg.Status, leg.Detail)
	}
}

func TestAnUnreferencedRowWithNoFileIsNotAGap(t *testing.T) {
	h := newOpsHarness(t)
	h.LoseOneBlob()
	sum := h.blobIDs[0]
	if _, err := h.Repo.DeleteAllBlobRefs(t.Context(), sum); err != nil {
		t.Fatalf("DeleteAllBlobRefs: %v", err)
	}
	if err := h.Repo.MarkBlobUnreferenced(t.Context(), sum, h.Clock.Now().Unix()); err != nil {
		t.Fatalf("MarkBlobUnreferenced: %v", err)
	}
	leg := ops.BlobConsistencyLeg(t.Context(), h.Repo, h.Blobs)
	if leg.Status != ops.Green {
		t.Fatalf("garbage awaiting the sweeper = %v (%s)", leg.Status, leg.Detail)
	}
}

func writeStray(t *testing.T, h *opsHarness, payload []byte) {
	t.Helper()
	sum := sha256.Sum256(payload)
	if _, _, err := h.Blobs.Put(t.Context(), sum[:], strings.NewReader(string(payload)), 1<<20); err != nil {
		t.Fatalf("Put: %v", err)
	}
}

// certAt builds a certificate valid over [notBefore, notAfter], self-issued.
func certAt(t *testing.T, notBefore, notAfter time.Time) *tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "chat.example"},
		Issuer:    pkix.Name{CommonName: "Test CA"},
		NotBefore: notBefore, NotAfter: notAfter, DNSNames: []string{"chat.example"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("certificate: %v", err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func cfgHolding(c *tls.Certificate, err error) *tls.Config {
	return &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return c, err }}
}

func TestTheCertificateLegGrades(t *testing.T) {
	now := time.Now()
	day := 24 * time.Hour
	for _, tc := range []struct {
		name string
		cert *tls.Certificate
		err  error
		last string
		want ops.Status
		in   string
	}{
		{"fresh", certAt(t, now.Add(-10*day), now.Add(80*day+time.Hour)), nil, "", ops.Green, "expires in 80d"},
		{"inside the renewal window", certAt(t, now.Add(-70*day), now.Add(20*day)), nil, "", ops.Yellow, "renewal window"},
		{"a failed renewal keeps serving", certAt(t, now.Add(-70*day), now.Add(20*day)), nil, "acme: 500", ops.Yellow, "acme: 500"},
		{"a failed renewal on a fresh certificate", certAt(t, now.Add(-10*day), now.Add(80*day)), nil, "acme: 500", ops.Yellow, "serving the existing certificate"},
		{"expired", certAt(t, now.Add(-100*day), now.Add(-day)), nil, "", ops.Red, "expired"},
		{"none yet", nil, os.ErrNotExist, "", ops.Yellow, "no certificate held"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leg := ops.CertificateLeg(t.Context(), "chat.example", cfgHolding(tc.cert, tc.err), tc.last)
			if leg.Status != tc.want || !strings.Contains(leg.Detail, tc.in) {
				t.Fatalf("got %v %q, want %v containing %q", leg.Status, leg.Detail, tc.want, tc.in)
			}
			if tc.want != ops.Green && leg.Fix == "" {
				t.Fatal("a non-green certificate leg must name a fix")
			}
		})
	}
}

func TestTheCertificateIsReadFromCertmagicStorageWithoutContactingACME(t *testing.T) {
	dir := t.TempDir()
	c := certAt(t, time.Now().Add(-time.Hour), time.Now().Add(80*24*time.Hour))
	key, ok := c.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatal("not an ECDSA key")
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	site := filepath.Join(dir, "certificates", "acme-v02.api.letsencrypt.org-directory", "chat.example")
	if err := os.MkdirAll(site, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(site, "chat.example.crt"),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Certificate[0]}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(site, "chat.example.key"),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	leg := ops.CertificateLeg(t.Context(), "chat.example",
		&tls.Config{GetCertificate: ops.CertificateFromStorage(dir, "chat.example")}, "")
	if leg.Status != ops.Green {
		t.Fatalf("a stored certificate = %v (%s)", leg.Status, leg.Detail)
	}
	leg = ops.CertificateLeg(t.Context(), "other.example",
		&tls.Config{GetCertificate: ops.CertificateFromStorage(dir, "other.example")}, "")
	if leg.Status != ops.Yellow {
		t.Fatalf("no stored certificate = %v (%s)", leg.Status, leg.Detail)
	}
}

// startTURN runs a real pion TURN server on a loopback TCP listener, with the
// authentication dillad uses (time-windowed REST credentials over a shared secret).
func startTURN(t *testing.T, realm, secret string) string {
	t.Helper()
	return startTURNRelayingOn(t, realm, secret, "127.0.0.1")
}

func startTURNRelayingOn(t *testing.T, realm, secret, relayIP string) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv, err := turn.NewServer(turn.ServerConfig{
		Realm:       realm,
		AuthHandler: turn.LongTermTURNRESTAuthHandler(secret, nil),
		ListenerConfigs: []turn.ListenerConfig{{
			Listener: ln,
			RelayAddressGenerator: &turn.RelayAddressGeneratorStatic{
				RelayAddress: net.ParseIP(relayIP), Address: relayIP,
			},
		}},
	})
	if err != nil {
		t.Fatalf("turn.NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

func TestTheTURNLegAllocatesAgainstARealServer(t *testing.T) {
	addr := startTURN(t, "chat.example", "s3cret-shared-secret")
	leg := ops.TURNLeg(t.Context(), addr, "chat.example", "s3cret-shared-secret")
	if leg.Status != ops.Green || !strings.Contains(leg.Detail, "127.0.0.1:") {
		t.Fatalf("a working TURN server = %v (%s)", leg.Status, leg.Detail)
	}

	leg = ops.TURNLeg(t.Context(), addr, "chat.example", "a different secret")
	if leg.Status != ops.Red || leg.Fix == "" {
		t.Fatalf("a wrong shared secret = %v (%s) fix=%q", leg.Status, leg.Detail, leg.Fix)
	}
}

// I12 (fix wave): a relay_ip that is no address of this host (the public IP on bridged Docker or a
// NATed LXC) lets the server start and authenticate, and every Allocate then fails to bind its relay
// socket: pion answers 508. The leg's fix names turn.relay_ip, not the secret or the port.
func TestTheTURNLegNamesRelayIPWhenTheRelayCannotBind(t *testing.T) {
	addr := startTURNRelayingOn(t, "chat.example", "s3cret-shared-secret", "203.0.113.10")
	leg := ops.TURNLeg(t.Context(), addr, "chat.example", "s3cret-shared-secret")
	if leg.Status != ops.Red || !strings.Contains(leg.Fix, "turn.relay_ip") {
		t.Fatalf("a relay_ip the host cannot bind = %v (%s) fix=%q, want red with a fix naming turn.relay_ip",
			leg.Status, leg.Detail, leg.Fix)
	}
}

func TestTheTURNLegIsRedWhenNothingListens(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	leg := ops.TURNLeg(t.Context(), addr, "chat.example", "s")
	if leg.Status != ops.Red || leg.Fix == "" {
		t.Fatalf("a closed port = %v (%s)", leg.Status, leg.Detail)
	}
}

func TestReportSummarySumsTheChecksUp(t *testing.T) {
	r := ops.Report{Legs: []ops.Leg{
		{Name: "a", Status: ops.Green},
		{Name: "b", Status: ops.Yellow},
		{Name: "c", Status: ops.Red},
	}}
	if got, want := r.Summary(), "FAIL  summary      3 checks: 1 OK, 1 WARN, 1 FAIL\n"; got != want {
		t.Fatalf("Summary() = %q, want %q", got, want)
	}
	if got, want := (ops.Report{Legs: r.Legs[:1]}).Summary(), "OK    summary      1 checks: 1 OK, 0 WARN, 0 FAIL\n"; got != want {
		t.Fatalf("green Summary() = %q, want %q", got, want)
	}
	if got, want := (ops.Report{Legs: r.Legs[:2]}).Summary(), "WARN  summary      2 checks: 1 OK, 1 WARN, 0 FAIL\n"; got != want {
		t.Fatalf("yellow Summary() = %q, want %q", got, want)
	}
	if got, want := (ops.Report{}).Summary(), "OK    summary      0 checks: 0 OK, 0 WARN, 0 FAIL\n"; got != want {
		t.Fatalf("empty Summary() = %q, want %q", got, want)
	}
}
