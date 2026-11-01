package api

import (
	"errors"
	"net/http"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/store"
)

// GetBotActorChannelAuthority resolves a human member's current permissions
// in one channel. The bot session is checked for channel visibility, but its
// own permission bits are never returned as the actor's authority.
func (d Deps) GetBotActorChannelAuthority(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	s, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	if s.Scope != auth.ScopeEnrolled || s.Bot == nil || s.Bot.State != store.BotInstallationReady {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "ready bot session required"))
		return
	}
	cid, err := server.PathID(r, "community")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	actor, err := server.PathID(r, "actor")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	channelID, err := server.PathID(r, "channel")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if cid != s.Bot.CommunityID {
		server.WriteError(w, server.Errorf(server.CodeNotFound, "bot installation absent"))
		return
	}
	installation, err := d.Repo.GetActiveBotInstallationByCommunity(r.Context(), cid)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			server.WriteError(w, notFound(err))
		} else {
			server.WriteError(w, d.storeError(r, err))
		}
		return
	}
	if installation.ID != s.Bot.InstallationID || installation.State != store.BotInstallationReady ||
		installation.UserID == nil || *installation.UserID != s.UserID || installation.DeviceID == nil || *installation.DeviceID != s.DeviceID {
		server.WriteError(w, server.Errorf(server.CodeNotFound, "bot installation absent"))
		return
	}
	if err := requireBotWire(s.WireVersion); err != nil {
		server.WriteError(w, err)
		return
	}
	channel, err := d.Repo.GetChannel(r.Context(), channelID)
	if err != nil {
		server.WriteError(w, notFound(err))
		return
	}
	if channel.CommunityID == nil || *channel.CommunityID != cid {
		server.WriteError(w, server.Errorf(server.CodeNotFound, "no such object"))
		return
	}
	if err := requireBotActorMember(r, d, cid, s.UserID); err != nil {
		server.WriteError(w, err)
		return
	}
	if err := requireBotActorMember(r, d, cid, actor); err != nil {
		server.WriteError(w, err)
		return
	}
	actorUser, err := d.Repo.GetUser(r.Context(), actor)
	if err != nil {
		server.WriteError(w, notFound(err))
		return
	}
	if actorUser.Kind != kindHuman {
		server.WriteError(w, server.Errorf(server.CodeNotFound, "no such object"))
		return
	}
	// Timeouts are an independent part of command authority: a timed-out human
	// cannot use utility commands even if their role bits still include send.
	if err := requireNotTimedOut(r.Context(), d.Repo, cid, actor, d.Clock.Now().Unix()); err != nil {
		server.WriteError(w, err)
		return
	}
	botSnapshot, err := LoadSnapshot(r.Context(), d.Repo, cid, s.UserID, &channelID)
	if err != nil {
		server.WriteError(w, notFound(err))
		return
	}
	if !botSnapshot.ResolveChannel(s.UserID).Has(PermViewChannel) {
		server.WriteError(w, server.Errorf(server.CodeNotFound, "no such object"))
		return
	}
	actorSnapshot, err := LoadSnapshot(r.Context(), d.Repo, cid, actor, &channelID)
	if err != nil {
		server.WriteError(w, notFound(err))
		return
	}
	bits := actorSnapshot.ResolveChannel(actor)
	d.write(w, r, http.StatusOK, []any{cid, actor, channelID, uint64(bits)})
}

func requireBotActorMember(r *http.Request, d Deps, community, user id.ID) error {
	if _, err := d.Repo.GetMember(r.Context(), community, user); err != nil {
		return notFound(err)
	}
	return nil
}
