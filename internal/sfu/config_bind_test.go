package sfu

import (
	"errors"
	"net"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/livekit/livekit-server/pkg/config"

	"github.com/dilla-chat/dilla/internal/sfu/sfutest"
)

// Branch review SFU-3: YAML renders the bind address quoted, so it cannot leave its own line, and
// refuses one that is not a loopback IP literal — the /rtc gate is the only way into LiveKit, and a
// hostname binds one resolved address while dillad dials another.
func TestTheBindAddressIsALoopbackLiteralRenderedQuoted(t *testing.T) {
	for _, bad := range []string{"localhost", "0.0.0.0", "::", "192.168.1.5", "127.0.0.1:7880",
		"127.0.0.1\n  - 0.0.0.0", "fe80::1%eth0", ""} {
		c := testConfig()
		c.BindAddress = bad
		if _, err := c.YAML(); err == nil {
			t.Errorf("bind address %q rendered", bad)
		}
	}
	for _, good := range []string{"127.0.0.1", "::1", "127.0.0.2"} {
		c := testConfig()
		c.BindAddress = good
		y, err := c.YAML()
		if err != nil {
			t.Fatalf("bind address %q: %v", good, err)
		}
		if !strings.Contains(y, "bind_addresses:\n  - \""+good+"\"\n") {
			t.Errorf("bind address %q not rendered quoted on its own line:\n%s", good, y)
		}
		conf, err := config.NewConfig(y, true, nil, nil)
		if err != nil {
			t.Fatalf("LiveKit refuses the rendered YAML for %q: %v", good, err)
		}
		if len(conf.BindAddresses) != 1 || conf.BindAddresses[0] != good {
			t.Errorf("LiveKit reads bind addresses %v, want [%s]", conf.BindAddresses, good)
		}
	}
}

// Parked item P1 ("SFU readiness decided by a TCP dial"): a squatter on the bind address cannot make
// Start report ready. LiveKit's own net.Listen on the literal address fails first (server.go:246-251),
// so Start fails with that EADDRINUSE within startTimeout and nothing it started is left listening.
func TestASquattedPortFailsStart(t *testing.T) {
	c := testConfig()
	c.Port, c.UDPPort = sfutest.FreePorts(t)
	squat, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", net.JoinHostPort(c.BindAddress, strconv.Itoa(c.Port)))
	if err != nil {
		t.Fatalf("squat: %v", err)
	}
	defer squat.Close()
	begin := time.Now()
	srv, err := Start(t.Context(), c)
	if err == nil {
		_ = srv.Stop(t.Context())
		t.Fatal("Start reported ready on a squatted port")
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Errorf("Start = %v, want an error wrapping EADDRINUSE", err)
	}
	if d := time.Since(begin); d > startTimeout {
		t.Errorf("Start took %s, past its %s timeout", d, startTimeout)
	}
}
