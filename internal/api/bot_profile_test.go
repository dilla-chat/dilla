package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func TestCommunityManagerCanRenameOnlyItsManagedBot(t *testing.T) {
	var notified id.ID
	h, deps := newTestAPIFull(t, nil, func(d *api.Deps) {
		d.BotProfileChanged = func(_ context.Context, community id.ID) { notified = community }
	})
	owner, _, ownerToken := seedAPISession(t, deps)
	member, _, memberToken := seedAPISession(t, deps)
	cid := seedBotCommunity(t, deps, owner.ID)
	ctx := context.Background()
	now := deps.Clock.Now().Unix()
	if err := deps.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: member.ID, Joined: now}); err != nil {
		t.Fatal(err)
	}
	negotiate, _ := cborx.Marshal([]any{[]any{uint64(3)}, []any{uint64(1)}, []any{uint64(1)}})
	for _, token := range []string{ownerToken, memberToken} {
		if rec := postCBORAuth(h, "/v1/sessions/negotiate", negotiate, token); rec.Code != http.StatusOK {
			t.Fatalf("negotiate = %d", rec.Code)
		}
	}
	bot := store.UserRow{ID: id.New(), Username: "managednamebot", Display: "Dilla Bot", Kind: 1,
		UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: now}
	device := store.DeviceRow{ID: id.New(), UserID: bot.ID, DSKPub: make([]byte, 32), CredentialBlob: []byte{1}, Created: now}
	installation := store.BotInstallationRow{ID: id.New(), CommunityID: cid, UserID: &bot.ID, DeviceID: &device.ID,
		State: store.BotInstallationReady, Revision: 1, ApprovedBy: owner.ID, ApprovedAt: now, Created: now, Updated: now}
	if err := deps.Repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.CreateUser(ctx, bot); err != nil {
			return err
		}
		if err := tx.CreateBotDevice(ctx, device); err != nil {
			return err
		}
		if err := tx.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: bot.ID, Joined: now}); err != nil {
			return err
		}
		return tx.CreateBotInstallation(ctx, installation)
	}); err != nil {
		t.Fatal(err)
	}
	path := "/v1/communities/" + cid.String() + "/bot/profile"
	name, _ := cborx.Marshal([]any{"  Music Bot  "})
	if rec := doBotCBOR(h, http.MethodPatch, path, name, "Bearer "+memberToken); rec.Code != http.StatusForbidden {
		t.Fatalf("member rename = %d, want 403", rec.Code)
	}
	if !notified.IsZero() {
		t.Fatal("forbidden rename sent a refresh")
	}
	blank, _ := cborx.Marshal([]any{"   "})
	if rec := doBotCBOR(h, http.MethodPatch, path, blank, "Bearer "+ownerToken); rec.Code != http.StatusBadRequest {
		t.Fatalf("blank name = %d, want 400", rec.Code)
	}
	if !notified.IsZero() {
		t.Fatal("invalid rename sent a refresh")
	}
	if rec := doBotCBOR(h, http.MethodPatch, path, name, "Bearer "+ownerToken); rec.Code != http.StatusOK {
		t.Fatalf("owner rename = %d (%x)", rec.Code, rec.Body.Bytes())
	} else {
		var value []any
		if err := cborx.Unmarshal(rec.Body.Bytes(), &value); err != nil || len(value) != 1 || value[0] != "Music Bot" {
			t.Fatalf("rename response = %v %x", err, rec.Body.Bytes())
		}
	}
	stored, err := deps.Repo.GetUser(ctx, bot.ID)
	if err != nil || stored.Display != "Music Bot" || stored.Username != bot.Username {
		t.Fatalf("stored bot = %+v, %v", stored, err)
	}
	if notified != cid {
		t.Fatalf("refresh community = %s, want %s", notified, cid)
	}
}
