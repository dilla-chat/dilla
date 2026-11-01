package service

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jonasthim/dilla/internal/bot/music"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

// View channel, send messages and read history (api.PermViewChannel|PermSendMessages|PermReadHistory), as a poll needs.
const musicAnnounceChannelSend = uint64(1<<0 | 1<<1 | 1<<6)

const (
	// musicAnnounceWindowMS: only a track that started at most this long ago is announced (security review L6).
	musicAnnounceWindowMS = 30_000
	// musicAnnounceRequesterGap: one announcement of a new track per requester per minute (L6).
	musicAnnounceRequesterGap = time.Minute
)

// MusicAnnounceLimiter remembers each requester's last announced track, so a retry of that track resumes its send while
// another track within a minute is refused. One per installation; the zero value is ready.
type MusicAnnounceLimiter struct {
	mu   sync.Mutex
	last map[id.ID]musicAnnounceRecord
}

type musicAnnounceRecord struct {
	item string
	at   time.Time
}

// reserve answers whether the requester may announce item now and, when it may, holds the requester's slot for item so a
// concurrent request for another track is refused meanwhile. done(sent) settles the slot: an accepted send keeps it; any
// other outcome (a refusal, an error, a send not accepted) restores what the limiter held before (Task S1, 8b leftover: a
// failed announcement must not spend the requester's minute).
func (l *MusicAnnounceLimiter) reserve(user id.ID, item string, now time.Time) (done func(sent bool), ok bool) {
	noop := func(bool) {}
	if l == nil {
		return noop, true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil {
		l.last = map[id.ID]musicAnnounceRecord{}
	}
	for key, record := range l.last {
		if now.Sub(record.at) > musicAnnounceRequesterGap {
			delete(l.last, key)
		}
	}
	prior, had := l.last[user]
	if had && prior.item != item && now.Sub(prior.at) < musicAnnounceRequesterGap {
		return noop, false
	}
	if had && prior.item == item {
		// A retry of the recorded track: it resumes its send and changes nothing.
		return noop, true
	}
	held := musicAnnounceRecord{item: item, at: now}
	l.last[user] = held
	return func(sent bool) {
		if sent {
			return
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		if current, ok := l.last[user]; !ok || current != held {
			return
		}
		if had {
			l.last[user] = prior
		} else {
			delete(l.last, user)
		}
	}, true
}

// NeutralizeBotMarkers keeps text a privileged member authors for the Bot to post (announcements, welcome messages,
// role panels) from carrying a player marker clients would draw a card from (security review, L-related).
func NeutralizeBotMarkers(text string) string {
	return strings.ReplaceAll(text, "[dilla-music:", "(dilla-music:")
}

// musicAnnounceChannelAuthority resolves the actor's effective bits in one channel, overrides included.
type musicAnnounceChannelAuthority interface {
	BotActorChannelAuthority(context.Context, id.ID, id.ID, id.ID) (uint64, bool, error)
}

// MusicAnnouncements publishes the public "started music" message (Signal README §6.6, Jonas 2026-10-28): when a
// requester's /play started playback, Glenn posts in the text channel where /play ran, through the installed Bot's MLS
// group and durable send receipt like a role panel. The body ends in the player marker `[dilla-music:v1:<voice>:<user>]`,
// from which clients draw the live now-playing card. Everything else about music stays private.
//
// The destination: /play runs in the requester's client and reaches the Bot through the requester's private interaction
// group, so the Bot never sees the text channel itself; the request names it. That name is never trusted on its own: the
// Bot posts only where the requester's effective permissions for that very channel (role overrides included, resolved by
// the server) allow viewing, sending and reading history (owners and administrators too: no shortcut), and only in a
// text channel of this community (ResolveTextGroup). A channel the requester cannot post in gets a refusal, no message.
//
// Only the member who requested the current track may announce it, only while they may DJ in that voice call. One message per (voice channel, current track): the operation id is derived from both, so a
// retry or a second request returns the first message's receipt instead of posting again.
type MusicAnnouncements struct {
	Notices     *MusicSkipNotices
	Music       *MusicInteractions
	Authority   rolePanelAuthority
	Channels    musicAnnounceChannelAuthority
	Limiter     *MusicAnnounceLimiter
	Destination rolePanelDestination
	Sender      rolePanelSender
	Discovery   rolePanelDiscovery
	Session     *client.Session
	CommunityID id.ID
	Now         func() time.Time
}

func (h MusicAnnouncements) Handlers() map[protocol.Operation]InteractionHandlerSpec {
	return map[protocol.Operation]InteractionHandlerSpec{
		protocol.OpMusicAnnounce: {AllowMusicChannelContext: true, RevisionInHandler: true, Handle: h.announce},
	}
}

// MusicAnnouncementText is the message body: a plain fallback line, then the marker clients render the card from. The
// queue item binds the card to this track: once another track plays, clients show the ended state (task review 2).
func MusicAnnouncementText(voice, requester id.ID, item string) string {
	return fmt.Sprintf("Music started.\n[dilla-music:v1:%s:%s:%s]", voice.String(), requester.String(), item)
}

// Invalid client input intentionally maps to an unavailable protocol result (a nil error).
func (h MusicAnnouncements) announce(ctx context.Context, actor InteractionActor, request protocol.Request) (InteractionResult, error) {
	if h.Music == nil || h.Music.Store == nil || h.Music.Authority == nil || h.Authority == nil || h.Channels == nil || h.Destination == nil || h.Sender == nil || h.Discovery == nil ||
		h.Session == nil || h.CommunityID.IsZero() || actor.User.IsZero() || actor.Kind != 0 || request.Operation != protocol.OpMusicAnnounce ||
		request.CommunityID != h.CommunityID || request.RequestID.IsZero() || request.ChannelID == nil || request.ChannelID.IsZero() ||
		request.Revision == nil || *request.Revision != 0 || request.Expires == nil || len(request.Args) != 1 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	voice := *request.ChannelID
	text, ok := request.Args[0].(id.ID)
	if !ok || text.IsZero() || text == voice {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	call, err := h.Music.Authority.CurrentCall(ctx, voice)
	if err != nil {
		return InteractionResult{}, err
	}
	if !validMusicCall(call, voice) {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	dj, err := h.Music.Authority.CanDJ(ctx, actor, call)
	if err != nil {
		return InteractionResult{}, err
	}
	if !dj {
		return InteractionResult{Status: interactionStatusForbidden}, nil
	}
	_, present, err := h.Authority.BotActorAuthority(ctx, h.CommunityID, actor.User)
	if err != nil {
		return InteractionResult{}, err
	}
	if !present {
		return InteractionResult{Status: interactionStatusForbidden}, nil
	}
	// Security review L4: always the server's channel resolver, owners and administrators included (it returns their
	// full bits for a channel they can see, and keeps a timed-out member out).
	bits, visible, err := h.Channels.BotActorChannelAuthority(ctx, h.CommunityID, actor.User, text)
	if err != nil {
		return InteractionResult{}, err
	}
	if !visible || bits&musicAnnounceChannelSend != musicAnnounceChannelSend {
		return InteractionResult{Status: interactionStatusForbidden}, nil
	}
	// The existing request also registers the command's text destination before playback.
	// Resolve the channel's MLS binding now, before remembering it or acknowledging registration.
	group, err := h.Destination.ResolveTextGroup(ctx, h.CommunityID, text)
	if err != nil || group.IsZero() {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	snapshot, err := h.Music.Store.Load(ctx, voice)
	if err != nil {
		return InteractionResult{}, err
	}
	state := snapshot.State
	if h.Music.CurrentState != nil {
		live, revision, err := h.Music.CurrentState(ctx, h.Music.InstallationID, h.CommunityID, voice)
		if err != nil {
			return InteractionResult{}, err
		}
		// Security review L5: a live state that is not the stored revision is a race, as in MusicInteractions.handle.
		if revision != snapshot.Revision || live.Revision != snapshot.Revision {
			current := snapshot.Revision
			return InteractionResult{Status: interactionStatusConflict, Revision: &current}, nil
		}
		state = live
	}
	clock := time.Now
	if h.Now != nil {
		clock = h.Now
	}
	// Idle/paused requests only register a destination. Do not sample discovery,
	// spend an announcement slot or post a started card on this path.
	if state.Current == nil || state.Paused {
		if h.Notices != nil {
			h.Notices.Register(voice, text, actor, call.GroupID)
			return InteractionResult{Data: []byte{0x80}}, nil
		}
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	if state.Current != nil && state.Current.RequesterID != actor.User.String() {
		return InteractionResult{Status: interactionStatusForbidden}, nil
	}
	// Security review L6: only a track that has just started (a stamped playhead within 30 s), and one new track per
	// requester per minute; a retry of the same track still resumes its first send.
	if state.PositionAtMS == 0 || music.PlayheadMS(state, clock().UnixMilli()) > musicAnnounceWindowMS {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	settle, allowed := h.Limiter.reserve(actor.User, state.Current.ID, clock())
	if !allowed {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	// The requester's slot is spent only by an accepted send; every other return below gives it back.
	sent := false
	defer func() { settle(sent) }()
	discovery, _, err := h.Discovery.DiscoverObserved(ctx)
	if err != nil {
		return InteractionResult{}, err
	}
	if discovery.Generation == 0 || discovery.InstanceID.IsZero() {
		return InteractionResult{}, client.ErrProtocol
	}
	created := clock().UTC().Unix()
	if created < 0 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	body, err := cborx.Marshal([]any{uint64(0), nil, MusicAnnouncementText(voice, actor.User, state.Current.ID), []any{}, nil})
	if err != nil {
		return InteractionResult{}, err
	}
	operation, attempt := musicAnnounceIDs(h.CommunityID, voice, state.Current.ID)
	receipt, err := h.Sender.Send(ctx, h.Session, SendInput{OperationID: operation, AttemptID: attempt, GroupID: group,
		RequestBody: body, OriginalNow: uint64(created), ExpectedGeneration: discovery.Generation})
	if err != nil {
		return InteractionResult{}, err
	}
	if receipt.OperationID != operation || receipt.AttemptID != attempt || receipt.CommunityID != h.CommunityID || receipt.GroupID != group {
		return InteractionResult{}, client.ErrProtocol
	}
	if receipt.Outcome != protocol.ReceiptAccepted || receipt.SendResponse == nil || receipt.SendResponse.Sequence == 0 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	// The message is in the channel: the requester's minute is spent, whatever happens to this answer.
	sent = true
	if h.Notices != nil {
		h.Notices.Register(voice, text, actor, call.GroupID)
	}
	data, err := cborx.Marshal([]any{text, receipt.SendResponse.Sequence})
	if err != nil {
		return InteractionResult{}, err
	}
	return InteractionResult{Data: data}, nil
}

// musicAnnounceIDs derives one send operation per (community, voice channel, current queue item).
func musicAnnounceIDs(community, voice id.ID, item string) (operation, attempt id.ID) {
	seed := append(append(append([]byte("dilla music announce v1\x00"), community[:]...), voice[:]...), item...)
	digest := sha256.Sum256(seed)
	copy(operation[:], digest[:len(operation)])
	next := sha256.Sum256(append([]byte("dilla music announce attempt v1\x00"), operation[:]...))
	copy(attempt[:], next[:len(attempt)])
	return operation, attempt
}
