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

	"github.com/jonasthim/dilla/internal/bot/music"
	"github.com/jonasthim/dilla/internal/bot/music/persistence"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/bot/service"
	"github.com/jonasthim/dilla/internal/id"
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
