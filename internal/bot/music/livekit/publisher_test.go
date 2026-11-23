package livekit

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
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

// counterStateStub is the helper's durable state as the counter cursor uses it (dilla-bot-core state.rs get and
// transaction): one row per key, compare-and-swap on each change's expected revision, and an idempotent receipt per
// transaction id: a transaction id seen before with the same changes replays its stored result without checking any
// revision (a different digest is a conflict). transactions counts commits, each a disk sync on the helper. The pool
// reserves from its own goroutines, so it is locked; afterGet and beforeCommit (optional) run outside the lock, for
// tests that interleave reservations.
type counterStateStub struct {
	mu           sync.Mutex
	rows         map[string]native.StateValue
	receipts     map[id.ID]stubReceipt
	transactions int
	afterGet     func()
	beforeCommit func()
}

type stubReceipt struct {
	digest [32]byte
	result []uint64
}

func changesDigest(changes []native.StateChange) [32]byte {
	h := sha256.New()
	for _, c := range changes {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], c.Area)
		h.Write(n[:])
		binary.BigEndian.PutUint64(n[:], uint64(len(c.Key)))
		h.Write(n[:])
		h.Write(c.Key)
		binary.BigEndian.PutUint64(n[:], c.ExpectedRevision)
		h.Write(n[:])
		binary.BigEndian.PutUint64(n[:], uint64(len(c.Value)))
		h.Write(n[:])
		h.Write(c.Value)
	}
	var d [32]byte
	copy(d[:], h.Sum(nil))
	return d
}

func (s *counterStateStub) StateGet(_ context.Context, area uint64, key []byte) (native.StateValue, error) {
	s.mu.Lock()
	value := s.rows[fmt.Sprintf("%d/%x", area, key)]
	s.mu.Unlock()
	if s.afterGet != nil {
		s.afterGet()
	}
	return value, nil
}

func (s *counterStateStub) StateTransaction(_ context.Context, tx id.ID, changes []native.StateChange) ([]uint64, error) {
	if s.beforeCommit != nil {
		s.beforeCommit()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if tx.IsZero() || len(changes) < 1 {
		return nil, errors.New("invalid state transaction")
	}
	digest := changesDigest(changes)
	if receipt, ok := s.receipts[tx]; ok {
		if receipt.digest != digest {
			return nil, errors.New("counter state transaction conflict")
		}
		return append([]uint64(nil), receipt.result...), nil
	}
	if s.rows == nil {
		s.rows, s.receipts = map[string]native.StateValue{}, map[id.ID]stubReceipt{}
	}
	for _, c := range changes {
		if s.rows[fmt.Sprintf("%d/%x", c.Area, c.Key)].Revision != c.ExpectedRevision {
			return nil, errors.New("counter state revision conflict")
		}
	}
	result := make([]uint64, 0, len(changes))
	for _, c := range changes {
		k := fmt.Sprintf("%d/%x", c.Area, c.Key)
		next := native.StateValue{Revision: s.rows[k].Revision + 1, Value: append([]byte(nil), c.Value...)}
		s.rows[k] = next
		result = append(result, next.Revision)
	}
	s.receipts[tx] = stubReceipt{digest: digest, result: result}
	s.transactions++
	return append([]uint64(nil), result...), nil
}

func (s *counterStateStub) commits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transactions
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
