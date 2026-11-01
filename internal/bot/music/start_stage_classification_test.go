package music

import (
	"context"
	"errors"
	"testing"
)

type classifiedPlaybackFailure struct{ permanent bool }

func (e classifiedPlaybackFailure) Error() string                  { return "bounded failure" }
func (e classifiedPlaybackFailure) PermanentPlaybackFailure() bool { return e.permanent }

func TestStartErrorUsesCauseRatherThanStage(t *testing.T) {
	for _, stage := range []string{"provider_check", "provider_pcm", "spotify_lease", "room_connect"} {
		for _, cause := range []error{errors.New("unknown"), classifiedPlaybackFailure{false}, context.Canceled, context.DeadlineExceeded, ErrSpotifyBusy} {
			if (&StartError{Stage: stage, Cause: cause}).ProviderFailure() {
				t.Errorf("%s with %v would discard queue item", stage, cause)
			}
		}
		if !(&StartError{Stage: stage, Cause: classifiedPlaybackFailure{true}}).ProviderFailure() {
			t.Errorf("%s lost permanent cause", stage)
		}
	}
	if (&StartError{Stage: "provider_pcm", Cause: errors.Join(classifiedPlaybackFailure{true}, context.Canceled)}).ProviderFailure() {
		t.Fatal("canceled attempt must pause")
	}
}
