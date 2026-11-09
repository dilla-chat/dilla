package youtube

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"time"
)

// Upstream keep-alive for the egress broker (Bot v2 Task 19 fix round 3, Jonas's decision of 2026-11-09).
//
// The broker relays CONNECT tunnels: TLS runs end to end between yt-dlp or ffmpeg inside the sandbox and YouTube, so
// the broker cannot reuse a connection a tunnel has used. What it keeps open instead is the step before: spare TCP
// connections to an allowed host, dialed ahead (the DNS lookup and the TCP handshake done), handed to the next CONNECT
// to exactly that host. The pool is shared by every broker of the Bot, so a lookup's sandbox and the stream's sandbox
// after it both draw from it, and a lookup primes the stream's googlevideo host before the stream asks for it.
//
// The rules are the broker's own:
//   - a spare is dialed only by dialPublicHTTPS, so only to an allowlisted host's public addresses on 443;
//   - a spare is handed over only for its exact host, after the host is checked against the allowlist again, its
//     remote address is checked to be public and on 443 again, and it is checked to be open with nothing received;
//   - a spare has carried no bytes: the tunnel it becomes is counted against the tunnel and process budgets as any
//     other, from its first byte;
//   - the pool is small and short-lived (maxIdlePerHost, maxIdleTotal, idleUpstreamTTL), and no one owner (a
//     sandbox's broker, or one community's lookups) holds more than maxIdlePerOwner of it (fix round 4, M3-1);
//   - a spare is dialed under its owner's context, so a broker or resolver that closes stops its dials.
const (
	maxIdlePerHost  = 2
	maxIdleTotal    = 8
	maxIdlePerOwner = 4
	idleUpstreamTTL = 8 * time.Second
	primeDeadline   = 4 * time.Second
)

type idleUpstream struct {
	conn  net.Conn
	owner string
	timer *time.Timer
}

type upstreamPool struct {
	mu      sync.Mutex
	idle    map[string][]*idleUpstream
	pending map[string]int
	owners  map[string]int // idle and pending spares by owner
	total   int
	dial    func(context.Context, string) (net.Conn, error)
	verify  func(host string, conn net.Conn) bool
	ttl     time.Duration
}

func newUpstreamPool(dial func(context.Context, string) (net.Conn, error)) *upstreamPool {
	return &upstreamPool{idle: map[string][]*idleUpstream{}, pending: map[string]int{}, owners: map[string]int{}, dial: dial, verify: verifyIdleUpstream, ttl: idleUpstreamTTL}
}

// upstreams is the Bot's one pool, shared by every broker.
var upstreams = newUpstreamPool(dialPublicHTTPS)

// take hands over a spare connection to exactly host, verified again, or nil (the caller dials).
func (p *upstreamPool) take(host string) net.Conn {
	if p == nil || !allowedYouTubeHost(host) {
		return nil
	}
	for {
		p.mu.Lock()
		list := p.idle[host]
		if len(list) == 0 {
			p.mu.Unlock()
			return nil
		}
		spare := list[len(list)-1]
		p.removeLocked(host, spare)
		p.mu.Unlock()
		if p.verify(host, spare.conn) {
			return spare.conn
		}
		_ = spare.conn.Close()
	}
}

// prime dials a spare to host for owner in the background, under ctx, when the pool has room for it.
func (p *upstreamPool) prime(ctx context.Context, owner, host string) {
	if p == nil || ctx == nil || ctx.Err() != nil || !allowedYouTubeHost(host) {
		return
	}
	p.mu.Lock()
	pendingTotal := 0
	for _, n := range p.pending {
		pendingTotal += n
	}
	if len(p.idle[host])+p.pending[host] >= maxIdlePerHost || p.total+pendingTotal >= maxIdleTotal || p.owners[owner] >= maxIdlePerOwner {
		p.mu.Unlock()
		return
	}
	p.pending[host]++
	p.owners[owner]++
	p.mu.Unlock()
	go func() {
		dialCtx, cancel := context.WithTimeout(ctx, primeDeadline)
		conn, err := p.dial(dialCtx, host)
		cancel()
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.pending[host]--; p.pending[host] <= 0 {
			delete(p.pending, host)
		}
		if err != nil || ctx.Err() != nil || len(p.idle[host]) >= maxIdlePerHost || p.total >= maxIdleTotal {
			p.releaseOwnerLocked(owner)
			if conn != nil {
				_ = conn.Close()
			}
			return
		}
		spare := &idleUpstream{conn: conn, owner: owner}
		spare.timer = time.AfterFunc(p.ttl, func() { p.expire(host, spare) })
		p.idle[host] = append(p.idle[host], spare)
		p.total++
	}()
}

func (p *upstreamPool) expire(host string, spare *idleUpstream) {
	p.mu.Lock()
	found := p.removeLocked(host, spare)
	p.mu.Unlock()
	if found {
		_ = spare.conn.Close()
	}
}

// removeLocked takes spare out of the pool (under p.mu) and reports whether it was there.
func (p *upstreamPool) removeLocked(host string, spare *idleUpstream) bool {
	list := p.idle[host]
	for i, candidate := range list {
		if candidate != spare {
			continue
		}
		if spare.timer != nil {
			spare.timer.Stop()
		}
		list = append(list[:i], list[i+1:]...)
		if len(list) == 0 {
			delete(p.idle, host)
		} else {
			p.idle[host] = list
		}
		p.total--
		p.releaseOwnerLocked(spare.owner)
		return true
	}
	return false
}

func (p *upstreamPool) releaseOwnerLocked(owner string) {
	if p.owners[owner]--; p.owners[owner] <= 0 {
		delete(p.owners, owner)
	}
}

// idleCount is the number of spares held (tests).
func (p *upstreamPool) idleCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.total
}

// verifyIdleUpstream checks a spare again before it is handed over: the host is allowlisted, the remote end is a
// public address on 443, and the connection is open with nothing received (a spare the server closed or wrote to is
// never handed over).
func verifyIdleUpstream(host string, conn net.Conn) bool {
	if !allowedYouTubeHost(host) {
		return false
	}
	tcp, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok || tcp.Port != 443 {
		return false
	}
	if !publicEgressAddress(tcp.AddrPort().Addr().Unmap().String()) {
		return false
	}
	return idleOpen(conn)
}

// idleOpen reports whether conn is open and has received nothing: a read under a 1 ms deadline (one already past
// would not reach the socket) times out when nothing is waiting, and answers EOF or bytes otherwise.
func idleOpen(conn net.Conn) bool {
	if err := conn.SetReadDeadline(time.Now().Add(time.Millisecond)); err != nil {
		return false
	}
	var one [1]byte
	n, err := conn.Read(one[:])
	if resetErr := conn.SetReadDeadline(time.Time{}); resetErr != nil {
		return false
	}
	return n == 0 && errors.Is(err, os.ErrDeadlineExceeded)
}
