package session

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/music/encode"
	"github.com/jonasthim/dilla/internal/bot/music/livekit"
	musicruntime "github.com/jonasthim/dilla/internal/bot/music/runtime"
	"github.com/jonasthim/dilla/internal/bot/music/spotify"
	"github.com/jonasthim/dilla/internal/bot/music/youtube"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/id"
)

type Spotify struct{ Adapter *spotify.Adapter }

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
	pcm, err := s.Source.Start(ctx, canonical)
	if err != nil {
		if pcm != nil {
			_ = pcm.Close()
		}
		return nil, err
	}
	if pcm == nil {
		return nil, ErrInvalid
	}
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
	return &prefetchedYouTubePCM{Reader: io.MultiReader(bytes.NewReader(frame[:]), source), source: source}, nil
}

type prefetchedYouTubePCM struct {
	io.Reader
	source *ownedPCM
}

func (p *prefetchedYouTubePCM) Close() error                      { return p.source.Close() }
func (p *prefetchedYouTubePCM) PCMFormat() musicruntime.PCMFormat { return p.source.PCMFormat() }

func (s Spotify) Start(ctx context.Context, track string) (io.ReadCloser, error) {
	return s.Adapter.Start(ctx, track)
}

type EncoderConfig struct {
	Capacity int
	Options  encode.Options
	start    func(context.Context, io.ReadCloser, musicruntime.PCMFormat, int, encode.Options) (Encoder, error)
}
type RuntimeEncoder struct{ Config EncoderConfig }

const youtubePCMRate = 44100

func (e RuntimeEncoder) Start(ctx context.Context, pcm io.ReadCloser) (Encoder, error) {
	format := musicruntime.PCMFormat{SampleRate: youtubePCMRate, Channels: 2}
	if source, ok := pcm.(interface{ PCMFormat() musicruntime.PCMFormat }); ok {
		format = source.PCMFormat()
	}
	if e.Config.start != nil {
		return e.Config.start(ctx, pcm, format, e.Config.Capacity, e.Config.Options)
	}
	return musicruntime.Start(ctx, pcm, format, e.Config.Capacity, e.Config.Options)
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
	source, production := s.Source.(youtube.PCMStarter)
	if !production {
		return nil
	}
	for _, path := range []string{source.YTDLP, source.FFmpeg, source.Node} {
		if path == "" {
			return youtube.ErrRunnerRequired
		}
		if _, err := exec.LookPath(path); err != nil {
			return youtube.ErrRunnerRequired
		}
	}
	for _, path := range []struct{ configured, fallback string }{{source.Bubblewrap, "bwrap"}, {source.Python, "python3"}} {
		name := path.configured
		if name == "" {
			name = path.fallback
		}
		if _, err := exec.LookPath(name); err != nil {
			return youtube.ErrSandboxUnavailable
		}
	}
	return nil
}
