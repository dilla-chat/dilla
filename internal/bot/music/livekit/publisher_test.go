package livekit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/music/counter"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/sframe"
)

type epochStub struct {
	called bool
	lastID id.ID
	epoch  native.CallMediaEpoch
	err    error
}

func (s *epochStub) CallMediaEpoch(_ context.Context, groupID id.ID) (native.CallMediaEpoch, error) {
	s.called = true
	s.lastID = groupID
	return s.epoch, s.err
}

func TestEpochLookupUsesBoundGroupNotChannelID(t *testing.T) {
	channelID, groupID := testID(3), testID(4)
	initial := native.CallMediaEpoch{OwnLeaf: 0, BaseKey: [16]byte{1}, Roster: []native.CallMediaMember{{Leaf: 0}}}
	epochs := &epochStub{epoch: initial}
	if err := revalidateEpoch(context.Background(), groupID, epochs, initial); err != nil {
		t.Fatalf("revalidate epoch: %v", err)
	}
	if !epochs.called || epochs.lastID != groupID || epochs.lastID == channelID {
		t.Fatalf("epoch lookup ID = %v, want group ID %v distinct from channel ID %v", epochs.lastID, groupID, channelID)
	}
}

func TestStartRejectsCallOutsideBoundGroupBeforeReadingKeys(t *testing.T) {
	group, other, callID := testID(1), testID(2), testID(3)
	epochs := &epochStub{}
	_, err := Start(context.Background(), client.Call{CallID: callID, GroupID: other}, group, epochs, nil, 0)
	if !errors.Is(err, ErrBinding) {
		t.Fatalf("Start error = %v, want ErrBinding", err)
	}
	if epochs.called {
		t.Fatal("read call epoch before validating group binding")
	}
}

func TestSenderForEpochUsesOwnLeafAndRequiresRosterBinding(t *testing.T) {
	e := native.CallMediaEpoch{Epoch: 7, OwnLeaf: 4, BaseKey: [16]byte{1}, Roster: []native.CallMediaMember{{Leaf: 4}}}
	if err := validateSenderEpoch(e); err != nil {
		t.Fatal(err)
	}
	e.OwnLeaf = 3
	if err := validateSenderEpoch(e); !errors.Is(err, ErrBinding) {
		t.Fatalf("senderForEpoch missing own leaf error = %v, want ErrBinding", err)
	}
}

func TestSenderForEpochAcceptsInitialEpochZero(t *testing.T) {
	e := native.CallMediaEpoch{OwnLeaf: 0, BaseKey: [16]byte{1}, Roster: []native.CallMediaMember{{Leaf: 0}}}
	if err := validateSenderEpoch(e); err != nil {
		t.Fatalf("initial MLS epoch zero rejected: %v", err)
	}
}

type counterStateStub struct{ value native.StateValue }

func (s *counterStateStub) StateGet(_ context.Context, _ uint64, _ []byte) (native.StateValue, error) {
	return s.value, nil
}

func (s *counterStateStub) StateTransaction(_ context.Context, tx id.ID, changes []native.StateChange) ([]uint64, error) {
	if tx.IsZero() || len(changes) != 1 || changes[0].ExpectedRevision != s.value.Revision {
		return nil, errors.New("counter state revision conflict")
	}
	s.value = native.StateValue{Revision: s.value.Revision + 1, Value: append([]byte(nil), changes[0].Value...)}
	return []uint64{s.value.Revision}, nil
}

func TestReservedSenderUsesDisjointCountersAcrossPublisherRestart(t *testing.T) {
	state := &counterStateStub{}
	group := testID(11)
	epoch := native.CallMediaEpoch{Epoch: 9, OwnLeaf: 4, BaseKey: [16]byte{1}, Roster: []native.CallMediaMember{{Leaf: 4}}}
	first, err := senderForReservedEpoch(context.Background(), state, group, epoch)
	if err != nil {
		t.Fatal(err)
	}
	firstFrame, err := first.Encrypt(sframe.Opus, sframe.Mic, 0, []byte{0xfc, 1, 2})
	if err != nil {
		t.Fatal(err)
	}
	second, err := senderForReservedEpoch(context.Background(), state, group, epoch) // simulated new publisher/process
	if err != nil {
		t.Fatal(err)
	}
	secondFrame, err := second.Encrypt(sframe.Opus, sframe.Mic, 0, []byte{0xfc, 1, 2})
	if err != nil {
		t.Fatal(err)
	}
	_, firstCounter, _, err := sframe.DecodeHeader(firstFrame)
	if err != nil {
		t.Fatal(err)
	}
	_, secondCounter, _, err := sframe.DecodeHeader(secondFrame)
	if err != nil {
		t.Fatal(err)
	}
	if firstCounter != 0 || secondCounter != counter.BlockSize {
		t.Fatalf("publisher counters = %d then %d, want 0 then %d", firstCounter, secondCounter, counter.BlockSize)
	}
}

func TestMonitorCleansUpWhenSourceEndsNormally(t *testing.T) {
	source := &sourceStub{done: make(chan struct{})}
	close(source.done)
	room := &roomStub{}
	s := &Session{room: room, source: source, cancel: func() {}, done: make(chan struct{})}
	initial := native.CallMediaEpoch{OwnLeaf: 0, BaseKey: [16]byte{1}, Roster: []native.CallMediaMember{{Leaf: 0}}}
	s.monitor(context.Background(), testID(7), &epochStub{}, initial, time.Minute)
	if err := s.Wait(); err != nil {
		t.Fatalf("normal source completion returned error: %v", err)
	}
	if !room.disconnected || !source.closed {
		t.Fatalf("cleanup incomplete: disconnected=%v sourceClosed=%v", room.disconnected, source.closed)
	}
}

func TestRoomDisconnectCallbackStopsSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	source := &sourceStub{done: make(chan struct{})}
	room := &roomStub{}
	s := &Session{room: room, source: source, cancel: cancel, done: make(chan struct{})}
	initial := native.CallMediaEpoch{OwnLeaf: 0, BaseKey: [16]byte{1}, Roster: []native.CallMediaMember{{Leaf: 0}}}
	go s.monitor(ctx, testID(7), &epochStub{}, initial, time.Minute)
	publisherRoomCallback(cancel).OnDisconnected()
	select {
	case <-s.Done():
	case <-time.After(time.Second):
		t.Fatal("session did not stop after LiveKit disconnected")
	}
	if !room.disconnected || !source.closed {
		t.Fatalf("cleanup incomplete: disconnected=%v sourceClosed=%v", room.disconnected, source.closed)
	}
}

func TestRevalidateEpochFailsClosedOnRotationAndLookupError(t *testing.T) {
	initial := native.CallMediaEpoch{OwnLeaf: 1, BaseKey: [16]byte{1}, Roster: []native.CallMediaMember{{Leaf: 1}}}
	epochs := &epochStub{epoch: initial}
	if err := revalidateEpoch(context.Background(), testID(3), epochs, initial); err != nil {
		t.Fatalf("unchanged epoch rejected: %v", err)
	}
	epochs.epoch.Epoch++
	if !errors.Is(revalidateEpoch(context.Background(), testID(3), epochs, initial), ErrEpochChanged) {
		t.Fatal("rotated epoch was not rejected")
	}
	epochs.err = errors.New("sensitive helper detail")
	err := revalidateEpoch(context.Background(), testID(3), epochs, initial)
	if err == nil || err.Error() == epochs.err.Error() {
		t.Fatalf("lookup failure was not redacted: %v", err)
	}
}

type roomStub struct{ disconnected bool }

func (r *roomStub) Disconnect() { r.disconnected = true }

type sourceStub struct {
	done   chan struct{}
	closed bool
}

func (s *sourceStub) Done() <-chan struct{} { return s.done }
func (s *sourceStub) Wait() error           { return nil }
func (s *sourceStub) Close() error          { s.closed = true; return nil }

func TestEpochStillBoundRejectsEpochLeafOrKeyChange(t *testing.T) {
	initial := native.CallMediaEpoch{Epoch: 9, OwnLeaf: 2, BaseKey: [16]byte{1}, Roster: []native.CallMediaMember{{Leaf: 2}}}
	if err := epochStillBound(initial, initial); err != nil {
		t.Fatalf("same epoch rejected: %v", err)
	}
	changed := initial
	changed.Epoch++
	if !errors.Is(epochStillBound(changed, initial), ErrEpochChanged) {
		t.Fatal("epoch advance was not rejected")
	}
	changed = initial
	changed.OwnLeaf++
	if !errors.Is(epochStillBound(changed, initial), ErrEpochChanged) {
		t.Fatal("leaf change was not rejected")
	}
	changed = initial
	changed.BaseKey[1] = 2
	if !errors.Is(epochStillBound(changed, initial), ErrEpochChanged) {
		t.Fatal("same epoch key replacement was not rejected")
	}
}

func testID(v byte) id.ID {
	var out id.ID
	out[0] = v
	return out
}
