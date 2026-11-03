package gateway

// factor_owed_test.go is the control row of nonEnrolled's "enrolled, factor owed" (web3.6 S1): the
// same enrolled session with the flag clear identifies, so the refusal there is the flag's and
// nothing else's.

import (
	"context"
	"testing"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/id"
)

func TestAnEnrolledSessionThatOwesNoFactorIdentifies(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	token := scopedSession(h, device, auth.Session{Scope: auth.ScopeEnrolled, FactorOwed: false})
	s := newRecordingSink(8, false)
	go h.gw.serve(context.Background(), s, "")
	h.readFrom(t, s) // hello

	p, err := payload(token, uint64(1), uint64(1), uint64(1), uint64(0))
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	s.feed(mustEncode(t, Frame{Op: OpIdentify, Payload: p, Replay: true}, 1))
	if in := h.readFrom(t, s); in.Op != OpReady {
		t.Fatalf("identify of an enrolled session owing nothing: op %d, want ready", in.Op)
	}
	if !h.gw.Online(device) {
		t.Fatal("an enrolled session owing nothing is not online")
	}
}
