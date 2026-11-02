package session

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/music"
	"github.com/jonasthim/dilla/internal/bot/music/livekit"
	"github.com/jonasthim/dilla/internal/id"
)

type sessionQueueStore struct {
	mu    sync.Mutex
	state music.State
}

func (s *sessionQueueStore) Load(context.Context, id.ID) (music.State, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, s.state.Revision, nil
}
func (s *sessionQueueStore) Save(_ context.Context, _ id.ID, expected uint64, state music.State) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if expected != s.state.Revision {
		return 0, music.ErrRevisionConflict
	}
	s.state = state
	return state.Revision, nil
}

func TestProviderPauseHoldsYouTubeAndReleasesSpotify(t *testing.T) {
	for _, provider := range []music.Provider{music.ProviderSpotify, music.ProviderYouTube} {
		t.Run(string(provider), func(t *testing.T) {
			community, channel := id.New(), id.New()
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			manager, err := music.NewSpotifyLeaseManager(dir)
			if err != nil {
				t.Fatal(err)
			}
			var encoders []*encoderStub
			var publishers []*pubStub
			publishes := 0
			starter := QueueStarter{OptionsFor: func(ctx context.Context, track music.Track) (Options, error) {
				enc := &encoderStub{done: make(chan struct{})}
				pub := &pubStub{done: make(chan struct{})}
				encoders = append(encoders, enc)
				publishers = append(publishers, pub)
				group := id.New()
				return Options{Community: community, Channel: channel, Provider: provider, Group: group, Call: client.Call{CallID: id.New(), GroupID: group},
					Acquire: AcquireSpotify(manager), PCM: plainPCMStart{source: &pcmStub{}}, Encoder: encStart{enc: enc},
					Publisher: publisherStartFunc(func(context.Context, client.Call, id.ID, livekit.EpochSource, Encoder) (Publisher, error) {
						publishes++
						return pub, nil
					}), Epochs: epochStub{}}, nil
			}}
			store := &sessionQueueStore{state: music.InitialState()}
			s, err := music.NewSupervisor(t.Context(), channel, store, starter)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			canonical := "abcdefghijk"
			if provider == music.ProviderSpotify {
				canonical = "spotify:track:1234567890123456789012"
			}
			state, err := s.Apply(t.Context(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{{ID: "a", Provider: provider, CanonicalID: canonical, Title: "A", RequesterID: "member"}}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.Apply(t.Context(), state.Revision, music.Operation{Kind: music.OpResume}); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if _, err = s.ApplyImmediate(t.Context(), music.Operation{Kind: music.OpPause}); err != nil {
					t.Fatal(err)
				}
				if provider == music.ProviderSpotify {
					if encoders[len(encoders)-1].closed.Load() != 1 || publishers[len(publishers)-1].closed.Load() != 1 {
						t.Fatal("Spotify pause retained encoder/publication")
					}
					other, err := manager.Acquire(community, id.New())
					if err != nil {
						t.Fatalf("Spotify pause retained slot: %v", err)
					}
					_ = other.Release()
				} else if encoders[0].closed.Load() != 0 || publishers[0].closed.Load() != 0 {
					t.Fatal("YouTube pause closed live resources")
				}
				if _, err = s.Apply(t.Context(), s.State().Revision, music.Operation{Kind: music.OpResume}); err != nil {
					t.Fatal(err)
				}
				if provider == music.ProviderYouTube && publishes != 1 {
					t.Fatalf("pause/resume republished YouTube: publishes=%d want=1", publishes)
				}
			}
			want := 1
			if provider == music.ProviderSpotify {
				want = 3
			}
			if publishes != want {
				t.Fatalf("publishes=%d want=%d", publishes, want)
			}
		})
	}
}

type resumeEncoder struct {
	encoderStub
	err      error
	position atomic.Int64
}

func (e *resumeEncoder) Pause()            {}
func (e *resumeEncoder) Resume() error     { return nil }
func (e *resumeEncoder) PositionMS() int64 { return e.position.Load() }
func (e *resumeEncoder) Wait() error       { return e.err }

type resumeEncoderStart struct{ enc *resumeEncoder }

func (e resumeEncoderStart) Start(context.Context, io.ReadCloser) (Encoder, error) { return e.enc, nil }

func TestSessionSourceFailureAfterWarmResumeSignalsColdRecovery(t *testing.T) {
	for _, test := range []struct {
		name     string
		err      error
		resumed  bool
		wantGone bool
	}{
		{"source_after_resume", io.ErrUnexpectedEOF, true, true},
		{"clean_eof_after_resume", nil, true, false},
		{"source_without_resume", io.ErrUnexpectedEOF, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			enc := &resumeEncoder{encoderStub: encoderStub{done: make(chan struct{})}, err: test.err}
			pub := &pubStub{done: make(chan struct{})}
			o := options(&pcmStart{lease: &leaseStub{}}, &enc.encoderStub, pub, &leaseStub{})
			o.Provider = music.ProviderYouTube
			o.Call.GroupID = o.Group
			o.Encoder = resumeEncoderStart{enc}
			s, err := Start(t.Context(), o)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if test.resumed {
				s.Pause()
				if err = s.Resume(); err != nil {
					t.Fatal(err)
				}
			}
			close(enc.done)
			if err = s.Wait(); errors.Is(err, music.ErrSourceGone) != test.wantGone {
				t.Fatalf("Wait=%v want source gone=%v", err, test.wantGone)
			}
		})
	}
}
func TestSessionSourceFailureAfterRecoveryWindowIsNotRetried(t *testing.T) {
	enc := &resumeEncoder{encoderStub: encoderStub{done: make(chan struct{})}, err: io.ErrUnexpectedEOF}
	pub := &pubStub{done: make(chan struct{})}
	o := options(&pcmStart{lease: &leaseStub{}}, &enc.encoderStub, pub, &leaseStub{})
	o.Provider = music.ProviderYouTube
	o.Call.GroupID = o.Group
	o.Encoder = resumeEncoderStart{enc}
	s, err := Start(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Pause()
	if err = s.Resume(); err != nil {
		t.Fatal(err)
	}
	// The recovery window is bounded: a later failure uses normal queue failure handling.
	s.mu.Lock()
	s.resumedAt = time.Now().Add(-time.Minute)
	s.mu.Unlock()
	close(enc.done)
	if err = s.Wait(); errors.Is(err, music.ErrSourceGone) {
		t.Fatalf("late failure offered cold retry: %v", err)
	}
}

type sourceEndingPublisher struct {
	pubStub
	entered, release chan struct{}
	err              error
}

func (p *sourceEndingPublisher) Wait() error {
	close(p.entered)
	<-p.release
	return p.err
}
func TestPublisherReportedSourceFailureAfterWarmResumeSignalsColdRecovery(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		wantGone bool
	}{
		{"source", io.ErrUnexpectedEOF, true},
		{"room_failure", livekit.ErrRoomConnection, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enc := &resumeEncoder{encoderStub: encoderStub{done: make(chan struct{})}, err: io.ErrUnexpectedEOF}
			pub := &sourceEndingPublisher{pubStub: pubStub{done: make(chan struct{})}, entered: make(chan struct{}), release: make(chan struct{}), err: tc.err}
			o := options(&pcmStart{lease: &leaseStub{}}, &enc.encoderStub, &pub.pubStub, &leaseStub{})
			o.Provider = music.ProviderYouTube
			o.Call.GroupID = o.Group
			o.Encoder = resumeEncoderStart{enc}
			o.Publisher = publisherStartFunc(func(context.Context, client.Call, id.ID, livekit.EpochSource, Encoder) (Publisher, error) {
				return pub, nil
			})
			s, err := Start(t.Context(), o)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			s.Pause()
			if err = s.Resume(); err != nil {
				t.Fatal(err)
			}
			close(pub.done)
			<-pub.entered // Force the publisher path; it can report the encoder's failure too.
			close(enc.done)
			close(pub.release)
			if err = s.Wait(); errors.Is(err, music.ErrSourceGone) != tc.wantGone {
				t.Fatalf("Wait=%v want source gone=%v", err, tc.wantGone)
			}
		})
	}
}
