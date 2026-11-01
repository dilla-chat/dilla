package service

import (
	"context"
	"testing"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/music"
	"github.com/jonasthim/dilla/internal/bot/music/persistence"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

// Task 10c: a new music session (a track enqueued while nothing plays or waits) starts at the
// installation's default volume; enqueueing onto a running session keeps the volume a DJ set.
func TestMusicEnqueueStartsANewSessionAtTheDefaultVolume(t *testing.T) {
	community, installationID, channel, actorID, callID := id.New(), id.New(), id.New(), id.New(), id.New()
	session := &musicAuthoritySessionFake{
		actor: client.ActorAuthority{Owner: true}, present: true,
		permissions: musicPermissionConnect | musicPermissionSpeak,
		call:        client.ChannelCallState{CallID: &callID, GroupID: ptrID(id.New()), Participants: []client.CallParticipant{{UserID: actorID}}},
	}
	authority := &CommunityMusicInteractionAuthority{
		Session: session,
		Store: &musicAuthorityStoreFake{installation: store.BotInstallationRow{
			ID: installationID, State: store.BotInstallationReady, Capabilities: musicBotCapability,
		}},
		InstallationID: installationID, CommunityID: community,
	}
	queue := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: music.InitialState()}}
	resolver := &musicInteractionResolverFake{tracks: []music.Track{{Provider: music.ProviderYouTube, CanonicalID: "LvH_ft4XR1Q", Title: "One"}}}
	volume := uint8(50)
	h := MusicInteractions{Store: queue, Authority: authority, Resolver: resolver, InstallationID: installationID, CommunityID: community,
		DefaultVolume: func() uint8 { return volume }}
	enqueue := func() {
		t.Helper()
		revision := queue.snapshot.Revision
		request := protocol.Request{CommunityID: community, ChannelID: &channel, Revision: &revision, Args: []any{"https://www.youtube.com/watch?v=LvH_ft4XR1Q"}}
		if result, err := h.handle(context.Background(), InteractionActor{User: actorID}, request, protocol.OpMusicEnqueue); err != nil || result.Status != 0 {
			t.Fatalf("enqueue = %+v, %v", result, err)
		}
	}
	enqueue()
	if got := queue.snapshot.State.Volume; got != 50 {
		t.Fatalf("new session volume = %d, want the default 50", got)
	}
	volume = 25
	enqueue()
	if got := queue.snapshot.State.Volume; got != 50 {
		t.Fatalf("volume after enqueueing onto a running session = %d, want it kept at 50", got)
	}
}
