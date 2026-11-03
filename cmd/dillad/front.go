package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/pires/go-proxyproto"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/exit"
	"github.com/dilla-chat/dilla/internal/obs"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/sfu"
)

// handshakeTimeout bounds the 443 demux's TLS handshake and its eight-byte
// peek (facts-http-gateway §5.3).
const handshakeTimeout = 10 * time.Second

// front is what tls.mode puts in front of the composition root's handler: the
// listener http.Server serves, the TURN relay when turn.enabled and the ACME
// state in the direct-TLS modes. close releases all of it; it runs after the
// HTTP drain. The in-process SFU is started before the composition root
// (startSFU), because the call routes and /rtc are built over it.
type front struct {
	http    net.Listener
	closers []func()
}

func (f *front) close() {
	for i := len(f.closers) - 1; i >= 0; i-- {
		f.closers[i]()
	}
	f.closers = nil
}

// frontDeps is what openFront reads and reports to.
type frontDeps struct {
	cfg     *config.Config
	log     *slog.Logger
	health  *obs.Health
	metrics *obs.Metrics
	stdout  io.Writer
	// relay is the revocation state the relay shares with the call routes (dillad.Options.Relay).
	relay *server.RelayRevocations
}

// openFront binds the listeners tls.mode chooses and starts what sits behind
// them. runCtx bounds the background ACME issuance.
//
//   - behind_proxy: only server.plain_listen. certmagic is not constructed;
//     the proxy terminates TLS and X-Forwarded-For is read against
//     server.trusted_proxy_cidrs. TURN, when turn.enabled, is its own plain
//     TCP listener on turn.listen — never 443, where the proxy routes only
//     HTTP — optionally behind a PROXY protocol header from a trusted proxy.
//   - acme_tls_alpn, acme_dns, acme_ip: server.listen through the 443 demux,
//     which terminates TLS with certmagic's certificate and hands STUN to the
//     TURN relay and everything else to HTTP. The certificate is obtained in
//     the background once the listener is up, because TLS-ALPN-01 is answered
//     on it; the tls readiness gate is red until one is held.
//
// The tls gate is touched only in the direct-TLS modes; otherwise it keeps
// what the composition root set.
func openFront(ctx, runCtx context.Context, d frontDeps) (*front, error) {
	f := &front{}
	var err error
	if d.cfg.BehindProxy() {
		err = openBehindProxy(ctx, d, f)
	} else {
		err = openDirectTLS(ctx, runCtx, d, f)
	}
	if err != nil {
		// Whatever did start is released: a half-open front would leave the TURN
		// relay or the demux's listener bound past a failed start.
		f.close()
		return nil, err
	}
	return f, nil
}

// listen binds addr and, when it asked the kernel for a port, prints which
// one it got: there is nowhere else an operator (or a test) can read it.
func listen(ctx context.Context, d frontDeps, what, addr string) (net.Listener, error) {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("serve: listen %s %s: %w: %w", what, addr, err, exit.Unavailable)
	}
	if strings.HasSuffix(addr, ":0") {
		fmt.Fprintln(d.stdout, ln.Addr().String())
	}
	return ln, nil
}

func openBehindProxy(ctx context.Context, d frontDeps, f *front) error {
	ln, err := listen(ctx, d, "server.plain_listen", d.cfg.Server.PlainListen)
	if err != nil {
		return err
	}
	f.http = ln
	f.closers = append(f.closers, func() { _ = ln.Close() })
	if !d.cfg.TURN.Enabled {
		return nil
	}
	tln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", d.cfg.TURN.Listen)
	if err != nil {
		return fmt.Errorf("serve: listen turn.listen %s: %w: %w", d.cfg.TURN.Listen, err, exit.Unavailable)
	}
	if d.cfg.TURN.ProxyProtocol {
		tln = proxyProtocolListener(tln, d.cfg.Server.TrustedProxyCIDRs)
	}
	return startTURN(d, f, tln)
}

// proxyProtocolListener requires a PROXY protocol header (v1 or v2) from a
// peer inside trusted and refuses every other peer: TURN is a byte stream with
// no X-Forwarded-For, so this header is the only way a relay behind a TCP proxy
// learns the client's address, and an untrusted peer must not be able to set
// it.
func proxyProtocolListener(ln net.Listener, trusted []netip.Prefix) net.Listener {
	return &proxyproto.Listener{
		Listener:          ln,
		ReadHeaderTimeout: handshakeTimeout,
		ConnPolicy: func(o proxyproto.ConnPolicyOptions) (proxyproto.Policy, error) {
			if ap, perr := netip.ParseAddrPort(o.Upstream.String()); perr == nil {
				for _, p := range trusted {
					if p.Contains(ap.Addr().Unmap()) {
						return proxyproto.REQUIRE, nil
					}
				}
			}
			return proxyproto.REJECT, nil
		},
	}
}

func openDirectTLS(ctx, runCtx context.Context, d frontDeps, f *front) error {
	domain := d.cfg.Instance.Domain
	if d.cfg.TLS.Mode == config.TLSModeACMEIP {
		domain = d.cfg.Instance.PublicIP.String()
	}
	domains := []string{domain}
	manageCtx, stopManage := context.WithCancel(runCtx)
	f.closers = append(f.closers, stopManage)
	tl, tlsCfg, err := server.BuildTLS(manageCtx, d.cfg.TLS, domains)
	if err != nil {
		return fmt.Errorf("serve: %w: %w", err, exit.Config)
	}
	f.closers = append(f.closers, tl.Close)
	tl.SetHooks(func(msg string) {
		// certmagic keeps serving the last certificate; this is the alert.
		d.metrics.CertRenewalFailures.Inc()
		d.log.Error("ACME issuance or renewal failed; the existing certificate, if any, is still served",
			"name", domain, "err", msg)
	}, func() {
		d.log.Info("certificate obtained", "name", domain)
	})

	raw, err := listen(ctx, d, "server.listen", d.cfg.Server.Listen)
	if err != nil {
		return err
	}
	dm := server.NewDemux(raw, tlsCfg, handshakeTimeout)
	go dm.Serve()
	f.closers = append(f.closers, func() { _ = dm.Close() })
	f.http = dm.HTTP()
	if d.cfg.TURN.Enabled {
		if err := startTURN(d, f, dm.TURN()); err != nil {
			return err
		}
	} else {
		// No relay: the demux closes every STUN connection it would route here.
		_ = dm.TURN().Close()
	}

	gate := d.health.Gate("tls")
	gate.Set(false, "obtaining a certificate for "+domain)
	go manageCertificate(manageCtx, tl, domains, gate, d.log)
	return nil
}

// manageCertificate runs the ACME round trip until a certificate is held,
// retrying with a back-off: a first start whose DNS has not propagated yet, or
// whose port 443 is not forwarded yet, must not need a restart once it is.
// From then on certmagic's cache renews in the background, and a failed
// renewal keeps the gate green — the existing certificate is still valid, and
// the alert is the failure hook, not taking the instance out of rotation.
func manageCertificate(ctx context.Context, tl *server.TLS, domains []string, gate *obs.Gate, log *slog.Logger) {
	backoff := time.Minute
	for {
		err := tl.Manage(ctx, domains)
		if err == nil {
			gate.Set(true, "certificate held for "+domains[0])
			return
		}
		if ctx.Err() != nil {
			return
		}
		gate.Set(false, "no certificate for "+domains[0]+" yet: "+err.Error())
		log.Error("obtaining a certificate failed; retrying", "name", domains[0], "err", err, "retry_in", backoff)
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		backoff = min(2*backoff, time.Hour)
	}
}

// hostInterface is one network interface of this host: its flags and its addresses.
type hostInterface struct {
	flags net.Flags
	addrs []net.Addr
}

// hostInterfaces lists this host's interfaces for startSFU and turnPeers as LiveKit's UDP mux reads
// them (pion/transport stdnet UpdateInterfaces: every interface with its addresses, failing as a
// whole when one's addresses cannot be read); a test replaces it.
var hostInterfaces = func() ([]hostInterface, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]hostInterface, 0, len(ifs))
	for _, ifc := range ifs {
		addrs, err := ifc.Addrs()
		if err != nil {
			return nil, err
		}
		out = append(out, hostInterface{flags: ifc.Flags, addrs: addrs})
	}
	return out, nil
}

// sfuConfigError is a startSFU or turnPeers failure dilla.toml causes; serve exits 78 for it.
type sfuConfigError struct{ err error }

func (e sfuConfigError) Error() string { return e.err.Error() }
func (e sfuConfigError) Unwrap() error { return e.err }

// livekitPortRangeLo and livekitPortRangeHi are the UDP ports LiveKit v1.13.7 receives media on
// when livekit.udp_port is 0 outside development mode (mediatransportutil rtcconfig/config.go:96-105).
const livekitPortRangeLo, livekitPortRangeHi = 50000, 60000

// sfuListenAddrs is node_ip and exactly the addresses LiveKit's UDP mux listens on, which are the
// only addresses it gathers host candidates from (livekit/ice v4.4.0-warp.2 gather.go:526-553): one
// socket on livekit.udp_port per address localInterfaces keeps (mediatransportutil
// transport/createmux.go:50-58), which walks the interfaces as this does (transport/ip.go:26-91,
// v0.0.0-20260821083140-f234b534b095, the version livekit-server v1.13.7 builds with): it skips an
// interface that is down (:45-47) and, without the loopback candidate, a loopback interface (:48-50)
// and a loopback address on any other (:69-71); it keeps IPv6 only when isSupportedIPv6 does (:73-78,
// 95-104: not IPv4-compatible — which ::1 is —, site-local or link-local); and it drops an address
// livekit.ips_excludes covers (:83-85, the filter of rtcconfig/webrtc_config.go:75-82, 140-141).
// IPv4 link-local addresses are kept, as LiveKit keeps them; turnPeers drops them. local reports
// whether node_ip is an address of this host, on any interface.
func sfuListenAddrs(lk config.LiveKit, interfaces func() ([]hostInterface, error)) (node netip.Addr, listen []netip.Addr, local bool, err error) {
	sc := sfuConfig(lk, "")
	node, err = netip.ParseAddr(sc.NodeIP)
	if err != nil {
		return netip.Addr{}, nil, false, sfuConfigError{fmt.Errorf("livekit.node_ip %q is not an IP address: %w", sc.NodeIP, err)}
	}
	node = node.Unmap()
	var excludes []netip.Prefix
	for _, s := range lk.IPsExcludes {
		p, perr := netip.ParsePrefix(s)
		if perr != nil {
			return netip.Addr{}, nil, false, sfuConfigError{fmt.Errorf("livekit.ips_excludes entry %q is not a CIDR prefix: %w", s, perr)}
		}
		excludes = append(excludes, p.Masked())
	}
	ifs, err := interfaces()
	if err != nil {
		return netip.Addr{}, nil, false, fmt.Errorf("list this host's interface addresses: %w", err)
	}
	loopback := sc.EnableLoopbackCandidate
	siteLocal := netip.MustParsePrefix("fec0::/10")
	v4Compatible := netip.MustParsePrefix("::/96")
	for _, ifc := range ifs {
		listening := ifc.flags&net.FlagUp != 0 && (loopback || ifc.flags&net.FlagLoopback == 0)
		for _, a := range ifc.addrs {
			var raw net.IP
			switch a := a.(type) {
			case *net.IPNet:
				raw = a.IP
			case *net.IPAddr:
				raw = a.IP
			}
			ip, ok := netip.AddrFromSlice(raw)
			if !ok {
				continue
			}
			ip = ip.Unmap()
			if ip == node {
				local = true
			}
			if !listening || (ip.IsLoopback() && !loopback) {
				continue
			}
			if ip.Is6() && (v4Compatible.Contains(ip) || siteLocal.Contains(ip) || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()) {
				continue
			}
			excluded := false
			for _, p := range excludes {
				if p.Contains(ip) {
					excluded = true
					break
				}
			}
			if !excluded {
				listen = append(listen, ip)
			}
		}
	}
	return node, listen, local, nil
}

// turnPeers is the co-located SFU's media addresses and ports, the only peers the relay admits (C9,
// G34, branch review TURN-1 and TURN-2), and the family anchor turn.relay_ip "auto" stays in
// (livekit.node_ip, or loopback when it is unset). The ports are livekit.udp_port, or LiveKit's
// 50000-60000 range when it is 0; the relay is UDP only, so livekit.tcp_port is never a peer.
//
// An address is admitted only where LiveKit both offers a candidate and receives on it. LiveKit
// offers its listen addresses (sfuListenAddrs) and node_ip in place of each (advertise_internal_ip
// false) or beside each (true), through its NAT 1:1 rewrite (rtcconfig/webrtc_config.go:112-113,
// 257-286) — and nothing at all when ips_excludes leaves no listen address, which startSFU refuses.
// So node_ip is admitted when it is a listen address of this host — the relay then reaches the SFU
// on-host — or when advertise_internal_ip is false, which makes it the SFU's only candidate: relayed
// media then leaves the host for node_ip and comes back (a hairpin through the router), and a
// warning says so. A node_ip of this host that ips_excludes covers is offered but not listened on,
// and is not admitted. With advertise_internal_ip and a node_ip that is not local (a public IP
// behind NAT), node_ip is NOT admitted — the browser pairs the relay with the host candidates
// instead, and a permission for node_ip would only open a path through the router — and the relay
// admits the listen addresses. An IPv4 link-local address is never admitted, though LiveKit listens
// on one: the relay never needs it, and 169.254.169.254 is a cloud's metadata service. With LiveKit
// off there is no SFU and no peer.
func turnPeers(cfg *config.Config, interfaces func() ([]hostInterface, error), log *slog.Logger) (netip.Addr, server.TURNPeers, error) {
	lk := cfg.LiveKit
	if !lk.Enabled {
		return netip.Addr{}, server.TURNPeers{}, nil
	}
	node, listen, local, err := sfuListenAddrs(lk, interfaces)
	if err != nil {
		return netip.Addr{}, server.TURNPeers{}, err
	}
	if lk.UDPPort < 0 || lk.UDPPort > 65535 {
		return netip.Addr{}, server.TURNPeers{}, sfuConfigError{fmt.Errorf("livekit.udp_port is %d; the range is 0..65535", lk.UDPPort)}
	}
	peers := server.TURNPeers{PortLo: uint16(lk.UDPPort), PortHi: uint16(lk.UDPPort)}
	if lk.UDPPort == 0 {
		peers.PortLo, peers.PortHi = livekitPortRangeLo, livekitPortRangeHi
	}
	seen := map[netip.Addr]bool{}
	add := func(a netip.Addr) {
		if a.IsValid() && !a.IsLinkLocalUnicast() && !seen[a] {
			seen[a] = true
			peers.Addrs = append(peers.Addrs, a)
		}
	}
	switch {
	case len(listen) == 0:
		// LiveKit offers nothing; startSFU refuses this configuration before the relay starts.
	case local:
		if slices.Contains(listen, node) {
			add(node)
		}
	case !lk.AdvertiseInternalIP:
		add(node)
		log.Warn("livekit.node_ip is not an address of this host and the SFU's only candidate: relayed media hairpins out through it and back",
			"node_ip", node.String())
	default:
		log.Warn("livekit.node_ip is not an address of this host, so the relay does not admit it; relayed media pairs with the host addresses LiveKit also offers",
			"node_ip", node.String())
	}
	if lk.AdvertiseInternalIP {
		for _, ip := range listen {
			add(ip)
		}
	}
	return node, peers, nil
}

// checkSFUConfig refuses, as configuration faults (exit 78, which the unit does not restart), what
// would make LiveKit fail to start or start with no usable candidate (branch review SFU-4, SFU-7):
// what sfu.Config.YAML refuses (an api_key outside [A-Za-z0-9_-], a short api secret, a malformed
// stun server or ips_excludes entry), a node_ip that is no IP address, a port outside its range,
// livekit.port and livekit.tcp_port sharing a number, livekit.webhook_listen on livekit.tcp_port or
// on livekit.port at an address livekit.bind_address overlaps (listenHostsOverlap), and an
// ips_excludes that leaves LiveKit no address to receive media on. Whatever sfu.Start fails on after
// this is the host's — a port another process holds, a LiveKit internal error — and stays exit 69.
func checkSFUConfig(lk config.LiveKit, sc sfu.Config, interfaces func() ([]hostInterface, error)) error {
	if _, err := sc.YAML(); err != nil {
		return sfuConfigError{fmt.Errorf("livekit: %w", err)}
	}
	if lk.Port < 1 || lk.Port > 65535 {
		return sfuConfigError{fmt.Errorf("livekit.port is %d; the range is 1..65535", lk.Port)}
	}
	if lk.UDPPort < 0 || lk.UDPPort > 65535 {
		return sfuConfigError{fmt.Errorf("livekit.udp_port is %d; the range is 0..65535 (0: LiveKit's 50000-60000)", lk.UDPPort)}
	}
	if lk.TCPPort < 0 || lk.TCPPort > 65535 {
		return sfuConfigError{fmt.Errorf("livekit.tcp_port is %d; the range is 0..65535 (0: off)", lk.TCPPort)}
	}
	if lk.TCPPort == lk.Port {
		return sfuConfigError{fmt.Errorf("livekit.tcp_port and livekit.port are both %d", lk.Port)}
	}
	if host, p, err := net.SplitHostPort(lk.WebhookListen); err == nil {
		wp, err := strconv.Atoi(p)
		switch {
		case err != nil:
		case lk.TCPPort != 0 && wp == lk.TCPPort:
			// LiveKit's TCP mux listens on every address.
			return sfuConfigError{fmt.Errorf("livekit.webhook_listen's port %d is also livekit.tcp_port", wp)}
		case wp == lk.Port && listenHostsOverlap(host, lk.BindAddress):
			return sfuConfigError{fmt.Errorf("livekit.webhook_listen %s is also livekit.port %d on livekit.bind_address %s",
				lk.WebhookListen, lk.Port, lk.BindAddress)}
		}
	}
	node, listen, _, err := sfuListenAddrs(lk, interfaces)
	if err != nil {
		return err
	}
	if len(listen) == 0 {
		return sfuConfigError{fmt.Errorf("livekit: no address of this host is left for LiveKit to receive media on "+
			"(livekit.ips_excludes %q, livekit.node_ip %s), so it would offer no candidate and every call would fail; "+
			"remove the exclude that covers the address the SFU needs", lk.IPsExcludes, node)}
	}
	return nil
}

// listenHostsOverlap reports whether TCP listeners on hosts a and b and one port would collide.
// LiveKit listens on livekit.port with net.Listen("tcp", bind_address:port) (livekit-server v1.13.7
// pkg/service/server.go:246), so a different address on the same port is no collision (re-review
// RR-7). An empty or unspecified host is a dual-stack wildcard in Go and overlaps every address;
// localhost may be either loopback address; any other name is assumed to overlap.
func listenHostsOverlap(a, b string) bool {
	hosts := func(h string) (addrs []netip.Addr, all bool) {
		if h == "" {
			return nil, true
		}
		if strings.EqualFold(h, "localhost") {
			return []netip.Addr{netip.AddrFrom4([4]byte{127, 0, 0, 1}), netip.IPv6Loopback()}, false
		}
		ip, err := netip.ParseAddr(h)
		if err != nil {
			return nil, true
		}
		return []netip.Addr{ip.Unmap()}, ip.IsUnspecified()
	}
	as, aAll := hosts(a)
	bs, bAll := hosts(b)
	if aAll || bAll {
		return true
	}
	for _, x := range as {
		if slices.Contains(bs, x) {
			return true
		}
	}
	return false
}

// turnStartError is serve's exit status for a relay that did not start: 78 (exit.Config, which the
// unit's RestartPreventExitStatus leaves stopped) only when dilla.toml is at fault, and 69
// (exit.Unavailable, restarted) for the host's network — a sandbox that blocks netlink, a bind.
func turnStartError(err error) error {
	if server.IsTURNConfigError(err) || isSFUConfigError(err) {
		return fmt.Errorf("serve: %w: %w", err, exit.Config)
	}
	return fmt.Errorf("serve: %w: %w", err, exit.Unavailable)
}

func isSFUConfigError(err error) bool {
	var ce sfuConfigError
	return errors.As(err, &ce)
}

// relayMetricsFor is the relay's metric surface as startTURN builds it: dillad's own *obs.Metrics,
// which holds every relay series — the backstops (capacity refusals, peer drops, cut overflows)
// included — on dillad's registry, the one /metrics serves (serve.go newMetrics; I-1 of the
// integration re-review). They are registered once, with the Metrics, so a relay restarted inside
// one process keeps counting on the same series.
func relayMetricsFor(m *obs.Metrics) server.TURNMetrics {
	return m
}

func startTURN(d frontDeps, f *front, ln net.Listener) error {
	anchor, peers, err := turnPeers(d.cfg, hostInterfaces, d.log)
	if err != nil {
		_ = ln.Close()
		return turnStartError(fmt.Errorf("turn: %w", err))
	}
	m := relayMetricsFor(d.metrics)
	t, err := server.StartTURN(d.cfg.TURN, ln, anchor, peers, m, d.relay, clock.System(), d.log)
	if err != nil {
		_ = ln.Close()
		return turnStartError(err)
	}
	f.closers = append(f.closers, func() { _ = t.Close() })
	return nil
}

// sfuConfig maps [livekit] onto the SFU's config: every key config.Validate accepts reaches LiveKit
// (livekit.max_voice_participants as the room cap) or the call routes (livekit.max_publishers is
// the publisher lease's, callsConfig), and the reserved ones Validate refuses (extra_config_file,
// use_external_ip) have nowhere to go. An unset node_ip is loopback, which needs the loopback
// candidate. room.auto_create is false (DEV-44, MD-16): the call route opens each room with
// CreateRoom before it mints a token for it.
func sfuConfig(lk config.LiveKit, secret string) sfu.Config {
	nodeIP := lk.NodeIP
	if nodeIP == "" {
		nodeIP = "127.0.0.1"
	}
	loopback := false
	if a, err := netip.ParseAddr(nodeIP); err == nil {
		loopback = a.IsLoopback()
	}
	return sfu.Config{
		Port:        lk.Port,
		BindAddress: lk.BindAddress,
		NodeIP:      nodeIP,
		UDPPort:     lk.UDPPort,
		TCPPort:     lk.TCPPort,
		// On a loopback-only node LiveKit gathers no host candidate without it
		// (internal/sfu's package comment).
		EnableLoopbackCandidate: loopback,
		APIKey:                  lk.APIKey,
		APISecret:               strings.TrimSpace(secret),
		AdvertiseInternalIP:     lk.AdvertiseInternalIP,
		STUNServers:             lk.STUNServers,
		MaxParticipants:         uint32(max(lk.MaxVoiceParticipants, 0)), //nolint:gosec // clamped at 0
		AutoCreate:              false,
		EmptyTimeout:            300,
		DepartureTimeout:        20,
		VP9:                     lk.VP9,
		WebhookURL:              webhookURL(lk.WebhookListen),
		LimitNumTracks:          int32(lk.LimitNumTracks),     //nolint:gosec // Validate bounds it to 0..MaxInt32
		LimitBytesPerSec:        float32(lk.LimitBytesPerSec), // LiveKit's config type
		IPsExcludes:             lk.IPsExcludes,
	}
}

func webhookURL(listen string) string {
	if listen == "" {
		return ""
	}
	return "http://" + listen + sfu.WebhookPath
}

// listenWebhook binds livekit.webhook_listen before the SFU starts, when LiveKit is enabled, and
// writes the bound address back into the config: sfuConfig renders LiveKit's webhook URL from it, so
// a port 0 (tests) reaches LiveKit as the port the kernel chose. serveWebhook serves the listener
// once the composition root exists; until then LiveKit's POSTs wait in the accept queue.
func listenWebhook(ctx context.Context, d frontDeps) (net.Listener, error) {
	lk := d.cfg.LiveKit
	if !lk.Enabled || lk.WebhookListen == "" {
		return nil, nil
	}
	ln, err := listen(ctx, d, "livekit.webhook_listen", lk.WebhookListen)
	if err != nil {
		return nil, err
	}
	d.cfg.LiveKit.WebhookListen = ln.Addr().String()
	return ln, nil
}

// serveWebhook serves exactly POST /livekit/webhook on ln (SP-21): sfu.NewWebhookHandler verifies
// under the SFU's own key and hands each event to sink on its worker. The returned stop shuts the
// listener down and drains the worker. A nil ln (LiveKit off) serves nothing.
func serveWebhook(d frontDeps, ln net.Listener, sink func(context.Context, *livekit.WebhookEvent)) (func(), error) {
	if ln == nil {
		return func() {}, nil
	}
	body, err := os.ReadFile(d.cfg.LiveKit.APISecretFile)
	if err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("serve: livekit.api_secret_file: %w: %w", err, exit.Config)
	}
	h := sfu.NewWebhookHandler(d.cfg.LiveKit.APIKey, strings.TrimSpace(string(body)), sink, d.log)
	mux := http.NewServeMux()
	mux.Handle("POST "+sfu.WebhookPath, h)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: handshakeTimeout}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			d.log.Error("the LiveKit webhook listener stopped", "err", err)
		}
	}()
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		if c, ok := h.(io.Closer); ok {
			_ = c.Close()
		}
	}, nil
}

// watchSFU reports a post-boot SFU exit and marks readiness red.
func watchSFU(ctx context.Context, done <-chan error, gate *obs.Gate, log *slog.Logger) <-chan error {
	out := make(chan error, 1)
	go func() {
		defer close(out)
		select {
		case err := <-done:
			gate.Set(false, "the in-process SFU exited: "+err.Error())
			log.Error("the in-process SFU exited; dillad exits for its supervisor to restart it", "err", err)
			out <- err
		case <-ctx.Done():
		}
	}()
	return out
}

// startSFU runs LiveKit v1.13.7 in this process through internal/sfu, on
// livekit.bind_address, when livekit.enabled. Its readiness gate is red until
// the SFU accepts connections. A failure stops serve: an instance configured
// for voice that silently has none is worse than one that says why it did not
// start.
//
// It runs before the composition root, which builds the call routes over the
// SFU's token mint and proxies /rtc to it, and returns the running SFU (nil
// when livekit.enabled is false) with the function that stops it; serve runs
// that after the HTTP drain.
func startSFU(ctx context.Context, d frontDeps) (*sfu.Server, func(), error) {
	lk := d.cfg.LiveKit
	if !lk.Enabled {
		return nil, func() {}, nil
	}
	if lk.Mode != "in_process" {
		return nil, nil, fmt.Errorf("serve: livekit.mode %q is not supported; the one mode is in_process: %w", lk.Mode, exit.Config)
	}
	gate := d.health.Gate("livekit")
	gate.Set(false, "starting the in-process SFU")
	body, err := os.ReadFile(lk.APISecretFile)
	if err != nil {
		return nil, nil, fmt.Errorf("serve: livekit.api_secret_file: %w: %w", err, exit.Config)
	}
	sc := sfuConfig(lk, string(body))
	sc.Log = d.log
	if err := checkSFUConfig(lk, sc, hostInterfaces); err != nil {
		gate.Set(false, err.Error())
		code := exit.Unavailable // the host's interface list could not be read
		if isSFUConfigError(err) {
			code = exit.Config
		}
		return nil, nil, fmt.Errorf("serve: %w: %w", err, code)
	}
	s, err := sfu.Start(ctx, sc)
	if err != nil {
		gate.Set(false, err.Error())
		return nil, nil, fmt.Errorf("serve: %w: %w", err, exit.Unavailable)
	}
	gate.Set(true, "in-process SFU on "+s.URL())
	return s, func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := s.Stop(stopCtx); err != nil && !errors.Is(err, context.Canceled) {
			d.log.Warn("stopping the SFU", "err", err)
		}
	}, nil
}
