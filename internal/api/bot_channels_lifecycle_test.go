package api_test

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// A Bot channel stops being one when its channel can no longer be one: a PATCH that makes it
// readable, a channel delete and a community delete each remove the mark in their own transaction
// and tell the Bot feed after the commit.
func TestBotChannelMarkGoesWithItsChannel(t *testing.T) {
	e := newEnv(t)
	log := slog.New(slog.DiscardHandler)
	events := &channelEvents{}
	api.NewCommunities(e.Repo, e.DS, e.Clk, log).WithBotChannelEvents(events).Register(e.Mux)
	api.NewChannels(e.Repo, e.DS, e.Clk, 10, log).WithBotChannelEvents(events).Register(e.Mux)
	owner, ownerTok := e.NewUser("owner")
	cid := createCommunity(t, e, ownerTok)
	ctx := context.Background()
	mark := func(ch id.ID) {
		t.Helper()
		if _, err := e.Repo.MarkBotChannel(ctx, store.BotChannelRow{ChannelID: ch, CommunityID: cid, MarkedAt: 1, MarkedBy: owner}); err != nil {
			t.Fatal(err)
		}
	}
	text := func(name string) id.ID {
		t.Helper()
		ch, _, status := newChannel(t, e, cid, ownerTok, uint64(api.ChannelText), uint64(api.ModeE2EE), 0, name)
		if status != http.StatusCreated {
			t.Fatalf("create %s = %d", name, status)
		}
		mark(ch)
		return ch
	}
	patched, deleted, kept := text("polls"), text("bot"), text("welcome")
	unmarked := func(ch id.ID) string { return "unmarked " + cid.String() + " " + ch.String() }

	// Renaming keeps the mark; switching to readable drops it.
	if status, body := e.Do(http.MethodPatch, "/v1/channels/"+patched.String(), ownerTok, []any{"polls2", nil, nil, nil, nil, nil, nil}); status != http.StatusNoContent {
		t.Fatalf("rename = %d %x", status, body)
	}
	if got := events.list(); len(got) != 0 {
		t.Fatalf("a rename unmarked: %v", got)
	}
	if status, body := e.Do(http.MethodPatch, "/v1/channels/"+patched.String(), ownerTok, []any{nil, nil, uint64(api.ModeReadable), nil, nil, nil, nil}); status != http.StatusNoContent {
		t.Fatalf("make readable = %d %x", status, body)
	}
	if got := events.list(); !slices.Equal(got, []string{unmarked(patched)}) {
		t.Fatalf("events after readable = %v", got)
	}
	if status, body := e.Do(http.MethodDelete, "/v1/channels/"+deleted.String(), ownerTok, nil); status != http.StatusNoContent {
		t.Fatalf("delete channel = %d %x", status, body)
	}
	if got := events.list(); !slices.Equal(got, []string{unmarked(patched), unmarked(deleted)}) {
		t.Fatalf("events after delete = %v", got)
	}
	rows, err := e.Repo.ListBotChannels(ctx, cid)
	if err != nil || len(rows) != 1 || rows[0].ChannelID != kept {
		t.Fatalf("Bot channels = %+v, %v; want only %s", rows, err, kept)
	}
	// The PATCH and DELETE removed their rows, not only hid them: re-making the channel text would not
	// bring a mark back.
	if removed, err := e.Repo.UnmarkBotChannel(ctx, cid, patched); err != nil || removed {
		t.Fatalf("the readable channel's row survived (%v, %v)", removed, err)
	}

	if err := e.Repo.PutBotCatalog(ctx, store.BotCatalogRow{CommunityID: cid, Revision: 1, Body: []byte{0x80}}); err != nil {
		t.Fatal(err)
	}
	if status, body := e.Do(http.MethodDelete, "/v1/communities/"+cid.String(), ownerTok, nil); status != http.StatusNoContent {
		t.Fatalf("delete community = %d %x", status, body)
	}
	if got := events.list(); !slices.Equal(got, []string{unmarked(patched), unmarked(deleted), unmarked(kept)}) {
		t.Fatalf("events after community delete = %v", got)
	}
	if _, err := e.Repo.GetBotCatalog(ctx, cid); err == nil {
		t.Fatal("the deleted community's catalog survived")
	}
	if removed, err := e.Repo.UnmarkBotChannel(ctx, cid, kept); err != nil || removed {
		t.Fatalf("the deleted community's mark survived (%v, %v)", removed, err)
	}
}
