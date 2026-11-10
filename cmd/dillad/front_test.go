package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"
	lkprom "github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	lkauth "github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"
	"github.com/pion/turn/v5"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/exit"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/obs"
	"github.com/dilla-chat/dilla/internal/ops"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/sfu"
)

// rewriteConfig loads cfgPath, applies tune and writes it back.
func rewriteConfig(t *testing.T, cfgPath string, tune func(*config.Config)) {
	t.Helper()
	c, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	tune(c)
	f, err := os.OpenFile(cfgPath, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("reopen dilla.toml: %v", err)
	}
	if err := c.WriteConfig(f); err != nil {
		f.Close()
		t.Fatalf("rewrite dilla.toml: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close dilla.toml: %v", err)
	}
	if _, err := config.Load(cfgPath); err != nil {
		t.Fatalf("the rewritten dilla.toml does not load: %v", err)
	}
}

// freePort is a TCP port on 127.0.0.1 nothing listens on right now.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// behind_proxy with turn.enabled: the API on server.plain_listen, and the TURN
// relay on its own operator-configured TCP port — never 443, where the proxy
// routes only HTTP. `dillad doctor`'s own TURN leg allocates against it.
func TestServeBehindAProxyRunsTURNOnItsOwnPort(t *testing.T) {
	cfgPath := bootstrapServeConfig(t)
	turnAddr := freePort(t)
	var secretFile, realm string
	rewriteConfig(t, cfgPath, func(c *config.Config) {
		c.TURN.Enabled = true
		c.TURN.Listen = turnAddr
		c.TURN.RelayIP = "127.0.0.1"
		secretFile, realm = c.TURN.SharedSecretFile, c.TURN.Realm
	})

	var stdout, stderr syncBuffer
	served := make(chan error, 1)
	go func() { served <- dispatch([]string{"serve", "--config=" + cfgPath}, &stdout, &stderr) }()
	addr := waitForListenAddr(t, &stdout, served)
	if err := probe(addr); err != nil {
		t.Fatalf("probe /healthz: %v", err)
	}
	secret, err := ops.ReadSecret(secretFile)
	if err != nil {
		t.Fatalf("read the TURN secret: %v", err)
	}
	leg := ops.TURNLeg(t.Context(), turnAddr, realm, secret)
	if leg.Status != ops.Green {
		t.Fatalf("TURN leg against turn.listen = %+v; stderr: %s", leg, stderr.String())
	}
	stopServe(t, served)
}

// The direct-TLS front end to end: server.listen through the 443 demux with
// certmagic's certificate (loaded from tls.storage_dir, so no ACME server is
// contacted), HTTP answered over TLS once the tls gate is green, and TURN over
// TLS on the same port.
func TestServeInACMEModeServesHTTPAndTURNOnOnePort(t *testing.T) {
	cfgPath := bootstrapServeConfig(t)
	// An ACME directory that answers 500 to everything: the certificate below
	// is already in storage and far from its renewal window, so nothing may
	// need the CA; if anything tried, it would fail rather than reach the
	// Internet.
	ca := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	defer ca.Close()

	var domain, secretFile, realm string
	var pool *x509.CertPool
	rewriteConfig(t, cfgPath, func(c *config.Config) {
		c.TLS.Mode = config.TLSModeACMETLSALPN
		c.TLS.CA = ca.URL + "/directory"
		c.Server.Listen = "127.0.0.1:0"
		c.Server.TrustedProxyCIDRs = nil
		c.TURN.Enabled = true
		c.TURN.Listen = ""
		c.TURN.RelayIP = "127.0.0.1"
		domain, secretFile, realm = c.Instance.Domain, c.TURN.SharedSecretFile, c.TURN.Realm
		pool = seedCertificate(t, c.TLS.StorageDir, c.TLS.CA, domain)
	})

	var stdout, stderr syncBuffer
	served := make(chan error, 1)
	go func() { served <- dispatch([]string{"serve", "--config=" + cfgPath}, &stdout, &stderr) }()
	addr := waitForListenAddr(t, &stdout, served)

	tlsCfg := &tls.Config{RootCAs: pool, ServerName: domain, MinVersion: tls.VersionTLS12}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg}}
	deadline := time.Now().Add(20 * time.Second)
	for {
		code, body, err := get(client, "https://"+addr+"/readyz")
		if err == nil && code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("/readyz over TLS never went green: %d %s %v; stderr: %s", code, body, err, stderr.String())
		}
		time.Sleep(100 * time.Millisecond)
	}

	secret, err := ops.ReadSecret(secretFile)
	if err != nil {
		t.Fatalf("read the TURN secret: %v", err)
	}
	conn, err := (&tls.Dialer{Config: tlsCfg}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial TURN over TLS: %v", err)
	}
	user, pass := server.TURNCredential(secret, id.New(), time.Hour, time.Now())
	tc, err := turn.NewClient(&turn.ClientConfig{
		TURNServerAddr: addr, Username: user, Password: pass, Realm: realm,
		Conn: turn.NewSTUNConn(conn), RTO: time.Second,
	})
	if err != nil {
		t.Fatalf("turn.NewClient: %v", err)
	}
	defer tc.Close()
	if err := tc.Listen(); err != nil {
		t.Fatalf("turn Listen: %v", err)
	}
	relay, err := tc.Allocate()
	if err != nil {
		t.Fatalf("a TURN allocation through the 443 demux failed: %v", err)
	}
	relay.Close()
	stopServe(t, served)
}

func get(c *http.Client, url string) (int, string, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	res, err := c.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b), nil
}

// seedCertificate writes a self-signed certificate for domain where certmagic
// v0.25.4's FileStorage keeps an issued one (StorageKeys.SiteCert,
// SitePrivateKey and SiteMeta under the CA's issuer key), and returns a pool
// holding it.
func seedCertificate(t *testing.T, storageDir, ca, domain string) *x509.CertPool {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: domain},
		DNSNames:     []string{domain},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:         true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalECPrivateKey: %v", err)
	}
	issuerKey := (&certmagic.ACMEIssuer{CA: ca}).IssuerKey()
	write := func(k string, body []byte) {
		p := filepath.Join(storageDir, filepath.FromSlash(k))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, body, 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	write(certmagic.StorageKeys.SiteCert(issuerKey, domain), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	write(certmagic.StorageKeys.SitePrivateKey(issuerKey, domain), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	write(certmagic.StorageKeys.SiteMeta(issuerKey, domain), fmt.Appendf(nil, `{"sans":[%q],"issuer_data":null}`, domain))
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return pool
}

// freeUDPPort is a UDP port on 127.0.0.1 nothing is bound to right now.
func freeUDPPort(t *testing.T) int {
	t.Helper()
	pc, err := (&net.ListenConfig{}).ListenPacket(t.Context(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	pc.Close()
	return port
}

// livekit.enabled starts LiveKit in this process through internal/sfu, and
// the livekit readiness gate is green only once it accepts connections; serve
// stops it on the way out.
func TestServeStartsTheSFU(t *testing.T) {
	t.Setenv(metricsTokenEnv, "dilla-media-task-8-metrics-probe")
	cfgPath := bootstrapServeConfig(t)
	port := freeTCPPort(t)
	rewriteConfig(t, cfgPath, func(c *config.Config) {
		c.LiveKit.Enabled = true
		c.LiveKit.BindAddress = "127.0.0.1"
		c.LiveKit.NodeIP = "127.0.0.1"
		c.LiveKit.Port = port
		c.LiveKit.UDPPort = freeUDPPort(t)
	})

	var stdout, stderr syncBuffer
	served := make(chan error, 1)
	go func() { served <- dispatch([]string{"serve", "--config=" + cfgPath}, &stdout, &stderr) }()
	addr := waitForListenAddr(t, &stdout, served)
	client := &http.Client{Timeout: 5 * time.Second}
	code, body, err := get(client, "http://"+addr+"/readyz")
	if err != nil || code != http.StatusOK {
		t.Fatalf("/readyz = %d %s %v", code, body, err)
	}
	if code, _, err := get(client, fmt.Sprintf("http://127.0.0.1:%d/", port)); err != nil || code != http.StatusOK {
		t.Fatalf("the SFU on livekit.port answered %d, %v", code, err)
	}
	// Plan 2 is served by the binary (task 19): an authenticated HTTP route (the community routes are socket-only) is behind the session
	// middleware, not missing, and LiveKit's signalling path answers on the instance's own
	// listener, proxied to the SFU — LiveKit's own refusal of a validate with no token, not the
	// mux's 404.
	if code, body, err := get(client, "http://"+addr+"/v1/auth/factors"); err != nil ||
		code != http.StatusUnauthorized {
		t.Fatalf("GET /v1/auth/factors through serve = %d %q %v, want 401", code, body, err)
	}
	if code, body, err := get(client, "http://"+addr+"/rtc/validate"); err != nil || code == http.StatusNotFound {
		t.Fatalf("GET /rtc/validate through serve = %d %q %v, want LiveKit's answer", code, body, err)
	}
	// A join with a client-chosen protocol number, as LiveKit records it: serve's /metrics merge must
	// not carry the label (branch review SFU-1).
	lkprom.RecordSessionJoinLatency(4242, time.Millisecond)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer dilla-media-task-8-metrics-probe")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	metricsBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Contains(metricsBody, []byte("# TYPE livekit_room_total ")) || !bytes.Contains(metricsBody, []byte("# TYPE dilla_gateway_connections ")) {
		t.Fatalf("/metrics = %d; want livekit_room_total and dilla_gateway_connections in one scrape", resp.StatusCode)
	}
	if !bytes.Contains(metricsBody, []byte("livekit_session_join_latency_ms_count{")) || bytes.Contains(metricsBody, []byte("protocol_version")) {
		t.Fatalf("/metrics serves the join latency without the client's protocol_version: %t, %t",
			bytes.Contains(metricsBody, []byte("livekit_session_join_latency_ms_count{")), !bytes.Contains(metricsBody, []byte("protocol_version")))
	}
	stopServe(t, served)
	if _, _, err := get(&http.Client{Timeout: time.Second}, fmt.Sprintf("http://127.0.0.1:%d/", port)); err == nil {
		t.Fatal("the SFU is still listening after serve returned")
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	_, p, err := net.SplitHostPort(freePort(t))
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	return port
}

// turn.proxy_protocol: a trusted proxy's PROXY header names the client; a peer
// outside server.trusted_proxy_cidrs is refused rather than allowed to name
// any address it likes.
func TestTheTURNListenerTakesPROXYHeadersOnlyFromTrustedProxies(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trusted string
		want    string // the remote address Accept reports, or "" for a refusal
	}{
		{"trusted", "127.0.0.1/32", "198.51.100.7:4242"},
		{"untrusted", "10.0.0.0/8", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			ln := proxyProtocolListener(raw, []netip.Prefix{netip.MustParsePrefix(tc.trusted)})
			defer ln.Close()
			got := make(chan string, 1)
			go func() {
				c, err := ln.Accept()
				if err != nil {
					got <- ""
					return
				}
				defer c.Close()
				buf := make([]byte, 4)
				if _, err := io.ReadFull(c, buf); err != nil {
					got <- ""
					return
				}
				got <- c.RemoteAddr().String()
			}()
			c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", raw.Addr().String())
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer c.Close()
			fmt.Fprintf(c, "PROXY TCP4 198.51.100.7 203.0.113.1 4242 443\r\nstun")
			select {
			case addr := <-got:
				if addr != tc.want {
					t.Fatalf("accepted from %q, want %q", addr, tc.want)
				}
			case <-time.After(5 * time.Second):
				if tc.want != "" {
					t.Fatal("the connection was never accepted")
				}
			}
		})
	}
}

func TestServeRefusesAnUnknownLiveKitMode(t *testing.T) {
	cfgPath := bootstrapServeConfig(t)
	c, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	c.LiveKit.Enabled = true
	c.LiveKit.Mode = "external"
	f, err := os.Create(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.WriteConfig(f); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	_, _, err = run(t, "serve", "--config="+cfgPath)
	if err == nil || !strings.Contains(err.Error(), "livekit.mode") {
		t.Fatalf("serve with livekit.mode = external: %v", err)
	}
}

// G34 / DEV-55: the relay admits livekit.node_ip only when it is an address of this host, or when
// advertise_internal_ip is false (the SFU's only candidate; the relay then hairpins through it, and
// says so); with advertise_internal_ip it admits the host addresses LiveKit also offers. node_ip is
// the relay family anchor either way. With LiveKit off there is no peer and no anchor.
func TestTheRelayPeersAreTheSFUsAddresses(t *testing.T) {
	ifaces := fakeIfaces("10.0.0.5/24", "127.0.0.1/8", "fe80::1/64", "2001:db8::5/64")
	var logged strings.Builder
	log := slog.New(slog.NewTextHandler(&logged, nil))
	for _, tc := range []struct {
		name      string
		nodeIP    string
		advertise bool
		anchor    string
		peers     string
		warning   string
	}{
		{"a local node_ip with the host candidates", "10.0.0.5", true, "10.0.0.5", "[10.0.0.5 2001:db8::5]", ""},
		{"a public node_ip with the host candidates", "203.0.113.7", true, "203.0.113.7", "[10.0.0.5 2001:db8::5]",
			"livekit.node_ip is not an address of this host, so the relay does not admit it"},
		{"a public node_ip as the only candidate", "203.0.113.7", false, "203.0.113.7", "[203.0.113.7]",
			"relayed media hairpins out through it"},
		{"no node_ip (loopback)", "", false, "127.0.0.1", "[127.0.0.1]", ""},
		{"loopback with the host candidates", "", true, "127.0.0.1", "[127.0.0.1 10.0.0.5 2001:db8::5]", ""},
	} {
		logged.Reset()
		cfg := config.Default()
		cfg.LiveKit.NodeIP, cfg.LiveKit.AdvertiseInternalIP = tc.nodeIP, tc.advertise
		anchor, peers, err := turnPeers(cfg, ifaces, log)
		if err != nil {
			t.Fatalf("%s: turnPeers: %v", tc.name, err)
		}
		if anchor.String() != tc.anchor || fmt.Sprint(peers.Addrs) != tc.peers {
			t.Errorf("%s: anchor %s, peers %v; want %s, %s", tc.name, anchor, peers, tc.anchor, tc.peers)
		}
		if tc.warning == "" && strings.Contains(logged.String(), "level=WARN") {
			t.Errorf("%s: unexpected warning %q", tc.name, logged.String())
		}
		if tc.warning != "" && !strings.Contains(logged.String(), tc.warning) {
			t.Errorf("%s: the warning %q was not logged (%q)", tc.name, tc.warning, logged.String())
		}
	}
	cfg := config.Default()
	cfg.LiveKit.Enabled = false
	if anchor, peers, err := turnPeers(cfg, ifaces, log); err != nil || anchor.IsValid() || len(peers.Addrs) != 0 {
		t.Errorf("with LiveKit off = %s, %v, %v; want no anchor, no peers", anchor, peers, err)
	}
}

// C8 (fix wave): a relay that fails on dilla.toml is exit 78, which systemd does not restart; one
// that fails to set up its network is exit 69, which it retries, and the message does not blame
// the config.
func TestATURNNetworkFailureIsUnavailableNotConfig(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.TURN.SharedSecretFile = filepath.Join(t.TempDir(), "missing")
	cfg.TURN.RelayIP = "127.0.0.1"
	d := frontDeps{cfg: cfg, log: slog.New(slog.DiscardHandler)}
	if err := startTURN(d, &front{}, ln); !errors.Is(err, exit.Config) {
		t.Errorf("a missing turn secret = %v, want exit.Config", err)
	}
	if err := turnStartError(errors.New("turn: failed to create network: netlinkrib")); !errors.Is(err, exit.Unavailable) || errors.Is(err, exit.Config) {
		t.Errorf("a network setup failure = %v, want exit.Unavailable only", err)
	}
}

// I13 (fix wave): every livekit.* key dilla.toml validates reaches the SFU's config, not only the
// ports and the key pair.
func TestTheSFUConfigCarriesTheLiveKitKeys(t *testing.T) {
	lk := config.Default().LiveKit
	lk.NodeIP = "203.0.113.7"
	lk.STUNServers = []string{"chat.example:3478"}
	lk.MaxVoiceParticipants = 12
	got := sfuConfig(lk, "  secret\n")
	if got.NodeIP != "203.0.113.7" || got.EnableLoopbackCandidate {
		t.Errorf("node ip %q, loopback candidate %t; want the public ip without the loopback candidate",
			got.NodeIP, got.EnableLoopbackCandidate)
	}
	if !got.AdvertiseInternalIP {
		t.Error("advertise_internal_ip (default true) did not reach the SFU config")
	}
	if len(got.STUNServers) != 1 || got.STUNServers[0] != "chat.example:3478" {
		t.Errorf("stun servers = %v, want [chat.example:3478]", got.STUNServers)
	}
	if got.MaxParticipants != 12 {
		t.Errorf("max participants = %d, want livekit.max_voice_participants 12", got.MaxParticipants)
	}
	if got.APISecret != "secret" || got.APIKey != lk.APIKey {
		t.Errorf("key pair = %q/%q", got.APIKey, got.APISecret)
	}
	if got.AutoCreate {
		t.Error("sfuConfig renders room.auto_create: true")
	}
	lk.NodeIP, lk.AdvertiseInternalIP = "", false
	got = sfuConfig(lk, "secret")
	if got.NodeIP != "127.0.0.1" || !got.EnableLoopbackCandidate || got.AdvertiseInternalIP {
		t.Errorf("unset node_ip: %+v, want 127.0.0.1 with the loopback candidate and no internal ip", got)
	}
	lk = config.Default().LiveKit
	lk.VP9 = true
	lk.WebhookListen = "127.0.0.1:7883"
	lk.LimitNumTracks, lk.LimitBytesPerSec = 4000, 125_000_000
	lk.IPsExcludes = []string{"172.17.0.0/16"}
	got = sfuConfig(lk, "secret")
	if !got.VP9 || got.WebhookURL != "http://127.0.0.1:7883/livekit/webhook" {
		t.Errorf("vp9 %t, webhook %q", got.VP9, got.WebhookURL)
	}
	if got.LimitNumTracks != 4000 || got.LimitBytesPerSec != 125_000_000 || len(got.IPsExcludes) != 1 {
		t.Errorf("limits %d/%v, excludes %v", got.LimitNumTracks, got.LimitBytesPerSec, got.IPsExcludes)
	}
	if got.AutoCreate || got.EmptyTimeout != 300 || got.DepartureTimeout != 20 {
		t.Errorf("room: auto_create %t, timeouts %d/%d; want false (task 10, MD-16), 300/20",
			got.AutoCreate, got.EmptyTimeout, got.DepartureTimeout)
	}
	lk.Enabled, lk.WebhookListen = true, ""
	if got = sfuConfig(lk, "secret"); got.WebhookURL != "" {
		t.Errorf("an empty webhook_listen rendered webhook %q", got.WebhookURL)
	}
}

// DEV-42 (a): a post-boot SFU exit turns the livekit gate red and is handed to serve, which exits
// exit.Unavailable for systemd to restart dillad; an ending serve stops the watcher quietly.
func TestAnSFUExitTurnsTheGateRedAndEndsServe(t *testing.T) {
	health := obs.NewHealth(clock.System())
	gate := health.Gate("livekit")
	gate.Set(true, "in-process SFU on ws://127.0.0.1:7880")
	done := make(chan error, 1)
	died := watchSFU(t.Context(), done, gate, slog.New(slog.DiscardHandler))
	done <- errors.New("http: Server closed")
	select {
	case err, ok := <-died:
		if !ok || err == nil {
			t.Fatalf("watchSFU delivered %v, %t", err, ok)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watchSFU did not report the exit")
	}
	rec := httptest.NewRecorder()
	health.Readiness().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))
	if rec.Code == http.StatusOK || !strings.Contains(rec.Body.String(), "livekit") {
		t.Fatalf("/readyz = %d %s, want the livekit gate red", rec.Code, rec.Body.String())
	}

	ctx, cancel := context.WithCancel(t.Context())
	quiet := watchSFU(ctx, make(chan error), obs.NewHealth(clock.System()).Gate("livekit"), slog.New(slog.DiscardHandler))
	cancel()
	select {
	case err, ok := <-quiet:
		if ok {
			t.Fatalf("a cancelled watcher delivered %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled watcher did not close its channel")
	}
}

// The webhook listener binds livekit.webhook_listen (writing a :0 port back for sfuConfig), serves
// only POST /livekit/webhook, and hands a verified event to the sink; an unsigned one is 401.
func TestTheWebhookListenerHandsVerifiedEventsToTheSink(t *testing.T) {
	const secret = "dilla-webhook-secret-0123456789abcdef"
	secretFile := filepath.Join(t.TempDir(), "livekit.secret")
	if err := os.WriteFile(secretFile, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.LiveKit.Enabled = true
	cfg.LiveKit.WebhookListen = "127.0.0.1:0"
	cfg.LiveKit.APIKey = "dilla"
	cfg.LiveKit.APISecretFile = secretFile
	d := frontDeps{cfg: cfg, log: slog.New(slog.DiscardHandler), stdout: io.Discard}
	ln, err := listenWebhook(t.Context(), d)
	if err != nil {
		t.Fatalf("listenWebhook: %v", err)
	}
	if cfg.LiveKit.WebhookListen == "127.0.0.1:0" {
		t.Fatal("the bound address was not written back into livekit.webhook_listen")
	}
	if got := sfuConfig(cfg.LiveKit, secret).WebhookURL; got != "http://"+cfg.LiveKit.WebhookListen+sfu.WebhookPath {
		t.Fatalf("LiveKit's webhook URL = %q, want the bound port", got)
	}
	got := make(chan *livekit.WebhookEvent, 1)
	stop, err := serveWebhook(d, ln, func(_ context.Context, ev *livekit.WebhookEvent) { got <- ev })
	if err != nil {
		t.Fatalf("serveWebhook: %v", err)
	}
	defer stop()

	target := "http://" + cfg.LiveKit.WebhookListen + sfu.WebhookPath
	post := func(sign bool) int {
		body, _ := protojson.Marshal(&livekit.WebhookEvent{Event: webhook.EventRoomFinished, Id: "EV_1",
			Room: &livekit.Room{Name: "room-1"}})
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, target, bytes.NewReader(body))
		req.Header.Set("Content-Type", webhook.ContentType)
		if sign {
			sum := sha256.Sum256(body)
			tok, _ := lkauth.NewAccessToken("dilla", secret).SetValidFor(5 * time.Minute).
				SetSha256(base64.StdEncoding.EncodeToString(sum[:])).ToJWT()
			req.Header.Set("Authorization", tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if status := post(false); status != http.StatusUnauthorized {
		t.Fatalf("an unsigned webhook = %d, want 401", status)
	}
	if status := post(true); status != http.StatusOK {
		t.Fatalf("a signed webhook = %d, want 200", status)
	}
	select {
	case ev := <-got:
		if ev.GetEvent() != webhook.EventRoomFinished || ev.GetRoom().GetName() != "room-1" {
			t.Fatalf("the sink got %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the sink never got the event")
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET %s = %d, want 405", sfu.WebhookPath, resp.StatusCode)
	}

	cfg.LiveKit.Enabled = false
	if ln, err := listenWebhook(t.Context(), d); ln != nil || err != nil {
		t.Fatalf("with LiveKit off listenWebhook = %v, %v; want nothing", ln, err)
	}
}
