package client

import (
	"context"

	"github.com/dilla-chat/dilla/internal/id"
)

// HintSender tells dillad that one scope's Bot state changed (Bot v2 Task 17): kind is StateScopeUser (scope is the
// member), StateScopeChannel (a Bot channel) or StateScopeCall (the voice channel). A hint carries no content; dillad
// raises the scope's revision and pushes op 63 to its audience.
type HintSender interface {
	Hint(ctx context.Context, kind uint8, scope id.ID) error
}

// SessionHints sends hints over a Bot session's state-hints route.
type SessionHints struct{ Session *Session }

// Hint implements HintSender.
func (h SessionHints) Hint(ctx context.Context, kind uint8, scope id.ID) error {
	_, err := h.Session.StateHint(ctx, kind, scope)
	return err
}
