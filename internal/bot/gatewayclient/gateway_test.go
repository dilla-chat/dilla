package gatewayclient

import (
	"errors"
	"testing"

	"github.com/dilla-chat/dilla/internal/cborx"
)

func TestDecodeServerFrameValidatesOpcodeShapeAndScope(t *testing.T) {
	instance := make([]byte, 16)
	instance[0] = 1
	validHello, err := cborx.Marshal([]any{uint64(OpHello), uint64(0), nil, []any{
		[]any{uint64(6)}, []any{uint64(1)}, []any{uint64(1)}, uint64(30000), uint64(131584), instance, uint64(1), uint64(300), uint64(300),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeServerFrame(validHello, MaxGatewayFrameBytes); err != nil {
		t.Fatalf("valid hello rejected: %v", err)
	}
	tests := []struct {
		name  string
		frame []any
	}{
		{"unknown opcode", []any{uint64(13), uint64(0), nil, []any{}}},
		{"wrong arity", []any{uint64(OpHeartbeatAck), uint64(0), nil, []any{uint64(1), uint64(2)}}},
		{"wrong scope", []any{uint64(OpHeartbeatAck), uint64(0), make([]byte, 16), []any{uint64(1)}}},
		{"server rejects client opcode", []any{uint64(OpIdentify), uint64(0), nil, []any{"", uint64(6), uint64(1), uint64(1), uint64(0)}}},
		{"heartbeat ack requires uint", []any{uint64(OpHeartbeatAck), uint64(0), nil, []any{"1"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := cborx.Marshal(tc.frame)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeServerFrame(b, MaxGatewayFrameBytes); err == nil {
				t.Fatal("malformed server frame accepted")
			}
		})
	}
}

func TestDecodeAcceptsConfiguredCiphertextAboveDefaultFrameSize(t *testing.T) {
	group, uploader := make([]byte, 16), make([]byte, 16)
	group[0], uploader[0] = 1, 2
	blob := make([]byte, 200<<10)
	raw, err := cborx.Marshal([]any{uint64(OpMessageCT), uint64(1), group, []any{uint64(1), uint64(0), uploader, blob, make([]byte, 32), uint64(1), nil}})
	if err != nil {
		t.Fatal(err)
	}
	if uint64(len(raw)) <= 131584 {
		t.Fatalf("fixture frame is only %d bytes", len(raw))
	}
	if _, err := DecodeServerFrame(raw, MaxGatewayFrameBytes); err != nil {
		t.Fatalf("configured large frame rejected: %v", err)
	}
	if _, err := DecodeServerFrame(raw, 131584); err == nil {
		t.Fatal("frame above advertised limit accepted")
	}
}

func TestAckRejectsSocketReceiptBeforeTheNextDurableFrame(t *testing.T) {
	c := &Client{streamID: 1, state: resumeState{}, ackAvailable: map[uint64][32]byte{1: {}, 2: {}}}
	first := Delivery{Frame: Frame{N: 1}, client: c, stream: 1, ackN: 1}
	second := Delivery{Frame: Frame{N: 2}, client: c, stream: 1, ackN: 2}
	if err := second.Ack(); !errors.Is(err, ErrAckOrder) {
		t.Fatalf("out-of-order ack error=%v", err)
	}
	if err := first.Ack(); err != nil {
		t.Fatalf("ack first durable frame: %v", err)
	}
	if err := second.Ack(); err != nil {
		t.Fatalf("ack second durable frame: %v", err)
	}
}

func TestStaleDeliveryCannotAckFreshStreamWithReusedN(t *testing.T) {
	c := &Client{streamID: 2, state: resumeState{}, ackAvailable: map[uint64][32]byte{1: {}}}
	old := Delivery{Frame: Frame{N: 1}, client: c, stream: 1, ackN: 1}
	if err := old.Ack(); !errors.Is(err, ErrStaleDelivery) {
		t.Fatalf("stale delivery ack error=%v", err)
	}
}

func TestUnackedDeliveryLimitIncludesItemsAlreadyReadByConsumer(t *testing.T) {
	c := &Client{events: make(chan Delivery, 1), streamID: 1, ackAvailable: make(map[uint64][32]byte)}
	queued, err := c.enqueueEvent(Frame{Op: OpMessagePlain, N: 1}, []byte{1})
	if err != nil || !queued {
		t.Fatalf("enqueue first delivery = %t, %v", queued, err)
	}
	first := <-c.events // consumer owns it, but has not durably handled it
	if queued, err = c.enqueueEvent(Frame{Op: OpMessagePlain, N: 2}, []byte{2}); !errors.Is(err, ErrBackpressure) || queued {
		t.Fatalf("second unacked delivery = %t, %v, want bounded backpressure", queued, err)
	}
	if err := first.Ack(); err != nil {
		t.Fatalf("ack first delivery: %v", err)
	}
	if queued, err = c.enqueueEvent(Frame{Op: OpMessagePlain, N: 2}, []byte{2}); err != nil || !queued {
		t.Fatalf("enqueue after durable ack = %t, %v", queued, err)
	}
}

func TestIdenticalPendingReplayIsDeduplicated(t *testing.T) {
	c := &Client{events: make(chan Delivery, 2), streamID: 1, ackAvailable: make(map[uint64][32]byte)}
	frame := Frame{Op: OpMessagePlain, N: 1}
	if queued, err := c.enqueueEvent(frame, []byte("same-frame")); err != nil || !queued {
		t.Fatalf("initial enqueue = %t, %v", queued, err)
	}
	<-c.events
	if queued, err := c.enqueueEvent(frame, []byte("same-frame")); err != nil || queued {
		t.Fatalf("duplicate replay enqueue = %t, %v, want skipped", queued, err)
	}
	if len(c.events) != 0 {
		t.Fatalf("duplicate replay queued %d deliveries", len(c.events))
	}
}

func TestServerErrorCodesAreClassifiedWithoutFatalHintsBeingQueued(t *testing.T) {
	tests := []struct {
		code      string
		handshake bool
		want      error
	}{
		{"E_VERSION", true, ErrVersion},
		{"E_UNAUTHENTICATED", true, ErrTerminal},
		{"E_FRAME_SHAPE", true, ErrProtocol},
		{"E_NOT_FOUND", true, ErrTerminal},
		{"E_VERSION", false, ErrVersion},
		{"E_UNAUTHENTICATED", false, ErrTerminal},
		{"E_FRAME_TYPE", false, ErrProtocol},
		{"E_NOT_FOUND", false, nil},
		{"E_RATE_LIMITED", false, nil},
		{"E_PRIVATE_UNKNOWN", false, ErrProtocol},
	}
	for _, tc := range tests {
		t.Run(tc.code+map[bool]string{true: "/handshake", false: "/live"}[tc.handshake], func(t *testing.T) {
			if got := classifyServerError(tc.code, tc.handshake); !errors.Is(got, tc.want) {
				t.Fatalf("classifyServerError(%q, %t)=%v, want %v", tc.code, tc.handshake, got, tc.want)
			}
		})
	}
}
