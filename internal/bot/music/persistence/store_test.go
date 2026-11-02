package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/jonasthim/dilla/internal/bot/music"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

type rowKey struct {
	area uint64
	key  string
}

type memoryNative struct {
	rows      map[rowKey]native.StateValue
	binding   native.Binding
	txCalls   int
	loseReply bool
}

func newMemoryNative() *memoryNative {
	return &memoryNative{
		rows:    make(map[rowKey]native.StateValue),
		binding: native.Binding{Instance: testID(9), Community: testID(1), Installation: testID(8)},
	}
}

func (m *memoryNative) Binding() native.Binding { return m.binding }

func (m *memoryNative) StateGet(_ context.Context, area uint64, key []byte) (native.StateValue, error) {
	return m.rows[rowKey{area, string(key)}], nil
}

func (m *memoryNative) StateTransaction(_ context.Context, _ id.ID, changes []native.StateChange) ([]uint64, error) {
	m.txCalls++
	change := changes[0]
	k := rowKey{change.Area, string(change.Key)}
	current := m.rows[k]
	if current.Revision != change.ExpectedRevision {
		return nil, ErrConflict
	}
	m.rows[k] = native.StateValue{Revision: current.Revision + 1, Value: append([]byte(nil), change.Value...)}
	if m.loseReply {
		return nil, native.ErrOutcomeUnknown
	}
	return []uint64{current.Revision + 1}, nil
}

func testID(n byte) (v id.ID) { v[15] = n; return }

func testTrack(idText string) music.Track {
	return music.Track{ID: idText, Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "title", RequesterID: "member"}
}

func applied(t *testing.T, state music.State, op music.Operation) music.State {
	t.Helper()
	next, err := music.Apply(state, state.Revision, op)
	if err != nil {
		t.Fatal(err)
	}
	return next
}

func newStore(t *testing.T, n Native, community id.ID) *Store {
	t.Helper()
	s, err := New(n, community)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStoreIsolatesVoiceChannels(t *testing.T) {
	ctx := context.Background()
	n := newMemoryNative()
	s := newStore(t, n, testID(1))
	one, err := s.Load(ctx, testID(2))
	if err != nil {
		t.Fatal(err)
	}
	one.State = applied(t, one.State, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{testTrack("one")}})
	if _, err = s.Save(ctx, testID(2), one.Revision, one.State); err != nil {
		t.Fatal(err)
	}
	two, err := s.Load(ctx, testID(3))
	if err != nil {
		t.Fatal(err)
	}
	if two.Revision != 0 || len(two.State.Queue) != 0 {
		t.Fatalf("channel 3 inherited channel 2 state: %#v", two)
	}
}

func TestStoreRejectsMismatchedNativeCommunity(t *testing.T) {
	if _, err := New(newMemoryNative(), testID(4)); err == nil {
		t.Fatal("store accepted a community different from the native binding")
	}
}

func TestStoreRejectsStaleRevisionWithoutPartialUpdate(t *testing.T) {
	ctx := context.Background()
	n := newMemoryNative()
	s := newStore(t, n, testID(1))
	initial, _ := s.Load(ctx, testID(2))
	first := applied(t, initial.State, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{testTrack("one")}})
	if _, err := s.Save(ctx, testID(2), 0, first); err != nil {
		t.Fatal(err)
	}
	stale := applied(t, initial.State, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{testTrack("two")}})
	if _, err := s.Save(ctx, testID(2), 0, stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale save error = %v", err)
	}
	got, err := s.Load(ctx, testID(2))
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != 1 || len(got.State.Queue) != 1 || got.State.Queue[0].ID != "one" {
		t.Fatalf("stale write partially changed row: %#v", got)
	}
	invalid := music.State{Revision: 2, Queue: []music.Track{testTrack("bad")}, Paused: true, Volume: 101}
	calls := n.txCalls
	if _, err = s.Save(ctx, testID(2), 1, invalid); err == nil {
		t.Fatal("invalid state saved")
	}
	if n.txCalls != calls {
		t.Fatal("invalid state reached native transaction")
	}
}

func TestLoadRejectsMalformedPersistedState(t *testing.T) {
	ctx := context.Background()
	for name, state := range map[string]music.State{
		"bad track":         {Revision: 1, Queue: []music.Track{{ID: "bad"}}, Paused: true, Volume: 100},
		"duplicate IDs":     {Revision: 1, Queue: []music.Track{testTrack("same"), testTrack("same")}, Paused: true, Volume: 100},
		"revision mismatch": {Revision: 2, Queue: []music.Track{}, Paused: true, Volume: 100},
	} {
		t.Run(name, func(t *testing.T) {
			n := newMemoryNative()
			s := newStore(t, n, testID(1))
			value, err := encode(state)
			if err != nil && name != "bad track" && name != "duplicate IDs" {
				t.Fatal(err)
			}
			if err != nil { // Manually create corrupt but syntactically valid stored input.
				body, _ := fmtJSON(state)
				value, _ = cborx.Marshal([]any{stateFormat, body})
			}
			key := rowKey{stateArea, string(s.key(testID(2)))}
			n.rows[key] = native.StateValue{Revision: 1, Value: value}
			if _, err = s.Load(ctx, testID(2)); !errors.Is(err, ErrMalformed) {
				t.Fatalf("load error = %v", err)
			}
		})
	}
	n := newMemoryNative()
	s := newStore(t, n, testID(1))
	key := rowKey{stateArea, string(s.key(testID(2)))}
	n.rows[key] = native.StateValue{Revision: 1, Value: []byte{0x81, 0x00}}
	if _, err := s.Load(ctx, testID(2)); !errors.Is(err, ErrMalformed) {
		t.Fatalf("malformed CBOR error = %v", err)
	}
}

func fmtJSON(state music.State) ([]byte, error) { return json.Marshal(state) }

func TestRestartWhilePlayingResetsStalePosition(t *testing.T) {
	ctx := context.Background()
	n := newMemoryNative()
	s := newStore(t, n, testID(1))
	snapshot, _ := s.Load(ctx, testID(2))
	snapshot.State = applied(t, snapshot.State, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{testTrack("one")}})
	snapshot, err := s.Save(ctx, testID(2), snapshot.Revision, snapshot.State)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.State = applied(t, snapshot.State, music.Operation{Kind: music.OpResume})
	snapshot.State.PositionMS = 1234
	if _, err := s.Save(ctx, testID(2), snapshot.Revision, snapshot.State); err != nil {
		t.Fatal(err)
	}
	restarted, err := s.Load(ctx, testID(2))
	if err != nil {
		t.Fatal(err)
	}
	if !restarted.State.Paused || restarted.State.Current == nil || restarted.State.PositionMS != 0 {
		t.Fatalf("restart retained a stale playing position: %#v", restarted.State)
	}
}

func TestStoreHandlesUncertainTransactionByExactReadback(t *testing.T) {
	ctx := context.Background()
	n := newMemoryNative()
	n.loseReply = true
	s := newStore(t, n, testID(1))
	snapshot, _ := s.Load(ctx, testID(2))
	snapshot.State = applied(t, snapshot.State, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{testTrack("one")}})
	got, err := s.Save(ctx, testID(2), 0, snapshot.State)
	if err != nil || got.Revision != 1 {
		t.Fatalf("exact readback did not resolve uncertain outcome: %#v, %v", got, err)
	}
}

func TestQueueAndEncodedValueBounds(t *testing.T) {
	state := music.InitialState()
	tracks := make([]music.Track, music.QueueLimit)
	for i := range tracks {
		tracks[i] = testTrack(fmt.Sprintf("track-%03d", i))
	}
	state = applied(t, state, music.Operation{Kind: music.OpEnqueue, Tracks: tracks})
	state.Revision = 1
	for i := range state.Queue {
		state.Queue[i].Title = string(make([]byte, music.MaxMetadataBytes))
	}
	if _, err := encode(state); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize state error = %v", err)
	}
	tooMany := music.State{Revision: 1, Queue: make([]music.Track, music.QueueLimit+1), Paused: true, Volume: 100}
	for i := range tooMany.Queue {
		tooMany.Queue[i] = testTrack(fmt.Sprintf("track-%03d", i))
	}
	if _, err := encode(tooMany); !errors.Is(err, ErrMalformed) {
		t.Fatalf("queue count bound error = %v", err)
	}
}
