package gatewayclient

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

const (
	defaultHandshakeTimeout = 15 * time.Second
	defaultWriteTimeout     = 10 * time.Second
	defaultQueueSize        = 64
	maxQueueSize            = 4096
	reconnectBase           = 500 * time.Millisecond
	reconnectCap            = 30 * time.Second
)

// CredentialFunc is called for every connection attempt so a renewed session
// can be used after a transient disconnect. Credential contents are never logged.
type CredentialFunc func(context.Context) (string, error)

type Config struct {
	Logger              *slog.Logger
	Client              *client.Client
	HTTPClient          *http.Client
	AllowLoopbackHTTP   bool
	ExpectedDeviceID    id.ID
	ExpectedUserID      id.ID
	HandshakeTimeout    time.Duration
	WriteTimeout        time.Duration
	HeartbeatAckTimeout time.Duration
	QueueSize           int
}

type Client struct {
	log                            *slog.Logger
	origin                         *url.URL
	http                           *http.Client
	instance, device, user         id.ID
	handshakeTimeout, writeTimeout time.Duration
	heartbeatAckTimeout            time.Duration
	events                         chan Delivery
	mu                             sync.Mutex
	current                        *connection
	state                          resumeState
	closed                         bool
	running                        bool
	streamID                       uint64
	ackAvailable                   map[uint64][32]byte
}

// Delivery is one validated frame handed to the durable consumer. Ack is bound
// to the stream generation that produced it, so a stale handler cannot advance
// a newly identified stream after reconnect or resynchronization.
type Delivery struct {
	Frame  Frame
	client *Client
	stream uint64
	ackN   uint64
}

func (d Delivery) Ack() error {
	if d.client == nil || d.Frame.N == 0 {
		return ErrAckOrder
	}
	return d.client.ack(d.stream, d.ackN)
}

type resumeState struct {
	token           []byte
	generation, ack uint64
}
type outbound struct {
	op      Opcode
	group   *id.ID
	payload []any
	done    chan error
}
type connection struct {
	ws          *websocket.Conn
	ctx         context.Context
	cancel      context.CancelFunc
	writes      chan outbound
	maxFrame    uint64
	heartbeat   time.Duration
	ackDeadline time.Duration
}

// New pins the gateway to the same already-validated HTTP client origin. Callers
// pass the same custom HTTP transport when the HTTP client uses private TLS roots.
// A plain-HTTP origin is accepted only when the caller explicitly enables
// same-host loopback use, matching bot/client.Config's rule.
func New(cfg Config) (*Client, error) {
	if cfg.Client == nil || cfg.ExpectedDeviceID.IsZero() || cfg.ExpectedUserID.IsZero() {
		return nil, ErrProtocol
	}
	u, err := url.Parse(cfg.Client.Origin())
	if err != nil || u == nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, ErrProtocol
	}
	if u.Scheme == "http" {
		host := strings.ToLower(u.Hostname())
		ip := net.ParseIP(host)
		if !cfg.AllowLoopbackHTTP || ((ip == nil || !ip.IsLoopback()) && host != "localhost") {
			return nil, ErrProtocol
		}
	}
	h := cfg.HTTPClient
	if h == nil {
		h = &http.Client{}
	}
	hcopy := *h
	hcopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	hcopy.Jar = nil
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = defaultHandshakeTimeout
	}
	if cfg.HandshakeTimeout > 2*time.Minute {
		return nil, ErrProtocol
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = defaultWriteTimeout
	}
	if cfg.WriteTimeout > time.Minute {
		return nil, ErrProtocol
	}
	if cfg.HeartbeatAckTimeout <= 0 {
		cfg.HeartbeatAckTimeout = 10 * time.Second
	}
	if cfg.HeartbeatAckTimeout > 2*time.Minute {
		return nil, ErrProtocol
	}
	if cfg.QueueSize == 0 {
		cfg.QueueSize = defaultQueueSize
	}
	if cfg.QueueSize < 1 || cfg.QueueSize > maxQueueSize {
		return nil, ErrProtocol
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Client{log: cfg.Logger, origin: u, http: &hcopy, instance: cfg.Client.InstanceID(), device: cfg.ExpectedDeviceID, user: cfg.ExpectedUserID,
		handshakeTimeout: cfg.HandshakeTimeout, writeTimeout: cfg.WriteTimeout, heartbeatAckTimeout: cfg.HeartbeatAckTimeout,
		events: make(chan Delivery, cfg.QueueSize), ackAvailable: make(map[uint64][32]byte)}, nil
}

// Events returns the bounded stream of validated server deliveries. For every
// frame with N > 0, the consumer must call Delivery.Ack only after durable handling or after
// persisting a durable authoritative-sync intent. Socket receipt alone is not an
// acknowledgement. Frames with N == 0 are ephemeral and cannot be acknowledged.
func (c *Client) Events() <-chan Delivery { return c.events }

func (c *Client) ack(stream, n uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if stream == 0 || stream != c.streamID {
		return ErrStaleDelivery
	}
	if n == 0 || n != c.state.ack+1 {
		return ErrAckOrder
	}
	if _, ok := c.ackAvailable[n]; !ok {
		return ErrAckOrder
	}
	delete(c.ackAvailable, n)
	c.state.ack = n
	return nil
}

// Run owns reconnect, heartbeat, and frame-reader goroutines until ctx is
// cancelled or a terminal session/protocol error occurs.
func (c *Client) Run(ctx context.Context, credential CredentialFunc) error {
	if ctx == nil || credential == nil {
		return ErrProtocol
	}
	ctx = client.WithoutCooperativeStop(client.WithBackground(ctx))
	c.mu.Lock()
	if c.running || c.closed {
		c.mu.Unlock()
		return ErrTerminal
	}
	c.running = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.closed = true
		current := c.current
		c.current = nil
		c.mu.Unlock()
		if current != nil {
			current.cancel()
			_ = current.ws.Close(websocket.StatusNormalClosure, "stopped")
		}
		close(c.events)
	}()
	failures := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		token, err := credential(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return ErrTerminal
		}
		if strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\r\n") {
			return ErrTerminal
		}
		started := time.Now()
		fresh, after, err := c.runConnection(ctx, token)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, ErrTerminal) || errors.Is(err, ErrVersion) || errors.Is(err, ErrIdentity) {
			return err
		}
		if errors.Is(err, ErrProtocol) {
			return err
		}
		if errors.Is(err, errReplayGap) {
			fresh = true
		}
		if errors.Is(err, ErrBackpressure) {
			fresh = true
		}
		if fresh {
			c.resetResume()
		}
		if time.Since(started) >= 10*time.Second {
			failures = 0
		}
		if failures < 16 {
			failures++
		}
		delay := reconnectDelay(failures, after)
		c.log.Info("bot gateway reconnect scheduled", "reason", err.Error(), "fresh_identify", fresh, "backoff", delay)
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}

func maxFrameReadLimit(value uint64) int64 {
	if value > MaxGatewayFrameBytes {
		return MaxGatewayFrameBytes
	}
	return int64(value)
}

func boundedMilliseconds(value uint64) time.Duration {
	if value > 120000 {
		value = 120000
	}
	return time.Duration(value) * time.Millisecond
}

func (c *Client) runConnection(parent context.Context, token string) (fresh bool, after time.Duration, runErr error) {
	ctx, cancel := context.WithTimeout(parent, c.handshakeTimeout)
	u := *c.origin
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = "/gateway"
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+token)
	ws, resp, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{HTTPClient: c.http, Subprotocols: []string{"dilla.v1"}, HTTPHeader: headers, CompressionMode: websocket.CompressionDisabled})
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if err != nil {
		if resp != nil && resp.StatusCode >= 300 && resp.StatusCode < 400 {
			cancel()
			return false, 0, ErrProtocol
		}
		if resp != nil && resp.StatusCode >= 400 && resp.StatusCode < 500 {
			cancel()
			return false, 0, ErrTerminal
		}
		cancel()
		return false, 0, errTransport
	}
	defer func() {
		cancel()
		if parent.Err() != nil {
			_ = ws.CloseNow()
		} else {
			_ = ws.Close(websocket.StatusNormalClosure, "reconnect")
		}
	}()
	if ws.Subprotocol() != "dilla.v1" {
		return false, 0, ErrProtocol
	}
	ws.SetReadLimit(MaxGatewayFrameBytes)
	messageType, raw, err := ws.Read(ctx)
	if err != nil {
		fresh, classified := closeOutcome(err)
		return fresh, 0, classified
	}
	if messageType != websocket.MessageBinary {
		return false, 0, ErrProtocol
	}
	helloFrame, err := DecodeServerFrame(raw, MaxGatewayFrameBytes)
	if err != nil || helloFrame.Op != OpHello {
		return false, 0, ErrProtocol
	}
	h := helloFrame.Hello
	if subtle.ConstantTimeCompare(h.InstanceID[:], c.instance[:]) != 1 {
		return false, 0, ErrIdentity
	}
	if !hasVersion(h.WireVersions, 3) || !hasVersion(h.E2EEVersions, 1) || !hasVersion(h.MediaVersions, 1) {
		return false, 0, ErrVersion
	}
	ws.SetReadLimit(maxFrameReadLimit(h.MaxFrameBytes))
	c.mu.Lock()
	rs := resumeState{token: append([]byte(nil), c.state.token...), generation: c.state.generation, ack: c.state.ack}
	c.mu.Unlock()
	resume := len(rs.token) == 32 && rs.generation == h.Generation
	if len(rs.token) == 32 && rs.generation != h.Generation {
		fresh = true
		c.resetResume()
		rs = resumeState{}
	}
	var op Opcode
	var payload []any
	if resume {
		op = OpResume
		payload = []any{"", rs.token, rs.generation, rs.ack}
	} else {
		op = OpIdentify
		payload = []any{"", uint64(3), uint64(1), uint64(1), uint64(0)}
	}
	frame, err := encodeFrame(op, 0, nil, payload)
	if err != nil {
		return fresh, 0, ErrProtocol
	}
	writeCtx, wcancel := context.WithTimeout(ctx, c.writeTimeout)
	err = ws.Write(writeCtx, websocket.MessageBinary, frame)
	wcancel()
	if err != nil {
		return fresh, 0, errTransport
	}
	// The server's resumed frame is stamped after the replay range but queued
	// before it. Buffer exactly that bounded range and expose it in n order.
	var first Frame
	var firstRaw []byte
	readCtx, rcancel := context.WithTimeout(ctx, c.handshakeTimeout)
	messageType, raw, err = ws.Read(readCtx)
	rcancel()
	if err != nil {
		fnew, classified := closeOutcome(err)
		return fresh || fnew, 0, classified
	}
	if messageType != websocket.MessageBinary {
		return fresh, 0, ErrProtocol
	}
	first, err = DecodeServerFrame(raw, h.MaxFrameBytes)
	if err != nil {
		return fresh, 0, ErrProtocol
	}
	firstRaw = append(firstRaw[:0], raw...)
	if first.Op == OpError {
		return fresh, 0, classifyServerError(first.ErrorCode, true)
	}
	if first.Op == OpInvalidSession {
		if resume {
			return true, 0, errReplayGap
		}
		return false, 0, ErrTerminal
	}
	if resume && first.Op == OpResumed {
		if err = c.acceptResumed(first, rs.ack); err != nil {
			return true, 0, errReplayGap
		}
		first.ResumeBarrier = true
	} else if first.Op == OpReady {
		if resume {
			return true, 0, ErrProtocol
		}
		if err = c.acceptReady(first, h); err != nil {
			return false, 0, err
		}
		fresh = false
	} else if first.Op == OpReconnect {
		if first.ReconnectAfterMS != nil {
			after = boundedMilliseconds(*first.ReconnectAfterMS)
		}
		return fresh, after, errors.New("gateway requested reconnect")
	} else {
		// During identify only ready (or a refusal/reconnect) is legal; during
		// resume only resumed (or a refusal/reconnect) is legal. Never wait for a
		// second frame after a well-formed frame in the wrong handshake state.
		return fresh, 0, ErrProtocol
	}
	cx, cancel := context.WithCancel(parent)
	conn := &connection{ws: ws, ctx: cx, cancel: cancel, writes: make(chan outbound, 32), maxFrame: h.MaxFrameBytes, heartbeat: boundedMilliseconds(h.HeartbeatMS)}
	conn.ackDeadline = c.heartbeatAckTimeout
	c.mu.Lock()
	c.current = conn
	c.mu.Unlock()
	rawFrames := make(chan []byte, 32)
	readerDone := make(chan error, 1)
	writerDone := make(chan error, 1)
	var ackTimer *time.Timer
	var ackC <-chan time.Time
	go c.readPump(conn, rawFrames, readerDone)
	go c.writePump(conn, writerDone)
	defer func() {
		cancel()
		if parent.Err() != nil {
			// Cancellation must not wait for a peer that never answers a WebSocket
			// close frame. Close performs a handshake with its own five-second wait.
			_ = ws.CloseNow()
		} else {
			_ = ws.Close(websocket.StatusNormalClosure, "reconnect")
		}
		<-readerDone
		<-writerDone
		if ackTimer != nil {
			ackTimer.Stop()
		}
		c.mu.Lock()
		if c.current == conn {
			c.current = nil
		}
		c.mu.Unlock()
	}()
	// Ready is a durable cursor digest hint; caller owns its reconciliation.
	if first.Op == OpReady {
		if _, err := c.enqueueEvent(first, firstRaw); err != nil {
			return true, 0, err
		}
	}
	var replay *Resumed
	if first.Op == OpResumed {
		replay = first.Resumed
	}
	var replayNext uint64
	nextN := first.N + 1
	if replay != nil {
		replayNext = replay.ReplayedFrom
		if replay.ReplayedTo+1 == replay.ReplayedFrom {
			if _, err := c.enqueueEvent(first, firstRaw); err != nil {
				return true, 0, err
			}
			replay = nil
		}
	}
	ticker := time.NewTicker(maxDuration(conn.heartbeat/2, 250*time.Millisecond))
	defer ticker.Stop()
	for {
		select {
		case <-parent.Done():
			return fresh, 0, nil
		case err := <-readerDone:
			readerDone <- err
			if err == nil {
				err = errors.New("gateway read stopped")
			}
			fnew, classified := closeOutcome(err)
			return fresh || fnew, 0, classified
		case err := <-writerDone:
			writerDone <- err
			return fresh, 0, errTransport
		case raw := <-rawFrames:
			f, e := DecodeServerFrame(raw, conn.maxFrame)
			if e != nil {
				if dropMalformedEphemeral(raw) {
					continue
				}
				return fresh, 0, ErrProtocol
			}
			switch f.Op {
			case OpHeartbeatAck:
				if ackTimer != nil {
					ackTimer.Stop()
					ackTimer = nil
					ackC = nil
				}
			case OpReconnect:
				if f.ReconnectAfterMS != nil {
					after = boundedMilliseconds(*f.ReconnectAfterMS)
				}
				return fresh, after, errors.New("gateway requested reconnect")
			case OpInvalidSession:
				if !f.InvalidSession.Resumable {
					return fresh, 0, ErrTerminal
				}
				return true, 0, errReplayGap
			case OpError:
				if err := classifyServerError(f.ErrorCode, false); err != nil {
					return fresh, 0, err
				}
				if _, err := c.enqueueEvent(f, raw); err != nil {
					return true, 0, err
				}
			default:
				if replay != nil {
					if f.N != replayNext || replayNext > replay.ReplayedTo {
						return true, 0, errReplayGap
					}
					f.Replayed = true
					c.mu.Lock()
					alreadyDurable := f.N <= c.state.ack
					c.mu.Unlock()
					if !alreadyDurable {
						if _, err := c.enqueueEvent(f, raw); err != nil {
							return true, 0, err
						}
					}
					replayNext++
					if replayNext == replay.ReplayedTo+1 {
						if _, err := c.enqueueEvent(first, firstRaw); err != nil {
							return true, 0, err
						}
						replay = nil
					}
					continue
				}
				if f.Op == OpHello || f.Op == OpReady || f.Op == OpResumed {
					return false, 0, ErrProtocol
				}
				if f.N > 0 {
					if f.N == ^uint64(0) || f.N != nextN {
						return true, 0, errReplayGap
					}
					nextN++
				}
				if _, err := c.enqueueEvent(f, raw); err != nil {
					return true, 0, err
				}
			}
		case <-ticker.C:
			if ackTimer != nil {
				continue
			}
			c.mu.Lock()
			lastN := c.state.ack
			c.mu.Unlock()
			if err := c.sendFrame(parent, OpHeartbeat, nil, []any{lastN, uint64(1)}); err != nil {
				return fresh, 0, err
			}
			ackTimer = time.NewTimer(conn.ackDeadline)
			ackC = ackTimer.C
		case <-ackC:
			return fresh, 0, errors.New("gateway heartbeat acknowledgement timeout")
		}
	}
}

// classifyServerError maps only the protocol's stable code. Human-readable
// server detail is intentionally discarded so it cannot leak credentials or
// frame contents through the transport error surface.
func classifyServerError(code string, handshake bool) error {
	switch code {
	case "E_VERSION":
		return ErrVersion
	case "E_UNAUTHENTICATED":
		return ErrTerminal
	case "E_FRAME_LIMIT", "E_FRAME_CBOR", "E_FRAME_SHAPE", "E_FRAME_TYPE":
		return ErrProtocol
	case "E_FORBIDDEN", "E_NOT_FOUND", "E_RATE_LIMITED":
		if handshake {
			return ErrTerminal
		}
		return nil
	default:
		return ErrProtocol
	}
}

func (c *Client) acceptReady(f Frame, h *Hello) error {
	r := f.Ready
	if r == nil || r.DeviceID != c.device || r.UserID != c.user || r.Generation != h.Generation {
		return ErrIdentity
	}
	if r.WireVersion != 3 || r.E2EEVersion != 1 || r.MediaVersion != 1 {
		return ErrVersion
	}
	c.mu.Lock()
	c.state = resumeState{token: append([]byte(nil), r.ResumeToken...), generation: r.Generation}
	c.streamID++
	if c.streamID == 0 {
		c.streamID++
	}
	c.ackAvailable = make(map[uint64][32]byte)
	c.mu.Unlock()
	if f.N != 1 {
		return ErrProtocol
	}
	return nil
}
func (c *Client) acceptResumed(f Frame, ack uint64) error {
	r := f.Resumed
	if ack >= ^uint64(0)-1 || r == nil || r.ReplayedFrom != ack+1 || r.ReplayedTo < ack || r.ReplayedTo >= ^uint64(0)-1 || r.ReplayedTo+1 != f.N {
		return ErrProtocol
	}
	c.mu.Lock()
	c.state.token = append([]byte(nil), r.ResumeToken...)
	c.mu.Unlock()
	return nil
}

func (c *Client) enqueueEvent(f Frame, raw []byte) (bool, error) {
	hash := sha256.Sum256(raw)
	c.mu.Lock()
	stream := c.streamID
	if f.N > 0 {
		if previous, ok := c.ackAvailable[f.N]; ok {
			c.mu.Unlock()
			if previous == hash {
				return false, nil
			}
			return false, ErrProtocol
		}
		if len(c.ackAvailable) >= cap(c.events) {
			c.mu.Unlock()
			return false, ErrBackpressure
		}
		c.ackAvailable[f.N] = hash
	}
	c.mu.Unlock()
	d := Delivery{Frame: f, client: c, stream: stream, ackN: f.N}
	select {
	case c.events <- d:
		return true, nil
	default:
		if f.N > 0 {
			c.mu.Lock()
			delete(c.ackAvailable, f.N)
			c.mu.Unlock()
		}
		return false, ErrBackpressure
	}
}
func (c *Client) resetResume() {
	c.mu.Lock()
	c.state = resumeState{}
	c.streamID++
	if c.streamID == 0 {
		c.streamID++
	}
	c.ackAvailable = make(map[uint64][32]byte)
	for {
		select {
		case <-c.events:
		default:
			c.mu.Unlock()
			return
		}
	}
}
func (c *Client) readPump(conn *connection, out chan<- []byte, done chan<- error) {
	for {
		messageType, b, err := conn.ws.Read(conn.ctx)
		if err != nil {
			done <- err
			return
		}
		if messageType != websocket.MessageBinary {
			done <- ErrProtocol
			return
		}
		select {
		case out <- b:
		case <-conn.ctx.Done():
			done <- conn.ctx.Err()
			return
		default:
			done <- ErrBackpressure
			return
		}
	}
}
func (c *Client) writePump(conn *connection, done chan<- error) {
	for {
		select {
		case <-conn.ctx.Done():
			done <- conn.ctx.Err()
			return
		case w := <-conn.writes:
			ctx, cancel := context.WithTimeout(conn.ctx, c.writeTimeout)
			b, err := encodeFrame(w.op, 0, w.group, w.payload)
			if err == nil {
				err = conn.ws.Write(ctx, websocket.MessageBinary, b)
			}
			cancel()
			if w.done != nil {
				w.done <- err
			}
			if err != nil {
				done <- err
				return
			}
		}
	}
}

func closeOutcome(err error) (bool, error) {
	if errors.Is(err, ErrBackpressure) {
		return true, ErrBackpressure
	}
	if errors.Is(err, ErrProtocol) {
		return false, ErrProtocol
	}
	code := websocket.CloseStatus(err)
	switch code {
	case 4003, 4004:
		return false, ErrTerminal
	case 4009:
		return true, errHeartbeatOverdue
	case 4006:
		return false, ErrVersion
	case 4002:
		return false, ErrProtocol
	case 4007:
		return true, errReplayGap
	default:
		return false, errTransport
	}
}

func (c *Client) sendFrame(ctx context.Context, op Opcode, group *id.ID, payload []any) error {
	c.mu.Lock()
	conn := c.current
	c.mu.Unlock()
	if conn == nil {
		return ErrProtocol
	}
	done := make(chan error, 1)
	select {
	case conn.writes <- outbound{op: op, group: group, payload: payload, done: done}:
	case <-ctx.Done():
		return ctx.Err()
	case <-conn.ctx.Done():
		return ErrProtocol
	}
	select {
	case err := <-done:
		if err != nil {
			return errTransport
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-conn.ctx.Done():
		return ErrProtocol
	}
}

func (c *Client) Subscribe(ctx context.Context, groups []id.ID) error {
	return c.groupList(ctx, OpSubscribe, groups)
}
func (c *Client) Unsubscribe(ctx context.Context, groups []id.ID) error {
	return c.groupList(ctx, OpUnsubscribe, groups)
}
func (c *Client) groupList(ctx context.Context, op Opcode, groups []id.ID) error {
	if len(groups) == 0 || len(groups) > 4096 {
		return ErrProtocol
	}
	seen := map[id.ID]struct{}{}
	vals := make([]any, 0, len(groups))
	for _, g := range groups {
		if g.IsZero() {
			return ErrProtocol
		}
		if _, ok := seen[g]; ok {
			return ErrProtocol
		}
		seen[g] = struct{}{}
		vals = append(vals, g)
	}
	return c.sendFrame(ctx, op, nil, []any{vals})
}
func (c *Client) CommitAck(ctx context.Context, group id.ID, round uint64) error {
	if group.IsZero() {
		return ErrProtocol
	}
	return c.sendFrame(ctx, OpCommitAck, &group, []any{round})
}

func encodeFrame(op Opcode, n uint64, group *id.ID, payload []any) ([]byte, error) {
	var gid any
	if group != nil {
		gid = *group
	}
	b, err := cborx.Marshal([]any{uint64(op), n, gid, payload})
	if err != nil {
		return nil, ErrProtocol
	}
	if len(b) > MaxGatewayFrameBytes {
		return nil, ErrProtocol
	}
	return b, nil
}
func hasVersion(v []uint64, want uint64) bool {
	for _, n := range v {
		if n == want {
			return true
		}
	}
	return false
}
func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
func reconnectDelay(failures int, after time.Duration) time.Duration {
	d := reconnectBase
	for i := 1; i < failures && d < reconnectCap; i++ {
		d *= 2
	}
	if d > reconnectCap {
		d = reconnectCap
	}
	jitterMax := d / 2
	var jitter time.Duration
	if jitterMax > 0 {
		if n, e := rand.Int(rand.Reader, big.NewInt(int64(jitterMax))); e == nil {
			jitter = time.Duration(n.Int64())
		}
	}
	if d+jitter < after {
		return after
	}
	return d + jitter
}
