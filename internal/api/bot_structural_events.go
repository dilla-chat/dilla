package api

import (
	"math"
	"net/http"
	"strconv"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

const botStructuralEventPageLimit = 250

type botStructuralEventAckRequest struct {
	_       struct{} `cbor:",toarray"`
	Through uint64
}

type botStructuralEventPageResponse struct {
	_         struct{} `cbor:",toarray"`
	Events    [][]any
	Next      uint64
	HighWater uint64
	Floor     uint64
}

type botStructuralEventsAPI struct {
	deps Deps
}

func registerBotStructuralEvents(mux *server.Mux, d Deps) {
	h := &botStructuralEventsAPI{deps: d}
	mux.SocketAuthed("GET /v1/communities/{community}/bot/events", d.socketChain(auth.ScopeEnrolled, dsClassRead), http.HandlerFunc(h.get))
	mux.SocketAuthed("PUT /v1/communities/{community}/bot/events/ack", d.socketChain(auth.ScopeEnrolled, dsClassWrite), http.HandlerFunc(h.ack))
}

func (h *botStructuralEventsAPI) get(w http.ResponseWriter, r *http.Request) {
	community, err := server.PathID(r, "community")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	after, limit, err := parseBotStructuralEventQuery(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	_, _, boundCommunity, err := managedSendSession(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if boundCommunity != community {
		server.WriteError(w, server.Errorf(server.CodeNotFound, "Bot event stream not found"))
		return
	}
	page, err := h.deps.Repo.ListBotStructuralEvents(r.Context(), community, after, limit)
	if err != nil {
		server.WriteError(w, h.deps.storeError(r, err))
		return
	}
	if after < page.Floor {
		server.WriteError(w, &server.Error{Code: server.CodePruned, Detail: "event cursor is below retention floor", Extra: []any{page.Floor, page.HighWater}})
		return
	}
	rows := make([][]any, 0, len(page.Events))
	for _, event := range page.Events {
		if protocol.ValidateEventContext(event, community) != nil {
			server.WriteError(w, server.Unavailable(1000, "stored Bot event has invalid community binding"))
			return
		}
		if _, err := protocol.EncodeEvent(event); err != nil {
			server.WriteError(w, server.Unavailable(1000, "stored Bot event is invalid"))
			return
		}
		rows = append(rows, []any{uint64(1), event.ID, event.Sequence, event.CommunityID, event.Timestamp, uint64(event.Kind), event.Data})
	}
	next := page.NextSequence
	if err := server.EncodeBody(w, http.StatusOK, botStructuralEventPageResponse{Events: rows, Next: next, HighWater: page.HighWater, Floor: page.Floor}); err != nil {
		h.deps.logf(r, "api: encode Bot structural event page", "err", err)
	}
}

func (h *botStructuralEventsAPI) ack(w http.ResponseWriter, r *http.Request) {
	community, err := server.PathID(r, "community")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	raw, err := server.ReadBody(w, r, 64)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var body botStructuralEventAckRequest
	if cborx.ExpectMajor(raw, cborx.MajorArray) != nil || cborx.Unmarshal(raw, &body) != nil || body.Through > math.MaxInt64 {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "body is not a valid Bot event acknowledgement"))
		return
	}
	_, installation, boundCommunity, err := managedSendSession(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if boundCommunity != community {
		server.WriteError(w, server.Errorf(server.CodeNotFound, "Bot event stream not found"))
		return
	}
	err = h.deps.Repo.Tx(r.Context(), func(tx store.Repository) error {
		return tx.AckBotStructuralEvents(r.Context(), installation, community, body.Through)
	})
	if err != nil {
		server.WriteError(w, h.deps.storeError(r, err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func parseBotStructuralEventQuery(r *http.Request) (uint64, int32, error) {
	query := r.URL.Query()
	for key, values := range query {
		if key != "after" && key != "limit" || len(values) != 1 {
			return 0, 0, server.Errorf(server.CodeInvalidRequest, "invalid Bot event query")
		}
	}
	after := uint64(0)
	if values, ok := query["after"]; ok {
		value, err := parseBotEventUint(values[0])
		if err != nil {
			return 0, 0, err
		}
		after = value
	}
	limit := uint64(100)
	if values, ok := query["limit"]; ok {
		value, err := parseBotEventUint(values[0])
		if err != nil || value < 1 || value > botStructuralEventPageLimit {
			return 0, 0, server.Errorf(server.CodeInvalidRequest, "limit must be between 1 and 250")
		}
		limit = value
	}
	pageSize, ok := checkedInt32(limit)
	if !ok {
		return 0, 0, server.Errorf(server.CodeInvalidRequest, "limit exceeds the supported range")
	}
	return after, pageSize, nil
}

func parseBotEventUint(value string) (uint64, error) {
	if value == "" {
		return 0, server.Errorf(server.CodeInvalidRequest, "cursor values must be unsigned decimal integers")
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return 0, server.Errorf(server.CodeInvalidRequest, "cursor values must be unsigned decimal integers")
		}
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil || parsed > math.MaxInt64 {
		return 0, server.Errorf(server.CodeInvalidRequest, "cursor exceeds the supported sequence range")
	}
	return parsed, nil
}
