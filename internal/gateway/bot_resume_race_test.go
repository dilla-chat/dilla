package gateway

import (
	"context"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
)

// racingAuth runs during while a resume persists its capabilities: after the resume's early botDead check, before it
// commits the connection live.
type racingAuth struct {
	*capabilityRecordingAuth
	during func()
}

func (a racingAuth) RecordCapabilities(ctx context.Context, hash []byte, wire, e2ee, media uint64) error {
	if a.during != nil {
		a.during()
	}
	return a.capabilityRecordingAuth.RecordCapabilities(ctx, hash, wire, e2ee, media)
}

// TestAResumeRacingANewerBotIdentifyIsRefused (Bot v2 Task 16, R2-2): a newer identify of the same Bot device
// replaces the parked connection after its resume passed the early botDead check. The resume re-checks botDead where
// it commits and is refused, so it never ends live but unfed (absent from the device's Bot index).
func TestAResumeRacingANewerBotIdentifyIsRefused(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	c, token := h.finishedBot(t, device)
	h.gw.suspend(c)
	newer := &conn{gw: h.gw, deviceID: device, isBot: true}
	recorder, ok := h.gw.opts.Auth.(*capabilityRecordingAuth)
	if !ok {
		t.Fatalf("auth is %T", h.gw.opts.Auth)
	}
	h.gw.opts.Auth = racingAuth{capabilityRecordingAuth: recorder, during: func() { h.gw.bots.add(newer) }}
	_, in := h.resumeFrom(t, c, token, 2)
	if in.Op != OpInvalidSession {
		t.Fatalf("the replaced connection's resume answered op %d, want invalid_session", in.Op)
	}
	if got := h.gw.bots.of(device); len(got) != 1 || got[0] != newer {
		t.Fatalf("the device has %d Bot connections, want only the newer one", len(got))
	}
	c.mu.Lock()
	state := c.state
	c.mu.Unlock()
	if state == stateReady {
		t.Fatal("the replaced connection is live")
	}
}
