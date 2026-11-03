package api

import (
	"context"
	"time"

	"github.com/livekit/protocol/livekit"

	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// VoiceGateway is where voice_state frames go; *gateway.Gateway is one.
type VoiceGateway interface {
	DeliverUser(userID id.ID, f gateway.Frame)
}

// The voice_state flag bits (protocol/02 § Gateway frames, DEV-60, ruling F10). The instance sets
// bits 0, 3 and 4 from what its SFU reports; bits 1 and 2 carry client-reported mute
// and deafen state after the live participant and channel audience checks.
const (
	VoiceInCall   uint64 = 1 << 0
	VoiceSelfMute uint64 = 1 << 1
	VoiceSelfDeaf uint64 = 1 << 2
	VoiceVideo    uint64 = 1 << 3
	VoiceScreen   uint64 = 1 << 4
)

// voiceTrackFlags reads the current published video sources from the SFU participant.
func voiceTrackFlags(part *livekit.ParticipantInfo) uint64 {
	var flags uint64
	for _, track := range part.GetTracks() {
		switch track.GetSource() {
		case livekit.TrackSource_CAMERA:
			flags |= VoiceVideo
		case livekit.TrackSource_SCREEN_SHARE:
			flags |= VoiceScreen
		}
	}
	return flags
}

const (
	// announceInterval is the least time between two deliveries of queued voice_state frames: what
	// one device's flags do within it is coalesced into its latest state (CALLS-5).
	announceInterval = 250 * time.Millisecond
	// maxPendingAnnouncements bounds the queued voice_state, one entry per (call, device); one beyond
	// it is dropped with a WARN (voice_state is advisory, and the next change of that device sends it).
	maxPendingAnnouncements = 4096
	// audienceTimeout bounds one resolution of a channel's audience on the announcer's goroutine.
	audienceTimeout = 10 * time.Second
)

// announceKey is one device in one call: its voice_state coalesces to the latest flags.
type announceKey struct{ call, dev id.ID }

// announcement is one queued voice_state.
type announcement struct {
	ch        store.ChannelRow
	user      id.ID
	flags     uint64
	call, dev id.ID
}

// announce queues one voice_state for every device of every user who may view ch (MD-11): the
// resolver's view_channel holders of a community channel, a DM's participants. It never resolves
// the audience on the caller's goroutine (CALLS-5): a webhook's worker only records the device's
// latest flags, and the announcer — its own goroutine, alive only while something is queued —
// delivers them at most every announceInterval, resolving each channel's audience once per delivery
// whatever the number of events behind it. So a member who publishes and unpublishes in a loop costs
// the webhook worker a map write per event, and the store one audience resolution per interval. The
// gateway delivers to the users' live connections only, so an offline user receives nothing.
func (c *CallEvents) announce(ctx context.Context, ch store.ChannelRow, callID, user, dev id.ID, flags uint64) {
	if c.gw == nil {
		return
	}
	k := announceKey{call: callID, dev: dev}
	c.amu.Lock()
	if c.apending == nil {
		c.apending = map[announceKey]announcement{}
	}
	if _, ok := c.apending[k]; !ok && len(c.apending) >= maxPendingAnnouncements {
		c.amu.Unlock()
		c.log.WarnContext(ctx, "the voice_state queue is full; this announcement is dropped", "device_id", dev.String())
		return
	}
	c.apending[k] = announcement{ch: ch, user: user, flags: flags, call: callID, dev: dev}
	start := !c.arunning
	c.arunning = true
	c.amu.Unlock()
	if start {
		go c.drainAnnouncements()
	}
}

// drainAnnouncements is the announcer: it delivers what is queued, waits announceInterval, and
// returns once a delivery found nothing more queued.
func (c *CallEvents) drainAnnouncements() {
	for {
		c.amu.Lock()
		batch := c.apending
		c.apending = nil
		if len(batch) == 0 {
			c.arunning = false
			c.amu.Unlock()
			return
		}
		c.adelivering = true
		c.amu.Unlock()
		c.deliver(batch)
		c.amu.Lock()
		c.adelivering = false
		c.amu.Unlock()
		time.Sleep(announceInterval)
	}
}

// deliver sends one batch, resolving each channel's audience once.
func (c *CallEvents) deliver(batch map[announceKey]announcement) {
	byChannel := map[id.ID][]announcement{}
	for _, a := range batch {
		byChannel[a.ch.ID] = append(byChannel[a.ch.ID], a)
	}
	for chID, list := range byChannel {
		ctx, cancel := context.WithTimeout(context.Background(), audienceTimeout)
		audience, err := EligibleUsers(ctx, c.repo, list[0].ch)
		cancel()
		if err != nil {
			c.log.Error("resolving a voice channel's audience failed", "channel_id", chID.String(), "err", err)
			continue
		}
		for _, a := range list {
			payload, err := gateway.VoiceStatePayload(a.user, a.dev, a.call, a.flags)
			if err != nil {
				c.log.Error("encoding a voice_state failed", "err", err)
				continue
			}
			f := gateway.Frame{
				Op: gateway.OpVoiceState, Payload: payload,
				Scope: gateway.FrameScope{Channels: []id.ID{chID}},
			}
			for _, u := range audience {
				c.gw.DeliverUser(u, f)
			}
		}
	}
}

// announcementsIdle reports whether nothing is queued or being delivered (a test's flush).
func (c *CallEvents) announcementsIdle() bool {
	c.amu.Lock()
	defer c.amu.Unlock()
	return len(c.apending) == 0 && !c.adelivering
}
