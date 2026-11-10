package service

import (
	"context"
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/music/persistence"
	"github.com/dilla-chat/dilla/internal/id"
)

// MusicStateReadWindow is how long one music.state computation answers the reads of a call's members (Bot v2 Task 19
// fix round 4, Task 21b ruling 3): a call hint makes every member of the call read the state at once.
const MusicStateReadWindow = 100 * time.Millisecond

// musicStateComputeTimeout bounds a shared computation, which runs apart from every reader's context.
const musicStateComputeTimeout = 10 * time.Second

// MusicStateReads holds the shared part of recent music.state reads per voice channel (the call scope), in two
// stages computed at most once per window each: the call (the current call and who is in it, which every reader's
// authorisation needs), and the queue state, computed only once a reader has been authorised (fix round 5, M4-3).
// Each member's read is still authorised on its own (CanUse, CanDJ, from the feed state and the shared call state)
// and encoded on its own (canControl differs). A computation runs apart from the reader that started it, under its
// own deadline: a reader that goes away fails only its own read. A committed change through the music handlers ends
// the channel's window.
type MusicStateReads struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[id.ID]*musicStateRead
}

type musicStateStage struct {
	done chan struct{}
	err  error
}

type musicStateRead struct {
	at       time.Time
	call     *musicStateStage
	shared   musicCallShared
	snapshot *musicStateStage
	state    persistence.Snapshot
}

type musicCallShared struct {
	call      MusicCallBinding
	callState *client.ChannelCallState
}

// NewMusicStateReads is an empty cache on the system clock.
func NewMusicStateReads() *MusicStateReads {
	return &MusicStateReads{now: time.Now, entries: map[id.ID]*musicStateRead{}}
}

func stageDone(s *musicStateStage) bool {
	if s == nil {
		return true
	}
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// stage runs compute at most once per window for channel's stage (pick), apart from every reader, and waits for it
// under ctx. A failed computation answers the reads waiting on it and is not kept.
func (c *MusicStateReads) stage(ctx context.Context, channel id.ID, pick func(*musicStateRead) **musicStateStage, compute func(context.Context, *musicStateRead) error) (*musicStateRead, error) {
	c.mu.Lock()
	now := c.now()
	for k, e := range c.entries {
		if now.Sub(e.at) >= MusicStateReadWindow && stageDone(e.call) && stageDone(e.snapshot) {
			delete(c.entries, k)
		}
	}
	e, ok := c.entries[channel]
	if !ok || now.Sub(e.at) >= MusicStateReadWindow {
		e = &musicStateRead{at: now}
		c.entries[channel] = e
	}
	slot := pick(e)
	s := *slot
	if s == nil {
		s = &musicStateStage{done: make(chan struct{})}
		*slot = s
		detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), musicStateComputeTimeout)
		go func() {
			defer cancel()
			s.err = compute(detached, e)
			close(s.done)
			if s.err != nil {
				c.mu.Lock()
				if *slot == s {
					*slot = nil
				}
				c.mu.Unlock()
			}
		}()
	}
	c.mu.Unlock()
	select {
	case <-s.done:
		return e, s.err
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}

// forget ends channel's window (a committed change).
func (c *MusicStateReads) forget(channel id.ID) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.entries, channel)
	c.mu.Unlock()
}

// readSnapshot is the durable queue state reconciled with the retained supervisor's; mismatch reports that their
// revisions differed. reason names the step that failed.
func (h MusicInteractions) readSnapshot(ctx context.Context, channel id.ID) (snapshot persistence.Snapshot, mismatch bool, reason string, err error) {
	reason = "state_read"
	snapshot, err = h.Store.Load(ctx, channel)
	if err != nil || h.CurrentState == nil {
		return snapshot, false, reason, err
	}
	reason = "runtime_state_read"
	state, revision, err := h.CurrentState(ctx, h.InstallationID, h.CommunityID, channel)
	if err != nil {
		return snapshot, false, reason, err
	}
	if revision != snapshot.Revision || state.Revision != snapshot.Revision {
		// A playback transition may have committed since the first read. State reads
		// always have a durable answer, and a missed hand-off adopts without starting.
		reason = "state_read"
		snapshot, err = h.Store.Load(ctx, channel)
		if err != nil {
			return snapshot, false, reason, err
		}
		if revision < snapshot.Revision && h.OnPersistedSnapshot != nil {
			_, _ = h.OnPersistedSnapshot(ctx, h.InstallationID, h.CommunityID, channel, snapshot.State, snapshot.Revision, false)
		}
	}
	if revision == snapshot.Revision && state.Revision == snapshot.Revision {
		snapshot.State = state
	}
	return snapshot, revision != snapshot.Revision || state.Revision != snapshot.Revision, reason, nil
}

// coalescedState answers one member's music.state from the call's shared reads: the member is authorised on their own
// before the queue state is read for anyone, and gets their own encoding.
func (h MusicInteractions) coalescedState(ctx context.Context, actor InteractionActor, channel id.ID, reason *string, group *id.ID) (InteractionResult, error) {
	*reason = "shared_call_read"
	read, err := h.StateReads.stage(ctx, channel, func(e *musicStateRead) **musicStateStage { return &e.call },
		func(detached context.Context, e *musicStateRead) error {
			call, err := h.Authority.CurrentCall(detached, channel)
			if err != nil {
				return err
			}
			e.shared = musicCallShared{call: call}
			if state, ok := musicCallMemoState(detached, channel); ok {
				e.shared.callState = &state
			}
			return nil
		})
	if err != nil {
		return InteractionResult{}, err
	}
	shared := read.shared
	*group = shared.call.GroupID
	*reason = "no_current_call"
	if !validMusicCall(shared.call, channel) {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	if shared.callState != nil {
		seedMusicCallMemo(ctx, channel, *shared.callState)
	}
	*reason = "can_use_lookup"
	allowed, err := h.Authority.CanUse(ctx, actor, shared.call)
	if err != nil {
		return InteractionResult{}, err
	}
	*reason = "forbidden"
	if !allowed {
		return InteractionResult{Status: interactionStatusForbidden}, nil
	}
	*reason = "shared_state_read"
	read, err = h.StateReads.stage(ctx, channel, func(e *musicStateRead) **musicStateStage { return &e.snapshot },
		func(detached context.Context, e *musicStateRead) error {
			snapshot, _, _, err := h.readSnapshot(detached, channel)
			e.state = snapshot
			return err
		})
	if err != nil {
		return InteractionResult{}, err
	}
	*reason = "can_dj_lookup"
	canControl, err := h.Authority.CanDJ(ctx, actor, shared.call)
	if err != nil {
		return InteractionResult{}, err
	}
	*reason = "state_encode"
	data, err := encodeMusicSnapshot(read.state.State, canControl, h.clock().UnixMilli())
	if err != nil {
		return InteractionResult{}, err
	}
	revision := read.state.Revision
	return InteractionResult{Status: 0, Revision: &revision, Data: data}, nil
}
