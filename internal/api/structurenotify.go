package api

import (
	"context"
	"log/slog"

	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// memberPage is how many members one read of the notifier takes; the member route pages at 200.
// A variable only so a test can lower it and reach the second page without 200 members.
var memberPage int32 = 200

// StructureNotifier tells a community's members that its structure changed: op 54
// community_changed `[community_id]` (protocol/02 § Gateway frames) to every live connection of
// every member, after the commit of a channel create, update or delete, of a community PATCH, of a
// join, leave, kick or ban, and of a role, grant or channel overwrite change; to the members of a
// deleted community; and once to the user a leave, kick or ban removed.
// The frame names only the community; a client re-reads what it may see through the routes that
// check its permissions. A nil *StructureNotifier sends nothing.
type StructureNotifier struct {
	gw   VoiceGateway
	repo store.Repository
	log  *slog.Logger
}

// NewStructureNotifier delivers through gw (*gateway.Gateway is one) to the members repo lists.
func NewStructureNotifier(gw VoiceGateway, repo store.Repository, log *slog.Logger) *StructureNotifier {
	return &StructureNotifier{gw: gw, repo: repo, log: log}
}

// frame is the op 54 frame naming cid; false (logged) when it cannot be encoded.
func (n *StructureNotifier) frame(ctx context.Context, cid id.ID) (gateway.Frame, bool) {
	p, err := gateway.CommunityChangedPayload(cid)
	if err != nil {
		n.log.ErrorContext(ctx, "encoding a community_changed failed", "err", err)
		return gateway.Frame{}, false
	}
	return gateway.Frame{Op: gateway.OpCommunityChanged, Payload: p}, true
}

// Changed sends community_changed for cid to every member. A failure is logged, not answered: the
// change is committed, and a client that missed the frame still re-reads on focus and on reconnect.
func (n *StructureNotifier) Changed(ctx context.Context, cid id.ID) {
	if n == nil || n.gw == nil {
		return
	}
	users, err := communityMemberIDs(ctx, n.repo, cid)
	if err != nil {
		n.log.ErrorContext(ctx, "listing members for community_changed failed", "community", cid, "err", err)
		return
	}
	n.ChangedTo(ctx, cid, users)
}

// Removed sends community_changed for cid once to every live connection of uid, a user a leave,
// kick or ban has just removed: they knew the community until this commit, and the frame tells
// them to re-read their list, which no longer names it.
func (n *StructureNotifier) Removed(ctx context.Context, cid, uid id.ID) {
	if n == nil || n.gw == nil {
		return
	}
	if f, ok := n.frame(ctx, cid); ok {
		n.gw.DeliverUser(uid, f)
	}
}

// ChangedUser sends community_changed only to uid. Member timeouts are personal state: the
// affected user's connected clients re-read the community, while other members need no update.
func (n *StructureNotifier) ChangedUser(ctx context.Context, cid, uid id.ID) {
	if n == nil || n.gw == nil {
		return
	}
	if f, ok := n.frame(ctx, cid); ok {
		n.gw.DeliverUser(uid, f)
	}
}

// ChangedTo sends community_changed for cid to users, a list the caller already read: a deleted
// community's members, listed inside the delete's transaction before the tombstone.
func (n *StructureNotifier) ChangedTo(ctx context.Context, cid id.ID, users []id.ID) {
	if n == nil || n.gw == nil {
		return
	}
	f, ok := n.frame(ctx, cid)
	if !ok {
		return
	}
	for _, u := range users {
		n.gw.DeliverUser(u, f)
	}
}

// communityMemberIDs reads every member of cid through repo, a page of memberPage at a time.
func communityMemberIDs(ctx context.Context, repo store.Repository, cid id.ID) ([]id.ID, error) {
	var out []id.ID
	after := id.ID{}
	for {
		page, err := repo.ListMembersOfCommunity(ctx, cid, after, memberPage)
		if err != nil {
			return nil, err
		}
		for _, m := range page {
			out = append(out, m.UserID)
		}
		if len(page) < int(memberPage) {
			return out, nil
		}
		after = page[len(page)-1].UserID
	}
}
