package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// Plan 2 task 16 (P2-D22): 00011_voice.sql's voice_sessions, on both engines.
// A call is keyed by its call group's call id (R9), so putting the same call id
// again reopens the call rather than conflicting.
func TestVoiceSessionsOpenEndAndReopen(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			ch := channelIn(seedCommunity(ctx, t, repo), 1, 0, 0, "voice", 0)
			if err := repo.CreateChannel(ctx, ch); err != nil {
				t.Fatalf("CreateChannel: %v", err)
			}
			callID, group := id.New(), id.New()

			if _, err := repo.GetVoiceSession(ctx, callID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetVoiceSession before any call = %v, want ErrNotFound", err)
			}
			if err := repo.EndVoiceSession(ctx, callID, 5); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("EndVoiceSession of no call = %v, want ErrNotFound", err)
			}
			v := store.VoiceSessionRow{
				CallID: callID, ChannelID: ch.ID, GroupID: &group, LivekitRoom: "room-a", Started: 10,
			}
			if err := repo.PutVoiceSession(ctx, v); err != nil {
				t.Fatalf("PutVoiceSession: %v", err)
			}
			got, err := repo.GetVoiceSession(ctx, callID)
			if err != nil {
				t.Fatalf("GetVoiceSession: %v", err)
			}
			if got.ChannelID != ch.ID || got.GroupID == nil || *got.GroupID != group ||
				got.LivekitRoom != "room-a" || got.Started != 10 || got.Ended != nil {
				t.Fatalf("row = %+v", got)
			}
			live, err := repo.ListLiveVoiceSessions(ctx, ch.ID)
			if err != nil || len(live) != 1 || live[0].CallID != callID {
				t.Fatalf("ListLiveVoiceSessions = %+v, %v", live, err)
			}
			// Putting a live call again changes nothing: a second device starting
			// the same call must land in the room the first one opened.
			if err := repo.PutVoiceSession(ctx, store.VoiceSessionRow{
				CallID: callID, ChannelID: ch.ID, LivekitRoom: "room-late", Started: 11,
			}); err != nil {
				t.Fatalf("PutVoiceSession on a live call: %v", err)
			}
			if got, _ := repo.GetVoiceSession(ctx, callID); got.LivekitRoom != "room-a" || got.Started != 10 ||
				got.GroupID == nil {
				t.Fatalf("a live call was rewritten: %+v", got)
			}

			if err := repo.EndVoiceSession(ctx, callID, 20); err != nil {
				t.Fatalf("EndVoiceSession: %v", err)
			}
			got, _ = repo.GetVoiceSession(ctx, callID)
			if got.Ended == nil || *got.Ended != 20 {
				t.Fatalf("ended = %v, want 20", got.Ended)
			}
			// Ending an ended call changes nothing and says so.
			if err := repo.EndVoiceSession(ctx, callID, 30); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("EndVoiceSession twice = %v, want ErrNotFound", err)
			}
			if live, _ := repo.ListLiveVoiceSessions(ctx, ch.ID); len(live) != 0 {
				t.Fatalf("an ended call is still live: %+v", live)
			}

			// The next call of the same call group reopens the row.
			v.LivekitRoom, v.Started, v.GroupID = "room-b", 40, nil
			if err := repo.PutVoiceSession(ctx, v); err != nil {
				t.Fatalf("PutVoiceSession again: %v", err)
			}
			got, _ = repo.GetVoiceSession(ctx, callID)
			if got.Ended != nil || got.Started != 40 || got.LivekitRoom != "room-b" || got.GroupID != nil {
				t.Fatalf("reopened row = %+v", got)
			}

			// A voice session names a real channel.
			if err := repo.PutVoiceSession(ctx, store.VoiceSessionRow{
				CallID: id.New(), ChannelID: id.New(), LivekitRoom: "x", Started: 1,
			}); err == nil {
				t.Fatal("a voice session for no channel was accepted")
			}
		})
	}
}

// Invariant 11's "Live calls end": EndAllVoiceSessions closes the open call
// groups (Plan 1) and now also ends every live voice_sessions row, leaving an
// already-ended one's time alone.
func TestEndAllVoiceSessionsEndsEveryLiveCall(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			ch := channelIn(seedCommunity(ctx, t, repo), 1, 0, 0, "voice", 0)
			if err := repo.CreateChannel(ctx, ch); err != nil {
				t.Fatalf("CreateChannel: %v", err)
			}
			a, b, done := id.New(), id.New(), id.New()
			for _, c := range []id.ID{a, b, done} {
				if err := repo.PutVoiceSession(ctx, store.VoiceSessionRow{
					CallID: c, ChannelID: ch.ID, LivekitRoom: c.String(), Started: 1,
				}); err != nil {
					t.Fatalf("PutVoiceSession: %v", err)
				}
			}
			if err := repo.EndVoiceSession(ctx, done, 2); err != nil {
				t.Fatalf("EndVoiceSession: %v", err)
			}
			if err := repo.EndAllVoiceSessions(ctx, 9); err != nil {
				t.Fatalf("EndAllVoiceSessions: %v", err)
			}
			for c, want := range map[id.ID]int64{a: 9, b: 9, done: 2} {
				got, err := repo.GetVoiceSession(ctx, c)
				if err != nil || got.Ended == nil || *got.Ended != want {
					t.Fatalf("call %s ended = %v (%v), want %d", c, got.Ended, err, want)
				}
			}
			// Inside a transaction too: the restore path runs it there.
			if err := repo.PutVoiceSession(ctx, store.VoiceSessionRow{
				CallID: a, ChannelID: ch.ID, LivekitRoom: "again", Started: 10,
			}); err != nil {
				t.Fatalf("reopen: %v", err)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				return tx.EndAllVoiceSessions(ctx, 11)
			}); err != nil {
				t.Fatalf("EndAllVoiceSessions in a Tx: %v", err)
			}
			if got, _ := repo.GetVoiceSession(ctx, a); got.Ended == nil || *got.Ended != 11 {
				t.Fatalf("ended in a Tx = %v, want 11", got.Ended)
			}
		})
	}
}
