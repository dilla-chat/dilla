package youtube

import (
	"archive/zip"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeYtDlpLibrary is a yt_dlp package with the entry point the worker uses (main, yt-dlp's own command line). It
// models the state a shared YoutubeDL would carry: a failed lookup poisons the module, so a later lookup in the same
// process fails too (R2-1). Inputs steer it: "fail" fails, "slow" hangs, "huge" prints without end. A lookup without
// "--" before its input is refused (R2-2), and --dump-single-json answers a playlist or a search listing.
const fakeYtDlpLibrary = `import json, os, sys, time
POISONED = []
PRINT = ` + "'''" + fakePrintJSON + "'''" + `
PLAYLIST = '{"_type":"playlist","extractor_key":"YoutubeTab","playlist_count":2,"entries":[{"_type":"url","ie_key":"Youtube","id":"aqz-KE-bpKQ","title":"one","duration":12,"availability":"public"},{"_type":"url","ie_key":"Youtube","id":"BaW_jenozKc","title":"two","duration":1}]}'
SEARCH = '{"_type":"playlist","extractor_key":"Youtube","playlist_count":1,"entries":[{"_type":"url","ie_key":"Youtube","id":"LvH_ft4XR1Q","title":"found","duration":3}]}'

def main(argv):
    if "--" not in argv:
        sys.stderr.write("ERROR: no -- before the input\n")
        raise SystemExit(2)
    split = argv.index("--")
    opts, urls = argv[:split], argv[split + 1:]
    url = urls[0] if urls else ""
    if POISONED:
        sys.stderr.write("ERROR: an earlier lookup's state\n")
        raise SystemExit(1)
    if "fail" in url:
        POISONED.append(1)
        sys.stderr.write("ERROR: [youtube] fail: Video unavailable\n")
        raise SystemExit(1)
    if "slow" in url:
        time.sleep(60)
    if "huge" in url:
        while True:
            sys.stdout.write("x" * 65536)
    if "--dump-single-json" in opts:
        sys.stdout.write((PLAYLIST if "list=" in url else SEARCH) + "\n")
        raise SystemExit(0)
    info = json.loads(PRINT)
    cache = opts[opts.index("--cache-dir") + 1] if "--cache-dir" in opts else "none"
    info["title"] = "library pid=%d ppid=%d cache=%s" % (os.getpid(), os.getppid(), cache)
    sys.stdout.write(json.dumps(info) + "\n")
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
	for name, body := range map[string]string{"yt_dlp/__init__.py": fakeYtDlpLibrary, "__main__.py": "raise SystemExit(3)\n"} {
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

// writeYtDlpVenv writes the shipped layout: a yt-dlp-venv whose bin/yt-dlp is an entry script and whose
// site-packages (for the sandbox's python3) holds the fake library.
func writeYtDlpVenv(t *testing.T, dir string) string {
	t.Helper()
	return writeYtDlpVenvWith(t, dir, map[string]string{"__init__.py": fakeYtDlpLibrary})
}

// writeYtDlpVenvWith writes the venv layout with files (paths relative to the yt_dlp package).
func writeYtDlpVenvWith(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	version, err := exec.CommandContext(t.Context(), "python3", "-I", "-c", `import sys; print("%d.%d" % sys.version_info[:2])`).Output()
	if err != nil {
		t.Skipf("python3: %v", err)
	}
	root := filepath.Join(dir, "yt-dlp-venv")
	pkg := filepath.Join(root, "lib", "python"+strings.TrimSpace(string(version)), "site-packages", "yt_dlp")
	if err := os.MkdirAll(pkg, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	all := map[string]string{
		filepath.Join(root, "pyvenv.cfg"):    "home = /usr/bin\n",
		filepath.Join(root, "bin", "yt-dlp"): "#!/bin/sh\nexit 3\n",
	}
	for rel, body := range files {
		all[filepath.Join(pkg, rel)] = body
	}
	for path, body := range all {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(root, "bin", "yt-dlp")
}

func libraryResolver(t *testing.T, ytdlp func(*testing.T, string) string, timeout time.Duration) (*WarmResolver, string) {
	t.Helper()
	dir := t.TempDir()
	bwrap, log := countingBwrap(t, dir)
	node := writeScript(t, dir, "node", `exit 0`)
	r, err := NewWarmResolver(SandboxConfig{Bubblewrap: bwrap, YTDLP: ytdlp(t, dir), Node: node, FFmpeg: "ffmpeg", LookupTimeout: timeout}, 6*time.Hour)
	if err != nil {
		t.Skipf("warm resolver unavailable: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r, log
}

func resolveTitle(t *testing.T, r *WarmResolver, query string) (string, error) {
	t.Helper()
	got, err := r.Resolve(context.Background(), query)
	if errors.Is(err, ErrSandboxUnavailable) {
		t.Skipf("bubblewrap network namespaces unavailable: %v", err)
	}
	if err != nil {
		return "", err
	}
	return got.Track.Title, nil
}

// Bot v2 Task 19 fix round 3: the worker imports yt-dlp once and runs each lookup in a child forked after the import,
// with the worker's private cache dir: one sandbox, a new process per lookup, the worker as its parent.
func TestWarmWorkerForksEachLookupAfterTheImport(t *testing.T) {
	for name, layout := range map[string]func(*testing.T, string) string{"zipapp": writeYtDlpZipapp, "venv": writeYtDlpVenv} {
		t.Run(name, func(t *testing.T) {
			r, log := libraryResolver(t, layout, 10*time.Second)
			var titles []string
			for _, query := range []string{"first song", "second song"} {
				title, err := resolveTitle(t, r, query)
				if err != nil {
					t.Fatalf("%q: %v", query, err)
				}
				titles = append(titles, title)
			}
			for _, title := range titles {
				if !strings.HasPrefix(title, "library pid=") || !strings.HasSuffix(title, "cache="+workerCacheDir) {
					t.Fatalf("lookup = %q, want the library with the worker's cache dir", title)
				}
			}
			pid := func(title string) string { return strings.Fields(title)[1] }
			parent := func(title string) string { return strings.Fields(title)[2] }
			if pid(titles[0]) == pid(titles[1]) || parent(titles[0]) != parent(titles[1]) {
				t.Fatalf("lookups = %q, want a new child of the same worker each", titles)
			}
			if n := launches(t, log); n != 1 {
				t.Fatalf("sandboxes launched = %d, want 1", n)
			}
		})
	}
}

// R2-1, R2-5: a failed lookup leaves nothing behind for the next one (its YoutubeDL, cookie jar and module state die
// with its child).
func TestAFailedLibraryLookupDoesNotAffectTheNext(t *testing.T) {
	r, _ := libraryResolver(t, writeYtDlpVenv, 10*time.Second)
	if _, err := resolveTitle(t, r, "https://www.youtube.com/watch?v=failfailfai"); err == nil {
		t.Fatal("a failed lookup answered")
	}
	for _, query := range []string{"next song", "another song", "a third song", "a fourth song"} {
		if _, err := resolveTitle(t, r, query); err != nil {
			t.Fatalf("%q after a failed lookup: %v", query, err)
		}
	}
}

// R2-3: a lookup past its timeout is cancelled, its child killed, and the worker serves the next lookup at once.
func TestACancelledLibraryLookupIsKilledAndTheWorkerSurvives(t *testing.T) {
	r, log := libraryResolver(t, writeYtDlpVenv, 2*time.Second)
	began := time.Now()
	if _, err := resolveTitle(t, r, "slow song"); err == nil {
		t.Fatal("a hanging lookup answered")
	}
	if waited := time.Since(began); waited > 10*time.Second {
		t.Fatalf("the hanging lookup was given up after %v", waited)
	}
	began = time.Now()
	if _, err := resolveTitle(t, r, "a quick song"); err != nil {
		t.Fatalf("the lookup after a cancelled one: %v", err)
	}
	if waited := time.Since(began); waited > 5*time.Second {
		t.Fatalf("the next lookup took %v", waited)
	}
	if n := launches(t, log); n != 1 {
		t.Fatalf("sandboxes launched = %d, want the worker kept", n)
	}
}

// R2-3: lookups run beside each other: a hanging one does not hold the next.
func TestLibraryLookupsRunConcurrently(t *testing.T) {
	r, _ := libraryResolver(t, writeYtDlpVenv, 20*time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = r.Resolve(context.Background(), "slow song")
	}()
	time.Sleep(200 * time.Millisecond)
	began := time.Now()
	if _, err := resolveTitle(t, r, "a quick song"); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(began); waited > 5*time.Second {
		t.Fatalf("a lookup beside a hanging one took %v", waited)
	}
	_ = r.Close()
	<-done
}

// An output past the limit is cut while it is read (the child prints without end) and refused.
func TestAnOversizedLibraryOutputIsCutAndRefused(t *testing.T) {
	r, _ := libraryResolver(t, writeYtDlpVenv, 20*time.Second)
	began := time.Now()
	if _, err := resolveTitle(t, r, "huge song"); err == nil {
		t.Fatal("an oversized output answered")
	}
	if waited := time.Since(began); waited > 10*time.Second {
		t.Fatalf("the oversized lookup ended after %v, want it cut as it was read", waited)
	}
	if _, err := resolveTitle(t, r, "a normal song"); err != nil {
		t.Fatalf("the lookup after an oversized one: %v", err)
	}
}

// R2-2: playlists and search go through the metadata runner in library mode, in the shipped venv layout, with "--"
// before the input.
func TestLibraryModeListsPlaylistsAndSearch(t *testing.T) {
	r, _ := libraryResolver(t, writeYtDlpVenv, 10*time.Second)
	meta := r.MetadataResolver()
	playlist, err := meta.Resolve(context.Background(), "https://www.youtube.com/playlist?list=PLabc_123")
	if errors.Is(err, ErrSandboxUnavailable) {
		t.Skipf("bubblewrap network namespaces unavailable: %v", err)
	}
	if err != nil || len(playlist) != 2 || playlist[0].CanonicalID != "aqz-KE-bpKQ" || playlist[1].CanonicalID != "BaW_jenozKc" {
		t.Fatalf("playlist = %+v, %v", playlist, err)
	}
	found, err := meta.Search(context.Background(), "study music")
	if err != nil || len(found) != 1 || found[0].CanonicalID != "LvH_ft4XR1Q" {
		t.Fatalf("search = %+v, %v", found, err)
	}
}
