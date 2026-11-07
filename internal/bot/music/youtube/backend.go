package youtube

import (
	"context"
	"errors"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/music"
)

// FixturesEnv names the directory a test-host build serves tracks from. A
// production build refuses to start while it is set.
const FixturesEnv = "DILLA_BOT_MUSIC_FIXTURES"

// ErrFixturesRefused means DILLA_BOT_MUSIC_FIXTURES is set in a build that does
// not carry the dilla_testhost tag: fixtures never play in a shipped Bot.
var ErrFixturesRefused = errors.New("youtube: " + FixturesEnv + " is only honoured by a dilla_testhost build")

// TrackResolver lists queue descriptors for a /play input or a search.
type TrackResolver interface {
	Resolve(ctx context.Context, input string) ([]music.Track, error)
	Search(ctx context.Context, input string) ([]music.Track, error)
}

// Backend is everything the Bot's music path needs from YouTube, built once
// per process so every installation shares one warm worker and one cache.
type Backend struct {
	// Streams starts a track's single-ffmpeg Ogg Opus stream and pre-buffers the next.
	Streams *StreamStarter
	// Tracks resolves /play inputs and searches to queue descriptors.
	Tracks TrackResolver

	warm *WarmResolver
}

// NewBackend builds the sandboxed warm resolver and the stream starter. cache
// <= 0 selects 6 h. In a dilla_testhost build with DILLA_BOT_MUSIC_FIXTURES set
// it serves fixture files through the same ffmpeg arguments instead.
func NewBackend(cfg SandboxConfig, cache time.Duration) (*Backend, error) {
	if backend, handled, err := fixtureBackend(cfg); handled {
		return backend, err
	}
	warm, err := NewWarmResolver(cfg, cache)
	if err != nil {
		return nil, err
	}
	streams, err := NewStreamStarter(cfg, warm)
	if err != nil {
		_ = warm.Close()
		return nil, err
	}
	return &Backend{Streams: streams, Tracks: warm.MetadataResolver(), warm: warm}, nil
}

// Close stops the worker and every pre-buffered stream.
func (b *Backend) Close() error {
	if b == nil {
		return nil
	}
	if b.Streams != nil {
		_ = b.Streams.Close()
	}
	if b.warm != nil {
		_ = b.warm.Close()
	}
	return nil
}
