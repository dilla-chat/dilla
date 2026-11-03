package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/announcements"
	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

type announcementInteractionNative struct {
	binding native.Binding
	value   native.StateValue
}

func (n *announcementInteractionNative) Binding() native.Binding { return n.binding }
func (n *announcementInteractionNative) StateGet(context.Context, uint64, []byte) (native.StateValue, error) {
	return n.value, nil
}
func (n *announcementInteractionNative) StateTransaction(_ context.Context, _ id.ID, changes []native.StateChange) ([]uint64, error) {
	if len(changes) != 1 || changes[0].ExpectedRevision != n.value.Revision {
		return nil, errors.New("state revision conflict")
	}
	n.value.Revision++
	n.value.Value = append([]byte(nil), changes[0].Value...)
	return []uint64{n.value.Revision}, nil
}

type announcementInteractionAuth struct {
	revision uint64
	allowed  bool
	user     id.ID
	calls    int
}

func (a *announcementInteractionAuth) CurrentRevision(context.Context, id.ID) (uint64, error) {
	return a.revision, nil
}
func (a *announcementInteractionAuth) CanManageCommunity(_ context.Context, user, community id.ID) (bool, error) {
	a.calls++
	a.user = user
	return a.allowed && !user.IsZero() && !community.IsZero(), nil
}

type announcementInteractionDestination struct{}

func (announcementInteractionDestination) ResolveTextGroup(context.Context, id.ID, id.ID) (id.ID, error) {
	return interactionID(44), nil
}

type announcementInteractionPostAuth bool

func (a announcementInteractionPostAuth) CanAnnounce(context.Context, id.ID) (bool, error) {
	return bool(a), nil
}

type announcementInteractionSender struct{}

func (announcementInteractionSender) SendAnnouncement(context.Context, announcements.SendRequest) (protocol.SendReceipt, error) {
	return protocol.SendReceipt{}, nil
}
func (announcementInteractionSender) LookupAnnouncement(context.Context, announcements.SendRequest) (protocol.SendReceipt, bool, error) {
	return protocol.SendReceipt{}, false, nil
}

func announcementInteractionsFixture(t *testing.T) (AnnouncementInteractions, *announcementInteractionAuth) {
	t.Helper()
	community := interactionID(2)
	n := &announcementInteractionNative{binding: native.Binding{Instance: interactionID(1), Community: community, Installation: interactionID(3)}}
	scheduler, err := announcements.NewScheduler(n, announcements.SchedulerOptions{
		Destination: announcementInteractionDestination{}, Authorization: announcementInteractionPostAuth(true),
		Sender: announcementInteractionSender{}, ReceiptLookup: announcementInteractionSender{},
		Now: func() time.Time { return time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	auth := &announcementInteractionAuth{revision: 7, allowed: true}
	return AnnouncementInteractions{Scheduler: scheduler, Authority: auth, CommunityID: community}, auth
}

func announcementPayload(t *testing.T, announcementID id.ID, text string, revision uint64) []byte {
	t.Helper()
	b, err := cborx.Marshal([]any{uint64(1), announcementID, interactionID(21), text, "daily", "UTC", "", uint64(10), uint64(0), uint64(0), uint64(1), revision})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func announcementMutationRequest(t *testing.T, requestID id.ID, operation protocol.Operation, revision uint64, args ...any) protocol.Request {
	t.Helper()
	expires := uint64(time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC).Unix() + 20)
	return protocol.Request{RequestID: requestID, CommunityID: interactionID(2), Operation: operation, Revision: &revision, Expires: &expires, Args: args}
}

func TestAnnouncementInteractionsPutUsesAuthenticatedActorAndScheduleCAS(t *testing.T) {
	h, auth := announcementInteractionsFixture(t)
	handlers := h.Handlers()
	if _, decodeErr := decodeAnnouncementValue(announcementPayload(t, interactionID(20), "private schedule text", 1)); decodeErr != nil {
		t.Fatalf("decode: %v", decodeErr)
	}
	revision := uint64(7)
	request := announcementMutationRequest(t, interactionID(31), protocol.OpAnnouncementsPut, 0, nil, announcementPayload(t, interactionID(20), "private schedule text", 1))
	result, err := handlers[protocol.OpAnnouncementsPut].Handle(context.Background(), InteractionActor{User: interactionID(6)}, request)
	if err != nil || result.Status != 0 || auth.user != interactionID(6) || result.Revision == nil || *result.Revision != 1 {
		t.Fatalf("put result=%+v auth user=%v err=%v", result, auth.user, err)
	}
	stale := announcementMutationRequest(t, interactionID(32), protocol.OpAnnouncementsPut, revision, interactionID(20), announcementPayload(t, interactionID(20), "private schedule text", 1))
	result, err = handlers[protocol.OpAnnouncementsPut].Handle(context.Background(), InteractionActor{User: interactionID(6)}, stale)
	if err != nil || result.Status != interactionStatusConflict || result.Revision == nil || *result.Revision != 2 {
		t.Fatalf("stale put result=%+v err=%v, want conflict", result, err)
	}
	if !handlers[protocol.OpAnnouncementsPut].RevisionInHandler || handlers[protocol.OpAnnouncementsPut].CurrentRevision != nil {
		t.Fatal("put handler must compare revision inside the atomic scheduler write path")
	}
}

func TestAnnouncementMutationReceiptReplayFingerprintAndRevocation(t *testing.T) {
	h, auth := announcementInteractionsFixture(t)
	put := h.Handlers()[protocol.OpAnnouncementsPut].Handle
	request := announcementMutationRequest(t, interactionID(60), protocol.OpAnnouncementsPut, 0, nil, announcementPayload(t, interactionID(20), "stable request", 1))
	actor := InteractionActor{User: interactionID(6)}
	first, err := put(context.Background(), actor, request)
	if err != nil || first.Status != 0 || first.Revision == nil || *first.Revision != 1 {
		t.Fatalf("first mutation result=%+v err=%v", first, err)
	}
	replay, err := put(context.Background(), actor, request)
	if err != nil || replay.Status != first.Status || replay.Revision == nil || *replay.Revision != *first.Revision {
		t.Fatalf("replayed mutation result=%+v err=%v, want %+v", replay, err, first)
	}
	changed := announcementMutationRequest(t, interactionID(60), protocol.OpAnnouncementsPut, 0, nil, announcementPayload(t, interactionID(22), "changed request", 1))
	conflict, err := put(context.Background(), actor, changed)
	if err != nil || conflict.Status != interactionStatusConflict || conflict.Revision != nil {
		t.Fatalf("request ID reuse result=%+v err=%v, want conflict without write", conflict, err)
	}
	rows, err := h.Scheduler.List(context.Background())
	if err != nil || len(rows) != 1 || rows[0].ID != interactionID(20) {
		t.Fatalf("request ID conflict changed schedules: rows=%+v err=%v", rows, err)
	}
	auth.allowed = false
	revoked, err := put(context.Background(), actor, request)
	if err != nil || revoked.Status != interactionStatusForbidden {
		t.Fatalf("revoked receipt replay result=%+v err=%v, want forbidden", revoked, err)
	}
}

func TestAnnouncementMutationRejectsExpiredRequest(t *testing.T) {
	h, _ := announcementInteractionsFixture(t)
	request := announcementMutationRequest(t, interactionID(61), protocol.OpAnnouncementsPut, 0, nil, announcementPayload(t, interactionID(20), "expired", 1))
	expires := uint64(time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC).Unix())
	request.Expires = &expires
	result, err := h.Handlers()[protocol.OpAnnouncementsPut].Handle(context.Background(), InteractionActor{User: interactionID(6)}, request)
	if err != nil || result.Status != interactionStatusExpired {
		t.Fatalf("expired mutation result=%+v err=%v, want expired", result, err)
	}
}

func TestAnnouncementInteractionsListIncludesTextAndPaginatesToFit(t *testing.T) {
	h, _ := announcementInteractionsFixture(t)
	put := h.Handlers()[protocol.OpAnnouncementsPut].Handle
	maxText := strings.Repeat("x", maxAnnouncementInteractionText)
	documentRevision := uint64(0)
	for i, announcementID := range []id.ID{interactionID(20), interactionID(22)} {
		request := announcementMutationRequest(t, interactionID(byte(40+i)), protocol.OpAnnouncementsPut, documentRevision, nil, announcementPayload(t, announcementID, maxText, 1))
		result, err := put(context.Background(), InteractionActor{User: interactionID(6)}, request)
		if err != nil || result.Status != 0 {
			t.Fatalf("seed put result=%+v err=%v", result, err)
		}
		documentRevision = *result.Revision
	}
	list := h.Handlers()[protocol.OpAnnouncementsList].Handle
	first, err := list(context.Background(), InteractionActor{User: interactionID(6)}, protocol.Request{CommunityID: interactionID(2), Args: []any{uint64(0)}})
	if err != nil || first.Status != 0 || first.Revision == nil || *first.Revision != 2 || len(first.Data) > announcementDataLimit {
		t.Fatalf("first list result=%+v err=%v", first, err)
	}
	var firstData []any
	if err := cborx.Unmarshal(first.Data, &firstData); err != nil || len(firstData) != 4 {
		t.Fatalf("first page data=%v err=%v", firstData, err)
	}
	page, ok := firstData[3].([]any)
	if !ok || len(page) != 1 {
		t.Fatalf("first page rows=%v, want one max-size entry within response limit", firstData[3])
	}
	row, ok := page[0].([]any)
	if !ok || len(row) != 11 || row[2] != maxText {
		t.Fatalf("first row text roundtrip failed: %v", page[0])
	}
	second, err := list(context.Background(), InteractionActor{User: interactionID(6)}, protocol.Request{CommunityID: interactionID(2), Args: []any{uint64(1)}})
	if err != nil || second.Status != 0 || len(second.Data) > announcementDataLimit {
		t.Fatalf("second list result=%+v err=%v", second, err)
	}
	var secondData []any
	if err := cborx.Unmarshal(second.Data, &secondData); err != nil || len(secondData) != 4 {
		t.Fatalf("second page data=%v err=%v", secondData, err)
	}
	secondPage, ok := secondData[3].([]any)
	if !ok || len(secondPage) != 1 || secondPage[0].([]any)[2] != maxText {
		t.Fatalf("second page did not return remaining max-size text: %v", secondData[3])
	}
}

func TestAnnouncementInteractionsReadOnlyHandlersAuthorizeCurrentManagerAndScopeCommunity(t *testing.T) {
	h, auth := announcementInteractionsFixture(t)
	handlers := h.ReadOnlyHandlers()
	if len(handlers) != 1 || handlers[protocol.OpAnnouncementsList].Handle == nil {
		t.Fatalf("read-only handlers = %v, want only announcements.list", handlers)
	}
	list := handlers[protocol.OpAnnouncementsList].Handle
	actor := InteractionActor{User: interactionID(6)}
	request := protocol.Request{CommunityID: interactionID(2), Args: []any{uint64(0)}}
	result, err := list(context.Background(), actor, request)
	if err != nil || result.Status != 0 || result.Revision == nil || *result.Revision != 0 || auth.user != actor.User || auth.calls != 1 {
		t.Fatalf("authorized list result=%+v auth user=%v calls=%d err=%v", result, auth.user, auth.calls, err)
	}
	auth.allowed = false
	result, err = list(context.Background(), actor, request)
	if err != nil || result.Status != interactionStatusForbidden || auth.calls != 2 {
		t.Fatalf("revoked manager list result=%+v calls=%d err=%v, want fresh forbidden lookup", result, auth.calls, err)
	}
	auth.allowed = true
	request.CommunityID = interactionID(3)
	result, err = list(context.Background(), actor, request)
	if err != nil || result.Status != interactionStatusForbidden {
		t.Fatalf("wrong community list result=%+v err=%v, want forbidden", result, err)
	}
}

func TestAnnouncementListDispatchRechecksPermissionOnIdenticalRetry(t *testing.T) {
	h, auth := announcementInteractionsFixture(t)
	dispatcher, event, binding := interactionFixture(t, h.ReadOnlyHandlers())
	body, err := protocol.EncodeRequest(protocol.Request{RequestID: interactionID(8), CommunityID: binding.Community, Operation: protocol.OpAnnouncementsList, Args: []any{uint64(0)}})
	if err != nil {
		t.Fatal(err)
	}
	event.Body = &body
	first, err := dispatcher.Dispatch(context.Background(), event)
	if err != nil || interactionResponse(t, first).Status != 0 {
		t.Fatalf("first response=%q err=%v; want authorized list", first, err)
	}

	auth.allowed = false
	second, err := dispatcher.Dispatch(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	response := interactionResponse(t, second)
	if response.Status != interactionStatusForbidden || len(response.Data) != 0 || auth.calls != 2 {
		t.Fatalf("revoked duplicate response=%+v lookups=%d; want fresh forbidden response", response, auth.calls)
	}
}

func TestAnnouncementListUsesFreshCommunityActorAuthority(t *testing.T) {
	h, _ := announcementInteractionsFixture(t)
	community, actor := interactionID(2), interactionID(6)
	session := &actorAuthoritySessionFake{authority: client.ActorAuthority{EffectiveBits: manageCommunityBit}, present: true}
	authority, err := NewCommunityActorAuthority(session, community, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.Authority = authority
	list := h.ReadOnlyHandlers()[protocol.OpAnnouncementsList].Handle
	request := protocol.Request{CommunityID: community, Args: []any{uint64(0)}}
	result, err := list(context.Background(), InteractionActor{User: actor}, request)
	if err != nil || result.Status != 0 || session.calls != 1 || session.actor != actor || session.community != community {
		t.Fatalf("current manager list result=%+v lookup=(%s,%s) calls=%d err=%v", result, session.actor, session.community, session.calls, err)
	}
	session.authority.EffectiveBits = 0
	result, err = list(context.Background(), InteractionActor{User: actor}, request)
	if err != nil || result.Status != interactionStatusForbidden || session.calls != 2 {
		t.Fatalf("revoked manager list result=%+v calls=%d err=%v", result, session.calls, err)
	}
}

func TestAnnouncementInteractionsRejectsMalformedPayloadAndDeletesAtScheduleRevision(t *testing.T) {
	h, auth := announcementInteractionsFixture(t)
	malformed, _ := cborx.Marshal([]any{uint64(1), interactionID(20), interactionID(21), uint64(99), "daily", "UTC", "", uint64(10), uint64(0), uint64(0), uint64(1), uint64(1)})
	result, err := h.Handlers()[protocol.OpAnnouncementsPut].Handle(context.Background(), InteractionActor{User: interactionID(6)}, announcementMutationRequest(t, interactionID(51), protocol.OpAnnouncementsPut, 0, nil, malformed))
	if err != nil || result.Status != interactionStatusUnavailable {
		t.Fatalf("malformed payload result=%+v err=%v", result, err)
	}
	put := h.Handlers()[protocol.OpAnnouncementsPut].Handle
	result, err = put(context.Background(), InteractionActor{User: interactionID(6)}, announcementMutationRequest(t, interactionID(52), protocol.OpAnnouncementsPut, 0, nil, announcementPayload(t, interactionID(20), "scheduled", 1)))
	if err != nil || result.Status != 0 {
		t.Fatalf("seed schedule result=%+v err=%v", result, err)
	}
	remove := h.Handlers()[protocol.OpAnnouncementsDelete].Handle
	result, err = remove(context.Background(), InteractionActor{User: interactionID(6)}, announcementMutationRequest(t, interactionID(53), protocol.OpAnnouncementsDelete, 1, interactionID(20), uint64(2)))
	if err != nil || result.Status != interactionStatusConflict {
		t.Fatalf("stale delete result=%+v err=%v", result, err)
	}
	result, err = remove(context.Background(), InteractionActor{User: interactionID(6)}, announcementMutationRequest(t, interactionID(54), protocol.OpAnnouncementsDelete, 2, interactionID(20), uint64(1)))
	if err != nil || result.Status != 0 {
		t.Fatalf("delete result=%+v err=%v", result, err)
	}
	auth.allowed = false
	result, err = remove(context.Background(), InteractionActor{User: interactionID(6)}, announcementMutationRequest(t, interactionID(55), protocol.OpAnnouncementsDelete, 3, interactionID(20), uint64(1)))
	if err != nil || result.Status != interactionStatusForbidden {
		t.Fatalf("unauthorized delete result=%+v err=%v", result, err)
	}
}
