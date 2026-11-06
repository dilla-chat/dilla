package api_test

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func TestMountedBotStructuralEventsPageAndAckAreInstallationScoped(t *testing.T) {
	for engine, repo := range policyEngines(t) {
		t.Run(engine, func(t *testing.T) { testMountedBotStructuralEventsPageAndAck(t, repo) })
	}
}

func testMountedBotStructuralEventsPageAndAck(t *testing.T, repo store.Repository) {
	h := newGroupsAPIWithRepository(t, repo, func(store.Repository) ds.Channels { return openChannels{} })
	ctx := context.Background()
	community, installation, token := seedReadyStructuralEventBot(t, h)
	now := uint64(h.deps.Clock.Now().Unix())
	for i := uint64(0); i < 2; i++ {
		appendStructuralHTTPEvent(t, h, protocol.Event{ID: id.New(), CommunityID: community, Timestamp: now + i,
			Kind: protocol.EventMemberJoin, Data: []any{id.New(), id.New()}})
	}
	path := "/v1/communities/" + community.String() + "/bot/events"
	first := h.do(t, http.MethodGet, path+"?after=0&limit=1", token, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("first Bot events page = %d %x, want 200", first.Code, first.Body.Bytes())
	}
	var page []any
	if err := cborx.Unmarshal(first.Body.Bytes(), &page); err != nil || len(page) != 4 {
		t.Fatalf("first Bot events page is not [events,next,high,floor]: %v %#v", err, page)
	}
	events, ok := page[0].([]any)
	if !ok || len(events) != 1 {
		t.Fatalf("first Bot events page rows = %#v, want one", page[0])
	}
	event, ok := events[0].([]any)
	if !ok || len(event) != 7 || event[0] != uint64(1) || event[2] != uint64(1) {
		t.Fatalf("first event row = %#v, want direct seven-field protocol tuple at sequence 1", events[0])
	}
	if page[1] != uint64(1) || page[2] != uint64(2) || page[3] != uint64(0) {
		t.Fatalf("first page cursors = %#v, want next=1 high_water=2 floor=0", page[1:])
	}
	ack := h.do(t, http.MethodPut, path+"/ack", token, mustCBOR(t, []any{uint64(1)}))
	if ack.Code != http.StatusNoContent {
		t.Fatalf("Bot event ack = %d %x, want 204", ack.Code, ack.Body.Bytes())
	}
	// Acknowledgement records consumer progress; it must not rewrite an explicit
	// cursor or hide an expired cursor from a recovering client.
	replay := h.do(t, http.MethodGet, path+"?after=0", token, nil)
	if replay.Code != http.StatusOK {
		t.Fatalf("explicit replay after ack = %d %x", replay.Code, replay.Body.Bytes())
	}
	var replayPage []any
	if err := cborx.Unmarshal(replay.Body.Bytes(), &replayPage); err != nil {
		t.Fatal(err)
	}
	if replayEvents, ok := replayPage[0].([]any); !ok || len(replayEvents) != 2 {
		t.Fatalf("explicit replay after ack = %#v, want both events", replayPage)
	}
	second := h.do(t, http.MethodGet, path+"?after=1", token, nil)
	if second.Code != http.StatusOK {
		t.Fatalf("Bot events after durable ack = %d %x, want 200", second.Code, second.Body.Bytes())
	}
	page = nil
	if err := cborx.Unmarshal(second.Body.Bytes(), &page); err != nil || len(page) != 4 {
		t.Fatalf("second Bot events page decode = %v %#v", err, page)
	}
	events, ok = page[0].([]any)
	if !ok || len(events) != 1 || page[1] != uint64(2) || page[2] != uint64(2) {
		t.Fatalf("second page after ack = %#v, want only sequence 2", page)
	}
	if got, err := h.deps.Repo.GetBotStructuralEventAck(ctx, installation, community); err != nil || got != 1 {
		t.Fatalf("stored installation ack = %d, %v; want 1", got, err)
	}
	for _, through := range []uint64{0, 3} {
		badAck := h.do(t, http.MethodPut, path+"/ack", token, mustCBOR(t, []any{through}))
		if badAck.Code != http.StatusBadRequest {
			t.Errorf("non-monotone/future Bot event ack %d = %d, want 400", through, badAck.Code)
		}
	}
	// A human enrolled session cannot use the same community's Bot feed.
	denied := h.do(t, http.MethodGet, path, h.session, nil)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("human session read Bot events = %d, want 403", denied.Code)
	}
}

func TestMountedBotStructuralEventsExpiredCursorReturnsFloorAndHighWater(t *testing.T) {
	for engine, repo := range policyEngines(t) {
		t.Run(engine, func(t *testing.T) { testMountedBotStructuralEventsExpiredCursor(t, repo) })
	}
}

func testMountedBotStructuralEventsExpiredCursor(t *testing.T, repo store.Repository) {
	h := newGroupsAPIWithRepository(t, repo, func(store.Repository) ds.Channels { return openChannels{} })
	community, _, token := seedReadyStructuralEventBot(t, h)
	now := h.deps.Clock.Now().Unix()
	appendStructuralHTTPEvent(t, h, protocol.Event{ID: id.New(), CommunityID: community, Timestamp: uint64(now - int64(31*24*time.Hour/time.Second)),
		Kind: protocol.EventMemberJoin, Data: []any{id.New(), id.New()}})
	if err := h.deps.Repo.Tx(context.Background(), func(tx store.Repository) error {
		_, err := tx.PruneBotStructuralEvents(context.Background(), now-int64(30*24*time.Hour/time.Second))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	path := "/v1/communities/" + community.String() + "/bot/events?after=0"
	got := h.do(t, http.MethodGet, path, token, nil)
	if got.Code != http.StatusGone {
		t.Fatalf("expired Bot event cursor = %d %x, want 410 E_PRUNED", got.Code, got.Body.Bytes())
	}
	var errBody []any
	if err := cborx.Unmarshal(got.Body.Bytes(), &errBody); err != nil || len(errBody) != 5 || errBody[0] != "E_PRUNED" || errBody[3] != uint64(1) || errBody[4] != uint64(1) {
		t.Fatalf("expired cursor error = %v %#v, want E_PRUNED with floor=1 high_water=1", err, errBody)
	}
	// Floor is the greatest removed sequence: a cursor equal to it asks for the
	// first retained event and does not have a gap.
	retained := h.do(t, http.MethodGet, "/v1/communities/"+community.String()+"/bot/events?after=1", token, nil)
	if retained.Code != http.StatusOK {
		t.Fatalf("cursor at pruned floor = %d %x, want 200", retained.Code, retained.Body.Bytes())
	}
}

func TestMountedBotStructuralEventsRejectMalformedQueriesAndAckCBOR(t *testing.T) {
	h := newGroupsAPI(t)
	community, _, token := seedReadyStructuralEventBot(t, h)
	path := "/v1/communities/" + community.String() + "/bot/events"
	for _, query := range []string{"?after=-1", "?after=1&after=2", "?limit=251", "?unknown=1"} {
		got := h.do(t, http.MethodGet, path+query, token, nil)
		if got.Code != http.StatusBadRequest {
			t.Errorf("query %q status = %d, want 400", query, got.Code)
		}
	}
	bad := h.do(t, http.MethodPut, path+"/ack", token, []byte{0x9f, 0xff})
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("malformed Bot event ack CBOR = %d, want 400", bad.Code)
	}
	wrongCommunity := h.do(t, http.MethodGet, "/v1/communities/"+id.New().String()+"/bot/events", token, nil)
	if wrongCommunity.Code != http.StatusForbidden {
		t.Fatalf("Bot feed for another community = %d, want scoped 403", wrongCommunity.Code)
	}
}

func TestMountedBotStructuralEventsRequireWireSix(t *testing.T) {
	h := newGroupsAPI(t)
	community, _, token := seedReadyStructuralEventBot(t, h)
	// A session negotiated before wire 6 (only the store can still write one).
	if err := h.deps.Repo.SetSessionVersions(context.Background(), auth.TokenHash(token), 2, 1, 1); err != nil {
		t.Fatal(err)
	}
	path := "/v1/communities/" + community.String() + "/bot/events"
	got := h.do(t, http.MethodGet, path, token, nil)
	if got.Code != http.StatusUpgradeRequired || errorCode(t, got) != "E_UPGRADE_REQUIRED" {
		t.Fatalf("wire-2 Bot events read = %d %x, want 426 E_UPGRADE_REQUIRED", got.Code, got.Body.Bytes())
	}
}

func appendStructuralHTTPEvent(t *testing.T, h *groupsAPI, event protocol.Event) {
	t.Helper()
	err := h.deps.Repo.Tx(context.Background(), func(tx store.Repository) error {
		_, err := tx.AppendBotStructuralEvent(context.Background(), event)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func seedReadyStructuralEventBot(t *testing.T, h *groupsAPI) (id.ID, id.ID, string) {
	t.Helper()
	ctx := context.Background()
	now := h.deps.Clock.Now().Unix()
	owner, _, _ := seedAPIDevice(t, h.deps)
	community := id.New()
	if err := h.deps.Repo.CreateCommunity(ctx, store.CommunityRow{ID: community, Owner: owner.ID, Name: "bot-structural-events", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: now}); err != nil {
		t.Fatal(err)
	}
	botID, deviceID, installationID, runtimeID := id.New(), id.New(), id.New(), id.New()
	verified := now
	if err := h.deps.Repo.CreateUser(ctx, store.UserRow{ID: botID, Username: "bot" + botID.String()[:8], Display: "Bot", Kind: 1, UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := h.deps.Repo.CreateDevice(ctx, store.DeviceRow{ID: deviceID, UserID: botID, DSKPub: make([]byte, 32), CredentialBlob: []byte{1}, VerifiedAt: &verified, Created: now, LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	if err := h.deps.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: community, UserID: botID, Joined: now}); err != nil {
		t.Fatal(err)
	}
	if err := h.deps.Repo.CreateBotRuntime(ctx, store.BotRuntimeRow{ID: runtimeID, TokenHash: bytes.Repeat([]byte{0x31}, 32), Created: now, Rotated: now}); err != nil {
		t.Fatal(err)
	}
	if err := h.deps.Repo.CreateBotInstallation(ctx, store.BotInstallationRow{ID: installationID, CommunityID: community, RuntimeID: &runtimeID, UserID: &botID, DeviceID: &deviceID,
		State: store.BotInstallationReady, Revision: 1, ApprovedBy: owner.ID, ApprovedAt: now, CredentialVerifiedAt: &verified, Created: now, Updated: now}); err != nil {
		t.Fatal(err)
	}
	token := "bot-structural-events-" + installationID.String()
	if err := h.deps.Repo.CreateSession(ctx, store.SessionRow{TokenHash: auth.TokenHash(token), DeviceID: deviceID, UserID: botID, Scope: uint8(auth.ScopeEnrolled), Created: now, Expires: now + 3600, IdleExpires: now + 3600}); err != nil {
		t.Fatal(err)
	}
	if err := h.deps.Repo.CreateBotSessionBinding(ctx, store.BotSessionBindingRow{TokenHash: auth.TokenHash(token), InstallationID: installationID, UserID: botID, DeviceID: deviceID, Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := h.deps.Sessions.RecordCapabilities(ctx, auth.TokenHash(token), 6, 1, 1); err != nil {
		t.Fatal(err)
	}
	return community, installationID, token
}
