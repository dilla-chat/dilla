package server_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/server"
)

// The one certificate every TLS test in this package verifies against: an
// ECDSA P-256 leaf for localhost, generated once. Verification stays on — the
// tests trust exactly this certificate rather than skipping the chain check.
var (
	certOnce sync.Once
	certDER  []byte
	certKey  *ecdsa.PrivateKey
	certErr  error
)

func testCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	certOnce.Do(func() {
		certKey, certErr = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if certErr != nil {
			return
		}
		tmpl := &x509.Certificate{
			SerialNumber:          big.NewInt(1),
			Subject:               pkix.Name{CommonName: "localhost"},
			DNSNames:              []string{"localhost"},
			IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
			NotBefore:             time.Now().Add(-time.Hour),
			NotAfter:              time.Now().Add(24 * time.Hour),
			KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
			ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			BasicConstraintsValid: true,
			IsCA:                  true,
		}
		certDER, certErr = x509.CreateCertificate(rand.Reader, tmpl, tmpl, &certKey.PublicKey, certKey)
	})
	if certErr != nil {
		t.Fatalf("test certificate: %v", certErr)
	}
	return tls.Certificate{Certificate: [][]byte{certDER}, PrivateKey: certKey}
}

// selfSignedListener is a raw TCP listener on 127.0.0.1:0 and the server-side
// TLS config for the test certificate, offering h2 and http/1.1 as dillad's
// own config does after certmagic's acme-tls/1.
func selfSignedListener(t *testing.T) (net.Listener, *tls.Config) {
	t.Helper()
	cert := testCertificate(t)
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln, &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"h2", "http/1.1"},
		MinVersion:   tls.VersionTLS12,
	}
}

// testCertPool holds exactly the test certificate.
func testCertPool(t *testing.T) *x509.CertPool {
	t.Helper()
	testCertificate(t)
	c, err := x509.ParseCertificate(certDER)
	if err != nil {
		t.Fatalf("parse test certificate: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(c)
	return pool
}

func TestLooksLikeSTUN(t *testing.T) {
	// RFC 8489 §5: the top two bits MUST be zero "enabling differentiation from
	// other protocols during multiplexing", and the magic cookie 0x2112A442 sits
	// at bytes [4:8].
	stun := []byte{0x00, 0x01, 0x00, 0x00, 0x21, 0x12, 0xA4, 0x42}
	if !server.LooksLikeSTUN(stun) {
		t.Fatal("a STUN Binding header was not recognised")
	}
	for _, b := range [][]byte{
		[]byte("GET /v1/i"),
		[]byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")[:8],
		{0xC0, 0x01, 0x00, 0x00, 0x21, 0x12, 0xA4, 0x42}, // top bits set
		{0x00, 0x01, 0x00, 0x00, 0x21, 0x12, 0xA4, 0x43}, // wrong cookie
		{0x00, 0x01, 0x00},                               // too short
	} {
		if server.LooksLikeSTUN(b) {
			t.Fatalf("%x was taken for STUN", b)
		}
	}
}

func TestTheDemuxReplaysThePeekedBytesToHTTP(t *testing.T) {
	raw, tlsCfg := selfSignedListener(t)
	d := server.NewDemux(raw, tlsCfg, 10*time.Second)
	go d.Serve()
	defer d.Close()

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "path=%s proto=%s tls=%v", r.URL.Path, r.Proto, r.TLS != nil)
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go srv.Serve(d.HTTP())
	defer srv.Close()

	// testCertPool holds the certificate selfSignedListener serves, so the test
	// verifies the chain rather than switching verification off.
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: testCertPool(t), ServerName: "localhost",
	}}}
	resp, err := c.Do(getRequest(t, "https://"+raw.Addr().String()+"/v1/instance"))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	// The first eight bytes of "GET /v1/instance" were consumed by the peek and
	// must reach net/http anyway, and r.TLS must be populated — Go 1.27 reads
	// ConnectionState() off a user-provided net.Conn.
	if string(b) != "path=/v1/instance proto=HTTP/1.1 tls=true" {
		t.Fatalf("body = %q", b)
	}
}

// Go 1.27 serves h2 on a user-provided net.Conn that reports a negotiated "h2"
// through ConnectionState(); the demux's peekConn is exactly such a conn, so the
// peek costs neither HTTP/2 nor the TLS state.
func TestTheDemuxKeepsHTTP2(t *testing.T) {
	raw, tlsCfg := selfSignedListener(t)
	d := server.NewDemux(raw, tlsCfg, 10*time.Second)
	go d.Serve()
	defer d.Close()

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "proto=%s tls=%v", r.Proto, r.TLS != nil)
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go srv.Serve(d.HTTP())
	defer srv.Close()

	tr := &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: testCertPool(t), ServerName: "localhost"},
		ForceAttemptHTTP2: true,
	}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr}).Do(getRequest(t, "https://"+raw.Addr().String()+"/"))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "proto=HTTP/2.0 tls=true" {
		t.Fatalf("body = %q", b)
	}
}

func TestTheDemuxRoutesSTUNToTheTURNBranch(t *testing.T) {
	raw, tlsCfg := selfSignedListener(t)
	d := server.NewDemux(raw, tlsCfg, 10*time.Second)
	go d.Serve()
	defer d.Close()

	got := make(chan []byte, 1)
	go func() {
		c, err := d.TURN().Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 20)
		n, _ := io.ReadFull(c, buf)
		got <- buf[:n]
	}()

	conn, err := (&tls.Dialer{Config: &tls.Config{
		RootCAs: testCertPool(t), ServerName: "localhost",
	}}).DialContext(t.Context(), "tcp", raw.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	header := append([]byte{0x00, 0x01, 0x00, 0x00, 0x21, 0x12, 0xA4, 0x42}, bytes.Repeat([]byte{7}, 12)...)
	if _, err := conn.Write(header); err != nil {
		t.Fatalf("Write: %v", err)
	}
	select {
	case b := <-got:
		// The peeked eight bytes MUST be replayed: pion reads the STUN header
		// itself through turn.NewSTUNConn.
		if !bytes.Equal(b, header) {
			t.Fatalf("the TURN branch saw %x, want %x", b, header)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the STUN stream never reached the TURN listener")
	}
}

// A client that completes the handshake and then says nothing — connection
// pre-warming, a speculative browser connection — is an HTTPS client until
// proven otherwise: STUN clients always send first. The connection goes to the
// HTTP branch after the peek deadline, with the deadline cleared, and still
// serves a request afterwards.
func TestADemuxConnectionThatSendsNothingGoesToHTTP(t *testing.T) {
	raw, tlsCfg := selfSignedListener(t)
	const peek = 300 * time.Millisecond
	d := server.NewDemux(raw, tlsCfg, peek)
	go d.Serve()
	defer d.Close()

	conn, err := (&tls.Dialer{Config: &tls.Config{
		RootCAs: testCertPool(t), ServerName: "localhost", NextProtos: []string{"http/1.1"},
	}}).DialContext(t.Context(), "tcp", raw.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	start := time.Now()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := d.HTTP().Accept()
		if err != nil {
			return
		}
		accepted <- c
	}()
	var sc net.Conn
	select {
	case sc = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("a silent connection never reached the HTTP listener")
	}
	defer sc.Close()
	if waited := time.Since(start); waited < peek/2 {
		t.Fatalf("the HTTP branch took the connection after %s, before the %s peek window ran out", waited, peek)
	}

	// The deadline the peek set must be gone: a read that outlives it works.
	time.Sleep(peek)
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: localhost\r\n\r\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	buf := make([]byte, len("GET / HTTP/1.1"))
	if _, err := io.ReadFull(sc, buf); err != nil {
		t.Fatalf("the server side could not read the late request: %v", err)
	}
	if string(buf) != "GET / HTTP/1.1" {
		t.Fatalf("server read %q", buf)
	}
}

// Close unwinds both branches: Accept on either returns net.ErrClosed, which is
// what makes http.Server.Serve and pion's turn.Server stop.
func TestClosingTheDemuxUnblocksBothBranches(t *testing.T) {
	raw, tlsCfg := selfSignedListener(t)
	d := server.NewDemux(raw, tlsCfg, time.Second)
	served := make(chan struct{})
	go func() {
		d.Serve()
		close(served)
	}()
	errs := make(chan error, 2)
	for _, ln := range []net.Listener{d.HTTP(), d.TURN()} {
		go func() {
			_, err := ln.Accept()
			errs <- err
		}()
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for range 2 {
		select {
		case err := <-errs:
			if !errors.Is(err, net.ErrClosed) {
				t.Fatalf("Accept after Close = %v, want net.ErrClosed", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Accept did not return after Close")
		}
	}
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after Close")
	}
}

func getRequest(t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return req
}
