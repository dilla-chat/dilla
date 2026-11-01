package botstrings

import (
	"strings"
	"testing"
)

func TestMusicSkippedNeverPublishesQueueMetadata(t *testing.T) {
	got := MusicSkipped("private title by private requester https://private.example", "provider unavailable")
	if strings.Contains(got, "private") || got != "Couldn't play a track. Skipping." {
		t.Fatalf("public notice disclosed queue metadata: %q", got)
	}
}

func TestMusicSkippedBatchUsesExactReasonFreeCopy(t *testing.T) {
	for _, reason := range []string{"provider unavailable", "audio unavailable", "private diagnostic"} {
		if got := MusicSkippedBatch(1, reason); got != "Couldn't play a track. Skipping." {
			t.Fatalf("singular=%q", got)
		}
		if got := MusicSkippedBatch(3, reason); got != "Couldn't play 3 tracks. Skipping." {
			t.Fatalf("plural=%q", got)
		}
	}
}
