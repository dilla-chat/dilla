package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/api"
	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/bot/runtime"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

// botSettingsFixture is a community with a ready installation run by one runtime, its owner (a
// manager, wire 3), a plain member and a moderator without manage_community, and a second runtime.
type botSettingsFixture struct {
	h                       http.Handler
	deps                    api.Deps
	cid, installation       id.ID
	ownerToken, memberToken string
	modToken                string
	runtimeToken, otherTok  string
}

func newBotSettingsFixture(t *testing.T, repo store.Repository) botSettingsFixture {
	t.Helper()
	var manager *runtime.Manager
	h, deps := newTestAPIWithRepository(t, repo, func(d *api.Deps) {
		manager = runtime.New(d.Repo, d.Clock.Now)
		d.BotRuntime = manager
		d.StepUps = api.NewStepUps(d.Clock, time.Minute)
	})
	ctx := context.Background()
	now := deps.Clock.Now().Unix()
	owner, _, ownerToken := seedAPISession(t, deps)
	member, _, memberToken := seedAPISession(t, deps)
	mod, _, modToken := seedAPISession(t, deps)
	for _, tok := range []string{ownerToken, memberToken, modToken} {
		if err := deps.Sessions.RecordCapabilities(ctx, auth.TokenHash(tok), 3, 1, 1); err != nil {
			t.Fatal(err)
		}
	}
	cid := seedBotCommunity(t, deps, owner.ID)
	roles, err := repo.ListRoles(ctx, cid)
	if err != nil {
		t.Fatal(err)
	}
	everyone := roles[0]
	everyone.Allow = uint64(api.PermViewChannel | api.PermSendMessages | api.PermReadHistory | api.PermCreateInvite)
	if err := repo.PutRole(ctx, everyone); err != nil {
		t.Fatal(err)
	}
	mods := store.RoleRow{ID: id.New(), CommunityID: cid, Name: "mods", Position: 1,
		Allow: uint64(api.PermManageMessages | api.PermKickMembers | api.PermBanMembers | api.PermManageRoles | api.PermManageChannels), Created: now}
	if err := repo.PutRole(ctx, mods); err != nil {
		t.Fatal(err)
	}
	for _, u := range []id.ID{member.ID, mod.ID} {
		if err := repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: u, Joined: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.PutMemberRole(ctx, cid, mod.ID, mods.ID); err != nil {
		t.Fatal(err)
	}
	var runtimeID id.ID
	var runtimeToken, otherTok string
	if _, err := manager.Create(ctx, func(got id.ID, token string) error { runtimeID, runtimeToken = got, token; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(ctx, func(_ id.ID, token string) error { otherTok = token; return nil }); err != nil {
		t.Fatal(err)
	}
	installation := id.New()
	if err := repo.CreateBotInstallation(ctx, store.BotInstallationRow{ID: installation, CommunityID: cid, RuntimeID: &runtimeID,
		State: store.BotInstallationReady, Revision: 1, Capabilities: 1 | 1<<8 | 1<<9, ApprovedBy: owner.ID, ApprovedAt: now, Created: now, Updated: now}); err != nil {
		t.Fatal(err)
	}
	return botSettingsFixture{h: h, deps: deps, cid: cid, installation: installation, ownerToken: ownerToken, memberToken: memberToken,
		modToken: modToken, runtimeToken: runtimeToken, otherTok: otherTok}
}

func (f botSettingsFixture) humanInstallation(t *testing.T) []any {
	t.Helper()
	rec := getAuth(f.h, "/v1/communities/"+f.cid.String()+"/bot", f.ownerToken)
	var value []any
	if rec.Code != http.StatusOK || cborx.Unmarshal(rec.Body.Bytes(), &value) != nil || len(value) != 13 {
		t.Fatalf("GET bot = %d %x, want the 13-field installation", rec.Code, rec.Body.Bytes())
	}
	return value
}

func TestBotMusicSettingsNeedManageCommunityAndThePrototypeValues(t *testing.T) {
	for engine, repo := range policyEngines(t) {
		t.Run(engine, func(t *testing.T) {
			f := newBotSettingsFixture(t, repo)
			value := f.humanInstallation(t)
			// An installation from before 00050: no restart asked, no version, today's defaults.
			if value[8] != uint64(0) || value[9] != uint64(0) || value[10] != nil || value[11] != uint64(100) || value[12] != uint64(30) {
				t.Fatalf("defaults = %v, want 0 0 null 100 30", value[8:])
			}
			path := "/v1/communities/" + f.cid.String() + "/bot/music-settings"
			for _, tc := range []struct {
				name  string
				token string
				body  []any
				want  int
			}{
				{"member", f.memberToken, []any{f.installation, uint64(50), uint64(15)}, http.StatusForbidden},
				{"moderator without manage_community", f.modToken, []any{f.installation, uint64(50), uint64(15)}, http.StatusForbidden},
				{"volume off the options", f.ownerToken, []any{f.installation, uint64(60), uint64(15)}, http.StatusBadRequest},
				{"volume above 255", f.ownerToken, []any{f.installation, uint64(256 + 50), uint64(15)}, http.StatusBadRequest},
				{"idle off the options", f.ownerToken, []any{f.installation, uint64(50), uint64(10)}, http.StatusBadRequest},
				{"another installation", f.ownerToken, []any{id.New(), uint64(50), uint64(15)}, http.StatusConflict},
			} {
				if rec := putCBORAuth(f.h, path, mustCBOR(t, tc.body), tc.token); rec.Code != tc.want {
					t.Fatalf("%s: PUT music settings = %d (%x), want %d", tc.name, rec.Code, rec.Body.Bytes(), tc.want)
				}
			}
			if v := f.humanInstallation(t); v[11] != uint64(100) || v[12] != uint64(30) {
				t.Fatalf("a refused PUT changed the settings: %v", v[11:])
			}
			rec := putCBORAuth(f.h, path, mustCBOR(t, []any{f.installation, uint64(50), uint64(15)}), f.ownerToken)
			var got []any
			if rec.Code != http.StatusOK || cborx.Unmarshal(rec.Body.Bytes(), &got) != nil || len(got) != 13 || got[11] != uint64(50) || got[12] != uint64(15) {
				t.Fatalf("owner PUT = %d %v", rec.Code, got)
			}
			// The runtime sees them in the widened list.
			list := doBotCBOR(f.h, http.MethodGet, "/v1/bot-runtime/installations?v=2", nil, "Bearer "+f.runtimeToken)
			var rows [][]any
			if list.Code != http.StatusOK || cborx.Unmarshal(list.Body.Bytes(), &rows) != nil || len(rows) != 1 || rows[0][11] != uint64(50) || rows[0][12] != uint64(15) {
				t.Fatalf("runtime list = %d %v", list.Code, rows)
			}
		})
	}
}

func TestBotRestartIsManagerOnlyCoalescedAuditedAndPickedUpByTheRuntime(t *testing.T) {
	for engine, repo := range policyEngines(t) {
		t.Run(engine, func(t *testing.T) {
			f := newBotSettingsFixture(t, repo)
			ctx := context.Background()
			restart := func(token string, installation id.ID) (int, []any) {
				t.Helper()
				rec := postCBORAuth(f.h, "/v1/communities/"+f.cid.String()+"/bot/restart", mustCBOR(t, []any{installation}), token)
				var out []any
				_ = cborx.Unmarshal(rec.Body.Bytes(), &out)
				return rec.Code, out
			}
			for name, token := range map[string]string{"member": f.memberToken, "moderator": f.modToken} {
				if code, _ := restart(token, f.installation); code != http.StatusForbidden {
					t.Fatalf("%s restart = %d, want 403", name, code)
				}
			}
			if code, _ := restart(f.ownerToken, id.New()); code != http.StatusConflict {
				t.Fatalf("restart of another installation = %d, want 409", code)
			}
			// A double click: two requests, one restart.
			for i := range 2 {
				code, out := restart(f.ownerToken, f.installation)
				if code != http.StatusOK || len(out) != 2 || out[0] != uint64(1) || out[1] != uint64(0) {
					t.Fatalf("restart %d = %d %v, want [1 0]", i, code, out)
				}
			}
			audits, err := repo.ListAudit(ctx, 0, 1000)
			if err != nil {
				t.Fatal(err)
			}
			restarts := 0
			for _, a := range audits {
				if a.Action == "bot.restart" && a.Target == f.installation.String() {
					restarts++
				}
			}
			if restarts != 1 {
				t.Fatalf("bot.restart audit rows = %d, want 1 for a coalesced double request", restarts)
			}
			// The runtime sees the request, restarts and reports back with its version.
			list := doBotCBOR(f.h, http.MethodGet, "/v1/bot-runtime/installations?v=2", nil, "Bearer "+f.runtimeToken)
			var rows [][]any
			if list.Code != http.StatusOK || cborx.Unmarshal(list.Body.Bytes(), &rows) != nil || len(rows) != 1 || rows[0][8] != uint64(1) || rows[0][9] != uint64(0) {
				t.Fatalf("runtime list = %d %v", list.Code, rows)
			}
			status := "/v1/bot-runtime/installations/" + f.installation.String() + "/status"
			for _, tc := range []struct {
				name, token string
				body        []any
				want        int
			}{
				{"another runtime", f.otherTok, []any{"0.4.2", uint64(1)}, http.StatusNotFound},
				{"empty version", f.runtimeToken, []any{"", uint64(1)}, http.StatusBadRequest},
				{"version with a space", f.runtimeToken, []any{"0.4 2", uint64(1)}, http.StatusBadRequest},
				{"version over 32 bytes", f.runtimeToken, []any{"0123456789012345678901234567890123", uint64(1)}, http.StatusBadRequest},
				{"human bearer", f.ownerToken, []any{"0.4.2", uint64(1)}, http.StatusUnauthorized},
			} {
				if rec := doBotCBOR(f.h, http.MethodPut, status, mustCBOR(t, tc.body), "Bearer "+tc.token); rec.Code != tc.want {
					t.Fatalf("%s: PUT status = %d (%x), want %d", tc.name, rec.Code, rec.Body.Bytes(), tc.want)
				}
			}
			// A runtime cannot claim to have handled more than was asked.
			if rec := doBotCBOR(f.h, http.MethodPut, status, mustCBOR(t, []any{"0.4.2", uint64(9)}), "Bearer "+f.runtimeToken); rec.Code != http.StatusNoContent {
				t.Fatalf("PUT status = %d (%x)", rec.Code, rec.Body.Bytes())
			}
			v := f.humanInstallation(t)
			if v[8] != uint64(1) || v[9] != uint64(1) || v[10] != "0.4.2" {
				t.Fatalf("after pickup = %v, want requested 1 handled 1 version 0.4.2", v[8:11])
			}
			// Handled never goes backwards; a later request is a new restart.
			if rec := doBotCBOR(f.h, http.MethodPut, status, mustCBOR(t, []any{"0.4.3", uint64(0)}), "Bearer "+f.runtimeToken); rec.Code != http.StatusNoContent {
				t.Fatalf("PUT status = %d", rec.Code)
			}
			if code, out := restart(f.ownerToken, f.installation); code != http.StatusOK || out[0] != uint64(2) || out[1] != uint64(1) {
				t.Fatalf("second restart = %d %v, want [2 1]", code, out)
			}
			if v := f.humanInstallation(t); v[9] != uint64(1) || v[10] != "0.4.3" {
				t.Fatalf("status = %v", v[8:11])
			}
			// A disabled installation has no runtime to restart.
			row, err := repo.GetBotInstallation(ctx, f.installation)
			if err != nil {
				t.Fatal(err)
			}
			row.State, row.Revision = store.BotInstallationDisabled, row.Revision+1
			if err := repo.UpdateBotInstallation(ctx, f.installation, row.Revision-1, row); err != nil {
				t.Fatal(err)
			}
			if code, _ := restart(f.ownerToken, f.installation); code != http.StatusConflict {
				t.Fatalf("restart of a disabled installation = %d, want 409", code)
			}
		})
	}
}

// An older runtime does not ask for v=2: it gets the eight fields it parses, with only the original
// capability byte, so bits 8 and 9 cannot trip its range checks. It never reports a version, and
// the manager's read shows none.
func TestOlderRuntimesSeeTheOriginalInstallationShape(t *testing.T) {
	f := newBotSettingsFixture(t, newEnv(t).Repo)
	list := doBotCBOR(f.h, http.MethodGet, "/v1/bot-runtime/installations", nil, "Bearer "+f.runtimeToken)
	var rows [][]any
	if list.Code != http.StatusOK || cborx.Unmarshal(list.Body.Bytes(), &rows) != nil || len(rows) != 1 || len(rows[0]) != 8 || rows[0][6] != uint64(1) {
		t.Fatalf("v1 runtime list = %d %v, want 8 fields and capabilities 1", list.Code, rows)
	}
	widened := doBotCBOR(f.h, http.MethodGet, "/v1/bot-runtime/installations?v=2", nil, "Bearer "+f.runtimeToken)
	if widened.Code != http.StatusOK || cborx.Unmarshal(widened.Body.Bytes(), &rows) != nil || len(rows[0]) != 13 || rows[0][6] != uint64(1|1<<8|1<<9) {
		t.Fatalf("v2 runtime list = %d %v, want 13 fields and capabilities 0x301", widened.Code, rows)
	}
	if v := f.humanInstallation(t); v[10] != nil || v[6] != uint64(1|1<<8|1<<9) {
		t.Fatalf("manager read = %v", v)
	}
}

// Capability bits 8 (polls) and 9 (reminders) are known to the configure route; bit 10 is not.
func TestConfigureBotKnowsPollsAndRemindersBits(t *testing.T) {
	f := newBotSettingsFixture(t, newEnv(t).Repo)
	path := "/v1/communities/" + f.cid.String() + "/bot?v=2"
	revision := uint64(1)
	if rec := putCBORAuth(f.h, path, mustCBOR(t, []any{id.New(), revision, uint64(1 << 10), ""}), f.ownerToken); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown bit 10 = %d (%x), want 400", rec.Code, rec.Body.Bytes())
	}
	if rec := putCBORAuth(f.h, path, mustCBOR(t, []any{id.New(), revision, uint64(1 << 8), ""}), f.memberToken); rec.Code != http.StatusForbidden {
		t.Fatalf("member switching Polls = %d, want 403", rec.Code)
	}
	// A legacy-shaped request (no ?v=2) cannot name bits 8 and 9 at all.
	legacy := "/v1/communities/" + f.cid.String() + "/bot"
	if rec := putCBORAuth(f.h, legacy, mustCBOR(t, []any{id.New(), revision, uint64(1 << 8), ""}), f.ownerToken); rec.Code != http.StatusBadRequest {
		t.Fatalf("legacy request naming Polls = %d, want 400", rec.Code)
	}
}
