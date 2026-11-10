//go:build dilla_testhost

package youtube

// This file only exists in a test-host build (-tags dilla_testhost). release.yml
// and server.yml read the shipped binaries' build info and fail the release if
// that tag is in a dillad or a dillabot.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/music"
)

func init() { localStream = startLocalStream }

// A test host may run without an owner; a shipped Bot may not.
func init() { requireOwner = false }

// FixtureResolver serves Opus files from a directory as if YouTube had
// resolved them. A video URL (or a bare 11-character name) selects <id>.opus;
// search text selects <text with spaces as dashes>.opus. The stream goes
// through StreamStarter and the same ffmpeg arguments as a real track; only
// the input is a file, read by ffmpeg directly (the test host has no network).
type FixtureResolver struct{ dir string }

// NewFixtureResolver serves the .opus files in dir.
func NewFixtureResolver(dir string) (*FixtureResolver, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, ErrInvalidInput
	}
	info, err := os.Stat(abs) // #nosec G703 -- test-host build; the directory is the operator-set fixture directory.
	if err != nil || !info.IsDir() {
		return nil, ErrInvalidInput
	}
	return &FixtureResolver{dir: abs}, nil
}

// FixturesStateDir is where a test-host Bot also finds fixtures: <DILLA_BOT_STATE_DIR>/music-fixtures. The supervised
// Bot's environment is fixed by bot-launch (it carries no DILLA_BOT_MUSIC_FIXTURES), so the e2e host copies the
// fixtures into the Bot's state directory instead (Bot v2 Task 19).
const FixturesStateDir = "music-fixtures"

// fixturesDir is DILLA_BOT_MUSIC_FIXTURES, else the state directory's fixture directory when it exists.
func fixturesDir() string {
	if dir := os.Getenv(FixturesEnv); dir != "" {
		return dir
	}
	state := os.Getenv("DILLA_BOT_STATE_DIR")
	if state == "" {
		return ""
	}
	dir := filepath.Join(state, FixturesStateDir)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() { // #nosec G703 -- test-host build; the Bot's own state directory.
		return ""
	}
	return dir
}

// FixturesConfigured reports whether NewBackend will answer from fixtures instead of the sandboxed tools.
func FixturesConfigured() bool { return fixturesDir() != "" }

func fixtureBackend(cfg SandboxConfig) (*Backend, bool, error) {
	dir := fixturesDir()
	if dir == "" {
		return nil, false, nil
	}
	fixtures, err := NewFixtureResolver(dir)
	if err != nil {
		return nil, true, err
	}
	// A fixture plays through startLocalStream, an unsandboxed ffmpeg reading the file, so ffmpeg is the only tool it
	// needs. NewStreamStarter also requires bubblewrap and python3, which a host without bwrap (the CI runner of the
	// Bot music e2e) does not have: the backend failed and every /play answered "can't do that here right now".
	if cfg.FFmpeg, err = lookupTool(cfg.FFmpeg, "ffmpeg"); err != nil {
		return nil, true, ErrPCMStart
	}
	return &Backend{streams: newStreamStarter(cfg, fixtures), tracks: fixtureTracks{fixtures}}, true, nil
}

// fixtureID maps a file stem to a queue-valid 11-character video id.
func fixtureID(stem string) string {
	if idPattern.MatchString(stem) {
		return stem
	}
	sum := sha256.Sum256([]byte(stem))
	return "fx" + base64.RawURLEncoding.EncodeToString(sum[:])[:9]
}

func (f *FixtureResolver) find(input string) (stem, path string, err error) {
	kind, canonical, nerr := NormalizeInput(input)
	if nerr != nil {
		if idPattern.MatchString(input) {
			kind, canonical = "bare", input
		} else {
			return "", "", nerr
		}
	}
	var want string
	switch kind {
	case "video":
		want = strings.TrimPrefix(canonical, "https://www.youtube.com/watch?v=")
	case "bare":
		want = canonical
	case "search":
		text := strings.TrimPrefix(canonical, "ytsearch5:")
		want = strings.Join(strings.Fields(strings.ToLower(text)), "-")
	default:
		return "", "", ErrInvalidInput
	}
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		return "", "", ErrExtractFailed
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".opus") || entry.IsDir() {
			continue
		}
		stem := strings.TrimSuffix(name, ".opus")
		if stem == want || fixtureID(stem) == want {
			return stem, filepath.Join(f.dir, name), nil
		}
	}
	return "", "", ErrExtractFailed
}

func (f *FixtureResolver) track(stem, path string) (music.Track, error) {
	duration, err := oggDuration(path)
	if err != nil {
		return music.Track{}, invalidMetadata("fixture")
	}
	id := fixtureID(stem)
	return music.Track{ID: "yt:" + id, Provider: music.ProviderYouTube, CanonicalID: id, Title: stem, Artist: "fixture", DurationMS: duration.Milliseconds()}, nil
}

// Resolve implements Resolver for StreamStarter.
func (f *FixtureResolver) Resolve(ctx context.Context, input string) (Resolved, error) {
	started := time.Now()
	stem, path, err := f.find(input)
	if err != nil {
		return Resolved{}, err
	}
	track, err := f.track(stem, path)
	if err != nil {
		return Resolved{}, err
	}
	music.MarkResolve(ctx, time.Since(started), false)
	return Resolved{Track: track, StreamURL: "file://" + path, Expires: time.Now().Add(DefaultResolveCache)}, nil
}

// tracks is the queue-side lookup behind TrackResolver.
func (f *FixtureResolver) tracks(input string) ([]music.Track, error) {
	stem, path, err := f.find(input)
	if err != nil {
		return nil, err
	}
	track, err := f.track(stem, path)
	if err != nil {
		return nil, err
	}
	return []music.Track{track}, nil
}

// fixtureTracks adapts FixtureResolver to TrackResolver; both interfaces want a
// method named Resolve with different results, so the queue side is its own type.
type fixtureTracks struct{ f *FixtureResolver }

func (t fixtureTracks) Resolve(_ context.Context, input string) ([]music.Track, error) {
	return t.f.tracks(input)
}
func (t fixtureTracks) Search(_ context.Context, input string) ([]music.Track, error) {
	return t.f.tracks(input)
}

// oggDuration reads the last page's granule position of an Ogg Opus file.
func oggDuration(path string) (time.Duration, error) {
	file, err := os.Open(path) // #nosec G304 -- test-host build; find() only returns files of the fixture directory.
	if err != nil {
		return 0, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	tail := int64(70 << 10)
	if info.Size() < tail {
		tail = info.Size()
	}
	buf := make([]byte, tail)
	if _, err := file.ReadAt(buf, info.Size()-tail); err != nil && err != io.EOF {
		return 0, err
	}
	for i := len(buf) - 27; i >= 0; i-- {
		if string(buf[i:i+4]) == "OggS" {
			granule := binary.LittleEndian.Uint64(buf[i+6 : i+14])
			if granule == ^uint64(0) {
				continue
			}
			return time.Duration(granule/48) * time.Millisecond, nil // #nosec G115 -- bounded by the 24 h track limit when queued.
		}
	}
	return 0, io.ErrUnexpectedEOF
}

// localProcess is an unsandboxed ffmpeg reading a fixture file.
type localProcess struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	once   sync.Once
	mu     sync.Mutex
	gone   bool
}

func startLocalStream(ctx context.Context, ffmpeg, path string, startMS int64) (io.ReadCloser, error) {
	// Only a file inside the fixture directory is ever read, never an arbitrary path.
	root, err := filepath.EvalSymlinks(fixturesDir())
	if err != nil || root == "" {
		return nil, ErrStreamURL
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || !withinPath(root, resolved) || !strings.HasSuffix(resolved, ".opus") {
		return nil, ErrStreamURL
	}
	cmd := exec.CommandContext(ctx, ffmpeg, ffmpegStreamArgs(resolved, startMS, false, "")...) // #nosec G204 -- test-host build only; the path comes from the fixture directory.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.Stderr = io.Discard
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, ErrPCMStart
	}
	if err := cmd.Start(); err != nil {
		return nil, ErrPCMStart
	}
	return &localProcess{cmd: cmd, stdout: stdout}, nil
}

func (p *localProcess) Read(b []byte) (int, error) {
	n, err := p.stdout.Read(b)
	if err == io.EOF {
		if waitErr := p.cmd.Wait(); waitErr != nil {
			err = ErrPCMStart
		}
		p.mu.Lock()
		p.gone = true
		p.mu.Unlock()
	}
	return n, err
}

func (p *localProcess) Close() error {
	p.once.Do(func() {
		if p.cmd.Process != nil {
			_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		}
		_ = p.stdout.Close()
		_ = p.cmd.Wait()
		p.mu.Lock()
		p.gone = true
		p.mu.Unlock()
	})
	return nil
}

func (p *localProcess) SourceGone() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.gone }
