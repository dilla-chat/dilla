package youtube

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestLiveYouTubeThroughTheSandbox runs the shipped path against real YouTube (Bot v2 Task 19 fix round 1): the warm
// worker's one yt-dlp run (the %(.{...})j print template, the worker's cache dir, yt-dlp loaded as a library), then one sandboxed ffmpeg fetching the
// googlevideo stream through the egress broker with the forwarded User-Agent and the protocol whitelist, to its first
// Ogg Opus bytes. It needs network and the tools, so it runs only with DILLA_YOUTUBE_LIVE=1 (DILLA_YTDLP may name the
// yt-dlp to use).
func TestLiveYouTubeThroughTheSandbox(t *testing.T) {
	if os.Getenv("DILLA_YOUTUBE_LIVE") != "1" {
		t.Skip("live YouTube check: set DILLA_YOUTUBE_LIVE=1")
	}
	look := func(name string) string {
		t.Helper()
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return path
	}
	cfg := SandboxConfig{Bubblewrap: look("bwrap"), Python: look("python3"), FFmpeg: look("ffmpeg"), Node: look("node"), YTDLP: os.Getenv("DILLA_YTDLP"),
		WarmUpLookup: os.Getenv("DILLA_YOUTUBE_LIVE_WARM") != "0"}
	if cfg.YTDLP == "" {
		cfg.YTDLP = look("yt-dlp")
	}
	backend, err := NewBackend(cfg, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	view, err := backend.For("live-check")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if cfg.WarmUpLookup {
		// A Bot's worker starts with the Bot, before any /play: its warm-up lookup has run by then.
		time.Sleep(4 * time.Second)
	}
	for i, video := range []string{"https://www.youtube.com/watch?v=aqz-KE-bpKQ", "https://www.youtube.com/watch?v=jNQXAC9IVRw", "https://www.youtube.com/watch?v=M7lc1UVf-VE"} {
		began := time.Now()
		tracks, err := view.Tracks.Resolve(ctx, video)
		if err != nil || len(tracks) != 1 || tracks[0].Title == "" {
			t.Fatalf("resolve %d (the print template): %v %+v", i, err, tracks)
		}
		resolved := time.Since(began)
		started := time.Now()
		stream, err := view.Streams.Start(ctx, video)
		if err != nil {
			t.Fatalf("stream %d (ffmpeg through the broker): %v", i, err)
		}
		head := make([]byte, 4)
		if _, err := io.ReadFull(stream, head); err != nil || !bytes.Equal(head, []byte("OggS")) {
			_ = stream.Close()
			t.Fatalf("stream %d first bytes = %q, %v (googlevideo refused the fetch, the User-Agent or the protocol whitelist)", i, head, err)
		}
		firstByte := time.Since(started)
		_ = stream.Close()
		again := time.Now()
		if _, err := view.Tracks.Resolve(ctx, video); err != nil {
			t.Fatal(err)
		}
		t.Logf("video %d %q: cold resolve %v, stream first Ogg bytes %v, repeated resolve %v (cache)",
			i, tracks[0].Title, resolved, firstByte, time.Since(again))
	}
}
