package server

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/pion/transport/v4"
	"github.com/pion/turn/v5"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/config"
)

// ACMEIPSettingsForTest is what acme_ip fixes about issuance when the operator
// sets nothing.
func ACMEIPSettingsForTest() acmeSettings {
	return settingsFor(config.TLS{Mode: ModeACMEIP})
}

// TURNAuthForTest is the auth handler StartTURN installs, on clk, for maxAge, reading rev (nil: a
// fresh RelayRevocations nothing cuts).
func TURNAuthForTest(secret string, clk clock.Clock, maxAge time.Duration, rev *RelayRevocations) turn.AuthHandler {
	if rev == nil {
		rev = NewRelayRevocations(maxAge, clk)
	}
	return turnAuth(secret, clk, maxAge, maxAge, rev) // turn.credential_ttl = maxAge
}

// CheckHoldersForTest runs one pass of the relay's barred re-check of the devices holding sockets.
func (r *RelayRevocations) CheckHoldersForTest() { r.checkHolders(context.Background()) }

// CutOfForTest is the cut time (unix milliseconds) that binds dev now — its own or the floor.
func (r *RelayRevocations) CutOfForTest(dev string) (int64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.liveCutLocked(dev)
}

// MintScansForTest is how many times Mint scanned its record for expired entries.
func (r *RelayRevocations) MintScansForTest() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mintScans
}

// MintsForTest is how many mints r holds.
func (r *RelayRevocations) MintsForTest() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.mints)
}

// SetHolderCheckEveryForTest makes the holders' re-check of a relay started afterwards run every d.
func (r *RelayRevocations) SetHolderCheckEveryForTest(d time.Duration) { r.holderEvery = d }

// HoldSocketForTest makes dev a holder of the relay socket pc, as an allocation would.
func (r *RelayRevocations) HoldSocketForTest(dev string, pc net.PacketConn) {
	c := &countingConn{PacketConn: pc, m: noTURNMetrics{}, dev: dev, rev: r}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.socks[dev] == nil {
		r.socks[dev] = map[*countingConn]struct{}{}
	}
	r.socks[dev][c] = struct{}{}
}

// TrackForTest is the relay generator's decision on a relay socket pc of dev, which pion creates
// right after the quota handler admitted dev's Allocate: true tracks it, false refuses it (508).
func (r *RelayRevocations) TrackForTest(dev string, pc net.PacketConn) bool {
	return r.track(dev, &countingConn{PacketConn: pc, m: noTURNMetrics{}, dev: dev, rev: r})
}

// TURNHandlersWithRevForTest is TURNHandlersForTest reading and feeding rev.
func TURNHandlersWithRevForTest(maxPerDevice int, rev *RelayRevocations) (turn.QuotaHandler, turn.EventHandler) {
	return turnHandlers(context.Background(), NewAllocationQuota(maxPerDevice), noTURNMetrics{}, rev, nil)
}

// RelayCutsForTest is how many cuts r holds.
func (r *RelayRevocations) RelayCutsForTest() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.cuts)
}

// BarredCacheLenForTest is how many barred lookups r has cached.
func (r *RelayRevocations) BarredCacheLenForTest() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cache == nil {
		return 0
	}
	return r.cache.ll.Len()
}

// MaxRelayCutsForTest and RelayCutsWarnForTest are the cut map's hard and soft caps.
const (
	MaxRelayCutsForTest  = maxRelayCuts
	RelayCutsWarnForTest = relayCutsWarn
)

// SetMaxCutsForTest lowers r's cut-map cap to n.
func (r *RelayRevocations) SetMaxCutsForTest(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.maxCuts = n
}

// OverflowsForTest is how many cuts raised r's floor.
func (r *RelayRevocations) OverflowsForTest() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.overflows
}

// ParkAllocateForTest makes every relay socket of dev wait, after it is bound and before it is
// tracked, until release is closed; parked is closed when the first one waits. restore undoes it.
func ParkAllocateForTest(dev string) (parked <-chan struct{}, release chan<- struct{}, restore func()) {
	p, rel := make(chan struct{}), make(chan struct{})
	var once sync.Once
	hook := func(d string) {
		if d != dev {
			return
		}
		once.Do(func() { close(p) })
		<-rel
	}
	prev := allocateHook.Swap(&hook)
	return p, rel, func() { allocateHook.Store(prev) }
}

// TURNHandlersForTest is the quota and event pair StartTURN installs over a fresh quota of
// maxPerDevice, reporting to m (nil reports nowhere).
func TURNHandlersForTest(maxPerDevice int, m TURNMetrics) (turn.QuotaHandler, turn.EventHandler) {
	if m == nil {
		m = noTURNMetrics{}
	}
	return turnHandlers(context.Background(), NewAllocationQuota(maxPerDevice), m, NewRelayRevocations(time.Hour, clock.System()), nil)
}

// FailTURNNetForTest makes StartTURN's network setup fail with err until the returned restore runs.
func FailTURNNetForTest(err error) (restore func()) {
	prev := newTURNNet
	newTURNNet = func() (transport.Net, error) { return nil, err }
	return func() { newTURNNet = prev }
}

// EmitForTest drives the certmagic OnEvent hook BuildTLS installed.
func (t *TLS) EmitForTest(ctx context.Context, event string, data map[string]any) error {
	return t.magic.OnEvent(ctx, event, data)
}

// TCPRelayRefusalsForTest asks the relay generator StartTURN installs (on 127.0.0.1, over a quota
// of one that dev holds) for a TCP relay listener and for a Connect's outbound TCP connection to
// peer, and answers both errors and whether dev's slot is free again.
func TCPRelayRefusalsForTest(dev string, peer net.Addr) (listenErr, connErr error, freed bool) {
	relayNet, err := newTURNNet()
	if err != nil {
		return err, err, false
	}
	q := NewAllocationQuota(1)
	q.Allow(dev)
	g := &countingRelay{
		RelayAddressGeneratorStatic: &turn.RelayAddressGeneratorStatic{
			RelayAddress: net.IPv4(127, 0, 0, 1), Address: "127.0.0.1", Net: relayNet,
		},
		m: noTURNMetrics{}, q: q,
	}
	if ln, _, err := g.AllocateListener(turn.AllocateListenerConfig{Network: "tcp4", UserID: dev}); err == nil {
		_ = ln.Close()
	} else {
		listenErr = err
	}
	if c, err := g.AllocateConn(turn.AllocateConnConfig{Network: "tcp4", UserID: dev,
		LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}, RemoteAddr: peer}); err == nil {
		_ = c.Close()
	} else {
		connErr = err
	}
	return listenErr, connErr, q.Allow(dev)
}

// IsNoReservationsForTest reports whether err is the relay generator's EVEN-PORT/RESERVATION-TOKEN
// refusal.
func IsNoReservationsForTest(err error) bool { return errors.Is(err, errNoReservations) }

// RelaySocketsForTest is how many relay sockets t holds registered as peers no relay may reach.
func (t *TURN) RelaySocketsForTest() int {
	t.peers.mu.RLock()
	defer t.peers.mu.RUnlock()
	n := 0
	for _, c := range t.peers.socks {
		n += c
	}
	return n
}

// SetMaxRelaySocketsForTest makes relays started afterwards hold at most n live allocations in all,
// until the returned restore runs.
func SetMaxRelaySocketsForTest(n int) (restore func()) {
	prev := maxRelaySockets
	maxRelaySockets = n
	return func() { maxRelaySockets = prev }
}

// SetTURNIdleForTest gives relays started afterwards the authentication window auth and the idle
// bound idle, until the returned restore runs.
func SetTURNIdleForTest(auth, idle time.Duration) (restore func()) {
	prev := turnIdleBounds
	turnIdleBounds.auth, turnIdleBounds.idle = auth, idle
	return func() { turnIdleBounds = prev }
}

// ReservationRefusalsForTest asks the relay generator StartTURN installs (on 127.0.0.1, over a quota
// of one that dev holds) for pion's EVEN-PORT probe socket (no user) and for a reserved port of dev,
// and answers how many relay sockets either left registered, whether dev's slot is free again, and
// both errors.
func ReservationRefusalsForTest(dev string) (sockets int, freed bool, probeErr, reservedErr error) {
	relayNet, err := newTURNNet()
	if err != nil {
		return 0, false, err, err
	}
	q := NewAllocationQuota(1)
	q.Allow(dev)
	peers := newRelayPeers(TURNPeers{})
	g := &countingRelay{
		RelayAddressGeneratorStatic: &turn.RelayAddressGeneratorStatic{
			RelayAddress: net.IPv4(127, 0, 0, 1), Address: "127.0.0.1", Net: relayNet,
		},
		m: noTURNMetrics{}, q: q, rev: NewRelayRevocations(time.Hour, clock.System()), peers: peers,
	}
	if pc, _, err := g.AllocatePacketConn(turn.AllocateListenerConfig{Network: "udp4"}); err == nil {
		_ = pc.Close()
	} else {
		probeErr = err
	}
	if pc, _, err := g.AllocatePacketConn(turn.AllocateListenerConfig{Network: "udp4", UserID: dev, RequestedPort: 40000}); err == nil {
		_ = pc.Close()
	} else {
		reservedErr = err
	}
	peers.mu.RLock()
	sockets = len(peers.socks)
	peers.mu.RUnlock()
	return sockets, q.Allow(dev), probeErr, reservedErr
}

// ChooseRelayIPForTest is the "auto" decision over a probe result and interface addresses.
func ChooseRelayIPForTest(probed net.IP, addrs []net.Addr, prefer netip.Addr) (net.IP, error) {
	return chooseRelayIP(probed, addrs, prefer)
}
