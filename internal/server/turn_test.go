package server_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/stun/v3"
	"github.com/pion/turn/v5"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
)

func TestTURNCredentialIsHMACOverExpiryColonDevice(t *testing.T) {
	dev := id.New()
	now := time.Unix(1_790_000_000, 0)
	user, pass := server.TURNCredential("s3cret", dev, time.Hour, now)
	// "<expiry_s>:<device_id>:<issued_ms>": the issue time is carried (review M6), in milliseconds
	// (re-review N1).
	wantUser := fmt.Sprintf("%d:%s:%d", now.Add(time.Hour).Unix(), dev.String(), now.UnixMilli())
	if user != wantUser {
		t.Fatalf("username = %q, want %q", user, wantUser)
	}
	mac := hmac.New(sha1.New, []byte("s3cret"))
	mac.Write([]byte(wantUser))
	if want := base64.StdEncoding.EncodeToString(mac.Sum(nil)); pass != want {
		t.Fatalf("password = %q, want %q", pass, want)
	}
	// dillad's auth handler checks the HMAC and the "<expiry>:<device>:<issued>" shape on every
	// request, and the expiry on Allocate only (G33).
	clk := clock.NewFake(now)
	auth := server.TURNAuthForTest("s3cret", clk, 2*time.Hour, nil)
	gotDev, key, ok := auth(&turn.RequestAttributes{Username: user, Realm: "chat.example.test", Method: stun.MethodAllocate})
	if !ok {
		t.Fatal("dillad refused a credential it minted")
	}
	if gotDev != dev.String() {
		t.Fatalf("user id = %q, want the device id %s", gotDev, dev)
	}
	if want := turn.GenerateAuthKey(user, "chat.example.test", pass); string(key) != string(want) {
		t.Fatal("the long-term key is not MD5(username:realm:password) over the minted password")
	}
	old, _ := server.TURNCredential("s3cret", dev, -time.Hour, now)
	if _, _, ok := auth(&turn.RequestAttributes{Username: old, Method: stun.MethodAllocate}); ok {
		t.Fatal("an expired credential allocated")
	}
	for _, bad := range []string{
		"not-a-number:" + dev.String() + fmt.Sprintf(":%d", now.Unix()),
		fmt.Sprintf("%d:%s", now.Add(time.Hour).Unix(), dev),                                 // no issue time
		fmt.Sprintf("%d:%s:x", now.Add(time.Hour).Unix(), dev),                               // an issue time that is no number
		fmt.Sprintf("%d:%s:%d", now.Add(time.Hour).Unix(), dev, now.Add(2*time.Hour).Unix()), // issued after it expires
	} {
		if _, _, ok := auth(&turn.RequestAttributes{Username: bad, Method: stun.MethodRefresh}); ok {
			t.Fatalf("the malformed username %q was accepted", bad)
		}
	}
	// And pion's own handler, on the wall clock, accepts the same shape: the
	// format is pion's, not a dilla dialect of it.
	fresh, _ := server.TURNCredential("s3cret", dev, time.Hour, time.Now())
	if _, _, ok := turn.LongTermTURNRESTAuthHandler("s3cret", nil)(&turn.RequestAttributes{Username: fresh}); !ok {
		t.Fatal("pion refused a credential dillad minted")
	}
}

func TestAtMostTwoAllocationsPerDevice(t *testing.T) {
	q := server.NewAllocationQuota(2)
	dev := id.New().String()
	for i := range 2 {
		if !q.Allow(dev) {
			t.Fatalf("allocation %d was refused", i)
		}
	}
	if q.Allow(dev) {
		t.Fatal("a third allocation was allowed")
	}
	if !q.Allow(id.New().String()) {
		t.Fatal("another device was refused")
	}
	q.Release(dev)
	if !q.Allow(dev) {
		t.Fatal("a slot did not free after Release")
	}
}

// pion v5.0.13 hands both handlers the user id the auth handler returned —
// the device id — not the REST username (internal/server/turn.go:212 and
// allocation_manager.go:279-281). The wired handlers take either spelling, and
// the release half is wired too: without it a device would be capped for the
// life of the process after allocations_per_device calls.
func TestTheQuotaHandlerParsesTheRESTUsername(t *testing.T) {
	dev := id.New().String()
	for _, name := range []string{dev, fmt.Sprintf("%d:%s", time.Now().Add(time.Hour).Unix(), dev),
		fmt.Sprintf("%d:%s:%d", time.Now().Add(time.Hour).Unix(), dev, time.Now().Unix())} {
		quota, events := server.TURNHandlersForTest(2, nil)
		for i := range 2 {
			if !quota(name, "chat.example.test", nil) {
				t.Fatalf("%q: allocation %d was refused", name, i)
			}
		}
		if quota(name, "chat.example.test", nil) {
			t.Fatalf("%q: a third allocation was allowed", name)
		}
		events.OnAllocationDeleted(nil, nil, "tcp", name, "chat.example.test")
		if !quota(name, "chat.example.test", nil) {
			t.Fatalf("%q: closing an allocation did not free its slot", name)
		}
	}
}

// End to end over the 443 demux: a TURN client speaks STUN inside TLS, the
// demux hands the stream to pion, and the quota holds per device across
// separate connections until an allocation is released.
func TestTURNAllocatesThroughTheDemuxAndHoldsTheQuota(t *testing.T) {
	raw, tlsCfg := selfSignedListener(t)
	d := server.NewDemux(raw, tlsCfg, 10*time.Second)
	go d.Serve()
	defer d.Close()

	secret := "0123456789abcdef0123456789abcdef"
	c := config.TURN{
		Enabled: true, Realm: "chat.example.test", RelayIP: "127.0.0.1",
		SharedSecretFile: writeFile(t, "turn.secret", secret+"\n"),
		CredentialTTL:    "1h", AllocationsPerDevice: 2,
	}
	srv, err := server.StartTURN(c, d.TURN(), netip.Addr{}, server.TURNPeers{}, nil, nil, clock.System(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("StartTURN: %v", err)
	}
	defer srv.Close()

	dev := id.New()
	user, pass := server.TURNCredential(secret, dev, time.Hour, time.Now())
	allocate := func() (net.PacketConn, *turn.Client, error) {
		conn, err := (&tls.Dialer{Config: &tls.Config{RootCAs: testCertPool(t), ServerName: "localhost"}}).DialContext(t.Context(), "tcp", raw.Addr().String())
		if err != nil {
			return nil, nil, err
		}
		client, err := turn.NewClient(&turn.ClientConfig{
			TURNServerAddr: raw.Addr().String(), Username: user, Password: pass,
			Realm: "chat.example.test", Conn: turn.NewSTUNConn(conn), RTO: time.Second,
		})
		if err != nil {
			conn.Close()
			return nil, nil, err
		}
		if err := client.Listen(); err != nil {
			client.Close()
			return nil, nil, err
		}
		relay, err := client.Allocate()
		if err != nil {
			client.Close()
			return nil, nil, err
		}
		return relay, client, nil
	}
	first, c1, err := allocate()
	if err != nil {
		t.Fatalf("first allocation: %v", err)
	}
	defer c1.Close()
	if host, _, _ := net.SplitHostPort(first.LocalAddr().String()); host != "127.0.0.1" {
		t.Fatalf("relayed address %s is not on turn.relay_ip", first.LocalAddr())
	}
	second, c2, err := allocate()
	if err != nil {
		t.Fatalf("second allocation: %v", err)
	}
	defer c2.Close()
	defer second.Close()
	if _, _, err := allocate(); err == nil || !strings.Contains(err.Error(), "486") {
		t.Fatalf("third allocation = %v, want 486 Allocation Quota Reached", err)
	}
	// Closing a relay refreshes it to lifetime 0, which deletes the allocation
	// and must hand the slot back.
	if err := first.Close(); err != nil {
		t.Fatalf("close relay: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		relay, c3, err := allocate()
		if err == nil {
			relay.Close()
			c3.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("an allocation after a release was still refused: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// plainTURNClient allocates a relay over a plain TCP connection to addr.
func plainTURNClient(t *testing.T, addr, secret string) net.PacketConn {
	t.Helper()
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial TURN: %v", err)
	}
	user, pass := server.TURNCredential(secret, id.New(), time.Hour, time.Now())
	client, err := turn.NewClient(&turn.ClientConfig{
		TURNServerAddr: addr, Username: user, Password: pass,
		Realm: "chat.example.test", Conn: turn.NewSTUNConn(conn), RTO: time.Second,
	})
	if err != nil {
		t.Fatalf("TURN client: %v", err)
	}
	t.Cleanup(client.Close)
	if err := client.Listen(); err != nil {
		t.Fatalf("client listen: %v", err)
	}
	relay, err := client.Allocate()
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	t.Cleanup(func() { _ = relay.Close() })
	return relay
}

// udpPeer is a UDP socket on addr that reports the first datagram it receives.
func udpPeer(t *testing.T, addr string) (net.PacketConn, <-chan string) {
	t.Helper()
	pc, err := (&net.ListenConfig{}).ListenPacket(t.Context(), "udp4", addr)
	if err != nil {
		t.Fatalf("listen %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 1500)
		n, _, err := pc.ReadFrom(buf)
		if err == nil {
			got <- string(buf[:n])
		}
	}()
	return pc, got
}

// sfuPeers admits the addresses of conns, standing in for the SFU, on the ports from the lowest of
// theirs to the highest.
func sfuPeers(conns ...net.PacketConn) server.TURNPeers {
	var p server.TURNPeers
	for _, c := range conns {
		ap := c.LocalAddr().(*net.UDPAddr).AddrPort()
		p.Addrs = append(p.Addrs, ap.Addr().Unmap())
		if p.PortLo == 0 || ap.Port() < p.PortLo {
			p.PortLo = ap.Port()
		}
		p.PortHi = max(p.PortHi, ap.Port())
	}
	return p
}

func startPlainTURN(t *testing.T, secret string, peers server.TURNPeers) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv, err := server.StartTURN(config.TURN{
		Enabled: true, Realm: "chat.example.test", RelayIP: "127.0.0.1",
		SharedSecretFile: writeFile(t, "turn.secret", secret), CredentialTTL: "1h", AllocationsPerDevice: 2,
	}, ln, netip.Addr{}, peers, nil, nil, clock.System(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("StartTURN: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

// C9 (fix wave): the relay's only legitimate peer is the co-located SFU. A member holding a call
// credential must not be able to relay into another loopback service or the LAN: CreatePermission
// (and so every send) to any other address is refused, and nothing is delivered there.
func TestTheRelayReachesOnlyTheSFU(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	sfu, toSFU := udpPeer(t, "127.0.0.2:0")
	victim, toVictim := udpPeer(t, "127.0.0.1:0")
	addr := startPlainTURN(t, secret, sfuPeers(sfu))
	relay := plainTURNClient(t, addr, secret)

	if _, err := relay.WriteTo([]byte("media"), sfu.LocalAddr()); err != nil {
		t.Fatalf("a write to the SFU at %s: %v", sfu.LocalAddr(), err)
	}
	select {
	case got := <-toSFU:
		if got != "media" {
			t.Fatalf("the SFU received %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the SFU received nothing through the relay")
	}

	if _, err := relay.WriteTo([]byte("hello internal service"), victim.LocalAddr()); err == nil {
		t.Errorf("a write to the loopback service at %s was accepted", victim.LocalAddr())
	}
	lan := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 53}
	if _, err := relay.WriteTo([]byte("dns"), lan); err == nil {
		t.Errorf("a write to %s was accepted", lan)
	}
	select {
	case got := <-toVictim:
		t.Fatalf("the loopback service received %q through the relay", got)
	case <-time.After(500 * time.Millisecond):
	}
}

// With LiveKit off there is no SFU on the host, so the relay admits no peer at all.
func TestWithoutAnSFUTheRelayAdmitsNoPeer(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	peer, got := udpPeer(t, "127.0.0.2:0")
	relay := plainTURNClient(t, startPlainTURN(t, secret, server.TURNPeers{}), secret)
	if _, err := relay.WriteTo([]byte("media"), peer.LocalAddr()); err == nil {
		t.Error("a write was accepted by a relay with no SFU to reach")
	}
	select {
	case m := <-got:
		t.Fatalf("the peer received %q", m)
	case <-time.After(500 * time.Millisecond):
	}
}

// M4: when the IPv4 route probe fails, the "auto" fallback stays in node_ip's address family (or,
// with no node_ip, in the family of the address that did resolve) and is a config error when the
// host has no address of that family.
func TestAutoRelayIPKeepsNodeIPsAddressFamily(t *testing.T) {
	ipn := func(s string) net.Addr {
		p := netip.MustParsePrefix(s)
		return &net.IPNet{IP: p.Addr().AsSlice(), Mask: net.CIDRMask(p.Bits(), p.Addr().BitLen())}
	}
	v6, v4 := ipn("2001:db8::5/64"), ipn("10.0.0.5/24")
	for _, tc := range []struct {
		name   string
		probed net.IP
		addrs  []net.Addr
		prefer string
		want   string // "" = a config error
	}{
		{"v4 node, v6 listed first", nil, []net.Addr{v6, v4}, "203.0.113.7", "10.0.0.5"},
		{"v6 node, v4 listed first", nil, []net.Addr{v4, v6}, "2001:db8::1", "2001:db8::5"},
		{"v4 node, only v6 on the host", nil, []net.Addr{ipn("127.0.0.1/8"), v6}, "203.0.113.7", ""},
		{"v6 node, only v4 on the host", nil, []net.Addr{v4}, "2001:db8::1", ""},
		{"no node_ip, first usable wins", nil, []net.Addr{v6, v4}, "", "2001:db8::5"},
		{"v4 probe of the node's family wins", net.ParseIP("10.9.9.9"), []net.Addr{v6}, "203.0.113.7", "10.9.9.9"},
		{"v4 probe against a v6 node is not taken", net.ParseIP("10.9.9.9"), []net.Addr{v6}, "2001:db8::1", "2001:db8::5"},
		{"link-local and loopback never count", nil, []net.Addr{ipn("fe80::1/64"), ipn("::1/128")}, "2001:db8::1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var prefer netip.Addr
			if tc.prefer != "" {
				prefer = netip.MustParseAddr(tc.prefer)
			}
			got, err := server.ChooseRelayIPForTest(tc.probed, tc.addrs, prefer)
			if tc.want == "" {
				if err == nil || !server.IsTURNConfigError(err) {
					t.Fatalf("got %v, %v; want a config error", got, err)
				}
				return
			}
			if err != nil || got.String() != tc.want {
				t.Fatalf("got %v, %v; want %s", got, err, tc.want)
			}
		})
	}
}

// I12 (fix wave): turn.relay_ip = "auto" is what init writes, because the public IP is not a local
// address on bridged Docker or a NATed LXC and every Allocate would fail to bind there. "auto" is
// the source address of this host's outbound route (or its first non-loopback interface address),
// which the relay can bind; an explicit IP is taken as written.
func TestAnAutoRelayIPIsALocalAddressTheRelayBinds(t *testing.T) {
	ip, err := server.ResolveRelayIP("auto", netip.Addr{})
	if err != nil {
		t.Fatalf(`ResolveRelayIP("auto"): %v`, err)
	}
	pc, err := (&net.ListenConfig{}).ListenPacket(t.Context(), "udp", net.JoinHostPort(ip.String(), "0"))
	if err != nil {
		t.Fatalf("the auto relay address %s is not bindable: %v", ip, err)
	}
	_ = pc.Close()
	if got, err := server.ResolveRelayIP("203.0.113.7", netip.Addr{}); err != nil || got.String() != "203.0.113.7" {
		t.Errorf("an explicit relay_ip = %v, %v", got, err)
	}
	if _, err := server.ResolveRelayIP("chat.example", netip.Addr{}); err == nil {
		t.Error("a relay_ip that is neither an IP nor auto was accepted")
	}

	const secret = "0123456789abcdef0123456789abcdef"
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv, err := server.StartTURN(config.TURN{
		Enabled: true, Realm: "chat.example.test", RelayIP: "auto",
		SharedSecretFile: writeFile(t, "turn.secret", secret), CredentialTTL: "1h", AllocationsPerDevice: 2,
	}, ln, netip.Addr{}, server.TURNPeers{}, nil, nil, clock.System(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf(`StartTURN with relay_ip "auto": %v`, err)
	}
	defer srv.Close()
	relay := plainTURNClient(t, ln.Addr().String(), secret)
	if host, _, _ := net.SplitHostPort(relay.LocalAddr().String()); host != ip.String() {
		t.Errorf("relayed address %s, want one on the auto address %s", relay.LocalAddr(), ip)
	}
}

func TestStartTURNRefusesAMissingSecret(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	_, err = server.StartTURN(config.TURN{
		Enabled: true, Realm: "chat.example.test", RelayIP: "127.0.0.1",
		SharedSecretFile: filepath.Join(t.TempDir(), "missing"), CredentialTTL: "1h", AllocationsPerDevice: 2,
	}, ln, netip.Addr{}, server.TURNPeers{}, nil, nil, clock.System(), slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("StartTURN ran without its shared secret")
	}
	if !server.IsTURNConfigError(err) {
		t.Errorf("a missing shared secret = %v, want a configuration error", err)
	}
}

// C8 (fix wave): a relay that cannot set up its network (no netlink under a sandbox that blocks
// AF_NETLINK) is not a dilla.toml mistake: StartTURN's error says so, and serve reports it as
// unavailable rather than as a configuration error that systemd never restarts.
func TestANetworkSetupFailureIsNotAConfigError(t *testing.T) {
	restore := server.FailTURNNetForTest(errors.New("netlinkrib: address family not supported by protocol"))
	defer restore()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	_, err = server.StartTURN(config.TURN{
		Enabled: true, Realm: "chat.example.test", RelayIP: "127.0.0.1",
		SharedSecretFile: writeFile(t, "turn.secret", "0123456789abcdef0123456789abcdef"),
		CredentialTTL:    "1h", AllocationsPerDevice: 2,
	}, ln, netip.Addr{}, server.TURNPeers{}, nil, nil, clock.System(), slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "netlinkrib") {
		t.Fatalf("StartTURN with no network = %v, want the network error", err)
	}
	if server.IsTURNConfigError(err) {
		t.Errorf("a network setup failure = %v, classified as a configuration error", err)
	}
}

func writeFile(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// fakeTURNMetrics records what the relay reports.
type fakeTURNMetrics struct {
	mu                        sync.Mutex
	refused, toClient, toPeer int
	full, dropped, overflows  int
	allocations               []int
}

func (m *fakeTURNMetrics) QuotaRefused() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refused++
}

func (m *fakeTURNMetrics) CapacityRefused() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.full++
}

func (m *fakeTURNMetrics) PeerDropped() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dropped++
}

func (m *fakeTURNMetrics) CutOverflow() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.overflows++
}

// extra is what the relay counted beyond snapshot: Allocates refused at the instance-wide cap,
// datagrams dropped for their peer, and overflowing cuts.
func (m *fakeTURNMetrics) extra() (full, dropped, overflows int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.full, m.dropped, m.overflows
}

func (m *fakeTURNMetrics) RelayBytes(toClient bool, n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if toClient {
		m.toClient += n
	} else {
		m.toPeer += n
	}
}
func (m *fakeTURNMetrics) Allocations(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.allocations = append(m.allocations, n)
}

func (m *fakeTURNMetrics) snapshot() (refused, toClient, toPeer int, allocations []int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.refused, m.toClient, m.toPeer, slices.Clone(m.allocations)
}

// G33: the expiry binds Allocate only; Refresh, CreatePermission and ChannelBind keep working after
// it until turn.max_allocation_age has passed since the credential was issued.
func TestTheCredentialExpiryBindsAllocateOnly(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	clk := clock.NewFake(now)
	user, _ := server.TURNCredential("s3cret", id.New(), time.Hour, now) // issued now, expires now+1h
	auth := server.TURNAuthForTest("s3cret", clk, 2*time.Hour, nil)
	ok := func(m stun.Method) bool {
		_, _, ok := auth(&turn.RequestAttributes{Username: user, Realm: "chat.example.test", Method: m})
		return ok
	}
	others := []stun.Method{stun.MethodRefresh, stun.MethodCreatePermission, stun.MethodChannelBind}
	if !ok(stun.MethodAllocate) {
		t.Fatal("a fresh credential could not allocate")
	}
	clk.Advance(90 * time.Minute) // expired, inside the max age
	if ok(stun.MethodAllocate) {
		t.Fatal("an expired credential allocated")
	}
	for _, m := range others {
		if !ok(m) {
			t.Fatalf("%s with an expired credential inside max_allocation_age was refused", m)
		}
	}
	clk.Advance(31 * time.Minute) // 2 h 1 min after issue
	for _, m := range others {
		if ok(m) {
			t.Fatalf("%s past max_allocation_age was accepted", m)
		}
	}
}

// startMeteredTURN is startPlainTURN with a TURN config and metrics of the test's own.
func startMeteredTURN(t *testing.T, secret string, c config.TURN, peers server.TURNPeers, m server.TURNMetrics, clk clock.Clock) string {
	t.Helper()
	return startRevokableTURN(t, secret, c, peers, m, nil, clk)
}

// startRevokableTURN is startMeteredTURN reading the test's own revocation state.
func startRevokableTURN(t *testing.T, secret string, c config.TURN, peers server.TURNPeers, m server.TURNMetrics,
	rev *server.RelayRevocations, clk clock.Clock) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	c.Enabled, c.Realm, c.RelayIP = true, "chat.example.test", "127.0.0.1"
	c.SharedSecretFile = writeFile(t, "turn.secret", secret)
	srv, err := server.StartTURN(c, ln, netip.MustParseAddr("127.0.0.1"), peers, m, rev, clk, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("StartTURN: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

// turnClientAs is a listening TURN client over a plain TCP connection to addr with the given
// credential, asking for family's relays (0: pion infers IPv4 from the connection).
func turnClientAs(t *testing.T, addr, user, pass string, family turn.RequestedAddressFamily) (*turn.Client, error) {
	t.Helper()
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		return nil, err
	}
	client, err := turn.NewClient(&turn.ClientConfig{
		TURNServerAddr: addr, Username: user, Password: pass,
		Realm: "chat.example.test", Conn: turn.NewSTUNConn(conn), RTO: time.Second,
		RequestedAddressFamily: family,
	})
	if err != nil {
		conn.Close()
		return nil, err
	}
	t.Cleanup(client.Close)
	if err := client.Listen(); err != nil {
		return nil, err
	}
	return client, nil
}

// allocateAs allocates a relay over a plain TCP connection with the given credential.
func allocateAs(t *testing.T, addr, user, pass string) (net.PacketConn, error) {
	t.Helper()
	client, err := turnClientAs(t, addr, user, pass, 0)
	if err != nil {
		return nil, err
	}
	relay, err := client.Allocate()
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = relay.Close() })
	return relay, nil
}

// SP-25 (Go half): an allocation made before the credential expired keeps binding new permissions
// after it; a fresh Allocate with it is refused; past turn.max_allocation_age nothing binds.
func TestAnExpiredCredentialStillBindsButCannotAllocate(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	now := time.Now()
	clk := clock.NewFake(now)
	peerA, gotA := udpPeer(t, "127.0.0.2:0")
	peerB, gotB := udpPeer(t, "127.0.0.3:0")
	addr := startMeteredTURN(t, secret, config.TURN{CredentialTTL: "1m", MaxAllocationAge: "3m", AllocationsPerDevice: 4},
		sfuPeers(peerA, peerB), nil, clk)
	user, pass := server.TURNCredential(secret, id.New(), time.Minute, now)
	relay, err := allocateAs(t, addr, user, pass)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	clk.Advance(2 * time.Minute) // the credential expired a minute ago
	if _, err := relay.WriteTo([]byte("after-expiry"), peerA.LocalAddr()); err != nil {
		t.Fatalf("a CreatePermission with an expired credential inside max_allocation_age: %v", err)
	}
	select {
	case got := <-gotA:
		if got != "after-expiry" {
			t.Fatalf("peer A received %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("peer A received nothing after the credential expired")
	}
	if _, err := allocateAs(t, addr, user, pass); err == nil {
		t.Fatal("an expired credential allocated")
	}
	clk.Advance(90 * time.Second) // 3 min 30 s after issue: past max_allocation_age
	// The refusal is the auth handler's (400), not the peer filter's (403): peer B is admitted.
	if _, err := relay.WriteTo([]byte("past-max-age"), peerB.LocalAddr()); err == nil ||
		!strings.Contains(err.Error(), "CreatePermission error response (error 400") {
		t.Fatalf("a CreatePermission past max_allocation_age = %v, want the auth handler's 400", err)
	}
	select {
	case got := <-gotB:
		t.Fatalf("peer B received %q past max_allocation_age", got)
	case <-time.After(time.Second):
	}
}

// Review M6: turn.max_allocation_age is measured from the issue time the credential carries, not
// from its expiry less the current turn.credential_ttl, so changing the TTL across a restart cannot
// lengthen an old credential's life: credentials issued together are bounded together whatever
// TTL each was minted with.
func TestTheMaxAgeIsMeasuredFromTheCarriedIssueTime(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	clk := clock.NewFake(now)
	auth := server.TURNAuthForTest("s3cret", clk, 2*time.Hour, nil)
	long, _ := server.TURNCredential("s3cret", id.New(), time.Hour, now)
	short, _ := server.TURNCredential("s3cret", id.New(), 5*time.Minute, now)
	refresh := func(user string) bool {
		_, _, ok := auth(&turn.RequestAttributes{Username: user, Realm: "chat.example.test", Method: stun.MethodRefresh})
		return ok
	}
	clk.Advance(2*time.Hour - time.Second)
	if !refresh(long) || !refresh(short) {
		t.Fatal("a Refresh inside max_allocation_age of the issue time was refused")
	}
	clk.Advance(2 * time.Second)
	if refresh(long) || refresh(short) {
		t.Fatal("a Refresh past max_allocation_age of the issue time was accepted")
	}
}

// I5: a real allocation's deletion frees its quota slot and moves the gauge — four allocations,
// one closed, and a fifth allocates with the gauge back at four.
func TestClosingAnAllocationFreesItsSlotAndTheGauge(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	m := &fakeTURNMetrics{}
	addr := startMeteredTURN(t, secret, config.TURN{CredentialTTL: "1h", AllocationsPerDevice: 4}, server.TURNPeers{}, m, clock.System())
	user, pass := server.TURNCredential(secret, id.New(), time.Hour, time.Now())
	var relays []net.PacketConn
	for i := range 4 {
		relay, err := allocateAs(t, addr, user, pass)
		if err != nil {
			t.Fatalf("allocation %d: %v", i+1, err)
		}
		relays = append(relays, relay)
	}
	if err := relays[0].Close(); err != nil { // a Refresh with lifetime 0
		t.Fatalf("close: %v", err)
	}
	waitGauge(t, m, 3)
	if _, err := allocateAs(t, addr, user, pass); err != nil {
		t.Fatalf("the fifth allocation after one closed: %v", err)
	}
	waitGauge(t, m, 4)
	if refused, _, _, _ := m.snapshot(); refused != 0 {
		t.Fatalf("quota refusals = %d, want none", refused)
	}
}

// waitGauge waits up to five seconds for the allocation gauge's latest value to be want.
func waitGauge(t *testing.T, m *fakeTURNMetrics, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, _, _, allocations := m.snapshot()
		if len(allocations) > 0 && allocations[len(allocations)-1] == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("allocation gauge %v, want it to reach %d", allocations, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// I1: a revoked device's credentials issued at or before the cut are refused on every method, a
// credential issued after it passes, and the cut is forgotten once max_allocation_age has passed.
func TestARevokedCredentialIsRefusedOnEveryMethod(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	clk := clock.NewFake(now)
	rev := server.NewRelayRevocations(2*time.Hour, clk)
	auth := server.TURNAuthForTest("s3cret", clk, 2*time.Hour, rev)
	dev, other := id.New(), id.New()
	before, _ := server.TURNCredential("s3cret", dev, time.Hour, now)
	otherCred, _ := server.TURNCredential("s3cret", other, time.Hour, now)
	ok := func(user string, m stun.Method) bool {
		_, _, ok := auth(&turn.RequestAttributes{Username: user, Realm: "chat.example.test", Method: m})
		return ok
	}
	methods := []stun.Method{stun.MethodAllocate, stun.MethodRefresh, stun.MethodCreatePermission,
		stun.MethodChannelBind, stun.MethodConnect, stun.MethodConnectionBind}
	clk.Advance(time.Minute)
	rev.Revoke(dev, clk.Now())
	for _, m := range methods {
		if ok(before, m) {
			t.Errorf("%s with a credential issued before the cut was accepted", m)
		}
		if !ok(otherCred, m) {
			t.Errorf("%s of another device was refused", m)
		}
	}
	atCut, _ := server.TURNCredential("s3cret", dev, time.Hour, clk.Now())
	if ok(atCut, stun.MethodAllocate) {
		t.Error("a credential issued in the cut's millisecond was accepted")
	}
	clk.Advance(time.Millisecond)
	after, _ := server.TURNCredential("s3cret", dev, time.Hour, clk.Now())
	for _, m := range methods {
		if !ok(after, m) {
			t.Errorf("%s with a credential issued after the cut was refused", m)
		}
	}
	clk.Advance(2*time.Hour + time.Minute)
	later, _ := server.TURNCredential("s3cret", dev, time.Hour, clk.Now())
	if !ok(later, stun.MethodAllocate) {
		t.Error("a fresh credential was refused past max_allocation_age of the cut")
	}
	if n := rev.RelayCutsForTest(); n != 0 {
		t.Errorf("the cut is still held %d entries past max_allocation_age", n)
	}
}

// Commit review: a burst of cuts never pushes a live cut out. Past the soft cap the map grows (with a
// WARN); at the hard cap a new cut raises the floor instead of evicting (next test), and every cut
// already held — the oldest included — stays enforced until turn.max_allocation_age has passed.
func TestABurstOfCutsNeverForgetsALiveCut(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_790_000_000, 0))
	var mu sync.Mutex
	var logged strings.Builder
	rev := server.NewRelayRevocations(2*time.Hour, clk).
		WithBarred(&fakeBarred{}, slog.New(slog.NewTextHandler(lockedWriter{&mu, &logged}, nil)))
	auth := server.TURNAuthForTest("s3cret", clk, 2*time.Hour, rev)
	first := id.New()
	clk.Advance(time.Minute)
	old, _ := server.TURNCredential("s3cret", first, time.Hour, clk.Now())
	if _, _, ok := auth(&turn.RequestAttributes{Username: old, Method: stun.MethodRefresh}); !ok {
		t.Fatal("a credential minted after the start was refused before any cut")
	}
	rev.Revoke(first, clk.Now())
	clk.Advance(time.Second) // first is the oldest cut, the one an evicting map would drop
	for range server.MaxRelayCutsForTest + 10 {
		rev.Revoke(id.New(), clk.Now())
	}
	if n := rev.RelayCutsForTest(); n != server.MaxRelayCutsForTest {
		t.Fatalf("cuts held = %d, want the hard cap %d", n, server.MaxRelayCutsForTest)
	}
	if _, _, ok := auth(&turn.RequestAttributes{Username: old, Method: stun.MethodRefresh}); ok {
		t.Fatal("a burst of cuts pushed out the oldest live cut: its device's old credential is admitted again")
	}
	mu.Lock()
	full := strings.Contains(logged.String(), "level=WARN")
	mu.Unlock()
	if !full {
		t.Fatal("the full cut map was not logged at WARN")
	}
	clk.Advance(2*time.Hour + time.Minute)
	rev.Revoke(id.New(), clk.Now()) // makes room by dropping the expired cuts only
	if n := rev.RelayCutsForTest(); n != 1 {
		t.Fatalf("cuts held after max_allocation_age = %d, want only the new one", n)
	}
}

// Commit review (amplification): a cut is recorded only for a device that can hold a relay
// credential. Revoking 100 000 devices that never had one minted leaves the cut map empty and the
// floor unset; a device whose credential was minted is cut and refused. This holds from the first
// moment of the process (re-review N3, N7): a credential minted before it started is refused by the
// start floor, so the mints it recorded are all the credentials a cut could have to refuse.
func TestOnlyADeviceThatCanHoldACredentialGetsACut(t *testing.T) {
	start := time.Unix(1_790_000_000, 0)
	clk := clock.NewFake(start)
	rev := server.NewRelayRevocations(2*time.Hour, clk).WithCredentialTTL(time.Hour)
	auth := server.TURNAuthForTest("s3cret", clk, 2*time.Hour, rev)
	rev.Revoke(id.New(), clk.Now())
	if n := rev.RelayCutsForTest(); n != 0 {
		t.Fatalf("a cut of a device that never minted, at the process start = %d entries, want none", n)
	}
	clk.Advance(time.Millisecond)
	bystander := id.New()
	bystanderOld, _ := server.TURNCredential("s3cret", bystander, time.Hour, clk.Now())
	clk.Advance(time.Second)
	for range 100_000 {
		rev.Revoke(id.New(), clk.Now())
	}
	if n := rev.RelayCutsForTest(); n != 0 {
		t.Fatalf("revoking devices that never minted a credential left %d cut entries", n)
	}
	if rev.OverflowsForTest() != 0 {
		t.Fatal("revoking devices that never minted a credential raised the floor")
	}
	if _, _, ok := auth(&turn.RequestAttributes{Username: bystanderOld, Method: stun.MethodRefresh}); !ok {
		t.Fatal("a bystander's credential was refused after cuts of devices that never minted")
	}
	minted := id.New()
	issued, _ := rev.Mint(minted, clk.Now())
	mintedCred, _ := server.TURNCredential("s3cret", minted, time.Hour, issued)
	clk.Advance(time.Second)
	rev.Revoke(minted, clk.Now())
	if _, _, ok := auth(&turn.RequestAttributes{Username: mintedCred, Method: stun.MethodRefresh}); ok {
		t.Fatal("a device that minted a credential and was cut kept it")
	}
}

// Commit review (mint race), re-review N1: Mint chooses the issue time under the lock Revoke takes,
// and it is the clock's time, never later. A Revoke after a Mint covers that credential, even in the
// same millisecond; a Mint in the millisecond of a cut — or for a request that began before the cut
// — mints nothing and answers a wait of a few milliseconds, after which it issues a credential newer
// than the cut, which works. A cut time is never later than the clock.
func TestMintAndRevokeAreOrdered(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_790_000_000, 0))
	rev := server.NewRelayRevocations(2*time.Hour, clk).WithCredentialTTL(time.Hour)
	clk.Advance(time.Millisecond)
	auth := server.TURNAuthForTest("s3cret", clk, 2*time.Hour, rev)
	ok := func(user string) bool {
		_, _, ok := auth(&turn.RequestAttributes{Username: user, Method: stun.MethodRefresh})
		return ok
	}
	dev := id.New()
	begun := clk.Now()
	issued, wait := rev.Mint(dev, begun)
	if wait != 0 || !issued.Equal(clk.Now()) {
		t.Fatalf("Mint = %v, wait %v; want the clock's time %v", issued, wait, clk.Now())
	}
	before, _ := server.TURNCredential("s3cret", dev, time.Hour, issued)
	rev.Revoke(dev, clk.Now()) // the same millisecond as the mint
	if ok(before) {
		t.Fatal("a credential minted before the cut, in the same millisecond, is still valid")
	}
	if _, wait := rev.Mint(dev, clk.Now()); wait != time.Millisecond {
		t.Fatalf("a mint in the cut's millisecond answered wait %v, want 1ms and no credential", wait)
	}
	clk.Advance(time.Millisecond)
	newer, wait := rev.Mint(dev, clk.Now())
	if wait != 0 || !newer.Equal(clk.Now()) {
		t.Fatalf("a mint a millisecond after the cut = %v, wait %v; want the clock's time", newer, wait)
	}
	after, _ := server.TURNCredential("s3cret", dev, time.Hour, newer)
	if !ok(after) {
		t.Fatal("a credential minted after the cut was refused")
	}
	rev.Revoke(dev, clk.Now()) // the same millisecond again: the cut covers the newer credential too
	if ok(after) {
		t.Fatal("a cut after the second mint left its credential valid")
	}
	// A request that began before a cut gets nothing, whenever it mints.
	begun = clk.Now()
	clk.Advance(time.Millisecond)
	rev.Revoke(dev, clk.Now())
	clk.Advance(time.Second)
	if _, wait := rev.Mint(dev, begun); wait <= 0 || wait > 10*time.Millisecond {
		t.Fatalf("a mint for a request cut while it was served answered wait %v, want a few milliseconds", wait)
	}
	// A cut is never ahead of the clock, whatever time it is asked for.
	rev.Revoke(dev, clk.Now().Add(time.Hour))
	if at, cut := rev.CutOfForTest(dev.String()); !cut || at != clk.Now().UnixMilli() {
		t.Fatalf("a cut asked for an hour ahead is at %d (%v), want the clock's %d", at, cut, clk.Now().UnixMilli())
	}
}

// Re-review N1, the reviewer's drift probe as a regression test. One member's refused starts — two
// a second for ten simulated minutes, alternating a start in its own cut's millisecond (Mint
// refuses) and a start the re-check after the mint refuses (which cuts it as of now) — move
// nothing: its cut never runs ahead of the clock, and a victim cut afterwards starts a millisecond
// later, with a credential issued at the clock's time.
func TestRefusedStartsMoveNoCutAndNoIssueTime(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_790_000_000, 0))
	rev := server.NewRelayRevocations(2*time.Hour, clk).WithCredentialTTL(time.Hour)
	auth := server.TURNAuthForTest("s3cret", clk, 2*time.Hour, rev)
	attacker, victim := id.New(), id.New()
	clk.Advance(time.Millisecond)
	rev.Mint(attacker, clk.Now())
	rev.Mint(victim, clk.Now())
	clk.Advance(time.Millisecond)
	rev.Revoke(attacker, clk.Now()) // one ordinary cut
	for i := range 1200 {
		clk.Advance(500 * time.Millisecond)
		begun := clk.Now()
		if i%2 == 0 {
			rev.Revoke(attacker, clk.Now())
			if _, wait := rev.Mint(attacker, begun); wait <= 0 {
				t.Fatalf("start %d: a mint in its cut's millisecond minted", i)
			}
		} else {
			issued, wait := rev.Mint(attacker, begun)
			if wait != 0 || !issued.Equal(clk.Now()) {
				t.Fatalf("start %d: Mint = %v, wait %v; want the clock's time", i, issued, wait)
			}
			rev.Revoke(attacker, clk.Now()) // the re-check after the mint refuses it
		}
		if at, _ := rev.CutOfForTest(attacker.String()); at > clk.Now().UnixMilli() {
			t.Fatalf("start %d: the attacker's cut is %d ms ahead of the clock", i, at-clk.Now().UnixMilli())
		}
	}
	rev.Revoke(victim, clk.Now())
	cutAt := clk.Now().UnixMilli()
	clk.Advance(time.Millisecond)
	issued, wait := rev.Mint(victim, clk.Now())
	if wait != 0 {
		t.Fatalf("the victim's start a millisecond after its cut was refused (wait %v)", wait)
	}
	if !issued.Equal(clk.Now()) {
		t.Fatalf("the victim's credential was issued at %v, %v ahead of the clock", issued, issued.Sub(clk.Now()))
	}
	if at, _ := rev.CutOfForTest(victim.String()); at != cutAt {
		t.Fatalf("the victim's cut moved from %d to %d", cutAt, at)
	}
	user, _ := server.TURNCredential("s3cret", victim, time.Hour, issued)
	if _, _, ok := auth(&turn.RequestAttributes{Username: user, Method: stun.MethodAllocate}); !ok {
		t.Fatal("the victim's fresh credential was refused")
	}
}

// Re-review N1: an honest client that retries a start once a second, each time just after a cut in
// the same millisecond, gets in on every retry after the few milliseconds the refusal names.
func TestAnHonestRetryAfterASameMillisecondCutSucceeds(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_790_000_000, 0))
	rev := server.NewRelayRevocations(2*time.Hour, clk).WithCredentialTTL(time.Hour)
	dev := id.New()
	clk.Advance(time.Millisecond)
	rev.Mint(dev, clk.Now())
	for i := range 120 {
		clk.Advance(time.Second)
		rev.Revoke(dev, clk.Now())
		_, wait := rev.Mint(dev, clk.Now())
		if wait <= 0 || wait > 10*time.Millisecond {
			t.Fatalf("retry %d: a start in its cut's millisecond answered wait %v, want a few milliseconds", i, wait)
		}
		clk.Advance(wait)
		if issued, wait := rev.Mint(dev, clk.Now()); wait != 0 || !issued.Equal(clk.Now()) {
			t.Fatalf("retry %d after %v: Mint = %v, wait %v; want a credential at the clock's time", i, wait, issued, wait)
		}
	}
}

// Re-review N7: a full mint record is not scanned on every mint — only once its earliest entry can
// have expired — and while it stays full every cut is recorded (fail closed).
func TestAFullMintRecordIsNotScannedOnEveryMint(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_790_000_000, 0))
	rev := server.NewRelayRevocations(2*time.Hour, clk).WithCredentialTTL(time.Hour)
	rev.SetMaxCutsForTest(4)
	clk.Advance(time.Millisecond)
	for range 4 {
		rev.Mint(id.New(), clk.Now())
	}
	for range 1000 {
		clk.Advance(time.Millisecond)
		rev.Mint(id.New(), clk.Now())
	}
	if n := rev.MintScansForTest(); n != 1 {
		t.Fatalf("1000 mints against a full record scanned it %d times, want once", n)
	}
	rev.Revoke(id.New(), clk.Now()) // a device whose mint could not be recorded
	if n := rev.RelayCutsForTest(); n != 1 {
		t.Fatalf("a cut while the mint record is full = %d entries, want it recorded", n)
	}
	clk.Advance(time.Hour)
	rev.Mint(id.New(), clk.Now()) // the first four have expired
	if n, held := rev.MintScansForTest(), rev.MintsForTest(); n != 2 || held != 1 {
		t.Fatalf("after the record's entries expired: %d scans, %d mints held; want 2, 1", n, held)
	}
}

// Commit review fail-open finding: a cut that finds the map full of live cuts fails closed. It
// raises the floor to its time, so its own device's old credential is refused, every other
// device's old credential too (they re-fetch), and credentials issued after the floor work; it is
// counted and logged at WARN, and the floor expires like a cut.
func TestAnOverflowingCutRaisesTheFloorForEveryDevice(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_790_000_000, 0))
	var mu sync.Mutex
	var logged strings.Builder
	rev := server.NewRelayRevocations(2*time.Hour, clk).WithCredentialTTL(time.Hour).
		WithBarred(&fakeBarred{}, slog.New(slog.NewTextHandler(lockedWriter{&mu, &logged}, nil)))
	rev.SetMaxCutsForTest(3)
	clk.Advance(time.Hour + time.Minute)
	start := clk.Now()
	auth := server.TURNAuthForTest("s3cret", clk, 2*time.Hour, rev)
	ok := func(user string) bool {
		_, _, ok := auth(&turn.RequestAttributes{Username: user, Method: stun.MethodRefresh})
		return ok
	}
	overflowing, unrelated := id.New(), id.New()
	overflowingOld, _ := server.TURNCredential("s3cret", overflowing, time.Hour, start)
	unrelatedOld, _ := server.TURNCredential("s3cret", unrelated, time.Hour, start)
	rev.Mint(overflowing, start)
	rev.Mint(unrelated, start)
	for range 3 {
		dev := id.New()
		rev.Mint(dev, start)
		rev.Revoke(dev, clk.Now())
	}
	clk.Advance(time.Second)
	if !ok(unrelatedOld) {
		t.Fatal("an unrelated device was refused before any overflow")
	}
	rev.Revoke(overflowing, clk.Now()) // the map is full of live cuts
	if n := rev.RelayCutsForTest(); n != 3 {
		t.Fatalf("cuts held = %d, want the cap 3: no live cut is evicted", n)
	}
	if ok(overflowingOld) {
		t.Fatal("the overflowing cut was forgotten: its device's old credential is admitted")
	}
	if ok(unrelatedOld) {
		t.Fatal("the floor did not refuse an unrelated device's credential issued before it")
	}
	if n := rev.OverflowsForTest(); n != 1 {
		t.Fatalf("overflows = %d, want 1", n)
	}
	mu.Lock()
	warned := strings.Contains(logged.String(), "level=WARN") && strings.Contains(logged.String(), "cut map is full")
	mu.Unlock()
	if !warned {
		t.Fatalf("the overflow was not logged at WARN: %q", logged.String())
	}
	clk.Advance(time.Second)
	for _, dev := range []id.ID{overflowing, unrelated} {
		fresh, _ := server.TURNCredential("s3cret", dev, time.Hour, clk.Now())
		if !ok(fresh) {
			t.Fatalf("a credential issued after the floor was refused for %s", dev)
		}
	}
	clk.Advance(2*time.Hour + time.Minute)
	renewed, _ := server.TURNCredential("s3cret", unrelated, time.Hour, start.Add(2*time.Hour))
	if !ok(renewed) {
		t.Fatal("the floor outlived turn.max_allocation_age")
	}
}

// Commit review race: an Allocate that passed the auth handler just before a cut must not leave a
// relay socket open. The allocation is parked after its socket is bound and before it is tracked,
// the device is revoked, and then the allocation is let go: it is refused (508), its socket and
// quota slot are freed, and the gauge never moves; a credential issued after the cut allocates.
func TestAnAllocationRacingACutIsRefused(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	m := &fakeTURNMetrics{}
	rev := server.NewRelayRevocations(2*time.Hour, clock.System())
	addr := startRevokableTURN(t, secret, config.TURN{CredentialTTL: "1h", AllocationsPerDevice: 1}, server.TURNPeers{}, m, rev, clock.System())
	dev := id.New()
	time.Sleep(2 * time.Millisecond) // a credential minted after the relay started
	user, pass := server.TURNCredential(secret, dev, time.Hour, time.Now())
	parked, release, restore := server.ParkAllocateForTest(dev.String())
	result := make(chan error, 1)
	go func() {
		_, err := allocateAs(t, addr, user, pass)
		result <- err
	}()
	select {
	case <-parked:
	case <-time.After(5 * time.Second):
		t.Fatal("the allocation never reached its relay socket")
	}
	rev.Revoke(dev, time.Now()) // returns before the parked socket is tracked
	close(release)
	restore()
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "508") {
			t.Fatalf("the allocation that raced the cut = %v, want 508", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the parked allocation never answered")
	}
	if _, _, _, allocations := m.snapshot(); len(allocations) != 0 {
		t.Fatalf("allocation gauge %v, want it never to move", allocations)
	}
	time.Sleep(2 * time.Millisecond)
	fresh, freshPass := server.TURNCredential(secret, dev, time.Hour, time.Now())
	if _, err := allocateAs(t, addr, fresh, freshPass); err != nil {
		t.Fatalf("a credential issued after the cut (the slot must be free): %v", err)
	}
}

// I1, end to end: revoking a device closes its relay sockets at once — pion deletes the allocation,
// which moves the gauge and frees the slot — and its credential cannot allocate again, while one
// issued after the cut can.
func TestRevokingADeviceClosesItsRelay(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	m := &fakeTURNMetrics{}
	rev := server.NewRelayRevocations(2*time.Hour, clock.System())
	peer, got := udpPeer(t, "127.0.0.2:0")
	addr := startRevokableTURN(t, secret, config.TURN{CredentialTTL: "1h", AllocationsPerDevice: 1},
		sfuPeers(peer), m, rev, clock.System())
	dev := id.New()
	time.Sleep(2 * time.Millisecond) // a credential minted after the relay started
	user, pass := server.TURNCredential(secret, dev, time.Hour, time.Now())
	relay, err := allocateAs(t, addr, user, pass)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	waitGauge(t, m, 1)
	rev.Revoke(dev, time.Now())
	waitGauge(t, m, 0)
	if _, err := relay.WriteTo([]byte("after-revoke"), peer.LocalAddr()); err == nil {
		select {
		case s := <-got:
			t.Fatalf("the peer received %q after the revoke", s)
		case <-time.After(500 * time.Millisecond):
		}
	}
	if _, err := allocateAs(t, addr, user, pass); err == nil {
		t.Fatal("a credential issued before the cut allocated")
	}
	time.Sleep(2 * time.Millisecond)
	fresh, freshPass := server.TURNCredential(secret, dev, time.Hour, time.Now())
	if _, err := allocateAs(t, addr, fresh, freshPass); err != nil {
		t.Fatalf("a credential issued after the cut (the slot must be free): %v", err)
	}
}

// fakeBarred is a BarredLookup with a switchable answer that counts its lookups.
type fakeBarred struct {
	mu     sync.Mutex
	barred map[id.ID]bool
	err    error
	calls  int
}

func (f *fakeBarred) DeviceBarred(_ context.Context, dev id.ID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.barred[dev], f.err
}

func (f *fakeBarred) set(dev id.ID, barred bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.barred == nil {
		f.barred = map[id.ID]bool{}
	}
	f.barred[dev], f.err = barred, err
}

func (f *fakeBarred) lookups() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// barredRelay is a relay on a fake clock whose revocation state consults look and logs to logged.
func barredRelay(t *testing.T, secret string, c config.TURN, peers server.TURNPeers, look *fakeBarred) (
	addr string, m *fakeTURNMetrics, rev *server.RelayRevocations, clk *clock.Fake, logged *strings.Builder) {
	t.Helper()
	m, clk, logged = &fakeTURNMetrics{}, clock.NewFake(time.Now()), &strings.Builder{}
	var mu sync.Mutex
	rev = server.NewRelayRevocations(2*time.Hour, clk).WithBarred(look,
		slog.New(slog.NewTextHandler(lockedWriter{&mu, logged}, nil)))
	addr = startRevokableTURN(t, secret, c, peers, m, rev, clk)
	return addr, m, rev, clk, logged
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *strings.Builder
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

// Coordinator security finding on I1: pion calls the auth handler before it checks
// MESSAGE-INTEGRITY, so the store-backed lookup must never run there. Requests with made-up device
// ids and wrong passwords cause no lookup and no cache entry, and a flood of them leaves a real
// device's cached state alone.
func TestUnauthenticatedRequestsNeverReachTheBarredLookup(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	look := &fakeBarred{}
	addr, _, rev, clk, _ := barredRelay(t, secret, config.TURN{CredentialTTL: "1h"}, server.TURNPeers{}, look)
	realDev := id.New()
	user, pass := server.TURNCredential(secret, realDev, time.Hour, clk.Now())
	if _, err := allocateAs(t, addr, user, pass); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if n, size := look.lookups(), rev.BarredCacheLenForTest(); n != 1 || size != 1 {
		t.Fatalf("after one authenticated Allocate: %d lookups, %d cached; want 1, 1", n, size)
	}
	for range 40 {
		bogus, _ := server.TURNCredential("not-the-secret", id.New(), time.Hour, clk.Now())
		client, err := turnClientAs(t, addr, bogus, "wrong-password", 0)
		if err != nil {
			t.Fatalf("TURN client: %v", err)
		}
		if _, err := client.Allocate(); err == nil {
			t.Fatal("an Allocate with a forged credential succeeded")
		}
	}
	if n, size := look.lookups(), rev.BarredCacheLenForTest(); n != 1 || size != 1 {
		t.Fatalf("after 40 forged Allocates: %d lookups, %d cached; want still 1, 1", n, size)
	}
}

// I1, the cross-process gap: a device another process revoked, in no call room, is refused once an
// authenticated request of it finds it barred (the lookup is cached 30 s): its allocations end at
// once, and a credential issued before that stays refused whatever the lookup later says. A device
// found barred cannot allocate. A lookup that fails caches nothing and is logged at WARN.
func TestABarredDeviceLosesTheRelayOnItsNextAuthenticatedRequest(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	look := &fakeBarred{}
	peer, _ := udpPeer(t, "127.0.0.2:0")
	addr, m, rev, clk, logged := barredRelay(t, secret, config.TURN{CredentialTTL: "1h"},
		sfuPeers(peer), look)
	dev := id.New()
	user, pass := server.TURNCredential(secret, dev, time.Hour, clk.Now())
	relay, err := allocateAs(t, addr, user, pass)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	waitGauge(t, m, 1)
	look.set(dev, true, nil) // `dillad admin device revoke` in another process
	clk.Advance(31 * time.Second)
	// The CreatePermission authenticates, OnAuth asks the lookup, the device is cut.
	_, _ = relay.WriteTo([]byte("x"), peer.LocalAddr())
	waitGauge(t, m, 0)
	look.set(dev, false, nil)
	clk.Advance(31 * time.Second)
	if _, err := allocateAs(t, addr, user, pass); err == nil {
		t.Fatal("a credential issued before the barred lookup's cut allocated")
	}

	barred := id.New()
	look.set(barred, true, nil)
	bUser, bPass := server.TURNCredential(secret, barred, time.Hour, clk.Now())
	if _, err := allocateAs(t, addr, bUser, bPass); err == nil {
		t.Fatal("a device the lookup reports barred allocated")
	}

	failing := id.New()
	look.set(failing, false, errors.New("database is locked"))
	fUser, fPass := server.TURNCredential(secret, failing, time.Hour, clk.Now())
	before := rev.BarredCacheLenForTest()
	if _, err := allocateAs(t, addr, fUser, fPass); err != nil {
		t.Fatalf("a device whose lookup failed: %v (bounded by max_allocation_age, not refused)", err)
	}
	if rev.BarredCacheLenForTest() != before {
		t.Fatal("a failed lookup was cached")
	}
	if s := logged.String(); !strings.Contains(s, "level=WARN") || !strings.Contains(s, "database is locked") {
		t.Fatalf("the failed lookup was not logged at WARN: %q", s)
	}
}

// I1: every 30 s the relay asks the barred lookup about each device holding a relay socket, so a
// device that only sends ChannelData (never authenticated) loses the relay when another process
// revokes it.
func TestTheRelayRechecksTheDevicesHoldingSockets(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	look := &fakeBarred{}
	addr, m, rev, clk, _ := barredRelay(t, secret, config.TURN{CredentialTTL: "1h"}, server.TURNPeers{}, look)
	dev := id.New()
	user, pass := server.TURNCredential(secret, dev, time.Hour, clk.Now())
	if _, err := allocateAs(t, addr, user, pass); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	waitGauge(t, m, 1)
	rev.CheckHoldersForTest()
	if _, _, _, allocations := m.snapshot(); allocations[len(allocations)-1] != 1 {
		t.Fatal("the re-check closed a relay of a device that is not barred")
	}
	look.set(dev, true, nil)
	clk.Advance(31 * time.Second)
	rev.CheckHoldersForTest()
	waitGauge(t, m, 0)
}

// SP-26 (Go half): with the default quota of 4 the fifth allocation of one device is 486, and the
// refusal is counted; the live-allocation gauge follows creations.
func TestTheFifthAllocationOfADeviceIsRefusedAndCounted(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	m := &fakeTURNMetrics{}
	addr := startMeteredTURN(t, secret, config.TURN{CredentialTTL: "1h", AllocationsPerDevice: 4}, server.TURNPeers{}, m, clock.System())
	user, pass := server.TURNCredential(secret, id.New(), time.Hour, time.Now())
	for i := range 4 {
		if _, err := allocateAs(t, addr, user, pass); err != nil {
			t.Fatalf("allocation %d: %v", i+1, err)
		}
	}
	if _, err := allocateAs(t, addr, user, pass); err == nil || !strings.Contains(err.Error(), "486") {
		t.Fatalf("the fifth allocation = %v, want 486 Allocation Quota Reached", err)
	}
	refused, _, _, allocations := m.snapshot()
	if refused != 1 || len(allocations) == 0 || allocations[len(allocations)-1] != 4 {
		t.Fatalf("refused %d, allocation gauge %v; want 1 and ending at 4", refused, allocations)
	}
}

// dilla_turn_relay_bytes_total: what the relay sends to a peer and what a peer sends back.
func TestTheRelayCountsItsBytesBothWays(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	m := &fakeTURNMetrics{}
	peer, got := udpPeer(t, "127.0.0.2:0")
	addr := startMeteredTURN(t, secret, config.TURN{CredentialTTL: "1h"}, sfuPeers(peer), m, clock.System())
	user, pass := server.TURNCredential(secret, id.New(), time.Hour, time.Now())
	relay, err := allocateAs(t, addr, user, pass)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if _, err := relay.WriteTo([]byte("media"), peer.LocalAddr()); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the peer received nothing")
	}
	if _, err := peer.WriteTo([]byte("reply!"), relay.LocalAddr()); err != nil {
		t.Fatalf("the peer's reply: %v", err)
	}
	_ = relay.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64)
	if n, _, err := relay.ReadFrom(buf); err != nil || string(buf[:n]) != "reply!" {
		t.Fatalf("the client read %q, %v", buf[:n], err)
	}
	// The relay counts a datagram after its socket write returns, so the peer can have answered and the
	// client read that answer before the count of the first write lands (seen on a 4-vCPU runner).
	waitFor(t, "both relay byte counters", func() bool {
		_, toClient, toPeer, _ := m.snapshot()
		return toPeer >= len("media") && toClient >= len("reply!")
	})
}

// tcpPeer is a TCP listener on addr that counts the connections it accepts.
func tcpPeer(t *testing.T, addr string) (net.Listener, func() int) {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp4", addr)
	if err != nil {
		t.Fatalf("listen %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var mu sync.Mutex
	accepted := 0
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			accepted++
			mu.Unlock()
			_, _ = c.Write([]byte("hello-from-a-local-tcp-service"))
			_ = c.Close()
		}
	}()
	return ln, func() int {
		mu.Lock()
		defer mu.Unlock()
		return accepted
	}
}

// C1 (task 13 review): the relay offers UDP relays only. An RFC 6062 Allocate with
// REQUESTED-TRANSPORT TCP is refused 508 and frees the quota slot it took, so no Connect can open
// a TCP connection to any port of an admitted peer address (the SFU's, often the host's own).
func TestTheRelayRefusesTCPAllocations(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	m := &fakeTURNMetrics{}
	service, accepted := tcpPeer(t, "127.0.0.2:0")
	addr := startMeteredTURN(t, secret, config.TURN{CredentialTTL: "1h", AllocationsPerDevice: 1},
		server.TURNPeers{Addrs: []netip.Addr{netip.MustParseAddr("127.0.0.2")}, PortLo: 1, PortHi: 65535}, m, clock.System())
	user, pass := server.TURNCredential(secret, id.New(), time.Hour, time.Now())
	client, err := turnClientAs(t, addr, user, pass, 0)
	if err != nil {
		t.Fatalf("TURN client: %v", err)
	}
	alloc, err := client.AllocateTCP()
	if err == nil {
		// The hole this test closes: Connect (through Dial) reaches the admitted peer's TCP service.
		if conn, derr := alloc.Dial("tcp", service.Addr().String()); derr == nil {
			_ = conn.Close()
		}
		_ = alloc.Close()
		t.Errorf("a TCP allocation succeeded (relay %s)", alloc.Addr())
	} else if !strings.Contains(err.Error(), "508") {
		t.Errorf("a TCP allocation = %v, want 508 Insufficient Capacity", err)
	}
	time.Sleep(100 * time.Millisecond)
	if n := accepted(); n != 0 {
		t.Errorf("the relay opened %d TCP connection(s) to the admitted peer's service", n)
	}
	// The refused allocation holds no slot of the device's quota of one: a UDP relay still allocates.
	if _, err := allocateAs(t, addr, user, pass); err != nil {
		t.Fatalf("a UDP allocation after the refused TCP one: %v", err)
	}
	if _, _, _, allocations := m.snapshot(); !slices.Equal(allocations, []int{1}) {
		t.Fatalf("allocation gauge %v, want only the UDP relay", allocations)
	}
}

// C1, the generator itself: the TCP listener of an allocation and a Connect's outbound connection are
// both refused, the allocation's slot is freed, and the peer's TCP service is never dialled.
func TestTheRelayGeneratorRefusesTCP(t *testing.T) {
	service, accepted := tcpPeer(t, "127.0.0.2:0")
	listenErr, connErr, freed := server.TCPRelayRefusalsForTest(id.New().String(), service.Addr())
	if listenErr == nil || connErr == nil {
		t.Fatalf("TCP relay listener = %v, Connect's connection = %v; want both refused", listenErr, connErr)
	}
	if !freed {
		t.Fatal("the refused TCP allocation kept its quota slot")
	}
	time.Sleep(50 * time.Millisecond)
	if n := accepted(); n != 0 {
		t.Fatalf("the generator dialled the peer's TCP service %d time(s)", n)
	}
}

// I3 (task 13 review): an allocation pion admitted but could not create — here an IPv6 relay asked
// of an IPv4 turn.relay_ip, which a client can ask for every time — frees its quota slot, so it
// cannot lock the device out of the relay until a restart.
func TestAFailedRelaySocketFreesItsQuotaSlot(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	m := &fakeTURNMetrics{}
	addr := startMeteredTURN(t, secret, config.TURN{CredentialTTL: "1h", AllocationsPerDevice: 2}, server.TURNPeers{}, m, clock.System())
	user, pass := server.TURNCredential(secret, id.New(), time.Hour, time.Now())
	for i := range 3 {
		client, err := turnClientAs(t, addr, user, pass, turn.RequestedAddressFamilyIPv6)
		if err != nil {
			t.Fatalf("TURN client: %v", err)
		}
		if _, err := client.Allocate(); err == nil || !strings.Contains(err.Error(), "508") {
			t.Fatalf("IPv6 allocation %d against an IPv4 relay_ip = %v, want 508", i+1, err)
		}
	}
	for i := range 2 {
		if _, err := allocateAs(t, addr, user, pass); err != nil {
			t.Fatalf("IPv4 allocation %d after the failed IPv6 ones: %v", i+1, err)
		}
	}
	if refused, _, _, _ := m.snapshot(); refused != 0 {
		t.Fatalf("quota refusals = %d, want none", refused)
	}
}
