package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// Messages is the application-ciphertext half of the delivery service's HTTP surface: endpoints 7
// (upload), 12 (catch-up), 17 (delete) and 18 (cursor) of interfaces.md §5.1.
//
// It mounts itself rather than hanging off `api.Deps`, for the same reason `Groups` does: a nil
// `*ds.DS` must not silently turn four documented routes into 501s.
type Messages struct {
	DS                 *ds.DS
	roles              *Roles
	MaxCiphertextBytes int64 // zero means defaultMaxCiphertextBytes
	MaxBody            int64 // §5.3's cap for the three small routes; zero means maxCBORBody
	// Limiter meters every route per device session from `[limits.rate]` (dsmeter.go); nil
	// meters nothing.
	Limiter *server.RateLimiter
	// AfterUpload runs after an accepted upload with the uploader's user and the group (AW2, Q1: a
	// message takes the sender out of the community's AFK list); nil does nothing.
	AfterUpload func(ctx context.Context, user, group id.ID)
}

func (h *Messages) WithRoles(roles *Roles) *Messages { h.roles = roles; return h }

// defaultMaxCiphertextBytes is the MLS ciphertext cap of the Global Constraints, 196608 bytes. The
// delivery service enforces it again on the decoded `private_message`, which is the enforcement
// that counts; the body cap here is the cheaper one, applied before the whole request is read.
const defaultMaxCiphertextBytes = 196608

func (h *Messages) maxCiphertext() int64 {
	if h.MaxCiphertextBytes == 0 {
		return defaultMaxCiphertextBytes
	}
	return h.MaxCiphertextBytes
}

func (h *Messages) max() int64 {
	if h.MaxBody == 0 {
		return maxCBORBody
	}
	return h.MaxBody
}

func (h *Messages) Register(mux *server.Mux, sessions *auth.Sessions) {
	// The ciphertext route is the one with a raised body cap; every other route uses MaxBody. The
	// cap is applied by DecodeBody's `max` argument, because plan-1a's internal/server has no
	// per-route limiting wrapper.
	mux.SocketAuthed("POST /v1/groups/{id}/message", socketMeter(sessions, h.Limiter, dsClassMessage), http.HandlerFunc(h.upload))
	mux.SocketAuthed("GET /v1/groups/{id}/messages", socketMeter(sessions, h.Limiter, dsClassRead), http.HandlerFunc(h.list))
	mux.SocketAuthed("DELETE /v1/groups/{id}/messages/{seq}", socketMeter(sessions, h.Limiter, dsClassWrite), http.HandlerFunc(h.delete))
	mux.SocketAuthed("POST /v1/groups/{id}/cursor", socketMeter(sessions, h.Limiter, dsClassWrite), http.HandlerFunc(h.cursor))
}

type uploadRequest struct {
	_              struct{} `cbor:",toarray"`
	Epoch          uint64
	PrivateMessage []byte
}

type uploadResponse struct {
	_           struct{} `cbor:",toarray"`
	Seq         uint64
	FrankingTag []byte
	RecvTS      uint64
}

func (h *Messages) upload(w http.ResponseWriter, r *http.Request) {
	groupID, err := server.PathID(r, "id")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	body, class, err := h.decodeUpload(w, r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	session, err := sessionOf(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	out, err := h.DS.Upload(r.Context(), session, groupID, body.Epoch, body.PrivateMessage, class)
	if err != nil {
		server.WriteError(w, dsError(err))
		return
	}
	if h.AfterUpload != nil {
		h.AfterUpload(r.Context(), session.UserID, groupID)
	}
	if err := server.EncodeBody(w, http.StatusOK, uploadResponse{
		Seq: out.Seq, FrankingTag: out.FrankingTag, RecvTS: out.RecvTS,
	}); err != nil {
		server.WriteError(w, err)
	}
}

// decodeUpload reads endpoint 7's body, [epoch, private_message(bstr), class(uint 0|1)] (web3.7
// SB1, protocol/02 § Upload class), as decodeCreateChannel reads its own: the raw array first,
// then the first two elements re-encoded and decoded into uploadRequest. Any length but 3 is 400,
// the two-element legacy upload web3.7 accepted included (web3.8 S1), and so is a class that is
// not the uint 0 or 1.
func (h *Messages) decodeUpload(w http.ResponseWriter, r *http.Request) (uploadRequest, uint8, error) {
	var body uploadRequest
	var raw []cbor.RawMessage
	// The 4096-byte allowance over the ciphertext cap is the CBOR framing around it: the array
	// header, the epoch, the byte-string header and the class.
	if err := server.DecodeBody(w, r, h.maxCiphertext()+4096, &raw); err != nil {
		return body, 0, err
	}
	shape := func(err error) error {
		return server.Errorf(server.CodeInvalidRequest, "body is not deterministic CBOR: %v", err)
	}
	if len(raw) != 3 {
		return body, 0, server.Errorf(server.CodeInvalidRequest,
			"body must be [epoch, private_message, class], got %d elements", len(raw))
	}
	b, err := cborx.Marshal(raw[:2])
	if err != nil {
		return body, 0, err
	}
	if err := cborx.Unmarshal(b, &body); err != nil {
		return body, 0, shape(err)
	}
	// A null decodes into a uint64 without error, so it is refused by its bytes first: the wire
	// is uint 0 or 1 (SB1 fix round 1).
	if bytes.Equal(raw[2], []byte{0xf6}) {
		return body, 0, server.Errorf(server.CodeInvalidRequest, "class must be 0 or 1")
	}
	var c uint64
	if err := cborx.Unmarshal(raw[2], &c); err != nil {
		return body, 0, shape(err)
	}
	if c > 1 {
		return body, 0, server.Errorf(server.CodeInvalidRequest, "class must be 0 or 1")
	}
	return body, uint8(c), nil
}

type messageItem struct {
	_              struct{} `cbor:",toarray"`
	Seq            uint64
	Epoch          uint64
	UploaderDevice id.ID
	Blob           []byte
	Commitment     []byte
	FrankingTag    []byte
	RecvTS         uint64
	Deleted        uint8
	// Class is the upload class (web3.7 SB1): 0 or 1, null for a row a web3.7 legacy upload
	// stored (web3.8 refuses that upload, but its rows stay until retention removes them).
	Class *uint64
}

func (h *Messages) list(w http.ResponseWriter, r *http.Request) {
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
	// Member-only, like the other three group reads: the ciphertext, the commitments and the
	// franking tags of a group are enough to correlate membership and message timing.
	rows, err := h.DS.Messages(r.Context(), groupID, session,
		queryFrom(r, "from"), queryLimit(r, "limit", 128))
	if err != nil {
		server.WriteError(w, dsError(err))
		return
	}
	out := make([]messageItem, 0, len(rows))
	for _, row := range rows {
		item := messageItem{
			Seq: row.Seq, Epoch: row.Epoch, UploaderDevice: row.UploaderDevice,
			Blob: row.Blob, Commitment: row.CommitmentC, FrankingTag: row.FrankingTag,
			RecvTS: uint64(row.Created), //nolint:gosec // G115: a unix second or row id this server wrote, never negative
		}
		if row.DeletedAt != nil {
			item.Deleted = 1
		}
		if row.Class != nil {
			c := uint64(*row.Class)
			item.Class = &c
		}
		out = append(out, item)
	}
	if err := server.EncodeBody(w, http.StatusOK, out); err != nil {
		server.WriteError(w, err)
	}
}

// delete is endpoint 17: the uploader deletes its own message. 204, no body.
func (h *Messages) delete(w http.ResponseWriter, r *http.Request) {
	groupID, err := server.PathID(r, "id")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	seq, err := strconv.ParseUint(r.PathValue("seq"), 10, 64)
	if err != nil {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "seq must be a decimal uint64"))
		return
	}
	session, err := sessionOf(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	deleteErr := h.DS.DeleteMessage(r.Context(), session, groupID, seq)
	var deliveryErr *ds.Error
	isAlreadyDeleted := errors.As(deleteErr, &deliveryErr) && deliveryErr.Code == string(server.CodeNotFound)
	if deleteErr != nil && !isAlreadyDeleted {
		server.WriteError(w, dsError(deleteErr))
		return
	}
	panelCleaned := false
	if h.roles != nil {
		group, err := h.roles.repo.GetGroup(r.Context(), groupID)
		if err == nil && group.Kind == 0 && group.CommunityID != nil {
			panel, panelErr := h.roles.repo.GetRolePanel(r.Context(), group.TargetID, seq)
			if panelErr == nil && panel.CommunityID == *group.CommunityID {
				mayRetryCleanup := deleteErr == nil
				if isAlreadyDeleted && !panel.SendOperationID.IsZero() && panel.AuthorID == session.UserID {
					members, memberErr := h.roles.repo.ListMembers(r.Context(), groupID)
					if memberErr != nil {
						server.WriteError(w, memberErr)
						return
					}
					for _, member := range members {
						if member.UserID == session.UserID && member.DeviceID == session.DeviceID {
							mayRetryCleanup = true
							break
						}
					}
				}
				if !mayRetryCleanup {
					server.WriteError(w, dsError(deleteErr))
					return
				}
				var affected []id.ID
				err = h.roles.repo.Tx(r.Context(), func(tx store.Repository) error {
					if err := tx.LockCommunity(r.Context(), *group.CommunityID); err != nil {
						return err
					}
					var err error
					affected, err = tx.RemoveReactionRolePanelGrants(r.Context(), *group.CommunityID, group.TargetID, seq)
					if err != nil {
						return err
					}
					if err := tx.DeleteRolePanel(r.Context(), group.TargetID, seq); err != nil && !errors.Is(err, store.ErrNotFound) {
						return err
					}
					return nil
				})
				if err != nil {
					server.WriteError(w, err)
					return
				}
				panelCleaned = true
				for _, user := range affected {
					h.roles.reactionRoleChanged(r.Context(), *group.CommunityID, user)
				}
			} else if panelErr != nil && !errors.Is(panelErr, store.ErrNotFound) {
				server.WriteError(w, panelErr)
				return
			}
		} else if err != nil && !errors.Is(err, store.ErrNotFound) {
			server.WriteError(w, err)
			return
		}
	}
	if deleteErr != nil && !panelCleaned {
		server.WriteError(w, dsError(deleteErr))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// cursor is endpoint 18: body [last_seq, last_epoch] -> 204.
func (h *Messages) cursor(w http.ResponseWriter, r *http.Request) {
	groupID, err := server.PathID(r, "id")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var body struct {
		_         struct{} `cbor:",toarray"`
		LastSeq   uint64
		LastEpoch uint64
	}
	if err := server.DecodeBody(w, r, h.max(), &body); err != nil {
		server.WriteError(w, err)
		return
	}
	session, err := sessionOf(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if err := h.DS.AdvanceCursor(r.Context(), session, groupID, body.LastSeq, body.LastEpoch); err != nil {
		server.WriteError(w, dsError(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
