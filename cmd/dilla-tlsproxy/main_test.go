package main

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"testing"
)

// The leaf verifies against the CA the proxy writes, for every host name and IP it was made for, and for no other.
func TestCertificatesChainToTheWrittenCA(t *testing.T) {
	cert, caPEM, err := certificates([]string{"dilla.test", "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("the CA PEM does not parse")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"dilla.test", "127.0.0.1"} {
		if _, err := leaf.Verify(x509.VerifyOptions{DNSName: name, Roots: roots}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "other.test", Roots: roots}); err == nil {
		t.Error("the leaf verified for a host it was not made for")
	}
	if _, _, err := certificates([]string{""}); err == nil {
		t.Error("no host names were accepted")
	}
}

// Bytes pass both ways through the TLS listener to the plain upstream.
func TestPipeCopiesBothWays(t *testing.T) {
	up, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = up.Close() }()
	go func() {
		c, err := up.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		buf := make([]byte, 4)
		if _, err := io.ReadFull(c, buf); err == nil {
			_, _ = c.Write(append([]byte("re:"), buf...))
		}
	}()
	cert, caPEM, err := certificates([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			pipe(c, up.Addr().String())
		}
	}()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(caPEM)
	d := &tls.Dialer{Config: &tls.Config{RootCAs: roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}}
	c, err := d.DialContext(t.Context(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 7)
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "re:ping" {
		t.Fatalf("got %q", got)
	}
}
