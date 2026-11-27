// Package runtime connects a closable Ogg Opus source to the bounded live Opus
// provider consumed by media.PublishLiveOpus.
package runtime

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/music/encode"
	"github.com/dilla-chat/dilla/internal/media"

	"github.com/dilla-chat/dilla/internal/bot/music"
)

var ErrInvalidConfig = errors.New("music runtime: invalid configuration")

// Options configures the Ogg Opus forwarder.
type Options = encode.Options

// Session owns one encoder and its source. Provider can be passed directly to
// media.PublishLiveOpus. InputDone closes when encoding ends; Done closes once the
// provider has also handed out its last queued sample, or the session is closed.
type Session struct {
	provider  *media.LiveOpusProvider
	cancel    context.CancelFunc
	inputDone chan struct{}
	done      chan struct{}
	mu        sync.RWMutex
	err       error
	source    *gatedPCM
}

// drainSlack bounds a drain beyond the queue's own length: a publisher that stops taking samples without ending the
// track (no error, no cancel) must not hold the track open forever. A paused provider does not count.
const drainSlack = 5 * time.Second

// StartOgg creates a bounded Opus provider and forwards a finished 48 kHz stereo Ogg Opus stream, the output
// of the sandboxed stream worker's single ffmpeg, in the background; the session owns and closes the source. No second encoder runs: the
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
	s := &Session{provider: provider, cancel: cancel, inputDone: make(chan struct{}), done: make(chan struct{})}
	s.source = &gatedPCM{ReadCloser: source, provider: provider, ctx: runCtx}
	limit := time.Duration(capacity)*media.LiveOpusSampleDuration + drainSlack
	go func() {
		defer close(s.done)
		err := encode.BridgeOgg(runCtx, s.source, provider, options)
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		close(s.inputDone)
		s.waitDrained(runCtx, limit)
	}()
	return s, nil
}

// waitDrained returns once the provider handed out its last queued sample, the session is closed, or the drain
// outlasted limit while not paused.
func (s *Session) waitDrained(ctx context.Context, limit time.Duration) {
	timer := time.NewTimer(limit)
	defer timer.Stop()
	for {
		select {
		case <-s.provider.Drained():
			return
		case <-ctx.Done():
			return
		case <-timer.C:
			if !s.provider.Paused() {
				return
			}
			timer.Reset(limit)
		}
	}
}

// Provider returns the bounded sample source accepted by media.PublishLiveOpus.
func (s *Session) Provider() *media.LiveOpusProvider { return s.provider }

// Done closes once the encoder exited and the provider handed out its last queued sample (the track played to its
// end), or once the session is closed.
func (s *Session) Done() <-chan struct{} { return s.done }

// InputDone closes when the encoder exits: the input ended, but up to the queue's length of audio may still be
// waiting to be sent.
func (s *Session) InputDone() <-chan struct{} { return s.inputDone }

// InputErr waits for InputDone and returns the encoder's terminal error.
func (s *Session) InputErr() error {
	<-s.inputDone
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.err
}

// Drained is the provider's Drained.
func (s *Session) Drained() <-chan struct{} { return s.provider.Drained() }

// HoldFirstSample is the provider's HoldFirstSample.
func (s *Session) HoldFirstSample(gate <-chan struct{}) { s.provider.HoldFirstSample(gate) }

// Wait waits for Done and returns the encoder's terminal error. It may be
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
	case <-s.inputDone:
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
