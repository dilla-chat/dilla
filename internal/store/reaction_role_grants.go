package store

import (
	"context"

	"github.com/dilla-chat/dilla/internal/id"
)

// ReactionRoleGrant identifies one panel/message/reaction assignment source.
// Its fields together make removing one panel reaction independent of every
// other way the member may hold the role.
type ReactionRoleGrant struct {
	CommunityID id.ID
	UserID      id.ID
	RoleID      id.ID
	ChannelID   id.ID
	MessageSeq  uint64
	Reaction    string
}

// ReactionRoleGrantSources stores individual reaction assignments while
// keeping member_roles as the effective permission relation.
type ReactionRoleGrantSources interface {
	AddReactionRoleGrant(context.Context, ReactionRoleGrant) error
	RemoveReactionRoleGrant(context.Context, ReactionRoleGrant) error
	RemoveReactionRolePanelGrants(context.Context, id.ID, id.ID, uint64) ([]id.ID, error)
	RemoveReactionRoleChannelGrants(context.Context, id.ID, id.ID) ([]id.ID, error)
	ListReactionRolePanelGrantUsers(context.Context, id.ID, id.ID, uint64) ([]id.ID, error)
}
