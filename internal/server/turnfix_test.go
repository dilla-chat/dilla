package server_test

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/stun/v3"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
)

// udpSink is a UDP socket on addr that collects every datagram it receives.
func udpSink(t *testing.T, addr string) (net.PacketConn, func() []string) {
	t.Helper()
	pc, err := (&net.ListenConfig{}).ListenPacket(t.Context(), "udp4", addr)
	if err != nil {
		t.Fatalf("listen %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	var mu sync.Mutex
	var got []string
	go func() {
		buf := make([]byte, 1500)
		for {
			n, _, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			mu.Lock()
			got = append(got, string(buf[:n]))
			mu.Unlock()
		}
	}()
	return pc, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
}

// waitFor polls cond for up to five seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// readsNothing reports what relay delivers to its client within d, if anything.
func readsNothing(t *testing.T, relay net.PacketConn, d time.Duration) (string, bool) {
	t.Helper()
	_ = relay.SetReadDeadline(time.Now().Add(d))
	defer func() { _ = relay.SetReadDeadline(time.Time{}) }()
	buf := make([]byte, 1500)
	n, _, err := relay.ReadFrom(buf)
	if err != nil {
		return "", true
	}
	return string(buf[:n]), false
}

// Branch review TURN-1: relay sockets are bound on turn.relay_ip, which is an admitted peer address
// in every default deployment (it is the SFU's), and pion's permissions are by IP only. One member's
// relay must still not reach another device's relay allocation: only the SFU's media port is a
// peer. Both devices keep reaching the SFU, and its replies reach them. Run with the SFU on one port
// (livekit.udp_port) and with LiveKit's port range covering every port (udp_port 0), where only the
// relay's own sockets are refused.
func TestOneRelayAllocationCannotReachAnother(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		name  string
		wide  bool
		peers func(sfu net.PacketConn) server.TURNPeers
	}{
		{"the SFU's one media port", false, func(sfu net.PacketConn) server.TURNPeers { return sfuPeers(sfu) }},
		{"a port range over every port", true, func(sfu net.PacketConn) server.TURNPeers {
			return server.TURNPeers{Addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}, PortLo: 1, PortHi: 65535}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sfu, toSFU := udpSink(t, "127.0.0.1:0")
			other, toOther := udpSink(t, "127.0.0.1:0") // another UDP service on the SFU's address
			m := &fakeTURNMetrics{}
			addr := startMeteredTURN(t, secret, config.TURN{CredentialTTL: "1h", AllocationsPerDevice: 2},
				tc.peers(sfu), m, clock.System())
			userA, passA := server.TURNCredential(secret, id.New(), time.Hour, time.Now())
			userB, passB := server.TURNCredential(secret, id.New(), time.Hour, time.Now())
			relayA, err := allocateAs(t, addr, userA, passA)
			if err != nil {
				t.Fatalf("allocate A: %v", err)
			}
			relayB, err := allocateAs(t, addr, userB, passB)
			if err != nil {
				t.Fatalf("allocate B: %v", err)
			}
			// Both reach the SFU (which gives both a permission for 127.0.0.1, B's relay address).
			for _, w := range []struct {
				relay net.PacketConn
				msg   string
			}{{relayA, "media-A"}, {relayB, "media-B"}} {
				if _, err := w.relay.WriteTo([]byte(w.msg), sfu.LocalAddr()); err != nil {
					t.Fatalf("a write to the SFU: %v", err)
				}
			}
			waitFor(t, "both devices' media at the SFU", func() bool {
				got := toSFU()
				return slices.Contains(got, "media-A") && slices.Contains(got, "media-B")
			})
			// A floods B's relayed address; nothing reaches B's client.
			for range 5 {
				if _, err := relayA.WriteTo([]byte("from-A"), relayB.LocalAddr()); err != nil {
					t.Fatalf("A's write to B's relay: %v", err)
				}
			}
			if got, none := readsNothing(t, relayB, time.Second); !none {
				t.Fatalf("B's client received %q from A's relay", got)
			}
			// The SFU's replies still reach both clients.
			for _, r := range []net.PacketConn{relayA, relayB} {
				if _, err := sfu.WriteTo([]byte("sfu-reply"), r.LocalAddr()); err != nil {
					t.Fatalf("the SFU's reply: %v", err)
				}
				_ = r.SetReadDeadline(time.Now().Add(5 * time.Second))
				buf := make([]byte, 64)
				if n, _, err := r.ReadFrom(buf); err != nil || string(buf[:n]) != "sfu-reply" {
					t.Fatalf("a client read %q, %v; want the SFU's reply", buf[:n], err)
				}
				_ = r.SetReadDeadline(time.Time{})
			}
			if tc.wide {
				return
			}
			// Another service on the SFU's address but not on its media port: unreachable both ways.
			if _, err := relayA.WriteTo([]byte("to-other"), other.LocalAddr()); err != nil {
				t.Fatalf("A's write to the other port: %v", err)
			}
			if _, err := other.WriteTo([]byte("from-other"), relayA.LocalAddr()); err != nil {
				t.Fatalf("the other port's write: %v", err)
			}
			if got, none := readsNothing(t, relayA, time.Second); !none {
				t.Fatalf("A's client received %q from a port that is not the SFU's", got)
			}
			if got := toOther(); len(got) != 0 {
				t.Fatalf("the other service received %q through the relay", got)
			}
			// The relay counts a drop on its own read goroutine, so the count is waited for, not read.
			waitFor(t, "at least 7 dropped datagrams counted", func() bool {
				_, dropped, _ := m.extra()
				return dropped >= 7
			})
		})
	}
}

// stunAllocate sends one Allocate for a UDP relay with extra attributes over a plain TCP connection
// to addr, authenticated with user and pass after the 401 challenge, and answers the error code (0
// for a success).
func stunAllocate(t *testing.T, addr, user, pass string, extra ...stun.Setter) int {
	t.Helper()
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	return stunAllocateOn(t, conn, user, pass, extra...)
}

// stunAllocateOn is stunAllocate on an open connection, which it leaves open with no deadline.
func stunAllocateOn(t *testing.T, conn net.Conn, user, pass string, extra ...stun.Setter) int {
	t.Helper()
	udp := stun.RawAttribute{Type: stun.AttrRequestedTransport, Value: []byte{17, 0, 0, 0}}
	code, _, err := stunRequestOn(conn, stun.MethodAllocate, user, pass, append([]stun.Setter{udp}, extra...)...)
	if err != nil {
		t.Fatal(err)
	}
	return code
}

// stunRequestOn sends one method request with extra attributes on conn, authenticated with user and
// pass after the 401 challenge, and answers the error code (0 for a success) or the failure to
// exchange it, and the authentication attributes (username, realm, nonce, integrity) it signed the
// request with, which sign later requests on conn too. It leaves conn open with no deadline.
func stunRequestOn(conn net.Conn, method stun.Method, user, pass string, extra ...stun.Setter) (int, []stun.Setter, error) {
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	defer func() { _ = conn.SetDeadline(time.Time{}) }()
	exchange := func(setters ...stun.Setter) (*stun.Message, error) {
		req := stun.MustBuild(append([]stun.Setter{stun.TransactionID,
			stun.NewType(method, stun.ClassRequest)}, setters...)...)
		if _, err := conn.Write(req.Raw); err != nil {
			return nil, fmt.Errorf("write: %w", err)
		}
		hdr := make([]byte, 20)
		if _, err := io.ReadFull(conn, hdr); err != nil {
			return nil, fmt.Errorf("read header: %w", err)
		}
		body := make([]byte, binary.BigEndian.Uint16(hdr[2:4]))
		if _, err := io.ReadFull(conn, body); err != nil {
			return nil, fmt.Errorf("read body: %w", err)
		}
		res := &stun.Message{Raw: slices.Concat(hdr, body)}
		if err := res.Decode(); err != nil {
			return nil, fmt.Errorf("decode: %w", err)
		}
		return res, nil
	}
	challenge, err := exchange(extra...)
	if err != nil {
		return 0, nil, err
	}
	var nonce stun.Nonce
	var realm stun.Realm
	if err := nonce.GetFrom(challenge); err != nil {
		return 0, nil, fmt.Errorf("no nonce in the challenge: %w", err)
	}
	if err := realm.GetFrom(challenge); err != nil {
		return 0, nil, fmt.Errorf("no realm in the challenge: %w", err)
	}
	auth := []stun.Setter{stun.NewUsername(user), realm, nonce, stun.NewLongTermIntegrity(user, realm.String(), pass)}
	res, err := exchange(slices.Concat(extra, auth, []stun.Setter{stun.Fingerprint})...)
	if err != nil {
		return 0, nil, err
	}
	if res.Type.Class == stun.ClassSuccessResponse {
		return 0, auth, nil
	}
	var code stun.ErrorCodeAttribute
	if err := code.GetFrom(res); err != nil {
		return 0, nil, fmt.Errorf("an error response without a code: %w", err)
	}
	return int(code.Code), auth, nil
}

// Branch review TURN-3: EVEN-PORT and RESERVATION-TOKEN are refused 508 before pion binds a port or
// takes a quota slot: with a quota of one, the device still allocates a plain relay afterwards, and
// the gauge saw only that one. The plain relay's connection stays open to the end: a TCP
// allocation ends when its connection closes, and stunAllocate closes its own, so the gauge's
// history would also hold the asynchronous deletion's 0, or not, depending on when it is read.
func TestTheRelayRefusesEvenPortAndReservations(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	m := &fakeTURNMetrics{}
	addr := startMeteredTURN(t, secret, config.TURN{CredentialTTL: "1h", AllocationsPerDevice: 1},
		server.TURNPeers{}, m, clock.System())
	user, pass := server.TURNCredential(secret, id.New(), time.Hour, time.Now())
	if code := stunAllocate(t, addr, user, pass, stun.RawAttribute{Type: stun.AttrEvenPort, Value: []byte{0x80}}); code != 508 {
		t.Fatalf("an EVEN-PORT Allocate = %d, want 508", code)
	}
	if code := stunAllocate(t, addr, user, pass,
		stun.RawAttribute{Type: stun.AttrReservationToken, Value: []byte("abcdefgh")}); code != 508 {
		t.Fatalf("a RESERVATION-TOKEN Allocate = %d, want 508", code)
	}
	plain, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = plain.Close() })
	if code := stunAllocateOn(t, plain, user, pass); code != 0 {
		t.Fatalf("a plain Allocate after the refused ones = %d, want success (the slot must be free)", code)
	}
	// pion reports the creation before it answers the Allocate, so the 1 is in the history by now.
	if refused, _, _, allocations := m.snapshot(); refused != 0 || !slices.Equal(allocations, []int{1}) {
		t.Fatalf("quota refusals %d, allocation gauge %v; want 0 and only the plain relay", refused, allocations)
	}
}

// TURN-3, the generator itself: pion's EVEN-PORT probe (no user) and a reserved port are refused,
// no relay socket stays registered, and the reserved port's slot is given back.
func TestTheRelayGeneratorRefusesEvenPortProbesAndReservedPorts(t *testing.T) {
	sockets, freed, probeErr, reservedErr := server.ReservationRefusalsForTest(id.New().String())
	// By identity: the restart floor would also refuse a probe socket (as a cut), but only after
	// binding it, and only until the floor expires (re-review RR-2).
	if !server.IsNoReservationsForTest(probeErr) || !server.IsNoReservationsForTest(reservedErr) {
		t.Fatalf("EVEN-PORT probe = %v, reserved port = %v; want both refused as reservations", probeErr, reservedErr)
	}
	if sockets != 0 {
		t.Fatalf("%d relay socket(s) registered after the refusals", sockets)
	}
	if !freed {
		t.Fatal("the refused reserved port kept its quota slot")
	}
}

// Branch review TURN-4: an overflowing cut is counted on the relay's metrics and logged at WARN even
// when another WARN of the relay (a failed barred lookup) was logged just before it.
func TestACutOverflowIsCountedAndWarnedPastOtherWarnings(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	look := &fakeBarred{}
	addr, m, rev, clk, logged := barredRelay(t, secret, config.TURN{CredentialTTL: "1h"}, server.TURNPeers{}, look)
	failing := id.New()
	look.set(failing, false, errors.New("database is locked"))
	user, pass := server.TURNCredential(secret, failing, time.Hour, clk.Now())
	if _, err := allocateAs(t, addr, user, pass); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if s := logged.String(); !strings.Contains(s, "database is locked") {
		t.Fatalf("the failed lookup was not logged: %q", s)
	}
	rev.SetMaxCutsForTest(1)
	rev.Revoke(id.New(), clk.Now())
	rev.Revoke(id.New(), clk.Now()) // the map is full: the floor rises
	if n := rev.OverflowsForTest(); n != 1 {
		t.Fatalf("overflows = %d, want 1", n)
	}
	if _, _, overflows := m.extra(); overflows != 1 {
		t.Fatalf("the overflow counter = %d, want 1", overflows)
	}
	if s := logged.String(); !strings.Contains(s, "cut map is full") {
		t.Fatalf("the overflow WARN was swallowed by the lookup's: %q", s)
	}
}

// Follow-up card 13's interim bound: the instance holds at most maxRelaySockets live allocations,
// whatever the devices. One past it is refused 486, counted apart from the per-device quota and
// logged at WARN; a slot freed anywhere admits the next.
func TestTheRelayCapsItsLiveAllocations(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	defer server.SetMaxRelaySocketsForTest(2)()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var mu sync.Mutex
	var logged strings.Builder
	m := &fakeTURNMetrics{}
	srv, err := server.StartTURN(config.TURN{Enabled: true, Realm: "chat.example.test", RelayIP: "127.0.0.1",
		SharedSecretFile: writeFile(t, "turn.secret", secret), CredentialTTL: "1h", AllocationsPerDevice: 4},
		ln, netip.MustParseAddr("127.0.0.1"), server.TURNPeers{}, m, nil, clock.System(),
		slog.New(slog.NewTextHandler(lockedWriter{&mu, &logged}, nil)))
	if err != nil {
		t.Fatalf("StartTURN: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	addr := ln.Addr().String()
	userA, passA := server.TURNCredential(secret, id.New(), time.Hour, time.Now())
	first, err := allocateAs(t, addr, userA, passA)
	if err != nil {
		t.Fatalf("allocation 1: %v", err)
	}
	if _, err := allocateAs(t, addr, userA, passA); err != nil {
		t.Fatalf("allocation 2: %v", err)
	}
	userB, passB := server.TURNCredential(secret, id.New(), time.Hour, time.Now())
	if _, err := allocateAs(t, addr, userB, passB); err == nil || !strings.Contains(err.Error(), "486") {
		t.Fatalf("another device's allocation past the instance cap = %v, want 486", err)
	}
	refused, _, _, _ := m.snapshot()
	if full, _, _ := m.extra(); full != 1 || refused != 0 {
		t.Fatalf("capacity refusals %d, quota refusals %d; want 1 and 0", full, refused)
	}
	mu.Lock()
	warned := strings.Contains(logged.String(), "level=WARN") && strings.Contains(logged.String(), "instance-wide maximum")
	mu.Unlock()
	if !warned {
		t.Fatalf("the cap was not logged at WARN: %q", logged.String())
	}
	_ = first.Close()
	waitGauge(t, m, 1)
	if _, err := allocateAs(t, addr, userB, passB); err != nil {
		t.Fatalf("an allocation after one ended: %v", err)
	}
}

// The quota's instance-wide count follows only real slots: releasing a device that holds none moves
// nothing.
func TestTheInstanceCapCountsOnlyHeldSlots(t *testing.T) {
	defer server.SetMaxRelaySocketsForTest(2)()
	q := server.NewAllocationQuota(4)
	a, b := id.New().String(), id.New().String()
	q.Release(b) // holds nothing
	if first, second := q.Allow(a), q.Allow(a); !first || !second {
		t.Fatal("the first two slots were refused")
	}
	if q.Allow(b) {
		t.Fatal("a third slot was allowed past the instance cap of 2")
	}
	q.Release(a)
	if !q.Allow(b) {
		t.Fatal("a slot freed by one device did not admit another")
	}
}

// Branch review TURN-6: a relay connection that makes no authenticated request within the
// authentication window is closed; one that authenticated lives past that window and is closed once
// it has read nothing for the idle bound, which ends its allocation.
func TestTheRelayBoundsConnectionsInTime(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	defer server.SetTURNIdleForTest(300*time.Millisecond, 1500*time.Millisecond)()
	sfu, toSFU := udpSink(t, "127.0.0.2:0")
	m := &fakeTURNMetrics{}
	addr := startMeteredTURN(t, secret, config.TURN{CredentialTTL: "1h"}, sfuPeers(sfu), m, clock.System())

	silent, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer silent.Close()
	_ = silent.SetReadDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	if _, err := silent.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("a connection that never authenticated was not closed by the relay (read: %v)", err)
	}
	if waited := time.Since(start); waited > 3*time.Second {
		t.Fatalf("the unauthenticated connection was closed after %s", waited)
	}

	user, pass := server.TURNCredential(secret, id.New(), time.Hour, time.Now())
	relay, err := allocateAs(t, addr, user, pass)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	time.Sleep(600 * time.Millisecond) // past the authentication window
	if _, err := relay.WriteTo([]byte("still-here"), sfu.LocalAddr()); err != nil {
		t.Fatalf("a write after the authentication window: %v", err)
	}
	waitFor(t, "the authenticated connection's media", func() bool { return slices.Contains(toSFU(), "still-here") })
	waitGauge(t, m, 0) // idle past 1.5 s: the connection closes and pion deletes its allocation
}

// bindingsUntilClosed sends an unauthenticated STUN Binding request on conn every 100 ms until the
// relay closes conn, and answers how long that took; it fails the test after five seconds.
func bindingsUntilClosed(t *testing.T, conn net.Conn) time.Duration {
	t.Helper()
	return requestsUntilClosed(t, conn, func() []byte { return stun.MustBuild(stun.TransactionID, stun.BindingRequest).Raw })
}

// requestsUntilClosed sends next() on conn every 100 ms, reading and discarding every answer, until
// the relay closes conn, and answers how long that took; it fails the test after five seconds.
func requestsUntilClosed(t *testing.T, conn net.Conn, next func() []byte) time.Duration {
	t.Helper()
	start := time.Now()
	closed := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, conn)
		close(closed)
	}()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	limit := time.After(5 * time.Second)
	for {
		select {
		case <-closed:
			return time.Since(start)
		case <-limit:
			t.Fatal("the relay kept a connection open for 5 s of requests")
			return 0
		case <-tick.C:
			_, _ = conn.Write(next())
		}
	}
}

// Re-review RR-3: the authentication window is fixed at the accept, not moved by what the
// connection sends: one that sends unauthenticated requests all along is closed at the window
// (300 ms here), well before the idle bound (1.5 s).
func TestTheRelayAuthWindowDoesNotSlide(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	defer server.SetTURNIdleForTest(300*time.Millisecond, 1500*time.Millisecond)()
	addr := startMeteredTURN(t, secret, config.TURN{CredentialTTL: "1h"}, server.TURNPeers{}, &fakeTURNMetrics{},
		clock.System())
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if waited := bindingsUntilClosed(t, conn); waited > time.Second {
		t.Fatalf("an unauthenticated connection was closed after %s, want at the 300 ms window", waited)
	}
}

// Re-review RR-1: only an authenticated request on a connection that holds an allocation (or the
// Allocate that creates one) moves its idle deadline. Unauthenticated bytes after an allocation, an
// Allocate refused at the quota, and a device revoked after its allocation do not keep a connection
// open: it is closed at the idle bound after its last allocation activity, or at the authentication
// window when it never held one.
func TestTheRelayIdleBoundMovesOnlyOnAuthenticatedRequests(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	defer server.SetTURNIdleForTest(300*time.Millisecond, 1500*time.Millisecond)()
	dial := func(t *testing.T, addr string) net.Conn {
		t.Helper()
		conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}

	t.Run("authenticated requests keep an allocation's connection", func(t *testing.T) {
		sfu, toSFU := udpSink(t, "127.0.0.2:0")
		m := &fakeTURNMetrics{}
		addr := startMeteredTURN(t, secret, config.TURN{CredentialTTL: "1h"}, sfuPeers(sfu), m, clock.System())
		user, pass := server.TURNCredential(secret, id.New(), time.Hour, time.Now())
		client, err := turnClientAs(t, addr, user, pass, 0)
		if err != nil {
			t.Fatalf("client: %v", err)
		}
		relay, err := client.Allocate()
		if err != nil {
			t.Fatalf("allocate: %v", err)
		}
		t.Cleanup(func() { _ = relay.Close() })
		for range 8 { // 3.2 s of authenticated CreatePermissions, past the 1.5 s idle bound
			if err := client.CreatePermission(sfu.LocalAddr()); err != nil {
				t.Fatalf("CreatePermission: %v", err)
			}
			time.Sleep(400 * time.Millisecond)
		}
		if _, err := relay.WriteTo([]byte("kept"), sfu.LocalAddr()); err != nil {
			t.Fatalf("a write after the idle bound: %v", err)
		}
		waitFor(t, "the kept connection's media", func() bool { return slices.Contains(toSFU(), "kept") })
		if _, _, _, allocations := m.snapshot(); !slices.Equal(allocations, []int{1}) {
			t.Fatalf("allocation gauge %v, want the one allocation never deleted", allocations)
		}
	})

	t.Run("unauthenticated requests after an allocation", func(t *testing.T) {
		addr := startMeteredTURN(t, secret, config.TURN{CredentialTTL: "1h"}, server.TURNPeers{}, &fakeTURNMetrics{},
			clock.System())
		conn := dial(t, addr)
		user, pass := server.TURNCredential(secret, id.New(), time.Hour, time.Now())
		if code := stunAllocateOn(t, conn, user, pass); code != 0 {
			t.Fatalf("Allocate = %d, want success", code)
		}
		if waited := bindingsUntilClosed(t, conn); waited > 3*time.Second {
			t.Fatalf("closed %s after the allocation, want at the 1.5 s idle bound", waited)
		}
	})

	t.Run("authenticated requests after the allocation ended", func(t *testing.T) {
		addr := startMeteredTURN(t, secret, config.TURN{CredentialTTL: "1h"}, server.TURNPeers{}, &fakeTURNMetrics{},
			clock.System())
		conn := dial(t, addr)
		user, pass := server.TURNCredential(secret, id.New(), time.Hour, time.Now())
		if code := stunAllocateOn(t, conn, user, pass); code != 0 {
			t.Fatalf("Allocate = %d, want success", code)
		}
		end := stun.RawAttribute{Type: stun.AttrLifetime, Value: []byte{0, 0, 0, 0}}
		code, auth, err := stunRequestOn(conn, stun.MethodRefresh, user, pass, end)
		if err != nil || code != 0 {
			t.Fatalf("the ending Refresh = %d, %v; want success", code, err)
		}
		// Authenticated Refreshes of an allocation that is gone (pion authenticates them, then answers
		// nothing).
		refresh := func() []byte {
			return stun.MustBuild(slices.Concat([]stun.Setter{stun.TransactionID,
				stun.NewType(stun.MethodRefresh, stun.ClassRequest), end}, auth, []stun.Setter{stun.Fingerprint})...).Raw
		}
		if waited := requestsUntilClosed(t, conn, refresh); waited > 3*time.Second {
			t.Fatalf("closed %s after the allocation ended, want at the 1.5 s idle bound", waited)
		}
	})

	t.Run("an Allocate refused at the quota", func(t *testing.T) {
		addr := startMeteredTURN(t, secret, config.TURN{CredentialTTL: "1h", AllocationsPerDevice: 1},
			server.TURNPeers{}, &fakeTURNMetrics{}, clock.System())
		user, pass := server.TURNCredential(secret, id.New(), time.Hour, time.Now())
		if code := stunAllocateOn(t, dial(t, addr), user, pass); code != 0 {
			t.Fatalf("the slot holder's Allocate = %d, want success", code)
		}
		conn := dial(t, addr)
		if code := stunAllocateOn(t, conn, user, pass); code != 486 {
			t.Fatalf("an Allocate past the quota = %d, want 486", code)
		}
		if waited := bindingsUntilClosed(t, conn); waited > time.Second {
			t.Fatalf("closed %s after a refused Allocate, want at the 300 ms authentication window", waited)
		}
	})

	t.Run("a device revoked after its allocation", func(t *testing.T) {
		rev := server.NewRelayRevocations(time.Hour, clock.System())
		addr := startRevokableTURN(t, secret, config.TURN{CredentialTTL: "1h"}, server.TURNPeers{}, &fakeTURNMetrics{},
			rev, clock.System())
		conn := dial(t, addr)
		dev := id.New()
		user, pass := server.TURNCredential(secret, dev, time.Hour, time.Now())
		if code := stunAllocateOn(t, conn, user, pass); code != 0 {
			t.Fatalf("Allocate = %d, want success", code)
		}
		rev.Revoke(dev, time.Now().Add(time.Second))
		if waited := bindingsUntilClosed(t, conn); waited > 3*time.Second {
			t.Fatalf("a revoked device's connection was closed %s after the allocation, want at the 1.5 s idle bound", waited)
		}
	})
}

// Re-review RR-4: a closed relay socket leaves the registry of sockets no relay may reach, whether
// its allocation ended or its device was revoked; a missed removal would grow the registry without
// bound and, with livekit.udp_port 0, black-hole a LiveKit socket that later gets that port.
func TestTheRelayForgetsClosedRelaySockets(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	rev := server.NewRelayRevocations(time.Hour, clock.System())
	srv, err := server.StartTURN(config.TURN{Enabled: true, Realm: "chat.example.test", RelayIP: "127.0.0.1",
		SharedSecretFile: writeFile(t, "turn.secret", secret), CredentialTTL: "1h"},
		ln, netip.MustParseAddr("127.0.0.1"), server.TURNPeers{}, &fakeTURNMetrics{}, rev, clock.System(),
		slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("StartTURN: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	addr := ln.Addr().String()
	devA, devB := id.New(), id.New()
	userA, passA := server.TURNCredential(secret, devA, time.Hour, time.Now())
	userB, passB := server.TURNCredential(secret, devB, time.Hour, time.Now())
	relayA, err := allocateAs(t, addr, userA, passA)
	if err != nil {
		t.Fatalf("allocate A: %v", err)
	}
	if _, err := allocateAs(t, addr, userB, passB); err != nil {
		t.Fatalf("allocate B: %v", err)
	}
	if n := srv.RelaySocketsForTest(); n != 2 {
		t.Fatalf("%d relay sockets registered, want 2", n)
	}
	_ = relayA.Close() // the allocation ends
	waitFor(t, "the ended allocation's socket to leave the registry", func() bool { return srv.RelaySocketsForTest() == 1 })
	rev.Revoke(devB, time.Now().Add(time.Second)) // the revocation closes B's socket
	waitFor(t, "the revoked socket to leave the registry", func() bool { return srv.RelaySocketsForTest() == 0 })
}
