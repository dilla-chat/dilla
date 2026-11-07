package youtube

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// funcWorker is a lookupWorker driven by a function.
type funcWorker struct {
	do     func(ctx context.Context, args []string) ([]byte, error)
	used   int64
	calls  atomic.Int32
	closed atomic.Bool
	once   sync.Once
	done   chan struct{}
}

func newFuncWorker(do func(ctx context.Context, args []string) ([]byte, error)) *funcWorker {
	return &funcWorker{do: do, done: make(chan struct{})}
}

func (f *funcWorker) Do(ctx context.Context, args []string, _ int) ([]byte, error) {
	f.calls.Add(1)
	return f.do(ctx, args)
}
func (f *funcWorker) Close() error {
	f.closed.Store(true)
	f.once.Do(func() { close(f.done) })
	return nil
}
func (f *funcWorker) Done() <-chan struct{} { return f.done }
func (f *funcWorker) egressUsed() int64     { return f.used }

func okWorker() *funcWorker {
	return newFuncWorker(func(context.Context, []string) ([]byte, error) {
		return videoReply("aqz-KE-bpKQ", warmBase.Add(7*time.Hour)), nil
	})
}

func TestWarmResolverTimeoutCancelsOnlyItsOwnRequest(t *testing.T) {
	var first atomic.Bool
	w := newFuncWorker(func(ctx context.Context, _ []string) ([]byte, error) {
		if first.CompareAndSwap(false, true) {
			<-ctx.Done() // the worker kills that one yt-dlp and acknowledges: ctx error, no wedge
			return nil, ctx.Err()
		}
		return videoReply("aqz-KE-bpKQ", warmBase.Add(7*time.Hour)), nil
	})
	r, _, _, starts := testWarm(t, w)
	r.timeout = 30 * time.Millisecond
	if _, err := r.Resolve(context.Background(), "hangs"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timed-out lookup = %v", err)
	}
	if w.closed.Load() {
		t.Fatal("a single timed-out lookup killed the worker other communities share")
	}
	if _, err := r.Resolve(context.Background(), "fine"); err != nil {
		t.Fatal(err)
	}
	if *starts != 1 || w.calls.Load() != 2 {
		t.Fatalf("starts=%d calls=%d, want the one worker to serve both", *starts, w.calls.Load())
	}
}

func TestWarmResolverReplacesAWorkerWhoseLookupsKeepFailing(t *testing.T) {
	// What an exhausted egress budget looks like: every tunnel is closed, so every yt-dlp run fails.
	failing := newFuncWorker(func(context.Context, []string) ([]byte, error) {
		return nil, &workerFailure{Code: 1, Stderr: "ERROR: Unable to download webpage: connection reset"}
	})
	healthy := okWorker()
	r, _, _, starts := testWarm(t, failing, healthy)
	r.recycleAfter = 1000
	for i := 0; i < workerMaxFailures; i++ {
		if _, err := r.Resolve(context.Background(), "song "+string(rune('a'+i))); !errors.Is(err, ErrExtractFailed) {
			t.Fatalf("failure %d = %v", i, err)
		}
	}
	if _, err := r.Resolve(context.Background(), "after the cap"); err != nil {
		t.Fatalf("the worker did not recover: %v", err)
	}
	if healthy.calls.Load() != 1 || *starts != 2 {
		t.Fatalf("replacement calls=%d starts=%d, want 1 and 2", healthy.calls.Load(), *starts)
	}
}

func TestWarmResolverKeepsAWorkerWhoseVideosAreSimplyUnavailable(t *testing.T) {
	w := newFuncWorker(func(context.Context, []string) ([]byte, error) {
		return nil, &workerFailure{Code: 1, Stderr: "ERROR: [youtube] xxxxxxxxxxx: Video unavailable"}
	})
	r, _, _, starts := testWarm(t, w)
	for i := 0; i < 2*workerMaxFailures; i++ {
		_, _ = r.Resolve(context.Background(), "gone "+string(rune('a'+i)))
	}
	if *starts != 1 {
		t.Fatalf("unavailable videos replaced a healthy worker %d times", *starts-1)
	}
}

func TestWarmResolverReplacesAWorkerWhoseEgressBudgetIsHalfSpent(t *testing.T) {
	spent := okWorker()
	spent.used = maxProcessBytes/2 + 1
	fresh := okWorker()
	r, _, _, starts := testWarm(t, spent, fresh)
	r.recycleAfter = 1000
	if _, err := r.Resolve(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(context.Background(), "two"); err != nil {
		t.Fatal(err)
	}
	if spent.calls.Load() != 1 || fresh.calls.Load() != 1 || *starts != 2 {
		t.Fatalf("spent=%d fresh=%d starts=%d, want the budget to retire the worker after one lookup", spent.calls.Load(), fresh.calls.Load(), *starts)
	}
}

func TestWarmResolverCountsFailedLookupsTowardRecycling(t *testing.T) {
	w := newFuncWorker(func(context.Context, []string) ([]byte, error) {
		return nil, &workerFailure{Code: 1, Stderr: "ERROR: [youtube] xxxxxxxxxxx: Video unavailable"}
	})
	next := okWorker()
	r, _, _, starts := testWarm(t, w, next)
	r.recycleAfter = 3
	for i := 0; i < 3; i++ {
		_, _ = r.Resolve(context.Background(), "gone "+string(rune('a'+i)))
	}
	if _, err := r.Resolve(context.Background(), "alive"); err != nil || *starts != 2 {
		t.Fatalf("after 3 counted failures: err=%v starts=%d, want a replacement", err, *starts)
	}
}

func TestWarmResolverRedactsTheInputFromWorkerErrors(t *testing.T) {
	w := newFuncWorker(func(context.Context, []string) ([]byte, error) {
		return nil, &workerFailure{Code: 1, Stderr: "ERROR: [youtube] aqz-KE-bpKQ: Video unavailable: https://www.youtube.com/watch?v=aqz-KE-bpKQ and Big Buck Bunny"}
	})
	r, _, _, _ := testWarm(t, w)
	for input, leaks := range map[string][]string{
		warmVideo:        {"aqz-KE-bpKQ", "youtube.com/watch"},
		"Big Buck Bunny": {"Big Buck Bunny"},
	} {
		_, err := r.Resolve(context.Background(), input)
		var process *ProcessError
		if !errors.As(err, &process) {
			t.Fatalf("%q: %v is not a ProcessError", input, err)
		}
		for _, leak := range leaks {
			if strings.Contains(process.Stderr, leak) {
				t.Fatalf("%q: stderr %q leaks %q", input, process.Stderr, leak)
			}
		}
		if FailureClass(err) != "video_unavailable" {
			t.Fatalf("class = %s", FailureClass(err))
		}
	}
}

func TestWarmResolverPartitionsTheCacheByOwner(t *testing.T) {
	w := newFakeLookup()
	r, _, _, _ := testWarm(t, w)
	a, b := WithOwner(context.Background(), "community-a"), WithOwner(context.Background(), "community-b")
	if _, err := r.Resolve(a, warmVideo); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(a, "Big Buck Bunny"); err != nil { // a different query: a lookup
		t.Fatal(err)
	}
	callsAfterA := w.calls.Load()
	if _, err := r.Resolve(a, warmVideo); err != nil || w.calls.Load() != callsAfterA {
		t.Fatalf("community A's repeat ran yt-dlp: %v", err)
	}
	if _, err := r.Resolve(b, warmVideo); err != nil {
		t.Fatal(err)
	}
	if w.calls.Load() != callsAfterA+1 {
		t.Fatalf("community B ran yt-dlp %d times for A's cached video, want its own lookup", w.calls.Load()-callsAfterA)
	}
	r.InvalidateCtx(a, warmVideo)
	callsBefore := w.calls.Load()
	if _, err := r.Resolve(b, warmVideo); err != nil || w.calls.Load() != callsBefore {
		t.Fatal("invalidating A's entry dropped B's")
	}
}

func TestWarmResolverRefusesAnAnswerThatLacksTheRequestedFields(t *testing.T) {
	for name, reply := range map[string]string{
		"no title":    `{"id":"aqz-KE-bpKQ","duration":5,"extractor_key":"Youtube","protocol":"https","url":"https://rr1---sn-abc.googlevideo.com/videoplayback"}`,
		"no url":      `{"id":"aqz-KE-bpKQ","title":"x","duration":5,"extractor_key":"Youtube","protocol":"https"}`,
		"no protocol": `{"id":"aqz-KE-bpKQ","title":"x","duration":5,"extractor_key":"Youtube","url":"https://rr1---sn-abc.googlevideo.com/videoplayback"}`,
		"manifest":    `{"id":"aqz-KE-bpKQ","title":"x","duration":5,"extractor_key":"Youtube","protocol":"m3u8_native","url":"https://rr1---sn-abc.googlevideo.com/videoplayback"}`,
	} {
		w := newFuncWorker(func(context.Context, []string) ([]byte, error) { return []byte(reply + "\n"), nil })
		r, _, _, _ := testWarm(t, w)
		_, err := r.Resolve(context.Background(), warmVideo)
		if !errors.Is(err, ErrMetadataInvalid) {
			t.Fatalf("%s: %v, want ErrMetadataInvalid", name, err)
		}
		if class := FailureClass(err); class != "invalid_metadata:print_keys" && class != "invalid_metadata:protocol" {
			t.Fatalf("%s: class %s", name, class)
		}
	}
}

func TestWarmResolverAsksForProgressiveHTTPSAndNoCacheAndKeepsTheUserAgent(t *testing.T) {
	w := newFakeLookup()
	r, _, _, _ := testWarm(t, w)
	got, err := r.Resolve(context.Background(), warmVideo)
	if err != nil {
		t.Fatal(err)
	}
	if got.UserAgent != testUserAgent {
		t.Fatalf("user agent = %q", got.UserAgent)
	}
	w.mu.Lock()
	args := strings.Join(w.args[0], " ")
	w.mu.Unlock()
	for _, want := range []string{"--no-cache-dir", "--format bestaudio[protocol=https] ", "http_headers", "protocol"} {
		if !strings.Contains(args, want) {
			t.Fatalf("lookup args lack %q: %s", want, args)
		}
	}
	if strings.Contains(args, "/bestaudio") {
		t.Fatalf("a fallback format could hand a manifest to ffmpeg: %s", args)
	}
	// The queue-side metadata runner shares the worker and the no-cache rule.
	_, _ = r.MetadataResolver().Resolve(context.Background(), "https://www.youtube.com/playlist?list=PLabc")
	w.mu.Lock()
	defer w.mu.Unlock()
	if last := w.args[len(w.args)-1]; !containsArg(last, "--no-cache-dir") {
		t.Fatalf("metadata args %v lack --no-cache-dir", last)
	}
}

func TestWarmBackendOnePlayIsOneWorkerRunAndARepeatIsNone(t *testing.T) {
	dir := t.TempDir()
	stream := testOgg(t, 0.5)
	ffmpeg := fakeStreamFFmpeg(t, dir, stream, "")
	w := newFakeLookup()
	warm, _, _, _ := testWarm(t, w)
	streams, _ := streamStarterForTest(t, dir, ffmpeg, warm)
	backend := newBackend(warm, streams)
	a := backend.For("community-a")
	ctx := context.Background()
	play := func(view *OwnerBackend, text string) {
		t.Helper()
		tracks, err := view.Tracks.Resolve(ctx, text)
		if err != nil || len(tracks) != 1 {
			t.Fatalf("%q: %+v, %v", text, tracks, err)
		}
		source, err := view.Streams.Start(ctx, videoURL(tracks[0].CanonicalID))
		if err != nil {
			if errors.Is(err, ErrSandboxUnavailable) {
				t.Skipf("bubblewrap network namespaces unavailable: %v", err)
			}
			t.Fatal(err)
		}
		_ = source.Close()
	}
	play(a, "some text")
	if w.calls.Load() != 1 {
		t.Fatalf("one /play ran the worker %d times, want 1 (metadata and stream URL from one lookup)", w.calls.Load())
	}
	play(a, "some text")
	play(a, "SOME  text")
	if w.calls.Load() != 1 {
		t.Fatalf("repeats ran the worker %d times in total, want still 1", w.calls.Load())
	}
	play(backend.For("community-b"), "some text")
	if w.calls.Load() != 2 {
		t.Fatalf("community B's first play ran the worker %d times in total, want its own lookup (2)", w.calls.Load())
	}
}

func TestPrefetchIsPartitionedByOwnerAndOnePerOwner(t *testing.T) {
	dir := t.TempDir()
	ffmpeg := fakeStreamFFmpeg(t, dir, testOgg(t, 0.5), "")
	resolver := &fakeResolver{}
	starter, log := streamStarterForTest(t, dir, ffmpeg, resolver)
	a, b := WithOwner(context.Background(), "a"), WithOwner(context.Background(), "b")
	const video = "https://www.youtube.com/watch?v=aqz-KE-bpKQ"
	const other = "https://www.youtube.com/watch?v=bcdefghijkl"
	if err := starter.Prefetch(a, video); err != nil {
		if errors.Is(err, ErrSandboxUnavailable) {
			t.Skipf("bubblewrap network namespaces unavailable: %v", err)
		}
		t.Fatal(err)
	}
	source, err := starter.Start(b, video)
	if err != nil {
		t.Fatal(err)
	}
	_ = source.Close()
	if n := launches(t, log); n != 2 {
		t.Fatalf("community B's start launched %d sandboxes in total, want its own (2): it must not claim A's buffer", n)
	}
	claimed, err := starter.Start(a, video)
	if err != nil {
		t.Fatal(err)
	}
	_ = claimed.Close()
	if n := launches(t, log); n != 2 {
		t.Fatalf("community A's start launched a sandbox (%d in total) instead of claiming its own buffer", n)
	}
	// One held stream per owner.
	if err := starter.Prefetch(a, video); err != nil {
		t.Fatal(err)
	}
	if err := starter.Prefetch(a, other); err != nil {
		t.Fatal(err)
	}
	if err := starter.Prefetch(b, video); err != nil {
		t.Fatal(err)
	}
	starter.mu.Lock()
	held := len(starter.prefetched)
	_, aVideo := starter.prefetched[prefetchKey("a", video)]
	_, aOther := starter.prefetched[prefetchKey("a", other)]
	starter.mu.Unlock()
	if held != 2 || aVideo || !aOther {
		t.Fatalf("held=%d aVideo=%v aOther=%v, want A's newest and B's only", held, aVideo, aOther)
	}
}

func TestStreamStarterLooksAgainWhenTheCachedURLWillNotOutliveTheTrack(t *testing.T) {
	dir := t.TempDir()
	ffmpeg := fakeStreamFFmpeg(t, dir, testOgg(t, 0.5), "")
	resolver := &expiringResolver{}
	starter, _ := streamStarterForTest(t, dir, ffmpeg, resolver)
	source, err := starter.Start(context.Background(), "https://www.youtube.com/watch?v=aqz-KE-bpKQ")
	if err != nil {
		if errors.Is(err, ErrSandboxUnavailable) {
			t.Skipf("bubblewrap network namespaces unavailable: %v", err)
		}
		t.Fatal(err)
	}
	_ = source.Close()
	if resolver.calls != 2 || resolver.invalidated != 1 {
		t.Fatalf("resolves=%d invalidations=%d, want a second lookup for a 10 minute track with 30 s of URL left", resolver.calls, resolver.invalidated)
	}
}

type expiringResolver struct{ calls, invalidated int }

func (e *expiringResolver) Resolve(context.Context, string) (Resolved, error) {
	e.calls++
	expires := time.Now().Add(30 * time.Second)
	if e.calls > 1 {
		expires = time.Now().Add(6 * time.Hour)
	}
	res := Resolved{StreamURL: "https://rr1---sn-abc.googlevideo.com/videoplayback?n=1", Expires: expires, UserAgent: testUserAgent}
	res.Track.DurationMS = 600_000
	return res, nil
}
func (e *expiringResolver) Invalidate(string) { e.invalidated++ }

// ---- real sandbox ----------------------------------------------------------

func TestWarmWorkerCarriesAReplyLargerThanFourTimesTheMetadataLimit(t *testing.T) {
	dir := t.TempDir()
	// 1.5 million NUL bytes: every one JSON-escapes to six, so the reply line is about 9 MB (> 4 x 2 MiB).
	r, _ := warmForTest(t, dir, `head -c 1500000 /dev/zero`, 20*time.Second)
	w, err := r.acquire(context.Background())
	if err != nil {
		if errors.Is(err, ErrSandboxUnavailable) {
			t.Skipf("bubblewrap network namespaces unavailable: %v", err)
		}
		t.Fatal(err)
	}
	out, err := w.Do(context.Background(), []string{"x"}, MaxMetadataBytes)
	if err != nil || len(out) != 1_500_000 || !bytes.Equal(out[:4], []byte{0, 0, 0, 0}) {
		t.Fatalf("large reply: %d bytes, %v", len(out), err)
	}
	select {
	case <-w.Done():
		t.Fatal("the large reply killed the worker")
	default:
	}
}

func TestWarmWorkerReleasesItsSlotWhenYtDlpCannotStart(t *testing.T) {
	dir := t.TempDir()
	r, _ := warmForTest(t, dir, `printf '%s\n' '`+fakePrintJSON+`'`, 10*time.Second)
	w, err := r.acquire(context.Background())
	if err != nil {
		if errors.Is(err, ErrSandboxUnavailable) {
			t.Skipf("bubblewrap network namespaces unavailable: %v", err)
		}
		t.Fatal(err)
	}
	ytdlp := filepath.Join(dir, "yt-dlp")
	if err := os.Chmod(ytdlp, 0o600); err != nil { // Popen now fails with an OSError for every request
		t.Fatal(err)
	}
	for i := 0; i < 3*4; i++ { // more than the worker's 4 slots
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := w.Do(ctx, []string{"x"}, MaxMetadataBytes)
		cancel()
		var failure *workerFailure
		if !errors.As(err, &failure) {
			t.Fatalf("request %d = %v, want the worker's failure reply (a leaked slot would hang here)", i, err)
		}
	}
	if err := os.Chmod(ytdlp, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Do(context.Background(), []string{"x"}, MaxMetadataBytes); err != nil {
		t.Fatalf("worker did not recover: %v", err)
	}
}

var _ = io.EOF
