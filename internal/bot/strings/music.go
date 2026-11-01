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
