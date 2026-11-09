package service

import (
	"bytes"
	"context"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

type overviewAuthorityFake struct {
	community, actor id.ID
	result           client.ActorAuthority
	present          bool
	calls            int
}

func (f *overviewAuthorityFake) BotActorAuthority(_ context.Context, community, actor id.ID) (client.ActorAuthority, bool, error) {
	f.calls++
	f.community, f.actor = community, actor
	return f.result, f.present, nil
}

func TestOverviewInteractionReturnsOnlyCurrentAuthorizedActorAuthority(t *testing.T) {
	community, actorID := interactionID(2), interactionID(6)
	authority := &overviewAuthorityFake{result: client.ActorAuthority{EffectiveBits: manageCommunityBit}, present: true}
	handler := OverviewInteractions{Authority: authority, CommunityID: community}.Handlers()[protocol.OpOverviewGet].Handle
	result, err := handler(context.Background(), InteractionActor{User: actorID, Kind: 0}, protocol.Request{CommunityID: community})
	if err != nil || result.Status != 0 {
		t.Fatalf("overview = %+v, %v; want success", result, err)
	}
	if authority.calls != 1 || authority.community != community || authority.actor != actorID {
		t.Fatalf("authority lookup = calls:%d community:%s actor:%s", authority.calls, authority.community, authority.actor)
	}
	var data []any
	if err := cborx.Unmarshal(result.Data, &data); err != nil || len(data) != 5 || len(result.Data) > overviewInteractionDataLimit {
		t.Fatalf("overview payload = %x, decoded=%v, err=%v", result.Data, data, err)
	}
	actorBytes, ok := data[1].([]byte)
	if data[0] != uint64(1) || !ok || !bytes.Equal(actorBytes, actorID[:]) || data[2] != manageCommunityBit || data[3] != uint64(0) || data[4] != uint64(0) {
		t.Fatalf("overview payload fields = %#v", data)
	}
}

func TestOverviewInteractionAdvertisesProviderStatusOnlyWhenEnabled(t *testing.T) {
	community, actorID := interactionID(2), interactionID(6)
	authority := &overviewAuthorityFake{result: client.ActorAuthority{Owner: true}, present: true}
	handler := OverviewInteractions{Authority: authority, CommunityID: community,
		SupportsMusicProviderStatus: true}.Handlers()[protocol.OpOverviewGet].Handle
	result, err := handler(context.Background(), InteractionActor{User: actorID, Kind: 0}, protocol.Request{CommunityID: community})
	if err != nil || result.Status != 0 {
		t.Fatalf("overview = %+v, %v; want success", result, err)
	}
	var data []any
	if err := cborx.Unmarshal(result.Data, &data); err != nil || len(data) != 6 || len(result.Data) > overviewInteractionDataLimit {
		t.Fatalf("overview payload = %x, decoded=%v, err=%v", result.Data, data, err)
	}
	ops, ok := data[5].([]any)
	if data[0] != uint64(2) || !ok || len(ops) != 1 || ops[0] != uint64(protocol.OpMusicProvidersStatus) {
		t.Fatalf("overview capability payload = %#v; want version 2 with operation 24", data)
	}
}

func TestOverviewInteractionHonorsDelegatedManagerRevocation(t *testing.T) {
	community, actorID := interactionID(2), interactionID(6)
	authority := &overviewAuthorityFake{result: client.ActorAuthority{EffectiveBits: manageCommunityBit}, present: true}
	handler := OverviewInteractions{Authority: authority, CommunityID: community}.Handlers()[protocol.OpOverviewGet].Handle
	actor := InteractionActor{User: actorID, Kind: 0}
	request := protocol.Request{CommunityID: community}
	granted, err := handler(context.Background(), actor, request)
	if err != nil || granted.Status != 0 {
		t.Fatalf("delegated manager result = %+v, %v; want success", granted, err)
	}
	// A fresh server lookup after the delegated role is removed must stop access.
	authority.result = client.ActorAuthority{EffectiveBits: 0}
	revoked, err := handler(context.Background(), actor, request)
	if err != nil || revoked.Status != interactionStatusForbidden || len(revoked.Data) != 0 {
		t.Fatalf("revoked manager result = %+v, %v; want empty forbidden response", revoked, err)
	}
	if authority.calls != 2 {
		t.Fatalf("authority lookups = %d, want fresh lookup for both requests", authority.calls)
	}
}

func TestOverviewInteractionRefusesNonAdminAndForeignCommunityWithoutSideEffects(t *testing.T) {
	community := interactionID(2)
	authority := &overviewAuthorityFake{result: client.ActorAuthority{EffectiveBits: 4}, present: true}
	handler := OverviewInteractions{Authority: authority, CommunityID: community}.Handlers()[protocol.OpOverviewGet].Handle
	result, err := handler(context.Background(), InteractionActor{User: interactionID(6), Kind: 0}, protocol.Request{CommunityID: community})
	if err != nil || result.Status != interactionStatusForbidden || len(result.Data) != 0 {
		t.Fatalf("non-admin result = %+v, %v; want empty forbidden response", result, err)
	}
	if authority.calls != 1 {
		t.Fatalf("authority calls = %d, want one current server lookup", authority.calls)
	}
	result, err = handler(context.Background(), InteractionActor{User: interactionID(6), Kind: 0}, protocol.Request{CommunityID: interactionID(90)})
	if err == nil || result.Status != 0 || authority.calls != 1 {
		t.Fatalf("foreign community result=%+v err=%v calls=%d; want refusal before authority lookup", result, err, authority.calls)
	}
	result, err = handler(context.Background(), InteractionActor{User: interactionID(6), Kind: 0}, protocol.Request{CommunityID: community})
	if err != nil || result.Status != interactionStatusForbidden || len(result.Data) != 0 || authority.calls != 2 {
		t.Fatalf("missing current member result=%+v err=%v calls=%d; want empty forbidden response", result, err, authority.calls)
	}
}

func TestOverviewInteractionRejectsBotActor(t *testing.T) {
	authority := &overviewAuthorityFake{result: client.ActorAuthority{Administrator: true}, present: true}
	handler := OverviewInteractions{Authority: authority, CommunityID: interactionID(2)}.Handlers()[protocol.OpOverviewGet].Handle
	if _, err := handler(context.Background(), InteractionActor{User: interactionID(6), Kind: 1}, protocol.Request{CommunityID: interactionID(2)}); err == nil || authority.calls != 0 {
		t.Fatalf("bot actor error=%v authority calls=%d; want refusal before lookup", err, authority.calls)
	}
}
