package api

import (
	"errors"
	"net/http"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

func (h *Roles) getBotJoinRole(w http.ResponseWriter, r *http.Request) {
	cid, actor, err := h.selfRoleCommunity(r, h.repo)
	if err != nil {
		h.fail(w, r, "get bot join role", err)
		return
	}
	snap, err := LoadSnapshot(r.Context(), h.repo, cid, actor.UserID, nil)
	if err != nil {
		h.fail(w, r, "get bot join role", notFound(err))
		return
	}
	if !snap.Resolve(actor.UserID).Has(PermManageRoles) {
		h.fail(w, r, "get bot join role", server.Errorf(server.CodeForbidden, "manage_roles required"))
		return
	}
	installation, err := h.repo.GetActiveBotInstallationByCommunity(r.Context(), cid)
	if errors.Is(err, store.ErrNotFound) || (err == nil && (installation.State != store.BotInstallationReady || installation.UserID == nil || installation.DeviceID == nil)) {
		h.fail(w, r, "get bot join role", server.Errorf(server.CodeNotFound, "ready community bot not found"))
		return
	}
	if err != nil {
		h.fail(w, r, "get bot join role", err)
		return
	}
	bot, err := h.repo.GetUser(r.Context(), *installation.UserID)
	if err != nil || bot.Kind != 1 {
		h.fail(w, r, "get bot join role", server.Errorf(server.CodeNotFound, "ready community bot not found"))
		return
	}
	if _, err := h.repo.GetMember(r.Context(), cid, bot.ID); err != nil {
		h.fail(w, r, "get bot join role", server.Errorf(server.CodeNotFound, "ready community bot not found"))
		return
	}
	policyRepo, ok := h.repo.(store.BotJoinRolePolicy)
	if !ok {
		h.fail(w, r, "get bot join role", errors.New("store does not support bot join role policy"))
		return
	}
	policy, err := policyRepo.GetBotJoinRolePolicy(r.Context(), installation.ID)
	if errors.Is(err, store.ErrNotFound) {
		_ = server.WriteCBOR(w, http.StatusOK, []any{nil, installation.ID})
		return
	}
	if err != nil {
		h.fail(w, r, "get bot join role", err)
		return
	}
	_ = server.WriteCBOR(w, http.StatusOK, []any{policy.RoleID, installation.ID})
}

type botJoinRoleRequest struct {
	_              struct{} `cbor:",toarray"`
	RequestID      id.ID
	RoleID         id.ID // zero clears the approved role
	InstallationID id.ID // fences a replacement installation
}

// configureBotJoinRole stores an approval only. Runtime assignment remains
// intentionally unwired until the endpoint can bind it to a live membership
// episode and an idempotent operation receipt.
func (h *Roles) configureBotJoinRole(w http.ResponseWriter, r *http.Request) {
	var req botJoinRoleRequest
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		refuseCBOR(w, r, err)
		return
	}
	cid, actor, err := h.selfRoleCommunity(r, h.repo)
	if err != nil {
		h.fail(w, r, "configure bot join role", err)
		return
	}
	if req.RequestID.IsZero() || req.InstallationID.IsZero() {
		h.fail(w, r, "configure bot join role", server.Errorf(server.CodeInvalidRequest, "request id required"))
		return
	}
	var installationID id.ID
	err = h.repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := tx.LockCommunity(r.Context(), cid); err != nil {
			return notFound(err)
		}
		if _, _, err := h.selfRoleCommunity(r, tx); err != nil {
			return err
		}
		snap, err := LoadSnapshot(r.Context(), tx, cid, actor.UserID, nil)
		if err != nil {
			return notFound(err)
		}
		actorBits := snap.Resolve(actor.UserID)
		if !actorBits.Has(PermManageRoles) {
			return server.Errorf(server.CodeForbidden, "manage_roles required")
		}
		installation, err := tx.GetActiveBotInstallationByCommunity(r.Context(), cid)
		if err != nil || installation.State != store.BotInstallationReady || installation.UserID == nil || installation.DeviceID == nil {
			return server.Errorf(server.CodeForbidden, "ready community bot required")
		}
		if installation.ID != req.InstallationID {
			return server.WithStatus(http.StatusConflict, server.Errorf(server.CodeInvalidRequest, "bot installation changed"))
		}
		bot, err := tx.GetUser(r.Context(), *installation.UserID)
		if err != nil || bot.Kind != 1 {
			return server.Errorf(server.CodeForbidden, "ready community bot required")
		}
		if _, err := tx.GetMember(r.Context(), cid, bot.ID); err != nil {
			return server.Errorf(server.CodeForbidden, "installed bot must be a community member")
		}
		policyRepo, ok := tx.(store.BotJoinRolePolicy)
		if !ok {
			return errors.New("store does not support bot join role policy")
		}
		if req.RoleID.IsZero() {
			if _, err := policyRepo.GetBotJoinRolePolicy(r.Context(), installation.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		} else {
			if err := h.selfRoleCandidate(r, tx, cid, actor.UserID, req.RoleID, actorBits, snap); err != nil {
				return err
			}
		}
		installationID = installation.ID
		return policyRepo.SetBotJoinRolePolicy(r.Context(), installation.ID, req.RoleID, actor.UserID, h.clk.Now().Unix())
	})
	if err != nil {
		h.fail(w, r, "configure bot join role", err)
		return
	}
	_ = server.WriteCBOR(w, http.StatusOK, []any{req.RequestID, installationID})
}
