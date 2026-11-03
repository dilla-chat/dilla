//go:build linux

package main

import (
	"log/slog"

	"github.com/dilla-chat/dilla/internal/bot/service"
	"github.com/dilla-chat/dilla/internal/id"
)

func logSkippedCommandEvents(logger *slog.Logger, installation id.ID, skipped []service.SkippedGroupEvent) {
	if logger == nil {
		return
	}
	for _, event := range skipped {
		fields := []any{"installation_id", installation.String(), "delivery_id", event.DeliveryID, "cause", event.Cause}
		if !event.FirstFailure.IsZero() {
			fields = append(fields, "first_failure", event.FirstFailure)
		}
		logger.Warn("bot interaction event skipped as undeliverable during command intake", fields...)
	}
}
