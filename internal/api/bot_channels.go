package api

import (
	"net/http"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// Bot v2 Task 9: Bot channels (spec "Bot channels", decision 7). An admin marks a text channel as a
// Bot channel; the Bot is an MLS member of, and posts in, only those. Marking and listing take the
// authority PUT /v1/communities/{community}/bot takes (humanForBot: a human member holding
// manage_community).

// BotChannelEvents hears that a Bot channel was marked or unmarked, after the change commits and
// before the route answers. Task 12's Bot feed implements it; Deps defaults to NoBotChannelEvents.
type BotChannelEvents interface {
	BotChannelMarked(community, channel id.ID)
	BotChannelUnmarked(community, channel id.ID)
}

// NoBotChannelEvents is the BotChannelEvents of a dillad with no Bot feed: it hears nothing.
type NoBotChannelEvents struct{}

func (NoBotChannelEvents) BotChannelMarked(id.ID, id.ID)   {}
func (NoBotChannelEvents) BotChannelUnmarked(id.ID, id.ID) {}

func registerBotChannels(m *server.Mux, d Deps) {
	m.SocketAuthed("PUT /v1/communities/{community}/channels/{channel}/bot-channel", d.socketChain(auth.ScopeEnrolled, dsClassWrite), http.HandlerFunc(d.MarkBotChannel))
	m.SocketAuthed("DELETE /v1/communities/{community}/channels/{channel}/bot-channel", d.socketChain(auth.ScopeEnrolled, dsClassWrite), http.HandlerFunc(d.UnmarkBotChannel))
	m.SocketAuthed("GET /v1/communities/{community}/bot/channels", d.socketChain(auth.ScopeEnrolled, dsClassRead), http.HandlerFunc(d.ListBotChannels))
}

func (d Deps) botChannelEvents() BotChannelEvents {
	if d.BotChannelEvents == nil {
		return NoBotChannelEvents{}
	}
	return d.BotChannelEvents
}

// botChannelTarget is the channel a mark names, which must be a live end-to-end text channel of
// cid that is not its AFK channel: the Bot joins the channel's MLS group, which a voice channel, a
// category, a readable channel and a DM do not offer it. A channel of another community, or none,
// is 404.
func botChannelTarget(r *http.Request, repo store.Repository, cid, channelID id.ID) error {
	ch, err := repo.GetChannel(r.Context(), channelID)
	if err != nil {
		return notFound(err)
	}
	if ch.CommunityID == nil || *ch.CommunityID != cid || ch.DeletedAt != nil {
		return server.Errorf(server.CodeNotFound, "channel")
	}
	if ch.Kind != ChannelText || ch.Mode != ModeE2EE {
		return server.Errorf(server.CodeInvalidRequest, "only a text channel can be a Bot channel")
	}
	afk, err := afkPointerOf(r.Context(), repo, cid)
	if err != nil {
		return err
	}
	if afk.ChannelID != nil && *afk.ChannelID == channelID {
		return server.Errorf(server.CodeInvalidRequest, "the AFK channel cannot be a Bot channel")
	}
	return nil
}

func botChannelPath(r *http.Request) (cid, channelID id.ID, err error) {
	if cid, err = server.PathID(r, "community"); err != nil {
		return
	}
	channelID, err = server.PathID(r, "channel")
	return
}

// MarkBotChannel marks a text channel as a Bot channel. A second mark keeps the first; the Bot feed
// hears only a new one. 204.
func (d Deps) MarkBotChannel(w http.ResponseWriter, r *http.Request) {
	s, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	cid, channelID, err := botChannelPath(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if _, _, err := humanForBot(r, d.Repo, cid, s); err != nil {
		server.WriteError(w, err)
		return
	}
	if err := requireBotWire(s.WireVersion); err != nil {
		server.WriteError(w, err)
		return
	}
	if err := botChannelTarget(r, d.Repo, cid, channelID); err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	created := false
	err = d.Repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := tx.LockBotCommunity(r.Context(), cid); err != nil {
			return notFound(err)
		}
		if _, _, err := humanForBot(r, tx, cid, s); err != nil {
			return err
		}
		if err := botChannelTarget(r, tx, cid, channelID); err != nil {
			return err
		}
		now := d.Clock.Now().Unix()
		if created, err = tx.MarkBotChannel(r.Context(), store.BotChannelRow{ChannelID: channelID, CommunityID: cid, MarkedAt: now, MarkedBy: s.UserID}); err != nil || !created {
			return err
		}
		return tx.Audit(r.Context(), store.AuditRow{Actor: &s.UserID, Action: "bot.channel_marked", Target: channelID.String(), Detail: cid.String(), At: now})
	})
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	if created {
		d.botChannelEvents().BotChannelMarked(cid, channelID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// UnmarkBotChannel removes a channel's Bot-channel mark. The Bot feed hears it before the answer, so
// the Bot leaves the channel's group without waiting for anything else. Unmarking a channel that is
// not marked is a 204 that tells nobody.
func (d Deps) UnmarkBotChannel(w http.ResponseWriter, r *http.Request) {
	s, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	cid, channelID, err := botChannelPath(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if _, _, err := humanForBot(r, d.Repo, cid, s); err != nil {
		server.WriteError(w, err)
		return
	}
	if err := requireBotWire(s.WireVersion); err != nil {
		server.WriteError(w, err)
		return
	}
	removed := false
	err = d.Repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := tx.LockBotCommunity(r.Context(), cid); err != nil {
			return notFound(err)
		}
		if _, _, err := humanForBot(r, tx, cid, s); err != nil {
			return err
		}
		if removed, err = tx.UnmarkBotChannel(r.Context(), cid, channelID); err != nil || !removed {
			return err
		}
		return tx.Audit(r.Context(), store.AuditRow{Actor: &s.UserID, Action: "bot.channel_unmarked", Target: channelID.String(), Detail: cid.String(), At: d.Clock.Now().Unix()})
	})
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	if removed {
		d.botChannelEvents().BotChannelUnmarked(cid, channelID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListBotChannels answers [[channel_id, marked_at, marked_by]…], oldest mark first.
func (d Deps) ListBotChannels(w http.ResponseWriter, r *http.Request) {
	s, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	cid, err := server.PathID(r, "community")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if _, _, err := humanForBot(r, d.Repo, cid, s); err != nil {
		server.WriteError(w, err)
		return
	}
	if err := requireBotWire(s.WireVersion); err != nil {
		server.WriteError(w, err)
		return
	}
	rows, err := d.Repo.ListBotChannels(r.Context(), cid)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	out := make([][]any, 0, len(rows))
	for _, row := range rows {
		at, ok := checkedUint64(row.MarkedAt)
		if !ok {
			server.WriteError(w, d.storeError(r, store.ErrConflict))
			return
		}
		out = append(out, []any{row.ChannelID, at, row.MarkedBy})
	}
	d.write(w, r, http.StatusOK, out)
}
