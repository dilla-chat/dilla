package youtube

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestMain keeps the shared pool's spare dials off the network in unit tests (a fake lookup's googlevideo host would be
// dialed for real); the opt-in live test keeps them.
func TestMain(m *testing.M) {
	if os.Getenv("DILLA_YOUTUBE_LIVE") != "1" {
		upstreams.dial = func(context.Context, string) (net.Conn, error) { return nil, errors.New("no network in unit tests") }
	}
	os.Exit(m.Run())
}

// loopbackUpstreams is a local TCP server standing in for YouTube: each accepted connection is kept, so a test can
// close or write to it as the server would.
type loopbackUpstreams struct {
	listener net.Listener
	mu       sync.Mutex
	accepted []net.Conn
}

func newLoopbackUpstreams(t *testing.T) *loopbackUpstreams {
	t.Helper()
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := &loopbackUpstreams{listener: l}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			u.mu.Lock()
			u.accepted = append(u.accepted, c)
			u.mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		u.mu.Lock()
		defer u.mu.Unlock()
		for _, c := range u.accepted {
			_ = c.Close()
		}
	})
	return u
}

func (u *loopbackUpstreams) last() net.Conn {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.accepted) == 0 {
		return nil
	}
	return u.accepted[len(u.accepted)-1]
}

// testPool dials the loopback server for every host and counts the dials by host. Its verify is the production one
// minus the public-address rule (the test server is on loopback).
func testPool(t *testing.T, u *loopbackUpstreams) (*upstreamPool, *sync.Map) {
	t.Helper()
	dials := &sync.Map{}
	p := newUpstreamPool(func(ctx context.Context, host string) (net.Conn, error) {
		if !allowedYouTubeHost(host) {
			return nil, errEgressPolicy
		}
		n, _ := dials.LoadOrStore(host, new(atomic.Int32))
		n.(*atomic.Int32).Add(1)
		var d net.Dialer
		return d.DialContext(ctx, "tcp", u.listener.Addr().String())
	})
	p.verify = func(host string, conn net.Conn) bool { return allowedYouTubeHost(host) && idleOpen(conn) }
	return p, dials
}

func dialCount(dials *sync.Map, host string) int32 {
	n, ok := dials.Load(host)
	if !ok {
		return 0
	}
	return n.(*atomic.Int32).Load()
}

func waitIdle(t *testing.T, p *upstreamPool, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for p.idleCount() != want {
		if time.Now().After(deadline) {
			t.Fatalf("spares = %d, want %d", p.idleCount(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A primed spare is handed to the next CONNECT to exactly its host, and to no other host.
func TestASpareGoesOnlyToItsOwnHost(t *testing.T) {
	u := newLoopbackUpstreams(t)
	p, dials := testPool(t, u)
	p.prime(t.Context(), "test", "www.youtube.com")
	waitIdle(t, p, 1)
	if conn := p.take("rr1---sn-abc.googlevideo.com"); conn != nil {
		t.Fatal("a spare to www.youtube.com was handed to another host")
	}
	conn := p.take("www.youtube.com")
	if conn == nil {
		t.Fatal("the spare was not handed over")
	}
	_ = conn.Close()
	if p.take("www.youtube.com") != nil || dialCount(dials, "www.youtube.com") != 1 {
		t.Fatal("a spare was handed over twice")
	}
}

// The pool dials only allowlisted hosts and holds at most maxIdlePerHost per host and maxIdleTotal in all.
func TestThePoolIsBoundedAndAllowlisted(t *testing.T) {
	u := newLoopbackUpstreams(t)
	p, dials := testPool(t, u)
	for _, host := range []string{"example.com", "youtube.com.evil.example", "127.0.0.1"} {
		p.prime(t.Context(), "test", host)
		if p.take(host) != nil || dialCount(dials, host) != 0 {
			t.Fatalf("%s was dialed or handed over", host)
		}
	}
	for i := 0; i < 5; i++ {
		p.prime(t.Context(), "test", "www.youtube.com")
	}
	waitIdle(t, p, maxIdlePerHost)
	for i := 0; i < 2*maxIdleTotal; i++ {
		p.prime(t.Context(), "owner-"+string(rune('a'+i)), "rr"+string(rune('a'+i))+"---sn-abc.googlevideo.com")
	}
	waitIdle(t, p, maxIdleTotal)
}

// Fix round 4 (M3-1): one owner (a sandbox's broker, or one community's lookups) holds at most maxIdlePerOwner
// spares, so it cannot fill the pool; another owner still finds room. A spare handed over frees its owner's slot.
func TestOneOwnerCannotFillThePool(t *testing.T) {
	u := newLoopbackUpstreams(t)
	p, _ := testPool(t, u)
	for i := 0; i < maxIdleTotal; i++ {
		p.prime(t.Context(), "greedy", "rr"+string(rune('a'+i))+"---sn-abc.googlevideo.com")
	}
	waitIdle(t, p, maxIdlePerOwner)
	p.prime(t.Context(), "other", "www.youtube.com")
	waitIdle(t, p, maxIdlePerOwner+1)
	if conn := p.take("rra---sn-abc.googlevideo.com"); conn != nil {
		_ = conn.Close()
	} else if conn := p.take("rre---sn-abc.googlevideo.com"); conn == nil {
		t.Fatal("no greedy spare to take")
	} else {
		_ = conn.Close()
	}
	p.prime(t.Context(), "greedy", "rrz---sn-abc.googlevideo.com")
	waitIdle(t, p, maxIdlePerOwner+1)
}

// Fix round 4 (M3-1): a spare is dialed under its owner's context: an owner that closed dials nothing more, and a
// dial that finishes after its owner closed is dropped.
func TestASpareIsDialedUnderItsOwnersContext(t *testing.T) {
	u := newLoopbackUpstreams(t)
	p, dials := testPool(t, u)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	p.prime(ctx, "closed", "www.youtube.com")
	time.Sleep(50 * time.Millisecond)
	if dialCount(dials, "www.youtube.com") != 0 || p.idleCount() != 0 {
		t.Fatal("a closed owner primed a spare")
	}
	block := make(chan struct{})
	p.dial = func(dialCtx context.Context, host string) (net.Conn, error) {
		<-block
		var d net.Dialer
		return d.DialContext(t.Context(), "tcp", u.listener.Addr().String())
	}
	ctx, cancel = context.WithCancel(t.Context())
	p.prime(ctx, "closing", "www.youtube.com")
	cancel()
	close(block)
	time.Sleep(50 * time.Millisecond)
	if p.idleCount() != 0 {
		t.Fatal("a spare dialed for an owner that closed was kept")
	}
	p.prime(t.Context(), "closing", "www.youtube.com")
	waitIdle(t, p, 1) // its slot was released
}

// A spare the server closed, or wrote to, is never handed over; one past its TTL is closed and dropped.
func TestASpareIsVerifiedAgainBeforeItIsHandedOver(t *testing.T) {
	u := newLoopbackUpstreams(t)
	p, _ := testPool(t, u)
	p.prime(t.Context(), "test", "www.youtube.com")
	waitIdle(t, p, 1)
	_ = u.last().Close()
	time.Sleep(20 * time.Millisecond)
	if conn := p.take("www.youtube.com"); conn != nil {
		t.Fatal("a closed spare was handed over")
	}
	p.prime(t.Context(), "test", "www.youtube.com")
	waitIdle(t, p, 1)
	if _, err := u.last().Write([]byte("unsolicited")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if conn := p.take("www.youtube.com"); conn != nil {
		t.Fatal("a spare that received bytes was handed over")
	}
	p.ttl = 30 * time.Millisecond
	p.prime(t.Context(), "test", "www.youtube.com")
	waitIdle(t, p, 1)
	waitIdle(t, p, 0)
}

// The production check refuses a spare whose remote end is not a public address on 443.
func TestTheProductionCheckRefusesANonPublicSpare(t *testing.T) {
	u := newLoopbackUpstreams(t)
	var d net.Dialer
	conn, err := d.DialContext(t.Context(), "tcp", u.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if verifyIdleUpstream("www.youtube.com", conn) {
		t.Fatal("a loopback spare passed the production check")
	}
}

// The broker hands a CONNECT a spare and primes the next: the second CONNECT to a host dials nothing on its own path.
func TestTheBrokerUsesAndRefillsSpares(t *testing.T) {
	u := newLoopbackUpstreams(t)
	p, dials := testPool(t, u)
	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "unix", t.TempDir()+"/egress.sock")
	if err != nil {
		t.Fatal(err)
	}
	b := newEgressBroker(context.Background(), listener)
	b.pool = p
	t.Cleanup(b.close)
	p.prime(t.Context(), "test", "www.youtube.com")
	waitIdle(t, p, 1)
	connect := func() {
		t.Helper()
		var d net.Dialer
		c, err := d.DialContext(t.Context(), "unix", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		if _, err := io.WriteString(c, "CONNECT www.youtube.com:443 HTTP/1.1\r\nHost: www.youtube.com:443\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		line, err := bufio.NewReader(c).ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "HTTP/1.1 200 ") {
			t.Fatalf("CONNECT = %q, %v", line, err)
		}
	}
	connect()
	waitIdle(t, p, 1) // the spare was used and a new one primed
	if n := dialCount(dials, "www.youtube.com"); n != 2 {
		t.Fatalf("dials = %d, want the first spare and its refill", n)
	}
	connect()
	waitIdle(t, p, 1)
	if n := dialCount(dials, "www.youtube.com"); n != 3 {
		t.Fatalf("dials = %d, want one refill per CONNECT and no dial on its path", n)
	}
}
