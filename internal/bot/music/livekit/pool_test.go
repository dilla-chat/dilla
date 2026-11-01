package livekit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/music/counter"
	"github.com/jonasthim/dilla/internal/bot/music/runtime"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/sframe"
)

func TestPoolRetainsRoomAcrossTrackSessionsAndUnpublishesEachTrack(t *testing.T) {
	p, err := NewPool(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	backend := &poolBackendStub{}
	p.backend = backend
	epochs := newPoolEpochs()
	call, group := poolCall(1, 2)

	firstSource := newPoolSource()
	first, err := p.startSource(context.Background(), call, group, epochs, firstSource)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); !errors.Is(err, context.Canceled) {
		t.Fatalf("first.Close() = %v, want context.Canceled", err)
	}
	if firstSource.closeCount != 1 || backend.unpublishes != 1 {
		t.Fatalf("first track cleanup: source closes=%d unpublishes=%d, want 1 each", firstSource.closeCount, backend.unpublishes)
	}
	if connects, disconnects, _ := backend.counts(); connects != 1 || disconnects != 0 {
		t.Fatalf("after first track: connects=%d disconnects=%d, want 1 and 0", connects, disconnects)
	}
	firstDeadline := poolDeadline(t, p)

	secondSource := newPoolSource()
	second, err := p.startSource(context.Background(), call, group, epochs, secondSource)
	if err != nil {
		t.Fatal(err)
	}
	if connects, _, _ := backend.counts(); connects != 1 {
		t.Fatalf("track restart opened %d rooms, want retained room", connects)
	}
	if deadline := poolDeadline(t, p); !deadline.Equal(firstDeadline) {
		t.Fatalf("track restart renewed room deadline: first=%v second=%v", firstDeadline, deadline)
	}
	if second.sender == first.sender {
		t.Fatal("successive tracks reused an SFrame sender")
	}
	firstFrame, err := first.sender.Encrypt(sframe.Opus, sframe.Mic, 0, []byte{0xfc, 1, 2})
	if err != nil {
		t.Fatal(err)
	}
	secondFrame, err := second.sender.Encrypt(sframe.Opus, sframe.Mic, 0, []byte{0xfc, 1, 2})
	if err != nil {
		t.Fatal(err)
	}
	_, firstCounter, _, err := sframe.DecodeHeader(firstFrame)
	if err != nil {
		t.Fatal(err)
	}
	_, secondCounter, _, err := sframe.DecodeHeader(secondFrame)
	if err != nil {
		t.Fatal(err)
	}
	if firstCounter != 0 || secondCounter != counter.BlockSize {
		t.Fatalf("pooled publisher counters = %d then %d, want 0 then %d", firstCounter, secondCounter, counter.BlockSize)
	}
	_ = second.Close()
	if _, _, unpublishes := backend.counts(); unpublishes != 2 || secondSource.closeCount != 1 {
		t.Fatalf("second track cleanup: source closes=%d unpublishes=%d, want 1 each", secondSource.closeCount, unpublishes)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, disconnects, _ := backend.counts(); disconnects != 1 {
		t.Fatalf("pool close disconnected room %d times, want once", disconnects)
	}
}

func TestPoolRejectsInvalidLifetimeAndTypedNilSource(t *testing.T) {
	for _, lifetime := range []time.Duration{0, -time.Second, maxPublisherLifetime + time.Nanosecond} {
		if _, err := NewPool(lifetime); !errors.Is(err, ErrBinding) {
			t.Fatalf("NewPool(%v) = %v, want ErrBinding", lifetime, err)
		}
	}
	p, err := NewPool(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	epochs := &epochStub{}
	call, group := poolCall(22, 23)
	var source *runtime.Session
	if _, err := p.Start(context.Background(), call, group, epochs, source); !errors.Is(err, ErrBinding) {
		t.Fatalf("Start with typed nil source = %v, want ErrBinding", err)
	}
	if epochs.called {
		t.Fatal("typed nil source reached key lookup")
	}
	_ = p.Close()
}

func TestPoolAbsoluteLifetimeExpiresWhileIdleAndRequiresReset(t *testing.T) {
	p, err := NewPool(40 * time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	backend := &poolBackendStub{}
	p.backend = backend
	epochs := newPoolEpochs()
	call, group := poolCall(3, 4)
	s, err := p.startSource(context.Background(), call, group, epochs, newPoolSource())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	waitFor(t, func() bool { _, disconnects, _ := backend.counts(); return disconnects == 1 })
	if _, err := p.startSource(context.Background(), call, group, epochs, newPoolSource()); !errors.Is(err, ErrPoolDead) {
		t.Fatalf("Start after idle expiry = %v, want ErrPoolDead", err)
	}
	if connects, _, _ := backend.counts(); connects != 1 {
		t.Fatalf("dead room auto-reconnected %d times, want no reconnect", connects)
	}
	if err := p.Reset(); err != nil {
		t.Fatalf("Reset after expiry: %v", err)
	}
	s2, err := p.startSource(context.Background(), call, group, epochs, newPoolSource())
	if err != nil {
		t.Fatalf("Start after explicit Reset: %v", err)
	}
	_ = s2.Close()
	if connects, _, _ := backend.counts(); connects != 2 {
		t.Fatalf("connects after explicit reset = %d, want 2", connects)
	}
	_ = p.Close()
}

func TestPoolDisconnectRacingTrackCloseLatchesDeadUntilReset(t *testing.T) {
	p, err := NewPool(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	backend := &poolBackendStub{}
	p.backend = backend
	epochs := newPoolEpochs()
	call, group := poolCall(5, 6)
	s, err := p.startSource(context.Background(), call, group, epochs, newPoolSource())
	if err != nil {
		t.Fatal(err)
	}
	backend.disconnectRoom(0)
	_ = s.Close() // EOF/close racing the room callback must not clear the dead latch.
	if _, err := p.startSource(context.Background(), call, group, epochs, newPoolSource()); !errors.Is(err, ErrPoolDead) {
		t.Fatalf("Start after disconnect = %v, want ErrPoolDead", err)
	}
	if connects, _, _ := backend.counts(); connects != 1 {
		t.Fatalf("disconnect caused %d connections, want no automatic redial", connects)
	}
	if err := p.Reset(); err != nil {
		t.Fatal(err)
	}
	s2, err := p.startSource(context.Background(), call, group, epochs, newPoolSource())
	if err != nil {
		t.Fatal(err)
	}
	backend.disconnectRoom(0) // a duplicate late callback from the reset room is stale.
	_ = s2.Close()
	if connects, _, _ := backend.counts(); connects != 2 {
		t.Fatalf("explicit reset connections = %d, want 2", connects)
	}
	s3, err := p.startSource(context.Background(), call, group, epochs, newPoolSource())
	if err != nil {
		t.Fatalf("stale callback poisoned replacement room: %v", err)
	}
	_ = s3.Close()
	_ = p.Close()
}

func TestPoolResetDoesNotRestartLiveRoomAndCloseIsPermanent(t *testing.T) {
	p, err := NewPool(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	backend := &poolBackendStub{}
	p.backend = backend
	epochs := newPoolEpochs()
	call, group := poolCall(7, 8)
	s, err := p.startSource(context.Background(), call, group, epochs, newPoolSource())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Reset(); err != nil {
		t.Fatal(err)
	}
	if connects, disconnects, _ := backend.counts(); connects != 1 || disconnects != 0 {
		t.Fatalf("Reset changed live room: connects=%d disconnects=%d", connects, disconnects)
	}
	_ = s.Close()
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Reset(); !errors.Is(err, ErrPoolClosed) {
		t.Fatalf("Reset after Close = %v, want ErrPoolClosed", err)
	}
	if _, err := p.startSource(context.Background(), call, group, epochs, newPoolSource()); !errors.Is(err, ErrPoolClosed) {
		t.Fatalf("Start after Close = %v, want ErrPoolClosed", err)
	}
	if _, disconnects, _ := backend.counts(); disconnects != 1 {
		t.Fatalf("Close disconnected room %d times, want once", disconnects)
	}
}

func TestPoolBindingChangesFailClosed(t *testing.T) {
	p, err := NewPool(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	backend := &poolBackendStub{}
	p.backend = backend
	epochs := newPoolEpochs()
	call, group := poolCall(9, 10)
	s, err := p.startSource(context.Background(), call, group, epochs, newPoolSource())
	if err != nil {
		t.Fatal(err)
	}
	changed := call
	changed.CallID = testID(11)
	if _, err := p.startSource(context.Background(), changed, group, epochs, newPoolSource()); !errors.Is(err, ErrBinding) {
		t.Fatalf("Start with changed call = %v, want ErrBinding", err)
	}
	if _, disconnects, _ := backend.counts(); disconnects != 1 {
		t.Fatalf("binding violation left room open: disconnects=%d", disconnects)
	}
	if err := p.Reset(); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	_ = p.Close()
}

func TestPoolSerializesTrackPublications(t *testing.T) {
	p, err := NewPool(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	backend := &poolBackendStub{publishGate: make(chan struct{}), publishEntered: make(chan struct{}, 1)}
	p.backend = backend
	epochs := newPoolEpochs()
	call, group := poolCall(12, 13)
	firstResult := make(chan *Session, 1)
	firstErr := make(chan error, 1)
	go func() {
		s, err := p.startSource(context.Background(), call, group, epochs, newPoolSource())
		firstResult <- s
		firstErr <- err
	}()
	select {
	case <-backend.publishEntered:
	case <-time.After(time.Second):
		t.Fatal("first publication did not reach backend")
	}
	if _, err := p.startSource(context.Background(), call, group, epochs, newPoolSource()); !errors.Is(err, ErrPoolBusy) {
		t.Fatalf("overlapping Start = %v, want ErrPoolBusy", err)
	}
	close(backend.publishGate)
	first := <-firstResult
	if err := <-firstErr; err != nil {
		t.Fatal(err)
	}
	if publishes := backend.publishCount(); publishes != 1 {
		t.Fatalf("concurrent starts published %d tracks, want one", publishes)
	}
	_ = first.Close()
	_ = p.Close()
}

func TestPoolUnpublishFailureTerminatesRoom(t *testing.T) {
	p, err := NewPool(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	backend := &poolBackendStub{unpublishErr: errors.New("test failure")}
	p.backend = backend
	epochs := newPoolEpochs()
	call, group := poolCall(14, 15)
	s, err := p.startSource(context.Background(), call, group, epochs, newPoolSource())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, disconnects, _ := backend.counts(); disconnects != 1 {
		t.Fatalf("failed unpublish left room connected: disconnects=%d", disconnects)
	}
	if _, err := p.startSource(context.Background(), call, group, epochs, newPoolSource()); !errors.Is(err, ErrPoolDead) {
		t.Fatalf("Start after failed unpublish = %v, want ErrPoolDead", err)
	}
	_ = p.Close()
}

func TestPoolDisconnectCallbackDuringUnpublishDoesNotDeadlock(t *testing.T) {
	p, err := NewPool(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	backend := &poolBackendStub{}
	p.backend = backend
	call, group := poolCall(24, 25)
	s, err := p.startSource(context.Background(), call, group, newPoolEpochs(), newPoolSource())
	if err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	backend.onUnpublish = func() { backend.disconnectRoom(0) }
	backend.mu.Unlock()
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("synchronous disconnect callback during unpublish deadlocked Close")
	}
	if _, err := p.startSource(context.Background(), call, group, newPoolEpochs(), newPoolSource()); !errors.Is(err, ErrPoolDead) {
		t.Fatalf("Start after callback = %v, want ErrPoolDead", err)
	}
	_ = p.Close()
}

func TestPoolCloseWaitsForConnectAndDisconnectsLateRoom(t *testing.T) {
	p, err := NewPool(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	backend := &poolBackendStub{connectGate: make(chan struct{}), connectEntered: make(chan struct{}, 1)}
	p.backend = backend
	call, group := poolCall(16, 17)
	startDone := make(chan error, 1)
	go func() {
		_, err := p.startSource(context.Background(), call, group, newPoolEpochs(), newPoolSource())
		startDone <- err
	}()
	select {
	case <-backend.connectEntered:
	case <-time.After(time.Second):
		t.Fatal("connect did not reach backend")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- p.Close() }()
	waitFor(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.closed })
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned before in-flight connect cleanup: %v", err)
	default:
	}
	close(backend.connectGate)
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if err := <-startDone; !errors.Is(err, ErrPoolClosed) {
		t.Fatalf("Start during Close = %v, want ErrPoolClosed", err)
	}
	if connects, disconnects, _ := backend.counts(); connects != 1 || disconnects != 1 {
		t.Fatalf("late connection cleanup: connects=%d disconnects=%d, want 1 each", connects, disconnects)
	}
}

func TestPoolCloseWaitsForPublicationCleanup(t *testing.T) {
	p, err := NewPool(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	backend := &poolBackendStub{publishGate: make(chan struct{}), publishEntered: make(chan struct{}, 1)}
	p.backend = backend
	call, group := poolCall(18, 19)
	startDone := make(chan error, 1)
	go func() {
		_, err := p.startSource(context.Background(), call, group, newPoolEpochs(), newPoolSource())
		startDone <- err
	}()
	select {
	case <-backend.publishEntered:
	case <-time.After(time.Second):
		t.Fatal("publish did not reach backend")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- p.Close() }()
	waitFor(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.closed })
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned before in-flight publication cleanup: %v", err)
	default:
	}
	close(backend.publishGate)
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if err := <-startDone; !errors.Is(err, ErrPoolClosed) {
		t.Fatalf("Start during Close = %v, want ErrPoolClosed", err)
	}
	if connects, disconnects, unpublishes := backend.counts(); connects != 1 || disconnects != 1 || unpublishes != 1 {
		t.Fatalf("publication teardown: connects=%d disconnects=%d unpublishes=%d, want 1 each", connects, disconnects, unpublishes)
	}
}

func TestPoolResetWaitsForInFlightPublicationCleanup(t *testing.T) {
	p, err := NewPool(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	backend := &poolBackendStub{publishGate: make(chan struct{}), publishEntered: make(chan struct{}, 1)}
	p.backend = backend
	call, group := poolCall(20, 21)
	startDone := make(chan error, 1)
	go func() {
		_, err := p.startSource(context.Background(), call, group, newPoolEpochs(), newPoolSource())
		startDone <- err
	}()
	select {
	case <-backend.publishEntered:
	case <-time.After(time.Second):
		t.Fatal("publish did not reach backend")
	}
	backend.disconnectRoom(0)
	resetDone := make(chan error, 1)
	go func() { resetDone <- p.Reset() }()
	waitFor(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.dead })
	select {
	case err := <-resetDone:
		t.Fatalf("Reset returned before in-flight publication cleanup: %v", err)
	default:
	}
	close(backend.publishGate)
	if err := <-startDone; !errors.Is(err, ErrPoolDead) {
		t.Fatalf("Start interrupted by room disconnect = %v, want ErrPoolDead", err)
	}
	if err := <-resetDone; err != nil {
		t.Fatal(err)
	}
	s, err := p.startSource(context.Background(), call, group, newPoolEpochs(), newPoolSource())
	if err != nil {
		t.Fatalf("Start after Reset cleanup: %v", err)
	}
	_ = s.Close()
	_ = p.Close()
	if connects, disconnects, unpublishes := backend.counts(); connects != 2 || disconnects != 2 || unpublishes != 2 {
		t.Fatalf("reset cleanup counts: connects=%d disconnects=%d unpublishes=%d", connects, disconnects, unpublishes)
	}
}

type poolBackendStub struct {
	mu             sync.Mutex
	connects       int
	disconnects    int
	unpublishes    int
	publishes      int
	unpublishErr   error
	publishGate    chan struct{}
	publishEntered chan struct{}
	connectGate    chan struct{}
	connectEntered chan struct{}
	onUnpublish    func()
	connections    []*poolRoomStub
}

func (b *poolBackendStub) connect(_ context.Context, _ client.Call, disconnected func()) (poolRoom, error) {
	b.mu.Lock()
	b.connects++
	room := &poolRoomStub{disconnected: disconnected, backend: b}
	b.connections = append(b.connections, room)
	gate, entered := b.connectGate, b.connectEntered
	b.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
	}
	if gate != nil {
		<-gate
	}
	return room, nil
}

func (b *poolBackendStub) publish(_ poolRoom, _ sourceLifecycle, _ *sframe.Sender) (func() error, error) {
	b.mu.Lock()
	b.publishes++
	gate, entered, unpublishErr := b.publishGate, b.publishEntered, b.unpublishErr
	b.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
	}
	if gate != nil {
		<-gate
	}
	return func() error {
		b.mu.Lock()
		b.unpublishes++
		callback := b.onUnpublish
		b.mu.Unlock()
		if callback != nil {
			callback()
		}
		return unpublishErr
	}, nil
}

func (b *poolBackendStub) disconnectRoom(i int) {
	b.mu.Lock()
	callback := b.connections[i].disconnected
	b.mu.Unlock()
	callback()
}

func (b *poolBackendStub) counts() (connects, disconnects, unpublishes int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.connects, b.disconnects, b.unpublishes
}

func (b *poolBackendStub) publishCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.publishes
}

type poolRoomStub struct {
	disconnected func()
	backend      *poolBackendStub
	once         sync.Once
}

func (r *poolRoomStub) Disconnect() {
	r.once.Do(func() {
		r.backend.mu.Lock()
		r.backend.disconnects++
		r.backend.mu.Unlock()
	})
}

type poolSourceStub struct {
	done       chan struct{}
	once       sync.Once
	mu         sync.Mutex
	closeCount int
}

func newPoolSource() *poolSourceStub            { return &poolSourceStub{done: make(chan struct{})} }
func (s *poolSourceStub) Done() <-chan struct{} { return s.done }
func (s *poolSourceStub) Wait() error           { return nil }
func (s *poolSourceStub) Close() error {
	s.mu.Lock()
	s.closeCount++
	s.mu.Unlock()
	s.once.Do(func() { close(s.done) })
	return nil
}

type poolEpochs struct {
	epochStub
	state *counterStateStub
}

func newPoolEpochs() *poolEpochs {
	return &poolEpochs{
		epochStub: epochStub{epoch: native.CallMediaEpoch{Epoch: 1, OwnLeaf: 1, BaseKey: [16]byte{1}, Roster: []native.CallMediaMember{{Leaf: 1}}}},
		state:     &counterStateStub{},
	}
}
func (e *poolEpochs) StateGet(ctx context.Context, version uint64, key []byte) (native.StateValue, error) {
	return e.state.StateGet(ctx, version, key)
}
func (e *poolEpochs) StateTransaction(ctx context.Context, tx id.ID, changes []native.StateChange) ([]uint64, error) {
	return e.state.StateTransaction(ctx, tx, changes)
}

func poolCall(callID, groupID byte) (client.Call, id.ID) {
	group := testID(groupID)
	return client.Call{CallID: testID(callID), GroupID: group, LiveKitURL: "wss://livekit.example", Token: "test-token"}, group
}

func waitFor(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become ready before timeout")
}

func poolDeadline(t *testing.T, p *Pool) time.Time {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	deadline, ok := p.roomCtx.Deadline()
	if !ok {
		t.Fatal("room context has no deadline")
	}
	return deadline
}

var _ counter.State = (*poolEpochs)(nil)

func TestPoolReplacementBindingFencesDelayedOldRoomCallback(t *testing.T) {
	p, err := NewPool(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	backend := &poolBackendStub{}
	p.backend = backend
	epochs := newPoolEpochs()
	call, group := poolCall(81, 82)
	first, err := p.startSource(context.Background(), call, group, epochs, newPoolSource())
	if err != nil {
		t.Fatal(err)
	}
	_ = first.Close()
	if err := p.Reset(); err != nil {
		t.Fatal(err)
	} // old room still appears healthy.
	replacement := call
	replacement.GroupID = testID(83)
	if _, err := p.startSource(context.Background(), replacement, replacement.GroupID, epochs, newPoolSource()); !errors.Is(err, ErrBinding) {
		t.Fatalf("replacement without explicit reset = %v", err)
	}
	if err := p.Reset(); err != nil {
		t.Fatal(err)
	}
	second, err := p.startSource(context.Background(), replacement, replacement.GroupID, epochs, newPoolSource())
	if err != nil {
		t.Fatal(err)
	}
	backend.disconnectRoom(0) // delayed callback belongs to the retired binding.
	_ = second.Close()
	third, err := p.startSource(context.Background(), replacement, replacement.GroupID, epochs, newPoolSource())
	if err != nil {
		t.Fatalf("old callback poisoned replacement: %v", err)
	}
	_ = third.Close()
	if connects, _, _ := backend.counts(); connects != 2 {
		t.Fatalf("connections=%d, want one per binding", connects)
	}
}

// Task 8c fix round 2 (Ruling AU): a connected room reports its RTT for the stage, rounded, only when it moved by RTTStep, and
// stops with the room.
func TestPoolReportsTheRoomRTTOnChange(t *testing.T) {
	p, err := NewPool(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	p.backend = &poolBackendStub{}
	var mu sync.Mutex
	rtts := []time.Duration{63 * time.Millisecond, 66 * time.Millisecond, 70400 * time.Microsecond}
	read := 0
	p.roomRTT = func(poolRoom) (time.Duration, bool) {
		mu.Lock()
		defer mu.Unlock()
		v := rtts[min(read, len(rtts)-1)]
		read++
		return v, true
	}
	p.rttEvery = 5 * time.Millisecond
	got := make(chan uint32, 8)
	var gotCall id.ID
	p.SetRTTReporter(func(_ context.Context, callID id.ID, ms uint32) error {
		mu.Lock()
		gotCall = callID
		mu.Unlock()
		got <- ms
		return nil
	})
	call, group := poolCall(1, 2)
	s, err := p.startSource(context.Background(), call, group, newPoolEpochs(), newPoolSource())
	if err != nil {
		t.Fatal(err)
	}
	want := []uint32{63, 70} // 66 is within RTTStep of 63
	for _, w := range want {
		select {
		case ms := <-got:
			if ms != w {
				t.Fatalf("reported %d, want %d", ms, w)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no report of %d", w)
		}
	}
	mu.Lock()
	if gotCall != call.CallID {
		t.Errorf("reported for call %v, want %v", gotCall, call.CallID)
	}
	mu.Unlock()
	_ = s.Close()
	_ = p.Close()
	select {
	case ms := <-got:
		t.Fatalf("a report after the room closed: %d", ms)
	case <-time.After(50 * time.Millisecond):
	}
}
