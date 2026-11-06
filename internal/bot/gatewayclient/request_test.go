package gatewayclient

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

// seenRequest is one request frame as the fake instance read it, with the connection it came on.
type seenRequest struct {
	conn          int32
	id            uint64
	method, route string
	body, idem    []byte
}

// answer is what the fake instance does with one request: reply, or close the socket without one.
type answer struct {
	status uint64
	body   []byte
	drop   bool
}

// requestServer is a fake instance: hello, identify or resume, ready, then a request loop driven by serve.
type requestServer struct {
	t                      *testing.T
	instance, device, user id.ID
	conns                  atomic.Int32
	mu                     sync.Mutex
	seen                   []seenRequest
	serve                  func(r seenRequest) answer
}

func (s *requestServer) requests() []seenRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]seenRequest(nil), s.seen...)
}

func (s *requestServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := s.conns.Add(1)
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"dilla.v1"}})
	if err != nil {
		return
	}
	defer ws.CloseNow()
	ws.SetReadLimit(32 << 20)
	ctx := r.Context()
	if sendServerFrame(ctx, ws, OpHello, 0, nil, helloPayload(s.instance, 7)) != nil {
		return
	}
	_, raw, err := ws.Read(ctx)
	if err != nil {
		return
	}
	first, err := decodeClientTestFrame(raw)
	if err != nil {
		s.t.Errorf("handshake frame: %v", err)
		return
	}
	switch first.Op {
	case OpIdentify:
		if sendServerFrame(ctx, ws, OpReady, 1, nil, readyPayload(s.device, s.user, 7, bytes32(uint8(n)))) != nil {
			return
		}
	case OpResume:
		ack, _ := first.Payload[3].(uint64)
		if sendServerFrame(ctx, ws, OpResumed, ack+1, nil, []any{ack + 1, ack, bytes32(uint8(n))}) != nil {
			return
		}
	default:
		s.t.Errorf("handshake op %d", first.Op)
		return
	}
	for {
		_, raw, err := ws.Read(ctx)
		if err != nil {
			return
		}
		f, err := decodeClientTestFrame(raw)
		if err != nil {
			s.t.Errorf("client frame: %v", err)
			return
		}
		if f.Op != OpRequest {
			continue
		}
		if len(f.Payload) != 5 {
			s.t.Errorf("request payload %v", f.Payload)
			return
		}
		req := seenRequest{conn: n}
		req.id, _ = f.Payload[0].(uint64)
		req.method, _ = f.Payload[1].(string)
		req.route, _ = f.Payload[2].(string)
		req.body, _ = f.Payload[3].([]byte)
		req.idem, _ = f.Payload[4].([]byte)
		s.mu.Lock()
		s.seen = append(s.seen, req)
		s.mu.Unlock()
		a := s.serve(req)
		if a.drop {
			_ = ws.Close(websocket.StatusGoingAway, "drop")
			return
		}
		if a.body == nil {
			a.body = []byte{}
		}
		if sendServerFrame(ctx, ws, OpResponse, 0, nil, []any{req.id, a.status, a.body}) != nil {
			return
		}
	}
}

// startRequestClient runs a gateway client against srv and acks every delivery, as the Bot's consumer does.
func startRequestClient(t *testing.T, srv *requestServer) (*Client, context.Context) {
	t.Helper()
	srv.t, srv.instance, srv.device, srv.user = t, testID(1), testID(2), testID(3)
	server := httptest.NewServer(srv)
	t.Cleanup(server.Close)
	api, err := client.New(client.Config{BaseURL: server.URL, InstanceID: srv.instance, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	gw, err := New(Config{Client: api, AllowLoopbackHTTP: true, ExpectedDeviceID: srv.device, ExpectedUserID: srv.user})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	done := make(chan error, 1)
	go func() { done <- gw.Run(ctx, func(context.Context) (string, error) { return "token", nil }) }()
	go func() {
		for d := range gw.Events() {
			_ = d.Ack()
		}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("Run did not stop")
		}
	})
	return gw, ctx
}

func refusalBody(t *testing.T, code string, retryMS any) []byte {
	t.Helper()
	b, err := cborx.Marshal([]any{code, "", retryMS})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var _ client.Transport = (*Client)(nil)

func TestDoSendsRequestFramesAndMatchesResponses(t *testing.T) {
	srv := &requestServer{serve: func(r seenRequest) answer {
		return answer{status: 200, body: []byte(r.method + " " + r.route)}
	}}
	gw, ctx := startRequestClient(t, srv)
	var wg sync.WaitGroup
	results := make([]string, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			route := "/v1/x/" + string(rune('a'+i))
			status, body, err := gw.Do(ctx, http.MethodGet, route, nil, nil)
			if err != nil || status != 200 {
				t.Errorf("Do %s = %d, %v", route, status, err)
			}
			results[i] = string(body)
		}()
	}
	wg.Wait()
	for i, got := range results {
		if want := "GET /v1/x/" + string(rune('a'+i)); got != want {
			t.Errorf("result %d = %q, want %q", i, got, want)
		}
	}
	for _, r := range srv.requests() {
		if r.idem != nil || len(r.body) != 0 {
			t.Errorf("a GET carried idem %x or body %x", r.idem, r.body)
		}
	}
}

func TestDoResendsAMutationLostInADisconnectWithTheSameIdem(t *testing.T) {
	var effects atomic.Int32
	var mu sync.Mutex
	record := map[string][]byte{}
	srv := &requestServer{}
	srv.serve = func(r seenRequest) answer {
		mu.Lock()
		defer mu.Unlock()
		if b, ok := record[string(r.idem)]; ok {
			return answer{status: 200, body: b}
		}
		effects.Add(1)
		record[string(r.idem)] = []byte{byte(effects.Load())}
		if r.conn == 1 {
			return answer{drop: true} // applied, but the answer is lost with the socket
		}
		return answer{status: 200, body: record[string(r.idem)]}
	}
	gw, ctx := startRequestClient(t, srv)
	idem := bytes.Repeat([]byte{9}, 16)
	status, body, err := gw.Do(ctx, http.MethodPost, "/v1/x", []byte{0x80}, idem)
	if err != nil || status != 200 || !bytes.Equal(body, []byte{1}) {
		t.Fatalf("Do = %d %x %v, want 200 01", status, body, err)
	}
	seen := srv.requests()
	if len(seen) != 2 || seen[0].conn == seen[1].conn || !bytes.Equal(seen[0].idem, idem) || !bytes.Equal(seen[1].idem, idem) {
		t.Fatalf("requests = %+v, want the same idem on two connections", seen)
	}
	if effects.Load() != 1 {
		t.Fatalf("effects = %d, want 1", effects.Load())
	}
}

func TestDoRequeuesOnBusyAndRetriesUnavailableWithTheSameIdem(t *testing.T) {
	var calls atomic.Int32
	srv := &requestServer{}
	srv.serve = func(r seenRequest) answer {
		switch calls.Add(1) {
		case 1:
			return answer{status: 429, body: refusalBody(t, "E_BUSY", nil)}
		case 2:
			return answer{status: 503, body: refusalBody(t, "E_UNAVAILABLE", uint64(10))}
		default:
			return answer{status: 204}
		}
	}
	gw, ctx := startRequestClient(t, srv)
	idem := bytes.Repeat([]byte{4}, 16)
	status, _, err := gw.Do(ctx, http.MethodPut, "/v1/y", []byte{0x80}, idem)
	if err != nil || status != 204 {
		t.Fatalf("Do = %d, %v, want 204", status, err)
	}
	seen := srv.requests()
	if len(seen) != 3 {
		t.Fatalf("sent %d times, want 3", len(seen))
	}
	for _, r := range seen {
		if !bytes.Equal(r.idem, idem) {
			t.Fatalf("a retry changed the idem: %x", r.idem)
		}
	}
}

func TestDoSurfacesConflictAndAnswersAndRequiresIdem(t *testing.T) {
	srv := &requestServer{serve: func(r seenRequest) answer {
		if r.route == "/v1/offline" {
			return answer{status: 503, body: refusalBody(t, "E_BOT_OFFLINE", nil)}
		}
		return answer{status: 409, body: refusalBody(t, "E_INVALID_REQUEST", nil)}
	}}
	gw, ctx := startRequestClient(t, srv)
	if status, _, err := gw.Do(ctx, http.MethodPost, "/v1/z", nil, []byte{1}); err != nil || status != 409 {
		t.Fatalf("Do = %d, %v, want the 409 answer", status, err)
	}
	if status, _, err := gw.Do(ctx, http.MethodPost, "/v1/offline", nil, []byte{2}); err != nil || status != 503 {
		t.Fatalf("Do = %d, %v, want the route's 503 answer", status, err)
	}
	if _, _, err := gw.Do(ctx, http.MethodPost, "/v1/z", nil, nil); !errors.Is(err, ErrIdempotencyRequired) {
		t.Fatalf("a mutation without idem = %v, want ErrIdempotencyRequired", err)
	}
	if n := len(srv.requests()); n != 2 {
		t.Fatalf("sent %d requests, want 2", n)
	}
}

func TestDoWaitsForTheFirstConnection(t *testing.T) {
	api, err := client.New(client.Config{BaseURL: "http://127.0.0.1:1", InstanceID: testID(1), AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	gw, err := New(Config{Client: api, AllowLoopbackHTTP: true, ExpectedDeviceID: testID(2), ExpectedUserID: testID(3)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, _, err := gw.Do(ctx, http.MethodGet, "/v1/x", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Do without a connection = %v, want it to wait for the caller's deadline", err)
	}
}

func TestResponseFrameCodec(t *testing.T) {
	raw, err := EncodeRequestFrame(7, http.MethodPost, "/v1/x", nil, []byte{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	f, err := decodeClientTestFrame(raw)
	if err != nil || f.Op != OpRequest || len(f.Payload) != 5 || !bytes.Equal(f.Payload[3].([]byte), []byte{}) {
		t.Fatalf("request frame = %+v, %v", f, err)
	}
	if _, err := EncodeRequestFrame(1, http.MethodGet, "/v1/x", make([]byte, MaxRequestBodyBytes+1), nil); err == nil {
		t.Fatal("an over-cap body was encoded")
	}
	good, _ := cborx.Marshal([]any{uint64(OpResponse), uint64(0), nil, []any{uint64(3), uint64(204), []byte{}}})
	if r, err := DecodeResponseFrame(good); err != nil || r.ID != 3 || r.Status != 204 {
		t.Fatalf("decode = %+v, %v", r, err)
	}
	for _, bad := range [][]any{
		{uint64(OpResponse), uint64(1), nil, []any{uint64(3), uint64(204), []byte{}}},
		{uint64(OpResponse), uint64(0), nil, []any{uint64(3), uint64(600), []byte{}}},
		{uint64(OpResponse), uint64(0), nil, []any{uint64(3), uint64(200), "x"}},
		{uint64(OpResponse), uint64(0), nil, []any{uint64(3), uint64(200)}},
	} {
		b, _ := cborx.Marshal(bad)
		if _, err := DecodeResponseFrame(b); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}
