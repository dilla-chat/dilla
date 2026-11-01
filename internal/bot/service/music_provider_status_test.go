package service

import (
	"context"
	"testing"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

func TestMusicProviderStatusRequiresFreshViewAuditLogAndRecomputesReplay(t *testing.T) {
	community, actorID, installation := interactionID(2), interactionID(6), interactionID(7)
	authority := &overviewAuthorityFake{result: clientAuthorityWithBits(1 << 16), present: true}
	observed := uint64(100)
	calls := 0
	h := MusicProviderStatusInteractions{Authority: authority, CommunityID: community, Snapshot: func(context.Context) MusicProviderStatusSnapshot {
		calls++
		return MusicProviderStatusSnapshot{InstallationID: installation, ObservedAt: observed, SpotifyAccount: ProviderAccountLinked,
			SpotifyPlayback: ProviderConfigured, YouTubePlayback: ProviderMissingRequirements}
	}}
	handler := h.Handlers()[protocol.OpMusicProvidersStatus]
	request := protocol.Request{CommunityID: community, Args: []any{}}
	actor := InteractionActor{User: actorID, Kind: 0}
	first, err := handler.Handle(context.Background(), actor, request)
	if err != nil || first.Status != 0 {
		t.Fatalf("status = %+v, %v", first, err)
	}
	observed = 101
	second, err := handler.Handle(context.Background(), actor, request)
	if err != nil || second.Status != 0 || calls != 2 {
		t.Fatalf("recomputed status = %+v, %v; calls=%d", second, err, calls)
	}
	var data []any
	if err := cborx.Unmarshal(second.Data, &data); err != nil || len(data) != 9 || data[0] != uint64(1) || data[2] != uint64(101) {
		t.Fatalf("provider snapshot = %#v, err=%v", data, err)
	}
	authority.result = clientAuthorityWithBits(1 << 17) // viewAuditLog alone is insufficient.
	denied, err := handler.Handle(context.Background(), actor, request)
	if err != nil || denied.Status != interactionStatusForbidden || calls != 2 {
		t.Fatalf("manage-community-only response = %+v, %v; reads=%d", denied, err, calls)
	}
}

func clientAuthorityWithBits(bits uint64) client.ActorAuthority {
	return client.ActorAuthority{EffectiveBits: bits}
}

func TestMusicProviderStatusRejectsWrongSnapshotIdentityAndBoundsPayload(t *testing.T) {
	community := interactionID(2)
	h := MusicProviderStatusInteractions{Authority: &overviewAuthorityFake{result: clientAuthorityWithBits(1 << 16), present: true}, CommunityID: community,
		Snapshot: func(context.Context) MusicProviderStatusSnapshot {
			return MusicProviderStatusSnapshot{InstallationID: id.New(), ObservedAt: 1,
				SpotifyAccount: ProviderAccountUnknown, SpotifyAccountReason: ProviderReasonUnavailable, SpotifyPlayback: ProviderSetupUnknown,
				SpotifyPlaybackReason: ProviderReasonUnavailable, YouTubePlayback: ProviderSetupUnknown, YouTubePlaybackReason: ProviderReasonUnavailable}
		}}
	result, err := h.Handlers()[protocol.OpMusicProvidersStatus].Handle(context.Background(), InteractionActor{User: interactionID(6)}, protocol.Request{CommunityID: community})
	if err != nil || result.Status != 0 || len(result.Data) > musicProviderStatusDataLimit {
		t.Fatalf("unknown bounded status = %+v, %v", result, err)
	}
	var data []any
	if err := cborx.Unmarshal(result.Data, &data); err != nil || len(data) != 9 {
		t.Fatalf("status tuple=%#v err=%v", data, err)
	}
}

func TestMusicProviderStatusDispatcherRecomputesFreshReplayAndAuthorization(t *testing.T) {
	community, installation := interactionID(2), interactionID(7)
	authority := &overviewAuthorityFake{result: clientAuthorityWithBits(1 << 16), present: true}
	observed, reads := uint64(100), 0
	handler := MusicProviderStatusInteractions{Authority: authority, CommunityID: community, Snapshot: func(context.Context) MusicProviderStatusSnapshot {
		reads++
		return MusicProviderStatusSnapshot{InstallationID: installation, ObservedAt: observed,
			SpotifyAccount: ProviderAccountUnknown, SpotifyPlayback: ProviderSetupUnknown, YouTubePlayback: ProviderSetupUnknown}
	}}
	dispatcher, event, binding := interactionFixture(t, handler.Handlers())
	body, err := protocol.EncodeRequest(protocol.Request{RequestID: interactionID(8), CommunityID: binding.Community,
		Operation: protocol.OpMusicProvidersStatus, Args: []any{}})
	if err != nil {
		t.Fatal(err)
	}
	event.Body = &body
	firstBody, err := dispatcher.Dispatch(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	observed = 101
	secondBody, err := dispatcher.Dispatch(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	first, err := protocol.DecodeResponse(firstBody)
	if err != nil {
		t.Fatal(err)
	}
	second, err := protocol.DecodeResponse(secondBody)
	if err != nil {
		t.Fatal(err)
	}
	var firstData, secondData []any
	if err := cborx.Unmarshal(first.Data, &firstData); err != nil {
		t.Fatal(err)
	}
	if err := cborx.Unmarshal(second.Data, &secondData); err != nil {
		t.Fatal(err)
	}
	if reads != 2 || authority.calls != 2 || firstData[2] != uint64(100) || secondData[2] != uint64(101) {
		t.Fatalf("replay reused old snapshot: reads=%d authority=%d first=%v second=%v", reads, authority.calls, firstData, secondData)
	}
	authority.result = clientAuthorityWithBits(1 << 17)
	deniedBody, err := dispatcher.Dispatch(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	denied, err := protocol.DecodeResponse(deniedBody)
	if err != nil || denied.Status != interactionStatusForbidden || reads != 2 || authority.calls != 3 {
		t.Fatalf("revoked replay response=%+v err=%v reads=%d authority=%d", denied, err, reads, authority.calls)
	}
}
