package youtube

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/music"
	"github.com/dilla-chat/dilla/internal/bot/music/encode"
)

type pcmStarterFunc func(context.Context, string) (io.ReadCloser, error)

func (f pcmStarterFunc) Start(ctx context.Context, source string) (io.ReadCloser, error) {
	return f(ctx, source)
}

func TestOpusStarterDoesNotStartUntilExplicitCallAndRejectsFailedMetadataTrack(t *testing.T) {
	var starts atomic.Int32
	starter := OpusStarter{PCM: pcmStarterFunc(func(context.Context, string) (io.ReadCloser, error) {
		starts.Add(1)
		return io.NopCloser(&zeroReader{}), nil
	})}
	if starts.Load() != 0 {
		t.Fatal("constructing OpusStarter started a source")
	}
	track := music.Track{Provider: music.ProviderYouTube, CanonicalID: "https://www.youtube.com/watch?v=not-a-valid-id"}
	if _, err := starter.Start(context.Background(), track); err == nil {
		t.Fatal("invalid metadata track accepted")
	}
	if starts.Load() != 0 {
		t.Fatal("invalid metadata track started PCM")
	}
}

func TestOpusStarterBridgesBoundedPCMEoFToOpusWithoutPublisher(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	var starts atomic.Int32
	const pcmBytes = 44100 * 2 * 2 / 4
	starter := OpusStarter{
		PCM: pcmStarterFunc(func(_ context.Context, source string) (io.ReadCloser, error) {
			starts.Add(1)
			if source != "https://www.youtube.com/watch?v=aqz-KE-bpKQ" {
				return nil, ErrInvalidInput
			}
			return io.NopCloser(io.LimitReader(zeroReader{}, pcmBytes)), nil
		}),
		Capacity: 100,
		Encoder:  encode.Options{FFmpegPath: ffmpeg},
	}
	track := music.Track{Provider: music.ProviderYouTube, CanonicalID: "aqz-KE-bpKQ"}
	// Construction and metadata validation remain inert; this explicit call owns
	// only PCM decoding and Opus encoding. It never creates a LiveKit publisher.
	session, err := starter.Start(context.Background(), track)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if starts.Load() != 1 {
		t.Fatalf("PCM starts = %d, want one", starts.Load())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var samples int
	for {
		if _, err := session.Provider().NextSample(ctx); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("NextSample = %v", err)
		}
		samples++
	}
	if samples == 0 {
		t.Fatal("encoder emitted no Opus samples")
	}
	if err := session.Wait(); err != nil {
		t.Fatalf("encoder EOF: %v", err)
	}
}

func TestOpusStarterCancellationClosesBoundedPCMSource(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	closed := make(chan struct{})
	starter := OpusStarter{
		PCM: pcmStarterFunc(func(_ context.Context, source string) (io.ReadCloser, error) {
			close(started)
			if source != "https://www.youtube.com/watch?v=aqz-KE-bpKQ" {
				return nil, ErrInvalidInput
			}
			return &testBlockingPCM{closed: closed}, nil
		}),
		Capacity: 1,
		Encoder:  encode.Options{StallTimeout: time.Hour},
	}
	session, err := starter.Start(ctx, music.Track{Provider: music.ProviderYouTube, CanonicalID: "aqz-KE-bpKQ"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not close sandbox PCM source")
	}
	if err := session.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("session error = %v, want context canceled", err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

type testBlockingPCM struct{ closed chan struct{} }

func (s *testBlockingPCM) Read([]byte) (int, error) { <-s.closed; return 0, io.ErrClosedPipe }
func (s *testBlockingPCM) Close() error {
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	return nil
}
