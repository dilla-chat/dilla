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

// MusicStateReads holds the shared part of recent music.state reads per voice channel (the call scope): the current
// call, its call state (who is in it) and the queue state. Each member's read is still authorised on its own (CanUse,
// CanDJ, from the feed state and the shared call state) and encoded on its own (canControl differs). A committed
// change through the music handlers ends the channel's window.
type MusicStateReads struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[id.ID]*musicStateRead
}

type musicStateRead struct {
	at    time.Time
	done  chan struct{}
	value musicStateShared
	err   error
}

type musicStateShared struct {
	call      MusicCallBinding
	callState *client.ChannelCallState
	snapshot  persistence.Snapshot
}

// NewMusicStateReads is an empty cache on the system clock.
func NewMusicStateReads() *MusicStateReads {
	return &MusicStateReads{now: time.Now, entries: map[id.ID]*musicStateRead{}}
}

// read is channel's shared state, computed at most once per window however many members read it. A failed
// computation answers the reads that waited on it and is not kept.
func (c *MusicStateReads) read(channel id.ID, compute func() (musicStateShared, error)) (musicStateShared, error) {
	c.mu.Lock()
	now := c.now()
	for k, e := range c.entries {
		if now.Sub(e.at) >= MusicStateReadWindow {
			select {
			case <-e.done:
				delete(c.entries, k)
			default:
			}
		}
	}
	e, ok := c.entries[channel]
	if !ok || now.Sub(e.at) >= MusicStateReadWindow {
		e = &musicStateRead{at: now, done: make(chan struct{})}
		c.entries[channel] = e
		c.mu.Unlock()
		e.value, e.err = compute()
		close(e.done)
		if e.err != nil {
			c.mu.Lock()
			if c.entries[channel] == e {
				delete(c.entries, channel)
			}
			c.mu.Unlock()
		}
		return e.value, e.err
	}
	c.mu.Unlock()
	<-e.done
	return e.value, e.err
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

// coalescedState answers one member's music.state from the call's shared read: the member is authorised on their own
// and gets their own encoding.
func (h MusicInteractions) coalescedState(ctx context.Context, actor InteractionActor, channel id.ID, reason *string, group *id.ID) (InteractionResult, error) {
	*reason = "shared_state_read"
	shared, err := h.StateReads.read(channel, func() (musicStateShared, error) {
		call, err := h.Authority.CurrentCall(ctx, channel)
		if err != nil {
			return musicStateShared{}, err
		}
		out := musicStateShared{call: call}
		if state, ok := musicCallMemoState(ctx, channel); ok {
			out.callState = &state
		}
		if !validMusicCall(call, channel) {
			return out, nil
		}
		out.snapshot, _, _, err = h.readSnapshot(ctx, channel)
		return out, err
	})
	if err != nil {
		return InteractionResult{}, err
	}
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
	*reason = "can_dj_lookup"
	canControl, err := h.Authority.CanDJ(ctx, actor, shared.call)
	if err != nil {
		return InteractionResult{}, err
	}
	*reason = "state_encode"
	data, err := encodeMusicSnapshot(shared.snapshot.State, canControl, h.clock().UnixMilli())
	if err != nil {
		return InteractionResult{}, err
	}
	revision := shared.snapshot.Revision
	return InteractionResult{Status: 0, Revision: &revision, Data: data}, nil
}
