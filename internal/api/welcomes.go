package api

import (
	"net/http"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
)

// welcomeItem is one row of protocol/02's row 15 response. RatchetTree is the tree of the
// WELCOMING epoch, not the live one: a dilla Welcome carries no tree and the group has moved on by
// the time a joiner collects.
type welcomeItem struct {
	_           struct{} `cbor:",toarray"`
	WelcomeID   uint64
	GroupID     id.ID
	Epoch       uint64
	CommitSeq   uint64
	Blob        []byte
	RatchetTree []byte
	TreeHash    []byte
}

// welcomes is row 15. The fetch does NOT consume: `StagedWelcome::new_from_welcome` consumes the
// key material inside the client even when the client then fails, so a client that crashed
// mid-join must be able to ask again, and only the DELETE marks a Welcome delivered.
func (h *Groups) welcomes(w http.ResponseWriter, r *http.Request) {
	session, err := sessionOf(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	rows, err := h.DS.Welcomes(r.Context(), session,
		queryCursor(r, "after"), queryLimit(r, "limit", 64))
	if err != nil {
		server.WriteError(w, dsError(err))
		return
	}
	out := make([]welcomeItem, 0, len(rows))
	for _, row := range rows {
		out = append(out, welcomeItem{
			WelcomeID:   uint64(row.WelcomeID), //nolint:gosec // G115: a unix second or row id this server wrote, never negative
			GroupID:     row.GroupID,
			Epoch:       row.Epoch,
			CommitSeq:   row.CommitSeq,
			Blob:        row.Blob,
			RatchetTree: row.RatchetTree,
			TreeHash:    row.TreeHash,
		})
	}
	if err := server.EncodeBody(w, http.StatusOK, out); err != nil {
		server.WriteError(w, err)
	}
}

// ackWelcome is row 16: the DELETE that marks one Welcome delivered. 204, no body.
func (h *Groups) ackWelcome(w http.ResponseWriter, r *http.Request) {
	welcomeID, err := parseWelcomeID(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	session, err := sessionOf(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if err := h.DS.AckWelcome(r.Context(), session, welcomeID); err != nil {
		server.WriteError(w, dsError(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
