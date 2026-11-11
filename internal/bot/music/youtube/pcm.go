package youtube

import (
	"errors"

	"github.com/dilla-chat/dilla/internal/bot/music"
)

// The stream errors. Bot v2 Task 23 removed the per-request PCM starter and its PCM-to-Opus path: the sandboxed
// stream worker's single ffmpeg delivers Ogg Opus (stream.go), which the encoder forwards as is.
var (
	ErrPCMLimit = errors.New("youtube: decoded PCM exceeds limit")
	ErrPCMStart = errors.New("youtube: audio process failed")
	// ErrSandboxUnavailable is the Bot's player failing, not a track: it also matches music.ErrPlayerUnavailable, so a
	// track that meets it stays in the queue (v0.1.0-bot2.2).
	ErrSandboxUnavailable error = &playerUnavailable{text: "youtube: production network sandbox unavailable"}
)

type playerUnavailable struct{ text string }

func (e *playerUnavailable) Error() string        { return e.text }
func (e *playerUnavailable) Is(target error) bool { return target == music.ErrPlayerUnavailable }
