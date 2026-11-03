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
	"log/slog"
	"math"
	"strconv"

	"github.com/dilla-chat/dilla/internal/bot/music"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
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
	// ErrUnsupportedProvider means an unsupported queue could not be safely
	// recovered, while allowing other voice channels to restore independently.
	ErrUnsupportedProvider = errors.New("persisted music state uses an unsupported provider")
	ErrTombstone           = errors.New("persisted music state was deleted")
	ErrTooLarge            = errors.New("persisted music state exceeds size limit")
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
// validated before use and always returned paused, preserving its position but
// preventing a process restart from resuming audio. Unsupported providers are
// removed only after atomically archiving the exact original snapshot through
// the encrypted helper.
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
	if err != nil || state.Revision != v.Revision {
		return Snapshot{}, ErrMalformed
	}
	if len(state.Queue) > music.QueueLimit {
		return Snapshot{}, ErrMalformed
	}
	unsupported := false
	if state.Current != nil {
		if state.Current.Provider == "" {
			return Snapshot{}, ErrMalformed
		}
		if state.Current.Provider != music.ProviderYouTube {
			state.Current = nil
			state.PositionMS, state.PositionAtMS = 0, 0
			unsupported = true
		}
	}
	queue := make([]music.Track, 0, len(state.Queue))
	for _, track := range state.Queue {
		if track.Provider == "" {
			return Snapshot{}, ErrMalformed
		}
		if track.Provider != music.ProviderYouTube {
			unsupported = true
			continue
		}
		queue = append(queue, track)
	}
	state.Queue = queue
	if unsupported {
		return s.quarantineUnsupported(ctx, channel, v, state)
	}
	if !valid(state) {
		return Snapshot{}, ErrMalformed
	}
	state.Paused = true
	return Snapshot{State: state, Revision: v.Revision}, nil
}

// quarantineUnsupported archives the exact old value and replaces only the active
// queue in one native CAS transaction. Both values remain encrypted by the helper.
// A fully valid paused remainder can then be cleared or edited using normal controls.
func (s *Store) quarantineUnsupported(ctx context.Context, channel id.ID, original native.StateValue, state music.State) (Snapshot, error) {
	if original.Revision >= math.MaxInt64 {
		return Snapshot{}, ErrUnsupportedProvider
	}
	state.Revision++
	state.Paused = true
	value, err := encode(state)
	if err != nil {
		return Snapshot{}, err
	}
	key := s.key(channel)
	archiveKey := s.quarantineKey(channel, original.Revision)
	seed := append([]byte("dilla.bot.music.quarantine.tx.v1\x00"), key...)
	seed = appendUint64(seed, original.Revision)
	seed = append(seed, original.Value...)
	seed = append(seed, value...)
	hash := sha256.Sum256(seed)
	tx := id.ID(hash[:16])
	if tx.IsZero() {
		return Snapshot{}, ErrUnsupportedProvider
	}
	revisions, txErr := s.native.StateTransaction(ctx, tx, []native.StateChange{
		{Area: stateArea, Key: archiveKey, ExpectedRevision: 0, Value: original.Value},
		{Area: stateArea, Key: key, ExpectedRevision: original.Revision, Value: value},
	})
	if txErr == nil && (len(revisions) != 2 || revisions[0] != 1 || revisions[1] != state.Revision) {
		txErr = errors.New("invalid music quarantine transaction revision")
	}
	if txErr != nil {
		// As with Save, a lost reply proves nothing. Require both exact committed
		// values before treating a possibly committed transaction as successful.
		archive, archiveErr := s.native.StateGet(ctx, stateArea, archiveKey)
		active, activeErr := s.native.StateGet(ctx, stateArea, key)
		if archiveErr != nil || activeErr != nil || archive.Tombstone || active.Tombstone ||
			archive.Revision != 1 || active.Revision != state.Revision ||
			!bytes.Equal(archive.Value, original.Value) || !bytes.Equal(active.Value, value) {
			return Snapshot{}, errors.Join(ErrUnsupportedProvider, txErr, archiveErr, activeErr)
		}
	}
	slog.WarnContext(ctx, "bot music queue recovered", "channel_id", channel.String(), "class", "unsupported_provider_quarantined")
	return Snapshot{State: clone(state), Revision: state.Revision}, nil
}

func (s *Store) quarantineKey(channel id.ID, revision uint64) []byte {
	return []byte("dilla.bot.music.quarantine.v1/" + s.community.String() + "/" + channel.String() + "/" + strconv.FormatUint(revision, 10))
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
