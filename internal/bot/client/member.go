package client

import (
	"context"
	"errors"
	"net/http"

	"github.com/jonasthim/dilla/internal/id"
)

// MemberProfile contains the current community nick and the user's bounded
// account names. Its presence is always established by the membership row.
type MemberProfile struct {
	UserID   id.ID
	Nick     string
	Username string
	Display  string
}

// Name returns the member's community nick, falling back to their account
// display name and then handle.
func (m MemberProfile) Name() string {
	if m.Nick != "" {
		return m.Nick
	}
	if m.Display != "" {
		return m.Display
	}
	return m.Username
}

// MemberProfile looks up one user in one community. A 404 means the user is
// not a current member; no event history or other community is consulted.
func (s *Session) MemberProfile(ctx context.Context, community, user id.ID) (MemberProfile, bool, error) {
	var out MemberProfile
	cid, err := pathID(community)
	if err != nil {
		return out, false, err
	}
	uid, err := pathID(user)
	if err != nil {
		return out, false, err
	}
	raw, _, err := s.do(ctx, http.MethodGet, "communities/"+cid+"/members/"+uid, nil, nil, MaxSmallBody, false)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return MemberProfile{}, false, nil
		}
		return out, false, err
	}
	a, err := decode(raw, 4)
	if err != nil {
		return out, false, err
	}
	if out.UserID, _ = asID(a[0]); out.UserID != user {
		return MemberProfile{}, false, ErrProtocol
	}
	var ok bool
	if out.Nick, ok = a[1].(string); !ok || len(out.Nick) > 256 {
		return MemberProfile{}, false, ErrProtocol
	}
	if out.Username, ok = a[2].(string); !ok || len(out.Username) > 256 {
		return MemberProfile{}, false, ErrProtocol
	}
	if out.Display, ok = a[3].(string); !ok || len(out.Display) > 512 {
		return MemberProfile{}, false, ErrProtocol
	}
	if out.Name() == "" {
		return MemberProfile{}, false, ErrProtocol
	}
	return out, true, nil
}

// CommunityMembers binds a device session to the community used by welcome's
// context-aware member resolver.
type CommunityMembers struct {
	session   *Session
	community id.ID
}

func (s *Session) CommunityMembers(community id.ID) (*CommunityMembers, error) {
	if s == nil || s.c == nil || community.IsZero() {
		return nil, ErrInvalidConfig
	}
	return &CommunityMembers{session: s, community: community}, nil
}

// Page reads the next ordered page for this community using the exclusive
// user ID cursor. CommunityMembersPage strictly validates the response.
func (m *CommunityMembers) Page(ctx context.Context, after id.ID) (CommunityMemberPage, error) {
	if m == nil || m.session == nil {
		return CommunityMemberPage{}, ErrInvalidConfig
	}
	return m.session.CommunityMembersPage(ctx, m.community, after)
}

func (m *CommunityMembers) ResolveCurrentMember(ctx context.Context, user id.ID) (bool, string, error) {
	if m == nil || m.session == nil {
		return false, "", ErrInvalidConfig
	}
	profile, present, err := m.session.MemberProfile(ctx, m.community, user)
	if err != nil || !present {
		return present, "", err
	}
	return true, profile.Name(), nil
}

// ResolveCurrentEpisode reads the exact current membership episode through the
// bot-scoped endpoint. The server serializes this read with membership changes.
func (m *CommunityMembers) ResolveCurrentEpisode(ctx context.Context, user id.ID) (id.ID, bool, error) {
	if m == nil || m.session == nil || m.community.IsZero() || user.IsZero() {
		return id.ID{}, false, ErrInvalidConfig
	}
	path := "communities/" + m.community.String() + "/bot/members/" + user.String() + "/episode"
	raw, _, err := m.session.c.do(ctx, m.session.token, http.MethodGet, path, nil, nil, MaxSmallBody, false)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return id.ID{}, false, nil
		}
		return id.ID{}, false, err
	}
	a, err := decode(raw, 2)
	if err != nil {
		return id.ID{}, false, err
	}
	gotMember, okMember := asID(a[0])
	episode, okEpisode := asID(a[1])
	if !okMember || !okEpisode || gotMember != user || episode.IsZero() {
		return id.ID{}, false, ErrProtocol
	}
	return episode, true, nil
}

// GrantJoinRole verifies that the event episode is still current before
// calling the idempotent server mutation. A changed or absent episode is stale.
func (m *CommunityMembers) GrantJoinRole(ctx context.Context, installation, user, episode id.ID) (JoinRoleGrantReceipt, bool, error) {
	current, present, err := m.ResolveCurrentEpisode(ctx, user)
	if err != nil || !present || current != episode {
		return JoinRoleGrantReceipt{}, false, err
	}
	return m.session.GrantJoinRole(ctx, m.community, installation, user, episode)
}
