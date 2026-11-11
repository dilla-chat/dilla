package music

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/id"
)

// StartError identifies the boundary that refused a start while retaining its cause.
type StartError struct {
	Stage string
	Cause error
}

func (e *StartError) Error() string { return e.Stage + ": " + e.Cause.Error() }
func (e *StartError) Unwrap() error { return e.Cause }
func (e *StartError) ProviderFailure() bool {
	if errors.Is(e.Cause, context.Canceled) || errors.Is(e.Cause, context.DeadlineExceeded) {
		return false
	}
	var cause interface{ PermanentPlaybackFailure() bool }
	return errors.As(e.Cause, &cause) && cause.PermanentPlaybackFailure()
}

type playbackLoggerKey struct{}

func WithPlaybackLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, playbackLoggerKey{}, logger)
}
func PlaybackLogger(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(playbackLoggerKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

// StartStage emits exactly one result for a boundary. It never logs error text. Each line names its queue item
// (PlaybackItemAttrs), read from ctx because the starter replaces ctx's logger.
func StartStage(ctx context.Context, stage string, err error) error {
	logger := PlaybackLogger(ctx)
	if attrs := PlaybackItemAttrs(ctx); attrs != nil {
		logger = logger.With(attrs...)
	}
	if stage == "published" {
		if branch := callJoinBranch(ctx); branch != "" {
			logger = logger.With("branch", branch)
		}
	}
	if err == nil {
		logger.InfoContext(ctx, "bot music playback start", "group_id", PlaybackGroup(ctx).String(), "stage", stage, "class", "ok")
		return nil
	}
	wrapped := &StartError{Stage: stage, Cause: err}
	if reason := EpochReason(err); reason != "" {
		logger = logger.With("epoch_reason", reason)
	}
	logger.WarnContext(ctx, "bot music playback start failed", "group_id", PlaybackGroup(ctx).String(), "stage", stage, "class", FailureClass(wrapped))
	return wrapped
}

// EpochReason is why a call's media epoch could not be used, from the fixed set leaf_missing (the Bot's own leaf is
// not in the epoch's roster), epoch_stale (the epoch went back or changed under the Bot) and group_unknown (the helper
// could not read the group's epoch), or "" when the error names none of them.
func EpochReason(err error) string {
	var reasoned interface{ EpochReason() string }
	if errors.As(err, &reasoned) {
		switch reason := reasoned.EpochReason(); reason {
		case "leaf_missing", "epoch_stale", "group_unknown":
			return reason
		}
	}
	return ""
}

type playbackItemKey struct{}

type playbackItem struct {
	id       string
	restored bool
}

// WithPlaybackItem tags ctx with the queue item a start or a playback belongs to: its opaque queue id (derived at
// enqueue from the random request id and the item index, never from the URL, video id, title or query) and whether it was loaded from storage when the
// supervisor was made (logged as restored). The handler's refused line logs queued_earlier instead: it cannot see
// what was loaded from storage, only whether the command itself enqueued the item.
func WithPlaybackItem(ctx context.Context, itemID string, restored bool) context.Context {
	return context.WithValue(ctx, playbackItemKey{}, playbackItem{id: itemID, restored: restored})
}

// PlaybackItem is ctx's queue item, as WithPlaybackItem set it.
func PlaybackItem(ctx context.Context) (itemID string, restored, ok bool) {
	item, ok := ctx.Value(playbackItemKey{}).(playbackItem)
	return item.id, item.restored, ok
}

// PlaybackItemAttrs are ctx's queue item as log attributes (item_id, restored), or nil when ctx names none.
func PlaybackItemAttrs(ctx context.Context) []any {
	item, restored, ok := PlaybackItem(ctx)
	if !ok {
		return nil
	}
	return []any{"item_id", item, "restored", restored}
}

type forcedRejoinKey struct{}

// WithForcedRejoin marks a start's retry after the Bot's own leaf was missing from the call's epoch: its call-group
// join must not trust the helper's membership (it joins again and catches up), and its publication catches up although
// the join was just made.
func WithForcedRejoin(ctx context.Context) context.Context {
	return context.WithValue(ctx, forcedRejoinKey{}, true)
}

// ForcedRejoin reports whether ctx is such a retry.
func ForcedRejoin(ctx context.Context) bool {
	forced, _ := ctx.Value(forcedRejoinKey{}).(bool)
	return forced
}

// ErrNeedsExplicitPlay is an automatic start that waits for a member's play (the room was left): the track did not
// fail, so it stays in the queue.
var ErrNeedsExplicitPlay = errors.New("music: only a member's play joins the call")

// ErrPlayerUnavailable is the Bot's own player failing (its provider sandbox cannot run) or the provider refusing
// every track for a while (rate limits, bot checks, its network), not the track: the track stays in the queue, paused.
var ErrPlayerUnavailable = errors.New("music: the Bot's player is unavailable")

// ErrConnectionEnded is a voice connection that ended under a track without the track failing: the server ended the
// call, the room was left idle, its transport died or its connection lifetime expired. The track stays, paused. The
// livekit errors match it (livekit imports music, so music cannot name them).
var ErrConnectionEnded = errors.New("music: the voice connection ended")

// ErrNotAdmitted is a track the Bot was not allowed to publish at that moment, not a track that broke: the installation
// is not ready with music (including a slow dillad read at admission), the Bot may not connect and speak in the
// channel, a user in the call is held by membership trust, or that trust is not available yet. The track stays in the
// queue, paused. The Bot's admission errors match it (cmd/dillabot; music cannot name them).
var ErrNotAdmitted = errors.New("music: the Bot was not admitted to publish in the call")

// ErrCallCheckFailed is a playing track whose call group could not be followed (failed catch-up checks): its media might
// not be under the call's current epoch, so it stopped. That is the track's own failure, whatever dillad error lies
// under it (controller ruling R7d), unless the Bot was not admitted (ErrNotAdmitted). The livekit catch-up errors match
// it (livekit imports music, so music cannot name them).
var ErrCallCheckFailed = errors.New("music: the call group could not be followed")

// FailureClass returns bounded classes only, never provider messages or URLs.
func FailureClass(err error) string {
	if err == nil {
		return "ok"
	}
	if errors.Is(err, context.Canceled) {
		return "ctx_canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline"
	}
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		return fmt.Sprintf("http_%d", apiErr.Status)
	}
	var classified interface{ FailureClass() string }
	if errors.As(err, &classified) {
		return classified.FailureClass()
	}
	var stage *StartError
	if errors.As(err, &stage) {
		switch stage.Stage {
		case "provider_check":
			return "invalid"
		case "provider_pcm":
			return "pcm_start"
		case "encoder":
			return "encoder"
		case "call_group_join":
			return "group_join"
		case "call_token":
			return "call_token"
		case "call_fields":
			return "binding"
		case "media_epoch":
			return "media_epoch"
		case "counter_reserve":
			return "counter_reserve"
		case "room_connect":
			return "room_connection"
		case "track_publish":
			return "track_publish"
		}
	}
	return "invalid"
}

type playbackGroupKey struct{}

func WithPlaybackGroup(ctx context.Context, group id.ID) context.Context {
	return context.WithValue(ctx, playbackGroupKey{}, group)
}
func PlaybackGroup(ctx context.Context) id.ID {
	group, _ := ctx.Value(playbackGroupKey{}).(id.ID)
	return group
}

type callJoinTraceKey struct{}

func WithCallJoinTrace(ctx context.Context) context.Context {
	return context.WithValue(ctx, callJoinTraceKey{}, new(atomic.Pointer[string]))
}
func RecordCallJoinBranch(ctx context.Context, branch string) {
	switch branch {
	case "already_member", "external_join", "resumed_intent", "forgot_stale_leaf":
	default:
		return
	}
	if trace, ok := ctx.Value(callJoinTraceKey{}).(*atomic.Pointer[string]); ok {
		trace.Store(&branch)
	}
}
func callJoinBranch(ctx context.Context) string {
	if trace, ok := ctx.Value(callJoinTraceKey{}).(*atomic.Pointer[string]); ok {
		if branch := trace.Load(); branch != nil {
			return *branch
		}
	}
	return ""
}
