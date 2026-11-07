package service

import (
	"context"
	"encoding/base64"
	"errors"
	"sync"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/catalog"
	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/intake"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

func interactionID(n byte) id.ID { var v id.ID; v[15] = n; return v }

type interactionNativeFake struct {
	binding        native.Binding
	group          native.GroupInfo
	groups         map[id.ID]native.GroupInfo
	err            error
	interaction    native.InteractionGroupInspectResult
	interactionErr error
}

func (f *interactionNativeFake) Binding() native.Binding { return f.binding }
func (f *interactionNativeFake) InspectGroup(_ context.Context, group id.ID) (native.GroupInfo, error) {
	if info, ok := f.groups[group]; ok {
		return info, f.err
	}
	return f.group, f.err
}
func (f *interactionNativeFake) InspectInteractionGroup(context.Context, native.InteractionGroupExpected, []byte) (native.InteractionGroupInspectResult, error) {
	return f.interaction, f.interactionErr
}

type interactionSnapshotSourceFunc func(context.Context, native.GroupEvent) (native.InteractionGroupExpected, []byte, error)

func (f interactionSnapshotSourceFunc) InteractionSnapshot(ctx context.Context, event native.GroupEvent) (native.InteractionGroupExpected, []byte, error) {
	return f(ctx, event)
}

func interactionFixture(t *testing.T, handlers map[protocol.Operation]InteractionHandlerSpec) (*InteractionDispatcher, native.GroupEvent, native.Binding) {
	t.Helper()
	b := native.Binding{Instance: interactionID(1), Community: interactionID(2), Installation: interactionID(3)}
	group := interactionID(4)
	channel := interactionID(5)
	n := &interactionNativeFake{binding: b, group: native.GroupInfo{ID: group, Kind: 0, Community: &b.Community, Target: channel, State: 2}}
	d, err := newInteractionDispatcher(n, handlers, func() uint64 { return 100 })
	if err != nil {
		t.Fatal(err)
	}
	return d, native.GroupEvent{DeliveryID: 1, Group: group, Community: b.Community, Channel: channel, SourceSequence: 1, SenderUser: interactionID(6), SenderDevice: interactionID(7), SenderKind: 0, SenderTier: 0, EnvelopeType: 0}, b
}

func interactionBody(t *testing.T, actor *id.ID, requestID id.ID, op protocol.Operation, revision, expires *uint64, args []any) string {
	t.Helper()
	if actor != nil {
		args = append([]any{*actor}, args...)
	}
	body, err := protocol.EncodeRequest(protocol.Request{RequestID: requestID, CommunityID: interactionID(2), Operation: op, Revision: revision, Expires: expires, Args: args})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func interactionResponse(t *testing.T, body string) protocol.Response {
	t.Helper()
	r, err := protocol.DecodeResponse(body)
	if err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return r
}

func TestInteractionDispatcherRejectsActorInjectedIntoApplicationBody(t *testing.T) {
	d, event, _ := interactionFixture(t, nil)
	forged := interactionID(99)
	raw, err := cborx.Marshal([]any{uint64(1), interactionID(8), interactionID(2), nil, uint64(protocol.OpOverviewGet), nil, nil, []any{}, forged})
	if err != nil {
		t.Fatal(err)
	}
	body := protocol.BodyPrefix + base64.RawURLEncoding.EncodeToString(raw)
	event.Body = &body
	if _, err := d.Dispatch(context.Background(), event); !errors.Is(err, protocol.ErrInvalid) {
		t.Fatalf("Dispatch error = %v, want protocol invalid for actor field in request", err)
	}
}

func TestInteractionDispatcherRefusesForeignCommunity(t *testing.T) {
	d, event, binding := interactionFixture(t, nil)
	body, err := protocol.EncodeRequest(protocol.Request{RequestID: interactionID(8), CommunityID: interactionID(90), Operation: protocol.OpOverviewGet, Args: []any{}})
	if err != nil {
		t.Fatal(err)
	}
	event.Body = &body
	got, err := d.Dispatch(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	response := interactionResponse(t, got)
	if response.Status != 2 || response.CommunityID != binding.Community {
		t.Fatalf("response = %+v, want forbidden in bound community", response)
	}
}

func TestInteractionDispatcherChecksNativeGroupAndChannelContext(t *testing.T) {
	d, event, _ := interactionFixture(t, nil)
	body := interactionBody(t, nil, interactionID(8), protocol.OpOverviewGet, nil, nil, nil)
	event.Body = &body
	event.Channel = interactionID(55)
	if _, err := d.Dispatch(context.Background(), event); !errors.Is(err, protocol.ErrContext) {
		t.Fatalf("mismatched event group/channel error = %v, want context refusal", err)
	}
}

func TestInteractionDispatcherRefusesUnboundInteractionGroup(t *testing.T) {
	d, event, binding := interactionFixture(t, nil)
	body := interactionBody(t, nil, interactionID(8), protocol.OpOverviewGet, nil, nil, nil)
	event.Body = &body
	// The native client has no typed installation/participant binding for kind 3
	// yet. A matching channel and community in a Go event cannot supply it.
	d.native = &interactionNativeFake{binding: binding, group: native.GroupInfo{
		ID: event.Group, Kind: 3, Community: &binding.Community, Target: event.Channel, State: 2,
	}}
	if _, err := d.Dispatch(context.Background(), event); !errors.Is(err, protocol.ErrContext) {
		t.Fatalf("unbound interaction group error = %v, want context refusal", err)
	}
}

func TestInteractionDispatcherAcceptsKind3OnlyAfterSnapshotMatchesNativeParticipant(t *testing.T) {
	called := false
	d, event, binding := interactionFixture(t, map[protocol.Operation]InteractionHandlerSpec{
		protocol.OpOverviewGet: {Handle: func(_ context.Context, actor InteractionActor, _ protocol.Request) (InteractionResult, error) {
			called = true
			if actor.User != interactionID(6) || actor.Device != interactionID(7) || actor.Tier != 1 {
				t.Errorf("actor = %+v, want authenticated browser-tier participant", actor)
			}
			return InteractionResult{Status: 0}, nil
		}},
	})
	body := interactionBody(t, nil, interactionID(8), protocol.OpOverviewGet, nil, nil, nil)
	event.Body = &body
	event.SenderTier = 1
	participant := interactionID(6)
	device := interactionID(7)
	event.Channel = participant
	n := d.native.(*interactionNativeFake)
	n.group = native.GroupInfo{ID: event.Group, Kind: 3, Community: &binding.Community, Target: event.Channel, State: 2}
	expected := native.InteractionGroupExpected{Instance: binding.Instance, Installation: binding.Installation, Community: binding.Community, BotUser: interactionID(10), BotDevice: interactionID(11), ParticipantUser: participant, ParticipantDevice: device, Group: event.Group, PolicyVersion: 3, ServerNow: 100}
	source := interactionSnapshotSourceFunc(func(context.Context, native.GroupEvent) (native.InteractionGroupExpected, []byte, error) {
		return expected, []byte{0x81}, nil
	})
	if _, err := d.Dispatch(context.Background(), event); !errors.Is(err, protocol.ErrContext) {
		t.Fatalf("kind-3 dispatch without configured trusted source = %v, want refusal", err)
	}
	d, err := newInteractionDispatcherWithSnapshots(d.native, d.handlers, d.now, source)
	if err != nil {
		t.Fatal(err)
	}
	n.interaction = native.InteractionGroupInspectResult{Binding: native.InteractionGroupBinding{Instance: binding.Instance, Installation: binding.Installation, Community: binding.Community, BotUser: expected.BotUser, ParticipantUser: participant, ParticipantDevice: device, PolicyVersion: 3, E2EEVersion: 1, MediaVersion: 0}, BotDevice: expected.BotDevice, Epoch: 1, State: 2, NextSequence: 1}
	// Generic inspection refuses kind 3 for the Bot helper. The dispatcher must
	// use its fresh signed interaction snapshot and native op22 inspection.
	n.err = &helper.StatusError{Status: interactionStatusForbidden}
	got, err := d.Dispatch(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if interactionResponse(t, got).Status != 0 || !called {
		t.Fatalf("kind-3 result = %s, handler called=%v", got, called)
	}
	called = false
	unknownTier := event
	unknownTier.SenderTier = 2
	if _, err := d.Dispatch(context.Background(), unknownTier); !errors.Is(err, protocol.ErrInvalid) || called {
		t.Fatalf("unknown event tier error=%v handler called=%v; want invalid event refusal", err, called)
	}
	forged := event
	forged.SenderUser = interactionID(99)
	if _, err := d.Dispatch(context.Background(), forged); !errors.Is(err, protocol.ErrContext) || called {
		t.Fatalf("forged event actor error=%v handler called=%v; want refusal before handler", err, called)
	}
	n.interaction.Binding.ParticipantDevice = interactionID(98)
	if _, err := d.Dispatch(context.Background(), event); !errors.Is(err, protocol.ErrContext) || called {
		t.Fatalf("native participant mismatch error=%v handler called=%v; want fail-closed refusal", err, called)
	}
	n.interaction.Binding.ParticipantDevice = device
	n.interactionErr = errors.New("snapshot signature rejected by native core")
	if _, err := d.Dispatch(context.Background(), event); !errors.Is(err, protocol.ErrContext) || called {
		t.Fatalf("native snapshot refusal error=%v handler called=%v; want fail-closed refusal", err, called)
	}
}

func TestInteractionDispatcherMarksExpiredMutation(t *testing.T) {
	d, event, _ := interactionFixture(t, map[protocol.Operation]InteractionHandlerSpec{
		protocol.OpMusicClear: {
			CurrentRevision: func(context.Context, protocol.Request) (uint64, error) { return 3, nil },
			Handle: func(context.Context, InteractionActor, protocol.Request) (InteractionResult, error) {
				t.Fatal("expired request reached handler")
				return InteractionResult{}, nil
			},
		},
	})
	rev, exp := uint64(3), uint64(100)
	body, err := protocol.EncodeRequest(protocol.Request{RequestID: interactionID(8), CommunityID: interactionID(2), ChannelID: idPtr(interactionID(5)), Operation: protocol.OpMusicClear, Revision: &rev, Expires: &exp, Args: []any{}})
	if err != nil {
		t.Fatal(err)
	}
	event.Body = &body
	got, err := d.Dispatch(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if response := interactionResponse(t, got); response.Status != 3 {
		t.Fatalf("status = %d, want expired", response.Status)
	}
}

func TestInteractionDispatcherReplaysDuplicateRequestAndRejectsChangedReuse(t *testing.T) {
	calls := 0
	h := InteractionHandlerSpec{RevisionInHandler: true, Handle: func(context.Context, InteractionActor, protocol.Request) (InteractionResult, error) {
		calls++
		return InteractionResult{Status: 0, Data: cborEmptyArray()}, nil
	}}
	d, event, _ := interactionFixture(t, map[protocol.Operation]InteractionHandlerSpec{protocol.OpSettingsPut: h})
	revision, expires := uint64(1), uint64(120)
	body, err := protocol.EncodeRequest(protocol.Request{RequestID: interactionID(8), CommunityID: interactionID(2), Operation: protocol.OpSettingsPut, Revision: &revision, Expires: &expires, Args: []any{uint64(0), []byte{0x80}}})
	if err != nil {
		t.Fatal(err)
	}
	event.Body = &body
	first, err := d.Dispatch(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.Dispatch(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || calls != 1 {
		t.Fatalf("duplicate response equal=%v calls=%d; want replay with one handler call", first == second, calls)
	}
	// Keep the same request id while changing the canonical request body.
	changed, err := protocol.EncodeRequest(protocol.Request{RequestID: interactionID(8), CommunityID: interactionID(2), Operation: protocol.OpSettingsGet, Args: []any{uint64(0)}})
	if err != nil {
		t.Fatal(err)
	}
	event.Body = &changed
	if _, err := d.Dispatch(context.Background(), event); !errors.Is(err, ErrInteractionRequestIDConflict) {
		t.Fatalf("changed request id reuse error = %v", err)
	}
}

func TestInteractionDispatcherRecomputesReadResponseAfterPermissionRevocation(t *testing.T) {
	allowed := true
	calls := 0
	h := InteractionHandlerSpec{RecomputeOnReplay: true, Handle: func(context.Context, InteractionActor, protocol.Request) (InteractionResult, error) {
		calls++
		if !allowed {
			return InteractionResult{}, errors.New("permission revoked")
		}
		return InteractionResult{Status: 0, Data: cborEmptyArray()}, nil
	}}
	d, event, _ := interactionFixture(t, map[protocol.Operation]InteractionHandlerSpec{protocol.OpOverviewGet: h})
	body := interactionBody(t, nil, interactionID(8), protocol.OpOverviewGet, nil, nil, nil)
	event.Body = &body
	first, err := d.Dispatch(context.Background(), event)
	if err != nil || interactionResponse(t, first).Status != 0 {
		t.Fatalf("first response=%q err=%v; want authorized read", first, err)
	}

	allowed = false
	second, err := d.Dispatch(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	response := interactionResponse(t, second)
	if response.Status != interactionStatusInternal || len(response.Data) != 0 || calls != 2 {
		t.Fatalf("revoked duplicate response=%+v calls=%d; want fresh denied response without data", response, calls)
	}
}

func TestInteractionDispatcherRefusesReadReplayAfterPermissionRevocation(t *testing.T) {
	allowed := true
	d, event, _ := interactionFixture(t, map[protocol.Operation]InteractionHandlerSpec{
		protocol.OpOverviewGet: {RecomputeOnReplay: true, Handle: func(context.Context, InteractionActor, protocol.Request) (InteractionResult, error) {
			if !allowed {
				return InteractionResult{Status: interactionStatusForbidden}, nil
			}
			return InteractionResult{Status: 0, Data: cborEmptyArray()}, nil
		}},
	})
	event.Body = ptrString(interactionBody(t, nil, interactionID(8), protocol.OpOverviewGet, nil, nil, nil))
	if err := d.ReauthorizeReadReplay(context.Background(), event); err != nil {
		t.Fatalf("authorized read replay: %v", err)
	}
	allowed = false
	if err := d.ReauthorizeReadReplay(context.Background(), event); err == nil {
		t.Fatal("read replay remained authorized after permission revocation")
	}
}

func TestInteractionDispatcherReplaysSameLogicalRequestAcrossNativeRedelivery(t *testing.T) {
	calls := 0
	d, event, _ := interactionFixture(t, map[protocol.Operation]InteractionHandlerSpec{
		protocol.OpOverviewGet: {Handle: func(context.Context, InteractionActor, protocol.Request) (InteractionResult, error) {
			calls++
			return InteractionResult{Status: 0, Data: cborEmptyArray()}, nil
		}},
	})
	body := interactionBody(t, nil, interactionID(8), protocol.OpOverviewGet, nil, nil, nil)
	event.Body = &body
	first, err := d.Dispatch(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	event.DeliveryID = 2
	event.SourceSequence = 2
	event.ReceivedAt = 101
	second, err := d.Dispatch(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || calls != 1 {
		t.Fatalf("redelivery response equal=%v calls=%d; want one logical execution", first == second, calls)
	}
}

func TestInteractionDispatcherScopesRequestIDByActorAndGroupContext(t *testing.T) {
	calls := 0
	d, event, _ := interactionFixture(t, map[protocol.Operation]InteractionHandlerSpec{
		protocol.OpOverviewGet: {Handle: func(_ context.Context, actor InteractionActor, _ protocol.Request) (InteractionResult, error) {
			calls++
			data, _ := cborx.Marshal([]any{actor.User})
			return InteractionResult{Status: 0, Data: data}, nil
		}},
	})
	body := interactionBody(t, nil, interactionID(8), protocol.OpOverviewGet, nil, nil, nil)
	event.Body = &body
	first, err := d.Dispatch(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	event.SenderUser = interactionID(60)
	second, err := d.Dispatch(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || calls != 2 {
		t.Fatalf("different actor responses equal=%v calls=%d; want independent dispatch", first == second, calls)
	}
	n := d.native.(*interactionNativeFake)
	secondGroup, secondChannel := interactionID(61), interactionID(62)
	n.groups = map[id.ID]native.GroupInfo{secondGroup: {ID: secondGroup, Kind: 0, Community: &n.binding.Community, Target: secondChannel, State: 2}}
	event.SenderUser = interactionID(6)
	event.Group, event.Channel = secondGroup, secondChannel
	_, err = d.Dispatch(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("different group handler calls=%d; want independent context-scoped dispatch", calls)
	}
}

func TestInteractionHandlerAtomicallyRejectsConcurrentStaleMutation(t *testing.T) {
	var mu sync.Mutex
	revision := uint64(5)
	var snapshots int
	bothRead := make(chan struct{})
	spec := InteractionHandlerSpec{
		CurrentRevision: func(context.Context, protocol.Request) (uint64, error) {
			mu.Lock()
			snapshot := revision
			snapshots++
			if snapshots == 2 {
				close(bothRead)
			}
			mu.Unlock()
			<-bothRead
			return snapshot, nil
		},
		Handle: func(_ context.Context, _ InteractionActor, request protocol.Request) (InteractionResult, error) {
			mu.Lock()
			defer mu.Unlock()
			if *request.Revision != revision {
				current := revision
				return InteractionResult{Status: 4, Revision: &current}, nil
			}
			revision++
			current := revision
			return InteractionResult{Status: 0, Revision: &current, Data: cborEmptyArray()}, nil
		},
	}
	d, event, binding := interactionFixture(t, map[protocol.Operation]InteractionHandlerSpec{protocol.OpSettingsPut: spec})
	rev, exp := uint64(5), uint64(120)
	request := func(requestID id.ID) string {
		body, err := protocol.EncodeRequest(protocol.Request{RequestID: requestID, CommunityID: binding.Community, Operation: protocol.OpSettingsPut, Revision: &rev, Expires: &exp, Args: []any{uint64(0), []byte{0x80}}})
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	firstEvent, secondEvent := event, event
	firstEvent.Body, secondEvent.Body = ptrString(request(interactionID(11))), ptrString(request(interactionID(12)))
	var responses [2]string
	var errs [2]error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); responses[0], errs[0] = d.Dispatch(context.Background(), firstEvent) }()
	go func() { defer wg.Done(); responses[1], errs[1] = d.Dispatch(context.Background(), secondEvent) }()
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Dispatch %d: %v", i, err)
		}
	}
	statuses := map[uint64]int{}
	for _, body := range responses {
		statuses[interactionResponse(t, body).Status]++
	}
	if statuses[0] != 1 || statuses[4] != 1 {
		t.Fatalf("statuses=%v, want one success and one atomic revision conflict", statuses)
	}
}

func TestInteractionDispatcherRejectsStaleRevision(t *testing.T) {
	calls := 0
	d, event, _ := interactionFixture(t, map[protocol.Operation]InteractionHandlerSpec{
		protocol.OpMusicClear: {
			CurrentRevision: func(context.Context, protocol.Request) (uint64, error) { return 4, nil },
			Handle: func(context.Context, InteractionActor, protocol.Request) (InteractionResult, error) {
				calls++
				return InteractionResult{Status: 0, Data: cborEmptyArray()}, nil
			},
		},
	})
	rev, exp := uint64(3), uint64(120)
	body, err := protocol.EncodeRequest(protocol.Request{RequestID: interactionID(8), CommunityID: interactionID(2), ChannelID: idPtr(interactionID(5)), Operation: protocol.OpMusicClear, Revision: &rev, Expires: &exp, Args: []any{}})
	if err != nil {
		t.Fatal(err)
	}
	event.Body = &body
	got, err := d.Dispatch(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	response := interactionResponse(t, got)
	if response.Status != 4 || response.Revision == nil || *response.Revision != 4 || calls != 0 {
		t.Fatalf("response=%+v handler calls=%d", response, calls)
	}
}

func TestInteractionDispatcherBoundsResponsePayload(t *testing.T) {
	d, event, _ := interactionFixture(t, map[protocol.Operation]InteractionHandlerSpec{
		protocol.OpOverviewGet: {Handle: func(context.Context, InteractionActor, protocol.Request) (InteractionResult, error) {
			data, _ := cborx.Marshal([]any{string(make([]byte, 2401))})
			return InteractionResult{Status: 0, Data: data}, nil
		}},
	})
	body := interactionBody(t, nil, interactionID(8), protocol.OpOverviewGet, nil, nil, nil)
	event.Body = &body
	got, err := d.Dispatch(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if response := interactionResponse(t, got); response.Status != 9 || len(response.Data) != 0 {
		t.Fatalf("response status=%d data bytes=%d, want bounded internal response", response.Status, len(response.Data))
	}
}

func TestInteractionDispatcherPassesOnlyNativeActorToHandler(t *testing.T) {
	want := InteractionActor{User: interactionID(6), Device: interactionID(7), Kind: 0, Tier: 0, Group: interactionID(4), Channel: interactionID(5)}
	called := false
	d, event, _ := interactionFixture(t, map[protocol.Operation]InteractionHandlerSpec{
		protocol.OpOverviewGet: {Handle: func(_ context.Context, actor InteractionActor, _ protocol.Request) (InteractionResult, error) {
			called = true
			if actor != want {
				t.Errorf("handler actor = %+v, want native event actor %+v", actor, want)
			}
			return InteractionResult{Status: 0, Data: cborEmptyArray()}, nil
		}},
	})
	body := interactionBody(t, nil, interactionID(8), protocol.OpOverviewGet, nil, nil, nil)
	event.Body = &body
	if _, err := d.Dispatch(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("handler was not called")
	}
}

func TestInteractionDispatcherUsesAuthenticatedEventActorAndUnavailableHandlers(t *testing.T) {
	d, event, binding := interactionFixture(t, nil)
	body := interactionBody(t, nil, interactionID(8), protocol.OpOverviewGet, nil, nil, nil)
	event.Body = &body
	got, err := d.Dispatch(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	response := interactionResponse(t, got)
	if response.Status != 5 || response.CommunityID != binding.Community {
		t.Fatalf("unregistered overview response = %+v, want unavailable", response)
	}
}

func TestInteractionDispatcherReportsClearUnavailable(t *testing.T) {
	d, event, binding := interactionFixture(t, nil)
	rev, exp := uint64(0), uint64(120)
	body, err := protocol.EncodeRequest(protocol.Request{RequestID: interactionID(9), CommunityID: binding.Community, ChannelID: idPtr(interactionID(5)), Operation: protocol.OpMusicClear, Revision: &rev, Expires: &exp, Args: []any{}})
	if err != nil {
		t.Fatal(err)
	}
	event.Body = &body
	got, err := d.Dispatch(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if response := interactionResponse(t, got); response.Status != 5 {
		t.Fatalf("music.clear status = %d, want unavailable", response.Status)
	}
}

func idPtr(v id.ID) *id.ID       { return &v }
func ptrString(v string) *string { return &v }
func cborEmptyArray() []byte     { b, _ := cborx.Marshal([]any{}); return b }

func TestInteractionDispatcherAllowsAuthorizedMusicVoiceContextOnlyForKind3(t *testing.T) {
	participant, device, voice := interactionID(6), interactionID(7), interactionID(15)
	called := false
	d, event, binding := interactionFixture(t, map[protocol.Operation]InteractionHandlerSpec{
		protocol.OpMusicState: {
			AllowMusicChannelContext: true,
			Handle: func(_ context.Context, actor InteractionActor, request protocol.Request) (InteractionResult, error) {
				called = true
				if actor.User != participant || actor.Device != device || request.ChannelID == nil || *request.ChannelID != voice {
					t.Errorf("actor=%+v channel=%v; want authenticated participant and requested voice channel", actor, request.ChannelID)
				}
				return InteractionResult{Status: 0}, nil
			},
		},
	})
	event.SenderTier = 1
	n := d.native.(*interactionNativeFake)
	event.Channel = participant
	n.group = native.GroupInfo{ID: event.Group, Kind: 3, Community: &binding.Community, Target: event.Channel, State: 2}
	expected := native.InteractionGroupExpected{Instance: binding.Instance, Installation: binding.Installation, Community: binding.Community, BotUser: interactionID(10), BotDevice: interactionID(11), ParticipantUser: participant, ParticipantDevice: device, Group: event.Group, PolicyVersion: 3, ServerNow: 100}
	d, err := newInteractionDispatcherWithSnapshots(n, d.handlers, d.now, interactionSnapshotSourceFunc(func(context.Context, native.GroupEvent) (native.InteractionGroupExpected, []byte, error) {
		return expected, []byte{0x81}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	n.interaction = native.InteractionGroupInspectResult{Binding: native.InteractionGroupBinding{Instance: binding.Instance, Installation: binding.Installation, Community: binding.Community, BotUser: expected.BotUser, ParticipantUser: participant, ParticipantDevice: device, PolicyVersion: 3, E2EEVersion: 1}, BotDevice: expected.BotDevice, Epoch: 1, State: 2, NextSequence: 1}
	request := protocol.Request{RequestID: interactionID(8), CommunityID: binding.Community, ChannelID: &voice, Operation: protocol.OpMusicState, Args: []any{}}
	body, err := protocol.EncodeRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	event.Body = &body
	got, err := d.Dispatch(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if response := interactionResponse(t, got); response.Status != 0 || !called {
		t.Fatalf("kind-3 Music response=%+v handler called=%v", response, called)
	}
	unauthorized, err := newInteractionDispatcherWithSnapshots(n, map[protocol.Operation]InteractionHandlerSpec{
		protocol.OpMusicState: {Handle: func(context.Context, InteractionActor, protocol.Request) (InteractionResult, error) {
			t.Fatal("kind-3 handler without voice-channel authorization reached")
			return InteractionResult{}, nil
		}}}, d.now, interactionSnapshotSourceFunc(func(context.Context, native.GroupEvent) (native.InteractionGroupExpected, []byte, error) {
		return expected, []byte{0x81}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	got, err = unauthorized.Dispatch(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if response := interactionResponse(t, got); response.Status != interactionStatusForbidden {
		t.Fatalf("kind-3 handler without channel authorization status=%d, want forbidden", response.Status)
	}

	// The same voice-channel request remains bound to the native text channel
	// for kind-0 events, even if a handler opts into Music context.
	ordinary, ordinaryEvent, _ := interactionFixture(t, map[protocol.Operation]InteractionHandlerSpec{
		protocol.OpMusicState: {AllowMusicChannelContext: true, Handle: func(context.Context, InteractionActor, protocol.Request) (InteractionResult, error) {
			t.Fatal("ordinary kind-0 channel mismatch reached handler")
			return InteractionResult{}, nil
		}},
	})
	ordinaryEvent.Body = &body
	got, err = ordinary.Dispatch(context.Background(), ordinaryEvent)
	if err != nil {
		t.Fatal(err)
	}
	if response := interactionResponse(t, got); response.Status != interactionStatusForbidden {
		t.Fatalf("kind-0 channel mismatch status=%d, want forbidden", response.Status)
	}
}

// ---- Bot v2 Task 15: sealed commands through the same feature handlers ----

type sealedCall struct {
	actor InteractionActor
	req   protocol.Request
}

type sealedHandlers struct {
	mu      sync.Mutex
	calls   []sealedCall
	results map[protocol.Operation][]InteractionResult
	err     error
}

func (s *sealedHandlers) spec(op protocol.Operation) InteractionHandlerSpec {
	return InteractionHandlerSpec{Handle: func(_ context.Context, actor InteractionActor, req protocol.Request) (InteractionResult, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.calls = append(s.calls, sealedCall{actor, req})
		if s.err != nil {
			return InteractionResult{}, s.err
		}
		if rs := s.results[op]; len(rs) > 0 {
			r := rs[0]
			s.results[op] = rs[1:]
			return r, nil
		}
		return InteractionResult{Data: []byte{0x80}}, nil
	}}
}

func sealedFixture(ops ...protocol.Operation) (*SealedDispatcher, *sealedHandlers) {
	h := &sealedHandlers{results: map[protocol.Operation][]InteractionResult{}}
	handlers := map[protocol.Operation]InteractionHandlerSpec{}
	for _, op := range ops {
		handlers[op] = h.spec(op)
	}
	return &SealedDispatcher{Community: interactionID(1), Handlers: handlers}, h
}

func sealedRequest(op protocol.Operation, args ...any) protocol.SealedRequest {
	return protocol.SealedRequest{RequestID: interactionID(9), CommunityID: interactionID(1), Operation: uint64(op), Args: args,
		SentAt: 1_800_000_000, UserID: interactionID(5), DeviceID: interactionID(6)}
}

func collect(d *SealedDispatcher, req protocol.SealedRequest) []intake.Answer {
	var out []intake.Answer
	d.Dispatch(context.Background(), req, func(a intake.Answer) { out = append(out, a) })
	return out
}

func TestSealedRemindCreatesThroughTheReminderHandler(t *testing.T) {
	d, h := sealedFixture(protocol.OpReminderCreate)
	got := collect(d, sealedRequest(protocol.OpReminderCreate, uint64(600), "stretch"))
	if len(got) != 1 || got[0].Status != 200 || !got[0].Final || string(got[0].Body) != "Saved. I’ll remind you in 10 minutes: stretch" {
		t.Fatalf("answers = %+v", got)
	}
	c := h.calls[0]
	if c.actor.User != interactionID(5) || c.actor.Device != interactionID(6) || c.actor.Kind != 0 {
		t.Fatalf("actor = %+v", c.actor)
	}
	r := c.req
	if r.RequestID != interactionID(9) || r.CommunityID != interactionID(1) || r.ChannelID != nil || r.Revision == nil || *r.Revision != 0 || r.Expires == nil || *r.Expires != 1_800_000_120 {
		t.Fatalf("v1 request = %+v", r)
	}
	var payload []any
	if err := cborx.Unmarshal(r.Args[0].([]byte), &payload); err != nil || len(payload) != 7 || payload[1] != uint64(0) || payload[2] != uint64(600) || payload[3] != "stretch" || payload[4] != "UTC" || payload[5] != nil {
		t.Fatalf("reminder payload = %#v %v", payload, err)
	}
	if bad := collect(d, sealedRequest(protocol.OpReminderCreate, uint64(600), nil)); len(bad) != 1 || bad[0].Status != 400 || len(h.calls) != 1 {
		t.Fatalf("a reminder without text = %+v", bad)
	}
}

func TestSealedReadAnswersRevisionAndData(t *testing.T) {
	d, h := sealedFixture(protocol.OpPollGet)
	rev := uint64(7)
	h.results[protocol.OpPollGet] = []InteractionResult{{Revision: &rev, Data: []byte{0x81, 0x01}}}
	poll := interactionID(44)
	got := collect(d, sealedRequest(protocol.OpReadPoll, poll[:]))
	if len(got) != 1 || got[0].Status != 200 || !got[0].Final {
		t.Fatalf("answers = %+v", got)
	}
	var body []any
	if err := cborx.Unmarshal(got[0].Body, &body); err != nil || len(body) != 2 || body[0] != uint64(7) || string(body[1].([]byte)) != "\x81\x01" {
		t.Fatalf("read body = %#v %v", body, err)
	}
	if r := h.calls[0].req; r.Operation != protocol.OpPollGet || r.Args[0] != poll || r.Revision != nil {
		t.Fatalf("v1 request = %+v", r)
	}
	// A refusal of a read is a bare code.
	h.results[protocol.OpPollGet] = []InteractionResult{{Status: 2}}
	if got := collect(d, sealedRequest(protocol.OpReadPoll, poll[:])); len(got) != 1 || got[0].Status != 403 || string(got[0].Body) != "E_FORBIDDEN" {
		t.Fatalf("refused read = %+v", got)
	}
}

func TestSealedUnknownOperationIs404(t *testing.T) {
	d, h := sealedFixture(protocol.OpReminderCreate)
	for _, op := range []protocol.Operation{999, protocol.OpPollGet} {
		got := collect(d, sealedRequest(op))
		if len(got) != 1 || got[0].Status != 404 || string(got[0].Body) != "E_BOT_UNSUPPORTED" || !got[0].Final {
			t.Fatalf("op %d = %+v", op, got)
		}
	}
	if len(h.calls) != 0 {
		t.Fatal("a handler ran")
	}
}

func TestSealedMusicMutationRetriesAConflictOnceWithTheCurrentRevision(t *testing.T) {
	d, h := sealedFixture(protocol.OpMusicVolume)
	current := uint64(12)
	h.results[protocol.OpMusicVolume] = []InteractionResult{{Status: 4, Revision: &current}, {Revision: &current, Data: []byte{0x80}}}
	req := sealedRequest(protocol.OpMusicVolume, uint64(40))
	req.ChannelID = interactionID(30)
	got := collect(d, req)
	if len(got) != 1 || got[0].Status != 200 || string(got[0].Body) != "Volume set to 40." {
		t.Fatalf("answers = %+v", got)
	}
	if len(h.calls) != 2 || *h.calls[1].req.Revision != 12 || *h.calls[1].req.ChannelID != interactionID(30) {
		t.Fatalf("calls = %+v", h.calls)
	}
	// Twice in conflict: answered, not looped.
	h.results[protocol.OpMusicVolume] = []InteractionResult{{Status: 4, Revision: &current}, {Status: 4, Revision: &current}}
	if got := collect(d, req); len(got) != 1 || got[0].Status != 409 || len(h.calls) != 4 {
		t.Fatalf("second conflict = %+v, calls %d", got, len(h.calls))
	}
	// Music needs the voice room as the channel.
	if got := collect(d, sealedRequest(protocol.OpMusicVolume, uint64(40))); len(got) != 1 || got[0].Status != 400 {
		t.Fatalf("no channel = %+v", got)
	}
}

func TestSealedPlayStreamsSearchingThenAnswer(t *testing.T) {
	d, _ := sealedFixture(protocol.OpMusicEnqueue)
	req := sealedRequest(protocol.OpMusicEnqueue, "never gonna")
	req.ChannelID = interactionID(30)
	got := collect(d, req)
	if len(got) != 2 || got[0].Final || string(got[0].Body) != "Searching…" || !got[1].Final || got[1].Status != 200 {
		t.Fatalf("answers = %+v", got)
	}
}

func TestSealedHandlerRefusalMapsStatus(t *testing.T) {
	d, h := sealedFixture(protocol.OpReminderCreate)
	for status, want := range map[uint64]uint16{1: 400, 2: 403, 3: 408, 4: 409, 5: 503, 6: 429, 7: 404, 8: 426, 9: 500} {
		h.results[protocol.OpReminderCreate] = []InteractionResult{{Status: status}}
		got := collect(d, sealedRequest(protocol.OpReminderCreate, uint64(600), "x"))
		if len(got) != 1 || got[0].Status != want || !got[0].Final {
			t.Fatalf("status %d = %+v, want %d", status, got, want)
		}
	}
	h.err = errors.New("boom")
	if got := collect(d, sealedRequest(protocol.OpReminderCreate, uint64(600), "x")); len(got) != 1 || got[0].Status != 500 {
		t.Fatalf("handler error = %+v", got)
	}
}

func TestSealedPollCreatePostsWithTheCommandChannel(t *testing.T) {
	d, h := sealedFixture(protocol.OpPollCreate)
	req := sealedRequest(protocol.OpPollCreate, "Lunch?", "Soup | Salad", uint64(1), nil, nil)
	req.ChannelID = interactionID(31)
	got := collect(d, req)
	if len(got) != 1 || got[0].Status != 200 || len(got[0].Body) != 0 {
		t.Fatalf("answers = %+v", got)
	}
	r := h.calls[0].req
	var payload []any
	if err := cborx.Unmarshal(r.Args[0].([]byte), &payload); err != nil || r.ChannelID != nil || *r.Revision != 0 {
		t.Fatalf("request = %+v %v", r, err)
	}
	ch := interactionID(31)
	if b, _ := payload[1].([]byte); string(b) != string(ch[:]) || payload[3] != "Lunch?" || len(payload[4].([]any)) != 2 || payload[5] != uint64(1) || payload[6] != uint64(86400) {
		t.Fatalf("poll payload = %#v", payload)
	}
}

func TestSealedMusicStateRendersNowPlaying(t *testing.T) {
	d, h := sealedFixture(protocol.OpMusicState)
	track := []any{"yt:x", "youtube", "x", "Song", "Band", "", uint64(1000), "u"}
	next := []any{"yt:y", "youtube", "y", "Next", "Other", "", uint64(1000), "u"}
	data, _ := cborx.Marshal([]any{uint64(1), uint64(3), track, []any{next}, uint64(0), uint64(0), uint64(50), uint64(0), uint64(0), uint64(1), uint64(0), uint64(0)})
	h.results[protocol.OpMusicState] = []InteractionResult{{Data: data}, {Data: data}}
	req := sealedRequest(protocol.OpMusicState)
	req.ChannelID = interactionID(30)
	got := collect(d, req)
	if len(got) != 1 || string(got[0].Body) != "Now playing: Song — Band\n1. Next — Other" {
		t.Fatalf("queue = %+v", got)
	}
	// The established copy (bot-handlers.ts): an empty player says so.
	empty, _ := cborx.Marshal([]any{uint64(1), uint64(3), nil, []any{}, uint64(0), uint64(0), uint64(50), uint64(0), uint64(0), uint64(1), uint64(0), uint64(0)})
	h.results[protocol.OpMusicState] = []InteractionResult{{Data: empty}}
	if got := collect(d, req); len(got) != 1 || string(got[0].Body) != "The music queue is empty." {
		t.Fatalf("empty queue = %+v", got)
	}
}

func TestEveryCatalogOperationHasASealedAdapter(t *testing.T) {
	for _, c := range catalog.Commands(catalog.AllFeatures()) {
		if !SealedOperationSupported(c.Operation) {
			t.Fatalf("catalog command %q (operation %d) has no sealed adapter", c.Name, c.Operation)
		}
		if read := c.HandlerKind == catalog.HandlerRead; read != sealedAdapters[c.Operation].read {
			t.Fatalf("catalog command %q: handler kind %d but the adapter's read is %v", c.Name, c.HandlerKind, !read)
		}
	}
}
