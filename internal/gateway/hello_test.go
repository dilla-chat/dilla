package gateway

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/id"
)

// Deviation B23 (ruling 44), closed by task 27a: `hello` grows from seven elements to nine, the
// last two being invariant 7's back-off window — backoff_ms and backoff_jitter_ms. The instance
// only ever addresses the elected committer, so the other devices learn how long to wait from
// hello, once per connection, and the window is whatever Options carries: the composition root
// fills it from ds.Policy.
func TestHelloAdvertisesTheBackoffWindow(t *testing.T) {
	g := New(Options{Backoff: 450 * time.Millisecond, BackoffJitter: 150 * time.Millisecond})
	c, ctx := dialGateway(t, g, &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": []string{"Bearer nobody"}},
		Subprotocols: []string{"dilla.v1"},
	})
	in := readWS(t, ctx, c)
	if in.Op != OpHello {
		t.Fatalf("first frame op %d, want hello", in.Op)
	}
	if len(in.Payload) != 9 {
		t.Fatalf("hello has %d elements, want 9", len(in.Payload))
	}
	for i, want := range map[int]uint64{7: 450, 8: 150} {
		got, err := rawUint(in.Payload[i])
		if err != nil {
			t.Fatalf("hello[%d]: %v", i, err)
		}
		if got != want {
			t.Errorf("hello[%d] = %d, want %d", i, got, want)
		}
	}
}

// Unset, the window is protocol/02's own: 300 ms + random(0..300 ms).
func TestTheBackoffWindowDefaultsToInvariant7s(t *testing.T) {
	o := Options{}.withDefaults()
	if o.Backoff != 300*time.Millisecond || o.BackoffJitter != 300*time.Millisecond {
		t.Fatalf("Backoff/BackoffJitter default to %v/%v, want 300ms/300ms", o.Backoff, o.BackoffJitter)
	}
}

// Bot v2 Task 1: an instance on wire 6 tells a client that is still on wire 5 to update. Its
// session was negotiated at wire 5 and it identifies at wire 5: the refusal is an error frame
// naming E_UPGRADE_REQUIRED and close 4006, and nothing is recorded. The same bearer identifying
// at wire 6 (the updated client) is admitted and its selection moves to 6.
func TestIdentifyOnWire5SessionIsRefused(t *testing.T) {
	h := newHarness(t)
	h.gw.opts.WireVersions = []uint64{6}
	recorder := &capabilityRecordingAuth{Authenticator: h}
	h.gw.opts.Auth = recorder
	device := id.New()
	hash := []byte("01234567890123456789012345678901")
	token := scopedSession(h, device, auth.Session{Scope: auth.ScopeEnrolled, WireVersion: 5, E2EEVersion: 1, MediaVersion: 1, TokenHash: hash})
	sink := newRecordingSink(8, false)
	if _, ok := h.gw.identifyConnection(context.Background(), sink, identifyInbound(t, token, 5)); ok {
		t.Fatal("a wire 5 identify was admitted by a wire 6 instance")
	}
	frame := h.readFrom(t, sink)
	if frame.Op != OpError {
		t.Fatalf("refusal op=%d, want error", frame.Op)
	}
	if code, err := rawText(frame.Payload[1]); err != nil || code != "E_UPGRADE_REQUIRED" {
		t.Fatalf("error code=%q err=%v, want E_UPGRADE_REQUIRED", code, err)
	}
	if got := h.waitCloseOn(t, sink); got != CloseVersion {
		t.Fatalf("close code=%d, want %d", got, CloseVersion)
	}
	if _, ok := recorder.selected(hash); ok {
		t.Fatal("the refused wire 5 identify recorded a selection")
	}
	updated := newRecordingSink(8, false)
	if c, ok := h.gw.identifyConnection(context.Background(), updated, identifyInbound(t, token, 6)); !ok || c == nil {
		t.Fatal("the same session identifying at wire 6 was refused")
	}
	if got, ok := recorder.selected(hash); !ok || got != [3]uint64{6, 1, 1} {
		t.Fatalf("recorded selection = %v %v, want [6 1 1]", got, ok)
	}
}
