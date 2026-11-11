// Package botstrings contains the Bot's user-facing copy.
package botstrings

import "fmt"

// MusicSkipped publishes no queue metadata or provider failure details. The
// arguments remain for compatibility with callers of the original copy helper.
func MusicSkipped(_, _ string) string {
	return "Couldn't play a track. Skipping."
}

// MusicSkippedBatch coalesces one start's skips without exposing their titles.
func MusicSkippedBatch(count int, reason string) string {
	if count <= 1 {
		return MusicSkipped("", reason)
	}
	return fmt.Sprintf("Couldn't play %d tracks. Skipping.", count)
}

// MusicTrackRemoved answers a /play whose track could not be played: it was removed from the queue (v0.1.0-bot2.2).
// Like the skip notice, it names no title, requester, URL or provider detail.
func MusicTrackRemoved() string {
	return "Couldn't play that track, so it was removed from the queue."
}
