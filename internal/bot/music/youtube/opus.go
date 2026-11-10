package youtube

import (
	"context"
	"io"
)

// PCMSourceStarter starts a track's stream: OwnedStreams in production (Ogg Opus from the stream worker),
// bounded fakes in tests. Bot v2 Task 23 removed the per-request PCM starter and its OpusStarter.
type PCMSourceStarter interface {
	Start(context.Context, string) (io.ReadCloser, error)
}
