package media

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4/pkg/media"
)

const (
	// LiveOpusSampleDuration is the fixed packet duration required by the music transport.
	LiveOpusSampleDuration = 20 * time.Millisecond
	// MaxLiveOpusSampleBytes bounds a single 20ms Opus packet at the codec's packet limit.
	MaxLiveOpusSampleBytes = 1275
	// MaxLiveOpusQueueSamples limits queued audio to two seconds.
	MaxLiveOpusQueueSamples = 100
)

var (
	ErrInvalidLiveOpusSample = errors.New("media: invalid live Opus sample")
	ErrLiveOpusClosed        = errors.New("media: live Opus provider is closed")
	ErrInvalidLiveOpusQueue  = errors.New("media: live Opus queue capacity must be between 1 and 100")
)

// LiveOpusProvider is a bounded bridge from an Opus encoder to LiveKit. Producers call
// WriteSample; the SDK consumes one copied packet at a time. It creates no goroutines of its own.
type LiveOpusProvider struct {
	mu         sync.Mutex
	queue      [][]byte
	head       int
	size       int
	closed     bool
	changed    chan struct{}
	paused     bool
	positionMS int64
}

var _ lksdk.SampleProvider = (*LiveOpusProvider)(nil)

// NewLiveOpusProvider creates a bounded queue. The upstream encoder is responsible for the music
// format (48 kHz stereo, 96 kbps, DTX disabled); each sample here is one nonempty, at-most-1275-byte
// Opus packet with a 20ms duration. A maximum capacity of 100 keeps buffered audio under 2s.
func NewLiveOpusProvider(capacity int) (*LiveOpusProvider, error) {
	if capacity < 1 || capacity > MaxLiveOpusQueueSamples {
		return nil, ErrInvalidLiveOpusQueue
	}
	return &LiveOpusProvider{
		queue:   make([][]byte, capacity),
		changed: make(chan struct{}),
	}, nil
}

// WriteSample blocks when the queue is full, until the SDK consumes a packet, ctx is cancelled,
// or the input is closed. The provider copies data only once capacity is available.
func (p *LiveOpusProvider) WriteSample(ctx context.Context, data []byte) error {
	if len(data) == 0 || len(data) > MaxLiveOpusSampleBytes {
		return ErrInvalidLiveOpusSample
	}
	if ctx == nil {
		return errors.New("media: nil context")
	}
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return ErrLiveOpusClosed
		}
		if err := ctx.Err(); err != nil {
			p.mu.Unlock()
			return err
		}
		if p.size < len(p.queue) {
			idx := (p.head + p.size) % len(p.queue)
			p.queue[idx] = append([]byte(nil), data...)
			p.size++
			p.signalLocked()
			p.mu.Unlock()
			return nil
		}
		changed := p.changed
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

// CloseInput marks end-of-stream. Buffered samples remain available to the SDK before EOF.
func (p *LiveOpusProvider) CloseInput() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.closed = true
		p.signalLocked()
	}
	return nil
}

func (p *LiveOpusProvider) NextSample(ctx context.Context) (media.Sample, error) {
	if ctx == nil {
		return media.Sample{}, errors.New("media: nil context")
	}
	for {
		if err := ctx.Err(); err != nil {
			return media.Sample{}, err
		}
		p.mu.Lock()
		if p.paused {
			p.mu.Unlock()
			return media.Sample{Data: append([]byte(nil), silentOpusFrame[:]...), Duration: LiveOpusSampleDuration}, nil
		}
		if p.size != 0 {
			data := p.queue[p.head]
			p.queue[p.head] = nil
			p.head = (p.head + 1) % len(p.queue)
			p.size--
			p.positionMS += 20
			p.signalLocked()
			p.mu.Unlock()
			return media.Sample{Data: data, Duration: LiveOpusSampleDuration}, nil
		}
		if p.closed {
			p.mu.Unlock()
			return media.Sample{}, io.EOF
		}
		changed := p.changed
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return media.Sample{}, ctx.Err()
		case <-changed:
		}
	}
}

func (p *LiveOpusProvider) OnBind() error { return nil }

// OnUnbind is a transport lifecycle notification. The same provider is rebound by LiveKit, so an
// unbind must leave the input queue usable. LiveKit cancels its write context separately; Close is
// the terminal signal that wakes blocked producers and consumers.
func (p *LiveOpusProvider) OnUnbind() error { return nil }

func (p *LiveOpusProvider) Close() error { return p.CloseInput() }

func (p *LiveOpusProvider) signalLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

// RFC 6716's 20ms Opus silence packet. LiveKit paces samples by Duration.
var silentOpusFrame = [...]byte{0xf8, 0xff, 0xfe}

func (p *LiveOpusProvider) Pause() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paused = true
	p.signalLocked()
}
func (p *LiveOpusProvider) Resume() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paused = false
	p.signalLocked()
	return nil
}
func (p *LiveOpusProvider) PositionMS() int64 { p.mu.Lock(); defer p.mu.Unlock(); return p.positionMS }
func (p *LiveOpusProvider) Paused() bool      { p.mu.Lock(); defer p.mu.Unlock(); return p.paused }

// WaitUnpaused gates PCM reads as well as the published samples. Closing wakes a blocked reader.
func (p *LiveOpusProvider) WaitUnpaused(ctx context.Context) error {
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return ErrLiveOpusClosed
		}
		if !p.paused {
			p.mu.Unlock()
			return nil
		}
		changed := p.changed
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}
