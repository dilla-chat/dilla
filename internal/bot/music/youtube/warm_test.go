package youtube

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var warmBase = time.Unix(1_800_000_000, 0)

// fakeLookup is a lookupWorker that counts yt-dlp invocations.
type fakeLookup struct {
	calls  atomic.Int32
	closed atomic.Bool
	hang   bool
	gate   chan struct{}
	reply  func(args []string) ([]byte, error)
	once   sync.Once
	done   chan struct{}
	args   [][]string
	mu     sync.Mutex
}

func newFakeLookup() *fakeLookup {
	return &fakeLookup{done: make(chan struct{}), reply: func(args []string) ([]byte, error) { return videoReply("aqz-KE-bpKQ", warmBase.Add(7*time.Hour)), nil }}
}

func videoReply(id string, expire time.Time) []byte {
	return []byte(fmt.Sprintf(`{"id":%q,"title":"Big Buck Bunny","uploader":"Blender","duration":596,"availability":"public","live_status":"not_live","is_live":false,"extractor_key":"Youtube","url":"https://rr1---sn-abc.googlevideo.com/videoplayback?expire=%d&id=o-xyz"}`+"\n", id, expire.Unix()))
}

func (f *fakeLookup) Do(ctx context.Context, args []string, limit int) ([]byte, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.args = append(f.args, append([]string(nil), args...))
	f.mu.Unlock()
	if f.gate != nil {
		<-f.gate
	}
	if f.hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return f.reply(args)
}
func (f *fakeLookup) Close() error {
	f.closed.Store(true)
	f.once.Do(func() { close(f.done) })
	return nil
}
func (f *fakeLookup) Done() <-chan struct{} { return f.done }

type warmClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *warmClock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *warmClock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func testWarm(t *testing.T, workers ...lookupWorker) (*WarmResolver, *warmClock, *[]time.Duration, *int) {
	t.Helper()
	clock := &warmClock{t: warmBase}
	var sleeps []time.Duration
	starts := 0
	next := 0
	r := newWarmResolver(SandboxConfig{YTDLP: "/usr/bin/yt-dlp", Node: "/usr/bin/node", LookupTimeout: 2 * time.Second}, 6*time.Hour, warmDeps{
		start: func(context.Context) (lookupWorker, error) {
			starts++
			if next >= len(workers) {
				return nil, ErrSandboxUnavailable
			}
			w := workers[next]
			next++
			if w == nil {
				return nil, ErrSandboxUnavailable
			}
			return w, nil
		},
		now: clock.now,
		sleep: func(_ context.Context, d time.Duration) error {
			sleeps = append(sleeps, d)
			clock.add(d)
			return nil
		},
	})
	t.Cleanup(func() { _ = r.Close() })
	return r, clock, &sleeps, &starts
}

const warmVideo = "https://www.youtube.com/watch?v=aqz-KE-bpKQ"

func TestWarmResolverRunsOneYtDlpPerLookup(t *testing.T) {
	w := newFakeLookup()
	r, clock, _, _ := testWarm(t, w, newFakeLookup())
	got, err := r.Resolve(context.Background(), warmVideo)
	if err != nil {
		t.Fatal(err)
	}
	if w.calls.Load() != 1 {
		t.Fatalf("first lookup ran yt-dlp %d times, want 1", w.calls.Load())
	}
	if got.Track.CanonicalID != "aqz-KE-bpKQ" || got.Track.DurationMS != 596000 || !strings.HasPrefix(got.StreamURL, "https://rr1---sn-abc.googlevideo.com/videoplayback?") {
		t.Fatalf("resolved %+v", got)
	}
	if !got.Expires.Equal(warmBase.Add(6 * time.Hour)) {
		t.Fatalf("expires %v, want the 6 h cache bound (the URL itself outlives it)", got.Expires)
	}
	clock.add(5 * time.Hour)
	if _, err := r.Resolve(context.Background(), warmVideo); err != nil {
		t.Fatal(err)
	}
	if w.calls.Load() != 1 {
		t.Fatalf("a cached query inside 6 h ran yt-dlp again: %d", w.calls.Load())
	}
	clock.add(time.Hour + time.Second)
	if _, err := r.Resolve(context.Background(), warmVideo); err != nil {
		t.Fatal(err)
	}
	if w.calls.Load() != 2 {
		t.Fatalf("after 6 h the lookup ran yt-dlp %d times in total, want 2", w.calls.Load())
	}
}

func TestWarmResolverCachesByNormalisedQueryAndByURL(t *testing.T) {
	w := newFakeLookup()
	r, _, _, _ := testWarm(t, w)
	if _, err := r.Resolve(context.Background(), "Big  Buck BUNNY"); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"big buck bunny", "BIG BUCK  bunny", warmVideo, "youtu.be/aqz-KE-bpKQ"} {
		if _, err := r.Resolve(context.Background(), input); err != nil {
			t.Fatalf("%q: %v", input, err)
		}
	}
	if w.calls.Load() != 1 {
		t.Fatalf("one search and four cache hits ran yt-dlp %d times, want 1", w.calls.Load())
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !containsArg(w.args[0], "ytsearch1:Big  Buck BUNNY") {
		t.Fatalf("search args = %v, want the first result only (ytsearch1)", w.args[0])
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestWarmResolverSingleFlightsConcurrentLookups(t *testing.T) {
	w := newFakeLookup()
	w.gate = make(chan struct{})
	r, _, _, _ := testWarm(t, w)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.Resolve(context.Background(), warmVideo); err != nil {
				t.Error(err)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(w.gate)
	wg.Wait()
	if w.calls.Load() != 1 {
		t.Fatalf("five concurrent identical lookups ran yt-dlp %d times, want 1", w.calls.Load())
	}
}

func TestWarmResolverKillsAHungWorkerAndRestarts(t *testing.T) {
	hung := newFakeLookup()
	hung.hang = true
	healthy := newFakeLookup()
	r, _, _, starts := testWarm(t, hung, healthy)
	r.timeout = 40 * time.Millisecond
	began := time.Now()
	_, err := r.Resolve(context.Background(), warmVideo)
	if !errors.Is(err, ErrExtractFailed) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hung lookup = %v, want ErrExtractFailed wrapping the deadline", err)
	}
	if time.Since(began) > 2*time.Second {
		t.Fatal("a hung worker wedged the resolve")
	}
	if !hung.closed.Load() {
		t.Fatal("the hung worker was not killed")
	}
	if _, err := r.Resolve(context.Background(), "another song"); err != nil {
		t.Fatalf("lookup after the restart: %v", err)
	}
	if healthy.calls.Load() != 1 || *starts != 2 {
		t.Fatalf("healthy worker calls=%d starts=%d, want 1 and 2", healthy.calls.Load(), *starts)
	}
}

func TestWarmResolverRestartsAnExitedWorkerWithBackoff(t *testing.T) {
	first := newFakeLookup()
	second := newFakeLookup()
	r, _, sleeps, starts := testWarm(t, first, nil, nil, second)
	if _, err := r.Resolve(context.Background(), warmVideo); err != nil {
		t.Fatal(err)
	}
	_ = first.Close() // the worker exits (crash, OOM, bwrap gone)
	if _, err := r.Resolve(context.Background(), "next song"); err != nil {
		t.Fatalf("resolve after the worker exited: %v", err)
	}
	if second.calls.Load() != 1 || *starts != 4 {
		t.Fatalf("second worker calls=%d starts=%d, want 1 and 4 (two failed restarts)", second.calls.Load(), *starts)
	}
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond}
	if len(*sleeps) != len(want) || (*sleeps)[0] != want[0] || (*sleeps)[1] != want[1] {
		t.Fatalf("backoff sleeps = %v, want %v", *sleeps, want)
	}
}

func TestWarmResolverGivesUpAfterBoundedRestarts(t *testing.T) {
	r, _, _, starts := testWarm(t, nil, nil, nil, nil, nil, nil)
	_, err := r.Resolve(context.Background(), warmVideo)
	if !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatalf("unavailable sandbox = %v", err)
	}
	if *starts > 4 {
		t.Fatalf("one resolve tried %d worker starts", *starts)
	}
}

func TestWarmResolverRefusesNonVideoInputAndForeignStreamHosts(t *testing.T) {
	w := newFakeLookup()
	r, _, _, _ := testWarm(t, w)
	for _, input := range []string{"https://www.youtube.com/playlist?list=PLabc", "ytsearch5:x", "file:///etc/passwd", ""} {
		if _, err := r.Resolve(context.Background(), input); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("%q = %v, want ErrInvalidInput", input, err)
		}
	}
	if w.calls.Load() != 0 {
		t.Fatal("a refused input reached yt-dlp")
	}
	w.reply = func([]string) ([]byte, error) {
		return []byte(`{"id":"aqz-KE-bpKQ","title":"x","duration":5,"extractor_key":"Youtube","url":"https://evil.example/a.webm"}` + "\n"), nil
	}
	if _, err := r.Resolve(context.Background(), warmVideo); !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("a stream URL on a foreign host = %v, want ErrMetadataInvalid", err)
	}
}

func TestWarmResolverInvalidateForcesAFreshLookup(t *testing.T) {
	w := newFakeLookup()
	r, _, _, _ := testWarm(t, w)
	if _, err := r.Resolve(context.Background(), warmVideo); err != nil {
		t.Fatal(err)
	}
	r.Invalidate(warmVideo)
	if _, err := r.Resolve(context.Background(), warmVideo); err != nil {
		t.Fatal(err)
	}
	if w.calls.Load() != 2 {
		t.Fatalf("invalidate then resolve ran yt-dlp %d times, want 2", w.calls.Load())
	}
}
