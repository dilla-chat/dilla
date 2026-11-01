package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/music"
)

func TestNormalizeInputAcceptsSearchVideoAndPlaylist(t *testing.T) {
	cases := []struct{ input, kind, canonical string }{
		{"https://youtube.com/shorts/aqz-KE-bpKQ?si=shared", "video", videoURL("aqz-KE-bpKQ")},
		{"https://youtube.com/live/aqz-KE-bpKQ?si=shared&feature=share", "video", videoURL("aqz-KE-bpKQ")},
		{"https://youtube.com/embed/aqz-KE-bpKQ", "video", videoURL("aqz-KE-bpKQ")},
		{"youtube.com/watch?v=aqz-KE-bpKQ", "video", videoURL("aqz-KE-bpKQ")},
		{"youtu.be/aqz-KE-bpKQ?si=shared", "video", videoURL("aqz-KE-bpKQ")},
		{"https://youtube.com/playlist?list=PLabc_123&si=shared", "playlist", "https://www.youtube.com/playlist?list=PLabc_123"},
		{"instrumental study music", "search", "ytsearch5:instrumental study music"},
		{"https://www.youtube.com/watch?v=aqz-KE-bpKQ", "video", "https://www.youtube.com/watch?v=aqz-KE-bpKQ"},
		{"https://youtu.be/aqz-KE-bpKQ", "video", "https://www.youtube.com/watch?v=aqz-KE-bpKQ"},
		{"https://youtube.com/playlist?list=PLabc_123", "playlist", "https://www.youtube.com/playlist?list=PLabc_123"},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			kind, canonical, err := NormalizeInput(tc.input)
			if err != nil || kind != tc.kind || canonical != tc.canonical {
				t.Fatalf("NormalizeInput() = %q, %q, %v", kind, canonical, err)
			}
		})
	}
}

func TestNormalizeInputRejectsUnsafeURLsAndExtractorSyntax(t *testing.T) {
	inputs := []string{
		"file:///etc/passwd", "ftp://youtube.com/a",
		"https://youtube.com.evil.test/watch?v=aqz-KE-bpKQ", "https://user:pass@youtube.com/watch?v=aqz-KE-bpKQ",
		"https://127.0.0.1/watch?v=aqz-KE-bpKQ", "https://youtube.com:443/watch?v=aqz-KE-bpKQ",
		"https://youtube.com/watch?v=aqz-KE-bpKQ&v=aqz-KE-bpKQ", "ytsearchall:cat", "youtube:aqz-KE-bpKQ",
		"https://youtube.com/shorts/aqz-KE-bpKQ?list=PLabc_123", "https://youtube.com/watch?v=aqz-KE-bpKQ#",
		"https://youtube.com/watch?v=aqz-KE-bpKQ&", "example.org song", "javascript:alert(1)",
	}
	for _, input := range inputs {
		if _, _, err := NormalizeInput(input); err == nil {
			t.Errorf("NormalizeInput(%q) accepted unsafe input", input)
		}
	}
}

func TestResolveBuildsBoundedQueueDescriptors(t *testing.T) {
	body := `{"_type":"playlist","extractor_key":"Youtube","playlist_count":2,"entries":[{"_type":"url","ie_key":"Youtube","id":"aqz-KE-bpKQ","title":"one","duration":12.345,"availability":"public"},{"_type":"url","ie_key":"Youtube","id":"BaW_jenozKc","title":"two","duration":1}]}`
	runner := runnerFunc(func(_ context.Context, executable string, args []string, limit int) ([]byte, error) {
		if executable != "/qualified/yt-dlp" || limit != MaxMetadataBytes {
			t.Fatalf("runner executable/limit = %q/%d", executable, limit)
		}
		if len(args) == 0 || args[len(args)-1] != "ytsearch5:study music" {
			t.Fatalf("yt-dlp final argument = %q", args[len(args)-1])
		}
		return []byte(body), nil
	})
	resolver := NonProductionMetadataResolver{YTDLP: "/qualified/yt-dlp", Node: "/qualified/node", runner: runner}
	tracks, err := resolver.Resolve(context.Background(), "study music")
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 2 || tracks[0].ID != "yt:aqz-KE-bpKQ" || tracks[0].Provider != "youtube" || tracks[0].DurationMS != 12345 || tracks[1].DurationMS != 1000 {
		t.Fatalf("resolved descriptors = %#v", tracks)
	}
}

func TestResolveAcceptsYoutubeTabPlaylistMetadataFromQualifiedYTDLP(t *testing.T) {
	// This bounded metadata shape was returned by yt-dlp 2026.10.21 for a
	// public two-item playlist. URLs and signed media data are intentionally omitted.
	body := `{"_type":"playlist","extractor":"youtube:tab","extractor_key":"YoutubeTab","playlist_count":2,"entries":[{"_type":"url","ie_key":"Youtube","id":"0VH1Lim8gL8","title":"Deep Learning State of the Art (2020)","uploader":"Lex Fridman","duration":5261,"availability":null},{"_type":"url","ie_key":"Youtube","id":"O5xeyoRL95U","title":"Deep Learning Basics: Introduction and Overview","uploader":"Lex Fridman","duration":4086,"availability":null}]}`
	resolver := NonProductionMetadataResolver{
		YTDLP: "/qualified/yt-dlp", Node: "/qualified/node",
		runner: runnerFunc(func(_ context.Context, _ string, args []string, _ int) ([]byte, error) {
			if got := args[len(args)-1]; got != "https://www.youtube.com/playlist?list=PLrAXtmErZgOeiKm4sgNOknGvNjby9efdf" {
				t.Fatalf("playlist URL passed to yt-dlp = %q", got)
			}
			return []byte(body), nil
		}),
	}
	tracks, err := resolver.Resolve(context.Background(), "https://www.youtube.com/playlist?list=PLrAXtmErZgOeiKm4sgNOknGvNjby9efdf")
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 2 || tracks[0].CanonicalID != "0VH1Lim8gL8" || tracks[0].Title != "Deep Learning State of the Art (2020)" ||
		tracks[1].CanonicalID != "O5xeyoRL95U" || tracks[1].Title != "Deep Learning Basics: Introduction and Overview" {
		t.Fatalf("resolved playlist entries = %#v, want the two ordered YouTube tracks", tracks)
	}
}

func TestResolvedVideoCanonicalIDFitsDurableQueueAndPlaybackAdapter(t *testing.T) {
	body := `{"_type":"video","extractor_key":"Youtube","id":"aqz-KE-bpKQ","title":"sample","duration":1}`
	resolver := NonProductionMetadataResolver{
		YTDLP: "/qualified/yt-dlp", Node: "/qualified/node",
		runner: runnerFunc(func(context.Context, string, []string, int) ([]byte, error) { return []byte(body), nil }),
	}
	tracks, err := resolver.Resolve(context.Background(), "https://www.youtube.com/watch?v=aqz-KE-bpKQ")
	if err != nil || len(tracks) != 1 {
		t.Fatalf("Resolve() = %#v, %v", tracks, err)
	}
	if tracks[0].CanonicalID != "aqz-KE-bpKQ" {
		t.Fatalf("resolver canonical ID = %q, want raw provider ID for queue and YouTubePCM adapter", tracks[0].CanonicalID)
	}
	queued := tracks[0]
	queued.ID = "queue-item-1"
	queued.RequesterID = "member-1"
	if _, err := music.Apply(music.InitialState(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{queued}}); err != nil {
		t.Fatalf("resolver-derived descriptor rejected by durable queue: %v", err)
	}
}

func TestResolveRequiresSandboxedRunner(t *testing.T) {
	resolver := NonProductionMetadataResolver{YTDLP: "/yt-dlp", Node: "/node"}
	if _, err := resolver.Resolve(context.Background(), "study music"); !errors.Is(err, ErrRunnerRequired) {
		t.Fatalf("Resolve without runner error = %v", err)
	}
	//nolint:staticcheck // This assertion verifies that Resolve rejects a nil context.
	if _, err := resolver.Resolve(nil, "study music"); !errors.Is(err, ErrExtractFailed) {
		t.Fatalf("Resolve with nil context error = %v", err)
	}
}

func TestResolveFailsClosedOnMetadataRunnerError(t *testing.T) {
	resolver := MetadataResolver{
		YTDLP: "/configured/yt-dlp",
		Node:  "/configured/node",
		runner: runnerFunc(func(context.Context, string, []string, int) ([]byte, error) {
			return nil, ErrExtractFailed
		}),
	}
	tracks, err := resolver.Resolve(context.Background(), "study music")
	if !errors.Is(err, ErrExtractFailed) || len(tracks) != 0 {
		t.Fatalf("Resolve after extractor failure = (%#v, %v), want no tracks and ErrExtractFailed", tracks, err)
	}
}

func TestIsolatedMetadataRunnerUsesArgvWithoutShellExpansion(t *testing.T) {
	dir := t.TempDir()
	marker := dir + "/injected"
	program := "#!/bin/sh\nprintf '%s' \"$1\"\n"
	extractor := filepath.Join(dir, "extractor")
	if err := os.WriteFile(extractor, []byte(program), 0700); err != nil {
		t.Fatal(err)
	}
	injected := "; touch " + marker
	runner := IsolatedMetadataRunner{Timeout: 5 * time.Second}
	output, err := runner.Run(context.Background(), extractor, []string{injected}, 4096)
	if err != nil {
		if errors.Is(err, ErrSandboxUnavailable) {
			t.Skipf("bubblewrap network namespaces unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if string(output) != injected {
		t.Fatalf("runner output = %q, want exact argv value %q", output, injected)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("argv was shell-expanded: marker stat error = %v", err)
	}
}

func TestIsolatedMetadataRunnerMountsPythonVenvByConfig(t *testing.T) {
	root := filepath.Join(t.TempDir(), "yt-dlp-venv")
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pyvenv.cfg"), []byte("home = /usr\n"), 0600); err != nil {
		t.Fatal(err)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python 3 unavailable")
	}
	if err := os.Symlink(python, filepath.Join(bin, "python3")); err != nil {
		t.Fatal(err)
	}
	extractor := filepath.Join(bin, "yt-dlp")
	if err := os.WriteFile(extractor, []byte("#!/"+filepath.Join(bin, "python3")+"\nprint('venv interpreter mounted')\n"), 0700); err != nil {
		t.Fatal(err)
	}
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("bubblewrap unavailable")
	}
	runner := IsolatedMetadataRunner{Bubblewrap: bwrap, Python: python, Timeout: 5 * time.Second}
	output, err := runner.Run(context.Background(), extractor, nil, 4096)
	if errors.Is(err, ErrSandboxUnavailable) {
		t.Skipf("bubblewrap network namespaces unavailable: %v", err)
	}
	if err != nil {
		t.Fatalf("Run with nonstandard venv path = %q, %v", output, err)
	}
	if string(output) != "venv interpreter mounted\n" {
		t.Fatalf("Run output = %q, want venv interpreter output", output)
	}
}

func TestAddPathMountDoesNotMountArbitraryPythonEnvironmentRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "home")
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pyvenv.cfg"), []byte("home = /usr\n"), 0600); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(bin, "yt-dlp")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}

	args := addPathMount(nil, executable)
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--ro-bind" && args[i+1] == root && args[i+2] == root {
			t.Fatalf("arbitrary Python environment root %q was mounted wholesale: %v", root, args)
		}
	}
	mountedExecutable := false
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--ro-bind" && args[i+1] == executable && args[i+2] == executable {
			mountedExecutable = true
			break
		}
	}
	if !mountedExecutable {
		t.Fatalf("executable %q was not mounted individually: %v", executable, args)
	}
}

func TestPythonVenvRootAcceptsLegacyDedicatedDirectoryName(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".youtube-venv")
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pyvenv.cfg"), []byte("home = /usr\n"), 0600); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(bin, "yt-dlp")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}

	got, ok := pythonVenvRoot(executable)
	if !ok || got != root {
		t.Fatalf("pythonVenvRoot(%q) = (%q, %v), want (%q, true)", executable, got, ok, root)
	}
}

func TestIsolatedMetadataRunnerBoundsCapturedOutput(t *testing.T) {
	dir := t.TempDir()
	extractor := filepath.Join(dir, "large-output")
	if err := os.WriteFile(extractor, []byte("#!/bin/sh\nhead -c 4096 /dev/zero\n"), 0700); err != nil {
		t.Fatal(err)
	}
	runner := IsolatedMetadataRunner{Timeout: 5 * time.Second}
	output, err := runner.Run(context.Background(), extractor, nil, 128)
	if errors.Is(err, ErrSandboxUnavailable) {
		t.Skipf("bubblewrap network namespaces unavailable: %v", err)
	}
	if !errors.Is(err, ErrMetadataTooLarge) || len(output) != 0 {
		t.Fatalf("bounded output = %d bytes, %v; want no output and ErrMetadataTooLarge", len(output), err)
	}
}

func TestMetadataResolverUsesProductionNetworkSandbox(t *testing.T) {
	dir := t.TempDir()
	ytdlp := filepath.Join(dir, "fake yt-dlp")
	metadata := `{"_type":"video","extractor_key":"Youtube","id":"aqz-KE-bpKQ","title":"sample","duration":1}`
	if err := os.WriteFile(ytdlp, []byte("#!/bin/sh\nprintf '%s' '"+metadata+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	node := filepath.Join(dir, "fake node")
	if err := os.WriteFile(node, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("bubblewrap unavailable")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python 3 unavailable")
	}
	resolver, err := NewMetadataResolver(ytdlp, node, bwrap, python)
	if err != nil {
		t.Fatal(err)
	}
	tracks, err := resolver.Resolve(context.Background(), "https://www.youtube.com/watch?v=aqz-KE-bpKQ")
	if err != nil {
		if errors.Is(err, ErrSandboxUnavailable) {
			t.Skipf("bubblewrap network namespaces unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if len(tracks) != 1 || tracks[0].CanonicalID != "aqz-KE-bpKQ" {
		t.Fatalf("resolved tracks = %#v", tracks)
	}
}

func TestResolveRejectsOversizedMetadataBeforeParsing(t *testing.T) {
	runner := runnerFunc(func(context.Context, string, []string, int) ([]byte, error) {
		return []byte(strings.Repeat("x", MaxMetadataBytes+1)), nil
	})
	resolver := NonProductionMetadataResolver{YTDLP: "/qualified/yt-dlp", Node: "/usr/bin/node", runner: runner}
	if _, err := resolver.Resolve(context.Background(), "study music"); !errors.Is(err, ErrMetadataTooLarge) {
		t.Fatalf("Resolve oversized metadata error = %v, want ErrMetadataTooLarge", err)
	}
}

func TestResolveRejectsIncompleteOrNonYouTubeMetadata(t *testing.T) {
	cases := []string{
		`{"_type":"playlist","playlist_count":2,"entries":[{"_type":"url","ie_key":"Youtube","id":"aqz-KE-bpKQ","title":"one"}]}`,
		`{"_type":"playlist","playlist_count":1,"entries":[{"_type":"url","ie_key":"Generic","id":"aqz-KE-bpKQ","title":"bad"}]}`,
		`{"_type":"playlist","extractor_key":"Generic","playlist_count":1,"entries":[{"_type":"url","ie_key":"Youtube","id":"aqz-KE-bpKQ","title":"one"}]}`,
		`{"_type":"playlist","extractor_key":"YoutubeTab","playlist_count":1,"entries":[{"_type":"url","ie_key":"Generic","id":"aqz-KE-bpKQ","title":"bad"}]}`,
		`{"_type":"playlist","playlist_count":1,"entries":[{"_type":"url","ie_key":"Youtube","id":"aqz-KE-bpKQ","title":"one"}]}`,
		`{"_type":"playlist","extractor_key":"Youtube","playlist_count":1,"entries":[{"_type":"","ie_key":"Youtube","id":"aqz-KE-bpKQ","title":"one"}]}`,
	}
	for _, body := range cases {
		r := NonProductionMetadataResolver{YTDLP: "/yt-dlp", Node: "/node", runner: runnerFunc(func(context.Context, string, []string, int) ([]byte, error) { return []byte(body), nil })}
		if _, err := r.Resolve(context.Background(), "study music"); err == nil {
			t.Errorf("Resolve accepted invalid metadata %s", body)
		}
	}
}

func TestResolveRejectsBlankRootType(t *testing.T) {
	body := `{"_type":"","extractor_key":"Youtube","id":"aqz-KE-bpKQ","title":"one"}`
	r := NonProductionMetadataResolver{YTDLP: "/yt-dlp", Node: "/node", runner: runnerFunc(func(context.Context, string, []string, int) ([]byte, error) { return []byte(body), nil })}
	if _, err := r.Resolve(context.Background(), "https://youtube.com/watch?v=aqz-KE-bpKQ"); !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("Resolve blank root _type error = %v, want ErrMetadataInvalid", err)
	}
}

func TestExecRunnerHonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	runner := ExecRunner{MaxStderr: 1024}
	_, err := runner.Run(ctx, "/bin/sh", []string{"-c", "sleep 5"}, 1024)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run cancellation error = %v, want context deadline", err)
	}
}

func TestExecRunnerAppliesItsOwnDeadlineAndKillsProcessGroup(t *testing.T) {
	marker := t.TempDir() + "/late-child-write"
	runner := ExecRunner{Timeout: 80 * time.Millisecond}
	_, err := runner.Run(context.Background(), "/bin/sh", []string{"-c", "sleep 0.2; touch \"$1\"", "sh", marker}, 1024)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run timeout error = %v, want context deadline", err)
	}
	time.Sleep(250 * time.Millisecond)
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("child process survived cancellation; marker stat error = %v", statErr)
	}
}

func TestExecRunnerStopsAtOutputLimit(t *testing.T) {
	runner := ExecRunner{}
	output, err := runner.Run(context.Background(), "/bin/sh", []string{"-c", "printf 123456"}, 3)
	if !errors.Is(err, ErrMetadataTooLarge) {
		t.Fatalf("Run output limit error = %v, want ErrMetadataTooLarge; output %q", err, output)
	}
}

type runnerFunc func(context.Context, string, []string, int) ([]byte, error)

func (f runnerFunc) Run(ctx context.Context, executable string, args []string, limit int) ([]byte, error) {
	return f(ctx, executable, args, limit)
}

func (runnerFunc) youtubeSandboxedRunner() {}

func TestNormalizeInputAcceptsShareAndAddressBarForms(t *testing.T) {
	canonical := "https://www.youtube.com/watch?v=aqz-KE-bpKQ"
	accepted := map[string]string{
		"https://youtu.be/aqz-KE-bpKQ?si=abc&t=42&feature=share&pp=xyz":                                                              canonical,
		"https://youtu.be/aqz-KE-bpKQ?unknown=anything":                                                                              canonical,
		"https://youtu.be/aqz-KE-bpKQ?si=" + strings.Repeat("x", 220):                                                                canonical,
		"https://www.youtube.com/watch?v=aqz-KE-bpKQ&t=42s&list=PLabc&index=2&si=x&pp=x&feature=share&ab_channel=Test&start_radio=1": canonical,
		"http://youtube.com/watch?v=aqz-KE-bpKQ":                                                                                     canonical,
		"https://m.youtube.com/watch?v=aqz-KE-bpKQ&si=x":                                                                             canonical,
		"https://youtube.com/watch?v=aqz-KE-bpKQ":                                                                                    canonical,
		"https://music.youtube.com/watch?v=aqz-KE-bpKQ":                                                                              canonical,
		"https://youtube.com/live/aqz-KE-bpKQ":                                                                                       canonical,
		"https://youtube.com/shorts/aqz-KE-bpKQ":                                                                                     canonical,
		"https://youtube.com/playlist?list=PLabc_123":                                                                                "https://www.youtube.com/playlist?list=PLabc_123",
	}
	for input, want := range accepted {
		t.Run(input, func(t *testing.T) {
			_, got, err := NormalizeInput(input)
			if err != nil || got != want {
				t.Fatalf("got %q, %v; want %q", got, err, want)
			}
		})
	}
	for _, input := range []string{
		"https://evil.example/watch?v=aqz-KE-bpKQ", "https://youtube.com.evil.example/watch?v=aqz-KE-bpKQ",
		"https://youtube.com/watch?v=bad", "javascript:alert(1)", "https://a@youtube.com/watch?v=aqz-KE-bpKQ",
		"https://youtube.com:443/watch?v=aqz-KE-bpKQ", "https://[::1]/watch?v=aqz-KE-bpKQ",
		"https://youtu.be/aqz-KE-bpKQ?si=" + strings.Repeat("x", 512),
	} {
		if _, _, err := NormalizeInput(input); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("accepted %q", input)
		}
	}
}

// The fixture is a field projection of a real pinned yt-dlp response, not a
// handcrafted response. See testdata/README.md for the exact command.
func TestResolveRecordedPinnedYTDLPVideo(t *testing.T) {
	body, err := os.ReadFile("testdata/yt-dlp-2026.10.21-aqz-KE-bpKQ.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{
		"https://www.youtube.com/watch?v=aqz-KE-bpKQ",
		"https://youtu.be/aqz-KE-bpKQ?si=shared",
		"https://youtube.com/shorts/aqz-KE-bpKQ?si=shared",
	} {
		t.Run(input, func(t *testing.T) {
			resolver := MetadataResolver{YTDLP: "/yt-dlp", Node: "/node", runner: runnerFunc(func(_ context.Context, _ string, args []string, _ int) ([]byte, error) {
				if got := args[len(args)-1]; got != videoURL("aqz-KE-bpKQ") {
					t.Fatalf("canonical=%q", got)
				}
				return body, nil
			})}
			tracks, err := resolver.Resolve(t.Context(), input)
			if err != nil || len(tracks) != 1 {
				t.Fatalf("Resolve real fixture: tracks=%v err=%v", tracks, err)
			}
			if tracks[0].CanonicalID != "aqz-KE-bpKQ" || tracks[0].DurationMS != 635000 || tracks[0].Artist != "Blender" {
				t.Fatalf("descriptor=%#v", tracks[0])
			}
		})
	}
}

func TestResolveMetadataFieldFailureClasses(t *testing.T) {
	body, err := os.ReadFile("testdata/yt-dlp-2026.10.21-aqz-KE-bpKQ.json")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, field string
		value       any
		reason      string
	}{
		{"missing duration", "duration", nil, "missing_duration"},
		{"zero duration", "duration", 0, "invalid_duration"},
		{"negative duration", "duration", -1, "invalid_duration"},
		{"nonnumeric duration", "duration", "private-value", "invalid_duration"},
		{"duration limit", "duration", 86401, "duration_limit"},
		{"private", "availability", "private", "availability"},
		{"live", "live_status", "is_live", "live"},
		{"live flag", "is_live", true, "live"},
		{"upcoming", "live_status", "is_upcoming", "live"},
		{"post live", "live_status", "post_live", "live"},
		{"extractor", "extractor_key", "Generic", "extractor"},
		{"type", "_type", "url", "type"},
		{"id", "id", "private-value", "id"},
		{"title", "title", "private-value\n", "title"},
		{"uploader", "uploader", "private-value\n", "uploader"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var data map[string]any
			if err := json.Unmarshal(body, &data); err != nil {
				t.Fatal(err)
			}
			data[tc.field] = tc.value
			encoded, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			resolver := MetadataResolver{YTDLP: "/yt-dlp", Node: "/node", runner: runnerFunc(func(context.Context, string, []string, int) ([]byte, error) { return encoded, nil })}
			tracks, err := resolver.Resolve(t.Context(), videoURL("aqz-KE-bpKQ"))
			if !errors.Is(err, ErrMetadataInvalid) || len(tracks) != 0 {
				t.Fatalf("tracks=%v err=%v", tracks, err)
			}
			wrapped := fmt.Errorf("resolve: %w", err)
			want := "invalid_metadata:" + tc.reason
			if got := FailureClass(wrapped); got != want {
				t.Fatalf("class=%q want=%q", got, want)
			}
			if !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "private-value") || FailureStderr(wrapped) != "" {
				t.Fatalf("unsafe metadata diagnostics: %v", err)
			}
		})
	}
	t.Run("absent duration", func(t *testing.T) {
		var data map[string]any
		if err := json.Unmarshal(body, &data); err != nil {
			t.Fatal(err)
		}
		delete(data, "duration")
		encoded, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		resolver := MetadataResolver{YTDLP: "/yt-dlp", Node: "/node", runner: runnerFunc(func(context.Context, string, []string, int) ([]byte, error) { return encoded, nil })}
		_, err = resolver.Resolve(t.Context(), videoURL("aqz-KE-bpKQ"))
		if !errors.Is(err, ErrMetadataInvalid) || FailureClass(err) != "invalid_metadata:missing_duration" {
			t.Fatalf("missing duration err=%v class=%s", err, FailureClass(err))
		}
	})
}

func TestResolveRecordedReportedVideo(t *testing.T) {
	body, err := os.ReadFile("testdata/yt-dlp-2026.10.21-LvH_ft4XR1Q.json")
	if err != nil {
		t.Fatal(err)
	}
	resolver := MetadataResolver{YTDLP: "/yt-dlp", Node: "/node", runner: runnerFunc(func(context.Context, string, []string, int) ([]byte, error) { return body, nil })}
	tracks, err := resolver.Resolve(t.Context(), videoURL("LvH_ft4XR1Q"))
	if err != nil || len(tracks) != 1 {
		t.Fatalf("reported video fixture tracks=%v err=%v", tracks, err)
	}
	if tracks[0].CanonicalID != "LvH_ft4XR1Q" || tracks[0].DurationMS != 69000 {
		t.Fatalf("descriptor=%#v", tracks[0])
	}
}

func TestNormalizeOrdinaryProviderAndHostSearchText(t *testing.T) {
	for _, input := range []string{"Spotify: top hits", "youtube.com rewind", "http", "https"} {
		kind, canonical, err := NormalizeInput(input)
		if err != nil || kind != "search" || canonical != "ytsearch5:"+input {
			t.Fatalf("input=%q kind=%q canonical=%q err=%v", input, kind, canonical, err)
		}
	}
}
