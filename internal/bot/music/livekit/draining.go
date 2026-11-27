package livekit

import "context"

// A track whose input ended still plays the tail its bounded queue holds (up to a second). The pool keeps that track
// published, in a draining slot beside the active one, until the tail has played out; the next track is saved, started
// and published meanwhile, and holds its first sample until the draining one played out, so the two never overlap.
// Each track keeps its own publication and its own sender, with its own counter block, until it is unpublished: the
// frame keying is per track as before.

// inputEnder is a source whose input can end before its audio does (runtime.Session).
type inputEnder interface{ InputDone() <-chan struct{} }

// drainer is a source that reports when its last queued sample was handed out (runtime.Session).
type drainer interface{ Drained() <-chan struct{} }

// sampleHolder is a source whose first sample can wait for a gate (runtime.Session).
type sampleHolder interface{ HoldFirstSample(<-chan struct{}) }

func inputEnded(source sourceLifecycle) bool {
	ender, ok := source.(inputEnder)
	if !ok {
		return false
	}
	select {
	case <-ender.InputDone():
		return true
	default:
		return false
	}
}

// demoteLocked moves the active track to the draining slot once its input ended (and it is not paused), when the
// slot is free.
func (p *Pool) demoteLocked() {
	a := p.active
	if a == nil || p.draining != nil || !inputEnded(a.source) || sourceIsPaused(a.source) {
		return
	}
	p.active, p.draining = nil, a
}

// waitSlotLocked returns with p.mu held once a new publication may take the active slot: nothing is active or
// starting, or the active track's input ended and it moved to the draining slot. When the draining slot is still
// taken by the track before it (a track shorter than its queue), it waits for that one to end, or for ctx. Anything
// else active is ErrPoolBusy.
func (p *Pool) waitSlotLocked(ctx context.Context) error {
	for {
		if p.closed || p.dead {
			return p.currentErrorLocked()
		}
		p.demoteLocked()
		if p.active == nil && !p.connecting {
			return nil
		}
		if p.connecting || p.draining == nil || !inputEnded(p.active.source) || sourceIsPaused(p.active.source) {
			return ErrPoolBusy
		}
		wait := p.draining.done
		p.mu.Unlock()
		select {
		case <-wait:
			p.mu.Lock()
		case <-ctx.Done():
			p.mu.Lock()
			return ctx.Err()
		}
	}
}

// playedOut closes once the session's last queued sample was handed out or the session ended (a skip or stop ends a
// draining track at once).
func (s *Session) playedOut() <-chan struct{} {
	d, ok := s.source.(drainer)
	if !ok {
		return s.done
	}
	out := make(chan struct{})
	go func() {
		select {
		case <-d.Drained():
		case <-s.done:
		}
		close(out)
	}()
	return out
}
