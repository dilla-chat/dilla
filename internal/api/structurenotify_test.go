package api_test

import (
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
)

// changedGW records every op 54 community_changed per user, as DeliverUser hands it to the user's
// live connections.
type changedGW struct {
	mu   sync.Mutex
	seen map[id.ID][]id.ID // user → the community ids it was told of, in order
}

func (g *changedGW) DeliverUser(u id.ID, f gateway.Frame) {
	if f.Op != gateway.OpCommunityChanged {
		return
	}
	var raw []cbor.RawMessage
	var cid id.ID
	if cborx.Unmarshal(f.Payload, &raw) != nil || len(raw) != 1 || cborx.Unmarshal(raw[0], &cid) != nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.seen[u] = append(g.seen[u], cid)
}

func (g *changedGW) count(u id.ID) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.seen[u])
}

// Each structural change a member's open tab must learn of is announced once, after its commit, to
// every member and to nobody else: a channel's create, update and delete, and a community PATCH. A
// refused change announces nothing.
func TestEveryStructuralChangeTellsTheMembersOnce(t *testing.T) {
	e := newEnv(t)
	log := slog.New(slog.DiscardHandler)
	gw := &changedGW{seen: map[id.ID][]id.ID{}}
	notify := api.NewStructureNotifier(gw, e.Repo, log)
	api.NewCommunities(e.Repo, e.DS, e.Clk, log).WithNotifier(notify).Register(e.Mux)
	api.NewChannels(e.Repo, e.DS, e.Clk, 10, log).WithNotifier(notify).Register(e.Mux)
	owner, ownerTok := e.NewUser("owner")
	cid := createCommunity(t, e, ownerTok)
	member, memberTok := e.NewUser("member")
	if status, body := join(e, cid, memberTok, nil); status != http.StatusOK {
		t.Fatalf("join = %d (%x)", status, body)
	}
	stranger, _ := e.NewUser("stranger")
	gw.reset() // the join above is announced too

	want := 0
	step := func(name string, status, wantStatus int) {
		t.Helper()
		if status != wantStatus {
			t.Fatalf("%s = %d, want %d", name, status, wantStatus)
		}
		if wantStatus < 300 {
			want++
		}
		for _, u := range []id.ID{owner, member} {
			if got := gw.count(u); got != want {
				t.Fatalf("after %s user %s was told %d times, want %d", name, u, got, want)
			}
		}
		if got := gw.count(stranger); got != 0 {
			t.Fatalf("after %s a non-member was told %d times", name, got)
		}
	}

	ch, _, status := newChannel(t, e, cid, ownerTok, 0, 0, 0, "planering")
	step("create", status, http.StatusCreated)
	status, _ = e.Do(http.MethodPatch, "/v1/channels/"+ch.String(), ownerTok, []any{"fika", nil, nil, nil, nil, nil, nil})
	step("update", status, http.StatusNoContent)
	status, _ = e.Do(http.MethodPatch, "/v1/channels/"+ch.String(), memberTok, []any{"nope", nil, nil, nil, nil, nil, nil})
	step("a refused update", status, http.StatusForbidden)
	status, _ = e.Do(http.MethodDelete, "/v1/channels/"+ch.String(), ownerTok, nil)
	step("delete", status, http.StatusNoContent)
	status, _ = e.Do(http.MethodPatch, "/v1/communities/"+cid.String(), ownerTok, []any{"Fika", nil, nil, nil})
	step("community patch", status, http.StatusOK)

	for _, u := range []id.ID{owner, member} {
		for _, got := range gw.seen[u] {
			if got != cid {
				t.Fatalf("user %s was told of community %s, want %s", u, got, cid)
			}
		}
	}
}

func TestMemberTimeoutNotifiesOnlyTheAffectedUsersLiveSessions(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	owner := mustSessionUser(t, e, ownerTok)
	target, targetTok := e.NewUser("timeout-target")
	joinCommunity(t, e, cid, targetTok)
	stranger, _ := e.NewUser("timeout-stranger")
	gw := &changedGW{seen: map[id.ID][]id.ID{}}
	notify := api.NewStructureNotifier(gw, e.Repo, slog.New(slog.DiscardHandler))
	api.NewMemberTimeouts(e.Repo, e.Clk).WithNotifier(notify).Register(e.Mux)

	path := "/v1/communities/" + cid.String() + "/members/" + target.String() + "/timeout"
	requestID := id.New()
	expires := uint64(e.Clk.Now().Add(time.Minute).Unix())
	if status, body := e.Do(http.MethodPut, path, ownerTok, []any{requestID, expires}); status != http.StatusOK {
		t.Fatalf("PUT timeout = %d (%x), want 200", status, body)
	}
	if got := gw.count(target); got != 1 {
		t.Fatalf("target wakeups after PUT = %d, want 1", got)
	}
	if got := gw.count(owner); got != 0 {
		t.Fatalf("owner wakeups after PUT = %d, want 0", got)
	}
	if got := gw.count(stranger); got != 0 {
		t.Fatalf("stranger wakeups after PUT = %d, want 0", got)
	}
	if status, body := e.Do(http.MethodDelete, path, ownerTok, []any{id.New()}); status != http.StatusNoContent {
		t.Fatalf("DELETE timeout = %d (%x), want 204", status, body)
	}
	if got := gw.count(target); got != 2 {
		t.Fatalf("target wakeups after clear = %d, want 2 total", got)
	}
}

// The notifier reads the members a page at a time, and a member past the first page is told too.
// The page is lowered to two so the third page is reached with five members.
func TestAMemberPastTheFirstPageIsToldToo(t *testing.T) {
	t.Cleanup(api.SetMemberPageForTest(2))
	e := newEnv(t)
	log := slog.New(slog.DiscardHandler)
	gw := &changedGW{seen: map[id.ID][]id.ID{}}
	notify := api.NewStructureNotifier(gw, e.Repo, log)
	api.NewCommunities(e.Repo, e.DS, e.Clk, log).WithNotifier(notify).Register(e.Mux)
	api.NewChannels(e.Repo, e.DS, e.Clk, 10, log).WithNotifier(notify).Register(e.Mux)
	owner, ownerTok := e.NewUser("owner")
	cid := createCommunity(t, e, ownerTok)
	users := []id.ID{owner}
	for _, name := range []string{"m1", "m2", "m3", "m4"} {
		u, tok := e.NewUser(name)
		if status, body := join(e, cid, tok, nil); status != http.StatusOK {
			t.Fatalf("join %s = %d (%x)", name, status, body)
		}
		users = append(users, u)
	}
	gw.reset() // the joins above are announced too

	if _, _, status := newChannel(t, e, cid, ownerTok, 0, 0, 0, "planering"); status != http.StatusCreated {
		t.Fatalf("create = %d, want 201", status)
	}
	for _, u := range users {
		if got := gw.count(u); got != 1 {
			t.Fatalf("member %s was told %d times, want 1", u, got)
		}
	}
}

func (g *changedGW) reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.seen = map[id.ID][]id.ID{}
}

// notifyEnv mounts the community, channel, ban and role routes over one notifier recording into
// the returned changedGW, and creates a community owned by "owner".
func notifyEnv(t *testing.T) (e *env, gw *changedGW, cid, owner id.ID, ownerTok string) {
	t.Helper()
	e = newEnv(t)
	log := slog.New(slog.DiscardHandler)
	gw = &changedGW{seen: map[id.ID][]id.ID{}}
	notify := api.NewStructureNotifier(gw, e.Repo, log)
	api.NewCommunities(e.Repo, e.DS, e.Clk, log).WithNotifier(notify).Register(e.Mux)
	api.NewChannels(e.Repo, e.DS, e.Clk, 10, log).WithNotifier(notify).Register(e.Mux)
	api.NewBans(e.Repo, e.DS, e.Clk, log).WithNotifier(notify).Register(e.Mux)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", log).WithDS(e.DS).WithNotifier(notify).Register(e.Mux)
	owner, ownerTok = e.NewUser("owner")
	cid = createCommunity(t, e, ownerTok)
	return e, gw, cid, owner, ownerTok
}

// joinAs joins cid as a new user named name and returns its id and token.
func joinAs(t *testing.T, e *env, cid id.ID, name string) (id.ID, string) {
	t.Helper()
	u, tok := e.NewUser(name)
	joinCommunity(t, e, cid, tok)
	return u, tok
}

// told asserts how many op 54 frames each user was sent since the last reset.
func told(t *testing.T, gw *changedGW, step string, want map[id.ID]int) {
	t.Helper()
	for u, n := range want {
		if got := gw.count(u); got != n {
			t.Fatalf("after %s user %s was told %d times, want %d", step, u, got, n)
		}
	}
}

// A membership change tells the community's members once, after the commit, and a user it removed
// once more; a ban of someone who is not a member sends that user nothing, and an unban nothing at
// all. A refused kick sends nothing.
func TestMembershipChangesTellTheMembersAndTheRemovedUser(t *testing.T) {
	path := func(cid, u id.ID, rest string) string {
		return "/v1/communities/" + cid.String() + rest + u.String()
	}
	t.Run("kick", func(t *testing.T) {
		e, gw, cid, owner, ownerTok := notifyEnv(t)
		target, _ := joinAs(t, e, cid, "target")
		other, _ := joinAs(t, e, cid, "other")
		gw.reset()
		if status, _ := e.Do(http.MethodDelete, path(cid, target, "/members/"), ownerTok, nil); status != http.StatusNoContent {
			t.Fatalf("kick = %d", status)
		}
		told(t, gw, "kick", map[id.ID]int{target: 1, owner: 1, other: 1})
	})
	t.Run("a refused kick", func(t *testing.T) {
		e, gw, cid, owner, _ := notifyEnv(t)
		target, _ := joinAs(t, e, cid, "target")
		other, otherTok := joinAs(t, e, cid, "other")
		gw.reset()
		if status, _ := e.Do(http.MethodDelete, path(cid, target, "/members/"), otherTok, nil); status != http.StatusForbidden {
			t.Fatalf("kick by a plain member = %d, want 403", status)
		}
		told(t, gw, "a refused kick", map[id.ID]int{target: 0, owner: 0, other: 0})
	})
	t.Run("ban of a member", func(t *testing.T) {
		e, gw, cid, owner, ownerTok := notifyEnv(t)
		target, _ := joinAs(t, e, cid, "target")
		other, _ := joinAs(t, e, cid, "other")
		gw.reset()
		if status, _ := e.Do(http.MethodPut, path(cid, target, "/bans/"), ownerTok, []any{"", nil}); status != http.StatusNoContent {
			t.Fatalf("ban = %d", status)
		}
		told(t, gw, "ban of a member", map[id.ID]int{target: 1, owner: 1, other: 1})
	})
	t.Run("ban of a non-member and unban", func(t *testing.T) {
		e, gw, cid, owner, ownerTok := notifyEnv(t)
		other, _ := joinAs(t, e, cid, "other")
		target, _ := e.NewUser("target")
		gw.reset()
		if status, _ := e.Do(http.MethodPut, path(cid, target, "/bans/"), ownerTok, []any{"", nil}); status != http.StatusNoContent {
			t.Fatalf("ban = %d", status)
		}
		told(t, gw, "ban of a non-member", map[id.ID]int{target: 0, owner: 1, other: 1})
		gw.reset()
		if status, _ := e.Do(http.MethodDelete, path(cid, target, "/bans/"), ownerTok, nil); status != http.StatusNoContent {
			t.Fatalf("unban = %d", status)
		}
		told(t, gw, "unban", map[id.ID]int{target: 0, owner: 0, other: 0})
	})
	t.Run("join", func(t *testing.T) {
		e, gw, cid, owner, _ := notifyEnv(t)
		old, _ := joinAs(t, e, cid, "old")
		gw.reset()
		joiner, tok := joinAs(t, e, cid, "joiner")
		told(t, gw, "join", map[id.ID]int{owner: 1, old: 1, joiner: 1})
		// Joining again writes no membership row and announces nothing.
		joinCommunity(t, e, cid, tok)
		told(t, gw, "a second join", map[id.ID]int{owner: 1, old: 1, joiner: 1})
	})
	t.Run("leave", func(t *testing.T) {
		e, gw, cid, owner, _ := notifyEnv(t)
		leaver, leaverTok := joinAs(t, e, cid, "leaver")
		other, _ := joinAs(t, e, cid, "other")
		gw.reset()
		if status, _ := e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/leave", leaverTok, nil); status != http.StatusNoContent {
			t.Fatalf("leave = %d", status)
		}
		told(t, gw, "leave", map[id.ID]int{leaver: 1, owner: 1, other: 1})
	})
	t.Run("delete", func(t *testing.T) {
		e, gw, cid, owner, ownerTok := notifyEnv(t)
		a, _ := joinAs(t, e, cid, "a")
		b, _ := joinAs(t, e, cid, "b")
		stranger, _ := e.NewUser("stranger")
		gw.reset()
		if status, _ := e.Do(http.MethodDelete, "/v1/communities/"+cid.String(), ownerTok, nil); status != http.StatusNoContent {
			t.Fatalf("delete = %d", status)
		}
		told(t, gw, "delete", map[id.ID]int{owner: 1, a: 1, b: 1, stranger: 0})
		for _, u := range []id.ID{owner, a, b} {
			if got := gw.seen[u][0]; got != cid {
				t.Fatalf("user %s was told of %s, want %s", u, got, cid)
			}
		}
	})
}

// Every role and overwrite mutation tells every member once; a non-member is never told.
func TestRoleAndOverwriteChangesTellTheMembers(t *testing.T) {
	e, gw, cid, owner, ownerTok := notifyEnv(t)
	m, _ := joinAs(t, e, cid, "member")
	stranger, _ := e.NewUser("stranger")
	ch, _, status := newChannel(t, e, cid, ownerTok, 0, 0, 0, "planering")
	if status != http.StatusCreated {
		t.Fatalf("create channel = %d", status)
	}
	gw.reset()
	want := 0
	step := func(name string, status, wantStatus int) {
		t.Helper()
		if status != wantStatus {
			t.Fatalf("%s = %d, want %d", name, status, wantStatus)
		}
		want++
		told(t, gw, name, map[id.ID]int{owner: want, m: want, stranger: 0})
	}

	role := createRole(t, e, cid, ownerTok, "mod", 3, api.PermPinMessages)
	step("role create", http.StatusCreated, http.StatusCreated)
	status, _ = e.Do(http.MethodPatch, "/v1/roles/"+role.String(), ownerTok, []any{"mods", nil, nil, nil, nil, nil, nil})
	step("role patch", status, http.StatusNoContent)
	grant(t, e, cid, ownerTok, m, role, http.StatusNoContent)
	step("grant", http.StatusNoContent, http.StatusNoContent)
	rolePath := "/v1/communities/" + cid.String() + "/members/" + m.String() + "/roles/" + role.String()
	status, _ = e.Do(http.MethodDelete, rolePath, ownerTok, nil)
	step("revoke", status, http.StatusNoContent)
	owPath := "/v1/channels/" + ch.String() + "/overwrites/0/" + role.String()
	status, _ = e.Do(http.MethodPut, owPath, ownerTok, []any{uint64(0), uint64(api.PermPinMessages)})
	step("overwrite put", status, http.StatusNoContent)
	status, _ = e.Do(http.MethodDelete, owPath, ownerTok, nil)
	step("overwrite delete", status, http.StatusNoContent)
	status, _ = e.Do(http.MethodDelete, "/v1/roles/"+role.String(), ownerTok, nil)
	step("role delete", status, http.StatusNoContent)
}

// A community deleted with 201 members tells each of them once: the members are read a page (200)
// at a time inside the delete's transaction, and the 201st is on the second page.
func TestADeletedCommunityTellsAll201Members(t *testing.T) {
	e, gw, cid, owner, ownerTok := notifyEnv(t)
	users := []id.ID{owner}
	for i := range 200 {
		u, _ := joinAs(t, e, cid, fmt.Sprintf("m%03d", i))
		users = append(users, u)
	}
	gw.reset()
	if status, _ := e.Do(http.MethodDelete, "/v1/communities/"+cid.String(), ownerTok, nil); status != http.StatusNoContent {
		t.Fatalf("delete = %d", status)
	}
	for _, u := range users {
		if got := gw.count(u); got != 1 {
			t.Fatalf("member %s was told %d times, want 1", u, got)
		}
	}
}
