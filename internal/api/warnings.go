package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// MemberWarnings serves human-authenticated warning actions. The reason is
// intentionally absent from this request and from server storage; encrypted
// case delivery is a separate client-to-Bot interaction.
type MemberWarnings struct {
	repo store.Repository
	clk  clock.Clock
}

func NewMemberWarnings(repo store.Repository, clk clock.Clock) *MemberWarnings {
	return &MemberWarnings{repo: repo, clk: clk}
}

func (h *MemberWarnings) Register(mux *server.Mux) {
	mux.HandleFunc("POST /v1/communities/{id}/members/{user_id}/warning", h.post)
}

type warningPostReq struct {
	_         struct{} `cbor:",toarray"`
	RequestID id.ID
}

func (h *MemberWarnings) post(w http.ResponseWriter, r *http.Request) {
	m, err := moderate(r, h.repo, PermModerateMembers)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var req warningPostReq
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	if req.RequestID.IsZero() {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "request_id is required"))
		return
	}
	now := h.clk.Now().Unix()
	if now <= 0 {
		server.WriteError(w, server.Errorf(server.CodeUnavailable, "server clock is unavailable"))
		return
	}
	nowUnix, ok := checkedUint64(now)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnavailable, "server clock is unavailable"))
		return
	}
	var actionID id.ID
	var revision uint64
	var acceptedAt int64
	if err := h.repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := tx.LockCommunity(r.Context(), m.community); err != nil {
			return notFound(err)
		}
		prior, priorErr := tx.GetManualWarningAction(r.Context(), m.community, req.RequestID)
		if priorErr == nil {
			if prior.ActorUserID != m.session.UserID || prior.ActorDeviceID != m.session.DeviceID || prior.TargetUserID != m.target {
				return botConflict("warning request id was used for a different action")
			}
			actionID, revision, acceptedAt = prior.ActionID, prior.Revision, prior.AcceptedAt
			return nil
		}
		if !errors.Is(priorErr, store.ErrNotFound) {
			return priorErr
		}
		if err := stillModerates(r.Context(), tx, m, PermModerateMembers); err != nil {
			return err
		}
		if _, err := tx.GetMember(r.Context(), m.community, m.target); err != nil {
			return notFound(err)
		}
		actionID = id.New()
		event, err := tx.AppendBotStructuralEvent(r.Context(), protocol.Event{ID: actionID, CommunityID: m.community,
			Timestamp: nowUnix, Kind: protocol.EventModeration,
			Data: []any{actionID, req.RequestID, m.session.UserID, m.session.DeviceID, m.target, nil, nil, uint64(0), nil}})
		if err != nil {
			return err
		}
		revision = event.Sequence
		acceptedAt = now
		if err := tx.PutManualWarningAction(r.Context(), store.ManualWarningActionRow{CommunityID: m.community, RequestID: req.RequestID,
			ActionID: actionID, ActorUserID: m.session.UserID, ActorDeviceID: m.session.DeviceID, TargetUserID: m.target,
			Revision: revision, AcceptedAt: acceptedAt}); err != nil {
			return err
		}
		return tx.Audit(r.Context(), store.AuditRow{Actor: &m.session.UserID, Action: "member.warn", Target: m.target.String(),
			Detail: fmt.Sprintf("%s %s %s", m.community, actionID, req.RequestID), At: now})
	}); err != nil {
		server.WriteError(w, err)
		return
	}
	acceptedAtUnix, ok := checkedUint64(acceptedAt)
	if !ok {
		server.WriteError(w, server.Unavailable(1000, "stored warning has an invalid timestamp"))
		return
	}
	_ = server.WriteCBOR(w, http.StatusOK, []any{req.RequestID, actionID, revision, acceptedAtUnix})
}
