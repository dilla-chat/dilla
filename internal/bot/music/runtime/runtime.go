// Package runtime connects a closable PCM source to the bounded live Opus
// provider consumed by media.PublishLiveOpus.
package runtime

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/music/encode"
	"github.com/dilla-chat/dilla/internal/media"

	"github.com/dilla-chat/dilla/internal/bot/music"
)

var ErrInvalidConfig = errors.New("music runtime: invalid configuration")

// PCMFormat describes the signed 16-bit little-endian PCM emitted by a source.
type PCMFormat struct {
	SampleRate int
	Channels   int
}

// Options configures the existing PCM-to-Opus encoder.
type Options = encode.Options

// Session owns one encoder and its source. Provider can be passed directly to
// media.PublishLiveOpus; Done closes when encoding ends or the session is closed.
type Session struct {
	provider *media.LiveOpusProvider
	cancel   context.CancelFunc
	done     chan struct{}
	mu       sync.RWMutex
	err      error
	source   *gatedPCM
}

// Start creates a bounded Opus provider and starts encoding source in the
// background. The source must implement io.ReadCloser so encoder cancellation
// can interrupt reads. Session owns and closes it after Start succeeds.
func Start(ctx context.Context, source io.ReadCloser, format PCMFormat, capacity int, options Options) (*Session, error) {
	if ctx == nil || source == nil || format.SampleRate < 8000 || format.SampleRate > 192000 ||
		(format.Channels != 1 && format.Channels != 2) {
		return nil, ErrInvalidConfig
	}
	provider, err := media.NewLiveOpusProvider(capacity)
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(client.WithoutCooperativeStop(client.WithBackground(ctx)))
	s := &Session{provider: provider, cancel: cancel, done: make(chan struct{})}
	s.source = &gatedPCM{ReadCloser: source, provider: provider, ctx: runCtx}
	go func() {
		err := encode.BridgeWithOptions(runCtx, s.source, format.SampleRate, format.Channels, provider, options)
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		close(s.done)
	}()
	return s, nil
}

// StartOgg is Start for a finished 48 kHz stereo Ogg Opus stream, the output
// of the sandboxed stream worker's single ffmpeg. No second encoder runs: the
// stream's 20 ms packets are validated and forwarded as they arrive.
func StartOgg(ctx context.Context, source io.ReadCloser, capacity int, options Options) (*Session, error) {
	if ctx == nil || source == nil {
		return nil, ErrInvalidConfig
	}
	provider, err := media.NewLiveOpusProvider(capacity)
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(client.WithoutCooperativeStop(client.WithBackground(ctx)))
	s := &Session{provider: provider, cancel: cancel, done: make(chan struct{})}
	s.source = &gatedPCM{ReadCloser: source, provider: provider, ctx: runCtx}
	go func() {
		err := encode.BridgeOgg(runCtx, s.source, provider, options)
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		close(s.done)
	}()
	return s, nil
}

// Provider returns the bounded sample source accepted by media.PublishLiveOpus.
func (s *Session) Provider() *media.LiveOpusProvider { return s.provider }

// Done closes when the encoder exits.
func (s *Session) Done() <-chan struct{} { return s.done }

// Wait waits for encoder completion and returns its terminal error. It may be
// called repeatedly, including concurrently.
func (s *Session) Wait() error {
	<-s.done
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.err
}

// Close cancels encoding, which closes the owned PCM source and provider input,
// then waits for all bridge goroutines to exit.
func (s *Session) Close() error {
	s.cancel()
	return s.Wait()
}

type gatedPCM struct {
	io.ReadCloser
	provider *media.LiveOpusProvider
	ctx      context.Context
	failed   atomic.Bool
}

func (p *gatedPCM) Read(b []byte) (int, error) {
	if err := p.provider.WaitUnpaused(p.ctx); err != nil {
		return 0, err
	}
	n, err := p.ReadCloser.Read(b)
	if err != nil {
		p.failed.Store(true)
	}
	return n, err
}
func (s *Session) Pause()            { s.provider.Pause() }
func (s *Session) PositionMS() int64 { return s.provider.PositionMS() }
func (s *Session) Resume() error {
	select {
	case <-s.done:
		return music.ErrSourceGone
	default:
	}
	if s.source.failed.Load() {
		return music.ErrSourceGone
	}
	if p, ok := s.source.ReadCloser.(interface{ SourceGone() bool }); ok && p.SourceGone() {
		return music.ErrSourceGone
	}
	return s.provider.Resume()
}

func (s *Session) Paused() bool { return s.provider.Paused() }
