package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/id"
)

// A resume that fails after the paused writer exists — the token cannot rotate, or `resumed`
// cannot be built — must stop that writer: it is parked on its gate and, unstopped, its run
// goroutine outlives the connection forever.
func TestAResumeThatFailsAfterThePausedWriterExistsStopsTheWriter(t *testing.T) {
	errInjected := errors.New("injected")
	for name, inject := range map[string]func(g *Gateway){
		"the resume token cannot rotate": func(g *Gateway) {
			g.newResumeTokenFn = func() ([]byte, error) { return nil, errInjected }
		},
		"resumed cannot be built": func(g *Gateway) {
			g.resumedPayloadFn = func(uint64, uint64, []byte) (cbor.RawMessage, error) { return nil, errInjected }
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			device := id.New()
			c := h.connect(t, device)
			token := c.resumeToken()
			c.mu.Lock()
			old := c.writer
			c.mu.Unlock()
			h.gw.suspend(c)
			inject(h.gw)

			s := newRecordingSink(16, false)
			go h.gw.serve(context.Background(), s, "")
			h.readFrom(t, s) // hello

			p, err := payload(h.tokenFor(device), token, h.generation, uint64(0))
			if err != nil {
				t.Fatalf("payload: %v", err)
			}
			s.feed(mustEncode(t, Frame{Op: OpResume, Payload: p, Replay: true}, 2))

			// The resume installs a new writer on c before it fails, so wait for one that is not
			// the writer suspend already stopped, then for that writer's run goroutine to end.
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				c.mu.Lock()
				w := c.writer
				c.mu.Unlock()
				if w != nil && w != old {
					select {
					case <-w.finished:
						return
					case <-time.After(50 * time.Millisecond):
					}
					continue
				}
				time.Sleep(time.Millisecond)
			}
			t.Fatal("the paused writer's run goroutine is still parked after the resume failed")
		})
	}
}
