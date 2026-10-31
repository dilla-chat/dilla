package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/fxamacker/cbor/v2"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/clock"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/obs"
	"github.com/jonasthim/dilla/internal/store"
)

// Options configures a Gateway. Every duration and byte count has a default; withDefaults fills
// them and enforces the ring-vs-queue contract.
type Options struct {
	// CurrentAppEntry returns the verified served web entry, never a staged file.
	CurrentAppEntry func() string
	// OnPresenceChanged publishes an account's newly derived presence to shared communities.
	OnPresenceChanged func(id.ID)
	// Store is deliberately narrow: the gateway calls exactly three storage methods, and holding
	// store.Repository here would make every gateway test build a database.
	Store   Store
	Clock   clock.Clock
	Log     *slog.Logger
	Metrics *obs.Metrics
	// Auth is an interface declared in this package, NOT *auth.Sessions: importing internal/auth
	// here closes the cycle auth -> ds -> gateway -> auth (deviation B9). *auth.Sessions satisfies
	// it structurally.
	Auth Authenticator

	// ScopeResolver and CommunityCompatibility are the narrow authoritative readers used by
	// wire-2 gateway admissions. They are separate from Store so codec/session tests do not need a
	// repository-shaped dependency. Production wires the resolver to current group/channel rows
	// and the shared bot capability policy to committed installation state.
	ScopeResolver          ScopeResolver
	CommunityCompatibility CommunityCompatibility

	// OnFactorOwed runs for a device whose connection the liveness sweep closed because its
	// account now owes a second factor (an operator's reset-factors, web3.6 S3), so the
	// composition root can cut the device from its live calls. Nil does nothing.
	OnFactorOwed func(device id.ID)

	// OnUserOffline runs once a user's last live connection has left the registry (AW2, Q2: it starts
	// the 30 s grace after which the user's AFK away rows clear, unless a connection comes back). A
	// second open connection, of any device of the user, keeps it from running. It runs on the
	// goroutine that removed the connection and must not block. Nil does nothing.
	OnUserOffline func(user id.ID)
	// OnTyping runs after an accepted typing_set on (AW2, Q1: typing is activity), with the user and
	// the channel. Nil does nothing.
	OnTyping func(ctx context.Context, user, channel id.ID)

	// Outstanding reports a group's non-void instance proposals, for ready's per-group digest.
	// The delivery service sets it in task 21; nil reads as 0.
	Outstanding func(ctx context.Context, groupID id.ID) uint64
	// CommitAck is invariant 7's acknowledgement handler, set by the delivery service in task 22.
	// Nil makes opcode 12 a no-op, which is what the codec-only tests want.
	CommitAck         func(ctx context.Context, groupID, deviceID id.ID, round uint64)
	TypingAudience    func(context.Context, id.ID, id.ID) ([]id.ID, error)
	VoiceSelfAudience func(context.Context, id.ID, id.ID) (id.ID, []id.ID, uint64, error)
	// VoiceSelfStored receives the self-mute and self-deafen bits (client bits 0 and 1) of a
	// voice_self that passed VoiceSelfAudience, keyed by (call, device), for the voice snapshot
	// (web-3 task 3); nil stores nothing.
	VoiceSelfStored func(call, device id.ID, bits uint64)

	// Advertised is what `hello` offers and what `identify` is validated against. Empty means
	// {1} for all three, the only versions this wave ships.
	WireVersions  []uint64
	E2EEVersions  []uint64
	MediaVersions []uint64

	// InstanceID and Generation are elements 5 and 6 of the hello payload (§2.3); TrustedOrigins
	// is http.trusted_origins (§6.1's transport paragraph). All three are additions to §6.1's
	// Options, recorded as deviation B10.
	InstanceID id.ID
	Generation uint64

	HeartbeatMS   uint64
	IdleClose     time.Duration
	ReadLimit     int64
	MaxFrameBytes uint64
	QueueFrames   int
	QueueBytes    int
	RingFrames    int
	RingBytes     int
	RingTTL       time.Duration
	ResumeWindow  time.Duration
	WriteDeadline time.Duration

	TrustedOrigins []string

	// Backoff and BackoffJitter are invariant 7's back-off window, elements 7 and 8 of hello
	// (deviation B23, ruling 44): "the other devices back off 300 ms + random(0..300 ms)". The
	// instance only ever addresses the elected committer, so every other device learns the window
	// here, once per connection. The composition root fills both from ds.Policy.
	Backoff       time.Duration
	BackoffJitter time.Duration

	// FramesPerSecond and FrameBurst are the inbound token bucket every connection reads through
	// (facts-gateway-design §6.3: 60 frames/s, burst 120; `gateway.frame_burst` and
	// `gateway.frame_burst_max`). Inbound is control-only, so a client past it is misbehaving:
	// it is sent E_RATE_LIMITED and closed 4008, which is resumable.
	FramesPerSecond float64
	FrameBurst      int
}

// withDefaults fills zero values and enforces the ring-vs-queue contract. The ring must be at
// least as large as the writer queue on both axes: a 4008 close is resumable, so whatever the
// queue dropped has to still be in the ring when the client comes back.
func (o Options) withDefaults() Options {
	if o.HeartbeatMS == 0 {
		o.HeartbeatMS = 30000
	}
	if o.IdleClose == 0 {
		o.IdleClose = 90 * time.Second
	}
	if o.ReadLimit == 0 {
		o.ReadLimit = 16384
	}
	if o.MaxFrameBytes == 0 {
		o.MaxFrameBytes = 131584
	}
	if o.QueueFrames == 0 {
		o.QueueFrames = 256
	}
	if o.QueueBytes == 0 {
		o.QueueBytes = 1 << 20
	}
	if o.RingFrames == 0 {
		o.RingFrames = 512
	}
	if o.RingBytes == 0 {
		o.RingBytes = 1 << 20
	}
	if o.RingTTL == 0 {
		o.RingTTL = 120 * time.Second
	}
	if o.ResumeWindow == 0 {
		o.ResumeWindow = 120 * time.Second
	}
	if o.WriteDeadline == 0 {
		o.WriteDeadline = 10 * time.Second
	}
	if o.Backoff == 0 {
		o.Backoff = 300 * time.Millisecond
	}
	if o.BackoffJitter == 0 {
		o.BackoffJitter = 300 * time.Millisecond
	}
	if o.FramesPerSecond <= 0 {
		o.FramesPerSecond = 60
	}
	if o.FrameBurst <= 0 {
		o.FrameBurst = 120
	}
	if o.RingFrames < o.QueueFrames {
		o.RingFrames = o.QueueFrames
	}
	if o.RingBytes < o.QueueBytes {
		o.RingBytes = o.QueueBytes
	}
	return o
}

func defaultOptions() Options { return Options{}.withDefaults() }

// Store is the storage surface the gateway uses. store.Repository satisfies it.
type Store interface {
	GroupsForDevice(ctx context.Context, deviceID id.ID) ([]id.ID, error)
	GetCursor(ctx context.Context, deviceID, groupID id.ID) (store.CursorRow, error)
	CountKeyPackages(ctx context.Context, deviceID id.ID, now int64) (int64, error)
}

// Authenticator resolves a bearer token to a device session. The gateway declares the interface
// rather than holding a *auth.Sessions so that the gateway's own tests need no session store;
// *auth.Sessions satisfies it structurally. See deviation B9.
type Authenticator interface {
	Resolve(ctx context.Context, bearer string) (auth.Session, error)
}

// CapabilityRecorder is optional for legacy test doubles and older in-process integrations. A
// production connection that selects wire 3 must have this recorder and persist before ready.
type CapabilityRecorder interface {
	RecordCapabilities(ctx context.Context, tokenHash []byte, wire, e2ee, media uint64) error
}

// Compile-time proof of the claim deviation B9 rests on: *auth.Sessions satisfies Authenticator
// without the gateway ever holding one. The gateway's own tests use a double, so without this the
// first thing to find out would be task 27a's wiring.
//
// The matching `var _ Store = (store.Repository)(nil)` waited for the embed that made it true:
// GroupsForDevice and CountKeyPackages live in store.MLS and GetCursor in store.Cursors, and
// Repository embedded neither until tasks 19 and 23 added them (deviation ID1). Task 23 closed
// that gap, so the assertion lands here with it.
var (
	_ Authenticator      = (*auth.Sessions)(nil)
	_ CapabilityRecorder = (*auth.Sessions)(nil)
	_ Store              = (store.Repository)(nil)
)

// OnlineDevice is one candidate for invariant 7's committer election.
type OnlineDevice struct {
	DeviceID  id.ID
	LeafIndex uint32
	IsBot     bool
	ReadyAt   time.Time
}

type Gateway struct {
	opts      Options
	reg       *registry
	tickets   *Tickets
	presence  *presence
	beat      heartbeatPolicy
	suspended sync.Map // resume token string -> *conn, inside the resume window

	// Test seams for the two steps of a resume that cannot fail with real inputs; nil means the
	// real one. They are set before the Gateway serves and never after.
	newResumeTokenFn func() ([]byte, error)
	resumedPayloadFn func(from, to uint64, resumeToken []byte) (cbor.RawMessage, error)

	// generation is the instance generation every `hello`, every `ready` and every resume check
	// reads. It is an atomic and NOT `opts.Generation` because invariant 11 moves it while
	// connections are live: `dillad restore` bumps it, and the whole point of the bump is that
	// outstanding resume tokens stop being accepted from that moment. Reading the immutable
	// Options field would mean a restored instance kept honouring pre-restore resume tokens for
	// the life of the process — replaying, to a client that never learned the database changed
	// underneath it, a ring of frames about epochs the instance no longer holds.
	//
	// Options.Generation is still the value the gateway STARTS at; SetGeneration is how the
	// delivery service publishes a later one.
	generation atomic.Uint64

	mu        sync.Mutex
	draining  bool
	telemetry *telemetryPool
}

func New(o Options) *Gateway {
	o = o.withDefaults()
	if o.Clock == nil {
		o.Clock = clock.System()
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	g := &Gateway{
		opts:     o,
		reg:      newRegistry(),
		tickets:  NewTickets(o.Clock),
		presence: newPresence(o.Clock),
		beat:     newHeartbeatPolicy(time.Duration(o.HeartbeatMS) * time.Millisecond), //nolint:gosec // G115: a non-negative duration in milliseconds
	}
	g.generation.Store(o.Generation)
	return g
}

// Generation is the instance generation the gateway is currently advertising.
func (g *Gateway) Generation() uint64 { return g.generation.Load() }

// SetGeneration publishes a new instance generation, which is what invariant 11's restore does.
// Every later `hello` and `ready` carries it, and every resume that presents the old one is
// refused — which is the whole mechanism by which a restore tells a client "what you remember
// about this instance is no longer true".
//
// It is MONOTONE, like the column behind it: a caller that named an older generation would
// otherwise revive exactly the tokens the last restore invalidated.
func (g *Gateway) SetGeneration(generation uint64) {
	for {
		current := g.generation.Load()
		if generation <= current {
			return
		}
		if g.generation.CompareAndSwap(current, generation) {
			return
		}
	}
}

// Tickets exposes the ticket store to the HTTP layer's POST /v1/gateway/ticket handler.
func (g *Gateway) Tickets() *Tickets { return g.tickets }

// register builds a ready connection over a transport sink and sends its `ready` frame, with the
// instance's own first advertised version on each of the three axes. The tests call it directly;
// the upgrade handler goes through registerVersions, because `ready` echoes what the client chose
// and the echo is built inside sendReady.
func (g *Gateway) register(ctx context.Context, session auth.Session, s sink) (*conn, error) {
	wire, e2ee, media := g.advertised()
	return g.registerVersions(ctx, session, s, [3]uint64{wire[0], e2ee[0], media[0]})
}

// registerVersions is register with the versions identify negotiated. They are set on the
// connection BEFORE ready is built: ready's elements 4, 5 and 6 are the echo, and a connection
// that learned its versions after sendReady would echo three zeros.
func (g *Gateway) registerVersions(ctx context.Context, session auth.Session, s sink, chosen [3]uint64) (*conn, error) {
	if session.Bot != nil && (session.Bot.State != store.BotInstallationJoining && session.Bot.State != store.BotInstallationReady || chosen[0] < 3) {
		return nil, errNotEnrolledSession
	}
	if err := g.recordCapabilities(ctx, session.TokenHash, chosen); err != nil {
		return nil, err
	}
	token, err := newResumeToken()
	if err != nil {
		return nil, err
	}
	now := g.opts.Clock.Now()
	c := &conn{
		state:        stateReady,
		userID:       session.UserID,
		deviceID:     session.DeviceID,
		scope:        session.Scope,
		pairingGroup: session.PairingGroup,
		isBot:        session.Bot != nil,
		tokenHash:    session.TokenHash,
		resume:       token,
		groups:       map[id.ID]struct{}{},
		lastLive:     now,
		ready:        now,
		gw:           g,
		ring: newRing(ringLimits{
			Frames: g.opts.RingFrames,
			Bytes:  g.opts.RingBytes,
			TTL:    g.opts.RingTTL,
		}, g.opts.Clock),
	}
	c.wire, c.e2ee, c.media = chosen[0], chosen[1], chosen[2]
	// Paused until the connection is live: see newPausedWriter.
	c.writer = newPausedWriter(s, writerLimits{
		Frames:   g.opts.QueueFrames,
		Bytes:    g.opts.QueueBytes,
		Deadline: g.opts.WriteDeadline,
	}, g.opts.Clock, g.opts.Log)
	// The server's own ping ticker starts at a jittered offset so 1,500 clients reconnecting
	// after a restart do not synchronise onto one second (task 17's firstDelay, which nothing
	// else calls).
	c.pingAt = now.Add(g.beat.firstDelay())
	if err := g.sendReady(ctx, c); err != nil {
		c.currentWriter().stop()
		return nil, err
	}
	g.sendAppBuild(c)
	// The connection joins the live registry only once `ready` is on its writer. reg.add before
	// sendReady publishes the connection while sendReady is still doing three store round-trips,
	// so a DeliverGroup landing in that window takes n = 1 and `ready` then carries n = 2 —
	// against protocol/02's "n ... the per-session replay sequence, starting at 1" and against
	// the hello/identify/ready handshake order a client is written to.
	g.addLive(c)
	// Only now may `ready` reach the wire: a client that has read it is a device Online counts.
	c.currentWriter().open()
	g.touch(ctx, c.deviceID, now)
	return c, nil
}

func (g *Gateway) recordCapabilities(ctx context.Context, tokenHash []byte, chosen [3]uint64) error {
	recorder, ok := g.opts.Auth.(CapabilityRecorder)
	if !ok {
		if chosen[0] >= 3 {
			return errors.New("gateway: wire 3 requires session capability persistence")
		}
		return nil
	}
	if len(tokenHash) != 32 {
		return errors.New("gateway: session capability binding is missing")
	}
	return recorder.RecordCapabilities(ctx, tokenHash, chosen[0], chosen[1], chosen[2])
}

// deviceToucher is the one optional store method the gateway calls: store.Repository's
// TouchDevice. It is not part of Store, so the narrow interface — and every double that satisfies
// it — is unchanged; a store without it simply records nothing.
type deviceToucher interface {
	TouchDevice(ctx context.Context, deviceID id.ID, lastSeen int64) error
}

// touch records that the device connected. protocol/01 § Cadence removes "a device that has not
// connected for 90 days", and invariant 10's cursor eligibility skips a device unseen for as long;
// both read `devices.last_seen`, and a connection reaching `ready` is the device connecting. A
// failure is logged, never a refusal: the connection is already good.
func (g *Gateway) touch(ctx context.Context, device id.ID, now time.Time) {
	t, ok := g.opts.Store.(deviceToucher)
	if !ok {
		return
	}
	if err := t.TouchDevice(ctx, device, now.Unix()); err != nil && g.opts.Log != nil {
		g.opts.Log.Warn("recording the device as seen failed", "device", device.String()[:8], "err", err)
	}
}

// addLive publishes a connection to the live registry and counts it. Every path that takes a
// connection back out goes through removeLive, so dilla_gateway_connections is the number of OPEN
// connections and not the number ever opened.
func (g *Gateway) addLive(c *conn) {
	g.reg.add(c)
	// Legacy clients retain their connection-time initial presence. Wire 5 only counts real input.
	if g.connectionWire(c) < 5 {
		g.presence.activity(c.deviceID, c.userID)
	}
	if g.opts.OnPresenceChanged != nil {
		g.opts.OnPresenceChanged(c.userID)
	}
	if g.opts.Metrics != nil {
		g.opts.Metrics.GatewayConnections.Inc()
	}
}

// removeLive takes a connection out of the live registry, decrementing the gauge only if this
// call is the one that removed it.
func (g *Gateway) removeLive(c *conn) {
	if removed, last := g.reg.remove(c); removed {
		if pool := g.telemetryPool(); pool != nil {
			pool.disconnect(c)
		}
		// A shutdown's closes are no user going offline: the stored away roster outlives a
		// restart for the users who reconnect (AW2, R-AW5).
		if last && g.opts.OnUserOffline != nil && !g.isDraining() {
			g.opts.OnUserOffline(c.userID)
		}
		if len(g.reg.connsOfDevice(c.deviceID)) == 0 {
			g.presence.clearActivity(c.deviceID)
		}
		if g.opts.OnPresenceChanged != nil {
			g.opts.OnPresenceChanged(c.userID)
		}
		if g.opts.Metrics != nil {
			g.opts.Metrics.GatewayConnections.Dec()
		}
		if len(g.reg.connsOfDevice(c.deviceID)) == 0 {
			g.presence.clearDeviceTyping(c.deviceID)
		}
	}
}

// closeConn closes one connection's socket with a code, and records on the connection whether the
// code leaves it resumable. protocol/02's close-code table is the authority: 4004 session_revoked
// and 4009 session_timeout are NOT resumable, and a connection closed with one of those must not
// come back through the resume window with the token it already holds.
func (g *Gateway) closeConn(c *conn, code CloseCode, reason string) *writer {
	if !code.Resumable() {
		c.denyResume()
	}
	w := c.currentWriter()
	w.sink.close(code, reason)
	return w
}

// suspend moves a connection out of the live registry into the resume window. Its ring survives
// for ResumeWindow so a reconnecting client can replay; it is NOT online while suspended.
//
// The expiry is swept against g.opts.Clock, not time.AfterFunc: every other deadline in the
// gateway (the ring TTL, liveness, tickets) is driven by the injected clock, and a wall-clock
// timer here would make the resume window the one behaviour no test can drive — under clock.Fake
// it would never fire, and `suspended` would grow without bound for the life of the test binary.
func (g *Gateway) suspend(c *conn) {
	now := g.opts.Clock.Now()
	c.mu.Lock()
	c.state = stateClosed
	c.suspendedAt = now
	token := string(c.resume)
	w := c.writer
	resumable := !c.noResume
	c.mu.Unlock()
	g.removeLive(c)
	// A connection closed with a non-resumable code (4004 session_revoked, 4009 session_timeout)
	// is NOT parked in the resume window. readLoop's `defer g.suspend(c)` fires after the close,
	// so without this gate a device whose sessions were just revoked could reconnect inside
	// ResumeWindow with nothing but its old resume token.
	if resumable {
		g.suspended.Store(token, c)
	}
	if w != nil {
		w.stop()
	}
	// WARN, not INFO: a suspended connection stops receiving fan-out, so when a device later
	// reports "nothing arrived" this line is the one that says the instance stopped sending.
	if g.opts.Log != nil {
		// The FULL id: harness device ids share a zero prefix, so eight digits name no one.
		g.opts.Log.Warn("gateway: connection suspended", "device", c.deviceID.String(),
			"resumable", resumable)
	}
}

// sweepSuspended drops suspended connections past the resume window. sweepLiveness calls it on
// every tick, so it shares that ticker.
func (g *Gateway) sweepSuspended() {
	now := g.opts.Clock.Now()
	g.suspended.Range(func(k, v any) bool {
		c, ok := v.(*conn)
		if !ok {
			g.suspended.Delete(k)
			return true
		}
		c.mu.Lock()
		expired := now.Sub(c.suspendedAt) > g.opts.ResumeWindow
		c.mu.Unlock()
		if expired {
			g.suspended.Delete(k)
		}
		return true
	})
}

// rotateResume issues a fresh resume token, which happens on every resumed frame.
func (g *Gateway) rotateResume(c *conn) error {
	newToken := g.newResumeTokenFn
	if newToken == nil {
		newToken = newResumeToken
	}
	token, err := newToken()
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.resume = token
	c.mu.Unlock()
	return nil
}

// DeliverGroup fans one frame out to every connection of every member device of a group,
// including the uploader's own (R30). The payload is encoded once by the caller.
func (g *Gateway) DeliverGroup(groupID id.ID, f Frame) {
	f.Scope = normalizeFrameScope(f)
	f.Scope.Groups = appendScopeID(f.Scope.Groups, groupID)
	requiresWire3, err := g.scopeRequiresWire3(context.Background(), f.Scope)
	if err != nil {
		return
	}
	for _, c := range g.reg.connsOfGroup(groupID) {
		if g.allowWire3Scoped(c, requiresWire3) {
			c.send(f)
		}
	}
	g.countFrame(f)
}

func (g *Gateway) DeliverDevice(deviceID id.ID, f Frame) {
	f.Scope = normalizeFrameScope(f)
	requiresWire3, err := g.scopeRequiresWire3(context.Background(), f.Scope)
	if err != nil {
		return
	}
	for _, c := range g.reg.connsOfDevice(deviceID) {
		if g.allowWire3Scoped(c, requiresWire3) {
			c.send(f)
		}
	}
	g.countFrame(f)
}

func (g *Gateway) DeliverUser(userID id.ID, f Frame) {
	f.Scope = normalizeFrameScope(f)
	requiresWire3, err := g.scopeRequiresWire3(context.Background(), f.Scope)
	if err != nil {
		return
	}
	for _, c := range g.reg.connsOfUser(userID) {
		if g.allowWire3Scoped(c, requiresWire3) {
			c.send(f)
		}
	}
	g.countFrame(f)
}

// SetChannelMembers replaces the users a readable channel's frames reach (Plan 2 task 8, P2-D14).
// A readable channel has no MLS group, so DeliverGroup cannot reach it; the api layer writes the
// channel's audience here — its channel_members that are still community members — before it
// delivers, which is how a user who lost the channel stops receiving it. An empty list
// unsubscribes everyone.
func (g *Gateway) SetChannelMembers(channelID id.ID, users []id.ID) {
	g.reg.setChannelMembers(channelID, users)
}

// DeliverChannel fans one frame out to every live connection of every user in the channel's
// audience, the author's own included (R30's rule for message.ct, applied to message.plain). The
// payload is encoded once by the caller; a replayable frame is recorded in each connection's ring,
// so a client that reconnects inside the ring window does not have to re-fetch.
func (g *Gateway) DeliverChannel(channelID id.ID, f Frame) {
	f.Scope = normalizeFrameScope(f)
	f.Scope.Channels = appendScopeID(f.Scope.Channels, channelID)
	requiresWire3, err := g.scopeRequiresWire3(context.Background(), f.Scope)
	if err != nil {
		return
	}
	for _, c := range g.reg.connsOfChannel(channelID) {
		if g.allowWire3Scoped(c, requiresWire3) {
			c.send(f)
		}
	}
	g.countFrame(f)
}

func (g *Gateway) countFrame(f Frame) {
	if g.opts.Metrics != nil {
		g.opts.Metrics.GatewayFrames.WithLabelValues(opLabel(f.Op), "out").Inc()
	}
}

// CloseDevice ends every connection of one device. Accepting a signed device list that revokes a
// device, or disabling a user, closes its sockets in the same transaction that deletes its
// sessions (protocol/02, "Device sessions", rule 6).
func (g *Gateway) CloseDevice(deviceID id.ID, code CloseCode, reason string) {
	for _, c := range g.reg.connsOfDevice(deviceID) {
		w := g.closeConn(c, code, reason)
		g.removeLive(c)
		w.stop()
	}
	if code.Resumable() {
		return
	}
	// A connection SUSPENDED before the revoke is still sitting in the resume window holding a
	// live token, and it is not in the registry the loop above walked. Revoking a device has to
	// take those with it, or the revocation only closes the sockets that happened to be open.
	g.suspended.Range(func(k, v any) bool {
		c, ok := v.(*conn)
		if !ok || c.deviceID != deviceID {
			return true
		}
		c.denyResume()
		g.suspended.Delete(k)
		return true
	})
}

// SetGroupMembers is written by the delivery service after every merge: it is the fan-out list and
// the input to the online predicate, and it is deliberately the DS's own view rather than a query.
func (g *Gateway) SetGroupMembers(groupID id.ID, devices []id.ID) {
	g.reg.setMembers(groupID, devices)
}

// SetGroupLeaves records each member device's leaf index, which invariant 7's election orders by.
func (g *Gateway) SetGroupLeaves(groupID id.ID, leaves map[id.ID]uint32) {
	g.reg.setLeaves(groupID, leaves)
}

// SetBotDevices marks devices that go first in the election (protocol/02 invariant 7).
func (g *Gateway) SetBotDevices(devices []id.ID) { g.reg.setBots(devices) }

// MaxFrameBytes is the frame budget this gateway advertises in `hello` (§2.3 element 4). A
// producer whose payload would not fit must not enqueue it: a client sizes its own websocket read
// limit from this number, so an oversize frame does not merely fail to arrive — it takes the
// connection down with it, and the client comes back to be sent the same frame again.
//
// It is exported for the delivery service's addressed `mls.welcome`, whose ratchet tree is the one
// payload in the protocol with no bound of its own: a 1,500-leaf tree is 620 KiB.
func (g *Gateway) MaxFrameBytes() uint64 { return g.opts.MaxFrameBytes }

// Online is the single source for invariants 5, 6 and 7. A device is online while it holds a
// connection in state ready whose last liveness mark — ready, resumed or a heartbeat frame — is
// newer than session_idle_close. A device whose only session is inside the resume grace window
// has no connection and is therefore not online.
func (g *Gateway) Online(deviceID id.ID) bool {
	now := g.opts.Clock.Now()
	for _, c := range g.reg.connsOfDevice(deviceID) {
		if c.online(now, g.opts.IdleClose) {
			return true
		}
	}
	return false
}

// Draining reports whether Shutdown has begun: from then on every connection is closing, and no
// user's lack of one means they went offline (AW2, R-AW5).
func (g *Gateway) Draining() bool { return g.isDraining() }

// isDraining reports whether Shutdown has begun.
func (g *Gateway) isDraining() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.draining
}

// UserConnected reports whether the user holds any live connection, ready or not, on any device.
// A connection in the resume window is not live.
func (g *Gateway) UserConnected(userID id.ID) bool {
	return len(g.reg.connsOfUser(userID)) > 0
}

func (g *Gateway) OnlineIn(groupID id.ID) []OnlineDevice {
	requiresWire3, err := g.scopeRequiresWire3(context.Background(), FrameScope{Groups: []id.ID{groupID}})
	if err != nil {
		return nil
	}
	minWire := uint64(0)
	if requiresWire3 {
		minWire = 3
	}
	return g.reg.onlineIn(groupID, g.opts.Clock.Now(), g.opts.IdleClose, minWire)
}

// sweepLiveness closes every connection whose heartbeat is overdue (4009) and every one whose
// session row is gone (4004, sweepSessions). It runs on Run's ticker in
// production and is called directly by tests driving a clock.Fake.
func (g *Gateway) sweepLiveness() {
	now := g.opts.Clock.Now()
	for _, c := range g.reg.all() {
		c.mu.Lock()
		overdue := now.Sub(c.lastLive) > g.beat.grace
		c.mu.Unlock()
		if overdue {
			// 4009 session_timeout is not resumable, so closeConn marks the connection before
			// suspend looks: the client re-identifies rather than replaying a dead session.
			g.closeConn(c, CloseSessionTimeout, "heartbeat overdue")
			g.suspend(c)
		}
	}
	g.sweepSessions(context.Background())
	for _, user := range g.presence.expireActivity(now) {
		if g.opts.OnPresenceChanged != nil {
			g.opts.OnPresenceChanged(user)
		}
	}
	g.sweepSuspended()
}

// Run starts the gateway's maintenance loop and returns the function that stops it. Every
// heartbeat interval, on g.opts.Clock, it closes the connections whose heartbeat is overdue
// (4009), drops the suspended connections past the resume window — their rings with them — and
// then runs each of `also`, which is how the composition root puts the auth throttle's and the
// session table's sweeps on the same ticker.
//
// The first timer is armed BEFORE Run returns, not inside the goroutine: under clock.Fake a timer
// created after the test's first Advance would be armed from the advanced time and never fire for
// the advance that was meant to trigger it. stop is idempotent and waits for the loop to exit.
func (g *Gateway) Run(ctx context.Context, also ...func(context.Context)) (stop func()) {
	interval := g.beat.interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	timer := g.opts.Clock.NewTimer(interval)
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C():
				g.sweepLiveness()
				for _, f := range also {
					f(ctx)
				}
				timer.Reset(interval)
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}

// Suspended is how many connections are parked in the resume window, each holding its ring. It
// is what the maintenance loop bounds, and what a test through the composition root reads.
func (g *Gateway) Suspended() int {
	n := 0
	g.suspended.Range(func(any, any) bool {
		n++
		return true
	})
	return n
}

// Shutdown tells every client to reconnect, waits for that frame to reach the wire, then closes
// 4010 (resumable).
//
// The drain is load-bearing, not politeness: `writer.run` selects between `done` and `queue`, and
// Go picks at random when both are ready, so closing the sink and stopping the writer immediately
// after enqueueing `reconnect` drops that frame about half the time —
// TestShutdownSendsReconnectThenClosesGoingAway would be flaky by construction. `ctx` bounds the
// wait; on expiry the writer is stopped and the socket closed regardless.
func (g *Gateway) Shutdown(ctx context.Context) error {
	g.mu.Lock()
	if g.draining {
		g.mu.Unlock()
		return nil
	}
	g.draining = true
	g.mu.Unlock()

	var telemetryErr error
	if pool := g.telemetryPool(); pool != nil {
		telemetryErr = pool.stop(ctx)
	}
	conns := g.reg.all()
	p, err := ReconnectPayload("going away", 2500)
	if err != nil {
		return err
	}
	for _, c := range conns {
		c.send(Frame{Op: OpReconnect, Payload: p})
	}
	for _, c := range conns {
		w := c.currentWriter()
		w.drain(ctx)
		w.sink.close(CloseGoingAway, "going away")
		g.removeLive(c)
	}
	return telemetryErr
}

// Handler serves the /gateway upgrade over HTTP/1.1 only: a WebSocket needs Hijacker, so
// http2xconnect is never set (R19, facts-http-gateway §6).
func (g *Gateway) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Accept echoes a subprotocol only if it is listed here, and a browser that cannot set an
		// Authorization header offers `dilla.v1` alongside `dilla.ticket.<t>`. Listing `dilla.v1`
		// first makes it the negotiated one; the ticket value is read off the raw header by
		// bearerOrTicket and is never echoed, which is what keeps the credential out of the
		// response.
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			OriginPatterns:     g.opts.TrustedOrigins,
			Subprotocols:       []string{"dilla.v1"},
			CompressionMode:    websocket.CompressionDisabled,
			InsecureSkipVerify: false,
		})
		if err != nil {
			return
		}
		c.SetReadLimit(g.opts.ReadLimit)
		g.serve(r.Context(), newWSSink(c), bearerOrTicket(r, g.tickets))
	})
}

func opLabel(op Op) string {
	if name, ok := opNames[op]; ok {
		return name
	}
	return "unknown"
}

var opNames = map[Op]string{
	OpHello: "hello", OpIdentify: "identify", OpResume: "resume", OpReady: "ready",
	OpResumed: "resumed", OpInvalidSession: "invalid_session", OpHeartbeat: "heartbeat",
	OpHeartbeatAck: "heartbeat_ack", OpReconnect: "reconnect", OpError: "error",
	OpSubscribe: "subscribe", OpUnsubscribe: "unsubscribe", OpCommitAck: "commit_ack",
	OpMLSHandshake: "mls.handshake", OpMLSCommitNeeded: "mls.commit_needed",
	OpMLSEpochChanged: "mls.epoch_changed", OpMessageCT: "message.ct",
	OpMLSWelcome: "mls.welcome", OpMessageDeleted: "message.deleted",
	OpMessagePlain: "message.plain", OpInteraction: "interaction",
	OpPresence: "presence", OpTyping: "typing", OpVoiceState: "voice_state",
	OpTypingSet: "typing_set", OpVoiceSelf: "voice_self", OpCallRing: "call_ring",
	OpCommunityChanged: "community_changed", OpAFKAway: "afk_away",
	OpAppBuild: "app_build", OpPresenceActivity: "presence_activity", OpCallStats: "call_stats",
}
