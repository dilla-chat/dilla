// Package persistence stores bounded music queue state in the native helper's
// encrypted, community-bound application state.
package persistence

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/jonasthim/dilla/internal/bot/music"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

const (
	stateArea      = 8
	stateKeyPrefix = "dilla.bot.music.v1/"
	stateFormat    = uint64(1)
	maxStateValue  = 256 * 1024
)

var (
	ErrConflict  = errors.New("music state revision conflict")
	ErrMalformed = errors.New("malformed persisted music state")
	ErrTombstone = errors.New("persisted music state was deleted")
	ErrTooLarge  = errors.New("persisted music state exceeds size limit")
)

// Native is the narrow encrypted application-state API used by Store.
type Native interface {
	Binding() native.Binding
	StateGet(context.Context, uint64, []byte) (native.StateValue, error)
	StateTransaction(context.Context, id.ID, []native.StateChange) ([]uint64, error)
}

// Snapshot contains a validated queue state. Revision is the native row
// revision and is intentionally equal to State.Revision.
type Snapshot struct {
	State    music.State
	Revision uint64
}

// Store is scoped to the community bound to its native client.
type Store struct {
	native    Native
	community id.ID
}

func New(n Native, community id.ID) (*Store, error) {
	if n == nil || community.IsZero() {
		return nil, errors.New("invalid music persistence binding")
	}
	binding := n.Binding()
	if binding.Instance.IsZero() || binding.Community.IsZero() || binding.Installation.IsZero() || binding.Community != community {
		return nil, errors.New("invalid music persistence binding")
	}
	return &Store{native: n, community: community}, nil
}

// Load returns a fresh paused queue for an absent row. Existing state is
// validated before use and always returned paused. Only a stored pause has an
// authoritative position; a playing snapshot contains an old playhead and
// restarts from zero after the process loses its live playback.
func (s *Store) Load(ctx context.Context, channel id.ID) (Snapshot, error) {
	if s == nil || channel.IsZero() {
		return Snapshot{}, errors.New("invalid music voice channel")
	}
	v, err := s.native.StateGet(ctx, stateArea, s.key(channel))
	if err != nil {
		return Snapshot{}, err
	}
	if v.Tombstone {
		return Snapshot{}, ErrTombstone
	}
	if v.Revision == 0 {
		if len(v.Value) != 0 {
			return Snapshot{}, ErrMalformed
		}
		return Snapshot{State: music.InitialState()}, nil
	}
	if v.Revision > math.MaxInt64 || len(v.Value) == 0 || len(v.Value) > maxStateValue {
		return Snapshot{}, ErrMalformed
	}
	state, err := decode(v.Value)
	if err != nil || state.Revision != v.Revision || !valid(state) {
		return Snapshot{}, ErrMalformed
	}
	if !state.Paused {
		state.PositionMS = 0
		state.PositionAtMS = 0
	}
	state.Paused = true
	return Snapshot{State: state, Revision: v.Revision}, nil
}

// Save atomically replaces one channel's state if expectedRevision is still
// current. State.Revision must advance exactly once. Unknown transaction
// outcomes are resolved by reading back the exact intended value and revision.
func (s *Store) Save(ctx context.Context, channel id.ID, expectedRevision uint64, state music.State) (Snapshot, error) {
	if s == nil || channel.IsZero() || expectedRevision > math.MaxInt64-1 ||
		state.Revision != expectedRevision+1 || !valid(state) {
		return Snapshot{}, errors.New("invalid music state update")
	}
	value, err := encode(state)
	if err != nil {
		return Snapshot{}, err
	}
	key := s.key(channel)
	seed := append([]byte("dilla.bot.music.state.tx.v1\x00"), key...)
	seed = appendUint64(seed, expectedRevision)
	seed = append(seed, value...)
	hash := sha256.Sum256(seed)
	tx := id.ID(hash[:16])
	if tx.IsZero() {
		return Snapshot{}, errors.New("invalid music state transaction id")
	}
	revisions, txErr := s.native.StateTransaction(ctx, tx, []native.StateChange{{
		Area: stateArea, Key: key, ExpectedRevision: expectedRevision, Value: value,
	}})
	if txErr == nil {
		if len(revisions) == 1 && revisions[0] == expectedRevision+1 {
			return Snapshot{State: clone(state), Revision: revisions[0]}, nil
		}
		txErr = errors.New("invalid music state transaction revision")
	}
	// Native mutations may have committed even when their reply was lost. Only
	// an exact readback proves success; all other outcomes remain an error.
	actual, readErr := s.native.StateGet(ctx, stateArea, key)
	if readErr == nil && !actual.Tombstone && actual.Revision == expectedRevision+1 && bytes.Equal(actual.Value, value) {
		return Snapshot{State: clone(state), Revision: actual.Revision}, nil
	}
	if readErr == nil && actual.Revision != expectedRevision {
		return Snapshot{}, errors.Join(ErrConflict, txErr)
	}
	if readErr != nil {
		return Snapshot{}, errors.Join(txErr, fmt.Errorf("music state outcome readback: %w", readErr))
	}
	return Snapshot{}, txErr
}

func (s *Store) key(channel id.ID) []byte {
	return []byte(stateKeyPrefix + s.community.String() + "/" + channel.String())
}

func encode(state music.State) ([]byte, error) {
	if !valid(state) {
		return nil, ErrMalformed
	}
	body, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	value, err := cborx.Marshal([]any{stateFormat, body})
	if err != nil {
		return nil, err
	}
	if len(value) > maxStateValue {
		return nil, ErrTooLarge
	}
	return value, nil
}

func decode(value []byte) (music.State, error) {
	var outer []any
	if err := cborx.Unmarshal(value, &outer); err != nil || len(outer) != 2 || outer[0] != stateFormat {
		return music.State{}, ErrMalformed
	}
	body, ok := outer[1].([]byte)
	if !ok || len(body) == 0 || len(body) > maxStateValue {
		return music.State{}, ErrMalformed
	}
	var state music.State
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&state); err != nil {
		return music.State{}, ErrMalformed
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return music.State{}, ErrMalformed
	}
	return state, nil
}

// Apply is the queue package's canonical state validator. A harmless
// same-volume operation validates every persisted field and track descriptor.
func valid(state music.State) bool {
	if state.Revision > math.MaxInt64 {
		return false
	}
	_, err := music.Apply(state, state.Revision, music.Operation{Kind: music.OpSetVolume, Volume: state.Volume})
	return err == nil
}

func clone(state music.State) music.State {
	state.Queue = append([]music.Track(nil), state.Queue...)
	if state.Queue == nil {
		state.Queue = []music.Track{}
	}
	if state.Current != nil {
		current := *state.Current
		state.Current = &current
	}
	return state
}

func appendUint64(dst []byte, value uint64) []byte {
	for shift := uint(56); ; shift -= 8 {
		dst = append(dst, byte((value>>shift)&0xff))
		if shift == 0 {
			return dst
		}
	}
}
