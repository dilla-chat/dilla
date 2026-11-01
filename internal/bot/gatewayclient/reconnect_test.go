package gatewayclient

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/jonasthim/dilla/internal/bot/client"
)

func TestCloseOutcomeHeartbeatOverdueRequiresFreshConnection(t *testing.T) {
	fresh, err := closeOutcome(websocket.CloseError{Code: 4009, Reason: "heartbeat overdue"})
	if !fresh || errors.Is(err, ErrTerminal) || !errors.Is(err, errHeartbeatOverdue) {
		t.Fatalf("heartbeat overdue: fresh=%v err=%v, want fresh retry", fresh, err)
	}
	for _, code := range []websocket.StatusCode{4003, 4004} {
		fresh, err := closeOutcome(websocket.CloseError{Code: code})
		if fresh || !errors.Is(err, ErrTerminal) {
			t.Fatalf("close %d: fresh=%v err=%v", code, fresh, err)
		}
	}
}

// A heartbeat-overdue close must recover inside Run, without the service calling Ensure again.
// Its old resume state is discarded because the gateway requires re-identification.
func TestRunReidentifiesAfterHeartbeatOverdueAndLogsReconnect(t *testing.T) {
	instance, device, user := testID(1), testID(2), testID(3)
	var upgrades atomic.Int32
	firstReady := make(chan struct{})
	releaseClose := make(chan struct{})
	secondIdentify := make(chan time.Time, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := upgrades.Add(1)
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"dilla.v1"}})
		if err != nil {
			return
		}
		defer ws.CloseNow()
		if err := sendServerFrame(ctx, ws, OpHello, 0, nil, helloPayload(instance, 7)); err != nil {
			return
		}
		_, raw, err := ws.Read(ctx)
		if err != nil {
			return
		}
		frame, err := decodeClientTestFrame(raw)
		if err != nil || frame.Op != OpIdentify {
			t.Errorf("connection %d handshake=%+v err=%v, want identify", n, frame, err)
			return
		}
		if err := sendServerFrame(ctx, ws, OpReady, 1, nil, readyPayload(device, user, 7, bytes32(uint8(n)))); err != nil {
			return
		}
		if n == 1 {
			close(firstReady)
			select {
			case <-releaseClose:
			case <-ctx.Done():
				return
			}
			_ = ws.Close(4009, "heartbeat overdue; PRIVATE_PEER_REASON")
		} else {
			secondIdentify <- time.Now()
			_, _, _ = ws.Read(ctx)
		}
	}))
	defer server.Close()
	api, err := client.New(client.Config{BaseURL: server.URL, InstanceID: instance, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	gw, err := New(Config{Client: api, AllowLoopbackHTTP: true, ExpectedDeviceID: device, ExpectedUserID: user,
		Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- gw.Run(ctx, func(context.Context) (string, error) { return "SECRET_CREDENTIAL", nil }) }()
	select {
	case <-firstReady:
	case <-ctx.Done():
		t.Fatal("initial identify timed out")
	}
	select {
	case ready := <-gw.Events():
		if ready.Frame.Op != OpReady {
			t.Fatalf("initial event=%d", ready.Frame.Op)
		}
		if err := ready.Ack(); err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("initial ready timed out")
	}
	closedAt := time.Now()
	close(releaseClose)
	select {
	case at := <-secondIdentify:
		if elapsed := at.Sub(closedAt); elapsed < reconnectBase || elapsed >= 2*time.Second {
			t.Fatalf("reconnect after %v, want bounded backoff %v..2s", elapsed, reconnectBase)
		}
	case err := <-done:
		t.Fatalf("Run stopped instead of reconnecting: %v", err)
	case <-ctx.Done():
		t.Fatal("heartbeat overdue never reconnected")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not stop")
	}
	output := logs.String()
	if !strings.Contains(output, `"level":"INFO"`) || !strings.Contains(output, "heartbeat overdue") || !strings.Contains(output, "reconnect") {
		t.Fatalf("missing informative reconnect log: %s", output)
	}
	if strings.Contains(output, "SECRET_CREDENTIAL") || strings.Contains(output, "PRIVATE_PEER_REASON") {
		t.Fatalf("reconnect logged private data: %s", output)
	}
}
