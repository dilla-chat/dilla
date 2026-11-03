package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/id"
)

// PATCH /v1/accounts/me is [display|null, status_msg|null, bio|null]; a null leaves the field alone.
func TestPatchMeWritesTheProfileAndGetMeReadsItBack(t *testing.T) {
	h, deps := newTestAPI(t)
	user, _, token := seedSession(t, deps)
	ctx := context.Background()

	if rec := cborCall(t, h, http.MethodPatch, "/v1/accounts/me", token, []any{"Ada L", "on a call", "first\nsecond"}); rec.Code != http.StatusNoContent {
		t.Fatalf("patch = %d: %s", rec.Code, rec.Body.String())
	}
	got, err := deps.Repo.GetUser(ctx, user.ID)
	if err != nil || got.Display != "Ada L" || got.StatusMsg != "on a call" || got.Bio != "first\nsecond" {
		t.Fatalf("stored = %+v, %v", got, err)
	}

	// nulls keep the other fields; an empty string clears one.
	if rec := cborCall(t, h, http.MethodPatch, "/v1/accounts/me", token, []any{nil, "", nil}); rec.Code != http.StatusNoContent {
		t.Fatalf("clear status = %d: %s", rec.Code, rec.Body.String())
	}
	got, _ = deps.Repo.GetUser(ctx, user.ID)
	if got.Display != "Ada L" || got.StatusMsg != "" || got.Bio != "first\nsecond" {
		t.Fatalf("after clearing the status: %+v", got)
	}
	if rec := cborCall(t, h, http.MethodPatch, "/v1/accounts/me", token, []any{nil, nil, nil}); rec.Code != http.StatusNoContent {
		t.Fatalf("all null = %d", rec.Code)
	}

	var me []cbor.RawMessage
	rec := doAuth(h, http.MethodGet, "/v1/accounts/me", token)
	if err := cbor.Unmarshal(rec.Body.Bytes(), &me); err != nil || len(me) != 9 {
		t.Fatalf("GET /me = %d elements, %v; want 9", len(me), err)
	}
	var display, status, bio string
	for i, p := range map[int]*string{2: &display, 7: &status, 8: &bio} {
		if err := cbor.Unmarshal(me[i], p); err != nil {
			t.Fatalf("element %d: %v", i, err)
		}
	}
	if display != "Ada L" || status != "" || bio != "first\nsecond" {
		t.Fatalf("GET /me = %q, %q, %q", display, status, bio)
	}
}

// glyph puts a code point between "a" and "b": the invisible characters are written as numbers, never as literals.
func glyph(r rune) string { return "a" + string(r) + "b" }

func TestPatchMeRefusesWhatTheProfileRulesRefuse(t *testing.T) {
	h, deps := newTestAPI(t)
	user, _, token := seedSession(t, deps)
	for name, body := range map[string][]any{
		"display too long":      {strings.Repeat("a", 65), nil, nil},
		"display bidi":          {glyph(0x202e), nil, nil},
		"display zero width":    {glyph(0x200b), nil, nil},
		"status too long":       {nil, strings.Repeat("a", 191), nil},
		"status newline":        {nil, "a\nb", nil},
		"status line separator": {nil, glyph(0x2028), nil},
		"status blank":          {nil, "   ", nil},
		"bio too long":          {nil, nil, strings.Repeat("a", 501)},
		"bio bidi":              {nil, nil, glyph(0x202e)},
		"bio arabic mark":       {nil, nil, glyph(0x061c)},
		"bio control":           {nil, nil, "a\x1bb"},
		"bio blank":             {nil, nil, "\n \n"},
		"a valid field beside an invalid one writes nothing": {"Valid", "ok", strings.Repeat("a", 501)},
	} {
		if rec := cborCall(t, h, http.MethodPatch, "/v1/accounts/me", token, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400: %s", name, rec.Code, rec.Body.String())
		}
	}
	got, _ := deps.Repo.GetUser(context.Background(), user.ID)
	if got.Display == "Valid" || got.StatusMsg != "" || got.Bio != "" {
		t.Fatalf("a refused patch wrote %+v", got)
	}
	if rec := doAuth(h, http.MethodPatch, "/v1/accounts/me", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no session = %d, want 401", rec.Code)
	}
}

// Status and bio are stored trimmed.
func TestPatchMeStoresTheTrimmedText(t *testing.T) {
	h, deps := newTestAPI(t)
	user, _, token := seedSession(t, deps)
	if rec := cborCall(t, h, http.MethodPatch, "/v1/accounts/me", token, []any{nil, "  hello  ", "\nbio\n"}); rec.Code != http.StatusNoContent {
		t.Fatalf("patch = %d: %s", rec.Code, rec.Body.String())
	}
	got, _ := deps.Repo.GetUser(context.Background(), user.ID)
	if got.StatusMsg != "hello" || got.Bio != "bio" {
		t.Fatalf("stored %q and %q, want them trimmed", got.StatusMsg, got.Bio)
	}
}

// Two devices that each change a different field do not lose each other's edit: a null leaves the
// stored value alone, whatever it is at the moment of the write.
func TestPatchMeWritesOnlyTheFieldsGiven(t *testing.T) {
	h, deps := newTestAPI(t)
	user, _, token := seedSession(t, deps)
	ctx := context.Background()
	if err := deps.Repo.UpdateProfile(ctx, user.ID, nil, profileStr("old status"), profileStr("old bio")); err != nil {
		t.Fatal(err)
	}
	// Another device changes the bio after this one last read the account.
	if err := deps.Repo.UpdateProfile(ctx, user.ID, nil, nil, profileStr("bio from device two")); err != nil {
		t.Fatal(err)
	}
	if rec := cborCall(t, h, http.MethodPatch, "/v1/accounts/me", token, []any{nil, "status from device one", nil}); rec.Code != http.StatusNoContent {
		t.Fatalf("patch = %d", rec.Code)
	}
	got, _ := deps.Repo.GetUser(ctx, user.ID)
	if got.StatusMsg != "status from device one" || got.Bio != "bio from device two" {
		t.Fatalf("stored %q and %q: one device's edit was lost", got.StatusMsg, got.Bio)
	}
}

func profileStr(s string) *string { return &s }

// PATCH /v1/accounts/me is a write like the presence writes beside it: the device's write bucket applies.
func TestPatchMeIsRateLimitedLikeThePresenceWrites(t *testing.T) {
	h, deps := newTestAPIWithConfig(t, func(c *config.Config) {
		c.Limits.Rate.WritePerSecond, c.Limits.Rate.WriteBurst = 0.001, 2
	})
	_, _, token := seedSession(t, deps)
	for i := 0; i < 2; i++ {
		if rec := cborCall(t, h, http.MethodPatch, "/v1/accounts/me", token, []any{nil, "s", nil}); rec.Code != http.StatusNoContent {
			t.Fatalf("patch %d = %d", i+1, rec.Code)
		}
	}
	rec := cborCall(t, h, http.MethodPatch, "/v1/accounts/me", token, []any{nil, "s", nil})
	if rec.Code != http.StatusTooManyRequests || refusalCode(t, rec) != "E_RATE_LIMITED" {
		t.Fatalf("the third patch = %d %q, want 429 E_RATE_LIMITED", rec.Code, refusalCode(t, rec))
	}
}

// A session that still owes its second factor reads the account but changes nothing.
func TestPatchMeIsRefusedForASessionOwingASecondFactor(t *testing.T) {
	h, deps := newTestAPI(t)
	user, _, token := seedSession(t, deps)
	if err := deps.Repo.SetFactorOwed(context.Background(), user.ID, true); err != nil {
		t.Fatal(err)
	}
	rec := cborCall(t, h, http.MethodPatch, "/v1/accounts/me", token, []any{nil, "x", nil})
	if rec.Code != http.StatusForbidden || refusalCode(t, rec) != "E_FACTOR_REQUIRED" {
		t.Fatalf("PATCH as an owed session = %d %q, want 403 E_FACTOR_REQUIRED", rec.Code, refusalCode(t, rec))
	}
	if got, _ := deps.Repo.GetUser(context.Background(), user.ID); got.StatusMsg != "" {
		t.Fatalf("an owed session wrote %q", got.StatusMsg)
	}
}

// The member list carries each member's status message and bio after the account fields.
func TestMemberListCarriesStatusAndBio(t *testing.T) {
	e := communityEnv(t)
	owner, ownerTok := e.NewUser("owner")
	cid := discCreate(t, e, ownerTok, "alpha")
	if err := e.Repo.UpdateProfile(t.Context(), owner, profileStr("Owner"), profileStr("afk"), profileStr("hello there")); err != nil {
		t.Fatal(err)
	}
	member, memberTok := e.NewUser("member")
	joinCommunity(t, e, cid, memberTok)
	var rows [][]cbor.RawMessage
	mustUnmarshal(t, discGet(t, e, "/v1/communities/"+cid.String()+"/members", memberTok), &rows)
	seen := 0
	for _, r := range rows {
		var uid id.ID
		var status, bio string
		mustUnmarshal(t, r[0], &uid)
		mustUnmarshal(t, r[7], &status)
		mustUnmarshal(t, r[8], &bio)
		switch uid {
		case owner:
			seen++
			if status != "afk" || bio != "hello there" {
				t.Fatalf("owner row = %q, %q", status, bio)
			}
		case member:
			seen++
			if status != "" || bio != "" {
				t.Fatalf("member row = %q, %q, want both empty", status, bio)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("saw %d of the two members in %d rows", seen, len(rows))
	}
}
