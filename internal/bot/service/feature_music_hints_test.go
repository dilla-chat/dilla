package service

import (
	"context"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/music"
	"github.com/dilla-chat/dilla/internal/bot/music/persistence"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/id"
)

func TestQueueChangeSendsCallHint(t *testing.T) {
	channel, callID := id.New(), id.New()
	store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: music.InitialState(), Revision: 0}}
	authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: callID, ChannelID: channel, Active: true}, user: true, dj: true}
	rec := &hintRecorder{}
	resolver := &musicInteractionResolverFake{tracks: []music.Track{{ID: "yt:abcdefghijk", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "One"}}}
	h := MusicInteractions{Store: store, Authority: authority, CommunityID: id.New(), Resolver: resolver, Hints: rec}
	actor := InteractionActor{User: id.New()}
	enqueue := protocol.Request{CommunityID: h.CommunityID, ChannelID: &channel, Revision: uint64Ptr(0), Args: []any{"One"}}
	if result, err := h.handle(context.Background(), actor, enqueue, protocol.OpMusicEnqueue); err != nil || result.Status != 0 {
		t.Fatalf("enqueue: %+v %v", result, err)
	}
	if got := rec.hints(); len(got) != 1 || got[0].kind != client.StateScopeCall || got[0].scope != channel {
		t.Fatalf("hints after a queue change = %+v, want one call hint for the voice channel", got)
	}
	// Reading the state changes nothing, so it hints nothing; nor does a refused change.
	read := protocol.Request{CommunityID: h.CommunityID, ChannelID: &channel}
	if result, err := h.handle(context.Background(), actor, read, protocol.OpMusicState); err != nil || result.Status != 0 {
		t.Fatalf("state: %+v %v", result, err)
	}
	authority.dj = false
	refused := protocol.Request{CommunityID: h.CommunityID, ChannelID: &channel, Revision: uint64Ptr(1)}
	if result, _ := h.handle(context.Background(), actor, refused, protocol.OpMusicShuffle); result.Status == 0 {
		t.Fatalf("a non-DJ shuffled the queue: %+v", result)
	}
	if got := rec.hints(); len(got) != 1 {
		t.Fatalf("hints after a read and a refusal = %+v, want still one", got)
	}
}

// /play needs no text channel (it queues in the voice room), and the started-music card goes only into a Bot channel
// the command names: with an unmarked target nothing is posted and the feature is flagged.
func TestPlayNeedsNoTextChannelAndPostsCardOnlyWithTarget(t *testing.T) {
	actor := id.New()
	f := newMusicAnnounceFixture(actor, false)
	m := &marks{set: map[id.ID]bool{}, known: true}
	gateFor(t, f.community, m)
	t.Cleanup(func() { ClearNeedsBotChannel(f.community, "music") })

	// Queueing needs no text channel: no destination, no sender.
	voice := f.voice
	resolver := &musicInteractionResolverFake{tracks: []music.Track{{ID: "yt:abcdefghijk", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "One"}}}
	queue := MusicInteractions{Store: &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: music.InitialState()}}, Authority: f.authority, CommunityID: f.community, Resolver: resolver}
	play := protocol.Request{CommunityID: f.community, ChannelID: &voice, Revision: uint64Ptr(0), Args: []any{"One"}}
	if result, err := queue.handle(context.Background(), InteractionActor{User: actor}, play, protocol.OpMusicEnqueue); err != nil || result.Status != 0 {
		t.Fatalf("play with no text channel: %+v %v", result, err)
	}
	if f.sender.calls != 0 {
		t.Fatalf("play posted %d messages, want none", f.sender.calls)
	}

	// The card with a target that is not a Bot channel: nothing posted, flagged.
	if result := f.run(t, actor); result.Status != interactionStatusUnavailable || f.sender.calls != 0 {
		t.Fatalf("card to an unmarked channel: %+v sends=%d", result, f.sender.calls)
	}
	if flags := NeedsBotChannelFlags(f.community); len(flags) != 1 || flags[0].Feature != "music" || flags[0].Channel != f.text {
		t.Fatalf("flags %+v, want music flagged for the target", flags)
	}

	// With a Bot channel as the target the card is posted and the flag clears.
	m.set[f.text] = true
	if result := f.run(t, actor); result.Status != 0 || f.sender.calls != 1 {
		t.Fatalf("card to a Bot channel: %+v sends=%d", result, f.sender.calls)
	}
	if flags := NeedsBotChannelFlags(f.community); len(flags) != 0 {
		t.Fatalf("flags %+v after posting, want none", flags)
	}
}

// stuckHints is a HintSender whose every send blocks until the test ends (a slow or reconnecting Bot socket).
type stuckHints struct{ release chan struct{} }

func (s stuckHints) Hint(ctx context.Context, _ uint8, _ id.ID) error {
	select {
	case <-s.release:
	case <-ctx.Done():
	}
	return nil
}

// Task 17 fix round 2 (I4): hints go through client.AsyncHints, so a sender that blocks delays neither a music command's
// reply nor a poll vote's (G2: the answer is RTT + 100 ms; a synchronous hint could add its 2 s timeout).
func TestBlockedHintSenderDelaysNoCommandReply(t *testing.T) {
	stuck := stuckHints{release: make(chan struct{})}
	t.Cleanup(func() { close(stuck.release) })
	hints := client.NewAsyncHints(stuck)

	channel, callID := id.New(), id.New()
	store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: music.InitialState(), Revision: 0}}
	authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: callID, ChannelID: channel, Active: true}, user: true, dj: true}
	resolver := &musicInteractionResolverFake{tracks: []music.Track{{ID: "yt:abcdefghijk", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "One"}}}
	h := MusicInteractions{Store: store, Authority: authority, CommunityID: id.New(), Resolver: resolver, Hints: hints}
	enqueue := protocol.Request{CommunityID: h.CommunityID, ChannelID: &channel, Revision: uint64Ptr(0), Args: []any{"One"}}
	start := time.Now()
	for i := range 3 {
		enqueue.Revision = uint64Ptr(uint64(i))
		if result, err := h.handle(context.Background(), InteractionActor{User: id.New()}, enqueue, protocol.OpMusicEnqueue); err != nil || result.Status != 0 {
			t.Fatalf("enqueue %d: %+v %v", i, result, err)
		}
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("three queue changes with a blocked hint sender took %v; the reply must not wait for the hint", elapsed)
	}
}
