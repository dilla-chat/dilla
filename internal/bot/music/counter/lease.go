// Package counter reserves disjoint SFrame microphone counter intervals in the
// Bot's encrypted native state before a music publisher starts.
package counter

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"

	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/sframe"
)

const (
	stateArea = uint64(9)
	// At 50 Opus frames per second, one block lasts over five hours; publisher
	// lifetime is capped at 30 minutes. Exhaustion still fails closed.
	BlockSize = uint64(1 << 20)
)

var ErrInvalid = errors.New("music counter reservation invalid")
var ErrExhausted = errors.New("music counter space exhausted")

// Range is an exclusive sequence interval already committed to encrypted state.
type Range struct{ Start, End uint64 }

type State interface {
	StateGet(context.Context, uint64, []byte) (native.StateValue, error)
	StateTransaction(context.Context, id.ID, []native.StateChange) ([]uint64, error)
}

// Reserve commits a fresh Mic/0 range for one call group, epoch and Bot leaf.
// A lost mutation response cannot cause reuse: the caller must stop publishing,
// and a later open reads the advanced cursor (skipping any unused range).
func Reserve(ctx context.Context, state State, group id.ID, epoch uint64, leaf uint16) (Range, error) {
	if ctx == nil || state == nil || group.IsZero() || epoch > math.MaxInt64 {
		return Range{}, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return Range{}, err
	}
	key := stateKey(group, epoch, leaf)
	v, err := state.StateGet(ctx, stateArea, key)
	if err != nil {
		return Range{}, err
	}
	if v.Tombstone {
		return Range{}, ErrInvalid
	}
	start := uint64(0)
	if v.Revision == 0 {
		if len(v.Value) != 0 {
			return Range{}, ErrInvalid
		}
	} else {
		var row []any
		if cborx.Unmarshal(v.Value, &row) != nil || len(row) != 2 || row[0] != uint64(1) {
			return Range{}, ErrInvalid
		}
		var ok bool
		start, ok = row[1].(uint64)
		if !ok || start > sframe.MaxSeq+1 {
			return Range{}, ErrInvalid
		}
	}
	if start > sframe.MaxSeq+1-BlockSize {
		return Range{}, ErrExhausted
	}
	end := start + BlockSize
	value, err := cborx.Marshal([]any{uint64(1), end})
	if err != nil {
		return Range{}, err
	}
	var revision [8]byte
	binary.BigEndian.PutUint64(revision[:], v.Revision)
	seed := make([]byte, 0, 32+len(key)+len(value))
	seed = append(seed, []byte("dilla.bot.music.sframe.reserve.v1\x00")...)
	seed = append(seed, key...)
	seed = append(seed, revision[:]...)
	seed = append(seed, value...)
	hash := sha256.Sum256(seed)
	tx := id.ID(hash[:16])
	if tx.IsZero() {
		return Range{}, ErrInvalid
	}
	revisions, err := state.StateTransaction(ctx, tx, []native.StateChange{{Area: stateArea, Key: key, ExpectedRevision: v.Revision, Value: value}})
	if err != nil {
		return Range{}, err
	}
	if len(revisions) != 1 || revisions[0] != v.Revision+1 {
		return Range{}, ErrInvalid
	}
	return Range{Start: start, End: end}, nil
}

func stateKey(group id.ID, epoch uint64, leaf uint16) []byte {
	key := make([]byte, 0, 40)
	key = append(key, []byte("sframe.mic.v1\x00")...)
	key = append(key, group[:]...)
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], epoch)
	key = append(key, b[:]...)
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], leaf)
	key = append(key, l[:]...)
	key = append(key, byte(sframe.Mic), 0) // slot and layer
	return key
}
