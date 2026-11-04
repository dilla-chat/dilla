//go:build linux

package main

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/music"
	"github.com/dilla-chat/dilla/internal/bot/music/persistence"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/bot/service"
	"github.com/dilla-chat/dilla/internal/id"
)

type controlLatencyPlayback struct {
	closedAt atomic.Int64
	done     chan struct{}
	once     sync.Once
}

func (p *controlLatencyPlayback) Done() <-chan struct{} { return p.done }
func (p *controlLatencyPlayback) Wait() error           { return nil }
func (p *controlLatencyPlayback) Close() error {
	p.once.Do(func() { p.closedAt.Store(time.Now().UnixNano()); close(p.done) })
	return nil
}

type controlLatencyStarter struct {
	block     bool
	entered   chan struct{}
	cancelled chan struct{}
	release   chan struct{}
	playback  *controlLatencyPlayback
	once      sync.Once
	calls     atomic.Int32
}

func (s *controlLatencyStarter) Start(ctx context.Context, _ music.Track) (music.Playback, error) {
	s.once.Do(func() { close(s.entered) })
	if s.block {
		<-ctx.Done()
		close(s.cancelled)
		// Deliberately slow provider cleanup must not hold the control response.
		<-s.release
		return nil, ctx.Err()
	}
	if s.calls.Add(1) > 1 {
		return &controlLatencyPlayback{done: make(chan struct{})}, nil
	}
	return s.playback, nil
}

// This Bot-internal bound starts at the gateway wake and ends at the audio boundary,
// using the real handler, registry, supervisor, persistence store and command workers.
// TestClientMusicControlsEndToEnd measures the actual client invocation with SQLite
// and the production source synchronization and durable-event poller.
func TestImmediateMusicControlsChangeAudioWithinOneSecond(t *testing.T) {
	testMusicControlLatency(t, false)
}

func TestPauseAndSkipUnderOneSecondDuringSweep(t *testing.T) { testMusicControlLatency(t, true) }

func testMusicControlLatency(t *testing.T, duringSweep bool) {
	for _, test := range []struct {
		name     string
		op       protocol.Operation
		starting bool
	}{
		{"pause_during_playback", protocol.OpMusicPause, false},
		{"skip_during_playback", protocol.OpMusicSkip, false},
		{"pause_during_start", protocol.OpMusicPause, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			op := test.op
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			installation, community, channel, group := id.New(), id.New(), id.New(), id.New()
			n := &runtimeStateNative{binding: native.Binding{Instance: id.New(), Community: community, Installation: installation}, rows: map[string]native.StateValue{}}
			store, err := persistence.New(n, community)
			if err != nil {
				t.Fatal(err)
			}
			starter := &controlLatencyStarter{block: test.starting, entered: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{}), playback: &controlLatencyPlayback{done: make(chan struct{})}}
			supervisor, err := music.NewSupervisor(ctx, channel, persistedMusicQueue{store}, starter)
			if err != nil {
				t.Fatal(err)
			}
			defer supervisor.Close()
			// Release cleanup before Close, including on any failed assertion.
			defer close(starter.release)
			queues := &musicQueueRuntime{store: store, community: community, supervisors: map[id.ID]*music.Supervisor{channel: supervisor}}
			registry := &musicQueueRegistry{runtimes: map[id.ID]*retainedMusicRuntime{installation: {community: community, queues: queues, native: n}}}
			state, err := music.Apply(music.InitialState(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{
				{ID: "first", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "First", RequesterID: "member"},
				{ID: "next", Provider: music.ProviderYouTube, CanonicalID: "bcdefghijkl", Title: "Next", RequesterID: "member"},
			}})
			if err != nil {
				t.Fatal(err)
			}
			saved, err := store.Save(ctx, channel, 0, state)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = registry.schedulePersistedSnapshot(ctx, installation, community, channel, saved.State, saved.Revision, false); err != nil {
				t.Fatal(err)
			}
			h := service.MusicInteractions{Store: store, Authority: probeAuthority{call: service.MusicCallBinding{CallID: channel, ChannelID: channel, GroupID: id.New(), Active: true}}, InstallationID: installation, CommunityID: community, CurrentState: registry.CurrentState, OnPersistedSnapshot: musicSnapshotCommitCallback(registry, nil), OnImmediateControl: registry.applyImmediateControl}
			runtime := newBotGatewayRuntime(ctx, nil, true, slog.New(slog.NewTextHandler(io.Discard, nil)))
			requests := make(chan protocol.Request, 1)
			results := make(chan service.InteractionResult, 1)
			failures := make(chan error, 1)
			runtime.SetInteractionPoller(installation, func(ctx context.Context, _ id.ID) error {
				select {
				case req := <-requests:
					result, err := h.Handlers()[req.Operation].Handle(ctx, service.InteractionActor{User: id.New(), Device: id.New()}, req)
					if err != nil {
						failures <- err
						return err
					}
					results <- result
				default:
				}
				return nil
			})
			runtime.StartCommandWorkers()
			t.Cleanup(runtime.Close)
			var sweepDone <-chan struct{}
			if duringSweep {
				sweepDone = blockedSweep(t, runtime)
			}
			if starter.block {
				revision := saved.Revision
				requests <- protocol.Request{RequestID: id.New(), CommunityID: community, ChannelID: &channel, Operation: protocol.OpMusicResume, Revision: &revision}
				runtime.notifyInteraction(installation, group)
				select {
				case res := <-results:
					if res.Status != 0 {
						t.Fatalf("resume status %d", res.Status)
					}
				case err := <-failures:
					t.Fatal(err)
				case <-time.After(3 * time.Second):
					t.Fatal("resume blocked the interaction drain")
				}
			} else {
				// Synchronous fixture setup returns only after the supervisor has
				// installed active playback. The timed skip still uses the real
				// asynchronous production handler through the gateway wake.
				if _, err := supervisor.Apply(ctx, saved.Revision, music.Operation{Kind: music.OpResume}); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-starter.entered:
			case <-time.After(time.Second):
				t.Fatal("start never entered")
			}
			// Drain this group's entire allowance. A control still reaches audio inside
			// one second at the per-group 2/s fairness bound, including command worker scheduling.
			runtime.pendingMu.Lock()
			budget := runtime.budgetLocked(installation, group)
			budget.refill(time.Now())
			budget.tokens = 0
			runtime.pendingMu.Unlock()
			stale := uint64(0) // Bot-owned advancement can never conflict with this control.
			requests <- protocol.Request{RequestID: id.New(), CommunityID: community, ChannelID: &channel, Operation: op, Revision: &stale}
			started := time.Now()
			runtime.notifyInteraction(installation, group)
			audioChanged := starter.playback.done
			if starter.block {
				audioChanged = starter.cancelled
			}
			select {
			case <-audioChanged:
			case err := <-failures:
				t.Fatal(err)
			case <-time.After(time.Second):
				t.Fatal("audio unchanged after one second")
			}
			elapsed := time.Since(started)
			if duringSweep {
				select {
				case <-sweepDone:
					t.Fatal("sweep ended before audio changed")
				default:
				}
			}
			t.Logf("%s request-to-audio latency: %v", map[protocol.Operation]string{protocol.OpMusicPause: "pause", protocol.OpMusicSkip: "skip"}[op], elapsed)
			if elapsed >= time.Second {
				t.Fatalf("control latency %v must be <1s", elapsed)
			}
			select {
			case res := <-results:
				if res.Status != 0 {
					t.Fatalf("control status %d", res.Status)
				}
			case err := <-failures:
				t.Fatal(err)
			case <-time.After(100 * time.Millisecond):
				t.Fatal("control response waited behind start cleanup")
			}
		})
	}
}

type warmControlPlayback struct {
	controlLatencyPlayback
	paused  atomic.Bool
	resumed chan struct{}
}

func (p *warmControlPlayback) Pause()          { p.paused.Store(true) }
func (p *warmControlPlayback) Resume() error   { p.paused.Store(false); close(p.resumed); return nil }
func (*warmControlPlayback) PositionMS() int64 { return 42000 }

type warmControlStarter struct {
	calls atomic.Int32
	p     *warmControlPlayback
}

func (s *warmControlStarter) Start(ctx context.Context, _ music.Track) (music.Playback, error) {
	if s.calls.Add(1) > 1 {
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return s.p, nil
}
func TestWarmMusicControlLatency(t *testing.T) {
	t.Run("resume_after_pause", func(t *testing.T) {
		installation, community, channel := id.New(), id.New(), id.New()
		n := &runtimeStateNative{binding: native.Binding{Instance: id.New(), Community: community, Installation: installation}, rows: map[string]native.StateValue{}}
		store, err := persistence.New(n, community)
		if err != nil {
			t.Fatal(err)
		}
		p := &warmControlPlayback{controlLatencyPlayback: controlLatencyPlayback{done: make(chan struct{})}, resumed: make(chan struct{})}
		starter := &warmControlStarter{p: p}
		s, err := music.NewSupervisor(t.Context(), channel, persistedMusicQueue{store}, starter)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		state, err := s.Apply(t.Context(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{{ID: "a", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "A", RequesterID: "m"}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.Apply(t.Context(), state.Revision, music.Operation{Kind: music.OpResume}); err != nil {
			t.Fatal(err)
		}
		queues := &musicQueueRuntime{store: store, community: community, supervisors: map[id.ID]*music.Supervisor{channel: s}}
		registry := &musicQueueRegistry{runtimes: map[id.ID]*retainedMusicRuntime{installation: {community: community, queues: queues, native: n}}}
		h := service.MusicInteractions{Store: store, Authority: probeAuthority{call: service.MusicCallBinding{CallID: channel, ChannelID: channel, GroupID: id.New(), Active: true}}, InstallationID: installation, CommunityID: community, CurrentState: registry.CurrentState, OnPersistedSnapshot: musicSnapshotCommitCallback(registry, nil), OnImmediateControl: registry.applyImmediateControl}
		actor := service.InteractionActor{User: id.New(), Device: id.New()}
		req := protocol.Request{RequestID: id.New(), CommunityID: community, ChannelID: &channel, Operation: protocol.OpMusicPause}
		res, err := h.Handlers()[req.Operation].Handle(t.Context(), actor, req)
		if err != nil || res.Status != 0 || !p.paused.Load() {
			t.Fatalf("pause status=%d err=%v", res.Status, err)
		}
		rev := s.State().Revision
		req.Operation = protocol.OpMusicResume
		req.RequestID = id.New()
		req.Revision = &rev
		res, err = h.Handlers()[req.Operation].Handle(t.Context(), actor, req)
		replyAt := time.Now()
		if err != nil || res.Status != 0 {
			t.Fatalf("resume status=%d err=%v", res.Status, err)
		}
		select {
		case <-p.resumed:
		case <-time.After(time.Second):
			t.Fatal("audio not resumed within 1s of reply")
		}
		if p.paused.Load() || starter.calls.Load() != 1 || time.Since(replyAt) >= time.Second {
			t.Fatalf("resume cold-started: calls=%d", starter.calls.Load())
		}
	})
}
