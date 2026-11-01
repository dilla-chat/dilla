package api_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/jonasthim/dilla/internal/api"
	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

// Task 10c: max_uses 0 is "∞" and ttl_seconds 0 is "never". Either needs manage_community on
// top of create_invite; the finite options stay open to every create_invite holder with their old
// bounds.
func TestOpenEndedInvitesNeedManageCommunity(t *testing.T) {
	e, cid, ownerTok := inviteEnv(t)
	path := "/v1/communities/" + cid.String() + "/invites"
	_, memberTok := e.NewUser("member")
	if status, _ := join(e, cid, memberTok, nil); status != http.StatusOK {
		t.Fatal("join failed")
	}
	// @everyone holds create_invite by default, so the member may make finite links...
	if status, body := e.Do(http.MethodPost, path, memberTok, []any{uint64(10), uint64(7 * 24 * 3600), uint64(0)}); status != http.StatusCreated {
		t.Fatalf("member finite invite = %d (%x), want 201", status, body)
	}
	// ...but not open-ended ones.
	for name, body := range map[string][]any{
		"never":           {uint64(10), uint64(0), uint64(0)},
		"unlimited":       {uint64(0), uint64(3600), uint64(0)},
		"never unlimited": {uint64(0), uint64(0), uint64(0)},
	} {
		if status, _ := e.Do(http.MethodPost, path, memberTok, body); status != http.StatusForbidden {
			t.Fatalf("member %s invite = %d, want 403", name, status)
		}
	}
	// A channel overwrite cannot hand out manage_community (a community bit), and holding
	// create_invite and manage_messages is not enough either.
	roles, err := e.Repo.ListRoles(t.Context(), cid)
	if err != nil {
		t.Fatal(err)
	}
	everyone := roles[0]
	mods := store.RoleRow{ID: id.New(), CommunityID: cid, Name: "mods", Position: 1,
		Allow: uint64(api.PermManageMessages | api.PermKickMembers | api.PermManageRoles), Created: 1}
	managers := store.RoleRow{ID: id.New(), CommunityID: cid, Name: "managers", Position: 2,
		Allow: uint64(api.PermManageCommunity), Created: 1}
	admins := store.RoleRow{ID: id.New(), CommunityID: cid, Name: "admins", Position: 3,
		Allow: uint64(api.PermAdministrator), Created: 1}
	for _, r := range []store.RoleRow{mods, managers, admins} {
		if err := e.Repo.PutRole(t.Context(), r); err != nil {
			t.Fatal(err)
		}
	}
	grant := func(tok string, role id.ID) {
		t.Helper()
		if err := e.Repo.PutMemberRole(t.Context(), cid, e.sess[tok].UserID, role); err != nil {
			t.Fatal(err)
		}
	}
	revoke := func(tok string, role id.ID) {
		t.Helper()
		if err := e.Repo.DeleteMemberRole(t.Context(), cid, e.sess[tok].UserID, role); err != nil {
			t.Fatal(err)
		}
	}
	grant(memberTok, mods.ID)
	if status, _ := e.Do(http.MethodPost, path, memberTok, []any{uint64(0), uint64(0), uint64(0)}); status != http.StatusForbidden {
		t.Fatalf("moderator without manage_community = %d, want 403", status)
	}
	revoke(memberTok, mods.ID)

	// manage_community and administrator both allow it; the owner always may.
	for _, tc := range []struct {
		name string
		role id.ID
	}{{"manage_community", managers.ID}, {"administrator", admins.ID}} {
		grant(memberTok, tc.role)
		if status, body := e.Do(http.MethodPost, path, memberTok, []any{uint64(0), uint64(0), uint64(0)}); status != http.StatusCreated {
			t.Fatalf("%s open-ended invite = %d (%x), want 201", tc.name, status, body)
		}
		revoke(memberTok, tc.role)
	}
	if status, body := e.Do(http.MethodPost, path, ownerTok, []any{uint64(0), uint64(0), uint64(0)}); status != http.StatusCreated {
		t.Fatalf("owner open-ended invite = %d (%x), want 201", status, body)
	}

	// manage_community does not replace create_invite: without it nothing is created at all.
	everyone.Allow &^= uint64(api.PermCreateInvite)
	if err := e.Repo.PutRole(t.Context(), everyone); err != nil {
		t.Fatal(err)
	}
	grant(memberTok, managers.ID)
	if status, _ := e.Do(http.MethodPost, path, memberTok, []any{uint64(0), uint64(0), uint64(0)}); status != http.StatusForbidden {
		t.Fatalf("manage_community without create_invite = %d, want 403", status)
	}
}

// An instance-admin grant is never open-ended, even from an instance admin.
func TestAGrantsAdminInviteIsAlwaysFinite(t *testing.T) {
	e, cid, _ := inviteEnv(t)
	path := "/v1/communities/" + cid.String() + "/invites"
	_, adminTok := newInstanceAdmin(t, e)
	if status, _ := join(e, cid, adminTok, nil); status != http.StatusOK {
		t.Fatal("admin join failed")
	}
	// The instance admin is a plain member here; give them manage_community so the only refusal
	// left is the grants_admin rule.
	role := store.RoleRow{ID: id.New(), CommunityID: cid, Name: "managers", Position: 1, Allow: uint64(api.PermManageCommunity), Created: 1}
	if err := e.Repo.PutRole(t.Context(), role); err != nil {
		t.Fatal(err)
	}
	if err := e.Repo.PutMemberRole(t.Context(), cid, e.sess[adminTok].UserID, role.ID); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]any{
		"never":     {uint64(1), uint64(0), uint64(1)},
		"unlimited": {uint64(0), uint64(60), uint64(1)},
	} {
		if status, _ := e.Do(http.MethodPost, path, adminTok, body); status != http.StatusBadRequest {
			t.Fatalf("grants_admin %s = %d, want 400", name, status)
		}
	}
}

// The create answer, the list and the landing CBOR say 0 for "never" and "∞"; the row keeps the
// fail-closed placeholders (1000 uses, the largest second).
func TestOpenEndedInvitesOnTheWire(t *testing.T) {
	e, cid, ownerTok := inviteEnv(t)
	status, body := e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/invites", ownerTok, []any{uint64(0), uint64(0), uint64(0)})
	if status != http.StatusCreated {
		t.Fatalf("POST = %d (%x)", status, body)
	}
	var created []cbor.RawMessage
	mustUnmarshalBody(t, body, &created)
	var code string
	var expires uint64
	mustUnmarshal(t, created[1], &code)
	mustUnmarshal(t, created[3], &expires)
	if expires != 0 {
		t.Fatalf("create expires = %d, want 0 (never)", expires)
	}
	row, err := e.Repo.GetInviteByHash(t.Context(), auth.HashInviteCode(code))
	if err != nil {
		t.Fatal(err)
	}
	if row.UnlimitedUses != 1 || row.MaxUses != 1000 || !row.NeverExpires() {
		t.Fatalf("stored row = %+v, want unlimited with 1000 placeholder and never expiring", row)
	}
	status, body = e.Do(http.MethodGet, "/v1/communities/"+cid.String()+"/invites", ownerTok, nil)
	if status != http.StatusOK {
		t.Fatalf("GET = %d", status)
	}
	var rows [][]cbor.RawMessage
	mustUnmarshal(t, body, &rows)
	var maxUses, expiresAt uint64
	mustUnmarshal(t, rows[0][4], &maxUses)
	mustUnmarshal(t, rows[0][7], &expiresAt)
	if maxUses != 0 || expiresAt != 0 {
		t.Fatalf("list max_uses=%d expires_at=%d, want 0 and 0", maxUses, expiresAt)
	}
	resp, raw := rawGet(t, e, "/i/"+code, "application/cbor")
	var landing []cbor.RawMessage
	if resp.StatusCode != http.StatusOK || cbor.Unmarshal(raw, &landing) != nil || len(landing) != 4 {
		t.Fatalf("landing CBOR = %d %x", resp.StatusCode, raw)
	}
	mustUnmarshal(t, landing[2], &expiresAt)
	if expiresAt != 0 {
		t.Fatalf("landing expires = %d, want 0", expiresAt)
	}
	_, page := landingBody(t, e, code)
	if !strings.Contains(page, "Never expires · No use limit") || strings.Contains(page, "<time") {
		t.Fatalf("landing meta for an open-ended invite is wrong:\n%s", page)
	}
}

// An unlimited invite keeps redeeming past the old 1000-use cap; a finite one stops at its count;
// "never" survives any clock.
func TestAnUnlimitedInviteRedeemsPastTheOldCap(t *testing.T) {
	e, cid, _ := inviteEnv(t)
	mk := func(unlimited bool, used uint64, expires int64) string {
		t.Helper()
		code, hash := auth.NewInviteCode()
		row := store.InviteRow{ID: id.New(), CodeHash: hash, CommunityID: &cid, MaxUses: 1000, UsedCount: used,
			Created: e.Clk.Now().Unix(), ExpiresAt: expires}
		if unlimited {
			row.UnlimitedUses = 1
		}
		if err := e.Repo.CreateInvite(t.Context(), row); err != nil {
			t.Fatal(err)
		}
		return code
	}
	far := e.Clk.Now().Add(time.Hour).Unix()
	unlimited := mk(true, 1000, store.InviteNeverExpires)
	finite := mk(false, 1000, far)
	if _, err := e.Repo.RedeemInvite(t.Context(), auth.HashInviteCode(unlimited), e.Clk.Now().Add(100*365*24*time.Hour).Unix()); err != nil {
		t.Fatalf("unlimited, never-expiring invite after 1000 uses and a century = %v, want redeemed", err)
	}
	if got := usedCount(t, e, unlimited); got != 1001 {
		t.Fatalf("used_count = %d, want 1001", got)
	}
	if _, err := e.Repo.RedeemInvite(t.Context(), auth.HashInviteCode(finite), e.Clk.Now().Unix()); err == nil {
		t.Fatal("a finite invite at its cap redeemed")
	}
	_, memberTok := e.NewUser("late")
	if status, body := join(e, cid, memberTok, unlimited); status != http.StatusOK {
		t.Fatalf("join with an unlimited invite past 1000 uses = %d (%x)", status, body)
	}
}
