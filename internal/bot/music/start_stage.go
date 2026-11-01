package music

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/id"
)

// StartError identifies the boundary that refused a start while retaining its cause.
type StartError struct {
	Stage string
	Cause error
}

func (e *StartError) Error() string { return e.Stage + ": " + e.Cause.Error() }
func (e *StartError) Unwrap() error { return e.Cause }
func (e *StartError) ProviderFailure() bool {
	if errors.Is(e.Cause, context.Canceled) || errors.Is(e.Cause, context.DeadlineExceeded) || errors.Is(e.Cause, ErrSpotifyBusy) {
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

// StartStage emits exactly one result for a boundary. It never logs error text.
func StartStage(ctx context.Context, stage string, err error) error {
	logger := PlaybackLogger(ctx)
	if err == nil {
		logger.InfoContext(ctx, "bot music playback start", "group_id", PlaybackGroup(ctx).String(), "stage", stage, "class", "ok")
		return nil
	}
	wrapped := &StartError{Stage: stage, Cause: err}
	logger.WarnContext(ctx, "bot music playback start failed", "group_id", PlaybackGroup(ctx).String(), "stage", stage, "class", FailureClass(wrapped))
	return wrapped
}

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
	if errors.Is(err, ErrSpotifyBusy) {
		return "provider_busy"
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
		case "spotify_lease":
			return "lease_unavailable"
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
