package youtube

import "errors"

// The stream errors. Bot v2 Task 23 removed the per-request PCM starter and its PCM-to-Opus path: the sandboxed
// stream worker's single ffmpeg delivers Ogg Opus (stream.go), which the encoder forwards as is.
var (
	ErrPCMLimit           = errors.New("youtube: decoded PCM exceeds limit")
	ErrPCMStart           = errors.New("youtube: audio process failed")
	ErrSandboxUnavailable = errors.New("youtube: production network sandbox unavailable")
)
