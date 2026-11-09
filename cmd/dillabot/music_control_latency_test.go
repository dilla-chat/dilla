//go:build linux

package main

import (
	"context"
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
