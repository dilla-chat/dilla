package api

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// Bot v2 Task 9: Bot channels (spec "Bot channels", decision 7). An admin marks a text channel as a
// Bot channel; the Bot is an MLS member of, and posts in, only those. Marking and unmarking take the
// authority PUT /v1/communities/{community}/bot takes (humanForBot: a human member holding
// manage_community); any human member lists the Bot channels they can view. A channel update that
// makes a Bot channel readable, a channel delete and a community delete unmark it in their own
// transaction (channels.go, communities.go).

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
	m.SocketAuthed("POST /v1/communities/{community}/channels/{channel}/bot-channel/re-add", d.socketChain(auth.ScopeEnrolled, dsClassWrite), http.HandlerFunc(d.ReAddBot))
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
func botChannelTarget(r *http.Request, repo store.Repository, cid, channelID, actor id.ID) error {
	ch, err := repo.GetChannel(r.Context(), channelID)
	if err != nil {
		return notFound(err)
	}
	if ch.CommunityID == nil || *ch.CommunityID != cid || ch.DeletedAt != nil {
		return server.Errorf(server.CodeNotFound, "channel")
	}
	if err := actorViews(r.Context(), repo, actor, ch); err != nil {
		return err
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
	if err := botChannelTarget(r, d.Repo, cid, channelID, s.UserID); err != nil {
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
		if err := botChannelTarget(r, tx, cid, channelID, s.UserID); err != nil {
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
		d.BotChannelsNotify.Changed(afterCommit(r), cid)
	}
	// Task 22: the Bot is proposed into the channel's group now (a second mark proposes again what is still missing);
	// a member of the group, the marking admin's client first, commits the Add, and the Bot joins from its Welcome.
	if err := d.proposeBotInto(afterCommit(r), channelID); err != nil {
		d.Log.WarnContext(r.Context(), "the Bot could not be proposed into its new Bot channel", "channel_id", channelID.String(), "error", err.Error())
	}
	w.WriteHeader(http.StatusNoContent)
}

// proposeBotInto materialises the channel's members and runs its group membership sync, which proposes the Bot's
// devices into a Bot channel's group (SyncGroupMembers). With no delivery service it proposes nothing.
func (d Deps) proposeBotInto(ctx context.Context, channelID id.ID) error {
	if d.BotMembership == nil {
		return nil
	}
	ch, err := d.Repo.GetChannel(ctx, channelID)
	if err != nil {
		return err
	}
	return MaterialiseChannelMembers(ctx, d.Repo, d.BotMembership, ch, d.Clock.Now().Unix())
}

// botAddVoider is the delivery service's VoidAddsOf (*ds.DS has it): it voids the named devices' outstanding Adds.
type botAddVoider interface {
	VoidAddsOf(ctx context.Context, groupID id.ID, devices []id.ID) (int, error)
}

// proposeBotOutOf takes the Bot out of an unmarked channel's text groups (Task 22 fix round 1, M1): its outstanding Adds
// are voided, so no member's commit carries it into a channel that is no longer a Bot channel, and a leaf it holds there
// is proposed out. Every group is attempted; the failures are returned together.
func (d Deps) proposeBotOutOf(ctx context.Context, cid, channelID id.ID) error {
	if d.BotMembership == nil {
		return nil
	}
	installation, err := d.Repo.GetActiveBotInstallationByCommunity(ctx, cid)
	if errors.Is(err, store.ErrNotFound) || err == nil && installation.UserID == nil {
		return nil
	}
	if err != nil {
		return err
	}
	bot := *installation.UserID
	devices, err := d.Repo.ListDevicesByUser(ctx, bot)
	if err != nil {
		return err
	}
	ids := make([]id.ID, 0, len(devices))
	for _, dev := range devices {
		ids = append(ids, dev.ID)
	}
	groups, err := d.Repo.GroupsForTarget(ctx, channelID, GroupText)
	if err != nil {
		return err
	}
	var errs []error
	for _, g := range groups {
		if voider, ok := d.BotMembership.(botAddVoider); ok && len(ids) > 0 {
			if _, err := voider.VoidAddsOf(ctx, g.GroupID, ids); err != nil {
				errs = append(errs, err)
			}
		}
		leaves, err := d.Repo.ListMembers(ctx, g.GroupID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, m := range leaves {
			if m.RemovedEpoch == nil && m.UserID == bot {
				if err := d.BotMembership.ProposeRemoveOf(ctx, g.GroupID, m.LeafIndex, m.DeviceID, id.New()); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	return errors.Join(errs...)
}

// actorViews is 404 unless actor holds view_channel in ch: a mark or unmark never acts in a channel its maker cannot see.
func actorViews(ctx context.Context, repo store.Repository, actor id.ID, ch store.ChannelRow) error {
	bits, err := NewResolver(repo).Resolve(ctx, actor, ch)
	if err != nil {
		return err
	}
	if !bits.Has(PermViewChannel) {
		return server.Errorf(server.CodeNotFound, "channel")
	}
	return nil
}

// ReAddBot is POST …/channels/{channel}/bot-channel/re-add (Task 22): the Bot page's "add the Bot again" for a Bot
// channel whose group the Bot cannot join on its own (its Welcome went stale: Task 16's needs re-add). It takes the
// authority a mark takes. While the Bot still holds a leaf in the channel's group, each such leaf is proposed out and the
// answer is [1]: ask again once that commit lands. Otherwise the Bot is proposed in and the answer is [0]. A channel that
// is not a Bot channel is 409 E_NOT_BOT_CHANNEL. A member of the group commits either proposal.
func (d Deps) ReAddBot(w http.ResponseWriter, r *http.Request) {
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
	ctx := r.Context()
	marked, err := d.Repo.ListBotChannels(ctx, cid)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	if !slices.ContainsFunc(marked, func(row store.BotChannelRow) bool { return row.ChannelID == channelID }) {
		server.WriteError(w, server.Errorf(server.CodeNotBotChannel, "the channel is not a Bot channel"))
		return
	}
	if ch, err := d.Repo.GetChannel(ctx, channelID); err != nil {
		server.WriteError(w, notFound(err))
		return
	} else if err := actorViews(ctx, d.Repo, s.UserID, ch); err != nil {
		server.WriteError(w, err)
		return
	}
	installation, err := d.Repo.GetActiveBotInstallationByCommunity(ctx, cid)
	if err != nil || installation.UserID == nil {
		server.WriteError(w, server.Errorf(server.CodeNotFound, "bot"))
		return
	}
	if d.BotMembership == nil {
		server.WriteError(w, server.Errorf(server.CodeUnavailable, "no delivery service"))
		return
	}
	groups, err := d.Repo.GroupsForTarget(ctx, channelID, GroupText)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	work := afterCommit(r)
	removing := false
	for _, g := range groups {
		leaves, err := d.Repo.ListMembers(ctx, g.GroupID)
		if err != nil {
			server.WriteError(w, d.storeError(r, err))
			return
		}
		for _, m := range leaves {
			if m.RemovedEpoch != nil || m.UserID != *installation.UserID {
				continue
			}
			removing = true
			if err := d.BotMembership.ProposeRemoveOf(work, g.GroupID, m.LeafIndex, m.DeviceID, id.New()); err != nil {
				d.Log.WarnContext(ctx, "the Bot's leaf could not be proposed out for a re-add", "channel_id", channelID.String(), "error", err.Error())
			}
		}
	}
	if err := d.Repo.Audit(ctx, store.AuditRow{Actor: &s.UserID, Action: "bot.channel_readd", Target: channelID.String(), Detail: cid.String(),
		At: d.Clock.Now().Unix()}); err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	state := uint64(1)
	if !removing {
		state = 0
		if err := d.proposeBotInto(work, channelID); err != nil {
			server.WriteError(w, d.storeError(r, err))
			return
		}
	}
	d.write(w, r, http.StatusOK, []any{state})
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
	// Task 22 fix round 1: an unmark, like a mark, needs view_channel there (a channel deleted since is no channel).
	if ch, err := d.Repo.GetChannel(r.Context(), channelID); err == nil && ch.CommunityID != nil && *ch.CommunityID == cid && ch.DeletedAt == nil {
		if err := actorViews(r.Context(), d.Repo, s.UserID, ch); err != nil {
			server.WriteError(w, err)
			return
		}
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
		d.BotChannelsNotify.Changed(afterCommit(r), cid)
		if err := d.proposeBotOutOf(afterCommit(r), cid, channelID); err != nil {
			d.Log.WarnContext(r.Context(), "the Bot could not be taken out of an unmarked channel", "channel_id", channelID.String(), "error", err.Error())
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListBotChannels answers [[channel_id, marked_at, marked_by]…], oldest mark first, to any human
// member: only the Bot channels that member can view, so no member learns a hidden channel's id.
// Administrators and the owner resolve to every permission and see them all.
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
	ctx := r.Context()
	user, err := d.Repo.GetUser(ctx, s.UserID)
	if err != nil {
		server.WriteError(w, notFound(err))
		return
	}
	if user.Kind != kindHuman {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "human session required"))
		return
	}
	if _, err := d.Repo.GetMember(ctx, cid, s.UserID); err != nil {
		server.WriteError(w, notFound(err))
		return
	}
	if _, err := d.Repo.GetCommunity(ctx, cid); err != nil {
		server.WriteError(w, notFound(err))
		return
	}
	if err := requireBotWire(s.WireVersion); err != nil {
		server.WriteError(w, err)
		return
	}
	rows, err := d.Repo.ListBotChannels(ctx, cid)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	channels := make([]store.ChannelRow, 0, len(rows))
	for _, row := range rows {
		ch, err := d.Repo.GetChannel(ctx, row.ChannelID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			server.WriteError(w, d.storeError(r, err))
			return
		}
		channels = append(channels, ch)
	}
	visible, err := viewableChannels(ctx, d.Repo, cid, s.UserID, channels)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	out := make([][]any, 0, len(rows))
	for _, row := range rows {
		if !visible[row.ChannelID] {
			continue
		}
		at, ok := checkedUint64(row.MarkedAt)
		if !ok {
			server.WriteError(w, d.storeError(r, store.ErrConflict))
			return
		}
		out = append(out, []any{row.ChannelID, at, row.MarkedBy})
	}
	d.write(w, r, http.StatusOK, out)
}
