package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/dilla-chat/dilla/internal/store"
)

const (
	botStructuralEventRetention     = 30 * 24 * time.Hour
	botStructuralEventPruneInterval = 24 * time.Hour
)

// runBotStructuralEventPruner prunes once at startup and then on the supplied
// interval. The caller owns its lifecycle and must wait for it to return before
// closing the repository.
func runBotStructuralEventPruner(
	ctx context.Context,
	interval time.Duration,
	now func() time.Time,
	prune func(context.Context, int64) (int64, error),
	log *slog.Logger,
) {
	pruneBotStructuralEvents(ctx, now, prune, log)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pruneBotStructuralEvents(ctx, now, prune, log)
		}
	}
}

func pruneBotStructuralEvents(
	ctx context.Context,
	now func() time.Time,
	prune func(context.Context, int64) (int64, error),
	log *slog.Logger,
) {
	if ctx.Err() != nil {
		return
	}
	cutoff := now().Add(-botStructuralEventRetention).Unix()
	deleted, err := prune(ctx, cutoff)
	if err != nil {
		log.Warn("could not prune retained bot structural events", "err", err)
		return
	}
	if deleted > 0 {
		log.Info("pruned retained bot structural events", "count", deleted)
	}
}

func pruneBotStructuralEventsInStore(ctx context.Context, repo store.Repository, cutoff int64) (int64, error) {
	var deleted int64
	err := repo.Tx(ctx, func(tx store.Repository) error {
		var err error
		deleted, err = tx.PruneBotStructuralEvents(ctx, cutoff)
		return err
	})
	return deleted, err
}
