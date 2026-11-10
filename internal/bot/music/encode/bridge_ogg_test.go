package encode

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/media"
)

// oggOpus renders a sine wave through the real ffmpeg with the exact output
// arguments the sandboxed stream worker uses (one ffmpeg, stream to Ogg Opus).
func oggOpus(t *testing.T, seconds float64, frameMS int) []byte {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	out, err := exec.CommandContext(context.Background(), "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100:duration="+strconv.FormatFloat(seconds, 'f', 3, 64),
		"-map", "0:a:0", "-ar", "48000", "-ac", "2",
		"-c:a", "libopus", "-application", "audio", "-b:a", "96k", "-vbr", "off", "-dtx", "0", "-frame_duration", strconv.Itoa(frameMS),
		"-flush_packets", "1", "-page_duration", "20000", "-f", "ogg", "pipe:1").Output()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestOpusFrames48kHz20ms(t *testing.T) {
	stream := oggOpus(t, 0.5, 20)
	provider, err := media.NewLiveOpusProvider(100)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := BridgeOgg(ctx, io.NopCloser(bytes.NewReader(stream)), provider, Options{}); err != nil {
		t.Fatal(err)
	}
	count := 0
	for {
		sample, err := provider.NextSample(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if sample.Duration != 20*time.Millisecond {
			t.Fatalf("packet duration %s, want 20ms", sample.Duration)
		}
		count++
	}
	if count < 24 || count > 27 {
		t.Fatalf("got %d packets for 500ms, want about 25", count)
	}
}

func TestBridgeOggRejectsNon20msFramesAndTruncatedStreams(t *testing.T) {
	provider, _ := media.NewLiveOpusProvider(100)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := BridgeOgg(ctx, io.NopCloser(bytes.NewReader(oggOpus(t, 0.5, 40))), provider, Options{}); !errors.Is(err, ErrInvalidOpus) {
		t.Fatalf("40 ms frames: %v, want ErrInvalidOpus", err)
	}
	provider, _ = media.NewLiveOpusProvider(100)
	stream := oggOpus(t, 0.5, 20)
	if err := BridgeOgg(ctx, io.NopCloser(bytes.NewReader(stream[:len(stream)/2])), provider, Options{}); !errors.Is(err, ErrInvalidOpus) {
		t.Fatalf("truncated stream: %v, want ErrInvalidOpus", err)
	}
}

func TestBridgeOggStallsWhenTheSourceStopsDelivering(t *testing.T) {
	provider, _ := media.NewLiveOpusProvider(100)
	reader, writer := io.Pipe()
	defer writer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := BridgeOgg(ctx, reader, provider, Options{StallTimeout: 50 * time.Millisecond}); !errors.Is(err, ErrStalled) {
		t.Fatalf("stalled source: %v, want ErrStalled", err)
	}
}
