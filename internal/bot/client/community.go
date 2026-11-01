package client

import (
	"bytes"
	"context"
	"math"
	"net/http"
	"net/url"
	"unicode/utf8"

	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

const MaxCommunityMemberPage = 200

// CommunityMember is one current membership row from the bounded community
// member listing. Kind 0 is a human account; kind 1 is a bot account.
type CommunityMember struct {
	UserID   id.ID
	Joined   int64
	Nick     string
	Roles    []id.ID
	Username string
	Display  string
	Kind     uint64
}

type CommunityMemberPage struct{ Members []CommunityMember }

// CommunityMembersPage reads one server-sized page. The cursor is exclusive;
// callers persist the last returned ID and request the next page with it.
func (s *Session) CommunityMembersPage(ctx context.Context, community, after id.ID) (CommunityMemberPage, error) {
	if s == nil || s.c == nil || community.IsZero() {
		return CommunityMemberPage{}, ErrInvalidConfig
	}
	cid, err := pathID(community)
	if err != nil {
		return CommunityMemberPage{}, err
	}
	q := url.Values{}
	if !after.IsZero() {
		q.Set("after", after.String())
	}
	raw, _, err := s.do(ctx, http.MethodGet, "communities/"+cid+"/members", q, nil, MaxDSBody, false)
	if err != nil {
		return CommunityMemberPage{}, err
	}
	var rows []any
	if cborx.Unmarshal(raw, &rows) != nil || len(rows) > MaxCommunityMemberPage {
		return CommunityMemberPage{}, ErrProtocol
	}
	out := CommunityMemberPage{Members: make([]CommunityMember, 0, len(rows))}
	last := after
	for _, rawRow := range rows {
		a, ok := rawRow.([]any)
		if !ok || (len(a) != 7 && len(a) != 9) {
			return CommunityMemberPage{}, ErrProtocol
		}
		// Older servers returned seven fields. Current memberResp appends the
		// account status message and bio after those fields.
		if len(a) == 9 {
			status, statusOK := a[7].(string)
			bio, bioOK := a[8].(string)
			if !statusOK || !bioOK || utf8.RuneCountInString(status) > 190 || utf8.RuneCountInString(bio) > 500 {
				return CommunityMemberPage{}, ErrProtocol
			}
		}
		member := CommunityMember{}
		if member.UserID, ok = asID(a[0]); !ok || member.UserID.IsZero() || bytes.Compare(member.UserID[:], last[:]) <= 0 {
			return CommunityMemberPage{}, ErrProtocol
		}
		joined, ok := memberJoined(a[1])
		if !ok {
			return CommunityMemberPage{}, ErrProtocol
		}
		member.Joined = joined
		if member.Nick, ok = a[2].(string); !ok || len(member.Nick) > 256 {
			return CommunityMemberPage{}, ErrProtocol
		}
		roles, ok := a[3].([]any)
		if !ok || len(roles) > 256 {
			return CommunityMemberPage{}, ErrProtocol
		}
		member.Roles = make([]id.ID, 0, len(roles))
		seenRoles := make(map[id.ID]struct{}, len(roles))
		for _, rawRole := range roles {
			role, valid := asID(rawRole)
			if !valid || role.IsZero() {
				return CommunityMemberPage{}, ErrProtocol
			}
			if _, exists := seenRoles[role]; exists {
				return CommunityMemberPage{}, ErrProtocol
			}
			seenRoles[role] = struct{}{}
			member.Roles = append(member.Roles, role)
		}
		if member.Username, ok = a[4].(string); !ok || len(member.Username) > 256 {
			return CommunityMemberPage{}, ErrProtocol
		}
		if member.Display, ok = a[5].(string); !ok || len(member.Display) > 512 {
			return CommunityMemberPage{}, ErrProtocol
		}
		if member.Kind, ok = asUint(a[6]); !ok || member.Kind > 1 {
			return CommunityMemberPage{}, ErrProtocol
		}
		out.Members = append(out.Members, member)
		last = member.UserID
	}
	return out, nil
}

func memberJoined(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, n >= 0
	case uint64:
		if n <= math.MaxInt64 {
			return int64(n), true
		}
	}
	return 0, false
}

// Community reads one community by ID. Managed bot sessions may use this
// member-scoped route for their bound community without listing memberships.
func (s *Session) Community(ctx context.Context, community id.ID) (Community, error) {
	var out Community
	if s == nil || s.c == nil {
		return out, ErrInvalidConfig
	}
	cid, err := pathID(community)
	if err != nil {
		return out, err
	}
	raw, _, err := s.do(ctx, http.MethodGet, "communities/"+cid, nil, nil, MaxSmallBody, false)
	if err != nil {
		return out, err
	}
	a, err := decode(raw, 8)
	if err != nil {
		return out, err
	}
	var ok bool
	if out.ID, ok = asID(a[0]); !ok || out.ID.IsZero() {
		return Community{}, ErrProtocol
	}
	if out.OwnerID, ok = asID(a[1]); !ok || out.OwnerID.IsZero() {
		return Community{}, ErrProtocol
	}
	if out.Name, ok = a[2].(string); !ok || out.Name == "" {
		return Community{}, ErrProtocol
	}
	if _, ok = a[3].([]byte); !ok {
		return Community{}, ErrProtocol
	}
	if out.PolicyVersion, ok = asUint(a[4]); !ok {
		return Community{}, ErrProtocol
	}
	for _, flag := range a[5:7] {
		if _, ok = asUint(flag); !ok {
			return Community{}, ErrProtocol
		}
	}
	switch a[7].(type) {
	case int64, uint64:
	default:
		return Community{}, ErrProtocol
	}
	return out, nil
}
