// Package api_test exercises ephemeral audiences against the API's real store fixture.
package api_test

import (
	"testing"

	"github.com/livekit/protocol/livekit"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func TestTypingAudienceUsesCurrentChannelViewPermission(t *testing.T) {
	e, community, ownerToken := channelEnv(t)
	channel, _, status := newChannel(t, e, community, ownerToken, 0, 0, 0, "typing")
	if status != 201 {
		t.Fatalf("channel status = %d", status)
	}
	owner := e.sess[ownerToken]
	outsider, outsiderToken := e.NewUser("outsider")
	_ = outsider
	denied := e.sess[outsiderToken]
	audience := api.TypingAudience(e.Repo)
	users, err := audience(t.Context(), owner.DeviceID, channel)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0] != owner.UserID {
		t.Fatalf("audience = %v", users)
	}
	_, hiddenErr := audience(t.Context(), denied.DeviceID, channel)
	_, absentErr := audience(t.Context(), owner.DeviceID, id.New())
	if hiddenErr == nil || absentErr == nil || hiddenErr.Error() != absentErr.Error() {
		t.Fatalf("hidden error %v differs from absent %v", hiddenErr, absentErr)
	}
}

func TestVoiceSelfAudienceRefusesUnknownCall(t *testing.T) {
	e, _, ownerToken := channelEnv(t)
	owner := e.sess[ownerToken]
	_, _, _, err := api.VoiceSelfAudience(e.Repo, &stubSFU{})(t.Context(), owner.DeviceID, id.New())
	if err == nil {
		t.Fatal("unknown call was accepted")
	}
}

func TestVoiceSelfAudienceRequiresCurrentRoomParticipant(t *testing.T) {
	e, community, ownerToken := channelEnv(t)
	channel, _, status := newChannel(t, e, community, ownerToken, 1, 1, 2, "voice")
	if status != 201 {
		t.Fatalf("channel status = %d", status)
	}
	owner := e.sess[ownerToken]
	call := id.New()
	const room = "ephemeral-room"
	if err := e.Repo.PutVoiceSession(t.Context(), store.VoiceSessionRow{
		CallID: call, ChannelID: channel, LivekitRoom: room, Started: 1,
	}); err != nil {
		t.Fatal(err)
	}
	sfu := &stubSFU{}
	audience := api.VoiceSelfAudience(e.Repo, sfu)
	if _, _, _, err := audience(t.Context(), owner.DeviceID, call); err == nil {
		t.Fatal("absent participant was accepted")
	}
	sfu.setPresent(room, &livekit.ParticipantInfo{Identity: owner.DeviceID.String()})
	gotChannel, users, flags, err := audience(t.Context(), owner.DeviceID, call)
	if err != nil {
		t.Fatal(err)
	}
	if gotChannel != channel || len(users) != 1 || users[0] != owner.UserID || flags != 0 {
		t.Fatalf("audience = %s %v flags %d", gotChannel, users, flags)
	}
}

func TestVoiceSelfAudienceRefusesDisconnectedParticipant(t *testing.T) {
	e, community, ownerToken := channelEnv(t)
	channel, _, status := newChannel(t, e, community, ownerToken, 1, 1, 2, "voice")
	if status != 201 {
		t.Fatalf("channel status = %d", status)
	}
	owner := e.sess[ownerToken]
	call := id.New()
	const room = "ephemeral-disconnected-room"
	if err := e.Repo.PutVoiceSession(t.Context(), store.VoiceSessionRow{
		CallID: call, ChannelID: channel, LivekitRoom: room, Started: 1,
	}); err != nil {
		t.Fatal(err)
	}
	sfu := &stubSFU{}
	sfu.setPresent(room, &livekit.ParticipantInfo{
		Identity: owner.DeviceID.String(), State: livekit.ParticipantInfo_DISCONNECTED,
	})
	_, _, _, err := api.VoiceSelfAudience(e.Repo, sfu)(t.Context(), owner.DeviceID, call)
	if err == nil || err.Error() != "E_NOT_FOUND" {
		t.Fatalf("disconnected participant admitted: error = %v, want E_NOT_FOUND", err)
	}
}

func TestVoiceSelfAudienceReturnsCurrentVideoAndScreenTracks(t *testing.T) {
	e, community, ownerToken := channelEnv(t)
	channel, _, status := newChannel(t, e, community, ownerToken, 1, 1, 2, "voice")
	if status != 201 {
		t.Fatalf("channel status = %d", status)
	}
	owner := e.sess[ownerToken]
	call := id.New()
	const room = "ephemeral-tracks-room"
	if err := e.Repo.PutVoiceSession(t.Context(), store.VoiceSessionRow{CallID: call, ChannelID: channel, LivekitRoom: room, Started: 1}); err != nil {
		t.Fatal(err)
	}
	sfu := &stubSFU{}
	sfu.setPresent(room, &livekit.ParticipantInfo{Identity: owner.DeviceID.String(), Tracks: []*livekit.TrackInfo{
		{Source: livekit.TrackSource_CAMERA}, {Source: livekit.TrackSource_SCREEN_SHARE},
	}})
	_, _, flags, err := api.VoiceSelfAudience(e.Repo, sfu)(t.Context(), owner.DeviceID, call)
	if err != nil {
		t.Fatal(err)
	}
	if flags != api.VoiceVideo|api.VoiceScreen {
		t.Fatalf("flags = %d, want video|screen (24)", flags)
	}
}
