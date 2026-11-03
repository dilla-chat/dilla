package youtube

import (
	"context"
	"errors"
	"io"

	"github.com/dilla-chat/dilla/internal/bot/music"
	"github.com/dilla-chat/dilla/internal/bot/music/encode"
	musicruntime "github.com/dilla-chat/dilla/internal/bot/music/runtime"
)

var ErrOpusSource = errors.New("youtube: invalid Opus source configuration")

// PCMSourceStarter is implemented by PCMStarter and permits bounded test
// sources to exercise the PCM-to-Opus bridge without live provider access.
type PCMSourceStarter interface {
	Start(context.Context, string) (io.ReadCloser, error)
}

// OpusStarter bridges one already-resolved YouTube video track into the bounded
// live Opus sample source. It does not publish to LiveKit or open a voice call;
// Start must be invoked explicitly by the owning playback lifecycle.
type OpusStarter struct {
	PCM      PCMSourceStarter
	Encoder  encode.Options
	Capacity int
}

// Start validates a resolved queue track, starts its sandboxed PCM source, and
// starts the existing PCM-to-Opus encoder. The returned runtime session owns
// both processes and exposes only a bounded Opus sample provider.
func (s OpusStarter) Start(ctx context.Context, track music.Track) (*musicruntime.Session, error) {
	if ctx == nil || s.PCM == nil || track.Provider != music.ProviderYouTube {
		return nil, ErrOpusSource
	}
	canonical, err := resolvedVideoInput(track.CanonicalID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	pcm, err := s.PCM.Start(ctx, canonical)
	if err != nil {
		return nil, err
	}
	capacity := s.Capacity
	if capacity == 0 {
		capacity = 50
	}
	encoded, err := musicruntime.Start(ctx, pcm, musicruntime.PCMFormat{SampleRate: 44100, Channels: 2}, capacity, s.Encoder)
	if err != nil {
		_ = pcm.Close()
		return nil, err
	}
	return encoded, nil
}

func resolvedVideoInput(value string) (string, error) {
	if idPattern.MatchString(value) {
		return videoURL(value), nil
	}
	kind, canonical, err := NormalizeInput(value)
	if err != nil || kind != "video" || canonical != value {
		return "", ErrInvalidInput
	}
	return canonical, nil
}
