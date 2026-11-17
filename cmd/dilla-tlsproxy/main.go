// Command dilla-tlsproxy puts HTTPS in front of a local dilla-testhost for the packaged macOS app's UI checks
// (plan 2026-11-16-gpui-macos-ci, Task 5): a release build takes https instances only (no --allow-loopback), so the
// check trusts this proxy's throwaway CA in the runner's System keychain and connects to https://localhost:<port>/.
//
// It makes a new CA and a leaf certificate for the given hosts at every start (nothing is kept but the CA's PEM, at
// -ca-out), terminates TLS and copies the bytes to -to, so HTTP and WebSocket upgrades pass unchanged. Test use only:
// never built into a release.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:9443", "the TLS listener")
	to := flag.String("to", "127.0.0.1:8443", "the plain upstream (dilla-testhost's public listener)")
	caOut := flag.String("ca-out", "", "where to write the CA certificate (PEM), to be trusted by the caller")
	hosts := flag.String("hosts", "localhost,127.0.0.1", "the leaf certificate's names, comma-separated")
	flag.Parse()
	if *caOut == "" {
		fmt.Fprintln(os.Stderr, "dilla-tlsproxy: -ca-out is required")
		os.Exit(2)
	}
	cert, caPEM, err := certificates(strings.Split(*hosts, ","))
	if err != nil {
		fmt.Fprintln(os.Stderr, "dilla-tlsproxy:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*caOut, caPEM, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "dilla-tlsproxy:", err)
		os.Exit(1)
	}
	ln, err := tls.Listen("tcp", *listen, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		fmt.Fprintln(os.Stderr, "dilla-tlsproxy:", err)
		os.Exit(1)
	}
	fmt.Printf("https://%s -> %s (CA %s)\n", ln.Addr(), *to, *caOut)
	for {
		c, err := ln.Accept()
		if err != nil {
			fmt.Fprintln(os.Stderr, "dilla-tlsproxy:", err)
			os.Exit(1)
		}
		go pipe(c, *to)
	}
}

// pipe copies one connection to and from the upstream until either side closes.
func pipe(c net.Conn, to string) {
	defer func() { _ = c.Close() }()
	up, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(context.Background(), "tcp", to)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dilla-tlsproxy: upstream:", err)
		return
	}
	defer func() { _ = up.Close() }()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(up, c)
		if tc, ok := up.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()
	go func() { defer wg.Done(); _, _ = io.Copy(c, up); _ = c.Close() }()
	wg.Wait()
}

// certificates makes a CA and a leaf for hosts signed by it, valid for a day.
func certificates(hosts []string) (tls.Certificate, []byte, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	now := time.Now()
	ca := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "dilla-tlsproxy test CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: hosts[0]},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			leaf.IPAddresses = append(leaf.IPAddresses, ip)
		} else if h != "" {
			leaf.DNSNames = append(leaf.DNSNames, h)
		}
	}
	if len(leaf.DNSNames)+len(leaf.IPAddresses) == 0 {
		return tls.Certificate{}, nil, errors.New("no host names")
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	return tls.Certificate{Certificate: [][]byte{leafDER, caDER}, PrivateKey: leafKey}, caPEM, nil
}
