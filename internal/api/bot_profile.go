package api

import (
	"context"
	"net/http"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/store"
)

// PatchBotProfile changes the managed bot's display name for its community.
// The bot handle is an immutable account identifier and is never changed here.
func (d Deps) PatchBotProfile(w http.ResponseWriter, r *http.Request) {
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
	var req struct {
		_       struct{} `cbor:",toarray"`
		Display string
	}
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
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
	display, err := auth.NormalizeDisplay(req.Display)
	if err != nil || display == "" {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "invalid bot display name"))
		return
	}
	err = d.Repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := tx.LockBotCommunity(r.Context(), cid); err != nil {
			return notFound(err)
		}
		if _, _, err := humanForBot(r, tx, cid, s); err != nil {
			return err
		}
		installation, err := tx.GetActiveBotInstallationByCommunity(r.Context(), cid)
		if err != nil {
			return notFound(err)
		}
		if installation.UserID == nil {
			return server.Errorf(server.CodeUnavailable, "managed bot is not enrolled")
		}
		bot, err := tx.GetUser(r.Context(), *installation.UserID)
		if err != nil || bot.Kind != 1 {
			return server.Errorf(server.CodeUnavailable, "managed bot identity is unavailable")
		}
		if _, err := tx.GetMember(r.Context(), cid, bot.ID); err != nil {
			return server.Errorf(server.CodeUnavailable, "managed bot membership is unavailable")
		}
		_, err = tx.UpdateProfile(r.Context(), bot.ID, &display, nil, nil)
		return err
	})
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	if d.BotProfileChanged != nil {
		d.BotProfileChanged(context.WithoutCancel(r.Context()), cid)
	}
	d.write(w, r, http.StatusOK, []any{display})
}
