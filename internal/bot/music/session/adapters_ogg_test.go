package session

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/music"
	"github.com/dilla-chat/dilla/internal/bot/music/encode"
	musicruntime "github.com/dilla-chat/dilla/internal/bot/music/runtime"
)

// oggStub is a provider stream that already is Ogg Opus (the single ffmpeg's output).
type oggStub struct {
	io.ReadCloser
}

func (*oggStub) OggOpus() bool { return true }

type oggProviderStub struct {
	prefetched []string
	started    int
}

func (s *oggProviderStub) Start(context.Context, string) (io.ReadCloser, error) {
	s.started++
	return &oggStub{ReadCloser: io.NopCloser(strings.NewReader("OggS...."))}, nil
}
func (s *oggProviderStub) Prefetch(_ context.Context, url string) error {
	s.prefetched = append(s.prefetched, url)
	return nil
}
func (s *oggProviderStub) Check(context.Context) error { return nil }

func TestRuntimeEncoderForwardsOggOpusWithoutASecondEncoder(t *testing.T) {
	wantEncoder := &encoderStub{done: make(chan struct{})}
	var oggCalls, pcmCalls int
	e := RuntimeEncoder{Config: EncoderConfig{Capacity: 7, Options: encode.Options{FFmpegPath: "ffmpeg-test"},
		start: func(context.Context, io.ReadCloser, musicruntime.PCMFormat, int, encode.Options) (Encoder, error) {
			pcmCalls++
			return wantEncoder, nil
		},
		startOgg: func(_ context.Context, _ io.ReadCloser, capacity int, options encode.Options) (Encoder, error) {
			oggCalls++
			if capacity != 7 || options.FFmpegPath != "ffmpeg-test" {
				t.Fatalf("config not forwarded: %d %+v", capacity, options)
			}
			return wantEncoder, nil
		}}}
	provider := &oggProviderStub{}
	stream, err := (YouTubePCM{Source: provider}).Start(context.Background(), "aqz-KE-bpKQ")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Start(context.Background(), &ownedPCM{ReadCloser: stream}); err != nil {
		t.Fatal(err)
	}
	if oggCalls != 1 || pcmCalls != 0 {
		t.Fatalf("Ogg path %d times, PCM encoder %d times; an Ogg Opus stream must not be encoded again", oggCalls, pcmCalls)
	}
}

func TestQueueStarterPrefetchAsksOnlyTheProvider(t *testing.T) {
	provider := &oggProviderStub{}
	options := 0
	starter := QueueStarter{OptionsFor: func(context.Context, music.Track) (Options, error) {
		options++
		return Options{PCM: YouTubePCM{Source: provider}}, nil // no call, no publisher: nothing may join a room
	}}
	err := starter.Prefetch(context.Background(), music.Track{Provider: music.ProviderYouTube, CanonicalID: "aqz-KE-bpKQ"})
	if err != nil {
		t.Fatal(err)
	}
	if len(provider.prefetched) != 1 || provider.prefetched[0] != "https://www.youtube.com/watch?v=aqz-KE-bpKQ" || provider.started != 0 {
		t.Fatalf("prefetched=%v started=%d, want one canonical prefetch and no start", provider.prefetched, provider.started)
	}
	if err := starter.Prefetch(context.Background(), music.Track{Provider: music.ProviderYouTube, CanonicalID: "not a video id"}); err == nil {
		t.Fatal("an invalid track reached the provider")
	}
}

func TestYouTubePCMCheckUsesTheProviderOwnCheck(t *testing.T) {
	if err := (YouTubePCM{Source: &oggProviderStub{}}).Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}
