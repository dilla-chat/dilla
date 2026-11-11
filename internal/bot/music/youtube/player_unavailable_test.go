package youtube

import (
	"errors"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/music"
)

// v0.1.0-bot2.2 Task 7 fix round 1: a provider-wide refusal keeps the track paused (music.ErrPlayerUnavailable); a
// refusal of the one video removes it.
func TestProviderWideRefusalsArePlayerUnavailable(t *testing.T) {
	for class, wide := range map[string]bool{
		"http_429": true, "bot_check": true, "network": true, "rate_limited": true,
		"http_403": false, "video_unavailable": false, "video_private": false, "": false,
	} {
		err := &ProcessError{Cause: errors.New("yt-dlp exited"), class: class}
		if got := errors.Is(err, music.ErrPlayerUnavailable); got != wide {
			t.Fatalf("class %q: player unavailable = %v, want %v", class, got, wide)
		}
	}
	if !errors.Is(ErrSandboxUnavailable, music.ErrPlayerUnavailable) {
		t.Fatal("the sandbox being unavailable is the player's failure")
	}
}
