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
	attempts int // attempts made, the first included
	timer    clock.Timer
	stop     chan struct{}
}

// job is one attempt of a push, run by the user's worker.
type job struct {
	push     []byte
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
		deferred: map[id.ID]int{}, unjudged: map[id.ID]bool{}, work: map[id.ID]*userWork{}}
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
		t.mu.Unlock()
		var o outcome
		if t.ctx.Err() != nil {
			o.err = t.ctx.Err()
		} else {
			o.got, o.err = t.apply(t.ctx, user, j.push)
		}
		t.settle(user, j, o)
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

// settle records the outcome of attempt j for user. Only an applied push releases the user (I-2).
func (t *Trust) settle(user id.ID, j *job, o outcome) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.resolveLocked(user, j)
	err := o.err
	switch {
	case err == nil || errors.Is(err, ErrHeld):
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
			delete(t.retries, user)
			t.givenUp[user] = true
			t.log.Warn("bot trust retried device list refused by the helper; the user stays held", "error", err.Error())
			return
		}
		t.log.Warn("bot trust device list refused by the helper", "error", err.Error())
	case j.attempts >= t.policy.Attempts || t.ctx.Err() != nil:
		if r := t.retries[user]; r != nil {
			r.cancel()
			delete(t.retries, user)
		}
		t.givenUp[user] = true
		t.log.Warn("bot trust device list given up after its retries; the user stays held", "attempts", j.attempts, "error", err.Error())
	default:
		delay := t.policy.First
		for i := 1; i < j.attempts && delay < t.policy.Max; i++ {
			delay *= 2
		}
		delay = min(delay, t.policy.Max)
		if old := t.retries[user]; old != nil {
			old.cancel() // this push supersedes the retried one: it carries the whole history
		}
		r := &pendingRetry{push: j.push, attempts: j.attempts, stop: make(chan struct{}), timer: t.clock.NewTimer(delay)}
		t.retries[user] = r
		t.log.Warn("bot trust device list held; retrying it out of band", "after", delay, "attempt", j.attempts, "error", err.Error())
		go t.await(user, r)
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
	current := t.retries[user] == r
	w := t.work[user]
	waiting := w != nil && w.queued != nil
	t.mu.Unlock()
	if !current || waiting || t.ctx.Err() != nil {
		return
	}
	t.enqueue(user, &job{push: r.push, attempts: r.attempts + 1, retry: r, done: make(chan outcome, 1)})
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
