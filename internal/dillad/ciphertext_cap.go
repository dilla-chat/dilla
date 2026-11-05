package dillad

import (
	"log/slog"

	"github.com/dilla-chat/dilla/internal/config"
)

// warnSmallCiphertextCap logs one WARN at startup when limits.max_ciphertext_bytes is below the
// default (signal3 final review M4). A message with ten files and their previews needs about
// 132 KB of ciphertext, so the signal2 value 131072 refuses it for everyone on the instance; an
// operator who kept the old value in dilla.toml learns it here instead of from users.
func warnSmallCiphertextCap(log *slog.Logger, cfg *config.Config) {
	recommended := config.Default().Limits.MaxCiphertextBytes
	if cfg.Limits.MaxCiphertextBytes >= recommended {
		return
	}
	log.Warn("limits.max_ciphertext_bytes is below the default: messages with many files and previews "+
		"(a ten-file message needs about 132 KB) will be refused for everyone",
		"setting", "limits.max_ciphertext_bytes", "value", cfg.Limits.MaxCiphertextBytes, "recommended", recommended)
}
