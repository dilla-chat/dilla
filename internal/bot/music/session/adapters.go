package session

import (
	"bytes"
	"context"
	"io"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/music"
	"github.com/dilla-chat/dilla/internal/bot/music/encode"
	"github.com/dilla-chat/dilla/internal/bot/music/livekit"
	musicruntime "github.com/dilla-chat/dilla/internal/bot/music/runtime"
	"github.com/dilla-chat/dilla/internal/bot/music/youtube"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/id"
)

// YouTubePCM maps the provider-native video ID stored in a queue entry to the
// canonical URL accepted by the sandboxed YouTube PCM source.
type YouTubePCM struct{ Source youtube.PCMSourceStarter }

func (s YouTubePCM) Start(ctx context.Context, track string) (io.ReadCloser, error) {
	if ctx == nil {
		return nil, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, context.Cause(ctx)
	}
	if s.Source == nil {
		return nil, youtube.ErrRunnerRequired
	}
	if len(track) != 11 {
		return nil, youtube.ErrInvalidInput
	}
	for i := 0; i < len(track); i++ {
		c := track[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return nil, youtube.ErrInvalidInput
		}
	}
	canonical := "https://www.youtube.com/watch?v=" + track
	kind, normalized, err := youtube.NormalizeInput(canonical)
	if err != nil || kind != "video" || normalized != canonical {
		return nil, youtube.ErrInvalidInput
	}
	ctx = youtube.WithDiagnosticLogger(ctx, music.PlaybackLogger(ctx))
	started := time.Now()
	var pcm io.ReadCloser
	if source, ok := s.Source.(interface {
		StartAt(context.Context, string, int64) (io.ReadCloser, error)
	}); ok {
		pcm, err = source.StartAt(ctx, canonical, music.StartAtMS(ctx))
	} else {
		pcm, err = s.Source.Start(ctx, canonical)
	}
	if err != nil {
		if pcm != nil {
			_ = pcm.Close()
		}
		return nil, err
	}
	if pcm == nil {
		return nil, ErrInvalid
	}
	readyAt := time.Now()
	music.PlaybackLogger(ctx).DebugContext(ctx, "youtube PCM start", "stage", "sandbox_ready", "elapsed_ms", readyAt.Sub(started).Milliseconds())
	source := &ownedPCM{ReadCloser: pcm}
	// The sandbox's READY handshake precedes yt-dlp. Prove the decoder produced
	// one s16 stereo sample before admitting the bot to a call, retaining it for
	// the encoder. Closing the source interrupts a blocked read on cancellation.
	stopCancel := context.AfterFunc(ctx, func() { _ = source.Close() })
	defer stopCancel()
	var frame [4]byte
	_, err = io.ReadFull(source, frame[:])
	if ctx.Err() != nil {
		_ = source.Close()
		return nil, context.Cause(ctx)
	}
	if err != nil {
		_ = source.Close()
		return nil, err
	}
	music.PlaybackLogger(ctx).DebugContext(ctx, "youtube PCM start", "stage", "first_sample", "elapsed_ms", time.Since(readyAt).Milliseconds())
	return &prefetchedYouTubePCM{Reader: io.MultiReader(bytes.NewReader(frame[:]), source), source: source}, nil
}

type prefetchedYouTubePCM struct {
	io.Reader
	source *ownedPCM
}

func (p *prefetchedYouTubePCM) Close() error { return p.source.Close() }

// OggOpus reports that the provider already delivers 48 kHz Ogg Opus from its
// single ffmpeg, so no second encoder runs over it.
func (p *prefetchedYouTubePCM) OggOpus() bool { return isOggOpus(p.source.ReadCloser) }

func isOggOpus(source io.Reader) bool {
	marked, ok := source.(interface{ OggOpus() bool })
	return ok && marked.OggOpus()
}

type EncoderConfig struct {
	Capacity int
	Options  encode.Options
	startOgg func(context.Context, io.ReadCloser, int, encode.Options) (Encoder, error)
}
type RuntimeEncoder struct{ Config EncoderConfig }

// Start forwards the provider's Ogg Opus: one ffmpeg in the stream worker already produced 48 kHz Opus in 20 ms
// frames. Bot v2 Task 23 removed the PCM-to-Opus path, so any other source is refused.
func (e RuntimeEncoder) Start(ctx context.Context, pcm io.ReadCloser) (Encoder, error) {
	if !isOggOpus(pcm) {
		if pcm != nil {
			_ = pcm.Close()
		}
		return nil, ErrInvalid
	}
	if e.Config.startOgg != nil {
		return e.Config.startOgg(ctx, pcm, e.Config.Capacity, e.Config.Options)
	}
	return musicruntime.StartOgg(ctx, pcm, e.Config.Capacity, e.Config.Options)
}

type LiveKitPublisher struct{ Lifetime time.Duration }

func (p LiveKitPublisher) Start(ctx context.Context, call client.Call, group id.ID, epochs livekit.EpochSource, enc Encoder) (Publisher, error) {
	concrete, ok := enc.(*musicruntime.Session)
	if !ok {
		_ = enc.Close()
		return nil, ErrInvalid
	}
	return livekit.Start(ctx, call, group, epochs, concrete, p.Lifetime)
}

var _ livekit.EpochSource = (*native.Client)(nil)

// RetainedLiveKitPublisher replaces a track inside the queue owner's authorized
// room. The QueueStarter closes the pool when that owner is torn down.
type RetainedLiveKitPublisher struct{ Pool *livekit.Pool }

func (p RetainedLiveKitPublisher) Start(ctx context.Context, call client.Call, group id.ID, epochs livekit.EpochSource, enc Encoder) (Publisher, error) {
	concrete, ok := enc.(*musicruntime.Session)
	if !ok || p.Pool == nil {
		_ = enc.Close()
		return nil, ErrInvalid
	}
	return p.Pool.Start(ctx, call, group, epochs, concrete)
}

// Check keeps a missing resolver/runtime from admitting the Bot to a call.
func (s YouTubePCM) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.Source == nil {
		return youtube.ErrRunnerRequired
	}
	if checked, ok := s.Source.(interface{ Check(context.Context) error }); ok {
		return checked.Check(ctx)
	}
	return nil
}

func (p *prefetchedYouTubePCM) SourceGone() bool { return p.source.SourceGone() }

// Prefetch asks the provider to resolve and buffer a track ahead of its start.
// A provider without pre-buffering is not an error: the track then starts cold.
func (s YouTubePCM) Prefetch(ctx context.Context, track string) error {
	if ctx == nil || s.Source == nil {
		return ErrInvalid
	}
	source, ok := s.Source.(interface {
		Prefetch(context.Context, string) error
	})
	if !ok {
		return nil
	}
	canonical := "https://www.youtube.com/watch?v=" + track
	if kind, normalized, err := youtube.NormalizeInput(canonical); err != nil || kind != "video" || normalized != canonical {
		return youtube.ErrInvalidInput
	}
	return source.Prefetch(youtube.WithDiagnosticLogger(ctx, music.PlaybackLogger(ctx)), canonical)
}
