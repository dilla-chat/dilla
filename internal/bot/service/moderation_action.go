package service

import (
	"context"

	"github.com/dilla-chat/dilla/internal/bot/native"
)

// ModerationActorAuthority resolves privileged roles from trusted community
// membership state, never from the message event itself.
type ModerationActorAuthority interface {
	ModerationActorRoles(context.Context, native.GroupEvent) (owner, admin bool, err error)
}
