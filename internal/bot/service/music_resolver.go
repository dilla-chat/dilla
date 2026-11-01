package service

import (
	"context"
	"log/slog"

	"github.com/jonasthim/dilla/internal/bot/music"
	"github.com/jonasthim/dilla/internal/bot/music/youtube"
	"github.com/jonasthim/dilla/internal/id"
)

// LoggingMusicTrackResolver records diagnostics without recording its input.
type LoggingMusicTrackResolver struct {
	Resolver       MusicTrackResolver
	Logger         *slog.Logger
	InstallationID id.ID
}

func (r LoggingMusicTrackResolver) Resolve(ctx context.Context, input string) ([]music.Track, error) {
	logger := r.Logger
	if logger == nil {
		logger = slog.Default()
	}
	ctx = youtube.WithDiagnosticLogger(ctx, logger.With("installation_id", r.InstallationID.String()))
	return r.Resolver.Resolve(ctx, input)
}

// Search preserves the resolver's search profile through the diagnostic wrapper.
// Enqueue always invokes Resolve and retains full track validation.
func (r LoggingMusicTrackResolver) Search(ctx context.Context, input string) ([]music.Track, error) {
	logger := r.Logger
	if logger == nil {
		logger = slog.Default()
	}
	ctx = youtube.WithDiagnosticLogger(ctx, logger.With("installation_id", r.InstallationID.String()))
	if search, ok := r.Resolver.(MusicSearchResolver); ok {
		return search.Search(ctx, input)
	}
	return r.Resolver.Resolve(ctx, input)
}
