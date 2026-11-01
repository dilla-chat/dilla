package session

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/jonasthim/dilla/internal/bot/music/encode"
	musicruntime "github.com/jonasthim/dilla/internal/bot/music/runtime"
	"github.com/jonasthim/dilla/internal/bot/music/spotify"
)

func TestRuntimeEncoderUsesProviderPCMFormat(t *testing.T) {
	for _, tc := range []struct {
		name string
		pcm  io.ReadCloser
		rate int
	}{
		{"youtube", io.NopCloser(nilReader{}), 44100},
		{"spotify", &spotify.Playback{}, 48000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got musicruntime.PCMFormat
			wantEncoder := &encoderStub{done: make(chan struct{})}
			e := RuntimeEncoder{Config: EncoderConfig{Capacity: 7, Options: encode.Options{FFmpegPath: "ffmpeg-test"}, start: func(_ context.Context, _ io.ReadCloser, format musicruntime.PCMFormat, capacity int, options encode.Options) (Encoder, error) {
				got = format
				if capacity != 7 || options.FFmpegPath != "ffmpeg-test" {
					t.Fatalf("config not forwarded: capacity=%d options=%+v", capacity, options)
				}
				return wantEncoder, nil
			}}}
			if _, err := e.Start(context.Background(), tc.pcm); err != nil {
				t.Fatal(err)
			}
			if got != (musicruntime.PCMFormat{SampleRate: tc.rate, Channels: 2}) {
				t.Fatalf("PCM format=%+v", got)
			}
		})
	}
}

type nilReader struct{}

func (nilReader) Read([]byte) (int, error) { return 0, io.EOF }

type youtubePCMStub struct {
	input string
	calls int
}

func (s *youtubePCMStub) Start(_ context.Context, input string) (io.ReadCloser, error) {
	s.input = input
	s.calls++
	return io.NopCloser(strings.NewReader("\x00\x01\x02\x03")), nil
}

func TestYouTubePCMAdaptsCanonicalQueueIDToSandboxURL(t *testing.T) {
	provider := &youtubePCMStub{}
	adapter := YouTubePCM{Source: provider}
	if _, err := adapter.Start(context.Background(), "aqz-KE-bpKQ"); err != nil {
		t.Fatal(err)
	}
	if provider.calls != 1 || provider.input != "https://www.youtube.com/watch?v=aqz-KE-bpKQ" {
		t.Fatalf("provider call=%d input=%q", provider.calls, provider.input)
	}
}

func TestYouTubePCMRejectsNonCanonicalQueueIDBeforeProvider(t *testing.T) {
	provider := &youtubePCMStub{}
	adapter := YouTubePCM{Source: provider}
	if _, err := adapter.Start(context.Background(), "https://www.youtube.com/watch?v=aqz-KE-bpKQ"); err == nil {
		t.Fatal("expected queue URL to be rejected")
	}
	if provider.calls != 0 {
		t.Fatal("invalid queue identifier reached PCM provider")
	}
}
