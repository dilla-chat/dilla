package gateway

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/fxamacker/cbor/v2"
	"golang.org/x/time/rate"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

// connState is where a connection is in the handshake.
type connState uint8

const (
	stateNew connState = iota
	stateReady
	stateClosed
)

// conn is one WebSocket connection. Fan-out is per connection, not per device: three tabs of one
// device have three n counters and three rings.
type conn struct {
	mu           sync.Mutex
	state        connState
	userID       id.ID
	deviceID     id.ID
	scope        auth.Scope
	isBot        bool
	pairingGroup *id.ID // ScopeProvisional only; see interfaces §2.2 point 4
	tokenHash    []byte
	resume       []byte // 32 CSPRNG bytes, rotated on every resumed
	// noResume is set when this connection was closed with a code protocol/02 marks as NOT
	// resumable (4004 session_revoked, 4009 session_timeout). suspend refuses to park such a
	// connection in the resume window, so its token dies with the socket.
	noResume    bool
	n           uint64
	groups      map[id.ID]struct{}
	lastLive    time.Time
	ready       time.Time
	suspendedAt time.Time
	pingAt      time.Time
	// The versions this connection negotiated at identify, echoed in ready.
	wire, e2ee, media uint64

	ring   *ring
	writer *writer
	gw     *Gateway
}

// denyResume marks the connection dead for resume purposes. It is called before the socket is
// closed with a non-resumable code, and it is what suspend and CloseDevice read.
func (c *conn) denyResume() {
	c.mu.Lock()
	c.noResume = true
	c.mu.Unlock()
}

// currentWriter reads the connection's writer under the lock. The writer is REPLACED on a resume —
// the connection comes back over a new sink — so every reader takes the lock: a producer fanning
// out while a resume swaps the field is otherwise a data race the -race gate catches.
func (c *conn) currentWriter() *writer {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writer
}

// send stamps, rings and queues one frame.
//
// Allocating n, ringing the frame and handing it to the writer is ONE critical section. Two
// producers delivering to the same connection concurrently — DeliverGroup for two groups that
// share a device, or a Welcome racing a group fan-out — would otherwise be stamped n = k and
// n = k+1 and then enqueued in the opposite order, so the client sees n go backwards and answers
// with close 4007. `go test -race` cannot see that: it is an ordering bug, not a data race.
// The writer's own enqueue is non-blocking, so the lock is never held across I/O.
func (c *conn) send(f Frame) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var n uint64
	if f.Replay {
		c.n++
		n = c.n
		if b, err := Encode(f, n); err == nil {
			c.ring.add(n, b, f.Scope)
		}
	}
	if c.writer == nil {
		return
	}
	c.writer.enqueue(f, n)
}

// mark records a liveness signal: ready, resumed or a heartbeat frame. Online is defined over
// this timestamp and nothing else (R10, protocol/02 invariant 6).
func (c *conn) mark(now time.Time) {
	c.mu.Lock()
	c.lastLive = now
	c.mu.Unlock()
}

func (c *conn) online(now time.Time, idle time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state == stateReady && now.Sub(c.lastLive) < idle
}

// resumeToken copies the connection's current resume token under the lock. `resume` is rewritten
// by the serve goroutine on every resumed, so any other goroutine — a test included — reads it
// through here.
func (c *conn) resumeToken() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.resume...)
}

func (c *conn) readyAt() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ready
}

func newResumeToken() ([]byte, error) {
	t := make([]byte, 32)
	if _, err := rand.Read(t); err != nil {
		return nil, err
	}
	return t, nil
}

// resumeMatch is a constant-time comparison: a resume token is a bearer credential for a
// connection's replay history.
func resumeMatch(a, b []byte) bool { return subtle.ConstantTimeCompare(a, b) == 1 }

// errNotResumable is returned by resume when the client must fall back to identify.
var errNotResumable = errors.New("gateway: not resumable")

// resumeOutcome is what a resume attempt produced.
type resumeOutcome struct {
	from, to uint64
	frames   [][]byte
	entries  []ringEntry
}

// tryResume validates a resume frame against a suspended connection's ring.
//
// Refusal is invalid_session with resumable = 0 when the token is unknown or expired, the
// generation differs, or last_n is below the ring floor; after that the client heals from ready's
// per-group digest plus the two catch-up endpoints against its durable cursors.
func (c *conn) tryResume(token []byte, generation, lastN, current uint64) (resumeOutcome, error) {
	out, err := c.tryResumeSnapshot(token, generation, lastN, current)
	if err != nil {
		return resumeOutcome{}, err
	}
	out.frames = make([][]byte, 0, len(out.entries))
	for _, entry := range out.entries {
		out.frames = append(out.frames, entry.bytes)
	}
	return out, nil
}

// tryResumeSnapshot copies replay scopes while holding only the connection and ring locks. The
// authoritative policy checks happen after this returns, so database I/O never runs under either.
func (c *conn) tryResumeSnapshot(token []byte, generation, lastN, current uint64) (resumeOutcome, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !resumeMatch(c.resume, token) || generation != current {
		return resumeOutcome{}, errNotResumable
	}
	floor := c.ring.floor()
	if floor != 0 && lastN < floor-1 {
		return resumeOutcome{}, errNotResumable
	}
	entries := c.ring.entriesSince(lastN)
	return resumeOutcome{from: lastN + 1, to: c.n, entries: entries}, nil
}

// identify resolves a session token to a device session. A token that does not resolve is
// close 4003; a revoked one is 4004.
//
// The return type is auth.Session, the leaf type of deviation B9 — not a `ds.Session`, which
// would make this package import internal/ds while internal/ds imports this one.
func (g *Gateway) identify(ctx context.Context, token string) (auth.Session, error) {
	session, err := g.opts.Auth.Resolve(ctx, token)
	if err != nil {
		return auth.Session{}, err
	}
	// Only an enrolled session opens a connection. A pending session reaches its own backup reads
	// and device list and nothing else (protocol/02 § Device sessions item 4, L-HTTP-57; security
	// review F4). A provisional session reaches only one pairing group's KeyPackage, Welcome and
	// handshakes (item 4), and fan-out here is keyed by user and channel, never by pairing group:
	// admitted, it would receive every `message.plain` and voice-state frame of the user's
	// (branch review REGISTRATION-DEVICES-01). No provisional session carries a pairing group yet
	// (deviation B27); if pairing needs the socket it must be admitted for that group alone. Both
	// are refused like a token that does not resolve, and so is any scope added later. So is an
	// enrolled session whose account owes a second factor (web3.6 S1, protocol/02 § Device
	// sessions item 8): it reaches only protocol/09's `F` and `P` routes, and the socket is none.
	if session.Scope != auth.ScopeEnrolled || session.FactorOwed {
		return auth.Session{}, errNotEnrolledSession
	}
	if session.Bot != nil && session.Bot.State != store.BotInstallationJoining && session.Bot.State != store.BotInstallationReady {
		return auth.Session{}, errNotEnrolledSession
	}
	return session, nil
}

// errNotEnrolledSession is identify's refusal of a pending or provisional session, and of an
// enrolled one whose account owes a second factor.
var errNotEnrolledSession = errors.New("gateway: only an enrolled session opens a gateway connection")

// wsSink adapts a coder/websocket connection to the writer's sink. Binary frames only: dilla's
// wire is CBOR, never text.
type wsSink struct{ c *websocket.Conn }

func newWSSink(c *websocket.Conn) sink { return &wsSink{c: c} }

func (s *wsSink) write(ctx context.Context, b []byte) error {
	return s.c.Write(ctx, websocket.MessageBinary, b)
}

func (s *wsSink) read(ctx context.Context) (websocket.MessageType, []byte, error) {
	return s.c.Read(ctx)
}

func (s *wsSink) close(code CloseCode, reason string) {
	// A structured failure has already been sent as an error frame; the close reason is capped at
	// 123 bytes, so it carries only the short form.
	if len(reason) > 100 {
		reason = reason[:100]
	}
	_ = s.c.Close(websocket.StatusCode(code), reason)
}

// bearerOrTicket takes the session token from the Authorization header, or spends a
// single-use ticket from Sec-WebSocket-Protocol for clients that cannot set headers (gap-38).
func bearerOrTicket(r *http.Request, tickets *Tickets) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	// coder/websocket v1.8.15 exports exactly four free functions — Accept (accept.go:102), Dial,
	// CloseStatus (close.go:78) and NetConn (netconn.go:48). There is no Subprotocols helper, so
	// the header is read directly.
	for proto := range strings.SplitSeq(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
		proto = strings.TrimSpace(proto)
		if after, ok := strings.CutPrefix(proto, "dilla.ticket."); ok {
			if token, ok := tickets.Take(after); ok {
				return token
			}
		}
	}
	return ""
}

// decodeAny decodes a frame in either direction. Decode itself refuses server opcodes from a
// client, which is what the gateway wants on the read path; the tests need the other direction.
func decodeAny(b []byte) (Inbound, error) {
	var elems []cbor.RawMessage
	if err := cborx.Unmarshal(b, &elems); err != nil {
		return Inbound{}, err
	}
	if len(elems) != 4 {
		return Inbound{}, frameErr("E_FRAME_SHAPE", "frame", CloseDecode)
	}
	op, err := rawUint(elems[0])
	if err != nil {
		return Inbound{}, err
	}
	n, err := rawUint(elems[1])
	if err != nil {
		return Inbound{}, err
	}
	code, _, ok := lookupOp(op)
	if !ok {
		return Inbound{}, frameErr("E_FRAME_TYPE", "op", CloseDecode)
	}
	var payload []cbor.RawMessage
	if err := cborx.Unmarshal(elems[3], &payload); err != nil {
		return Inbound{}, err
	}
	return Inbound{Op: code, CID: n, Payload: payload}, nil
}

// serve runs one connection: hello, then exactly one identify or resume frame, then the read loop.
//
// The hello frame is written SYNCHRONOUSLY, not through a second writer: the connection's one
// writer is created inside register, and two writers over one sink would call
// websocket.Conn.Write concurrently, which coder/websocket does not permit and which the race
// detector flags even against the recording double. `writer.stop` closes a channel; it does not
// wait for a goroutine already inside sink.write.
func (g *Gateway) serve(ctx context.Context, s sink, token string) {
	wire, e2ee, media := g.advertised()
	hello, err := HelloPayload(wire, e2ee, media,
		g.opts.HeartbeatMS, g.opts.MaxFrameBytes, g.opts.InstanceID, g.Generation(),
		uint64(g.opts.Backoff/time.Millisecond), uint64(g.opts.BackoffJitter/time.Millisecond)) //nolint:gosec // G115: a non-negative duration in milliseconds
	if err != nil {
		s.close(CloseUnknown, "hello")
		return
	}
	b, err := Encode(Frame{Op: OpHello, Payload: hello}, 0)
	if err != nil {
		s.close(CloseUnknown, "hello")
		return
	}
	writeCtx, cancel := context.WithTimeout(ctx, g.opts.WriteDeadline)
	err = s.write(writeCtx, b)
	cancel()
	if err != nil {
		return
	}

	// Exactly one frame is read before the connection is identified. Anything else — including a
	// heartbeat or a subscribe — is close 4001.
	readCtx, cancelRead := context.WithTimeout(ctx, g.beat.grace)
	typ, raw, err := s.read(readCtx)
	cancelRead()
	if err != nil {
		s.close(CloseNotIdentified, "no identify")
		return
	}
	if typ != websocket.MessageBinary {
		s.close(CloseDecode, "text frame")
		return
	}
	in, err := Decode(raw, int(g.opts.ReadLimit))
	if err != nil {
		g.refuse(ctx, s, err)
		return
	}
	// The credential may have travelled on the HTTP upgrade instead of in the identify payload.
	in.handshakeToken = token

	switch in.Op {
	case OpResume:
		if c, ok := g.resumeConnection(ctx, s, in); ok {
			g.readLoop(ctx, c, s)
			return
		}
		// A refused resume has already sent invalid_session; the client falls back to identify on
		// its next connection, so this one ends here rather than waiting for a second frame.
		s.close(CloseNotIdentified, "resume refused")
		return
	case OpIdentify:
		c, ok := g.identifyConnection(ctx, s, in)
		if !ok {
			return
		}
		g.readLoop(ctx, c, s)
	default:
		s.close(CloseNotIdentified, "first frame is not identify or resume")
	}
}

// advertised is the instance's version sets, defaulting to {1} for all three. The {1} wire default
// serves this package's unit tests only: the real list comes from dillad.New, the one composition
// root, which passes dillad.Options.WireVersions ({4, 3, 2} since wire 4: people 4, managed Bots 3). A
// root that left it nil would offer wire 1 while the delivery service refuses its uploads.
func (g *Gateway) advertised() (wire, e2ee, media []uint64) {
	pick := func(v []uint64) []uint64 {
		if len(v) == 0 {
			return []uint64{1}
		}
		return v
	}
	return pick(g.opts.WireVersions), pick(g.opts.E2EEVersions), pick(g.opts.MediaVersions)
}

// identifyConnection handles op 1: [bearer, wire_version, e2ee_version, media_version, caps].
//
// The client picks its versions from hello's three lists (protocol/07's "Negotiation": the client
// takes the highest it shares with the instance); the instance's job is to refuse a choice that is
// not in the set it advertised. A mismatch is an error frame naming E_VERSION and close 4006,
// which is the only reason CloseVersion exists.
func (g *Gateway) identifyConnection(ctx context.Context, s sink, in Inbound) (*conn, bool) {
	bearer, err := rawText(in.Payload[0])
	if err != nil {
		g.refuse(ctx, s, frameErr("E_FRAME_SHAPE", "identify token: "+err.Error(), CloseDecode))
		return nil, false
	}
	wire, e2ee, media := g.advertised()
	chosen := [3]uint64{}
	for i, set := range [][]uint64{wire, e2ee, media} {
		v, err := rawUint(in.Payload[i+1])
		if err != nil {
			g.refuse(ctx, s, frameErr("E_FRAME_SHAPE", "identify version: "+err.Error(), CloseDecode))
			return nil, false
		}
		if !slices.Contains(set, v) {
			g.refuse(ctx, s, frameErr("E_VERSION",
				fmt.Sprintf("version %d is not advertised; this instance offers %v", v, set),
				CloseVersion))
			return nil, false
		}
		chosen[i] = v
	}
	if bearer == "" {
		bearer = tokenFromHandshake(in)
	}
	session, err := g.identify(ctx, bearer)
	if err != nil {
		g.refuse(ctx, s, frameErr("E_UNAUTHENTICATED", "identify", CloseUnauthenticated))
		return nil, false
	}
	if session.Bot != nil && chosen[0] < 3 {
		g.refuse(ctx, s, frameErr("E_VERSION", "managed bots require wire version 3; supported versions are [3]", CloseVersion))
		return nil, false
	}
	c, err := g.registerVersions(ctx, session, s, chosen)
	if err != nil {
		s.close(CloseUnknown, "register")
		return nil, false
	}
	return c, true
}

// resumeConnection handles op 2: [session_token, resume_token, generation, last_n]. On success it
// sends `resumed` [replayed_from, replayed_to], replays the ring, and rotates the resume token —
// which is what makes §6.1's "rotated on every resumed" true end to end rather than only in a
// unit test.
//
// Element 0 is a CREDENTIAL, not decoration. A resume that is authenticated by possession of the
// resume token alone would let a device whose sessions were just revoked back into the live
// registry for the whole ResumeWindow, so the session token is resolved through Options.Auth and
// the session it names must be the connection's own device.
func (g *Gateway) resumeConnection(ctx context.Context, s sink, in Inbound) (*conn, bool) {
	refuse := func(reason string) (*conn, bool) {
		if p, err := InvalidSessionPayload(false, reason); err == nil {
			if b, err := Encode(Frame{Op: OpInvalidSession, Payload: p}, 0); err == nil {
				writeCtx, cancel := context.WithTimeout(ctx, g.opts.WriteDeadline)
				_ = s.write(writeCtx, b)
				cancel()
			}
		}
		return nil, false
	}
	bearer, err := rawText(in.Payload[0])
	if err != nil {
		return refuse("malformed session token")
	}
	token, err := rawBytes(in.Payload[1])
	if err != nil {
		return refuse("malformed resume token")
	}
	generation, err := rawUint(in.Payload[2])
	if err != nil {
		return refuse("malformed generation")
	}
	lastN, err := rawUint(in.Payload[3])
	if err != nil {
		return refuse("malformed last_n")
	}
	v, ok := g.suspended.Load(string(token))
	if !ok {
		return refuse("unknown resume token")
	}
	c, ok := v.(*conn)
	if !ok {
		return refuse("unknown resume token")
	}
	// The credential may have travelled on the HTTP upgrade instead of in the payload, exactly as
	// identify allows.
	if bearer == "" {
		bearer = tokenFromHandshake(in)
	}
	session, err := g.identify(ctx, bearer)
	if err != nil {
		return refuse("unauthenticated")
	}
	if session.DeviceID != c.deviceID {
		return refuse("resume token does not belong to this session")
	}
	if session.UserID != c.userID {
		return refuse("resume token does not belong to this account")
	}
	if c.isBot != (session.Bot != nil) {
		return refuse("resume session type changed")
	}
	if session.Bot != nil && (c.wire < 3 || session.WireVersion < 3) {
		return refuse("managed bot wire capability is not resumable; identify again")
	}
	out, err := c.tryResumeSnapshot(token, generation, lastN, g.Generation())
	if err != nil {
		g.suspended.Delete(string(token))
		return refuse("resume window elapsed or n below the ring floor")
	}
	// Replay is all-or-nothing: skipping a newly blocked entry would create an n hole and hide
	// protected history from the client. Revalidate every retained entry against committed state
	// before restoring the connection or queueing any replay bytes.
	originalWire := g.connectionWire(c)
	for _, entry := range out.entries {
		requiresWire3, scopeErr := g.scopeRequiresWire3(ctx, entry.scope)
		if scopeErr != nil || (requiresWire3 && originalWire < 3) {
			g.suspended.Delete(string(token))
			return refuse("replay scope is no longer resumable; identify again")
		}
		out.frames = append(out.frames, entry.bytes)
	}
	versions := [3]uint64{c.wire, c.e2ee, c.media}
	if err := g.recordCapabilities(ctx, session.TokenHash, versions); err != nil {
		return refuse("session capability persistence failed")
	}

	// The connection comes back into the live registry over the NEW sink.
	g.suspended.Delete(string(token))
	now := g.opts.Clock.Now()
	c.mu.Lock()
	c.state = stateReady
	c.lastLive = now
	c.tokenHash = append(c.tokenHash[:0], session.TokenHash...)
	// Paused until the connection is live again, as on identify: see newPausedWriter.
	c.writer = newPausedWriter(s, writerLimits{
		Frames:   g.opts.QueueFrames,
		Bytes:    g.opts.QueueBytes,
		Deadline: g.opts.WriteDeadline,
	}, g.opts.Clock, g.opts.Log)
	w := c.writer
	c.mu.Unlock()

	// The token rotates BEFORE `resumed` is built, not after the replay: `resumed` carries the
	// rotated value as element 2, and it is the only frame that does — the client's next resume
	// answers what it reads here, so a stale read would leave it holding the spent token.
	if err := g.rotateResume(c); err != nil {
		w.stop()
		s.close(CloseUnknown, "rotate")
		return nil, false
	}
	resumedPayload := g.resumedPayloadFn
	if resumedPayload == nil {
		resumedPayload = ResumedPayload
	}
	p, err := resumedPayload(out.from, out.to, c.resumeToken())
	if err != nil {
		w.stop()
		s.close(CloseUnknown, "resumed")
		return nil, false
	}
	c.send(Frame{Op: OpResumed, Payload: p, Replay: true})
	g.sendAppBuild(c)
	for _, frame := range out.frames {
		w.enqueueRaw(frame)
	}
	// Only now does the connection go back into the live registry. Publishing it before `resumed`
	// and the replay are on the writer lets a concurrent fan-out land ahead of `resumed` or in
	// among the replayed frames — the non-monotonic n a client answers with close 4007.
	g.addLive(c)
	w.open()
	return c, true
}

// refuse sends the structured error frame and then closes, because a close reason is capped at
// 123 bytes and the client needs the code.
func (g *Gateway) refuse(ctx context.Context, s sink, err error) {
	var fe *FrameError
	if !errors.As(err, &fe) {
		s.close(CloseUnknown, "error")
		return
	}
	if p, perr := ErrorPayload(0, fe.Code, fe.Detail); perr == nil {
		if b, berr := Encode(Frame{Op: OpError, Payload: p}, 0); berr == nil {
			writeCtx, cancel := context.WithTimeout(ctx, g.opts.WriteDeadline)
			_ = s.write(writeCtx, b)
			cancel()
		}
	}
	s.close(fe.Close, fe.Code)
}

// tokenFromHandshake is the credential taken from the HTTP upgrade (an Authorization header or a
// spent ticket), which serve threaded in. An identify frame may carry an empty token when the
// credential travelled on the handshake instead.
func tokenFromHandshake(in Inbound) string { return in.handshakeToken }

// sendReady fills the per-group digest a client heals from when its resume is refused.
func (g *Gateway) sendReady(ctx context.Context, c *conn) error {
	groups, err := g.opts.Store.GroupsForDevice(ctx, c.deviceID)
	if err != nil {
		return err
	}
	digests := make([]GroupDigest, 0, len(groups))
	readyScope := FrameScope{}
	for _, groupID := range groups {
		requiresWire3, scopeErr := g.scopeRequiresWire3(ctx, FrameScope{Groups: []id.ID{groupID}})
		if scopeErr != nil {
			return scopeErr
		}
		if requiresWire3 && c.wire < 3 {
			continue
		}
		cursor, err := g.opts.Store.GetCursor(ctx, c.deviceID, groupID)
		if err != nil {
			return err
		}
		var outstanding uint64
		if g.opts.Outstanding != nil {
			// §2.3 fixes ready's group rows as [group_id, epoch, last_seq,
			// proposals_outstanding]. A hardcoded 0 here would tell a client reconnecting into a
			// frozen group that nothing is outstanding — and the digest is exactly what a client
			// with a refused resume heals from.
			outstanding = g.opts.Outstanding(ctx, groupID)
		}
		digests = append(digests, GroupDigest{
			GroupID:              groupID,
			Epoch:                cursor.LastEpoch,
			LastSeq:              cursor.LastSeq,
			ProposalsOutstanding: outstanding,
		})
		c.mu.Lock()
		c.groups[groupID] = struct{}{}
		c.mu.Unlock()
		readyScope.Groups = append(readyScope.Groups, groupID)
	}
	remaining, err := g.opts.Store.CountKeyPackages(ctx, c.deviceID, g.opts.Clock.Now().Unix())
	if err != nil {
		return err
	}
	p, err := ReadyPayload(c.deviceID, c.userID, g.Generation(), c.resume,
		c.wire, c.e2ee, c.media, uint64(remaining), digests) //nolint:gosec // G115: a COUNT(*) result, never negative
	if err != nil {
		return err
	}
	c.send(Frame{Op: OpReady, Payload: p, Replay: true, Scope: readyScope})
	c.mark(g.opts.Clock.Now())
	return nil
}

// maxSubscribedGroups caps one connection's subscription set. A device is in at most this many
// groups in any instance this version targets; the set is otherwise bounded only by the read limit
// per frame and by nothing across frames.
const maxSubscribedGroups = 4096

// subscribe adds the groups of one subscribe frame that the device is a member of — the store's
// GroupsForDevice, the same list `ready` digests — up to maxSubscribedGroups. A frame naming more
// than the cap is refused whole (E_FRAME_LIMIT); a group the device is not in is not added and the
// client is told (E_FORBIDDEN), so the set can hold neither strangers' groups nor unbounded junk.
func (g *Gateway) subscribe(ctx context.Context, c *conn, cid uint64, groups []id.ID) {
	refuse := func(code, detail string) {
		if p, err := ErrorPayload(cid, code, detail); err == nil {
			c.send(Frame{Op: OpError, Payload: p})
		}
	}
	if len(groups) > maxSubscribedGroups {
		refuse("E_FRAME_LIMIT", fmt.Sprintf("%d groups in one subscribe; at most %d", len(groups), maxSubscribedGroups))
		return
	}
	mine, err := g.opts.Store.GroupsForDevice(ctx, c.deviceID)
	if err != nil {
		return
	}
	member := make(map[id.ID]bool, len(mine))
	for _, gid := range mine {
		member[gid] = true
	}
	refused, versionRefused, full := 0, 0, false
	allowed := make([]id.ID, 0, len(groups))
	for _, gid := range groups {
		if !member[gid] {
			refused++
			continue
		}
		requiresWire3, scopeErr := g.scopeRequiresWire3(ctx, FrameScope{Groups: []id.ID{gid}})
		if scopeErr != nil {
			refuse("E_UNAVAILABLE", "group compatibility state is unavailable")
			continue
		}
		if requiresWire3 && g.connectionWire(c) < 3 {
			versionRefused++
			continue
		}
		allowed = append(allowed, gid)
	}
	c.mu.Lock()
	for _, gid := range allowed {
		if _, exists := c.groups[gid]; exists {
			continue
		}
		if len(c.groups) >= maxSubscribedGroups {
			full = true
			continue
		}
		c.groups[gid] = struct{}{}
	}
	c.mu.Unlock()
	if refused > 0 {
		refuse("E_FORBIDDEN", fmt.Sprintf("%d of the groups are not this device's", refused))
	}
	if versionRefused > 0 {
		refuse("E_VERSION", fmt.Sprintf("%d groups require wire version 3", versionRefused))
	}
	if full {
		refuse("E_FRAME_LIMIT", fmt.Sprintf("the subscription set is full at %d groups", maxSubscribedGroups))
	}
}

// readLoop reads one frame at a time. Conn.Read plus cborx.Unmarshal, never the streaming
// decoder: coder/websocket's reader grows 2*cap+512 without bound.
func (g *Gateway) readLoop(ctx context.Context, c *conn, s sink) {
	defer g.suspend(c)
	inbound := rate.NewLimiter(rate.Limit(g.opts.FramesPerSecond), g.opts.FrameBurst)
	for {
		typ, b, err := s.read(ctx)
		if err != nil {
			if g.opts.Log != nil {
				g.opts.Log.Warn("gateway: read loop ended", "device", c.deviceID.String(),
					"err", err.Error())
			}
			return
		}
		// The injected clock, not the wall clock x/time/rate would read by itself.
		if !inbound.AllowN(g.opts.Clock.Now(), 1) {
			if p, perr := ErrorPayload(0, "E_RATE_LIMITED", "inbound frame rate"); perr == nil {
				c.send(Frame{Op: OpError, Payload: p})
			}
			g.closeConn(c, CloseRateLimited, "inbound frame rate")
			return
		}
		if typ != websocket.MessageBinary {
			s.close(CloseDecode, "text frame")
			return
		}
		in, err := Decode(b, int(g.opts.ReadLimit))
		if err != nil {
			var fe *FrameError
			if errors.As(err, &fe) {
				if p, perr := ErrorPayload(0, fe.Code, fe.Detail); perr == nil {
					c.send(Frame{Op: OpError, Payload: p})
					c.currentWriter().drain(ctx)
				}
				s.close(fe.Close, fe.Code)
			}
			return
		}
		g.handle(ctx, c, in)
	}
}

// handle dispatches one inbound frame. heartbeat marks liveness and trims the ring; subscribe and
// unsubscribe adjust this connection's group set.
func (g *Gateway) handle(ctx context.Context, c *conn, in Inbound) {
	switch in.Op {
	case OpCallStats:
		g.handleCallStats(c, in)
	case OpPresenceActivity:
		g.handlePresenceActivity(ctx, c, in)
	case OpTypingSet:
		g.handleTypingSet(ctx, c, in)
	case OpVoiceSelf:
		g.handleVoiceSelf(ctx, c, in)
	case OpHeartbeat:
		lastN, err := rawUint(in.Payload[0])
		if err != nil {
			return
		}
		c.mark(g.opts.Clock.Now())
		c.ring.trim(lastN)
		if p, err := HeartbeatAckPayload(uint64(g.opts.Clock.Now().Unix())); err == nil { //nolint:gosec // G115: a unix second or row id this server wrote, never negative
			c.send(Frame{Op: OpHeartbeatAck, Payload: p})
		}
	case OpCommitAck:
		// Invariant 7's acknowledgement. The frame is group-scoped, so the group comes from
		// element 2, not from the payload.
		if in.GroupID == nil || g.opts.CommitAck == nil {
			return
		}
		mine, err := g.opts.Store.GroupsForDevice(ctx, c.deviceID)
		if err != nil || !slices.Contains(mine, *in.GroupID) {
			return
		}
		requiresWire3, scopeErr := g.scopeRequiresWire3(ctx, FrameScope{Groups: []id.ID{*in.GroupID}})
		if scopeErr != nil || (requiresWire3 && g.connectionWire(c) < 3) {
			return
		}
		round, err := rawUint(in.Payload[0])
		if err != nil {
			return
		}
		c.mark(g.opts.Clock.Now())
		g.opts.CommitAck(ctx, *in.GroupID, c.deviceID, round)
	case OpSubscribe, OpUnsubscribe:
		var groups []id.ID
		if err := cborx.Unmarshal(in.Payload[0], &groups); err != nil {
			return
		}
		if in.Op == OpUnsubscribe {
			c.mu.Lock()
			for _, gid := range groups {
				delete(c.groups, gid)
			}
			c.mu.Unlock()
			break
		}
		g.subscribe(ctx, c, in.CID, groups)
	}
	if g.opts.Metrics != nil {
		g.opts.Metrics.GatewayFrames.WithLabelValues(opLabel(in.Op), "in").Inc()
	}
}

func (g *Gateway) signalError(c *conn, code string) {
	if p, err := ErrorPayload(0, code, code); err == nil {
		c.send(Frame{Op: OpError, Payload: p})
	}
}

func signalTarget(in Inbound) (id.ID, uint64, error) {
	var target id.ID
	if err := cborx.Unmarshal(in.Payload[0], &target); err != nil {
		return id.ID{}, 0, err
	}
	flags, err := rawUint(in.Payload[1])
	return target, flags, err
}

func (g *Gateway) handleTypingSet(ctx context.Context, c *conn, in Inbound) {
	if !g.presence.chargeSignal(c.userID) {
		g.signalError(c, "E_RATE_LIMITED")
		return
	}
	channel, on, err := signalTarget(in)
	if err != nil || on > 1 {
		g.signalError(c, "E_FRAME_SHAPE")
		return
	}
	if g.opts.TypingAudience == nil {
		g.signalError(c, "E_NOT_FOUND")
		return
	}
	audience, err := g.opts.TypingAudience(ctx, c.deviceID, channel)
	if err != nil || len(audience) == 0 {
		g.signalError(c, "E_NOT_FOUND")
		return
	}
	// Recheck the permission and recipients at the delivery boundary.
	audience, err = g.opts.TypingAudience(ctx, c.deviceID, channel)
	if err != nil || len(audience) == 0 {
		g.signalError(c, "E_NOT_FOUND")
		return
	}
	requiresWire3, scopeErr := g.scopeRequiresWire3(ctx, FrameScope{Channels: []id.ID{channel}})
	if scopeErr != nil {
		g.signalError(c, "E_UNAVAILABLE")
		return
	}
	if requiresWire3 && g.connectionWire(c) < 3 {
		g.signalError(c, "E_VERSION")
		return
	}
	if !g.acceptTyping(channel, c.userID, c.deviceID, on == 1) {
		g.signalError(c, "E_RATE_LIMITED")
		return
	}
	expires := uint64(0)
	if on == 1 {
		expires = uint64(g.opts.Clock.Now().Add(typingTTL).Unix()) //nolint:gosec // server timestamps are nonnegative
	}
	p, err := TypingPayload(channel, c.userID, c.deviceID, on == 1, expires)
	if err != nil {
		return
	}
	for _, user := range audience {
		g.DeliverUser(user, Frame{Op: OpTyping, Payload: p, Scope: FrameScope{Channels: []id.ID{channel}}})
	}
	// Typing is activity (AW2, Q1): it takes the user out of the community's AFK list.
	if on == 1 && g.opts.OnTyping != nil {
		g.opts.OnTyping(ctx, c.userID, channel)
	}
}

func (g *Gateway) handleVoiceSelf(ctx context.Context, c *conn, in Inbound) {
	if !g.presence.chargeSignal(c.userID) {
		g.signalError(c, "E_RATE_LIMITED")
		return
	}
	call, bits, err := signalTarget(in)
	if err != nil || bits > 3 {
		g.signalError(c, "E_FRAME_SHAPE")
		return
	}
	if g.opts.VoiceSelfAudience == nil {
		g.signalError(c, "E_NOT_FOUND")
		return
	}
	channel, audience, _, err := g.opts.VoiceSelfAudience(ctx, c.deviceID, call)
	if err != nil || channel == (id.ID{}) || len(audience) == 0 {
		g.signalError(c, "E_NOT_FOUND")
		return
	}
	channel, audience, roomFlags, err := g.opts.VoiceSelfAudience(ctx, c.deviceID, call)
	if err != nil || channel == (id.ID{}) || len(audience) == 0 {
		g.signalError(c, "E_NOT_FOUND")
		return
	}
	requiresWire3, scopeErr := g.scopeRequiresWire3(ctx, FrameScope{Channels: []id.ID{channel}})
	if scopeErr != nil {
		g.signalError(c, "E_UNAVAILABLE")
		return
	}
	if requiresWire3 && g.connectionWire(c) < 3 {
		g.signalError(c, "E_VERSION")
		return
	}
	// The callback proves live SFU membership. Bit zero is server-owned; client bits
	// zero and one map only to the self-mute and self-deaf fields.
	flags := uint64(1) | (bits << 1) | (roomFlags & ((1 << 3) | (1 << 4)))
	if g.opts.VoiceSelfStored != nil {
		g.opts.VoiceSelfStored(call, c.deviceID, bits)
	}
	p, err := VoiceStatePayload(c.userID, c.deviceID, call, flags)
	if err != nil {
		return
	}
	for _, user := range audience {
		g.DeliverUser(user, Frame{Op: OpVoiceState, Payload: p, Scope: FrameScope{Channels: []id.ID{channel}}})
	}
}
