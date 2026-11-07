package youtube

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	musicruntime "github.com/dilla-chat/dilla/internal/bot/music/runtime"
)

// testOgg renders seconds of 48 kHz stereo Ogg Opus in 20 ms frames with the real ffmpeg.
func testOgg(t *testing.T, seconds float64) []byte {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	out, err := exec.CommandContext(context.Background(), "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration="+strconv.FormatFloat(seconds, 'f', 3, 64),
		"-map", "0:a:0", "-ar", "48000", "-ac", "2", "-c:a", "libopus", "-application", "audio", "-b:a", "96k", "-vbr", "off", "-dtx", "0",
		"-frame_duration", "20", "-flush_packets", "1", "-page_duration", "20000", "-f", "ogg", "pipe:1").Output()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// fakeStreamFFmpeg is an "ffmpeg" that checks it was given the single-ffmpeg
// argument list and then emits stream. The pause after the first bytes keeps
// the process alive long enough for the process accounting below.
func fakeStreamFFmpeg(t *testing.T, dir string, stream []byte, extra string) string {
	t.Helper()
	encoded := base64.StdEncoding.EncodeToString(stream)
	body := extra + `
case " $* " in *" -f ogg "*) ;; *) exit 33;; esac
case " $* " in *" -c:a libopus "*) ;; *) exit 34;; esac
case " $* " in *" -frame_duration 20 "*) ;; *) exit 35;; esac
case " $* " in *" -ar 48000 "*) ;; *) exit 36;; esac
test "$HTTPS_PROXY" = "http://127.0.0.1:3128" || exit 37
base64 -d > /tmp/stream.opus <<'DATA'
` + encoded + `
DATA
head -c 2000 /tmp/stream.opus
sleep 0.4
tail -c +2001 /tmp/stream.opus`
	return writeScript(t, dir, "ffmpeg", body)
}

type fakeResolver struct {
	calls       atomic.Int32
	invalidated atomic.Int32
	urls        []string
}

func (f *fakeResolver) Resolve(_ context.Context, input string) (Resolved, error) {
	n := int(f.calls.Add(1))
	u := "https://rr1---sn-abc.googlevideo.com/videoplayback?expire=4102444800&n=" + strconv.Itoa(n)
	if len(f.urls) > 0 {
		u = f.urls[min(n-1, len(f.urls)-1)]
	}
	return Resolved{StreamURL: u, Expires: time.Now().Add(time.Hour)}, nil
}
func (f *fakeResolver) Invalidate(string) { f.invalidated.Add(1) }

func streamStarterForTest(t *testing.T, dir, ffmpeg string, resolver Resolver) (*StreamStarter, string) {
	t.Helper()
	bwrap, log := countingBwrap(t, dir)
	node := writeScript(t, dir, "node", `exit 0`)
	starter, err := NewStreamStarter(SandboxConfig{Bubblewrap: bwrap, FFmpeg: ffmpeg, Node: node}, resolver)
	if err != nil {
		t.Skipf("stream starter unavailable: %v", err)
	}
	t.Cleanup(func() { _ = starter.Close() })
	return starter, log
}

// descendantsRunning returns the command lines of every process below this
// test process whose command line mentions marker.
func descendantsRunning(t *testing.T, marker string) []string {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Skip("no /proc")
	}
	parent := map[int]int{}
	cmdline := map[int]string{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// pid (comm) state ppid ...: comm may hold spaces, so split after the last ')'.
		rest := stat[bytes.LastIndexByte(stat, ')')+2:]
		fields := strings.Fields(string(rest))
		if len(fields) < 2 {
			continue
		}
		ppid, _ := strconv.Atoi(fields[1])
		parent[pid] = ppid
		raw, _ := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		// Only a process that IS the program counts: argv[0], or argv[1] behind an interpreter.
		// (bwrap and python carry the program in their arguments too.)
		argv := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		if argv[0] == marker || (len(argv) > 1 && strings.HasSuffix(argv[0], "sh") && argv[1] == marker) {
			cmdline[pid] = strings.Join(argv, " ")
		}
	}
	var found []string
	for pid, line := range cmdline {
		for p := parent[pid]; p > 1; p = parent[p] {
			if p == os.Getpid() {
				found = append(found, line)
				break
			}
		}
	}
	sort.Strings(found)
	return found
}

func TestOneFfmpegFromStreamToOpus(t *testing.T) {
	dir := t.TempDir()
	stream := testOgg(t, 1.0)
	ffmpeg := fakeStreamFFmpeg(t, dir, stream, "")
	starter, log := streamStarterForTest(t, dir, ffmpeg, &fakeResolver{})
	// The Go side must never start a second ffmpeg: point the encoder at one that records its use.
	marker := filepath.Join(dir, "second-ffmpeg-ran")
	second := writeScript(t, dir, "second-ffmpeg", `touch "`+marker+`"; exit 1`)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	began := time.Now()
	source, err := starter.Start(ctx, "https://www.youtube.com/watch?v=aqz-KE-bpKQ")
	if err != nil {
		if errors.Is(err, ErrSandboxUnavailable) {
			t.Skipf("bubblewrap network namespaces unavailable: %v", err)
		}
		t.Fatal(err)
	}
	t.Logf("start returned after %v", time.Since(began))
	if marked, ok := source.(interface{ OggOpus() bool }); !ok || !marked.OggOpus() {
		t.Fatal("the stream is not marked Ogg Opus, so the session would run a second encoder over it")
	}
	session, err := musicruntime.StartOgg(ctx, source, 100, musicruntime.Options{FFmpegPath: second})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.Provider().NextSample(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("first sample after %v, stream %d bytes", time.Since(began), len(stream))
	if running := descendantsRunning(t, filepath.Join(dir, "ffmpeg")); len(running) != 1 {
		t.Fatalf("%d ffmpeg processes are running below the Bot while a track streams, want exactly 1: %q", len(running), running)
	}
	samples := 1
	for {
		_, err := session.Provider().NextSample(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		samples++
	}
	if samples < 45 || samples > 55 {
		t.Fatalf("got %d 20 ms packets for a 1 s track", samples)
	}
	if err := session.Wait(); err != nil {
		t.Fatalf("stream ended with %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a second ffmpeg ran in the Bot after the sandboxed one")
	}
	if n := launches(t, log); n != 1 {
		t.Fatalf("one track launched %d sandboxes, want 1", n)
	}
}

func TestStreamStarterPrefetchIsClaimedByTheNextStart(t *testing.T) {
	dir := t.TempDir()
	stream := testOgg(t, 0.5)
	ffmpeg := fakeStreamFFmpeg(t, dir, stream, "")
	resolver := &fakeResolver{}
	starter, log := streamStarterForTest(t, dir, ffmpeg, resolver)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	const video = "https://www.youtube.com/watch?v=aqz-KE-bpKQ"
	if err := starter.Prefetch(ctx, video); err != nil {
		if errors.Is(err, ErrSandboxUnavailable) {
			t.Skipf("bubblewrap network namespaces unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if err := starter.Prefetch(ctx, video); err != nil {
		t.Fatal(err)
	}
	source, err := starter.Start(ctx, video)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	got, err := io.ReadAll(source)
	if err != nil || !bytes.Equal(got, stream) {
		t.Fatalf("pre-buffered stream differs from the track: %d bytes, %v", len(got), err)
	}
	if resolver.calls.Load() != 1 || launches(t, log) != 1 {
		t.Fatalf("prefetch+start resolved %d times and launched %d sandboxes, want 1 and 1", resolver.calls.Load(), launches(t, log))
	}
}

func TestStreamStarterRetriesOnceWithAFreshLookupWhenTheURLIsRefused(t *testing.T) {
	dir := t.TempDir()
	stream := testOgg(t, 0.5)
	ffmpeg := fakeStreamFFmpeg(t, dir, stream, `case "$*" in *expired=1*) exit 1;; esac`)
	resolver := &fakeResolver{urls: []string{
		"https://rr1---sn-abc.googlevideo.com/videoplayback?expired=1",
		"https://rr1---sn-abc.googlevideo.com/videoplayback?fresh=1",
	}}
	starter, _ := streamStarterForTest(t, dir, ffmpeg, resolver)
	source, err := starter.Start(context.Background(), "https://www.youtube.com/watch?v=aqz-KE-bpKQ")
	if err != nil {
		if errors.Is(err, ErrSandboxUnavailable) {
			t.Skipf("bubblewrap network namespaces unavailable: %v", err)
		}
		t.Fatal(err)
	}
	defer source.Close()
	if got, err := io.ReadAll(source); err != nil || !bytes.Equal(got, stream) {
		t.Fatalf("after the retry: %d bytes, %v", len(got), err)
	}
	if resolver.calls.Load() != 2 || resolver.invalidated.Load() != 1 {
		t.Fatalf("resolves=%d invalidations=%d, want 2 and 1", resolver.calls.Load(), resolver.invalidated.Load())
	}
}

func TestStreamStarterRefusesStreamsTheSandboxMustNotFetch(t *testing.T) {
	dir := t.TempDir()
	ffmpeg := writeScript(t, dir, "ffmpeg", `exit 0`)
	for _, u := range []string{
		"http://rr1---sn-abc.googlevideo.com/videoplayback", "https://evil.example/a", "file:///etc/passwd",
		"https://rr1---sn-abc.googlevideo.com:8443/x", "https://user@rr1---sn-abc.googlevideo.com/x", "https://rr1---sn-abc.googlevideo.com/x y",
		"-i", "concat:/etc/passwd", "",
	} {
		starter, _ := streamStarterForTest(t, dir, ffmpeg, &fakeResolver{urls: []string{u}})
		if _, err := starter.Start(context.Background(), "https://www.youtube.com/watch?v=aqz-KE-bpKQ"); !errors.Is(err, ErrStreamURL) {
			t.Fatalf("stream URL %q = %v, want ErrStreamURL", u, err)
		}
	}
	starter, _ := streamStarterForTest(t, dir, ffmpeg, &fakeResolver{})
	for _, in := range []string{"some search text", "https://www.youtube.com/playlist?list=PLabc", "youtu.be/aqz-KE-bpKQ"} {
		if _, err := starter.Start(context.Background(), in); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("Start(%q) = %v, want ErrInvalidInput (only a canonical video URL)", in, err)
		}
	}
}

func TestFfmpegStreamArgsSeekBeforeInputAndNeverUseShellOrFilters(t *testing.T) {
	args := ffmpegStreamArgs("https://rr1---sn-abc.googlevideo.com/videoplayback?x=1", 12500, true)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-ss 12.500 -i https://rr1---sn-abc.googlevideo.com/videoplayback?x=1 ") {
		t.Fatalf("seek must precede -i: %s", joined)
	}
	for _, want := range []string{"-http_proxy http://127.0.0.1:3128", "-c:a libopus", "-frame_duration 20", "-ar 48000", "-f ogg pipe:1", "-vn"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %s", want, joined)
		}
	}
	if strings.Contains(strings.Join(ffmpegStreamArgs("/x/a.opus", 0, false), " "), "-reconnect") || strings.Contains(joined, "-protocol_whitelist") {
		t.Fatal("unexpected option")
	}
}

// Cached resolve plus a warm sandbox: the third /play of the same song.
func TestRepeatedTrackStartsUnder500msP95(t *testing.T) {
	dir := t.TempDir()
	stream := testOgg(t, 0.5)
	ffmpeg := fakeStreamFFmpeg(t, dir, stream, "")
	lookup := newFakeLookup()
	warm, _, _, _ := testWarm(t, lookup)
	starter, _ := streamStarterForTest(t, dir, ffmpeg, warm)
	const video = "https://www.youtube.com/watch?v=aqz-KE-bpKQ"
	if _, err := warm.Resolve(context.Background(), video); err != nil { // the cold lookup of the first play
		t.Fatal(err)
	}
	var durations []time.Duration
	for i := 0; i < 20; i++ {
		began := time.Now()
		source, err := starter.Start(context.Background(), video)
		if err != nil {
			if errors.Is(err, ErrSandboxUnavailable) {
				t.Skipf("bubblewrap network namespaces unavailable: %v", err)
			}
			t.Fatal(err)
		}
		var head [4]byte
		if _, err := io.ReadFull(source, head[:]); err != nil || string(head[:]) != "OggS" {
			t.Fatalf("first bytes %q, %v", head[:], err)
		}
		durations = append(durations, time.Since(began))
		_ = source.Close()
	}
	if lookup.calls.Load() != 1 {
		t.Fatalf("twenty repeated starts ran yt-dlp %d times, want only the first lookup", lookup.calls.Load())
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	if p95 := durations[18]; p95 > 500*time.Millisecond {
		t.Fatalf("repeated track p95 = %v, want <= 500ms (%v)", p95, durations)
	}
}
