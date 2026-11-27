package livekit

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/music/runtime"
	"github.com/dilla-chat/dilla/internal/sframe"
)

// drainBackend is a fake participant: it records each published source, so the test consumes its samples the way the
// SDK's write loop would, and counts unpublications.
type drainBackend struct {
	*poolBackendStub
	mu      sync.Mutex
	sources []sourceLifecycle
}

func (b *drainBackend) publish(room poolRoom, source sourceLifecycle, sender *sframe.Sender) (func() error, error) {
	b.mu.Lock()
	b.sources = append(b.sources, source)
	b.mu.Unlock()
	return b.poolBackendStub.publish(room, source, sender)
}

func (b *drainBackend) unpublishes() int { _, _, n := b.counts(); return n }

// drainTrack starts a runtime session whose Ogg stream holds packets 20 ms Opus packets, all of which fit the queue,
// and waits for its input to end: the queue then holds the whole track.
func drainTrack(t *testing.T, packets int) *runtime.Session {
	t.Helper()
	session, err := runtime.StartOgg(context.Background(), io.NopCloser(bytes.NewReader(testOggStream(packets))), packets, runtime.Options{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.InputDone():
	case <-time.After(time.Second):
		t.Fatal("the encoder did not reach the end of its input")
	}
	if err := session.InputErr(); err != nil {
		t.Fatalf("input ended with %v, want a clean EOF", err)
	}
	return session
}

func nextSample(t *testing.T, s *runtime.Session) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := s.Provider().NextSample(ctx); err != nil {
		t.Fatalf("NextSample: %v", err)
	}
}

func newDrainPool(t *testing.T) (*Pool, *drainBackend) {
	t.Helper()
	p, err := NewPool(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	backend := &drainBackend{poolBackendStub: &poolBackendStub{}}
	p.backend = backend
	return p, backend
}

func TestPooledSessionUnpublishesOnlyAfterDrain(t *testing.T) {
	p, backend := newDrainPool(t)
	epochs := newPoolEpochs()
	call, group := poolCall(91, 92)
	source := drainTrack(t, 50)
	s, err := p.Start(context.Background(), call, group, epochs, source)
	if err != nil {
		t.Fatal(err)
	}
	for range 49 {
		nextSample(t, source)
	}
	time.Sleep(50 * time.Millisecond)
	if n := backend.unpublishes(); n != 0 {
		t.Fatalf("the track was unpublished %d times with one queued sample left", n)
	}
	select {
	case <-s.Done():
		t.Fatal("the pooled session ended with one queued sample left")
	default:
	}
	nextSample(t, source)
	select {
	case <-s.Done():
	case <-time.After(time.Second):
		t.Fatal("the pooled session did not end once its queue drained")
	}
	if err := s.Wait(); err != nil {
		t.Fatalf("drained session ended with %v, want a clean end", err)
	}
	if n := backend.unpublishes(); n != 1 {
		t.Fatalf("unpublishes after the drain = %d, want 1", n)
	}
}

func TestNextTrackPublishesDuringDrainAndHoldsItsFirstSample(t *testing.T) {
	p, backend := newDrainPool(t)
	epochs := newPoolEpochs()
	call, group := poolCall(93, 94)
	firstSource := drainTrack(t, 5)
	first, err := p.Start(context.Background(), call, group, epochs, firstSource)
	if err != nil {
		t.Fatal(err)
	}
	secondSource := drainTrack(t, 5)
	second, err := p.Start(context.Background(), call, group, epochs, secondSource)
	if err != nil {
		t.Fatalf("the next track could not publish while the first drained: %v", err)
	}
	defer second.Close()
	held := make(chan error, 1)
	go func() { _, err := secondSource.Provider().NextSample(context.Background()); held <- err }()
	for range 4 {
		nextSample(t, firstSource)
	}
	select {
	case err := <-held:
		t.Fatalf("the next track's first sample overlapped the draining track: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	nextSample(t, firstSource)
	select {
	case err := <-held:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("the next track's first sample stayed held after the drain")
	}
	<-first.Done()
	if n := backend.publishCount(); n != 2 {
		t.Fatalf("publications = %d, want 2", n)
	}
}

func TestSkipDuringDrainStopsAtOnce(t *testing.T) {
	p, backend := newDrainPool(t)
	epochs := newPoolEpochs()
	call, group := poolCall(95, 96)
	firstSource := drainTrack(t, 50)
	first, err := p.Start(context.Background(), call, group, epochs, firstSource)
	if err != nil {
		t.Fatal(err)
	}
	nextSample(t, firstSource)
	secondSource := drainTrack(t, 5)
	second, err := p.Start(context.Background(), call, group, epochs, secondSource)
	if err != nil {
		t.Fatalf("the next track could not publish while the first drained: %v", err)
	}
	defer second.Close()
	held := make(chan error, 1)
	go func() { _, err := secondSource.Provider().NextSample(context.Background()); held <- err }()
	// /skip (or /stop) closes the draining track: it is unpublished at once, its tail is dropped, and the next track
	// plays.
	closed := make(chan struct{})
	go func() { _ = first.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("closing the draining track waited for its tail")
	}
	if n := backend.unpublishes(); n != 1 {
		t.Fatalf("unpublishes after the skip = %d, want the draining track's 1", n)
	}
	select {
	case err := <-held:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("the next track stayed held after the draining one was skipped")
	}
	if n := backend.publishCount(); n != 2 {
		t.Fatalf("publications = %d, want 2 (nothing published twice)", n)
	}
	_ = second.Close()
	if n := backend.unpublishes(); n != 2 {
		t.Fatalf("unpublishes after stopping the next track = %d, want 2", n)
	}
}

// testOggStream is a checked Ogg Opus stream (48 kHz stereo) of packets 20 ms CELT packets, one per page.
func testOggStream(packets int) []byte {
	var out bytes.Buffer
	head := []byte{'O', 'p', 'u', 's', 'H', 'e', 'a', 'd', 1, 2, 0, 0, 0x80, 0xbb, 0, 0, 0, 0, 0}
	tags := []byte{'O', 'p', 'u', 's', 'T', 'a', 'g', 's', 0, 0, 0, 0, 0, 0, 0, 0}
	out.Write(testOggPage(0, 0x02, 0, head))
	out.Write(testOggPage(1, 0, 0, tags))
	for i := range packets {
		flags := byte(0)
		if i == packets-1 {
			flags = 0x04
		}
		out.Write(testOggPage(uint32(i+2), flags, uint64(i+1)*960, []byte{0xfc, byte(i), 0xaa}))
	}
	return out.Bytes()
}

func testOggPage(sequence uint32, flags byte, granule uint64, payload []byte) []byte {
	page := make([]byte, 27+1+len(payload))
	copy(page, "OggS")
	page[5] = flags
	binary.LittleEndian.PutUint64(page[6:14], granule)
	binary.LittleEndian.PutUint32(page[14:18], 7)
	binary.LittleEndian.PutUint32(page[18:22], sequence)
	page[26] = 1
	page[27] = byte(len(payload))
	copy(page[28:], payload)
	var crc uint32
	for _, v := range page {
		crc ^= uint32(v) << 24
		for range 8 {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
	}
	binary.LittleEndian.PutUint32(page[22:26], crc)
	return page
}
