//go:build linux

package main

// Bot v2 Task 16 fix round 4: an ended gateway connection takes its feed worker with it (N-7's production trigger), and
// stagings are bounded and end with the feed (N-8).

import (
	"bytes"
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/gatewayclient"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
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

// countingEpisodes counts the episode reads in flight and the most at once; each read hangs until released or its
// context ends.
type countingEpisodes struct {
	release                 chan struct{}
	inFlight, peak, started atomic.Int64
}

func (c *countingEpisodes) ResolveCurrentEpisode(ctx context.Context, _ id.ID) (id.ID, bool, error) {
	c.started.Add(1)
	n := c.inFlight.Add(1)
	for {
		p := c.peak.Load()
		if n <= p || c.peak.CompareAndSwap(p, n) {
			break
		}
	}
	defer c.inFlight.Add(-1)
	select {
	case <-c.release:
	case <-ctx.Done():
	}
	return id.ID{}, false, ctx.Err()
}

// TestStagingIsBoundedAndEndsWithTheFeed (N-8): twelve known members' lists change at once. At most
// maxConcurrentStagings episode reads run at a time; when the feed is closed the reads in flight end at once and the
// waiting stagings never start.
func TestStagingIsBoundedAndEndsWithTheFeed(t *testing.T) {
	ctx := t.Context()
	community := feedID(1)
	eps := &countingEpisodes{release: make(chan struct{})}
	defer close(eps.release)
	h := newInstallationFeed(feedID(9), community, feedID(8), quietLogger(), func() {})
	h.setDeps(feedDeps{
		ingest: func(context.Context, id.ID, []byte) (native.DeviceListIngest, error) {
			return native.DeviceListIngest{Changed: true, Version: 2}, nil
		},
		episodes: eps,
		stage:    func(context.Context, id.ID, id.ID) error { return nil },
	})
	const members = 12
	h.SnapshotBegin(ctx)
	for i := range members {
		h.Member(ctx, MemberEvent{Community: community, User: feedID(0x40 + byte(i)), Joined: true, Snapshot: true})
	}
	h.SnapshotDone(ctx)
	for i := range members {
		_ = h.DeviceList(ctx, feedID(0x40+byte(i)), []byte{1}, false)
	}
	waitFor(t, "the first stagings in flight", time.Second, func() bool { return eps.inFlight.Load() == maxConcurrentStagings })
	time.Sleep(50 * time.Millisecond)
	if peak, started := eps.peak.Load(), eps.started.Load(); peak != maxConcurrentStagings || started != maxConcurrentStagings {
		t.Fatalf("%d stagings at once (peak %d) for %d changed lists, want %d", started, peak, members, maxConcurrentStagings)
	}
	h.close()
	waitFor(t, "the stagings in flight ended", time.Second, func() bool { return eps.inFlight.Load() == 0 })
	time.Sleep(50 * time.Millisecond)
	if started := eps.started.Load(); started != maxConcurrentStagings {
		t.Fatalf("%d stagings started in all; want only the %d in flight at close, none of the waiting ones", started, maxConcurrentStagings)
	}
}
