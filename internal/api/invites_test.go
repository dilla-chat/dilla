package api_test

import (
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/jonasthim/dilla/internal/api"
	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/config"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/store"
)

// inviteEnv mounts what Plan 1a's serve mounts for invites: the community
// routes, the join route behind the ("invite", client address) bucket, and the
// two unauthenticated invite routes behind the same bucket. The plan's
// api.NewInvites(...).RegisterPublic and e.UseRateLimiter are Deps'
// RegisterPublicInvites and a limiter handed to both handlers here, the shapes
// Plan 1 shipped.
func inviteEnv(t *testing.T) (*env, id.ID, string) {
	t.Helper()
	return inviteEnvFor(t, "dilla.example", "")
}

// inviteEnvFor is inviteEnv with the instance domain the landing page names.
func inviteEnvFor(t *testing.T, domain, font string) (*env, id.ID, string) {
	t.Helper()
	e := newEnv(t)
	log := slog.New(slog.DiscardHandler)
	cfg := config.Default()
	limiter := server.NewRateLimiter(cfg.Limits.Rate, e.Clk)
	api.NewCommunities(e.Repo, e.DS, e.Clk, log).
		WithInviteMeter(limiter, cfg.Server.TrustedProxyCIDRs).Register(e.Mux)
	api.NewInvites(e.Repo, e.Clk, "https://dilla.example", log).Register(e.Mux)
	api.Deps{
		Repo: e.Repo, Clock: e.Clk, Log: log, Limiter: limiter, Config: cfg,
		Domain: domain, LandingFont: font,
	}.RegisterPublicInvites(e.Mux)
	_, tok := e.NewUser("owner")
	return e, createCommunity(t, e, tok), tok
}

// mintInvite creates one community invite and returns its plaintext code.
func mintInvite(t *testing.T, e *env, cid id.ID, tok string, maxUses, ttl uint64) string {
	t.Helper()
	status, body := e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/invites", tok,
		[]any{maxUses, ttl, uint64(0)})
	if status != http.StatusCreated {
		t.Fatalf("POST invite = %d (%x)", status, body)
	}
	var out []cbor.RawMessage
	mustUnmarshalBody(t, body, &out)
	var code string
	mustUnmarshal(t, out[1], &code)
	return code
}

func setPolicy(t *testing.T, e *env, cid id.ID, tok, policy string) {
	t.Helper()
	if status, body := e.Do(http.MethodPatch, "/v1/communities/"+cid.String(), tok,
		[]any{nil, []byte(policy), nil, nil}); status != http.StatusOK {
		t.Fatalf("PATCH policy = %d (%x)", status, body)
	}
}

func join(e *env, cid id.ID, tok string, invite any) (int, []byte) {
	return e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/join", tok, []any{invite})
}

func usedCount(t *testing.T, e *env, code string) uint64 {
	t.Helper()
	inv, err := e.Repo.GetInviteByHash(t.Context(), auth.HashInviteCode(code))
	if err != nil {
		t.Fatalf("GetInviteByHash: %v", err)
	}
	return inv.UsedCount
}

// rawGet is GET without a body, returning the response so a test can read its
// headers; e.Do returns only the status and the body.
func rawGet(t *testing.T, e *env, path, accept string) (getResult, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, e.Srv.URL+path, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := e.Srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return getResult{StatusCode: resp.StatusCode, Header: resp.Header}, body
}

// getResult is the part of an http.Response a test reads once the body is
// drained and closed.
type getResult struct {
	StatusCode int
	Header     http.Header
}

func TestACommunityInviteAuthorisesAJoin(t *testing.T) {
	e, cid, ownerTok := inviteEnv(t)
	// Make the community closed: no invite, no join.
	setPolicy(t, e, cid, ownerTok, `{"join":"invite"}`)
	member, memberTok := e.NewUser("newcomer")
	if status, body := join(e, cid, memberTok, nil); status != http.StatusForbidden {
		t.Fatalf("join without an invite = %d (%x)", status, body)
	}
	code := mintInvite(t, e, cid, ownerTok, 1, 3600)
	if status, body := join(e, cid, memberTok, code); status != http.StatusOK {
		t.Fatalf("join with an invite = %d (%x)", status, body)
	}
	if _, err := e.Repo.GetMember(t.Context(), cid, member); err != nil {
		t.Fatalf("the join did not write a member row: %v", err)
	}
	if got := usedCount(t, e, code); got != 1 {
		t.Fatalf("used_count = %d, want 1", got)
	}
}

func TestThePreviewNeverMutatesAndCarriesNoStore(t *testing.T) {
	e, cid, ownerTok := inviteEnv(t)
	code := mintInvite(t, e, cid, ownerTok, 5, 3600)

	resp, _ := rawGet(t, e, "/i/"+code, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /i/{code} = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := resp.Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("Referrer-Policy = %q", got)
	}
	if got := usedCount(t, e, code); got != 0 {
		t.Fatalf("the preview consumed a use: used_count = %d", got)
	}
}

func TestThePreviewNamesTheCommunity(t *testing.T) {
	e, cid, ownerTok := inviteEnv(t)
	if status, _ := e.Do(http.MethodPatch, "/v1/communities/"+cid.String(), ownerTok,
		[]any{"Kryptering <b>", nil, nil, nil}); status != http.StatusOK {
		t.Fatal("PATCH name failed")
	}
	code := mintInvite(t, e, cid, ownerTok, 5, 3600)

	resp, body := rawGet(t, e, "/i/"+code, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /i/{code} = %d", resp.StatusCode)
	}
	// html/template escapes the name: a community owner writes it, and the page
	// is handed to strangers.
	if !strings.Contains(string(body), "Kryptering &lt;b&gt;") || strings.Contains(string(body), "<b>") {
		t.Fatalf("the page does not carry the escaped community name:\n%s", body)
	}

	resp, body = rawGet(t, e, "/i/"+code, "application/cbor")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CBOR preview = %d", resp.StatusCode)
	}
	var out []cbor.RawMessage
	mustUnmarshal(t, body, &out)
	if len(out) != 4 {
		t.Fatalf("preview has %d elements, want 4", len(out))
	}
	var gotCID id.ID
	var name string
	mustUnmarshal(t, out[1], &gotCID)
	mustUnmarshal(t, out[3], &name)
	if gotCID != cid || name != "Kryptering <b>" {
		t.Fatalf("preview = community %s name %q", gotCID, name)
	}

	// A deleted community's invite is dead, and answers as an unknown one does.
	if status, _ := e.Do(http.MethodDelete, "/v1/communities/"+cid.String(), ownerTok, nil); status != http.StatusNoContent {
		t.Fatal("DELETE community failed")
	}
	resp, _ = rawGet(t, e, "/i/"+code, "")
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("preview of a deleted community's invite = %d, want 410", resp.StatusCode)
	}
}

func TestMaxUsesIsEnforcedUnderConcurrency(t *testing.T) {
	e, cid, ownerTok := inviteEnv(t)
	code := mintInvite(t, e, cid, ownerTok, 3, 3600)
	setPolicy(t, e, cid, ownerTok, `{"join":"invite"}`)

	const attempts = 12
	toks := make([]string, attempts)
	for i := range toks {
		_, toks[i] = e.NewUser(fmt.Sprintf("u%02d", i))
	}
	var wg sync.WaitGroup
	results := make([]int, attempts)
	for i := range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], _ = join(e, cid, toks[i], code)
		}()
	}
	wg.Wait()
	var ok int
	for _, s := range results {
		switch s {
		case http.StatusOK:
			ok++
		case http.StatusGone, http.StatusTooManyRequests:
		default:
			t.Fatalf("unexpected status %d", s)
		}
	}
	if ok != 3 {
		t.Fatalf("%d joins succeeded, want exactly 3 (%v)", ok, results)
	}
	if got := usedCount(t, e, code); got != 3 {
		t.Fatalf("used_count = %d, want 3", got)
	}
	members, err := e.Repo.ListMembersOfCommunity(t.Context(), cid, id.ID{}, 100)
	if err != nil {
		t.Fatalf("ListMembersOfCommunity: %v", err)
	}
	if len(members) != 1+3 { // the owner and three joiners
		t.Fatalf("%d members, want 4", len(members))
	}
}

func TestARevokedInviteIsGone(t *testing.T) {
	e, cid, ownerTok := inviteEnv(t)
	code := mintInvite(t, e, cid, ownerTok, 5, 3600)
	inv, err := e.Repo.GetInviteByHash(t.Context(), auth.HashInviteCode(code))
	if err != nil {
		t.Fatalf("GetInviteByHash: %v", err)
	}
	if status, _ := e.Do(http.MethodDelete, "/v1/invites/"+inv.ID.String(), ownerTok, nil); status != http.StatusNoContent {
		t.Fatal("DELETE invite failed")
	}
	_, tok := e.NewUser("late")
	status, body := join(e, cid, tok, code)
	if status != http.StatusGone {
		t.Fatalf("join with a revoked invite = %d", status)
	}
	if got := e.ErrCode(body); got != "E_INVITE_INVALID" {
		t.Fatalf("code = %s, want E_INVITE_INVALID", got)
	}
	if resp, _ := rawGet(t, e, "/i/"+code, ""); resp.StatusCode != http.StatusGone {
		t.Fatalf("preview of a revoked invite = %d, want 410", resp.StatusCode)
	}

	// Expiry is the same refusal.
	code2 := mintInvite(t, e, cid, ownerTok, 5, 60)
	e.Clk.Advance(2 * time.Minute)
	if status, _ := join(e, cid, tok, code2); status != http.StatusGone {
		t.Fatalf("join with an expired invite = %d", status)
	}

	// Revoking twice is a no-op that keeps the first revocation time.
	before, _ := e.Repo.GetInviteByHash(t.Context(), auth.HashInviteCode(code))
	e.Clk.Advance(time.Hour)
	if status, _ := e.Do(http.MethodDelete, "/v1/invites/"+inv.ID.String(), ownerTok, nil); status != http.StatusNoContent {
		t.Fatal("second DELETE invite failed")
	}
	after, _ := e.Repo.GetInviteByHash(t.Context(), auth.HashInviteCode(code))
	if before.RevokedAt == nil || after.RevokedAt == nil || *before.RevokedAt != *after.RevokedAt {
		t.Fatalf("revoked_at moved: %v then %v", before.RevokedAt, after.RevokedAt)
	}
}

// The burst half only. Plan 1a's server.RateLimiter takes a clock.Clock but uses
// it solely to age buckets out; the token bucket itself is
// golang.org/x/time/rate, whose Allow() reads time.Now(). Advancing e.Clk
// therefore refills nothing, and an assertion about refill would be a test of the
// wall clock. The refill rate is tested where it can be: in
// internal/server/ratelimit_test.go.
//
// Eight requests inside one burst window take well under a second, so "5 allowed,
// 3 limited" is deterministic.
func TestTheInviteBucketBurstsAtFive(t *testing.T) {
	e, cid, ownerTok := inviteEnv(t)
	code := mintInvite(t, e, cid, ownerTok, 1000, 3600)
	var ok, limited int
	for range 8 {
		resp, _ := rawGet(t, e, "/i/"+code, "")
		switch resp.StatusCode {
		case http.StatusOK:
			ok++
		case http.StatusTooManyRequests:
			limited++
		default:
			t.Fatalf("unexpected status %d", resp.StatusCode)
		}
	}
	if ok != 5 || limited != 3 {
		t.Fatalf("%d allowed and %d limited of 8, want 5 and 3 (burst 5)", ok, limited)
	}
	// A limited response carries Retry-After, which is what tells a client the
	// refill rate without the test having to wait for it.
	resp, _ := rawGet(t, e, "/i/"+code, "")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("a 429 carries no Retry-After")
	}
}

// The join is on the same bucket as the preview: a client that has spent its
// burst on previews cannot then guess codes through the join.
func TestTheJoinSharesTheInviteBucket(t *testing.T) {
	e, cid, ownerTok := inviteEnv(t)
	code := mintInvite(t, e, cid, ownerTok, 1000, 3600)
	for range 5 {
		if resp, _ := rawGet(t, e, "/i/"+code, ""); resp.StatusCode != http.StatusOK {
			t.Fatalf("preview = %d", resp.StatusCode)
		}
	}
	_, tok := e.NewUser("guesser")
	status, body := join(e, cid, tok, code)
	if status != http.StatusTooManyRequests {
		t.Fatalf("join on a spent bucket = %d, want 429", status)
	}
	if got := e.ErrCode(body); got != "E_RATE_LIMITED" {
		t.Fatalf("code = %s", got)
	}
	if got := usedCount(t, e, code); got != 0 {
		t.Fatalf("a rate-limited join burned %d use(s)", got)
	}
}

func TestARefusedJoinBurnsNoInviteUse(t *testing.T) {
	e, cid, ownerTok := inviteEnv(t)
	if status, _ := e.Do(http.MethodPatch, "/v1/communities/"+cid.String(), ownerTok,
		[]any{nil, nil, uint64(7 * 24 * 3600), nil}); status != http.StatusOK {
		t.Fatal("PATCH min_account_age_seconds failed")
	}
	// The invite must outlive the eight days the account needs to age: the
	// plan's 3600-second ttl would expire before the final join, and the test
	// would then be about expiry.
	code := mintInvite(t, e, cid, ownerTok, 1, 20*24*3600)
	_, freshTok := e.NewUser("fresh") // created at e.Clk.Now(), so too young
	if status, _ := join(e, cid, freshTok, code); status != http.StatusForbidden {
		t.Fatal("a too-young account was allowed to join")
	}
	if got := usedCount(t, e, code); got != 0 {
		t.Fatalf("a refused join burned %d use(s)", got)
	}
	// And the one use is still there for someone who passes the gate.
	e.Clk.Advance(8 * 24 * time.Hour)
	if status, _ := join(e, cid, freshTok, code); status != http.StatusOK {
		t.Fatal("the invite did not survive the refused join")
	}
}

// A ban gates a join that carries a valid invite, and burns no use.
func TestABannedAccountCannotJoinWithAnInvite(t *testing.T) {
	e, cid, ownerTok := inviteEnv(t)
	setPolicy(t, e, cid, ownerTok, `{"join":"invite"}`)
	code := mintInvite(t, e, cid, ownerTok, 1, 3600)
	banned, tok := e.NewUser("banned")
	if err := e.Repo.PutBan(t.Context(), store.BanRow{
		CommunityID: cid, UserID: banned, Reason: "spam", ByUser: ownerOf(t, e, cid), Created: e.Clk.Now().Unix(),
	}); err != nil {
		t.Fatalf("PutBan: %v", err)
	}
	status, body := join(e, cid, tok, code)
	if status != http.StatusForbidden {
		t.Fatalf("banned join = %d (%x)", status, body)
	}
	if got := usedCount(t, e, code); got != 0 {
		t.Fatalf("a banned join burned %d use(s)", got)
	}
}

// An invite minted for one community is refused, and burns nothing, at another.
func TestAnInviteForAnotherCommunityBurnsNoUse(t *testing.T) {
	e, cid, ownerTok := inviteEnv(t)
	other := createCommunity(t, e, ownerTok)
	setPolicy(t, e, cid, ownerTok, `{"join":"invite"}`)
	code := mintInvite(t, e, other, ownerTok, 1, 3600)
	_, tok := e.NewUser("wrong-door")
	status, body := join(e, cid, tok, code)
	if status != http.StatusGone {
		t.Fatalf("join with another community's invite = %d (%x)", status, body)
	}
	if got := usedCount(t, e, code); got != 0 {
		t.Fatalf("the refused join burned %d use(s)", got)
	}
	if status, _ := join(e, other, tok, code); status != http.StatusOK {
		t.Fatal("the invite no longer works at its own community")
	}
}

// CanonicalizeInviteCode returns an I candidate and an L candidate for a typed
// 1. The first candidate that does not exist must not end the search: a
// RedeemInvite on an unknown hash reports the same ErrExhausted a spent invite
// does, and treating it as terminal answers 410 for a legitimate code.
func TestACodeWithAnAmbiguousCharacterRedeems(t *testing.T) {
	e, cid, ownerTok := inviteEnv(t)
	setPolicy(t, e, cid, ownerTok, `{"join":"invite"}`)
	var code string
	for range 200 {
		code = mintInvite(t, e, cid, ownerTok, 1, 3600)
		// An L is the second candidate of its position, so the I candidate is
		// tried first and does not exist.
		if strings.Contains(code, "L") {
			break
		}
	}
	if !strings.Contains(code, "L") {
		t.Fatal("no minted code carried an L in 200 tries")
	}
	typed := strings.Replace(code, "L", "1", 1)
	if strings.Count(typed, "1") != 1 {
		t.Fatalf("typed spelling %q is not ambiguous in exactly one place", typed)
	}
	// Typed the way a person copies a code from a page: lower case, with hyphens.
	typed = strings.ToLower(typed[:5] + "-" + typed[5:])
	_, tok := e.NewUser("typist")
	if status, body := join(e, cid, tok, typed); status != http.StatusOK {
		t.Fatalf("join with the ambiguous spelling %q = %d (%x), want 200", typed, status, body)
	}
	if got := usedCount(t, e, code); got != 1 {
		t.Fatalf("used_count = %d, want 1", got)
	}
}

func TestAMalformedInviteIsABadRequest(t *testing.T) {
	e, cid, _ := inviteEnv(t)
	_, tok := e.NewUser("typo")
	status, body := join(e, cid, tok, "not a code")
	if status != http.StatusBadRequest || e.ErrCode(body) != "E_INVALID_REQUEST" {
		t.Fatalf("join with a malformed code = %d %s", status, e.ErrCode(body))
	}
}

func TestInviteRoutesAreGatedAndBounded(t *testing.T) {
	e, cid, ownerTok := inviteEnv(t)
	path := "/v1/communities/" + cid.String() + "/invites"

	// A non-member does not learn that the community exists.
	_, strangerTok := e.NewUser("stranger")
	if status, _ := e.Do(http.MethodPost, path, strangerTok, []any{uint64(1), uint64(60), uint64(0)}); status != http.StatusNotFound {
		t.Fatalf("stranger POST = %d, want 404", status)
	}
	if status, _ := e.Do(http.MethodGet, path, strangerTok, nil); status != http.StatusNotFound {
		t.Fatalf("stranger GET = %d, want 404", status)
	}

	// A member without create_invite is refused. @everyone loses the bit.
	roles, err := e.Repo.ListRoles(t.Context(), cid)
	if err != nil {
		t.Fatalf("ListRoles: %v", err)
	}
	everyone := roles[0]
	everyone.Allow &^= uint64(api.PermCreateInvite)
	if err := e.Repo.PutRole(t.Context(), everyone); err != nil {
		t.Fatalf("PutRole: %v", err)
	}
	_, memberTok := e.NewUser("member")
	if status, _ := join(e, cid, memberTok, nil); status != http.StatusOK {
		t.Fatal("join failed")
	}
	if status, _ := e.Do(http.MethodPost, path, memberTok, []any{uint64(1), uint64(60), uint64(0)}); status != http.StatusForbidden {
		t.Fatalf("member without create_invite POST = %d, want 403", status)
	}
	if status, _ := e.Do(http.MethodGet, path, memberTok, nil); status != http.StatusForbidden {
		t.Fatalf("member without create_invite GET = %d, want 403", status)
	}

	// Bounds.
	for name, body := range map[string][]any{
		// 0 uses and 0 ttl mean unlimited and never (Task 10c; invites_unlimited_test.go).
		"too many uses":  {uint64(1001), uint64(60), uint64(0)},
		"too long a ttl": {uint64(1), uint64(30*24*3600 + 1), uint64(0)},
		"bad admin flag": {uint64(1), uint64(60), uint64(2)},
	} {
		if status, _ := e.Do(http.MethodPost, path, ownerTok, body); status != http.StatusBadRequest {
			t.Fatalf("%s: POST = %d, want 400", name, status)
		}
	}
	// grants_admin is an instance-admin power, not a community owner's.
	if status, _ := e.Do(http.MethodPost, path, ownerTok, []any{uint64(1), uint64(60), uint64(1)}); status != http.StatusForbidden {
		t.Fatalf("owner asking grants_admin = %d, want 403", status)
	}
	// The instance-admin bit is not a community permission: the admin needs
	// create_invite like anyone, so @everyone gets it back.
	everyone.Allow |= uint64(api.PermCreateInvite)
	if err := e.Repo.PutRole(t.Context(), everyone); err != nil {
		t.Fatalf("PutRole: %v", err)
	}
	_, adminTok := newInstanceAdmin(t, e)
	if status, _ := join(e, cid, adminTok, nil); status != http.StatusOK {
		t.Fatal("admin join failed")
	}
	status, body := e.Do(http.MethodPost, path, adminTok, []any{uint64(1), uint64(60), uint64(1)})
	if status != http.StatusCreated {
		t.Fatalf("instance admin asking grants_admin = %d (%x)", status, body)
	}
}

// newInstanceAdmin is NewUser for an account whose users.flags carries the
// instance-admin bit, which only a bootstrap invite sets in production.
func newInstanceAdmin(t *testing.T, e *env) (id.ID, string) {
	t.Helper()
	uid, did := id.New(), id.New()
	now := e.Clk.Now().Unix()
	u := newAPITestUser(uid, "root", now)
	u.Flags = store.UserFlagInstanceAdmin
	if err := e.Repo.CreateUser(t.Context(), u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := e.Repo.CreateDevice(t.Context(), newAPITestDevice(did, uid, now)); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	tok := uid.String()
	e.sess[tok] = auth.Session{UserID: uid, DeviceID: did, Scope: auth.ScopeEnrolled}
	return uid, tok
}

func TestTheInviteListNamesEveryInviteAndNeverACode(t *testing.T) {
	e, cid, ownerTok := inviteEnv(t)
	owner := ownerOf(t, e, cid)
	code := mintInvite(t, e, cid, ownerTok, 7, 3600)

	status, body := e.Do(http.MethodGet, "/v1/communities/"+cid.String()+"/invites", ownerTok, nil)
	if status != http.StatusOK {
		t.Fatalf("GET invites = %d (%x)", status, body)
	}
	if strings.Contains(string(body), code) {
		t.Fatal("the list carries a plaintext code")
	}
	var rows [][]cbor.RawMessage
	mustUnmarshal(t, body, &rows)
	if len(rows) != 1 || len(rows[0]) != 9 {
		t.Fatalf("list = %d rows of %d elements, want 1 of 9", len(rows), len(rows[0]))
	}
	var inCommunity, by id.ID
	var grants, maxUses, used uint64
	mustUnmarshal(t, rows[0][1], &inCommunity)
	mustUnmarshal(t, rows[0][2], &by)
	mustUnmarshal(t, rows[0][3], &grants)
	mustUnmarshal(t, rows[0][4], &maxUses)
	mustUnmarshal(t, rows[0][5], &used)
	if inCommunity != cid || by != owner || grants != 0 || maxUses != 7 || used != 0 {
		t.Fatalf("row = %v %v %d %d %d", inCommunity, by, grants, maxUses, used)
	}
	if string(rows[0][8]) != string([]byte{0xf6}) {
		t.Fatalf("revoked_at = %x, want null", []byte(rows[0][8]))
	}
}

func TestOnlyThePermittedMayRevokeAnInvite(t *testing.T) {
	e, cid, ownerTok := inviteEnv(t)
	code := mintInvite(t, e, cid, ownerTok, 5, 3600)
	inv, err := e.Repo.GetInviteByHash(t.Context(), auth.HashInviteCode(code))
	if err != nil {
		t.Fatalf("GetInviteByHash: %v", err)
	}
	path := "/v1/invites/" + inv.ID.String()

	_, strangerTok := e.NewUser("stranger")
	if status, _ := e.Do(http.MethodDelete, path, strangerTok, nil); status != http.StatusNotFound {
		t.Fatalf("stranger DELETE = %d, want 404", status)
	}
	_, memberTok := e.NewUser("member")
	if status, _ := join(e, cid, memberTok, nil); status != http.StatusOK {
		t.Fatal("join failed")
	}
	if status, _ := e.Do(http.MethodDelete, path, memberTok, nil); status != http.StatusForbidden {
		t.Fatalf("member (not creator, no manage_community) DELETE = %d, want 403", status)
	}
	if status, _ := e.Do(http.MethodDelete, "/v1/invites/"+id.New().String(), ownerTok, nil); status != http.StatusNotFound {
		t.Fatalf("DELETE of an unknown invite = %d, want 404", status)
	}
	if got, _ := e.Repo.GetInviteByHash(t.Context(), inv.CodeHash); got.RevokedAt != nil {
		t.Fatal("a refused DELETE revoked the invite")
	}
}

// Ruling 34: the landing page is the only URL the instance mints for an invite, and it leads into
// the web client on the same origin, still with no script and no form.
func TestTheLandingPageLinksIntoTheWebClient(t *testing.T) {
	e, cid, ownerTok := inviteEnv(t)
	code := mintInvite(t, e, cid, ownerTok, 5, 3600)

	resp, body := rawGet(t, e, "/i/"+code, "text/html")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /i/{code} = %d", resp.StatusCode)
	}
	page := string(body)
	if !strings.Contains(page, `href="/welcome?invite=`+code+`"`) || !strings.Contains(page, `>Open dilla in this browser</a>`) {
		t.Fatalf("the landing page does not link into the web client:\n%s", page)
	}
	if !strings.Contains(page, "Or paste the code into a dilla client for dilla.example. This page never asks for a password.") {
		t.Fatalf("the landing page lost the paste-the-code sentence:\n%s", page)
	}
	for _, absent := range []string{"Open a dilla client", "<script", "<form"} {
		if strings.Contains(page, absent) {
			t.Fatalf("the landing page contains %q:\n%s", absent, page)
		}
	}
}

// landingEnv mints an invite straight into the store with an exact expiry and
// use count, for the tests that read the page's wording. The community is the
// env's own (named "Midgard Crew") unless instance is set, which is the shape
// `dillad init` writes for the bootstrap invite.
func landingEnv(t *testing.T, instance bool, maxUses, used uint64, expiresIn time.Duration) (*env, string) {
	t.Helper()
	return landingEnvFont(t, "", instance, maxUses, used, expiresIn)
}

// landingEnvFont is landingEnv with Deps.LandingFont set (the path api.LandingFont found).
func landingEnvFont(t *testing.T, font string, instance bool, maxUses, used uint64, expiresIn time.Duration) (*env, string) {
	t.Helper()
	e, cid, tok := inviteEnvFor(t, "dilla.test", font)
	if status, _ := e.Do(http.MethodPatch, "/v1/communities/"+cid.String(), tok,
		[]any{"Midgard Crew", nil, nil, nil}); status != http.StatusOK {
		t.Fatal("PATCH name failed")
	}
	code, hash := auth.NewInviteCode()
	row := store.InviteRow{
		ID: id.New(), CodeHash: hash, MaxUses: maxUses, UsedCount: used,
		Created: e.Clk.Now().Unix(), ExpiresAt: e.Clk.Now().Add(expiresIn).Unix(),
	}
	if !instance {
		row.CommunityID = &cid
	}
	if err := e.Repo.CreateInvite(t.Context(), row); err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	return e, code
}

func landingBody(t *testing.T, e *env, code string) (getResult, string) {
	t.Helper()
	resp, body := rawGet(t, e, "/i/"+code, "text/html")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /i/{code} = %d (%s)", resp.StatusCode, body)
	}
	return resp, string(body)
}

// Step 1 of INV: a community invite made through the route the "invite link"
// action calls always names its community.
func TestALandingOfACommunityInviteNamesTheCommunity(t *testing.T) {
	e, cid, tok := inviteEnv(t)
	if status, _ := e.Do(http.MethodPatch, "/v1/communities/"+cid.String(), tok,
		[]any{"Midgard Crew", nil, nil, nil}); status != http.StatusOK {
		t.Fatal("PATCH name failed")
	}
	code := mintInvite(t, e, cid, tok, 5, 3600)
	_, page := landingBody(t, e, code)
	if !strings.Contains(page, "Midgard Crew") {
		t.Fatalf("the landing page does not name the community:\n%s", page)
	}
	for _, want := range []string{
		"<title>Invitation to Midgard Crew on dilla.example</title>",
		"You have been invited to Midgard Crew",
		"on dilla.example",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("the landing page lacks %q:\n%s", want, page)
		}
	}
}

// The invite `dillad init` prints has no community, by design: the page then
// names the instance and prints no community line.
func TestLandingBootstrapInviteHasNoCommunity(t *testing.T) {
	e, code := landingEnv(t, true, 1, 0, 24*time.Hour)
	_, page := landingBody(t, e, code)
	for _, want := range []string{
		"<title>Invitation to dilla.test</title>",
		"<h1>You have been invited to dilla.test</h1>",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("the landing page lacks %q:\n%s", want, page)
		}
	}
	for _, absent := range []string{"Midgard", "Community", "community"} {
		if strings.Contains(page, absent) {
			t.Fatalf("an instance invite's page contains %q:\n%s", absent, page)
		}
	}
}

// R-A2: the page's one action is a 36px button for a fine pointer and a 44px primary target on a
// phone or any coarse pointer. The page has no script, so the query is its own.
func TestLandingOpenLinkIs44OnPhone(t *testing.T) {
	e, code := landingEnv(t, false, 5, 0, 24*time.Hour)
	_, page := landingBody(t, e, code)
	for _, want := range []string{
		".open{display:inline-flex;align-items:center;min-height:2.25rem",
		"@media (pointer: coarse), (max-width: 40rem){.open{min-height:2.75rem}}",
		`<span class="tile" aria-hidden="true">D</span>DILLA<span class="caret"`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("the landing page lacks %q:\n%s", want, page)
		}
	}
}

func TestLandingHasNoScriptNoFormOneLink(t *testing.T) {
	e, code := landingEnv(t, false, 5, 0, 24*time.Hour)
	_, page := landingBody(t, e, code)
	for _, absent := range []string{"<script", "<form", "<link", "<img", "http:", "https:", "src="} {
		if strings.Contains(page, absent) {
			t.Fatalf("the landing page contains %q:\n%s", absent, page)
		}
	}
	// The only url( a page may hold is the same-origin font; this env's web client carries none.
	if strings.Contains(page, "url(") {
		t.Fatalf("a page with no font names a url(:\n%s", page)
	}
	if n := strings.Count(page, "href="); n != 1 {
		t.Fatalf("the landing page has %d href=, want 1:\n%s", n, page)
	}
	if !strings.Contains(page, `href="/welcome?invite=`) {
		t.Fatalf("the one link is not same-origin /welcome?invite=:\n%s", page)
	}
}

func TestLandingHeaders(t *testing.T) {
	e, code := landingEnv(t, false, 5, 0, 24*time.Hour)
	resp, _ := landingBody(t, e, code)
	for k, want := range map[string]string{
		"Cache-Control":           "no-store",
		"Referrer-Policy":         "no-referrer",
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "default-src 'none'; style-src 'unsafe-inline'; font-src 'self'",
	} {
		if got := resp.Header.Get(k); got != want {
			t.Fatalf("%s = %q, want %q", k, got, want)
		}
	}
}

// R43: the page names the JetBrains Mono latin woff2 of the served web client, found once, and nothing else.
func TestLandingFont(t *testing.T) {
	const font = "/assets/jetbrains-mono-latin-wght-normal-abc.woff2"
	for _, tc := range []struct {
		name string
		tree fs.FS
		want string
	}{
		{"a tree with the font", fstest.MapFS{
			"assets/jetbrains-mono-latin-wght-normal-abc.woff2":    {Data: []byte("x")},
			"assets/jetbrains-mono-cyrillic-wght-normal-def.woff2": {Data: []byte("x")},
		}, font},
		{"an empty tree", fstest.MapFS{}, ""},
		{"no tree", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := api.LandingFont(tc.tree); got != tc.want {
				t.Fatalf("LandingFont = %q, want %q", got, tc.want)
			}
			e, code := landingEnvFont(t, api.LandingFont(tc.tree), false, 5, 0, 24*time.Hour)
			_, page := landingBody(t, e, code)
			if tc.want == "" {
				if strings.Contains(page, "@font-face") || strings.Contains(page, "url(") {
					t.Fatalf("a page without a font names one:\n%s", page)
				}
				if !strings.Contains(page, "ui-monospace") {
					t.Fatalf("the system stack is gone:\n%s", page)
				}
				return
			}
			if !strings.Contains(page, "@font-face") || !strings.Contains(page, `url("`+tc.want+`")`) {
				t.Fatalf("the page does not name %s:\n%s", tc.want, page)
			}
			if n := strings.Count(page, "url("); n != 1 {
				t.Fatalf("%d url( in the page, want 1", n)
			}
			if !strings.Contains(page, `"JetBrains Mono",ui-monospace`) {
				t.Fatalf("the font is not first in the stack:\n%s", page)
			}
		})
	}
}

func TestLandingPlurals(t *testing.T) {
	for _, tc := range []struct {
		remaining uint64
		want      string
	}{{1, "1 use left"}, {2, "2 uses left"}} {
		e, code := landingEnv(t, false, 5, 5-tc.remaining, 24*time.Hour)
		_, page := landingBody(t, e, code)
		if !strings.Contains(page, tc.want) {
			t.Fatalf("remaining %d: page lacks %q:\n%s", tc.remaining, tc.want, page)
		}
	}
}

func TestLandingExpiry(t *testing.T) {
	start := time.Unix(1_790_000_000, 0).UTC()
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{23*time.Hour + 59*time.Minute, "Expires in 23 hours"},
		{61 * time.Minute, "Expires in 1 hour"},
		{12 * time.Minute, "Expires in 12 minutes"},
		{30 * time.Second, "Expires in less than a minute"},
		{72 * time.Hour, "Expires on 24 September 2026 (UTC)"},
	} {
		e, code := landingEnv(t, false, 5, 0, tc.in)
		_, page := landingBody(t, e, code)
		if !strings.Contains(page, tc.want) {
			t.Fatalf("in %s: page lacks %q:\n%s", tc.in, tc.want, page)
		}
		iso := start.Add(tc.in).Format(time.RFC3339)
		if !strings.Contains(page, `<time datetime="`+iso+`">`) {
			t.Fatalf("in %s: page lacks <time datetime=%q>:\n%s", tc.in, iso, page)
		}
	}
}

func TestLandingOpenGraph(t *testing.T) {
	e, code := landingEnv(t, false, 5, 0, 24*time.Hour)
	_, page := landingBody(t, e, code)
	for _, want := range []string{
		`<meta property="og:title" content="Invitation to Midgard Crew on dilla.test">`,
		`<meta property="og:description" content="Join Midgard Crew on dilla.test, a dilla server. Open the link to accept.">`,
		`<meta name="description" content="Join Midgard Crew on dilla.test, a dilla server. Open the link to accept.">`,
		`<meta property="og:type" content="website">`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("the page lacks %s:\n%s", want, page)
		}
	}
	for _, absent := range []string{"og:url", "og:image"} {
		if strings.Contains(page, absent) {
			t.Fatalf("the page carries %s", absent)
		}
	}
	for _, line := range strings.Split(page, "\n") {
		if strings.Contains(line, "<meta") && (strings.Contains(line, code) || strings.Contains(line, "Expires") || strings.Contains(line, "use left") || strings.Contains(line, "uses left")) {
			t.Fatalf("a meta tag carries the code, expiry or uses: %s", line)
		}
	}

	e, code = landingEnv(t, true, 1, 0, 24*time.Hour)
	_, page = landingBody(t, e, code)
	for _, want := range []string{
		`<meta property="og:title" content="Invitation to dilla.test">`,
		`<meta property="og:description" content="Join dilla.test, a dilla server. Open the link to accept.">`,
		`<meta name="description" content="Join dilla.test, a dilla server. Open the link to accept.">`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("the instance page lacks %s:\n%s", want, page)
		}
	}
}

// The template's literal colours are the Signal tokens, dark (the signal theme)
// and light (the light theme the system preference selects), read from the
// generated tokens.css so a token change fails here until the page follows.
func TestLandingTokensMatchSignal(t *testing.T) {
	raw, err := os.ReadFile("../../packages/design-tokens/dist/tokens.css")
	if err != nil {
		t.Fatalf("read tokens.css: %v", err)
	}
	css := string(raw)
	block := func(head string) string {
		at := strings.Index(css, head)
		if at < 0 {
			t.Fatalf("tokens.css has no %s block", head)
		}
		b := css[at:]
		return b[:strings.Index(b, "}")]
	}
	dark := block(`[data-theme="signal"] {`)
	light := block("@media (prefers-color-scheme: light)")

	e, code := landingEnv(t, false, 5, 0, 24*time.Hour)
	_, page := landingBody(t, e, code)
	cut := strings.Index(page, "prefers-color-scheme: light")
	if cut < 0 {
		t.Fatalf("the page has no light scheme:\n%s", page)
	}
	pageDark, pageLight := page[:cut], page[cut:]
	decl := regexp.MustCompile(`--(bg|bg-2|surface|fg|fg-2|accent|accent-ink|hairline):\s*(#[0-9A-Fa-f]{6})`)
	find := func(src, name string) string {
		for _, m := range decl.FindAllStringSubmatch(src, -1) {
			if m[1] == name {
				return m[2]
			}
		}
		return ""
	}
	for _, name := range []string{"bg", "bg-2", "surface", "fg", "fg-2", "accent", "accent-ink", "hairline"} {
		for _, side := range []struct{ tokens, page string }{{dark, pageDark}, {light, pageLight}} {
			want, got := find(side.tokens, name), find(side.page, name)
			if want == "" || !strings.EqualFold(got, want) {
				t.Fatalf("--%s = %q in the page, %q in tokens.css", name, got, want)
			}
		}
	}
}

// Guard, green at start: a landing page never spends a use.
func TestLandingNeverMutates(t *testing.T) {
	e, code := landingEnv(t, false, 5, 0, 24*time.Hour)
	for range 3 {
		landingBody(t, e, code)
	}
	if got := usedCount(t, e, code); got != 0 {
		t.Fatalf("used_count = %d after 3 GETs", got)
	}
}
