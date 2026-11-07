package store_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// Bot v2 Task 9: the catalog, the command-key publication, Bot channels and the state revisions,
// on both engines.

func TestStoreBotCatalogRoundTripsAndReplaces(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			cid := seedCommunity(ctx, t, repo)
			if _, err := repo.GetBotCatalog(ctx, cid); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetBotCatalog before any publish = %v, want ErrNotFound", err)
			}
			first := store.BotCatalogRow{CommunityID: cid, Revision: 1, Body: []byte{0x80}}
			if err := repo.PutBotCatalog(ctx, first); err != nil {
				t.Fatalf("PutBotCatalog: %v", err)
			}
			second := store.BotCatalogRow{CommunityID: cid, Revision: 2, Body: []byte{0x81, 0x01}}
			if err := repo.PutBotCatalog(ctx, second); err != nil {
				t.Fatalf("PutBotCatalog (replace): %v", err)
			}
			got, err := repo.GetBotCatalog(ctx, cid)
			if err != nil || got.Revision != 2 || !bytes.Equal(got.Body, second.Body) || got.CommunityID != cid {
				t.Fatalf("GetBotCatalog = %+v, %v; want the second publish", got, err)
			}
			if err := repo.PutBotCatalog(ctx, store.BotCatalogRow{CommunityID: cid, Revision: 0, Body: []byte{0x80}}); err == nil {
				t.Fatal("a zero revision was stored")
			}
		})
	}
}

func TestStoreBotCommandKeyRoundTripsAndReplaces(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			cid := seedCommunity(ctx, t, repo)
			if _, err := repo.GetBotCommandKey(ctx, cid); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetBotCommandKey before any publish = %v, want ErrNotFound", err)
			}
			for i, row := range []store.BotCommandKeyRow{
				{CommunityID: cid, KeyID: id.New(), Publication: bytes.Repeat([]byte{1}, 180), PublishedAt: 20},
				{CommunityID: cid, KeyID: id.New(), Publication: bytes.Repeat([]byte{2}, 181), PublishedAt: 21},
			} {
				if err := repo.PutBotCommandKey(ctx, row); err != nil {
					t.Fatalf("PutBotCommandKey %d: %v", i, err)
				}
				got, err := repo.GetBotCommandKey(ctx, cid)
				if err != nil || got.CommunityID != cid || got.KeyID != row.KeyID || got.PublishedAt != row.PublishedAt ||
					!bytes.Equal(got.Publication, row.Publication) {
					t.Fatalf("GetBotCommandKey %d = %+v, %v; want %+v", i, got, err, row)
				}
			}
		})
	}
}

func TestStoreBotChannelMarkUnmarkList(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			cid := seedCommunity(ctx, t, repo)
			admin := seedUser(ctx, t, repo)
			text, other := channelIn(cid, 0, 0, 0, "bot", 1), channelIn(cid, 0, 0, 0, "polls", 2)
			for _, ch := range []store.ChannelRow{text, other} {
				if err := repo.CreateChannel(ctx, ch); err != nil {
					t.Fatalf("CreateChannel: %v", err)
				}
			}
			if rows, err := repo.ListBotChannels(ctx, cid); err != nil || rows == nil || len(rows) != 0 {
				t.Fatalf("ListBotChannels (none) = %#v, %v; want an empty non-nil slice", rows, err)
			}
			mark := store.BotChannelRow{ChannelID: text.ID, CommunityID: cid, MarkedAt: 30, MarkedBy: admin.ID}
			if created, err := repo.MarkBotChannel(ctx, mark); err != nil || !created {
				t.Fatalf("MarkBotChannel = %v, %v; want a new mark", created, err)
			}
			if created, err := repo.MarkBotChannel(ctx, store.BotChannelRow{ChannelID: text.ID, CommunityID: cid, MarkedAt: 31, MarkedBy: admin.ID}); err != nil || created {
				t.Fatalf("MarkBotChannel again = %v, %v; want the existing mark kept", created, err)
			}
			if _, err := repo.MarkBotChannel(ctx, store.BotChannelRow{ChannelID: other.ID, CommunityID: cid, MarkedAt: 32, MarkedBy: admin.ID}); err != nil {
				t.Fatalf("MarkBotChannel other: %v", err)
			}
			rows, err := repo.ListBotChannels(ctx, cid)
			if err != nil || len(rows) != 2 {
				t.Fatalf("ListBotChannels = %+v, %v; want two", rows, err)
			}
			found := map[id.ID]store.BotChannelRow{}
			for _, r := range rows {
				found[r.ChannelID] = r
			}
			if found[text.ID] != mark {
				t.Fatalf("the first mark = %+v, want %+v (a second mark keeps the first)", found[text.ID], mark)
			}
			if removed, err := repo.UnmarkBotChannel(ctx, cid, text.ID); err != nil || !removed {
				t.Fatalf("UnmarkBotChannel = %v, %v; want removed", removed, err)
			}
			if removed, err := repo.UnmarkBotChannel(ctx, cid, text.ID); err != nil || removed {
				t.Fatalf("UnmarkBotChannel again = %v, %v; want nothing removed", removed, err)
			}
			// A soft-deleted channel is not listed.
			if err := repo.DeleteChannel(ctx, other.ID, 40); err != nil {
				t.Fatalf("DeleteChannel: %v", err)
			}
			if rows, err := repo.ListBotChannels(ctx, cid); err != nil || len(rows) != 0 {
				t.Fatalf("ListBotChannels after unmark and delete = %+v, %v; want none", rows, err)
			}
		})
	}
}

func TestStoreBotStateRevisionsBumpAndReadByScope(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			user := seedUser(ctx, t, repo)
			chA, chB, call := id.New(), id.New(), id.New()
			if rev, rows, err := repo.BotStateRevisions(ctx, user.ID, []id.ID{chA}); err != nil || rev != 0 || len(rows) != 0 {
				t.Fatalf("BotStateRevisions (none) = %d %+v %v", rev, rows, err)
			}
			for want := uint64(1); want <= 3; want++ {
				got, err := repo.BumpBotStateRevision(ctx, store.BotScopeUser, user.ID)
				if err != nil || got != want {
					t.Fatalf("BumpBotStateRevision user = %d, %v; want %d", got, err, want)
				}
			}
			if _, err := repo.BumpBotStateRevision(ctx, store.BotScopeChannel, chA); err != nil {
				t.Fatal(err)
			}
			if _, err := repo.BumpBotStateRevision(ctx, store.BotScopeChannel, chB); err != nil {
				t.Fatal(err)
			}
			if got, err := repo.BumpBotStateRevision(ctx, store.BotScopeChannel, chB); err != nil || got != 2 {
				t.Fatalf("second channel bump = %d, %v; want 2", got, err)
			}
			if _, err := repo.BumpBotStateRevision(ctx, store.BotScopeCall, call); err != nil {
				t.Fatal(err)
			}
			if _, err := repo.BumpBotStateRevision(ctx, 3, call); err == nil {
				t.Fatal("scope kind 3 was accepted")
			}
			rev, rows, err := repo.BotStateRevisions(ctx, user.ID, []id.ID{chB, call, id.New()})
			if err != nil || rev != 3 {
				t.Fatalf("BotStateRevisions = %d, %v; want user revision 3", rev, err)
			}
			want := map[store.BotStateRevisionRow]bool{
				{ScopeKind: store.BotScopeChannel, ScopeID: chB, Revision: 2}: true,
				{ScopeKind: store.BotScopeCall, ScopeID: call, Revision: 1}:   true,
			}
			if len(rows) != len(want) {
				t.Fatalf("rows = %+v, want exactly chB and the call (chA was not asked for)", rows)
			}
			for _, r := range rows {
				if !want[r] {
					t.Fatalf("unexpected row %+v", r)
				}
			}
		})
	}
}

func TestStoreBotChannelListAndCommunityDeleteDropState(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			cid, other := seedCommunity(ctx, t, repo), seedCommunity(ctx, t, repo)
			admin := seedUser(ctx, t, repo)
			text, kept := channelIn(cid, 0, 0, 0, "bot", 1), channelIn(other, 0, 0, 0, "bot", 1)
			for _, ch := range []store.ChannelRow{text, kept} {
				if err := repo.CreateChannel(ctx, ch); err != nil {
					t.Fatal(err)
				}
			}
			for _, row := range []store.BotChannelRow{
				{ChannelID: text.ID, CommunityID: cid, MarkedAt: 1, MarkedBy: admin.ID},
				{ChannelID: kept.ID, CommunityID: other, MarkedAt: 1, MarkedBy: admin.ID},
			} {
				if _, err := repo.MarkBotChannel(ctx, row); err != nil {
					t.Fatal(err)
				}
			}
			// A channel that is no longer an end-to-end text channel is not listed, whatever its row says.
			readable := text
			readable.Mode, readable.Visibility = 1, 2
			if err := repo.UpdateChannel(ctx, readable); err != nil {
				t.Fatal(err)
			}
			if rows, err := repo.ListBotChannels(ctx, cid); err != nil || len(rows) != 0 {
				t.Fatalf("ListBotChannels of a readable channel = %+v, %v; want none", rows, err)
			}
			if err := repo.UpdateChannel(ctx, text); err != nil {
				t.Fatal(err)
			}
			if err := repo.PutBotCatalog(ctx, store.BotCatalogRow{CommunityID: cid, Revision: 1, Body: []byte{0x80}}); err != nil {
				t.Fatal(err)
			}
			if err := repo.PutBotCommandKey(ctx, store.BotCommandKeyRow{CommunityID: cid, KeyID: id.New(), Publication: []byte{1}, PublishedAt: 1}); err != nil {
				t.Fatal(err)
			}
			if _, err := repo.DeleteBotCommunityState(ctx, cid); err == nil {
				t.Fatal("DeleteBotCommunityState ran outside a transaction")
			}
			var unmarked []id.ID
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				var err error
				unmarked, err = tx.DeleteBotCommunityState(ctx, cid)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if len(unmarked) != 1 || unmarked[0] != text.ID {
				t.Fatalf("unmarked = %v, want [%s]", unmarked, text.ID)
			}
			if _, err := repo.GetBotCatalog(ctx, cid); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("catalog after delete: %v", err)
			}
			if _, err := repo.GetBotCommandKey(ctx, cid); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("command key after delete: %v", err)
			}
			if rows, err := repo.ListBotChannels(ctx, other); err != nil || len(rows) != 1 {
				t.Fatalf("the other community's mark = %+v, %v; want kept", rows, err)
			}
		})
	}
}
