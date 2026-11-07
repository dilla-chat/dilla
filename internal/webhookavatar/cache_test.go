package webhookavatar

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"strconv"
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
	used, n := s.comm[community].bytes, s.comm[community].order.Len()
	s.mu.Unlock()
	if used > CommunityBytes || n != 1 {
		t.Fatalf("community holds %d bytes in %d images, want at most %d", used, n, CommunityBytes)
	}
}

// Fix round 2 N-3: every image is charged EntryOverhead, the count is capped at MaxImages, and the oldest go first.
func TestCacheChargesOverheadAndCapsTheCount(t *testing.T) {
	s, clk := newTestService(t, &countingFetch{})
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range MaxImages + 10 {
		b := []byte{byte(i), byte(i >> 8), byte(i >> 16)}
		s.storeLocked(id.New(), sha256.Sum256(b), b, clk.Now())
	}
	if len(s.images) != MaxImages || s.imgOrder.Len() != MaxImages {
		t.Fatalf("%d images (%d in order), want MaxImages %d", len(s.images), s.imgOrder.Len(), MaxImages)
	}
	if s.total != MaxImages*(3+EntryOverhead) {
		t.Fatalf("total %d bytes, want each image charged its PNG plus %d", s.total, EntryOverhead)
	}
	if len(s.comm) != MaxImages {
		t.Fatalf("%d community lists, want one per cached image's community", len(s.comm))
	}
}

// Fix round 2 N-3: expired images and URL outcomes are swept, not only skipped.
func TestCacheSweepsExpiredEntries(t *testing.T) {
	img := pngOf(t, 4, 4)
	f := &countingFetch{answer: map[string][]byte{"https://example.com/a.png": img}}
	s, clk := newTestService(t, f)
	community := id.New()
	s.Resolve(context.Background(), community, id.New(), "https://example.com/a.png", time.Second)
	s.Resolve(context.Background(), community, id.New(), "https://example.com/missing.png", time.Second)
	clk.Advance(PositiveTTL)
	sum := sha256.Sum256(img)
	if _, ok := s.Get(community, sum[:]); ok {
		t.Fatal("an expired image was served")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.images) != 0 || s.total != 0 || len(s.comm) != 0 || len(s.urls) != 0 || s.urlOrder.Len() != 0 {
		t.Fatalf("after expiry: %d images, %d bytes, %d communities, %d urls", len(s.images), s.total, len(s.comm), len(s.urls))
	}
}

// Fix round 2 N-2: a fetch the webhook's own bucket refuses spends nothing of the community's or the instance's, and
// one community cannot use up the instance's fetches.
func TestCacheNarrowBucketsAreAskedFirst(t *testing.T) {
	f := &countingFetch{answer: map[string][]byte{}}
	s, _ := newTestService(t, f)
	community, webhook := id.New(), id.New()
	for i := range 20 {
		s.Resolve(context.Background(), community, webhook, "https://example.com/"+strconv.Itoa(i)+".png", 0)
	}
	s.Wait()
	if f.count() != 2 {
		t.Fatalf("%d fetches for one webhook, want its burst of 2", f.count())
	}
	if global := s.o.Limiter.Tokens(server.WebhookAvatarGlobalClass, "instance"); global < float64(server.WebhookAvatarGlobalClass.Burst)-2 {
		t.Fatalf("the instance bucket holds %.1f tokens, want all but the 2 spent", global)
	}
	// Many webhooks of one community stop at the community's burst.
	for i := range 20 {
		s.Resolve(context.Background(), community, id.New(), "https://example.com/c"+strconv.Itoa(i)+".png", 0)
	}
	s.Wait()
	if want := server.WebhookAvatarCommunityClass.Burst; f.count() != want {
		t.Fatalf("%d fetches for one community, want its burst of %d", f.count(), want)
	}
	// Another community still fetches.
	s.Resolve(context.Background(), id.New(), id.New(), "https://example.com/other.png", 0)
	s.Wait()
	if f.count() != server.WebhookAvatarCommunityClass.Burst+1 {
		t.Fatalf("another community's fetch was refused (%d fetches)", f.count())
	}
}
