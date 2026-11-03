package api

import (
	"errors"
	"net/http"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// GetBotActorAuthority returns the current community-wide permissions of a
// member for the bot bound to this ready installation. The actor is always
// re-resolved from membership and role state; callers cannot supply authority.
func (d Deps) GetBotActorAuthority(w http.ResponseWriter, r *http.Request) {
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
	if _, err := d.Repo.GetMember(r.Context(), cid, actor); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			server.WriteError(w, notFound(err))
		} else {
			server.WriteError(w, d.storeError(r, err))
		}
		return
	}
	snap, err := LoadSnapshot(r.Context(), d.Repo, cid, actor, nil)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			server.WriteError(w, notFound(err))
		} else {
			server.WriteError(w, d.storeError(r, err))
		}
		return
	}
	owner := actor == snap.Owner
	bits := snap.Resolve(actor)
	admin := bits.Has(PermAdministrator)
	var ownerFlag, adminFlag uint64
	if owner {
		ownerFlag = 1
	}
	if admin {
		adminFlag = 1
	}
	roles, err := d.Repo.ListMemberRoles(r.Context(), cid, actor)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	roleValues := make([]any, len(roles))
	for i := range roles {
		roleValues[i] = roles[i]
	}
	d.write(w, r, http.StatusOK, []any{uint64(bits), ownerFlag, adminFlag, roleValues})
}
