package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/permbits"
	"github.com/dilla-chat/dilla/internal/store"
)

const (
	musicBotCapability     uint16 = 1 << 0 // bot_installations capability bit 0
	musicPermissionConnect uint64 = 1 << 7
	musicPermissionSpeak   uint64 = 1 << 8
)

type musicAuthoritySession interface {
	BotActorAuthority(context.Context, id.ID, id.ID) (client.ActorAuthority, bool, error)
	ChannelCall(context.Context, id.ID) (client.ChannelCallState, error)
	ChannelPermissions(context.Context, id.ID) (uint64, error)
	BotActorChannelAuthority(context.Context, id.ID, id.ID, id.ID) (uint64, bool, error)
}

// musicPermissionView is api.PermViewChannel: who may read a voice channel's player (security review M2).
const musicPermissionView = permbits.ViewChannel

// musicPermissionControl is api.PermControlMusic: who may change playback in a voice channel.
const musicPermissionControl = permbits.ControlMusic

type musicAuthorityStore interface {
	GetActiveBotInstallationByCommunity(context.Context, id.ID) (store.BotInstallationRow, error)
}

// MusicAuthz is the Bot's feed-state permission cache for the community (intake.Authz, Bot v2 Task 15): roles,
// channel overwrites of voice channels (feed kind 9) and member timeouts (kind 10), in memory and ordered by the feed
// barrier before a command's intake check.
type MusicAuthz interface {
	Ready() bool
	Allowed(community, channel, user id.ID, permissionBit uint64, communityScoped bool) bool
	Permissions(user id.ID) uint64
	TimedOut(user id.ID, now time.Time) bool
}

// ErrMusicAuthzNotReady refuses a music check while the feed's first snapshot is not applied (retryable).
var ErrMusicAuthzNotReady = errors.New("music authority: the feed state is not ready")

// musicAuthzAdministrator is api.PermAdministrator (owners resolve to every bit, this one included).
const musicAuthzAdministrator uint64 = 1 << 20

// CommunityMusicInteractionAuthority resolves permissions, installation state,
// and the live call roster afresh for each authorization check.
//
// With Authz set (Bot v2 Task 19, ruling on I1/I3) every permission and membership check, the Bot's own connect and
// speak included, resolves from the feed state in memory, and the only request left is the call read (ChannelCall)
// for "is the actor in this call", made once per command (withMusicCallMemo) and shared by CurrentCall and the
// in-call check. A recheck right before a commit is therefore in memory and always made.
type CommunityMusicInteractionAuthority struct {
	Session        musicAuthoritySession
	Store          musicAuthorityStore
	InstallationID id.ID
	CommunityID    id.ID
	Authz          MusicAuthz
	BotUser        id.ID
	Now            func() time.Time
}

type musicCallMemoKey struct{}

// musicCallMemo keeps one command's call reads: the call state of a channel is read once per command.
type musicCallMemo struct {
	mu     sync.Mutex
	states map[id.ID]client.ChannelCallState
}

// withMusicCallMemo makes the music authority read each channel's call once for the rest of ctx (one command).
func withMusicCallMemo(ctx context.Context) context.Context {
	if _, ok := ctx.Value(musicCallMemoKey{}).(*musicCallMemo); ok {
		return ctx
	}
	return context.WithValue(ctx, musicCallMemoKey{}, &musicCallMemo{states: map[id.ID]client.ChannelCallState{}})
}

// refreshMusicCallMemo makes the next call read of ctx's command go to dillad again: the recheck before a commit
// (after the resolve, and on every retry) sees a call that changed and an actor who left (Bot v2 Task 19 fix round 2).
func refreshMusicCallMemo(ctx context.Context) {
	if memo, ok := ctx.Value(musicCallMemoKey{}).(*musicCallMemo); ok {
		memo.mu.Lock()
		clear(memo.states)
		memo.mu.Unlock()
	}
}

func (a *CommunityMusicInteractionAuthority) channelCall(ctx context.Context, channel id.ID) (client.ChannelCallState, error) {
	memo, ok := ctx.Value(musicCallMemoKey{}).(*musicCallMemo)
	if !ok || a.Authz == nil {
		return a.Session.ChannelCall(ctx, channel)
	}
	memo.mu.Lock()
	state, seen := memo.states[channel]
	memo.mu.Unlock()
	if seen {
		return state, nil
	}
	state, err := a.Session.ChannelCall(ctx, channel)
	if err == nil {
		memo.mu.Lock()
		memo.states[channel] = state
		memo.mu.Unlock()
	}
	return state, err
}

func (a *CommunityMusicInteractionAuthority) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// authzReady is the feed state, or ErrMusicAuthzNotReady.
func (a *CommunityMusicInteractionAuthority) authzReady() error {
	if !a.Authz.Ready() || a.BotUser.IsZero() {
		return ErrMusicAuthzNotReady
	}
	return nil
}

// BotCanSpeak reports whether the Bot itself may connect and speak in channel, from the feed state (no request).
func (a *CommunityMusicInteractionAuthority) BotCanSpeak(channel id.ID) bool {
	if a == nil || a.Authz == nil || a.authzReady() != nil {
		return false
	}
	return a.Authz.Allowed(a.CommunityID, channel, a.BotUser, musicPermissionConnect|musicPermissionSpeak, false)
}

func (a *CommunityMusicInteractionAuthority) CurrentCall(ctx context.Context, channel id.ID) (MusicCallBinding, error) {
	if err := a.valid(); err != nil || channel.IsZero() {
		return MusicCallBinding{}, errors.New("invalid music authority binding")
	}
	installation, err := a.currentInstallation(ctx)
	if err != nil {
		return MusicCallBinding{}, err
	}
	if installation.ID.IsZero() {
		return MusicCallBinding{}, nil
	}
	state, err := a.channelCall(ctx, channel)
	if err != nil {
		return MusicCallBinding{}, err
	}
	if state.CallID == nil || state.GroupID == nil {
		return MusicCallBinding{ChannelID: channel}, nil
	}
	return MusicCallBinding{CallID: *state.CallID, GroupID: *state.GroupID, ChannelID: channel, Active: true}, nil
}

//nolint:nilerr // Invalid client input intentionally maps to an unavailable protocol result.
func (a *CommunityMusicInteractionAuthority) CanUse(ctx context.Context, actor InteractionActor, call MusicCallBinding) (bool, error) {
	if err := a.validCall(call); err != nil || actor.User.IsZero() {
		return false, nil
	}
	installation, err := a.currentInstallation(ctx)
	if err != nil || installation.ID.IsZero() {
		return false, err
	}
	if a.Authz != nil {
		if err := a.authzReady(); err != nil {
			return false, err
		}
		// The Bot may connect and speak there, and the actor may see the voice channel (security review M2).
		return a.BotCanSpeak(call.ChannelID) && a.Authz.Allowed(a.CommunityID, call.ChannelID, actor.User, musicPermissionView, false), nil
	}
	bits, err := a.Session.ChannelPermissions(ctx, call.ChannelID)
	if err != nil || bits&(musicPermissionConnect|musicPermissionSpeak) != musicPermissionConnect|musicPermissionSpeak {
		return false, err
	}
	_, present, err := a.Session.BotActorAuthority(ctx, a.CommunityID, actor.User)
	if err != nil || !present {
		return false, err
	}
	// Security review M2: the player (title, requester, queue) is only for members who can see this voice channel,
	// resolved per channel with its overrides.
	view, visible, err := a.Session.BotActorChannelAuthority(ctx, a.CommunityID, actor.User, call.ChannelID)
	if err != nil || !visible || view&musicPermissionView == 0 {
		return false, err
	}
	// Managers can issue an emergency stop while away from the call. All other
	// state changes are separately gated by CanDJ and call membership.
	return true, nil
}

// CanDJ reports whether actor may change playback in call: the actor must be a
// current member who is in this exact call, and hold view_channel and
// control_music in the call's voice channel as the server resolves it now,
// with that channel's overwrites (Task 10c; this replaced the per-installation
// DJ role approval). Owners and administrators resolve to every bit, so they
// always can, but still only from inside the call and only while the Bot
// itself may connect and speak there.
//
//nolint:nilerr // Invalid client input intentionally maps to an unavailable protocol result.
func (a *CommunityMusicInteractionAuthority) CanDJ(ctx context.Context, actor InteractionActor, call MusicCallBinding) (bool, error) {
	if err := a.validCall(call); err != nil || actor.User.IsZero() {
		return false, nil
	}
	installation, err := a.currentInstallation(ctx)
	if err != nil || installation.ID.IsZero() {
		return false, err
	}
	if a.Authz != nil {
		if err := a.authzReady(); err != nil {
			return false, err
		}
		if !a.Authz.Allowed(a.CommunityID, call.ChannelID, actor.User, musicPermissionView|musicPermissionControl, false) ||
			a.Authz.TimedOut(actor.User, a.now()) || !a.BotCanSpeak(call.ChannelID) {
			return false, nil
		}
		return a.actorInCall(ctx, actor.User, call)
	}
	_, present, err := a.Session.BotActorAuthority(ctx, a.CommunityID, actor.User)
	if err != nil || !present {
		return false, err
	}
	member, err := a.actorInCall(ctx, actor.User, call)
	if err != nil || !member {
		return false, err
	}
	bits, visible, err := a.Session.BotActorChannelAuthority(ctx, a.CommunityID, actor.User, call.ChannelID)
	if err != nil || !visible || bits&(musicPermissionView|musicPermissionControl) != musicPermissionView|musicPermissionControl {
		return false, err
	}
	return a.botCanSpeak(ctx, call.ChannelID)
}

//nolint:nilerr // Invalid client input intentionally maps to an unavailable protocol result.
func (a *CommunityMusicInteractionAuthority) CanEmergencyStop(ctx context.Context, actor InteractionActor, call MusicCallBinding) (bool, error) {
	if err := a.validCall(call); err != nil || actor.User.IsZero() {
		return false, nil
	}
	installation, err := a.currentInstallation(ctx)
	if err != nil || installation.ID.IsZero() {
		return false, err
	}
	if a.Authz != nil {
		if err := a.authzReady(); err != nil {
			return false, err
		}
		return a.Authz.Permissions(actor.User)&musicAuthzAdministrator != 0, nil
	}
	resolved, present, err := a.Session.BotActorAuthority(ctx, a.CommunityID, actor.User)
	return present && (resolved.Owner || resolved.Administrator), err
}

func (a *CommunityMusicInteractionAuthority) actorInCall(ctx context.Context, user id.ID, binding MusicCallBinding) (bool, error) {
	state, err := a.channelCall(ctx, binding.ChannelID)
	if err != nil {
		return false, err
	}
	if state.CallID == nil || *state.CallID != binding.CallID {
		return false, nil
	}
	for _, participant := range state.Participants {
		if participant.UserID == user {
			return true, nil
		}
	}
	return false, nil
}

func (a *CommunityMusicInteractionAuthority) currentInstallation(ctx context.Context) (store.BotInstallationRow, error) {
	installation, err := a.Store.GetActiveBotInstallationByCommunity(ctx, a.CommunityID)
	if errors.Is(err, store.ErrNotFound) {
		return store.BotInstallationRow{}, nil
	}
	if err != nil {
		return store.BotInstallationRow{}, err
	}
	if installation.ID != a.InstallationID || installation.State != store.BotInstallationReady || installation.RemovedAt != nil || installation.Capabilities&musicBotCapability == 0 {
		return store.BotInstallationRow{}, nil
	}
	return installation, nil
}

func (a *CommunityMusicInteractionAuthority) botCanSpeak(ctx context.Context, channel id.ID) (bool, error) {
	bits, err := a.Session.ChannelPermissions(ctx, channel)
	if err != nil {
		return false, err
	}
	return bits&(musicPermissionConnect|musicPermissionSpeak) == musicPermissionConnect|musicPermissionSpeak, nil
}

func (a *CommunityMusicInteractionAuthority) valid() error {
	if a == nil || a.Session == nil || a.Store == nil || a.CommunityID.IsZero() || a.InstallationID.IsZero() {
		return errors.New("missing music interaction authority dependency")
	}
	return nil
}

func (a *CommunityMusicInteractionAuthority) validCall(call MusicCallBinding) error {
	if err := a.valid(); err != nil || !validMusicCall(call, call.ChannelID) {
		return errors.New("invalid music call binding")
	}
	return nil
}

var _ MusicInteractionAuthority = (*CommunityMusicInteractionAuthority)(nil)
