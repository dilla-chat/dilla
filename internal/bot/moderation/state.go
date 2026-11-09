package moderation

import (
	"context"

	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/id"
)

// configStateClient is the helper's encrypted application-state area as the evidence store uses it. (The name is the
// AutoModeration configuration store's, which Bot v2 Task 23 removed.)
type configStateClient interface {
	Binding() native.Binding
	StateGet(context.Context, uint64, []byte) (native.StateValue, error)
	StateTransaction(context.Context, id.ID, []native.StateChange) ([]uint64, error)
}

func validConfigBinding(b native.Binding) bool {
	return !b.Instance.IsZero() && !b.Community.IsZero() && !b.Installation.IsZero()
}

func configID(v any) (id.ID, bool) {
	b, ok := v.([]byte)
	if !ok || len(b) != len(id.ID{}) {
		return id.ID{}, false
	}
	var out id.ID
	copy(out[:], b)
	return out, true
}
