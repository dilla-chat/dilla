package session

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/music"
)

type youtubeSourceFunc func(context.Context, string) (io.ReadCloser, error)

func (f youtubeSourceFunc) Start(ctx context.Context, input string) (io.ReadCloser, error) {
	return f(ctx, input)
}

type countedPCM struct {
	io.ReadCloser
	closes atomic.Int32
}

func (p *countedPCM) Close() error { p.closes.Add(1); return p.ReadCloser.Close() }

func TestYouTubePCMPrefetchPreservesEveryByte(t *testing.T) {
	data := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	source := &countedPCM{ReadCloser: io.NopCloser(bytes.NewReader(data))}
	adapter := YouTubePCM{Source: youtubeSourceFunc(func(context.Context, string) (io.ReadCloser, error) { return source, nil })}
	pcm, err := adapter.Start(t.Context(), "aqz-KE-bpKQ")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(pcm)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("PCM=%v error=%v", got, err)
	}
	if err := pcm.Close(); err != nil {
		t.Fatal(err)
	}
	if source.closes.Load() != 1 {
		t.Fatal("prefetched PCM did not own source close")
	}
}

func TestYouTubePCMPrefetchCancellationClosesBlockedSource(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			reader, writer := io.Pipe()
			defer writer.Close()
			source := &countedPCM{ReadCloser: reader}
			started := make(chan struct{})
			adapter := YouTubePCM{Source: youtubeSourceFunc(func(context.Context, string) (io.ReadCloser, error) { close(started); return source, nil })}
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(context.Canceled)
			done := make(chan error, 1)
			go func() {
				pcm, err := adapter.Start(ctx, "aqz-KE-bpKQ")
				if pcm != nil {
					_ = pcm.Close()
				}
				done <- err
			}()
			<-started
			cancel(cause)
			select {
			case err := <-done:
				if !errors.Is(err, cause) {
					t.Fatalf("blocked prefetch=%v want %v", err, cause)
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation did not unblock PCM prefetch")
			}
			if source.closes.Load() != 1 {
				t.Fatalf("source closed %d times", source.closes.Load())
			}
		})
	}
}

func TestYouTubePCMEmptyOrTruncatedAudioPauses(t *testing.T) {
	for _, data := range []string{"", "\x00\x01"} {
		adapter := YouTubePCM{Source: youtubeSourceFunc(func(context.Context, string) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(data)), nil
		})}
		pcm, err := adapter.Start(t.Context(), "aqz-KE-bpKQ")
		if pcm != nil {
			_ = pcm.Close()
		}
		if err == nil {
			t.Fatalf("accepted %d PCM bytes", len(data))
		}
		var failure *music.StartError
		if !errors.As(providerStage(t.Context(), "provider_pcm", err), &failure) {
			t.Fatal("missing start failure")
		}
		if failure.ProviderFailure() {
			t.Fatal("unknown/empty audio must pause")
		}
	}
}

func TestYouTubePCMLogsSandboxReadyAndFirstSampleSeparately(t *testing.T) {
	var logs bytes.Buffer
	ctx := music.WithPlaybackLogger(t.Context(), slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	adapter := YouTubePCM{Source: youtubeSourceFunc(func(context.Context, string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader([]byte{0, 1, 2, 3})), nil
	})}
	pcm, err := adapter.Start(ctx, "abcdefghijk")
	if err != nil {
		t.Fatal(err)
	}
	defer pcm.Close()
	if !strings.Contains(logs.String(), `"stage":"sandbox_ready"`) || !strings.Contains(logs.String(), `"stage":"first_sample"`) || !strings.Contains(logs.String(), `"elapsed_ms":`) || strings.Contains(logs.String(), "abcdefghijk") {
		t.Fatalf("missing/redaction timing log: %s", logs.String())
	}
}
