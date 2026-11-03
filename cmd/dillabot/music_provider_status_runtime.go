//go:build linux

package main

import (
	"context"
	"os/exec"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/service"
	"github.com/jonasthim/dilla/internal/id"
)

func newMusicProviderStatusInteractions(installation, community id.ID, authority serviceMusicProviderAuthority) service.MusicProviderStatusInteractions {
	return service.MusicProviderStatusInteractions{
		Authority:   authority,
		CommunityID: community,
		Snapshot: func(ctx context.Context) service.MusicProviderStatusSnapshot {
			snapshot := service.MusicProviderStatusSnapshot{
				InstallationID:        installation,
				ObservedAt:            unixSecondsOrZero(time.Now()),
				YouTubePlayback:       service.ProviderSetupUnknown,
				YouTubePlaybackReason: service.ProviderReasonUnavailable,
			}
			snapshot.YouTubePlayback, snapshot.YouTubePlaybackReason = youtubePlaybackSetup()
			return snapshot
		},
	}
}

type serviceMusicProviderAuthority interface {
	BotActorAuthority(context.Context, id.ID, id.ID) (client.ActorAuthority, bool, error)
}

func youtubePlaybackSetup() (service.ProviderSetup, service.ProviderReason) {
	if _, err := newYouTubeMetadataResolver(); err != nil {
		return service.ProviderMissingRequirements, service.ProviderReasonDependenciesMissing
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return service.ProviderMissingRequirements, service.ProviderReasonDependenciesMissing
	}
	return service.ProviderConfigured, service.ProviderReasonNone
}
