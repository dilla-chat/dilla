package gatewayclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
)

func TestRunResumesThroughServerReplayBarrierAndRotatesToken(t *testing.T) {
	instance, device, user := testID(1), testID(2), testID(3)
	drop := []chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	var upgrades atomic.Int32
	seenOps := make(chan testClientFrame, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(upgrades.Add(1))
		if got := r.Header.Get("Authorization"); got != fmt.Sprintf("Bearer session-%d", n) {
			t.Errorf("upgrade credential = %q", got)
		}
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"dilla.v1"}, CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer ws.Close(websocket.StatusNormalClosure, "done")
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := sendServerFrame(ctx, ws, OpHello, 0, nil, helloPayload(instance, 7)); err != nil {
			t.Errorf("hello: %v", err)
			return
		}
		_, b, err := ws.Read(ctx)
		if err != nil {
			t.Errorf("read handshake: %v", err)
			return
		}
		frame, err := decodeClientTestFrame(b)
		if err != nil {
			t.Errorf("decode handshake: %v", err)
			return
		}
		if n == 1 {
			if frame.Op != OpIdentify {
				t.Errorf("first opcode=%d, want identify", frame.Op)
				return
			}
			if err := sendServerFrame(ctx, ws, OpReady, 1, nil, readyPayload(device, user, 7, bytes32(10))); err != nil {
				t.Errorf("ready: %v", err)
				return
			}
		} else {
			if frame.Op != OpResume {
				t.Errorf("connection %d opcode=%d, want resume", n, frame.Op)
				return
			}
			wantAck := uint64(1)
			if n == 3 {
				wantAck = 4
			}
			if frame.Payload[3] != wantAck {
				t.Errorf("resume last_n=%v, want %d", frame.Payload[3], wantAck)
				return
			}
			wantToken := bytes32(10)
			if n == 3 {
				wantToken = bytes32(12)
			}
			if !bytesEqual(frame.Payload[1].([]byte), wantToken) {
				t.Errorf("resume token did not rotate at reconnect %d", n)
				return
			}
			barrier := uint64(3)
			from, to := uint64(2), uint64(2)
			if n == 3 {
				barrier = 5
				from, to = 5, 4
			}
			if err := sendServerFrame(ctx, ws, OpResumed, barrier, nil, []any{from, to, bytes32(uint8(10 + n))}); err != nil {
				t.Errorf("resumed: %v", err)
				return
			}
			if n == 2 {
				if err := sendServerFrame(ctx, ws, OpMessagePlain, 2, nil, []any{device, uint64(1), user, []byte{1}, bytes32(12), uint64(0), uint64(0)}); err != nil {
					t.Errorf("replay message: %v", err)
					return
				}
				if err := sendServerFrame(ctx, ws, OpMessagePlain, 4, nil, []any{device, uint64(2), user, []byte{2}, bytes32(12), uint64(0), uint64(0)}); err != nil {
					t.Errorf("message: %v", err)
					return
				}
			}
		}
		inbound := make(chan []byte, 8)
		go func() {
			for {
				_, data, e := ws.Read(ctx)
				if e != nil {
					return
				}
				select {
				case inbound <- data:
				case <-ctx.Done():
					return
				}
			}
		}()
		for {
			select {
			case <-drop[n-1]:
				return
			case b := <-inbound:
				f, e := decodeClientTestFrame(b)
				if e != nil {
					t.Errorf("decode client frame: %v", e)
					return
				}
				if f.Op == OpHeartbeat {
					if e := sendServerFrame(ctx, ws, OpHeartbeatAck, 0, nil, []any{uint64(100)}); e != nil {
						return
					}
				} else {
					select {
					case seenOps <- f:
					case <-ctx.Done():
						return
					}
				}
			case <-ctx.Done():
				return
			}
		}
	}))
	defer server.Close()
	instanceID, _ := id.Parse(instance.String())
	httpClient, err := client.New(client.Config{BaseURL: server.URL, InstanceID: instanceID, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	gw, err := New(Config{Client: httpClient, AllowLoopbackHTTP: true, ExpectedDeviceID: device, ExpectedUserID: user, QueueSize: 8})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var creds atomic.Int32
	go func() {
		done <- gw.Run(ctx, func(context.Context) (string, error) { return fmt.Sprintf("session-%d", creds.Add(1)), nil })
	}()
	readEvent := func(want Opcode) Delivery {
		t.Helper()
		select {
		case d := <-gw.Events():
			if d.Frame.Op != want {
				t.Fatalf("event opcode=%d, want %d", d.Frame.Op, want)
			}
			return d
		case <-time.After(3 * time.Second):
			select {
			case err := <-done:
				t.Fatalf("Run exited while waiting for opcode %d: %v", want, err)
			default:
				t.Fatalf("timed out waiting for opcode %d", want)
			}
			return Delivery{}
		}
	}
	ready := readEvent(OpReady)
	if err := ready.Ack(); err != nil {
		t.Fatalf("ack ready: %v", err)
	}
	close(drop[0])
	replayed := readEvent(OpMessagePlain)
	if replayed.Frame.N != 2 {
		t.Fatalf("first resumed data frame n=%d, want replay n=2 before barrier", replayed.Frame.N)
	}
	if err := replayed.Ack(); err != nil {
		t.Fatalf("ack replay: %v", err)
	}
	resumed := readEvent(OpResumed)
	if resumed.Frame.N != 3 {
		t.Fatalf("resume barrier n=%d, want 3", resumed.Frame.N)
	}
	if err := resumed.Ack(); err != nil {
		t.Fatalf("ack replay barrier: %v", err)
	}
	message := readEvent(OpMessagePlain)
	if message.Frame.N != 4 {
		t.Fatalf("live frame n=%d, want 4 after barrier", message.Frame.N)
	}
	if message.Frame.MessagePlain == nil || message.Frame.MessagePlain.Seq != 2 {
		t.Fatalf("decoded message=%+v", message.Frame.MessagePlain)
	}
	if err := message.Ack(); err != nil {
		t.Fatalf("ack message: %v", err)
	}
	close(drop[1])
	resumed = readEvent(OpResumed)
	if err := resumed.Ack(); err != nil {
		t.Fatalf("ack second resumed: %v", err)
	}
	group := testID(5)
	if err := gw.Subscribe(ctx, []id.ID{group}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := gw.CommitAck(ctx, group, 9); err != nil {
		t.Fatalf("commit ack: %v", err)
	}
	for _, want := range []Opcode{OpSubscribe, OpCommitAck} {
		select {
		case got := <-seenOps:
			if got.Op != want {
				t.Fatalf("outbound opcode=%d, want %d", got.Op, want)
			}
			if want == OpCommitAck {
				bytes, ok := got.GroupID.([]byte)
				if !ok || !bytesEqual(bytes, group[:]) {
					t.Fatalf("commit_ack group=%v, want %x", got.GroupID, group)
				}
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for outbound opcode %d", want)
		}
	}
	cancel()
	close(drop[2])
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop promptly")
	}
	if got := creds.Load(); got != 3 {
		t.Fatalf("credential callback calls=%d, want 3", got)
	}
}

func TestUnackedDeliveryIsDeduplicatedAcrossDisconnectDuringHandler(t *testing.T) {
	instance, device, user := testID(31), testID(32), testID(33)
	drop := []chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	replayReady, continueReplay := make(chan struct{}), make(chan struct{})
	var upgrades atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(upgrades.Add(1))
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"dilla.v1"}, CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			return
		}
		defer ws.Close(websocket.StatusNormalClosure, "test")
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		// A closed drop channel must interrupt a blocked read immediately. Merely
		// checking it around ws.Read makes disconnect timing depend on the next
		// heartbeat, which can outlast the test's replay synchronization bound.
		go func() {
			select {
			case <-drop[n-1]:
				_ = ws.CloseNow()
			case <-ctx.Done():
			}
		}()
		if err := sendServerFrame(ctx, ws, OpHello, 0, nil, helloPayload(instance, 7)); err != nil {
			t.Errorf("hello: %v", err)
			return
		}
		_, raw, err := ws.Read(ctx)
		if err != nil {
			return
		}
		handshake, err := decodeClientTestFrame(raw)
		if err != nil {
			t.Errorf("handshake: %v", err)
			return
		}
		if n == 1 {
			if handshake.Op != OpIdentify {
				t.Errorf("first handshake op=%d, want identify", handshake.Op)
				return
			}
			if err := sendServerFrame(ctx, ws, OpReady, 1, nil, readyPayload(device, user, 7, bytes32(40))); err != nil {
				return
			}
		} else {
			if handshake.Op != OpResume || handshake.Payload[3] != uint64(1) {
				t.Errorf("resume handshake=%+v, want ack=1", handshake)
				return
			}
			wantToken := bytes32(40)
			if n == 3 {
				wantToken = bytes32(42) // rotated by the first resumed, even while replay was unacked
			}
			if !bytesEqual(handshake.Payload[1].([]byte), wantToken) {
				t.Errorf("connection %d resumed with stale token", n)
				return
			}
			barrierN, replayTo, barrierToken := uint64(3), uint64(2), bytes32(42)
			if n == 3 {
				barrierN, replayTo, barrierToken = 4, 3, bytes32(43)
			}
			if err := sendServerFrame(ctx, ws, OpResumed, barrierN, nil, []any{uint64(2), replayTo, barrierToken}); err != nil {
				return
			}
		}
		if n > 1 {
			if n == 3 {
				close(replayReady) // allow the application to commit n=2 before its replay arrives
				select {
				case <-continueReplay:
				case <-ctx.Done():
					return
				}
			}
			if err := sendServerFrame(ctx, ws, OpMessagePlain, 2, nil, []any{device, uint64(1), user, []byte{9}, bytes32(44), uint64(0), uint64(0)}); err != nil {
				return
			}
			if n == 3 {
				// This is the original resumed frame in the replay ring. Preserve its
				// bytes exactly: it is an ordinary historical replay entry.
				if err := sendServerFrame(ctx, ws, OpResumed, 3, nil, []any{uint64(2), uint64(2), bytes32(42)}); err != nil {
					return
				}
			}
		}
		if n == 3 {
			if err := sendServerFrame(ctx, ws, OpMessagePlain, 5, nil, []any{device, uint64(2), user, []byte{10}, bytes32(44), uint64(0), uint64(0)}); err != nil {
				return
			}
		}
		for {
			if _, _, err := ws.Read(ctx); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	instanceID, _ := id.Parse(instance.String())
	httpClient, err := client.New(client.Config{BaseURL: server.URL, InstanceID: instanceID, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	bot, err := New(Config{Client: httpClient, AllowLoopbackHTTP: true, ExpectedDeviceID: device, ExpectedUserID: user, QueueSize: 4})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- bot.Run(ctx, func(context.Context) (string, error) { return "session", nil }) }()
	read := func() Delivery {
		t.Helper()
		select {
		case d := <-bot.Events():
			return d
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for delivery")
			return Delivery{}
		}
	}
	if ready := read(); ready.Frame.Op != OpReady || ready.Ack() != nil {
		t.Fatalf("ready delivery=%+v", ready)
	}
	close(drop[0])
	first := read() // handler owns n=2 but has not made it durable yet
	if first.Frame.Op != OpMessagePlain || first.Frame.N != 2 {
		t.Fatalf("first in-flight delivery=%+v", first.Frame)
	}
	barrier := read()
	if barrier.Frame.Op != OpResumed || barrier.Frame.N != 3 {
		t.Fatalf("first replay barrier=%+v", barrier.Frame)
	}
	close(drop[1]) // interruption in the middle of application handling
	select {
	case <-replayReady:
	case <-time.After(2 * time.Second):
		t.Fatal("third resume did not reach its announced replay range")
	}
	if err := first.Ack(); err != nil { // races the already-requested replay of n=2
		t.Fatalf("commit in-flight handler before replay: %v", err)
	}
	close(continueReplay)
	// The replay of already durable n=2 and the old n=3 resumed control are
	// skipped. The new barrier and next live frame remain visible in sequence.
	newBarrier := read()
	if newBarrier.Frame.Op != OpResumed || newBarrier.Frame.N != 4 {
		t.Fatalf("new replay barrier=%+v, want n=4", newBarrier.Frame)
	}
	live := read()
	if live.Frame.N != 5 || live.Frame.Op != OpMessagePlain {
		t.Fatalf("after reconnect got duplicate/lost event %+v, want live n=5", live.Frame)
	}
	if err := barrier.Ack(); err != nil {
		t.Fatalf("held replay barrier ack: %v", err)
	}
	if err := newBarrier.Ack(); err != nil {
		t.Fatalf("ack new replay barrier: %v", err)
	}
	if err := live.Ack(); err != nil {
		t.Fatalf("ack live event: %v", err)
	}
	cancel()
	close(drop[2])
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after cancellation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
}

func TestRunRejectsChangedInstanceAndWireDowngrade(t *testing.T) {
	for _, tc := range []struct {
		name          string
		helloInstance id.ID
		wire          []uint64
		want          error
	}{
		{name: "changed instance", helloInstance: testID(9), wire: []uint64{6}, want: ErrIdentity},
		{name: "wire 2 only", helloInstance: testID(1), wire: []uint64{2}, want: ErrVersion},
		{name: "wire 5 only", helloInstance: testID(1), wire: []uint64{5, 4, 3, 2}, want: ErrVersion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ws, e := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"dilla.v1"}, CompressionMode: websocket.CompressionDisabled})
				if e != nil {
					return
				}
				defer ws.Close(websocket.StatusNormalClosure, "test")
				p := helloPayload(tc.helloInstance, 1)
				p[0] = tc.wire
				if e = sendServerFrame(r.Context(), ws, OpHello, 0, nil, p); e != nil {
					return
				}
				_, _, _ = ws.Read(r.Context())
			}))
			defer server.Close()
			instanceID, _ := id.Parse(testID(1).String())
			httpClient, e := client.New(client.Config{BaseURL: server.URL, InstanceID: instanceID, AllowLoopbackHTTP: true})
			if e != nil {
				t.Fatal(e)
			}
			gw, e := New(Config{Client: httpClient, AllowLoopbackHTTP: true, ExpectedDeviceID: testID(2), ExpectedUserID: testID(3)})
			if e != nil {
				t.Fatal(e)
			}
			err := gw.Run(context.Background(), func(context.Context) (string, error) { return "session", nil })
			if !errors.Is(err, tc.want) {
				t.Fatalf("Run error=%v, want %v", err, tc.want)
			}
		})
	}
}

func TestRunRejectsTextFramesAtEveryGatewayReadStage(t *testing.T) {
	for _, stage := range []string{"hello", "ready", "established"} {
		t.Run(stage, func(t *testing.T) {
			instance, device, user := testID(51), testID(52), testID(53)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"dilla.v1"}, CompressionMode: websocket.CompressionDisabled})
				if err != nil {
					return
				}
				defer ws.Close(websocket.StatusNormalClosure, "test")
				ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
				defer cancel()
				hello, _ := encodeFrame(OpHello, 0, nil, helloPayload(instance, 1))
				if stage == "hello" {
					_ = ws.Write(ctx, websocket.MessageText, hello)
					return
				}
				if err := ws.Write(ctx, websocket.MessageBinary, hello); err != nil {
					return
				}
				_, _, err = ws.Read(ctx)
				if err != nil {
					return
				}
				ready, _ := encodeFrame(OpReady, 1, nil, readyPayload(device, user, 1, bytes32(55)))
				if stage == "ready" {
					_ = ws.Write(ctx, websocket.MessageText, ready)
					return
				}
				if err = ws.Write(ctx, websocket.MessageBinary, ready); err != nil {
					return
				}
				_ = ws.Write(ctx, websocket.MessageText, ready) // valid CBOR bytes, forbidden text message
				_, _, _ = ws.Read(ctx)
			}))
			defer server.Close()
			instanceID, _ := id.Parse(instance.String())
			httpClient, err := client.New(client.Config{BaseURL: server.URL, InstanceID: instanceID, AllowLoopbackHTTP: true})
			if err != nil {
				t.Fatal(err)
			}
			bot, err := New(Config{Client: httpClient, AllowLoopbackHTTP: true, ExpectedDeviceID: device, ExpectedUserID: user})
			if err != nil {
				t.Fatal(err)
			}
			err = bot.Run(context.Background(), func(context.Context) (string, error) { return "session", nil })
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("Run error=%v, want strict binary-frame rejection", err)
			}
		})
	}
}

func TestRunRejectsMismatchedReadyIdentityGenerationAndVersions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func([]any)
		want   error
	}{
		{"device", func(p []any) { p[0] = testID(70) }, ErrIdentity},
		{"user", func(p []any) { p[1] = testID(71) }, ErrIdentity},
		{"generation", func(p []any) { p[2] = uint64(2) }, ErrIdentity},
		{"wire version", func(p []any) { p[4] = uint64(5) }, ErrVersion},
		{"e2ee version", func(p []any) { p[5] = uint64(2) }, ErrVersion},
		{"media version", func(p []any) { p[6] = uint64(2) }, ErrVersion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			instance, device, user := testID(72), testID(73), testID(74)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"dilla.v1"}, CompressionMode: websocket.CompressionDisabled})
				if err != nil {
					return
				}
				defer ws.Close(websocket.StatusNormalClosure, "test")
				ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
				defer cancel()
				if err = sendServerFrame(ctx, ws, OpHello, 0, nil, helloPayload(instance, 1)); err != nil {
					return
				}
				if _, _, err = ws.Read(ctx); err != nil {
					return
				}
				p := readyPayload(device, user, 1, bytes32(75))
				tc.mutate(p)
				_ = sendServerFrame(ctx, ws, OpReady, 1, nil, p)
			}))
			defer server.Close()
			instanceID, _ := id.Parse(instance.String())
			httpClient, err := client.New(client.Config{BaseURL: server.URL, InstanceID: instanceID, AllowLoopbackHTTP: true})
			if err != nil {
				t.Fatal(err)
			}
			bot, err := New(Config{Client: httpClient, AllowLoopbackHTTP: true, ExpectedDeviceID: device, ExpectedUserID: user})
			if err != nil {
				t.Fatal(err)
			}
			if err = bot.Run(context.Background(), func(context.Context) (string, error) { return "session", nil }); !errors.Is(err, tc.want) {
				t.Fatalf("Run error=%v, want %v", err, tc.want)
			}
		})
	}
}

func TestHandshakeRejectsUnexpectedValidOpcodeWithoutRetryLoop(t *testing.T) {
	for _, tc := range []struct {
		name   string
		resume bool
		op     Opcode
	}{
		{"identify heartbeat ack", false, OpHeartbeatAck},
		{"identify delivery", false, OpMessagePlain},
		{"identify resumed", false, OpResumed},
		{"resume ready", true, OpReady},
		{"resume delivery", true, OpMessagePlain},
		{"resume heartbeat ack", true, OpHeartbeatAck},
	} {
		t.Run(tc.name, func(t *testing.T) {
			instance, device, user := testID(101), testID(102), testID(103)
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := int(attempts.Add(1))
				ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"dilla.v1"}, CompressionMode: websocket.CompressionDisabled})
				if err != nil {
					return
				}
				defer ws.Close(websocket.StatusNormalClosure, "test")
				ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
				defer cancel()
				if err = sendServerFrame(ctx, ws, OpHello, 0, nil, helloPayload(instance, 9)); err != nil {
					return
				}
				_, raw, err := ws.Read(ctx)
				if err != nil {
					return
				}
				handshake, err := decodeClientTestFrame(raw)
				if err != nil {
					t.Errorf("decode handshake: %v", err)
					return
				}
				if tc.resume && n == 1 {
					if handshake.Op != OpIdentify {
						t.Errorf("initial op=%d, want identify", handshake.Op)
						return
					}
					_ = sendServerFrame(ctx, ws, OpReady, 1, nil, readyPayload(device, user, 9, bytes32(104)))
					return
				}
				if tc.resume && handshake.Op != OpResume || !tc.resume && handshake.Op != OpIdentify {
					t.Errorf("handshake op=%d in attempt %d", handshake.Op, n)
					return
				}
				switch tc.op {
				case OpHeartbeatAck:
					_ = sendServerFrame(ctx, ws, OpHeartbeatAck, 0, nil, []any{uint64(1)})
				case OpMessagePlain:
					_ = sendServerFrame(ctx, ws, OpMessagePlain, 2, nil, []any{device, uint64(1), user, []byte{1}, bytes32(105), uint64(0), uint64(0)})
				case OpResumed:
					_ = sendServerFrame(ctx, ws, OpResumed, 2, nil, []any{uint64(1), uint64(1), bytes32(105)})
				case OpReady:
					_ = sendServerFrame(ctx, ws, OpReady, 1, nil, readyPayload(device, user, 9, bytes32(105)))
				}
			}))
			defer server.Close()
			instanceID, _ := id.Parse(instance.String())
			httpClient, err := client.New(client.Config{BaseURL: server.URL, InstanceID: instanceID, AllowLoopbackHTTP: true})
			if err != nil {
				t.Fatal(err)
			}
			bot, err := New(Config{Client: httpClient, AllowLoopbackHTTP: true, ExpectedDeviceID: device, ExpectedUserID: user, HandshakeTimeout: 500 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			err = bot.Run(ctx, func(context.Context) (string, error) { return "session", nil })
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("Run error=%v, want immediate protocol-state error", err)
			}
			wantAttempts := int32(1)
			if tc.resume {
				wantAttempts = 2
			}
			if got := attempts.Load(); got != wantAttempts {
				t.Fatalf("handshake attempts=%d, want bounded %d", got, wantAttempts)
			}
		})
	}
}

func TestRunDeliversDeletedPlainMessageThenContinues(t *testing.T) {
	instance, device, user, channel := testID(111), testID(112), testID(113), testID(114)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"dilla.v1"}, CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			return
		}
		defer ws.Close(websocket.StatusNormalClosure, "test")
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		defer cancel()
		if err = sendServerFrame(ctx, ws, OpHello, 0, nil, helloPayload(instance, 10)); err != nil {
			return
		}
		if _, _, err = ws.Read(ctx); err != nil {
			return
		}
		if err = sendServerFrame(ctx, ws, OpReady, 1, nil, readyPayload(device, user, 10, bytes32(115))); err != nil {
			return
		}
		deletedPayload, e := gateway.MessagePlainPayload(channel, 1, user, nil, bytes32(116), 0, 1)
		if e != nil {
			t.Errorf("production deleted-message payload: %v", e)
			return
		}
		var deleted []any
		if e = cborx.Unmarshal(deletedPayload, &deleted); e != nil {
			t.Errorf("decode production deleted-message payload: %v", e)
			return
		}
		if err = sendServerFrame(ctx, ws, OpMessagePlain, 2, nil, deleted); err != nil {
			return
		}
		laterPayload, e := gateway.MessagePlainPayload(channel, 2, user, []byte{9}, bytes32(117), 0, 0)
		if e != nil {
			t.Errorf("production message payload: %v", e)
			return
		}
		var later []any
		if e = cborx.Unmarshal(laterPayload, &later); e != nil {
			t.Errorf("decode production message payload: %v", e)
			return
		}
		_ = sendServerFrame(ctx, ws, OpMessagePlain, 3, nil, later)
		_, _, _ = ws.Read(ctx)
	}))
	defer server.Close()
	instanceID, _ := id.Parse(instance.String())
	httpClient, err := client.New(client.Config{BaseURL: server.URL, InstanceID: instanceID, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	bot, err := New(Config{Client: httpClient, AllowLoopbackHTTP: true, ExpectedDeviceID: device, ExpectedUserID: user})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- bot.Run(ctx, func(context.Context) (string, error) { return "session", nil }) }()
	for i, wantN := range []uint64{1, 2, 3} {
		select {
		case d := <-bot.Events():
			if d.Frame.N != wantN {
				t.Fatalf("delivery %d n=%d, want %d", i, d.Frame.N, wantN)
			}
			if i == 1 && (d.Frame.MessagePlain == nil || !d.Frame.MessagePlain.Deleted || len(d.Frame.MessagePlain.Envelope) != 0) {
				t.Fatalf("deleted message was not decoded: %+v", d.Frame.MessagePlain)
			}
			if i == 2 && (d.Frame.MessagePlain == nil || d.Frame.MessagePlain.Deleted || len(d.Frame.MessagePlain.Envelope) != 1) {
				t.Fatalf("later message was not delivered after deletion: %+v", d.Frame.MessagePlain)
			}
			if err := d.Ack(); err != nil {
				t.Fatalf("ack n=%d: %v", d.Frame.N, err)
			}
		case err := <-done:
			t.Fatalf("Run exited before delivery n=%d: %v", wantN, err)
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for n=%d", wantN)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not cancel")
	}
}

func TestRunFallsBackToIdentifyAfterExpiredResumeOrGenerationChange(t *testing.T) {
	for _, scenario := range []string{"expired resume", "generation change"} {
		t.Run(scenario, func(t *testing.T) {
			instance, device, user := testID(76), testID(77), testID(78)
			dropFirst := make(chan struct{})
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := int(attempts.Add(1))
				generation := uint64(1)
				if scenario == "generation change" && n > 1 {
					generation = 2
				}
				ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"dilla.v1"}, CompressionMode: websocket.CompressionDisabled})
				if err != nil {
					return
				}
				defer ws.Close(websocket.StatusNormalClosure, "test")
				ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
				defer cancel()
				if err = sendServerFrame(ctx, ws, OpHello, 0, nil, helloPayload(instance, generation)); err != nil {
					return
				}
				_, raw, err := ws.Read(ctx)
				if err != nil {
					return
				}
				handshake, err := decodeClientTestFrame(raw)
				if err != nil {
					t.Errorf("handshake: %v", err)
					return
				}
				if n == 1 {
					if handshake.Op != OpIdentify {
						t.Errorf("first handshake op=%d", handshake.Op)
						return
					}
					if err = sendServerFrame(ctx, ws, OpReady, 1, nil, readyPayload(device, user, generation, bytes32(80))); err != nil {
						return
					}
					select {
					case <-dropFirst:
					case <-ctx.Done():
					}
					return
				}
				if scenario == "expired resume" && n == 2 {
					if handshake.Op != OpResume {
						t.Errorf("expired attempt op=%d, want resume", handshake.Op)
						return
					}
					_ = sendServerFrame(ctx, ws, OpInvalidSession, 0, nil, []any{uint64(0), "expired"})
					return
				}
				if handshake.Op != OpIdentify {
					t.Errorf("fallback handshake op=%d, want identify", handshake.Op)
					return
				}
				_ = sendServerFrame(ctx, ws, OpReady, 1, nil, readyPayload(device, user, generation, bytes32(81)))
			}))
			defer server.Close()
			instanceID, _ := id.Parse(instance.String())
			httpClient, err := client.New(client.Config{BaseURL: server.URL, InstanceID: instanceID, AllowLoopbackHTTP: true})
			if err != nil {
				t.Fatal(err)
			}
			bot, err := New(Config{Client: httpClient, AllowLoopbackHTTP: true, ExpectedDeviceID: device, ExpectedUserID: user})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- bot.Run(ctx, func(context.Context) (string, error) { return "session", nil }) }()
			select {
			case first := <-bot.Events():
				if first.Frame.Op != OpReady || first.Ack() != nil {
					t.Fatalf("initial ready=%+v", first.Frame)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("initial identify did not become ready")
			}
			close(dropFirst)
			select {
			case second := <-bot.Events():
				if second.Frame.Op != OpReady || second.Frame.Ready.Generation != 2 && scenario == "generation change" {
					t.Fatalf("fallback ready=%+v", second.Frame)
				}
				if err := second.Ack(); err != nil {
					t.Fatalf("ack fallback ready: %v", err)
				}
			case err := <-done:
				t.Fatalf("Run exited before fallback identify: %v", err)
			case <-time.After(4 * time.Second):
				t.Fatal("fresh identify did not become ready")
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("Run after cancellation: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Run did not cancel")
			}
		})
	}
}

func TestTwoIdentityClientsKeepResumeTokensAndCursorsIsolated(t *testing.T) {
	instance := testID(91)
	type identity struct {
		token        string
		device, user id.ID
		drop         chan struct{}
	}
	identities := []identity{
		{token: "session-alpha", device: testID(92), user: testID(93), drop: make(chan struct{})},
		{token: "session-beta", device: testID(94), user: testID(95), drop: make(chan struct{})},
	}
	var mu sync.Mutex
	counts := map[string]int{}
	byToken := map[string]identity{identities[0].token: identities[0], identities[1].token: identities[1]}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who, ok := byToken[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		counts[who.token]++
		n := counts[who.token]
		mu.Unlock()
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"dilla.v1"}, CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			return
		}
		defer ws.Close(websocket.StatusNormalClosure, "test")
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := sendServerFrame(ctx, ws, OpHello, 0, nil, helloPayload(instance, 8)); err != nil {
			return
		}
		_, raw, err := ws.Read(ctx)
		if err != nil {
			return
		}
		handshake, err := decodeClientTestFrame(raw)
		if err != nil {
			t.Errorf("%s handshake decode: %v", who.token, err)
			return
		}
		if n == 1 {
			if handshake.Op != OpIdentify {
				t.Errorf("%s first op=%d, want identify", who.token, handshake.Op)
				return
			}
			seed := uint8(96)
			if who.token == "session-beta" {
				seed = 97
			}
			if err := sendServerFrame(ctx, ws, OpReady, 1, nil, readyPayload(who.device, who.user, 8, bytes32(seed))); err != nil {
				return
			}
			select {
			case <-who.drop:
			case <-ctx.Done():
			}
			return
		}
		seed := uint8(96)
		rotated := uint8(98)
		if who.token == "session-beta" {
			seed, rotated = 97, 99
		}
		if handshake.Op != OpResume || handshake.Payload[3] != uint64(1) || !bytesEqual(handshake.Payload[1].([]byte), bytes32(seed)) {
			t.Errorf("%s resume state leaked or was incorrect: %+v", who.token, handshake)
			return
		}
		_ = sendServerFrame(ctx, ws, OpResumed, 2, nil, []any{uint64(2), uint64(1), bytes32(rotated)})
		<-ctx.Done()
	}))
	defer server.Close()
	instanceID, _ := id.Parse(instance.String())
	type running struct {
		bot      *Client
		ctx      context.Context
		cancel   context.CancelFunc
		done     chan error
		identity identity
	}
	clients := make([]running, len(identities))
	for i, who := range identities {
		httpClient, err := client.New(client.Config{BaseURL: server.URL, InstanceID: instanceID, AllowLoopbackHTTP: true})
		if err != nil {
			t.Fatal(err)
		}
		bot, err := New(Config{Client: httpClient, AllowLoopbackHTTP: true, ExpectedDeviceID: who.device, ExpectedUserID: who.user})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		clients[i] = running{bot: bot, ctx: ctx, cancel: cancel, done: done, identity: who}
		go func(bot *Client, runCtx context.Context, credential string, done chan<- error) {
			done <- bot.Run(runCtx, func(context.Context) (string, error) { return credential, nil })
		}(bot, ctx, who.token, done)
	}
	for i := range clients {
		c := &clients[i]
		select {
		case ready := <-c.bot.Events():
			if ready.Frame.Op != OpReady || ready.Frame.Ready.DeviceID != c.identity.device || ready.Ack() != nil {
				t.Fatalf("identity %s received wrong ready: %+v", c.identity.token, ready.Frame)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("identity %s did not become ready", c.identity.token)
		}
		close(c.identity.drop)
	}
	for i := range clients {
		c := &clients[i]
		select {
		case resumed := <-c.bot.Events():
			want := uint8(98)
			if c.identity.token == "session-beta" {
				want = 99
			}
			if resumed.Frame.Op != OpResumed || resumed.Frame.Resumed == nil || resumed.Frame.Resumed.ResumeToken[0] != want {
				t.Fatalf("identity %s received cross-wired resume: %+v", c.identity.token, resumed.Frame)
			}
			if err := resumed.Ack(); err != nil {
				t.Fatalf("identity %s ack: %v", c.identity.token, err)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("identity %s did not resume", c.identity.token)
		}
	}
	for i := range clients {
		clients[i].cancel()
	}
	for i := range clients {
		select {
		case err := <-clients[i].done:
			if err != nil {
				t.Fatalf("Run %s: %v", clients[i].identity.token, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("Run %s did not cancel", clients[i].identity.token)
		}
	}
}

func TestRunCancellationStopsReconnectWait(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	instanceID, _ := id.Parse(testID(1).String())
	httpClient, err := client.New(client.Config{BaseURL: server.URL, InstanceID: instanceID, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	gw, err := New(Config{Client: httpClient, AllowLoopbackHTTP: true, ExpectedDeviceID: testID(2), ExpectedUserID: testID(3)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gw.Run(ctx, func(context.Context) (string, error) { return "session", nil }) }()
	deadline := time.Now().Add(2 * time.Second)
	for attempts.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if attempts.Load() == 0 {
		t.Fatal("gateway dial did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not stop during reconnect wait")
	}
}

func TestRunCancellationDuringHandshakeDoesNotWaitForPeerClose(t *testing.T) {
	connected := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"dilla.v1"}})
		if err != nil {
			return
		}
		defer ws.CloseNow()
		close(connected)
		<-r.Context().Done() // Deliberately never read or answer a close frame.
	}))
	defer server.Close()
	instance := testID(91)
	httpClient, err := client.New(client.Config{BaseURL: server.URL, InstanceID: instance, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	gw, err := New(Config{Client: httpClient, AllowLoopbackHTTP: true, ExpectedDeviceID: testID(92), ExpectedUserID: testID(93)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gw.Run(ctx, func(context.Context) (string, error) { return "session", nil }) }()
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("gateway did not connect")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after handshake cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run waited for a peer close frame after cancellation")
	}
}

func TestRunClosesConnectionWhenHeartbeatAckIsMissing(t *testing.T) {
	instance, device, user := testID(1), testID(2), testID(3)
	closed := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, e := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"dilla.v1"}, CompressionMode: websocket.CompressionDisabled})
		if e != nil {
			return
		}
		defer ws.Close(websocket.StatusNormalClosure, "test")
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if e = sendServerFrame(ctx, ws, OpHello, 0, nil, helloPayload(instance, 1)); e != nil {
			return
		}
		if _, _, e = ws.Read(ctx); e != nil {
			return
		}
		if e = sendServerFrame(ctx, ws, OpReady, 1, nil, readyPayload(device, user, 1, bytes32(10))); e != nil {
			return
		}
		for {
			if _, _, e = ws.Read(ctx); e != nil {
				closed <- struct{}{}
				return
			}
		}
	}))
	defer server.Close()
	instanceID, _ := id.Parse(instance.String())
	httpClient, err := client.New(client.Config{BaseURL: server.URL, InstanceID: instanceID, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	gw, err := New(Config{Client: httpClient, AllowLoopbackHTTP: true, ExpectedDeviceID: device, ExpectedUserID: user, HeartbeatAckTimeout: 150 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gw.Run(ctx, func(context.Context) (string, error) { return "session", nil }) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("connection remained open without heartbeat acknowledgement")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not stop promptly")
	}
}

type testClientFrame struct {
	Op      Opcode
	GroupID any
	Payload []any
}

func decodeClientTestFrame(raw []byte) (testClientFrame, error) {
	var f []cbor.RawMessage
	if err := cborx.Unmarshal(raw, &f); err != nil || len(f) != 4 {
		return testClientFrame{}, ErrProtocol
	}
	var op uint64
	if err := cborx.Unmarshal(f[0], &op); err != nil {
		return testClientFrame{}, ErrProtocol
	}
	var p []any
	if err := cborx.Unmarshal(f[3], &p); err != nil {
		return testClientFrame{}, ErrProtocol
	}
	var group any
	if !isNull(f[2]) {
		var b []byte
		if err := cborx.Unmarshal(f[2], &b); err != nil {
			return testClientFrame{}, ErrProtocol
		}
		group = b
	}
	return testClientFrame{Op: Opcode(op), GroupID: group, Payload: p}, nil
}
func sendServerFrame(ctx context.Context, ws *websocket.Conn, op Opcode, n uint64, gid *id.ID, payload []any) error {
	b, e := encodeFrame(op, n, gid, payload)
	if e != nil {
		return e
	}
	return ws.Write(ctx, websocket.MessageBinary, b)
}

const testBackoffMS uint64 = 300

func helloPayload(instance id.ID, generation uint64) []any {
	return []any{[]any{uint64(6)}, []any{uint64(1)}, []any{uint64(1)}, uint64(1000), uint64(131584), instance, generation, testBackoffMS, testBackoffMS}
}
func readyPayload(device, user id.ID, generation uint64, token []byte) []any {
	return readyPayloadBoot(device, user, generation, token, testID(0x77))
}

// readyPayloadBoot is a ready whose element 9 is the dillad process's boot id.
func readyPayloadBoot(device, user id.ID, generation uint64, token []byte, boot id.ID) []any {
	return []any{device, user, generation, token, uint64(6), uint64(1), uint64(1), uint64(0), []any{}, boot}
}
func testID(v byte) id.ID    { var x id.ID; x[0] = v; return x }
func bytes32(v uint8) []byte { b := make([]byte, 32); b[0] = v; return b }
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
