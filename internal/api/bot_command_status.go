package api

import (
	"errors"
	"net/http"

	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/store"
)

// GetBotCommandStatus exposes the minimum live installation lifecycle data
// needed by ordinary community members to decide whether bot commands can run.
// Configuration, capabilities, and credential details remain manager-only.
func (d Deps) GetBotCommandStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
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
	user, err := d.Repo.GetUser(r.Context(), s.UserID)
	if err != nil {
		server.WriteError(w, notFound(err))
		return
	}
	if user.Kind != kindHuman {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "human session required"))
		return
	}
	if _, err := d.Repo.GetMember(r.Context(), cid, s.UserID); err != nil {
		server.WriteError(w, notFound(err))
		return
	}
	if err := requireBotWire(s.WireVersion); err != nil {
		server.WriteError(w, err)
		return
	}
	row, err := d.Repo.GetActiveBotInstallationByCommunity(r.Context(), cid)
	if errors.Is(err, store.ErrNotFound) {
		d.write(w, r, http.StatusOK, nil)
		return
	}
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	var botUser any
	if row.UserID != nil {
		botUser = *row.UserID
	}
	d.write(w, r, http.StatusOK, []any{uint64(1), row.ID, botUser, uint64(row.State), row.Revision})
}
