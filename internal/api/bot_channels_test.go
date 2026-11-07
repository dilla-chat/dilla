package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func deleteAuthCBOR(h http.Handler, path, token string) *httptest.ResponseRecorder {
	return doBotCBOR(h, http.MethodDelete, path, nil, "Bearer "+token)
}

func (f *catalogFixture) botChannelPath(channel id.ID) string {
	return "/v1/communities/" + f.community.String() + "/channels/" + channel.String() + "/bot-channel"
}

func (f *catalogFixture) listBotChannels(t *testing.T, token string) []id.ID {
	t.Helper()
	rec := getAuth(f.h, "/v1/communities/"+f.community.String()+"/bot/channels", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET bot channels = %d %x", rec.Code, rec.Body.Bytes())
	}
	var rows [][]cbor.RawMessage
	if err := cborx.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("bot channels %x: %v", rec.Body.Bytes(), err)
	}
	out := make([]id.ID, 0, len(rows))
	for _, r := range rows {
		var ch, by id.ID
		var at uint64
		if len(r) != 3 || cborx.Unmarshal(r[0], &ch) != nil || cborx.Unmarshal(r[1], &at) != nil || cborx.Unmarshal(r[2], &by) != nil {
			t.Fatalf("bot channel row %x, want [channel_id, marked_at, marked_by]", r)
		}
		out = append(out, ch)
	}
	return out
}

func TestOnlyTextChannelsCanBeBotChannels(t *testing.T) {
	f := newCatalogFixture(t)
	for name, ch := range map[string]id.ID{"voice": f.voice, "AFK": f.afk, "category": f.category, "readable": f.readable} {
		if rec := putCBORAuth(f.h, f.botChannelPath(ch), nil, f.ownerTok); rec.Code != http.StatusBadRequest {
			t.Fatalf("marking the %s channel = %d %x, want 400", name, rec.Code, rec.Body.Bytes())
		}
	}
	// A DM belongs to no community, and a channel of another community is not this one's.
	ctx := context.Background()
	now := f.deps.Clock.Now().Unix()
	dm := store.ChannelRow{ID: id.New(), Kind: api.ChannelDM, Name: "dm", SettingsJSON: []byte(`{}`), HostPolicyVersion: 1, Created: now}
	if err := f.deps.Repo.CreateChannel(ctx, dm); err != nil {
		t.Fatal(err)
	}
	if rec := putCBORAuth(f.h, f.botChannelPath(dm.ID), nil, f.ownerTok); rec.Code != http.StatusNotFound {
		t.Fatalf("marking a DM = %d, want 404", rec.Code)
	}
	// Only someone who may manage the Bot marks a channel.
	if rec := putCBORAuth(f.h, f.botChannelPath(f.text), nil, f.memberTok); rec.Code != http.StatusForbidden {
		t.Fatalf("a plain member marking = %d, want 403", rec.Code)
	}
	if rec := getAuth(f.h, "/v1/communities/"+f.community.String()+"/bot/channels", f.memberTok); rec.Code != http.StatusForbidden {
		t.Fatalf("a plain member listing = %d, want 403", rec.Code)
	}
	if got := f.listBotChannels(t, f.adminTok); len(got) != 0 {
		t.Fatalf("Bot channels after refusals = %v, want none", got)
	}
	if rec := putCBORAuth(f.h, f.botChannelPath(f.text), nil, f.adminTok); rec.Code != http.StatusNoContent {
		t.Fatalf("marking the text channel = %d %x, want 204", rec.Code, rec.Body.Bytes())
	}
	if rec := putCBORAuth(f.h, f.botChannelPath(f.text), nil, f.adminTok); rec.Code != http.StatusNoContent {
		t.Fatalf("marking it again = %d, want 204", rec.Code)
	}
	if got := f.listBotChannels(t, f.ownerTok); !slices.Equal(got, []id.ID{f.text}) {
		t.Fatalf("Bot channels = %v, want [%s]", got, f.text)
	}
	if got := f.events.list(); len(got) != 1 || got[0] != "marked "+f.community.String()+" "+f.text.String() {
		t.Fatalf("events = %v, want one mark", got)
	}
}

func TestUnmarkingNotifiesTheBotFeed(t *testing.T) {
	f := newCatalogFixture(t)
	if rec := putCBORAuth(f.h, f.botChannelPath(f.text), nil, f.ownerTok); rec.Code != http.StatusNoContent {
		t.Fatalf("mark = %d", rec.Code)
	}
	if rec := deleteAuthCBOR(f.h, f.botChannelPath(f.text), f.memberTok); rec.Code != http.StatusForbidden {
		t.Fatalf("a plain member unmarking = %d, want 403", rec.Code)
	}
	if got := f.events.list(); len(got) != 1 {
		t.Fatalf("events after a refused unmark = %v", got)
	}
	// The notification is made before the answer: by the time the DELETE returns, the Bot feed knows.
	if rec := deleteAuthCBOR(f.h, f.botChannelPath(f.text), f.ownerTok); rec.Code != http.StatusNoContent {
		t.Fatalf("unmark = %d %x", rec.Code, rec.Body.Bytes())
	}
	want := []string{"marked " + f.community.String() + " " + f.text.String(), "unmarked " + f.community.String() + " " + f.text.String()}
	if got := f.events.list(); !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if got := f.listBotChannels(t, f.ownerTok); len(got) != 0 {
		t.Fatalf("Bot channels after unmark = %v", got)
	}
	// Unmarking a channel that is not marked tells nobody.
	if rec := deleteAuthCBOR(f.h, f.botChannelPath(f.text), f.ownerTok); rec.Code != http.StatusNoContent {
		t.Fatalf("second unmark = %d", rec.Code)
	}
	if got := f.events.list(); len(got) != 2 {
		t.Fatalf("a no-op unmark notified: %v", got)
	}
}

// The api.BotChannelEvents default is a no-op, so a Deps without one wires and serves.
func TestBotChannelEventsDefaultsToNoop(t *testing.T) {
	var _ api.BotChannelEvents = api.NoBotChannelEvents{}
	h, deps := newTestAPI(t)
	owner, _, _, token := seedKeyedSession(t, deps)
	ctx := context.Background()
	now := deps.Clock.Now().Unix()
	cid := id.New()
	if err := deps.Repo.CreateCommunity(ctx, store.CommunityRow{ID: cid, Owner: owner.ID, Name: "c", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: owner.ID, Joined: now}); err != nil {
		t.Fatal(err)
	}
	ch := store.ChannelRow{ID: id.New(), CommunityID: &cid, Kind: api.ChannelText, Name: "bot", SettingsJSON: []byte(`{}`), HostPolicyVersion: 1, Created: now}
	if err := deps.Repo.CreateChannel(ctx, ch); err != nil {
		t.Fatal(err)
	}
	path := "/v1/communities/" + cid.String() + "/channels/" + ch.ID.String() + "/bot-channel"
	if rec := putCBORAuth(h, path, nil, token); rec.Code != http.StatusNoContent {
		t.Fatalf("mark = %d %x", rec.Code, rec.Body.Bytes())
	}
	if rec := deleteAuthCBOR(h, path, token); rec.Code != http.StatusNoContent {
		t.Fatalf("unmark = %d %x", rec.Code, rec.Body.Bytes())
	}
}
