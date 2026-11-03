package service

import (
	"context"
	"errors"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/id"
)

const manageCommunityBit uint64 = 1 << 16

type actorAuthoritySession interface {
	BotActorAuthority(context.Context, id.ID, id.ID) (client.ActorAuthority, bool, error)
}

// CommunityActorAuthority resolves the actor against the current membership
// and permissions returned by the server for an authenticated bot session.
// Revision is supplied by the caller's durable announcement scheduler.
type CommunityActorAuthority struct {
	session   actorAuthoritySession
	community id.ID
	revision  func(context.Context, id.ID) (uint64, error)
}

func NewCommunityActorAuthority(session actorAuthoritySession, community id.ID, revision func(context.Context, id.ID) (uint64, error)) (*CommunityActorAuthority, error) {
	if session == nil || community.IsZero() {
		return nil, errors.New("missing community actor authority dependency")
	}
	return &CommunityActorAuthority{session: session, community: community, revision: revision}, nil
}

func (a *CommunityActorAuthority) lookup(ctx context.Context, community, actor id.ID) (client.ActorAuthority, bool, error) {
	if a == nil || a.session == nil || a.community.IsZero() || community.IsZero() || actor.IsZero() {
		return client.ActorAuthority{}, false, errors.New("invalid community actor authority lookup")
	}
	if community != a.community {
		return client.ActorAuthority{}, false, errors.New("actor authority community mismatch")
	}
	return a.session.BotActorAuthority(ctx, community, actor)
}

// ModerationActorRoles derives exemptions solely from the event's authenticated
// sender identity and a current server lookup.
func (a *CommunityActorAuthority) ModerationActorRoles(ctx context.Context, event native.GroupEvent) (owner, admin bool, err error) {
	resolved, present, err := a.lookup(ctx, event.Community, event.SenderUser)
	if err != nil || !present {
		return false, false, err
	}
	return resolved.Owner, resolved.Administrator, nil
}

// CurrentRevision delegates to the durable schedule store; actor authority
// responses deliberately contain no schedule revision.
func (a *CommunityActorAuthority) CurrentRevision(ctx context.Context, community id.ID) (uint64, error) {
	if a == nil || community != a.community || a.revision == nil {
		return 0, errors.New("missing announcement revision authority")
	}
	return a.revision(ctx, community)
}

// CanManageCommunity performs a fresh authenticated lookup for the actor
// supplied by InteractionDispatcher's authenticated native event.
func (a *CommunityActorAuthority) CanManageCommunity(ctx context.Context, actor, community id.ID) (bool, error) {
	resolved, present, err := a.lookup(ctx, community, actor)
	if err != nil || !present {
		return false, err
	}
	return resolved.Owner || resolved.Administrator || resolved.EffectiveBits&manageCommunityBit != 0, nil
}

var _ ModerationActorAuthority = (*CommunityActorAuthority)(nil)
var _ AnnouncementInteractionAuthority = (*CommunityActorAuthority)(nil)
