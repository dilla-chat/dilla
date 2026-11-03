package gateway

import (
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// An operator's `reset-factors` runs in another process and keeps the sessions (web3.6 D5): the row
// still resolves but its account now owes a second factor. The sweep closes that socket 4003, the
// code a re-identify would meet, not resumable, and tells the composition root so it can cut the
// device's calls. The control is TestASweepLeavesAConnectionWithALiveSessionAndReadsItOnce, which
// leaves a live, unowed session alone.
func TestASweepClosesAConnectionWhoseAccountNowOwesAFactor(t *testing.T) {
	h := newSessionHarness(t)
	var owed []id.ID
	h.gw.opts.OnFactorOwed = func(device id.ID) { owed = append(owed, device) }
	token := h.conn.resumeToken()
	now := h.clk.Now().Unix()
	h.ledger.put(store.SessionRow{
		TokenHash: h.hash, DeviceID: h.device, UserID: id.New(), FactorOwed: true,
		Created: now, Expires: now + 30*24*3600, IdleExpires: now + 30*24*3600,
	})

	h.clk.Advance(h.gw.beat.interval)
	h.gw.sweepLiveness()

	if code := h.waitClose(t, h.conn); code != CloseUnauthenticated {
		t.Fatalf("close code = %d, want 4003", code)
	}
	if _, ok := h.gw.suspended.Load(string(token)); ok {
		t.Fatal("a connection closed 4003 is resumable: it would skip the owed check")
	}
	if len(owed) != 1 || owed[0] != h.device {
		t.Fatalf("OnFactorOwed saw %v, want the one device", owed)
	}
}
