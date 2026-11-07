package webhookavatar

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
)

// countingFetch answers each URL from a table and counts the fetches.
type countingFetch struct {
	mu     sync.Mutex
	n      int
	answer map[string][]byte
}

func (c *countingFetch) Fetch(_ context.Context, raw string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	if b, ok := c.answer[raw]; ok {
		return b, nil
	}
	return nil, ErrAddress
}

func (c *countingFetch) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func newTestService(t *testing.T, fetch Fetch) (*Service, *clock.Fake) {
	t.Helper()
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	cfg := config.Default()
	return New(Options{Clock: clk, Limiter: server.NewRateLimiter(cfg.Limits.Rate, clk), Fetcher: fetch}), clk
}

func TestCacheFetchesOnceServesPerCommunityAndExpires(t *testing.T) {
	img := pngOf(t, 4, 4)
	f := &countingFetch{answer: map[string][]byte{"https://example.com/a.png": img}}
	s, clk := newTestService(t, f)
	community, other, webhook := id.New(), id.New(), id.New()
	got := s.Resolve(context.Background(), community, webhook, "https://example.com/a.png", time.Second)
	sum := sha256.Sum256(img)
	if !bytes.Equal(got, sum[:]) {
		t.Fatalf("Resolve = %x, want the image's SHA-256", got)
	}
	if again := s.Resolve(context.Background(), community, webhook, "https://example.com/a.png", time.Second); !bytes.Equal(again, got) || f.count() != 1 {
		t.Fatalf("second Resolve = %x after %d fetches, want the cached id and 1 fetch", again, f.count())
	}
	if b, ok := s.Get(community, got); !ok || !bytes.Equal(b, img) {
		t.Fatal("the cached image is not served to its community")
	}
	if _, ok := s.Get(other, got); ok {
		t.Fatal("another community reads the image")
	}
	clk.Advance(PositiveTTL)
	if _, ok := s.Get(community, got); ok {
		t.Fatal("the image outlived its TTL")
	}
}

func TestCacheRemembersAFailureAndMetersFetches(t *testing.T) {
	f := &countingFetch{answer: map[string][]byte{}}
	s, clk := newTestService(t, f)
	community, webhook := id.New(), id.New()
	for range 5 {
		if got := s.Resolve(context.Background(), community, webhook, "https://internal.test/x.png", time.Second); got != nil {
			t.Fatalf("a failed fetch answered %x", got)
		}
	}
	if f.count() != 1 {
		t.Fatalf("%d fetches of a failing URL within NegativeTTL, want 1", f.count())
	}
	clk.Advance(NegativeTTL)
	s.Resolve(context.Background(), community, webhook, "https://internal.test/x.png", time.Second)
	if f.count() != 2 {
		t.Fatalf("%d fetches after NegativeTTL, want 2", f.count())
	}
	// NegativeTTL refilled the webhook's burst of 2: one more distinct URL is fetched, the next is not.
	s.Resolve(context.Background(), community, webhook, "https://internal.test/y.png", time.Second)
	s.Resolve(context.Background(), community, webhook, "https://internal.test/z.png", time.Second)
	if f.count() != 3 {
		t.Fatalf("%d fetches past the per-webhook bucket, want 3", f.count())
	}
	// A non-https URL never reaches the fetcher.
	if s.Resolve(context.Background(), community, id.New(), "http://example.com/a.png", time.Second) != nil || f.count() != 3 {
		t.Fatal("a non-https URL was fetched")
	}
}

// blockingFetch holds every fetch until release.
type blockingFetch struct {
	release chan struct{}
	img     []byte
}

func (b *blockingFetch) Fetch(ctx context.Context, _ string) ([]byte, error) {
	select {
	case <-b.release:
		return b.img, nil
	case <-ctx.Done():
		return nil, errors.New("timeout")
	}
}

// A slow fetch does not hold the post: Resolve answers nil after its wait, and the image serves the next post.
func TestCacheSlowFetchFollowsTheNextPost(t *testing.T) {
	img := pngOf(t, 4, 4)
	f := &blockingFetch{release: make(chan struct{}), img: img}
	s, _ := newTestService(t, f)
	community, webhook := id.New(), id.New()
	start := time.Now()
	if got := s.Resolve(context.Background(), community, webhook, "https://example.com/a.png", 50*time.Millisecond); got != nil {
		t.Fatalf("a slow fetch answered %x", got)
	}
	if time.Since(start) > time.Second {
		t.Fatal("Resolve waited past its bound")
	}
	close(f.release)
	s.Wait()
	sum := sha256.Sum256(img)
	if got := s.Resolve(context.Background(), community, webhook, "https://example.com/a.png", 0); !bytes.Equal(got, sum[:]) {
		t.Fatalf("the next post's avatar = %x", got)
	}
}

func TestCacheCommunityCap(t *testing.T) {
	s, clk := newTestService(t, &countingFetch{})
	community := id.New()
	s.mu.Lock()
	chunk := bytes.Repeat([]byte{1}, CommunityBytes/2)
	for i := range 3 {
		clk.Advance(time.Second)
		b := append([]byte{byte(i)}, chunk...)
		s.storeLocked(community, sha256.Sum256(b), b, clk.Now())
	}
	used, n := s.perComm[community], len(s.images[community])
	s.mu.Unlock()
	if used > CommunityBytes || n != 1 {
		t.Fatalf("community holds %d bytes in %d images, want at most %d", used, n, CommunityBytes)
	}
}
