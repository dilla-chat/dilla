package gatewayclient

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"net/http"
	"time"

	"github.com/dilla-chat/dilla/internal/cborx"
)

// Bot v2: app requests over the gateway socket. Client implements client.Transport: one request frame (op 59) per
// attempt, answered by a response frame (op 60) with the same id on the same connection, in any order.
//
// Ordering is the same as over HTTPS: concurrent Do calls are written in no promised order and the instance runs them
// concurrently; only calls a caller made one after the other (each awaited) are applied in that order.
//
// Retry rules, the same as the app's SocketRequests:
//   - While there is no connection a request waits for the next one (or the caller's deadline).
//   - A request whose answer was lost with its connection is sent again on the next one, with the same idempotency
//     key, so the instance's replay record (and the route's own id) answers it instead of applying it twice. The record
//     lives in one dillad process for 5 minutes, so a written mutation is NOT sent again on a connection to another
//     incarnation (another boot id, restore generation or instance id) or once ResendWindow has passed since its first
//     write: Do returns ErrOutcomeUnknown, and the caller treats the mutation as maybe applied.
//   - 429 E_BUSY (the instance's in-flight or byte budget): sent again once another answer frees room, or after a
//     short jittered pause. It is never returned to the caller.
//   - 503 E_UNAVAILABLE (the instance's dispatch deadline, or its shutdown): sent again with the same key after a
//     jittered backoff, at most unavailableAttempts times in all.
//   - Every other answer, 409 (a key reused for another request) and 401/426 included, is returned as it is.

const (
	// MaxRequestsInFlight is the instance's per-connection cap (gateway.MaxInFlightRequests); more only earns E_BUSY.
	MaxRequestsInFlight = 64
	busyPause           = 100 * time.Millisecond
	unavailableAttempts = 5
	unavailableBase     = 500 * time.Millisecond
	unavailableCap      = 8 * time.Second
	maxRetryAfter       = time.Minute
	// ResendWindow is how long after its first write a mutation may be written again: below the 5 minutes the instance
	// keeps its answer (api.SocketDispatchOptions.ReplayWindow).
	ResendWindow = 4 * time.Minute
)

// ErrOutcomeUnknown is a written mutation that is not written again: its answer was lost and the instance that took it
// may be gone, so it may or may not have been applied.
var ErrOutcomeUnknown = errors.New("bot gateway: the request was written and its answer was lost; it may have been applied")

// ErrIdempotencyRequired is a mutation sent without an idempotency key: a programming error, never sent.
var ErrIdempotencyRequired = errors.New("bot gateway: a mutation needs an idempotency key")

// errConnectionLost ends one attempt whose connection went away before its answer arrived.
var errConnectionLost = errors.New("bot gateway: connection lost before the answer")

// Do sends one app request over the socket and returns the instance's status and body. A transport error means no
// answer was received (the caller's context ended, or the client stopped for good).
func (c *Client) Do(ctx context.Context, method, route string, body, idem []byte) (int, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if method != http.MethodGet && method != http.MethodHead && len(idem) == 0 {
		return 0, nil, ErrIdempotencyRequired
	}
	if len(method) == 0 || len(method) > maxRequestMethod || len(route) == 0 || len(route) > maxRequestRoute ||
		len(body) > MaxRequestBodyBytes || len(idem) > maxRequestIdem {
		return 0, nil, ErrProtocol
	}
	unavailable := 0
	var wrote bool
	var wroteUnder string
	var firstWrite time.Time
	mutation := method != http.MethodGet && method != http.MethodHead
	for {
		conn, err := c.waitConnection(ctx)
		if err != nil {
			return 0, nil, err
		}
		if mutation && wrote && (conn.incarnation != wroteUnder || time.Since(firstWrite) >= ResendWindow) {
			return 0, nil, ErrOutcomeUnknown
		}
		select {
		case c.slots <- struct{}{}:
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		case <-conn.ctx.Done():
			continue
		}
		answer, sent, err := c.roundTrip(ctx, conn, method, route, body, idem)
		<-c.slots
		if sent && !wrote {
			wrote, wroteUnder, firstWrite = true, conn.incarnation, time.Now()
		}
		if errors.Is(err, errConnectionLost) {
			continue
		}
		if err != nil {
			return 0, nil, err
		}
		code, retryAfter := refusalOf(answer)
		switch {
		case answer.Status == http.StatusTooManyRequests && code == "E_BUSY":
			if err := c.waitForRoom(ctx); err != nil {
				return 0, nil, err
			}
			continue
		case answer.Status == http.StatusServiceUnavailable && code == "E_UNAVAILABLE" && unavailable+1 < unavailableAttempts:
			unavailable++
			if err := sleepContext(ctx, unavailableDelay(unavailable, retryAfter)); err != nil {
				return 0, nil, err
			}
			continue
		}
		return answer.Status, answer.Body, nil
	}
}

// waitConnection returns the current identified connection, waiting for one while the client reconnects.
func (c *Client) waitConnection(ctx context.Context) (*connection, error) {
	for {
		c.mu.Lock()
		conn, closed, changed := c.current, c.closed, c.connChanged
		c.mu.Unlock()
		if conn != nil && conn.ctx.Err() == nil {
			return conn, nil
		}
		if closed {
			return nil, ErrTerminal
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// setCurrentLocked replaces the current connection and wakes every request waiting for one. c.mu is held.
func (c *Client) setCurrentLocked(conn *connection) {
	c.current = conn
	if c.connChanged != nil {
		close(c.connChanged)
	}
	c.connChanged = make(chan struct{})
}

// signalRoom wakes every request paused by E_BUSY: an answer means the instance has one more free slot.
func (c *Client) signalRoom() {
	c.mu.Lock()
	if c.room != nil {
		close(c.room)
	}
	c.room = make(chan struct{})
	c.mu.Unlock()
}

func (c *Client) waitForRoom(ctx context.Context) error {
	c.mu.Lock()
	room := c.room
	c.mu.Unlock()
	t := time.NewTimer(busyPause + jitter(busyPause))
	defer t.Stop()
	select {
	case <-room:
	case <-t.C:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (c *Client) roundTrip(ctx context.Context, conn *connection, method, route string, body, idem []byte) (ResponseFrame, bool, error) {
	requestID := c.nextRequest.Add(1)
	frame, err := EncodeRequestFrame(requestID, method, route, body, idem)
	if err != nil {
		return ResponseFrame{}, false, err
	}
	answer := make(chan ResponseFrame, 1)
	conn.pendingMu.Lock()
	conn.pending[requestID] = answer
	conn.pendingMu.Unlock()
	defer func() {
		conn.pendingMu.Lock()
		delete(conn.pending, requestID)
		conn.pendingMu.Unlock()
	}()
	written := make(chan error, 1)
	select {
	case conn.writes <- outbound{op: OpRequest, raw: frame, done: written}:
	case <-ctx.Done():
		return ResponseFrame{}, false, ctx.Err()
	case <-conn.ctx.Done():
		return ResponseFrame{}, false, errConnectionLost
	}
	// Queued for the writer: from here the frame may have reached the instance.
	sent := true
	select {
	case err := <-written:
		if err != nil {
			return ResponseFrame{}, sent, errConnectionLost
		}
	case <-ctx.Done():
		return ResponseFrame{}, sent, ctx.Err()
	case <-conn.ctx.Done():
		return ResponseFrame{}, sent, errConnectionLost
	}
	select {
	case r := <-answer:
		return r, sent, nil
	case <-ctx.Done():
		return ResponseFrame{}, sent, ctx.Err()
	case <-conn.ctx.Done():
		// An answer read just before the connection ended still counts.
		select {
		case r := <-answer:
			return r, sent, nil
		default:
			return ResponseFrame{}, sent, errConnectionLost
		}
	}
}

// refusalOf reads the stable code and retry_after of a refusal body ([code, detail, retry_after_ms|null, ...]).
func refusalOf(r ResponseFrame) (string, *uint64) {
	if r.Status < 400 {
		return "", nil
	}
	var a []any
	if cborx.Unmarshal(r.Body, &a) != nil || len(a) < 3 {
		return "", nil
	}
	code, _ := a[0].(string)
	if n, ok := a[2].(uint64); ok {
		return code, &n
	}
	return code, nil
}

func unavailableDelay(attempt int, retryAfterMS *uint64) time.Duration {
	d := unavailableBase << (attempt - 1)
	if d > unavailableCap || d <= 0 {
		d = unavailableCap
	}
	if retryAfterMS != nil {
		d = time.Duration(min(*retryAfterMS, uint64(maxRetryAfter/time.Millisecond))) * time.Millisecond
	}
	return d + jitter(250*time.Millisecond)
}

func jitter(upTo time.Duration) time.Duration {
	if upTo <= 0 {
		return 0
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(upTo)))
	if err != nil {
		return 0
	}
	return time.Duration(n.Int64())
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
