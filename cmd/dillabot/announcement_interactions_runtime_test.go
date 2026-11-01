//go:build linux

package main

import (
	"context"
	"errors"
	"testing"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/moderation"
	"github.com/jonasthim/dilla/internal/bot/music"
	"github.com/jonasthim/dilla/internal/bot/music/persistence"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/bot/service"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

func TestRuntimeInteractionHandlersRegisterAnnouncementMutationsWithInHandlerRevisionChecks(t *testing.T) {
	community := id.New()
	state := &runtimeWelcomeSettingsNative{binding: native.Binding{Instance: id.New(), Community: community, Installation: id.New()}}
	var unavailableRuntime *service.AutoModerationRuntime
	handlers := runtimeInteractionHandlers(
		service.OverviewInteractions{CommunityID: community},
		service.AnnouncementInteractions{Authority: runtimeWelcomeAuthority{}, CommunityID: community},
		service.ModerationInteractions{CommunityID: community, InstallationID: id.New(), EvidenceReader: runtimeModerationEvidenceReader{}},
		nil,
		state,
		nil,
		unavailableRuntime,
	)
	if len(handlers) != 11 {
		t.Fatalf("runtime handlers has %d operations, want overview, announcements, welcome, moderation reads, and automatic moderation settings", len(handlers))
	}
	if handlers[protocol.OpOverviewGet].Handle == nil || handlers[protocol.OpAnnouncementsList].Handle == nil || handlers[protocol.OpWelcomePreview].Handle == nil || handlers[protocol.OpModerationCases].Handle == nil || handlers[protocol.OpModerationEvidenceList].Handle == nil || handlers[protocol.OpSettingsGet].Handle == nil || handlers[protocol.OpAutoModerationGet].Handle == nil {
		t.Fatalf("runtime handlers missing read operations: %v", handlers)
	}
	if put, ok := handlers[protocol.OpSettingsPut]; !ok || put.Handle == nil || !put.RevisionInHandler || put.CurrentRevision != nil {
		t.Fatalf("runtime settings.put handler=%+v; want in-handler atomic revision check", put)
	}
	if put, ok := handlers[protocol.OpAnnouncementsPut]; !ok || put.Handle == nil || !put.RevisionInHandler || put.CurrentRevision != nil {
		t.Fatalf("runtime announcements.put handler=%+v; want in-handler atomic revision check", put)
	}
	if remove, ok := handlers[protocol.OpAnnouncementsDelete]; !ok || remove.Handle == nil || !remove.RevisionInHandler || remove.CurrentRevision != nil {
		t.Fatalf("runtime announcements.delete handler=%+v; want in-handler atomic revision check", remove)
	}
	if put, ok := handlers[protocol.OpAutoModerationPut]; !ok || put.Handle == nil || !put.RevisionInHandler || put.CurrentRevision != nil {
		t.Fatalf("runtime automod.put handler=%+v; want in-handler atomic revision check", put)
	}
	if _, ok := handlers[protocol.OpModerationEvidence]; ok {
		t.Fatal("runtime handlers registered moderation.evidence")
	}
	if _, ok := handlers[protocol.OpModerationEvidenceList]; !ok {
		t.Fatal("runtime handlers omitted moderation.evidence.list")
	}
	result, err := handlers[protocol.OpSettingsGet].Handle(context.Background(), service.InteractionActor{User: id.New()}, protocol.Request{CommunityID: community, Operation: protocol.OpSettingsGet, Args: []any{uint64(5)}})
	if err != nil || result.Status != 0 || result.Revision == nil || *result.Revision != 0 || state.reads != 1 {
		t.Fatalf("runtime welcome settings get=%+v reads=%d err=%v", result, state.reads, err)
	}
}

func TestRuntimeInteractionHandlersRegisterProviderStatusAsRecomputedRead(t *testing.T) {
	community := id.New()
	status := service.MusicProviderStatusInteractions{CommunityID: community, Authority: runtimeProviderStatusAuthority{}, Snapshot: func(context.Context) service.MusicProviderStatusSnapshot {
		return service.MusicProviderStatusSnapshot{InstallationID: id.New(), ObservedAt: 1, SpotifyAccount: service.ProviderAccountUnknown,
			SpotifyAccountReason: service.ProviderReasonUnavailable, SpotifyPlayback: service.ProviderSetupUnknown,
			SpotifyPlaybackReason: service.ProviderReasonUnavailable, YouTubePlayback: service.ProviderSetupUnknown,
			YouTubePlaybackReason: service.ProviderReasonUnavailable}
	}}
	base := runtimeInteractionHandlers(service.OverviewInteractions{CommunityID: community}, service.AnnouncementInteractions{CommunityID: community}, service.ModerationInteractions{CommunityID: community}, nil, nil, nil, nil)
	if _, ok := base[protocol.OpMusicProvidersStatus]; ok {
		t.Fatal("provider status registered without explicit runtime provider")
	}
	incomplete := runtimeInteractionHandlers(service.OverviewInteractions{Authority: runtimeProviderStatusAuthority{}, CommunityID: community},
		service.AnnouncementInteractions{CommunityID: community}, service.ModerationInteractions{CommunityID: community}, nil, nil, nil, nil,
		service.MusicProviderStatusInteractions{Authority: runtimeProviderStatusAuthority{}})
	if _, ok := incomplete[protocol.OpMusicProvidersStatus]; ok {
		t.Fatal("provider status registered without a snapshot handler")
	}
	legacy, err := incomplete[protocol.OpOverviewGet].Handle(context.Background(), service.InteractionActor{User: id.New()}, protocol.Request{CommunityID: community})
	if err != nil || legacy.Status != 0 {
		t.Fatalf("overview with incomplete provider status = %+v, %v", legacy, err)
	}
	var legacyPayload []any
	if err := cborx.Unmarshal(legacy.Data, &legacyPayload); err != nil || len(legacyPayload) != 5 || legacyPayload[0] != uint64(1) {
		t.Fatalf("incomplete provider overview capability = %#v, err=%v; want legacy version 1", legacyPayload, err)
	}
	registered := runtimeInteractionHandlers(service.OverviewInteractions{Authority: runtimeProviderStatusAuthority{}, CommunityID: community}, service.AnnouncementInteractions{CommunityID: community}, service.ModerationInteractions{CommunityID: community}, nil, nil, nil, nil, status)
	handler, ok := registered[protocol.OpMusicProvidersStatus]
	if !ok || handler.Handle == nil || !handler.RecomputeOnReplay {
		t.Fatalf("provider status handler = %+v, present=%v", handler, ok)
	}
	overview := registered[protocol.OpOverviewGet]
	result, err := overview.Handle(context.Background(), service.InteractionActor{User: id.New()}, protocol.Request{CommunityID: community})
	if err != nil || result.Status != 0 {
		t.Fatalf("overview with provider status = %+v, %v", result, err)
	}
	var payload []any
	if err := cborx.Unmarshal(result.Data, &payload); err != nil || len(payload) != 6 || payload[0] != uint64(2) {
		t.Fatalf("provider status overview capability = %#v, err=%v; want version 2", payload, err)
	}
}

type runtimeProviderStatusAuthority struct{}

func (runtimeProviderStatusAuthority) BotActorAuthority(context.Context, id.ID, id.ID) (client.ActorAuthority, bool, error) {
	return client.ActorAuthority{Owner: true}, true, nil
}

type runtimeModerationEvidenceReader struct{}

func (runtimeModerationEvidenceReader) List(context.Context, id.ID, []byte, uint64) ([]moderation.Evidence, []byte, error) {
	return nil, nil, nil
}

type runtimeManualEvidenceWriter struct{}

func (runtimeManualEvidenceWriter) BoundTo(id.ID, id.ID) bool                            { return true }
func (runtimeManualEvidenceWriter) Save(context.Context, moderation.EvidenceInput) error { return nil }

type runtimeManualRemovalLookup struct{}

func (runtimeManualRemovalLookup) AcceptedManualRemoval(context.Context, id.ID, id.ID) (service.AcceptedManualRemovalReceipt, error) {
	return service.AcceptedManualRemovalReceipt{}, nil
}

type runtimeManualWarningLookup struct{}

func (runtimeManualWarningLookup) AcceptedManualWarning(context.Context, id.ID, id.ID) (service.AcceptedManualWarningReceipt, error) {
	return service.AcceptedManualWarningReceipt{}, nil
}

func TestRuntimeRolePanelPublishRequiresRolesCapability(t *testing.T) {
	handler := service.RolePanelInteractions{}
	if got := runtimeRolePanelHandlers(0, handler); len(got) != 0 {
		t.Fatalf("role panel handlers without Roles capability = %v", got)
	}
	got := runtimeRolePanelHandlers(botRolesCapability, handler)
	if registered, ok := got[protocol.OpRolePanelPublish]; !ok || registered.Handle == nil || !registered.RevisionInHandler {
		t.Fatalf("role panel publish handler=%+v; want authorized interaction handler", registered)
	}
}

type runtimeWelcomeAuthority struct{}

func (runtimeWelcomeAuthority) CanManageCommunity(context.Context, id.ID, id.ID) (bool, error) {
	return true, nil
}

type runtimeWelcomeSettingsNative struct {
	binding native.Binding
	reads   int
}

func (n *runtimeWelcomeSettingsNative) Binding() native.Binding { return n.binding }
func (n *runtimeWelcomeSettingsNative) StateGet(context.Context, uint64, []byte) (native.StateValue, error) {
	n.reads++
	return native.StateValue{}, nil
}
func (*runtimeWelcomeSettingsNative) StateTransaction(context.Context, id.ID, []native.StateChange) ([]uint64, error) {
	return nil, errors.New("unexpected runtime welcome settings write")
}

type runtimeMusicStoreFake struct{}

func (runtimeMusicStoreFake) Load(context.Context, id.ID) (persistence.Snapshot, error) {
	return persistence.Snapshot{}, nil
}
func (runtimeMusicStoreFake) Save(context.Context, id.ID, uint64, music.State) (persistence.Snapshot, error) {
	return persistence.Snapshot{}, nil
}

type runtimeMusicAuthorityFake struct{}

func (runtimeMusicAuthorityFake) CurrentCall(context.Context, id.ID) (service.MusicCallBinding, error) {
	return service.MusicCallBinding{}, nil
}
func (runtimeMusicAuthorityFake) CanUse(context.Context, service.InteractionActor, service.MusicCallBinding) (bool, error) {
	return false, nil
}
func (runtimeMusicAuthorityFake) CanDJ(context.Context, service.InteractionActor, service.MusicCallBinding) (bool, error) {
	return false, nil
}
func (runtimeMusicAuthorityFake) CanEmergencyStop(context.Context, service.InteractionActor, service.MusicCallBinding) (bool, error) {
	return false, nil
}

func TestRuntimeInteractionHandlersRegisterMusicOnlyWithCompleteAuthorityAndStore(t *testing.T) {
	community := id.New()
	base := func(music *service.MusicInteractions) map[protocol.Operation]service.InteractionHandlerSpec {
		return runtimeInteractionHandlers(service.OverviewInteractions{CommunityID: community}, service.AnnouncementInteractions{CommunityID: community}, service.ModerationInteractions{CommunityID: community}, music, nil, nil, nil)
	}
	if handlers := base(&service.MusicInteractions{Authority: runtimeMusicAuthorityFake{}}); len(handlers) != 8 {
		t.Fatalf("music handlers registered without encrypted queue store: got %d operations", len(handlers))
	}
	music := &service.MusicInteractions{Store: runtimeMusicStoreFake{}, Authority: runtimeMusicAuthorityFake{}, CommunityID: community, InstallationID: id.New()}
	handlers := base(music)
	if len(handlers) != 22 {
		t.Fatalf("runtime handlers has %d operations, want 8 existing plus 14 music operations", len(handlers))
	}
	for _, op := range []protocol.Operation{protocol.OpMusicState, protocol.OpMusicSearch, protocol.OpMusicEnqueue, protocol.OpMusicPause, protocol.OpMusicResume, protocol.OpMusicSkip, protocol.OpMusicStop, protocol.OpMusicSeek, protocol.OpMusicVolume, protocol.OpMusicShuffle, protocol.OpMusicRepeat, protocol.OpMusicRemove, protocol.OpMusicMove, protocol.OpMusicClear} {
		spec, ok := handlers[op]
		if !ok || spec.Handle == nil {
			t.Fatalf("runtime music operation %d was not registered", op)
		}
		if op != protocol.OpMusicState && op != protocol.OpMusicSearch && !spec.RevisionInHandler {
			t.Fatalf("music mutation %d lacks in-handler revision check", op)
		}
	}
}

func TestRuntimeInteractionHandlersRegisterManualEvidenceOnlyWithBothReceiptLookups(t *testing.T) {
	community, installation := id.New(), id.New()
	base := service.ModerationInteractions{CommunityID: community, InstallationID: installation,
		EvidenceReader: runtimeModerationEvidenceReader{}, EvidenceWriter: runtimeManualEvidenceWriter{}}
	build := func(m service.ModerationInteractions) map[protocol.Operation]service.InteractionHandlerSpec {
		return runtimeInteractionHandlers(service.OverviewInteractions{CommunityID: community}, service.AnnouncementInteractions{CommunityID: community}, m, nil, nil, nil, nil)
	}
	for _, partial := range []service.ModerationInteractions{
		base,
		{CommunityID: community, InstallationID: installation, EvidenceReader: base.EvidenceReader,
			EvidenceWriter: base.EvidenceWriter, ManualWarningLookup: runtimeManualWarningLookup{}},
		{CommunityID: community, InstallationID: installation, EvidenceReader: base.EvidenceReader,
			EvidenceWriter: base.EvidenceWriter, ManualRemovalLookup: runtimeManualRemovalLookup{}},
	} {
		if _, ok := build(partial)[protocol.OpModerationEvidence]; ok {
			t.Fatal("manual evidence write registered without both accepted-action receipt lookups")
		}
	}
	complete := base
	complete.ManualRemovalLookup = runtimeManualRemovalLookup{}
	complete.ManualWarningLookup = runtimeManualWarningLookup{}
	if spec, ok := build(complete)[protocol.OpModerationEvidence]; !ok || spec.Handle == nil {
		t.Fatalf("manual evidence write handler=%+v; want handler with both receipt lookups", spec)
	}
}
