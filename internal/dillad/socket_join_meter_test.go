package dillad_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/dillad"
	"github.com/dilla-chat/dilla/internal/id"
)

// dialForwarded is dialReady with an X-Forwarded-For on the upgrade, as a trusted proxy sends it.
func dialForwarded(t *testing.T, ctx context.Context, ts *httptest.Server, token, forwarded string) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/gateway", //nolint:bodyclose // websocket.Dial documents that the handshake response body never needs closing
		&websocket.DialOptions{
			HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + token}, "X-Forwarded-For": []string{forwarded}},
			Subprotocols: []string{"dilla.v1"},
		})
	if err != nil {
		t.Fatalf("dial /gateway: %v", err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	if op, _ := readFrame(t, ctx, c); op != 0 {
		t.Fatalf("first frame op %d, want hello", op)
	}
	identify(t, ctx, c)
	for {
		op, _ := readFrame(t, ctx, c)
		if op == 3 {
			return c
		}
	}
}

// socketStatus sends one request frame and reads until its response frame, answering its status.
func socketStatus(t *testing.T, ctx context.Context, c *websocket.Conn, reqID uint64, method, route string) uint64 {
	t.Helper()
	body, err := cborx.Marshal([]any{nil})
	if err != nil {
		t.Fatal(err)
	}
	idem := make([]byte, 16)
	idem[0], idem[8] = byte(reqID), byte(reqID>>8)
	frame, err := cborx.Marshal([]any{uint64(59), uint64(0), nil, []any{reqID, method, route, body, idem}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(ctx, websocket.MessageBinary, frame); err != nil {
		t.Fatalf("write request: %v", err)
	}
	for {
		op, payload := readFrame(t, ctx, c)
		if op != 60 || len(payload) < 2 {
			continue
		}
		if got, _ := payload[0].(uint64); got != reqID {
			continue
		}
		status, _ := payload[1].(uint64)
		return status
	}
}

// TestSocketJoinMetersTheForwardedClient: behind a trusted proxy the join's invite bucket is keyed by the client address
// the proxy forwarded for the upgrade (ClientAddr = server.RealIP), so two sockets with different X-Forwarded-For get
// separate buckets and one socket's burst does not refuse the other.
func TestSocketJoinMetersTheForwardedClient(t *testing.T) {
	cfg, code := testConfigInvite(t)
	cfg.Log.Level = "warn"
	cfg.Server.TrustedProxyCIDRs = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	s, err := dillad.New(context.Background(), dillad.Options{Config: cfg, Clock: clock.System(), Wasm: sharedRuntime(t)})
	if err != nil {
		t.Fatalf("dillad.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	token := accountToken(t, s, code)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	first := dialForwarded(t, ctx, ts, token, "203.0.113.1")
	second := dialForwarded(t, ctx, ts, token, "203.0.113.2")
	route := "/v1/communities/" + id.New().String() + "/join"
	burst := cfg.Limits.Rate.InviteBurst
	var reqID uint64
	next := func() uint64 { reqID++; return reqID }
	for i := 0; i < burst; i++ {
		if st := socketStatus(t, ctx, first, next(), "POST", route); st == http.StatusTooManyRequests {
			t.Fatalf("join %d of %d on the first socket was refused inside the burst", i+1, burst)
		}
	}
	if st := socketStatus(t, ctx, first, next(), "POST", route); st != http.StatusTooManyRequests {
		t.Fatalf("join %d on the first socket = %d, want 429", burst+1, st)
	}
	if st := socketStatus(t, ctx, second, next(), "POST", route); st == http.StatusTooManyRequests {
		t.Fatal("the second socket (another forwarded client) shares the first socket's join bucket")
	}
}
