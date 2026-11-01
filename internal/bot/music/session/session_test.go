package session

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/music"
	"github.com/jonasthim/dilla/internal/bot/music/livekit"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/id"
)

type leaseStub struct{ released atomic.Int32 }

func (l *leaseStub) Release() error { l.released.Add(1); return nil }

type pcmStub struct{ closed atomic.Int32 }

func (p *pcmStub) Read([]byte) (int, error) { return 0, io.EOF }
func (p *pcmStub) Close() error             { p.closed.Add(1); return nil }

type pcmStart struct {
	lease *leaseStub
	calls atomic.Int32
	fail  bool
}

type plainPCMStart struct{ source *pcmStub }

func (p plainPCMStart) Start(context.Context, string) (io.ReadCloser, error) { return p.source, nil }

func (p *pcmStart) Start(context.Context, string) (io.ReadCloser, error) {
	p.calls.Add(1)
	if p.lease.released.Load() != 0 {
		panic("lease already released")
	}
	if p.fail {
		return nil, errors.New("pcm")
	}
	return &pcmStub{}, nil
}

type encoderStub struct {
	done   chan struct{}
	closed atomic.Int32
}

func (e *encoderStub) Done() <-chan struct{} { return e.done }
func (e *encoderStub) Wait() error           { return nil }
func (e *encoderStub) Close() error          { e.closed.Add(1); return nil }

type encStart struct {
	enc  *encoderStub
	fail bool
}

func (e encStart) Start(context.Context, io.ReadCloser) (Encoder, error) {
	if e.fail {
		return nil, errors.New("encoder")
	}
	return e.enc, nil
}

type pubStub struct {
	done   chan struct{}
	closed atomic.Int32
}

func (p *pubStub) Done() <-chan struct{} { return p.done }
func (p *pubStub) Wait() error           { return nil }
func (p *pubStub) Close() error          { p.closed.Add(1); return nil }

type pubStart struct {
	pub  *pubStub
	fail bool
}

func (p pubStart) Start(context.Context, client.Call, id.ID, livekit.EpochSource, Encoder) (Publisher, error) {
	if p.fail {
		return nil, errors.New("publisher")
	}
	return p.pub, nil
}

type epochStub struct{}

func (epochStub) CallMediaEpoch(context.Context, id.ID) (native.CallMediaEpoch, error) {
	return native.CallMediaEpoch{}, nil
}

func options(pcm *pcmStart, enc *encoderStub, pub *pubStub, lease *leaseStub) Options {
	return Options{Community: id.New(), Channel: id.New(), Track: "spotify:track:123", Call: client.Call{CallID: id.New(), GroupID: id.New()}, Group: id.New(), Acquire: func(id.ID, id.ID) (Lease, error) { return lease, nil }, PCM: pcm, Encoder: encStart{enc: enc}, Publisher: pubStart{pub: pub}, Epochs: epochStub{}}
}

func TestStartAcquiresLeaseBeforeAudioAndReleasesAfterCompletion(t *testing.T) {
	lease := &leaseStub{}
	pcm := &pcmStart{lease: lease}
	enc := &encoderStub{done: make(chan struct{})}
	pub := &pubStub{done: make(chan struct{})}
	o := options(pcm, enc, pub, lease)
	o.Call.GroupID = o.Group
	s, err := Start(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if pcm.calls.Load() != 1 {
		t.Fatalf("PCM starts=%d", pcm.calls.Load())
	}
	if lease.released.Load() != 0 {
		t.Fatal("lease released before playback ended")
	}
	close(pub.done)
	if err := s.Wait(); err != nil {
		t.Fatal(err)
	}
	if lease.released.Load() != 1 || enc.closed.Load() != 1 || pub.closed.Load() != 1 {
		t.Fatalf("cleanup lease=%d encoder=%d publisher=%d", lease.released.Load(), enc.closed.Load(), pub.closed.Load())
	}
}

func TestStartFailuresReleaseLeaseAndPartialResources(t *testing.T) {
	t.Run("lease", func(t *testing.T) {
		lease := &leaseStub{}
		p := &pcmStart{lease: lease}
		e := &encoderStub{done: make(chan struct{})}
		pub := &pubStub{done: make(chan struct{})}
		o := options(p, e, pub, lease)
		o.Call.GroupID = o.Group
		o.Acquire = func(id.ID, id.ID) (Lease, error) { return nil, errors.New("busy") }
		if _, err := Start(context.Background(), o); err == nil {
			t.Fatal("expected error")
		}
		if p.calls.Load() != 0 {
			t.Fatal("audio started without lease")
		}
	})
	t.Run("PCM", func(t *testing.T) {
		lease := &leaseStub{}
		p := &pcmStart{lease: lease, fail: true}
		e := &encoderStub{done: make(chan struct{})}
		pub := &pubStub{done: make(chan struct{})}
		o := options(p, e, pub, lease)
		o.Call.GroupID = o.Group
		if _, err := Start(context.Background(), o); err == nil {
			t.Fatal("expected error")
		}
		if lease.released.Load() != 1 || p.calls.Load() != 1 {
			t.Fatal("lease/PCM cleanup mismatch")
		}
	})
	t.Run("publisher", func(t *testing.T) {
		lease := &leaseStub{}
		p := &pcmStart{lease: lease}
		e := &encoderStub{done: make(chan struct{})}
		o := options(p, e, &pubStub{done: make(chan struct{})}, lease)
		o.Call.GroupID = o.Group
		o.Publisher = pubStart{fail: true}
		if _, err := Start(context.Background(), o); err == nil {
			t.Fatal("expected error")
		}
		if lease.released.Load() != 1 || e.closed.Load() != 1 {
			t.Fatal("partial resources not closed")
		}
	})
}

func TestConcurrentCloseReleasesLeaseOnce(t *testing.T) {
	lease := &leaseStub{}
	p := &pcmStart{lease: lease}
	e := &encoderStub{done: make(chan struct{})}
	pub := &pubStub{done: make(chan struct{})}
	o := options(p, e, pub, lease)
	o.Call.GroupID = o.Group
	s, err := Start(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = s.Close() }()
	}
	wg.Wait()
	if lease.released.Load() != 1 {
		t.Fatalf("lease releases=%d", lease.released.Load())
	}
}

func TestYouTubeSessionDoesNotAcquireInstallationSpotifyLease(t *testing.T) {
	lease := &leaseStub{}
	enc := &encoderStub{done: make(chan struct{})}
	pub := &pubStub{done: make(chan struct{})}
	o := options(&pcmStart{lease: lease}, enc, pub, lease)
	o.Provider = "youtube"
	o.Acquire = nil
	o.PCM = plainPCMStart{source: &pcmStub{}}
	o.Call.GroupID = o.Group
	s, err := Start(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if lease.released.Load() != 0 {
		t.Fatal("YouTube session touched Spotify lease")
	}
	_ = s.Close()
}

func TestQueueStarterRejectsProviderMismatchBeforePlayback(t *testing.T) {
	called := false
	lease := &leaseStub{}
	enc := &encoderStub{done: make(chan struct{})}
	pub := &pubStub{done: make(chan struct{})}
	starter := QueueStarter{OptionsFor: func(context.Context, music.Track) (Options, error) {
		called = true
		o := options(&pcmStart{lease: lease}, enc, pub, lease)
		o.Provider = music.ProviderYouTube
		o.Acquire = nil
		o.PCM = plainPCMStart{source: &pcmStub{}}
		o.Call.GroupID = o.Group
		return o, nil
	}}
	playback, err := starter.Start(context.Background(), music.Track{Provider: music.ProviderSpotify, CanonicalID: "track"})
	if playback != nil {
		_ = playback.Close()
	}
	if !called || !errors.Is(err, ErrInvalid) {
		t.Fatalf("mismatched provider was accepted: called=%v err=%v", called, err)
	}
}

type publisherStartFunc func(context.Context, client.Call, id.ID, livekit.EpochSource, Encoder) (Publisher, error)

func (f publisherStartFunc) Start(ctx context.Context, c client.Call, g id.ID, e livekit.EpochSource, enc Encoder) (Publisher, error) {
	return f(ctx, c, g, e, enc)
}

func TestQueueStarterRechecksMembershipAfterServerDisconnect(t *testing.T) {
	calls := 0
	var encoders []*encoderStub
	pub := &pubStub{done: make(chan struct{})}
	starter := QueueStarter{OptionsFor: func(context.Context, music.Track) (Options, error) {
		calls++
		enc := &encoderStub{done: make(chan struct{})}
		encoders = append(encoders, enc)
		o := options(&pcmStart{lease: &leaseStub{}}, enc, pub, &leaseStub{})
		o.Provider = music.ProviderYouTube
		o.Acquire = nil
		o.PCM = plainPCMStart{source: &pcmStub{}}
		o.Call.GroupID = o.Group
		attempt := calls
		o.Publisher = publisherStartFunc(func(context.Context, client.Call, id.ID, livekit.EpochSource, Encoder) (Publisher, error) {
			if attempt == 1 {
				return nil, context.Canceled
			}
			if attempt == 2 {
				return nil, livekit.ErrRoomConnection
			}
			return pub, nil
		})
		return o, nil
	}}
	playback, err := starter.Start(context.Background(), music.Track{Provider: music.ProviderYouTube, CanonicalID: "video"})
	if err != nil {
		t.Fatal(err)
	}
	defer playback.Close()
	if calls != 3 || encoders[0].closed.Load() != 1 || encoders[1].closed.Load() != 1 {
		t.Fatalf("fresh options=%d first encoder cleanup=%d", calls, encoders[0].closed.Load())
	}
}

func TestQueueStarterDoesNotRetryCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	starter := QueueStarter{OptionsFor: func(context.Context, music.Track) (Options, error) {
		calls++
		cancel()
		return Options{}, context.Canceled
	}}
	if _, err := starter.Start(ctx, music.Track{Provider: music.ProviderYouTube, CanonicalID: "video"}); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
}

func TestQueueStarterBoundsDisconnectRetriesAndKeepsBindingRefusalsTerminal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure error
		want    int
	}{{"server disconnect", context.Canceled, 3}, {"epoch changed", livekit.ErrEpochChanged, 3}, {"room connection", livekit.ErrRoomConnection, 3}, {"binding refusal", livekit.ErrBinding, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			starter := QueueStarter{OptionsFor: func(context.Context, music.Track) (Options, error) { calls++; return Options{}, tc.failure }}
			_, err := starter.Start(context.Background(), music.Track{Provider: music.ProviderYouTube, CanonicalID: "video"})
			if !errors.Is(err, tc.failure) || calls != tc.want {
				t.Fatalf("error=%v calls=%d want=%d", err, calls, tc.want)
			}
		})
	}
}

func TestQueueStarterReleasesSpotifyLeaseBeforeRetry(t *testing.T) {
	var leases []*leaseStub
	var encoders []*encoderStub
	pub := &pubStub{done: make(chan struct{})}
	starter := QueueStarter{OptionsFor: func(context.Context, music.Track) (Options, error) {
		if len(leases) > 0 && (leases[len(leases)-1].released.Load() != 1 || encoders[len(encoders)-1].closed.Load() != 1) {
			t.Fatal("retry began before cleanup")
		}
		lease := &leaseStub{}
		leases = append(leases, lease)
		enc := &encoderStub{done: make(chan struct{})}
		encoders = append(encoders, enc)
		o := options(&pcmStart{lease: lease}, enc, pub, lease)
		o.Call.GroupID = o.Group
		attempt := len(leases)
		o.Publisher = publisherStartFunc(func(context.Context, client.Call, id.ID, livekit.EpochSource, Encoder) (Publisher, error) {
			if attempt == 1 {
				return nil, livekit.ErrEpochChanged
			}
			return pub, nil
		})
		return o, nil
	}}
	playback, err := starter.Start(context.Background(), music.Track{Provider: music.ProviderSpotify, CanonicalID: "track"})
	if err != nil {
		t.Fatal(err)
	}
	_ = playback.Close()
	if len(leases) != 2 || leases[1].released.Load() != 1 || encoders[1].closed.Load() != 1 {
		t.Fatal("retry resources not released exactly once")
	}
}

func TestQueueStarterOnlyExplicitPlaybackResetsConnection(t *testing.T) {
	resets, optionsCalls := 0, 0
	stopped := errors.New("test stops before resources")
	starter := QueueStarter{
		ResetConnection: func() error { resets++; return nil },
		OptionsFor:      func(context.Context, music.Track) (Options, error) { optionsCalls++; return Options{}, stopped },
	}
	track := music.Track{Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk"}
	if _, err := starter.Start(context.Background(), track); !errors.Is(err, stopped) {
		t.Fatal(err)
	}
	if resets != 0 {
		t.Fatal("automatic advancement reset a removed connection")
	}
	if _, err := starter.StartExplicit(context.Background(), track); !errors.Is(err, stopped) {
		t.Fatal(err)
	}
	if resets != 1 || optionsCalls != 2 {
		t.Fatalf("resets=%d options=%d", resets, optionsCalls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := starter.StartExplicit(ctx, track); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if resets != 1 || optionsCalls != 2 {
		t.Fatal("cancelled playback authorized a reconnect")
	}
	starter.ResetConnection = func() error { return ErrInvalid }
	if _, err := starter.StartExplicit(context.Background(), track); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if optionsCalls != 2 {
		t.Fatal("failed reset acquired fresh resources")
	}
}

func TestExplicitStartRefreshesBindingAfterDelayedRoomDisconnect(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		explicit              bool
		second                error
		wantCalls, wantResets int
	}{
		{"explicit replacement", true, ErrInvalid, 2, 2},
		{"automatic advancement", false, ErrInvalid, 1, 0},
		{"repeated binding failure is terminal", true, livekit.ErrBinding, 2, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, resets := 0, 0
			starter := QueueStarter{ResetConnection: func() error { resets++; return nil }, OptionsFor: func(context.Context, music.Track) (Options, error) {
				calls++
				if calls == 1 {
					return Options{}, livekit.ErrBinding
				}
				return Options{}, tc.second
			}}
			track := music.Track{Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk"}
			var err error
			if tc.explicit {
				_, err = starter.StartExplicit(context.Background(), track)
			} else {
				_, err = starter.Start(context.Background(), track)
			}
			wantErr := tc.second
			if !tc.explicit {
				wantErr = livekit.ErrBinding
			}
			if !errors.Is(err, wantErr) || calls != tc.wantCalls || resets != tc.wantResets {
				t.Fatalf("err=%v options=%d resets=%d", err, calls, resets)
			}
		})
	}
}

type schedulingPCM struct{ api *client.Client }

func (p schedulingPCM) Start(ctx context.Context, _ string) (io.ReadCloser, error) {
	if client.RequestPriority(ctx) != 0 || client.CooperativeCheckpoint(ctx) != nil {
		return nil, errors.New("playback retained a worker turn")
	}
	_, err := p.api.Discover(ctx)
	var response *client.APIError
	if !errors.As(err, &response) || response.Status != 503 {
		return nil, errors.New("playback retained the command's interactive priority")
	}
	return &pcmStub{}, nil
}

type schedulingTransport struct{}

func (schedulingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 503, Header: http.Header{"Content-Type": []string{"application/cbor"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
}
func TestPlaybackSessionClearsInteractiveScheduling(t *testing.T) {
	api, err := client.New(client.Config{BaseURL: "https://example.invalid", InstanceID: id.New(), HTTPClient: &http.Client{Transport: schedulingTransport{}}})
	if err != nil {
		t.Fatal(err)
	}
	lease := &leaseStub{}
	enc := &encoderStub{done: make(chan struct{})}
	pub := &pubStub{done: make(chan struct{})}
	o := options(&pcmStart{lease: lease}, enc, pub, lease)
	o.Call.GroupID = o.Group
	o.PCM = schedulingPCM{api: api}
	ctx := client.WithInteractive(client.WithCooperativeStop(t.Context(), func() bool { return true }))
	playback, err := Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer playback.Close()
}

func TestStartClosesAudioAndLeaseOnLateCallFailure(t *testing.T) {
	for _, stage := range []string{"call_group_join", "call_token", "call_fields"} {
		t.Run(stage, func(t *testing.T) {
			lease, pcm := &leaseStub{}, &pcmStub{}
			enc := &encoderStub{done: make(chan struct{})}
			pub := &pubStub{done: make(chan struct{})}
			o := options(&pcmStart{lease: lease}, enc, pub, lease)
			o.PCM = plainPCMStart{source: pcm}
			failure := errors.New("late call failure")
			o.Call, o.Group = client.Call{}, id.ID{}
			o.PrepareCall = func(context.Context) (client.Call, error) {
				if lease.released.Load() != 0 || enc.closed.Load() != 0 {
					t.Fatal("resources closed before call preparation")
				}
				return client.Call{}, &music.StartError{Stage: stage, Cause: failure}
			}
			_, err := Start(context.Background(), o)
			if !errors.Is(err, failure) {
				t.Fatalf("start = %v", err)
			}
			if pcm.closed.Load() != 1 || enc.closed.Load() != 1 || lease.released.Load() != 1 || pub.closed.Load() != 0 {
				t.Fatalf("cleanup pcm=%d encoder=%d lease=%d publisher=%d", pcm.closed.Load(), enc.closed.Load(), lease.released.Load(), pub.closed.Load())
			}
		})
	}
}
