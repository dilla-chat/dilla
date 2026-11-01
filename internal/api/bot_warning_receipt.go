package api

import (
	"net/http"

	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/store"
)

// registerBotWarningReceipt exposes only authoritative receipt metadata to the
// installation that owns the community's encrypted moderation state.
func (d Deps) registerBotWarningReceipt(m *server.Mux) {
	path := "GET /v1/bot-runtime/installations/{installation}/moderation/warnings/{request}"
	m.Handle(path, BotSessionRoute(d.Sessions, d.Limiter)(path, http.HandlerFunc(d.getBotWarningReceipt)))
}

func (d Deps) getBotWarningReceipt(w http.ResponseWriter, r *http.Request) {
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
		installation.Capabilities&uint16(botCapWarnings) == 0 || s.Bot.Capabilities&uint16(botCapWarnings) == 0 {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "Warnings-capable Bot installation required"))
		return
	}
	row, err := d.Repo.GetManualWarningAction(r.Context(), installation.CommunityID, requestID)
	if err != nil {
		server.WriteError(w, notFound(err))
		return
	}
	if row.CommunityID != installation.CommunityID || row.RequestID != requestID || row.ActionID.IsZero() ||
		row.ActorUserID.IsZero() || row.ActorDeviceID.IsZero() || row.TargetUserID.IsZero() || row.Revision == 0 || row.AcceptedAt <= 0 {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "accepted warning receipt is unavailable"))
		return
	}
	_ = server.WriteCBOR(w, http.StatusOK, []any{row.RequestID, row.ActionID, row.CommunityID, row.ActorUserID,
		row.ActorDeviceID, row.TargetUserID, row.Revision, uint64(row.AcceptedAt)})
}
