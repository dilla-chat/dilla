package dilladtest

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
)

// CI run 44520186466: a caller probed a free port, released it, and passed it on; another test took
// it before LiveKit bound it. With SFUAnyPorts the host picks the ports itself, so it can take that
// loss and start again on fresh ones: here the first pick is a port this test is holding (TCP for
// the signalling port, UDP for the ICE mux), and the host must still start, on other ports.
func TestAnAnyPortSFUStartsAgainOnFreshPortsWhenItsPickIsTaken(t *testing.T) {
	core := corePath(t)
	for _, proto := range []string{"tcp", "udp"} {
		t.Run(proto, func(t *testing.T) {
			var lc net.ListenConfig
			var held int
			if proto == "tcp" {
				ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatalf("hold a tcp port: %v", err)
				}
				defer func() { _ = ln.Close() }()
				held = ln.Addr().(*net.TCPAddr).Port
			} else {
				pc, err := lc.ListenPacket(t.Context(), "udp", "127.0.0.1:0")
				if err != nil {
					t.Fatalf("hold a udp port: %v", err)
				}
				defer func() { _ = pc.Close() }()
				held = pc.LocalAddr().(*net.UDPAddr).Port
			}

			picks := 0
			sfuPorts = func(ctx context.Context) (int, int, error) {
				picks++
				tcp, udp, err := freeSFUPorts(ctx)
				if picks == 1 && proto == "tcp" {
					tcp = held
				} else if picks == 1 {
					udp = held
				}
				return tcp, udp, err
			}
			defer func() { sfuPorts = freeSFUPorts }()

			h, err := NewHost(context.Background(), HostOptions{
				DataDir: t.TempDir(), CorePath: core, SFU: true, SFUAnyPorts: true,
			})
			if err != nil {
				t.Fatalf("NewHost with a taken first pick: %v", err)
			}
			defer func() { _ = h.Close(context.Background()) }()
			if picks != 2 {
				t.Errorf("the host picked ports %d times, want 2 (the taken pick, then a fresh one)", picks)
			}
			if proto == "tcp" && strings.HasSuffix(h.SFU().URL(), ":"+strconv.Itoa(held)) {
				t.Errorf("the SFU is at %s, the held port", h.SFU().URL())
			}
		})
	}
}

// A fixed port is the caller's choice: a taken one fails the start, once, naming the port, rather
// than moving the SFU somewhere the caller does not look.
func TestAFixedPortSFUDoesNotMoveOffATakenPort(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold a tcp port: %v", err)
	}
	defer func() { _ = ln.Close() }()
	held := ln.Addr().(*net.TCPAddr).Port
	_, udp, err := freeSFUPorts(t.Context())
	if err != nil {
		t.Fatalf("pick a udp port: %v", err)
	}
	_, err = NewHost(context.Background(), HostOptions{
		DataDir: t.TempDir(), CorePath: corePath(t), SFU: true, SFUPort: held, SFUUDPPort: udp,
	})
	if err == nil || !addrInUse(err) || !strings.Contains(err.Error(), "127.0.0.1:"+strconv.Itoa(held)) {
		t.Fatalf("NewHost on a taken fixed port: %v, want address already in use on %d", err, held)
	}
}
