package api_test

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

func TestBotCommandStatusIsMemberReadableAndExposesLifecycleOnly(t *testing.T) {
	h, deps := newTestAPI(t)
	ctx := context.Background()
	owner, _, _ := seedAPISession(t, deps)
	member, _, memberToken := seedAPISession(t, deps)
	now := deps.Clock.Now().Unix()
	cid := id.New()
	if err := deps.Repo.CreateCommunity(ctx, store.CommunityRow{ID: cid, Owner: owner.ID, Name: "command-status", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: now}); err != nil {
		t.Fatal(err)
	}
	for _, user := range []id.ID{owner.ID, member.ID} {
		if err := deps.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: user, Joined: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := deps.Sessions.RecordCapabilities(ctx, auth.TokenHash(memberToken), 3, 1, 1); err != nil {
		t.Fatal(err)
	}
	path := "/v1/communities/" + cid.String() + "/bot/command-status"
	noInstallation := getAuth(h, path, memberToken)
	if noInstallation.Code != http.StatusOK || noInstallation.Header().Get("Cache-Control") != "no-store" || !bytes.Equal(noInstallation.Body.Bytes(), []byte{0xf6}) {
		t.Fatalf("missing bot status = %d cache=%q body=%x, want no-store null", noInstallation.Code, noInstallation.Header().Get("Cache-Control"), noInstallation.Body.Bytes())
	}
	if denied := getAuth(h, "/v1/communities/"+id.New().String()+"/bot/command-status", memberToken); denied.Code != http.StatusNotFound {
		t.Fatalf("foreign community status = %d, want hidden 404", denied.Code)
	}
	outsider, _, outsiderToken := seedAPISession(t, deps)
	if err := deps.Sessions.RecordCapabilities(ctx, auth.TokenHash(outsiderToken), 3, 1, 1); err != nil {
		t.Fatal(err)
	}
	if denied := getAuth(h, path, outsiderToken); denied.Code != http.StatusNotFound {
		t.Fatalf("nonmember status = %d, want hidden 404 (user %s)", denied.Code, outsider.ID)
	}
	if denied := getAuth(h, "/v1/communities/"+cid.String()+"/bot", memberToken); denied.Code != http.StatusForbidden {
		t.Fatalf("ordinary member installation configuration route = %d, want unchanged manager-only 403", denied.Code)
	}
	installation := store.BotInstallationRow{ID: id.New(), CommunityID: cid, State: store.BotInstallationPending, Revision: 7,
		ApprovedBy: owner.ID, ApprovedAt: now, Created: now, Updated: now, Capabilities: 0xff}
	if err := deps.Repo.CreateBotInstallation(ctx, installation); err != nil {
		t.Fatal(err)
	}
	pending := getAuth(h, path, memberToken)
	var tuple []any
	if pending.Code != http.StatusOK || cborx.Unmarshal(pending.Body.Bytes(), &tuple) != nil || len(tuple) != 5 {
		t.Fatalf("pending status = %d %x tuple=%#v", pending.Code, pending.Body.Bytes(), tuple)
	}
	installBytes, installOK := tuple[1].([]byte)
	if tuple[0] != uint64(1) || !installOK || !bytes.Equal(installBytes, installation.ID[:]) || tuple[2] != nil || tuple[3] != uint64(store.BotInstallationPending) || tuple[4] != uint64(7) {
		t.Fatalf("pending status tuple = %#v", tuple)
	}
	bot := id.New()
	device := id.New()
	if err := deps.Repo.CreateUser(ctx, store.UserRow{ID: bot, Username: "command-status-bot", Display: "Bot", Kind: 1,
		UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.CreateDevice(ctx, store.DeviceRow{ID: device, UserID: bot, DSKPub: make([]byte, 32), CredentialBlob: []byte{1}, Created: now, LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	installation.UserID = &bot
	installation.DeviceID = &device
	installation.State = store.BotInstallationDisabled
	installation.Revision++
	installation.Capabilities = 0
	installation.Updated = now + 1
	if err := deps.Repo.Tx(ctx, func(tx store.Repository) error {
		return tx.UpdateBotInstallation(ctx, installation.ID, installation.Revision-1, installation)
	}); err != nil {
		t.Fatal(err)
	}
	disabled := getAuth(h, path, memberToken)
	if disabled.Code != http.StatusOK || cborx.Unmarshal(disabled.Body.Bytes(), &tuple) != nil || len(tuple) != 5 {
		t.Fatalf("disabled status = %d %x tuple=%#v", disabled.Code, disabled.Body.Bytes(), tuple)
	}
	botBytes, botOK := tuple[2].([]byte)
	if tuple[0] != uint64(1) || !botOK || !bytes.Equal(botBytes, bot[:]) || tuple[3] != uint64(store.BotInstallationDisabled) || tuple[4] != installation.Revision {
		t.Fatalf("disabled status tuple = %#v", tuple)
	}
	removedAt := now + 2
	installation.RemovedAt = &removedAt
	installation.Revision++
	installation.Updated = removedAt
	if err := deps.Repo.Tx(ctx, func(tx store.Repository) error {
		return tx.UpdateBotInstallation(ctx, installation.ID, installation.Revision-1, installation)
	}); err != nil {
		t.Fatal(err)
	}
	removed := getAuth(h, path, memberToken)
	if removed.Code != http.StatusOK || !bytes.Equal(removed.Body.Bytes(), []byte{0xf6}) {
		t.Fatalf("removed bot status = %d body=%x, want null", removed.Code, removed.Body.Bytes())
	}
}
