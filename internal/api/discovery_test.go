package api_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// discCommunity is one row of GET /v1/communities (L-HTTP-01).
type discCommunity struct {
	_             struct{} `cbor:",toarray"`
	CommunityID   id.ID
	Name          string
	Owner         id.ID
	PolicyVersion uint64
}

// discMember is one row of GET /v1/communities/{id}/members (L-HTTP-03).
type discMember struct {
	_        struct{} `cbor:",toarray"`
	UserID   id.ID
	Joined   int64
	Nick     string
	Roles    []id.ID
	Username string
	Display  string
	Kind     uint64
	Status   string
	Bio      string
}

// discGet is one GET that must answer 200; it returns the body.
func discGet(t *testing.T, e *env, path, tok string) []byte {
	t.Helper()
	status, body := e.Do(http.MethodGet, path, tok, nil)
	if status != http.StatusOK {
		t.Fatalf("GET %s = %d (%x), want 200", path, status, body)
	}
	return body
}

// discMine is GET /v1/communities as tok, every row checked to be exactly four elements.
func discMine(t *testing.T, e *env, tok string) []discCommunity {
	t.Helper()
	body := discGet(t, e, "/v1/communities", tok)
	var raw [][]cbor.RawMessage
	mustUnmarshal(t, body, &raw)
	for i, r := range raw {
		if len(r) != 4 {
			t.Fatalf("GET /v1/communities row %d has %d elements, want 4", i, len(r))
		}
	}
	var rows []discCommunity
	mustUnmarshal(t, body, &rows)
	return rows
}

// discCreate is POST /v1/communities with a name, answering the new community's id.
func discCreate(t *testing.T, e *env, tok, name string) id.ID {
	t.Helper()
	status, body := e.Do(http.MethodPost, "/v1/communities", tok, []any{name, []byte(`{}`), uint64(0), uint64(0)})
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/communities %q = %d (%x)", name, status, body)
	}
	var created []cbor.RawMessage
	mustUnmarshal(t, body, &created)
	var cid id.ID
	mustUnmarshal(t, created[0], &cid)
	return cid
}

// discChannelGroup is element 11 of GET /v1/channels/{id}, the document checked to be twelve
// elements long.
func discChannelGroup(t *testing.T, e *env, tok string, ch id.ID) *id.ID {
	t.Helper()
	var doc []cbor.RawMessage
	mustUnmarshal(t, discGet(t, e, "/v1/channels/"+ch.String(), tok), &doc)
	if len(doc) != 12 {
		t.Fatalf("GET /v1/channels/{id} has %d elements, want 12", len(doc))
	}
	var g *id.ID
	mustUnmarshal(t, doc[11], &g)
	return g
}

// discListedGroup is element 10 of ch's row of GET /v1/communities/{cid}/channels, every row
// checked to be eleven elements long; listed is false when ch has no row.
func discListedGroup(t *testing.T, e *env, tok string, cid, ch id.ID) (g *id.ID, listed bool) {
	t.Helper()
	var rows [][]cbor.RawMessage
	mustUnmarshal(t, discGet(t, e, "/v1/communities/"+cid.String()+"/channels", tok), &rows)
	for i, r := range rows {
		if len(r) != 11 {
			t.Fatalf("channel list row %d has %d elements, want 11", i, len(r))
		}
		var rowID id.ID
		mustUnmarshal(t, r[0], &rowID)
		if rowID == ch {
			mustUnmarshal(t, r[10], &g)
			listed = true
		}
	}
	return g, listed
}

// discMembers is GET /v1/communities/{cid}/members as tok, keyed by user id, every row checked to
// be nine elements long (the last two are the status message and the bio, EOD E6).
func discMembers(t *testing.T, e *env, cid id.ID, tok string) map[id.ID]discMember {
	t.Helper()
	body := discGet(t, e, "/v1/communities/"+cid.String()+"/members", tok)
	var raw [][]cbor.RawMessage
	mustUnmarshal(t, body, &raw)
	for i, r := range raw {
		if len(r) != 9 {
			t.Fatalf("member row %d has %d elements, want 9", i, len(r))
		}
	}
	var rows []discMember
	mustUnmarshal(t, body, &rows)
	out := make(map[id.ID]discMember, len(rows))
	for _, m := range rows {
		out[m.UserID] = m
	}
	return out
}

// discSeedEpochUnknownTextGroup writes one open, epoch-unknown text group row bound to target.
func discSeedEpochUnknownTextGroup(t *testing.T, e *env, target, cid id.ID) id.ID {
	t.Helper()
	c := cid
	deadline := e.Clk.Now().Unix() + 3600
	g := store.GroupRow{
		GroupID: id.New(), Binding: []byte{0x80}, Kind: 0, CommunityID: &c, TargetID: target,
		Ciphersuite: 1, ExternalSenderKeyID: id.New(), E2EEVersion: 1, MediaVersion: 1,
		PolicyVersion: 1, EpochUnknown: true, HealDeadline: &deadline, Created: e.Clk.Now().Unix(),
	}
	if err := e.Repo.CreateGroup(t.Context(), g); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	return g.GroupID
}

// discSameID reports whether got names want.
func discSameID(got *id.ID, want id.ID) bool { return got != nil && *got == want }

// F13: GET /v1/communities lists the caller's own memberships and nothing else.
func TestListMyCommunitiesShowsOnlyTheCallersOwnMemberships(t *testing.T) {
	e := communityEnv(t)
	owner, ownerTok := e.NewUser("owner")
	_, memberTok := e.NewUser("member")
	_, strangerTok := e.NewUser("stranger")
	_, otherTok := e.NewUser("other")
	alpha := discCreate(t, e, ownerTok, "alpha")
	beta := discCreate(t, e, ownerTok, "beta")
	foreign := discCreate(t, e, otherTok, "foreign")
	joinCommunity(t, e, alpha, memberTok)

	// A caller with no membership gets the empty array, byte for byte: never null, never another
	// user's communities.
	if body := discGet(t, e, "/v1/communities", strangerTok); !bytes.Equal(body, []byte{0x80}) {
		t.Fatalf("a caller with no membership got %x, want 80 (the empty array)", body)
	}

	want := slices.SortedFunc(slices.Values([]id.ID{alpha, beta}), func(a, b id.ID) int { return bytes.Compare(a[:], b[:]) })
	rows := discMine(t, e, ownerTok)
	if len(rows) != 2 || rows[0].CommunityID != want[0] || rows[1].CommunityID != want[1] {
		t.Fatalf("the owner's list = %+v, want alpha and beta in id order %v", rows, want)
	}
	if slices.ContainsFunc(rows, func(r discCommunity) bool { return r.CommunityID == foreign }) {
		t.Fatalf("the owner's list names %s, a community it is not a member of", foreign)
	}
	for _, r := range rows {
		wantName := map[id.ID]string{alpha: "alpha", beta: "beta"}[r.CommunityID]
		if r.Name != wantName || r.Owner != owner || r.PolicyVersion != 1 {
			t.Fatalf("row %+v, want name %q, owner %s, policy_version 1", r, wantName, owner)
		}
	}
	if rows := discMine(t, e, memberTok); len(rows) != 1 || rows[0].CommunityID != alpha || rows[0].Name != "alpha" || rows[0].Owner != owner {
		t.Fatalf("the member's list = %+v, want only alpha", rows)
	}

	// The listing follows the community's policy version.
	if status, body := e.Do(http.MethodPatch, "/v1/communities/"+alpha.String(), ownerTok,
		[]any{nil, []byte(`{"retention_days":30}`), nil, nil}); status != http.StatusOK {
		t.Fatalf("PATCH = %d (%x)", status, body)
	}
	if rows := discMine(t, e, memberTok); len(rows) != 1 || rows[0].PolicyVersion != 2 {
		t.Fatalf("after the policy PATCH the member's list = %+v, want policy_version 2", rows)
	}

	// Leaving takes the community off the caller's list; deleting takes it off everyone's.
	if status, body := e.Do(http.MethodPost, "/v1/communities/"+alpha.String()+"/leave", memberTok, []any{}); status != http.StatusNoContent {
		t.Fatalf("leave = %d (%x)", status, body)
	}
	if body := discGet(t, e, "/v1/communities", memberTok); !bytes.Equal(body, []byte{0x80}) {
		t.Fatalf("after leaving the member's list is %x, want 80", body)
	}
	if status, body := e.Do(http.MethodDelete, "/v1/communities/"+beta.String(), ownerTok, nil); status != http.StatusNoContent {
		t.Fatalf("DELETE = %d (%x)", status, body)
	}
	if rows := discMine(t, e, ownerTok); len(rows) != 1 || rows[0].CommunityID != alpha {
		t.Fatalf("after deleting beta the owner's list = %+v, want only alpha", rows)
	}
}

// GET /v1/communities is "E": no session is 401, and a session that is not enrolled is refused by
// the handler itself even where a middleware let it through.
func TestListMyCommunitiesNeedsAnEnrolledSession(t *testing.T) {
	e := communityEnv(t)
	_, tok := e.NewUser("owner")
	discCreate(t, e, tok, "alpha")

	status, body := e.Do(http.MethodGet, "/v1/communities", "", nil)
	if status != http.StatusUnauthorized || e.ErrCode(body) != "E_UNAUTHENTICATED" {
		t.Fatalf("no session = %d (%x), want 401 E_UNAUTHENTICATED", status, body)
	}
	for _, scope := range []auth.Scope{auth.ScopePending, auth.ScopeProvisional} {
		_, scopedTok := e.NewUser("scoped")
		s := e.sess[scopedTok]
		s.Scope = scope
		e.sess[scopedTok] = s
		status, body := e.Do(http.MethodGet, "/v1/communities", scopedTok, nil)
		if status != http.StatusForbidden || e.ErrCode(body) != "E_FORBIDDEN" {
			t.Fatalf("scope %d = %d (%x), want 403 E_FORBIDDEN", scope, status, body)
		}
	}
}

// L-HTTP-02: both channel reads carry the channel's oldest open text group, epoch-unknown or not,
// null before one is registered and after the last one closes; a call group is not a text group.
func TestChannelReadsCarryTheOldestOpenTextGroup(t *testing.T) {
	e, cid, tok := channelEnv(t)
	secret, _, status := newChannel(t, e, cid, tok, 0, 0, 0, "secret")
	if status != http.StatusCreated {
		t.Fatalf("POST channel = %d", status)
	}
	both := func(what string, want *id.ID) {
		t.Helper()
		got := discChannelGroup(t, e, tok, secret)
		listed, ok := discListedGroup(t, e, tok, cid, secret)
		if !ok {
			t.Fatalf("%s: the channel is not listed", what)
		}
		for name, g := range map[string]*id.ID{"GET /v1/channels/{id}": got, "the channel list": listed} {
			if want == nil && g != nil {
				t.Fatalf("%s: %s text_group_id = %s, want null", what, name, g)
			}
			if want != nil && !discSameID(g, *want) {
				t.Fatalf("%s: %s text_group_id = %v, want %s", what, name, g, *want)
			}
		}
	}

	seedGroupOfKind(t, e, secret, cid, 1) // the channel's call group
	both("before any text group is registered", nil)

	e.Clk.Advance(time.Second)
	first := seedTextGroup(t, e, secret, cid)
	e.Clk.Advance(time.Second)
	second := discSeedEpochUnknownTextGroup(t, e, secret, cid)
	both("two open text groups", &first)

	if err := e.Repo.CloseGroup(t.Context(), first, e.Clk.Now().Unix()); err != nil {
		t.Fatalf("CloseGroup: %v", err)
	}
	both("the oldest closed, an epoch-unknown one open", &second)

	if err := e.Repo.CloseGroup(t.Context(), second, e.Clk.Now().Unix()); err != nil {
		t.Fatalf("CloseGroup: %v", err)
	}
	both("every text group closed", nil)
}

// L-HTTP-02: a channel that carries no text group answers null even when a text-kind group row
// names it (one the delivery service would refuse to register: TextGroupAllowed is false).
func TestChannelsWithoutATextGroupAnswerNull(t *testing.T) {
	e, cid, tok := channelEnv(t)
	for _, c := range []struct {
		name            string
		kind, mode, vis uint64
	}{
		{"plain", 0, 1, 0},   // a server-readable text channel
		{"public", 0, 0, 2},  // discoverable: forced readable at creation
		{"voice", 1, 0, 0},   // a voice channel
		{"section", 2, 0, 0}, // a category
	} {
		ch, _, status := newChannel(t, e, cid, tok, c.kind, c.mode, c.vis, c.name)
		if status != http.StatusCreated {
			t.Fatalf("POST channel %s = %d", c.name, status)
		}
		seedTextGroup(t, e, ch, cid)
		if g := discChannelGroup(t, e, tok, ch); g != nil {
			t.Fatalf("%s: GET /v1/channels/{id} text_group_id = %s, want null", c.name, g)
		}
		g, listed := discListedGroup(t, e, tok, cid, ch)
		if !listed {
			t.Fatalf("%s: not listed", c.name)
		}
		if g != nil {
			t.Fatalf("%s: the channel list's text_group_id = %s, want null", c.name, g)
		}
	}
}

// F13: the discovery elements reach only callers who could already read the channel or the
// community: a non-member gets 404 on all three reads, and a member an overwrite takes the channel
// from gets 404 and no row, although the channel has a text group.
func TestDiscoveryReadsStillHideWhatTheCallerCannotView(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	secret, _, _ := newChannel(t, e, cid, ownerTok, 0, 0, 0, "secret")
	group := seedTextGroup(t, e, secret, cid)
	_, memberTok := e.NewUser("member")
	joinCommunity(t, e, cid, memberTok)
	_, outsiderTok := e.NewUser("outsider")

	if g := discChannelGroup(t, e, memberTok, secret); !discSameID(g, group) {
		t.Fatalf("the member's text_group_id = %v, want %s", g, group)
	}
	for _, path := range []string{
		"/v1/channels/" + secret.String(),
		"/v1/communities/" + cid.String() + "/channels",
		"/v1/communities/" + cid.String() + "/members",
	} {
		status, body := e.Do(http.MethodGet, path, outsiderTok, nil)
		if status != http.StatusNotFound || e.ErrCode(body) != "E_NOT_FOUND" {
			t.Fatalf("a non-member's GET %s = %d (%x), want 404 E_NOT_FOUND", path, status, body)
		}
	}

	roles, err := e.Repo.ListRoles(t.Context(), cid)
	if err != nil || len(roles) != 1 {
		t.Fatalf("ListRoles = %v, %v; want the one @everyone role", roles, err)
	}
	if status, body := e.Do(http.MethodPut, "/v1/channels/"+secret.String()+"/overwrites/0/"+roles[0].ID.String(), ownerTok,
		[]any{uint64(0), uint64(api.PermViewChannel)}); status != http.StatusNoContent {
		t.Fatalf("PUT deny-view = %d (%x)", status, body)
	}
	status, body := e.Do(http.MethodGet, "/v1/channels/"+secret.String(), memberTok, nil)
	if status != http.StatusNotFound || e.ErrCode(body) != "E_NOT_FOUND" {
		t.Fatalf("GET a channel the member may not view = %d (%x), want 404 E_NOT_FOUND", status, body)
	}
	if _, listed := discListedGroup(t, e, memberTok, cid, secret); listed {
		t.Fatal("a channel the member may not view is listed")
	}
	if g := discChannelGroup(t, e, ownerTok, secret); !discSameID(g, group) {
		t.Fatalf("the owner's text_group_id = %v, want %s", g, group)
	}
}

// L-HTTP-03: member rows carry the account's username, display name and kind.
func TestMemberRowsCarryUsernameDisplayAndKind(t *testing.T) {
	e := communityEnv(t)
	owner, ownerTok := e.NewUser("owner")
	cid := discCreate(t, e, ownerTok, "alpha")
	member, memberTok := e.NewUser("member")
	joinCommunity(t, e, cid, memberTok)
	bot := id.New()
	botRow := newAPITestUser(bot, "relay", e.Clk.Now().Unix())
	botRow.Kind = 1
	if err := e.Repo.CreateUser(t.Context(), botRow); err != nil {
		t.Fatalf("CreateUser bot: %v", err)
	}
	if err := e.Repo.PutMember(t.Context(), store.MemberOfCommunityRow{
		CommunityID: cid, UserID: bot, Joined: e.Clk.Now().Unix(), Nick: "",
	}); err != nil {
		t.Fatalf("PutMember bot: %v", err)
	}

	rows := discMembers(t, e, cid, memberTok)
	if len(rows) != 3 {
		t.Fatalf("members = %+v, want owner, member and bot", rows)
	}
	for _, uid := range []id.ID{owner, member, bot} {
		u, err := e.Repo.GetUser(t.Context(), uid)
		if err != nil {
			t.Fatalf("GetUser: %v", err)
		}
		m, err := e.Repo.GetMember(t.Context(), cid, uid)
		if err != nil {
			t.Fatalf("GetMember: %v", err)
		}
		r, ok := rows[uid]
		if !ok {
			t.Fatalf("%s is missing from the member rows", uid)
		}
		if u.Username == "" || u.Display == "" {
			t.Fatalf("fixture: user %s has an empty username or display", uid)
		}
		if r.Username != u.Username || r.Display != u.Display || r.Kind != uint64(u.Kind) ||
			r.Nick != m.Nick || r.Joined != m.Joined {
			t.Fatalf("row %+v, want username %q, display %q, kind %d, nick %q, joined %d",
				r, u.Username, u.Display, u.Kind, m.Nick, m.Joined)
		}
	}
	if rows[owner].Display != "owner" || rows[owner].Kind != 0 || rows[bot].Kind != 1 {
		t.Fatalf("owner %+v, bot %+v: want display \"owner\", kinds 0 and 1", rows[owner], rows[bot])
	}
}

// discUserLookup hides or breaks GetUser for chosen users; every other method is the real store.
type discUserLookup struct {
	store.Repository
	mu      sync.Mutex
	missing map[id.ID]bool
	broken  map[id.ID]bool
}

var errDiscDisk = errors.New("disc: the disk is gone")

func (r *discUserLookup) GetUser(ctx context.Context, uid id.ID) (store.UserRow, error) {
	r.mu.Lock()
	missing, broken := r.missing[uid], r.broken[uid]
	r.mu.Unlock()
	switch {
	case missing:
		return store.UserRow{}, store.ErrNotFound
	case broken:
		return store.UserRow{}, errDiscDisk
	}
	return r.Repository.GetUser(ctx, uid)
}

// L-HTTP-03: a member whose user row is gone answers "", "", 0 and the list still answers; any
// other lookup failure is a 500, never a row with made-up names.
func TestMemberRowsSurviveAMissingUserRowAndFailOnAStoreError(t *testing.T) {
	e := newEnv(t)
	repo := &discUserLookup{Repository: e.Repo, missing: map[id.ID]bool{}, broken: map[id.ID]bool{}}
	api.NewCommunities(repo, e.DS, e.Clk, slog.New(slog.DiscardHandler)).Register(e.Mux)
	_, ownerTok := e.NewUser("owner")
	cid := discCreate(t, e, ownerTok, "alpha")
	ghost, ghostTok := e.NewUser("ghost")
	joinCommunity(t, e, cid, ghostTok)

	repo.mu.Lock()
	repo.missing[ghost] = true
	repo.mu.Unlock()
	rows := discMembers(t, e, cid, ownerTok)
	g, ok := rows[ghost]
	if len(rows) != 2 || !ok {
		t.Fatalf("members = %+v, want the owner and the ghost", rows)
	}
	if g.Username != "" || g.Display != "" || g.Kind != 0 {
		t.Fatalf("a member without a user row = %+v, want \"\", \"\", 0", g)
	}

	repo.mu.Lock()
	repo.missing[ghost] = false
	repo.broken[ghost] = true
	repo.mu.Unlock()
	status, body := e.Do(http.MethodGet, "/v1/communities/"+cid.String()+"/members", ownerTok, nil)
	if status != http.StatusInternalServerError || e.ErrCode(body) != "E_INTERNAL" {
		t.Fatalf("a failing user lookup = %d (%x), want 500 E_INTERNAL", status, body)
	}
}
