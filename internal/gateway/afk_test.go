package gateway

import (
	"context"
	"sync"
	"testing"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/id"
)

// connectAs registers a ready connection of device for user (the harness's connect gives every
// device a fresh user).
func (h *harness) connectAs(t *testing.T, user, device id.ID) *conn {
	t.Helper()
	sink := newRecordingSink(1024, false)
	session := auth.Session{DeviceID: device, UserID: user, Scope: auth.ScopeEnrolled}
	h.mu.Lock()
	h.tokens[h.tokenFor(device)] = session
	h.mu.Unlock()
	c, err := h.gw.register(context.Background(), session, sink)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	h.mu.Lock()
	h.sinks[c] = sink
	h.mu.Unlock()
	return c
}

// offlineRecorder records every OnUserOffline call.
type offlineRecorder struct {
	mu    sync.Mutex
	users []id.ID
}

func (r *offlineRecorder) record(u id.ID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.users = append(r.users, u)
}

func (r *offlineRecorder) calls() []id.ID {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]id.ID(nil), r.users...)
}

// Q2 (AW2): OnUserOffline runs when a user's LAST live connection leaves the registry, once, and
// not while a second tab (another connection, same or another device) is still open.
// UserConnected answers the same predicate.
func TestOnUserOfflineFiresOnTheLastConnectionOnly(t *testing.T) {
	h := newHarness(t)
	rec := &offlineRecorder{}
	h.gw.opts.OnUserOffline = rec.record
	user := id.New()
	laptop, phone := id.New(), id.New()
	tab1 := h.connectAs(t, user, laptop)
	tab2 := h.connectAs(t, user, laptop)
	other := h.connectAs(t, user, phone)
	if !h.gw.UserConnected(user) {
		t.Fatal("UserConnected = false with three open connections")
	}
	h.suspend(tab1)
	h.suspend(other)
	if got := rec.calls(); len(got) != 0 {
		t.Fatalf("OnUserOffline ran %v while a second tab is open", got)
	}
	if !h.gw.UserConnected(user) {
		t.Fatal("UserConnected = false with one tab open")
	}
	h.suspend(tab2)
	if got := rec.calls(); len(got) != 1 || got[0] != user {
		t.Fatalf("OnUserOffline calls = %v, want [%s] once", got, user)
	}
	if h.gw.UserConnected(user) {
		t.Fatal("UserConnected = true with no connection")
	}
	// A connection removed twice (CloseDevice then readLoop's suspend) does not run it again.
	h.suspend(tab2)
	if got := rec.calls(); len(got) != 1 {
		t.Fatalf("OnUserOffline calls after a double remove = %v, want one", got)
	}
}

// R-AW5: a shutdown closes every connection, and that is no user going offline: the stored away
// roster must survive a restart for users who reconnect, so Shutdown never runs OnUserOffline.
func TestShutdownDoesNotRunOnUserOffline(t *testing.T) {
	h := newHarness(t)
	rec := &offlineRecorder{}
	h.gw.opts.OnUserOffline = rec.record
	h.connectAs(t, id.New(), id.New())
	if err := h.gw.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := rec.calls(); len(got) != 0 {
		t.Fatalf("Shutdown ran OnUserOffline for %v", got)
	}
}

// Q1 (AW2): an accepted typing_set on reaches OnTyping with the user and the channel; a refused one
// (hidden channel) and a typing off do not.
func TestOnTypingRunsForAnAcceptedTypingOn(t *testing.T) {
	h := newHarness(t)
	a := h.connect(t, id.New())
	h.drainReady(t, a)
	visible, hidden := id.New(), id.New()
	h.gw.opts.TypingAudience = func(_ context.Context, _, ch id.ID) ([]id.ID, error) {
		if ch != visible {
			return nil, nil
		}
		return []id.ID{a.userID}, nil
	}
	var mu sync.Mutex
	var seen [][2]id.ID
	h.gw.opts.OnTyping = func(_ context.Context, user, ch id.ID) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, [2]id.ID{user, ch})
	}
	h.gw.handle(context.Background(), a, ephemeralInbound(t, OpTypingSet, hidden, uint64(1)))
	expectErrorCode(t, h, a.deviceID, "E_NOT_FOUND")
	h.gw.handle(context.Background(), a, ephemeralInbound(t, OpTypingSet, visible, uint64(1)))
	if got := h.waitFrame(t, a.deviceID); got.Op != OpTyping {
		t.Fatalf("op = %d, want typing", got.Op)
	}
	h.clk.Advance(4e9)
	h.gw.handle(context.Background(), a, ephemeralInbound(t, OpTypingSet, visible, uint64(0)))
	if got := h.waitFrame(t, a.deviceID); got.Op != OpTyping {
		t.Fatalf("op = %d, want typing", got.Op)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != [2]id.ID{a.userID, visible} {
		t.Fatalf("OnTyping calls = %v, want one for (%s, %s)", seen, a.userID, visible)
	}
}
