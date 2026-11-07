package youtube

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fakePrintJSON = `{"id":"aqz-KE-bpKQ","title":"Big Buck Bunny","uploader":"Blender","duration":596,"availability":"public","live_status":"not_live","is_live":false,"extractor_key":"Youtube","url":"https://rr1---sn-abc.googlevideo.com/videoplayback?expire=4102444800&id=o-xyz"}`

// countingBwrap logs one line per sandbox launch, then runs the real bubblewrap
// with exactly the arguments it was given.
func countingBwrap(t *testing.T, dir string) (script, log string) {
	t.Helper()
	bwrapPath, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("bubblewrap is not installed")
	}
	log = filepath.Join(dir, "bwrap.log")
	script = writeScript(t, dir, "bwrap", `echo launch >> "`+log+`"; exec `+bwrapPath+` "$@"`)
	return script, log
}

func launches(t *testing.T, log string) int {
	t.Helper()
	data, err := os.ReadFile(log)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "launch")
}

func warmForTest(t *testing.T, dir, ytdlpBody string, timeout time.Duration) (*WarmResolver, string) {
	t.Helper()
	bwrap, log := countingBwrap(t, dir)
	ytdlp := writeScript(t, dir, "yt-dlp", ytdlpBody)
	node := writeScript(t, dir, "node", `exit 0`)
	r, err := NewWarmResolver(SandboxConfig{Bubblewrap: bwrap, YTDLP: ytdlp, Node: node, FFmpeg: "ffmpeg", LookupTimeout: timeout}, 6*time.Hour)
	if err != nil {
		t.Skipf("warm resolver unavailable: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r, log
}

func TestWarmWorkerServesManyLookupsFromOneSandbox(t *testing.T) {
	dir := t.TempDir()
	r, log := warmForTest(t, dir, `test "$HTTPS_PROXY" = "http://127.0.0.1:3128" || exit 31
for a; do last="$a"; done
printf '%s\n' '`+fakePrintJSON+`'`, 10*time.Second)
	for _, query := range []string{"first song", "second song", "third song"} {
		got, err := r.Resolve(context.Background(), query)
		if errors.Is(err, ErrSandboxUnavailable) {
			t.Skipf("bubblewrap network namespaces unavailable: %v", err)
		}
		if err != nil || got.Track.CanonicalID != "aqz-KE-bpKQ" {
			t.Fatalf("%q = %+v, %v", query, got, err)
		}
		r.Invalidate(query) // each lookup must reach the worker, not the cache
		r.Invalidate("https://www.youtube.com/watch?v=aqz-KE-bpKQ")
	}
	if n := launches(t, log); n != 1 {
		t.Fatalf("three lookups launched %d sandboxes, want 1 warm worker", n)
	}
}

func TestWarmWorkerHungYtDlpIsKilledAndTheWorkerRestarts(t *testing.T) {
	dir := t.TempDir()
	r, log := warmForTest(t, dir, `for a; do last="$a"; done
case "$last" in *hang*) sleep 30;; esac
printf '%s\n' '`+fakePrintJSON+`'`, 400*time.Millisecond)
	if _, err := r.Resolve(context.Background(), "warm up"); err != nil {
		if errors.Is(err, ErrSandboxUnavailable) {
			t.Skipf("bubblewrap network namespaces unavailable: %v", err)
		}
		t.Fatal(err)
	}
	began := time.Now()
	if _, err := r.Resolve(context.Background(), "please hang"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hung lookup = %v, want the deadline", err)
	}
	if time.Since(began) > 5*time.Second {
		t.Fatal("the hung lookup wedged the resolver")
	}
	if _, err := r.Resolve(context.Background(), "healthy again"); err != nil {
		t.Fatalf("lookup after the kill: %v", err)
	}
	if n := launches(t, log); n != 2 {
		t.Fatalf("sandbox launches = %d, want 2 (initial and one restart)", n)
	}
}

func TestWarmWorkerSandboxArgvMatchesTheMetadataWorker(t *testing.T) {
	dir := t.TempDir()
	ytdlp := writeScript(t, dir, "yt-dlp", `exit 0`)
	node := writeScript(t, dir, "node", `exit 0`)
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	tools := []string{ytdlp, node}
	socket := filepath.Join(dir, "e.sock")
	// The per-request metadata worker (exec mode) and the long-lived worker
	// (serve mode) must differ only in the worker's own arguments.
	perRequest, _, err := sandboxCommand(context.Background(), "/usr/bin/bwrap", python, tools, []string{"exec", "1024", ytdlp, "--version"}, socket)
	if err != nil {
		t.Skipf("sandbox command unavailable: %v", err)
	}
	warm, _, err := sandboxCommand(context.Background(), "/usr/bin/bwrap", python, tools, serveWorkerArgs(ytdlp, 1024), socket)
	if err != nil {
		t.Fatal(err)
	}
	before := func(args []string) []string {
		for i, a := range args {
			if a == "-c" {
				return args[:i+2] // up to and including the embedded worker script
			}
		}
		t.Fatal("no -c in sandbox argv")
		return nil
	}
	a, b := before(perRequest.Args), before(warm.Args)
	if strings.Join(a, "\x00") != strings.Join(b, "\x00") {
		t.Fatalf("sandbox argv changed:\n per-request %q\n warm        %q", a, b)
	}
}
