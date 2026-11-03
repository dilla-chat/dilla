package service

import (
	"context"
	"errors"
	"math"

	"github.com/dilla-chat/dilla/internal/bot/community/welcome"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

const (
	welcomeSettingsSection = uint64(5)
	welcomeSettingsArea    = uint64(8)
	welcomeSettingsKey     = "dilla.bot.welcome.config.v1"
)

// WelcomeInteractions reads and writes encrypted welcome settings and previews
// templates. The authenticated actor is supplied by InteractionDispatcher.
type WelcomeInteractions struct {
	Authority   AnnouncementInteractionAuthority
	Native      WelcomeSettingsNative
	CommunityID id.ID
}

func (h WelcomeInteractions) Handlers() map[protocol.Operation]InteractionHandlerSpec {
	return map[protocol.Operation]InteractionHandlerSpec{
		protocol.OpSettingsGet:    {RecomputeOnReplay: true, Handle: h.getSettings},
		protocol.OpSettingsPut:    {RevisionInHandler: true, Handle: h.putSettings},
		protocol.OpWelcomePreview: {RecomputeOnReplay: true, Handle: h.preview},
	}
}

// WelcomeSettingsNative is the encrypted state surface used by welcome settings.
type WelcomeSettingsNative interface {
	Binding() native.Binding
	StateGet(context.Context, uint64, []byte) (native.StateValue, error)
	StateTransaction(context.Context, id.ID, []native.StateChange) ([]uint64, error)
}

func (h WelcomeInteractions) authorizeSettings(ctx context.Context, actor InteractionActor, request protocol.Request, mutation bool) (bool, InteractionResult, error) {
	if h.Authority == nil || h.Native == nil || h.CommunityID.IsZero() || actor.User.IsZero() {
		return false, InteractionResult{}, errors.New("missing welcome settings dependency")
	}
	if request.CommunityID != h.CommunityID || len(request.Args) == 0 || request.Args[0] != welcomeSettingsSection {
		return false, InteractionResult{Status: interactionStatusForbidden}, nil
	}
	if h.Native.Binding().Community != h.CommunityID {
		return false, InteractionResult{}, errors.New("welcome settings native binding mismatch")
	}
	allowed, err := h.Authority.CanManageCommunity(ctx, actor.User, h.CommunityID)
	if err != nil {
		return false, InteractionResult{}, err
	}
	if !allowed {
		return false, InteractionResult{Status: interactionStatusForbidden}, nil
	}
	if mutation && (request.Revision == nil || *request.Revision >= math.MaxInt64 || request.Expires == nil) {
		return false, InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	return true, InteractionResult{}, nil
}

//nolint:nilerr // Invalid client input intentionally maps to an unavailable protocol result.
func (h WelcomeInteractions) getSettings(ctx context.Context, actor InteractionActor, request protocol.Request) (InteractionResult, error) {
	if len(request.Args) != 1 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	allowed, denied, err := h.authorizeSettings(ctx, actor, request, false)
	if err != nil || !allowed {
		return denied, err
	}
	state, err := h.Native.StateGet(ctx, welcomeSettingsArea, []byte(welcomeSettingsKey))
	if err != nil {
		return InteractionResult{}, err
	}
	if state.Revision > math.MaxInt64 || state.Tombstone {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	data := state.Value
	if state.Revision == 0 {
		data, err = cborx.Marshal([]any{})
		if err != nil {
			return InteractionResult{}, err
		}
	} else if _, err = decodeWelcomeSettings(data); err != nil {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	revision := state.Revision
	return InteractionResult{Revision: &revision, Data: data}, nil
}

//nolint:nilerr // Invalid client input intentionally maps to an unavailable protocol result.
func (h WelcomeInteractions) putSettings(ctx context.Context, actor InteractionActor, request protocol.Request) (InteractionResult, error) {
	if len(request.Args) != 2 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	allowed, denied, err := h.authorizeSettings(ctx, actor, request, true)
	if err != nil || !allowed {
		return denied, err
	}
	raw, ok := request.Args[1].([]byte)
	if !ok || len(raw) > 2400 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	cfg, err := decodeWelcomeSettings(raw)
	if err != nil {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	change, err := welcome.ConfigStateChange(cfg)
	if err != nil {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	state, err := h.Native.StateGet(ctx, welcomeSettingsArea, []byte(welcomeSettingsKey))
	if err != nil {
		return InteractionResult{}, err
	}
	if state.Revision > math.MaxInt64 || state.Tombstone {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	change.ExpectedRevision = *request.Revision
	revisions, txErr := h.Native.StateTransaction(ctx, request.RequestID, []native.StateChange{change})
	if txErr != nil {
		if state.Revision != *request.Revision {
			current := state.Revision
			return InteractionResult{Status: interactionStatusConflict, Revision: &current}, nil
		}
		latest, readErr := h.Native.StateGet(ctx, welcomeSettingsArea, []byte(welcomeSettingsKey))
		if readErr == nil && latest.Revision != *request.Revision {
			current := latest.Revision
			return InteractionResult{Status: interactionStatusConflict, Revision: &current}, nil
		}
		return InteractionResult{}, txErr
	}
	if len(revisions) != 1 || revisions[0] != *request.Revision+1 || revisions[0] > math.MaxInt64 {
		return InteractionResult{}, errors.New("invalid welcome settings revision")
	}
	revision := revisions[0]
	data, err := cborx.Marshal([]any{})
	if err != nil {
		return InteractionResult{}, err
	}
	return InteractionResult{Revision: &revision, Data: data}, nil
}

func decodeWelcomeSettings(raw []byte) (welcome.Config, error) {
	var fields []any
	if cborx.ExpectMajor(raw, cborx.MajorArray) != nil || cborx.Unmarshal(raw, &fields) != nil || len(fields) != 4 || fields[0] != uint64(1) {
		return welcome.Config{}, errors.New("invalid welcome settings document")
	}
	channel, ok := fields[1].([]byte)
	if !ok || len(channel) != 16 {
		return welcome.Config{}, errors.New("invalid welcome settings channel")
	}
	template, okTemplate := fields[2].(string)
	server, okServer := fields[3].(string)
	if !okTemplate || !okServer {
		return welcome.Config{}, errors.New("invalid welcome settings text")
	}
	var cfg welcome.Config
	copy(cfg.ChannelID[:], channel)
	cfg.Template, cfg.ServerName = template, server
	if _, err := welcome.ConfigStateChange(cfg); err != nil {
		return welcome.Config{}, err
	}
	return cfg, nil
}

//nolint:nilerr // Invalid client input intentionally maps to an unavailable protocol result.
func (h WelcomeInteractions) preview(ctx context.Context, actor InteractionActor, request protocol.Request) (InteractionResult, error) {
	if h.Authority == nil || h.CommunityID.IsZero() || actor.User.IsZero() {
		return InteractionResult{}, errors.New("missing welcome preview authority")
	}
	if request.CommunityID != h.CommunityID {
		return InteractionResult{Status: interactionStatusForbidden}, nil
	}
	allowed, err := h.Authority.CanManageCommunity(ctx, actor.User, h.CommunityID)
	if err != nil {
		return InteractionResult{}, err
	}
	if !allowed {
		return InteractionResult{Status: interactionStatusForbidden}, nil
	}
	if len(request.Args) != 1 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	template, ok := request.Args[0].(string)
	if !ok {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	text, err := welcome.Preview(template, "member", "server", "channel")
	if err != nil {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	data, err := cborx.Marshal([]any{uint64(1), text})
	if err != nil {
		return InteractionResult{}, err
	}
	return InteractionResult{Status: 0, Data: data}, nil
}
