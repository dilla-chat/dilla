//go:build linux

package main

import (
	"log/slog"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/id"
)

func logBotRateLimit(logger *slog.Logger, installation id.ID, refusal *client.APIError) {
	var retry uint64
	if refusal.RetryAfterMS != nil {
		retry = *refusal.RetryAfterMS
	}
	logger.Info("bot rate limited; resuming next tick", "installation_id", installation.String(), "retry_after_ms", retry)
}

// A joined maintenance error is deferred only when every cause is transient.
func reconciliationRateLimit(err error) *client.APIError { return client.RateLimitError(err) }

// installationGone is only what the pass already read: dillad's current row says the installation is disabled or
// removed (state 4 and up, the same test the installation listing gets in reconcile).
func installationGone(installation client.Installation) bool { return installation.State >= 4 }

// cleanupUnreadyInstallation answers a pass that did not reach readiness. A non-deferred error there (a dillad 409 or
// 5xx, a Welcome the membership reconcile could not apply) is not about the helper: a usable retained helper stays,
// with its music paused. Only an unusable helper, one the error names as failed, or a gone installation's is closed
// (KeepAfterPassError).
func cleanupUnreadyInstallation(installation id.ID, ready, gone bool, passErr error, registry *musicQueueRegistry) {
	if !ready && !client.IsDeferred(passErr) {
		_ = registry.KeepAfterPassError(installation, gone, passErr)
	}
}

// A deferred channel read must retain the helper and the existing music handlers. Any other restore error pauses
// music and keeps a usable helper the same way.
func cleanupFailedMusicRestore(logger *slog.Logger, installation id.ID, gone bool, err error, registry *musicQueueRegistry) error {
	logger.Error("bot music queue restore paused", "installation_id", installation.String(), "error_class", helper.Class(err))
	if client.IsDeferred(err) {
		return err
	}
	_ = registry.KeepAfterPassError(installation, gone, err)
	return nil
}
