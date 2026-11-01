package api_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/api"
	"github.com/jonasthim/dilla/internal/auth"
	botprotocol "github.com/jonasthim/dilla/internal/bot/protocol"
	botruntime "github.com/jonasthim/dilla/internal/bot/runtime"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

func TestBotInstallationEnableRequiresStepUpAndReplaysOnlyTheSameRequest(t *testing.T) {
	for engine, repo := range policyEngines(t) {
		t.Run(engine, func(t *testing.T) { testBotInstallationEnableRequiresStepUpAndReplaysOnlyTheSameRequest(t, repo) })
	}
}

func TestBotOnlyCommunityRouteChecksAuthorityBeforeWireRequirement(t *testing.T) {
	h, deps := newTestAPI(t)
	owner, _, ownerToken := seedAPISession(t, deps)
	member, _, memberToken := seedAPISession(t, deps)
	cid := id.New()
	now := deps.Clock.Now().Unix()
	if err := deps.Repo.CreateCommunity(context.Background(), store.CommunityRow{ID: cid, Owner: owner.ID, Name: "test", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: now}); err != nil {
		t.Fatal(err)
	}
	for _, user := range []id.ID{owner.ID, member.ID} {
		if err := deps.Repo.PutMember(context.Background(), store.MemberOfCommunityRow{CommunityID: cid, UserID: user, Joined: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := deps.Repo.PutRole(context.Background(), store.RoleRow{ID: id.New(), CommunityID: cid, Name: "@everyone", Position: 0, Created: now}); err != nil {
		t.Fatal(err)
	}
	path := "/v1/communities/" + cid.String() + "/bot"
	if rec := getAuth(h, path, memberToken); rec.Code != http.StatusForbidden {
		t.Fatalf("unauthorized bot route = %d, want 403 before E_VERSION", rec.Code)
	}
	if rec := getAuth(h, path, ownerToken); rec.Code != http.StatusBadRequest || refusalCode(t, rec) != "E_VERSION" {
		t.Fatalf("authorized wire-0 bot route = %d, want 400 E_VERSION", rec.Code)
	}
	unknown := "/v1/communities/" + id.New().String() + "/bot"
	if rec := getAuth(h, unknown, ownerToken); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown community bot route = %d, want 404 before E_VERSION", rec.Code)
	}
}

func TestBotInstallationAbsentIsTypedNullAfterAuthorization(t *testing.T) {
	h, deps := newTestAPI(t)
	owner, _, token := seedAPISession(t, deps)
	cid := seedBotCommunity(t, deps, owner.ID)
	negotiate, err := cborx.Marshal([]any{[]any{uint64(3)}, []any{uint64(1)}, []any{uint64(1)}})
	if err != nil {
		t.Fatal(err)
	}
	if rec := postCBORAuth(h, "/v1/sessions/negotiate", negotiate, token); rec.Code != http.StatusOK {
		t.Fatalf("negotiate wire 3 = %d", rec.Code)
	}
	rec := getAuth(h, "/v1/communities/"+cid.String()+"/bot", token)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), []byte{0xf6}) {
		t.Fatalf("absent bot response = %d %x, want CBOR null", rec.Code, rec.Body.Bytes())
	}
	if rec := getAuth(h, "/v1/communities/"+id.New().String()+"/bot", token); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown community response = %d, want 404", rec.Code)
	}
}

func TestConfigureBotChecksCapabilityAuthorityBeforeWireUpgrade(t *testing.T) {
	h, deps := newTestAPI(t)
	owner, _, ownerToken := seedAPISession(t, deps)
	manager, _, managerToken := seedAPISession(t, deps)
	cid := seedBotCommunity(t, deps, owner.ID)
	ctx := context.Background()
	if err := deps.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: manager.ID, Joined: deps.Clock.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	managerRole := store.RoleRow{ID: id.New(), CommunityID: cid, Name: "bot manager", Position: 10,
		Allow: uint64(api.PermViewChannel | api.PermReadHistory | api.PermSendMessages | api.PermAddReactions | api.PermManageCommunity), Created: deps.Clock.Now().Unix()}
	if err := deps.Repo.PutRole(ctx, managerRole); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.PutMemberRole(ctx, cid, manager.ID, managerRole.ID); err != nil {
		t.Fatal(err)
	}
	path := "/v1/communities/" + cid.String() + "/bot"
	encode := func() []byte {
		body, err := cborx.Marshal([]any{id.New(), nil, uint64(16), ""}) // warnings requires PermModerateMembers
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	for _, tc := range []struct {
		name  string
		setup func()
	}{
		{name: "wire 0", setup: func() {}},
		{name: "wire 2", setup: func() {
			negotiate, _ := cborx.Marshal([]any{[]any{uint64(2)}, []any{uint64(1)}, []any{uint64(1)}})
			if rec := postCBORAuth(h, "/v1/sessions/negotiate", negotiate, managerToken); rec.Code != http.StatusOK {
				t.Fatalf("manager negotiate wire 2 = %d (%x)", rec.Code, rec.Body.Bytes())
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup()
			denied := putCBORAuth(h, path, encode(), managerToken)
			if denied.Code != http.StatusForbidden || refusalCode(t, denied) != "E_FORBIDDEN" {
				t.Fatalf("manager lacking requested capability permission = %d (%x), want 403 E_FORBIDDEN", denied.Code, denied.Body.Bytes())
			}
			if _, err := deps.Repo.GetActiveBotInstallationByCommunity(ctx, cid); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unauthorized configure installation lookup = %v, want no installation", err)
			}
		})
	}

	// The owner remains authorized, but wire 2 must refuse before creating or changing an installation.
	negotiate, _ := cborx.Marshal([]any{[]any{uint64(2)}, []any{uint64(1)}, []any{uint64(1)}})
	if rec := postCBORAuth(h, "/v1/sessions/negotiate", negotiate, ownerToken); rec.Code != http.StatusOK {
		t.Fatalf("owner negotiate wire 2 = %d (%x)", rec.Code, rec.Body.Bytes())
	}
	upgraded := putCBORAuth(h, path, encode(), ownerToken)
	if upgraded.Code != http.StatusBadRequest || refusalCode(t, upgraded) != "E_VERSION" {
		t.Fatalf("authorized wire-2 configure = %d (%x), want 400 E_VERSION", upgraded.Code, upgraded.Body.Bytes())
	}
	if _, err := deps.Repo.GetActiveBotInstallationByCommunity(ctx, cid); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("wire-refused configure installation lookup = %v, want no installation", err)
	}

	// A manager with the requested capability permission still keeps the prior
	// managed-role hierarchy refusal ahead of the compatibility response.
	managerRole.Allow |= uint64(api.PermModerateMembers)
	if err := deps.Repo.PutRole(ctx, managerRole); err != nil {
		t.Fatal(err)
	}
	managedRole := store.RoleRow{ID: id.New(), CommunityID: cid, Name: "managed bot", Position: managerRole.Position, Created: deps.Clock.Now().Unix()}
	if err := deps.Repo.PutRole(ctx, managedRole); err != nil {
		t.Fatal(err)
	}
	installation := store.BotInstallationRow{ID: id.New(), CommunityID: cid, RoleID: &managedRole.ID,
		State: store.BotInstallationPending, Revision: 1, Capabilities: 0, ApprovedBy: owner.ID,
		ApprovedAt: deps.Clock.Now().Unix(), Created: deps.Clock.Now().Unix(), Updated: deps.Clock.Now().Unix()}
	if err := deps.Repo.CreateBotInstallation(ctx, installation); err != nil {
		t.Fatal(err)
	}
	expectedRevision := uint64(1)
	ceilingBody, err := cborx.Marshal([]any{id.New(), expectedRevision, uint64(16), ""})
	if err != nil {
		t.Fatal(err)
	}
	deniedByHierarchy := putCBORAuth(h, path, ceilingBody, managerToken)
	if deniedByHierarchy.Code != http.StatusForbidden || refusalCode(t, deniedByHierarchy) != "E_FORBIDDEN" {
		t.Fatalf("manager at managed-role ceiling = %d (%x), want 403 E_FORBIDDEN", deniedByHierarchy.Code, deniedByHierarchy.Body.Bytes())
	}
	unchanged, err := deps.Repo.GetActiveBotInstallationByCommunity(ctx, cid)
	if err != nil || unchanged.ID != installation.ID || unchanged.Revision != installation.Revision || unchanged.Capabilities != installation.Capabilities {
		t.Fatalf("installation after managed-role refusal = %+v, %v; want unchanged %+v", unchanged, err, installation)
	}
}

func TestConfigureBotReplayPrecedesCurrentManagedRoleCeiling(t *testing.T) {
	h, deps := newTestAPIWired(t, func(d *api.Deps) {
		d.StepUps = api.NewStepUps(d.Clock, time.Minute)
	})
	owner, _, ownerToken := seedAPISession(t, deps)
	manager, _, managerToken := seedAPISession(t, deps)
	cid := seedBotCommunity(t, deps, owner.ID)
	ctx := context.Background()
	if err := deps.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: manager.ID, Joined: deps.Clock.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	managerRole := store.RoleRow{ID: id.New(), CommunityID: cid, Name: "bot manager", Position: 10,
		Allow: uint64(api.PermViewChannel | api.PermReadHistory | api.PermSendMessages | api.PermAddReactions | api.PermManageCommunity | api.PermModerateMembers), Created: deps.Clock.Now().Unix()}
	if err := deps.Repo.PutRole(ctx, managerRole); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.PutMemberRole(ctx, cid, manager.ID, managerRole.ID); err != nil {
		t.Fatal(err)
	}
	managedRole := store.RoleRow{ID: id.New(), CommunityID: cid, Name: "managed bot", Position: 5, Created: deps.Clock.Now().Unix()}
	if err := deps.Repo.PutRole(ctx, managedRole); err != nil {
		t.Fatal(err)
	}
	path := "/v1/communities/" + cid.String() + "/bot"
	negotiate := func(token string, version uint64) {
		t.Helper()
		body, err := cborx.Marshal([]any{[]any{version}, []any{uint64(1)}, []any{uint64(1)}})
		if err != nil {
			t.Fatal(err)
		}
		if rec := postCBORAuth(h, "/v1/sessions/negotiate", body, token); rec.Code != http.StatusOK {
			t.Fatalf("negotiate wire %d = %d (%x)", version, rec.Code, rec.Body.Bytes())
		}
	}
	negotiate(managerToken, 3)
	negotiate(ownerToken, 3)
	issueStepUp := func(user id.ID, token string) string {
		t.Helper()
		sess, err := deps.Sessions.Resolve(ctx, token)
		if err != nil {
			t.Fatal(err)
		}
		step, _ := deps.StepUps.Issue(user, sess.TokenHash)
		return step
	}
	encode := func(request id.ID, revision *uint64, caps uint64, step string) []byte {
		t.Helper()
		body, err := cborx.Marshal([]any{request, revision, caps, step})
		if err != nil {
			t.Fatal(err)
		}
		return body
	}

	// Manager accepts C1 and receives receipt A.
	receiptA := id.New()
	first := putCBORAuth(h, path, encode(receiptA, nil, 16, issueStepUp(manager.ID, managerToken)), managerToken)
	if first.Code != http.StatusCreated {
		t.Fatalf("initial C1 configure = %d (%x), want 201", first.Code, first.Body.Bytes())
	}
	current, err := deps.Repo.GetActiveBotInstallationByCommunity(ctx, cid)
	if err != nil {
		t.Fatal(err)
	}
	current.RoleID = &managedRole.ID
	current.Revision++
	current.Updated = deps.Clock.Now().Unix()
	if err := deps.Repo.UpdateBotInstallation(ctx, current.ID, current.Revision-1, current); err != nil {
		t.Fatalf("attach managed role: %v", err)
	}

	// Owner changes capabilities to C2, advancing the live installation.
	revision := current.Revision
	changed := putCBORAuth(h, path, encode(id.New(), &revision, 0, issueStepUp(owner.ID, ownerToken)), ownerToken)
	if changed.Code != http.StatusOK {
		t.Fatalf("owner configure C2 = %d (%x), want 200", changed.Code, changed.Body.Bytes())
	}
	current, err = deps.Repo.GetActiveBotInstallationByCommunity(ctx, cid)
	if err != nil {
		t.Fatal(err)
	}
	// A legacy-shaped request (no ?v=2) keeps Polls and Reminders, which a new installation starts with
	// (Task 10c fix round 1, I1).
	if current.Revision != revision+1 || current.Capabilities != uint16(botPollsRemindersMask) {
		t.Fatalf("installation after C2 = revision %d capabilities %d, want %d and %d", current.Revision, current.Capabilities, revision+1, botPollsRemindersMask)
	}

	// The manager retains C1's permission but now ranks below the managed bot role.
	managerRole.Position = 4
	if err := deps.Repo.PutRole(ctx, managerRole); err != nil {
		t.Fatal(err)
	}

	// The exact wire-3 receipt replay returns its historical result without step-up,
	// despite the changed live configuration and hierarchy.
	replay := putCBORAuth(h, path, encode(receiptA, nil, 16, ""), managerToken)
	if replay.Code != http.StatusOK {
		t.Fatalf("wire-3 replay A after C2/rank change = %d (%x), want 200", replay.Code, replay.Body.Bytes())
	}
	var replayValue []any
	if err := cborx.Unmarshal(replay.Body.Bytes(), &replayValue); err != nil {
		t.Fatal(err)
	}
	// The legacy create started with Polls and Reminders on (Task 10c fix round 1, I1).
	if len(replayValue) < 7 || replayValue[3] != uint64(1) || replayValue[6] != uint64(16|botPollsRemindersMask) {
		t.Fatalf("wire-3 replay result = %#v, want historical revision 1 / capabilities 16|768", replayValue)
	}

	negotiate(managerToken, 2)
	oldWireReplay := putCBORAuth(h, path, encode(receiptA, nil, 16, ""), managerToken)
	if oldWireReplay.Code != http.StatusBadRequest || refusalCode(t, oldWireReplay) != "E_VERSION" {
		t.Fatalf("wire-2 replay A = %d (%x), want 400 E_VERSION", oldWireReplay.Code, oldWireReplay.Body.Bytes())
	}

	// A fresh configure is a new action, so the current managed-role ceiling still
	// refuses it before E_VERSION and before recording an operation.
	freshID := id.New()
	currentRevision := current.Revision
	fresh := putCBORAuth(h, path, encode(freshID, &currentRevision, 16, ""), managerToken)
	if fresh.Code != http.StatusForbidden || refusalCode(t, fresh) != "E_FORBIDDEN" {
		t.Fatalf("wire-2 fresh configure at managed-role ceiling = %d (%x), want 403 E_FORBIDDEN", fresh.Code, fresh.Body.Bytes())
	}
	unchanged, err := deps.Repo.GetActiveBotInstallationByCommunity(ctx, cid)
	if err != nil || unchanged.Revision != currentRevision || unchanged.Capabilities != botPollsRemindersMask {
		t.Fatalf("installation after refused fresh configure = %+v, %v; want revision %d capabilities 768", unchanged, err, currentRevision)
	}
	if _, err := deps.Repo.GetBotOperation(ctx, cid, freshID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("fresh configure receipt lookup = %v, want no receipt", err)
	}
}

func testBotInstallationEnableRequiresStepUpAndReplaysOnlyTheSameRequest(t *testing.T, repo store.Repository) {
	h, deps := newTestAPIWithRepository(t, repo, func(d *api.Deps) {
		d.StepUps = api.NewStepUps(d.Clock, time.Minute)
	})
	owner, _, token := seedAPISession(t, deps)
	cid := id.New()
	ctx := context.Background()
	if err := deps.Repo.CreateCommunity(ctx, store.CommunityRow{ID: cid, Owner: owner.ID, Name: "test", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: deps.Clock.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: owner.ID, Joined: deps.Clock.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.PutRole(ctx, store.RoleRow{ID: id.New(), CommunityID: cid, Name: "@everyone", Position: 0, Created: deps.Clock.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	negotiate, _ := cborx.Marshal([]any{[]any{uint64(3)}, []any{uint64(1)}, []any{uint64(1)}})
	nonManager, _, nonManagerToken := seedAPISession(t, deps)
	if err := deps.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: nonManager.ID, Joined: deps.Clock.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	if rec := postCBORAuth(h, "/v1/sessions/negotiate", negotiate, nonManagerToken); rec.Code != http.StatusOK {
		t.Fatalf("nonmanager negotiate wire 3 = %d (%x)", rec.Code, rec.Body.Bytes())
	}
	if rec := postCBORAuth(h, "/v1/sessions/negotiate", negotiate, token); rec.Code != http.StatusOK {
		t.Fatalf("negotiate wire 3 = %d (%x)", rec.Code, rec.Body.Bytes())
	}
	path := "/v1/communities/" + cid.String() + "/bot"
	unauthorizedBody, _ := cborx.Marshal([]any{id.New(), nil, uint64(0), ""})
	if denied := putCBORAuth(h, path, unauthorizedBody, nonManagerToken); denied.Code != http.StatusForbidden {
		t.Fatalf("nonmanager enable = %d (%x), want 403", denied.Code, denied.Body.Bytes())
	}
	requestID := id.New()
	encode := func(step string) []byte {
		body, err := cborx.Marshal([]any{requestID, nil, uint64(0), step})
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	for _, step := range []string{"", "forged-step-up"} {
		rec := putCBORAuth(h, path, encode(step), token)
		if rec.Code == http.StatusCreated || rec.Code == http.StatusOK {
			t.Fatalf("enable with step-up %q unexpectedly succeeded", step)
		}
	}
	step, _ := deps.StepUps.Issue(owner.ID, func() []byte {
		s, err := deps.Sessions.Resolve(ctx, token)
		if err != nil {
			t.Fatal(err)
		}
		return s.TokenHash
	}())
	first := putCBORAuth(h, path, encode(step), token)
	if first.Code != http.StatusCreated {
		t.Fatalf("enable = %d (%x), want 201", first.Code, first.Body.Bytes())
	}
	page, err := deps.Repo.ListBotStructuralEvents(ctx, cid, 0, 10)
	if err != nil || len(page.Events) != 1 || page.Events[0].Kind != botprotocol.EventInstallation || page.Events[0].Data[1] != uint64(store.BotInstallationPending) || page.Events[0].Data[2] != uint64(1) {
		t.Fatalf("enable event = %+v, %v; want one pending installation revision 1", page, err)
	}
	replay := putCBORAuth(h, path, encode(""), token)
	if replay.Code != http.StatusOK {
		t.Fatalf("matching completed replay = %d (%x), want 200", replay.Code, replay.Body.Bytes())
	}
	noopID := id.New()
	noopBody, _ := cborx.Marshal([]any{noopID, uint64(1), uint64(0), ""})
	if noop := putCBORAuth(h, path, noopBody, token); noop.Code != http.StatusOK {
		t.Fatalf("new no-op capability approval = %d (%x), want 200", noop.Code, noop.Body.Bytes())
	}
	repurposedNoop, _ := cborx.Marshal([]any{noopID, uint64(1), uint64(1), ""})
	if conflict := putCBORAuth(h, path, repurposedNoop, token); conflict.Code != http.StatusConflict {
		t.Fatalf("reusing no-op request id for changed capabilities = %d (%x), want 409", conflict.Code, conflict.Body.Bytes())
	}

	conflictID := id.New()
	conflict, _ := cborx.Marshal([]any{conflictID, nil, uint64(0), ""})
	if rec := putCBORAuth(h, path, conflict, token); rec.Code != http.StatusConflict {
		t.Fatalf("new request with stale nil revision = %d, want 409", rec.Code)
	}
	row, err := deps.Repo.GetActiveBotInstallationByCommunity(ctx, cid)
	if err != nil || row.State != store.BotInstallationPending || row.Revision != 1 {
		t.Fatalf("installation after retries = %+v, %v; want pending revision 1", row, err)
	}
	page, err = deps.Repo.ListBotStructuralEvents(ctx, cid, 0, 10)
	if err != nil || len(page.Events) != 1 {
		t.Fatalf("replay/noop event page = %+v, %v; want exactly one event", page, err)
	}
	disableBody, _ := cborx.Marshal([]any{id.New(), uint64(1)})
	disablePath := path + "/disable"
	if rec := postCBORAuth(h, disablePath, disableBody, token); rec.Code != http.StatusOK {
		t.Fatalf("disable = %d (%x)", rec.Code, rec.Body.Bytes())
	}
	page, err = deps.Repo.ListBotStructuralEvents(ctx, cid, 0, 10)
	if err != nil || len(page.Events) != 2 || page.Events[1].Kind != botprotocol.EventInstallation || page.Events[1].Data[1] != uint64(store.BotInstallationDisabled) || page.Events[1].Data[2] != uint64(2) {
		t.Fatalf("disable event page = %+v, %v; want one disabled event revision 2", page, err)
	}
}

func TestRuntimeRoutesRejectOrdinaryHumanBearer(t *testing.T) {
	h, deps := newTestAPI(t)
	_, _, humanToken := seedAPISession(t, deps)
	installation := id.New().String()
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/v1/bot-runtime/installations"},
		{http.MethodPost, "/v1/bot-runtime/installations/" + installation + "/claim"},
		{http.MethodPost, "/v1/bot-runtime/installations/" + installation + "/enroll"},
	} {
		rec := doBotCBOR(h, tc.method, tc.path, nil, "Bearer "+humanToken)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with human bearer = %d (%x), want 401", tc.method, tc.path, rec.Code, rec.Body.Bytes())
		}
	}
}

func TestRuntimeDiscoveryFiltersBeforeLimitAndPaginatesVisibleRows(t *testing.T) {
	for engine, repo := range policyEngines(t) {
		t.Run(engine, func(t *testing.T) {
			var manager *botruntime.Manager
			h, deps := newTestAPIWithRepository(t, repo, func(d *api.Deps) {
				manager = botruntime.New(d.Repo, d.Clock.Now)
				d.BotRuntime = manager
			})
			ctx := context.Background()
			now := deps.Clock.Now().Unix()
			ownerID := id.New()
			if err := repo.CreateUser(ctx, store.UserRow{ID: ownerID, Username: "discovery" + ownerID.String()[:8], Display: "Owner",
				UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: now}); err != nil {
				t.Fatal(err)
			}
			var otherRuntimeID id.ID
			var runtimeToken string
			if _, err := manager.Create(ctx, func(_ id.ID, token string) error { runtimeToken = token; return nil }); err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Create(ctx, func(got id.ID, _ string) error { otherRuntimeID = got; return nil }); err != nil {
				t.Fatal(err)
			}
			insert := func(installationID id.ID, runtime *id.ID) {
				t.Helper()
				communityID := id.New()
				if err := repo.CreateCommunity(ctx, store.CommunityRow{ID: communityID, Owner: ownerID, Name: "discovery" + communityID.String()[:8],
					PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: now}); err != nil {
					t.Fatalf("CreateCommunity: %v", err)
				}
				if err := repo.CreateBotInstallation(ctx, store.BotInstallationRow{ID: installationID, CommunityID: communityID,
					RuntimeID: runtime, State: store.BotInstallationPending, Revision: 1, ApprovedBy: ownerID,
					ApprovedAt: now, Created: now, Updated: now}); err != nil {
					t.Fatalf("CreateBotInstallation: %v", err)
				}
			}
			for i := 1; i <= 110; i++ {
				var installID id.ID
				installID[0] = 0x10
				binary.BigEndian.PutUint16(installID[14:], uint16(i))
				insert(installID, &otherRuntimeID)
			}
			visible := make([]id.ID, 205)
			for i := range visible {
				visible[i][0] = 0x20
				binary.BigEndian.PutUint16(visible[i][14:], uint16(i+1))
				insert(visible[i], nil)
			}

			fetch := func(after id.ID) []id.ID {
				t.Helper()
				path := "/v1/bot-runtime/installations"
				if !after.IsZero() {
					path += "?after=" + after.String()
				}
				rec := doBotCBOR(h, http.MethodGet, path, nil, "Bearer "+runtimeToken)
				if rec.Code != http.StatusOK {
					t.Fatalf("GET runtime installations after %s = %d (%x)", after, rec.Code, rec.Body.Bytes())
				}
				var tuples []any
				if err := cborx.Unmarshal(rec.Body.Bytes(), &tuples); err != nil {
					t.Fatalf("decode discovery response: %v", err)
				}
				got := make([]id.ID, len(tuples))
				for i, tuple := range tuples {
					fields, ok := tuple.([]any)
					if !ok || len(fields) == 0 {
						t.Fatalf("installation tuple %d is %T (%v)", i, tuple, tuple)
					}
					got[i] = mustBotID(t, fields[0])
				}
				return got
			}
			first := fetch(id.ID{})
			if len(first) != 100 {
				t.Fatalf("first visible page has %d rows, want 100", len(first))
			}
			second := fetch(first[len(first)-1])
			if len(second) != 100 {
				t.Fatalf("second visible page has %d rows, want 100", len(second))
			}
			third := fetch(second[len(second)-1])
			if len(third) != 5 {
				t.Fatalf("third visible page has %d rows, want 5", len(third))
			}
			all := append(append(append([]id.ID(nil), first...), second...), third...)
			if len(all) != len(visible) {
				t.Fatalf("discovered %d rows, want %d", len(all), len(visible))
			}
			for i := range visible {
				if all[i] != visible[i] {
					t.Fatalf("discovery row %d = %s, want visible %s (foreign installations must remain private)", i, all[i], visible[i])
				}
			}
		})
	}
}

func TestDeviceDSKLockSerializesBotReuseWithoutRejectingHumanDuplicates(t *testing.T) {
	for engine, repo := range policyEngines(t) {
		t.Run(engine, func(t *testing.T) {
			for index, tc := range []struct {
				name        string
				kinds       []uint8
				botAPI      []bool
				wantSuccess int
			}{
				{name: "bot-bot", kinds: []uint8{1, 1}, botAPI: []bool{true, true}, wantSuccess: 1},
				{name: "human-bot", kinds: []uint8{0, 1}, botAPI: []bool{false, true}, wantSuccess: 1},
				{name: "human-human legacy duplicates", kinds: []uint8{0, 0}, botAPI: []bool{false, false}, wantSuccess: 2},
			} {
				t.Run(tc.name, func(t *testing.T) {
					userIDs := make([]id.ID, len(tc.kinds))
					for i, kind := range tc.kinds {
						userIDs[i] = id.New()
						if err := repo.CreateUser(context.Background(), store.UserRow{ID: userIDs[i], Username: "dsk" + userIDs[i].String()[:8],
							Display: "DSK race", Kind: kind, UMKPub: make([]byte, 32), SSKPub: make([]byte, 32),
							SigUMKSSK: make([]byte, 64), Created: 1}); err != nil {
							t.Fatal(err)
						}
					}
					dsk := make([]byte, 32)
					dsk[0], dsk[31] = byte(index+1), 0xA5
					start := make(chan struct{})
					results := make(chan error, len(tc.kinds))
					var ready sync.WaitGroup
					for i := range tc.kinds {
						device := store.DeviceRow{ID: id.New(), UserID: userIDs[i], DSKPub: append([]byte(nil), dsk...),
							CredentialBlob: []byte{1}, Created: 1}
						ready.Add(1)
						go func(botAPI bool) {
							defer ready.Done()
							<-start
							results <- repo.Tx(context.Background(), func(tx store.Repository) error {
								if botAPI {
									return tx.CreateBotDevice(context.Background(), device)
								}
								return tx.CreateDevice(context.Background(), device)
							})
						}(tc.botAPI[i])
					}
					close(start)
					ready.Wait()
					close(results)
					successes, conflicts := 0, 0
					for err := range results {
						if err == nil {
							successes++
						} else if errors.Is(err, store.ErrConflict) {
							conflicts++
						} else {
							t.Errorf("CreateDevice result: %v", err)
						}
					}
					if successes != tc.wantSuccess || conflicts != len(tc.kinds)-tc.wantSuccess {
						t.Fatalf("successes=%d conflicts=%d, want %d/%d", successes, conflicts, tc.wantSuccess, len(tc.kinds)-tc.wantSuccess)
					}
				})
			}
			botID, humanID, deviceID := id.New(), id.New(), id.New()
			for userID, kind := range map[id.ID]uint8{botID: 1, humanID: 0} {
				if err := repo.CreateUser(context.Background(), store.UserRow{ID: userID, Username: "revoked" + userID.String()[:8],
					Display: "Revoked DSK", Kind: kind, UMKPub: make([]byte, 32), SSKPub: make([]byte, 32),
					SigUMKSSK: make([]byte, 64), Created: 1}); err != nil {
					t.Fatal(err)
				}
			}
			dsk := make([]byte, 32)
			dsk[0] = 0xF0
			revokedAt := int64(2)
			if err := repo.CreateDevice(context.Background(), store.DeviceRow{ID: deviceID, UserID: botID, DSKPub: dsk,
				CredentialBlob: []byte{1}, RevokedAt: &revokedAt, Created: 1}); err != nil {
				t.Fatalf("seed revoked Bot DSK: %v", err)
			}
			if err := repo.CreateDevice(context.Background(), store.DeviceRow{ID: id.New(), UserID: humanID, DSKPub: dsk,
				CredentialBlob: []byte{1}, Created: 1}); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("reuse revoked Bot DSK = %v, want ErrConflict", err)
			}
		})
	}
}

func TestRuntimeClaimEnrollmentAndReplayUseVerifiedBotCredential(t *testing.T) {
	for engine, repo := range policyEngines(t) {
		t.Run(engine, func(t *testing.T) { testRuntimeClaimEnrollmentAndReplayUseVerifiedBotCredential(t, repo) })
	}
}

func TestRemoveManagedBotInstallationEmitsMembershipLeave(t *testing.T) {
	for engine, repo := range policyEngines(t) {
		t.Run(engine, func(t *testing.T) {
			h, deps := newTestAPIWithRepository(t, repo, nil)
			owner, _, token := seedAPISession(t, deps)
			cid := seedBotCommunity(t, deps, owner.ID)
			negotiate, _ := cborx.Marshal([]any{[]any{uint64(3)}, []any{uint64(1)}, []any{uint64(1)}})
			if rec := postCBORAuth(h, "/v1/sessions/negotiate", negotiate, token); rec.Code != http.StatusOK {
				t.Fatalf("negotiate wire 3 = %d (%x)", rec.Code, rec.Body.Bytes())
			}
			ctx := context.Background()
			now := deps.Clock.Now().Unix()
			botUser := store.UserRow{ID: id.New(), Username: "bot" + id.New().String()[:8], Display: "Managed bot", Kind: 1,
				UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: now}
			device := store.DeviceRow{ID: id.New(), UserID: botUser.ID, DSKPub: make([]byte, 32), CredentialBlob: []byte{1}, Created: now}
			installation := store.BotInstallationRow{ID: id.New(), CommunityID: cid, UserID: &botUser.ID, DeviceID: &device.ID,
				State: store.BotInstallationReady, Revision: 1, ApprovedBy: owner.ID, ApprovedAt: now, Created: now, Updated: now}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				if err := tx.CreateUser(ctx, botUser); err != nil {
					return err
				}
				if err := tx.CreateBotDevice(ctx, device); err != nil {
					return err
				}
				if err := tx.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: botUser.ID, Joined: now}); err != nil {
					return err
				}
				return tx.CreateBotInstallation(ctx, installation)
			}); err != nil {
				t.Fatal(err)
			}
			member, err := repo.GetMember(ctx, cid, botUser.ID)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := cborx.Marshal([]any{id.New(), uint64(1)})
			path := "/v1/communities/" + cid.String() + "/bot"
			if rec := doBotCBOR(h, http.MethodDelete, path, body, "Bearer "+token); rec.Code != http.StatusNoContent {
				t.Fatalf("remove managed bot = %d (%x)", rec.Code, rec.Body.Bytes())
			}
			page, err := repo.ListBotStructuralEvents(ctx, cid, 0, 10)
			if err != nil || len(page.Events) != 2 {
				t.Fatalf("remove events = %+v, %v; want installation and member leave", page, err)
			}
			var leave *botprotocol.Event
			for i := range page.Events {
				if page.Events[i].Kind == botprotocol.EventMemberLeave {
					leave = &page.Events[i]
				}
			}
			if leave == nil || len(leave.Data) != 2 || leave.Data[0] != botUser.ID || leave.Data[1] != member.EpisodeID {
				t.Fatalf("managed bot leave event = %+v; want [%s,%s]", leave, botUser.ID, member.EpisodeID)
			}
			if _, err := repo.GetMember(ctx, cid, botUser.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("managed bot membership after removal = %v, want not found", err)
			}
		})
	}
}

func TestBotChannelSyncWaitsForKeyPackagesAndStaysBoundToItsInstallation(t *testing.T) {
	_, deps := newTestAPI(t)
	ownerA, ownerDeviceA, _ := seedAPISession(t, deps)
	ownerB, ownerDeviceB, _ := seedAPISession(t, deps)
	cidA := seedBotCommunity(t, deps, ownerA.ID)
	cidB := seedBotCommunity(t, deps, ownerB.ID)
	ctx := context.Background()
	now := deps.Clock.Now().Unix()
	botID, deviceID := id.New(), id.New()
	device := store.DeviceRow{ID: deviceID, UserID: botID, DSKPub: make([]byte, 32), CredentialBlob: []byte{1}, Created: now}
	if err := deps.Repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.CreateUser(ctx, store.UserRow{ID: botID, Username: "syncbot", Kind: 1, UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: now}); err != nil {
			return err
		}
		if err := tx.CreateBotDevice(ctx, device); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var activeRole id.ID
	for _, cid := range []id.ID{cidA, cidB} {
		role := store.RoleRow{ID: id.New(), CommunityID: cid, Name: "bot viewer", Position: 1,
			Allow: uint64(api.PermViewChannel | api.PermReadHistory | api.PermSendMessages), Created: now}
		if err := deps.Repo.PutRole(ctx, role); err != nil {
			t.Fatal(err)
		}
		if err := deps.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: botID, Joined: now}); err != nil {
			t.Fatal(err)
		}
		if err := deps.Repo.PutMemberRole(ctx, cid, botID, role.ID); err != nil {
			t.Fatal(err)
		}
		if cid == cidA {
			activeRole = role.ID
		}
	}
	if err := deps.Repo.Tx(ctx, func(tx store.Repository) error {
		return tx.CreateBotInstallation(ctx, store.BotInstallationRow{ID: id.New(), CommunityID: cidA, UserID: &botID, DeviceID: &deviceID, RoleID: &activeRole,
			State: store.BotInstallationJoining, Revision: 1, ApprovedBy: ownerA.ID, ApprovedAt: now, Created: now, Updated: now})
	}); err != nil {
		t.Fatal(err)
	}
	channelA, channelB, groupA, groupB := id.New(), id.New(), id.New(), id.New()
	for _, item := range []struct{ cid, channel, group, owner, ownerDevice id.ID }{
		{cidA, channelA, groupA, ownerA.ID, ownerDeviceA.ID},
		{cidB, channelB, groupB, ownerB.ID, ownerDeviceB.ID},
	} {
		c := item.cid
		if err := deps.Repo.CreateChannel(ctx, store.ChannelRow{ID: item.channel, CommunityID: &c, Kind: api.ChannelText, Mode: api.ModeE2EE, Name: "encrypted", SettingsJSON: []byte(`{}`), HostPolicyVersion: 1, Created: now}); err != nil {
			t.Fatal(err)
		}
		if err := deps.Repo.CreateGroup(ctx, store.GroupRow{GroupID: item.group, Binding: []byte{0x80}, Kind: api.GroupText, CommunityID: &c, TargetID: item.channel, Ciphersuite: 1, Epoch: 1, ExternalSenderKeyID: id.New(), E2EEVersion: 1, MediaVersion: 1, PolicyVersion: 1, Created: now}); err != nil {
			t.Fatal(err)
		}
		ownerDevice, err := deps.Repo.GetDevice(ctx, item.ownerDevice)
		if err != nil {
			t.Fatal(err)
		}
		if err := deps.Repo.ReplaceMembers(ctx, item.group, 0, []store.MemberRow{{GroupID: item.group, UserID: item.owner, DeviceID: item.ownerDevice, SignatureKey: ownerDevice.DSKPub}}); err != nil {
			t.Fatal(err)
		}
	}
	dsvc := &recordingDS{}
	if err := api.SyncEnrolledBotChannels(ctx, deps.Repo, dsvc, botID, deviceID, now); err != nil {
		t.Fatalf("sync before KeyPackages: %v", err)
	}
	if got := dsvc.Adds; len(got) != 0 {
		t.Fatalf("Adds before KeyPackages = %+v, want none", got)
	}
	ref := id.New()
	if err := deps.Repo.PutKeyPackages(ctx, deviceID, []store.KeyPackageRow{{DeviceID: deviceID, KPRef: ref[:], Blob: []byte{1}, Expires: now + 3600, Created: now}}); err != nil {
		t.Fatal(err)
	}
	resolved, err := api.NewResolver(deps.Repo).Resolve(ctx, botID, store.ChannelRow{ID: channelA, CommunityID: &cidA, Kind: api.ChannelText, Mode: api.ModeE2EE})
	if err != nil || !resolved.Has(api.PermViewChannel) {
		t.Fatalf("bot channel permissions = %v, %v; want view", resolved, err)
	}
	if err := api.SyncEnrolledBotChannels(ctx, deps.Repo, dsvc, botID, deviceID, now); err != nil {
		t.Fatalf("sync after KeyPackages: %v", err)
	}
	channelMembers, _ := deps.Repo.ListChannelMembers(ctx, channelA)
	packageCount, _ := deps.Repo.CountKeyPackages(ctx, deviceID, now)
	if len(dsvc.Adds) != 1 || dsvc.Adds[0].Group != groupA || dsvc.Adds[0].Device != deviceID {
		t.Fatalf("Adds after KeyPackages = %+v, channel members=%v packages=%d; want only bot device %s in bound group %s (foreign group %s must remain untouched)", dsvc.Adds, channelMembers, packageCount, deviceID, groupA, groupB)
	}
}

func testRuntimeClaimEnrollmentAndReplayUseVerifiedBotCredential(t *testing.T, repo store.Repository) {
	var manager *botruntime.Manager
	var enrollmentHooks []store.BotInstallationRow
	h, deps := newTestAPIWithRepository(t, repo, func(d *api.Deps) {
		d.StepUps = api.NewStepUps(d.Clock, time.Minute)
		manager = botruntime.New(d.Repo, d.Clock.Now)
		d.BotRuntime = manager
		d.BotEnrolled = func(ctx context.Context, row store.BotInstallationRow) {
			if ctx.Err() != nil {
				t.Errorf("post-enrollment context is canceled: %v", ctx.Err())
			}
			enrollmentHooks = append(enrollmentHooks, row)
		}
	})
	owner, _, humanToken := seedAPISession(t, deps)
	cid := seedBotCommunity(t, deps, owner.ID)
	negotiate, _ := cborx.Marshal([]any{[]any{uint64(3)}, []any{uint64(1)}, []any{uint64(1)}})
	if rec := postCBORAuth(h, "/v1/sessions/negotiate", negotiate, humanToken); rec.Code != http.StatusOK {
		t.Fatalf("negotiate wire 3 = %d (%x)", rec.Code, rec.Body.Bytes())
	}
	ownerSession, err := deps.Sessions.Resolve(context.Background(), humanToken)
	if err != nil {
		t.Fatal(err)
	}
	step, _ := deps.StepUps.Issue(owner.ID, ownerSession.TokenHash)
	communityPath := "/v1/communities/" + cid.String() + "/bot"
	configureID := id.New()
	configure, _ := cborx.Marshal([]any{configureID, nil, uint64(0), step})
	enabled := putCBORAuth(h, communityPath, configure, humanToken)
	if enabled.Code != http.StatusCreated {
		t.Fatalf("enable = %d (%x)", enabled.Code, enabled.Body.Bytes())
	}
	var installationResponse []any
	if err := cborx.Unmarshal(enabled.Body.Bytes(), &installationResponse); err != nil {
		t.Fatal(err)
	}
	installationID := mustBotID(t, installationResponse[0])
	var runtimeID id.ID
	var runtimeToken string
	if _, err := manager.Create(context.Background(), func(got id.ID, token string) error {
		runtimeID, runtimeToken = got, token
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	authHeader := "Bearer " + runtimeToken
	claimRequestID := id.New()
	claimBody, _ := cborx.Marshal([]any{claimRequestID})
	claim := doBotCBOR(h, http.MethodPost, "/v1/bot-runtime/installations/"+installationID.String()+"/claim", claimBody, authHeader)
	if claim.Code != http.StatusOK {
		t.Fatalf("claim = %d (%x)", claim.Code, claim.Body.Bytes())
	}
	var claimResponse []any
	if err := cborx.Unmarshal(claim.Body.Bytes(), &claimResponse); err != nil {
		t.Fatal(err)
	}
	claimID := mustBotID(t, claimResponse[1])
	deviceID := id.New()
	credential := makeTestCredential(t, deviceID, 1)
	enrollRequestID := id.New()
	enrollPath := "/v1/bot-runtime/installations/" + installationID.String() + "/enroll"
	makeEnrollBody := func(request id.ID, identity testHumanCredential, umk, ssk, dsk, raw []byte) []byte {
		t.Helper()
		var credentialFields []any
		if err := cborx.Unmarshal(raw, &credentialFields); err != nil || len(credentialFields) < 4 {
			t.Fatalf("decode enrollment credential device id: %v", err)
		}
		credentialDeviceID, ok := credentialFields[3].([]byte)
		if !ok || len(credentialDeviceID) != 16 {
			t.Fatal("enrollment credential has malformed device id")
		}
		body, err := cborx.Marshal([]any{request, claimID, umk, ssk, identity.SigUMKSSK,
			[]any{credentialDeviceID, dsk, uint64(0), uint64(0), raw}})
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	humanCredential := makeTestCredential(t, deviceID, 0)
	wrongDSK := append([]byte(nil), credential.DSKPub...)
	wrongDSK[0] ^= 1
	wrongUMK := append([]byte(nil), credential.UMKPub...)
	wrongUMK[0] ^= 1
	wrongSSK := append([]byte(nil), credential.SSKPub...)
	wrongSSK[0] ^= 1
	badSignature := append([]byte(nil), credential.Credential...)
	badSignature[len(badSignature)-1] ^= 1
	wrongTier := mutateBotCredential(t, credential, id.ID{}, 1, 0)
	wrongUser := id.New()
	wrongUserCredential := mutateBotCredential(t, credential, wrongUser, 0, 0)
	for _, invalid := range []struct {
		name      string
		identity  testHumanCredential
		umk, ssk  []byte
		dsk, blob []byte
	}{
		{name: "human kind", identity: humanCredential, umk: humanCredential.UMKPub, ssk: humanCredential.SSKPub, dsk: humanCredential.DSKPub, blob: humanCredential.Credential},
		{name: "foreign DSK", identity: credential, umk: credential.UMKPub, ssk: credential.SSKPub, dsk: wrongDSK, blob: credential.Credential},
		{name: "mismatched UMK", identity: credential, umk: wrongUMK, ssk: credential.SSKPub, dsk: credential.DSKPub, blob: credential.Credential},
		{name: "mismatched SSK", identity: credential, umk: credential.UMKPub, ssk: wrongSSK, dsk: credential.DSKPub, blob: credential.Credential},
		{name: "browser tier", identity: credential, umk: credential.UMKPub, ssk: credential.SSKPub, dsk: credential.DSKPub, blob: wrongTier},
		{name: "assigned foreign user", identity: credential, umk: credential.UMKPub, ssk: credential.SSKPub, dsk: credential.DSKPub, blob: wrongUserCredential},
		{name: "bad signature", identity: credential, umk: credential.UMKPub, ssk: credential.SSKPub, dsk: credential.DSKPub, blob: badSignature},
	} {
		bad := doBotCBOR(h, http.MethodPost, enrollPath,
			makeEnrollBody(id.New(), invalid.identity, invalid.umk, invalid.ssk, invalid.dsk, invalid.blob), authHeader)
		if bad.Code != http.StatusBadRequest {
			t.Errorf("enrollment with %s = %d (%x), want 400", invalid.name, bad.Code, bad.Body.Bytes())
		}
	}

	// This credential and submitted DSK are internally valid, but the public key is
	// already registered on another device. Enrollment must reject it transactionally.
	duplicateCredential := makeTestCredential(t, id.New(), 1)
	registeredDSKDevice := id.New()
	if err := deps.Repo.CreateDevice(context.Background(), store.DeviceRow{ID: registeredDSKDevice, UserID: owner.ID,
		DSKPub: duplicateCredential.DSKPub, CredentialBlob: []byte{1}, Tier: 0, SignerTier: 0, Created: deps.Clock.Now().Unix()}); err != nil {
		t.Fatalf("seed existing device DSK: %v", err)
	}
	usersBefore := countRepoUsersAndDevices(t, deps.Repo)
	membersBefore, err := deps.Repo.ListMembersOfCommunity(context.Background(), cid, id.ID{}, 100)
	if err != nil {
		t.Fatalf("list community members before duplicate enrollment: %v", err)
	}
	duplicateEnroll := makeEnrollBody(id.New(), duplicateCredential, duplicateCredential.UMKPub, duplicateCredential.SSKPub,
		duplicateCredential.DSKPub, duplicateCredential.Credential)
	if rec := doBotCBOR(h, http.MethodPost, enrollPath, duplicateEnroll, authHeader); rec.Code != http.StatusBadRequest {
		t.Fatalf("enrollment reusing registered DSK = %d (%x), want a client refusal", rec.Code, rec.Body.Bytes())
	}
	if usersAfter := countRepoUsersAndDevices(t, deps.Repo); usersAfter != usersBefore {
		t.Fatalf("duplicate DSK enrollment left account/device rows: before=%v after=%v", usersBefore, usersAfter)
	}
	membersAfter, err := deps.Repo.ListMembersOfCommunity(context.Background(), cid, id.ID{}, 100)
	if err != nil || len(membersAfter) != len(membersBefore) {
		t.Fatalf("duplicate DSK enrollment left membership: before=%d after=%d err=%v", len(membersBefore), len(membersAfter), err)
	}
	beforeEnrollEvents, err := deps.Repo.ListBotStructuralEvents(context.Background(), cid, 0, 10)
	if err != nil || len(beforeEnrollEvents.Events) != 1 || beforeEnrollEvents.Events[0].Kind != botprotocol.EventInstallation {
		t.Fatalf("rejected enrollments emitted events: %+v, %v", beforeEnrollEvents, err)
	}

	enrollBody := makeEnrollBody(enrollRequestID, credential, credential.UMKPub, credential.SSKPub, credential.DSKPub, credential.Credential)
	enrolled := doBotCBOR(h, http.MethodPost, enrollPath, enrollBody, authHeader)
	if enrolled.Code != http.StatusCreated {
		t.Fatalf("enroll = %d (%x)", enrolled.Code, enrolled.Body.Bytes())
	}
	if len(enrollmentHooks) != 1 || enrollmentHooks[0].CommunityID != cid || enrollmentHooks[0].UserID == nil || enrollmentHooks[0].DeviceID == nil {
		t.Fatalf("post-commit enrollment hook = %+v, want exactly the enrolled community, user, and device", enrollmentHooks)
	}
	var enrollmentResult []any
	if err := cborx.Unmarshal(enrolled.Body.Bytes(), &enrollmentResult); err != nil {
		t.Fatal(err)
	}
	userID := mustBotID(t, enrollmentResult[0])
	user, err := deps.Repo.GetUser(context.Background(), userID)
	if err != nil || user.Kind != 1 {
		t.Fatalf("enrolled user = %+v, %v; want Kind=1", user, err)
	}
	nonce, _, err := deps.Sessions.Challenge(context.Background(), deviceID)
	if err != nil {
		t.Fatalf("challenge for enrolled bot = %v", err)
	}
	preimage := auth.SessionPreimage(deps.Sessions.InstanceID(), deviceID, nonce, auth.PurposeSession)
	botSession, err := deps.Sessions.Establish(context.Background(), auth.EstablishRequest{
		DeviceID: deviceID, Nonce: nonce, Purpose: auth.PurposeSession,
		Sig: ed25519.Sign(ed25519.PrivateKey(credential.DSKPriv), preimage),
	})
	if err != nil {
		t.Fatalf("establish bound bot session = %v", err)
	}
	botToken := botSession.Token
	if negotiated := postCBORAuth(h, "/v1/sessions/negotiate", negotiate, botToken); negotiated.Code != http.StatusOK {
		t.Fatalf("bot negotiate wire 3 = %d (%x)", negotiated.Code, negotiated.Body.Bytes())
	}
	finalCredential := mutateBotCredential(t, credential, userID, 0, 0)
	credentialRequestID := id.New()
	credentialBody, _ := cborx.Marshal([]any{credentialRequestID, uint64(2), finalCredential})
	credentialPath := "/v1/bot-runtime/installations/" + installationID.String() + "/credential"
	finalized := putCBORAuth(h, credentialPath, credentialBody, botToken)
	if finalized.Code != http.StatusOK {
		t.Fatalf("finalize assigned bot credential = %d (%x)", finalized.Code, finalized.Body.Bytes())
	}
	finalizedDevice, err := deps.Repo.GetDevice(context.Background(), deviceID)
	if err != nil || finalizedDevice.VerifiedAt == nil || *finalizedDevice.VerifiedAt != deps.Clock.Now().Unix() {
		t.Fatalf("finalized Bot device verified_at = %+v, %v; want the server verification time", finalizedDevice, err)
	}
	if retry := putCBORAuth(h, credentialPath, credentialBody, botToken); retry.Code != http.StatusOK {
		t.Fatalf("matching final credential replay while joining = %d (%x)", retry.Code, retry.Body.Bytes())
	}
	page, err := deps.Repo.ListBotStructuralEvents(context.Background(), cid, 0, 10)
	if err != nil || len(page.Events) != 3 {
		t.Fatalf("enrollment event page = %+v, %v; want pending, enrolling, joining exactly once", page, err)
	}
	for i, state := range []uint8{store.BotInstallationPending, store.BotInstallationEnrolling, store.BotInstallationJoining} {
		event := page.Events[i]
		if event.Kind != botprotocol.EventInstallation || event.Data[0] != installationID || event.Data[1] != uint64(state) || event.Data[2] != uint64(i+1) || event.Sequence != uint64(i+1) {
			t.Fatalf("installation event %d = %+v, want state %d revision %d", i, event, state, i+1)
		}
	}
	installationAfterRetry, err := deps.Repo.GetActiveBotInstallationByCommunity(context.Background(), cid)
	if err != nil || installationAfterRetry.Revision != 3 || installationAfterRetry.State != store.BotInstallationJoining {
		t.Fatalf("matching credential retry changed installation phase/revision: %+v, %v", installationAfterRetry, err)
	}
	changedCredential := append([]byte(nil), finalCredential...)
	changedCredential[len(changedCredential)-1] ^= 1
	changedBody, _ := cborx.Marshal([]any{credentialRequestID, uint64(2), changedCredential})
	if changed := putCBORAuth(h, credentialPath, changedBody, botToken); changed.Code != http.StatusConflict {
		t.Fatalf("same credential request id with changed payload = %d (%x), want 409", changed.Code, changed.Body.Bytes())
	}
	credentialReceipt, err := deps.Repo.GetBotOperation(context.Background(), cid, credentialRequestID)
	if err != nil || credentialReceipt.ActorKind != 2 {
		t.Fatalf("final credential receipt actor kind = %d, %v; want bot actor kind 2", credentialReceipt.ActorKind, err)
	}
	wrongActorRequestID := id.New()
	wrongActorPayload, _ := cborx.Marshal(struct {
		Request, Installation id.ID
		Revision              uint64
		Credential            []byte
	}{wrongActorRequestID, installationID, 3, finalCredential})
	wrongActorHash := sha256.Sum256(wrongActorPayload)
	deviceActor := deviceID
	if err := deps.Repo.CreateBotOperation(context.Background(), store.BotOperationRow{
		CommunityID: cid, RequestID: wrongActorRequestID, InstallationID: installationID,
		ActorKind: 0, ActorID: userID, ActorDevice: &deviceActor, Operation: 6,
		PayloadHash: wrongActorHash[:], ResultCBOR: []byte{0x80}, Created: deps.Clock.Now().Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	wrongActorBody, _ := cborx.Marshal([]any{wrongActorRequestID, uint64(3), finalCredential})
	if rejected := putCBORAuth(h, credentialPath, wrongActorBody, botToken); rejected.Code != http.StatusConflict {
		t.Fatalf("receipt substituted with a human actor kind = %d (%x), want 409", rejected.Code, rejected.Body.Bytes())
	}
	readyBody, _ := cborx.Marshal([]any{id.New(), uint64(3)})
	readyPath := "/v1/bot-runtime/installations/" + installationID.String() + "/ready"
	if ready := postCBORAuth(h, readyPath, readyBody, botToken); ready.Code != http.StatusConflict {
		t.Fatalf("ready without signed device list/key packages = %d (%x), want 409", ready.Code, ready.Body.Bytes())
	}
	if fake, ok := deps.Clock.(interface{ Advance(time.Duration) }); ok {
		fake.Advance(601 * time.Second)
	} else {
		t.Fatal("test clock does not support advancing time")
	}
	if replay := doBotCBOR(h, http.MethodPost, enrollPath, enrollBody, authHeader); replay.Code != http.StatusOK {
		t.Fatalf("same enrollment replay after claim expiry = %d (%x), want 200", replay.Code, replay.Body.Bytes())
	}
	if err := manager.Rotate(context.Background(), runtimeID, func(got id.ID, token string) error {
		if got != runtimeID {
			t.Fatalf("rotation changed runtime id from %s to %s", runtimeID, got)
		}
		runtimeToken = token
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	listOld := doBotCBOR(h, http.MethodGet, "/v1/bot-runtime/installations", nil, authHeader)
	if listOld.Code != http.StatusUnauthorized {
		t.Fatalf("old runtime credential after rotation = %d, want 401", listOld.Code)
	}
	if replay := doBotCBOR(h, http.MethodPost, enrollPath, enrollBody, "Bearer "+runtimeToken); replay.Code != http.StatusOK {
		t.Fatalf("same enrollment receipt after rotation = %d (%x), want 200", replay.Code, replay.Body.Bytes())
	}
	if err := manager.Revoke(context.Background(), runtimeID); err != nil {
		t.Fatalf("revoke runtime credential: %v", err)
	}
	if denied := doBotCBOR(h, http.MethodGet, "/v1/bot-runtime/installations", nil, "Bearer "+runtimeToken); denied.Code != http.StatusUnauthorized {
		t.Fatalf("runtime list after revocation = %d (%x), want 401", denied.Code, denied.Body.Bytes())
	}
	if denied := putCBORAuth(h, credentialPath, credentialBody, botToken); denied.Code != http.StatusUnauthorized {
		t.Fatalf("bot credential replay after runtime revocation = %d (%x), want 401", denied.Code, denied.Body.Bytes())
	}
}

func mutateBotCredential(t *testing.T, credential testHumanCredential, userID id.ID, tier, signerTier byte) []byte {
	t.Helper()
	var fields []any
	if err := cborx.Unmarshal(credential.Credential, &fields); err != nil {
		t.Fatal(err)
	}
	fields[2], fields[5], fields[6] = userID[:], uint64(tier), uint64(signerTier)
	deviceID, ok := fields[3].([]byte)
	if !ok || len(deviceID) != 16 {
		t.Fatal("test credential has malformed device id")
	}
	message := append([]byte("dilla dsk v1"), deviceID...)
	message = append(message, credential.DSKPub...)
	message = append(message, byte(fields[4].(uint64)), tier, signerTier)
	fields[9] = ed25519.Sign(ed25519.PrivateKey(credential.SSKPriv), message)
	raw, err := cborx.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func countRepoUsersAndDevices(t *testing.T, repo store.Repository) [2]int {
	t.Helper()
	var counts [2]int
	after := id.ID{}
	for {
		users, err := repo.ListUsers(context.Background(), after, 100)
		if err != nil {
			t.Fatalf("ListUsers: %v", err)
		}
		counts[0] += len(users)
		for _, user := range users {
			devices, err := repo.ListDevicesByUser(context.Background(), user.ID)
			if err != nil {
				t.Fatalf("ListDevicesByUser(%s): %v", user.ID, err)
			}
			counts[1] += len(devices)
		}
		if len(users) < 100 {
			return counts
		}
		after = users[len(users)-1].ID
	}
}

func seedBotCommunity(t *testing.T, d api.Deps, owner id.ID) id.ID {
	t.Helper()
	cid := id.New()
	ctx := context.Background()
	if err := d.Repo.CreateCommunity(ctx, store.CommunityRow{ID: cid, Owner: owner, Name: "test", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: d.Clock.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	if err := d.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: owner, Joined: d.Clock.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	if err := d.Repo.PutRole(ctx, store.RoleRow{ID: id.New(), CommunityID: cid, Name: "@everyone", Position: 0, Created: d.Clock.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	return cid
}

func mustBotID(t *testing.T, raw any) id.ID {
	t.Helper()
	var out id.ID
	b, ok := raw.([]byte)
	if !ok || len(b) != len(out) {
		t.Fatalf("value %T (%v) is not an ID", raw, raw)
	}
	copy(out[:], b)
	return out
}

func doBotCBOR(h http.Handler, method, path string, body []byte, authorization string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/cbor")
	req.Header.Set("Authorization", authorization)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func putCBORAuth(h http.Handler, path string, body []byte, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPut, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/cbor")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
