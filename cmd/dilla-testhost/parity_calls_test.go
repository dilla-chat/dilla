package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
)

type deliveries struct {
	to     []id.ID
	frames []gateway.Frame
}

func (d *deliveries) DeliverUser(user id.ID, f gateway.Frame) {
	d.to = append(d.to, user)
	d.frames = append(d.frames, f)
}

func rep(b byte) id.ID {
	var v id.ID
	for i := range v {
		v[i] = b
	}
	return v
}

func hexID(b byte) string {
	return strings.Repeat(string("0123456789abcdef"[b>>4])+string("0123456789abcdef"[b&15]), 16)
}

func post(t *testing.T, h http.Handler, path, body string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(body)))
	return rec.Code
}

// Lane G: the parity world seeds who is in a call as the voice_state frames dillad sends, one per participant to every
// named user, scoped to the call's channel; and a ring as a state-1 call_ring scoped to its DM.
func TestParityCallsDeliverVoiceStateAndRingFrames(t *testing.T) {
	d := &deliveries{}
	h := parityCalls(func() parityDeliverer { return d })
	body := `{"to":["` + hexID(1) + `","` + hexID(2) + `"],"channel":"` + hexID(9) + `","call":"` + hexID(9) +
		`","participants":[{"user":"` + hexID(3) + `","device":"` + hexID(4) + `","flags":17}]}`
	if code := post(t, h, "/debug/parity/voice", body); code != http.StatusNoContent {
		t.Fatalf("voice: %d", code)
	}
	if len(d.frames) != 2 || d.frames[0].Op != gateway.OpVoiceState || d.to[1] != rep(2) {
		t.Fatalf("voice frames: %+v to %v", d.frames, d.to)
	}
	if got := d.frames[0].Scope.Channels; len(got) != 1 || got[0] != rep(9) {
		t.Fatalf("scope %v", got)
	}
	want, err := gateway.VoiceStatePayload(rep(3), rep(4),
		rep(9), 17)
	if err != nil || string(d.frames[0].Payload) != string(want) {
		t.Fatalf("payload %x, want %x (%v)", d.frames[0].Payload, want, err)
	}
	ring := `{"to":"` + hexID(1) + `","call":"` + hexID(7) + `","channel":"` + hexID(7) + `","caller":"` + hexID(3) + `","expires":1800000000}`
	if code := post(t, h, "/debug/parity/ring", ring); code != http.StatusNoContent {
		t.Fatalf("ring: %d", code)
	}
	last := d.frames[len(d.frames)-1]
	if last.Op != gateway.OpCallRing {
		t.Fatalf("ring frame %+v", last)
	}
	// Wave 2 review, lane G minor: the ring goes to its one recipient, scoped to its DM, with dillad's own payload.
	if to := d.to[len(d.to)-1]; to != rep(1) {
		t.Fatalf("ring recipient %v", to)
	}
	if got := last.Scope.Channels; len(got) != 1 || got[0] != rep(7) {
		t.Fatalf("ring scope %v", got)
	}
	wantRing, err := gateway.CallRingPayload(rep(7), rep(7), rep(3), 1, 1800000000)
	if err != nil || string(last.Payload) != string(wantRing) {
		t.Fatalf("ring payload %x, want %x (%v)", last.Payload, wantRing, err)
	}
}

func TestParityCallsRefuseBadInput(t *testing.T) {
	d := &deliveries{}
	h := parityCalls(func() parityDeliverer { return d })
	for _, body := range []string{
		`{"to":["xyz"],"channel":"` + hexID(9) + `","call":"` + hexID(9) + `","participants":[]}`,
		`{"to":[],"channel":"` + hexID(9) + `","call":"` + hexID(9) + `","participants":[{"user":"` + hexID(3) + `","device":"` + hexID(4) + `","flags":32}]}`,
		`not json`,
	} {
		if code := post(t, h, "/debug/parity/voice", body); code != http.StatusBadRequest {
			t.Fatalf("%s: %d", body, code)
		}
	}
	for _, body := range []string{
		`{"to":"xyz","call":"` + hexID(7) + `","channel":"` + hexID(7) + `","caller":"` + hexID(3) + `","expires":1}`,
		`{"to":"` + hexID(1) + `","call":"` + hexID(7) + `","channel":"short","caller":"` + hexID(3) + `","expires":1}`,
		`not json`,
	} {
		if code := post(t, h, "/debug/parity/ring", body); code != http.StatusBadRequest {
			t.Fatalf("ring %s: %d", body, code)
		}
	}
	if len(d.frames) != 0 {
		t.Fatalf("a refused request delivered %d frames", len(d.frames))
	}
}
