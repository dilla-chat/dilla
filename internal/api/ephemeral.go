// Package api resolves the current channel and call audience for ephemeral gateway signals.
package api

import (
	"context"
	"errors"

	"github.com/livekit/protocol/livekit"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

var errEphemeralNotFound = errors.New("E_NOT_FOUND")

// TypingAudience admits a live device that can view the channel and returns its
// current recipients. Missing and hidden channels have the same refusal.
func TypingAudience(repo store.Repository) func(context.Context, id.ID, id.ID) ([]id.ID, error) {
	return func(ctx context.Context, deviceID, channelID id.ID) ([]id.ID, error) {
		dev, err := repo.GetDevice(ctx, deviceID)
		if err != nil || dev.RevokedAt != nil || dev.QuarantinedAt != nil {
			return nil, errEphemeralNotFound
		}
		ch, err := repo.GetChannel(ctx, channelID)
		if err != nil {
			return nil, errEphemeralNotFound
		}
		bits, err := NewResolver(repo).Resolve(ctx, dev.UserID, ch)
		if err != nil || !bits.Has(PermViewChannel) {
			return nil, errEphemeralNotFound
		}
		users, err := EligibleUsers(ctx, repo, ch)
		if err != nil {
			return nil, errEphemeralNotFound
		}
		return users, nil
	}
}

// VoiceSelfAudience admits only a device currently present in the live SFU room.
func VoiceSelfAudience(repo store.Repository, sfu CallTokens) func(context.Context, id.ID, id.ID) (id.ID, []id.ID, uint64, error) {
	return func(ctx context.Context, deviceID, callID id.ID) (id.ID, []id.ID, uint64, error) {
		if sfu == nil {
			return id.ID{}, nil, 0, errEphemeralNotFound
		}
		call, err := repo.GetVoiceSession(ctx, callID)
		if err != nil || call.Ended != nil {
			return id.ID{}, nil, 0, errEphemeralNotFound
		}
		parts, err := sfu.Participants(ctx, call.LivekitRoom)
		if err != nil {
			return id.ID{}, nil, 0, errEphemeralNotFound
		}
		var flags uint64
		present := false
		for _, part := range parts {
			if part.GetIdentity() == deviceID.String() && part.GetState() != livekit.ParticipantInfo_DISCONNECTED {
				present = true
				flags = voiceTrackFlags(part)
				break
			}
		}
		if !present {
			return id.ID{}, nil, 0, errEphemeralNotFound
		}
		users, err := TypingAudience(repo)(ctx, deviceID, call.ChannelID)
		if err != nil {
			return id.ID{}, nil, 0, errEphemeralNotFound
		}
		return call.ChannelID, users, flags, nil
	}
}
