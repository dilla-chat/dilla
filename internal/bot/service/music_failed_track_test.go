package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/music"
	"github.com/dilla-chat/dilla/internal/bot/music/persistence"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	botstrings "github.com/dilla-chat/dilla/internal/bot/strings"
	"github.com/dilla-chat/dilla/internal/id"
)

// v0.1.0-bot2.2 Task 7: a /play whose track failed answers that it was removed; refusals name their queue item and
// never the track.

const (
	fixtureURL   = "https://www.youtube.com/watch?v=dQw4w9WgXcQ"
	fixtureVideo = "dQw4w9WgXcQ"
	fixtureTitle = "Fixture Title Never Logged"
)

func failedTrackHandler(t *testing.T, seed music.State, logs *bytes.Buffer) (*MusicInteractions, *musicInteractionStoreFake, id.ID) {
	t.Helper()
	channel, community := id.New(), id.New()
	store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: seed, Revision: seed.Revision}}
	h := &MusicInteractions{Store: store, CommunityID: community, InstallationID: id.New(), DeviceID: id.New(),
		Authority: &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: channel, GroupID: id.New(), ChannelID: channel, Active: true}, user: true, dj: true},
		Resolver:  &musicInteractionResolverFake{tracks: []music.Track{{Provider: music.ProviderYouTube, CanonicalID: fixtureVideo, Title: fixtureTitle}}},
		Logger:    slog.New(slog.NewJSONHandler(logs, nil))}
	return h, store, channel
}

func playFixture(t *testing.T, h *MusicInteractions, channel id.ID) InteractionResult {
	t.Helper()
	result, err := h.handle(withSealedPlay(t.Context()), InteractionActor{User: id.New()},
		protocol.Request{CommunityID: h.CommunityID, ChannelID: &channel, Args: []any{fixtureURL}}, protocol.OpMusicEnqueue)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestPlaybackLinesCarryItemID(t *testing.T) {
	var logs bytes.Buffer
	h, store, channel := failedTrackHandler(t, music.InitialState(), &logs)
	h.OnPlay = func(context.Context, id.ID, id.ID, id.ID, music.State, uint64) (uint64, error) {
		return 0, &music.StartError{Stage: "room_connect", Cause: errors.New("room refused " + fixtureURL)}
	}
	if result := playFixture(t, h, channel); result.Status != interactionStatusUnavailable {
		t.Fatalf("result = %+v", result)
	}
	current := store.snapshot.State.Current
	if current == nil {
		t.Fatal("the /play left no queue item")
	}
	var row map[string]any
	if err := json.Unmarshal(logs.Bytes(), &row); err != nil {
		t.Fatal(err)
	}
	// The handler cannot see what the supervisor loaded from storage, so the refused line says queued_earlier (not
	// enqueued by this command), never "restored", whose meaning on the playback lines is "loaded from storage".
	if _, ambiguous := row["restored"]; row["msg"] != "bot music operation refused" || row["item_id"] != current.ID || row["queued_earlier"] != false || ambiguous {
		t.Fatalf("refused line = %v, want item_id %s and queued_earlier=false", row, current.ID)
	}
	for _, secret := range []string{fixtureURL, fixtureVideo, fixtureTitle} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("a log line carries track content %q: %s", secret, logs.String())
		}
	}
}

func TestFailedStartAnswersRemoved(t *testing.T) {
	var logs bytes.Buffer
	h, store, channel := failedTrackHandler(t, music.InitialState(), &logs)
	h.OnPlay = func(_ context.Context, _, _, _ id.ID, state music.State, revision uint64) (uint64, error) {
		return revision + 1, &music.TrackRemovedError{ItemID: state.Current.ID, Cause: &music.StartError{Stage: "provider_pcm", Cause: errors.New("http 403")}}
	}
	result := playFixture(t, h, channel)
	if result.Status != interactionStatusUnavailable || result.Note != botstrings.MusicTrackRemoved() {
		t.Fatalf("result = %+v, want the removed answer", result)
	}
	if store.saves != 1 {
		t.Fatalf("saves = %d: the handler must not pause the removed track back into the queue", store.saves)
	}
	if strings.Contains(strings.ToLower(result.Note), "encrypt") {
		t.Fatalf("note %q", result.Note)
	}

	// The next queued track started in its place: the answer says both.
	next := music.Track{ID: "next", Provider: music.ProviderYouTube, CanonicalID: fixtureVideo, Title: "Next", Artist: "Band"}
	h.OnPlay = func(_ context.Context, _, _, _ id.ID, state music.State, revision uint64) (uint64, error) {
		return revision + 1, &music.TrackRemovedError{ItemID: state.Current.ID, Playing: &next, Cause: &music.StartError{Stage: "provider_pcm", Cause: errors.New("http 403")}}
	}
	store.snapshot = persistence.Snapshot{State: music.InitialState()}
	result = playFixture(t, h, channel)
	if result.Status != 0 || result.Note != botstrings.MusicTrackRemoved()+" Now playing: Next — Band." {
		t.Fatalf("result = %+v", result)
	}
}

func TestPlayAppendsLastWhenPausedQueue(t *testing.T) {
	seed := music.InitialState()
	x := music.Track{ID: "x", Provider: music.ProviderYouTube, CanonicalID: fixtureVideo, Title: "X", RequesterID: "member"}
	y := music.Track{ID: "y", Provider: music.ProviderYouTube, CanonicalID: fixtureVideo, Title: "Y", RequesterID: "member"}
	seed.Current, seed.Queue = &x, []music.Track{y}
	var logs bytes.Buffer
	h, _, channel := failedTrackHandler(t, seed, &logs)
	var handed music.State
	h.OnPlay = func(_ context.Context, _, _, _ id.ID, state music.State, revision uint64) (uint64, error) {
		handed = state
		return revision, nil
	}
	if result := playFixture(t, h, channel); result.Status != 0 {
		t.Fatalf("result = %+v", result)
	}
	if handed.Current == nil || handed.Current.ID != "x" || len(handed.Queue) != 2 || handed.Queue[0].ID != "y" || handed.Queue[1].Title != fixtureTitle {
		t.Fatalf("handed = %+v, want X (resumed), Y, then the new track last", handed)
	}
}
