//go:build linux

package main

// Bot v2 Task 16 fix round 4: an ended gateway connection takes its feed worker with it (N-7's production trigger), and
// stagings are bounded and end with the feed (N-8).

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/gatewayclient"
	"github.com/dilla-chat/dilla/internal/gateway"
)

// TestAnEndedConnectionsFeedWorkerStops (N-7's trigger): dillad ends a connection with a terminal error after ready.
// The connection's feed worker ends with it, so it cannot keep applying that stream's device lists beside the next
// connection's snapshot (two Pushed callers for one user on one installationFeed). A device list handed to the ended
// connection's feed is not applied.
func TestAnEndedConnectionsFeedWorkerStops(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	community, installation := feedID(1), feedID(9)
	var d *fakeDillad
	d = newFakeDillad(t, func(ctx context.Context, ws *websocket.Conn, _ uint64, _ bool) {
		_ = d.send(ctx, ws, gatewayclient.OpError, 0, []any{uint64(0), "E_UNAUTHENTICATED", "session revoked"}) // terminal
		<-ctx.Done()
	})
	api, err := client.New(client.Config{BaseURL: supervisedOrigin, InstanceID: d.instance, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	launch, err := gatewayclient.NewLaunchToken(bytes.Clone(d.launch))
	if err != nil {
		t.Fatal(err)
	}
	r := newBotGatewayRuntime(ctx, api, true, quietLogger())
	r.httpClient = &http.Client{Transport: unixTransport(d.sock)}
	r.launch = launch
	defer r.Close()
	if err := r.Ensure(installation, community, d.dev, d.user, "session-bearer"); err != nil {
		t.Fatal(err)
	}
	e := r.entry(installation)
	if e == nil {
		t.Fatal("no entry after Ensure")
	}
	select {
	case <-e.done:
	case <-ctx.Done():
		t.Fatal("the connection did not end on the terminal error")
	}
	// The connection is over. Its feed worker must be too: a device list handed to its feed now is never applied.
	h := &recordingFeed{}
	e.feed.setFeed(h)
	e.feed.deliver(gatewayclient.Delivery{Frame: gatewayclient.Frame{Op: gatewayclient.OpBotEvent, N: 2,
		BotPayload: botEventRaw(t, gateway.BotEventDeviceList, []any{feedID(4), []byte{0xad}})}}, nil)
	time.Sleep(200 * time.Millisecond)
	h.mu.Lock()
	lists := h.lists
	h.mu.Unlock()
	if lists != 0 {
		t.Fatalf("the ended connection's feed worker applied %d device lists after the connection ended; want none", lists)
	}
}
