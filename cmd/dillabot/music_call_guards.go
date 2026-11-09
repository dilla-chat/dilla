//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/intake"
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
	errMusicCatchUpIncomplete    = errors.New("music call group: still behind after the catch-up budget")
	errMusicInstallationNotReady = errors.New("music: the installation is not ready with music")
	errMusicBotCannotSpeak       = errors.New("music: the Bot may not connect and speak in the voice channel")
)

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
}

const musicCatchUpPasses = 16

func (c musicCallCatchUp) run(ctx context.Context, group id.ID) error {
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

func (a musicAdmission) admit(ctx context.Context, epoch native.CallMediaEpoch) error {
	if a.ready == nil || !a.ready(ctx) {
		return errMusicInstallationNotReady
	}
	if a.authz == nil || !a.authz.Ready() || a.botUser.IsZero() || !a.authz.Allowed(a.community, a.channel, a.botUser, musicBotConnectSpeak, false) {
		return errMusicBotCannotSpeak
	}
	if a.trust == nil {
		return errors.New("music call-group trust unavailable")
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
