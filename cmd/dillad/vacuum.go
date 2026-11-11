package main

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/dilla-chat/dilla/internal/store/sqlite"
)

// incrementalVacuumPages is how many free pages one hourly pass returns to the filesystem.
const incrementalVacuumPages = 1000

// runIncrementalVacuum returns free pages to the filesystem every interval until ctx ends. It logs only a pass that
// freed pages, or failed.
func runIncrementalVacuum(ctx context.Context, db *sql.DB, interval time.Duration, log *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		freed, err := sqlite.IncrementalVacuum(ctx, db, incrementalVacuumPages)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			log.Warn("incremental vacuum failed", "err", err)
		case freed > 0:
			log.Info("database free pages returned", "pages", freed)
		}
	}
}
