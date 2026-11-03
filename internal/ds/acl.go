package ds

import (
	"context"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// ACL is the eligibility half of invariant 4's Add clause: "a credential whose user is eligible
// under the channel's ACL". The resolver is Plan 2's — permissions live with the structure tables
// — so the clause is an injected seam. The composition root injects api.ResolverACL, the
// permission resolver over roles and channel overwrites (Plan 2 task 3, NV-B6 closed); a DS built
// without one falls back to DenyUnlessMember, which admits a user only where the instance can
// already see them in the group. That default is deliberately conservative: a permissive one
// would leave a security clause of invariant 4 silently unimplemented, which is the one outcome
// worse than a strict one.
//
// It is declared here, with its Plan-1 stub, because ds.Options names the seam (deviation D16),
// so the package does not build without it. Task 20's brief
// re-declares this interface beside checkAddedMember, which is its first caller; that step is a
// check against this declaration, not a second one.
type ACL interface {
	// Eligible reports whether userID may be added to groupID. An error means "the resolver
	// cannot answer", which every caller treats as a refusal, never as a pass.
	Eligible(ctx context.Context, groupID, userID id.ID) (bool, error)
}

// BatchACL is an ACL that can also answer for many users of one group in one call, with the same
// verdict Eligible gives each. The sweeper's reconcile asks about every leaf holder of a group;
// answered one user at a time by an ACL that reads the group's members per question
// (DenyUnlessMember), that is O(leaves²) store reads a group. An ACL without it is asked per user.
type BatchACL interface {
	EligibleUsers(ctx context.Context, groupID id.ID, users []id.ID) (map[id.ID]bool, error)
}

var _ BatchACL = DenyUnlessMember{}

// DenyUnlessMember is the Plan-1 ACL: a user is eligible only where the instance can already see
// them as a member of the group. That admits the ordinary re-add of a device belonging to a user
// already in the group and refuses everything else (NV-B6). It is the default of a DS built with
// no ACL, and api.ResolverACL still answers with it for the groups the resolver has no rule for
// (pairing and interaction groups, and a DM-shaped group whose target is no DM channel).
type DenyUnlessMember struct{ Store store.Repository }

func (a DenyUnlessMember) Eligible(ctx context.Context, groupID, userID id.ID) (bool, error) {
	members, err := a.Store.ListMembers(ctx, groupID)
	if err != nil {
		return false, err
	}
	for _, m := range members {
		if m.UserID == userID && m.RemovedEpoch == nil {
			return true, nil
		}
	}
	return false, nil
}

// EligibleUsers is Eligible for each of users, over one read of the group's members.
func (a DenyUnlessMember) EligibleUsers(ctx context.Context, groupID id.ID, users []id.ID) (map[id.ID]bool, error) {
	members, err := a.Store.ListMembers(ctx, groupID)
	if err != nil {
		return nil, err
	}
	live := make(map[id.ID]bool, len(members))
	for _, m := range members {
		if m.RemovedEpoch == nil {
			live[m.UserID] = true
		}
	}
	out := make(map[id.ID]bool, len(users))
	for _, u := range users {
		out[u] = live[u]
	}
	return out, nil
}
