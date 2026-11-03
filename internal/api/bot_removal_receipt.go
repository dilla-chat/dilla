package api

import (
	"net/http"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// A bot can attach encrypted evidence only after resolving an accepted,
// immutable human action. This endpoint returns structural receipt data only.
func (d Deps) registerBotRemovalReceipt(m *server.Mux) {
	path := "GET /v1/bot-runtime/installations/{installation}/moderation/removals/{request}"
	m.Handle(path, BotSessionRoute(d.Sessions, d.Limiter)(path, http.HandlerFunc(d.getBotRemovalReceipt)))
}

func (d Deps) getBotRemovalReceipt(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	s, ok := session(r)
	if !ok || s.Bot == nil || s.Bot.State != store.BotInstallationReady {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "ready Bot session required"))
		return
	}
	if err := requireBotWire(s.WireVersion); err != nil {
		server.WriteError(w, err)
		return
	}
	installationID, err := server.PathID(r, "installation")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	requestID, err := server.PathID(r, "request")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if installationID != s.Bot.InstallationID || s.Bot.CommunityID.IsZero() {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "Bot session does not own installation"))
		return
	}
	installation, err := d.Repo.GetActiveBotInstallationByCommunity(r.Context(), s.Bot.CommunityID)
	if err != nil || installation.ID != installationID || installation.State != store.BotInstallationReady ||
		installation.RemovedAt != nil || installation.UserID == nil || *installation.UserID != s.UserID ||
		installation.DeviceID == nil || *installation.DeviceID != s.DeviceID ||
		uint64(installation.Capabilities)&botCapRemoval == 0 || uint64(s.Bot.Capabilities)&botCapRemoval == 0 {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "Removal-capable Bot installation required"))
		return
	}
	row, err := d.Repo.GetModerationRemovalByRequest(r.Context(), requestID)
	if err != nil {
		server.WriteError(w, notFound(err))
		return
	}
	proof, err := protocol.DecodeRemovalProof(row.Proof)
	if err != nil || row.RequestID != requestID || row.CommunityID != installation.CommunityID ||
		proof.RequestID != row.RequestID || proof.CommunityID != row.CommunityID || proof.GroupID != row.GroupID ||
		proof.TargetSequence != row.TargetSequence || row.ActionID.IsZero() || row.Revision == 0 || row.AcceptedAt <= 0 {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "accepted removal receipt is unavailable"))
		return
	}
	if err := server.EncodeBody(w, http.StatusOK, []any{row.ActionID, row.CommunityID, row.GroupID,
		proof.ActorUser, proof.ActorDevice, row.TargetSequence, row.Revision, uint64(row.AcceptedAt)}); err != nil {
		server.WriteError(w, err)
	}
}
