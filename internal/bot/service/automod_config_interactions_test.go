package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/moderation"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

type automodConfigAuthorityFake struct {
	authority client.ActorAuthority
	present   bool
	err       error
	calls     int
	actor     id.ID
	community id.ID
}

type automodGrantRedeemerFake struct {
	accepted bool
	err      error
	calls    int
	binding  AutoModerationGrantBinding
	token    string
}

type automodConfigRuntimeFake struct{ persists int }

func (r *automodConfigRuntimeFake) PersistConfig(_ context.Context, _ moderation.Config, _ uint64, persist func() error) error {
	r.persists++
	return persist()
}

func (g *automodGrantRedeemerFake) RedeemAutoModerationGrant(_ context.Context, token string, binding AutoModerationGrantBinding) (bool, error) {
	g.calls++
	g.binding, g.token = binding, token
	return g.accepted, g.err
}

func (a *automodConfigAuthorityFake) BotActorAuthority(_ context.Context, community, actor id.ID) (client.ActorAuthority, bool, error) {
	a.calls++
	a.actor, a.community = actor, community
	return a.authority, a.present, a.err
}

type automodConfigNativeFake struct {
	binding      native.Binding
	state        native.StateValue
	transactions map[id.ID]automodConfigTransaction
	writes       int
	reads        int
}

type automodConfigTransaction struct {
	change   native.StateChange
	revision uint64
}

func (n *automodConfigNativeFake) Binding() native.Binding { return n.binding }
func (n *automodConfigNativeFake) StateGet(context.Context, uint64, []byte) (native.StateValue, error) {
	n.reads++
	return n.state, nil
}
func (n *automodConfigNativeFake) StateTransaction(_ context.Context, tx id.ID, changes []native.StateChange) ([]uint64, error) {
	if prior, ok := n.transactions[tx]; ok {
		if len(changes) != 1 || changes[0].Area != prior.change.Area || changes[0].ExpectedRevision != prior.change.ExpectedRevision || !bytes.Equal(changes[0].Value, prior.change.Value) || !bytes.Equal(changes[0].Key, prior.change.Key) {
			return nil, errors.New("transaction ID reused with changed payload")
		}
		return []uint64{prior.revision}, nil
	}
	if len(changes) != 1 || changes[0].ExpectedRevision != n.state.Revision {
		return nil, moderation.ErrConfigConflict
	}
	n.writes++
	n.state = native.StateValue{Revision: n.state.Revision + 1, Value: append([]byte(nil), changes[0].Value...)}
	if n.transactions == nil {
		n.transactions = make(map[id.ID]automodConfigTransaction)
	}
	n.transactions[tx] = automodConfigTransaction{change: changes[0], revision: n.state.Revision}
	return []uint64{n.state.Revision}, nil
}

func automodConfigFixture() (AutoModerationConfigInteractions, *automodConfigAuthorityFake, *automodConfigNativeFake, InteractionActor) {
	community := id.New()
	n := &automodConfigNativeFake{binding: native.Binding{Instance: id.New(), Community: community, Installation: id.New()}}
	authority := &automodConfigAuthorityFake{present: true, authority: client.ActorAuthority{EffectiveBits: manageCommunityBit}}
	h := AutoModerationConfigInteractions{Authority: authority, Store: moderation.NewStore(n), GrantRedeemer: &automodGrantRedeemerFake{accepted: true}, Runtime: &automodConfigRuntimeFake{}, CommunityID: community}
	return h, authority, n, InteractionActor{User: id.New(), Device: id.New(), Kind: 0, Group: id.New()}
}

func automodConfigDocument(t *testing.T, enabled bool, rate, duplicate, mentions moderation.Action, exemptions ...string) []byte {
	t.Helper()
	users := make([]any, len(exemptions))
	for i, user := range exemptions {
		users[i] = user
	}
	var on uint64
	if enabled {
		on = 1
	}
	body, err := cborx.Marshal([]any{uint64(1), on, string(rate), string(duplicate), string(mentions), uint64(600000), users})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func automodConfigPutRequest(community, requestID id.ID, revision uint64, document []byte) protocol.Request {
	expires := uint64(time.Now().Unix() + 20)
	return protocol.Request{RequestID: requestID, CommunityID: community, Operation: protocol.OpAutoModerationPut, Revision: &revision, Expires: &expires, Args: []any{"step-up-grant", document}}
}

func TestAutoModerationConfigGetRequiresCurrentManagerAndReturnsEncryptedStateRevision(t *testing.T) {
	h, authority, n, actor := automodConfigFixture()
	get := h.Handlers()[protocol.OpAutoModerationGet]
	request := protocol.Request{CommunityID: h.CommunityID, Operation: protocol.OpAutoModerationGet}
	result, err := get.Handle(context.Background(), actor, request)
	if err != nil || result.Status != 0 || result.Revision == nil || *result.Revision != 0 || authority.calls != 1 {
		t.Fatalf("get result=%+v calls=%d err=%v", result, authority.calls, err)
	}
	var fields []any
	if err := cborx.Unmarshal(result.Data, &fields); err != nil || len(fields) != 7 || fields[1] != uint64(0) {
		t.Fatalf("default config=%#v err=%v", fields, err)
	}
	if authority.actor != actor.User || authority.community != h.CommunityID {
		t.Fatalf("authority lookup actor=%v community=%v", authority.actor, authority.community)
	}
	authority.authority.EffectiveBits = 0
	reads := n.reads
	result, err = get.Handle(context.Background(), actor, request)
	if err != nil || result.Status != interactionStatusForbidden || n.reads != reads {
		t.Fatalf("revoked read=%+v reads=%d/%d err=%v", result, n.reads, reads, err)
	}
}

func TestAutoModerationConfigPutEnforcesActionPermissionsAndRevisionCAS(t *testing.T) {
	h, authority, n, actor := automodConfigFixture()
	doc := automodConfigDocument(t, true, moderation.ActionRemove, moderation.ActionWarn, moderation.ActionLog, "user-7")
	put := h.Handlers()[protocol.OpAutoModerationPut].Handle
	request := automodConfigPutRequest(h.CommunityID, id.New(), 0, doc)
	result, err := put(context.Background(), actor, request)
	if err != nil || result.Status != interactionStatusForbidden || n.writes != 0 || n.state.Revision != 0 || h.GrantRedeemer.(*automodGrantRedeemerFake).calls != 0 {
		t.Fatalf("unauthorized action write=%+v writes=%d state=%+v grant calls=%d err=%v", result, n.writes, n.state, h.GrantRedeemer.(*automodGrantRedeemerFake).calls, err)
	}
	authority.authority.EffectiveBits |= autoModerationManageMessagesBit | autoModerationModerateMembersBit
	result, err = put(context.Background(), actor, request)
	if err != nil || result.Status != 0 || result.Revision == nil || *result.Revision != 1 || n.writes != 1 {
		t.Fatalf("authorized put=%+v writes=%d err=%v", result, n.writes, err)
	}
	stale := automodConfigPutRequest(h.CommunityID, id.New(), 0, doc)
	result, err = put(context.Background(), actor, stale)
	if err != nil || result.Status != interactionStatusConflict || result.Revision == nil || *result.Revision != 1 || n.writes != 1 {
		t.Fatalf("stale put=%+v writes=%d err=%v", result, n.writes, err)
	}
}

func TestAutoModerationConfigPutFailsClosedWhenRuntimeUnavailable(t *testing.T) {
	h, _, n, actor := automodConfigFixture()
	h.Runtime = nil
	request := automodConfigPutRequest(h.CommunityID, id.New(), 0,
		automodConfigDocument(t, false, moderation.ActionLog, moderation.ActionLog, moderation.ActionLog))
	grant := h.GrantRedeemer.(*automodGrantRedeemerFake)
	result, err := h.Handlers()[protocol.OpAutoModerationPut].Handle(context.Background(), actor, request)
	if err != nil || result.Status != interactionStatusUnavailable || n.writes != 0 || grant.calls != 0 {
		t.Fatalf("PUT without active runtime result=%+v writes=%d grant calls=%d err=%v", result, n.writes, grant.calls, err)
	}
}

func TestAutoModerationConfigPutFailsClosedForTypedNilRuntime(t *testing.T) {
	h, _, n, actor := automodConfigFixture()
	var runtime *AutoModerationRuntime
	h.Runtime = runtime
	request := automodConfigPutRequest(h.CommunityID, id.New(), 0,
		automodConfigDocument(t, false, moderation.ActionLog, moderation.ActionLog, moderation.ActionLog))
	grant := h.GrantRedeemer.(*automodGrantRedeemerFake)
	result, err := h.Handlers()[protocol.OpAutoModerationPut].Handle(context.Background(), actor, request)
	if err != nil || result.Status != interactionStatusUnavailable || n.writes != 0 || grant.calls != 0 {
		t.Fatalf("PUT with typed-nil runtime result=%+v writes=%d grant calls=%d err=%v", result, n.writes, grant.calls, err)
	}
}

func TestAutoModerationConfigPutUsesDistinctRemovalAndWarningPermissions(t *testing.T) {
	for _, tc := range []struct {
		name        string
		action      moderation.Action
		requiredBit uint64
		otherBit    uint64
	}{
		{name: "remove requires manage_messages", action: moderation.ActionRemove, requiredBit: autoModerationManageMessagesBit, otherBit: autoModerationModerateMembersBit},
		{name: "warn requires moderate_members", action: moderation.ActionWarn, requiredBit: autoModerationModerateMembersBit, otherBit: autoModerationManageMessagesBit},
		{name: "timeout requires moderate_members", action: moderation.ActionTimeout, requiredBit: autoModerationModerateMembersBit, otherBit: autoModerationManageMessagesBit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, authority, n, actor := automodConfigFixture()
			put := h.Handlers()[protocol.OpAutoModerationPut].Handle
			doc := automodConfigDocument(t, true, tc.action, moderation.ActionLog, moderation.ActionLog)
			request := automodConfigPutRequest(h.CommunityID, id.New(), 0, doc)
			authority.authority.EffectiveBits |= tc.otherBit
			result, err := put(context.Background(), actor, request)
			if err != nil || result.Status != interactionStatusForbidden || n.writes != 0 {
				t.Fatalf("other action permission accepted write=%+v writes=%d err=%v", result, n.writes, err)
			}
			authority.authority.EffectiveBits &^= tc.otherBit
			authority.authority.EffectiveBits |= tc.requiredBit
			result, err = put(context.Background(), actor, request)
			if err != nil || result.Status != 0 || n.writes != 1 {
				t.Fatalf("required action permission denied write=%+v writes=%d err=%v", result, n.writes, err)
			}
		})
	}
}

func TestAutoModerationConfigPutRejectsCrossCommunityOversizedAndPermissionLoss(t *testing.T) {
	h, authority, n, actor := automodConfigFixture()
	put := h.Handlers()[protocol.OpAutoModerationPut].Handle
	doc := automodConfigDocument(t, true, moderation.ActionWarn, moderation.ActionLog, moderation.ActionLog)
	request := automodConfigPutRequest(h.CommunityID, id.New(), 0, doc)
	foreign := request
	foreign.CommunityID = id.New()
	result, err := put(context.Background(), actor, foreign)
	if err != nil || result.Status != interactionStatusForbidden || authority.calls != 0 || n.reads != 0 || n.writes != 0 {
		t.Fatalf("cross-community put=%+v auth=%d reads=%d writes=%d err=%v", result, authority.calls, n.reads, n.writes, err)
	}
	oversized := request
	oversized.Args = []any{"step-up-grant", make([]byte, autoModerationConfigMaxBytes+1)}
	result, err = put(context.Background(), actor, oversized)
	if err != nil || result.Status != interactionStatusUnavailable || n.writes != 0 {
		t.Fatalf("oversized put=%+v writes=%d err=%v", result, n.writes, err)
	}
	authority.authority.EffectiveBits |= autoModerationModerateMembersBit
	result, err = put(context.Background(), actor, request)
	if err != nil || result.Status != 0 || n.state.Revision != 1 {
		t.Fatalf("initial put=%+v state=%+v err=%v", result, n.state, err)
	}
	authority.authority.EffectiveBits &^= autoModerationModerateMembersBit
	next := automodConfigPutRequest(h.CommunityID, id.New(), 1, doc)
	result, err = put(context.Background(), actor, next)
	if err != nil || result.Status != interactionStatusForbidden || n.writes != 1 || n.state.Revision != 1 {
		t.Fatalf("write after permission loss=%+v writes=%d state=%+v err=%v", result, n.writes, n.state, err)
	}
}

func TestAutoModerationConfigPutRequiresExactStepUpGrant(t *testing.T) {
	h, authority, n, actor := automodConfigFixture()
	authority.authority.EffectiveBits |= autoModerationModerateMembersBit
	grant := h.GrantRedeemer.(*automodGrantRedeemerFake)
	grant.accepted = false
	put := h.Handlers()[protocol.OpAutoModerationPut].Handle
	request := automodConfigPutRequest(h.CommunityID, id.New(), 0, automodConfigDocument(t, true, moderation.ActionLog, moderation.ActionLog, moderation.ActionWarn))
	result, err := put(context.Background(), actor, request)
	if err != nil || result.Status != interactionStatusForbidden || grant.calls != 1 || n.writes != 0 {
		t.Fatalf("denied grant result=%+v grant calls=%d writes=%d err=%v", result, grant.calls, n.writes, err)
	}
	grant.accepted = true
	result, err = put(context.Background(), actor, request)
	wantHash := sha256.Sum256(request.Args[1].([]byte))
	if err != nil || result.Status != 0 || grant.calls != 2 || n.writes != 1 || grant.binding.ActorUser != actor.User || grant.binding.ActorDevice != actor.Device || grant.binding.Group != actor.Group || grant.binding.RequestID != request.RequestID || grant.binding.CommunityID != request.CommunityID || grant.binding.Operation != protocol.OpAutoModerationPut || grant.binding.ExpectedRevision != 0 || grant.binding.RequestExpires != *request.Expires || grant.binding.ConfigHash != wantHash || grant.token != "step-up-grant" {
		t.Fatalf("accepted grant result=%+v grant=%+v writes=%d err=%v", result, grant, n.writes, err)
	}
	h.GrantRedeemer = nil
	missing := automodConfigPutRequest(h.CommunityID, id.New(), 1, automodConfigDocument(t, true, moderation.ActionWarn, moderation.ActionLog, moderation.ActionLog))
	result, err = h.Handlers()[protocol.OpAutoModerationPut].Handle(context.Background(), actor, missing)
	if err != nil || result.Status != interactionStatusUnavailable || n.writes != 1 {
		t.Fatalf("missing grant dependency=%+v writes=%d err=%v", result, n.writes, err)
	}
}

func TestAutoModerationConfigPutExactReplayUsesRequestIDReceipt(t *testing.T) {
	h, authority, n, actor := automodConfigFixture()
	authority.authority.EffectiveBits |= autoModerationModerateMembersBit
	put := h.Handlers()[protocol.OpAutoModerationPut].Handle
	request := automodConfigPutRequest(h.CommunityID, id.New(), 0, automodConfigDocument(t, true, moderation.ActionLog, moderation.ActionLog, moderation.ActionWarn))
	first, err := put(context.Background(), actor, request)
	if err != nil || first.Status != 0 {
		t.Fatalf("first put=%+v err=%v", first, err)
	}
	replay, err := put(context.Background(), actor, request)
	if err != nil || replay.Status != 0 || replay.Revision == nil || *replay.Revision != 1 || n.writes != 1 || n.state.Revision != 1 {
		t.Fatalf("replay=%+v writes=%d state=%+v err=%v", replay, n.writes, n.state, err)
	}
}

func TestAutoModerationConfigPutActivatesNewRevisionBeforeReturning(t *testing.T) {
	h, authority, _, actor := automodConfigFixture()
	authority.authority.EffectiveBits |= autoModerationModerateMembersBit
	now := time.Unix(1_800_000_000, 0)
	var punitive int
	runtime := NewAutoModerationRuntime(moderation.Config{}, automodAuthorityFunc(ordinaryAutomodAuthority), automodSinkFunc(func(_ context.Context, action ModerationAction) error {
		if action.Decision.Action == moderation.ActionWarn || action.Decision.Action == moderation.ActionTimeout || action.Decision.Action == moderation.ActionRemove {
			punitive++
		}
		return nil
	}), func() time.Time { return now })
	h.Runtime = runtime
	put := h.Handlers()[protocol.OpAutoModerationPut].Handle
	request := automodConfigPutRequest(h.CommunityID, id.New(), 0, automodConfigDocument(t, true, moderation.ActionLog, moderation.ActionLog, moderation.ActionWarn))
	result, err := put(context.Background(), actor, request)
	if err != nil || result.Status != 0 || result.Revision == nil || *result.Revision != 1 {
		t.Fatalf("enabled config put=%+v err=%v", result, err)
	}
	body := "test"
	event := native.GroupEvent{DeliveryID: 1, Community: h.CommunityID, Group: actor.Group, Channel: id.New(), SourceSequence: 1,
		ReceivedAt: uint64(now.Unix()), SenderUser: id.New(), SenderDevice: id.New(), EnvelopeType: 0, Body: &body, Message: id.New()}
	decision, err := runtime.Handle(context.Background(), event, false, []string{"1", "2", "3", "4", "5", "6"})
	if err != nil || decision.Action != moderation.ActionWarn || punitive != 1 {
		t.Fatalf("new revision not active on return: decision=%+v punitive=%d err=%v", decision, punitive, err)
	}
	disabled := automodConfigPutRequest(h.CommunityID, id.New(), 1, automodConfigDocument(t, false, moderation.ActionLog, moderation.ActionLog, moderation.ActionLog))
	result, err = put(context.Background(), actor, disabled)
	if err != nil || result.Status != 0 || result.Revision == nil || *result.Revision != 2 {
		t.Fatalf("disable config put=%+v err=%v", result, err)
	}
	event.DeliveryID++
	event.SourceSequence++
	event.Message = id.New()
	decision, err = runtime.Handle(context.Background(), event, false, []string{"1", "2", "3", "4", "5", "6"})
	if err != nil || decision.Action != moderation.ActionLog || punitive != 1 {
		t.Fatalf("disable not active on return: decision=%+v punitive=%d err=%v", decision, punitive, err)
	}
}
