package gateway

import (
	"context"
	"testing"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

func subscribeFrame(t *testing.T, groups []id.ID) Inbound {
	t.Helper()
	raw, err := cborx.Marshal(groups)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return Inbound{Op: OpSubscribe, CID: 7, Payload: []cbor.RawMessage{raw}}
}

func subscribed(c *conn) map[id.ID]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[id.ID]bool{}
	for gid := range c.groups {
		out[gid] = true
	}
	return out
}

func expectErrorCode(t *testing.T, h *harness, device id.ID, want string) {
	t.Helper()
	in := h.waitFrame(t, device)
	if in.Op != OpError {
		t.Fatalf("op %d, want error", in.Op)
	}
	code, err := rawText(in.Payload[1])
	if err != nil {
		t.Fatalf("error code: %v", err)
	}
	if code != want {
		t.Fatalf("code = %s, want %s", code, want)
	}
}

// subscribe adds only the device's own groups, and a frame past the cap adds nothing. Until the
// final review it added every id a client named, without bound, to a per-connection map.
func TestSubscribeIsBoundedToTheDevicesGroups(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	a, b, stranger := id.New(), id.New(), id.New()
	h.setGroupsForDevice(device, a)
	c := h.connect(t, device)
	h.drainReady(t, c)

	// The device joins b after ready; subscribing to it is how the connection learns.
	h.setGroupsForDevice(device, a, b)
	h.gw.handle(context.Background(), c, subscribeFrame(t, []id.ID{b, stranger}))
	got := subscribed(c)
	if !got[a] || !got[b] {
		t.Fatalf("subscribed = %v, want the device's groups a and b", got)
	}
	if got[stranger] {
		t.Fatal("a group the device is not in was subscribed")
	}
	expectErrorCode(t, h, device, "E_FORBIDDEN")

	many := make([]id.ID, maxSubscribedGroups+1)
	for i := range many {
		many[i] = id.New()
	}
	h.gw.handle(context.Background(), c, subscribeFrame(t, many))
	if n := len(subscribed(c)); n != 2 {
		t.Fatalf("a subscribe past the cap left %d groups, want the 2 it had", n)
	}
	expectErrorCode(t, h, device, "E_FRAME_LIMIT")
}
