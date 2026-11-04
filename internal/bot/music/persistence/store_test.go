package persistence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/music"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
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
	for _, change := range changes {
		current := m.rows[rowKey{change.Area, string(change.Key)}]
		if current.Revision != change.ExpectedRevision {
			return nil, ErrConflict
		}
	}
	revisions := make([]uint64, len(changes))
	for i, change := range changes {
		k := rowKey{change.Area, string(change.Key)}
		revisions[i] = change.ExpectedRevision + 1
		m.rows[k] = native.StateValue{Revision: revisions[i], Value: append([]byte(nil), change.Value...)}
	}
	if m.loseReply {
		return nil, native.ErrOutcomeUnknown
	}
	return revisions, nil
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

func unsupportedStateValue(t *testing.T, state music.State) []byte {
	t.Helper()
	body, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	value, err := cborx.Marshal([]any{stateFormat, body})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func legacyQueueState() music.State {
	old := testTrack("retired")
	old.Provider = music.Provider("spotify")
	old.CanonicalID = "spotify:track:0123456789012345678901"
	queued := old
	queued.ID = "retired-queued"
	return music.State{Revision: 1, Current: &old, Queue: []music.Track{queued, testTrack("supported")}, PositionMS: 4000, PositionAtMS: 5000, Volume: 75, Repeat: music.RepeatQueue}
}

func TestLoadQuarantinesUnsupportedProvidersAndPreservesExactOriginal(t *testing.T) {
	for _, loseReply := range []bool{false, true} {
		t.Run(fmt.Sprint("lost reply=", loseReply), func(t *testing.T) {
			n := newMemoryNative()
			n.loseReply = loseReply
			s := newStore(t, n, testID(1))
			channel := testID(2)
			original := unsupportedStateValue(t, legacyQueueState())
			key := rowKey{stateArea, string(s.key(channel))}
			n.rows[key] = native.StateValue{Revision: 1, Value: original}
			got, err := s.Load(t.Context(), channel)
			if err != nil {
				t.Fatal(err)
			}
			if got.Revision != 2 || !got.State.Paused || got.State.Current != nil || len(got.State.Queue) != 1 || got.State.Queue[0].ID != "supported" || got.State.PositionMS != 0 || got.State.PositionAtMS != 0 || got.State.Volume != 75 || got.State.Repeat != music.RepeatQueue {
				t.Fatalf("recovered state=%+v", got)
			}
			archive := n.rows[rowKey{stateArea, string(s.quarantineKey(channel, 1))}]
			if archive.Revision != 1 || !bytes.Equal(archive.Value, original) {
				t.Fatal("original queue was not archived exactly")
			}
			if n.txCalls != 1 || len(n.rows) != 2 {
				t.Fatal("quarantine was not one two-row transaction")
			}
			if _, err := s.Load(t.Context(), channel); err != nil || n.txCalls != 1 {
				t.Fatalf("repeat load repeated quarantine: %v", err)
			}
			cleared := applied(t, got.State, music.Operation{Kind: music.OpClear})
			if _, err := s.Save(t.Context(), channel, got.Revision, cleared); err != nil {
				t.Fatalf("normal queue clear failed after recovery: %v", err)
			}
			if !bytes.Equal(n.rows[rowKey{stateArea, string(s.quarantineKey(channel, 1))}].Value, original) {
				t.Fatal("normal controls changed the archive")
			}
		})
	}
}

func TestQuarantineConflictDoesNotPartiallyRewriteQueue(t *testing.T) {
	n := newMemoryNative()
	s := newStore(t, n, testID(1))
	channel := testID(2)
	original := unsupportedStateValue(t, legacyQueueState())
	key := rowKey{stateArea, string(s.key(channel))}
	n.rows[key] = native.StateValue{Revision: 1, Value: original}
	archiveKey := rowKey{stateArea, string(s.quarantineKey(channel, 1))}
	n.rows[archiveKey] = native.StateValue{Revision: 1, Value: []byte{0x81, 0x01}}
	if _, err := s.Load(t.Context(), channel); !errors.Is(err, ErrUnsupportedProvider) {
		t.Fatalf("conflicting quarantine error=%v", err)
	}
	if n.rows[key].Revision != 1 || !bytes.Equal(n.rows[key].Value, original) || !bytes.Equal(n.rows[archiveKey].Value, []byte{0x81, 0x01}) {
		t.Fatal("conflicting transaction partially changed state")
	}
}

func TestQuarantineValidatesSupportedRemainderBeforeWriting(t *testing.T) {
	n := newMemoryNative()
	s := newStore(t, n, testID(1))
	channel := testID(2)
	state := legacyQueueState()
	state.Queue[1].CanonicalID = "invalid"
	original := unsupportedStateValue(t, state)
	key := rowKey{stateArea, string(s.key(channel))}
	n.rows[key] = native.StateValue{Revision: 1, Value: original}
	if _, err := s.Load(t.Context(), channel); !errors.Is(err, ErrMalformed) {
		t.Fatalf("malformed remainder accepted: %v", err)
	}
	if n.txCalls != 0 || !bytes.Equal(n.rows[key].Value, original) {
		t.Fatal("invalid remainder changed persisted state")
	}
}

func TestQuarantinePreservesSupportedCurrentTrackAndPosition(t *testing.T) {
	n := newMemoryNative()
	s := newStore(t, n, testID(1))
	channel := testID(2)
	state := legacyQueueState()
	current := testTrack("current")
	state.Current = &current
	original := unsupportedStateValue(t, state)
	key := rowKey{stateArea, string(s.key(channel))}
	n.rows[key] = native.StateValue{Revision: 1, Value: original}
	got, err := s.Load(t.Context(), channel)
	if err != nil {
		t.Fatal(err)
	}
	if got.State.Current == nil || got.State.Current.ID != "current" || got.State.PositionMS != state.PositionMS || got.State.PositionAtMS != state.PositionAtMS || !got.State.Paused {
		t.Fatalf("supported current track changed: %+v", got.State)
	}
}

type corruptedQuarantineReply struct {
	*memoryNative
	corruptActive bool
}

func (n corruptedQuarantineReply) StateTransaction(ctx context.Context, tx id.ID, changes []native.StateChange) ([]uint64, error) {
	if _, err := n.memoryNative.StateTransaction(ctx, tx, changes); err != nil {
		return nil, err
	}
	i := 0
	if n.corruptActive {
		i = 1
	}
	k := rowKey{changes[i].Area, string(changes[i].Key)}
	row := n.rows[k]
	row.Value = []byte{0x81, 0x00}
	n.rows[k] = row
	return nil, native.ErrOutcomeUnknown
}
func TestQuarantineLostReplyRequiresBothExactRows(t *testing.T) {
	for _, corruptActive := range []bool{false, true} {
		n := corruptedQuarantineReply{newMemoryNative(), corruptActive}
		s := newStore(t, n, testID(1))
		channel := testID(2)
		n.rows[rowKey{stateArea, string(s.key(channel))}] = native.StateValue{Revision: 1, Value: unsupportedStateValue(t, legacyQueueState())}
		if _, err := s.Load(t.Context(), channel); !errors.Is(err, ErrUnsupportedProvider) {
			t.Fatalf("inexact readback accepted: %v", err)
		}
	}
}
