package webhookavatar

import (
	"container/list"
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
	// MaxImages bounds the cached images in all, whatever their size; the oldest go first.
	MaxImages = 16384
	// EntryOverhead is charged against the byte caps for every image on top of its PNG: the map and list entries and
	// the struct around it, so a flood of tiny images still meets the caps (fix round 2 N-3).
	EntryOverhead = 512
	// MaxURLs bounds the remembered URLs, positive and negative (fix round 3 R2-3: 16384).
	MaxURLs = 16384
	// URLEntryOverhead is charged per remembered URL on top of the URL's own bytes.
	URLEntryOverhead = 256
	// MaxURLTableBytes bounds the remembered URLs in bytes, URL plus URLEntryOverhead each.
	MaxURLTableBytes = 8 << 20
	// sweepBudget is the most expired entries one call removes from each table.
	sweepBudget = 32
)

// Fetch is what the cache asks of a fetcher; *Fetcher is one.
type Fetch interface {
	Fetch(ctx context.Context, raw string) ([]byte, error)
}

// Options configures a Service.
type Options struct {
	Clock clock.Clock
	// Limiter meters fetches per webhook (server.WebhookAvatarClass), per community
	// (server.WebhookAvatarCommunityClass) and for the instance (server.WebhookAvatarGlobalClass). Nil fetches nothing.
	Limiter *server.RateLimiter
	// Fetcher fetches and processes; nil is NewFetcher with the system resolver, Self and SelfDomain.
	Fetcher Fetch
	// Self is the instance's own addresses, refused as fetch targets (used when Fetcher is nil).
	Self []netip.Addr
	// SelfDomain is instance.domain, whose addresses are refused too (used when Fetcher is nil).
	SelfDomain string
}

type urlKey struct {
	community id.ID
	url       string
}

// urlEntry is one URL's outcome in one community: a fetch in flight (done open), a cached image (ok, hash), or a
// recent failure (!ok).
type urlEntry struct {
	key     urlKey
	done    chan struct{}
	ok      bool
	hash    [32]byte
	expires time.Time
	// elem is the entry's place in okURLs or failURLs, in on that list; both are nil while a fetch is in flight.
	elem *list.Element
	in   *list.List
}

type imgKey struct {
	community id.ID
	hash      [32]byte
}

type imageEntry struct {
	key     imgKey
	png     []byte
	expires time.Time
	global  *list.Element // in Service.imgOrder
	local   *list.Element // in its community's list
}

func (e *imageEntry) cost() int { return len(e.png) + EntryOverhead }

// imageOf and urlOf read a list element back; the lists hold nothing else.
func imageOf(el *list.Element) *imageEntry {
	img, _ := el.Value.(*imageEntry)
	return img
}

func (e *urlEntry) cost() int { return len(e.key.url) + URLEntryOverhead }

func urlOf(el *list.Element) *urlEntry {
	e, _ := el.Value.(*urlEntry)
	return e
}

type communityImages struct {
	order *list.List
	bytes int
}

// Service is the avatar cache: images content-addressed by SHA-256 of the re-encoded PNG, scoped to the community
// whose webhook named them, kept PositiveTTL; failures kept NegativeTTL. It lives in memory: a restart forgets it,
// and the next post with the URL fetches again.
//
// Both tables are kept in lists in the order their entries were last stored, so the oldest is always at the front:
// eviction is O(1) per entry, and since every image's expiry is its store time plus one TTL, expired images are
// exactly a prefix of the list and are swept from the front in O(expired).
type Service struct {
	o Options

	mu   sync.Mutex
	urls map[urlKey]*urlEntry
	// okURLs and failURLs hold the finished URL entries, successes and failures apart (fix round 3 R2-3), each in the
	// order it finished: each list has one TTL, so its expired entries are a prefix, and a 24 h success at the front
	// never holds back the sweep of 10-minute failures. urlBytes is the table's charged size.
	okURLs   *list.List
	failURLs *list.List
	urlBytes int
	images   map[imgKey]*imageEntry
	imgOrder *list.List
	comm     map[id.ID]*communityImages
	total    int
	wg       sync.WaitGroup
}

// New builds a Service.
func New(o Options) *Service {
	if o.Fetcher == nil {
		o.Fetcher = NewFetcher(FetchOptions{Self: o.Self, SelfDomain: o.SelfDomain})
	}
	return &Service{o: o, urls: map[urlKey]*urlEntry{}, okURLs: list.New(), failURLs: list.New(), images: map[imgKey]*imageEntry{},
		imgOrder: list.New(), comm: map[id.ID]*communityImages{}}
}

// Resolve is the avatar id for raw in community, or nil. A cached image answers at once; a URL that failed within
// NegativeTTL answers nil at once; otherwise a fetch starts (when the webhook's, the community's and the instance's
// fetch buckets all allow it) and Resolve waits for it at most wait. A fetch still running then finishes in the
// background and its image serves the webhook's next post. Resolve never fails: a missing avatar is nil.
func (s *Service) Resolve(ctx context.Context, community, webhook id.ID, raw string, wait time.Duration) []byte {
	if _, err := CheckURL(raw); err != nil {
		return nil
	}
	key := urlKey{community, raw}
	now := s.o.Clock.Now()
	s.mu.Lock()
	s.sweepLocked(now)
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
		if !s.allowLocked(community, webhook) {
			s.mu.Unlock()
			return nil
		}
		if e == nil {
			e = &urlEntry{key: key}
			s.urls[key] = e
			s.urlBytes += e.cost()
		} else {
			s.detachURLLocked(e)
		}
		e.done = make(chan struct{})
		s.trimURLsLocked()
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
	img := s.images[imgKey{community, e.hash}]
	if img == nil || !now.Before(img.expires) {
		return nil, false
	}
	out := e.hash
	return out[:], true
}

// allowLocked takes one token from the webhook's, the community's and the instance's fetch buckets, or none: all
// three are peeked first (under s.mu, which every caller holds), so a refusal by the narrower bucket never spends the
// wider one's token (fix round 2 N-2). The narrowest is asked first.
func (s *Service) allowLocked(community, webhook id.ID) bool {
	l := s.o.Limiter
	if l == nil {
		return false
	}
	checks := []struct {
		class server.Class
		key   string
	}{
		{server.WebhookAvatarClass, webhook.String()},
		{server.WebhookAvatarCommunityClass, community.String()},
		{server.WebhookAvatarGlobalClass, "instance"},
	}
	for _, c := range checks {
		if ok, _ := l.Peek(c.class, c.key); !ok {
			return false
		}
	}
	for _, c := range checks {
		if ok, _ := l.Allow(c.class, c.key); !ok {
			return false
		}
	}
	return true
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
	if err != nil || len(png)+EntryOverhead > CommunityBytes {
		e.ok, e.expires = false, now.Add(NegativeTTL)
		e.in = s.failURLs
		e.elem = s.failURLs.PushBack(e)
		return
	}
	e.hash = sha256.Sum256(png)
	e.ok, e.expires = true, now.Add(PositiveTTL)
	e.in = s.okURLs
	e.elem = s.okURLs.PushBack(e)
	s.storeLocked(key.community, e.hash, png, now)
}

func (s *Service) storeLocked(community id.ID, hash [32]byte, png []byte, now time.Time) {
	s.sweepLocked(now)
	key := imgKey{community, hash}
	c := s.comm[community]
	if old := s.images[key]; old != nil {
		old.expires = now.Add(PositiveTTL)
		s.imgOrder.MoveToBack(old.global)
		c.order.MoveToBack(old.local)
		return
	}
	img := &imageEntry{key: key, png: png, expires: now.Add(PositiveTTL)}
	for c != nil && c.bytes+img.cost() > CommunityBytes && c.order.Len() > 0 {
		s.removeLocked(imageOf(c.order.Front()))
		c = s.comm[community] // removeLocked drops an emptied community
	}
	for (s.total+img.cost() > TotalBytes || len(s.images) >= MaxImages) && s.imgOrder.Len() > 0 {
		s.removeLocked(imageOf(s.imgOrder.Front()))
	}
	if c = s.comm[community]; c == nil {
		c = &communityImages{order: list.New()}
		s.comm[community] = c
	}
	img.global = s.imgOrder.PushBack(img)
	img.local = c.order.PushBack(img)
	s.images[key] = img
	c.bytes += img.cost()
	s.total += img.cost()
}

func (s *Service) removeLocked(img *imageEntry) {
	delete(s.images, img.key)
	s.imgOrder.Remove(img.global)
	s.total -= img.cost()
	if c := s.comm[img.key.community]; c != nil {
		c.order.Remove(img.local)
		if c.bytes -= img.cost(); c.order.Len() == 0 {
			delete(s.comm, img.key.community)
		}
	}
}

// sweepLocked removes expired entries from the front of the image list and both URL lists, at most sweepBudget from
// each per call. Each list has one TTL and is in finishing order, so its expired entries are exactly a prefix.
func (s *Service) sweepLocked(now time.Time) {
	for i := 0; i < sweepBudget; i++ {
		f := s.imgOrder.Front()
		if f == nil || now.Before(imageOf(f).expires) {
			break
		}
		s.removeLocked(imageOf(f))
	}
	for _, l := range []*list.List{s.failURLs, s.okURLs} {
		for i := 0; i < sweepBudget; i++ {
			f := l.Front()
			if f == nil || now.Before(urlOf(f).expires) {
				break
			}
			s.dropURLLocked(urlOf(f))
		}
	}
}

// detachURLLocked takes a finished entry off its list, for a new fetch; it stays in the table and keeps its charge.
func (s *Service) detachURLLocked(e *urlEntry) {
	if e.in != nil {
		e.in.Remove(e.elem)
		e.in, e.elem = nil, nil
	}
}

// dropURLLocked forgets a finished entry.
func (s *Service) dropURLLocked(e *urlEntry) {
	s.detachURLLocked(e)
	delete(s.urls, e.key)
	s.urlBytes -= e.cost()
}

// trimURLsLocked keeps the URL table at MaxURLs entries and MaxURLTableBytes, dropping the oldest failures first, then the
// oldest successes. A fetch in flight is on neither list and is never dropped; the fetch buckets keep those few.
func (s *Service) trimURLsLocked() {
	for len(s.urls) > MaxURLs || s.urlBytes > MaxURLTableBytes {
		f := s.failURLs.Front()
		if f == nil {
			f = s.okURLs.Front()
		}
		if f == nil {
			return
		}
		s.dropURLLocked(urlOf(f))
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
	now := s.o.Clock.Now()
	s.sweepLocked(now)
	img := s.images[imgKey{community, h}]
	if img == nil || !now.Before(img.expires) {
		return nil, false
	}
	return img.png, true
}

// Start runs the fetcher's background refresh of instance.domain (fix round 3 S3) until stop is called; stop waits for
// it to end. A Service whose fetcher is not a *Fetcher has nothing to run.
func (s *Service) Start() (stop func()) {
	f, ok := s.o.Fetcher.(*Fetcher)
	if !ok {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.RunSelfRefresh(ctx)
	}()
	return func() {
		cancel()
		<-done
	}
}

// Wait waits for the fetches in flight (tests and shutdown).
func (s *Service) Wait() { s.wg.Wait() }
