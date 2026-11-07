package gateway

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// Bot v2 Task 13: an incoming webhook (op 66) reaches only a live Bot runtime connection, unringed, and
// the POST waits for that connection's op 67 ack.

const webhookTestText = "the boss has been defeated by Ada"

func webhookPayload(t *testing.T, delivery id.ID) cbor.RawMessage {
	t.Helper()
	avatar := bytes.Repeat([]byte{0xa7}, 32)
	p, err := BotWebhookPayload(delivery, id.New(), id.New(), id.New(), "Terraria", avatar, webhookTestText, false)
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

// Fix round 1 I-1: one Bot device holds at most BotWebhookPendingPerDevice waiting deliveries. The next is busy
// before anything is sent, and another device still delivers.
func TestWebhookPendingIsCappedPerBotDevice(t *testing.T) {
	h := newHarness(t)
	busyDevice, otherDevice := id.New(), id.New()
	busyBot, _ := h.finishedBot(t, busyDevice)
	otherBot, _ := h.finishedBot(t, otherDevice)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	results := make([]<-chan webhookResult, 0, BotWebhookPendingPerDevice)
	for range BotWebhookPendingPerDevice {
		d := id.New()
		results = append(results, deliverAsync(h, ctx, busyDevice, d, webhookPayload(t, d)))
	}
	for range BotWebhookPendingPerDevice {
		if in := h.waitFrame(t, busyDevice); in.Op != OpBotWebhook {
			t.Fatalf("op %d, want bot_webhook", in.Op)
		}
	}
	if _, err := h.gw.DeliverBotWebhook(ctx, busyDevice, id.New(), webhookPayload(t, id.New())); !errors.Is(err, ErrBotWebhookBusy) {
		t.Fatalf("delivery %d to one device = %v, want ErrBotWebhookBusy", BotWebhookPendingPerDevice+1, err)
	}
	other := id.New()
	res := deliverAsync(h, ctx, otherDevice, other, webhookPayload(t, other))
	if in := h.waitFrame(t, otherDevice); in.Op != OpBotWebhook {
		t.Fatalf("other device got op %d", in.Op)
	}
	h.gw.handle(context.Background(), otherBot, ackInbound(t, other[:], 0))
	if r := <-res; r.err != nil {
		t.Fatalf("the other device's delivery = %v", r.err)
	}
	cancel()
	for _, r := range results {
		<-r
	}
	_ = busyBot
	if n := h.gw.webhooks.len(); n != 0 {
		t.Fatalf("%d pending after all returned", n)
	}
}

// Fix round 1 I-1: a webhook never fills the Bot's writer queue past half; past that it is busy and nothing is sent,
// so it can never be the frame that closes the Bot's socket for overflow.
func TestWebhookLeavesHalfTheBotQueueFree(t *testing.T) {
	h := newHarness(t)
	h.gw.opts.BotQueueFrames = 4
	device := id.New()
	bot, _ := h.finishedBot(t, device)
	h.stall(bot)
	for i := range 3 {
		if !h.gw.DeliverBot(device, botEventFrame(t, BotEventMember, uint64(i))) {
			t.Fatal("feed event not taken")
		}
	}
	if _, err := h.gw.DeliverBotWebhook(context.Background(), device, id.New(), webhookPayload(t, id.New())); !errors.Is(err, ErrBotWebhookBusy) {
		t.Fatalf("webhook into a half-full Bot queue = %v, want ErrBotWebhookBusy", err)
	}
	h.release(bot)
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

// TestWebhookAcksFromTheBotAreNotChargedToTheInboundFrameBucket (Bot v2 task 14): every op 66 the Bot is sent asks for
// one op 67 back within 2 s, so a burst of webhook posts must not close the Bot's connection 4008. Op 67 rides outside
// the inbound bucket on a Bot connection, as an admitted op 59 request does; on a person's connection it is charged.
func TestWebhookAcksFromTheBotAreNotChargedToTheInboundFrameBucket(t *testing.T) {
	for _, tc := range []struct {
		name   string
		bot    bool
		closed bool
	}{{"bot", true, false}, {"person", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.gw.opts.WireVersions = []uint64{auth.WireVersion}
			h.gw.opts.Auth = &capabilityRecordingAuth{Authenticator: h}
			device := id.New()
			session := auth.Session{Scope: auth.ScopeEnrolled, WireVersion: auth.WireVersion, TokenHash: tokenHash()}
			if tc.bot {
				session.Bot = &auth.BotSessionBinding{InstallationID: id.New(), CommunityID: id.New(), State: store.BotInstallationReady}
			}
			token := scopedSession(h, device, session)
			s := newRecordingSink(512, false)
			go h.gw.serve(context.Background(), s, token)
			h.readFrom(t, s) // hello
			p, err := payload(token, auth.WireVersion, uint64(1), uint64(1), uint64(0))
			if err != nil {
				t.Fatal(err)
			}
			s.feed(mustEncode(t, Frame{Op: OpIdentify, Payload: p, Replay: true}, 1))
			select {
			case b := <-s.frames:
				if in, err := decodeAny(b); err != nil || in.Op != OpReady {
					t.Fatalf("op %d (%v), want ready", in.Op, err)
				}
			case code := <-s.closed:
				t.Fatalf("identify closed %d", code)
			case <-time.After(2 * time.Second):
				t.Fatal("no ready")
			}
			frames := make([][]byte, 0, h.gw.opts.FrameBurst*2)
			for range h.gw.opts.FrameBurst * 2 {
				delivery := id.New()
				ack, err := payload(delivery[:], uint64(0))
				if err != nil {
					t.Fatal(err)
				}
				frames = append(frames, mustEncode(t, Frame{Op: OpBotWebhookAck, Payload: ack}, 0))
			}
			// A closed connection stops reading, so the feeder must not block the test.
			go func() {
				for _, f := range frames {
					select {
					case s.reads <- f:
					case <-time.After(time.Second):
						return
					}
				}
			}()
			select {
			case code := <-s.closed:
				if !tc.closed {
					t.Fatalf("the Bot's connection was closed %d for its webhook acks", code)
				}
				if code != CloseRateLimited {
					t.Fatalf("close code = %d, want 4008", code)
				}
			case <-time.After(500 * time.Millisecond):
				if tc.closed {
					t.Fatal("a person's connection sent twice the burst of op 67 frames and stayed open")
				}
			}
		})
	}
}
