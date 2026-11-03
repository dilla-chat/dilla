package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

func connectAs(t *testing.T, h *harness, device, user id.ID) *conn {
	t.Helper()
	sink := newRecordingSink(1024, false)
	c, err := h.gw.register(context.Background(), auth.Session{DeviceID: device, UserID: user, Scope: auth.ScopeEnrolled}, sink)
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.sinks[c] = sink
	h.mu.Unlock()
	return c
}

// A single authenticated caller must not turn rapid self-state toggles into
// unbounded SFU lookups and recipient frames.
func TestVoiceSelfFloodIsBoundedBeforeAudienceLookup(t *testing.T) {
	h := newHarness(t)
	sender, recipient := h.connect(t, id.New()), h.connect(t, id.New())
	h.drainReady(t, sender)
	h.drainReady(t, recipient)
	call, channel := id.New(), id.New()
	lookups := 0
	h.gw.opts.VoiceSelfAudience = func(context.Context, id.ID, id.ID) (id.ID, []id.ID, uint64, error) {
		lookups++
		return channel, []id.ID{recipient.userID}, 0, nil
	}
	for i := range 128 {
		h.gw.handle(context.Background(), sender, ephemeralInbound(t, OpVoiceSelf, call, uint64(i%4)))
		if got := h.waitFrame(t, recipient.deviceID); got.Op != OpVoiceState {
			t.Fatalf("frame %d op = %d", i, got.Op)
		}
	}
	h.gw.handle(context.Background(), sender, ephemeralInbound(t, OpVoiceSelf, call, uint64(0)))
	expectErrorCode(t, h, sender.deviceID, "E_RATE_LIMITED")
	noQueuedFrame(t, h, recipient)
	if lookups != 256 {
		t.Fatalf("audience lookups = %d, want 256 before rejected frame", lookups)
	}
}

func TestVoiceSelfSustainedSocketFloodIsBounded(t *testing.T) {
	h := newHarness(t)
	sender, recipient := h.connect(t, id.New()), h.connect(t, id.New())
	h.drainReady(t, sender)
	h.drainReady(t, recipient)
	call, channel := id.New(), id.New()
	lookups := 0
	h.gw.opts.VoiceSelfAudience = func(context.Context, id.ID, id.ID) (id.ID, []id.ID, uint64, error) {
		lookups++
		return channel, []id.ID{recipient.userID}, 0, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.gw.readLoop(ctx, sender, h.sink(sender)); close(done) }()
	defer func() { cancel(); <-done }()
	relays, refused := 0, 0
	for i := range 200 {
		// 20 frames/s stays below readLoop's 60 frames/s connection meter.
		h.clk.Advance(50 * time.Millisecond)
		p, err := cborx.Marshal([]any{call, uint64(i % 4)})
		if err != nil {
			t.Fatal(err)
		}
		h.sink(sender).feed(mustEncode(t, Frame{Op: OpVoiceSelf, Payload: cbor.RawMessage(p)}, 0))
		select {
		case raw := <-h.sink(recipient).frames:
			got, err := decodeAny(raw)
			if err != nil || got.Op != OpVoiceState {
				t.Fatalf("relay %d: op %d, err %v", i, got.Op, err)
			}
			relays++
		case raw := <-h.sink(sender).frames:
			got, err := decodeAny(raw)
			if err != nil || got.Op != OpError {
				t.Fatalf("refusal %d: op %d, err %v", i, got.Op, err)
			}
			code, err := rawText(got.Payload[1])
			if err != nil || code != "E_RATE_LIMITED" {
				t.Fatalf("refusal %d: code %q, err %v", i, code, err)
			}
			refused++
		case <-time.After(2 * time.Second):
			t.Fatalf("no outcome for frame %d", i)
		}
	}
	if relays > 140 || refused == 0 || lookups != 2*relays {
		t.Fatalf("relays=%d refused=%d lookups=%d", relays, refused, lookups)
	}
}

func TestTypingFloodChargesMeterBeforeAudienceWork(t *testing.T) {
	h := newHarness(t)
	sender := h.connect(t, id.New())
	h.drainReady(t, sender)
	channel := id.New()
	lookups := 0
	h.gw.opts.TypingAudience = func(context.Context, id.ID, id.ID) ([]id.ID, error) {
		lookups++
		return []id.ID{sender.userID}, nil
	}
	for range 128 {
		h.gw.handle(context.Background(), sender, ephemeralInbound(t, OpTypingSet, channel, uint64(1)))
		if lookups == 2 {
			if got := h.waitFrame(t, sender.deviceID); got.Op != OpTyping {
				t.Fatalf("first op = %d", got.Op)
			}
		} else {
			expectErrorCode(t, h, sender.deviceID, "E_RATE_LIMITED")
		}
	}
	before := lookups
	h.gw.handle(context.Background(), sender, ephemeralInbound(t, OpTypingSet, channel, uint64(1)))
	expectErrorCode(t, h, sender.deviceID, "E_RATE_LIMITED")
	if lookups != before {
		t.Fatalf("rejected frame did %d audience lookups", lookups-before)
	}
}

func TestTypingCooldownSurvivesLastConnectionRemoval(t *testing.T) {
	h := newHarness(t)
	sender, recipient := h.connect(t, id.New()), h.connect(t, id.New())
	h.drainReady(t, sender)
	h.drainReady(t, recipient)
	channel := id.New()
	h.gw.opts.TypingAudience = func(context.Context, id.ID, id.ID) ([]id.ID, error) {
		return []id.ID{recipient.userID}, nil
	}
	h.gw.handle(context.Background(), sender, ephemeralInbound(t, OpTypingSet, channel, uint64(1)))
	if got := h.waitFrame(t, recipient.deviceID); got.Op != OpTyping {
		t.Fatalf("first op = %d", got.Op)
	}
	h.gw.removeLive(sender)
	reconnected := connectAs(t, h, sender.deviceID, sender.userID)
	if got := h.readFrom(t, h.sink(reconnected)); got.Op != OpReady {
		t.Fatalf("reconnected first op = %d", got.Op)
	}
	h.gw.handle(context.Background(), reconnected, ephemeralInbound(t, OpTypingSet, channel, uint64(1)))
	noQueuedFrame(t, h, recipient)
	got := h.readFrom(t, h.sink(reconnected))
	if got.Op != OpError {
		t.Fatalf("reconnected op = %d, want rate refusal", got.Op)
	}
	if code, err := rawText(got.Payload[1]); err != nil || code != "E_RATE_LIMITED" {
		t.Fatalf("reconnected code = %q, err = %v", code, err)
	}
}

func TestSignalFloodAcrossSessionsOfOneUserSharesBudget(t *testing.T) {
	h := newHarness(t)
	first, recipient := h.connect(t, id.New()), h.connect(t, id.New())
	second := connectAs(t, h, id.New(), first.userID)
	for _, c := range []*conn{first, second, recipient} {
		h.drainReady(t, c)
	}
	call, channel := id.New(), id.New()
	lookups := 0
	h.gw.opts.VoiceSelfAudience = func(context.Context, id.ID, id.ID) (id.ID, []id.ID, uint64, error) {
		lookups++
		return channel, []id.ID{recipient.userID}, 0, nil
	}
	for i := range 128 {
		sender := first
		if i%2 == 1 {
			sender = second
		}
		h.gw.handle(context.Background(), sender, ephemeralInbound(t, OpVoiceSelf, call, uint64(i%4)))
		if got := h.waitFrame(t, recipient.deviceID); got.Op != OpVoiceState {
			t.Fatalf("frame %d op = %d", i, got.Op)
		}
	}
	h.gw.handle(context.Background(), second, ephemeralInbound(t, OpVoiceSelf, call, uint64(0)))
	expectErrorCode(t, h, second.deviceID, "E_RATE_LIMITED")
	noQueuedFrame(t, h, recipient)
	if lookups != 256 {
		t.Fatalf("audience lookups = %d, want 256 before rejected frame", lookups)
	}
}

func TestIdleSignalBudgetsAndDisconnectedCooldownsExpire(t *testing.T) {
	h := newHarness(t)
	user, device, channel := id.New(), id.New(), id.New()
	if !h.gw.presence.chargeSignal(user) || !h.gw.acceptTyping(channel, user, device, true) {
		t.Fatal("initial signal was refused")
	}
	h.gw.presence.clearDeviceTyping(device)
	h.clk.Advance(4 * time.Minute)
	h.gw.presence.chargeSignal(id.New())
	h.gw.presence.mu.Lock()
	defer h.gw.presence.mu.Unlock()
	if len(h.gw.presence.cooldown) != 0 {
		t.Fatalf("expired cooldowns = %d", len(h.gw.presence.cooldown))
	}
	if _, ok := h.gw.presence.signalBudget[user]; ok {
		t.Fatal("idle user's budget was retained")
	}
}
