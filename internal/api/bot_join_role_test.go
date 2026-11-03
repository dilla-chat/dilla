package api_test

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func TestBotJoinRoleApprovalStoresConfigurationWithoutAssigningRoles(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	h := api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler))
	h.Register(e.Mux)
	role := createRole(t, e, cid, ownerTok, "newcomer", 2, api.PermViewChannel|api.PermSendMessages)
	community, err := e.Repo.GetCommunity(t.Context(), cid)
	if err != nil {
		t.Fatal(err)
	}
	bot := store.UserRow{ID: id.New(), Username: "joinbot" + id.New().String()[:8], Display: "Join bot", Kind: 1,
		UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: e.Clk.Now().Unix()}
	device := store.DeviceRow{ID: id.New(), UserID: bot.ID, DSKPub: make([]byte, 32), CredentialBlob: []byte{1}, Created: e.Clk.Now().Unix()}
	dsk := id.New()
	copy(device.DSKPub, dsk[:])
	installation := store.BotInstallationRow{ID: id.New(), CommunityID: cid, UserID: &bot.ID, DeviceID: &device.ID,
		State: store.BotInstallationReady, Revision: 1, ApprovedBy: community.Owner, ApprovedAt: e.Clk.Now().Unix(), Created: e.Clk.Now().Unix(), Updated: e.Clk.Now().Unix()}
	if err := e.Repo.Tx(t.Context(), func(tx store.Repository) error {
		if err := tx.CreateUser(t.Context(), bot); err != nil {
			return fmt.Errorf("create bot user: %w", err)
		}
		if err := tx.CreateBotDevice(t.Context(), device); err != nil {
			return fmt.Errorf("create bot device: %w", err)
		}
		if err := tx.PutMember(t.Context(), store.MemberOfCommunityRow{CommunityID: cid, UserID: bot.ID, Joined: e.Clk.Now().Unix()}); err != nil {
			return fmt.Errorf("add bot member: %w", err)
		}
		if err := tx.CreateBotInstallation(t.Context(), installation); err != nil {
			return fmt.Errorf("create installation: %w", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	path := "/v1/communities/" + cid.String() + "/bot-join-role"
	memberID, memberTok := e.NewUser("join-policy-reader")
	if err := e.Repo.PutMember(t.Context(), store.MemberOfCommunityRow{CommunityID: cid, UserID: memberID, Joined: e.Clk.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	if status, body := e.Do(http.MethodGet, path, memberTok, nil); status != http.StatusForbidden {
		t.Fatalf("member read approved role = %d (%x), want 403", status, body)
	}
	assertRead := func(want id.ID) {
		t.Helper()
		status, body := e.Do(http.MethodGet, path, ownerTok, nil)
		if status != http.StatusOK {
			t.Fatalf("read approved role = %d (%x), want 200", status, body)
		}
		var response []any
		if err := cborx.Unmarshal(body, &response); err != nil {
			t.Fatal(err)
		}
		if len(response) != 2 {
			t.Fatalf("read response = %#v, want two fields", response)
		}
		if want.IsZero() {
			if response[0] != nil {
				t.Fatalf("approved role = %#v, want null", response[0])
			}
		} else if got, ok := response[0].([]byte); !ok || string(got) != string(want[:]) {
			t.Fatalf("approved role = %#v, want %s", response[0], want)
		}
		if got, ok := response[1].([]byte); !ok || string(got) != string(installation.ID[:]) {
			t.Fatalf("installation = %#v, want %s", response[1], installation.ID)
		}
	}
	assertRead(id.ID{})
	if status, body := e.Do(http.MethodPut, path, ownerTok, []any{id.New(), role, id.New()}); status != http.StatusConflict {
		t.Fatalf("stale installation approval = %d (%x), want 409", status, body)
	}
	if status, body := e.Do(http.MethodPut, path, ownerTok, []any{id.New(), role, installation.ID}); status != http.StatusOK {
		t.Fatalf("configure approved role = %d (%x), want 200", status, body)
	}
	assertRead(role)
	policy, err := e.Repo.(store.BotJoinRolePolicy).GetBotJoinRolePolicy(t.Context(), installation.ID)
	if err != nil || policy.RoleID != role || policy.ApprovedBy != community.Owner {
		t.Fatalf("policy = %+v, %v", policy, err)
	}
	if held, err := e.Repo.ListMemberRoles(t.Context(), cid, bot.ID); err != nil || len(held) != 0 {
		t.Fatalf("configuration assigned a role immediately: %v, %v", held, err)
	}
	if status, body := e.Do(http.MethodPut, path, ownerTok, []any{id.New(), id.ID{}, installation.ID}); status != http.StatusOK {
		t.Fatalf("clear approved role = %d (%x), want 200", status, body)
	}
	if _, err := e.Repo.(store.BotJoinRolePolicy).GetBotJoinRolePolicy(t.Context(), installation.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("policy after clear = %v, want not found", err)
	}
	assertRead(id.ID{})
}

func TestBotJoinRoleReadRequiresReadyInstallation(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	path := "/v1/communities/" + cid.String() + "/bot-join-role"
	if status, body := e.Do(http.MethodGet, path, ownerTok, nil); status != http.StatusNotFound {
		t.Fatalf("read without installation = %d (%x), want 404", status, body)
	}
}
