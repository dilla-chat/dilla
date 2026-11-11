package service

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"log/slog"
	"math"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/music"
	"github.com/dilla-chat/dilla/internal/bot/music/persistence"
	"github.com/dilla-chat/dilla/internal/bot/music/youtube"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	botstrings "github.com/dilla-chat/dilla/internal/bot/strings"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

// MusicCallBinding is the live call identity for the requested voice channel.
// A missing/inactive call makes all operations unavailable.
type MusicCallBinding struct {
	CallID    id.ID
	GroupID   id.ID
	ChannelID id.ID
	Active    bool
}

// MusicInteractionAuthority must resolve these checks from current server
// state. CanDJ requires control_music in the call's voice channel (owners and
// administrators hold every bit), plus actor membership in the specified voice
// call and Bot voice permissions.
// CanEmergencyStop is reserved for owner/admin and does not require call presence.
type MusicInteractionAuthority interface {
	CurrentCall(context.Context, id.ID) (MusicCallBinding, error)
	CanUse(context.Context, InteractionActor, MusicCallBinding) (bool, error)
	CanDJ(context.Context, InteractionActor, MusicCallBinding) (bool, error)
	CanEmergencyStop(context.Context, InteractionActor, MusicCallBinding) (bool, error)
}

type musicStateStore interface {
	Load(context.Context, id.ID) (persistence.Snapshot, error)
	Save(context.Context, id.ID, uint64, music.State) (persistence.Snapshot, error)
}

type musicRuntimeStateReader func(context.Context, id.ID, id.ID, id.ID) (music.State, uint64, error)

// MusicSearchResolver permits bounded suggestions without weakening enqueue validation.
type MusicSearchResolver interface {
	Search(context.Context, string) ([]music.Track, error)
}

type MusicTrackResolver interface {
	Resolve(context.Context, string) ([]music.Track, error)
}

// MusicInteractions authorizes encrypted queue mutations and hands committed
// state or immediate controls to the retained playback supervisor.
type MusicInteractions struct {
	Logger         *slog.Logger
	Store          musicStateStore
	Authority      MusicInteractionAuthority
	Resolver       MusicTrackResolver
	InstallationID id.ID
	CommunityID    id.ID
	DeviceID       id.ID
	// CurrentState exposes the retained supervisor's live state. The handler
	// accepts it only when its revision still matches the durable store row.
	CurrentState musicRuntimeStateReader
	// OnImmediateControl serializes pause/skip with the supervisor, rebasing onto its current revision.
	OnImmediateControl func(context.Context, id.ID, id.ID, id.ID, music.Operation) (music.State, error)
	// Now makes the write-boundary expiry check deterministic in tests. Nil uses
	// the system clock.
	Now func() time.Time
	// OnPersistedSnapshot applies the exact state returned by a successful CAS
	// commit. startPlayback is true for an explicit Resume or for a Skip that
	// advances an already-playing queue.
	OnPersistedSnapshot func(context.Context, id.ID, id.ID, id.ID, music.State, uint64, bool) (uint64, error)
	// DefaultVolume is the installation's Music page default (Task 10c). A new session, a track
	// enqueued while nothing is playing or queued, starts at it. Nil keeps the queue's volume.
	DefaultVolume func() uint8
	// Hints tells the room's members (a call hint, scope the voice channel) that the queue or the playing track
	// changed, after a mutation committed (Bot v2 Task 17). Nil sends none.
	Hints HintSender
	// StateReads shares one music.state computation per voice channel (the call scope) over MusicStateReadWindow:
	// the call read and the state read; each member is still authorised and gets their own answer (Bot v2 Task 19
	// fix round 4, Task 21b ruling 3). Nil computes every read.
	StateReads *MusicStateReads
	// OnPlayOpened begins the voice room's call-group join and room connection for an authorised sealed /play,
	// before its track resolves (Bot v2 Task 19: the join runs beside the resolve). ctx carries the room's current call
	// group (music.PlaybackGroup). It must not block. Nil joins at the start, after the resolve.
	OnPlayOpened func(ctx context.Context, installation, community, channel id.ID)
	// OnPlay hands a committed sealed /play that starts playback to the supervisor and returns once the first frame is
	// published, so the /play answers "Now playing" when audio starts. Nil hands it to OnPersistedSnapshot.
	OnPlay func(ctx context.Context, installation, community, channel id.ID, state music.State, revision uint64) (uint64, error)
	// OnPlayAbandoned is a sealed /play that began a warm and ended without starting a track (refused, not resolved,
	// or only queued): the warmed call is dropped and the idle room left (Bot v2 Task 19 fix round 1, M3). Nil does
	// nothing.
	OnPlayAbandoned func(ctx context.Context, installation, community, channel id.ID)
}

type sealedPlayKey struct{}

// withSealedPlay marks ctx as a sealed /play (Bot v2 Task 19): the enqueue also plays when nothing plays, as the v1
// client's /play did with its enqueue and resume, and answers once audio starts.
func withSealedPlay(ctx context.Context) context.Context {
	return context.WithValue(ctx, sealedPlayKey{}, true)
}

func sealedPlay(ctx context.Context) bool { v, _ := ctx.Value(sealedPlayKey{}).(bool); return v }

// nowPlayingText is the established /nowplaying copy (web bot-handlers.ts): "Now playing: T — A."
func nowPlayingText(state music.State) string {
	if state.Current == nil {
		return ""
	}
	text := state.Current.Title
	if state.Current.Artist != "" {
		text += " — " + state.Current.Artist
	}
	return "Now playing: " + text + "."
}

func (h MusicInteractions) Handlers() map[protocol.Operation]InteractionHandlerSpec {
	ops := []protocol.Operation{
		protocol.OpMusicState, protocol.OpMusicSearch, protocol.OpMusicEnqueue,
		protocol.OpMusicPause, protocol.OpMusicResume, protocol.OpMusicSkip,
		protocol.OpMusicStop, protocol.OpMusicSeek, protocol.OpMusicVolume,
		protocol.OpMusicShuffle, protocol.OpMusicRepeat, protocol.OpMusicRemove,
		protocol.OpMusicMove, protocol.OpMusicClear,
	}
	result := make(map[protocol.Operation]InteractionHandlerSpec, len(ops))
	for _, op := range ops {
		operation := op
		spec := InteractionHandlerSpec{AllowMusicChannelContext: true, Handle: func(ctx context.Context, actor InteractionActor, req protocol.Request) (InteractionResult, error) {
			return h.handle(ctx, actor, req, operation)
		}}
		if operation != protocol.OpMusicState && operation != protocol.OpMusicSearch {
			spec.RevisionInHandler = true
		}
		result[operation] = spec
	}
	return result
}

func (h MusicInteractions) handle(ctx context.Context, actor InteractionActor, req protocol.Request, op protocol.Operation) (result InteractionResult, failure error) {
	immediate := op == protocol.OpMusicPause || op == protocol.OpMusicSkip
	play := op == protocol.OpMusicEnqueue && sealedPlay(ctx)
	// CurrentCall and the in-call check share one call read; the recheck before the commit reads the call again.
	ctx = withMusicCallMemo(ctx)
	playStarted := false
	if play {
		// A /play has a latency target (G3): its requests take the Bot's next request token, as pause and skip do.
		ctx = client.WithImmediateControl(ctx)
	}
	reason := "dependency_or_context"
	var diagnostic error
	var group id.ID
	// itemID is the queue item the operation acted on (v0.1.0-bot2.2): its opaque queue id (since v0.1.0-bot2.3
	// derived from the sender and the random request id, see musicItemID), never anything derived from the track.
	// queuedEarlier reports an item this command did not enqueue (it was already in the queue). It is logged as
	// queued_earlier, not restored: on the supervisor's start and ended lines restored means "loaded from storage when
	// the supervisor was made", which the handler cannot see.
	var itemID string
	var itemKey musicItemKey         // the enqueue's item-id key
	var ourItems map[string]struct{} // every id the enqueue could have committed
	var enqueued []music.Track
	queuedEarlier := func() bool {
		if itemID == "" {
			return false
		}
		for _, track := range enqueued {
			if track.ID == itemID {
				return false
			}
		}
		return true
	}
	defer func() {
		if result.Status == 0 && failure == nil {
			if op != protocol.OpMusicState && op != protocol.OpMusicSearch && req.ChannelID != nil {
				// A committed change ends the call's shared state window: the next state read sees it.
				h.StateReads.forget(*req.ChannelID)
				hint(ctx, h.Hints, client.StateScopeCall, *req.ChannelID)
			}
			return
		}
		logger := h.Logger
		if logger == nil {
			logger = slog.Default()
		}
		status := result.Status
		if failure != nil {
			status = interactionStatusInternal
		}
		if diagnostic == nil {
			diagnostic = failure
		}
		detail := ""
		if diagnostic != nil {
			detail = youtube.DiagnosticText(diagnostic.Error())
			var stage *music.StartError
			if errors.As(diagnostic, &stage) {
				detail = music.FailureClass(diagnostic)
			}
		}
		logger.WarnContext(ctx, "bot music operation refused", "installation_id", h.InstallationID.String(), "device_id", h.DeviceID.String(), "group_id", group.String(), "op", op, "item_id", itemID, "queued_earlier", queuedEarlier(), "status", status, "reason", reason, "error", detail, "stderr", youtube.FailureStderr(diagnostic))
	}()
	if h.Store == nil || h.Authority == nil || h.CommunityID.IsZero() || req.CommunityID != h.CommunityID || req.ChannelID == nil || req.ChannelID.IsZero() {
		return InteractionResult{}, errors.New("missing music interaction dependency or context")
	}
	channel := *req.ChannelID
	if op == protocol.OpMusicState && h.StateReads != nil {
		return h.coalescedState(ctx, actor, channel, &reason, &group)
	}
	reason = "current_call_lookup"
	call, err := h.Authority.CurrentCall(ctx, channel)
	group = call.GroupID
	if err != nil {
		return InteractionResult{}, err
	}
	reason = "no_current_call"
	if !validMusicCall(call, channel) {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	reason = "can_use_lookup"
	allowed, err := h.Authority.CanUse(ctx, actor, call)
	if err != nil {
		return InteractionResult{}, err
	}
	reason = "forbidden"
	if !allowed {
		return InteractionResult{Status: interactionStatusForbidden}, nil
	}
	if immediate {
		reason = "can_dj_lookup"
		allowed, err = h.Authority.CanDJ(ctx, actor, call)
		if err != nil {
			return InteractionResult{}, err
		}
		reason = "forbidden"
		if !allowed {
			return InteractionResult{Status: interactionStatusForbidden}, nil
		}
		ctx = client.WithImmediateControl(ctx)
	}
	if op == protocol.OpMusicSearch {
		reason = "provider_unavailable"
		if h.Resolver == nil {
			return InteractionResult{Status: interactionStatusUnavailable}, nil
		}
		reason = "invalid_args"
		if len(req.Args) != 1 {
			return InteractionResult{Status: interactionStatusUnavailable}, nil
		}
		query, ok := req.Args[0].(string)
		if !ok {
			return InteractionResult{Status: interactionStatusUnavailable}, nil
		}
		var tracks []music.Track
		var resolveErr error
		if search, ok := h.Resolver.(MusicSearchResolver); ok {
			tracks, resolveErr = search.Search(ctx, query)
		} else {
			tracks, resolveErr = h.Resolver.Resolve(ctx, query)
		}
		reason, diagnostic = "resolve_failed", resolveErr
		if resolveErr != nil || len(tracks) > youtube.MaxSearchResults {
			return InteractionResult{Status: interactionStatusUnavailable}, nil
		}
		reason = "search_encode"
		data, encodeErr := encodeMusicSearchResults(tracks)
		diagnostic = encodeErr
		if encodeErr != nil {
			return InteractionResult{Status: interactionStatusUnavailable}, nil
		}
		return InteractionResult{Status: 0, Data: data}, nil
	}
	// Immediate controls reconcile durable state once, under the supervisor's
	// serialization. Avoid handler/runtime reads before the same reconciliation.
	directImmediate := immediate && h.OnImmediateControl != nil
	var snapshot persistence.Snapshot
	if !directImmediate {
		var mismatch bool
		snapshot, mismatch, reason, err = h.readSnapshot(ctx, channel)
		if err != nil {
			return InteractionResult{}, err
		}
		if op != protocol.OpMusicState && op != protocol.OpMusicEnqueue && !immediate && mismatch {
			reason = "runtime_revision_mismatch"
			currentRevision := snapshot.Revision
			return InteractionResult{Status: interactionStatusConflict, Revision: &currentRevision}, nil
		}
	}
	if op == protocol.OpMusicState {
		reason = "can_dj_lookup"
		canControl, err := h.Authority.CanDJ(ctx, actor, call)
		if err != nil {
			return InteractionResult{}, err
		}
		reason = "state_encode"
		data, err := encodeMusicSnapshot(snapshot.State, canControl, h.clock().UnixMilli())
		if err != nil {
			return InteractionResult{}, err
		}
		revision := snapshot.Revision
		return InteractionResult{Status: 0, Revision: &revision, Data: data}, nil
	}
	reason = "client_revision_stale"
	if op != protocol.OpMusicEnqueue && !immediate && (req.Revision == nil || *req.Revision != snapshot.Revision) {
		revision := snapshot.Revision
		return InteractionResult{Status: interactionStatusConflict, Revision: &revision}, nil
	}
	if op == protocol.OpMusicStop {
		reason = "can_dj_lookup"
		allowed, err = h.Authority.CanDJ(ctx, actor, call)
		if err != nil {
			return InteractionResult{}, err
		}
		reason = "forbidden"
		if !allowed {
			reason = "can_emergency_stop_lookup"
			allowed, err = h.Authority.CanEmergencyStop(ctx, actor, call)
			if err != nil {
				return InteractionResult{}, err
			}
		}
	} else if !immediate {
		reason = "can_dj_lookup"
		allowed, err = h.Authority.CanDJ(ctx, actor, call)
	}
	if err != nil {
		return InteractionResult{}, err
	}
	reason = "forbidden"
	if !allowed {
		return InteractionResult{Status: interactionStatusForbidden}, nil
	}
	var opValue music.Operation
	// replayed is an enqueue whose request already committed its items (found before the resolve): it has no
	// tracks of its own to append.
	replayed := false
	if op == protocol.OpMusicEnqueue {
		reason = "invalid_args"
		if len(req.Args) != 1 {
			return InteractionResult{Status: interactionStatusUnavailable}, nil
		}
		input, ok := req.Args[0].(string)
		if !ok {
			return InteractionResult{Status: interactionStatusUnavailable}, nil
		}
		// The command's items take ids derived from its sender and request id (v0.1.0-bot2.3), so a replay of the same
		// request finds what it already enqueued. A request without an id (none reaches here from the wire) keeps
		// random ids.
		itemKey = musicItemKey{user: actor.User, device: actor.Device, request: req.RequestID}
		if itemKey.request.IsZero() {
			itemKey.request = id.New()
		}
		ourItems = musicItemIDs(itemKey)
		reason = "already_enqueued"
		if answer, matched, applied := enqueuedEarlier(snapshot, ourItems, play); applied {
			itemID = matched
			if !replayNeedsStart(snapshot.State, play) {
				// Answered before the resolve and the room warm: a replay adds nothing and starts nothing.
				return answer, nil
			}
			// A replayed /play whose player is paused completes the original /play's post-enqueue step (the
			// resume below) without resolving or appending again.
			replayed = true
		}
		var tracks []music.Track
		var resolveErr error
		reason = "provider_unavailable"
		if h.Resolver == nil && !replayed {
			return InteractionResult{Status: interactionStatusUnavailable}, nil
		}
		if play && h.OnPlayOpened != nil {
			// Authorised: join the room while the track resolves.
			h.OnPlayOpened(music.WithPlaybackGroup(ctx, call.GroupID), h.InstallationID, h.CommunityID, channel)
			if h.OnPlayAbandoned != nil {
				defer func() {
					if !playStarted {
						h.OnPlayAbandoned(context.WithoutCancel(ctx), h.InstallationID, h.CommunityID, channel)
					}
				}()
			}
		}
		if !replayed {
			tracks, resolveErr = h.Resolver.Resolve(ctx, input)
			reason, diagnostic = "resolve_failed", resolveErr
			if resolveErr != nil || len(tracks) == 0 || len(tracks) > youtube.MaxPlaylistItems {
				return InteractionResult{Status: interactionStatusUnavailable}, nil
			}
			reason = "provider_mismatch"
			for i := range tracks {
				if tracks[i].Provider != music.ProviderYouTube {
					return InteractionResult{Status: interactionStatusUnavailable}, nil
				}
				tracks[i].ID = musicItemID(itemKey, i)
				tracks[i].RequesterID = actor.User.String()
			}
			enqueued, itemID = tracks, tracks[0].ID
			kind := music.OpEnqueue
			if len(tracks) > 1 {
				kind = music.OpImportPlaylist
			}
			opValue = music.Operation{Kind: kind, Tracks: tracks}
		}
	} else {
		var ok bool
		reason = "invalid_operation"
		opValue, ok = musicOperation(op, req.Args)
		if !ok {
			return InteractionResult{Status: interactionStatusUnavailable}, nil
		}
	}
	if op == protocol.OpMusicShuffle {
		opValue.Enabled = !snapshot.State.Shuffle
	}
	if op != protocol.OpMusicEnqueue && snapshot.State.Current != nil {
		itemID = snapshot.State.Current.ID
	}

	// Enqueue resolves once, then rebases each bounded CAS attempt onto current state.
	// Other mutations retain their single revision-checked attempt.
	for attempt := 0; ; attempt++ {
		// alreadyApplied is a replayed /play whose items are committed and whose player is paused: this attempt
		// appends nothing and only runs the /play's resume.
		alreadyApplied := false
		if op == protocol.OpMusicEnqueue {
			reason = "state_read"
			snapshot, err = h.Store.Load(ctx, channel)
			if err != nil {
				return InteractionResult{}, err
			}
			if h.CurrentState != nil {
				reason = "runtime_state_read"
				state, revision, readErr := h.CurrentState(ctx, h.InstallationID, h.CommunityID, channel)
				if readErr != nil {
					return InteractionResult{}, readErr
				}
				if revision == snapshot.Revision && state.Revision == snapshot.Revision {
					snapshot.State = state
				}
			}
			// A concurrent run of this same request (a replay) may have committed since the last read; the re-apply
			// keeps the derived ids, so it is seen here rather than queued a second time.
			reason = "already_enqueued"
			answer, matched, applied := enqueuedEarlier(snapshot, ourItems, play)
			if applied && !replayNeedsStart(snapshot.State, play) {
				// The warm stays when the room plays, whichever track it plays.
				itemID, playStarted = matched, roomPlaying(snapshot.State)
				return answer, nil
			}
			if !applied && replayed {
				// The item this replay found has left the queue since (played, removed or cleared): it has no tracks to
				// append, so it ends as a conflict rather than guessing.
				reason = "replayed_item_gone"
				return InteractionResult{Status: interactionStatusConflict}, nil
			}
			alreadyApplied = applied
		}
		opValue.NowMS = h.clock().UnixMilli()
		base := snapshot.State
		if (op == protocol.OpMusicEnqueue) && h.DefaultVolume != nil && base.Current == nil && len(base.Queue) == 0 {
			// A new session: nothing plays and nothing waits, so the default volume applies.
			if v := h.DefaultVolume(); v >= 1 && v <= 100 {
				base.Volume = v
			}
		}
		staleSkip := false
		if !directImmediate && opValue.Kind == music.OpSkip && opValue.ExpectedCurrentItemID != nil {
			currentID := ""
			if snapshot.State.Current != nil {
				currentID = snapshot.State.Current.ID
			}
			staleSkip = currentID != *opValue.ExpectedCurrentItemID
		}
		var next music.State
		if alreadyApplied {
			// The enqueue's revision step without its append: the resume below commits as revision+1, as it does after
			// a fresh append.
			if snapshot.Revision == math.MaxUint64 {
				reason = "apply_failed"
				return InteractionResult{Status: interactionStatusUnavailable}, nil
			}
			next = base
			next.Revision = snapshot.Revision + 1
		} else if !directImmediate && !staleSkip {
			reason = "apply_failed"
			next, err = music.Apply(base, snapshot.Revision, opValue)
			diagnostic = err
			if errors.Is(err, music.ErrRevisionConflict) {
				return InteractionResult{Status: interactionStatusConflict}, nil
			}
			if errors.Is(err, music.ErrInvalidOperation) || errors.Is(err, music.ErrInvalidTrack) || errors.Is(err, music.ErrQueueFull) {
				return InteractionResult{Status: interactionStatusUnavailable}, nil
			}
			if err != nil {
				return InteractionResult{}, err
			}
		}
		startsPlay := false
		if play && next.Paused {
			// Nothing plays (or the player is paused): the /play starts it, in the same committed revision as its enqueue.
			reason = "apply_failed"
			resumed, resumeErr := music.Apply(next, next.Revision, music.Operation{Kind: music.OpResume, NowMS: opValue.NowMS})
			diagnostic = resumeErr
			if resumeErr != nil {
				return InteractionResult{}, resumeErr
			}
			resumed.Revision = next.Revision
			next, startsPlay = resumed, !resumed.Paused && resumed.Current != nil
		}
		// Recheck the call binding and role immediately before CAS persistence: the permissions in memory (feed-state
		// authority), the call read again from dillad after the resolve and on every retry (fix round 2), so a call that
		// changed or an actor who left the call is seen.
		refreshMusicCallMemo(ctx)
		reason = "current_call_lookup"
		current, err := h.Authority.CurrentCall(ctx, channel)
		if err != nil {
			return InteractionResult{}, err
		}
		reason = "call_changed"
		if current != call {
			return InteractionResult{Status: interactionStatusConflict}, nil
		}
		if op == protocol.OpMusicStop {
			reason = "can_dj_lookup"
			allowed, err = h.Authority.CanDJ(ctx, actor, current)
			if err != nil {
				return InteractionResult{}, err
			}
			reason = "forbidden"
			if !allowed {
				reason = "can_emergency_stop_lookup"
				allowed, err = h.Authority.CanEmergencyStop(ctx, actor, current)
				if err != nil {
					return InteractionResult{}, err
				}
			}
		} else {
			reason = "can_dj_lookup"
			allowed, err = h.Authority.CanDJ(ctx, actor, current)
		}
		if err != nil {
			return InteractionResult{}, err
		}
		reason = "forbidden"
		if !allowed {
			return InteractionResult{Status: interactionStatusForbidden}, nil
		}
		reason = "expired"
		if req.Expires != nil {
			now := time.Now
			if h.Now != nil {
				now = h.Now
			}
			currentTime := now().Unix()
			if currentTime >= 0 && *req.Expires <= uint64(currentTime) {
				return InteractionResult{Status: interactionStatusExpired}, nil
			}
		}
		if directImmediate || staleSkip {
			reason = "immediate_control"
			state := snapshot.State
			if directImmediate {
				var controlErr error
				state, controlErr = h.OnImmediateControl(ctx, h.InstallationID, h.CommunityID, channel, opValue)
				if controlErr != nil {
					return InteractionResult{}, controlErr
				}
			}
			// A stale fallback skip returns the unchanged snapshot only after the
			// same final authority and expiry checks as a queue mutation.
			// A bounded authenticated snapshot lets the client settle the control in
			// one Bot operation, including queues whose visible prefix is truncated.
			data, encodeErr := encodeMusicSnapshot(state, true, h.clock().UnixMilli())
			if encodeErr != nil {
				return InteractionResult{}, encodeErr
			}
			revision := state.Revision
			return InteractionResult{Status: 0, Revision: &revision, Data: data}, nil
		}
		reason = "save_failed"
		saved, err := h.Store.Save(ctx, channel, snapshot.Revision, next)
		diagnostic = err
		if errors.Is(err, persistence.ErrConflict) || errors.Is(err, music.ErrRevisionConflict) {
			if op == protocol.OpMusicEnqueue && attempt < 2 {
				continue
			}
			return InteractionResult{Status: interactionStatusConflict}, nil
		}
		if err != nil {
			return InteractionResult{}, err
		}
		appliedRevision := saved.Revision
		if startsPlay && saved.State.Current != nil {
			itemID = saved.State.Current.ID
		}
		if startsPlay && h.OnPlay != nil {
			var handoffErr error
			appliedRevision, handoffErr = h.OnPlay(ctx, h.InstallationID, h.CommunityID, channel, saved.State, saved.Revision)
			reason, diagnostic = "snapshot_handoff", handoffErr
			if answer, removed := trackRemovedResult(handoffErr, appliedRevision, saved.Revision); removed {
				// The track failed and left the queue (v0.1.0-bot2.2): the answer says so, and nothing is paused back.
				playStarted = answer.Status == 0
				return answer, nil
			}
			if handoffErr != nil {
				appliedRevision = h.pauseUnstartedPlayback(ctx, channel, saved.Revision, appliedRevision)
				if appliedRevision == 0 {
					appliedRevision = saved.Revision
				}
				return InteractionResult{Status: interactionStatusUnavailable, Revision: &appliedRevision}, nil
			}
			playStarted = true
			revision := appliedRevision
			return InteractionResult{Status: 0, Revision: &revision, Data: []byte{0x80}, Note: nowPlayingText(saved.State)}, nil
		}
		if h.OnPersistedSnapshot != nil {
			startPlayback := op == protocol.OpMusicResume || op == protocol.OpMusicSkip && !snapshot.State.Paused && next.Current != nil || startsPlay
			var handoffErr error
			appliedRevision, handoffErr = h.OnPersistedSnapshot(ctx, h.InstallationID, h.CommunityID, channel, saved.State, saved.Revision, startPlayback)
			reason, diagnostic = "snapshot_handoff", handoffErr
			if answer, removed := trackRemovedResult(handoffErr, appliedRevision, saved.Revision); removed {
				return answer, nil
			}
			if handoffErr != nil {
				if startPlayback {
					appliedRevision = h.pauseUnstartedPlayback(ctx, channel, saved.Revision, appliedRevision)
				}
				if appliedRevision == 0 {
					appliedRevision = saved.Revision
				}
				return InteractionResult{Status: interactionStatusUnavailable, Revision: &appliedRevision}, nil
			}
		}
		revision := appliedRevision
		return InteractionResult{Status: 0, Revision: &revision, Data: []byte{0x80}}, nil
	}
}

// musicItemDomain separates the enqueue's item ids from every other hash of a request id.
const musicItemDomain = "dilla/music-item/v1"

// musicItemKey is what an enqueue's item ids derive from: the sealed command's sender and its request id.
type musicItemKey struct{ user, device, request id.ID }

// musicItemID is the queue id of the index-th track a command enqueues (v0.1.0-bot2.3): the first 16 bytes of
// SHA-256(musicItemDomain ‖ sender user id ‖ sender device id ‖ request id ‖ index as 4-byte big-endian), as 32
// lowercase hex characters. The ids are fixed-length binary, so the concatenation is unambiguous. The request id is
// random, so the id stays opaque to any observer; it never takes the URL, video id, title or query. The same request
// from the same sender always derives the same ids, which makes a replayed enqueue findable in the queue.
func musicItemID(key musicItemKey, index int) string {
	sum := sha256.New()
	sum.Write([]byte(musicItemDomain))
	sum.Write(key.user[:])
	sum.Write(key.device[:])
	sum.Write(key.request[:])
	var be [4]byte
	binary.BigEndian.PutUint32(be[:], uint32(index)) //nolint:gosec // index < youtube.MaxPlaylistItems
	sum.Write(be[:])
	var item id.ID
	copy(item[:], sum.Sum(nil))
	return item.String()
}

// musicItemIDs is every id the key's command could have enqueued: one per playlist index.
func musicItemIDs(key musicItemKey) map[string]struct{} {
	ids := make(map[string]struct{}, youtube.MaxPlaylistItems)
	for i := 0; i < youtube.MaxPlaylistItems; i++ {
		ids[musicItemID(key, i)] = struct{}{}
	}
	return ids
}

// enqueuedEarlier reports an enqueue already applied: any of its items is the current track or queued (item 0 of a
// playlist may have played out). The answer adds nothing. It carries the current revision, and the now-playing note
// when a /play finds one of its tracks playing. item is the matched queue id, for the log line.
func enqueuedEarlier(snapshot persistence.Snapshot, ours map[string]struct{}, play bool) (answer InteractionResult, item string, applied bool) {
	state := snapshot.State
	revision := snapshot.Revision
	answer = InteractionResult{Status: 0, Revision: &revision, Data: []byte{0x80}}
	if state.Current != nil {
		if _, found := ours[state.Current.ID]; found {
			if play && !state.Paused {
				answer.Note = nowPlayingText(state)
			}
			return answer, state.Current.ID, true
		}
	}
	for _, track := range state.Queue {
		if _, found := ours[track.ID]; found {
			return answer, track.ID, true
		}
	}
	return InteractionResult{}, "", false
}

// roomPlaying reports a state whose current track plays: a /play that ends there leaves the room warm.
func roomPlaying(state music.State) bool {
	return !state.Paused && state.Current != nil
}

// replayNeedsStart reports a replayed /play that must still run the original /play's post-enqueue step: the player
// is paused (the start was lost, or the user paused since), and a fresh /play resumes it after its append.
func replayNeedsStart(state music.State, play bool) bool {
	return play && state.Paused
}

// trackRemovedResult answers a start whose track failed and was removed from the queue and from persistence
// (music.TrackRemovedError): unavailable with the removed note, or, when the next queued track started in its place,
// success naming it. removed is false for any other result.
func trackRemovedResult(err error, applied, committed uint64) (result InteractionResult, removed bool) {
	var trackRemoved *music.TrackRemovedError
	if !errors.As(err, &trackRemoved) {
		return InteractionResult{}, false
	}
	revision := applied
	if revision == 0 {
		revision = committed
	}
	note := botstrings.MusicTrackRemoved()
	if trackRemoved.Playing != nil {
		return InteractionResult{Status: 0, Revision: &revision, Data: []byte{0x80}, Note: note + " " + nowPlayingText(music.State{Current: trackRemoved.Playing})}, true
	}
	return InteractionResult{Status: interactionStatusUnavailable, Revision: &revision, Note: note}, true
}

func (h MusicInteractions) clock() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h MusicInteractions) pauseUnstartedPlayback(ctx context.Context, channel id.ID, committedRevision, observedRevision uint64) uint64 {
	pausedCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	current, err := h.Store.Load(pausedCtx, channel)
	if err != nil {
		return observedRevision
	}
	if current.Revision != committedRevision {
		return current.Revision
	}
	next, err := music.Apply(current.State, current.Revision, music.Operation{Kind: music.OpPause, NowMS: h.clock().UnixMilli()})
	if err != nil {
		return current.Revision
	}
	saved, err := h.Store.Save(pausedCtx, channel, current.Revision, next)
	if err != nil {
		latest, loadErr := h.Store.Load(pausedCtx, channel)
		if loadErr == nil {
			return latest.Revision
		}
		return current.Revision
	}
	return saved.Revision
}

func validMusicCall(call MusicCallBinding, channel id.ID) bool {
	return call.Active && !call.CallID.IsZero() && call.ChannelID == channel
}

func musicOperation(op protocol.Operation, args []any) (music.Operation, bool) {
	operation := music.Operation{}
	uintArg := func(i int) (uint64, bool) {
		if i >= len(args) {
			return 0, false
		}
		v, ok := args[i].(uint64)
		return v, ok
	}
	item := func(i int) (string, bool) {
		if i >= len(args) {
			return "", false
		}
		v, ok := args[i].(id.ID)
		return v.String(), ok && !v.IsZero()
	}
	switch op {
	case protocol.OpMusicPause:
		operation.Kind = music.OpPause
	case protocol.OpMusicResume:
		operation.Kind = music.OpResume
	case protocol.OpMusicSkip:
		operation.Kind = music.OpSkip
		if len(args) > 1 {
			return operation, false
		}
		if len(args) == 1 {
			expected, ok := args[0].(string)
			if !ok {
				return operation, false
			}
			if expected != "" {
				item, err := id.Parse(expected)
				if err != nil || item.IsZero() {
					return operation, false
				}
			}
			operation.ExpectedCurrentItemID = &expected
		}
	case protocol.OpMusicStop:
		operation.Kind = music.OpStop
	case protocol.OpMusicClear:
		operation.Kind = music.OpClear
	case protocol.OpMusicSeek:
		v, ok := uintArg(0)
		if !ok || v > math.MaxInt64 {
			return operation, false
		}
		operation.Kind, operation.PositionMS = music.OpSeek, int64(v)
	case protocol.OpMusicVolume:
		v, ok := uintArg(0)
		if !ok || v > 100 {
			return operation, false
		}
		operation.Kind, operation.Volume = music.OpSetVolume, uint8(v)
	case protocol.OpMusicShuffle:
		operation.Kind, operation.Enabled = music.OpSetShuffle, true
	case protocol.OpMusicRepeat:
		v, ok := uintArg(0)
		if !ok || v > uint64(music.RepeatQueue) {
			return operation, false
		}
		operation.Kind, operation.Repeat = music.OpSetRepeat, music.RepeatMode(v)
	case protocol.OpMusicRemove:
		v, ok := item(0)
		if !ok {
			return operation, false
		}
		operation.Kind, operation.ItemID = music.OpRemove, v
	case protocol.OpMusicMove:
		v, ok := item(0)
		if !ok || len(args) < 2 {
			return operation, false
		}
		operation.Kind, operation.ItemID = music.OpReorder, v
		if args[1] != nil {
			before, ok := item(1)
			if !ok {
				return operation, false
			}
			operation.BeforeID = before
		}
	default:
		return operation, false
	}
	return operation, true
}

// encodeMusicSnapshot writes snapshot v1. Slots 10 and 11 (Ruling AI) are the Bot's Unix ms at which slot 4's position
// was the playhead (0: unknown) and the Bot's Unix ms now, so a client can run the playhead on without trusting its clock.
// Oversized snapshots expose the largest queue prefix that fits and append slot 12 with
// the original queue count. This explicitly marks truncation without changing durable state.
func encodeMusicSnapshot(state music.State, canControl bool, nowMS int64) ([]byte, error) {
	if state.PositionMS < 0 || state.PositionAtMS < 0 || nowMS < 0 {
		return nil, music.ErrInvalidOperation
	}
	queue := make([]any, 0, len(state.Queue))
	for i := range state.Queue {
		t := state.Queue[i]
		row, err := encodeMusicTrack(&t)
		if err != nil {
			return nil, err
		}
		queue = append(queue, row)
	}
	current, err := encodeMusicTrack(state.Current)
	if err != nil {
		return nil, err
	}
	// Slot 9 is the current actor's DJ/call-membership affordance. Every
	// mutation still rechecks authority at its own write boundary.
	fields := []any{uint64(1), state.Revision, current, queue, uint64(state.PositionMS), boolUint(state.Paused), uint64(state.Volume), boolUint(state.Shuffle), uint64(state.Repeat), boolUint(canControl),
		uint64(state.PositionAtMS), uint64(nowMS)}
	data, err := cborx.Marshal(fields)
	if err != nil || len(data) <= 2400 {
		return data, err
	}
	fields = append(fields, uint64(len(queue)))
	fields[3] = queue[:0]
	best, err := cborx.Marshal(fields)
	if err != nil {
		return nil, err
	}
	if len(best) > 2400 {
		return nil, protocol.ErrLimit
	}
	// Encoded size grows monotonically with the number of complete queue rows.
	low, high := 1, len(queue)
	for low <= high {
		mid := low + (high-low)/2
		fields[3] = queue[:mid]
		candidate, err := cborx.Marshal(fields)
		if err != nil {
			return nil, err
		}
		if len(candidate) <= 2400 {
			best = candidate
			low = mid + 1
		} else {
			high = mid - 1
		}
	}
	return best, nil
}

func encodeMusicTrack(t *music.Track) (any, error) {
	if t == nil {
		return nil, nil
	}
	if t.DurationMS < 0 {
		return nil, music.ErrInvalidTrack
	}
	return []any{t.ID, string(t.Provider), t.CanonicalID, t.Title, t.Artist, t.Album, uint64(t.DurationMS), t.RequesterID}, nil
}

func encodeMusicSearchResults(tracks []music.Track) ([]byte, error) {
	rows := make([]any, 0, len(tracks))
	for i := range tracks {
		track := tracks[i]
		if track.Provider != music.ProviderYouTube || track.ID == "" || track.ID != "yt:"+track.CanonicalID {
			return nil, errors.New("music search returned an unsupported provider")
		}
		// Run the queue's full descriptor validation on a disposable copy while
		// leaving search results free of a requester or queue-item identity.
		validated := track
		validated.RequesterID = "search"
		if _, err := music.Apply(music.InitialState(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{validated}}); err != nil {
			return nil, err
		}
		row, err := encodeMusicTrack(&track)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	for len(rows) > 0 {
		data, err := cborx.Marshal([]any{uint64(1), rows})
		if err != nil {
			return nil, err
		}
		if len(data) <= 2400 {
			return data, nil
		}
		rows = rows[:len(rows)-1]
	}
	data, err := cborx.Marshal([]any{uint64(1), []any{}})
	if err != nil {
		return nil, err
	}
	if len(tracks) > 0 {
		return nil, errors.New("music search result exceeds response limit")
	}
	return data, nil
}
