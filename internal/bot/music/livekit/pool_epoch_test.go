package livekit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/sframe"
)

// movingEpochs is the helper's call epoch, moved by the test (a member's commit applied by the catch-up).
type movingEpochs struct {
	mu    sync.Mutex
	epoch native.CallMediaEpoch
	err   error
	state *counterStateStub
}

func (e *movingEpochs) CallMediaEpoch(context.Context, id.ID) (native.CallMediaEpoch, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.epoch, e.err
}
func (e *movingEpochs) set(epoch native.CallMediaEpoch, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.epoch, e.err = epoch, err
}
func (e *movingEpochs) StateGet(ctx context.Context, version uint64, key []byte) (native.StateValue, error) {
	return e.state.StateGet(ctx, version, key)
}
func (e *movingEpochs) StateTransaction(ctx context.Context, tx id.ID, changes []native.StateChange) ([]uint64, error) {
	return e.state.StateTransaction(ctx, tx, changes)
}

func epochAt(n uint64, key byte, members ...uint64) native.CallMediaEpoch {
	e := native.CallMediaEpoch{Epoch: n, OwnLeaf: 1, BaseKey: [16]byte{key}}
	for _, leaf := range members {
		e.Roster = append(e.Roster, native.CallMediaMember{Leaf: leaf})
	}
	return e
}

func frameKID(t *testing.T, s *sframe.Sender) uint64 {
	t.Helper()
	frame, err := s.Encrypt(sframe.Opus, sframe.Mic, 0, []byte{0xfc, 1, 2})
	if err != nil {
		t.Fatal(err)
	}
	kid, _, _, err := sframe.DecodeHeader(frame)
	if err != nil {
		t.Fatal(err)
	}
	return kid
}

// C1: a member leaves (the commit moves the call from epoch 4 to 5 and removes leaf 2): the next frame is under epoch
// 5's key, so the removed member, who holds only epoch 4's, cannot decrypt anything the Bot sends after the commit.
func TestQueuedTrackRekeysAfterAMemberLeaves(t *testing.T) {
	group := testID(9)
	epochs := &movingEpochs{epoch: epochAt(4, 4, 1, 2), state: &counterStateStub{}}
	sender, err := senderForReservedEpoch(context.Background(), epochs, group, epochs.epoch)
	if err != nil {
		t.Fatal(err)
	}
	s := &Session{sender: sender}
	caughtUp := 0
	guards := CallGuards{CatchUp: func(context.Context, id.ID) error { caughtUp++; return nil }}
	failures := 0
	active, err := s.followEpoch(context.Background(), guards, group, epochs, epochs.epoch, &failures)
	if err != nil || active.Epoch != 4 || frameKID(t, sender) != sframe.KID(1, 4) {
		t.Fatalf("unchanged epoch: active=%d err=%v", active.Epoch, err)
	}
	epochs.set(epochAt(5, 5, 1), nil) // leaf 2 removed
	active, err = s.followEpoch(context.Background(), guards, group, epochs, active, &failures)
	if err != nil || active.Epoch != 5 {
		t.Fatalf("after the commit: active=%d err=%v", active.Epoch, err)
	}
	if kid := frameKID(t, sender); kid != sframe.KID(1, 5) {
		t.Fatalf("the frame after the commit has kid %d, want epoch 5's %d", kid, sframe.KID(1, 5))
	}
	if caughtUp != 2 {
		t.Fatalf("catch-ups = %d, want one per check", caughtUp)
	}
}

func TestATrackStopsWhenItsCallCannotBeFollowed(t *testing.T) {
	group := testID(9)
	start := func(t *testing.T) (*Session, *movingEpochs) {
		epochs := &movingEpochs{epoch: epochAt(4, 4, 1, 2), state: &counterStateStub{}}
		sender, err := senderForReservedEpoch(context.Background(), epochs, group, epochs.epoch)
		if err != nil {
			t.Fatal(err)
		}
		return &Session{sender: sender}, epochs
	}
	t.Run("the Bot's own leaf was removed", func(t *testing.T) {
		s, epochs := start(t)
		epochs.set(native.CallMediaEpoch{}, errors.New("call epoch own leaf missing"))
		failures := 0
		if _, err := s.followEpoch(context.Background(), CallGuards{}, group, epochs, epochs.epoch, &failures); err == nil {
			t.Fatal("publishing went on without the Bot's leaf")
		}
	})
	t.Run("a held user is in the new roster", func(t *testing.T) {
		s, epochs := start(t)
		held := errors.New("held")
		guards := CallGuards{Admit: func(_ context.Context, e native.CallMediaEpoch) error {
			if len(e.Roster) > 2 {
				return held
			}
			return nil
		}}
		initial := epochs.epoch
		epochs.set(epochAt(5, 5, 1, 2, 3), nil)
		failures := 0
		if _, err := s.followEpoch(context.Background(), guards, group, epochs, initial, &failures); !errors.Is(err, held) {
			t.Fatalf("admission = %v", err)
		}
		if kid := frameKID(t, s.sender); kid != sframe.KID(1, 4) {
			t.Fatal("the sender moved to an epoch that was not admitted")
		}
	})
	t.Run("the call group stays behind", func(t *testing.T) {
		s, epochs := start(t)
		down := errors.New("unavailable")
		guards := CallGuards{CatchUp: func(context.Context, id.ID) error { return down }}
		failures := 0
		for i := 1; i < maxCatchUpFailures; i++ {
			if _, err := s.followEpoch(context.Background(), guards, group, epochs, epochs.epoch, &failures); err != nil {
				t.Fatalf("failure %d stopped the track early: %v", i, err)
			}
		}
		if _, err := s.followEpoch(context.Background(), guards, group, epochs, epochs.epoch, &failures); !errors.Is(err, ErrCallBehind) {
			t.Fatalf("after %d failures = %v, want ErrCallBehind", maxCatchUpFailures, err)
		}
		terminal := CallGuards{CatchUp: func(context.Context, id.ID) error { return ErrCallCatchUpTerminal }}
		failures = 0
		if _, err := s.followEpoch(context.Background(), terminal, group, epochs, epochs.epoch, &failures); !errors.Is(err, ErrCallBehind) {
			t.Fatalf("terminal catch-up = %v", err)
		}
	})
}

// A hint for the call group re-keys a playing track at once, without waiting for the tick.
func TestAHintRekeysAPlayingTrack(t *testing.T) {
	p, err := NewPool(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	p.backend = &poolBackendStub{}
	call, group := poolCall(51, 52)
	epochs := &movingEpochs{epoch: epochAt(4, 4, 1, 2), state: &counterStateStub{}}
	var mu sync.Mutex
	var nudge func()
	p.SetCallGuards(CallGuards{Watch: func(_ id.ID, n func()) func() {
		mu.Lock()
		nudge = n
		mu.Unlock()
		return func() {}
	}})
	session, err := p.startSource(context.Background(), call, group, epochs, newPoolSource())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return nudge != nil })
	epochs.set(epochAt(5, 5, 1), nil)
	mu.Lock()
	nudge()
	mu.Unlock()
	deadline := time.Now().Add(time.Second)
	for frameKID(t, session.sender) != sframe.KID(1, 5) {
		if time.Now().After(deadline) {
			t.Fatal("the hint did not re-key the track")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
