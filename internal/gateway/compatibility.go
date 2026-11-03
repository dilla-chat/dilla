package gateway

import (
	"context"
	"errors"

	"github.com/dilla-chat/dilla/internal/id"
)

// ErrCompatibilityUnavailable is returned when the gateway cannot resolve a scoped resource or
// read the current community installation state. Scoped traffic fails closed on this error.
var ErrCompatibilityUnavailable = errors.New("gateway: compatibility state unavailable")

// GroupResource is the authoritative compatibility scope of a stored MLS group.
type GroupResource struct {
	Kind        uint8
	CommunityID *id.ID
}

// ScopeResolver is deliberately separate from Gateway.Store. Production resolves only the group
// or channel binding needed for a frame; the shared capability policy then reads committed
// installation state. Implementations must not cache resource bindings across admissions.
type ScopeResolver interface {
	GroupResource(context.Context, id.ID) (GroupResource, error)
	ChannelCommunity(context.Context, id.ID) (*id.ID, error)
}

// CommunityCompatibility is satisfied by internal/bot/capability.Policy. It is consulted for
// every scoped admission and has no process-local enabled-state cache.
type CommunityCompatibility interface {
	Enabled(context.Context, id.ID) (bool, error)
}

func (g *Gateway) scopeRequiresWire3(ctx context.Context, scope FrameScope) (bool, error) {
	if len(scope.Groups) == 0 && len(scope.Channels) == 0 {
		return false, nil
	}
	if g.opts.ScopeResolver == nil || g.opts.CommunityCompatibility == nil {
		return false, ErrCompatibilityUnavailable
	}
	requiresWire3 := false
	for _, groupID := range scope.Groups {
		resource, err := g.opts.ScopeResolver.GroupResource(ctx, groupID)
		if err != nil {
			return false, errors.Join(ErrCompatibilityUnavailable, err)
		}
		if resource.Kind == 3 {
			requiresWire3 = true // interaction groups always require wire 3, including null community ids
			continue
		}
		if resource.CommunityID != nil {
			enabled, err := g.opts.CommunityCompatibility.Enabled(ctx, *resource.CommunityID)
			if err != nil {
				return false, errors.Join(ErrCompatibilityUnavailable, err)
			}
			if enabled {
				requiresWire3 = true
			}
		}
	}
	for _, channelID := range scope.Channels {
		communityID, err := g.opts.ScopeResolver.ChannelCommunity(ctx, channelID)
		if err != nil {
			return false, errors.Join(ErrCompatibilityUnavailable, err)
		}
		if communityID == nil {
			continue // DM channel
		}
		enabled, err := g.opts.CommunityCompatibility.Enabled(ctx, *communityID)
		if err != nil {
			return false, errors.Join(ErrCompatibilityUnavailable, err)
		}
		if enabled {
			requiresWire3 = true
		}
	}
	return requiresWire3, nil
}

func (g *Gateway) connectionWire(c *conn) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wire
}

func (g *Gateway) allowWire3Scoped(c *conn, requiresWire3 bool) bool {
	return !requiresWire3 || g.connectionWire(c) >= 3
}

func appendScopeID(ids []id.ID, value id.ID) []id.ID {
	for _, existing := range ids {
		if existing == value {
			return ids
		}
	}
	return append(ids, value)
}

func normalizeFrameScope(f Frame) FrameScope {
	scope := f.Scope.clone()
	if f.GroupID != nil {
		scope.Groups = appendScopeID(scope.Groups, *f.GroupID)
	}
	return scope
}
