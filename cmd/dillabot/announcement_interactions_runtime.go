//go:build linux

package main

import (
	"github.com/jonasthim/dilla/internal/bot/moderation"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/bot/service"
)

func runtimeInteractionHandlers(overview service.OverviewInteractions, announcements service.AnnouncementInteractions, moderationInteractions service.ModerationInteractions, music *service.MusicInteractions, nativeClient service.WelcomeSettingsNative, automodGrant service.AutoModerationGrantRedeemer, automodRuntime service.AutoModerationConfigRuntime, providerStatus ...service.MusicProviderStatusInteractions) map[protocol.Operation]service.InteractionHandlerSpec {
	statusRegistered := len(providerStatus) > 0 && providerStatus[0].Authority != nil && providerStatus[0].Snapshot != nil
	overview.SupportsMusicProviderStatus = statusRegistered
	handlers := overview.Handlers()
	for operation, handler := range announcements.Handlers() {
		handlers[operation] = handler
	}
	for operation, handler := range (service.WelcomeInteractions{Authority: announcements.Authority, CommunityID: announcements.CommunityID, Native: nativeClient}).Handlers() {
		handlers[operation] = handler
	}
	if nativeClient != nil {
		config := service.AutoModerationConfigInteractions{Authority: overview.Authority, Store: moderation.NewStore(nativeClient), GrantRedeemer: automodGrant, Runtime: automodRuntime, CommunityID: overview.CommunityID}
		for operation, handler := range config.Handlers() {
			handlers[operation] = handler
		}
	}
	if cases, ok := moderationInteractions.Handlers()[protocol.OpModerationCases]; ok {
		handlers[protocol.OpModerationCases] = cases
	}
	if moderationInteractions.EvidenceReader != nil {
		if evidence, ok := moderationInteractions.Handlers()[protocol.OpModerationEvidenceList]; ok {
			handlers[protocol.OpModerationEvidenceList] = evidence
		}
	}
	if moderationInteractions.ManualRemovalLookup != nil && moderationInteractions.ManualWarningLookup != nil && moderationInteractions.EvidenceWriter != nil {
		if evidence, ok := moderationInteractions.Handlers()[protocol.OpModerationEvidence]; ok {
			handlers[protocol.OpModerationEvidence] = evidence
		}
	}
	if music != nil && music.Store != nil && music.Authority != nil {
		for operation, handler := range music.Handlers() {
			handlers[operation] = handler
		}
	}
	if statusRegistered {
		for operation, handler := range providerStatus[0].Handlers() {
			handlers[operation] = handler
		}
	}
	return handlers
}

func runtimeRolePanelHandlers(capabilities uint64, handler service.RolePanelInteractions) map[protocol.Operation]service.InteractionHandlerSpec {
	if capabilities&botRolesCapability == 0 {
		return nil
	}
	return handler.Handlers()
}

// runtimeMusicAnnounceHandlers registers the public "started music" message only with the music interactions it reads.
func runtimeMusicAnnounceHandlers(music *service.MusicInteractions, handler service.MusicAnnouncements) map[protocol.Operation]service.InteractionHandlerSpec {
	if music == nil || music.Store == nil || music.Authority == nil {
		return nil
	}
	return handler.Handlers()
}
