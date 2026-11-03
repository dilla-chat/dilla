// Package api serves dilla's HTTP API.
// discovery.go is the web client's discovery reads (C11, F13; dilla-web-1 task 9): the caller's own communities, and the text group id both channel reads carry.
package api

import (
	"context"
	"net/http"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// myCommunityResp is one row of GET /v1/communities (L-HTTP-01).
type myCommunityResp struct {
	_             struct{} `cbor:",toarray"`
	CommunityID   id.ID
	Name          string
	Owner         id.ID
	PolicyVersion uint64
}

func (c *Communities) mine(w http.ResponseWriter, r *http.Request) {
	s, err := enrolledSession(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	rows, err := c.repo.ListCommunitiesForUser(r.Context(), s.UserID)
	if err != nil {
		c.fail(w, r, "list my communities", err)
		return
	}
	out := make([]myCommunityResp, 0, len(rows))
	for _, row := range rows {
		out = append(out, myCommunityResp{CommunityID: row.ID, Name: row.Name, Owner: row.Owner, PolicyVersion: row.PolicyVersion})
	}
	if err := server.EncodeBody(w, http.StatusOK, out); err != nil {
		c.log.Error("encode my communities", "err", err)
	}
}

// textGroupID is L-HTTP-02's text_group_id: the oldest open text group of a channel that carries
// one, epoch-unknown or not, and nil for every other channel without a store call.
func textGroupID(ctx context.Context, repo store.Repository, ch store.ChannelRow) (*id.ID, error) {
	if !TextGroupAllowed(ch) {
		return nil, nil
	}
	groups, err := repo.GroupsForTarget(ctx, ch.ID, groupText)
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return nil, nil
	}
	groupID := groups[0].GroupID
	return &groupID, nil
}
