// Package operationid derives stable, installation-scoped moderation operation IDs.
package operationid

import (
	"crypto/sha256"
	"encoding/binary"
	"math"

	"github.com/dilla-chat/dilla/internal/id"
)

// Automatic returns the stable operation ID for one group message revision.
// The action and rules stay in the request hash, so changing a decision for
// an already handled source conflicts with its durable receipt.
func Automatic(group id.ID, revision uint64) id.ID {
	if group.IsZero() || revision == 0 || revision > math.MaxInt64 {
		return id.ID{}
	}
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], revision)
	sum := sha256.Sum256(append(append([]byte("dilla:automod:enforce:v1\x00"), group[:]...), encoded[:]...))
	var operation id.ID
	copy(operation[:], sum[:len(operation)])
	if operation.IsZero() {
		operation[15] = 1
	}
	return operation
}
