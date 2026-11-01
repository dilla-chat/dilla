package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/ds"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/store"
)

// registerBotInteractionGroupHeal mounts the restore-only Bot heal surface. It is scoped by both
// installation and the current accepted kind-3 registration; generic bot group routes stay narrow.
func (h *Groups) registerBotInteractionGroupHeal(mux *server.Mux, sessions *auth.Sessions) {
	bot := BotSessionRoute(sessions, h.Limiter)
	base := "/v1/bot-runtime/installations/{installation}/interaction-groups/{group}/heal"
	mux.Handle("GET "+base, bot("GET "+base, dsMeter(h.Limiter, dsClassRead, h.botInteractionGroupHealStatus)))
	mux.Handle("POST "+base, bot("POST "+base, dsMeter(h.Limiter, dsClassCommit, h.botInteractionGroupHeal)))
}

type botHealStatusResponse struct {
	_           struct{} `cbor:",toarray"`
	Epoch       uint64
	NextSeq     uint64
	Generation  uint64
	NeedFromSeq uint64
	NeedsHeal   uint64
}

func (h *Groups) botInteractionGroupHealStatus(w http.ResponseWriter, r *http.Request) {
	if !h.botInteractionGroupHealAuthorized(w, r, func(session auth.Session, groupID id.ID, _ ds.CurrentBotInteractionGroupContext, _ store.GroupRow) error {
		status, err := h.DS.HealStatus(r.Context(), session, groupID)
		if err != nil {
			return dsError(err)
		}
		row, err := h.Deps.Repo.GetGroup(r.Context(), groupID)
		if err != nil {
			return err
		}
		needsHeal := uint64(0)
		if row.EpochUnknown {
			needsHeal = 1
		}
		return server.EncodeBody(w, http.StatusOK, botHealStatusResponse{Epoch: status.Epoch, NextSeq: status.NextSeq,
			Generation: status.Generation, NeedFromSeq: status.NeedFromSeq, NeedsHeal: needsHeal})
	}) {
		return
	}
}

func (h *Groups) botInteractionGroupHeal(w http.ResponseWriter, r *http.Request) {
	var body healRequestBody
	if err := server.DecodeBody(w, r, h.max(), &body); err != nil {
		server.WriteError(w, err)
		return
	}
	if !h.botInteractionGroupHealAuthorized(w, r, func(session auth.Session, groupID id.ID, current ds.CurrentBotInteractionGroupContext, checkedRow store.GroupRow) error {
		tail := make([]ds.HealTailItem, 0, len(body.Tail))
		for _, item := range body.Tail {
			tail = append(tail, ds.HealTailItem{Seq: item.Seq, Epoch: item.Epoch, Kind: item.Kind, Sender: item.Sender, Blob: item.Blob})
		}
		out, err := h.DS.HealWithGuard(r.Context(), session, groupID,
			ds.HealRequest{GroupInfo: body.GroupInfo, Tail: tail, RatchetTree: body.RatchetTree},
			func(ctx context.Context, tx store.Repository, row store.GroupRow) error {
				if row.GroupID != checkedRow.GroupID || row.Kind != 3 || row.CommunityID == nil || *row.CommunityID != current.Binding.CommunityID ||
					row.ClosedAt != nil || !row.EpochUnknown || row.Epoch != checkedRow.Epoch || !bytes.Equal(row.Binding, checkedRow.Binding) ||
					!bytes.Equal(row.PublicGroupState, checkedRow.PublicGroupState) || !bytes.Equal(row.TreeHash, checkedRow.TreeHash) {
					return server.Errorf(server.CodeForbidden, "current interaction group changed before heal acceptance")
				}
				receipt, err := tx.GetBotGroupRegistrationReceiptByGroup(ctx, groupID)
				if err != nil || receipt.InstallationID != current.Registration.InstallationID || receipt.UserID != current.Registration.UserID ||
					receipt.DeviceID != current.Registration.DeviceID || receipt.CommunityID != current.Registration.CommunityID ||
					receipt.GroupID != current.Registration.GroupID || receipt.OperationID != current.Registration.OperationID ||
					!bytes.Equal(receipt.BodySHA256, current.Registration.BodySHA256) {
					return server.Errorf(server.CodeForbidden, "current interaction registration changed before heal acceptance")
				}
				_, err = h.Deps.verifyCurrentInteractionLiveState(ctx, current, h.Deps.Clock.Now().Unix(), current.Binding.InstallationID,
					current.Binding.CommunityID, session.UserID, session.DeviceID, current.Binding.ParticipantUserID,
					current.Binding.ParticipantDeviceID, true, tx)
				return err
			})
		if err != nil {
			return dsError(err)
		}
		return server.EncodeBody(w, http.StatusOK, epochNextSeqResponse{Epoch: out.Epoch, NextSeq: out.Seq + 1})
	}) {
		return
	}
}

func (h *Groups) botInteractionGroupHealAuthorized(w http.ResponseWriter, r *http.Request,
	fn func(auth.Session, id.ID, ds.CurrentBotInteractionGroupContext, store.GroupRow) error) bool {
	if h.DS == nil || h.Deps.Repo == nil || h.Deps.DeviceLists == nil || h.Deps.Clock == nil {
		server.WriteError(w, server.Errorf(server.CodeUnavailable, "Bot interaction group heal is unavailable"))
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
	var current ds.CurrentBotInteractionGroupContext
	var checkedRow store.GroupRow
	err = h.DS.WithCurrentBotInteractionGroupHealRead(r.Context(), groupID, func(latest ds.CurrentBotInteractionGroupContext, tx store.Repository) error {
		if latest.Registration.GroupID != groupID || latest.Binding.InstallationID != installationID ||
			latest.Binding.CommunityID != session.Bot.CommunityID || latest.Binding.BotUserID != session.UserID ||
			latest.Registration.UserID != session.UserID || latest.Registration.DeviceID != session.DeviceID {
			return errors.New("interaction group does not match the current Bot session")
		}
		return latest.WithDeviceListVerification(r.Context(), func(ctx context.Context) error {
			_, err := h.Deps.verifyCurrentInteractionLiveState(ctx, latest, h.Deps.Clock.Now().Unix(), installationID,
				session.Bot.CommunityID, session.UserID, session.DeviceID, latest.Binding.ParticipantUserID,
				latest.Binding.ParticipantDeviceID, true, tx)
			if err != nil {
				return err
			}
			checkedRow, err = tx.GetGroup(r.Context(), groupID)
			if err != nil {
				return err
			}
			current = latest
			return nil
		})
	})
	if err == nil {
		if err := fn(session, groupID, current, checkedRow); err != nil {
			var apiErr *server.Error
			var dsErr *ds.Error
			switch {
			case errors.As(err, &apiErr):
				server.WriteError(w, apiErr)
			case errors.As(err, &dsErr):
				server.WriteError(w, server.WithStatus(dsErr.Status, &server.Error{Code: server.Code(dsErr.Code), Detail: dsErr.Detail}))
			default:
				server.WriteError(w, server.Errorf(server.CodeInternal, "Bot interaction group heal failed"))
			}
			return false
		}
		return true
	}
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
