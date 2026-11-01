package api_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/jonasthim/dilla/internal/api"
	"github.com/jonasthim/dilla/internal/gateway"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

type presenceGatewayFake struct {
	mu     sync.Mutex
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
	g.mu.Lock()
	defer g.mu.Unlock()
	g.to = append(g.to, user)
	g.frames = append(g.frames, frame)
}

func (g *presenceGatewayFake) snapshot() ([]id.ID, []gateway.Frame) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]id.ID(nil), g.to...), append([]gateway.Frame(nil), g.frames...)
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

// These integration tests catch missing profile hints, redundant reloads and cross-community leaks.
type profilePresenceEnv struct {
	h                     http.Handler
	deps                  api.Deps
	gw                    *presenceGatewayFake
	owner, peer, stranger id.ID
	token                 string
	communities           []id.ID
}

func newProfilePresenceEnv(t *testing.T) profilePresenceEnv {
	t.Helper()
	gw := &presenceGatewayFake{live: map[id.ID]bool{}}
	h, deps := newTestAPIWired(t, func(d *api.Deps) { d.Presence = api.NewPresenceService(d.Repo, gw, d.Clock) })
	owner, _, _ := seedSession(t, deps)
	peer, _, token := seedSession(t, deps)
	stranger, _, _ := seedSession(t, deps)
	e := profilePresenceEnv{h: h, deps: deps, gw: gw, owner: owner.ID, peer: peer.ID, stranger: stranger.ID, token: token}
	for _, users := range [][]id.ID{{owner.ID, peer.ID}, {owner.ID, peer.ID}, {owner.ID, stranger.ID}} {
		cid := id.New()
		e.communities = append(e.communities, cid)
		if err := deps.Repo.CreateCommunity(t.Context(), store.CommunityRow{ID: cid, Owner: owner.ID, Name: "test", PolicyJSON: []byte("{}"), PolicyVersion: 1, Created: 1}); err != nil {
			t.Fatal(err)
		}
		for _, user := range users {
			gw.live[user] = true
			if err := deps.Repo.PutMember(t.Context(), store.MemberOfCommunityRow{CommunityID: cid, UserID: user, Joined: 1}); err != nil {
				t.Fatal(err)
			}
		}
	}
	return e
}

func (e profilePresenceEnv) patch(t *testing.T, body []any) {
	t.Helper()
	if rec := cborCall(t, e.h, http.MethodPatch, "/v1/accounts/me", e.token, body); rec.Code != http.StatusNoContent {
		t.Fatalf("patch = %d: %s", rec.Code, rec.Body.String())
	}
}

func (e profilePresenceEnv) advance(d time.Duration) {
	// The instance clock is deliberately frozen, as in the browser testhost.
	time.Sleep(d)
	synctest.Wait()
}

func (e profilePresenceEnv) wantHints(t *testing.T, rounds int) {
	t.Helper()
	to, frames := e.gw.snapshot()
	if len(frames) != 4*rounds {
		t.Fatalf("got %d hints, want %d", len(frames), 4*rounds)
	}
	for _, cid := range e.communities[:2] {
		want, err := cbor.Marshal([]any{cid})
		if err != nil {
			t.Fatal(err)
		}
		for _, user := range []id.ID{e.owner, e.peer} {
			count := 0
			for i, frame := range frames {
				if to[i] == user && frame.Op == gateway.OpCommunityChanged && bytes.Equal(frame.Payload, want) {
					count++
				}
			}
			if count != rounds {
				t.Fatalf("community %s recipient %s: %d hints, want %d", cid, user, count, rounds)
			}
		}
	}
}

func TestPatchMeTellsCoMembersToReReadTheMemberList(t *testing.T) {
	for _, field := range []int{0, 1, 2} {
		t.Run([]string{"display", "status", "bio"}[field], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newProfilePresenceEnv(t)
				body := []any{nil, nil, nil}
				body[field] = "Björn"
				e.patch(t, body)
				e.advance(2 * time.Second)
				e.wantHints(t, 1)
			})
		})
	}
}

func TestPatchMeNoVisibleChangeSendsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newProfilePresenceEnv(t)
		if err := e.deps.Repo.UpdateProfile(t.Context(), e.peer, profileStr("Björn"), profileStr("working"), profileStr("hello")); err != nil {
			t.Fatal(err)
		}
		e.patch(t, []any{nil, nil, nil})
		e.patch(t, []any{"Björn", "  working  ", "hello"})
		e.advance(3 * time.Second)
		if _, frames := e.gw.snapshot(); len(frames) != 0 {
			t.Fatalf("no-op patches sent %d frames", len(frames))
		}
	})
}

func TestPatchMeRapidEditsCoalesce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newProfilePresenceEnv(t)
		e.patch(t, []any{"Björn", nil, nil})
		e.advance(time.Second)
		e.patch(t, []any{nil, "working", nil})
		e.patch(t, []any{nil, nil, "hello"})
		e.advance(999 * time.Millisecond)
		if _, frames := e.gw.snapshot(); len(frames) != 0 {
			t.Fatal("burst notified before the coalescing window elapsed")
		}
		e.advance(time.Millisecond)
		e.wantHints(t, 1)
		// The final write is persisted before the sole hint, and a later edit gets a new hint.
		user, err := e.deps.Repo.GetUser(t.Context(), e.peer)
		if err != nil || user.Display != "Björn" || user.StatusMsg != "working" || user.Bio != "hello" {
			t.Fatalf("profile = %+v, %v", user, err)
		}
		e.patch(t, []any{nil, "available", nil})
		e.advance(2 * time.Second)
		e.wantHints(t, 2)
	})
}

func TestPatchMeOnlyNotifiesCommunitiesTheUserBelongsTo(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newProfilePresenceEnv(t)
		e.patch(t, []any{"Björn", nil, nil})
		// Membership is resolved at delivery, so leaving during the window sends no stale hint.
		if err := e.deps.Repo.DeleteMember(t.Context(), e.communities[1], e.peer); err != nil {
			t.Fatal(err)
		}
		e.advance(2 * time.Second)
		to, frames := e.gw.snapshot()
		if len(frames) != 2 {
			t.Fatalf("got %d hints, want 2 for the remaining community", len(frames))
		}
		want, err := cbor.Marshal([]any{e.communities[0]})
		if err != nil {
			t.Fatal(err)
		}
		for i, frame := range frames {
			if to[i] == e.stranger || frame.Op != gateway.OpCommunityChanged || !bytes.Equal(frame.Payload, want) {
				t.Fatalf("hint escaped the user's community: %s %+v", to[i], frame)
			}
		}
	})
}
