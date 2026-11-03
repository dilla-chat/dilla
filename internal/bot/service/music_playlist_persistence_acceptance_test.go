package service

import (
	"context"
	"fmt"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/music"
	"github.com/dilla-chat/dilla/internal/bot/music/persistence"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/id"
)

// playlistAcceptanceNative provides the same revision-checked state API used by
// the encrypted native helper while retaining rows across Store instances.
type playlistAcceptanceNative struct {
	binding native.Binding
	rows    map[string]native.StateValue
	writes  int
}

func (n *playlistAcceptanceNative) Binding() native.Binding { return n.binding }
func (n *playlistAcceptanceNative) StateGet(_ context.Context, area uint64, key []byte) (native.StateValue, error) {
	return n.rows[fmt.Sprintf("%d:%s", area, key)], nil
}
func (n *playlistAcceptanceNative) StateTransaction(_ context.Context, _ id.ID, changes []native.StateChange) ([]uint64, error) {
	if len(changes) != 1 {
		return nil, fmt.Errorf("expected one state change, got %d", len(changes))
	}
	change := changes[0]
	key := fmt.Sprintf("%d:%s", change.Area, change.Key)
	current := n.rows[key]
	if current.Revision != change.ExpectedRevision {
		return nil, persistence.ErrConflict
	}
	n.writes++
	n.rows[key] = native.StateValue{Revision: current.Revision + 1, Value: append([]byte(nil), change.Value...)}
	return []uint64{current.Revision + 1}, nil
}

// This exercises the registered interaction handler and real persistence.Store:
// an imported playlist that crosses the queue cap must leave the stored queue
// byte-for-byte logically unchanged after the handler reports refusal.
func TestYouTubePlaylistOverflowLeavesPersistedQueueUnchanged(t *testing.T) {
	ctx := context.Background()
	community, channel, user := id.New(), id.New(), id.New()
	nativeStore := &playlistAcceptanceNative{
		binding: native.Binding{Instance: id.New(), Community: community, Installation: id.New()},
		rows:    make(map[string]native.StateValue),
	}
	queueStore, err := persistence.New(nativeStore, community)
	if err != nil {
		t.Fatal(err)
	}
	before, err := queueStore.Load(ctx, channel)
	if err != nil {
		t.Fatal(err)
	}
	tracks := make([]music.Track, music.QueueLimit-1)
	for i := range tracks {
		tracks[i] = music.Track{ID: fmt.Sprintf("existing-%03d", i), Provider: music.ProviderYouTube,
			CanonicalID: "abcdefghijk", Title: "Existing", RequesterID: "prior-requester"}
	}
	state, err := music.Apply(before.State, before.Revision, music.Operation{Kind: music.OpEnqueue, Tracks: tracks})
	if err != nil {
		t.Fatal(err)
	}
	before, err = queueStore.Save(ctx, channel, before.Revision, state)
	if err != nil {
		t.Fatal(err)
	}
	writesBeforeImport := nativeStore.writes

	authority := &musicInteractionAuthorityFake{
		call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, dj: true,
	}
	resolver := &musicInteractionResolverFake{tracks: []music.Track{
		{ID: "resolved-one", Provider: music.ProviderYouTube, CanonicalID: "12345678901", Title: "Playlist one"},
		{ID: "resolved-two", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "Playlist two"},
	}}
	h := MusicInteractions{Store: queueStore, Authority: authority, CommunityID: community, Resolver: resolver}
	request := protocol.Request{CommunityID: community, ChannelID: &channel, Revision: uint64Ptr(before.Revision),
		Args: []any{"https://www.youtube.com/playlist?list=PLabc_123"}}
	result, err := h.Handlers()[protocol.OpMusicEnqueue].Handle(ctx, InteractionActor{User: user}, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != interactionStatusUnavailable {
		t.Fatalf("playlist enqueue status=%d, want unavailable for cap overflow", result.Status)
	}
	if nativeStore.writes != writesBeforeImport {
		t.Fatalf("overflow import committed %d state write(s), want none", nativeStore.writes-writesBeforeImport)
	}

	restartedStore, err := persistence.New(nativeStore, community)
	if err != nil {
		t.Fatal(err)
	}
	after, err := restartedStore.Load(ctx, channel)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision || len(after.State.Queue) != music.QueueLimit-1 {
		t.Fatalf("persisted snapshot after refusal: revision=%d queue=%d, want revision=%d queue=%d",
			after.Revision, len(after.State.Queue), before.Revision, music.QueueLimit-1)
	}
	for i, track := range after.State.Queue {
		if track.ID != tracks[i].ID {
			t.Fatalf("persisted queue item %d=%q, want original %q", i, track.ID, tracks[i].ID)
		}
	}
}

// This crosses the resolver, registered interaction handler, encrypted state
// store, and queue reducer. It also checks that a normal listener cannot use
// DJ controls, while an authorized DJ can resume and skip the imported tracks.
func TestYouTubePlaylistImportPersistsAndDJControlsUseCommittedQueue(t *testing.T) {
	ctx := context.Background()
	community, channel, dj, listener := id.New(), id.New(), id.New(), id.New()
	nativeStore := &playlistAcceptanceNative{
		binding: native.Binding{Instance: id.New(), Community: community, Installation: id.New()},
		rows:    make(map[string]native.StateValue),
	}
	queueStore, err := persistence.New(nativeStore, community)
	if err != nil {
		t.Fatal(err)
	}
	authority := &musicInteractionAuthorityFake{
		call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, dj: true,
	}
	resolver := &musicInteractionResolverFake{tracks: []music.Track{
		{Provider: music.ProviderYouTube, CanonicalID: "12345678901", Title: "First"},
		{Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "Second"},
	}}
	h := MusicInteractions{Store: queueStore, Authority: authority, CommunityID: community, Resolver: resolver}
	handlers := h.Handlers()

	initial, err := queueStore.Load(ctx, channel)
	if err != nil {
		t.Fatal(err)
	}
	importReq := protocol.Request{CommunityID: community, ChannelID: &channel, Revision: uint64Ptr(initial.Revision),
		Args: []any{"https://www.youtube.com/playlist?list=PLabc_123"}}
	imported, err := handlers[protocol.OpMusicEnqueue].Handle(ctx, InteractionActor{User: dj}, importReq)
	if err != nil || imported.Status != 0 || imported.Revision == nil || *imported.Revision != 1 {
		t.Fatalf("playlist import result=%+v err=%v, want committed revision 1", imported, err)
	}

	// A new Store instance models a process restart and confirms both items
	// reached durable state as one revision.
	restartedStore, err := persistence.New(nativeStore, community)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := restartedStore.Load(ctx, channel)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision != 1 || len(snapshot.State.Queue) != 2 || snapshot.State.Queue[0].Title != "First" || snapshot.State.Queue[1].Title != "Second" {
		t.Fatalf("reloaded playlist snapshot=%+v, want both ordered tracks at revision 1", snapshot)
	}

	// Pause is a DJ control. Refusal must not consume a durable revision.
	authority.dj = false
	pauseReq := protocol.Request{CommunityID: community, ChannelID: &channel, Revision: uint64Ptr(snapshot.Revision)}
	refused, err := handlers[protocol.OpMusicPause].Handle(ctx, InteractionActor{User: listener}, pauseReq)
	if err != nil || refused.Status != interactionStatusForbidden {
		t.Fatalf("listener pause result=%+v err=%v, want forbidden", refused, err)
	}
	authority.dj = true

	resumeReq := protocol.Request{CommunityID: community, ChannelID: &channel, Revision: uint64Ptr(snapshot.Revision)}
	resumed, err := handlers[protocol.OpMusicResume].Handle(ctx, InteractionActor{User: dj}, resumeReq)
	if err != nil || resumed.Status != 0 || resumed.Revision == nil || *resumed.Revision != 2 {
		t.Fatalf("DJ resume result=%+v err=%v, want committed revision 2", resumed, err)
	}
	snapshot, err = restartedStore.Load(ctx, channel)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State.Current == nil || snapshot.State.Current.Title != "First" || len(snapshot.State.Queue) != 1 || snapshot.State.Queue[0].Title != "Second" {
		t.Fatalf("resumed persisted snapshot=%+v, want first current and second queued", snapshot)
	}
	skipReq := protocol.Request{CommunityID: community, ChannelID: &channel, Revision: uint64Ptr(snapshot.Revision)}
	skipped, err := handlers[protocol.OpMusicSkip].Handle(ctx, InteractionActor{User: dj}, skipReq)
	if err != nil || skipped.Status != 0 || skipped.Revision == nil || *skipped.Revision != 3 {
		t.Fatalf("DJ skip result=%+v err=%v, want committed revision 3", skipped, err)
	}
	snapshot, err = restartedStore.Load(ctx, channel)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State.Current == nil || snapshot.State.Current.Title != "Second" || len(snapshot.State.Queue) != 0 {
		t.Fatalf("skipped persisted snapshot=%+v, want second current", snapshot)
	}
}
