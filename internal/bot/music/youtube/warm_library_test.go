package youtube

import (
	"archive/zip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeYtDlpLibrary is a yt_dlp package with the library surface the worker uses (parse_options, YoutubeDL, the
// output Namespace, DownloadError). Each download reports the worker's pid, how many YoutubeDL it has made and whether
// it was given the worker's cache dir, in the title.
const fakeYtDlpLibrary = `import json, os
from . import utils
MADE = []
PRINT = ` + "'''" + fakePrintJSON + "'''" + `

class _Parsed:
    def __init__(self, opts):
        self.ydl_opts = opts

def parse_options(argv):
    split = argv.index("--")
    return _Parsed({"argv": argv[:split]})

class YoutubeDL:
    def __init__(self, opts):
        MADE.append(1)
        self.opts = opts
        self._out_files = utils.Namespace(out=None, error=None, screen=None, console=None)

    def download(self, urls):
        if any("fail" in u for u in urls):
            self._out_files.error.write("ERROR: [youtube] fail: Video unavailable\n")
            raise utils.DownloadError("unavailable")
        info = json.loads(PRINT)
        argv = self.opts["argv"]
        cache = argv[argv.index("--cache-dir") + 1] if "--cache-dir" in argv else "none"
        info["title"] = "library pid=%d made=%d cache=%s" % (os.getpid(), len(MADE), cache)
        self._out_files.out.write(json.dumps(info) + "\n")
        return 0
`

const fakeYtDlpUtils = `class Namespace:
    def __init__(self, **values):
        self.__dict__.update(values)

class DownloadError(Exception):
    pass
`

// writeYtDlpZipapp writes a zipapp yt-dlp whose package is the fake library above.
func writeYtDlpZipapp(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "yt-dlp")
	file, err := os.Create(path) // #nosec G304 -- a test's temp dir
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(file)
	for name, body := range map[string]string{"yt_dlp/__init__.py": fakeYtDlpLibrary, "yt_dlp/utils.py": fakeYtDlpUtils, "__main__.py": "raise SystemExit(3)\n"} {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// Bot v2 Task 19 fix round 2: the long-lived worker loads yt-dlp as a library from the configured yt-dlp (here its
// zipapp) and keeps one YoutubeDL for its lookups, in one process, with its private cache dir; a lookup failure is that
// lookup's, not the worker's.
func TestWarmWorkerLoadsYtDlpAsALibrary(t *testing.T) {
	dir := t.TempDir()
	bwrap, log := countingBwrap(t, dir)
	node := writeScript(t, dir, "node", `exit 0`)
	r, err := NewWarmResolver(SandboxConfig{Bubblewrap: bwrap, YTDLP: writeYtDlpZipapp(t, dir), Node: node, FFmpeg: "ffmpeg", LookupTimeout: 10 * time.Second}, 6*time.Hour)
	if err != nil {
		t.Skipf("warm resolver unavailable: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	var titles []string
	for _, query := range []string{"first song", "second song"} {
		got, err := r.Resolve(context.Background(), query)
		if errors.Is(err, ErrSandboxUnavailable) {
			t.Skipf("bubblewrap network namespaces unavailable: %v", err)
		}
		if err != nil {
			t.Fatalf("%q: %v", query, err)
		}
		titles = append(titles, got.Track.Title)
		r.invalidate("", query)
		r.invalidate("", "https://www.youtube.com/watch?v=aqz-KE-bpKQ")
	}
	if !strings.HasPrefix(titles[0], "library pid=") || !strings.HasSuffix(titles[0], "made=1 cache="+workerCacheDir) {
		t.Fatalf("first lookup = %q, want the library, one YoutubeDL and the worker's cache dir", titles[0])
	}
	if titles[1] != titles[0] {
		t.Fatalf("second lookup = %q, want the same process and the same YoutubeDL as %q", titles[1], titles[0])
	}
	if _, err := r.Resolve(context.Background(), "https://www.youtube.com/watch?v=failfailfai"); err == nil {
		t.Fatal("a failed library lookup answered")
	}
	if _, err := r.Resolve(context.Background(), "third song"); err != nil {
		t.Fatalf("the worker did not survive a failed lookup: %v", err)
	}
	if n := launches(t, log); n != 1 {
		t.Fatalf("sandboxes launched = %d, want 1", n)
	}
}
