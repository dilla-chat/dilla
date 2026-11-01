//go:build linux

package main

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/service"
	"github.com/jonasthim/dilla/internal/id"
)

func newMusicProviderStatusInteractions(installation, community id.ID, authority serviceMusicProviderAuthority, credentials *service.SpotifyCredentials, cfg config) service.MusicProviderStatusInteractions {
	return service.MusicProviderStatusInteractions{
		Authority:   authority,
		CommunityID: community,
		Snapshot: func(ctx context.Context) service.MusicProviderStatusSnapshot {
			snapshot := service.MusicProviderStatusSnapshot{
				InstallationID:        installation,
				ObservedAt:            unixSecondsOrZero(time.Now()),
				SpotifyAccount:        service.ProviderAccountUnknown,
				SpotifyAccountReason:  service.ProviderReasonUnavailable,
				SpotifyPlayback:       service.ProviderSetupUnknown,
				SpotifyPlaybackReason: service.ProviderReasonUnavailable,
				YouTubePlayback:       service.ProviderSetupUnknown,
				YouTubePlaybackReason: service.ProviderReasonUnavailable,
			}
			if credentials != nil {
				snapshot.SpotifyAccount, snapshot.SpotifyAccountReason = credentials.InspectStatus(ctx)
			}
			snapshot.SpotifyPlayback, snapshot.SpotifyPlaybackReason = spotifyPlaybackSetup(cfg)
			if credentials == nil && snapshot.SpotifyPlayback == service.ProviderConfigured {
				snapshot.SpotifyPlayback, snapshot.SpotifyPlaybackReason = service.ProviderSetupUnknown, service.ProviderReasonUnavailable
			}
			snapshot.YouTubePlayback, snapshot.YouTubePlaybackReason = youtubePlaybackSetup()
			return snapshot
		},
	}
}

type serviceMusicProviderAuthority interface {
	BotActorAuthority(context.Context, id.ID, id.ID) (client.ActorAuthority, bool, error)
}

func spotifyPlaybackSetup(cfg config) (service.ProviderSetup, service.ProviderReason) {
	if cfg.SpotifyHelperExecutable == "" {
		return service.ProviderMissingRequirements, service.ProviderReasonHelperMissing
	}
	if validateHelperExecutable(cfg.SpotifyHelperExecutable) != nil {
		return service.ProviderMissingRequirements, service.ProviderReasonHelperInvalid
	}
	if validatePrivateStateDir(cfg.StateDir) != nil {
		return service.ProviderMissingRequirements, service.ProviderReasonStateDirectoryUnavailable
	}
	if strings.TrimSpace(cfg.SpotifyClientID) == "" || strings.ContainsAny(cfg.SpotifyClientID, "\r\n\x00") || !strings.HasPrefix(cfg.SpotifyRedirectURL, "https://") {
		return service.ProviderMissingRequirements, service.ProviderReasonDependenciesMissing
	}
	return service.ProviderConfigured, service.ProviderReasonNone
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
