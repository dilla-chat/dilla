package service

import (
	"context"
	"errors"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

const (
	OpMusicProvidersStatus       = protocol.OpMusicProvidersStatus
	musicProviderStatusDataLimit = 256
)

type ProviderAccount uint64

const (
	ProviderAccountUnknown ProviderAccount = iota
	ProviderAccountNotLinked
	ProviderAccountLinked
	ProviderAccountNeedsAttention
)

type ProviderSetup uint64

const (
	ProviderSetupUnknown ProviderSetup = iota
	ProviderConfigured
	ProviderMissingRequirements
)

type ProviderReason uint64

const (
	ProviderReasonNone ProviderReason = iota
	ProviderReasonMissingCredentials
	ProviderReasonInvalidCredentials
	ProviderReasonBindingMismatch
	ProviderReasonAccountMismatch
	ProviderReasonHelperMissing
	ProviderReasonHelperInvalid
	ProviderReasonStateDirectoryUnavailable
	ProviderReasonDependenciesMissing
	ProviderReasonUnsupportedBot
	ProviderReasonUnavailable
)

// MusicProviderStatusSnapshot is a single fresh observation. Its fields are
// fixed enums so runtime errors, paths, account identifiers and secrets cannot leak.
type MusicProviderStatusSnapshot struct {
	InstallationID        id.ID
	ObservedAt            uint64
	SpotifyAccount        ProviderAccount
	SpotifyAccountReason  ProviderReason
	SpotifyPlayback       ProviderSetup
	SpotifyPlaybackReason ProviderReason
	YouTubePlayback       ProviderSetup
	YouTubePlaybackReason ProviderReason
}

type musicProviderStatusAuthority interface {
	BotActorAuthority(context.Context, id.ID, id.ID) (client.ActorAuthority, bool, error)
}

// MusicProviderStatusInteractions authorizes every read against current community
// permissions and recomputes its bounded response for exact request replays.
type MusicProviderStatusInteractions struct {
	Authority   musicProviderStatusAuthority
	CommunityID id.ID
	Snapshot    func(context.Context) MusicProviderStatusSnapshot
}

func (h MusicProviderStatusInteractions) Handlers() map[protocol.Operation]InteractionHandlerSpec {
	return map[protocol.Operation]InteractionHandlerSpec{
		protocol.OpMusicProvidersStatus: {RecomputeOnReplay: true, Handle: h.get},
	}
}

func (h MusicProviderStatusInteractions) get(ctx context.Context, actor InteractionActor, request protocol.Request) (InteractionResult, error) {
	if h.Authority == nil || h.Snapshot == nil || h.CommunityID.IsZero() || request.CommunityID != h.CommunityID || actor.User.IsZero() || actor.Kind != 0 {
		return InteractionResult{}, errors.New("missing music provider status authority or context")
	}
	authority, present, err := h.Authority.BotActorAuthority(ctx, h.CommunityID, actor.User)
	if err != nil {
		return InteractionResult{}, err
	}
	if !present || (!authority.Owner && !authority.Administrator && authority.EffectiveBits&manageCommunityBit == 0) {
		return InteractionResult{Status: interactionStatusForbidden}, nil
	}
	snapshot := h.Snapshot(ctx)
	if snapshot.InstallationID.IsZero() || snapshot.ObservedAt == 0 || snapshot.SpotifyAccount > ProviderAccountNeedsAttention ||
		snapshot.SpotifyAccountReason > ProviderReasonUnavailable || snapshot.SpotifyPlayback > ProviderMissingRequirements ||
		snapshot.SpotifyPlaybackReason > ProviderReasonUnavailable || snapshot.YouTubePlayback > ProviderMissingRequirements ||
		snapshot.YouTubePlaybackReason > ProviderReasonUnavailable {
		return InteractionResult{}, errors.New("invalid music provider status snapshot")
	}
	data, err := cborx.Marshal([]any{uint64(1), snapshot.InstallationID, snapshot.ObservedAt,
		uint64(snapshot.SpotifyAccount), uint64(snapshot.SpotifyAccountReason), uint64(snapshot.SpotifyPlayback),
		uint64(snapshot.SpotifyPlaybackReason), uint64(snapshot.YouTubePlayback), uint64(snapshot.YouTubePlaybackReason)})
	if err != nil {
		return InteractionResult{}, err
	}
	if len(data) > musicProviderStatusDataLimit {
		return InteractionResult{}, errors.New("music provider status exceeds response limit")
	}
	return InteractionResult{Status: 0, Data: data}, nil
}
