package sfu

import (
	"errors"
	"net/url"
	"testing"

	"github.com/livekit/protocol/livekit"
)

// The /rtc gate admits exactly the signalling protocols dilla's clients speak: 17 (livekit-client
// 2.22.3 and server-sdk-go) and 18 (the desktop's livekit 0.10.1 Rust SDK, livekit-signaling 0.1.9).
// A neighbour of either, and a value that does not parse, is refused before LiveKit labels a metric.
func TestCheckClientAdmitsExactlyTheProtocolsDillaClientsSpeak(t *testing.T) {
	for _, p := range []int32{17, 18} {
		if err := (Join{Protocol: p}).CheckClient(); err != nil {
			t.Errorf("protocol %d refused: %v", p, err)
		}
	}
	for _, p := range []int32{0, 16, 19, -17} {
		if err := (Join{Protocol: p}).CheckClient(); !errors.Is(err, ErrClientProtocol) {
			t.Errorf("protocol %d: got %v, want ErrClientProtocol", p, err)
		}
	}
	// The v0 query form: an unparsable `protocol` reads as 0, which is refused.
	for _, raw := range []string{"17", "18", "seventeen", "17.0", "", "99999999999"} {
		j, err := ReadJoin(url.Values{"protocol": {raw}})
		if err != nil {
			t.Fatalf("ReadJoin(%q): %v", raw, err)
		}
		admitted := j.CheckClient() == nil
		if want := raw == "17" || raw == "18"; admitted != want {
			t.Errorf("protocol=%q admitted %v, want %v", raw, admitted, want)
		}
	}
	// The v1 SDK enum: the Rust SDK (the desktop) and the JS SDK (the web client) are LiveKit's own
	// values; a value the enum does not name is refused.
	for _, sdk := range []livekit.ClientInfo_SDK{livekit.ClientInfo_RUST, livekit.ClientInfo_JS, livekit.ClientInfo_GO} {
		if err := (Join{Protocol: 18, SDK: sdk}).CheckClient(); err != nil {
			t.Errorf("sdk %v refused: %v", sdk, err)
		}
	}
	if err := (Join{Protocol: 18, SDK: 999}).CheckClient(); !errors.Is(err, ErrClientProtocol) {
		t.Errorf("sdk 999: got %v, want ErrClientProtocol", err)
	}
}
