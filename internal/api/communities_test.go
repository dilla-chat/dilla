package api_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func communityEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	log := slog.New(slog.DiscardHandler)
	api.NewCommunities(e.Repo, e.DS, e.Clk, log).Register(e.Mux)
	// Task 1 ships api.Roles with only the grant route; task 3 adds the other six.
	// Without this registration the mux answers 404, not 403, to the 2FA test below.
	api.NewRoles(e.Repo, e.Clk, "dilla.example", log).Register(e.Mux)
	return e
}

func TestCreateCommunityMakesCreatorOwnerAndMember(t *testing.T) {
	e := communityEnv(t)
	owner, tok := e.NewUser("owner")

	status, body := e.Do(http.MethodPost, "/v1/communities", tok,
		[]any{"Kryptering", []byte(`{"retention_days":0}`), uint64(0), uint64(0)})
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/communities = %d, body %x", status, body)
	}
	var created []cbor.RawMessage
	if err := cborx.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(created) != 3 {
		t.Fatalf("response has %d elements, want 3", len(created))
	}
	var cid, everyone id.ID
	var version uint64
	mustUnmarshal(t, created[0], &cid)
	mustUnmarshal(t, created[1], &everyone)
	mustUnmarshal(t, created[2], &version)
	if version != 1 {
		t.Fatalf("policy_version = %d, want 1", version)
	}

	got, err := e.Repo.GetCommunity(t.Context(), cid)
	if err != nil {
		t.Fatalf("GetCommunity: %v", err)
	}
	if got.Owner != owner {
		t.Fatalf("owner = %x, want %x", got.Owner, owner)
	}
	members, err := e.Repo.ListMembersOfCommunity(t.Context(), cid, id.ID{}, 10)
	if err != nil {
		t.Fatalf("ListMembersOfCommunity: %v", err)
	}
	if len(members) != 1 || members[0].UserID != owner {
		t.Fatalf("members = %+v, want just the owner", members)
	}
	roles, err := e.Repo.ListRoles(t.Context(), cid)
	if err != nil {
		t.Fatalf("ListRoles: %v", err)
	}
	if len(roles) != 1 || roles[0].ID != everyone || roles[0].Position != 0 || roles[0].Name != "@everyone" {
		t.Fatalf("roles = %+v, want one @everyone at position 0", roles)
	}
}

func TestGetCurrentCommunityMemberIsScopedToCallerMembership(t *testing.T) {
	e := communityEnv(t)
	_, ownerTok := e.NewUser("owner")
	community := createCommunity(t, e, ownerTok)
	member, _ := e.NewUser("member")
	if err := e.Repo.PutMember(t.Context(), store.MemberOfCommunityRow{CommunityID: community, UserID: member, Nick: "Ada", Joined: e.Clk.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	path := "/v1/communities/" + community.String() + "/members/" + member.String()
	status, body := e.Do(http.MethodGet, path, ownerTok, nil)
	if status != http.StatusOK {
		t.Fatalf("GET member = %d (%x), want 200", status, body)
	}
	var got struct {
		_        struct{} `cbor:",toarray"`
		UserID   id.ID
		Nick     string
		Username string
		Display  string
	}
	mustUnmarshal(t, body, &got)
	if got.UserID != member || got.Nick != "Ada" || got.Display == "" {
		t.Fatalf("member profile = %+v", got)
	}
	outsider, outsiderTok := e.NewUser("outsider")
	if status, _ := e.Do(http.MethodGet, path, outsiderTok, nil); status != http.StatusNotFound {
		t.Errorf("unrelated caller GET member = %d, want 404", status)
	}
	missingPath := "/v1/communities/" + community.String() + "/members/" + outsider.String()
	if status, _ := e.Do(http.MethodGet, missingPath, ownerTok, nil); status != http.StatusNotFound {
		t.Errorf("absent member GET = %d, want 404", status)
	}
	otherCommunity := createCommunity(t, e, ownerTok)
	crossOnly, _ := e.NewUser("crossonly")
	if err := e.Repo.PutMember(t.Context(), store.MemberOfCommunityRow{CommunityID: otherCommunity, UserID: crossOnly, Nick: "Other community name", Joined: e.Clk.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	crossPath := "/v1/communities/" + community.String() + "/members/" + crossOnly.String()
	if status, _ := e.Do(http.MethodGet, crossPath, ownerTok, nil); status != http.StatusNotFound {
		t.Errorf("member from another community GET = %d, want 404", status)
	}
}

func TestMembershipEventsUseOneEpisodeForJoinLeaveAndNewEpisodeForRejoin(t *testing.T) {
	e := communityEnv(t)
	owner, ownerTok := e.NewUser("owner")
	cid := createCommunity(t, e, ownerTok)
	user, token := e.NewUser("member")
	base := "/v1/communities/" + cid.String()

	if status, body := e.Do(http.MethodPost, base+"/join", token, []any{nil}); status != http.StatusOK {
		t.Fatalf("join = %d (%x)", status, body)
	}
	first, err := e.Repo.GetMember(t.Context(), cid, user)
	if err != nil || first.EpisodeID.IsZero() {
		t.Fatalf("joined member = %+v, err=%v", first, err)
	}
	if status, body := e.Do(http.MethodPost, base+"/join", token, []any{nil}); status != http.StatusOK {
		t.Fatalf("idempotent join = %d (%x)", status, body)
	}
	if status, body := e.Do(http.MethodPost, base+"/leave", token, []any{}); status != http.StatusNoContent {
		t.Fatalf("leave = %d (%x)", status, body)
	}
	if status, body := e.Do(http.MethodPost, base+"/join", token, []any{nil}); status != http.StatusOK {
		t.Fatalf("rejoin = %d (%x)", status, body)
	}
	second, err := e.Repo.GetMember(t.Context(), cid, user)
	if err != nil || second.EpisodeID.IsZero() || second.EpisodeID == first.EpisodeID {
		t.Fatalf("rejoined member = %+v, err=%v; first episode=%s", second, err, first.EpisodeID)
	}
	if status, body := e.Do(http.MethodDelete, base+"/members/"+user.String(), ownerTok, nil); status != http.StatusNoContent {
		t.Fatalf("kick = %d (%x)", status, body)
	}
	if status, body := e.Do(http.MethodPost, base+"/join", token, []any{nil}); status != http.StatusOK {
		t.Fatalf("join after kick = %d (%x)", status, body)
	}
	third, err := e.Repo.GetMember(t.Context(), cid, user)
	if err != nil || third.EpisodeID == second.EpisodeID {
		t.Fatalf("membership after kick = %+v, err=%v", third, err)
	}
	api.NewBans(e.Repo, e.DS, e.Clk, slog.New(slog.DiscardHandler)).Register(e.Mux)
	if status, body := e.Do(http.MethodPut, base+"/bans/"+user.String(), ownerTok, []any{"", nil}); status != http.StatusNoContent {
		t.Fatalf("ban = %d (%x)", status, body)
	}

	page, err := e.Repo.ListBotStructuralEvents(t.Context(), cid, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 7 {
		t.Fatalf("events = %+v, want creator join, then join/leave/rejoin/kick/join/ban", page.Events)
	}
	wantKinds := []protocol.EventKind{protocol.EventMemberJoin, protocol.EventMemberJoin, protocol.EventMemberLeave, protocol.EventMemberJoin, protocol.EventMemberLeave, protocol.EventMemberJoin, protocol.EventMemberLeave}
	for i, event := range page.Events {
		if event.Kind != wantKinds[i] {
			t.Fatalf("event %d kind = %v, want %v", i, event.Kind, wantKinds[i])
		}
	}
	memberEvents := []protocol.Event{page.Events[1], page.Events[2], page.Events[3], page.Events[4], page.Events[5], page.Events[6]}
	for _, event := range memberEvents {
		data := event.Data
		if len(data) != 2 || data[0] != user {
			t.Fatalf("membership event data = %#v, want [user_id, episode_id]", data)
		}
	}
	joinEpisode, leaveEpisode := memberEvents[0].Data[1], memberEvents[1].Data[1]
	rejoinEpisode, kickEpisode := memberEvents[2].Data[1], memberEvents[3].Data[1]
	postKickEpisode, banEpisode := memberEvents[4].Data[1], memberEvents[5].Data[1]
	if joinEpisode != leaveEpisode || rejoinEpisode == joinEpisode || rejoinEpisode != second.EpisodeID || kickEpisode != rejoinEpisode || postKickEpisode == kickEpisode || banEpisode != postKickEpisode || postKickEpisode != third.EpisodeID {
		t.Fatalf("event episode ids = %v %v %v %v %v %v; current before ban=%s", joinEpisode, leaveEpisode, rejoinEpisode, kickEpisode, postKickEpisode, banEpisode, third.EpisodeID)
	}
	if page.Events[0].Data[0] != owner {
		t.Fatalf("creator membership event = %#v", page.Events[0].Data)
	}
}

func TestPolicyRoundTripBumpsVersion(t *testing.T) {
	e := communityEnv(t)
	_, tok := e.NewUser("owner")
	cid := createCommunity(t, e, tok)

	status, body := e.Do(http.MethodPatch, "/v1/communities/"+cid.String(), tok,
		[]any{nil, []byte(`{"retention_days":30}`), nil, nil})
	if status != http.StatusOK {
		t.Fatalf("PATCH = %d (%x)", status, body)
	}
	var patched []uint64
	if err := cborx.Unmarshal(body, &patched); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if patched[0] != 2 {
		t.Fatalf("policy_version = %d, want 2", patched[0])
	}

	got, err := e.Repo.GetCommunity(t.Context(), cid)
	if err != nil {
		t.Fatalf("GetCommunity: %v", err)
	}
	if string(got.PolicyJSON) != `{"retention_days":30}` || got.PolicyVersion != 2 {
		t.Fatalf("stored = %+v", got)
	}

	// A PATCH that does not carry a policy does not bump the version.
	if status, _ := e.Do(http.MethodPatch, "/v1/communities/"+cid.String(), tok,
		[]any{"Renamed", nil, nil, nil}); status != http.StatusOK {
		t.Fatalf("PATCH name = %d", status)
	}
	got, _ = e.Repo.GetCommunity(t.Context(), cid)
	if got.PolicyVersion != 2 || got.Name != "Renamed" {
		t.Fatalf("after rename = %+v", got)
	}
}

func TestJoinBelowMinAccountAgeIsForbidden(t *testing.T) {
	e := communityEnv(t)
	_, ownerTok := e.NewUser("owner")
	cid := createCommunity(t, e, ownerTok)

	// Require a 7-day-old account.
	if status, _ := e.Do(http.MethodPatch, "/v1/communities/"+cid.String(), ownerTok,
		[]any{nil, nil, uint64(7 * 24 * 3600), nil}); status != http.StatusOK {
		t.Fatal("PATCH min_account_age_seconds failed")
	}

	_, newTok := e.NewUser("fresh") // created at e.Clk.Now()
	status, body := e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/join", newTok, []any{nil})
	if status != http.StatusForbidden {
		t.Fatalf("join = %d, want 403", status)
	}
	if code := e.ErrCode(body); code != "E_FORBIDDEN" {
		t.Fatalf("code = %s, want E_FORBIDDEN", code)
	}

	e.Clk.Advance(8 * 24 * time.Hour)
	if status, body := e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/join", newTok, []any{nil}); status != http.StatusOK {
		t.Fatalf("join after ageing = %d (%x)", status, body)
	}
}

func TestRequireMod2FARefusesRoleGrantWithoutASecondFactor(t *testing.T) {
	e := communityEnv(t)
	_, ownerTok := e.NewUser("owner")
	cid := createCommunity(t, e, ownerTok)
	if status, _ := e.Do(http.MethodPatch, "/v1/communities/"+cid.String(), ownerTok,
		[]any{nil, nil, nil, uint64(1)}); status != http.StatusOK {
		t.Fatal("PATCH require_mod_2fa failed")
	}

	target, targetTok := e.NewUser("mod")
	if status, _ := e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/join", targetTok, []any{nil}); status != http.StatusOK {
		t.Fatal("join failed")
	}

	// A moderator role is any role whose allow carries a moderation bit.
	modRole := id.New()
	if err := e.Repo.PutRole(t.Context(), store.RoleRow{
		ID: modRole, CommunityID: cid, Name: "mod", Position: 5,
		// store.RoleRow.Allow is uint64 (§4.1/§4.2: roles.allow is a counter);
		// api.Bits is a distinct named type, so the conversion is explicit.
		Allow:   uint64(api.PermManageMessages | api.PermKickMembers),
		Created: e.Clk.Now().Unix(),
	}); err != nil {
		t.Fatalf("PutRole: %v", err)
	}

	path := "/v1/communities/" + cid.String() + "/members/" + target.String() + "/roles/" + modRole.String()
	status, body := e.Do(http.MethodPut, path, ownerTok, []any{})
	if status != http.StatusForbidden {
		t.Fatalf("grant = %d, want 403 (%x)", status, body)
	}
	if code := e.ErrCode(body); code != "E_TARGET_NO_FACTOR" {
		t.Fatalf("code = %s, want E_TARGET_NO_FACTOR", code)
	}

	// With TOTP confirmed, the same grant succeeds.
	if err := e.Repo.PutTOTP(t.Context(), newConfirmedTOTP(target, e.Clk.Now().Unix())); err != nil {
		t.Fatalf("PutTOTP: %v", err)
	}
	if status, body := e.Do(http.MethodPut, path, ownerTok, []any{}); status != http.StatusNoContent {
		t.Fatalf("grant after TOTP = %d (%x)", status, body)
	}
}

// A passkey is the other second factor, and it is the branch that silently did
// not work while requireSecondFactor passed an empty rp_id: §4.3 indexes
// webauthn_credentials on (rp_id, user_id), so a query with rp_id = ” matches
// nothing however many passkeys the user holds.
func TestRequireMod2FAAcceptsAPasskeyWithNoTOTP(t *testing.T) {
	e := communityEnv(t)
	_, ownerTok := e.NewUser("owner")
	cid := createCommunity(t, e, ownerTok)
	if status, _ := e.Do(http.MethodPatch, "/v1/communities/"+cid.String(), ownerTok,
		[]any{nil, nil, nil, uint64(1)}); status != http.StatusOK {
		t.Fatal("PATCH require_mod_2fa failed")
	}
	target, targetTok := e.NewUser("mod")
	if status, _ := e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/join", targetTok, []any{nil}); status != http.StatusOK {
		t.Fatal("join failed")
	}
	modRole := id.New()
	if err := e.Repo.PutRole(t.Context(), store.RoleRow{
		ID: modRole, CommunityID: cid, Name: "mod", Position: 5,
		Allow: uint64(api.PermManageMessages), Created: e.Clk.Now().Unix(),
	}); err != nil {
		t.Fatalf("PutRole: %v", err)
	}
	// The harness registers api.NewRoles(..., "dilla.example", ...), so the
	// credential must be stored under that rp_id to be found.
	if err := e.Repo.PutWebauthnCredential(t.Context(), newTestCredential(target, "dilla.example", e.Clk.Now().Unix())); err != nil {
		t.Fatalf("PutWebauthnCredential: %v", err)
	}
	path := "/v1/communities/" + cid.String() + "/members/" + target.String() + "/roles/" + modRole.String()
	if status, body := e.Do(http.MethodPut, path, ownerTok, []any{}); status != http.StatusNoContent {
		t.Fatalf("grant with a passkey and no TOTP = %d (%x)", status, body)
	}
	held, err := e.Repo.ListMemberRoles(t.Context(), cid, target)
	if err != nil {
		t.Fatalf("ListMemberRoles: %v", err)
	}
	if len(held) != 1 || held[0] != modRole {
		t.Fatalf("roles held = %v, want [%v]", held, modRole)
	}
}

// communities.policy_json is TEXT and the API is the only thing standing
// between a client and a blob no engine-side JSON function can read: without
// the check, one engine stores the bytes and the other answers 500.
func TestANonJSONPolicyIsRefusedOnBothEngines(t *testing.T) {
	e := communityEnv(t)
	_, tok := e.NewUser("owner")
	status, body := e.Do(http.MethodPost, "/v1/communities", tok,
		[]any{"c", []byte("not json"), uint64(0), uint64(0)})
	if status != http.StatusBadRequest {
		t.Fatalf("POST with a non-JSON policy = %d (%x), want 400", status, body)
	}
	if code := e.ErrCode(body); code != "E_INVALID_REQUEST" {
		t.Fatalf("code = %s, want E_INVALID_REQUEST", code)
	}
}

// The policy document carries the join mode, the stored-not-enforced screening
// flag (R17) and the two halves of retention (R28): archival, default
// indefinite, and delivery, which a community may only shorten. It is stored
// byte for byte, and a document the later tasks could not read is refused
// rather than stored.
func TestPolicyDocumentCarriesJoinScreeningAndRetention(t *testing.T) {
	e := communityEnv(t)
	_, tok := e.NewUser("owner")

	full := []byte(`{"join":"invite","screening":true,"retention_days":365,"delivery_retention_days":7}`)
	status, body := e.Do(http.MethodPost, "/v1/communities", tok,
		[]any{"c", full, uint64(0), uint64(0)})
	if status != http.StatusCreated {
		t.Fatalf("POST with a full policy = %d (%x)", status, body)
	}
	var created []cbor.RawMessage
	mustUnmarshal(t, body, &created)
	var cid id.ID
	mustUnmarshal(t, created[0], &cid)

	status, body = e.Do(http.MethodGet, "/v1/communities/"+cid.String(), tok, nil)
	if status != http.StatusOK {
		t.Fatalf("GET = %d (%x)", status, body)
	}
	var doc []cbor.RawMessage
	mustUnmarshal(t, body, &doc)
	if len(doc) != 8 {
		t.Fatalf("GET has %d elements, want 8", len(doc))
	}
	var policy []byte
	mustUnmarshal(t, doc[3], &policy)
	if !bytes.Equal(policy, full) {
		t.Fatalf("policy read back = %s, want %s", policy, full)
	}

	parsed, err := api.ParseCommunityPolicy(policy)
	if err != nil {
		t.Fatalf("ParseCommunityPolicy: %v", err)
	}
	if !parsed.InviteOnly() || !parsed.Screening || parsed.RetentionDays != 365 || parsed.DeliveryRetentionDays != 7 {
		t.Fatalf("parsed = %+v", parsed)
	}
	// The empty document is every default: open, unscreened, retained indefinitely.
	def, err := api.ParseCommunityPolicy([]byte(`{}`))
	if err != nil {
		t.Fatalf("ParseCommunityPolicy({}): %v", err)
	}
	if def.InviteOnly() || def.Screening || def.RetentionDays != 0 || def.DeliveryRetentionDays != 0 {
		t.Fatalf("defaults = %+v", def)
	}

	for _, bad := range []string{
		`[1]`,                            // not an object
		`null`,                           // not an object
		`{"retension_days":3}`,           // a misspelt key is refused, not ignored
		`{"join":"closed"}`,              // not a join mode
		`{"screening":"yes"}`,            // wrong type
		`{"retention_days":-1}`,          // negative
		`{"retention_days":1.5}`,         // fractional
		`{"retention_days":36501}`,       // over a century
		`{"delivery_retention_days":31}`, // may only shorten the 30-day window
		`{"retention_days":1} {}`,        // trailing document
		"{\"join\":\"open\xff\"}",        // not UTF-8
	} {
		status, body := e.Do(http.MethodPatch, "/v1/communities/"+cid.String(), tok,
			[]any{nil, []byte(bad), nil, nil})
		if status != http.StatusBadRequest || e.ErrCode(body) != "E_INVALID_REQUEST" {
			t.Errorf("PATCH policy %q = %d (%x), want 400 E_INVALID_REQUEST", bad, status, body)
		}
	}
	got, _ := e.Repo.GetCommunity(t.Context(), cid)
	if got.PolicyVersion != 1 || !bytes.Equal(got.PolicyJSON, full) {
		t.Fatalf("a refused PATCH changed the policy: %+v", got)
	}
}

func TestCommunityFieldBounds(t *testing.T) {
	e := communityEnv(t)
	_, tok := e.NewUser("owner")
	for name, req := range map[string][]any{
		"empty name":       {"", []byte(`{}`), uint64(0), uint64(0)},
		"control in name":  {"a\x00b", []byte(`{}`), uint64(0), uint64(0)},
		"long name":        {string(bytes.Repeat([]byte("x"), 256)), []byte(`{}`), uint64(0), uint64(0)},
		"require_mod_2fa":  {"c", []byte(`{}`), uint64(0), uint64(2)},
		"min account age":  {"c", []byte(`{}`), uint64(1) << 63, uint64(0)},
		"five elements":    {"c", []byte(`{}`), uint64(0), uint64(0), uint64(0)},
		"policy too large": {"c", append(append([]byte(`{"x":"`), bytes.Repeat([]byte("a"), 16*1024)...), '"', '}'), uint64(0), uint64(0)},
	} {
		status, body := e.Do(http.MethodPost, "/v1/communities", tok, req)
		if status != http.StatusBadRequest {
			t.Errorf("%s: POST = %d (%x), want 400", name, status, body)
		}
	}
}

// Membership is the gate on every community read: a non-member learns nothing,
// not even that the community exists, and only the owner may change it.
func TestCommunityMembershipGates(t *testing.T) {
	e := communityEnv(t)
	owner, ownerTok := e.NewUser("owner")
	cid := createCommunity(t, e, ownerTok)
	base := "/v1/communities/" + cid.String()

	outsider, outsiderTok := e.NewUser("outsider")
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, base},
		{http.MethodGet, base + "/members"},
		{http.MethodDelete, base},
	} {
		status, body := e.Do(c.method, c.path, outsiderTok, nil)
		if status != http.StatusNotFound || e.ErrCode(body) != "E_NOT_FOUND" {
			t.Errorf("outsider %s %s = %d (%x), want 404", c.method, c.path, status, body)
		}
	}
	if status, _ := e.Do(http.MethodGet, base, "", nil); status != http.StatusUnauthorized {
		t.Errorf("GET without a session = %d, want 401", status)
	}
	if status, _ := e.Do(http.MethodGet, "/v1/communities/"+id.New().String(), ownerTok, nil); status != http.StatusNotFound {
		t.Errorf("GET unknown = %d, want 404", status)
	}
	if status, _ := e.Do(http.MethodGet, "/v1/communities/NOT-HEX", ownerTok, nil); status != http.StatusBadRequest {
		t.Errorf("GET malformed id = %d, want 400", status)
	}

	// Joining is idempotent and keeps the member's nick.
	if status, _ := e.Do(http.MethodPost, base+"/join", outsiderTok, []any{nil}); status != http.StatusOK {
		t.Fatal("join failed")
	}
	if err := e.Repo.PutMember(t.Context(), store.MemberOfCommunityRow{
		CommunityID: cid, UserID: outsider, Joined: e.Clk.Now().Unix(), Nick: "Åsa 🌲",
	}); err != nil {
		t.Fatalf("PutMember: %v", err)
	}
	if status, _ := e.Do(http.MethodPost, base+"/join", outsiderTok, []any{nil}); status != http.StatusOK {
		t.Fatal("second join failed")
	}
	if m, err := e.Repo.GetMember(t.Context(), cid, outsider); err != nil || m.Nick != "Åsa 🌲" {
		t.Fatalf("second join rewrote the member: %+v, %v", m, err)
	}

	// A member reads the community and the member list, but may not change it.
	status, body := e.Do(http.MethodGet, base+"/members", outsiderTok, nil)
	if status != http.StatusOK {
		t.Fatalf("GET members = %d (%x)", status, body)
	}
	var members []struct {
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
	mustUnmarshal(t, body, &members)
	if len(members) != 2 {
		t.Fatalf("members = %+v, want 2", members)
	}
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPatch, base, []any{"mine", nil, nil, nil}},
		{http.MethodDelete, base, nil},
		{http.MethodDelete, base + "/members/" + owner.String(), nil},
	} {
		status, body := e.Do(c.method, c.path, outsiderTok, c.body)
		if status != http.StatusForbidden || e.ErrCode(body) != "E_FORBIDDEN" {
			t.Errorf("member %s %s = %d (%x), want 403", c.method, c.path, status, body)
		}
	}

	// The owner cannot leave and cannot be removed; a member can do both.
	if status, _ := e.Do(http.MethodPost, base+"/leave", ownerTok, []any{}); status != http.StatusForbidden {
		t.Errorf("owner leave = %d, want 403", status)
	}
	if status, _ := e.Do(http.MethodDelete, base+"/members/"+owner.String(), ownerTok, nil); status != http.StatusForbidden {
		t.Errorf("owner removing themself = %d, want 403", status)
	}
	if status, body := e.Do(http.MethodPost, base+"/leave", outsiderTok, []any{}); status != http.StatusNoContent {
		t.Fatalf("leave = %d (%x)", status, body)
	}
	if status, _ := e.Do(http.MethodGet, base, outsiderTok, nil); status != http.StatusNotFound {
		t.Errorf("GET after leaving = %d, want 404", status)
	}
	if status, _ := e.Do(http.MethodPost, base+"/join", outsiderTok, []any{nil}); status != http.StatusOK {
		t.Fatal("re-join failed")
	}
	if status, body := e.Do(http.MethodDelete, base+"/members/"+outsider.String(), ownerTok, nil); status != http.StatusNoContent {
		t.Fatalf("owner removing a member = %d (%x)", status, body)
	}
	if status, _ := e.Do(http.MethodDelete, base+"/members/"+outsider.String(), ownerTok, nil); status != http.StatusNotFound {
		t.Errorf("removing a non-member = %d, want 404", status)
	}

	// A disabled account cannot join.
	disabled, disabledTok := e.NewUser("disabled")
	at := e.Clk.Now().Unix()
	if err := e.Repo.SetUserDisabled(t.Context(), disabled, &at); err != nil {
		t.Fatalf("SetUserDisabled: %v", err)
	}
	if status, _ := e.Do(http.MethodPost, base+"/join", disabledTok, []any{nil}); status != http.StatusForbidden {
		t.Errorf("disabled join = %d, want 403", status)
	}

	// A session that is not enrolled is refused even where the middleware let
	// it through: every community route is "E" in protocol/09.
	_, pendingTok := e.NewUser("pending")
	s := e.sess[pendingTok]
	s.Scope = auth.ScopePending
	e.sess[pendingTok] = s
	if status, _ := e.Do(http.MethodPost, "/v1/communities", pendingTok,
		[]any{"c", []byte(`{}`), uint64(0), uint64(0)}); status != http.StatusForbidden {
		t.Errorf("pending session creating a community = %d, want 403", status)
	}

	// Deleting tombstones the community for everyone, the owner included.
	if status, body := e.Do(http.MethodDelete, base, ownerTok, nil); status != http.StatusNoContent {
		t.Fatalf("DELETE = %d (%x)", status, body)
	}
	if status, _ := e.Do(http.MethodGet, base, ownerTok, nil); status != http.StatusNotFound {
		t.Errorf("GET after delete = %d, want 404", status)
	}
	if status, _ := e.Do(http.MethodPost, base+"/join", outsiderTok, []any{nil}); status != http.StatusNotFound {
		t.Errorf("join after delete = %d, want 404", status)
	}
}

func TestRoleGrantGates(t *testing.T) {
	e := communityEnv(t)
	_, ownerTok := e.NewUser("owner")
	cid := createCommunity(t, e, ownerTok)
	member, memberTok := e.NewUser("member")
	if status, _ := e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/join", memberTok, []any{nil}); status != http.StatusOK {
		t.Fatal("join failed")
	}
	role := id.New()
	if err := e.Repo.PutRole(t.Context(), store.RoleRow{
		ID: role, CommunityID: cid, Name: "helper", Position: 1,
		Allow: uint64(api.PermPinMessages), Created: e.Clk.Now().Unix(),
	}); err != nil {
		t.Fatalf("PutRole: %v", err)
	}
	grant := func(tok string, user, r id.ID) int {
		status, _ := e.Do(http.MethodPut, "/v1/communities/"+cid.String()+"/members/"+user.String()+"/roles/"+r.String(), tok, []any{})
		return status
	}
	if got := grant(memberTok, member, role); got != http.StatusForbidden {
		t.Errorf("a non-owner granting = %d, want 403", got)
	}
	if got := grant(ownerTok, member, id.New()); got != http.StatusNotFound {
		t.Errorf("granting an unknown role = %d, want 404", got)
	}
	if got := grant(ownerTok, id.New(), role); got != http.StatusNotFound {
		t.Errorf("granting to a non-member = %d, want 404", got)
	}
	// A role of another community is not grantable here.
	otherCid := createCommunity(t, e, ownerTok)
	other := id.New()
	if err := e.Repo.PutRole(t.Context(), store.RoleRow{ID: other, CommunityID: otherCid, Name: "x", Created: 1}); err != nil {
		t.Fatalf("PutRole: %v", err)
	}
	if got := grant(ownerTok, member, other); got != http.StatusNotFound {
		t.Errorf("granting another community's role = %d, want 404", got)
	}
	// require_mod_2fa is 0 and the role carries no moderation bit: no second
	// factor is needed.
	if got := grant(ownerTok, member, role); got != http.StatusNoContent {
		t.Fatalf("grant = %d, want 204", got)
	}
	status, body := e.Do(http.MethodGet, "/v1/communities/"+cid.String()+"/members", ownerTok, nil)
	if status != http.StatusOK {
		t.Fatalf("GET members = %d", status)
	}
	var members []struct {
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
	mustUnmarshal(t, body, &members)
	for _, m := range members {
		if m.UserID == member && (len(m.Roles) != 1 || m.Roles[0] != role) {
			t.Fatalf("member roles = %v, want [%v]", m.Roles, role)
		}
	}
}

// createCommunity posts ["c", {}, 0, 0] and returns the decoded community_id.
func createCommunity(t *testing.T, e *env, tok string) id.ID {
	t.Helper()
	status, body := e.Do(http.MethodPost, "/v1/communities", tok,
		[]any{"c", []byte("{}"), uint64(0), uint64(0)})
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/communities = %d (%x)", status, body)
	}
	var created []cbor.RawMessage
	mustUnmarshal(t, body, &created)
	var cid id.ID
	mustUnmarshal(t, created[0], &cid)
	return cid
}

// mustUnmarshal is cborx.Unmarshal with t.Fatalf.
func mustUnmarshal(t *testing.T, data []byte, v any) {
	t.Helper()
	if err := cborx.Unmarshal(data, v); err != nil {
		t.Fatalf("cborx.Unmarshal(%x): %v", data, err)
	}
}

// newConfirmedTOTP is a store.TOTPRow with ConfirmedAt set.
func newConfirmedTOTP(user id.ID, now int64) store.TOTPRow {
	return store.TOTPRow{
		UserID: user, Secret: make([]byte, 20), Digits: 6, Period: 30,
		Algorithm: "SHA1", ConfirmedAt: &now, Created: now,
	}
}

// newTestCredential is a store.WebauthnCredentialRow under rpID with the
// minimum non-null columns of §4.3.
func newTestCredential(user id.ID, rpID string, now int64) store.WebauthnCredentialRow {
	cred := id.New()
	return store.WebauthnCredentialRow{
		CredID: cred[:], RPID: rpID, UserID: user, PublicKey: []byte{1},
		Flags: []byte{0}, ExtensionsJSON: "{}", Created: now,
	}
}

func TestOwnPermissionsRead(t *testing.T) {
	e := communityEnv(t)
	_, ownerTok := e.NewUser("owner")
	cid := createCommunity(t, e, ownerTok)
	path := "/v1/communities/" + cid.String() + "/permissions"
	read := func(tok string) (uint64, uint64) {
		t.Helper()
		status, body := e.Do(http.MethodGet, path, tok, nil)
		if status != http.StatusOK {
			t.Fatalf("GET permissions = %d (%x)", status, body)
		}
		var got struct {
			_    struct{} `cbor:",toarray"`
			Bits uint64
			Top  uint64
		}
		mustUnmarshal(t, body, &got)
		return got.Bits, got.Top
	}
	join := func(tok string) {
		t.Helper()
		if status, _ := e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/join", tok, []any{nil}); status != http.StatusOK {
			t.Fatal("join failed")
		}
	}
	grant := func(user id.ID, position, allow uint64) {
		t.Helper()
		role := id.New()
		if err := e.Repo.PutRole(t.Context(), store.RoleRow{ID: role, CommunityID: cid, Name: "r", Position: position,
			Allow: allow, Created: e.Clk.Now().Unix()}); err != nil {
			t.Fatalf("PutRole: %v", err)
		}
		if err := e.Repo.PutMemberRole(t.Context(), cid, user, role); err != nil {
			t.Fatalf("PutMemberRole: %v", err)
		}
	}

	if bits, top := read(ownerTok); bits != uint64(api.PermAll) || top != 1<<31 {
		t.Errorf("owner = [%d, %d], want [PermAll, 2147483648]", bits, top)
	}

	_, freshTok := e.NewUser("fresh")
	join(freshTok)
	if bits, top := read(freshTok); bits != uint64(api.DefaultEveryoneAllow) || top != 0 {
		t.Errorf("fresh member = [%d, %d], want [everyone, 0]", bits, top)
	}

	modID, modTok := e.NewUser("mod")
	join(modTok)
	grant(modID, 5, uint64(api.PermKickMembers))
	if bits, top := read(modTok); bits != uint64(api.DefaultEveryoneAllow|api.PermKickMembers) || top != 5 {
		t.Errorf("moderator = [%d, %d], want [everyone|kick, 5]", bits, top)
	}

	adminID, adminTok := e.NewUser("admin")
	join(adminTok)
	grant(adminID, 3, uint64(api.PermAdministrator))
	if bits, top := read(adminTok); bits != uint64(api.PermAll) || top != 3 {
		t.Errorf("administrator = [%d, %d], want [PermAll, 3]", bits, top)
	}

	// @everyone with view denied resolves to nothing for a plain member.
	roles, err := e.Repo.ListRoles(t.Context(), cid)
	if err != nil {
		t.Fatalf("ListRoles: %v", err)
	}
	everyone := roles[0]
	everyone.Allow, everyone.Deny = 0, uint64(api.PermViewChannel)
	if err := e.Repo.PutRole(t.Context(), everyone); err != nil {
		t.Fatalf("PutRole everyone: %v", err)
	}
	if bits, top := read(freshTok); bits != 0 || top != 0 {
		t.Errorf("view denied = [%d, %d], want [0, 0]", bits, top)
	}

	_, strangerTok := e.NewUser("stranger")
	if status, _ := e.Do(http.MethodGet, path, strangerTok, nil); status != http.StatusNotFound {
		t.Errorf("non-member = %d, want 404", status)
	}
	if status, _ := e.Do(http.MethodGet, "/v1/communities/"+id.New().String()+"/permissions", ownerTok, nil); status != http.StatusNotFound {
		t.Errorf("unknown community = %d, want 404", status)
	}
}

// handOverFirst is a Repository whose next Tx first hands cid to `to`, committed on its own: a
// hand-over that lands between a handler's pre-transaction checks and its transaction.
type handOverFirst struct {
	store.Repository
	mu      sync.Mutex
	cid, to id.ID
	armed   bool
}

func (r *handOverFirst) arm(cid, to id.ID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cid, r.to, r.armed = cid, to, true
}

func (r *handOverFirst) Tx(ctx context.Context, fn func(store.Repository) error) error {
	r.mu.Lock()
	armed := r.armed
	r.armed = false
	r.mu.Unlock()
	if armed {
		if err := r.SetCommunityOwner(ctx, r.cid, r.to); err != nil {
			return err
		}
	}
	return r.Repository.Tx(ctx, fn)
}

// Ownership is re-checked under the community lock: a hand-over that commits after a handler's
// first checks cannot leave the community with an owner who left, was banned or was kicked, nor
// let the previous owner delete it or kick with the authority they no longer hold.
func TestAHandOverRacingARemovalLeavesTheOwnerInPlace(t *testing.T) {
	setup := func(t *testing.T) (*env, *handOverFirst, id.ID, id.ID, string) {
		t.Helper()
		e := newEnv(t)
		log := slog.New(slog.DiscardHandler)
		hook := &handOverFirst{Repository: e.Repo}
		api.NewCommunities(hook, e.DS, e.Clk, log).Register(e.Mux)
		api.NewBans(hook, e.DS, e.Clk, log).Register(e.Mux)
		owner, ownerTok := e.NewUser("owner")
		return e, hook, createCommunity(t, e, ownerTok), owner, ownerTok
	}
	member := func(t *testing.T, e *env, cid, u id.ID) {
		t.Helper()
		if _, err := e.Repo.GetMember(t.Context(), cid, u); err != nil {
			t.Fatalf("user %s is no longer a member: %v", u, err)
		}
	}

	t.Run("a leaver who became the owner", func(t *testing.T) {
		e, hook, cid, _, _ := setup(t)
		leaver, leaverTok := joinAs(t, e, cid, "leaver")
		hook.arm(cid, leaver)
		status, body := e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/leave", leaverTok, []any{})
		if status != http.StatusForbidden || e.ErrCode(body) != "E_FORBIDDEN" {
			t.Fatalf("leave = %d (%x), want 403", status, body)
		}
		member(t, e, cid, leaver)
	})
	t.Run("a ban whose target became the owner", func(t *testing.T) {
		e, hook, cid, _, ownerTok := setup(t)
		target, _ := joinAs(t, e, cid, "target")
		hook.arm(cid, target)
		status, body := e.Do(http.MethodPut, "/v1/communities/"+cid.String()+"/bans/"+target.String(), ownerTok, []any{"", nil})
		if status != http.StatusForbidden || e.ErrCode(body) != "E_FORBIDDEN" {
			t.Fatalf("ban = %d (%x), want 403", status, body)
		}
		member(t, e, cid, target)
		if _, err := e.Repo.GetBan(t.Context(), cid, target); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetBan = %v, want no ban", err)
		}
	})
	t.Run("a kick by the previous owner", func(t *testing.T) {
		e, hook, cid, _, ownerTok := setup(t)
		heir, _ := joinAs(t, e, cid, "heir")
		target, _ := joinAs(t, e, cid, "target")
		hook.arm(cid, heir)
		status, body := e.Do(http.MethodDelete, "/v1/communities/"+cid.String()+"/members/"+target.String(), ownerTok, nil)
		if status != http.StatusForbidden || e.ErrCode(body) != "E_FORBIDDEN" {
			t.Fatalf("kick = %d (%x), want 403", status, body)
		}
		member(t, e, cid, target)
	})
	t.Run("a delete by the previous owner", func(t *testing.T) {
		e, hook, cid, _, ownerTok := setup(t)
		heir, _ := joinAs(t, e, cid, "heir")
		hook.arm(cid, heir)
		status, body := e.Do(http.MethodDelete, "/v1/communities/"+cid.String(), ownerTok, nil)
		if status != http.StatusForbidden || e.ErrCode(body) != "E_FORBIDDEN" {
			t.Fatalf("delete = %d (%x), want 403", status, body)
		}
		if _, err := e.Repo.GetCommunity(t.Context(), cid); err != nil {
			t.Fatalf("the community is gone: %v", err)
		}
	})
}

// The owner hands the community to another member: one audit row, op 54 to the members, and the
// previous owner is an ordinary member who keeps their roles and may now leave.
func TestTheOwnerHandsTheCommunityToAnotherMember(t *testing.T) {
	e, gw, cid, owner, ownerTok := notifyEnv(t)
	heir, heirTok := joinAs(t, e, cid, "heir")
	plain, plainTok := joinAs(t, e, cid, "plain")
	stranger, _ := e.NewUser("stranger")
	bot := id.New()
	botRow := newAPITestUser(bot, "relay", e.Clk.Now().Unix())
	botRow.Kind = 1
	if err := e.Repo.CreateUser(t.Context(), botRow); err != nil {
		t.Fatalf("CreateUser bot: %v", err)
	}
	if err := e.Repo.PutMember(t.Context(), store.MemberOfCommunityRow{
		CommunityID: cid, UserID: bot, Joined: e.Clk.Now().Unix(),
	}); err != nil {
		t.Fatalf("PutMember bot: %v", err)
	}
	role := createRole(t, e, cid, ownerTok, "elder", 3, api.PermPinMessages)
	grant(t, e, cid, ownerTok, owner, role, http.StatusNoContent)
	base := "/v1/communities/" + cid.String()
	hand := func(tok string, to id.ID) (int, []byte) {
		t.Helper()
		return e.Do(http.MethodPost, base+"/owner", tok, []any{to})
	}

	status, body := hand(plainTok, heir)
	if status != http.StatusForbidden || e.ErrCode(body) != "E_FORBIDDEN" ||
		!bytes.Contains(body, []byte("only the owner can hand over a community")) {
		t.Fatalf("a non-owner handing over = %d (%x), want 403", status, body)
	}
	if status, body := hand(ownerTok, stranger); status != http.StatusNotFound || e.ErrCode(body) != "E_NOT_FOUND" {
		t.Fatalf("a non-member target = %d (%x), want 404", status, body)
	}
	if status, body := hand(ownerTok, owner); status != http.StatusBadRequest || e.ErrCode(body) != "E_INVALID_REQUEST" {
		t.Fatalf("the owner as target = %d (%x), want 400", status, body)
	}
	if status, body := hand(ownerTok, bot); status != http.StatusBadRequest || e.ErrCode(body) != "E_INVALID_REQUEST" {
		t.Fatalf("a bot target = %d (%x), want 400", status, body)
	}
	// A disabled or a deleted account cannot own a community, as neither may join one.
	disabled, _ := joinAs(t, e, cid, "disabled")
	deleted, _ := joinAs(t, e, cid, "deleted")
	at := e.Clk.Now().Unix()
	if err := e.Repo.SetUserDisabled(t.Context(), disabled, &at); err != nil {
		t.Fatalf("SetUserDisabled: %v", err)
	}
	if err := e.Repo.TombstoneUser(t.Context(), deleted, at); err != nil {
		t.Fatalf("TombstoneUser: %v", err)
	}
	for name, to := range map[string]id.ID{"disabled": disabled, "deleted": deleted} {
		status, body := hand(ownerTok, to)
		if status != http.StatusBadRequest || e.ErrCode(body) != "E_INVALID_REQUEST" ||
			!bytes.Contains(body, []byte("that account can no longer own a community")) {
			t.Fatalf("a %s target = %d (%x), want 400", name, status, body)
		}
	}
	if got, _ := e.Repo.GetCommunity(t.Context(), cid); got.Owner != owner {
		t.Fatalf("a refused hand-over changed the owner to %s", got.Owner)
	}

	gw.reset()
	if status, body := hand(ownerTok, heir); status != http.StatusNoContent {
		t.Fatalf("hand over = %d (%x), want 204", status, body)
	}
	told(t, gw, "hand over", map[id.ID]int{owner: 1, heir: 1, plain: 1, stranger: 0})
	if got, _ := e.Repo.GetCommunity(t.Context(), cid); got.Owner != heir {
		t.Fatalf("owner = %s, want %s", got.Owner, heir)
	}
	audit, err := e.Repo.ListAudit(t.Context(), 0, 100)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	found := 0
	for _, a := range audit {
		if a.Action == "community.transfer" {
			found++
			if a.Actor == nil || *a.Actor != owner || a.Target != heir.String() || a.Detail != owner.String() {
				t.Fatalf("audit row = %+v, want actor and detail %s, target %s", a, owner, heir)
			}
		}
	}
	if found != 1 {
		t.Fatalf("community.transfer audit rows = %d, want 1", found)
	}
	// The previous owner is still a member and keeps their role.
	if held, err := e.Repo.ListMemberRoles(t.Context(), cid, owner); err != nil || len(held) != 1 || held[0] != role {
		t.Fatalf("previous owner's roles = %v, %v, want [%s]", held, err, role)
	}
	// The previous owner may no longer hand it over or delete it; the new owner may.
	if status, _ := hand(ownerTok, plain); status != http.StatusForbidden {
		t.Fatalf("the previous owner handing over again = %d, want 403", status)
	}
	if status, _ := e.Do(http.MethodDelete, base, ownerTok, nil); status != http.StatusForbidden {
		t.Fatalf("the previous owner's DELETE = %d, want 403", status)
	}
	if status, body := e.Do(http.MethodPost, base+"/leave", ownerTok, []any{}); status != http.StatusNoContent {
		t.Fatalf("the previous owner's leave = %d (%x), want 204", status, body)
	}
	if status, body := e.Do(http.MethodDelete, base, heirTok, nil); status != http.StatusNoContent {
		t.Fatalf("the new owner's DELETE = %d (%x), want 204", status, body)
	}
}
