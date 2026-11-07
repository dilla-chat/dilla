package gatewayclient

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/coder/websocket"

	"github.com/dilla-chat/dilla/internal/bot/client"
)

// TestBotEventFramesAreReadUpToTheOneMiBPushBound (Bot v2 task 14): dillad pushes a device-list history of up to 1 MiB
// in one op 65 (api.BotFeedMaxDeviceListBytes), far above the hello's max_frame_bytes. The Bot reads such a frame, in a
// snapshot (n = 0) and live (ringed); every other server frame stays held to the hello's bound.
func TestBotEventFramesAreReadUpToTheOneMiBPushBound(t *testing.T) {
	instance, device, user := testID(1), testID(2), testID(3)
	push := bytes.Repeat([]byte{0x5a}, 1<<20) // the largest push dillad sends
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"dilla.v1"}})
		if err != nil {
			return
		}
		defer ws.CloseNow()
		if sendServerFrame(ctx, ws, OpHello, 0, nil, helloPayload(instance, 7)) != nil {
			return
		}
		if _, _, err := ws.Read(ctx); err != nil {
			return
		}
		if sendServerFrame(ctx, ws, OpReady, 1, nil, readyPayload(device, user, 7, bytes32(1))) != nil {
			return
		}
		_ = sendServerFrame(ctx, ws, OpBotEvent, 0, nil, []any{uint64(1), []any{testID(9), push}})
		_ = sendServerFrame(ctx, ws, OpBotEvent, 2, nil, []any{uint64(1), []any{testID(9), push}})
		_, _, _ = ws.Read(ctx)
	}))
	defer server.Close()
	api, err := client.New(client.Config{BaseURL: server.URL, InstanceID: instance, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	gw, err := New(Config{Client: api, AllowLoopbackHTTP: true, ExpectedDeviceID: device, ExpectedUserID: user})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = gw.Run(ctx, func(context.Context) (string, error) { return "token", nil }) }()
	var events []Frame
	for len(events) < 3 {
		select {
		case d, ok := <-gw.Events():
			if !ok {
				t.Fatalf("the client stopped after %d events", len(events))
			}
			events = append(events, d.Frame)
		case <-t.Context().Done():
			t.Fatal("timed out")
		}
	}
	if events[0].Op != OpReady || events[1].Op != OpBotEvent || events[1].N != 0 || events[2].Op != OpBotEvent || events[2].N != 2 {
		t.Fatalf("events %d/%d, %d/%d, %d/%d; want ready, a snapshot bot_event and a ringed one",
			events[0].Op, events[0].N, events[1].Op, events[1].N, events[2].Op, events[2].N)
	}
}

// The op 65 bound is exactly the 1 MiB push plus its framing; another op keeps the hello's bound.
func TestServerFrameLimitWidensOnlyBotEvents(t *testing.T) {
	event, err := encodeFrame(OpBotEvent, 4, nil, []any{uint64(1), []any{testID(9), bytes.Repeat([]byte{1}, 1<<20)}})
	if err != nil {
		t.Fatal(err)
	}
	if limit := serverFrameLimit(event, 131584); limit != MaxBotEventFrameBytes || uint64(len(event)) > limit {
		t.Fatalf("op 65 limit %d for a %d-byte frame, want %d", limit, len(event), MaxBotEventFrameBytes)
	}
	if _, err := DecodeServerFrame(event, serverFrameLimit(event, 131584)); err != nil {
		t.Fatalf("a 1 MiB push does not decode: %v", err)
	}
	other, err := encodeFrame(OpCommunityChanged, 0, nil, []any{testID(9)})
	if err != nil {
		t.Fatal(err)
	}
	if limit := serverFrameLimit(other, 131584); limit != 131584 {
		t.Fatalf("op 54 limit %d, want the hello's 131584", limit)
	}
	if MaxBotEventFrameBytes < (1<<20)+64 || MaxBotEventFrameBytes > (1<<20)+4096 {
		t.Fatalf("MaxBotEventFrameBytes %d is not the 1 MiB push plus framing", MaxBotEventFrameBytes)
	}
}
