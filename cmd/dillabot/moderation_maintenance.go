//go:build linux

package main

import (
	"context"
	"log/slog"

	"github.com/dilla-chat/dilla/internal/id"
)

type expiredEvidencePurger interface {
	PurgeExpiredResumable(context.Context) (int, error)
}

// maintainModerationEvidence reports the bounded result of one resumable purge
// page. Errors are kept out of logs because backend diagnostics may contain
// details about encrypted state.
func maintainModerationEvidence(ctx context.Context, logger *slog.Logger, installation id.ID, purger expiredEvidencePurger) error {
	removed, err := purger.PurgeExpiredResumable(ctx)
	if err != nil {
		logger.Error("bot moderation evidence retention maintenance failed", "installation_id", installation.String(), "stage", "retention_purge")
		return err
	}
	logger.Info("bot moderation evidence retention maintenance complete", "installation_id", installation.String(), "removed_records", removed)
	return nil
}
