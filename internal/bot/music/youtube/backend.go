package youtube

import (
	"context"
	"errors"
	"io"
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
	// Both are reachable only through For, so nothing can use the cache or the buffers without an owner.
	streams *StreamStarter
	tracks  TrackResolver

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
	return newBackend(warm, streams), nil
}

func newBackend(warm *WarmResolver, streams *StreamStarter) *Backend {
	return &Backend{streams: streams, tracks: warmTracks{warm: warm, metadata: warm.MetadataResolver()}, warm: warm}
}

// warmTracks answers a /play: a video or a search text goes through the warm
// resolver, so it is one yt-dlp run (none when cached) that also seeds the
// stream URL the track's start will use. Playlists and suggestions keep the
// flat metadata listing.
type warmTracks struct {
	warm     *WarmResolver
	metadata MetadataResolver
}

func (t warmTracks) Resolve(ctx context.Context, input string) ([]music.Track, error) {
	kind, _, err := NormalizeInput(input)
	if err != nil {
		return nil, err
	}
	if kind == "playlist" {
		return t.metadata.Resolve(ctx, input)
	}
	resolved, err := t.warm.Resolve(ctx, input)
	if err != nil {
		return nil, err
	}
	return []music.Track{resolved.Track}, nil
}

func (t warmTracks) Search(ctx context.Context, input string) ([]music.Track, error) {
	return t.metadata.Search(ctx, input)
}

// For returns the view of the backend one community (or installation) uses.
// Its lookups are cached and its pre-buffers held apart from every other
// owner's: another community never hits this one's cache entries or claims its
// buffer, and its start timings report Cold for its own lookups only. The cost
// is one extra yt-dlp run per community for the same song.
// The owner must be non-empty printable text without NUL, or For fails closed.
func (b *Backend) For(owner string) (*OwnerBackend, error) {
	if b == nil || b.streams == nil || b.tracks == nil || !validOwner(owner) {
		return nil, ErrInvalidInput
	}
	return &OwnerBackend{Streams: &OwnedStreams{streams: b.streams, owner: owner}, Tracks: ownedTracks{base: b.tracks, owner: owner}}, nil
}

// OwnerBackend is a Backend scoped to one owner.
type OwnerBackend struct {
	Streams *OwnedStreams
	Tracks  TrackResolver
}

type ownedTracks struct {
	base  TrackResolver
	owner string
}

func (t ownedTracks) Resolve(ctx context.Context, input string) ([]music.Track, error) {
	return t.base.Resolve(WithOwner(ctx, t.owner), input)
}

func (t ownedTracks) Search(ctx context.Context, input string) ([]music.Track, error) {
	return t.base.Search(WithOwner(ctx, t.owner), input)
}

// OwnedStreams is a StreamStarter scoped to one owner. It satisfies
// PCMSourceStarter, so it is the YouTube source of that owner's music runtime.
type OwnedStreams struct {
	streams *StreamStarter
	owner   string
}

func (o *OwnedStreams) Start(ctx context.Context, source string) (io.ReadCloser, error) {
	return o.streams.Start(WithOwner(ctx, o.owner), source)
}

func (o *OwnedStreams) StartAt(ctx context.Context, source string, ms int64) (io.ReadCloser, error) {
	return o.streams.StartAt(WithOwner(ctx, o.owner), source, ms)
}

func (o *OwnedStreams) Prefetch(ctx context.Context, source string) error {
	return o.streams.Prefetch(WithOwner(ctx, o.owner), source)
}

func (o *OwnedStreams) Check(ctx context.Context) error { return o.streams.Check(ctx) }

// Close stops the worker and every pre-buffered stream.
func (b *Backend) Close() error {
	if b == nil {
		return nil
	}
	if b.streams != nil {
		_ = b.streams.Close()
	}
	if b.warm != nil {
		_ = b.warm.Close()
	}
	return nil
}
