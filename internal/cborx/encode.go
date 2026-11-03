// Package cborx implements the deterministic-CBOR subset dilla speaks on the
// wire and across the wasi ABI: major types 0 (unsigned integer), 2 (byte
// string), 3 (text string) and 4 (array), plus the simple value null (0xf6).
// Negative integers, maps, tags, floats, every other simple value, indefinite
// lengths, non-minimal integer heads and trailing bytes are rejected on decode.
//
// The rules are the ones dilla-core's cbor module enforces in Rust and
// packages/protocol-vectors/src/cbor.ts enforces in TypeScript, so the three
// implementations accept and reject exactly the same byte strings.
package cborx

import (
	"fmt"
	"sync"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/id"
)

// MaxNesting mirrors dilla-core's cbor::MAX_NESTING.
const MaxNesting = 8

// The CBOR major types dilla allows.
const (
	MajorUint  byte = 0
	MajorBytes byte = 2
	MajorText  byte = 3
	MajorArray byte = 4
)

var (
	encOnce sync.Once
	encMode cbor.EncMode
	encErr  error
)

// EncMode returns the shared RFC 8949 Core Deterministic encoding mode. The
// mode is safe for concurrent use and is cached, as the library's own
// documentation recommends.
func EncMode() (cbor.EncMode, error) {
	encOnce.Do(func() {
		encMode, encErr = cbor.CoreDetEncOptions().EncMode()
	})
	return encMode, encErr
}

// Marshal encodes v with EncMode. Structs must carry the `cbor:",toarray"` tag
// on a leading `_ struct{}` field: dilla's wire formats are fixed-position
// arrays, never maps.
func Marshal(v any) ([]byte, error) {
	em, err := EncMode()
	if err != nil {
		return nil, fmt.Errorf("cborx: encode mode: %w", err)
	}
	return em.Marshal(v)
}

// Raw wraps already-encoded deterministic CBOR so it is spliced into a larger
// value instead of being re-encoded.
func Raw(b []byte) cbor.RawMessage { return cbor.RawMessage(b) }

// MarshalFrame encodes a gateway frame: the four-element array
// [op, n, group_id, payload] of protocol/02 § Gateway frames. payload is spliced
// verbatim, so the hot path encodes a fan-out payload once and re-frames it per
// connection with only n changed. groupID nil emits CBOR null (0xf6).
func MarshalFrame(op, n uint64, groupID *id.ID, payload cbor.RawMessage) ([]byte, error) {
	var gid any
	if groupID != nil {
		gid = *groupID
	}
	frame := []any{op, n, gid, payload}
	b, err := Marshal(frame)
	if err != nil {
		return nil, fmt.Errorf("cborx: marshal frame op %d: %w", op, err)
	}
	return b, nil
}
