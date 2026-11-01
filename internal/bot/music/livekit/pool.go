package livekit

import (
	"context"
	"errors"
	"sync"
	"time"

	lkproto "github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/music/counter"
	"github.com/jonasthim/dilla/internal/bot/music/runtime"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/media"
	"github.com/jonasthim/dilla/internal/sframe"
)

var (
	ErrPoolCallEnded = errors.New("music livekit: server call ended")
	ErrPoolClosed    = errors.New("music livekit: publisher pool is closed")
	ErrPoolDead      = errors.New("music livekit: publisher room ended; explicit reset required")
	ErrPoolBusy      = errors.New("music livekit: publisher track is already active")
)

type poolRoom interface{ Disconnect() }

type poolBackend interface {
	connect(context.Context, client.Call, func()) (poolRoom, error)
	publish(poolRoom, sourceLifecycle, *sframe.Sender) (func() error, error)
}

type liveKitPoolBackend struct{}

func (liveKitPoolBackend) connect(ctx context.Context, call client.Call, disconnected func()) (poolRoom, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	logger := newSDKLogger(ctx)
	room, err := lksdk.ConnectToRoomWithToken(call.LiveKitURL, call.Token,
		&lksdk.RoomCallback{OnDisconnected: disconnected}, lksdk.WithAutoSubscribe(false), lksdk.WithLogger(logger))
	if err != nil {
		return nil, logger.wrap(err)
	}
	return room, nil
}

func (liveKitPoolBackend) publish(room poolRoom, source sourceLifecycle, sender *sframe.Sender) (func() error, error) {
	liveRoom, ok := room.(*lksdk.Room)
	runtimeSource, sourceOK := source.(*runtime.Session)
	if !ok || !sourceOK || runtimeSource == nil || sender == nil {
		return nil, ErrBinding
	}
	publication, err := media.PublishLiveOpus(liveRoom, runtimeSource.Provider(), sender)
	if err != nil {
		return nil, err
	}
	return func() error { return liveRoom.LocalParticipant.UnpublishTrack(publication.SID()) }, nil
}

type poolBinding struct {
	callID id.ID
	group  id.ID
	url    string
}

func bindingFor(call client.Call, group id.ID) poolBinding {
	return poolBinding{callID: call.CallID, group: group, url: call.LiveKitURL}
}

type poolConnection struct {
	room poolRoom
	once sync.Once
}

func (c *poolConnection) disconnect() {
	if c != nil && c.room != nil {
		c.once.Do(c.room.Disconnect)
	}
}

// Pool reuses one authorized room across consecutive track publications. Its
// lifetime begins when the room connection starts and is never extended by
// track restarts or idle time.
type Pool struct {
	mu       sync.Mutex
	lifetime time.Duration
	backend  poolBackend

	closed      bool
	dead        bool
	deadReason  error
	connecting  bool
	bound       *poolBinding
	roomCtx     context.Context
	roomCancel  context.CancelFunc
	connection  *poolConnection
	active      *Session
	connectDone chan struct{}
	startDone   chan struct{}
	closeDone   chan struct{}
	generation  uint64
	roomEnded   func()

	// rtt reports the room's RTT for the stage (Signal task 8c: the bot's "63 ms"); nil reports nothing. roomRTT reads it
	// from a room (lksdkRoomRTT by default; a test's fake), rttEvery is the read cadence.
	rtt      func(context.Context, id.ID, uint32) error
	roomRTT  func(poolRoom) (time.Duration, bool)
	rttEvery time.Duration
}

// RTTStep is how far the room's RTT must move before it is reported again.
const RTTStep = 5 * time.Millisecond

// SetRTTReporter makes every room the pool connects report its RTT to the SFU through report (client.Session.ReportCallRTT:
// dillad writes it as this device's dilla.rtt attribute) every 2 s while it changes by RTTStep or more. A refused report is
// dropped; the next read tries again.
func (p *Pool) SetRTTReporter(report func(context.Context, id.ID, uint32) error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rtt = report
}

// SetRoomEndedReporter reports an explicit idle leave or definitive server call
// end once per generation.
// Transport disconnects, lifetime expiry and epoch changes may reconnect to the
// same call and retain its registration. Reports run outside the pool lock.
func (p *Pool) SetRoomEndedReporter(report func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.roomEnded = report
}

// lksdkRoomRTT is the selected ICE candidate pair's current round-trip time on the room's publisher connection (the
// subscriber's when there is none), as the web reads its own (the transport's selected pair).
func lksdkRoomRTT(room poolRoom) (time.Duration, bool) {
	r, ok := room.(*lksdk.Room)
	if !ok || r.LocalParticipant == nil {
		return 0, false
	}
	pc := r.LocalParticipant.GetPublisherPeerConnection()
	if pc == nil {
		pc = r.LocalParticipant.GetSubscriberPeerConnection()
	}
	if pc == nil {
		return 0, false
	}
	for _, st := range pc.GetStats() {
		pair, ok := st.(webrtc.ICECandidatePairStats)
		if ok && pair.Nominated && pair.State == webrtc.StatsICECandidatePairStateSucceeded && pair.CurrentRoundTripTime > 0 {
			return time.Duration(pair.CurrentRoundTripTime * float64(time.Second)), true
		}
	}
	return 0, false
}

// reportRTT reads room's RTT every rttEvery until ctx ends and reports it when it moved by RTTStep or more.
func (p *Pool) reportRTT(ctx context.Context, callID id.ID, room poolRoom, report func(context.Context, id.ID, uint32) error) {
	read, every := p.roomRTT, p.rttEvery
	if read == nil {
		read = lksdkRoomRTT
	}
	if every <= 0 {
		every = 2 * time.Second
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	sent := time.Duration(-1)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		rtt, ok := read(room)
		if !ok {
			continue
		}
		rtt = min(rtt.Round(time.Millisecond), 5*time.Second)
		if sent >= 0 && (rtt-sent).Abs() < RTTStep {
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, every)
		err := report(rctx, callID, uint32(rtt/time.Millisecond)) //nolint:gosec // G115: at most 5000
		cancel()
		if err == nil {
			sent = rtt
		}
	}
}

// NewPool creates a publisher room pool whose absolute connection lifetime is
// bounded to at most 30 minutes.
func NewPool(lifetime time.Duration) (*Pool, error) {
	if lifetime <= 0 || lifetime > maxPublisherLifetime {
		return nil, ErrBinding
	}
	return &Pool{lifetime: lifetime, backend: liveKitPoolBackend{}, closeDone: make(chan struct{})}, nil
}

// Start publishes one encoder session into the retained room. A completed
// track leaves the room connected; only explicit Reset can replace a dead room.
func (p *Pool) Start(ctx context.Context, call client.Call, boundGroup id.ID, epochs EpochSource, source *runtime.Session) (*Session, error) {
	if source == nil {
		return nil, ErrBinding
	}
	return p.startSource(ctx, call, boundGroup, epochs, source)
}

func (p *Pool) startSource(ctx context.Context, call client.Call, boundGroup id.ID, epochs EpochSource, source sourceLifecycle) (*Session, error) {
	if ctx == nil || source == nil || epochs == nil || call.CallID.IsZero() || call.GroupID.IsZero() || boundGroup.IsZero() ||
		call.GroupID != boundGroup || call.LiveKitURL == "" || call.Token == "" {
		if source != nil {
			_ = source.Close()
		}
		return nil, ErrBinding
	}
	initial, err := epochs.CallMediaEpoch(ctx, boundGroup)
	if err == nil {
		err = validateSenderEpoch(initial)
	}
	if err = startStage(ctx, "media_epoch", err); err != nil {
		_ = source.Close()
		return nil, err
	}
	state, ok := epochs.(counter.State)
	if !ok {
		_ = source.Close()
		return nil, startStage(ctx, "counter_reserve", ErrBinding)
	}
	sender, err := senderForReservedEpoch(ctx, state, boundGroup, initial)
	if err = startStage(ctx, "counter_reserve", err); err != nil {
		_ = source.Close()
		return nil, err
	}
	binding := bindingFor(call, boundGroup)
	roomCtx, conn, generation, err := p.acquireRoom(ctx, call, binding)
	if err == nil {
		err = revalidateEpoch(roomCtx, boundGroup, epochs, initial)
		if err != nil {
			p.markDead(generation, err)
		}
	}
	if err = startStage(ctx, "room_connect", err); err != nil {
		_ = source.Close()
		return nil, err
	}

	trackCtx, trackCancel := context.WithCancel(roomCtx)
	stopParent := context.AfterFunc(ctx, trackCancel)
	p.mu.Lock()
	if p.closed || p.dead || p.generation != generation || p.connection != conn {
		p.mu.Unlock()
		stopParent()
		trackCancel()
		_ = source.Close()
		return nil, startStage(ctx, "track_publish", p.currentError())
	}
	if p.active != nil || p.connecting {
		p.mu.Unlock()
		stopParent()
		trackCancel()
		_ = source.Close()
		return nil, startStage(ctx, "track_publish", ErrPoolBusy)
	}
	p.connecting = true // reserves the single publication slot during PublishTrack.
	startDone := make(chan struct{})
	p.startDone = startDone
	p.mu.Unlock()
	if err := trackCtx.Err(); err != nil {
		stopParent()
		trackCancel()
		_ = source.Close()
		terminal := p.lifecycleError(err)
		p.finishStarting(generation, startDone)
		return nil, startStage(ctx, "track_publish", terminal)
	}

	unpublish, err := p.backend.publish(conn.room, source, sender)
	if err != nil {
		stopParent()
		trackCancel()
		_ = source.Close()
		p.markDead(generation, err)
		p.finishStarting(generation, startDone)
		return nil, startStage(ctx, "track_publish", err)
	}
	if err := trackCtx.Err(); err != nil {
		_ = unpublish()
		stopParent()
		trackCancel()
		_ = source.Close()
		p.markDead(generation, err)
		terminal := p.lifecycleError(err)
		p.finishStarting(generation, startDone)
		return nil, startStage(ctx, "track_publish", terminal)
	}
	s := &Session{
		source: source, cancel: trackCancel, done: make(chan struct{}),
		unpublish: unpublish, pool: p, generation: generation, sender: sender,
		stopParent: stopParent,
	}
	p.mu.Lock()
	if p.closed || p.dead || p.generation != generation || p.connection != conn {
		p.mu.Unlock()
		s.finish(ErrPoolDead)
		terminal := p.lifecycleError(ErrPoolDead)
		close(s.done)
		p.finishStarting(generation, startDone)
		return nil, startStage(ctx, "track_publish", terminal)
	}
	p.active = s
	p.finishStartingLocked(generation, startDone)
	p.mu.Unlock()
	_ = startStage(ctx, "track_publish", nil)
	go s.monitorPooled(trackCtx, boundGroup, epochs, initial)
	return s, nil
}

func (p *Pool) acquireRoom(ctx context.Context, call client.Call, binding poolBinding) (context.Context, *poolConnection, uint64, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, nil, 0, ErrPoolClosed
	}
	if p.dead {
		p.mu.Unlock()
		return nil, nil, 0, ErrPoolDead
	}
	if p.bound != nil && *p.bound != binding {
		gen := p.generation
		p.mu.Unlock()
		p.markDead(gen, ErrBinding)
		return nil, nil, 0, ErrBinding
	}
	if p.active != nil || p.connecting {
		p.mu.Unlock()
		return nil, nil, 0, ErrPoolBusy
	}
	if p.connection != nil {
		ctx := p.roomCtx
		conn := p.connection
		gen := p.generation
		p.mu.Unlock()
		return ctx, conn, gen, nil
	}
	if p.bound == nil {
		b := binding
		p.bound = &b
	}
	roomCtx, cancel := context.WithTimeout(detachedLoggerContext(ctx), p.lifetime)
	p.roomCtx, p.roomCancel = roomCtx, cancel
	p.generation++
	gen := p.generation
	p.connecting = true
	connectDone := make(chan struct{})
	p.connectDone = connectDone
	p.mu.Unlock()

	connectCtx, cancelConnect := context.WithCancel(roomCtx)
	stopParentCancel := context.AfterFunc(ctx, cancelConnect)
	room, err := p.backend.connect(connectCtx, call, func() { p.markDead(gen, ErrPoolDead) })
	connectErr := connectCtx.Err()
	stopParentCancel()
	cancelConnect()
	p.mu.Lock()
	if err != nil {
		if p.generation == gen {
			p.connecting = false
		}
		p.finishConnectLocked(gen, connectDone)
		p.mu.Unlock()
		err = connectionFailure(err)
		p.markDead(gen, err)
		return nil, nil, 0, err
	}
	conn := &poolConnection{room: room}
	if p.closed || p.dead || p.generation != gen || roomCtx.Err() != nil || connectErr != nil {
		p.connecting = false
		p.mu.Unlock()
		conn.disconnect()
		p.mu.Lock()
		p.finishConnectLocked(gen, connectDone)
		p.mu.Unlock()
		if connectErr != nil {
			if lifecycleErr := p.lifecycleError(connectErr); !errors.Is(lifecycleErr, connectErr) {
				return nil, nil, 0, lifecycleErr
			}
			p.markDead(gen, connectErr)
			return nil, nil, 0, connectErr
		}
		return nil, nil, 0, p.currentError()
	}
	p.connection = conn
	p.connecting = false
	p.finishConnectLocked(gen, connectDone)
	report := p.rtt
	p.mu.Unlock()
	if report != nil {
		go p.reportRTT(roomCtx, call.CallID, room, report)
	}
	go func() {
		<-roomCtx.Done()
		p.markDead(gen, roomCtx.Err())
	}()
	return roomCtx, conn, gen, nil
}

func (p *Pool) finishConnectLocked(generation uint64, done chan struct{}) {
	if p.generation == generation && p.connectDone == done {
		p.connectDone = nil
		close(done)
	}
}

func (p *Pool) finishStarting(generation uint64, done chan struct{}) {
	p.mu.Lock()
	p.finishStartingLocked(generation, done)
	p.mu.Unlock()
}

func (p *Pool) finishStartingLocked(generation uint64, done chan struct{}) {
	if p.generation == generation && p.startDone == done {
		p.startDone = nil
		p.connecting = false
		close(done)
	}
}

func (p *Pool) markDead(generation uint64, cause error) {
	p.mu.Lock()
	if p.generation != generation || p.closed || p.dead {
		p.mu.Unlock()
		return
	}
	if errors.Is(cause, ErrPoolDead) && p.connection != nil {
		if room, ok := p.connection.room.(interface {
			DisconnectReason() lkproto.DisconnectReason
		}); ok {
			switch room.DisconnectReason() {
			case lkproto.DisconnectReason_ROOM_DELETED, lkproto.DisconnectReason_ROOM_CLOSED:
				cause = ErrPoolCallEnded
			}
		}
	}
	conn, cancel, report := p.markDeadLocked(cause)
	ctx := p.roomCtx
	p.mu.Unlock()
	logRoomEnded(ctx, cause)
	if report != nil {
		report()
	}
	teardown(conn, cancel)
}

// markDeadLocked marks the pool dead and hands back what the caller must tear down once it has
// released p.mu.
func (p *Pool) markDeadLocked(cause error) (*poolConnection, context.CancelFunc, func()) {
	p.dead = true
	p.deadReason = cause
	var report func()
	if errors.Is(cause, ErrPoolIdle) || errors.Is(cause, ErrPoolCallEnded) {
		report = p.roomEnded
	}
	return p.connection, p.roomCancel, report
}

func teardown(conn *poolConnection, cancel context.CancelFunc) {
	if cancel != nil {
		cancel()
	}
	if conn != nil {
		conn.disconnect()
	}
}

func (p *Pool) currentError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrPoolClosed
	}
	return ErrPoolDead
}

func (p *Pool) lifecycleError(fallback error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrPoolClosed
	}
	if p.dead {
		return ErrPoolDead
	}
	return fallback
}

// ErrPoolIdle is the reason a room was left because nothing played for the idle-leave time.
var ErrPoolIdle = errors.New("music room left after idle timeout")

// LeaveIdle disconnects a retained room that has no active or starting track (Task 10c: the
// Music page's "Leave voice when idle"). The next explicit Start connects a fresh room; automatic
// EOF advancement cannot, exactly as after any other disconnect. A busy, connecting or already
// empty pool is left alone.
func (p *Pool) LeaveIdle() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrPoolClosed
	}
	if p.dead || p.connection == nil || p.active != nil || p.connecting || p.connectDone != nil || p.startDone != nil {
		p.mu.Unlock()
		return nil
	}
	// Fix round 1 (I4): the idle check and the dead mark share one critical section, so a Start
	// that begins after the check finds the pool dead and waits for Reset rather than racing a
	// disconnect of the room it just reused. The disconnect itself runs outside the lock.
	conn, cancel, report := p.markDeadLocked(ErrPoolIdle)
	ctx := p.roomCtx
	p.mu.Unlock()
	logRoomEnded(ctx, ErrPoolIdle)
	if report != nil {
		report()
	}
	teardown(conn, cancel)
	return p.Reset()
}

// Reset is deliberately a no-op while the room is healthy. A dead or expired
// room is fully torn down before the next explicit Start may connect again.
func (p *Pool) Reset() error {
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return ErrPoolClosed
		}
		if !p.dead {
			p.mu.Unlock()
			return nil
		}
		if pending := p.connectDone; pending != nil {
			p.mu.Unlock()
			<-pending
			continue
		}
		if pending := p.startDone; pending != nil {
			p.mu.Unlock()
			<-pending
			continue
		}
		gen, active, conn, cancel := p.generation, p.active, p.connection, p.roomCancel
		p.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if active != nil {
			_ = active.Close()
		}
		if conn != nil {
			conn.disconnect()
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return ErrPoolClosed
		}
		if p.generation == gen && p.dead && p.connectDone == nil {
			p.connection, p.roomCtx, p.roomCancel, p.bound = nil, nil, nil, nil
			p.active, p.connecting, p.dead, p.deadReason = nil, false, false, nil
			// Invalidate the old room's deadline watcher and disconnect callback.
			p.generation++
		}
		p.mu.Unlock()
	}
}

// Close permanently closes the pool and waits for any active track cleanup.
func (p *Pool) Close() error {
	p.mu.Lock()
	if p.closed {
		done := p.closeDone
		p.mu.Unlock()
		if done != nil {
			<-done
		}
		return nil
	}
	p.closed, p.dead = true, true
	active, cancel, connectDone, startDone := p.active, p.roomCancel, p.connectDone, p.startDone
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if active != nil {
		_ = active.Close()
	}
	if connectDone != nil {
		<-connectDone
	}
	if startDone != nil {
		<-startDone
	}
	p.mu.Lock()
	active, conn := p.active, p.connection
	p.mu.Unlock()
	if active != nil {
		_ = active.Close()
	}
	if conn != nil {
		conn.disconnect()
	}
	close(p.closeDone)
	return nil
}

func (p *Pool) releaseTrack(s *Session) {
	p.mu.Lock()
	if p.active == s && p.generation == s.generation {
		p.active = nil
	}
	p.mu.Unlock()
}

func (s *Session) monitorPooled(ctx context.Context, groupID id.ID, epochs EpochSource, initial native.CallMediaEpoch) {
	defer close(s.done)
	defer s.pool.releaseTrack(s)
	ticker := time.NewTicker(epochPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.finish(s.pool.deadCause(s.generation, ctx.Err()))
			return
		case <-s.source.Done():
			s.finish(s.source.Wait())
			return
		case <-ticker.C:
			if err := revalidateEpoch(ctx, groupID, epochs, initial); err != nil {
				s.pool.markDead(s.generation, err)
				s.finish(err)
				return
			}
		}
	}
}

func (p *Pool) deadCause(generation uint64, fallback error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.generation == generation && p.deadReason != nil {
		return p.deadReason
	}
	return fallback
}
