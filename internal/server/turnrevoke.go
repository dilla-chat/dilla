package server

import (
	"container/list"
	"context"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
)

// BarredLookup answers whether a device may no longer take part in calls — revoked, quarantined, or
// of a disabled or deleted user — as the database says, which another process (`dillad admin`) may
// have written. A device id with no device row is NOT barred: only a holder of turn's shared secret
// can mint a credential for one (`dillad doctor`'s relay probe does, for a fresh id), and the call
// routes mint only for devices that exist. api.BarredDevices is one.
type BarredLookup interface {
	DeviceBarred(ctx context.Context, device id.ID) (bool, error)
}

const (
	// relayCutsWarn and maxRelayCuts bound the cut map. An entry lives at most
	// turn.max_allocation_age, so the map holds the cuts of one max-age window, each made by a
	// server-side event (a call cut, an authenticated barred lookup) and never by a request alone. A
	// live cut is never evicted — forgetting one would re-admit its device's old credentials — so
	// past relayCutsWarn the map grows with a WARN, and a cut that finds maxRelayCuts live cuts
	// already held fails closed: it raises the floor (RelayRevocations.floor) to its time, which
	// refuses every device's credentials issued at or before it — a relay reconnect for everyone,
	// logged at WARN and counted, rather than a forgotten revocation. A cut is recorded only for a
	// device that can hold a relay credential (a mint within turn.credential_ttl, a relay socket or a
	// pending Allocate: Revoke), because devices per user are not capped and one member revoking its
	// own devices must not be able to fill the map and reset the relay for everyone. The mint record
	// shares the bound.
	relayCutsWarn = 4096
	maxRelayCuts  = 65536
	// pendingAllocateWindow is how long the issue time of an authenticated Allocate is held for the
	// allocation pion creates right after it on the same goroutine (OnAuth, then the quota handler,
	// then the relay socket: internal/server/turn.go:33-237).
	pendingAllocateWindow = 5 * time.Second
	// maxAuthedPerDevice bounds the authenticated Allocates of one device the relay holds before
	// pion's quota handler sees them (one per client transport address).
	maxAuthedPerDevice = 8
	// barredTTL is how long the relay trusts one barred lookup, and how often it re-checks the
	// devices that hold relay sockets. A "not barred" answer cached just before another process
	// revoked the device is trusted until it expires, and the re-check that finds it expired runs on
	// the next tick: a device holding sockets loses the relay within 2 × barredTTL plus one lookup
	// (barredLookupTimeout) of a revocation another process wrote, while the store answers.
	barredTTL = 30 * time.Second
	// maxBarredCache bounds the lookup cache (least recently used out).
	maxBarredCache = 4096
	// barredLookupTimeout bounds one lookup; maxBarredLookups bounds the lookups in flight. pion
	// calls the auth handler on the client connection's own goroutine, so a slow store stalls only
	// that client, and never longer than the timeout; past the bound the relay does not wait.
	barredLookupTimeout = 2 * time.Second
	maxBarredLookups    = 8
)

// RelayRevocations is what the relay knows about devices that lost the right to it (task 13 review
// I1). It is created once by the composition root and shared by the relay (StartTURN) and the call
// routes (api.Calls.WithRelay).
//
// Every time it holds is a unix time in milliseconds, the unit of the issue time a credential
// carries ("<expiry_s>:<device_id>:<issued_ms>"), and none is ever ahead of the clock: a mint's
// issue time is the clock's, a cut's time is the clock's at the latest (re-review N1).
//
//   - A cut (Revoke) records device → cut time. The relay's auth handler refuses every method of a
//     credential issued at or before its device's cut, and the device's live relay sockets are
//     closed at once: pion then deletes their allocations (allocation.go:375-381), which fires
//     OnAllocationDeleted and so frees the quota slots and moves the gauge. A credential issued
//     after the cut passes, which only a device that may take part in calls again can get. An
//     entry is dropped once turn.max_allocation_age has passed since the cut, when no credential
//     issued before it is honoured anyway, and never before; a cut that cannot be stored raises
//     the floor for every device instead (relayCutsWarn, maxRelayCuts). A
//     socket created for an Allocate that passed the auth handler just before a cut is refused
//     when it is tracked (track), so a cut leaves no socket of an earlier credential open.
//   - The floor starts at the process start (re-review N3): the cuts are held in memory only, so
//     every credential minted before this process started is refused. A restart drops every call
//     anyway (the SFU is in-process); clients re-POST the calls route for a fresh credential.
//   - Mint chooses a credential's issue time under the lock Revoke takes, so every cut is ordered
//     before or after it. It refuses — no credential, and the call route answers a transient 429 —
//     when the device has a live cut in the current millisecond or since the request began, which
//     the credential would otherwise be ordered against by luck.
//   - The barred lookup (WithBarred) covers what no cut reaches — a device another process revoked
//     that is in no call room. It is asked (through a cache of barredTTL) only about a device whose
//     request pion has authenticated — the OnAuth event, after the MESSAGE-INTEGRITY check — and,
//     every barredTTL, about each device that holds relay sockets; a barred device is cut. The
//     auth handler itself, which pion calls before the integrity check, reads the cut map only, so
//     no unauthenticated request causes a store read or a cache entry. A lookup that fails, times
//     out or finds the bound of lookups in flight reached caches nothing and is logged at WARN: the
//     relay never waits on the database, and such a device is bounded by turn.max_allocation_age
//     until a later lookup succeeds. The cut map stays authoritative whatever the lookup says.
type RelayRevocations struct {
	clk  clock.Clock
	keep int64 // milliseconds a cut is kept: turn.max_allocation_age

	mu      sync.Mutex
	cuts    map[string]int64
	socks   map[string]map[*countingConn]struct{}
	pending map[string]pendingAllocate
	// authed holds, per device and client transport address, the issue time of an Allocate pion
	// has authenticated and not yet put to its quota handler (authenticated, admitAllocate).
	authed  map[string]map[string]authedAllocate
	nAuthed int
	// nextExpiry is the earliest time a held cut expires, kept from the last scan (scanned) and
	// lowered by every cut recorded since.
	nextExpiry int64
	scanned    bool
	// floor is the watermark every device's credentials are refused at or before, until
	// turn.max_allocation_age has passed since it (0: none). It starts just before the process
	// started, and a cut that overflows the map raises it; overflows counts those. maxCuts is
	// maxRelayCuts (a test lowers it).
	floor     int64
	overflows uint64
	maxCuts   int
	// overflowed counts an overflow on dillad's metrics (dilla_turn_cut_overflows_total); StartTURN
	// installs it.
	overflowed func()
	// mints is device → the last time a relay credential was minted for it (Mint), kept for ttl
	// (turn.credential_ttl in milliseconds; 0 disables the gate: every cut is recorded).
	// mintsFullUntil, while in the future, is a window in which a mint could not be recorded, which
	// makes Revoke record every cut (fail closed). mintsNextExpiry and mintsScanned amortise the
	// scan for expired mints as nextExpiry does the cuts'; mintScans counts the scans (tests).
	mints           map[string]int64
	ttl             int64
	mintsFullUntil  int64
	mintsNextExpiry int64
	mintsScanned    bool
	mintScans       int

	barred   BarredLookup
	log      *slog.Logger
	cache    *barredCache
	inflight chan struct{}
	// warned is when each WARN message was last logged: the limiter is per message, so a burst of
	// lookup failures cannot swallow the overflow WARN (branch review TURN-4).
	warned map[string]time.Time
	// holderEvery is how often the holders' re-check runs: barredTTL (a test shortens it).
	holderEvery time.Duration
}

// NewRelayRevocations keeps each cut for maxAllocationAge (turn.max_allocation_age; 2 h when unset).
// Every credential issued before now (the process start) is refused.
func NewRelayRevocations(maxAllocationAge time.Duration, clk clock.Clock) *RelayRevocations {
	if maxAllocationAge <= 0 {
		maxAllocationAge = 2 * time.Hour
	}
	return &RelayRevocations{
		clk: clk, keep: maxAllocationAge.Milliseconds(), log: slog.New(slog.DiscardHandler),
		cuts: map[string]int64{}, socks: map[string]map[*countingConn]struct{}{},
		pending: map[string]pendingAllocate{}, authed: map[string]map[string]authedAllocate{},
		maxCuts: maxRelayCuts, mints: map[string]int64{}, warned: map[string]time.Time{},
		floor:       clk.Now().UnixMilli() - 1,
		holderEvery: barredTTL,
	}
}

// WithCredentialTTL turns on the mint gate (Revoke) with turn.credential_ttl, and returns r.
func (r *RelayRevocations) WithCredentialTTL(ttl time.Duration) *RelayRevocations {
	r.ttl = ttl.Milliseconds()
	return r
}

// Mint records that a relay credential is minted for device, for a request that began at since, and
// answers its issue time: the clock's time, never later. It is chosen under the lock Revoke takes,
// so every cut of the device is ordered before the credential (which is then newer than it) or
// after it (which then covers it). When the device has a live cut (or the floor) in the current
// millisecond, or one made since the request began, Mint mints nothing and answers how long to wait
// instead (wait > 0): such a credential would be ordered against the cut by luck, and the request
// was cut while it was served. Nothing is recorded and no cut moves, so a refused mint costs no one
// anything; the call route answers a transient 429 and the device retries. Mint is what makes a
// later cut of the device worth recording (mayHoldCredentialLocked).
func (r *RelayRevocations) Mint(device id.ID, since time.Time) (issued time.Time, wait time.Duration) {
	now := r.clk.Now().UnixMilli()
	dev := device.String()
	r.mu.Lock()
	defer r.mu.Unlock()
	if at, cut := r.liveCutLocked(dev); cut && (at >= now || at >= since.UnixMilli()) {
		return time.Time{}, time.Duration(max(at-now+1, 1)) * time.Millisecond
	}
	if r.ttl > 0 {
		r.recordMintLocked(dev, now)
	}
	return time.UnixMilli(now), 0
}

// recordMintLocked records a mint for dev at now. A full record drops its expired entries first —
// scanning only once its earliest entry can have expired — and, still full, makes every cut
// recorded until this credential has expired.
func (r *RelayRevocations) recordMintLocked(dev string, now int64) {
	if _, ok := r.mints[dev]; !ok && len(r.mints) >= r.maxCuts {
		if !r.mintsScanned || now > r.mintsNextExpiry {
			r.mintScans++
			r.mintsScanned, r.mintsNextExpiry = true, math.MaxInt64
			for d, t := range r.mints {
				if now > t+r.ttl {
					delete(r.mints, d)
				} else {
					r.mintsNextExpiry = min(r.mintsNextExpiry, t+r.ttl)
				}
			}
		}
		if len(r.mints) >= r.maxCuts {
			r.mintsFullUntil = max(r.mintsFullUntil, now+r.ttl+2000)
			return
		}
	}
	r.mints[dev] = max(r.mints[dev], now)
	if r.mintsScanned {
		r.mintsNextExpiry = min(r.mintsNextExpiry, now+r.ttl)
	}
}

// mayHoldCredentialLocked reports whether dev may hold a relay credential a cut would refuse: a
// mint within turn.credential_ttl, a relay socket, or an authenticated or pending Allocate — or the
// gate cannot tell
// (off, or a mint it could not record), which records the cut. A credential minted before this
// process started is refused by the floor, so the mints this process recorded are all there are.
func (r *RelayRevocations) mayHoldCredentialLocked(dev string) bool {
	now := r.clk.Now().UnixMilli()
	if r.ttl <= 0 || now < r.mintsFullUntil {
		return true
	}
	if t, ok := r.mints[dev]; ok {
		if now <= t+r.ttl {
			return true
		}
		delete(r.mints, dev)
	}
	if len(r.socks[dev]) > 0 || len(r.authed[dev]) > 0 {
		return true
	}
	p, ok := r.pending[dev]
	return ok && time.Duration(now-p.last)*time.Millisecond <= pendingAllocateWindow
}

// pendingAllocate is what the relay holds about a device's Allocates that its quota handler admitted
// and whose relay socket is not tracked yet: how many, the oldest issue time among their credentials,
// and when the last was admitted. It is conservative — the oldest issue time only goes down until the
// record ends — so a cut covering any of them refuses the device's sockets for the window.
type pendingAllocate struct {
	count     int
	minIssued int64
	last      int64
}

// authedAllocate is one authenticated Allocate waiting for pion's quota handler.
type authedAllocate struct {
	issued int64
	at     int64
}

// authenticated records an Allocate of dev from the client transport address src, authenticated
// with a credential issued at issued. Only OnAuth with a verdict of true calls it, so no
// unauthenticated request adds a record. pion runs its quota handler for the same request next, on
// the same goroutine — unless it refuses the Allocate first (437, 440, a malformed attribute), when
// the record is never used: it is replaced by the next Allocate from src, and expires with
// pendingAllocateWindow. A device holds at most maxAuthedPerDevice records (its oldest goes); a
// record that cannot be held at all leaves its socket refused while the device has a live cut.
func (r *RelayRevocations) authenticated(dev, src string, issued int64) {
	now := r.clk.Now().UnixMilli()
	r.mu.Lock()
	defer r.mu.Unlock()
	set := r.authed[dev]
	if _, ok := set[src]; !ok {
		if len(set) >= maxAuthedPerDevice {
			oldest, at := "", int64(math.MaxInt64)
			for s, a := range set {
				if a.at < at {
					oldest, at = s, a.at
				}
			}
			delete(set, oldest)
			r.nAuthed--
		}
		if set == nil && r.nAuthed >= r.maxCuts {
			r.pruneAuthedLocked(now)
			if r.nAuthed >= r.maxCuts {
				return
			}
		}
		if set == nil {
			set = map[string]authedAllocate{}
			r.authed[dev] = set
		}
		r.nAuthed++
	}
	set[src] = authedAllocate{issued: issued, at: now}
}

func (r *RelayRevocations) pruneAuthedLocked(now int64) {
	for dev, set := range r.authed {
		for src, a := range set {
			if time.Duration(now-a.at)*time.Millisecond > pendingAllocateWindow {
				delete(set, src)
				r.nAuthed--
			}
		}
		if len(set) == 0 {
			delete(r.authed, dev)
		}
	}
}

// takeAuthedLocked removes and answers dev's authenticated Allocate from src, if it is fresh.
func (r *RelayRevocations) takeAuthedLocked(dev, src string, now int64) (authedAllocate, bool) {
	set := r.authed[dev]
	a, ok := set[src]
	if !ok {
		return authedAllocate{}, false
	}
	delete(set, src)
	r.nAuthed--
	if len(set) == 0 {
		delete(r.authed, dev)
	}
	return a, time.Duration(now-a.at)*time.Millisecond <= pendingAllocateWindow
}

// admitAllocate is the quota handler's answer for dev's Allocate from src. Admitted, the issue time
// OnAuth recorded becomes pending for the relay socket pion creates next (track). Refused (486, or a
// device known to be barred), the record is dropped and nothing stays pending (re-review N6).
func (r *RelayRevocations) admitAllocate(dev, src string, admitted bool) {
	now := r.clk.Now().UnixMilli()
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.takeAuthedLocked(dev, src, now)
	if !ok || !admitted {
		return
	}
	p, have := r.pending[dev]
	if !have || time.Duration(now-p.last)*time.Millisecond > pendingAllocateWindow {
		p = pendingAllocate{minIssued: a.issued}
		if !have && len(r.pending) >= r.maxCuts {
			r.prunePendingLocked(now)
		}
	}
	p.count++
	p.minIssued = min(p.minIssued, a.issued)
	p.last = now
	r.pending[dev] = p
}

// dropPending gives back one of dev's pending Allocates whose relay socket pion could not create
// (the generator failed, or the Allocate asked for a TCP relay).
func (r *RelayRevocations) dropPending(dev string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.pending[dev]; ok {
		if p.count--; p.count <= 0 {
			delete(r.pending, dev)
		} else {
			r.pending[dev] = p
		}
	}
}

func (r *RelayRevocations) prunePendingLocked(now int64) {
	for dev, p := range r.pending {
		if time.Duration(now-p.last)*time.Millisecond > pendingAllocateWindow {
			delete(r.pending, dev)
		}
	}
}

// setOverflowCounter makes every overflowing cut call count (StartTURN: the metric).
func (r *RelayRevocations) setOverflowCounter(count func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.overflowed = count
}

// WithBarred makes the relay consult b about every device it authenticates and every device that
// holds a relay socket, and returns r. log receives the lookups that fail open and the cut map's
// overflow.
func (r *RelayRevocations) WithBarred(b BarredLookup, log *slog.Logger) *RelayRevocations {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	r.barred, r.log = b, log
	r.cache = newBarredCache(maxBarredCache)
	r.inflight = make(chan struct{}, maxBarredLookups)
	return r
}

// Revoke cuts device from the relay as of at, or as of now when at is later: every credential of
// it issued at or before that millisecond is refused from now on, and its relay sockets are closed.
// It takes a lock, writes a map and closes sockets, and never blocks on anything else, so a request
// path may call it. A device that cannot hold a relay credential (mayHoldCredentialLocked) gets no
// cut entry: there is nothing to refuse.
func (r *RelayRevocations) Revoke(device id.ID, at time.Time) {
	r.revoke(device.String(), at.UnixMilli())
}

func (r *RelayRevocations) revoke(dev string, at int64) {
	r.mu.Lock()
	// A cut time is never ahead of the clock (re-review N1): every credential minted before the cut
	// carries an issue time at or before it, and none minted after it is covered.
	at = min(at, r.clk.Now().UnixMilli())
	recorded := true
	if prev, ok := r.cuts[dev]; (!ok || at > prev) && (ok || r.mayHoldCredentialLocked(dev)) {
		if !ok && len(r.cuts) >= min(relayCutsWarn, r.maxCuts) {
			r.dropExpiredCutsLocked()
		}
		if !ok && len(r.cuts) >= r.maxCuts {
			// Fail closed: no live cut is evicted, and this one is enforced for every device.
			recorded = false
			r.floor = max(r.floor, at)
			r.overflows++
		} else {
			r.cuts[dev] = at
			if r.scanned {
				r.nextExpiry = min(r.nextExpiry, at+r.keep)
			}
		}
	}
	size := len(r.cuts)
	conns := r.socks[dev]
	delete(r.socks, dev)
	overflowed := r.overflowed
	r.mu.Unlock()
	for c := range conns {
		_ = c.Close()
	}
	switch {
	case !recorded:
		if overflowed != nil {
			overflowed()
		}
		r.warn("the relay's cut map is full; every credential issued up to this cut is refused for every device, which re-fetch theirs",
			"device", dev, "cuts", size)
	case size > relayCutsWarn:
		r.warn("the relay holds more cuts than expected within one turn.max_allocation_age", "cuts", size)
	}
}

// dropExpiredCutsLocked forgets every cut older than turn.max_allocation_age. A live cut is never
// dropped.
// It scans the map only once its oldest cut can have expired (nextExpiry), so a burst of cuts past
// the soft cap does not scan it once per cut.
func (r *RelayRevocations) dropExpiredCutsLocked() {
	now := r.clk.Now().UnixMilli()
	if r.scanned && now <= r.nextExpiry {
		return
	}
	r.scanned, r.nextExpiry = true, math.MaxInt64
	for dev, at := range r.cuts {
		if now > at+r.keep {
			delete(r.cuts, dev)
		} else {
			r.nextExpiry = min(r.nextExpiry, at+r.keep)
		}
	}
}

// cutCovers reports whether a credential of dev issued at issued (unix milliseconds) is cut.
func (r *RelayRevocations) cutCovers(dev string, issued int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	at, ok := r.liveCutLocked(dev)
	return ok && issued <= at
}

// liveCutLocked is the cut time that binds dev now — its own cut or the floor, the later of the two
// that has not expired — and whether there is one. An expired cut or floor is dropped.
func (r *RelayRevocations) liveCutLocked(dev string) (int64, bool) {
	now := r.clk.Now().UnixMilli()
	at, ok := r.cuts[dev]
	if ok && now > at+r.keep {
		delete(r.cuts, dev)
		ok = false
	}
	if r.floor != 0 && now > r.floor+r.keep {
		r.floor = 0
	}
	if r.floor != 0 && (!ok || r.floor > at) {
		return r.floor, true
	}
	return at, ok
}

// knownBarred reports whether the cache holds a fresh "barred" answer for dev. It reads memory
// only — no lookup, no insertion — so any caller may use it.
func (r *RelayRevocations) knownBarred(dev string) bool {
	if r.barred == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	bar, ok := r.cache.get(dev, r.clk.Now())
	return ok && bar
}

// checkBarred is the cached barred lookup for dev, which cuts a barred device as of now. It must be
// called only for a device whose request pion has authenticated, or one that holds a relay socket:
// it reads the store on a cache miss and caches the answer. A lookup that fails, or that finds
// maxBarredLookups already in flight, caches nothing and reports false: the device stays bounded
// by the cut map and turn.max_allocation_age, and its next authenticated request or the holders'
// re-check asks again. ctx ends with the relay (TURN.Close).
func (r *RelayRevocations) checkBarred(ctx context.Context, dev string) bool {
	if r.barred == nil {
		return false
	}
	now := r.clk.Now()
	r.mu.Lock()
	bar, ok := r.cache.get(dev, now)
	r.mu.Unlock()
	if ok {
		return bar
	}
	device, err := id.Parse(dev)
	if err != nil {
		return false
	}
	select {
	case r.inflight <- struct{}{}:
	default:
		r.warn("the relay's barred lookups are saturated; this device is checked on its next request", "device", dev)
		return false
	}
	lctx, cancel := context.WithTimeout(ctx, barredLookupTimeout)
	bar, err = r.barred.DeviceBarred(lctx, device)
	cancel()
	<-r.inflight
	if err != nil {
		if ctx.Err() == nil {
			r.warn("the relay's barred lookup failed; this device is checked on its next request", "device", dev, "err", err)
		}
		return false
	}
	r.mu.Lock()
	r.cache.put(dev, bar, now.Add(barredTTL))
	r.mu.Unlock()
	if bar {
		r.revoke(dev, now.UnixMilli())
	}
	return bar
}

// warn logs msg at most once per barredTTL, so a flood of fail-open lookups or overflowing cuts
// cannot flood the log. The limit is per message — the few constant messages of this file — so one
// kind never silences another.
func (r *RelayRevocations) warn(msg string, args ...any) {
	now := r.clk.Now()
	r.mu.Lock()
	last, seen := r.warned[msg]
	quiet := seen && now.Sub(last) < barredTTL
	if !quiet {
		r.warned[msg] = now
	}
	r.mu.Unlock()
	if !quiet {
		r.log.Warn(msg, args...)
	}
}

// checkHolders asks the barred lookup about every device holding a relay socket, which cuts the
// barred ones: what keeps a device that only sends ChannelData — never authenticated — from
// outliving its revocation by another process. It stops once ctx ends.
func (r *RelayRevocations) checkHolders(ctx context.Context) {
	if r.barred == nil {
		return
	}
	r.mu.Lock()
	devs := make([]string, 0, len(r.socks))
	for dev := range r.socks {
		devs = append(devs, dev)
	}
	r.mu.Unlock()
	for _, dev := range devs {
		if ctx.Err() != nil {
			return
		}
		r.checkBarred(ctx, dev)
	}
}

// startWatch runs checkHolders every holderEvery until the returned stop runs. stop cancels the
// lookups in flight and returns once the watch has ended (re-review N5).
func (r *RelayRevocations) startWatch(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(r.holderEvery)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				r.checkHolders(ctx)
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// track records c as one of dev's relay sockets, or refuses it (false) when a live cut of dev may
// cover the credential it was allocated with. It decides under the mutex revoke takes, so once
// Revoke(dev, t) has returned no socket of a credential issued at or before t can join the set
// revoke closed: the allocation's issue time is the one OnAuth recorded for the Allocate the quota
// handler admitted (admitAllocate) and pion creates this socket for, and with a live cut a socket is
// admitted only when every admitted Allocate of dev in the last pendingAllocateWindow used a
// credential issued after the cut. Allocates of a device that straddle its cut are refused together
// for that window, even one with a fresh credential; its client's next attempt after it succeeds.
func (r *RelayRevocations) track(dev string, c *countingConn) bool {
	now := r.clk.Now().UnixMilli()
	r.mu.Lock()
	defer r.mu.Unlock()
	p, havePending := r.pending[dev]
	if havePending && time.Duration(now-p.last)*time.Millisecond > pendingAllocateWindow {
		delete(r.pending, dev)
		havePending = false
	}
	// Each socket consumes one pending Allocate, admitted or refused, so the record ends with the
	// allocations it describes; its oldest issue time stays until then.
	if havePending {
		if p.count--; p.count <= 0 {
			delete(r.pending, dev)
		} else {
			r.pending[dev] = p
		}
	}
	if at, cut := r.liveCutLocked(dev); cut && (!havePending || p.minIssued <= at) {
		return false
	}
	set := r.socks[dev]
	if set == nil {
		set = map[*countingConn]struct{}{}
		r.socks[dev] = set
	}
	set[c] = struct{}{}
	return true
}

func (r *RelayRevocations) untrack(dev string, c *countingConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if set := r.socks[dev]; set != nil {
		delete(set, c)
		if len(set) == 0 {
			delete(r.socks, dev)
		}
	}
}

// barredCache is a bounded least-recently-used cache of barred lookups; its owner locks it.
type barredCache struct {
	max int
	ll  *list.List
	m   map[string]*list.Element
}

type barredEntry struct {
	dev   string
	bar   bool
	until time.Time
}

func newBarredCache(maxEntries int) *barredCache {
	return &barredCache{max: maxEntries, ll: list.New(), m: map[string]*list.Element{}}
}

func (c *barredCache) get(dev string, now time.Time) (bool, bool) {
	e, ok := c.m[dev]
	if !ok {
		return false, false
	}
	ent, _ := e.Value.(*barredEntry) // the list holds only *barredEntry
	if !now.Before(ent.until) {
		c.ll.Remove(e)
		delete(c.m, dev)
		return false, false
	}
	c.ll.MoveToFront(e)
	return ent.bar, true
}

func (c *barredCache) put(dev string, bar bool, until time.Time) {
	if e, ok := c.m[dev]; ok {
		ent, _ := e.Value.(*barredEntry) // the list holds only *barredEntry
		ent.bar, ent.until = bar, until
		c.ll.MoveToFront(e)
		return
	}
	c.m[dev] = c.ll.PushFront(&barredEntry{dev: dev, bar: bar, until: until})
	if c.ll.Len() > c.max {
		last := c.ll.Back()
		c.ll.Remove(last)
		if ent, ok := last.Value.(*barredEntry); ok {
			delete(c.m, ent.dev)
		}
	}
}
