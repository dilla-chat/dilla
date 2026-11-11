//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/intake"
	"github.com/dilla-chat/dilla/internal/bot/music"
	"github.com/dilla-chat/dilla/internal/bot/music/livekit"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/service"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// Bot v2 Task 19 fix round 1: the guards that keep the Bot's music under its call's current epoch and current
// authority (review C1, I2, M4).

// musicBotConnectSpeak is api.PermConnect | api.PermSpeak: the Bot's own rights in a voice channel.
const musicBotConnectSpeak = 1<<7 | 1<<8

var (
	errMusicCatchUpIncomplete          = errors.New("music call group: still behind after the catch-up budget")
	errMusicInstallationNotReady error = &musicNotAdmittedError{"music: the installation is not ready with music"}
	errMusicBotCannotSpeak       error = &musicNotAdmittedError{"music: the Bot may not connect and speak in the voice channel"}
	errMusicTrustUnavailable     error = &musicNotAdmittedError{"music call-group trust unavailable"}
)

// musicNotAdmittedError is an admission refusal: the Bot may not publish in the call right now, which says nothing
// about the track (v0.1.0-bot2.2 final review I2). It matches music.ErrNotAdmitted, so the track stays queued, paused.
type musicNotAdmittedError struct{ msg string }

func (e *musicNotAdmittedError) Error() string      { return e.msg }
func (*musicNotAdmittedError) Is(target error) bool { return target == music.ErrNotAdmitted }

// musicCallSyncer is service.SourceSync as the catch-up uses it.
type musicCallSyncer interface {
	Sync(context.Context, *client.Session, native.GroupExpected) (service.SourceSyncResult, error)
}

// musicCallCatchUp pages a call group's newer handshakes and messages (the members' commits) into the helper
// (ApplyGroup, through the source sync's paging and validation), so the helper's epoch is the call's current one. A
// group the helper lost or must resync is terminal.
type musicCallCatchUp struct {
	sync               musicCallSyncer
	session            *client.Session
	community, channel id.ID
	// local is the helper's view of the group (native InspectGroup) and remote dillad's (one read): a group the
	// helper is current with costs that one read, and the paging (with its cursor write) runs only when behind.
	local  func(context.Context, id.ID) (native.GroupInfo, error)
	remote func(context.Context, id.ID) (client.GroupInfo, error)
	// helperNow resolves the installation's current helper on every check (v0.1.0-bot2.2 F4, as the commands do since
	// Task 3): helper.ErrClosed once the helper this catch-up uses is closed, unusable or no longer the registered one.
	// Nil checks nothing.
	helperNow func() error
}

const musicCatchUpPasses = 16

// musicHelperNow is a catch-up's helperNow: nil while own is still the installation's registered helper and usable;
// otherwise helper.ErrClosed. The identity check current != own holds because the registry and the pass hold the same
// *native.Client: a pass reuses the retained client (retainedNativeClient) and registers exactly that one, so the
// two differ only once the registered helper was retired or replaced, and then own is closed.
func musicHelperNow(registry *musicQueueRegistry, installation, community id.ID, own musicRuntimeNative) error {
	current := registry.Native(installation, community)
	if current == nil || current != own || !nativeUsable(current) {
		return helper.ErrClosed
	}
	return nil
}

// run catches group up. A closed or unusable helper is terminal at once (the track stops at its first failed check
// with error_class=closed, instead of after maxCatchUpFailures checks).
func (c musicCallCatchUp) run(ctx context.Context, group id.ID) error {
	if c.helperNow != nil {
		if err := c.helperNow(); err != nil {
			return fmt.Errorf("%w: %w", livekit.ErrCallCatchUpTerminal, err)
		}
	}
	err := c.catchUp(ctx, group)
	if err != nil && !errors.Is(err, livekit.ErrCallCatchUpTerminal) && errors.Is(err, helper.ErrClosed) {
		return fmt.Errorf("%w: %w", livekit.ErrCallCatchUpTerminal, err)
	}
	return err
}

func (c musicCallCatchUp) catchUp(ctx context.Context, group id.ID) error {
	if c.sync == nil || c.session == nil {
		return errors.New("music call group catch-up unavailable")
	}
	// The members' commits decide which key the Bot's media is under: the catch-up takes the Bot's next request token,
	// as a pause or a skip does, rather than waiting behind background work while a removed member could still decrypt.
	ctx = client.WithImmediateControl(ctx)
	if c.local != nil && c.remote != nil {
		local, err := c.local(ctx, group)
		if err != nil {
			return err
		}
		remote, err := c.remote(ctx, group)
		if err != nil {
			return err
		}
		if remote.NextSequence != 0 && remote.NextSequence <= local.NextSequence {
			return nil
		}
	}
	expected := native.GroupExpected{Group: group, Community: c.community, Channel: c.channel, Kind: 1}
	for pass := 0; pass < musicCatchUpPasses; pass++ {
		result, err := c.sync.Sync(ctx, c.session, expected)
		switch {
		case errors.Is(err, service.ErrSourceSyncBusy):
			// Another pass of this group runs (a hint and a tick at once): wait for it briefly, then look again.
			if err := waitMusicCallJoin(ctx, 20*time.Millisecond); err != nil {
				return err
			}
			continue
		case errors.Is(err, service.ErrSourceGroupGone), errors.Is(err, service.ErrSourceNeedsResync):
			return fmt.Errorf("%w: %w", livekit.ErrCallCatchUpTerminal, err)
		case err != nil:
			return err
		}
		if !result.More {
			return nil
		}
	}
	return errMusicCatchUpIncomplete
}

// musicAdmission is the in-memory check before every publication and every re-key (I2, M4): the installation is ready
// with music (its row, read again only after an op 54), the Bot may connect and speak in the channel (feed state), and
// no user in the epoch's roster is held by membership trust.
type musicAdmission struct {
	community, channel, botUser id.ID
	authz                       service.MusicAuthz
	trust                       musicJoinTrust
	ready                       func(context.Context) bool
}

// musicAdmitReadDeadline bounds admission's installation read (the row is read again from dillad when its cached copy
// is past its max age): a slow dillad fails that tick's admission closed instead of holding the monitor, and with it
// the catch-up a removed member's window depends on (Bot v2 Task 19 fix round 3, R2-4).
var musicAdmitReadDeadline = 2 * time.Second

func (a musicAdmission) admit(ctx context.Context, epoch native.CallMediaEpoch) error {
	if a.ready == nil {
		return errMusicInstallationNotReady
	}
	readCtx, cancel := context.WithTimeout(ctx, musicAdmitReadDeadline)
	ready := a.ready(readCtx)
	cancel()
	if !ready {
		return errMusicInstallationNotReady
	}
	if a.authz == nil || !a.authz.Ready() || a.botUser.IsZero() || !a.authz.Allowed(a.community, a.channel, a.botUser, musicBotConnectSpeak, false) {
		return errMusicBotCannotSpeak
	}
	if a.trust == nil {
		return errMusicTrustUnavailable
	}
	for _, member := range epoch.Roster {
		if !member.UserID.IsZero() && member.UserID != a.botUser && a.trust.Held(member.UserID) {
			return &errMusicCallJoinHeld{}
		}
	}
	return nil
}

// installationMusicReady reads the installation row (reused until the next op 54) and answers whether it is the
// installation, ready, not removed, with the music capability.
func installationMusicReady(store runtimeMusicAuthorityStore) func(context.Context) bool {
	return func(ctx context.Context) bool {
		row, err := store.GetActiveBotInstallationByCommunity(ctx, store.community)
		return err == nil && row.ID == store.installation && row.State == botInstallationReady && row.RemovedAt == nil && row.Capabilities&botMusicCapability != 0
	}
}

const botInstallationReady = store.BotInstallationReady

// musicAuthz is installation's feed-state permission cache (the sealed intake's Authz, Task 15).
func (r *botGatewayRuntime) musicAuthz(installation, community id.ID) *intake.Authz {
	return r.sealed.entry(installation, community).authz
}

// newMusicCallGuards builds a voice room's guards. subscribe asks dillad for the call group's handshake and epoch
// hints, which nudge the playing track to catch up at once (the monitor tick covers a lost hint).
func newMusicCallGuards(installation id.ID, catchUp musicCallCatchUp, admission musicAdmission, subscribe func(context.Context, []id.ID)) livekit.CallGuards {
	return livekit.CallGuards{
		CatchUp: catchUp.run,
		Admit:   admission.admit,
		Watch: func(group id.ID, nudge func()) func() {
			if subscribe != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				subscribe(ctx, []id.ID{group})
				cancel()
			}
			return musicSignals.watchCallGroup(installation, group, nudge)
		},
	}
}
