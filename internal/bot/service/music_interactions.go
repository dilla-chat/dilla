package service

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net/url"
	"strings"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/music"
	"github.com/jonasthim/dilla/internal/bot/music/persistence"
	"github.com/jonasthim/dilla/internal/bot/music/youtube"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
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

type SpotifyMusicTrackResolver interface {
	ResolveSpotify(context.Context, string, id.ID) ([]music.Track, error)
}

// MusicInteractions authorizes encrypted queue mutations and hands committed
// state or immediate controls to the retained playback supervisor.
type MusicInteractions struct {
	Logger          *slog.Logger
	Store           musicStateStore
	Authority       MusicInteractionAuthority
	Resolver        MusicTrackResolver
	SpotifyResolver SpotifyMusicTrackResolver
	InstallationID  id.ID
	CommunityID     id.ID
	DeviceID        id.ID
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
	reason := "dependency_or_context"
	var diagnostic error
	var group id.ID
	defer func() {
		if result.Status == 0 && failure == nil {
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
		logger.WarnContext(ctx, "bot music operation refused", "installation_id", h.InstallationID.String(), "device_id", h.DeviceID.String(), "group_id", group.String(), "op", op, "status", status, "reason", reason, "error", detail, "stderr", youtube.FailureStderr(diagnostic))
	}()
	if h.Store == nil || h.Authority == nil || h.CommunityID.IsZero() || req.CommunityID != h.CommunityID || req.ChannelID == nil || req.ChannelID.IsZero() {
		return InteractionResult{}, errors.New("missing music interaction dependency or context")
	}
	channel := *req.ChannelID
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
		reason = "state_read"
		snapshot, err = h.Store.Load(ctx, channel)
		if err != nil {
			return InteractionResult{}, err
		}
		if h.CurrentState != nil {
			reason = "runtime_state_read"
			state, revision, err := h.CurrentState(ctx, h.InstallationID, h.CommunityID, channel)
			if err != nil {
				return InteractionResult{}, err
			}
			if revision != snapshot.Revision || state.Revision != snapshot.Revision {
				// A playback transition may have committed since the first read. State reads
				// always have a durable answer, and a missed hand-off adopts without starting.
				snapshot, err = h.Store.Load(ctx, channel)
				if err != nil {
					return InteractionResult{}, err
				}
				if revision < snapshot.Revision && h.OnPersistedSnapshot != nil {
					_, _ = h.OnPersistedSnapshot(ctx, h.InstallationID, h.CommunityID, channel, snapshot.State, snapshot.Revision, false)
				}
			}
			if revision == snapshot.Revision && state.Revision == snapshot.Revision {
				snapshot.State = state
			}
			if op != protocol.OpMusicState && !immediate && (revision != snapshot.Revision || state.Revision != snapshot.Revision) {
				reason = "revision_conflict"
				currentRevision := snapshot.Revision
				return InteractionResult{Status: interactionStatusConflict, Revision: &currentRevision}, nil
			}
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
	reason = "revision_conflict"
	if !immediate && (req.Revision == nil || *req.Revision != snapshot.Revision) {
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
	if op == protocol.OpMusicEnqueue {
		reason = "invalid_args"
		if len(req.Args) != 1 {
			return InteractionResult{Status: interactionStatusUnavailable}, nil
		}
		input, ok := req.Args[0].(string)
		if !ok {
			return InteractionResult{Status: interactionStatusUnavailable}, nil
		}
		var tracks []music.Track
		var resolveErr error
		spotifyInput := isSpotifyCatalogInput(input)
		if spotifyInput {
			reason = "provider_unavailable"
			if h.SpotifyResolver == nil {
				return InteractionResult{Status: interactionStatusUnavailable}, nil
			}
			tracks, resolveErr = h.SpotifyResolver.ResolveSpotify(ctx, input, actor.User)
		} else {
			reason = "provider_unavailable"
			if h.Resolver == nil {
				return InteractionResult{Status: interactionStatusUnavailable}, nil
			}
			tracks, resolveErr = h.Resolver.Resolve(ctx, input)
		}
		reason, diagnostic = "resolve_failed", resolveErr
		if resolveErr != nil || len(tracks) == 0 || len(tracks) > youtube.MaxPlaylistItems || (spotifyInput && len(tracks) > 500) {
			return InteractionResult{Status: interactionStatusUnavailable}, nil
		}
		reason = "provider_mismatch"
		for i := range tracks {
			if spotifyInput && tracks[i].Provider != music.ProviderSpotify || !spotifyInput && tracks[i].Provider != music.ProviderYouTube {
				return InteractionResult{Status: interactionStatusUnavailable}, nil
			}
			tracks[i].ID = id.New().String()
			tracks[i].RequesterID = actor.User.String()
		}
		kind := music.OpEnqueue
		if len(tracks) > 1 {
			kind = music.OpImportPlaylist
		}
		opValue = music.Operation{Kind: kind, Tracks: tracks}
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
	if !directImmediate && !staleSkip {
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
	// Recheck the call binding and role immediately before CAS persistence.
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
		return InteractionResult{Status: interactionStatusConflict}, nil
	}
	if err != nil {
		return InteractionResult{}, err
	}
	appliedRevision := saved.Revision
	if h.OnPersistedSnapshot != nil {
		startPlayback := op == protocol.OpMusicResume || op == protocol.OpMusicSkip && !snapshot.State.Paused && next.Current != nil
		var handoffErr error
		appliedRevision, handoffErr = h.OnPersistedSnapshot(ctx, h.InstallationID, h.CommunityID, channel, saved.State, saved.Revision, startPlayback)
		reason, diagnostic = "snapshot_handoff", handoffErr
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

func (h MusicInteractions) clock() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// isSpotifyCatalogInput keeps Spotify catalog links on their authenticated
// resolver path. The catalog itself performs strict URI and URL validation;
// this dispatch only prevents malformed Spotify links reaching YouTube.
func isSpotifyCatalogInput(input string) bool {
	if strings.HasPrefix(input, "spotify:") {
		return true
	}
	u, err := url.Parse(input)
	if err != nil {
		return strings.Contains(strings.ToLower(input), "spotify.com/")
	}
	host := strings.ToLower(u.Hostname())
	return host == "open.spotify.com" || host == "www.spotify.com"
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
