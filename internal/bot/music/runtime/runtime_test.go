package runtime

import (
	"context"
	"errors"
	"github.com/jonasthim/dilla/internal/media"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testSource struct {
	closed chan struct{}
	once   sync.Once
}

func (s *testSource) Read([]byte) (int, error) {
	<-s.closed
	return 0, io.ErrClosedPipe
}
func (s *testSource) Close() error { s.once.Do(func() { close(s.closed) }); return nil }

func TestStartExposesBoundedProviderAndClosesOnCancel(t *testing.T) {
	source := &testSource{closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	s, err := Start(ctx, source, PCMFormat{SampleRate: 48000, Channels: 2}, 2, Options{FFmpegPath: "/missing/ffmpeg"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Provider() == nil {
		t.Fatal("Provider() = nil")
	}
	cancel()
	select {
	case <-source.closed:
	case <-time.After(time.Second):
		t.Fatal("source was not closed after cancellation")
	}
	select {
	case <-s.Done():
	case <-time.After(time.Second):
		t.Fatal("session did not finish after cancellation")
	}
	if err := s.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait() = %v, want clean completion or context cancellation", err)
	}
}

func TestStartRejectsInvalidArguments(t *testing.T) {
	if _, err := Start(context.Background(), nil, PCMFormat{SampleRate: 48000, Channels: 2}, 1, Options{}); err == nil {
		t.Fatal("Start(nil source) succeeded")
	}
	if _, err := Start(context.Background(), &testSource{closed: make(chan struct{})}, PCMFormat{SampleRate: 1, Channels: 2}, 1, Options{}); err == nil {
		t.Fatal("Start(invalid format) succeeded")
	}
	if _, err := Start(context.Background(), &testSource{closed: make(chan struct{})}, PCMFormat{SampleRate: 48000, Channels: 2}, 0, Options{}); err == nil {
		t.Fatal("Start(invalid queue size) succeeded")
	}
}

func TestSessionCloseIsSafeWhenRepeated(t *testing.T) {
	source := &testSource{closed: make(chan struct{})}
	s, err := Start(context.Background(), source, PCMFormat{SampleRate: 48000, Channels: 2}, 1, Options{FFmpegPath: "/missing/ffmpeg"})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = s.Close() }()
	}
	wg.Wait()
	if _, err := s.Provider().NextSample(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("NextSample() after close = %v, want EOF", err)
	}
}

func TestPausedRuntimeDoesNotReadPCM(t *testing.T) {
	provider, err := media.NewLiveOpusProvider(1)
	if err != nil {
		t.Fatal(err)
	}
	provider.Pause()
	source := &countedPausePCM{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	gated := &gatedPCM{ReadCloser: source, provider: provider, ctx: ctx}
	entered := make(chan struct{})
	done := make(chan struct{})
	go func() { close(entered); _, _ = gated.Read(make([]byte, 4)); close(done) }()
	<-entered
	cancel()
	<-done
	if source.reads.Load() != 0 {
		t.Fatalf("paused source read %d times", source.reads.Load())
	}
}

type countedPausePCM struct{ reads atomic.Int32 }

func (p *countedPausePCM) Read([]byte) (int, error) { p.reads.Add(1); return 0, io.EOF }
func (*countedPausePCM) Close() error               { return nil }
