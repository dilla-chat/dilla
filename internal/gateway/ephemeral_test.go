// Package gateway tests ephemeral signal admission and relay through decoded frames.
package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

func ephemeralInbound(t *testing.T, op Op, payload ...any) Inbound {
	t.Helper()
	p, err := cborx.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Encode(Frame{Op: op, Payload: cbor.RawMessage(p)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	in, err := Decode(b, 16384)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func noQueuedFrame(t *testing.T, h *harness, c *conn) {
	t.Helper()
	select {
	case raw := <-h.sink(c).frames:
		in, err := decodeAny(raw)
		if err != nil {
			t.Fatal(err)
		}
		t.Fatalf("unexpected op %d", in.Op)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestTypingSetIsChannelBoundAndNotReplayed(t *testing.T) {
	h := newHarness(t)
	a, b, outsider := h.connect(t, id.New()), h.connect(t, id.New()), h.connect(t, id.New())
	for _, c := range []*conn{a, b, outsider} {
		h.drainReady(t, c)
	}
	channel := id.New()
	h.gw.opts.TypingAudience = func(_ context.Context, device, got id.ID) ([]id.ID, error) {
		if got != channel || device == outsider.deviceID {
			return nil, nil
		}
		return []id.ID{a.userID, b.userID}, nil
	}
	before := len(b.ring.since(0))
	h.gw.handle(context.Background(), a, ephemeralInbound(t, OpTypingSet, channel, uint64(1)))
	got := h.waitFrame(t, b.deviceID)
	if got.Op != OpTyping || got.CID != 0 || len(got.Payload) != 5 {
		t.Fatalf("typing frame = %+v", got)
	}
	var ch, user, device id.ID
	if err := cborx.Unmarshal(got.Payload[0], &ch); err != nil {
		t.Fatal(err)
	}
	if err := cborx.Unmarshal(got.Payload[1], &user); err != nil {
		t.Fatal(err)
	}
	if err := cborx.Unmarshal(got.Payload[2], &device); err != nil {
		t.Fatal(err)
	}
	if ch != channel || user != a.userID || device != a.deviceID {
		t.Fatalf("identity/channel = %s %s %s", ch, user, device)
	}
	if len(b.ring.since(0)) != before {
		t.Fatal("ephemeral entered replay ring")
	}
	noQueuedFrame(t, h, outsider)
}

func TestTypingSetRefusesHiddenChannelBeforeFanout(t *testing.T) {
	h := newHarness(t)
	member, outsider := h.connect(t, id.New()), h.connect(t, id.New())
	h.drainReady(t, member)
	h.drainReady(t, outsider)
	channel := id.New()
	h.gw.opts.TypingAudience = func(_ context.Context, device, got id.ID) ([]id.ID, error) {
		if device != member.deviceID || got != channel {
			return nil, nil
		}
		return []id.ID{member.userID}, nil
	}
	h.gw.handle(context.Background(), outsider, ephemeralInbound(t, OpTypingSet, channel, uint64(1)))
	expectErrorCode(t, h, outsider.deviceID, "E_NOT_FOUND")
	noQueuedFrame(t, h, member)
}

func TestTypingSetThrottlesPerDeviceChannelAndExpires(t *testing.T) {
	h := newHarness(t)
	a := h.connect(t, id.New())
	h.drainReady(t, a)
	one, two := id.New(), id.New()
	h.gw.opts.TypingAudience = func(_ context.Context, _ id.ID, _ id.ID) ([]id.ID, error) { return []id.ID{a.userID}, nil }
	h.gw.handle(context.Background(), a, ephemeralInbound(t, OpTypingSet, one, uint64(1)))
	if got := h.waitFrame(t, a.deviceID); got.Op != OpTyping {
		t.Fatalf("op = %d", got.Op)
	}
	h.gw.handle(context.Background(), a, ephemeralInbound(t, OpTypingSet, one, uint64(1)))
	expectErrorCode(t, h, a.deviceID, "E_RATE_LIMITED")
	h.gw.handle(context.Background(), a, ephemeralInbound(t, OpTypingSet, two, uint64(1)))
	if got := h.waitFrame(t, a.deviceID); got.Op != OpTyping {
		t.Fatalf("op = %d", got.Op)
	}
	if !h.gw.IsTyping(one, a.userID, a.deviceID) {
		t.Fatal("one is not live")
	}
	h.clk.Advance(11 * time.Second)
	if h.gw.IsTyping(one, a.userID, a.deviceID) {
		t.Fatal("one survived TTL")
	}
}

func TestTypingSetOffDoesNotResetOnThrottle(t *testing.T) {
	h := newHarness(t)
	a := h.connect(t, id.New())
	h.drainReady(t, a)
	channel := id.New()
	h.gw.opts.TypingAudience = func(_ context.Context, _ id.ID, _ id.ID) ([]id.ID, error) {
		return []id.ID{a.userID}, nil
	}
	h.gw.handle(context.Background(), a, ephemeralInbound(t, OpTypingSet, channel, uint64(1)))
	if got := h.waitFrame(t, a.deviceID); got.Op != OpTyping {
		t.Fatalf("on op = %d", got.Op)
	}
	h.gw.handle(context.Background(), a, ephemeralInbound(t, OpTypingSet, channel, uint64(0)))
	if got := h.waitFrame(t, a.deviceID); got.Op != OpTyping {
		t.Fatalf("off op = %d", got.Op)
	}
	if h.gw.IsTyping(channel, a.userID, a.deviceID) {
		t.Fatal("off left typing live")
	}
	h.gw.handle(context.Background(), a, ephemeralInbound(t, OpTypingSet, channel, uint64(1)))
	expectErrorCode(t, h, a.deviceID, "E_RATE_LIMITED")
	if h.gw.IsTyping(channel, a.userID, a.deviceID) {
		t.Fatal("rejected on became live")
	}
	h.clk.Advance(3 * time.Second)
	h.gw.handle(context.Background(), a, ephemeralInbound(t, OpTypingSet, channel, uint64(1)))
	if got := h.waitFrame(t, a.deviceID); got.Op != OpTyping {
		t.Fatalf("on after interval op = %d", got.Op)
	}
}

func TestTypingRecentHistoryIsBoundedAndClearedOnDisconnect(t *testing.T) {
	h := newHarness(t)
	a := h.connect(t, id.New())
	h.drainReady(t, a)
	h.gw.opts.TypingAudience = func(_ context.Context, _ id.ID, _ id.ID) ([]id.ID, error) {
		return []id.ID{a.userID}, nil
	}
	for range 64 {
		channel := id.New()
		h.gw.handle(context.Background(), a, ephemeralInbound(t, OpTypingSet, channel, uint64(1)))
		if got := h.waitFrame(t, a.deviceID); got.Op != OpTyping {
			t.Fatalf("on op = %d", got.Op)
		}
		h.gw.handle(context.Background(), a, ephemeralInbound(t, OpTypingSet, channel, uint64(0)))
		if got := h.waitFrame(t, a.deviceID); got.Op != OpTyping {
			t.Fatalf("off op = %d", got.Op)
		}
	}
	h.gw.handle(context.Background(), a, ephemeralInbound(t, OpTypingSet, id.New(), uint64(1)))
	expectErrorCode(t, h, a.deviceID, "E_RATE_LIMITED")
	h.clk.Advance(3 * time.Second)
	channel := id.New()
	h.gw.handle(context.Background(), a, ephemeralInbound(t, OpTypingSet, channel, uint64(1)))
	if got := h.waitFrame(t, a.deviceID); got.Op != OpTyping {
		t.Fatalf("on after history expiry op = %d", got.Op)
	}
	stopped := id.New()
	h.gw.handle(context.Background(), a, ephemeralInbound(t, OpTypingSet, stopped, uint64(1)))
	if got := h.waitFrame(t, a.deviceID); got.Op != OpTyping {
		t.Fatalf("second on op = %d", got.Op)
	}
	h.gw.handle(context.Background(), a, ephemeralInbound(t, OpTypingSet, stopped, uint64(0)))
	if got := h.waitFrame(t, a.deviceID); got.Op != OpTyping {
		t.Fatalf("second off op = %d", got.Op)
	}
	h.gw.removeLive(a)
	if h.gw.IsTyping(channel, a.userID, a.deviceID) {
		t.Fatal("disconnect retained live typing")
	}
	h.gw.presence.mu.Lock()
	defer h.gw.presence.mu.Unlock()
	for key := range h.gw.presence.recentTyping {
		if key[1] == a.deviceID {
			t.Fatal("disconnect retained recent typing history")
		}
	}
}

func TestVoiceSelfRequiresLiveCallAndValidFlags(t *testing.T) {
	h := newHarness(t)
	a := h.connect(t, id.New())
	h.drainReady(t, a)
	b := h.connect(t, id.New())
	h.drainReady(t, b)
	call, channel := id.New(), id.New()
	callbackCalls := 0
	h.gw.opts.VoiceSelfAudience = func(_ context.Context, _ id.ID, got id.ID) (id.ID, []id.ID, uint64, error) {
		callbackCalls++
		if got != call {
			return id.ID{}, nil, 0, nil
		}
		return channel, []id.ID{a.userID, b.userID}, 0, nil
	}
	p, err := cborx.Marshal([]any{call, uint64(4)})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Encode(Frame{Op: OpVoiceSelf, Payload: cbor.RawMessage(p)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { h.gw.readLoop(ctx, a, h.sink(a)); close(done) }()
	h.sink(a).feed(raw)
	expectErrorCode(t, h, a.deviceID, "E_FRAME_SHAPE")
	h.waitClose(t, a)
	<-done
	noQueuedFrame(t, h, a)
	noQueuedFrame(t, h, b)
	if callbackCalls != 0 {
		t.Fatalf("invalid frame dispatched to audience callback %d times", callbackCalls)
	}
	if len(a.ring.since(0)) != 1 {
		t.Fatal("invalid frame entered replay ring")
	}
	h.gw.handle(context.Background(), b, ephemeralInbound(t, OpVoiceSelf, id.New(), uint64(1)))
	expectErrorCode(t, h, b.deviceID, "E_NOT_FOUND")
	noQueuedFrame(t, h, b)
}

func TestVoiceSelfRefusesDisconnectedParticipantWithoutRelay(t *testing.T) {
	h := newHarness(t)
	sender, recipient := h.connect(t, id.New()), h.connect(t, id.New())
	h.drainReady(t, sender)
	h.drainReady(t, recipient)
	call := id.New()
	h.gw.opts.VoiceSelfAudience = func(_ context.Context, device, got id.ID) (id.ID, []id.ID, uint64, error) {
		if device != sender.deviceID || got != call {
			t.Fatalf("audience called for device %s, call %s", device, got)
		}
		// The API refuses an SFU snapshot row whose state is DISCONNECTED.
		return id.ID{}, nil, 0, errors.New("E_NOT_FOUND")
	}
	h.gw.handle(context.Background(), sender, ephemeralInbound(t, OpVoiceSelf, call, uint64(1)))
	expectErrorCode(t, h, sender.deviceID, "E_NOT_FOUND")
	noQueuedFrame(t, h, recipient)
}

func TestVoiceSelfCombinesLiveRoomAndSelfBits(t *testing.T) {
	for _, tc := range []struct {
		name  string
		bits  uint64
		flags uint64
	}{
		{name: "neither", bits: 0, flags: 1},
		{name: "mute", bits: 1, flags: 3},
		{name: "deaf", bits: 2, flags: 5},
		{name: "both", bits: 3, flags: 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			a, b := h.connect(t, id.New()), h.connect(t, id.New())
			h.drainReady(t, a)
			h.drainReady(t, b)
			call, channel := id.New(), id.New()
			h.gw.opts.VoiceSelfAudience = func(_ context.Context, device, got id.ID) (id.ID, []id.ID, uint64, error) {
				if device != a.deviceID || got != call {
					return id.ID{}, nil, 0, nil
				}
				return channel, []id.ID{b.userID}, 0, nil
			}
			// web-3 task 3: the admitted bits are handed to the store the voice snapshot reads.
			var stored [][3]any
			h.gw.opts.VoiceSelfStored = func(gotCall, dev id.ID, bits uint64) {
				stored = append(stored, [3]any{gotCall, dev, bits})
			}
			before := len(b.ring.since(0))
			h.gw.handle(context.Background(), a, ephemeralInbound(t, OpVoiceSelf, call, tc.bits))
			if len(stored) != 1 || stored[0] != [3]any{call, a.deviceID, tc.bits} {
				t.Fatalf("stored self flags = %v, want one (%s, %s, %d)", stored, call, a.deviceID, tc.bits)
			}
			frame := h.waitFrame(t, b.deviceID)
			if frame.Op != OpVoiceState || frame.CID != 0 || len(frame.Payload) != 4 {
				t.Fatalf("voice frame = %+v", frame)
			}
			var user, device, gotCall id.ID
			if err := cborx.Unmarshal(frame.Payload[0], &user); err != nil {
				t.Fatal(err)
			}
			if err := cborx.Unmarshal(frame.Payload[1], &device); err != nil {
				t.Fatal(err)
			}
			if err := cborx.Unmarshal(frame.Payload[2], &gotCall); err != nil {
				t.Fatal(err)
			}
			flags, err := rawUint(frame.Payload[3])
			if err != nil {
				t.Fatal(err)
			}
			if user != a.userID || device != a.deviceID || gotCall != call || flags != tc.flags {
				t.Fatalf("voice identity/flags = %s %s %s %d, want %s %s %s %d", user, device, gotCall, flags, a.userID, a.deviceID, call, tc.flags)
			}
			if got := len(b.ring.since(0)); got != before {
				t.Fatalf("ephemeral entered replay ring: before %d, after %d", before, got)
			}
		})
	}
}

func TestVoiceSelfMuteKeepsVideoFromRoom(t *testing.T) {
	h := newHarness(t)
	a, b := h.connect(t, id.New()), h.connect(t, id.New())
	h.drainReady(t, a)
	h.drainReady(t, b)
	call, channel := id.New(), id.New()
	h.gw.opts.VoiceSelfAudience = func(_ context.Context, device, got id.ID) (id.ID, []id.ID, uint64, error) {
		if device != a.deviceID || got != call {
			return id.ID{}, nil, 0, nil
		}
		return channel, []id.ID{b.userID}, 1 << 3, nil
	}
	h.gw.handle(context.Background(), a, ephemeralInbound(t, OpVoiceSelf, call, uint64(1)))
	frame := h.waitFrame(t, b.deviceID)
	flags, err := rawUint(frame.Payload[3])
	if err != nil {
		t.Fatal(err)
	}
	if flags != 11 {
		t.Fatalf("flags = %d, want in-call|mute|video (11)", flags)
	}
}

func TestTypingCapAndOffAreScopedToOneDeviceAndChannel(t *testing.T) {
	h := newHarness(t)
	a := h.connect(t, id.New())
	h.drainReady(t, a)
	h.gw.opts.TypingAudience = func(_ context.Context, _ id.ID, _ id.ID) ([]id.ID, error) { return []id.ID{a.userID}, nil }
	ids := make([]id.ID, 65)
	for i := range ids {
		ids[i] = id.New()
	}
	for _, ch := range ids[:64] {
		h.gw.handle(context.Background(), a, ephemeralInbound(t, OpTypingSet, ch, uint64(1)))
		if got := h.waitFrame(t, a.deviceID); got.Op != OpTyping {
			t.Fatalf("op = %d", got.Op)
		}
	}
	h.gw.handle(context.Background(), a, ephemeralInbound(t, OpTypingSet, ids[64], uint64(1)))
	expectErrorCode(t, h, a.deviceID, "E_RATE_LIMITED")
	h.gw.handle(context.Background(), a, ephemeralInbound(t, OpTypingSet, ids[0], uint64(0)))
	if got := h.waitFrame(t, a.deviceID); got.Op != OpTyping {
		t.Fatalf("off op = %d", got.Op)
	}
	if h.gw.IsTyping(ids[0], a.userID, a.deviceID) {
		t.Fatal("off left the channel live")
	}
	if !h.gw.IsTyping(ids[1], a.userID, a.deviceID) {
		t.Fatal("off cleared a different channel")
	}
	h.gw.handle(context.Background(), a, ephemeralInbound(t, OpTypingSet, ids[64], uint64(1)))
	if got := h.waitFrame(t, a.deviceID); got.Op != OpTyping {
		t.Fatalf("replacement op = %d", got.Op)
	}
}

func TestTypingAudienceIsReadAgainBeforeDelivery(t *testing.T) {
	h := newHarness(t)
	a, b := h.connect(t, id.New()), h.connect(t, id.New())
	h.drainReady(t, a)
	h.drainReady(t, b)
	channel := id.New()
	calls := 0
	h.gw.opts.TypingAudience = func(_ context.Context, _ id.ID, _ id.ID) ([]id.ID, error) {
		calls++
		if calls == 1 {
			return []id.ID{a.userID, b.userID}, nil
		}
		return []id.ID{a.userID}, nil
	}
	h.gw.handle(context.Background(), a, ephemeralInbound(t, OpTypingSet, channel, uint64(1)))
	if calls != 2 {
		t.Fatalf("audience calls = %d, want admission and fan-out", calls)
	}
	noQueuedFrame(t, h, b)
}
