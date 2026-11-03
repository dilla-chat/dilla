package api

import (
	"net/http"

	"github.com/dilla-chat/dilla/internal/auth"
	botprotocol "github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

type botMessageRequest struct {
	_                  struct{} `cbor:",toarray"`
	OperationID        id.ID
	AttemptID          id.ID
	PredecessorAttempt *id.ID
	ExpectedGeneration uint64
	Epoch              uint64
	Ciphertext         []byte
	Class              uint8
}

type botMessageResolveRequest struct {
	_                  struct{} `cbor:",toarray"`
	AttemptID          id.ID
	PredecessorAttempt *id.ID
	GroupID            id.ID
	ExpectedGeneration uint64
	Epoch              uint64
	Class              uint8
	CiphertextSHA256   []byte
}

// RegisterBotMessages mounts the managed-Bot send, receipt and resolve surface.
// The authenticated Bot session supplies installation and community authority.
func (h *Groups) RegisterBotMessages(mux *server.Mux, sessions *auth.Sessions) {
	enrolled := func(f http.HandlerFunc) http.Handler { return sessions.Middleware(f, auth.ScopeEnrolled) }
	mux.Handle("POST /v1/groups/{group}/bot/messages", enrolled(dsMeter(h.Limiter, dsClassWrite, h.postBotMessage)))
	mux.Handle("GET /v1/communities/{community}/bot/receipts/{operation}", enrolled(dsMeter(h.Limiter, dsClassRead, h.getBotMessageReceipt)))
	mux.Handle("POST /v1/communities/{community}/bot/receipts/{operation}/resolve", enrolled(dsMeter(h.Limiter, dsClassWrite, h.resolveBotMessage)))
}

func (h *Groups) postBotMessage(w http.ResponseWriter, r *http.Request) {
	groupID, err := server.PathID(r, "group")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var body botMessageRequest
	if err := server.DecodeBody(w, r, h.max(), &body); err != nil {
		server.WriteError(w, err)
		return
	}
	session, installation, community, err := managedSendSession(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	out, err := h.DS.AcceptBotMessage(r.Context(), ds.BotMessageRequest{
		Session: session, InstallationID: installation, OperationID: body.OperationID,
		AttemptID: body.AttemptID, PredecessorAttempt: body.PredecessorAttempt,
		CommunityID: community, GroupID: groupID, ExpectedGeneration: body.ExpectedGeneration,
		Epoch: body.Epoch, Class: body.Class, Ciphertext: body.Ciphertext,
	})
	if err != nil {
		server.WriteError(w, dsError(err))
		return
	}
	writeBotReceipt(w, out.Receipt, body.OperationID, body.AttemptID, community, groupID)
}

func (h *Groups) getBotMessageReceipt(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	community, err := server.PathID(r, "community")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	operation, err := server.PathID(r, "operation")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	values := r.URL.Query()["attempt"]
	if len(values) != 1 {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "attempt must name one attempt id"))
		return
	}
	attempt, err := id.Parse(values[0])
	if err != nil {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "attempt: %v", err))
		return
	}
	session, _, boundCommunity, err := managedSendSession(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if boundCommunity != community {
		server.WriteError(w, server.Errorf(server.CodeNotFound, "send receipt not found"))
		return
	}
	out, err := h.DS.BotMessageReceipt(r.Context(), session, community, operation, attempt)
	if err != nil {
		server.WriteError(w, dsError(err))
		return
	}
	writeBotReceipt(w, out.Receipt, operation, attempt, community, id.ID{})
}

func (h *Groups) resolveBotMessage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	community, err := server.PathID(r, "community")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	operation, err := server.PathID(r, "operation")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var body botMessageResolveRequest
	if err := server.DecodeBody(w, r, h.max(), &body); err != nil {
		server.WriteError(w, err)
		return
	}
	session, installation, boundCommunity, err := managedSendSession(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if boundCommunity != community {
		server.WriteError(w, server.Errorf(server.CodeNotFound, "send receipt not found"))
		return
	}
	out, err := h.DS.ResolveBotMessage(r.Context(), ds.BotMessageResolveRequest{
		Session: session, InstallationID: installation, OperationID: operation,
		AttemptID: body.AttemptID, PredecessorAttempt: body.PredecessorAttempt,
		CommunityID: community, GroupID: body.GroupID, ExpectedGeneration: body.ExpectedGeneration,
		Epoch: body.Epoch, Class: body.Class, CiphertextSHA256: body.CiphertextSHA256,
	})
	if err != nil {
		server.WriteError(w, dsError(err))
		return
	}
	writeBotReceipt(w, out.Receipt, operation, body.AttemptID, community, body.GroupID)
}

func managedSendSession(r *http.Request) (ds.Session, id.ID, id.ID, error) {
	s, ok := auth.FromContext(r.Context())
	if !ok {
		return ds.Session{}, id.ID{}, id.ID{}, server.Errorf(server.CodeUnauthenticated, "no device session")
	}
	if s.Scope != auth.ScopeEnrolled || s.Bot == nil || s.WireVersion != 3 || s.Bot.State != store.BotInstallationReady {
		return ds.Session{}, id.ID{}, id.ID{}, server.Errorf(server.CodeForbidden, "a current ready Bot session is required")
	}
	return s, s.Bot.InstallationID, s.Bot.CommunityID, nil
}

func writeBotReceipt(w http.ResponseWriter, raw []byte, operation, attempt, community, group id.ID) {
	receipt, err := botprotocol.DecodeReceipt(raw)
	if err != nil || receipt.OperationID != operation || receipt.AttemptID != attempt || receipt.CommunityID != community || !group.IsZero() && receipt.GroupID != group {
		server.WriteError(w, server.Unavailable(1000, "stored Bot send receipt is invalid"))
		return
	}
	w.Header().Set("Content-Type", "application/cbor")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}
