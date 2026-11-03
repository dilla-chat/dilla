package gateway

import (
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/id"
)

func TestTypingExpiresAfterTenSeconds(t *testing.T) {
	h := newHarness(t)
	user, device := id.New(), id.New()
	channel := id.New()
	h.gw.SetTyping(channel, user, device, true)
	if !h.gw.IsTyping(channel, user, device) {
		t.Fatal("typing must be live immediately")
	}
	h.clk.Advance(9 * time.Second)
	if !h.gw.IsTyping(channel, user, device) {
		t.Fatal("typing must still be live at 9 s")
	}
	h.clk.Advance(2 * time.Second)
	if h.gw.IsTyping(channel, user, device) {
		t.Fatal("typing must expire after 10 s")
	}
}

func TestEphemeralFramesAreNeverReplayed(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	c := h.connect(t, device)
	// `ready` is itself replayable (deviation B8), so it has already taken n = 1 and sits in the
	// ring. What this test is about is that an ephemeral frame takes NEITHER.
	nBefore, ringBefore := c.n, len(c.ring.since(0))
	p, err := PresencePayload(id.New(), 1, 1758659000, "")
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	h.gw.DeliverDevice(device, Frame{Op: OpPresence, Payload: p}) // Replay false
	if c.n != nBefore {
		t.Fatalf("n = %d after an ephemeral frame, want %d", c.n, nBefore)
	}
	if got := len(c.ring.since(0)); got != ringBefore {
		t.Fatalf("the ring holds %d frames after an ephemeral one, want %d", got, ringBefore)
	}
}

func TestPresenceUsesAnyActiveDeviceAndManualChoices(t *testing.T) {
	h := newHarness(t)
	user, first, second := id.New(), id.New(), id.New()
	a := h.connectUser(t, first, user)
	_ = h.connectUser(t, second, user)
	if got := h.gw.PresenceStatus(user, 1); got != 1 {
		t.Fatalf("connected = %d, want online", got)
	}
	h.clk.Advance(9 * time.Minute)
	h.gw.Activity(second)
	h.clk.Advance(2 * time.Minute)
	if got := h.gw.PresenceStatus(user, 1); got != 1 {
		t.Fatalf("active second device = %d, want online", got)
	}
	if got := h.gw.PresenceStatus(user, 2); got != 2 {
		t.Fatalf("manual idle = %d", got)
	}
	if got := h.gw.PresenceStatus(user, 3); got != 3 {
		t.Fatalf("dnd = %d", got)
	}
	if got := h.gw.PresenceStatus(user, 4); got != 0 {
		t.Fatalf("invisible = %d", got)
	}
	h.clk.Advance(10 * time.Minute)
	if got := h.gw.PresenceStatus(user, 1); got != 2 {
		t.Fatalf("inactive devices = %d, want idle", got)
	}
	h.gw.removeLive(a)
	h.gw.removeLive(h.gw.reg.connsOfDevice(second)[0])
	if got := h.gw.PresenceStatus(user, 1); got != 0 {
		t.Fatalf("disconnected = %d", got)
	}
}
