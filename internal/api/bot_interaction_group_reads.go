package api

import (
	"context"
	"errors"
	"math"
	"net/http"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/ds"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/store"
)

// registerBotInteractionGroupReads mounts the only Bot read path for private kind-3
// groups. Values are selected under the same transaction and group fence that proves
// the currently accepted two-leaf MLS roster.
func (h *Groups) registerBotInteractionGroupReads(mux *server.Mux, sessions *auth.Sessions) {
	bot := BotSessionRoute(sessions, h.Limiter)
	base := "/v1/bot-runtime/installations/{installation}/interaction-groups/{group}"
	mux.Handle("GET "+base+"/info", bot("GET "+base+"/info", http.HandlerFunc(h.botInteractionGroupInfo)))
	mux.Handle("GET "+base+"/handshakes", bot("GET "+base+"/handshakes", http.HandlerFunc(h.botInteractionGroupHandshakes)))
	mux.Handle("GET "+base+"/messages", bot("GET "+base+"/messages", http.HandlerFunc(h.botInteractionGroupMessages)))
	mux.Handle("POST "+base+"/cursor", bot("POST "+base+"/cursor", http.HandlerFunc(h.botInteractionGroupCursor)))
}

func (h *Groups) botInteractionGroupRead(w http.ResponseWriter, r *http.Request, fn func(ds.CurrentBotInteractionGroupContext, store.Repository, auth.Session, id.ID) error) bool {
	if h.DS == nil || h.Deps.Repo == nil || h.Deps.DeviceLists == nil || h.Deps.Clock == nil {
		server.WriteError(w, server.Errorf(server.CodeUnavailable, "Bot interaction group reads are unavailable"))
		return false
	}
	session, err := sessionOf(r)
	if err != nil || session.Bot == nil || session.Bot.State != store.BotInstallationReady || session.UserID.IsZero() || session.DeviceID.IsZero() {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "ready Bot session required"))
		return false
	}
	installationID, err := server.PathID(r, "installation")
	if err != nil {
		server.WriteError(w, err)
		return false
	}
	groupID, err := server.PathID(r, "group")
	if err != nil {
		server.WriteError(w, err)
		return false
	}
	if installationID != session.Bot.InstallationID || session.Bot.CommunityID.IsZero() {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "current Bot interaction group unavailable"))
		return false
	}
	err = h.DS.WithCurrentBotInteractionGroupRead(r.Context(), groupID, func(current ds.CurrentBotInteractionGroupContext, tx store.Repository) error {
		if current.Registration.GroupID != groupID || current.Binding.InstallationID != installationID ||
			current.Binding.CommunityID != session.Bot.CommunityID || current.Binding.BotUserID != session.UserID ||
			current.Registration.UserID != session.UserID || current.Registration.DeviceID != session.DeviceID {
			return errors.New("interaction group does not match the current Bot session")
		}
		return current.WithDeviceListVerification(r.Context(), func(ctx context.Context) error {
			now := h.Deps.Clock.Now().Unix()
			if _, err := h.Deps.verifyCurrentInteractionLiveState(ctx, current, now, installationID, session.Bot.CommunityID,
				session.UserID, session.DeviceID, current.Binding.ParticipantUserID, current.Binding.ParticipantDeviceID, true, tx); err != nil {
				return err
			}
			return fn(current, tx, session, groupID)
		})
	})
	if err != nil {
		var apiErr *server.Error
		var dsErr *ds.Error
		switch {
		case errors.As(err, &apiErr):
			server.WriteError(w, apiErr)
		case errors.As(err, &dsErr):
			server.WriteError(w, server.WithStatus(dsErr.Status, &server.Error{Code: server.Code(dsErr.Code), Detail: dsErr.Detail}))
		default:
			server.WriteError(w, server.Errorf(server.CodeForbidden, "current Bot interaction group unavailable"))
		}
		return false
	}
	return true
}

func (h *Groups) botInteractionGroupInfo(w http.ResponseWriter, r *http.Request) {
	var out groupInfoResponse
	if !h.botInteractionGroupRead(w, r, func(current ds.CurrentBotInteractionGroupContext, tx store.Repository, _ auth.Session, groupID id.ID) error {
		row, err := tx.GetGroup(r.Context(), groupID)
		if err != nil || row.Kind != 3 || row.CommunityID == nil || *row.CommunityID != current.Binding.CommunityID ||
			row.ClosedAt != nil || row.EpochUnknown || row.Epoch != current.Epoch || row.Seq == math.MaxInt64 {
			return errors.New("current interaction group row mismatch")
		}
		out = groupInfoResponse{Epoch: row.Epoch, GroupInfo: row.GroupInfoBlob, TreeHash: append([]byte(nil), row.TreeHash...), NextSeq: row.Seq + 1}
		return nil
	}) {
		return
	}
	if err := server.EncodeBody(w, http.StatusOK, out); err != nil {
		server.WriteError(w, err)
	}
}

func (h *Groups) botInteractionGroupHandshakes(w http.ResponseWriter, r *http.Request) {
	from, limit, err := botInteractionPageBounds(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var out []handshakeItem
	if !h.botInteractionGroupRead(w, r, func(_ ds.CurrentBotInteractionGroupContext, tx store.Repository, _ auth.Session, groupID id.ID) error {
		group, err := tx.GetGroup(r.Context(), groupID)
		if err != nil {
			return err
		}
		if group.HandshakesPruned > 0 && from <= group.HandshakesPruned {
			return server.WithStatus(http.StatusGone, server.Errorf(server.CodePruned, "seq %d is older than the retention floor %d", from, group.HandshakesPruned+1))
		}
		rows, err := tx.ListHandshakes(r.Context(), groupID, from, limit)
		if err != nil {
			return err
		}
		out = make([]handshakeItem, 0, len(rows))
		for _, row := range rows {
			out = append(out, handshakeItem{Seq: row.Seq, Epoch: row.Epoch, Kind: row.Kind, Sender: row.SenderLeaf, Blob: row.Blob})
		}
		return nil
	}) {
		return
	}
	if err := server.EncodeBody(w, http.StatusOK, out); err != nil {
		server.WriteError(w, err)
	}
}

func (h *Groups) botInteractionGroupMessages(w http.ResponseWriter, r *http.Request) {
	from, limit, err := botInteractionPageBounds(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var out []messageItem
	if !h.botInteractionGroupRead(w, r, func(_ ds.CurrentBotInteractionGroupContext, tx store.Repository, _ auth.Session, groupID id.ID) error {
		group, err := tx.GetGroup(r.Context(), groupID)
		if err != nil {
			return err
		}
		if group.PrunedBelow > 0 && from <= group.PrunedBelow {
			return server.WithStatus(http.StatusGone, server.Errorf(server.CodePruned, "seq %d is older than the retention floor %d", from, group.PrunedBelow+1))
		}
		rows, err := tx.ListAppMessages(r.Context(), groupID, from, limit)
		if err != nil {
			return err
		}
		out = make([]messageItem, 0, len(rows))
		for _, row := range rows {
			recvTS, ok := checkedUint64(row.Created)
			if !ok {
				return store.ErrConflict
			}
			item := messageItem{Seq: row.Seq, Epoch: row.Epoch, UploaderDevice: row.UploaderDevice, Blob: row.Blob,
				Commitment: row.CommitmentC, FrankingTag: row.FrankingTag, RecvTS: recvTS}
			if row.DeletedAt != nil {
				item.Deleted = 1
			}
			if row.Class != nil {
				class := uint64(*row.Class)
				item.Class = &class
			}
			out = append(out, item)
		}
		return nil
	}) {
		return
	}
	if err := server.EncodeBody(w, http.StatusOK, out); err != nil {
		server.WriteError(w, err)
	}
}

func (h *Groups) botInteractionGroupCursor(w http.ResponseWriter, r *http.Request) {
	var body struct {
		_         struct{} `cbor:",toarray"`
		LastSeq   uint64
		LastEpoch uint64
	}
	if err := server.DecodeBody(w, r, maxCBORBody, &body); err != nil {
		server.WriteError(w, err)
		return
	}
	if !h.botInteractionGroupRead(w, r, func(_ ds.CurrentBotInteractionGroupContext, tx store.Repository, session auth.Session, groupID id.ID) error {
		group, err := tx.GetGroup(r.Context(), groupID)
		if err != nil {
			return err
		}
		if body.LastSeq > group.Seq {
			return server.Errorf(server.CodeInvalidRequest, "last_seq exceeds the group's high-water")
		}
		cursor, err := tx.GetCursor(r.Context(), session.DeviceID, groupID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if body.LastSeq >= cursor.LastSeq {
			return tx.PutCursor(r.Context(), session.DeviceID, groupID, body.LastSeq, body.LastEpoch, h.Deps.Clock.Now().Unix())
		}
		return nil
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func botInteractionPageBounds(r *http.Request) (uint64, int32, error) {
	from := queryFrom(r, "from")
	limit := queryUint(r, "limit", 256)
	if limit == 0 || limit > 256 {
		return 0, 0, server.Errorf(server.CodeInvalidRequest, "limit must be between 1 and 256")
	}
	pageSize, ok := checkedInt32(limit)
	if !ok {
		return 0, 0, server.Errorf(server.CodeInvalidRequest, "limit exceeds the supported range")
	}
	return from, pageSize, nil
}
