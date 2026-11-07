package webhookavatar

import (
	"context"
	"crypto/sha256"
	"net/netip"
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
)

const (
	// PositiveTTL is how long a fetched avatar, and the URL that named it, are kept.
	PositiveTTL = 24 * time.Hour
	// NegativeTTL is how long a failed fetch is remembered: a hostile URL is fetched at most once in that time.
	NegativeTTL = 10 * time.Minute
	// CommunityBytes bounds one community's cached images; the oldest go first.
	CommunityBytes = 4 << 20
	// TotalBytes bounds every community's cached images together; the oldest go first.
	TotalBytes = 128 << 20
	// MaxURLs bounds the remembered URLs, positive and negative.
	MaxURLs = 65536
)

// Fetch is what the cache asks of a fetcher; *Fetcher is one.
type Fetch interface {
	Fetch(ctx context.Context, raw string) ([]byte, error)
}

// Options configures a Service.
type Options struct {
	Clock clock.Clock
	// Limiter meters fetches per webhook (server.WebhookAvatarClass) and for the instance
	// (server.WebhookAvatarGlobalClass). Nil fetches nothing.
	Limiter *server.RateLimiter
	// Fetcher fetches and processes; nil is NewFetcher with the system resolver and Self.
	Fetcher Fetch
	// Self is the instance's own addresses, refused as fetch targets (used when Fetcher is nil).
	Self []netip.Addr
}

type urlKey struct {
	community id.ID
	url       string
}

// urlEntry is one URL's outcome in one community: a fetch in flight (done open), a cached image (ok, hash), or a
// recent failure (!ok).
type urlEntry struct {
	done    chan struct{}
	ok      bool
	hash    [32]byte
	expires time.Time
}

type imageEntry struct {
	png     []byte
	added   time.Time
	expires time.Time
}

// Service is the avatar cache: images content-addressed by SHA-256 of the re-encoded PNG, scoped to the community
// whose webhook named them, kept PositiveTTL; failures kept NegativeTTL. It lives in memory: a restart forgets it,
// and the next post with the URL fetches again.
type Service struct {
	o Options

	mu      sync.Mutex
	urls    map[urlKey]*urlEntry
	images  map[id.ID]map[[32]byte]*imageEntry
	perComm map[id.ID]int
	total   int
	wg      sync.WaitGroup
}

// New builds a Service.
func New(o Options) *Service {
	if o.Fetcher == nil {
		o.Fetcher = NewFetcher(FetchOptions{Self: o.Self})
	}
	return &Service{o: o, urls: map[urlKey]*urlEntry{}, images: map[id.ID]map[[32]byte]*imageEntry{}, perComm: map[id.ID]int{}}
}

// Resolve is the avatar id for raw in community, or nil. A cached image answers at once; a URL that failed within
// NegativeTTL answers nil at once; otherwise a fetch starts (when the webhook's and the instance's fetch buckets
// allow it) and Resolve waits for it at most wait. A fetch still running then finishes in the background and its
// image serves the webhook's next post. Resolve never fails: a missing avatar is nil.
func (s *Service) Resolve(ctx context.Context, community, webhook id.ID, raw string, wait time.Duration) []byte {
	if _, err := CheckURL(raw); err != nil {
		return nil
	}
	key := urlKey{community, raw}
	now := s.o.Clock.Now()
	s.mu.Lock()
	e := s.urls[key]
	start := e == nil
	if e != nil && e.done == nil { // finished
		if now.Before(e.expires) {
			if !e.ok {
				s.mu.Unlock()
				return nil // failed recently: not fetched again before NegativeTTL
			}
			if hash, ok := s.hitLocked(community, e, now); ok {
				s.mu.Unlock()
				return hash
			}
		}
		start = true // expired, or its image was evicted
	}
	if start {
		if !s.allowLocked(webhook) {
			s.mu.Unlock()
			return nil
		}
		e = &urlEntry{done: make(chan struct{})}
		s.urls[key] = e
		s.trimURLsLocked(now)
		s.wg.Add(1)
		go s.fetch(key, e) //nolint:gosec // G118: detached on purpose: the fetch outlives the POST that started it and has its own 3 s bound
	}
	done := e.done
	s.mu.Unlock()
	if done != nil {
		t := time.NewTimer(wait)
		select {
		case <-done:
		case <-t.C:
		case <-ctx.Done():
		}
		t.Stop()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.done != nil {
		return nil
	}
	hash, _ := s.hitLocked(community, e, s.o.Clock.Now())
	return hash
}

// hitLocked is e's avatar id when e is a success whose image is still cached and fresh.
func (s *Service) hitLocked(community id.ID, e *urlEntry, now time.Time) ([]byte, bool) {
	if !e.ok {
		return nil, false
	}
	img := s.images[community][e.hash]
	if img == nil || !now.Before(img.expires) {
		return nil, false
	}
	out := e.hash
	return out[:], true
}

func (s *Service) allowLocked(webhook id.ID) bool {
	if s.o.Limiter == nil {
		return false
	}
	if ok, _ := s.o.Limiter.Allow(server.WebhookAvatarGlobalClass, "instance"); !ok {
		return false
	}
	ok, _ := s.o.Limiter.Allow(server.WebhookAvatarClass, webhook.String())
	return ok
}

// fetch runs one fetch, detached from the POST that started it, and records its outcome.
func (s *Service) fetch(key urlKey, e *urlEntry) {
	defer s.wg.Done()
	ctx, cancel := context.WithTimeout(context.Background(), FetchTimeout)
	defer cancel()
	png, err := s.o.Fetcher.Fetch(ctx, key.url)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.o.Clock.Now()
	done := e.done
	e.done = nil
	defer close(done)
	if err != nil || len(png) > CommunityBytes {
		e.ok, e.expires = false, now.Add(NegativeTTL)
		return
	}
	e.hash = sha256.Sum256(png)
	e.ok, e.expires = true, now.Add(PositiveTTL)
	s.storeLocked(key.community, e.hash, png, now)
}

func (s *Service) storeLocked(community id.ID, hash [32]byte, png []byte, now time.Time) {
	if old := s.images[community][hash]; old != nil {
		old.expires = now.Add(PositiveTTL)
		return
	}
	for s.perComm[community]+len(png) > CommunityBytes && s.evictOldestLocked(&community) {
	}
	for s.total+len(png) > TotalBytes && s.evictOldestLocked(nil) {
	}
	// Read the map after evicting: an eviction that emptied it removed it.
	byHash := s.images[community]
	if byHash == nil {
		byHash = map[[32]byte]*imageEntry{}
		s.images[community] = byHash
	}
	byHash[hash] = &imageEntry{png: png, added: now, expires: now.Add(PositiveTTL)}
	s.perComm[community] += len(png)
	s.total += len(png)
}

// evictOldestLocked drops the oldest image of community (any community when nil), and reports whether one went.
func (s *Service) evictOldestLocked(community *id.ID) bool {
	var oc id.ID
	var oh [32]byte
	var oldest *imageEntry
	for c, byHash := range s.images {
		if community != nil && c != *community {
			continue
		}
		for h, img := range byHash {
			if oldest == nil || img.added.Before(oldest.added) {
				oc, oh, oldest = c, h, img
			}
		}
	}
	if oldest == nil {
		return false
	}
	delete(s.images[oc], oh)
	if len(s.images[oc]) == 0 {
		delete(s.images, oc)
	}
	if s.perComm[oc] -= len(oldest.png); s.perComm[oc] <= 0 {
		delete(s.perComm, oc)
	}
	s.total -= len(oldest.png)
	return true
}

// trimURLsLocked keeps the URL table under MaxURLs: expired finished entries go first, then any finished one.
func (s *Service) trimURLsLocked(now time.Time) {
	if len(s.urls) <= MaxURLs {
		return
	}
	for k, e := range s.urls {
		if e.done == nil && !now.Before(e.expires) {
			delete(s.urls, k)
		}
	}
	for k, e := range s.urls {
		if len(s.urls) <= MaxURLs {
			return
		}
		if e.done == nil {
			delete(s.urls, k)
		}
	}
}

// Get is the cached PNG with this avatar id in community, while it is fresh.
func (s *Service) Get(community id.ID, avatarID []byte) ([]byte, bool) {
	if len(avatarID) != sha256.Size {
		return nil, false
	}
	var h [32]byte
	copy(h[:], avatarID)
	s.mu.Lock()
	defer s.mu.Unlock()
	img := s.images[community][h]
	if img == nil || !s.o.Clock.Now().Before(img.expires) {
		return nil, false
	}
	return img.png, true
}

// Wait waits for the fetches in flight (tests and shutdown).
func (s *Service) Wait() { s.wg.Wait() }
