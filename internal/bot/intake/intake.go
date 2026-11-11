// Package intake is the Bot's sealed command intake (Bot v2 Task 15). dillad relays a member's sealed
// command (gateway op 64) with the user and device it authenticated; the intake has the helper open and
// verify it (op 22), authorises it from the feed state only (Authz), dispatches it to the feature
// handler, and streams each answer back sealed to the request's one-time reply key (op 23) through
// POST /v1/bot-runtime/replies: one request per answer and nothing else.
//
// Answers. The sealed plaintext is the deterministic CBOR [status uint ≤ 65535, body bstr, final uint
// 0|1] (Task 20's contract): the client acts only on the sealed final; status ≥ 400 is a refusal and its
// body may be a bare code. Refusals, each logged with its class and never its content:
//
//	helper refusal (op 22)                 answer
//	1 malformed, 2 unknown key             none: the seal did not open, so there is no reply key
//	3 bad signature, 4 wrong device,       401 E_BOT_SIGNATURE
//	5 device unlisted, 6 device revoked
//	7 stale sent_at                        408 E_BOT_STALE
//	8 replay                               the answers already sent, re-sealed to this send's key;
//	                                       none when the request is not kept (dropped and logged);
//	                                       409 E_BOT_REPLAY for another user's or device's request
//	the user is Held (a failed ingest)     401 E_BOT_SIGNATURE (retryable: see below)
//	no feed bound, or no snapshot applied  503 E_BOT_OFFLINE (retryable)
//	operation not in the catalog           404 E_BOT_UNSUPPORTED
//	the member lacks the permission_bit    403 E_FORBIDDEN (in the command's channel; an unknown
//	                                       channel refuses any non-zero bit)
//	a timed-out member, not a sealed read  403 E_FORBIDDEN
//
// Retry. A client's Retry re-sends the same request_id sealed with a fresh reply key, and keeps only the
// newest key's handle. The helper verifies the new seal and answers Replay with its reply key, never
// recording anything twice. The intake keeps every verified request's answers for 5 minutes (the
// dedupe window), bounded in number, and on a Replay from the same user and device re-seals them, at
// their own seq, to the new key; answers still streaming go to the newest key. The handler never runs
// twice. A retryable refusal (held, not ready) keeps the verified request instead, and a Retry after the
// cause cleared handles it then.
//
// Redelivery (Task 21b fix round 3). dillad carries a Bot connection's unacknowledged commands to its next
// connection, so the same sealed bytes can arrive twice. The intake keeps a hash of every seal it verified for a
// request: a seal it has seen is a redelivery, answered with the kept answers to the newest key and never run, even
// after a retryable refusal (the member was told it failed). Only a new seal, the member's own Retry, runs a
// retryable request again. A Replay of a request the intake does not keep (the Bot restarted, the request was
// forgotten, or the duplicate opened before the first copy was recorded) is dropped and logged, not answered
// E_BOT_REPLAY: the first copy's answers, or the client's own timeout, settle it.
package intake

import (
	"container/list"
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
)

// RelayedCommand is one op 64: the request id and community dillad relayed it under, the user and device
// it authenticated (never anything from the seal), and the sealed command.
type RelayedCommand struct {
	RequestID, CommunityID, UserID, DeviceID id.ID
	Sealed                                   []byte
	// Ready, when set, waits (bounded) until the Bot has applied every feed event delivered before this command (fix
	// round 1 I-3); false answers the command offline, retryable, without authorising it.
	Ready func(context.Context) bool
}

// Answer is one answer to a command. A handler may answer several times ("Searching…" then "Now
// playing"); the last answer is Final, and nothing after it is sent.
type Answer struct {
	Status uint16
	Body   []byte
	Final  bool
}

// Dispatcher runs an opened, verified and authorised command's feature handler.
type Dispatcher interface {
	Dispatch(ctx context.Context, req protocol.SealedRequest, reply func(Answer))
}

// ReplySender posts one sealed answer to dillad (POST /v1/bot-runtime/replies).
type ReplySender interface {
	Reply(ctx context.Context, device, request id.ID, seq uint32, sealed []byte, final bool) error
}

// Helper is the dilla-bot-core helper's command half (ops 22 and 23).
type Helper interface {
	OpenCommand(ctx context.Context, sealed []byte, user, device id.ID, now time.Time) (helper.OpenedCommand, error)
	SealReply(ctx context.Context, replyKey [32]byte, requestID id.ID, seq uint32, body []byte) ([]byte, error)
}

// HeldChecker reports whether a user's latest device-list ingest failed (cmd/dillabot's
// installationFeed.Held, Task 14): the helper may still hold the user's old list, so their commands
// are refused until a later ingest succeeds.
type HeldChecker interface {
	Held(user id.ID) bool
}

const (
	// DedupeWindow is how long a request's answers are kept for a Retry (spec "Dedupe 5 min").
	DedupeWindow = 5 * time.Minute
	// DefaultCapacity bounds the kept requests. dillad keeps 256 per device and 65536 in all; one
	// community's Bot needs far fewer, and past the cap the oldest is forgotten (a Retry of it is 409).
	DefaultCapacity = 4096
	// DefaultByteBudget bounds the kept answers' bodies in all (fix round 1 M-3): past it the oldest requests are
	// forgotten, as past DefaultCapacity.
	DefaultByteBudget = 32 << 20
	// MaxAnswers is the most answers one command gets, final included: dillad's BotRepliesPerRequest.
	MaxAnswers = 16
	// maxSeals bounds the seal hashes kept per request: the first send and the newest Retries.
	maxSeals = 8
)

// Status codes and bodies of the intake's own answers.
const (
	StatusOK          uint16 = 200
	StatusBadRequest  uint16 = 400
	StatusSignature   uint16 = 401
	StatusForbidden   uint16 = 403
	StatusUnsupported uint16 = 404
	StatusStale       uint16 = 408
	StatusReplay      uint16 = 409
	StatusKeyStale    uint16 = 410
	StatusInternal    uint16 = 500
	StatusOffline     uint16 = 503

	CodeSignature   = "E_BOT_SIGNATURE"
	CodeStale       = "E_BOT_STALE"
	CodeReplay      = "E_BOT_REPLAY"
	CodeForbidden   = "E_FORBIDDEN"
	CodeUnsupported = "E_BOT_UNSUPPORTED"
	CodeKeyStale    = "E_BOT_KEY_STALE"
	CodeOffline     = "E_BOT_OFFLINE"
	CodeInternal    = "E_INTERNAL"

	// NeedsChannelText answers a channel-scoped command sent with no channel.
	NeedsChannelText = "Run this command in a channel."
)

// argsNameOnly reports whether every channel-typed argument given (16 bytes or 32 hex digits) names req's channel.
func argsNameOnly(req protocol.SealedRequest, indices []int) bool {
	for _, i := range indices {
		if i >= len(req.Args) || req.Args[i] == nil {
			continue
		}
		var named id.ID
		switch v := req.Args[i].(type) {
		case []byte:
			if len(v) != len(named) {
				return false
			}
			copy(named[:], v)
		case string:
			parsed, err := id.Parse(v)
			if err != nil {
				return false
			}
			named = parsed
		default:
			return false
		}
		if named != req.ChannelID {
			return false
		}
	}
	return true
}

// EncodeAnswer is the sealed plaintext of an answer: [status, body, final].
func EncodeAnswer(a Answer) []byte {
	body := a.Body
	if body == nil {
		body = []byte{}
	}
	final := uint64(0)
	if a.Final {
		final = 1
	}
	b, err := cborx.Marshal([]any{uint64(a.Status), body, final})
	if err != nil {
		// Three scalars never fail to encode.
		panic(err)
	}
	return b
}

// ErrAnswer is an answer plaintext that is not [status ≤ 65535, body bstr, final 0|1].
var ErrAnswer = errors.New("intake: malformed answer")

// DecodeAnswer reads an answer plaintext strictly.
func DecodeAnswer(b []byte) (Answer, error) {
	var raw []any
	if cborx.Unmarshal(b, &raw) != nil || len(raw) != 3 {
		return Answer{}, ErrAnswer
	}
	status, okStatus := raw[0].(uint64)
	body, okBody := raw[1].([]byte)
	final, okFinal := raw[2].(uint64)
	if !okStatus || status > 65535 || !okBody || !okFinal || final > 1 {
		return Answer{}, ErrAnswer
	}
	return Answer{Status: uint16(status), Body: body, Final: final == 1}, nil
}

func refusal(status uint16, code string) Answer {
	return Answer{Status: status, Body: []byte(code), Final: true}
}

// record is one verified request: who sent it, the newest reply key, and what was answered.
type record struct {
	mu        sync.Mutex
	requestID id.ID
	user      id.ID
	device    id.ID
	at        time.Time
	replyKey  [32]byte
	answers   []Answer
	// bytes is the kept answers' body bytes counted in the intake's total, and dropped is whether the record left the
	// cache (evicted, expired or replaced). Both are guarded by i.mu (fix round 2, N-2): a dropped record's later
	// answers are not counted, and its bytes were subtracted once, when it was dropped.
	bytes   int64
	dropped bool
	seq     uint32 // the next answer's seq: len(answers)
	final   bool
	// retry is the verified request of a retryable refusal: a Retry handles it once the cause cleared.
	retry *protocol.SealedRequest
	// seals is the SHA-256 of each seal verified for this request, newest last, at most maxSeals (Task 21b fix
	// round 3, I1): a seal seen before is a redelivery, never the member's Retry.
	seals [][sha256.Size]byte
	elem  *list.Element
}

// Intake handles one community's relayed commands. Handle is safe for concurrent use.
type Intake struct {
	helper   Helper
	authz    *Authz
	dispatch Dispatcher
	send     ReplySender
	clk      clock.Clock

	mu         sync.Mutex
	held       HeldChecker
	operations func(op uint64) (OperationRule, bool)
	botUser    id.ID
	log        *slog.Logger
	keyStale   func()
	ready      func() bool
	// The start-up park (final review I2): for park after started, a command that meets a start-up gate (the feed
	// barrier not settled, the Authz without its first snapshot, the feed handler not bound) waits for it to open off
	// the worker, checked every parkStep, before it is refused. At most parkCap commands are parked at once, and at
	// most one per sender; anything over is refused at once. Held never parks. After the window nothing parks.
	park, parkStep time.Duration
	started        time.Time
	parkCap        int
	parked         int
	parkedBy       map[id.ID]bool
	capacity       int
	byteBudget     int64
	bytes          int64
	records        map[id.ID]*record
	order          list.List
}

// New is an intake answering for authz's community.
func New(h Helper, authz *Authz, dispatch Dispatcher, send ReplySender, clk clock.Clock) *Intake {
	if clk == nil {
		clk = clock.System()
	}
	return &Intake{helper: h, authz: authz, dispatch: dispatch, send: send, clk: clk, log: slog.New(slog.DiscardHandler),
		capacity: DefaultCapacity, byteBudget: DefaultByteBudget, records: map[id.ID]*record{},
		park: ParkBudget, parkStep: parkStep, started: time.Now(), parkCap: DefaultParkCap, parkedBy: map[id.ID]bool{}}
}

// DefaultParkCap is the most commands parked at once: one per sealed worker of the runtime.
const DefaultParkCap = 4

// SetParkCap sets the most commands parked at once (one per worker); 0 parks nothing.
func (i *Intake) SetParkCap(n int) { i.mu.Lock(); i.parkCap = max(n, 0); i.mu.Unlock() }

// SetParkWindow sets how long after its start the intake parks start-up commands; 0 parks nothing.
func (i *Intake) SetParkWindow(d time.Duration) { i.mu.Lock(); i.park = max(d, 0); i.mu.Unlock() }

// tryPark takes a park slot for user while the start-up window is open and the caps allow it.
func (i *Intake) tryPark(user id.ID) (time.Time, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	deadline := i.started.Add(i.park)
	if !time.Now().Before(deadline) || i.parked >= i.parkCap || i.parkedBy[user] {
		return time.Time{}, false
	}
	i.parked++
	i.parkedBy[user] = true
	return deadline, true
}

func (i *Intake) unpark(user id.ID) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.parked--
	delete(i.parkedBy, user)
}

// ParkBudget is the spec's first-answer budget ("Bot restart ready within 5 s"): for this long after the intake starts,
// a command that meets a start-up gate is parked until it opens instead of refused (final review I2).
const ParkBudget = 5 * time.Second

const parkStep = 250 * time.Millisecond

// SetHeld sets the held-user query (Task 14's installationFeed.Held).
func (i *Intake) SetHeld(h HeldChecker) { i.mu.Lock(); i.held = h; i.mu.Unlock() }

// OperationRule is how the intake authorises one catalog operation: its permission_bit, checked in the
// command's channel, and whether it is a sealed read (handler_kind 1), which a timed-out member may still send.
//
// ChannelScoped operations act in the request's channel: one with no channel is refused (400) before dispatch, and its
// permission_bit is resolved there. ChannelArgs are the indices of channel-typed arguments; each one given must name
// the request's channel, or the command is refused (403): the intake authorises exactly one channel, the one the
// handler acts on (rule chosen over checking each named channel, so a handler never acts in a channel nobody checked).
type OperationRule struct {
	Permission    uint64
	Read          bool
	ChannelScoped bool
	ChannelArgs   []int
}

// SetOperations sets the catalog's operations: each one's rule, and whether the Bot offers it.
func (i *Intake) SetOperations(f func(op uint64) (OperationRule, bool)) {
	i.mu.Lock()
	i.operations = f
	i.mu.Unlock()
}

// SetBotUser names the Bot's own account: only it may send OpProbe.
func (i *Intake) SetBotUser(user id.ID) { i.mu.Lock(); i.botUser = user; i.mu.Unlock() }

// SetLogger sets the refusal log.
func (i *Intake) SetLogger(l *slog.Logger) {
	if l != nil {
		i.mu.Lock()
		i.log = l
		i.mu.Unlock()
	}
}

// SetKeyStale is called when a command was sealed to a key the helper does not hold: the publisher
// checks the current key and republishes it if it changed, so the client's catalog refetch finds it.
func (i *Intake) SetKeyStale(f func()) { i.mu.Lock(); i.keyStale = f; i.mu.Unlock() }

// SetReady adds a readiness gate beside the Authz's own (a snapshot applied): the runtime's "this
// installation has a feed handler bound". While either is false every command is answered 503
// E_BOT_OFFLINE (retryable), so a Held query that cannot know yet never lets a command through.
func (i *Intake) SetReady(f func() bool) { i.mu.Lock(); i.ready = f; i.mu.Unlock() }

func (i *Intake) isReady() bool {
	i.mu.Lock()
	ready := i.ready
	i.mu.Unlock()
	return i.authz.Ready() && (ready == nil || ready())
}

func (i *Intake) config() (HeldChecker, func(uint64) (OperationRule, bool), id.ID, *slog.Logger, func()) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.held, i.operations, i.botUser, i.log, i.keyStale
}

func (i *Intake) refused(ctx context.Context, log *slog.Logger, class string, status uint16) {
	log.InfoContext(ctx, "bot command refused", "class", class, "status", status)
}

// Handle answers one relayed command. It returns once the command's last answer was sent (or it was
// dropped), or once it was parked at a start-up gate, which answers it later off the caller's goroutine (final
// review I2).
func (i *Intake) Handle(ctx context.Context, cmd RelayedCommand) {
	held, operations, botUser, log, keyStale := i.config()
	if cmd.CommunityID != i.authz.Community() || cmd.RequestID.IsZero() || cmd.UserID.IsZero() || cmd.DeviceID.IsZero() || len(cmd.Sealed) == 0 {
		i.refused(ctx, log, "foreign", 0)
		return
	}
	// Before the open (the helper's device lists come from the feed too) and before any authorisation.
	settle := func() bool { return cmd.Ready == nil || cmd.Ready(ctx) }
	settled := settle()
	now := i.clk.Now()
	opened, err := i.helper.OpenCommand(ctx, cmd.Sealed, cmd.UserID, cmd.DeviceID, now)
	if err != nil {
		// Outcome unknown: the helper may have recorded the id. The client times out and retries.
		log.WarnContext(ctx, "bot command open failed", "error_class", helper.Class(err))
		return
	}
	switch opened.Refusal {
	case helper.RefusalNone:
	case helper.RefusalMalformed:
		i.refused(ctx, log, "malformed", 0)
		return
	case helper.RefusalUnknownKey:
		i.refused(ctx, log, "unknown_key", 0)
		if keyStale != nil {
			keyStale()
		}
		return
	case helper.RefusalBadSignature, helper.RefusalWrongDevice, helper.RefusalDeviceUnlisted, helper.RefusalDeviceRevoked:
		i.refused(ctx, log, refusalClass(opened.Refusal), StatusSignature)
		i.sendOnce(ctx, cmd, opened.ReplyKey, refusal(StatusSignature, CodeSignature))
		return
	case helper.RefusalStale:
		i.refused(ctx, log, "stale", StatusStale)
		i.sendOnce(ctx, cmd, opened.ReplyKey, refusal(StatusStale, CodeStale))
		return
	case helper.RefusalReplay:
		// The key is bound to the verified seal's own request (fix round 1 M-1): a seal of another request, relayed
		// under this id, gets nothing.
		if opened.ReplayRequestID != cmd.RequestID {
			i.refused(ctx, log, "replay_mismatch", 0)
			return
		}
		i.replay(ctx, log, cmd, opened.ReplyKey, held, operations, botUser, settled, settle)
		return
	default:
		i.refused(ctx, log, "unknown_refusal", 0)
		return
	}
	req := opened.Request
	if req.RequestID != cmd.RequestID || req.CommunityID != cmd.CommunityID || req.UserID != cmd.UserID || req.DeviceID != cmd.DeviceID {
		i.refused(ctx, log, "mismatch", 0)
		return
	}
	rec := i.remember(cmd, req.ReplyKey, now)
	i.run(ctx, log, rec, req, held, operations, botUser, settled, settle, true)
}

// parkThenRun waits, holding one park slot, until the start-up gates open or deadline passes, then runs the command
// once more without parking: it is answered, or refused retryable if a gate is still closed.
func (i *Intake) parkThenRun(ctx context.Context, log *slog.Logger, rec *record, req protocol.SealedRequest, held HeldChecker, operations func(uint64) (OperationRule, bool), botUser id.ID, settled bool, settle func() bool, deadline time.Time) {
	defer i.unpark(req.UserID)
	i.mu.Lock()
	step := i.parkStep
	i.mu.Unlock()
	for !settled || !i.isReady() {
		if wait := min(step, time.Until(deadline)); wait <= 0 || !sleepCtx(ctx, wait) {
			break
		}
		if !settled && settle != nil {
			settled = settle()
		}
	}
	i.run(ctx, log, rec, req, held, operations, botUser, settled, settle, false)
}

// sleepCtx waits d, or until ctx ends (false).
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func refusalClass(r helper.CommandRefusal) string {
	switch r {
	case helper.RefusalBadSignature:
		return "bad_signature"
	case helper.RefusalWrongDevice:
		return "wrong_device"
	case helper.RefusalDeviceUnlisted:
		return "device_unlisted"
	case helper.RefusalDeviceRevoked:
		return "device_revoked"
	}
	return "unknown_refusal"
}

// run authorises a verified request and dispatches it, or refuses it, answering through rec.
func (i *Intake) run(ctx context.Context, log *slog.Logger, rec *record, req protocol.SealedRequest, held HeldChecker, operations func(uint64) (OperationRule, bool), botUser id.ID, settled bool, settle func() bool, mayPark bool) {
	retryable := func(class string, status uint16, code string) {
		i.refused(ctx, log, class, status)
		rec.mu.Lock()
		kept := req
		rec.retry = &kept
		rec.mu.Unlock()
		i.answer(ctx, rec, refusal(status, code))
	}
	// The retryable gates: the feed barrier, the sender's held device list, the installation's feed. Held answers at once
	// (a held sender must not tie anything up). The start-up gates (barrier, feed) open within moments of a start, so
	// inside the start-up window the command is parked off this goroutine until they open (final review I2).
	if mayPark && (!settled || !i.isReady()) {
		if deadline, ok := i.tryPark(req.UserID); ok {
			go i.parkThenRun(ctx, log, rec, req, held, operations, botUser, settled, settle, deadline)
			return
		}
	}
	switch {
	case !settled:
		retryable("feed_behind", StatusOffline, CodeOffline)
		return
	case held != nil && held.Held(req.UserID):
		retryable("held", StatusSignature, CodeSignature)
		return
	case !i.isReady():
		retryable("not_ready", StatusOffline, CodeOffline)
		return
	}
	if req.Operation == uint64(protocol.OpProbe) {
		if botUser.IsZero() || req.UserID != botUser {
			i.refused(ctx, log, "forbidden", StatusForbidden)
			i.answer(ctx, rec, refusal(StatusForbidden, CodeForbidden))
			return
		}
		i.answer(ctx, rec, Answer{Status: StatusOK, Final: true})
		return
	}
	// The Bot's own account sends nothing but its probe (Task 21b fix round 1, review M1): it is no member acting through
	// its own commands, whatever the feed lists for it.
	if !botUser.IsZero() && req.UserID == botUser {
		i.refused(ctx, log, "bot_self", StatusForbidden)
		i.answer(ctx, rec, refusal(StatusForbidden, CodeForbidden))
		return
	}
	rule, known := OperationRule{}, false
	if operations != nil {
		rule, known = operations(req.Operation)
	}
	if !known {
		i.refused(ctx, log, "unsupported", StatusUnsupported)
		i.answer(ctx, rec, refusal(StatusUnsupported, CodeUnsupported))
		return
	}
	// A timed-out member can still read but cannot act, as dillad refuses their sends.
	if !rule.Read && i.authz.TimedOut(req.UserID, i.clk.Now()) {
		i.refused(ctx, log, "timed_out", StatusForbidden)
		i.answer(ctx, rec, refusal(StatusForbidden, CodeForbidden))
		return
	}
	if rule.ChannelScoped && req.ChannelID.IsZero() {
		i.refused(ctx, log, "no_channel", StatusBadRequest)
		i.answer(ctx, rec, Answer{Status: StatusBadRequest, Body: []byte(NeedsChannelText), Final: true})
		return
	}
	if !argsNameOnly(req, rule.ChannelArgs) {
		i.refused(ctx, log, "channel_mismatch", StatusForbidden)
		i.answer(ctx, rec, refusal(StatusForbidden, CodeForbidden))
		return
	}
	if !i.authz.Allowed(req.CommunityID, req.ChannelID, req.UserID, rule.Permission, !rule.ChannelScoped) {
		i.refused(ctx, log, "forbidden", StatusForbidden)
		i.answer(ctx, rec, refusal(StatusForbidden, CodeForbidden))
		return
	}
	i.dispatch.Dispatch(ctx, req, func(a Answer) { i.answer(ctx, rec, a) })
	rec.mu.Lock()
	final := rec.final
	rec.mu.Unlock()
	if !final {
		log.WarnContext(ctx, "bot command handler ended without a final answer", "operation", req.Operation)
		i.answer(ctx, rec, refusal(StatusInternal, CodeInternal))
	}
}

// replay answers a Retry: the answers already sent, re-sealed to the newest key, or the retryable
// request handled now, or 409.
func (i *Intake) replay(ctx context.Context, log *slog.Logger, cmd RelayedCommand, key [32]byte, held HeldChecker, operations func(uint64) (OperationRule, bool), botUser id.ID, settled bool, settle func() bool) {
	rec := i.lookup(cmd.RequestID, i.clk.Now())
	if rec == nil {
		// Not kept (fix round 3, M5): a duplicate whose first copy is still being recorded, or one after a restart or
		// eviction. Whatever the first copy answered stands; a 409 here could end a client still waiting on it.
		i.refused(ctx, log, "replay_unknown", 0)
		return
	}
	if rec.user != cmd.UserID || rec.device != cmd.DeviceID || key == ([32]byte{}) {
		i.refused(ctx, log, "replay", StatusReplay)
		i.sendOnce(ctx, cmd, key, refusal(StatusReplay, CodeReplay))
		return
	}
	seal := sha256.Sum256(cmd.Sealed)
	rec.mu.Lock()
	if slices.Contains(rec.seals, seal) {
		// A redelivery (fix round 3, I1): the kept answers again, to the newest key, and nothing runs.
		defer rec.mu.Unlock()
		i.refused(ctx, log, "redelivery", 0)
		var seq uint32
		for _, a := range rec.answers {
			i.sendLocked(ctx, rec, seq, a)
			seq++
		}
		return
	}
	rec.addSeal(seal)
	rec.replyKey = key
	if retry := rec.retry; retry != nil {
		// A retryable refusal: forget it and handle the request now, answering to the new key.
		rec.retry, rec.answers, rec.seq, rec.final = nil, nil, 0, false
		i.release(rec)
		rec.mu.Unlock()
		req := *retry
		req.ReplyKey = key
		// The helper has just verified this seal's sent_at against now: the request is as fresh as the Retry (fix
		// round 1 M-4), so a mutation's expiry follows it, not the first send's.
		req.SentAt = uint64(max(i.clk.Now().Unix(), 0))
		i.run(ctx, log, rec, req, held, operations, botUser, settled, settle, true)
		return
	}
	defer rec.mu.Unlock()
	var seq uint32
	for _, a := range rec.answers {
		i.sendLocked(ctx, rec, seq, a)
		seq++
	}
}

// answer sends a's next seq to rec's newest key and keeps it for a Retry. Nothing follows a final.
func (i *Intake) answer(ctx context.Context, rec *record, a Answer) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.final {
		return
	}
	if len(rec.answers) >= MaxAnswers-1 && !a.Final {
		// dillad delivers at most 16 answers per request; a handler streaming more is ended here.
		a = refusal(StatusInternal, CodeInternal)
	}
	seq := rec.seq
	rec.seq++
	rec.answers = append(rec.answers, a)
	i.charge(rec, int64(len(a.Body)))
	rec.final = a.Final
	i.sendLocked(ctx, rec, seq, a)
}

func (i *Intake) sendLocked(ctx context.Context, rec *record, seq uint32, a Answer) {
	i.seal(ctx, rec.device, rec.requestID, rec.replyKey, seq, a)
}

// sendOnce answers a refusal that is not kept (the helper recorded nothing for it).
func (i *Intake) sendOnce(ctx context.Context, cmd RelayedCommand, key [32]byte, a Answer) {
	if key == ([32]byte{}) {
		return
	}
	i.seal(ctx, cmd.DeviceID, cmd.RequestID, key, 0, a)
}

func (i *Intake) seal(ctx context.Context, device, request id.ID, key [32]byte, seq uint32, a Answer) {
	_, _, _, log, _ := i.config()
	sealed, err := i.helper.SealReply(ctx, key, request, seq, EncodeAnswer(a))
	if err != nil {
		log.WarnContext(ctx, "bot command answer not sealed", "error_class", helper.Class(err), "op", uint64(protocol.HelperReplySeal), "seq", seq)
		return
	}
	if err := i.send.Reply(ctx, device, request, seq, sealed, a.Final); err != nil {
		log.WarnContext(ctx, "bot command answer not delivered", "error_class", helper.Class(err))
	}
}

// remember keeps a verified request for DedupeWindow, evicting expired and over-capacity ones first.
func (i *Intake) remember(cmd RelayedCommand, key [32]byte, now time.Time) *record {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.expireLocked(now)
	if old := i.records[cmd.RequestID]; old != nil {
		i.dropLocked(old.elem) // replaced: its bytes leave the count (fix round 2, N-2)
	}
	for i.order.Len() >= i.capacity && i.order.Len() > 0 {
		i.dropLocked(i.order.Front())
	}
	rec := &record{requestID: cmd.RequestID, user: cmd.UserID, device: cmd.DeviceID, at: now, replyKey: key}
	rec.addSeal(sha256.Sum256(cmd.Sealed))
	rec.elem = i.order.PushBack(rec)
	i.records[cmd.RequestID] = rec
	return rec
}

// addSeal keeps a verified seal's hash, forgetting the oldest past maxSeals. rec.mu is held, or rec is not shared yet.
func (rec *record) addSeal(seal [sha256.Size]byte) {
	rec.seals = append(rec.seals, seal)
	if over := len(rec.seals) - maxSeals; over > 0 {
		rec.seals = slices.Delete(rec.seals, 0, over)
	}
}

func (i *Intake) lookup(request id.ID, now time.Time) *record {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.expireLocked(now)
	return i.records[request]
}

func (i *Intake) expireLocked(now time.Time) {
	for e := i.order.Front(); e != nil; e = i.order.Front() {
		rec, _ := e.Value.(*record)
		if now.Sub(rec.at) <= DedupeWindow {
			return
		}
		i.dropLocked(e)
	}
}

func (i *Intake) dropLocked(e *list.Element) {
	rec, _ := e.Value.(*record)
	i.order.Remove(e)
	delete(i.records, rec.requestID)
	i.bytes -= rec.bytes
	rec.bytes, rec.dropped = 0, true
}

// release takes a cached record's answers out of the count (a retryable refusal's answers are forgotten for its Retry).
func (i *Intake) release(rec *record) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.bytes -= rec.bytes
	rec.bytes = 0
}

// charge counts n more body bytes of rec, while it is cached, and forgets the oldest requests while the total is over
// the budget.
func (i *Intake) charge(rec *record, n int64) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if rec.dropped {
		return // no longer cached: its answers are not kept for a Retry, so not counted
	}
	rec.bytes += n
	i.bytes += n
	for i.bytes > i.byteBudget && i.order.Len() > 0 {
		i.dropLocked(i.order.Front())
	}
}

// cachedBytes is the kept answers' body bytes (tests).
func (i *Intake) cachedBytes() int64 {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.bytes
}

// cached is the number of kept requests (tests).
func (i *Intake) cached() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.order.Len()
}
