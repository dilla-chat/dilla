package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/exit"
	"github.com/dilla-chat/dilla/internal/obs"
)

// fakeIfaces is an interface list for turnPeers and startSFU: each address on an up interface of
// its own, flagged loopback when the address is.
func fakeIfaces(addrs ...string) func() ([]hostInterface, error) {
	var out []hostInterface
	for _, a := range addrs {
		flags := net.FlagUp
		if ip, _, err := net.ParseCIDR(a); err == nil && ip.IsLoopback() {
			flags |= net.FlagLoopback
		}
		out = append(out, iface(flags, a))
	}
	return func() ([]hostInterface, error) { return out, nil }
}

// iface is one interface with flags and the CIDR addresses addrs.
func iface(flags net.Flags, addrs ...string) hostInterface {
	h := hostInterface{flags: flags}
	for _, a := range addrs {
		ip, n, err := net.ParseCIDR(a)
		if err != nil {
			panic(err)
		}
		h.addrs = append(h.addrs, &net.IPNet{IP: ip, Mask: n.Mask})
	}
	return h
}

// Re-review RR-5: the relay admits, and startSFU's check counts, only addresses LiveKit's UDP mux
// really listens on. Like mediatransportutil's localInterfaces it skips a down interface, a loopback
// interface without the loopback candidate (whatever address it carries), and ::1 even with it (an
// IPv4-compatible address to isSupportedIPv6); with the candidate, a loopback interface's other
// addresses are kept. IPv4 link-local is listened on but never admitted.
func TestTheRelayPeersFollowLiveKitsInterfaceWalk(t *testing.T) {
	ifaces := func() ([]hostInterface, error) {
		return []hostInterface{
			iface(net.FlagUp|net.FlagLoopback, "127.0.0.1/8", "::1/128", "10.9.9.9/32"),
			iface(net.FlagUp, "10.0.0.5/24", "169.254.1.1/16", "2001:db8::5/64"),
			iface(0, "192.168.5.5/24", "2001:db8::9/64"), // down
		}, nil
	}
	log := slog.New(slog.DiscardHandler)
	for _, tc := range []struct {
		name, nodeIP, listen, peers string
	}{
		{"the loopback candidate (no node_ip)", "", "[127.0.0.1 10.9.9.9 10.0.0.5 169.254.1.1 2001:db8::5]",
			"[127.0.0.1 10.9.9.9 10.0.0.5 2001:db8::5]"},
		{"a host node_ip", "10.0.0.5", "[10.0.0.5 169.254.1.1 2001:db8::5]", "[10.0.0.5 2001:db8::5]"},
		{"a node_ip on a down interface", "192.168.5.5", "[10.0.0.5 169.254.1.1 2001:db8::5]", "[10.0.0.5 2001:db8::5]"},
	} {
		cfg := config.Default()
		cfg.LiveKit.NodeIP, cfg.LiveKit.AdvertiseInternalIP = tc.nodeIP, true
		_, listen, _, err := sfuListenAddrs(cfg.LiveKit, ifaces)
		if err != nil {
			t.Fatalf("%s: sfuListenAddrs: %v", tc.name, err)
		}
		if got := fmt.Sprint(listen); got != tc.listen {
			t.Errorf("%s: LiveKit listens on %s, want %s", tc.name, got, tc.listen)
		}
		_, peers, err := turnPeers(cfg, ifaces, log)
		if err != nil {
			t.Fatalf("%s: turnPeers: %v", tc.name, err)
		}
		if got := fmt.Sprint(peers.Addrs); got != tc.peers {
			t.Errorf("%s: the relay admits %s, want %s", tc.name, got, tc.peers)
		}
	}
	// Only a down interface's address escapes the excludes: LiveKit has no address, and that is
	// refused before it starts.
	lk := config.Default().LiveKit
	lk.Enabled, lk.NodeIP, lk.IPsExcludes = true, "10.0.0.5", []string{"10.0.0.0/8", "2001:db8::/32"}
	err := checkSFUConfig(lk, sfuConfig(lk, strings.Repeat("s", 40)), func() ([]hostInterface, error) {
		return []hostInterface{iface(net.FlagUp, "10.0.0.5/24", "2001:db8::5/64"), iface(0, "192.168.5.5/24")}, nil
	})
	if !isSFUConfigError(err) || !strings.Contains(err.Error(), "no address of this host is left") {
		t.Fatalf("checkSFUConfig with only a down interface's address left = %v, want the no-address refusal", err)
	}
}

// Re-review RR-7: livekit.webhook_listen collides with livekit.port only on an address LiveKit's
// listener on bind_address overlaps, and with livekit.tcp_port on any address (LiveKit's TCP mux
// listens on all). Nothing is bound: checkSFUConfig only reads the configuration. bind_address is
// always a loopback IP literal here, as LiveKit's listener must be.
func TestTheWebhookPortCollidesOnlyWhereLiveKitListens(t *testing.T) {
	for _, tc := range []struct {
		webhook, bind string
		tcpPort       int
		refused       bool
	}{
		{"[::1]:7880", "127.0.0.1", 0, false},
		{"127.0.0.2:7880", "127.0.0.1", 0, false},
		{"127.0.0.1:7880", "::1", 0, false},
		{"127.0.0.1:7880", "127.0.0.1", 0, true},
		{"localhost:7880", "127.0.0.1", 0, true},
		{"localhost:7880", "::1", 0, true},
		{"[::1]:7880", "::1", 0, true},
		{"[::1]:7881", "127.0.0.1", 7881, true},
		{"127.0.0.1:7883", "127.0.0.1", 7881, false},
	} {
		lk := config.Default().LiveKit
		lk.Enabled, lk.Port, lk.TCPPort, lk.BindAddress, lk.WebhookListen = true, 7880, tc.tcpPort, tc.bind, tc.webhook
		err := checkSFUConfig(lk, sfuConfig(lk, strings.Repeat("s", 40)), fakeIfaces("10.0.0.5/24"))
		if refused := isSFUConfigError(err) && strings.Contains(err.Error(), "webhook_listen"); refused != tc.refused || (!tc.refused && err != nil) {
			t.Errorf("webhook_listen %s, bind_address %s, tcp_port %d: checkSFUConfig = %v; want refused %v",
				tc.webhook, tc.bind, tc.tcpPort, err, tc.refused)
		}
	}
}

// Branch review TURN-2 / SFU-4: the relay admits only addresses LiveKit both offers and listens on,
// so livekit.ips_excludes removes an address from the relay's peers as it does from LiveKit's UDP
// mux; a node_ip of this host that is excluded is offered by LiveKit's rewrite but not listened on,
// and is not admitted. The relay's ports are livekit.udp_port, or LiveKit's 50000-60000 range for 0.
func TestTheRelayPeersHonourIPsExcludes(t *testing.T) {
	ifaces := fakeIfaces("10.0.0.5/24", "172.17.0.2/16", "127.0.0.1/8", "fe80::1/64", "2001:db8::5/64", "fec0::7/64")
	log := slog.New(slog.DiscardHandler)
	for _, tc := range []struct {
		name      string
		nodeIP    string
		advertise bool
		excludes  []string
		udpPort   int
		peers     string
		lo, hi    uint16
	}{
		{"no excludes", "10.0.0.5", true, nil, 7882, "[10.0.0.5 172.17.0.2 2001:db8::5]", 7882, 7882},
		{"a bridge range excluded", "10.0.0.5", true, []string{"172.16.0.0/12"}, 7882, "[10.0.0.5 2001:db8::5]", 7882, 7882},
		{"node_ip itself excluded", "10.0.0.5", true, []string{"10.0.0.0/8"}, 7882, "[172.17.0.2 2001:db8::5]", 7882, 7882},
		{"a public node_ip with the bridge excluded", "203.0.113.7", true, []string{"172.16.0.0/12", "2001:db8::/32"}, 7882, "[10.0.0.5]", 7882, 7882},
		{"a public node_ip as the only candidate", "203.0.113.7", false, []string{"172.16.0.0/12"}, 7882, "[203.0.113.7]", 7882, 7882},
		{"everything excluded: LiveKit offers nothing", "203.0.113.7", false, []string{"0.0.0.0/0", "::/0"}, 7882, "[]", 7882, 7882},
		{"udp_port 0 is LiveKit's range", "10.0.0.5", false, nil, 0, "[10.0.0.5]", 50000, 60000},
	} {
		cfg := config.Default()
		cfg.LiveKit.NodeIP, cfg.LiveKit.AdvertiseInternalIP = tc.nodeIP, tc.advertise
		cfg.LiveKit.IPsExcludes, cfg.LiveKit.UDPPort = tc.excludes, tc.udpPort
		_, peers, err := turnPeers(cfg, ifaces, log)
		if err != nil {
			t.Fatalf("%s: turnPeers: %v", tc.name, err)
		}
		if got := fmt.Sprint(peers.Addrs); got != tc.peers || peers.PortLo != tc.lo || peers.PortHi != tc.hi {
			t.Errorf("%s: peers %s on %d-%d; want %s on %d-%d", tc.name, got, peers.PortLo, peers.PortHi, tc.peers, tc.lo, tc.hi)
		}
	}
}

// sfuDeps is a frontDeps for startSFU with LiveKit enabled on a valid secret.
func sfuDeps(t *testing.T, edit func(*config.LiveKit)) frontDeps {
	t.Helper()
	cfg := config.Default()
	secret := filepath.Join(t.TempDir(), "livekit.secret")
	if err := os.WriteFile(secret, []byte(strings.Repeat("s", 40)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.LiveKit.Enabled, cfg.LiveKit.APISecretFile = true, secret
	cfg.LiveKit.Port, cfg.LiveKit.UDPPort, cfg.LiveKit.WebhookListen = freeTCPPort(t), freeUDPPort(t), ""
	edit(&cfg.LiveKit)
	return frontDeps{cfg: cfg, log: slog.New(slog.DiscardHandler), health: obs.NewHealth(clock.System())}
}

// Branch review SFU-7 and SFU-4: what dilla.toml gets wrong in [livekit] stops serve with exit 78,
// which systemd does not restart; what the host gets wrong (a port another process holds) stays 69.
// An ips_excludes that leaves LiveKit no address to listen on — the bridged-Docker recipe the
// deploy README used to give — is such a configuration fault.
func TestStartSFUExitsConfigForConfigFaultsAndUnavailableForTheHost(t *testing.T) {
	bridged := fakeIfaces("127.0.0.1/8", "172.17.0.2/16")
	for _, tc := range []struct {
		name   string
		edit   func(*config.LiveKit)
		ifaces func() ([]hostInterface, error)
		want   error
		says   string
	}{
		{"an api_key outside [A-Za-z0-9_-]", func(lk *config.LiveKit) { lk.APIKey = "dilla key" }, nil, exit.Config, "api key"},
		{"a node_ip that is no address", func(lk *config.LiveKit) { lk.NodeIP = "sfu.example.org" }, nil, exit.Config, "node_ip"},
		{"tcp_port on livekit.port", func(lk *config.LiveKit) { lk.TCPPort = lk.Port }, nil, exit.Config, "tcp_port"},
		{"a port out of range", func(lk *config.LiveKit) { lk.UDPPort = 70000 }, nil, exit.Config, "udp_port"},
		{"the webhook on livekit.port", func(lk *config.LiveKit) { lk.WebhookListen = fmt.Sprintf("127.0.0.1:%d", lk.Port) }, nil, exit.Config, "webhook_listen"},
		{"a bridged container with its bridge excluded", func(lk *config.LiveKit) {
			lk.NodeIP, lk.IPsExcludes = "203.0.113.7", []string{"172.16.0.0/12"}
		}, bridged, exit.Config, "no address of this host is left"},
		{"the host's interfaces cannot be listed", func(*config.LiveKit) {},
			func() ([]hostInterface, error) { return nil, errors.New("netlink refused") }, exit.Unavailable, "netlink refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.ifaces != nil {
				prev := hostInterfaces
				hostInterfaces = tc.ifaces
				defer func() { hostInterfaces = prev }()
			}
			_, _, err := startSFU(t.Context(), sfuDeps(t, tc.edit))
			if !errors.Is(err, tc.want) || (!errors.Is(tc.want, exit.Config) && errors.Is(err, exit.Config)) ||
				!strings.Contains(err.Error(), tc.says) {
				t.Fatalf("startSFU = %v; want %v saying %q", err, tc.want, tc.says)
			}
		})
	}
	t.Run("livekit.port held by another process", func(t *testing.T) {
		held, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer held.Close()
		port := held.Addr().(*net.TCPAddr).Port
		s, stop, err := startSFU(t.Context(), sfuDeps(t, func(lk *config.LiveKit) { lk.Port = port }))
		if err == nil {
			stop()
			t.Fatalf("the SFU started on a held port (%v)", s.URL())
		}
		if !errors.Is(err, exit.Unavailable) || errors.Is(err, exit.Config) {
			t.Fatalf("startSFU on a held port = %v; want exit.Unavailable only", err)
		}
	})
}

// Branch review TURN-4 and the card 13 cap: the relay's backstop counters reach dillad's /metrics,
// label-free, on dillad's own registry, and a relay restarted in one process counts on the same
// series.
func TestTheRelayBackstopCountersAreRegistered(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	metrics := obs.NewMetrics(reg, reg)
	m := relayMetricsFor(metrics)
	m.CutOverflow()
	m.CapacityRefused()
	m.PeerDropped()
	m.PeerDropped()
	again := relayMetricsFor(metrics) // the relay restarted
	again.CutOverflow()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	got := map[string]float64{}
	for _, f := range families {
		switch f.GetName() {
		case "dilla_turn_cut_overflows_total", "dilla_turn_capacity_refusals_total", "dilla_turn_peer_drops_total":
		default:
			continue
		}
		if len(f.GetMetric()) != 1 || len(f.GetMetric()[0].GetLabel()) != 0 {
			t.Fatalf("%s: %d series; want one label-free series", f.GetName(), len(f.GetMetric()))
		}
		got[f.GetName()] = f.GetMetric()[0].GetCounter().GetValue()
	}
	want := map[string]float64{
		"dilla_turn_cut_overflows_total":     2,
		"dilla_turn_capacity_refusals_total": 1,
		"dilla_turn_peer_drops_total":        2,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("counters %v, want %v", got, want)
	}
}
