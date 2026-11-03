package api_test

import (
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/id"
)

// mountSignUp mounts POST /v1/accounts on e, the real registration handler.
func mountSignUp(t *testing.T, e *env) {
	cfg := config.Default()
	deps := api.Deps{
		Repo: e.Repo, Clock: e.Clk, Log: slog.New(slog.DiscardHandler), Config: cfg,
		Registration: cfg.Registration, Domain: "dilla.example",
		Sessions:    auth.NewSessions(e.Repo, e.Clk, cfg.Auth.Session, id.New(), 1),
		BotIdentity: sharedTestVerifier(t),
	}
	e.Mux.HandleFunc("POST /v1/accounts", deps.CreateAccount)
}

// signUpWith registers one account with code, the way a friend's first visit
// does, and returns the new user's token in e's session stand-in, enrolled, so
// the test can go on to join.
func signUpWith(t *testing.T, e *env, code, username string) (id.ID, string) {
	t.Helper()
	did := id.New()
	credential := makeTestHumanCredential(t, did)
	status, body := e.Do(http.MethodPost, "/v1/accounts", "", []any{
		code, username, username,
		credential.UMKPub, credential.SSKPub, credential.SigUMKSSK, nil,
		[]any{did, credential.DSKPub, uint64(0), uint64(0), credential.Credential},
	})
	if status != http.StatusOK {
		t.Fatalf("POST /v1/accounts = %d (%x)", status, body)
	}
	var out []cbor.RawMessage
	mustUnmarshalBody(t, body, &out)
	var uid id.ID
	mustUnmarshal(t, out[0], &uid)
	tok := "signup-" + uid.String()
	e.sess[tok] = auth.Session{UserID: uid, DeviceID: did, Scope: auth.ScopeEnrolled}
	return uid, tok
}

// WB walk I1: a new person costs a community invite one use, not two. The
// sign-up redeems the invite and the instance remembers who redeemed it; the
// join that follows, by that person with that invite, spends nothing more.
func TestASignUpAndItsJoinSpendOneInviteUse(t *testing.T) {
	e, cid, ownerTok := inviteEnv(t)
	mountSignUp(t, e)
	setPolicy(t, e, cid, ownerTok, `{"join":"invite"}`)
	code := mintInvite(t, e, cid, ownerTok, 1, 3600)

	friend, friendTok := signUpWith(t, e, code, "friend")
	if got := usedCount(t, e, code); got != 1 {
		t.Fatalf("after the sign-up used_count = %d, want 1", got)
	}
	if status, body := join(e, cid, friendTok, code); status != http.StatusOK {
		t.Fatalf("the sign-up's own join with its last use = %d (%x), want 200", status, body)
	}
	if _, err := e.Repo.GetMember(t.Context(), cid, friend); err != nil {
		t.Fatalf("the join wrote no member row: %v", err)
	}
	if got := usedCount(t, e, code); got != 1 {
		t.Fatalf("after the join used_count = %d, want still 1", got)
	}

	// The free join is the sign-up's, once. Someone who did not sign up with the
	// invite meets it used up.
	_, otherTok := e.NewUser("other")
	if status, _ := join(e, cid, otherTok, code); status != http.StatusGone {
		t.Fatalf("a stranger's join with the used-up invite = %d, want 410", status)
	}
	// And the person who did, once they have left, pays like anyone else.
	if status, body := e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/leave", friendTok, []any{}); status != http.StatusNoContent {
		t.Fatalf("leave = %d (%x)", status, body)
	}
	if status, _ := join(e, cid, friendTok, code); status != http.StatusGone {
		t.Fatalf("a second join after leaving = %d, want 410 (the free join was spent)", status)
	}
}

// The remembered sign-up does not outlive a revocation: a revoked invite admits
// nobody, the person who signed up with it included.
func TestARevokedInviteRefusesTheSignUpsJoin(t *testing.T) {
	e, cid, ownerTok := inviteEnv(t)
	mountSignUp(t, e)
	code := mintInvite(t, e, cid, ownerTok, 5, 3600)
	_, friendTok := signUpWith(t, e, code, "friend")

	inv, err := e.Repo.GetInviteByHash(t.Context(), auth.HashInviteCode(code))
	if err != nil {
		t.Fatalf("GetInviteByHash: %v", err)
	}
	if status, body := e.Do(http.MethodDelete, "/v1/invites/"+inv.ID.String(), ownerTok, nil); status != http.StatusNoContent {
		t.Fatalf("revoke = %d (%x)", status, body)
	}
	if status, _ := join(e, cid, friendTok, code); status != http.StatusGone {
		t.Fatalf("the sign-up's join with a revoked invite = %d, want 410", status)
	}
}

// Nor does it outlive the invite's expiry: once the TTL has passed, the person
// who signed up with the invite meets it expired like anyone else.
func TestAnExpiredInviteRefusesTheSignUpsJoin(t *testing.T) {
	e, cid, ownerTok := inviteEnv(t)
	mountSignUp(t, e)
	code := mintInvite(t, e, cid, ownerTok, 5, 3600)
	friend, friendTok := signUpWith(t, e, code, "friend")

	e.Clk.Advance(3601 * time.Second)
	if status, _ := join(e, cid, friendTok, code); status != http.StatusGone {
		t.Fatalf("the sign-up's join with an expired invite = %d, want 410", status)
	}
	if _, err := e.Repo.GetMember(t.Context(), cid, friend); err == nil {
		t.Fatal("the refused join wrote a member row")
	}
}
