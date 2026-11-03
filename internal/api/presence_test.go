package api_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

type presenceGatewayFake struct {
	live   map[id.ID]bool
	to     []id.ID
	frames []gateway.Frame
}

type flakyPresenceMembers struct {
	store.Repository
	fail bool
}

func (r *flakyPresenceMembers) ListMembersOfCommunity(ctx context.Context, community, after id.ID, limit int32) ([]store.MemberOfCommunityRow, error) {
	if r.fail {
		r.fail = false
		return nil, errors.New("temporary member read failure")
	}
	return r.Repository.ListMembersOfCommunity(ctx, community, after, limit)
}

func (g *presenceGatewayFake) PresenceStatus(user id.ID, choice uint8) uint8 {
	if !g.live[user] || choice == 4 {
		return 0
	}
	return choice
}
func (g *presenceGatewayFake) Activity(id.ID) {}
func (g *presenceGatewayFake) DeliverUser(user id.ID, frame gateway.Frame) {
	g.to = append(g.to, user)
	g.frames = append(g.frames, frame)
}

func TestPresenceChoiceAndSnapshotAreMemberScoped(t *testing.T) {
	gw := &presenceGatewayFake{live: map[id.ID]bool{}}
	h, deps := newTestAPIWired(t, func(d *api.Deps) { d.Presence = api.NewPresenceService(d.Repo, gw, d.Clock) })
	owner, _, ownerToken := seedSession(t, deps)
	peer, _, peerToken := seedSession(t, deps)
	stranger, _, strangerToken := seedSession(t, deps)
	ctx := context.Background()
	cid := id.New()
	now := deps.Clock.Now().Unix()
	if err := deps.Repo.CreateCommunity(ctx, store.CommunityRow{ID: cid, Owner: owner.ID, Name: "test", PolicyJSON: []byte("{}"), PolicyVersion: 1, Created: now}); err != nil {
		t.Fatal(err)
	}
	for _, user := range []id.ID{owner.ID, peer.ID} {
		if err := deps.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: user, Joined: now}); err != nil {
			t.Fatal(err)
		}
	}
	gw.live[owner.ID], gw.live[peer.ID] = true, true
	path := "/v1/communities/" + cid.String() + "/presence"
	if rec := doAuth(h, http.MethodGet, path, strangerToken); rec.Code != http.StatusForbidden {
		t.Fatalf("stranger snapshot = %d", rec.Code)
	}
	if rec := cborCall(t, h, http.MethodPatch, "/v1/accounts/me/presence", ownerToken, []any{uint64(4)}); rec.Code != http.StatusNoContent {
		t.Fatalf("patch = %d: %s", rec.Code, rec.Body.String())
	}
	if choice, err := deps.Repo.GetPresenceChoice(ctx, owner.ID); err != nil || choice != 4 {
		t.Fatalf("stored choice = %d, %v", choice, err)
	}
	var me []cbor.RawMessage
	if err := cbor.Unmarshal(doAuth(h, http.MethodGet, "/v1/accounts/me", ownerToken).Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	var persisted uint8
	if err := cbor.Unmarshal(me[6], &persisted); err != nil || persisted != 4 {
		t.Fatalf("account choice = %d, %v", persisted, err)
	}
	if rec := cborCall(t, h, http.MethodPatch, "/v1/accounts/me/presence", ownerToken, []any{uint64(5)}); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid choice = %d", rec.Code)
	}
	var rows [][]cbor.RawMessage
	rec := doAuth(h, http.MethodGet, path, peerToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("member snapshot = %d: %s", rec.Code, rec.Body.String())
	}
	if err := cbor.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("snapshot rows = %d", len(rows))
	}
	for _, row := range rows {
		var user id.ID
		var status uint8
		if err := cbor.Unmarshal(row[0], &user); err != nil {
			t.Fatal(err)
		}
		if err := cbor.Unmarshal(row[1], &status); err != nil {
			t.Fatal(err)
		}
		if user == owner.ID && status != 0 {
			t.Fatalf("invisible leaked as %d", status)
		}
	}
	for _, to := range gw.to {
		if to == stranger.ID {
			t.Fatal("presence sent to stranger")
		}
	}
	if err := deps.Repo.DeleteMember(ctx, cid, peer.ID); err != nil {
		t.Fatal(err)
	}
	gw.to = nil
	if rec := cborCall(t, h, http.MethodPatch, "/v1/accounts/me/presence", ownerToken, []any{uint64(1)}); rec.Code != http.StatusNoContent {
		t.Fatalf("second patch = %d", rec.Code)
	}
	for _, to := range gw.to {
		if to == peer.ID {
			t.Fatal("presence sent to removed member")
		}
	}
}

func TestPresenceBroadcastRetriesAfterAudienceReadFailure(t *testing.T) {
	_, deps := newTestAPI(t)
	owner, _, _ := seedSession(t, deps)
	peer, _, _ := seedSession(t, deps)
	ctx := context.Background()
	cid := id.New()
	if err := deps.Repo.CreateCommunity(ctx, store.CommunityRow{ID: cid, Owner: owner.ID, Name: "test", PolicyJSON: []byte("{}"), PolicyVersion: 1, Created: 1}); err != nil {
		t.Fatal(err)
	}
	for _, user := range []id.ID{owner.ID, peer.ID} {
		if err := deps.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: user, Joined: 1}); err != nil {
			t.Fatal(err)
		}
	}
	gw := &presenceGatewayFake{live: map[id.ID]bool{owner.ID: true}}
	service := api.NewPresenceService(&flakyPresenceMembers{Repository: deps.Repo, fail: true}, gw, deps.Clock)
	service.Changed(ctx, owner.ID)
	if len(gw.to) != 0 {
		t.Fatalf("broadcast after failed audience read: %v", gw.to)
	}
	service.Changed(ctx, owner.ID)
	for _, to := range gw.to {
		if to == peer.ID {
			return
		}
	}
	t.Fatalf("retry did not reach peer: %v", gw.to)
}
