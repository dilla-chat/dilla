package service

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jonasthim/dilla/internal/bot/music"
	"github.com/jonasthim/dilla/internal/bot/music/persistence"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

func TestMusicSnapshotsBoundLargeQueuesWithoutMutatingTheirContents(t *testing.T) {
	tracks := make([]music.Track, music.QueueLimit)
	for i := range tracks {
		tracks[i] = music.Track{ID: fmt.Sprintf("track-%03d", i), Provider: music.ProviderYouTube,
			CanonicalID: "abcdefghijk", Title: strings.Repeat("t", music.MaxMetadataBytes),
			Artist: strings.Repeat("a", 128), Album: strings.Repeat("b", 128), RequesterID: "member"}
	}
	state, err := music.Apply(music.InitialState(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: tracks})
	if err != nil {
		t.Fatal(err)
	}
	state, err = music.Apply(state, state.Revision, music.Operation{Kind: music.OpResume})
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []protocol.Operation{protocol.OpMusicState, protocol.OpMusicPause} {
		t.Run(fmt.Sprintf("operation_%d", operation), func(t *testing.T) {
			community, channel := id.New(), id.New()
			store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: state, Revision: state.Revision}}
			beforeQueue := append([]music.Track(nil), state.Queue...)
			authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, dj: true}
			h := MusicInteractions{Store: store, Authority: authority, CommunityID: community,
				OnImmediateControl: func(ctx context.Context, _, _, ch id.ID, op music.Operation) (music.State, error) {
					next, err := music.Apply(store.snapshot.State, store.snapshot.Revision, op)
					if err != nil {
						return music.State{}, err
					}
					saved, err := store.Save(ctx, ch, store.snapshot.Revision, next)
					return saved.State, err
				}}
			request := protocol.Request{RequestID: id.New(), CommunityID: community, ChannelID: &channel, Operation: operation}
			result, err := h.Handlers()[operation].Handle(t.Context(), InteractionActor{User: id.New()}, request)
			if err != nil || result.Status != 0 {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if _, err := protocol.EncodeResponseCBOR(protocol.Response{RequestID: request.RequestID, CommunityID: community,
				Status: result.Status, Revision: result.Revision, Data: result.Data}); err != nil {
				t.Fatalf("snapshot cannot be sent as a protocol response: %v", err)
			}
			var fields []any
			if err := cborx.Unmarshal(result.Data, &fields); err != nil {
				t.Fatal(err)
			}
			if len(fields) != 13 || fields[12] != uint64(len(beforeQueue)) {
				t.Fatalf("snapshot fields=%d, want 13 with original queue total %d", len(fields), len(beforeQueue))
			}
			prefix, ok := fields[3].([]any)
			if !ok || len(prefix) == 0 || len(prefix) >= len(beforeQueue) {
				t.Fatalf("queue prefix=%#v, want nonempty truncated prefix", fields[3])
			}
			wantCurrent, _ := encodeMusicTrack(state.Current)
			if !reflect.DeepEqual(fields[2], wantCurrent) {
				t.Fatal("snapshot changed the current track")
			}
			for i, row := range prefix {
				want, _ := encodeMusicTrack(&beforeQueue[i])
				if !reflect.DeepEqual(row, want) {
					t.Fatalf("queue row %d is not the original prefix row", i)
				}
			}
			nextRow, _ := encodeMusicTrack(&beforeQueue[len(prefix)])
			fields[3] = append(prefix, nextRow)
			tooLarge, err := cborx.Marshal(fields)
			if err != nil || len(tooLarge) <= 2400 {
				t.Fatalf("prefix is not maximal: next row gives bytes=%d err=%v", len(tooLarge), err)
			}
			wantSaves := 0
			if operation == protocol.OpMusicPause {
				wantSaves = 1
			}
			if store.saves != wantSaves || !reflect.DeepEqual(store.snapshot.State.Queue, beforeQueue) {
				t.Fatalf("snapshot truncation changed durable queue contents or added saves: saves=%d want=%d", store.saves, wantSaves)
			}
		})
	}
}

func TestMusicSnapshotKeepsFullLegacyShapeWhenItFits(t *testing.T) {
	state := music.InitialState()
	state.Queue = []music.Track{{ID: "track", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "Track", RequesterID: "member"}}
	data, err := encodeMusicSnapshot(state, false, 123)
	if err != nil {
		t.Fatal(err)
	}
	var fields []any
	if err := cborx.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 12 {
		t.Fatalf("snapshot fields=%d, want unchanged 12-slot shape", len(fields))
	}
	queue, ok := fields[3].([]any)
	if !ok || len(queue) != 1 {
		t.Fatalf("queue=%#v, want full queue", fields[3])
	}
}
