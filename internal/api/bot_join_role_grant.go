package api

import (
	"errors"
	"net/http"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/store"
)

type botJoinRoleGrantRequest struct {
	_         struct{} `cbor:",toarray"`
	RequestID id.ID
	MemberID  id.ID
	EpisodeID id.ID
}

// GrantBotJoinRole assigns the currently approved role to one live membership
// episode. The durable receipt makes a retry stable after membership changes.
func (d Deps) GrantBotJoinRole(w http.ResponseWriter, r *http.Request) {
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
	if cid != s.Bot.CommunityID {
		server.WriteError(w, server.Errorf(server.CodeNotFound, "bot installation absent"))
		return
	}
	if err := requireBotWire(s.WireVersion); err != nil {
		server.WriteError(w, err)
		return
	}
	var req botJoinRoleGrantRequest
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		refuseCBOR(w, r, err)
		return
	}
	if req.RequestID.IsZero() || req.MemberID.IsZero() || req.EpisodeID.IsZero() {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "request id, member, and episode required"))
		return
	}
	var receipt store.BotJoinRoleGrantReceipt
	noPolicy := false
	err = d.Repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := tx.LockCommunity(r.Context(), cid); err != nil {
			return notFound(err)
		}
		installation, err := tx.GetActiveBotInstallationByCommunity(r.Context(), cid)
		if err != nil || installation.ID != s.Bot.InstallationID || installation.State != store.BotInstallationReady ||
			installation.UserID == nil || *installation.UserID != s.UserID || installation.DeviceID == nil || *installation.DeviceID != s.DeviceID {
			return server.Errorf(server.CodeForbidden, "ready bot installation required")
		}
		if installation.Capabilities&uint16(botCapRoles) == 0 || s.Bot.Capabilities&uint16(botCapRoles) == 0 {
			return server.Errorf(server.CodeForbidden, "roles capability required")
		}
		if _, err := tx.GetMember(r.Context(), cid, s.UserID); err != nil {
			return server.Errorf(server.CodeForbidden, "installed bot must be a current community member")
		}

		// The store returns an immutable receipt before checking mutable membership
		// or policy state. For a fresh request, it writes inside this transaction;
		// any failed authority check below rolls that write back atomically.
		var created bool
		receipt, created, err = tx.GrantBotJoinRoleForEpisode(r.Context(), store.BotJoinRoleGrantRequest{
			RequestID: req.RequestID, InstallationID: installation.ID, CommunityID: cid, UserID: req.MemberID, EpisodeID: req.EpisodeID,
		})
		if errors.Is(err, store.ErrNotFound) {
			member, memberErr := tx.GetMember(r.Context(), cid, req.MemberID)
			if memberErr == nil && member.EpisodeID == req.EpisodeID {
				policyRepo, ok := tx.(store.BotJoinRolePolicy)
				if !ok {
					return errors.New("store does not support bot join role policy")
				}
				_, policyErr := policyRepo.GetBotJoinRolePolicy(r.Context(), installation.ID)
				if errors.Is(policyErr, store.ErrNotFound) {
					noPolicy = true
					return nil
				}
			}
		}
		if err != nil || !created {
			return err
		}
		member, err := tx.GetMember(r.Context(), cid, req.MemberID)
		if err != nil || member.EpisodeID != req.EpisodeID {
			return server.Errorf(server.CodeNotFound, "current member episode not found")
		}
		policyRepo, ok := tx.(store.BotJoinRolePolicy)
		if !ok {
			return errors.New("store does not support bot join role policy")
		}
		policy, err := policyRepo.GetBotJoinRolePolicy(r.Context(), installation.ID)
		if err != nil || policy.CommunityID != cid || policy.RoleID != receipt.RoleID {
			return server.Errorf(server.CodeForbidden, "approved join role unavailable")
		}
		approver, err := tx.GetUser(r.Context(), policy.ApprovedBy)
		if err != nil || approver.Kind != kindHuman {
			return server.Errorf(server.CodeForbidden, "join role approval is no longer valid")
		}
		if _, err := tx.GetMember(r.Context(), cid, policy.ApprovedBy); err != nil {
			return server.Errorf(server.CodeForbidden, "join role approver is no longer a member")
		}
		snap, err := LoadSnapshot(r.Context(), tx, cid, policy.ApprovedBy, nil)
		if err != nil {
			return server.Errorf(server.CodeForbidden, "join role approval is no longer valid")
		}
		bits := snap.Resolve(policy.ApprovedBy)
		if !bits.Has(PermManageRoles) {
			return server.Errorf(server.CodeForbidden, "join role approver lacks manage_roles")
		}
		roles := NewRoles(d.Repo, d.Clock, d.Domain, d.Log)
		if err := roles.selfRoleCandidate(r, tx, cid, policy.ApprovedBy, receipt.RoleID, bits, snap); err != nil {
			return server.Errorf(server.CodeForbidden, "join role approval is no longer valid")
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			err = notFound(err)
		}
		server.WriteError(w, d.storeError(r, err))
		return
	}
	if noPolicy {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if d.BotJoinRoleGranted != nil {
		d.BotJoinRoleGranted(afterCommit(r), cid, req.MemberID)
	}
	_ = server.WriteCBOR(w, http.StatusOK, []any{receipt.RequestID, receipt.InstallationID, receipt.CommunityID, receipt.UserID, receipt.EpisodeID, receipt.RoleID})
}

// GetBotMemberEpisode supplies a ready installed bot with a member's current
// episode token. The grant route independently rechecks it under its lock.
func (d Deps) GetBotMemberEpisode(w http.ResponseWriter, r *http.Request) {
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
	memberID, err := server.PathID(r, "member")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if cid != s.Bot.CommunityID {
		server.WriteError(w, server.Errorf(server.CodeNotFound, "bot installation absent"))
		return
	}
	if err := requireBotWire(s.WireVersion); err != nil {
		server.WriteError(w, err)
		return
	}
	var episode id.ID
	err = d.Repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := tx.LockCommunity(r.Context(), cid); err != nil {
			return notFound(err)
		}
		installation, err := tx.GetActiveBotInstallationByCommunity(r.Context(), cid)
		if err != nil || installation.ID != s.Bot.InstallationID || installation.State != store.BotInstallationReady ||
			installation.UserID == nil || *installation.UserID != s.UserID || installation.DeviceID == nil || *installation.DeviceID != s.DeviceID {
			return server.Errorf(server.CodeForbidden, "ready bot installation required")
		}
		if _, err := tx.GetMember(r.Context(), cid, s.UserID); err != nil {
			return server.Errorf(server.CodeForbidden, "installed bot must be a current community member")
		}
		member, err := tx.GetMember(r.Context(), cid, memberID)
		if err != nil {
			return notFound(err)
		}
		episode = member.EpisodeID
		return nil
	})
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	_ = server.WriteCBOR(w, http.StatusOK, []any{memberID, episode})
}
