//go:build linux

package main

import (
	"context"
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/id"
)

// helperStarts runs native helper starts off the Bot's loop (fix round, I-2). Opening an upgraded v1 store runs a
// one-off cleanup in the helper that may take minutes (helper.Options.OpenTimeout, 10 min). A start inside the pass
// held the loop for all of it: no text commands, due reminders, reloads or passes for any other installation.
//
// A pass that needs a new helper begins its start here and leaves the installation pending; the start asks for a pass
// of that installation when it ends, ok or not, and that pass takes the result. One installation has at most one start
// in flight, and a start ends with the Bot (its context is the Bot's) or with its installation (retain).
type helperStarts struct {
	mu sync.Mutex
	// ctx is the Bot's: shutdown cancels every start.
	ctx context.Context
	// finished asks for a pass of an installation whose start ended (botGatewayRuntime.requestPass).
	finished func(id.ID)
	entries  map[id.ID]*helperStart
	closed   bool
	wg       sync.WaitGroup
}

// helperStart is one installation's start: in flight until done, then its result until a pass takes it.
type helperStart struct {
	cancel  context.CancelFunc
	done    bool
	dropped bool // its installation went away (or the Bot is stopping): the result is closed, never handed out
	client  *native.Client
	err     error
}

// helperStartRecheck is how long a pass leaves an installation whose helper is still starting before it looks again.
// The start's own ask for a pass normally comes first; this is the net under it.
const helperStartRecheck = 5 * time.Minute

// take returns installation's finished start, once: its client, or why it failed. pending reports a start in flight;
// with neither pending nor finished, no start was begun.
func (s *helperStarts) take(installation id.ID) (client *native.Client, pending, finished bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[installation]
	switch {
	case e == nil:
		return nil, false, false, nil
	case !e.done:
		return nil, true, false, nil
	}
	delete(s.entries, installation)
	return e.client, false, true, e.err
}

// passHelper is a pass's native helper for an installation that has no usable retained one: the result of its
// finished start (the client, or why the start failed), or pending while a start is in flight. With none of these, the
// pass begins a start (beginPassHelper) and leaves the installation pending: the pass the start asks for when it ends
// takes the result.
func passHelper(starts *helperStarts, installation id.ID) (client *native.Client, pending bool, err error) {
	client, pending, _, err = starts.take(installation)
	return client, pending, err
}

// beginPassHelper begins installation's start with key, which it consumes: start owns it, or it is cleared here when no
// start began (one is in flight, or the Bot is stopping).
func beginPassHelper(starts *helperStarts, installation id.ID, key []byte, start func(context.Context, []byte) (*native.Client, error)) {
	if !starts.begin(installation, func(ctx context.Context) (*native.Client, error) {
		defer clear(key)
		return start(ctx, key)
	}) {
		clear(key)
	}
}

// begin starts installation's helper with start, in its own goroutine, unless a start is in flight or the Bot is
// stopping. It reports whether it began one.
func (s *helperStarts) begin(installation id.ID, start func(context.Context) (*native.Client, error)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.entries[installation] != nil {
		return false
	}
	parent := s.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	e := &helperStart{cancel: cancel}
	if s.entries == nil {
		s.entries = map[id.ID]*helperStart{}
	}
	s.entries[installation] = e
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		client, err := start(ctx)
		cancel()
		s.mu.Lock()
		dropped := e.dropped
		if dropped {
			if s.entries[installation] == e {
				delete(s.entries, installation)
			}
		} else {
			e.done, e.client, e.err = true, client, err
		}
		closed := s.closed
		s.mu.Unlock()
		if dropped && client != nil {
			_ = client.CloseWithReason(helper.ReasonStartDropped)
		}
		if !closed && s.finished != nil {
			s.finished(installation)
		}
	}()
	return true
}

// retain cancels the starts of the installations not in keep and closes their finished helpers.
func (s *helperStarts) retain(keep map[id.ID]bool) {
	s.mu.Lock()
	var closing []*native.Client
	for installation, e := range s.entries {
		if keep[installation] {
			continue
		}
		if e.done {
			if e.client != nil {
				closing = append(closing, e.client)
			}
			delete(s.entries, installation)
			continue
		}
		// It stays in entries until its goroutine ends, so no second start of the installation can begin meanwhile.
		e.dropped = true
		e.cancel()
	}
	s.mu.Unlock()
	for _, c := range closing {
		_ = c.CloseWithReason(helper.ReasonStartDropped)
	}
}

// close cancels every start, waits for them to end and begins no more.
func (s *helperStarts) close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.retain(nil)
	s.wg.Wait()
}
