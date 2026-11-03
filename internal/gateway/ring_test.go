package gateway

import (
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/clock"
)

func TestTheRingIsBoundedOnCountBytesAndTTL(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	r := newRing(ringLimits{Frames: 3, Bytes: 120, TTL: 60 * time.Second}, clk)

	for n := uint64(1); n <= 4; n++ {
		r.add(n, make([]byte, 20), FrameScope{})
	}
	if got := len(r.since(0)); got != 3 {
		t.Fatalf("frames after the count bound: %d, want 3", got)
	}
	if r.floor() != 2 {
		t.Fatalf("floor = %d, want 2 (n=1 was evicted)", r.floor())
	}

	r2 := newRing(ringLimits{Frames: 100, Bytes: 100, TTL: 60 * time.Second}, clk)
	r2.add(1, make([]byte, 60), FrameScope{})
	r2.add(2, make([]byte, 60), FrameScope{})
	if got := len(r2.since(0)); got != 1 {
		t.Fatalf("frames after the byte bound: %d, want 1", got)
	}

	r3 := newRing(ringLimits{Frames: 100, Bytes: 1 << 20, TTL: 30 * time.Second}, clk)
	r3.add(1, make([]byte, 8), FrameScope{})
	clk.Advance(31 * time.Second)
	r3.add(2, make([]byte, 8), FrameScope{})
	if got := len(r3.since(0)); got != 1 {
		t.Fatalf("frames after the TTL bound: %d, want 1", got)
	}
}

func TestAHeartbeatsLastNTrimsTheRing(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	r := newRing(ringLimits{Frames: 16, Bytes: 1 << 20, TTL: time.Minute}, clk)
	for n := uint64(1); n <= 8; n++ {
		r.add(n, []byte{byte(n)}, FrameScope{})
	}
	r.trim(5)
	if got := r.floor(); got != 6 {
		t.Fatalf("floor after trim(5) = %d, want 6", got)
	}
	if got := len(r.since(5)); got != 3 {
		t.Fatalf("since(5) = %d frames, want 3", got)
	}
	if _, ok := r.replayable(4); ok {
		t.Fatal("a trimmed n must not be replayable")
	}
}
