// Package trust is the Bot's MLS membership trust, driven by pushed device lists (Bot v2 Task 16). It replaces the
// v1 sender trust refresh (internal/bot/service/source_trust.go, deleted with the v1 intake by Task 23), whose
// all-member passes read every member's signed history on a timer and per command.
//
// dillad pushes a user's whole signed device-list history (op 65 device_list) when it changes. Pushed hands it to
// helper op 24, then Changed re-judges only that user's current leaves in the Bot's groups: the helper's pending
// leaves are, for a Bot, exactly the leaves whose user's list or pinned key changed since they were last judged
// (core/dilla-core pending_leaves). That is O(that user's leaves) per push, and nothing on a timer.
//
// A user whose push cannot be applied is Held: their commands and leaves are refused (fail closed). Only a push that
// applies releases a held user. Held covers:
//   - a push not yet applied after the inline budget (its attempt still runs, off the caller);
//   - a transient failure of op 24 or of the re-judging (a helper restart, a timeout, a busy, unavailable or internal
//     helper status, any failure of the re-judging): the push is retried out of band on the RetryPolicy, per user,
//     so a failing user never blocks another user's events. When the attempts run out the user stays held until a
//     later push for them applies. A newer push that the helper refuses outright does not cancel that retry;
//   - a key change held from a push (a list under a UMK other than the pinned one). Only the user's MLS leaf, a newer
//     list under the old key or an admin re-pin resolves it; there is no admin re-pin UI yet. Until then each push
//     answers held again;
//   - a list behind its signed head: held until a push whose history reaches that head.
//
// Enforcing Held at command intake is the intake's job (Task 15); Trust only keeps Held correct.
//
// # The per-user invariant: only the newest push applying releases the user
//
// Each user's whole trust state has one owner, their userState, and it changes only under Trust.mu, one message at a
// time, in the order the messages take the lock (fix round 4, N-7). The messages are: a fresh push and its inline
// budget running out (Pushed), the worker taking its next attempt and an attempt settling (drive), a retry falling due
// (await), a fresh helper (Kick) and Close. Each is handled in one hold of the lock from its first check to its last
// write: nothing takes a generation in one hold and queues in another, nothing is decided outside the lock and
// written inside it. Changed, Sweep and the unjudged flag touch no hold. Within that, these rules make the invariant
// hold by construction:
//
//  1. A fresh push gets the user's next generation, becomes the live push (the top of the live stack) and is queued, in
//     the same hold (pushLocked). So queueing order equals generation order: whatever waits when a fresh push arrives
//     is older than it.
//  2. Per user at most one attempt runs and at most one waits. A job never replaces a waiting job of the same or a
//     newer generation; the older job is the one superseded (enqueueLocked). A superseded fresh push keeps its stack
//     entry beneath the newer push (N-6), so a rollback can reach it. A retry job is queued only while its retry is the
//     user's and its push is the live one, and it is dropped before it runs if that stopped being true (takeLocked).
//  3. Only an attempt of the live generation changes the user's hold (settleLocked). An older attempt may still update
//     the helper's cache (op 24 ignores rows at or below the cached version) but releases nobody.
//  4. The live push always has an attempt running, waiting or armed, or it has settled: it is queued when pushed (1),
//     run by the worker, re-armed on a transient failure, and when the live push is refused outright it leaves the
//     stack and the new top is retried unless an attempt of it is in flight (rollBackLocked). A retry that is refused
//     outright or runs out of attempts leaves the user given up, which is held.
//
// Hence a user is never left not held while their newest applicable push is unapplied: that push either applied and
// released them, or holds them (deferred, retried, or given up). Two concurrent Pushed calls for one user (two feed
// workers on one installation) are ordered by the lock like any two messages.
package trust

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
)

// TrustCore is the helper as Trust uses it (native.Client).
type TrustCore interface {
	// IngestDeviceList is helper op 24: the pushed history of user, unchanged.
	IngestDeviceList(ctx context.Context, user id.ID, push []byte) (native.DeviceListIngest, error)
	// PendingLeaves is helper group op 17: for a Bot, the leaves whose user's list or pin changed since they were
	// judged, plus the leaves of users with no list yet.
	PendingLeaves(ctx context.Context) ([]native.GroupPendingLeaf, error)
	// RevalidateGroup is helper group op 13: judge one current leaf against its user's cached list and pin, then
	// release that leaf's held messages the verdict allows.
	RevalidateGroup(ctx context.Context, group id.ID, leaf uint64, limit uint64) (uint32, error)
}

// RetryPolicy is the out-of-band retry of one user's failed push: the first retry First after the failure, each next
// one twice as long after the previous failure, capped at Max, and Attempts attempts in all (the first included).
type RetryPolicy struct {
	First    time.Duration
	Max      time.Duration
	Attempts int
}

// DefaultRetry is the Bot's policy (the brief's): 1 s doubling to 5 min, 8 attempts.
var DefaultRetry = RetryPolicy{First: 1 * time.Second, Max: 5 * time.Minute, Attempts: 8}

// DefaultInlineBudget is how long Pushed waits for a push's first attempt before it hands the attempt off (M-1). The
// attempt is never cancelled (a helper call cut off mid-flight closes the helper); it finishes off the caller.
const DefaultInlineBudget = 2 * time.Second

// revalidateLimit is op 13's page bound; the helper drains every page of the leaf in one call.
const revalidateLimit = 256

// ErrHeld is returned by Pushed when the push applied but the user stays refused: a key change is pending, or the list
// is behind its signed head.
var ErrHeld = errors.New("bot trust: the user's pushed list is held")

// ErrDeferred is returned by Pushed when the push's attempt did not finish within the inline budget (or another
// attempt of the user is in flight): it finishes off the caller, and the user is held until a push applies.
var ErrDeferred = errors.New("bot trust: the push is still being applied; the user is held")

// errSuperseded resolves a queued push that a newer push of the same user replaced before it ran.
var errSuperseded = errors.New("bot trust: superseded by a newer push")

// changedError is a failure of re-judging the user's leaves after op 24 applied: always transient (M-2), whatever
// helper status it carries, so it does not unwrap to the status.
type changedError struct{ err error }

func (e *changedError) Error() string {
	return "bot trust: re-judging the user's leaves failed: " + e.err.Error()
}

// pendingRetry is one user's pending out-of-band retry.
type pendingRetry struct {
	push     []byte
	gen      uint64 // the push's generation (fix round 2, N-1)
	attempts int    // attempts made, the first included
	timer    clock.Timer
	stop     chan struct{}
}

// livePush is one entry of a user's live stack: a push not refused outright, superseded or not. applied is set when an
// attempt of it applied while a newer push owned the hold: a rollback to it then settles from that outcome instead of
// running the push again (fix round 4).
type livePush struct {
	gen      uint64
	push     []byte
	attempts int
	applied  *outcome
}

// job is one attempt of a push, run by the user's worker.
type job struct {
	push     []byte
	gen      uint64        // the push's generation: a fresh push gets the user's next, a retry its push's
	attempts int           // this attempt's number
	retry    *pendingRetry // the retry this attempt runs, nil for a fresh push
	done     chan outcome  // buffered, written once
	settled  bool          // guarded by Trust.mu
	deferred bool          // guarded by Trust.mu: Pushed returned before it settled
}

type outcome struct {
	got native.DeviceListIngest
	err error
}

// userState is one user's whole trust state and its single owner (see the invariant above). Every field is guarded
// by Trust.mu, and every change to it is one message handled in one hold of that lock.
type userState struct {
	// gen is the last generation given to a fresh push. live is the pushes neither refused outright nor settled under
	// a newer push, oldest first: its last entry is the live push, the one whose attempt owns the hold (N-1). A push
	// refused outright leaves the stack, so the push before it is live again (I-2, N-3); a superseded push stays
	// (N-6).
	gen  uint64
	live []livePush
	// keyHeld: the latest applied push answered held or behind. retry: the latest push failed transiently and is
	// retried out of band. givenUp: the retries ran out, or a retried push was refused outright. deferred: pushes
	// still in flight past the inline budget. Each one holds the user.
	keyHeld  bool
	retry    *pendingRetry
	givenUp  bool
	deferred int
	// unjudged: the list changed but the leaves were not re-judged yet (the re-judging failed): a retry re-judges
	// them even though its op 24 then changes nothing. Written only by the user's running attempt.
	unjudged bool
	// The worker: at most one attempt runs (current is its generation) and at most one waits (queued, the newest).
	running bool
	current uint64
	queued  *job
}

// liveGen is the user's live generation (0 when none).
func (u *userState) liveGen() uint64 {
	if l := u.live; len(l) > 0 {
		return l[len(l)-1].gen
	}
	return 0
}

// held reports whether the user is held, by any of the four holds.
func (u *userState) held() bool {
	return u.keyHeld || u.retry != nil || u.givenUp || u.deferred > 0
}

// inFlight reports whether an attempt of gen runs or waits.
func (u *userState) inFlight(gen uint64) bool {
	return u.running && u.current == gen || u.queued != nil && u.queued.gen == gen
}

// markApplied remembers that an attempt of gen applied with outcome o while it was not the live push.
func (u *userState) markApplied(gen uint64, o outcome) {
	for i := range u.live {
		if u.live[i].gen == gen {
			u.live[i].applied = &o
			return
		}
	}
}

// dropLive takes gen off the live stack.
func (u *userState) dropLive(gen uint64) {
	for i, e := range u.live {
		if e.gen == gen {
			u.live = append(u.live[:i:i], u.live[i+1:]...)
			return
		}
	}
}

// settleLive keeps only the live push on the stack: it applied, failed transiently or gave up, so no older push can
// become live again.
func (u *userState) settleLive() {
	if l := u.live; len(l) > 1 {
		u.live = l[len(l)-1:]
	}
}

// maxLiveStack bounds a user's live stack: superseded pushes stay on it (N-6), so a burst of pushes while one attempt
// hangs could otherwise grow it. The oldest entries that no attempt runs or waits for go first, never the live one: a
// rollback reaches them only if every newer push is refused outright.
const maxLiveStack = 8

func (u *userState) boundLive() {
	l := u.live
	for i := 0; len(l) > maxLiveStack && i < len(l)-1; {
		if u.inFlight(l[i].gen) {
			i++
			continue
		}
		l = append(l[:i:i], l[i+1:]...)
	}
	u.live = l
}

// Trust is one installation's membership trust. It outlives helpers: its TrustCore resolves the current one.
type Trust struct {
	core   TrustCore
	policy RetryPolicy
	clock  clock.Clock
	log    *slog.Logger
	inline time.Duration
	// groups, when set, says which groups the Bot is in for trust work: left groups (not a Bot channel) cost none
	// (M-5).
	groups func(id.ID) bool

	ctx    context.Context
	cancel context.CancelFunc

	// mu is the one lock: every per-user message is handled in one hold of it (see the invariant above).
	mu    sync.Mutex
	users map[id.ID]*userState
	// applied, when set, is told of every push that applied and changed the user's list (fix round 2, N-4).
	applied func(ctx context.Context, user id.ID, got native.DeviceListIngest)
}

// New returns the trust of one installation over core, retrying failed pushes on retry. Close stops its retries.
func New(core TrustCore, retry RetryPolicy) *Trust {
	if retry.Attempts < 1 {
		retry.Attempts = 1
	}
	if retry.First <= 0 {
		retry.First = DefaultRetry.First
	}
	if retry.Max < retry.First {
		retry.Max = retry.First
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Trust{core: core, policy: retry, clock: clock.System(), log: slog.New(slog.DiscardHandler), inline: DefaultInlineBudget,
		ctx: ctx, cancel: cancel, users: map[id.ID]*userState{}}
}

// WithApplied sets the callback told, off the caller, of every push that applied and changed its user's list,
// deferred pushes and retries included (fix round 2, N-4).
func (t *Trust) WithApplied(applied func(ctx context.Context, user id.ID, got native.DeviceListIngest)) *Trust {
	t.mu.Lock()
	t.applied = applied
	t.mu.Unlock()
	return t
}

// WithClock sets the clock the retries wait on (tests).
func (t *Trust) WithClock(c clock.Clock) *Trust {
	t.clock = c
	return t
}

// WithLogger sets the logger.
func (t *Trust) WithLogger(l *slog.Logger) *Trust {
	if l != nil {
		t.log = l
	}
	return t
}

// WithInlineBudget sets how long Pushed waits for a push's first attempt (DefaultInlineBudget).
func (t *Trust) WithInlineBudget(d time.Duration) *Trust {
	if d > 0 {
		t.inline = d
	}
	return t
}

// WithGroups limits Changed and Sweep to the groups for which in reports true (the Bot's Bot channel and call
// groups); nil, the default, is every group.
func (t *Trust) WithGroups(in func(id.ID) bool) *Trust {
	t.mu.Lock()
	t.groups = in
	t.mu.Unlock()
	return t
}

// Close stops every pending retry and every queued attempt. Held users stay held.
func (t *Trust) Close() {
	t.cancel()
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, u := range t.users {
		if u.retry != nil {
			u.retry.cancel()
		}
	}
}

func (r *pendingRetry) cancel() {
	if r.timer != nil {
		r.timer.Stop()
	}
	select {
	case <-r.stop:
	default:
		close(r.stop)
	}
}

// Held reports whether user's commands and leaves are refused: a push of theirs has not applied yet past the inline
// budget, failed and has not applied since (being retried, or given up), or their latest applied push answered held
// or behind.
func (t *Trust) Held(user id.ID) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	u := t.users[user]
	return u != nil && u.held()
}

// userLocked is user's state, created on first use. A user's generations never reset within one Trust.
func (t *Trust) userLocked(user id.ID) *userState {
	u := t.users[user]
	if u == nil {
		u = &userState{}
		t.users[user] = u
	}
	return u
}

// Pushed applies user's pushed history: op 24, then Changed when the list changed. It waits for the attempt at most
// the inline budget and never waits on another attempt of the user: then it returns ErrDeferred, the attempt finishes
// off the caller and the user is held until a push applies. A transient failure holds the user and schedules an
// out-of-band retry (it is returned, so the caller can log it or bind a fresh helper); a definitive refusal of the
// push is returned and changes no hold. A held or behind answer applies and returns ErrHeld.
func (t *Trust) Pushed(ctx context.Context, user id.ID, push []byte) (native.DeviceListIngest, error) {
	t.mu.Lock()
	u := t.userLocked(user)
	j := t.pushLocked(user, u, push)
	t.mu.Unlock()
	wait := time.NewTimer(t.inline)
	defer wait.Stop()
	select {
	case o := <-j.done:
		return o.got, o.err
	case <-wait.C:
	case <-ctx.Done():
	}
	t.mu.Lock()
	if !j.settled {
		j.deferred = true
		u.deferred++
	}
	t.mu.Unlock()
	select {
	case o := <-j.done:
		return o.got, o.err
	default:
		return native.DeviceListIngest{}, ErrDeferred
	}
}

// pushLocked is the fresh-push message (rule 1): the push takes the user's next generation, becomes the live push and
// is queued, all in the caller's hold of the lock. The returned job's done answers the attempt's outcome.
func (t *Trust) pushLocked(user id.ID, u *userState, push []byte) *job {
	u.gen++
	j := &job{push: push, gen: u.gen, attempts: 1, done: make(chan outcome, 1)}
	u.live = append(u.live, livePush{gen: j.gen, push: push, attempts: 1})
	u.boundLive()
	t.enqueueLocked(user, u, j)
	return j
}

// enqueueLocked makes j the user's waiting attempt, or supersedes it (rule 2), in the caller's hold of the lock. It
// answers false when j was not queued: j is then resolved and answered errSuperseded. Not queued are a retry job whose
// retry is no longer the user's or whose push is no longer live, and any job while a job of the same or a newer
// generation waits (fix round 3 N-5, fix round 4 N-7). A job of a newer generation supersedes the waiting one, which
// is resolved and answered errSuperseded; a superseded fresh push keeps its stack entry (N-6).
func (t *Trust) enqueueLocked(user id.ID, u *userState, j *job) bool {
	queue := j.retry == nil || u.retry == j.retry && j.gen == u.liveGen()
	if old := u.queued; queue && old != nil {
		if old.gen >= j.gen {
			queue = false
		} else {
			t.resolveLocked(u, old)
			old.done <- outcome{err: errSuperseded}
		}
	}
	if !queue {
		t.resolveLocked(u, j)
		j.done <- outcome{err: errSuperseded}
		return false
	}
	u.queued = j
	if !u.running {
		u.running = true
		go t.drive(user, u)
	}
	return true
}

// resolveLocked marks j settled and ends its deferral.
func (t *Trust) resolveLocked(u *userState, j *job) {
	if j.settled {
		return
	}
	j.settled = true
	if j.deferred {
		if u.deferred--; u.deferred < 0 {
			u.deferred = 0
		}
	}
}

// drive is the user's worker: it runs the attempts one at a time until none waits. Taking an attempt and settling it
// are each one message under the lock; the helper calls run between them, off the lock.
func (t *Trust) drive(user id.ID, u *userState) {
	for {
		t.mu.Lock()
		j := t.takeLocked(u)
		applied := t.applied
		t.mu.Unlock()
		if j == nil {
			return
		}
		var o outcome
		if t.ctx.Err() != nil {
			o.err = t.ctx.Err()
		} else {
			o.got, o.err = t.apply(t.ctx, user, u, j.push)
		}
		t.mu.Lock()
		t.settleLocked(user, u, j, o)
		t.mu.Unlock()
		if applied != nil && o.got.Changed && (o.err == nil || errors.Is(o.err, ErrHeld)) {
			applied(t.ctx, user, o.got)
		}
		j.done <- o
	}
}

// takeLocked is the worker's take message: the waiting job becomes the running one. A retry of a push that is no
// longer the user's live one, or that is no longer the user's retry, is dropped instead (rule 2; fix round 2, N-1):
// a newer push carries the whole history and owns the hold. With nothing to run the worker stops (nil).
func (t *Trust) takeLocked(u *userState) *job {
	for {
		j := u.queued
		u.queued = nil
		if j == nil {
			u.running, u.current = false, 0
			return nil
		}
		if j.retry != nil && (u.retry != j.retry || j.gen != u.liveGen()) {
			t.resolveLocked(u, j)
			j.done <- outcome{err: errSuperseded}
			continue
		}
		u.current = j.gen
		return j
	}
}

// apply is one attempt: op 24, then the user's leaves when the list changed (or an earlier re-judging of the user's
// leaves failed). A push that changed nothing (a snapshot re-sending a known list) costs op 24 alone; leaves left
// unjudged across a restart are found by Sweep.
func (t *Trust) apply(ctx context.Context, user id.ID, u *userState, push []byte) (native.DeviceListIngest, error) {
	got, err := t.core.IngestDeviceList(ctx, user, push)
	if err != nil {
		return got, err
	}
	t.mu.Lock()
	need := got.Changed || u.unjudged
	if need {
		u.unjudged = true
	}
	t.mu.Unlock()
	if need {
		if err := t.Changed(ctx, user); err != nil {
			return got, &changedError{err}
		}
		t.mu.Lock()
		u.unjudged = false
		t.mu.Unlock()
	}
	if got.State == native.DeviceListHeld || got.State == native.DeviceListBehind {
		return got, ErrHeld
	}
	return got, nil
}

// rollBackLocked runs after the live push was refused outright and left the live stack (rule 4): the push live again
// owns the user's hold. Unless an attempt of it is still running or waiting, it is retried (its retry, or a new one),
// so the user is released only when it applies again, never left held with nothing to retry it (fix round 2, N-1,
// N-3).
func (t *Trust) rollBackLocked(user id.ID, u *userState) {
	l := u.live
	if len(l) == 0 {
		return
	}
	e := l[len(l)-1]
	if u.inFlight(e.gen) {
		return
	}
	if e.applied != nil {
		// It already applied while a newer push owned the hold, and every push since was refused outright, which
		// changes nothing in the helper. Its outcome is the user's hold now (rule 3); no attempt needs to run.
		t.appliedLocked(u, *e.applied)
		return
	}
	// A fresh retry object: the old one's waiter reads its timer and stop channel without the lock.
	next := &pendingRetry{push: e.push, gen: e.gen, attempts: e.attempts}
	if r := u.retry; r != nil {
		r.cancel()
		if r.gen == e.gen {
			next.push, next.attempts = r.push, r.attempts
		}
	}
	u.retry = next
	t.armLocked(user, u, next)
}

// retryDelay is the wait before attempt number attempts+1.
func (t *Trust) retryDelay(attempts int) time.Duration {
	delay := t.policy.First
	for i := 1; i < attempts && delay < t.policy.Max; i++ {
		delay *= 2
	}
	return min(delay, t.policy.Max)
}

// armLocked starts r's wait for its next attempt.
func (t *Trust) armLocked(user id.ID, u *userState, r *pendingRetry) {
	r.stop = make(chan struct{})
	r.timer = t.clock.NewTimer(t.retryDelay(r.attempts))
	go t.await(user, u, r)
}

// settleLocked is the settle message: the outcome of attempt j. Only the live push applying releases the user (rule
// 3; I-2, fix round 2 N-1); an attempt of an older push changes no hold.
func (t *Trust) settleLocked(user id.ID, u *userState, j *job, o outcome) {
	t.resolveLocked(u, j)
	err := o.err
	if j.gen != u.liveGen() {
		// An older push: the newer one owns the hold. One that applied is remembered for a rollback; one refused
		// outright can never become live again.
		switch {
		case err == nil || errors.Is(err, ErrHeld):
			u.markApplied(j.gen, o)
		case Definitive(err):
			t.log.Warn("bot trust device list refused by the helper", "error", err.Error())
			if j.retry == nil {
				u.dropLive(j.gen)
			}
		}
		return
	}
	switch {
	case err == nil || errors.Is(err, ErrHeld):
		t.appliedLocked(u, o)
	case Definitive(err):
		// This push can never apply; the helper keeps the list it had. An earlier push still waiting for its retry
		// keeps waiting (a newer push the helper refuses does not supersede it). A retry that is refused outright gives
		// up: its push never applied, so the user stays held.
		if j.retry != nil && u.retry == j.retry {
			u.settleLive()
			u.retry = nil
			u.givenUp = true
			t.log.Warn("bot trust retried device list refused by the helper; the user stays held", "error", err.Error())
			return
		}
		t.log.Warn("bot trust device list refused by the helper", "error", err.Error())
		if j.retry == nil {
			// The refused push never counts: the push live before it is live again (fix round 2, N-3).
			u.dropLive(j.gen)
			t.rollBackLocked(user, u)
		}
	case j.attempts >= t.policy.Attempts || t.ctx.Err() != nil:
		u.settleLive()
		if r := u.retry; r != nil {
			r.cancel()
			u.retry = nil
		}
		u.givenUp = true
		t.log.Warn("bot trust device list given up after its retries; the user stays held", "attempts", j.attempts, "error", err.Error())
	default:
		u.settleLive()
		if old := u.retry; old != nil {
			old.cancel() // this push supersedes the retried one: it carries the whole history
		}
		r := &pendingRetry{push: j.push, gen: j.gen, attempts: j.attempts}
		u.retry = r
		t.log.Warn("bot trust device list held; retrying it out of band", "after", t.retryDelay(j.attempts), "attempt", j.attempts, "error", err.Error())
		t.armLocked(user, u, r)
	}
}

// appliedLocked settles the live push as applied with outcome o: the one path that releases a user (rule 3). A held
// or behind answer keeps the user held by the key instead. The entry remembers the outcome, so a rollback to it (every
// newer push refused outright) settles from it again instead of running the push a second time.
func (t *Trust) appliedLocked(u *userState, o outcome) {
	u.settleLive()
	if n := len(u.live); n > 0 {
		u.live[n-1].applied = &o
	}
	if r := u.retry; r != nil {
		r.cancel()
		u.retry = nil
	}
	u.givenUp = false
	if errors.Is(o.err, ErrHeld) {
		u.keyHeld = true
		if o.got.State == native.DeviceListBehind {
			t.log.Info("bot trust device list behind its signed head; the user is refused until a history reaching it arrives", "version", o.got.Version)
		} else {
			t.log.Info("bot trust key change held from a push; the user is refused until it is resolved", "version", o.got.Version)
		}
	} else {
		u.keyHeld = false
	}
}

// await queues r's next attempt once its timer fires. Kick and Close stop the wait (Kick then queues it itself).
func (t *Trust) await(user id.ID, u *userState, r *pendingRetry) {
	select {
	case <-t.ctx.Done():
		return
	case <-r.stop:
		return
	case <-r.timer.C():
	}
	t.queueRetry(user, u, r)
}

// queueRetry is the retry-due message: r's next attempt is checked and queued in one hold of the lock.
func (t *Trust) queueRetry(user id.ID, u *userState, r *pendingRetry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.queueRetryLocked(user, u, r)
}

// queueRetryLocked queues r's next attempt unless r is no longer the user's retry, its push is no longer live, or a
// job of the same or a newer generation waits (fix round 3, N-5): that job decides, and if it is refused outright the
// rollback retries r's push. Nothing is queued after Close.
func (t *Trust) queueRetryLocked(user id.ID, u *userState, r *pendingRetry) {
	if t.ctx.Err() != nil {
		return
	}
	t.enqueueLocked(user, u, &job{push: r.push, gen: r.gen, attempts: r.attempts + 1, retry: r, done: make(chan outcome, 1)})
}

// Kick retries every user whose push is waiting for a retry now: a fresh helper was bound (the failures were a closed
// helper). A user whose retries ran out is not retried; their next push is. Each user's retry is stopped and queued
// in this one hold of the lock (N-5).
func (t *Trust) Kick() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for user, u := range t.users {
		r := u.retry
		if r == nil {
			continue
		}
		select {
		case <-r.stop:
			continue
		default:
		}
		r.timer.Stop()
		close(r.stop) // the waiting goroutine leaves; this kick queues the attempt
		t.queueRetryLocked(user, u, r)
	}
}

func (t *Trust) inGroup(group id.ID) bool {
	t.mu.Lock()
	in := t.groups
	t.mu.Unlock()
	return in == nil || in(group)
}

// revalidate judges each leaf in turn. A leaf the helper no longer has (not_found: its group or leaf went away after
// the pending read) is skipped; any other failure is kept and the rest still run (M-2). A left group costs nothing
// (M-5).
func (t *Trust) revalidate(ctx context.Context, leaves []native.GroupPendingLeaf, want func(native.GroupPendingLeaf) bool) error {
	var first error
	for _, l := range leaves {
		if !want(l) || !t.inGroup(l.Group) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := t.core.RevalidateGroup(ctx, l.Group, uint64(l.Leaf), revalidateLimit)
		var status *helper.StatusError
		switch {
		case err == nil:
		case errors.As(err, &status) && status.Status == 7: // not_found
		case first == nil:
			first = err
		}
	}
	return first
}

// Changed re-judges user's current leaves in the Bot's groups whose list or pin changed since they were judged: one
// helper call per such leaf, none for any other user.
func (t *Trust) Changed(ctx context.Context, user id.ID) error {
	if user.IsZero() {
		return errors.New("bot trust: no user")
	}
	leaves, err := t.core.PendingLeaves(ctx)
	if err != nil {
		return err
	}
	return t.revalidate(ctx, leaves, func(l native.GroupPendingLeaf) bool { return l.User == user })
}

// Sweep re-judges every leaf the helper reports as changed since it was judged, whoever's it is: one pending-leaves
// read and one call per reported leaf, no list reads. The feed runs it once per applied snapshot, so leaves whose
// list changed while their re-judging was cut short (a Bot restart between op 24 and op 13) are judged without
// waiting for the user's next push. A current Bot channel costs one read and nothing else.
func (t *Trust) Sweep(ctx context.Context) error {
	leaves, err := t.core.PendingLeaves(ctx)
	if err != nil {
		return err
	}
	return t.revalidate(ctx, leaves, func(native.GroupPendingLeaf) bool { return true })
}

// Definitive reports whether err is a helper refusal of op 24 that a retry of the same push cannot change: invalid,
// forbidden, expired, conflict, not_found or upgrade_required. busy, unavailable, internal, an unknown status, every
// non-status failure (a closed helper, a timeout, a malformed answer) and any failure of re-judging the leaves are
// transient (R2-4, M-2).
func Definitive(err error) bool {
	var changed *changedError
	if errors.As(err, &changed) {
		return false
	}
	var status *helper.StatusError
	if !errors.As(err, &status) {
		return false
	}
	switch status.Status {
	case 1, 2, 3, 4, 7, 8:
		return true
	}
	return false
}
