package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/id"
)

func TestAppBuildHintRequiresWireFiveAndIsEphemeral(t *testing.T) {
	for _, wire := range []uint64{2, 3, 4, 5} {
		t.Run(string(rune('0'+wire)), func(t *testing.T) {
			h := newHarness(t)
			h.gw.opts.WireVersions = []uint64{wire}
			h.gw.opts.Auth = &capabilityRecordingAuth{Authenticator: h}
			h.gw.opts.CurrentAppEntry = func() string { return "/assets/index-current.js" }
			c := h.connect(t, id.New())
			h.drainReady(t, c)
			if wire < 5 {
				select {
				case <-h.sink(c).frames:
					t.Fatal("legacy session received a new frame")
				case <-time.After(20 * time.Millisecond):
				}
				return
			}
			frame := h.waitFrame(t, c.deviceID)
			if frame.Op != OpAppBuild || frame.CID != 0 || frame.GroupID != nil || len(frame.Payload) != 1 {
				t.Fatalf("app build frame=%+v", frame)
			}
			entry, err := rawText(frame.Payload[0])
			if err != nil || entry != "/assets/index-current.js" {
				t.Fatalf("entry=%q err=%v", entry, err)
			}
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.n != 1 {
				t.Fatalf("ephemeral hint changed replay sequence: %d", c.n)
			}
		})
	}
}

func TestAppBuildHintOnResumeUsesActualSelectedWire(t *testing.T) {
	for _, wire := range []uint64{3, 4, 5} {
		t.Run(string(rune('0'+wire)), func(t *testing.T) {
			h := newHarness(t)
			h.gw.opts.Auth = &capabilityRecordingAuth{Authenticator: h}
			h.gw.opts.WireVersions = []uint64{wire}
			entry := "/assets/first.js"
			h.gw.opts.CurrentAppEntry = func() string { return entry }
			c := h.connect(t, id.New())
			h.drainReady(t, c)
			if wire == 5 {
				h.waitFrame(t, c.deviceID)
			}
			token := c.resumeToken()
			h.gw.suspend(c)
			h.gw.opts.WireVersions = []uint64{5, 4, 3, 2}
			entry = "/assets/current.js"
			s := newRecordingSink(16, false)
			go h.gw.serve(context.Background(), s, "")
			h.readFrom(t, s)
			p, err := payload(h.tokenFor(c.deviceID), token, h.generation, uint64(1))
			if err != nil {
				t.Fatal(err)
			}
			s.feed(mustEncode(t, Frame{Op: OpResume, Payload: p, Replay: true}, 2))
			if in := h.readFrom(t, s); in.Op != OpResumed || len(in.Payload) != 3 {
				t.Fatalf("resume=%+v", in)
			}
			if wire == 5 {
				in := h.readFrom(t, s)
				if in.Op != OpAppBuild || in.CID != 0 {
					t.Fatalf("hint=%+v", in)
				}
				got, err := rawText(in.Payload[0])
				if err != nil || got != entry {
					t.Fatalf("entry=%q err=%v", got, err)
				}
			} else {
				select {
				case in := <-s.frames:
					t.Fatalf("legacy resume got extra frame: %x", in)
				case <-time.After(20 * time.Millisecond):
				}
			}
		})
	}
}

func TestAppBuildHintNeverAdvertisesInvalidEntry(t *testing.T) {
	for _, entry := range []string{"", "https://other.test/a.js", "/assets/../a.js", "/assets/a.css"} {
		h := newHarness(t)
		h.gw.opts.Auth = &capabilityRecordingAuth{Authenticator: h}
		h.gw.opts.WireVersions = []uint64{5}
		h.gw.opts.CurrentAppEntry = func() string { return entry }
		c := h.connect(t, id.New())
		h.drainReady(t, c)
		select {
		case <-h.sink(c).frames:
			t.Fatalf("advertised %q", entry)
		case <-time.After(20 * time.Millisecond):
		}
	}
}
