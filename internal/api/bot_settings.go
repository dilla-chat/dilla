package api

import (
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"

	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/store"
)

// Task 10c: the Bot page's Restart, the runtime's reported version, and the Music page's default
// volume and idle-leave timeout. All three live on the installation row (migration 00050).

// botRuntimeVersion is what a runtime may report as its version: 1..32 bytes of a build string
// such as "0.4.2", "0.4.2+abc123" or "dev".
var botRuntimeVersion = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_-]{0,31}$`)

func registerBotSettings(m *server.Mux, d Deps) {
	botSession := BotSessionRoute(d.Sessions, d.Limiter)
	musicPath := "PUT /v1/communities/{community}/bot/music-settings"
	m.Handle(musicPath, botSession(musicPath, http.HandlerFunc(d.PutBotMusicSettings)))
	restartPath := "POST /v1/communities/{community}/bot/restart"
	m.Handle(restartPath, botSession(restartPath, http.HandlerFunc(d.RestartBotInstallation)))
	m.HandleFunc("PUT /v1/bot-runtime/installations/{installation}/status", d.PutBotRuntimeStatus)
}

// botInstallationValueV2 is the widened installation: the eight base fields followed by
// [restart_requested, restart_handled, runtime_version|null, music_volume, music_idle_minutes].
func botInstallationValueV2(row store.BotInstallationRow) ([]any, error) {
	value, err := botInstallationValue(row)
	if err != nil {
		return nil, err
	}
	var version any
	if row.RuntimeVersion != nil {
		version = *row.RuntimeVersion
	}
	return append(value, row.RestartRequested, row.RestartHandled, version, uint64(row.MusicVolume), uint64(row.MusicIdleMinutes)), nil
}

// botRuntimeInstallationValue is what a runtime is shown. A runtime that asks for the widened
// shape (v=2) gets it with the full capability mask; an older runtime gets the eight base fields
// with only the original capability byte, which it can parse.
func botRuntimeInstallationValue(row store.BotInstallationRow, widened bool) ([]any, error) {
	if widened {
		return botInstallationValueV2(row)
	}
	legacy := row
	legacy.Capabilities &= uint16(botLegacyCaps)
	return botInstallationValue(legacy)
}

type botMusicSettingsRequest struct {
	_              struct{} `cbor:",toarray"`
	InstallationID id.ID
	Volume         uint64
	IdleMinutes    uint64
}

// PutBotMusicSettings stores the Music page's defaults. It needs manage_community like every
// installation setting; the values are exactly the prototype's options.
func (d Deps) PutBotMusicSettings(w http.ResponseWriter, r *http.Request) {
	s, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	cid, err := server.PathID(r, "community")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var req botMusicSettingsRequest
	if err := server.DecodeBody(w, r, maxBotRuntimeBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	if req.InstallationID.IsZero() || req.Volume > 255 || req.IdleMinutes > 255 ||
		!slices.Contains(store.BotMusicVolumes, uint8(req.Volume)) || !slices.Contains(store.BotMusicIdleMinutes, uint8(req.IdleMinutes)) {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "volume must be 25, 50, 75 or 100 and idle minutes 5, 15 or 30"))
		return
	}
	if _, _, err := humanForBot(r, d.Repo, cid, s); err != nil {
		server.WriteError(w, err)
		return
	}
	if err := requireBotWire(s.WireVersion); err != nil {
		server.WriteError(w, err)
		return
	}
	var row store.BotInstallationRow
	err = d.Repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := tx.LockBotCommunity(r.Context(), cid); err != nil {
			return notFound(err)
		}
		if _, _, err := humanForBot(r, tx, cid, s); err != nil {
			return err
		}
		current, err := tx.GetActiveBotInstallationByCommunity(r.Context(), cid)
		if err != nil {
			return notFound(err)
		}
		if current.ID != req.InstallationID {
			return botConflict("bot installation changed")
		}
		volume, idle := uint8(req.Volume), uint8(req.IdleMinutes) //nolint:gosec // G115: checked against the option lists above
		if err := tx.SetBotMusicSettings(r.Context(), current.ID, volume, idle); err != nil {
			return err
		}
		if err := tx.Audit(r.Context(), store.AuditRow{
			Actor: &s.UserID, Action: "bot.music_settings", Target: current.ID.String(),
			Detail: cid.String() + " volume=" + strconv.Itoa(int(volume)) + " idle=" + strconv.Itoa(int(idle)), At: d.Clock.Now().Unix(),
		}); err != nil {
			return err
		}
		row, err = tx.GetBotInstallation(r.Context(), current.ID)
		return err
	})
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	value, err := botInstallationValueV2(row)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	d.write(w, r, http.StatusOK, value)
}

type botRestartRequest struct {
	_              struct{} `cbor:",toarray"`
	InstallationID id.ID
}

// RestartBotInstallation records a restart request for the runtime to pick up on its next poll.
// It needs manage_community. A request while an earlier one is still unhandled joins it, so a
// double click or a repeated request restarts the installation once.
func (d Deps) RestartBotInstallation(w http.ResponseWriter, r *http.Request) {
	s, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	cid, err := server.PathID(r, "community")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var req botRestartRequest
	if err := server.DecodeBody(w, r, maxBotRuntimeBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	if req.InstallationID.IsZero() {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "installation id is required"))
		return
	}
	if _, _, err := humanForBot(r, d.Repo, cid, s); err != nil {
		server.WriteError(w, err)
		return
	}
	if err := requireBotWire(s.WireVersion); err != nil {
		server.WriteError(w, err)
		return
	}
	var requested, handled uint64
	err = d.Repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := tx.LockBotCommunity(r.Context(), cid); err != nil {
			return notFound(err)
		}
		if _, _, err := humanForBot(r, tx, cid, s); err != nil {
			return err
		}
		current, err := tx.GetActiveBotInstallationByCommunity(r.Context(), cid)
		if err != nil {
			return notFound(err)
		}
		if current.ID != req.InstallationID {
			return botConflict("bot installation changed")
		}
		if current.State == store.BotInstallationDisabled || current.RuntimeID == nil {
			return botConflict("no runtime is running this installation")
		}
		before := current.RestartRequested
		requested, err = tx.RequestBotRestart(r.Context(), current.ID)
		if err != nil {
			return err
		}
		handled = current.RestartHandled
		if requested == before {
			return nil // joined a pending request: nothing new to audit
		}
		return tx.Audit(r.Context(), store.AuditRow{
			Actor: &s.UserID, Action: "bot.restart", Target: current.ID.String(),
			Detail: cid.String() + " request=" + strconv.FormatUint(requested, 10), At: d.Clock.Now().Unix(),
		})
	})
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	d.write(w, r, http.StatusOK, []any{requested, handled})
}

type botRuntimeStatusRequest struct {
	_       struct{} `cbor:",toarray"`
	Version *string
	Handled uint64
}

// PutBotRuntimeStatus is the runtime reporting, for one installation it owns, its build version
// and the last restart request it has finished. An older runtime never calls it, so its
// installation shows no version.
func (d Deps) PutBotRuntimeStatus(w http.ResponseWriter, r *http.Request) {
	runtimeID, ok := d.authenticateBotRuntime(w, r, true)
	if !ok {
		return
	}
	installationID, err := server.PathID(r, "installation")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var req botRuntimeStatusRequest
	if err := server.DecodeBody(w, r, maxBotRuntimeBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	if req.Version != nil && !botRuntimeVersion.MatchString(*req.Version) || req.Handled > 1<<62 {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "version must be 1..32 characters of [0-9A-Za-z.+_-]"))
		return
	}
	bearer := runtimeBearer(r)
	err = d.Repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := verifyLiveRuntime(r.Context(), tx, runtimeID, bearer); err != nil {
			return err
		}
		return tx.SetBotRuntimeStatus(r.Context(), installationID, runtimeID, req.Version, req.Handled)
	})
	if errors.Is(err, store.ErrNotFound) {
		// Another runtime's, removed or unknown: one indistinguishable answer.
		server.WriteError(w, server.Errorf(server.CodeNotFound, "no such object"))
		return
	}
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
