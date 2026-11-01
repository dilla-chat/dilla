package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/music"
	"github.com/jonasthim/dilla/internal/bot/music/youtube"
	"github.com/jonasthim/dilla/internal/id"
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

func TestYouTubeDeferredProcessRefusalFailsBeforeCallJoin(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	tool, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, raw, class string
		permanent        bool
	}{
		{"private", "ERROR: This video is private", "video_private", true},
		{"rate limit", "ERROR: HTTP Error 429: Too Many Requests", "http_429", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			release := filepath.Join(dir, "release")
			shim := filepath.Join(dir, "bwrap")
			script := fmt.Sprintf(`#!%s
import os,socket,sys,time
args=sys.argv[1:]
s=socket.socket(socket.AF_UNIX,socket.SOCK_STREAM)
s.connect(args[args.index("--bind")+1])
s.sendall(b"DILLA-EGRESS-READY /1.0\r\n\r\n")
s.recv(4096)
s.close()
while not os.path.exists(%q): time.sleep(0.001)
sys.stderr.write(%q+"\n")
sys.exit(1)
`, python, release, tc.raw)
			if err := os.WriteFile(shim, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			production := youtube.PCMStarter{Bubblewrap: shim, Python: python, YTDLP: tool, FFmpeg: tool, Node: tool, Timeout: time.Second}
			source := youtubeSourceFunc(func(ctx context.Context, input string) (io.ReadCloser, error) {
				pcm, err := production.Start(ctx, input)
				if err != nil {
					return nil, err
				}
				if err := os.WriteFile(release, nil, 0600); err != nil {
					_ = pcm.Close()
					return nil, err
				}
				return pcm, nil
			})
			joined := false
			group := id.New()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			session, failure := Start(ctx, Options{
				Community: id.New(), Channel: id.New(), Provider: music.ProviderYouTube, Track: "aqz-KE-bpKQ",
				PCM: YouTubePCM{Source: source}, Encoder: encStart{enc: &encoderStub{done: make(chan struct{})}}, Publisher: pubStart{pub: &pubStub{done: make(chan struct{})}}, Epochs: epochStub{},
				PrepareCall: func(context.Context) (client.Call, error) {
					joined = true
					return client.Call{CallID: id.New(), GroupID: group}, nil
				},
			})
			if session != nil {
				_ = session.Close()
			}
			if joined {
				t.Fatal("call joined before first PCM frame proved provider readiness")
			}
			var process *youtube.ProcessError
			var start *music.StartError
			if !errors.As(failure, &process) || !errors.As(failure, &start) || start.ProviderFailure() != tc.permanent || music.FailureClass(failure) != tc.class {
				t.Fatalf("error=%v class=%s want class=%s permanent=%v", failure, music.FailureClass(failure), tc.class, tc.permanent)
			}
		})
	}
}
