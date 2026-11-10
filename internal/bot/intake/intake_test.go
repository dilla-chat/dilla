package intake

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
)

// fakeHelper opens a sealed command by looking it up: the test decides each answer. A sealed reply
// is "key[0] seq body" so a test can read back which key, seq and answer it was sealed with.
type fakeHelper struct {
	mu     sync.Mutex
	opens  map[string]helper.OpenedCommand
	err    error
	ops    int
	sealed int
}

func (h *fakeHelper) OpenCommand(_ context.Context, sealed []byte, user, device id.ID, _ time.Time) (helper.OpenedCommand, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ops++
	if h.err != nil {
		return helper.OpenedCommand{}, h.err
	}
	o, ok := h.opens[string(sealed)]
	if !ok {
		return helper.OpenedCommand{Refusal: helper.RefusalMalformed}, nil
	}
	if o.Refusal == helper.RefusalNone {
		o.Request.UserID, o.Request.DeviceID = user, device
	}
	return o, nil
}

func (h *fakeHelper) SealReply(_ context.Context, key [32]byte, _ id.ID, seq uint32, body []byte) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ops++
	h.sealed++
	return cborx.Marshal([]any{uint64(key[0]), uint64(seq), body})
}

func (h *fakeHelper) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ops
}

type sentReply struct {
	device, request id.ID
	seq             uint32
	key             byte
	answer          Answer
	final           bool
}

type fakeSender struct {
	mu      sync.Mutex
	replies []sentReply
	at      []time.Time
}

func (s *fakeSender) Reply(_ context.Context, device, request id.ID, seq uint32, sealed []byte, final bool) error {
	var outer []any
	if err := cborx.Unmarshal(sealed, &outer); err != nil || len(outer) != 3 {
		return errors.New("not a fake seal")
	}
	key, _ := outer[0].(uint64)
	plain, _ := outer[2].([]byte)
	answer, err := DecodeAnswer(plain)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replies = append(s.replies, sentReply{device: device, request: request, seq: seq, key: byte(key), answer: answer, final: final})
	s.at = append(s.at, time.Now())
	return nil
}

func (s *fakeSender) all() []sentReply {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sentReply(nil), s.replies...)
}

type funcDispatcher struct {
	mu    sync.Mutex
	calls int
	last  protocol.SealedRequest
	fn    func(req protocol.SealedRequest, reply func(Answer))
}

func (d *funcDispatcher) Dispatch(_ context.Context, req protocol.SealedRequest, reply func(Answer)) {
	d.mu.Lock()
	d.calls++
	d.last = req
	d.mu.Unlock()
	d.fn(req, reply)
}

func (d *funcDispatcher) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

const (
	opRemind    = 80
	opSend      = 70 // a command whose permission_bit is send_messages, in its channel
	opRead      = 91 // a sealed read
	opCommunity = 50 // a community-scoped command whose permission_bit is send_messages
)

type world struct {
	t         *testing.T
	community id.ID
	user      id.ID
	device    id.ID
	everyone  id.ID
	helper    *fakeHelper
	sender    *fakeSender
	dispatch  *funcDispatcher
	authz     *Authz
	clk       *clock.Fake
	in        *Intake
	held      map[id.ID]bool
}

type heldSet struct{ w *world }

func (h heldSet) Held(user id.ID) bool { return h.w.held[user] }

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, community: id.New(), user: id.New(), device: id.New(), everyone: id.New(),
		helper: &fakeHelper{opens: map[string]helper.OpenedCommand{}}, sender: &fakeSender{}, clk: clock.NewFake(time.Unix(1_800_000_000, 0)),
		held: map[id.ID]bool{}}
	w.dispatch = &funcDispatcher{fn: func(_ protocol.SealedRequest, reply func(Answer)) {
		reply(Answer{Status: 200, Body: []byte("Saved"), Final: true})
	}}
	w.authz = NewAuthz(w.community)
	w.authz.SnapshotBegin()
	w.authz.Owner(id.New(), true)
	w.authz.Role(w.everyone, PermViewChannel|PermSendMessages, 0, 0, false, true)
	w.authz.Member(w.user, true, nil, true)
	w.authz.SnapshotDone()
	w.in = New(w.helper, w.authz, w.dispatch, w.sender, w.clk)
	w.in.park = 0 // the gate tests below see the refusal at once; the parking tests set a budget
	w.in.SetHeld(heldSet{w})
	w.in.SetOperations(func(op uint64) (OperationRule, bool) {
		switch op {
		case opRemind:
			return OperationRule{}, true
		case 51:
			return OperationRule{Permission: PermManageCommunity}, true
		case opSend:
			// Like poll create: channel-scoped, its argument 1 is a channel.
			return OperationRule{Permission: PermSendMessages, ChannelScoped: true, ChannelArgs: []int{1}}, true
		case opCommunity:
			return OperationRule{Permission: PermSendMessages}, true
		case opRead:
			return OperationRule{Read: true}, true
		}
		return OperationRule{}, false
	})
	return w
}

// command registers a sealed command the helper opens as a verified request for op, with reply key k.
func (w *world) command(op uint64, k byte) (RelayedCommand, protocol.SealedRequest) {
	request := id.New()
	sealed := append([]byte("sealed-"), request[:]...)
	sealed = append(sealed, k)
	var key [32]byte
	key[0] = k
	req := protocol.SealedRequest{RequestID: request, CommunityID: w.community, Operation: op, SentAt: uint64(w.clk.Now().Unix()), ReplyKey: key}
	w.helper.opens[string(sealed)] = helper.OpenedCommand{Request: req}
	return RelayedCommand{RequestID: request, CommunityID: w.community, UserID: w.user, DeviceID: w.device, Sealed: sealed}, req
}

// retry is the same request sealed again with a fresh reply key k: the helper answers Replay with k.
func (w *world) retry(cmd RelayedCommand, k byte) RelayedCommand {
	sealed := append(append([]byte(nil), cmd.Sealed...), k, k)
	var key [32]byte
	key[0] = k
	w.helper.opens[string(sealed)] = helper.OpenedCommand{Refusal: helper.RefusalReplay, ReplyKey: key, ReplayRequestID: cmd.RequestID}
	cmd.Sealed = sealed
	return cmd
}

func (w *world) refusedWith(cmd RelayedCommand, refusal helper.CommandRefusal, k byte) RelayedCommand {
	sealed := append(append([]byte(nil), cmd.Sealed...), 0xee, byte(refusal))
	o := helper.OpenedCommand{Refusal: refusal}
	if refusal >= helper.RefusalBadSignature {
		o.ReplyKey[0] = k
	}
	if refusal == helper.RefusalReplay {
		o.ReplayRequestID = cmd.RequestID
	}
	w.helper.opens[string(sealed)] = o
	cmd.Sealed = sealed
	return cmd
}

func expectOne(t *testing.T, got []sentReply, status uint16, body string) sentReply {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("replies = %+v, want one", got)
	}
	r := got[0]
	if r.answer.Status != status || string(r.answer.Body) != body || !r.answer.Final || !r.final || r.seq != 0 {
		t.Fatalf("reply = %+v, want %d %s final at seq 0", r, status, body)
	}
	return r
}

func TestIntakeRefusesWrongSignatureWrongDeviceAndReplay(t *testing.T) {
	cases := []struct {
		name    string
		refusal helper.CommandRefusal
		status  uint16
		body    string
	}{
		{"wrong signature", helper.RefusalBadSignature, 401, "E_BOT_SIGNATURE"},
		{"wrong device", helper.RefusalWrongDevice, 401, "E_BOT_SIGNATURE"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			cmd, _ := w.command(opRemind, 1)
			cmd = w.refusedWith(cmd, c.refusal, 7)
			w.in.Handle(context.Background(), cmd)
			r := expectOne(t, w.sender.all(), c.status, c.body)
			if r.key != 7 || r.device != w.device || r.request != cmd.RequestID {
				t.Fatalf("the refusal went to key %d device %v request %v", r.key, r.device, r.request)
			}
			if w.dispatch.count() != 0 {
				t.Fatal("a refused command reached a handler")
			}
		})
	}
	// The other classes: unlisted and revoked are 401, stale is 408.
	for refusal, status := range map[helper.CommandRefusal]uint16{helper.RefusalDeviceUnlisted: 401, helper.RefusalDeviceRevoked: 401, helper.RefusalStale: 408} {
		w := newWorld(t)
		cmd, _ := w.command(opRemind, 1)
		w.in.Handle(context.Background(), w.refusedWith(cmd, refusal, 7))
		if got := w.sender.all(); len(got) != 1 || got[0].answer.Status != status {
			t.Fatalf("refusal %d answered %+v, want %d", refusal, got, status)
		}
	}
	// A seal that did not open gets no reply: there is no key to seal one to.
	w := newWorld(t)
	cmd, _ := w.command(opRemind, 1)
	w.in.Handle(context.Background(), w.refusedWith(cmd, helper.RefusalMalformed, 7))
	if got := w.sender.all(); len(got) != 0 {
		t.Fatalf("a malformed seal was answered: %+v", got)
	}
}

func TestIntakeAnswersUnsupportedOperationWithoutCapabilityCheck(t *testing.T) {
	w := newWorld(t)
	cmd, _ := w.command(9999, 3)
	w.in.Handle(context.Background(), cmd)
	expectOne(t, w.sender.all(), 404, "E_BOT_UNSUPPORTED")
	if w.dispatch.count() != 0 {
		t.Fatal("an unknown operation reached a handler")
	}
	// One open and one seal: no capability round trip, nothing else.
	if n := w.helper.count(); n != 2 {
		t.Fatalf("helper ops = %d, want 2", n)
	}
}

func TestIntakeAuthorisesFromFeedState(t *testing.T) {
	w := newWorld(t)
	mods := id.New()
	w.authz.Role(mods, PermManageCommunity, 0, 3, false, false)
	w.authz.Member(w.user, true, []id.ID{mods}, false)
	cmd, _ := w.command(51, 1)
	w.in.Handle(context.Background(), cmd)
	expectOne(t, w.sender.all(), 200, "Saved")
	// A role event removes the bit: the very next command is refused, with no request but its reply.
	w.authz.Role(mods, 0, PermManageCommunity, 3, false, false)
	w.sender = &fakeSender{}
	w.in.send = w.sender
	next, _ := w.command(51, 2)
	w.in.Handle(context.Background(), next)
	expectOne(t, w.sender.all(), 403, "E_FORBIDDEN")
	if w.dispatch.count() != 1 {
		t.Fatalf("handler calls = %d, want 1", w.dispatch.count())
	}
	// A member who left is refused too.
	w.authz.Member(w.user, false, nil, false)
	w.sender = &fakeSender{}
	w.in.send = w.sender
	left, _ := w.command(opRemind, 3)
	w.in.Handle(context.Background(), left)
	expectOne(t, w.sender.all(), 403, "E_FORBIDDEN")
}

func TestHandlerCanStreamTwoAnswers(t *testing.T) {
	w := newWorld(t)
	w.dispatch.fn = func(_ protocol.SealedRequest, reply func(Answer)) {
		reply(Answer{Status: 202, Body: []byte("Searching…")})
		reply(Answer{Status: 200, Body: []byte("Now playing: x"), Final: true})
		reply(Answer{Status: 200, Body: []byte("after the final"), Final: true})
	}
	cmd, _ := w.command(opRemind, 4)
	w.in.Handle(context.Background(), cmd)
	got := w.sender.all()
	if len(got) != 2 {
		t.Fatalf("replies = %+v, want two (nothing after the final)", got)
	}
	if got[0].seq != 0 || got[0].final || got[0].answer.Final || string(got[0].answer.Body) != "Searching…" {
		t.Fatalf("first = %+v", got[0])
	}
	if got[1].seq != 1 || !got[1].final || !got[1].answer.Final || string(got[1].answer.Body) != "Now playing: x" {
		t.Fatalf("second = %+v", got[1])
	}
}

func TestHandlerWithoutAFinalAnswerEndsTheStream(t *testing.T) {
	w := newWorld(t)
	w.dispatch.fn = func(_ protocol.SealedRequest, reply func(Answer)) {
		reply(Answer{Status: 202, Body: []byte("Searching…")})
	}
	cmd, _ := w.command(opRemind, 4)
	w.in.Handle(context.Background(), cmd)
	got := w.sender.all()
	if len(got) != 2 || !got[1].final || got[1].answer.Status != 500 {
		t.Fatalf("replies = %+v, want a final 500 after the handler returned", got)
	}
}

// TestStaleKeyCommandGetsNoReplyAndRepublishesTheKey is the brief's TestStaleKeyCommandGets410 (Review
// Focus 2). A command sealed to a key the helper no longer holds cannot be opened, so it has no reply key:
// no sealed answer, 410 or other, can reach the client (the brief's own rule). The Bot logs the class and
// asks its publisher to check the current key, so the client's catalog refetch finds it. Within the 24 h
// overlap the helper still opens the previous key and the command is served (the first half).
func TestStaleKeyCommandGetsNoReplyAndRepublishesTheKey(t *testing.T) {
	w := newWorld(t)
	stale := 0
	w.in.SetKeyStale(func() { stale++ })
	previous, _ := w.command(opRemind, 5)
	w.in.Handle(context.Background(), previous)
	expectOne(t, w.sender.all(), 200, "Saved")
	cmd, _ := w.command(opRemind, 6)
	w.in.Handle(context.Background(), w.refusedWith(cmd, helper.RefusalUnknownKey, 6))
	if got := w.sender.all(); len(got) != 1 {
		t.Fatalf("an unknown key was answered: %+v", got)
	}
	if stale != 1 {
		t.Fatalf("key-stale hook calls = %d, want 1", stale)
	}
}

func TestRetryResealsAnswersToTheNewestReplyKey(t *testing.T) {
	w := newWorld(t)
	w.dispatch.fn = func(_ protocol.SealedRequest, reply func(Answer)) {
		reply(Answer{Status: 202, Body: []byte("Searching…")})
		reply(Answer{Status: 200, Body: []byte("Now playing: x"), Final: true})
	}
	cmd, _ := w.command(opRemind, 1)
	w.in.Handle(context.Background(), cmd)
	w.in.Handle(context.Background(), w.retry(cmd, 9))
	got := w.sender.all()
	if len(got) != 4 || w.dispatch.count() != 1 {
		t.Fatalf("replies = %+v, handler calls %d; want 4 replies and one handler call", got, w.dispatch.count())
	}
	for i, r := range got[2:] {
		if r.key != 9 || r.seq != uint32(i) || !bytes.Equal(r.answer.Body, got[i].answer.Body) || r.final != got[i].final {
			t.Fatalf("re-sent %d = %+v, want the same answer sealed to key 9 at seq %d", i, r, i)
		}
	}
	// After five minutes the request is forgotten: a replay is dropped (fix round 3, M5), never run.
	w.clk.Advance(5*time.Minute + time.Second)
	w.in.Handle(context.Background(), w.retry(cmd, 10))
	if got := w.sender.all(); len(got) != 4 || w.dispatch.count() != 1 {
		t.Fatalf("late replay = %+v, %d runs", got, w.dispatch.count())
	}
}

func TestRetryOfAnotherDeviceIsRefused(t *testing.T) {
	w := newWorld(t)
	cmd, _ := w.command(opRemind, 1)
	w.in.Handle(context.Background(), cmd)
	other := w.retry(cmd, 9)
	other.DeviceID = id.New()
	w.in.Handle(context.Background(), other)
	if got := w.sender.all(); len(got) != 2 || got[1].answer.Status != 409 || got[1].key != 9 {
		t.Fatalf("replies = %+v", got)
	}
}

func TestIntakeRefusesAHeldUser(t *testing.T) {
	// The user's revocation ingest failed, so the helper still holds the old list and opens the command.
	w := newWorld(t)
	w.held[w.user] = true
	cmd, _ := w.command(opRemind, 1)
	w.in.Handle(context.Background(), cmd)
	expectOne(t, w.sender.all(), 401, "E_BOT_SIGNATURE")
	if w.dispatch.count() != 0 {
		t.Fatal("a held user's command reached a handler")
	}
	// Still held: a Retry is refused again, not served from the refusal.
	w.in.Handle(context.Background(), w.retry(cmd, 2))
	if got := w.sender.all(); len(got) != 2 || got[1].answer.Status != 401 || got[1].key != 2 || w.dispatch.count() != 0 {
		t.Fatalf("held retry = %+v", got)
	}
	// Once a later ingest succeeds, a Retry of the same request is handled.
	delete(w.held, w.user)
	w.in.Handle(context.Background(), w.retry(cmd, 3))
	got := w.sender.all()
	if len(got) != 3 || got[2].answer.Status != 200 || got[2].key != 3 || w.dispatch.count() != 1 {
		t.Fatalf("released retry = %+v", got)
	}
}

func TestCommandBeforeTheFirstSnapshotIsOfflineAndRetryable(t *testing.T) {
	w := newWorld(t)
	w.authz = NewAuthz(w.community)
	w.in.authz = w.authz
	cmd, _ := w.command(opRemind, 1)
	w.in.Handle(context.Background(), cmd)
	expectOne(t, w.sender.all(), 503, "E_BOT_OFFLINE")
	w.authz.SnapshotBegin()
	w.authz.Role(w.everyone, PermViewChannel, 0, 0, false, true)
	w.authz.Member(w.user, true, nil, true)
	w.authz.SnapshotDone()
	w.in.Handle(context.Background(), w.retry(cmd, 2))
	if got := w.sender.all(); len(got) != 2 || got[1].answer.Status != 200 || w.dispatch.count() != 1 {
		t.Fatalf("retry after ready = %+v", got)
	}
}

// The runtime's feed gate (no feed handler bound for the installation yet) fails closed like an unready
// Authz: 503 E_BOT_OFFLINE, retryable, nothing dispatched.
func TestCommandWithoutAFeedIsOfflineAndRetryable(t *testing.T) {
	w := newWorld(t)
	feed := false
	w.in.SetReady(func() bool { return feed })
	cmd, _ := w.command(opRemind, 1)
	w.in.Handle(context.Background(), cmd)
	expectOne(t, w.sender.all(), 503, "E_BOT_OFFLINE")
	if w.dispatch.count() != 0 {
		t.Fatal("dispatched without a feed")
	}
	feed = true
	w.in.Handle(context.Background(), w.retry(cmd, 2))
	if got := w.sender.all(); len(got) != 2 || got[1].answer.Status != 200 || w.dispatch.count() != 1 {
		t.Fatalf("retry once the feed is bound = %+v", got)
	}
}

// startUp opens the intake's start-up park window now: budget long, gates checked every millisecond.
func (w *world) startUp(budget time.Duration) {
	w.in.mu.Lock()
	w.in.park, w.in.parkStep, w.in.started = budget, time.Millisecond, time.Now()
	w.in.mu.Unlock()
}

// commandAs is command sent by another member of the community.
func (w *world) commandAs(user id.ID, op uint64, k byte) RelayedCommand {
	cmd, _ := w.command(op, k)
	cmd.UserID = user
	return cmd
}

// waitReplies waits (real time, bounded) until the sender holds n replies.
func (w *world) waitReplies(n int) []sentReply {
	w.t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if got := w.sender.all(); len(got) >= n {
			return got
		}
	}
	w.t.Fatalf("replies = %+v, want %d", w.sender.all(), n)
	return nil
}

// Final review I2: right after a Bot start the first command can meet a start-up gate that opens within moments (the
// feed not yet bound, the barrier not yet settled). Inside the start-up window it is parked, off the worker, and
// answered once the gate opens: never a retryable refusal the person sees as a failure.
func TestFirstCommandIsParkedAtAStartUpGateUntilItOpens(t *testing.T) {
	for _, gate := range []string{"not_ready", "feed_behind"} {
		t.Run(gate, func(t *testing.T) {
			w := newWorld(t)
			w.startUp(5 * time.Second)
			var open atomic.Bool
			cmd, _ := w.command(opRemind, 1)
			switch gate {
			case "not_ready":
				w.in.SetReady(open.Load)
			case "feed_behind":
				cmd.Ready = func(context.Context) bool { return open.Load() }
			}
			w.in.Handle(context.Background(), cmd)
			if got := w.sender.all(); len(got) != 0 {
				t.Fatalf("a parked command was answered at once: %+v", got)
			}
			open.Store(true)
			expectOne(t, w.waitReplies(1), 200, "Saved")
			if w.dispatch.count() != 1 {
				t.Fatalf("dispatched %d times, want once", w.dispatch.count())
			}
		})
	}
}

// A start-up gate that stays closed past the window is still the retryable refusal.
func TestAParkedCommandIsRefusedOnceTheWindowEnds(t *testing.T) {
	w := newWorld(t)
	w.startUp(20 * time.Millisecond)
	w.in.SetReady(func() bool { return false })
	cmd, _ := w.command(opRemind, 1)
	w.in.Handle(context.Background(), cmd)
	expectOne(t, w.waitReplies(1), 503, "E_BOT_OFFLINE")
	// After the window nothing parks: the next command is refused at once.
	next, _ := w.command(opRemind, 2)
	w.in.Handle(context.Background(), next)
	if got := w.sender.all(); len(got) != 2 || got[1].answer.Status != 503 {
		t.Fatalf("after the window = %+v, want a 503 at once", got)
	}
}

// The coordinator's I2 fix: a held sender ties nothing up. Even inside the start-up window, each of its commands is
// answered 401 at once, on the caller's goroutine, and another member's command right after is answered as usual.
func TestAHeldSendersCommandsAreAnsweredAtOnceAndBlockNoOne(t *testing.T) {
	w := newWorld(t)
	w.startUp(5 * time.Second)
	w.held[w.user] = true
	start := time.Now()
	for k := range byte(6) {
		cmd, _ := w.command(opRemind, k+1)
		w.in.Handle(context.Background(), cmd)
		if got := w.sender.all(); len(got) != int(k)+1 || got[k].answer.Status != 401 {
			t.Fatalf("held command %d: replies %+v, want a 401 at once", k, got)
		}
	}
	other := id.New()
	w.authz.Member(other, true, nil, true)
	w.in.Handle(context.Background(), w.commandAs(other, opRemind, 9))
	got := w.sender.all()
	if len(got) != 7 || got[6].answer.Status != 200 || w.dispatch.count() != 1 {
		t.Fatalf("another member's command after a held sender's = %+v", got)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("a held sender's commands took %v", elapsed)
	}
}

// At most one parked command per sender and parkCap in all; the rest are refused retryable at once.
func TestParkedCommandsAreCappedPerSenderAndInAll(t *testing.T) {
	w := newWorld(t)
	w.startUp(5 * time.Second)
	w.in.SetParkCap(2)
	var open atomic.Bool
	w.in.SetReady(open.Load)
	a, b, c := id.New(), id.New(), id.New()
	for _, u := range []id.ID{a, b, c} {
		w.authz.Member(u, true, nil, true)
	}
	w.in.Handle(context.Background(), w.commandAs(a, opRemind, 1))
	w.in.Handle(context.Background(), w.commandAs(a, opRemind, 2)) // a's second: over the per-sender cap
	w.in.Handle(context.Background(), w.commandAs(b, opRemind, 3))
	w.in.Handle(context.Background(), w.commandAs(c, opRemind, 4)) // over the total cap
	got := w.sender.all()
	if len(got) != 2 || got[0].answer.Status != 503 || got[0].key != 2 || got[1].answer.Status != 503 || got[1].key != 4 {
		t.Fatalf("over the caps = %+v, want 503 at once for keys 2 and 4", got)
	}
	open.Store(true)
	got = w.waitReplies(4)
	for _, r := range got[2:] {
		if r.answer.Status != 200 || (r.key != 1 && r.key != 3) {
			t.Fatalf("parked answers = %+v, want 200 for keys 1 and 3", got)
		}
	}
}

func TestProbeIsAnsweredForTheBotOnly(t *testing.T) {
	w := newWorld(t)
	bot := id.New()
	w.in.SetBotUser(bot)
	cmd, _ := w.command(uint64(protocol.OpProbe), 1)
	cmd.UserID = bot
	w.in.Handle(context.Background(), cmd)
	expectOne(t, w.sender.all(), 200, "")
	member, _ := w.command(uint64(protocol.OpProbe), 2)
	w.in.Handle(context.Background(), member)
	if got := w.sender.all(); len(got) != 2 || got[1].answer.Status != 403 {
		t.Fatalf("a member's probe = %+v", got)
	}
}

func TestForeignCommunityAndMismatchedRequestAreDropped(t *testing.T) {
	w := newWorld(t)
	cmd, _ := w.command(opRemind, 1)
	foreign := cmd
	foreign.CommunityID = id.New()
	w.in.Handle(context.Background(), foreign)
	mismatched := cmd
	mismatched.RequestID = id.New()
	w.in.Handle(context.Background(), mismatched)
	if got := w.sender.all(); len(got) != 0 || w.dispatch.count() != 0 {
		t.Fatalf("replies = %+v, handler calls %d", got, w.dispatch.count())
	}
}

func TestHelperErrorGetsNoReply(t *testing.T) {
	w := newWorld(t)
	w.helper.err = errors.New("helper gone")
	cmd, _ := w.command(opRemind, 1)
	w.in.Handle(context.Background(), cmd)
	if got := w.sender.all(); len(got) != 0 {
		t.Fatalf("replies = %+v", got)
	}
}

func TestAnswerCacheIsBounded(t *testing.T) {
	w := newWorld(t)
	w.in.capacity = 3
	var first RelayedCommand
	for i := range 5 {
		cmd, _ := w.command(opRemind, byte(i+1))
		if i == 0 {
			first = cmd
		}
		w.in.Handle(context.Background(), cmd)
	}
	if n := w.in.cached(); n != 3 {
		t.Fatalf("cached = %d, want 3", n)
	}
	// The oldest was evicted: its Retry is dropped (fix round 3, M5), never dispatched again.
	w.in.Handle(context.Background(), w.retry(first, 50))
	if got := w.sender.all(); len(got) != 5 || w.dispatch.count() != 5 {
		t.Fatalf("evicted retry = %+v, handler calls %d", got, w.dispatch.count())
	}
}

// TestFirstAnswerWithin100msOfRelay: relay → first Reply within 100 ms for an op that answers "Saved". The
// clock is fake and never advances: nothing on the path waits on a timer.
func TestFirstAnswerWithin100msOfRelay(t *testing.T) {
	w := newWorld(t)
	cmd, _ := w.command(opRemind, 1)
	start := time.Now()
	done := make(chan struct{})
	go func() { w.in.Handle(context.Background(), cmd); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Handle waited on something")
	}
	w.sender.mu.Lock()
	defer w.sender.mu.Unlock()
	if len(w.sender.at) != 1 || string(w.sender.replies[0].answer.Body) != "Saved" {
		t.Fatalf("replies = %+v", w.sender.replies)
	}
	if took := w.sender.at[0].Sub(start); took > 100*time.Millisecond {
		t.Fatalf("relay to first answer took %v", took)
	}
}

func TestAnswerEncodingIsStatusBodyFinal(t *testing.T) {
	b := EncodeAnswer(Answer{Status: 410, Body: nil, Final: true})
	var raw []any
	if err := cborx.Unmarshal(b, &raw); err != nil || len(raw) != 3 || raw[0] != uint64(410) || raw[2] != uint64(1) {
		t.Fatalf("answer = %x (%v)", b, raw)
	}
	if body, ok := raw[1].([]byte); !ok || len(body) != 0 {
		t.Fatalf("an empty body is an empty byte string, got %#v", raw[1])
	}
	got, err := DecodeAnswer(b)
	if err != nil || got.Status != 410 || !got.Final || len(got.Body) != 0 {
		t.Fatalf("decode = %+v %v", got, err)
	}
}

// Ruling (security review): the command's channel decides, a timeout refuses, an unknown channel fails closed.
func TestIntakeAuthorisesInTheCommandsChannelAndRefusesTimeouts(t *testing.T) {
	w := newWorld(t)
	quiet, open := id.New(), id.New()
	w.authz.ChannelOverwrites(quiet, []Overwrite{{Kind: OverwriteEveryone, Target: w.everyone, Deny: PermSendMessages}}, false, false)
	w.authz.ChannelOverwrites(open, nil, false, false)
	run := func(op uint64, channel id.ID, k byte) Answer {
		t.Helper()
		cmd, _ := w.command(op, k)
		o := w.helper.opens[string(cmd.Sealed)]
		o.Request.ChannelID = channel
		w.helper.opens[string(cmd.Sealed)] = o
		w.in.Handle(context.Background(), cmd)
		got := w.sender.all()
		return got[len(got)-1].answer
	}
	if a := run(opSend, quiet, 1); a.Status != 403 {
		t.Fatalf("a channel deny overwrite: %+v", a)
	}
	if a := run(opSend, open, 2); a.Status != 200 {
		t.Fatalf("the same command in another channel: %+v", a)
	}
	if a := run(opSend, id.New(), 3); a.Status != 403 {
		t.Fatalf("a channel the Bot knows nothing of: %+v", a)
	}
	// A timed-out member: commands are refused, a sealed read still answers (dillad lets a timed-out member read).
	w.authz.MemberTimeout(w.user, uint64(w.clk.Now().Unix())+600, false)
	if a := run(opRemind, id.ID{}, 4); a.Status != 403 || string(a.Body) != "E_FORBIDDEN" {
		t.Fatalf("a timed-out member's command: %+v", a)
	}
	if a := run(opRead, id.ID{}, 5); a.Status != 200 {
		t.Fatalf("a timed-out member's read: %+v", a)
	}
	w.clk.Advance(601 * time.Second)
	if a := run(opRemind, id.ID{}, 6); a.Status != 200 {
		t.Fatalf("after the timeout: %+v", a)
	}
}

// Review ruling: a zero channel never skips a channel deny; a channel argument must name the request's channel.
func TestIntakeChannelScopeAndChannelArguments(t *testing.T) {
	w := newWorld(t)
	quiet, open := id.New(), id.New()
	w.authz.ChannelOverwrites(quiet, []Overwrite{{Kind: OverwriteEveryone, Target: w.everyone, Deny: PermSendMessages}}, false, false)
	w.authz.ChannelOverwrites(open, nil, false, false)
	run := func(op uint64, channel id.ID, args []any, k byte) Answer {
		t.Helper()
		cmd, _ := w.command(op, k)
		o := w.helper.opens[string(cmd.Sealed)]
		o.Request.ChannelID, o.Request.Args = channel, args
		w.helper.opens[string(cmd.Sealed)] = o
		w.in.Handle(context.Background(), cmd)
		got := w.sender.all()
		return got[len(got)-1].answer
	}
	// A channel-scoped operation with no channel is refused before dispatch.
	if a := run(opSend, id.ID{}, []any{"q", nil}, 1); a.Status != 400 || w.dispatch.count() != 0 {
		t.Fatalf("zero channel: %+v", a)
	}
	// Its channel argument names a channel where the member is denied, while req.ChannelID allows: refused.
	if a := run(opSend, open, []any{"q", quiet[:]}, 2); a.Status != 403 || w.dispatch.count() != 0 {
		t.Fatalf("a denied argument channel: %+v", a)
	}
	if a := run(opSend, open, []any{"q", quiet.String()}, 3); a.Status != 403 || w.dispatch.count() != 0 {
		t.Fatalf("a denied argument channel as text: %+v", a)
	}
	// The argument naming the request's channel, or none, is served.
	if a := run(opSend, open, []any{"q", open[:]}, 4); a.Status != 200 {
		t.Fatalf("the same channel: %+v", a)
	}
	if a := run(opSend, open, []any{"q", nil}, 5); a.Status != 200 {
		t.Fatalf("no argument channel: %+v", a)
	}
	// A community-scoped operation still works without a channel.
	if a := run(opCommunity, id.ID{}, nil, 6); a.Status != 200 {
		t.Fatalf("community-scoped without a channel: %+v", a)
	}
}

// Fix round 1, I-2: a new snapshot (a reconnect) makes the cache not ready until it ends, so no command is authorised
// against the state from before the outage; a command in that window is 503, retryable, and its Retry is served once
// the snapshot is applied, against the snapshot's state.
func TestCommandDuringAResnapshotIsOfflineThenServedFromTheNewState(t *testing.T) {
	w := newWorld(t)
	cmd, _ := w.command(opRemind, 1)
	w.authz.SnapshotBegin() // the Bot reconnected: dillad streams a fresh snapshot
	w.in.Handle(context.Background(), cmd)
	expectOne(t, w.sender.all(), 503, "E_BOT_OFFLINE")
	if w.dispatch.count() != 0 {
		t.Fatal("dispatched against the pre-outage state")
	}
	// The snapshot no longer names the member: the Retry is refused from the new state.
	w.authz.Role(w.everyone, PermViewChannel, 0, 0, false, true)
	w.authz.SnapshotDone()
	w.in.Handle(context.Background(), w.retry(cmd, 2))
	if got := w.sender.all(); len(got) != 2 || got[1].answer.Status != 403 || w.dispatch.count() != 0 {
		t.Fatalf("retry after the snapshot = %+v", got)
	}
}

// Fix round 1, M-1: a Replay binds its reply key to the verified seal's own request id. A relay that pairs a seal of
// one request with another request's id gets nothing: no answer is moved to that key.
func TestReplayOfAnotherRequestsSealIsDropped(t *testing.T) {
	w := newWorld(t)
	first, _ := w.command(opRemind, 1)
	second, _ := w.command(opRemind, 2)
	w.in.Handle(context.Background(), first)
	w.in.Handle(context.Background(), second)
	// A fresh seal of the second request, relayed under the first one's id.
	forged := w.retry(second, 9)
	forged.RequestID = first.RequestID
	w.in.Handle(context.Background(), forged)
	if got := w.sender.all(); len(got) != 2 {
		t.Fatalf("replies = %+v, want none for the mismatched replay", got)
	}
}

// Fix round 1, M-6: a Retry from another user is refused, never served from the cache.
func TestRetryOfAnotherUserIsRefused(t *testing.T) {
	w := newWorld(t)
	cmd, _ := w.command(opRemind, 1)
	w.in.Handle(context.Background(), cmd)
	other := w.retry(cmd, 9)
	other.UserID = id.New()
	w.in.Handle(context.Background(), other)
	if got := w.sender.all(); len(got) != 2 || got[1].answer.Status != 409 || got[1].key != 9 || w.dispatch.count() != 1 {
		t.Fatalf("replies = %+v", got)
	}
}

// Fix round 1, M-6 and M-4: a Retry of a retryable refusal runs every gate again (a member released from the hold but
// meanwhile denied the bit is refused), and a served Retry's request is as fresh as the seal the helper just verified.
func TestRetryAfterARetryableRefusalRerunsAuthorisationWithAFreshSentAt(t *testing.T) {
	w := newWorld(t)
	mods := id.New()
	w.authz.Role(mods, PermManageCommunity, 0, 3, false, false)
	w.authz.Member(w.user, true, []id.ID{mods}, false)
	w.held[w.user] = true
	cmd, _ := w.command(51, 1)
	w.in.Handle(context.Background(), cmd)
	expectOne(t, w.sender.all(), 401, "E_BOT_SIGNATURE")
	delete(w.held, w.user)
	w.authz.Role(mods, 0, PermManageCommunity, 3, false, false)
	w.in.Handle(context.Background(), w.retry(cmd, 2))
	if got := w.sender.all(); len(got) != 2 || got[1].answer.Status != 403 || w.dispatch.count() != 0 {
		t.Fatalf("retry after the bit was removed = %+v", got)
	}
	// A 403 is final: a further Retry gets it again and runs nothing.
	w.authz.Role(mods, PermManageCommunity, 0, 3, false, false)
	w.in.Handle(context.Background(), w.retry(cmd, 3))
	if got := w.sender.all(); len(got) != 3 || got[2].answer.Status != 403 || w.dispatch.count() != 0 {
		t.Fatalf("retry after a final refusal = %+v", got)
	}

	// M-4: held, then released three minutes later: the served Retry carries the Retry's time as sent_at, so a
	// mutation's expiry (sent_at + 120 s) is not already past.
	w2 := newWorld(t)
	w2.held[w2.user] = true
	cmd2, _ := w2.command(opRemind, 1)
	w2.in.Handle(context.Background(), cmd2)
	w2.clk.Advance(3 * time.Minute)
	delete(w2.held, w2.user)
	w2.in.Handle(context.Background(), w2.retry(cmd2, 2))
	if w2.dispatch.count() != 1 || w2.dispatch.last.SentAt != uint64(w2.clk.Now().Unix()) {
		t.Fatalf("dispatched %d times with sent_at %d, want once at %d", w2.dispatch.count(), w2.dispatch.last.SentAt, w2.clk.Now().Unix())
	}
}

// Fix round 1, M-3: the cache is bounded in bytes too; past the budget the oldest requests are forgotten.
func TestAnswerCacheIsBoundedInBytes(t *testing.T) {
	w := newWorld(t)
	big := make([]byte, 1000)
	w.dispatch.fn = func(_ protocol.SealedRequest, reply func(Answer)) {
		reply(Answer{Status: 200, Body: big, Final: true})
	}
	w.in.byteBudget = 2500
	var first RelayedCommand
	for i := range 4 {
		cmd, _ := w.command(opRemind, byte(i+1))
		if i == 0 {
			first = cmd
		}
		w.in.Handle(context.Background(), cmd)
	}
	if n, b := w.in.cached(), w.in.cachedBytes(); n != 2 || b > 2500 {
		t.Fatalf("cached %d requests, %d bytes", n, b)
	}
	w.in.Handle(context.Background(), w.retry(first, 50))
	if got := w.sender.all(); len(got) != 4 || w.dispatch.count() != 4 {
		t.Fatalf("evicted retry = %+v, %d runs", got, w.dispatch.count())
	}
}

// keptBytes is the sum of the cached requests' answer bodies, read record by record (tests).
func keptBytes(in *Intake) int64 {
	in.mu.Lock()
	recs := make([]*record, 0, len(in.records))
	for _, r := range in.records {
		recs = append(recs, r)
	}
	in.mu.Unlock()
	var n int64
	for _, r := range recs {
		r.mu.Lock()
		for _, a := range r.answers {
			n += int64(len(a.Body))
		}
		r.mu.Unlock()
	}
	return n
}

func expectExactBytes(t *testing.T, in *Intake, what string) {
	t.Helper()
	if counted, kept := in.cachedBytes(), keptBytes(in); counted != kept {
		t.Fatalf("%s: counted %d bytes, the cache holds %d", what, counted, kept)
	}
}

// Fix round 2, N-2: the byte count is exactly the cached answers' bytes: a request evicted while its handler still
// answers is not counted again, a replaced record is subtracted, and concurrent Retries do not subtract twice.
func TestAnswerCacheByteCountStaysExact(t *testing.T) {
	// Eviction mid-answer: capacity 1, and a second command arrives between the first one's two answers.
	w := newWorld(t)
	w.in.capacity = 1
	body := make([]byte, 100)
	second, _ := w.command(opRemind, 2)
	first := true
	w.dispatch.fn = func(_ protocol.SealedRequest, reply func(Answer)) {
		if first {
			first = false
			reply(Answer{Status: 202, Body: body})
			w.in.Handle(context.Background(), second) // evicts the first request
			reply(Answer{Status: 200, Body: body, Final: true})
			return
		}
		reply(Answer{Status: 200, Body: body, Final: true})
	}
	cmd, _ := w.command(opRemind, 1)
	w.in.Handle(context.Background(), cmd)
	expectExactBytes(t, w.in, "eviction mid-answer")

	// Replace: the same request id opened as a fresh request again.
	w2 := newWorld(t)
	w2.dispatch.fn = func(_ protocol.SealedRequest, reply func(Answer)) {
		reply(Answer{Status: 200, Body: body, Final: true})
	}
	again, _ := w2.command(opRemind, 1)
	w2.in.Handle(context.Background(), again)
	w2.in.Handle(context.Background(), again)
	expectExactBytes(t, w2.in, "replace")

	// Concurrent Retries of a retryable refusal, with evictions pressing on the cache meanwhile.
	w3 := newWorld(t)
	w3.in.capacity = 3
	w3.held[w3.user] = true
	held, _ := w3.command(opRemind, 1)
	w3.in.Handle(context.Background(), held)
	retries := make([]RelayedCommand, 0, 20)
	others := make([]RelayedCommand, 0, 20)
	for i := range 20 {
		retries = append(retries, w3.retry(held, byte(10+i)))
		o, _ := w3.command(opRemind, byte(40+i))
		others = append(others, o)
	}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(2)
		go func() { defer wg.Done(); w3.in.Handle(context.Background(), retries[i]) }()
		go func() { defer wg.Done(); w3.in.Handle(context.Background(), others[i]) }()
	}
	wg.Wait()
	expectExactBytes(t, w3.in, "concurrent retries")
}

// Task 21b fix round 1, review M1: the Bot's own account sends nothing but its health probe. Any other operation from it is
// refused before authorisation or dispatch, even when the feed lists it as a member with every permission.
func TestTheBotsOwnUserMayOnlyProbe(t *testing.T) {
	w := newWorld(t)
	bot := id.New()
	w.in.SetBotUser(bot)
	w.authz.Owner(bot, false)
	w.authz.Member(bot, true, nil, false)
	cmd, _ := w.command(opRemind, 1)
	cmd.UserID = bot
	w.in.Handle(context.Background(), cmd)
	expectOne(t, w.sender.all(), 403, "E_FORBIDDEN")
	if w.dispatch.count() != 0 {
		t.Fatalf("the Bot's own command reached a handler (%d calls)", w.dispatch.count())
	}
}

// redeliver is dillad relaying the very same sealed bytes again (a carried op 64 after the Bot's session reset): the
// helper has recorded the request, so the same seal now opens as Replay with its own reply key.
func (w *world) redeliver(cmd RelayedCommand, k byte) RelayedCommand {
	var key [32]byte
	key[0] = k
	w.helper.opens[string(cmd.Sealed)] = helper.OpenedCommand{Refusal: helper.RefusalReplay, ReplyKey: key, ReplayRequestID: cmd.RequestID}
	return cmd
}

// Task 21b fix round 3, I1: a carried duplicate of a command whose first copy got a retryable refusal is a redelivery,
// not the member's Retry. It gets the refusal again and runs nothing; the member's own Retry (a new seal) still runs.
func TestARedeliveredSealOfARetryableRefusalDoesNotRun(t *testing.T) {
	w := newWorld(t)
	w.held[w.user] = true
	cmd, _ := w.command(opRemind, 1)
	w.in.Handle(context.Background(), cmd)
	expectOne(t, w.sender.all(), 401, "E_BOT_SIGNATURE")
	delete(w.held, w.user)
	w.in.Handle(context.Background(), w.redeliver(cmd, 1))
	got := w.sender.all()
	if w.dispatch.count() != 0 {
		t.Fatal("a redelivered seal ran the command the member was told failed")
	}
	if len(got) != 2 || got[1].answer.Status != 401 || got[1].key != 1 || got[1].seq != 0 || !got[1].final {
		t.Fatalf("redelivery answered %+v, want the kept 401 again", got)
	}
	w.in.Handle(context.Background(), w.retry(cmd, 2))
	if got := w.sender.all(); len(got) != 3 || got[2].answer.Status != 200 || got[2].key != 2 || w.dispatch.count() != 1 {
		t.Fatalf("the member's Retry after the redelivery = %+v, %d runs", got, w.dispatch.count())
	}
	// A redelivery of the Retry's own seal, or of the first one, after it ran runs nothing more.
	w.in.Handle(context.Background(), w.retry(cmd, 2))
	w.in.Handle(context.Background(), w.redeliver(cmd, 1))
	if w.dispatch.count() != 1 {
		t.Fatalf("redeliveries after the Retry ran the handler %d times", w.dispatch.count())
	}
}

// Task 21b fix round 3, M5: a Replay the intake keeps no record of (the Bot restarted, or the duplicate opened before
// the first copy was recorded) is dropped: the first copy answers, and a client still waiting on it is never told
// E_BOT_REPLAY.
func TestAReplayWithNoRecordIsDropped(t *testing.T) {
	w := newWorld(t)
	cmd, _ := w.command(opRemind, 1)
	w.in.Handle(context.Background(), w.redeliver(cmd, 1))
	if got := w.sender.all(); len(got) != 0 {
		t.Fatalf("a replay with no record was answered: %+v", got)
	}
	if w.dispatch.count() != 0 {
		t.Fatal("a replay with no record ran")
	}
}
