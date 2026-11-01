package api_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/jonasthim/dilla/internal/api"
	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/clock"
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
	h, deps := newTestAPIWired(t, func(d *api.Deps) { d.Presence = api.NewPresenceService(d.Repo, gw, d.Clock, d.Log) })
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
	service := api.NewPresenceService(&flakyPresenceMembers{Repository: deps.Repo, fail: true}, gw, deps.Clock, deps.Log)
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
	return newProfilePresenceEnvWithRepo(t, nil)
}

func newProfilePresenceEnvWithRepo(t *testing.T, wrap func(store.Repository) store.Repository) profilePresenceEnv {
	t.Helper()
	gw := &presenceGatewayFake{live: map[id.ID]bool{}}
	h, deps := newTestAPIWired(t, func(d *api.Deps) {
		if wrap != nil {
			d.Repo = wrap(d.Repo)
		}
		d.Presence = api.NewPresenceService(d.Repo, gw, d.Clock, d.Log)
	})
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
				e.advance(10 * time.Second)
				e.wantHints(t, 1)
			})
		})
	}
}

func TestPatchMeNoVisibleChangeSendsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newProfilePresenceEnv(t)
		if _, err := e.deps.Repo.UpdateProfile(t.Context(), e.peer, profileStr("Björn"), profileStr("working"), profileStr("hello")); err != nil {
			t.Fatal(err)
		}
		e.patch(t, []any{nil, nil, nil})
		e.patch(t, []any{"Björn", "  working  ", "hello"})
		e.advance(11 * time.Second)
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
		e.advance(8999 * time.Millisecond)
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
		e.advance(10 * time.Second)
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
		e.advance(10 * time.Second)
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

func TestPatchMeTwoEditorsCoalescePerCommunity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newProfilePresenceEnv(t)
		e.patch(t, []any{"Björn", nil, nil})
		e.advance(5 * time.Second)
		if _, err := e.deps.Repo.UpdateProfile(t.Context(), e.owner, profileStr("Owner edited"), nil, nil); err != nil {
			t.Fatal(err)
		}
		e.deps.Presence.ProfileChanged(t.Context(), e.owner)
		e.advance(4999 * time.Millisecond)
		if _, frames := e.gw.snapshot(); len(frames) != 0 {
			t.Fatal("hint before 10 seconds")
		}
		e.advance(time.Millisecond)
		e.wantHints(t, 1)
		e.advance(5 * time.Second)
		// The owner's third community has its own window, started at t=5.
		_, frames := e.gw.snapshot()
		if len(frames) != 6 {
			t.Fatalf("got %d hints, want one per member of each community", len(frames))
		}
	})
}

func TestPatchMeOneEditorThreeCommunities(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newProfilePresenceEnv(t)
		if err := e.deps.Repo.PutMember(t.Context(), store.MemberOfCommunityRow{CommunityID: e.communities[2], UserID: e.peer, Joined: 1}); err != nil {
			t.Fatal(err)
		}
		for round := 1; round <= 2; round++ {
			e.patch(t, []any{fmt.Sprintf("name %d", round), nil, nil})
			e.advance(10 * time.Second)
			to, frames := e.gw.snapshot()
			if len(frames) != 7*round {
				t.Fatalf("got %d hints, want %d", len(frames), 7*round)
			}
			for _, cid := range e.communities {
				want, _ := cbor.Marshal([]any{cid})
				count := 0
				for i, frame := range frames {
					if to[i] == e.peer && bytes.Equal(frame.Payload, want) {
						count++
					}
				}
				if count != round {
					t.Fatalf("community %s: %d hints, want %d", cid, count, round)
				}
			}
		}
	})
}

func TestPatchMeHintStormIsBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newProfilePresenceEnv(t)
		for i := 0; i < 100; i++ {
			e.patch(t, []any{fmt.Sprintf("name %d", i), nil, nil})
			e.deps.Clock.(*clock.Fake).Advance(600 * time.Millisecond)
			e.advance(600 * time.Millisecond)
		}
		e.advance(10 * time.Second)
		_, frames := e.gw.snapshot()
		if len(frames) > 24 {
			t.Fatalf("100 edits sent %d hints, want at most 6 per community/recipient", len(frames))
		}
		user, err := e.deps.Repo.GetUser(t.Context(), e.peer)
		if err != nil || user.Display != "name 99" {
			t.Fatalf("rate cap lost saved profile: %+v, %v", user, err)
		}
	})
}

func TestPatchMeEditorHintRateResetsAfterAMinute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newProfilePresenceEnv(t)
		for i := 0; i < 6; i++ {
			e.patch(t, []any{fmt.Sprintf("name %d", i), nil, nil})
		}
		e.advance(10 * time.Second)
		e.wantHints(t, 1)
		e.patch(t, []any{"over budget", nil, nil})
		e.advance(10 * time.Second)
		e.wantHints(t, 1)
		e.advance(40 * time.Second)
		e.patch(t, []any{"next minute", nil, nil})
		e.advance(10 * time.Second)
		e.wantHints(t, 2)
	})
}

func TestPatchMeInvisibleSendsNoHint(t *testing.T) {
	for _, invisibleAtDelivery := range []bool{false, true} {
		t.Run(fmt.Sprint(invisibleAtDelivery), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newProfilePresenceEnv(t)
				if invisibleAtDelivery {
					e.patch(t, []any{"Björn", nil, nil})
					e.advance(time.Second)
				}
				if err := e.deps.Repo.SetPresenceChoice(t.Context(), e.peer, 4); err != nil {
					t.Fatal(err)
				}
				if !invisibleAtDelivery {
					e.patch(t, []any{"Björn", nil, nil})
				}
				e.advance(11 * time.Second)
				if _, frames := e.gw.snapshot(); len(frames) != 0 {
					t.Fatalf("Invisible edit sent %d hints", len(frames))
				}
				user, err := e.deps.Repo.GetUser(t.Context(), e.peer)
				if err != nil || user.Display != "Björn" {
					t.Fatalf("invisible edit not saved: %+v %v", user, err)
				}
			})
		})
	}
}

func TestPatchMeTwoDevicesCoalesce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newProfilePresenceEnv(t)
		_, device, private := seedAPIDevice(t, e.deps)
		device.ID, device.UserID = id.New(), e.peer
		if err := e.deps.Repo.CreateDevice(t.Context(), device); err != nil {
			t.Fatal(err)
		}
		nonce, _, err := e.deps.Sessions.Challenge(t.Context(), device.ID)
		if err != nil {
			t.Fatal(err)
		}
		preimage := auth.SessionPreimage(e.deps.Sessions.InstanceID(), device.ID, nonce, auth.PurposeSession)
		second, err := e.deps.Sessions.Establish(t.Context(), auth.EstablishRequest{
			DeviceID: device.ID, Nonce: nonce, Purpose: auth.PurposeSession, Sig: ed25519.Sign(private, preimage),
		})
		if err != nil {
			t.Fatal(err)
		}

		e.patch(t, []any{"Björn", nil, nil})
		e.advance(5 * time.Second)
		e.token = second.Token
		e.patch(t, []any{nil, "second device", nil})
		e.advance(5 * time.Second)
		e.wantHints(t, 1)
		e.advance(10 * time.Second)
		e.wantHints(t, 1)
	})
}

func TestPatchMeCoMemberLeavingDuringWindowGetsNoHint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newProfilePresenceEnv(t)
		leaver, _, _ := seedSession(t, e.deps)
		e.gw.live[leaver.ID] = true
		if err := e.deps.Repo.PutMember(t.Context(), store.MemberOfCommunityRow{CommunityID: e.communities[1], UserID: leaver.ID, Joined: 1}); err != nil {
			t.Fatal(err)
		}
		e.patch(t, []any{"Björn", nil, nil})
		e.advance(time.Second)
		if err := e.deps.Repo.DeleteMember(t.Context(), e.communities[1], leaver.ID); err != nil {
			t.Fatal(err)
		}
		e.advance(9 * time.Second)
		e.wantHints(t, 1) // Only the two remaining members of each community receive hints.
		to, _ := e.gw.snapshot()
		for _, recipient := range to {
			if recipient == leaver.ID {
				t.Fatal("former co-member got a hint")
			}
		}
	})
}

// Stall an UPDATE after the old implementation's separate read. Another PATCH and
// its hint finish first, then this request restores the original display value.
type stalledProfileUpdate struct {
	store.Repository
	entered, release chan struct{}
}

func (r *stalledProfileUpdate) UpdateProfile(ctx context.Context, user id.ID, display, status, bio *string) (bool, error) {
	if display != nil && *display == "User" {
		close(r.entered)
		<-r.release
	}
	return r.Repository.UpdateProfile(ctx, user, display, status, bio)
}

func TestPatchMeConcurrentPatchDecidesChangeAtUpdate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var stalled *stalledProfileUpdate
		e := newProfilePresenceEnvWithRepo(t, func(repo store.Repository) store.Repository {
			stalled = &stalledProfileUpdate{Repository: repo, entered: make(chan struct{}), release: make(chan struct{})}
			return stalled
		})
		done := make(chan int, 1)
		go func() {
			done <- cborCall(t, e.h, http.MethodPatch, "/v1/accounts/me", e.token, []any{"User", nil, nil}).Code
		}()
		<-stalled.entered
		e.patch(t, []any{"Björn", nil, nil})
		e.advance(10 * time.Second)
		e.wantHints(t, 1)
		close(stalled.release)
		if code := <-done; code != http.StatusNoContent {
			t.Fatalf("concurrent PATCH = %d", code)
		}
		e.advance(10 * time.Second)
		e.wantHints(t, 2)
		user, err := e.deps.Repo.GetUser(t.Context(), e.peer)
		if err != nil || user.Display != "User" {
			t.Fatalf("final profile = %+v, %v", user, err)
		}
	})
}

func TestProfileHintsDrainFlushesPendingAndStopsNewWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newProfilePresenceEnv(t)
		e.patch(t, []any{"Björn", nil, nil})
		synctest.Wait()
		drainer, ok := any(e.deps.Presence).(interface{ Drain(context.Context) error })
		if !ok {
			t.Fatal("profile hints cannot drain on shutdown")
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if err := drainer.Drain(ctx); err != nil {
			t.Fatalf("drain = %v", err)
		}
		e.wantHints(t, 1)
		e.patch(t, []any{"after shutdown", nil, nil})
		e.advance(11 * time.Second)
		e.wantHints(t, 1)
		if err := drainer.Drain(ctx); err != nil {
			t.Fatalf("second drain = %v", err)
		}
	})
}

// Faults are at the store boundary; scheduling, encoding, fan-out and logs remain real.
type profileHintFailureRepo struct {
	store.Repository
	operation string
	entered   chan struct{}
}

func (r *profileHintFailureRepo) ListCommunitiesForUser(ctx context.Context, user id.ID) ([]store.CommunityRow, error) {
	if r.operation == "communities" {
		return nil, errors.New("profile hint store fault")
	}
	return r.Repository.ListCommunitiesForUser(ctx, user)
}
func (r *profileHintFailureRepo) ListMembersOfCommunity(ctx context.Context, community, after id.ID, limit int32) ([]store.MemberOfCommunityRow, error) {
	if r.operation == "members" {
		return nil, errors.New("profile hint store fault")
	}
	if r.operation == "blocked" {
		r.entered <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return r.Repository.ListMembersOfCommunity(ctx, community, after, limit)
}

func TestProfileHintsLogStoreErrors(t *testing.T) {
	for _, operation := range []string{"communities", "members"} {
		t.Run(operation, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newProfilePresenceEnv(t)
				var logs bytes.Buffer
				service := api.NewPresenceService(&profileHintFailureRepo{Repository: e.deps.Repo, operation: operation}, e.gw, e.deps.Clock, slog.New(slog.NewTextHandler(&logs, nil)))
				service.ProfileChanged(t.Context(), e.peer)
				e.advance(10 * time.Second)
				if !strings.Contains(logs.String(), "profile hint store fault") {
					t.Fatalf("missing store failure log: %s", logs.String())
				}
				if _, frames := e.gw.snapshot(); len(frames) != 0 {
					t.Fatalf("failed reads sent %d hints", len(frames))
				}
			})
		})
	}
}

func TestProfileHintsDrainHonorsDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newProfilePresenceEnv(t)
		entered := make(chan struct{}, 2)
		service := api.NewPresenceService(&profileHintFailureRepo{Repository: e.deps.Repo, operation: "blocked", entered: entered}, e.gw, e.deps.Clock, e.deps.Log)
		service.ProfileChanged(t.Context(), e.peer)
		e.advance(10 * time.Second)
		<-entered
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if err := service.Drain(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("drain = %v, want deadline", err)
		}
		synctest.Wait()
		if _, frames := e.gw.snapshot(); len(frames) != 0 {
			t.Fatal("cancelled hint delivered")
		}
	})
}
