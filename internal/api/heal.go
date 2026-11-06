package api

import (
	"net/http"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/server"
)

// RegisterHeal mounts endpoints 13 and 14, invariant 11's two routes: what the instance still
// needs after a restore, and the member-driven heal that supplies it. It is a fourth Register
// method on Groups for the reason the other three are separate — the delivery service's surface
// grows one named group per task.
func (h *Groups) RegisterHeal(mux *server.Mux, sessions *auth.Sessions) {
	h.registerBotInteractionGroupHeal(mux, sessions)
	mux.SocketAuthed("GET /v1/groups/{id}/heal", socketMeter(sessions, h.Limiter, dsClassRead), http.HandlerFunc(h.healStatus))
	mux.SocketAuthed("GET /v1/groups/{id}/heal-state", socketMeter(sessions, h.Limiter, dsClassRead), http.HandlerFunc(h.memberHealState))
	mux.SocketAuthed("POST /v1/groups/{id}/heal", socketMeter(sessions, h.Limiter, dsClassCommit), http.HandlerFunc(h.heal))
}

type healStatusResponse struct {
	_           struct{} `cbor:",toarray"`
	Epoch       uint64
	NextSeq     uint64
	Generation  uint64
	NeedFromSeq uint64
}

type memberHealStateResponse struct {
	_           struct{} `cbor:",toarray"`
	Epoch       uint64
	NextSeq     uint64
	Generation  uint64
	NeedFromSeq uint64
	NeedsHeal   uint64
}

// memberHealState adds a restore marker for current enrolled members while the
// original four-field /heal status remains compatible with older clients.
func (h *Groups) memberHealState(w http.ResponseWriter, r *http.Request) {
	groupID, err := server.PathID(r, "id")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	session, err := sessionOf(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	status, err := h.DS.HealStatus(r.Context(), session, groupID)
	if err != nil {
		server.WriteError(w, dsError(err))
		return
	}
	needsHeal := uint64(0)
	if status.NeedsHeal {
		needsHeal = 1
	}
	if err := server.EncodeBody(w, http.StatusOK, memberHealStateResponse{
		Epoch: status.Epoch, NextSeq: status.NextSeq, Generation: status.Generation,
		NeedFromSeq: status.NeedFromSeq, NeedsHeal: needsHeal,
	}); err != nil {
		server.WriteError(w, err)
	}
}

// healStatus is endpoint 13: `[epoch, next_seq, generation, need_from_seq]`. The generation is how
// a client that has been away tells a restore from an ordinary gap — it is the same number `hello`
// and `ready` carry, and a change in it is what makes the rest of this answer worth acting on. It
// is member-only: a non-member gets the same E_NOT_FOUND an unknown group does.
func (h *Groups) healStatus(w http.ResponseWriter, r *http.Request) {
	groupID, err := server.PathID(r, "id")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	session, err := sessionOf(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	out, err := h.DS.HealStatus(r.Context(), session, groupID)
	if err != nil {
		server.WriteError(w, dsError(err))
		return
	}
	if err := server.EncodeBody(w, http.StatusOK, healStatusResponse{
		Epoch: out.Epoch, NextSeq: out.NextSeq,
		Generation: out.Generation, NeedFromSeq: out.NeedFromSeq,
	}); err != nil {
		server.WriteError(w, err)
	}
}

// healTailItem is one uploaded handshake: `[seq, epoch, kind, sender, blob]`, the same five
// positions `GET /v1/groups/{id}/handshakes` serves them in — a healing client replays back
// exactly what it was given. `sender` is the leaf index or null (the instance's external sender).
type healTailItem struct {
	_      struct{} `cbor:",toarray"`
	Seq    uint64
	Epoch  uint64
	Kind   uint8
	Sender *uint32
	Blob   []byte
}

type healRequestBody struct {
	_           struct{} `cbor:",toarray"`
	GroupInfo   []byte
	Tail        []healTailItem
	RatchetTree []byte
}

// epochNextSeqResponse is §2.5's `[epoch, next_seq]`, and the order is NOT the commit endpoint's
// `[seq, epoch]`: protocol/02's table spells the two rows differently and a client decodes by
// position.
type epochNextSeqResponse struct {
	_       struct{} `cbor:",toarray"`
	Epoch   uint64
	NextSeq uint64
}

// heal is endpoint 14. The body cap is the delivery service's own — a reseeding heal carries the
// whole ratchet tree, which is 620 KiB for the committed 1,500-leaf fixture, on top of up to 64
// handshakes.
func (h *Groups) heal(w http.ResponseWriter, r *http.Request) {
	groupID, err := server.PathID(r, "id")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var body healRequestBody
	if err := server.DecodeBody(w, r, h.max(), &body); err != nil {
		server.WriteError(w, err)
		return
	}
	session, err := sessionOf(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	tail := make([]ds.HealTailItem, 0, len(body.Tail))
	for _, item := range body.Tail {
		tail = append(tail, ds.HealTailItem{
			Seq: item.Seq, Epoch: item.Epoch, Kind: item.Kind,
			Sender: item.Sender, Blob: item.Blob,
		})
	}
	out, err := h.DS.Heal(r.Context(), session, groupID, ds.HealRequest{
		GroupInfo: body.GroupInfo, Tail: tail, RatchetTree: body.RatchetTree,
	})
	if err != nil {
		server.WriteError(w, dsError(err))
		return
	}
	// `next_seq` is the high-water plus one, the same number every other catch-up answer carries:
	// what the caller asks for next, not what it just wrote.
	if err := server.EncodeBody(w, http.StatusOK,
		epochNextSeqResponse{Epoch: out.Epoch, NextSeq: out.Seq + 1}); err != nil {
		server.WriteError(w, err)
	}
}
