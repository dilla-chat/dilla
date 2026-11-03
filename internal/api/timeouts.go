package api

import (
	"errors"
	"math"
	"net/http"
	"strconv"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// MemberTimeouts serves the human timeout routes. Bot moderation remains unavailable until
// capability approval and live installation hierarchy can be proved for this action.
type MemberTimeouts struct {
	repo   store.Repository
	clk    clock.Clock
	notify *StructureNotifier
}

func NewMemberTimeouts(repo store.Repository, clk clock.Clock) *MemberTimeouts {
	return &MemberTimeouts{repo: repo, clk: clk}
}

// WithNotifier sends the timed-out member's live sessions a community_changed wakeup after a
// successful update or clear, so each open client refreshes its advisory timeout state.
func (h *MemberTimeouts) WithNotifier(n *StructureNotifier) *MemberTimeouts {
	h.notify = n
	return h
}

func (h *MemberTimeouts) Register(mux *server.Mux) {
	mux.HandleFunc("PUT /v1/communities/{id}/members/{user_id}/timeout", h.put)
	mux.HandleFunc("DELETE /v1/communities/{id}/members/{user_id}/timeout", h.delete)
	mux.HandleFunc("GET /v1/communities/{id}/members/{user_id}/timeout", h.get)
}

type timeoutPutReq struct {
	_         struct{} `cbor:",toarray"`
	RequestID id.ID
	Expires   uint64
}
type timeoutDeleteReq struct {
	_         struct{} `cbor:",toarray"`
	RequestID id.ID
}

func (h *MemberTimeouts) put(w http.ResponseWriter, r *http.Request) {
	m, err := moderate(r, h.repo, PermModerateMembers)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var req timeoutPutReq
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	now := h.clk.Now().Unix()
	nowUnix, nowOK := checkedUint64(now)
	expires, expiresOK := checkedInt64(req.Expires)
	if !nowOK {
		server.WriteError(w, server.Errorf(server.CodeUnavailable, "server clock is unavailable"))
		return
	}
	if !expiresOK || req.RequestID.IsZero() || req.Expires <= nowUnix || req.Expires-nowUnix > maxBanSeconds || req.Expires > math.MaxInt64 {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "request_id and a future expires timestamp are required"))
		return
	}
	if err := h.repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := tx.LockCommunity(r.Context(), m.community); err != nil {
			return notFound(err)
		}
		if err := stillModerates(r.Context(), tx, m, PermModerateMembers); err != nil {
			return err
		}
		if _, err := tx.GetMember(r.Context(), m.community, m.target); err != nil {
			return notFound(err)
		}
		if err := tx.PutMemberTimeout(r.Context(), store.MemberTimeoutRow{CommunityID: m.community, UserID: m.target, ByUser: m.session.UserID, Created: now, Expires: expires}); err != nil {
			return err
		}
		return tx.Audit(r.Context(), store.AuditRow{Actor: &m.session.UserID, Action: "member.timeout", Target: m.target.String(), Detail: m.community.String() + " " + strconv.FormatInt(expires, 10) + " " + req.RequestID.String(), At: now})
	}); err != nil {
		server.WriteError(w, err)
		return
	}
	h.notify.ChangedUser(r.Context(), m.community, m.target)
	_ = server.WriteCBOR(w, http.StatusOK, []any{req.RequestID, expires})
}

func (h *MemberTimeouts) delete(w http.ResponseWriter, r *http.Request) {
	m, err := moderate(r, h.repo, PermModerateMembers)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var req timeoutDeleteReq
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	if req.RequestID.IsZero() {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "request_id is required"))
		return
	}
	now := h.clk.Now().Unix()
	if err := h.repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := tx.LockCommunity(r.Context(), m.community); err != nil {
			return notFound(err)
		}
		if err := stillModerates(r.Context(), tx, m, PermModerateMembers); err != nil {
			return err
		}
		if err := tx.DeleteMemberTimeout(r.Context(), m.community, m.target); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// A lost 204 response may retry with the same request ID. Clearing an
				// already absent timeout is the same completed state and has no new audit.
				return nil
			}
			return err
		}
		return tx.Audit(r.Context(), store.AuditRow{Actor: &m.session.UserID, Action: "member.timeout.clear", Target: m.target.String(), Detail: m.community.String() + " " + req.RequestID.String(), At: now})
	}); err != nil {
		server.WriteError(w, err)
		return
	}
	h.notify.ChangedUser(r.Context(), m.community, m.target)
	w.WriteHeader(http.StatusNoContent)
}

func (h *MemberTimeouts) get(w http.ResponseWriter, r *http.Request) {
	s, err := enrolledSession(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	cid, err := server.PathID(r, "id")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	target, err := server.PathID(r, "user_id")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if _, err := h.repo.GetCommunity(r.Context(), cid); err != nil {
		server.WriteError(w, notFound(err))
		return
	}
	if _, err := h.repo.GetMember(r.Context(), cid, s.UserID); err != nil {
		server.WriteError(w, notFound(err))
		return
	}
	if target != s.UserID {
		m, err := moderate(r, h.repo, PermModerateMembers)
		if err != nil {
			server.WriteError(w, err)
			return
		}
		if err := outranks(r.Context(), h.repo, m.snap, s.UserID, target, cid); err != nil {
			server.WriteError(w, err)
			return
		}
	}
	now := h.clk.Now().Unix()
	row, err := h.repo.GetMemberTimeout(r.Context(), cid, target, now)
	var expires *int64
	if err == nil {
		expires = &row.Expires
	} else if !errors.Is(err, store.ErrNotFound) {
		server.WriteError(w, err)
		return
	}
	_ = server.WriteCBOR(w, http.StatusOK, []any{expires})
}
