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

// livePush is one entry of a user's live stack: a push not refused outright and not superseded.
type livePush struct {
	gen      uint64
	push     []byte
	attempts int
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

// userWork is one user's worker: at most one attempt runs, and at most one waits (the newest).
type userWork struct {
	running bool
	current uint64 // the generation of the running attempt
	queued  *job
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

	mu sync.Mutex
	// keyHeld is the users whose latest applied push answered held or behind.
	keyHeld map[id.ID]bool
	// retries is the users whose latest push failed transiently: retried out of band, held meanwhile.
	retries map[id.ID]*pendingRetry
	// givenUp is the users whose retries ran out: held until a later push for them applies.
	givenUp map[id.ID]bool
	// deferred is the users with a push still in flight past the inline budget.
	deferred map[id.ID]int
	// unjudged is the users whose list changed but whose leaves were not re-judged yet (the re-judging failed): a
	// retry re-judges them even though its op 24 then changes nothing.
	unjudged map[id.ID]bool
	work     map[id.ID]*userWork
	// gen is the last generation given to a fresh push of each user. live is, per user, the generations not refused
	// outright and not superseded, oldest first: its last is the live push. Only an attempt of the live generation
	// changes a user's hold (fix round 2, N-1): an older push or its retry can apply, but never releases the user. A
	// push refused outright leaves the stack, so the push before it is live again (I-2, N-3).
	gen  map[id.ID]uint64
	live map[id.ID][]livePush
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
		ctx: ctx, cancel: cancel, keyHeld: map[id.ID]bool{}, retries: map[id.ID]*pendingRetry{}, givenUp: map[id.ID]bool{},
		deferred: map[id.ID]int{}, unjudged: map[id.ID]bool{}, work: map[id.ID]*userWork{}, gen: map[id.ID]uint64{}, live: map[id.ID][]livePush{}}
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
	for _, r := range t.retries {
		r.cancel()
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
	return t.keyHeld[user] || t.retries[user] != nil || t.givenUp[user] || t.deferred[user] > 0
}

// Pushed applies user's pushed history: op 24, then Changed when the list changed. It waits for the attempt at most
// the inline budget and never waits on another attempt of the user: then it returns ErrDeferred, the attempt finishes
// off the caller and the user is held until a push applies. A transient failure holds the user and schedules an
// out-of-band retry (it is returned, so the caller can log it or bind a fresh helper); a definitive refusal of the
// push is returned and changes no hold. A held or behind answer applies and returns ErrHeld.
func (t *Trust) Pushed(ctx context.Context, user id.ID, push []byte) (native.DeviceListIngest, error) {
	j := &job{push: push, attempts: 1, done: make(chan outcome, 1)}
	t.mu.Lock()
	t.gen[user]++
	j.gen = t.gen[user]
	t.live[user] = append(t.live[user], livePush{gen: j.gen, push: push, attempts: 1})
	t.mu.Unlock()
	t.enqueue(user, j)
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
		t.deferred[user]++
	}
	t.mu.Unlock()
	select {
	case o := <-j.done:
		return o.got, o.err
	default:
		return native.DeviceListIngest{}, ErrDeferred
	}
}

// enqueue makes j the user's next attempt: a waiting older one is superseded (a push carries the whole history).
func (t *Trust) enqueue(user id.ID, j *job) {
	t.mu.Lock()
	defer t.mu.Unlock()
	w := t.work[user]
	if w == nil {
		w = &userWork{}
		t.work[user] = w
	}
	if old := w.queued; old != nil {
		if old.retry == nil {
			// The superseded fresh push never runs: it leaves the live stack.
			t.dropLiveLocked(user, old.gen)
		}
		t.resolveLocked(user, old)
		old.done <- outcome{err: errSuperseded}
	}
	w.queued = j
	if !w.running {
		w.running = true
		go t.drive(user, w)
	}
}

// resolveLocked marks j settled and ends its deferral.
func (t *Trust) resolveLocked(user id.ID, j *job) {
	if j.settled {
		return
	}
	j.settled = true
	if j.deferred {
		if t.deferred[user]--; t.deferred[user] <= 0 {
			delete(t.deferred, user)
		}
	}
}

// drive runs the user's attempts one at a time until none waits.
func (t *Trust) drive(user id.ID, w *userWork) {
	for {
		t.mu.Lock()
		j := w.queued
		w.queued = nil
		if j == nil {
			w.running = false
			t.mu.Unlock()
			return
		}
		// A retry of a push that is no longer the user's live one, or that is no longer the user's retry, is dropped:
		// a newer push carries the whole history and owns the user's hold (fix round 2, N-1).
		if j.retry != nil && (t.retries[user] != j.retry || j.gen != t.liveLocked(user)) {
			t.resolveLocked(user, j)
			t.mu.Unlock()
			j.done <- outcome{err: errSuperseded}
			continue
		}
		applied := t.applied
		w.current = j.gen
		t.mu.Unlock()
		var o outcome
		if t.ctx.Err() != nil {
			o.err = t.ctx.Err()
		} else {
			o.got, o.err = t.apply(t.ctx, user, j.push)
		}
		t.settle(user, j, o)
		if applied != nil && o.got.Changed && (o.err == nil || errors.Is(o.err, ErrHeld)) {
			applied(t.ctx, user, o.got)
		}
		j.done <- o
	}
}

// apply is one attempt: op 24, then the user's leaves when the list changed (or an earlier re-judging of the user's
// leaves failed). A push that changed nothing (a snapshot re-sending a known list) costs op 24 alone; leaves left
// unjudged across a restart are found by Sweep.
func (t *Trust) apply(ctx context.Context, user id.ID, push []byte) (native.DeviceListIngest, error) {
	got, err := t.core.IngestDeviceList(ctx, user, push)
	if err != nil {
		return got, err
	}
	t.mu.Lock()
	need := got.Changed || t.unjudged[user]
	if need {
		t.unjudged[user] = true
	}
	t.mu.Unlock()
	if need {
		if err := t.Changed(ctx, user); err != nil {
			return got, &changedError{err}
		}
		t.mu.Lock()
		delete(t.unjudged, user)
		t.mu.Unlock()
	}
	if got.State == native.DeviceListHeld || got.State == native.DeviceListBehind {
		return got, ErrHeld
	}
	return got, nil
}

// liveLocked is user's live generation (0 when none).
func (t *Trust) liveLocked(user id.ID) uint64 {
	if l := t.live[user]; len(l) > 0 {
		return l[len(l)-1].gen
	}
	return 0
}

// dropLiveLocked takes gen off user's live stack.
func (t *Trust) dropLiveLocked(user id.ID, gen uint64) {
	l := t.live[user]
	for i, e := range l {
		if e.gen == gen {
			t.live[user] = append(l[:i:i], l[i+1:]...)
			break
		}
	}
	if len(t.live[user]) == 0 {
		delete(t.live, user)
	}
}

// settleLiveLocked keeps only user's live push on the stack: it applied, failed transiently or gave up, so no older
// push can become live again.
func (t *Trust) settleLiveLocked(user id.ID) {
	if l := t.live[user]; len(l) > 1 {
		t.live[user] = l[len(l)-1:]
	}
}

// inFlightLocked reports whether an attempt of gen runs or waits for user.
func (t *Trust) inFlightLocked(user id.ID, gen uint64) bool {
	w := t.work[user]
	return w != nil && (w.running && w.current == gen || w.queued != nil && w.queued.gen == gen)
}

// rollBackLocked runs after a push was refused outright and left the live stack: the push live again owns the
// user's hold. Unless an attempt of it is still running or waiting, it is retried (its retry, or a new one), so the
// user is released only when it applies again, never left held with nothing to retry it (fix round 2, N-1, N-3).
func (t *Trust) rollBackLocked(user id.ID) {
	l := t.live[user]
	if len(l) == 0 {
		return
	}
	e := l[len(l)-1]
	if t.inFlightLocked(user, e.gen) {
		return
	}
	// A fresh retry object: the old one's waiter reads its timer and stop channel without the lock.
	next := &pendingRetry{push: e.push, gen: e.gen, attempts: e.attempts}
	if r := t.retries[user]; r != nil {
		r.cancel()
		if r.gen == e.gen {
			next.push, next.attempts = r.push, r.attempts
		}
	}
	t.retries[user] = next
	t.armLocked(user, next)
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
func (t *Trust) armLocked(user id.ID, r *pendingRetry) {
	r.stop = make(chan struct{})
	r.timer = t.clock.NewTimer(t.retryDelay(r.attempts))
	go t.await(user, r)
}

// settle records the outcome of attempt j for user. Only the user's live push applying releases the user (I-2,
// fix round 2 N-1); an attempt of an older push changes no hold.
func (t *Trust) settle(user id.ID, j *job, o outcome) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.resolveLocked(user, j)
	err := o.err
	if j.gen != t.liveLocked(user) {
		// An older push: the newer one owns the hold. One refused outright can never become live again.
		if Definitive(err) {
			t.log.Warn("bot trust device list refused by the helper", "error", err.Error())
			if j.retry == nil {
				t.dropLiveLocked(user, j.gen)
			}
		}
		return
	}
	switch {
	case err == nil || errors.Is(err, ErrHeld):
		t.settleLiveLocked(user)
		if r := t.retries[user]; r != nil {
			r.cancel()
			delete(t.retries, user)
		}
		delete(t.givenUp, user)
		if errors.Is(err, ErrHeld) {
			t.keyHeld[user] = true
			if o.got.State == native.DeviceListBehind {
				t.log.Info("bot trust device list behind its signed head; the user is refused until a history reaching it arrives", "version", o.got.Version)
			} else {
				t.log.Info("bot trust key change held from a push; the user is refused until it is resolved", "version", o.got.Version)
			}
		} else {
			delete(t.keyHeld, user)
		}
	case Definitive(err):
		// This push can never apply; the helper keeps the list it had. An earlier push still waiting for its retry
		// keeps waiting (a newer push the helper refuses does not supersede it). A retry that is refused outright gives
		// up: its push never applied, so the user stays held.
		if j.retry != nil && t.retries[user] == j.retry {
			t.settleLiveLocked(user)
			delete(t.retries, user)
			t.givenUp[user] = true
			t.log.Warn("bot trust retried device list refused by the helper; the user stays held", "error", err.Error())
			return
		}
		t.log.Warn("bot trust device list refused by the helper", "error", err.Error())
		if j.retry == nil {
			// The refused push never counts: the push live before it is live again (fix round 2, N-3).
			t.dropLiveLocked(user, j.gen)
			t.rollBackLocked(user)
		}
	case j.attempts >= t.policy.Attempts || t.ctx.Err() != nil:
		t.settleLiveLocked(user)
		if r := t.retries[user]; r != nil {
			r.cancel()
			delete(t.retries, user)
		}
		t.givenUp[user] = true
		t.log.Warn("bot trust device list given up after its retries; the user stays held", "attempts", j.attempts, "error", err.Error())
	default:
		t.settleLiveLocked(user)
		if old := t.retries[user]; old != nil {
			old.cancel() // this push supersedes the retried one: it carries the whole history
		}
		r := &pendingRetry{push: j.push, gen: j.gen, attempts: j.attempts}
		t.retries[user] = r
		t.log.Warn("bot trust device list held; retrying it out of band", "after", t.retryDelay(j.attempts), "attempt", j.attempts, "error", err.Error())
		t.armLocked(user, r)
	}
}

// await queues r's next attempt once its timer fires. Kick and Close stop the wait (Kick then queues it itself).
func (t *Trust) await(user id.ID, r *pendingRetry) {
	select {
	case <-t.ctx.Done():
		return
	case <-r.stop:
		return
	case <-r.timer.C():
	}
	t.queueRetry(user, r)
}

// queueRetry queues r's next attempt unless r is no longer the user's retry or a newer push already waits.
func (t *Trust) queueRetry(user id.ID, r *pendingRetry) {
	t.mu.Lock()
	current := t.retries[user] == r && r.gen == t.liveLocked(user)
	w := t.work[user]
	waiting := w != nil && w.queued != nil // a newer push decides; if it is refused, the rollback retries r
	t.mu.Unlock()
	if !current || waiting || t.ctx.Err() != nil {
		return
	}
	t.enqueue(user, &job{push: r.push, gen: r.gen, attempts: r.attempts + 1, retry: r, done: make(chan outcome, 1)})
}

// Kick retries every user whose push is waiting for a retry now: a fresh helper was bound (the failures were a closed
// helper). A user whose retries ran out is not retried; their next push is.
func (t *Trust) Kick() {
	t.mu.Lock()
	due := map[id.ID]*pendingRetry{}
	for u, r := range t.retries {
		select {
		case <-r.stop:
			continue
		default:
		}
		r.timer.Stop()
		close(r.stop) // the waiting goroutine leaves; this kick queues the attempt
		due[u] = r
	}
	t.mu.Unlock()
	for u, r := range due {
		t.queueRetry(u, r)
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
