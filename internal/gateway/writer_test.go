package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
)

// recordingSink is the transport double. Its stall gate is a mutex-guarded bool plus a
// sync.Cond, never a channel field that is reassigned: the writer goroutine reads the gate on
// every write while the test goroutine flips it, and `go test -race -count=4` is a gate this
// package must pass.
type recordingSink struct {
	frames chan []byte
	closed chan CloseCode
	reads  chan []byte

	mu      sync.Mutex
	cond    *sync.Cond
	stalled bool
	shut    bool
}

func newRecordingSink(depth int, stalled bool) *recordingSink {
	s := &recordingSink{
		frames:  make(chan []byte, depth),
		closed:  make(chan CloseCode, 1),
		reads:   make(chan []byte, 16),
		stalled: stalled,
	}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// reads is what the test hands the read loop. A closed sink returns io.EOF, which ends readLoop.
func (s *recordingSink) read(ctx context.Context) (websocket.MessageType, []byte, error) {
	select {
	case b, ok := <-s.reads:
		if !ok {
			return 0, nil, io.EOF
		}
		return websocket.MessageBinary, b, nil
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}
}

// feed queues one inbound frame for the read loop.
func (s *recordingSink) feed(b []byte) { s.reads <- b }

func (s *recordingSink) write(_ context.Context, b []byte) error {
	s.mu.Lock()
	for s.stalled && !s.shut {
		s.cond.Wait()
	}
	shut := s.shut
	s.mu.Unlock()
	if shut {
		return errors.New("sink closed")
	}
	s.frames <- b
	return nil
}

// stall and release are the only way the gate moves after construction.
func (s *recordingSink) stall() {
	s.mu.Lock()
	s.stalled = true
	s.mu.Unlock()
}

func (s *recordingSink) release() {
	s.mu.Lock()
	s.stalled = false
	s.mu.Unlock()
	s.cond.Broadcast()
}

// close records the FIRST close code and ignores the rest, which is what websocket.Conn.Close does
// (a second Close returns an error and changes nothing on the wire). The code is recorded under the
// lock that flips `shut`, before the broadcast: a writer blocked in write wakes on that broadcast,
// sees `shut`, and closes 4000 "write" — and with the code recorded afterwards, that 4000 could
// win the race against the 4008 whose close woke it. It did, on a loaded CI runner.
func (s *recordingSink) close(code CloseCode, _ string) {
	s.mu.Lock()
	first := !s.shut
	s.shut = true
	if first {
		select {
		case s.closed <- code:
		default:
		}
	}
	s.mu.Unlock()
	// Waking every blocked writer is what stops a stalled test from leaking its goroutine.
	s.cond.Broadcast()
}

// readLoop's deferred suspend and sweepLiveness's 4009 suspend can stop the same writer at the
// same moment (go-harness, run 38384557541: "close of closed channel" in writer.stop). Every
// concurrent stop must be safe; a check-then-close lets two callers both see done open.
func TestConcurrentStopsOfOneWriterDoNotPanic(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	for range 200 {
		sink := newRecordingSink(4, false)
		w := newWriter(sink, writerLimits{Frames: 4, Bytes: 1 << 20, Deadline: time.Second}, clk)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				w.stop()
			}()
		}
		close(start)
		wg.Wait()
		<-w.finished
		sink.release()
	}
}

func TestAnOverflowingWriterClosesFourZeroZeroEight(t *testing.T) {
	sink := newRecordingSink(64, true)
	t.Cleanup(sink.release)
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	w := newWriter(sink, writerLimits{Frames: 2, Bytes: 1 << 20, Deadline: time.Second}, clk)
	t.Cleanup(w.stop)

	payload := cbor.RawMessage{0x80} // []
	for range 8 {
		w.enqueue(Frame{Op: OpPresence, Payload: payload}, 0)
	}
	select {
	case code := <-sink.closed:
		if code != CloseRateLimited {
			t.Fatalf("close code = %d, want 4008", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an overflowing queue must close the connection, not block the producer")
	}
}

// The byte bound is enforced independently of the frame bound: 4 frames of 64 KiB stay well under
// QueueFrames and well over a 128 KiB QueueBytes, and the connection must still close 4008.
func TestAWriterOverTheByteBoundClosesFourZeroZeroEightWhileUnderTheFrameBound(t *testing.T) {
	sink := newRecordingSink(64, true)
	t.Cleanup(sink.release)
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	w := newWriter(sink, writerLimits{Frames: 256, Bytes: 128 << 10, Deadline: time.Second}, clk)
	t.Cleanup(w.stop)

	big, err := cborx.Marshal([]any{make([]byte, 64<<10)})
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	for range 4 {
		w.enqueue(Frame{Op: OpMessageCT, Payload: cbor.RawMessage(big), Replay: true}, 1)
	}
	select {
	case code := <-sink.closed:
		if code != CloseRateLimited {
			t.Fatalf("close code = %d, want 4008", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the byte bound is declared but not enforced: four 64 KiB frames fitted in a 128 KiB queue")
	}
}

// Deliver* must never block a producer: the DS holds no lock while fanning out, and a stalled
// socket must not stall a commit.
func TestEnqueueNeverBlocks(t *testing.T) {
	sink := newRecordingSink(1, false)
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	w := newWriter(sink, writerLimits{Frames: 1, Bytes: 64, Deadline: time.Second}, clk)
	t.Cleanup(w.stop)

	done := make(chan struct{})
	go func() {
		for range 1000 {
			w.enqueue(Frame{Op: OpPresence, Payload: cbor.RawMessage{0x80}}, 0)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("enqueue blocked")
	}
}

// The first heartbeat is jittered into [0, heartbeat_ms) so 1,500 clients reconnecting after a
// restart do not synchronise onto one second (protocol/02, "Liveness"). firstDelay is armed by
// Gateway.register in task 18; this is the distribution test interfaces §10 task 17 names.
func TestTheFirstHeartbeatIsJitteredIntoTheInterval(t *testing.T) {
	p := newHeartbeatPolicy(30 * time.Second)
	buckets := make([]int, 6)
	for range 6000 {
		d := p.firstDelay()
		if d < 0 || d >= p.interval {
			t.Fatalf("firstDelay = %v, want [0, %v)", d, p.interval)
		}
		buckets[int(d/(5*time.Second))]++
	}
	for i, n := range buckets {
		if n == 0 {
			t.Errorf("bucket %d (%ds-%ds) is empty; the first beat is not jittered across the interval",
				i, i*5, (i+1)*5)
		}
	}
}

// syncBuffer is a log destination two goroutines touch: the writer goroutine through the
// slog.Handler, the test goroutine when it reads back what was written.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A malformed mls.commit_needed payload is a delivery-service bug, not a reason to disconnect a
// client that has done nothing wrong: the frame is logged and dropped, the connection stays up and
// the writer keeps serving the queue. Before this test logDropped closed the sink and returned
// true, so the writer went on writing into a socket it had just closed and the connection ended on
// whatever the next failed write reported rather than on the stated reason.
func TestAMalformedCommitNeededIsLoggedAndDroppedWithTheConnectionUp(t *testing.T) {
	sink := newRecordingSink(4, false)
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	var logs syncBuffer
	w := newWriterWithLogger(sink, writerLimits{Frames: 4, Bytes: 1 << 20, Deadline: time.Second},
		clk, slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(w.stop)

	// Three elements, not four: rebaseDeadline refuses it with E_FRAME_SHAPE.
	bad, err := cborx.Marshal([]any{uint64(7), [][]byte{}, uint64(3000)})
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	gid := id.ID{1, 2, 3}
	w.enqueue(Frame{Op: OpMLSCommitNeeded, GroupID: &gid, Payload: cbor.RawMessage(bad)}, 0)

	// The next frame proves the writer is still serving the queue and the sink is still open.
	w.enqueue(Frame{Op: OpPresence, Payload: cbor.RawMessage{0x80}}, 0)
	select {
	case b := <-sink.frames:
		if len(b) == 0 {
			t.Fatal("empty frame")
		}
	case code := <-sink.closed:
		t.Fatalf("one malformed commit_needed payload closed the connection with %d", code)
	case <-time.After(2 * time.Second):
		t.Fatal("the writer stopped after dropping a frame")
	}

	select {
	case code := <-sink.closed:
		t.Fatalf("the connection was closed with %d after a dropped frame", code)
	default:
	}

	line := logs.String()
	if !strings.Contains(line, "E_FRAME_SHAPE") {
		t.Errorf("the drop was not logged with its error: %q", line)
	}
	if !strings.Contains(line, `"op":17`) {
		t.Errorf("the drop was not logged with its opcode: %q", line)
	}
	if !strings.Contains(line, gid.String()) {
		t.Errorf("the drop was not logged with its group: %q", line)
	}
}
