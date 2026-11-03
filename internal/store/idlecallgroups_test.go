package store_test

import (
	"context"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// web-3 task 3b, L-HTTP-105 (RV-2): the idle call-group close lists the open call groups no live
// voice session names and created at or before the cutoff, oldest first, at most limit of them.
func TestListIdleCallGroups(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			ch := channelIn(seedCommunity(ctx, t, repo), 1, 0, 0, "voice", 0)
			if err := repo.CreateChannel(ctx, ch); err != nil {
				t.Fatalf("CreateChannel: %v", err)
			}
			call := func(created int64) store.GroupRow {
				t.Helper()
				g := store.GroupRow{
					GroupID: id.New(), Binding: []byte{0x80}, Kind: 1, CommunityID: ch.CommunityID, TargetID: ch.ID,
					CallID: &ch.ID, Ciphersuite: 1, ExternalSenderKeyID: id.New(), E2EEVersion: 1,
					MediaVersion: 1, PolicyVersion: 1, Created: created,
				}
				if err := repo.CreateGroup(ctx, g); err != nil {
					t.Fatalf("CreateGroup: %v", err)
				}
				return g
			}
			older, old, atCutoff := call(100), call(200), call(300)
			call(301)                   // newer than the cutoff
			seedGroup(ctx, t, repo, 50) // a text group, never a call group's business
			closed := call(150)
			if err := repo.CloseGroup(ctx, closed.GroupID, 160); err != nil {
				t.Fatalf("CloseGroup: %v", err)
			}
			live := call(120)
			if err := repo.PutVoiceSession(ctx, store.VoiceSessionRow{
				CallID: id.New(), ChannelID: ch.ID, GroupID: &live.GroupID, LivekitRoom: "r", Started: 130,
			}); err != nil {
				t.Fatalf("PutVoiceSession: %v", err)
			}
			// An ended session naming a group keeps nothing open.
			ended := call(110)
			endedCall := id.New()
			if err := repo.PutVoiceSession(ctx, store.VoiceSessionRow{
				CallID: endedCall, ChannelID: ch.ID, GroupID: &ended.GroupID, LivekitRoom: "e", Started: 111,
			}); err != nil {
				t.Fatalf("PutVoiceSession: %v", err)
			}
			if err := repo.EndVoiceSession(ctx, endedCall, 112); err != nil {
				t.Fatalf("EndVoiceSession: %v", err)
			}

			got, err := repo.ListIdleCallGroups(ctx, 300, 10)
			if err != nil {
				t.Fatalf("ListIdleCallGroups: %v", err)
			}
			want := []id.ID{older.GroupID, ended.GroupID, old.GroupID, atCutoff.GroupID}
			if len(got) != len(want) {
				t.Fatalf("idle groups = %d, want %d: %+v", len(got), len(want), got)
			}
			for i, g := range got {
				if g.GroupID != want[i] {
					t.Fatalf("idle group %d = %s, want %s", i, g.GroupID, want[i])
				}
			}
			if got, err := repo.ListIdleCallGroups(ctx, 300, 2); err != nil || len(got) != 2 ||
				got[0].GroupID != older.GroupID || got[1].GroupID != ended.GroupID {
				t.Fatalf("limited idle groups = %+v, %v, want the two oldest", got, err)
			}
		})
	}
}
