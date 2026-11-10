package youtube

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// clientResolver answers a lookup per player client, as the warm resolver does: the first with playerClients[0],
// a retry after a refused stream with the next.
type clientResolver struct {
	mu      sync.Mutex
	plain   int
	next    []string // the failed clients ResolveNextClient was asked past
	invalid int
}

func (c *clientResolver) resolved(client string) Resolved {
	return Resolved{StreamURL: "https://rr1---sn-abc.googlevideo.com/videoplayback?expire=4102444800&client=" + client,
		Expires: time.Now().Add(time.Hour), UserAgent: testUserAgent, Client: client}
}

func (c *clientResolver) Resolve(context.Context, string) (Resolved, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.plain++
	return c.resolved(playerClients[0]), nil
}

func (c *clientResolver) ResolveNextClient(_ context.Context, _ string, failed Resolved) (Resolved, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next = append(c.next, failed.Client)
	next := nextPlayerClient(failed.Client)
	if next == "" {
		return Resolved{}, ErrExtractFailed
	}
	return c.resolved(next), nil
}

func (c *clientResolver) Invalidate(string) { c.mu.Lock(); c.invalid++; c.mu.Unlock() }

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *syncBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.buf.String() }

func clientStarter(t *testing.T, ffmpegExtra string, resolver Resolver) (*StreamStarter, *syncBuffer, []byte) {
	t.Helper()
	dir := t.TempDir()
	stream := testOgg(t, 0.5)
	ffmpeg := fakeStreamFFmpeg(t, dir, stream, ffmpegExtra)
	bwrap, _ := countingBwrap(t, dir)
	logs := &syncBuffer{}
	starter, err := NewStreamStarter(SandboxConfig{Bubblewrap: bwrap, FFmpeg: ffmpeg, Node: writeScript(t, dir, "node", `exit 0`),
		Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}, resolver)
	if err != nil {
		t.Skipf("stream starter unavailable: %v", err)
	}
	t.Cleanup(func() { _ = starter.Close() })
	return starter, logs, stream
}

// Bot v2 Task 19 fix round 5 (the 403 ruling, option A): googlevideo refusing a stream at its start (403) makes the
// one retry a lookup with the next player client, and the client that worked is logged (no URL, no video id).
func TestARefusedStreamIsLookedUpAgainWithTheNextPlayerClient(t *testing.T) {
	resolver := &clientResolver{}
	refused := `case "$*" in *client=` + playerClients[0] + `*) echo "[https @ 0x1] HTTP error 403 Forbidden" >&2; exit 1;; esac`
	starter, logs, stream := clientStarter(t, refused, resolver)
	source, err := starter.Start(context.Background(), "https://www.youtube.com/watch?v=aqz-KE-bpKQ")
	if errors.Is(err, ErrSandboxUnavailable) {
		t.Skipf("bubblewrap network namespaces unavailable: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if got, err := io.ReadAll(source); err != nil || !bytes.Equal(got, stream) {
		t.Fatalf("the next client's stream: %d bytes, %v", len(got), err)
	}
	resolver.mu.Lock()
	plain, next := resolver.plain, append([]string(nil), resolver.next...)
	resolver.mu.Unlock()
	if plain != 1 || len(next) != 1 || next[0] != playerClients[0] {
		t.Fatalf("lookups: %d plain, next past %v; want 1 and [%s]", plain, next, playerClients[0])
	}
	text := logs.String()
	if !strings.Contains(text, "client="+playerClients[1]) || !strings.Contains(text, "refused_client="+playerClients[0]) {
		t.Fatalf("the client that worked was not logged:\n%s", text)
	}
	if strings.Contains(text, "googlevideo") || strings.Contains(text, "aqz-KE-bpKQ") {
		t.Fatalf("a log names the stream URL or the video:\n%s", text)
	}
}

// A stream that fails for another reason keeps the plain retry (a fresh lookup with the same client).
func TestAStreamFailureThatIsNotA403KeepsThePlainRetry(t *testing.T) {
	resolver := &clientResolver{}
	// Every start of the first client fails with a non-403 error: the plain retry runs, and no next-client lookup.
	starter, _, _ := clientStarter(t, `case "$*" in *client=`+playerClients[0]+`*) echo "Connection reset by peer" >&2; exit 1;; esac`, resolver)
	_, err := starter.Start(context.Background(), "https://www.youtube.com/watch?v=aqz-KE-bpKQ")
	if errors.Is(err, ErrSandboxUnavailable) {
		t.Skipf("bubblewrap network namespaces unavailable: %v", err)
	}
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	if err == nil || resolver.plain != 2 || len(resolver.next) != 0 {
		t.Fatalf("err=%v plain lookups=%d next-client lookups=%v, want a failure after 2 plain lookups and none by client", err, resolver.plain, resolver.next)
	}
}
