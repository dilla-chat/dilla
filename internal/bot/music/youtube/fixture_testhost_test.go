//go:build dilla_testhost

package youtube

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/music"
	musicruntime "github.com/dilla-chat/dilla/internal/bot/music/runtime"
	"github.com/dilla-chat/dilla/internal/id"
)

func writeFixture(t *testing.T, dir, stem string, seconds float64) []byte {
	t.Helper()
	stream := testOgg(t, seconds)
	if err := os.WriteFile(filepath.Join(dir, stem+".opus"), stream, 0o600); err != nil {
		t.Fatal(err)
	}
	return stream
}

func TestFixtureResolverServesFilesThroughTheSameFfmpegArguments(t *testing.T) {
	dir := t.TempDir()
	fixtures := t.TempDir()
	stream := writeFixture(t, fixtures, "alpha-track", 1.0)
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	log := filepath.Join(dir, "argv.log")
	recorder := writeScript(t, dir, "ffmpeg", `printf '%s\n' "$*" >> "`+log+`"; exec `+ffmpegPath+` "$@"`)
	t.Setenv(FixturesEnv, fixtures)
	backend, err := NewBackend(SandboxConfig{FFmpeg: recorder}, 0)
	if err != nil {
		t.Skipf("backend unavailable: %v", err)
	}
	defer backend.Close()

	tracks, err := backend.Tracks.Resolve(context.Background(), "alpha track")
	if err != nil || len(tracks) != 1 || tracks[0].Title != "alpha-track" || tracks[0].DurationMS < 900 || tracks[0].DurationMS > 1100 {
		t.Fatalf("fixture lookup by search text = %+v, %v", tracks, err)
	}
	byURL, err := backend.Tracks.Resolve(context.Background(), "https://www.youtube.com/watch?v=alpha-track")
	if err != nil || byURL[0].CanonicalID != tracks[0].CanonicalID {
		t.Fatalf("fixture lookup by video id = %+v, %v", byURL, err)
	}
	source, err := backend.Streams.Start(context.Background(), "https://www.youtube.com/watch?v="+tracks[0].CanonicalID)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	got, err := io.ReadAll(source)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(got, []byte("OggS")) || len(got) < len(stream)/2 {
		t.Fatalf("fixture stream = %d bytes, not an Ogg Opus re-encode of %d bytes", len(got), len(stream))
	}
	recorded, _ := os.ReadFile(log)
	want := strings.Join(ffmpegStreamArgs(filepath.Join(fixtures, "alpha-track.opus"), 0, false, ""), " ")
	if strings.TrimSpace(string(recorded)) != want {
		t.Fatalf("ffmpeg argv\n got %q\nwant %q", strings.TrimSpace(string(recorded)), want)
	}
	if _, err := backend.Tracks.Resolve(context.Background(), "no such song"); err == nil {
		t.Fatal("an absent fixture resolved")
	}
}

type memQueue struct {
	mu    sync.Mutex
	state music.State
}

func (m *memQueue) Load(context.Context, id.ID) (music.State, uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.Queue == nil {
		m.state = music.InitialState()
	}
	return m.state, m.state.Revision, nil
}

func (m *memQueue) Save(_ context.Context, _ id.ID, expected uint64, state music.State) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if state.Revision != expected+1 {
		return 0, errors.New("revision conflict")
	}
	m.state = state
	return state.Revision, nil
}

// fixturePlayer is a TrackStarter made of the real stream starter and the real
// Opus runtime, with a consumer standing in for LiveKit that stamps each frame.
type fixturePlayer struct {
	streams *StreamStarter
	mu      sync.Mutex
	first   map[string]time.Time
	last    map[string]time.Time
	ended   map[string]bool
}

type fixturePlayback struct {
	sess *musicruntime.Session
	done chan struct{}
}

func (p *fixturePlayback) Done() <-chan struct{} { return p.done }
func (p *fixturePlayback) Wait() error           { <-p.done; return p.sess.Wait() }
func (p *fixturePlayback) Close() error {
	_ = p.sess.Close()
	<-p.done
	return nil
}

func (p *fixturePlayer) Start(ctx context.Context, track music.Track) (music.Playback, error) {
	source, err := p.streams.StartAt(ctx, videoURL(track.CanonicalID), music.StartAtMS(ctx))
	if err != nil {
		return nil, err
	}
	sess, err := musicruntime.StartOgg(ctx, source, 100, musicruntime.Options{})
	if err != nil {
		_ = source.Close()
		return nil, err
	}
	playback := &fixturePlayback{sess: sess, done: make(chan struct{})}
	go func() {
		defer func() {
			p.mu.Lock()
			p.ended[track.ID] = true
			p.mu.Unlock()
			close(playback.done)
		}()
		for first := true; ; first = false {
			if _, err := sess.Provider().NextSample(context.Background()); err != nil {
				return
			}
			now := time.Now()
			p.mu.Lock()
			if first {
				p.first[track.ID] = now
			}
			p.last[track.ID] = now
			p.mu.Unlock()
			time.Sleep(2 * time.Millisecond) // about ten times real time, so the next track's pre-buffer finishes first
		}
	}()
	return playback, nil
}

func (p *fixturePlayer) Prefetch(ctx context.Context, track music.Track) error {
	return p.streams.Prefetch(ctx, videoURL(track.CanonicalID))
}

func TestNextQueuedTrackGapUnder150ms(t *testing.T) {
	fixtures := t.TempDir()
	writeFixture(t, fixtures, "alpha-track", 3.0)
	writeFixture(t, fixtures, "bravo-track", 3.0)
	t.Setenv(FixturesEnv, fixtures)
	backend, err := NewBackend(SandboxConfig{FFmpeg: "ffmpeg"}, 0)
	if err != nil {
		t.Skipf("backend unavailable: %v", err)
	}
	defer backend.Close()
	var gaps []time.Duration
	for run := 0; run < 20; run++ {
		player := &fixturePlayer{streams: backend.Streams, first: map[string]time.Time{}, last: map[string]time.Time{}, ended: map[string]bool{}}
		supervisor, err := music.NewSupervisor(context.Background(), id.ID{15: 1}, &memQueue{}, player)
		if err != nil {
			t.Fatal(err)
		}
		a, err := backend.Tracks.Resolve(context.Background(), "alpha-track")
		if err != nil {
			t.Fatal(err)
		}
		b, err := backend.Tracks.Resolve(context.Background(), "bravo-track")
		if err != nil {
			t.Fatal(err)
		}
		for i := range a {
			a[i].RequesterID, b[i].RequesterID = "member", "member"
		}
		state, err := supervisor.Apply(context.Background(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: append(a, b...)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := supervisor.Apply(context.Background(), state.Revision, music.Operation{Kind: music.OpResume}); err != nil {
			t.Fatal(err)
		}
		deadline := time.After(15 * time.Second)
		for {
			player.mu.Lock()
			aDone := player.ended[a[0].ID]
			_, bStarted := player.first[b[0].ID]
			done := aDone && bStarted
			player.mu.Unlock()
			if done {
				time.Sleep(20 * time.Millisecond)
				break
			}
			select {
			case <-deadline:
				t.Fatalf("run %d: the queue never reached the second track", run)
			case <-time.After(2 * time.Millisecond):
			}
		}
		player.mu.Lock()
		gaps = append(gaps, player.first[b[0].ID].Sub(player.last[a[0].ID]))
		player.mu.Unlock()
		_ = supervisor.Close()
	}
	sorted := append([]time.Duration(nil), gaps...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	t.Logf("gaps over 20 runs: min %v median %v max %v", sorted[0], sorted[10], sorted[19])
	if sorted[19] > 150*time.Millisecond {
		t.Fatalf("the longest gap between the last frame of A and the first frame of B was %v, want <= 150ms (%v)", sorted[19], gaps)
	}
}
