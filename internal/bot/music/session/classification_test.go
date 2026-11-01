package session

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/jonasthim/dilla/internal/bot/music"
	"github.com/jonasthim/dilla/internal/bot/music/livekit"
	"github.com/jonasthim/dilla/internal/bot/music/spotify"
	"github.com/jonasthim/dilla/internal/bot/music/youtube"
)

func TestProviderFailuresDiscardOnlyPermanentCauses(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cause     error
		permanent bool
	}{
		{"invalid URL", youtube.ErrInvalidInput, true},
		{"invalid metadata", youtube.ErrMetadataInvalid, true},
		{"sandbox", youtube.ErrSandboxUnavailable, false},
		{"runner", youtube.ErrRunnerRequired, false},
		{"credential load", spotify.ErrCredentialUnavailable, false},
		{"production helper playback", fmt.Errorf("%w: playback/provider_unavailable", spotify.ErrPlaybackFailed), true},
		{"helper invalid request", fmt.Errorf("%w: playback/invalid_request", spotify.ErrPlaybackFailed), false},
		{"helper credential", fmt.Errorf("%w: authorization/credential_unavailable", spotify.ErrPlaybackFailed), false},
		{"helper unclassified", spotify.ErrPlaybackFailed, false},
		{"DNS", &net.DNSError{Err: "temporary", Name: "private.invalid"}, false},
		{"canceled", context.Canceled, false}, {"deadline", context.DeadlineExceeded, false},
		{"lease busy", music.ErrSpotifyBusy, false}, {"room", livekit.ErrRoomConnection, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, stage := range []string{"provider_check", "provider_pcm", "spotify_lease"} {
				err := providerStage(context.Background(), stage, tc.cause)
				var failure *music.StartError
				if !errors.As(err, &failure) || failure.ProviderFailure() != tc.permanent {
					t.Fatalf("stage %s permanent=%v: %v", stage, tc.permanent, err)
				}
				if !errors.Is(err, tc.cause) {
					t.Fatal("original cause lost")
				}
			}
		})
	}
}

func TestQueueStarterClearsCallRegistrationAfterLeave(t *testing.T) {
	for _, closeOwner := range []bool{false, true} {
		for _, fail := range []bool{false, true} {
			leaves, cleared := 0, 0
			starter := QueueStarter{}
			leave := func() error {
				leaves++
				if fail {
					return ErrInvalid
				}
				return nil
			}
			if closeOwner {
				starter.CloseConnection = leave
			} else {
				starter.LeaveConnection = leave
			}
			starter.OnCallEnded = func() { cleared++ }
			var err error
			if closeOwner {
				err = starter.Close()
			} else {
				err = starter.LeaveIdle()
			}
			if leaves != 1 || (fail && (!errors.Is(err, ErrInvalid) || cleared != 0)) || (!fail && (err != nil || cleared != 1)) {
				t.Fatalf("close=%v failure=%v leaves=%d cleared=%d error=%v", closeOwner, fail, leaves, cleared, err)
			}
		}
	}
}
