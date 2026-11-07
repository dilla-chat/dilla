package gateway

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

// Bot v2 Task 13: an incoming webhook (op 66) reaches only a live Bot runtime connection, unringed, and
// the POST waits for that connection's op 67 ack.

const webhookTestText = "the boss has been defeated by Ada"

func webhookPayload(t *testing.T, delivery id.ID) cbor.RawMessage {
	t.Helper()
	avatar := "https://example.test/a.png"
	p, err := BotWebhookPayload(delivery, id.New(), id.New(), id.New(), "Terraria", &avatar, webhookTestText, false)
	if err != nil {
		t.Fatalf("BotWebhookPayload: %v", err)
	}
	return p
}

func ackInbound(t *testing.T, delivery []byte, status uint64) Inbound {
	t.Helper()
	b, err := cborx.Marshal([]any{delivery, status})
	if err != nil {
		t.Fatal(err)
	}
	var elems []cbor.RawMessage
	if err := cborx.Unmarshal(b, &elems); err != nil {
		t.Fatal(err)
	}
	return Inbound{Op: OpBotWebhookAck, Payload: elems}
}

type webhookResult struct {
	status uint64
	err    error
}

func deliverAsync(h *harness, ctx context.Context, device, delivery id.ID, p cbor.RawMessage) <-chan webhookResult {
	out := make(chan webhookResult, 1)
	go func() {
		status, err := h.gw.DeliverBotWebhook(ctx, device, delivery, p)
		out <- webhookResult{status, err}
	}()
	return out
}

func TestWebhookReachesTheLiveBotUnringedAndWaitsForItsAck(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	bot, _ := h.finishedBot(t, device)
	delivery := id.New()
	res := deliverAsync(h, context.Background(), device, delivery, webhookPayload(t, delivery))
	in := h.waitFrame(t, device)
	if in.Op != OpBotWebhook || in.CID != 0 || len(in.Payload) != 8 {
		t.Fatalf("Bot got op %d n %d with %d elements, want an unringed bot_webhook of 8", in.Op, in.CID, len(in.Payload))
	}
	// Not ringed: nothing in the Bot connection's ring holds the text.
	for _, e := range bot.ring.entriesSince(0) {
		if bytes.Contains(e.bytes, []byte(webhookTestText)) {
			t.Fatal("the Bot connection's ring holds the webhook text")
		}
	}
	h.gw.handle(context.Background(), bot, ackInbound(t, delivery[:], 0))
	select {
	case r := <-res:
		if r.err != nil || r.status != 0 {
			t.Fatalf("DeliverBotWebhook = %d, %v; want 0, nil", r.status, r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the ack did not answer the delivery")
	}
	// A second ack for the same delivery is ignored; nothing is pending any more.
	h.gw.handle(context.Background(), bot, ackInbound(t, delivery[:], 0))
	if n := h.gw.webhooks.len(); n != 0 {
		t.Fatalf("%d deliveries pending after the ack, want 0", n)
	}
}

func TestWebhookIsOfflineUnlessTheBotIsLive(t *testing.T) {
	ctx := context.Background()
	t.Run("no connection", func(t *testing.T) {
		h := newHarness(t)
		if _, err := h.gw.DeliverBotWebhook(ctx, id.New(), id.New(), webhookPayload(t, id.New())); !errors.Is(err, ErrBotNotLive) {
			t.Fatalf("err = %v, want ErrBotNotLive", err)
		}
	})
	t.Run("a human connection of the device", func(t *testing.T) {
		h := newHarness(t)
		device := id.New()
		c := h.connect(t, device)
		h.drainReady(t, c)
		if _, err := h.gw.DeliverBotWebhook(ctx, device, id.New(), webhookPayload(t, id.New())); !errors.Is(err, ErrBotNotLive) {
			t.Fatalf("err = %v, want ErrBotNotLive", err)
		}
		noFrameOn(t, h.sink(c))
	})
	t.Run("parked in the resume window", func(t *testing.T) {
		h := newHarness(t)
		device := id.New()
		bot, _ := h.finishedBot(t, device)
		h.gw.suspend(bot)
		if _, err := h.gw.DeliverBotWebhook(ctx, device, id.New(), webhookPayload(t, id.New())); !errors.Is(err, ErrBotNotLive) {
			t.Fatalf("err = %v, want ErrBotNotLive", err)
		}
		for _, e := range bot.ring.entriesSince(0) {
			if bytes.Contains(e.bytes, []byte(webhookTestText)) {
				t.Fatal("a parked Bot connection's ring holds the webhook text")
			}
		}
	})
	t.Run("mid-snapshot", func(t *testing.T) {
		h := newHarness(t)
		device := id.New()
		streams := newBotStreams()
		c := h.connectBot(t, device, streams)
		h.readyResumeToken(t, c)
		_ = streams.next(t) // the snapshot is owed and not finished
		if _, err := h.gw.DeliverBotWebhook(ctx, device, id.New(), webhookPayload(t, id.New())); !errors.Is(err, ErrBotNotLive) {
			t.Fatalf("err = %v, want ErrBotNotLive", err)
		}
	})
}

// Only the Bot connection a delivery was sent on may ack it. An ack from a human connection, from another
// Bot connection or for an id never sent is ignored, and the POST times out.
func TestWebhookAckIsTakenOnlyFromTheConnectionItWasSentOn(t *testing.T) {
	h := newHarness(t)
	device, otherBotDevice, humanDevice := id.New(), id.New(), id.New()
	h.finishedBot(t, device)
	otherBot, _ := h.finishedBot(t, otherBotDevice)
	human := h.connect(t, humanDevice)
	h.drainReady(t, human)
	delivery := id.New()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	res := deliverAsync(h, ctx, device, delivery, webhookPayload(t, delivery))
	if in := h.waitFrame(t, device); in.Op != OpBotWebhook {
		t.Fatalf("op %d, want bot_webhook", in.Op)
	}
	h.gw.handle(context.Background(), human, ackInbound(t, delivery[:], 0))
	h.gw.handle(context.Background(), otherBot, ackInbound(t, delivery[:], 0))
	stranger := id.New()
	h.gw.handle(context.Background(), otherBot, ackInbound(t, stranger[:], 0))
	r := <-res
	if !errors.Is(r.err, ErrBotWebhookTimeout) {
		t.Fatalf("DeliverBotWebhook = %d, %v; want ErrBotWebhookTimeout", r.status, r.err)
	}
	if n := h.gw.webhooks.len(); n != 0 {
		t.Fatalf("%d deliveries pending after the timeout, want 0", n)
	}
}

// The pending table is bounded: past BotWebhookPendingCap a delivery is refused before anything is sent.
func TestWebhookPendingTableIsBounded(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	bot, _ := h.finishedBot(t, device)
	h.gw.webhooks.mu.Lock()
	h.gw.webhooks.byID = map[id.ID]*webhookWait{}
	for range BotWebhookPendingCap {
		h.gw.webhooks.byID[id.New()] = &webhookWait{c: bot, done: make(chan uint64, 1)}
	}
	h.gw.webhooks.mu.Unlock()
	if _, err := h.gw.DeliverBotWebhook(context.Background(), device, id.New(), webhookPayload(t, id.New())); !errors.Is(err, ErrBotWebhookBusy) {
		t.Fatalf("err = %v, want ErrBotWebhookBusy", err)
	}
	noFrameOn(t, h.sink(bot))
}

func TestWebhookAckFrameShape(t *testing.T) {
	ok := mustFrameBytes(t, []any{uint64(OpBotWebhookAck), uint64(0), nil, []any{bytes.Repeat([]byte{1}, 16), uint64(0)}})
	if in, err := Decode(ok, 1<<16); err != nil || in.Op != OpBotWebhookAck {
		t.Fatalf("Decode(ack) = %v, %v", in.Op, err)
	}
	for name, p := range map[string][]any{
		"short id":       {bytes.Repeat([]byte{1}, 15), uint64(0)},
		"text id":        {"0123456789abcdef", uint64(0)},
		"text status":    {bytes.Repeat([]byte{1}, 16), "0"},
		"three elements": {bytes.Repeat([]byte{1}, 16), uint64(0), uint64(0)},
	} {
		b := mustFrameBytes(t, []any{uint64(OpBotWebhookAck), uint64(0), nil, p})
		var fe *FrameError
		if _, err := Decode(b, 1<<16); !errors.As(err, &fe) || fe.Code != "E_FRAME_SHAPE" {
			t.Fatalf("%s: Decode = %v, want E_FRAME_SHAPE", name, err)
		}
	}
	// Op 66 is server-to-client only.
	in := mustFrameBytes(t, []any{uint64(OpBotWebhook), uint64(0), nil, []any{1, 2, 3, 4, 5, 6, 7, 8}})
	var fe *FrameError
	if _, err := Decode(in, 1<<16); !errors.As(err, &fe) || fe.Code != "E_FRAME_TYPE" {
		t.Fatalf("client op 66 = %v, want E_FRAME_TYPE", err)
	}
}

func mustFrameBytes(t *testing.T, v any) []byte {
	t.Helper()
	b, err := cborx.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
