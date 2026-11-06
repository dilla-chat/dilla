// Package sockettest is a minimal gateway client for tests that drive HTTP handlers over the socket
// request shim (op 59 request, op 60 response). It speaks hello, identify and ready, then sends
// request frames and matches responses to them by id. It is test support only: nothing in a
// shipping binary imports it.
package sockettest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/gateway"
)

// Client is one identified gateway connection.
type Client struct {
	ws  *websocket.Conn
	ctx context.Context

	mu      sync.Mutex
	nextID  uint64
	pending map[uint64]chan gateway.SocketResponse
	order   []uint64 // response ids in arrival order
	errs    []string // codes of error frames, in arrival order
	closed  chan struct{}
	readErr error
}

// Dial connects to a gateway served at baseURL (an httptest server URL; the path /gateway is not
// added, so pass the URL of a server whose handler is Gateway.Handler) and identifies with bearer
// at wire 6.
func Dial(t testing.TB, baseURL, bearer string) *Client {
	t.Helper()
	c, err := DialContext(context.Background(), baseURL, bearer)
	if err != nil {
		t.Fatalf("sockettest: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// DialContext is Dial without a testing.TB.
func DialContext(ctx context.Context, baseURL, bearer string) (*Client, error) {
	url := "ws" + strings.TrimPrefix(baseURL, "http")
	ws, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{ //nolint:bodyclose // websocket.Dial documents that the handshake response body never needs closing
		Subprotocols: []string{"dilla.v1"},
	})
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	ws.SetReadLimit(64 << 20)
	readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, _, err := ws.Read(readCtx); err != nil { // hello
		return nil, fmt.Errorf("hello: %w", err)
	}
	identify, err := cborx.Marshal([]any{bearer, uint64(6), uint64(1), uint64(1), uint64(0)})
	if err != nil {
		return nil, err
	}
	frame, err := cborx.MarshalFrame(uint64(gateway.OpIdentify), 1, nil, identify)
	if err != nil {
		return nil, err
	}
	if err := ws.Write(readCtx, websocket.MessageBinary, frame); err != nil {
		return nil, fmt.Errorf("identify: %w", err)
	}
	for {
		_, b, err := ws.Read(readCtx)
		if err != nil {
			return nil, fmt.Errorf("ready: %w", err)
		}
		op, err := opOf(b)
		if err != nil {
			return nil, err
		}
		if op == gateway.OpReady {
			break
		}
		if op == gateway.OpError {
			return nil, fmt.Errorf("identify refused: %x", b)
		}
	}
	c := &Client{
		ws: ws, ctx: context.Background(),
		pending: map[uint64]chan gateway.SocketResponse{},
		closed:  make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

func opOf(b []byte) (gateway.Op, error) {
	op, ok := gateway.FrameOp(b)
	if !ok {
		return 0, errors.New("sockettest: not a frame")
	}
	return op, nil
}

func (c *Client) readLoop() {
	defer close(c.closed)
	for {
		_, b, err := c.ws.Read(c.ctx)
		if err != nil {
			c.mu.Lock()
			c.readErr = err
			c.mu.Unlock()
			return
		}
		op, err := opOf(b)
		if err != nil {
			continue
		}
		switch op {
		case gateway.OpResponse:
			resp, err := gateway.DecodeResponseFrame(b)
			if err != nil {
				continue
			}
			c.mu.Lock()
			c.order = append(c.order, resp.ID)
			if ch, ok := c.pending[resp.ID]; ok {
				ch <- resp
			} else {
				// An id this client never sent: nothing waits for it.
			}
			c.mu.Unlock()
		case gateway.OpError:
			var elems []cbor.RawMessage
			_ = cborx.Unmarshal(b, &elems)
			var payload []any
			if len(elems) == 4 {
				_ = cborx.Unmarshal(elems[3], &payload)
			}
			c.mu.Lock()
			if len(payload) == 3 {
				c.errs = append(c.errs, fmt.Sprint(payload[1]))
			}
			c.mu.Unlock()
		}
	}
}

// Send writes one request frame and returns its id without waiting for the answer.
func (c *Client) Send(method, route string, body, idem []byte) (uint64, error) {
	c.mu.Lock()
	c.nextID++
	reqID := c.nextID
	c.pending[reqID] = make(chan gateway.SocketResponse, 1)
	c.mu.Unlock()
	if body == nil {
		body = []byte{}
	}
	frame, err := gateway.EncodeRequestFrame(gateway.SocketRequest{ID: reqID, Method: method, Route: route, Body: body, Idem: idem})
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(c.ctx, 30*time.Second)
	defer cancel()
	return reqID, c.ws.Write(ctx, websocket.MessageBinary, frame)
}

// Await waits for the response to id.
func (c *Client) Await(reqID uint64, timeout time.Duration) (gateway.SocketResponse, error) {
	c.mu.Lock()
	ch, ok := c.pending[reqID]
	c.mu.Unlock()
	if !ok {
		return gateway.SocketResponse{}, fmt.Errorf("sockettest: no request %d is pending", reqID)
	}
	defer func() {
		c.mu.Lock()
		delete(c.pending, reqID)
		c.mu.Unlock()
	}()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case resp := <-ch:
		return resp, nil
	case <-c.closed:
		select {
		case resp := <-ch:
			return resp, nil
		default:
		}
		c.mu.Lock()
		err := c.readErr
		c.mu.Unlock()
		return gateway.SocketResponse{}, fmt.Errorf("sockettest: connection closed: %w", err)
	case <-t.C:
		return gateway.SocketResponse{}, fmt.Errorf("sockettest: request %d unanswered after %s", reqID, timeout)
	}
}

// SendRaw writes an already-encoded frame.
func (c *Client) SendRaw(frame []byte) error {
	ctx, cancel := context.WithTimeout(c.ctx, 30*time.Second)
	defer cancel()
	return c.ws.Write(ctx, websocket.MessageBinary, frame)
}

// CloseStatus is the close code the instance closed the connection with, once Done is closed, or
// -1.
func (c *Client) CloseStatus() websocket.StatusCode {
	c.mu.Lock()
	defer c.mu.Unlock()
	return websocket.CloseStatus(c.readErr)
}

// Done is closed once the connection's read side has ended.
func (c *Client) Done() <-chan struct{} { return c.closed }

// Do sends one request and waits for its response.
func (c *Client) Do(method, route string, body, idem []byte) (gateway.SocketResponse, error) {
	reqID, err := c.Send(method, route, body, idem)
	if err != nil {
		return gateway.SocketResponse{}, err
	}
	return c.Await(reqID, 30*time.Second)
}

// Order is the ids of every response received so far, in arrival order.
func (c *Client) Order() []uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]uint64(nil), c.order...)
}

// Errors is the codes of every error frame received so far.
func (c *Client) Errors() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.errs...)
}

// Close closes the connection.
func (c *Client) Close() { _ = c.ws.Close(websocket.StatusNormalClosure, "") }
