// Package trust is the Bot's MLS membership trust, driven by pushed device lists (Bot v2 Task 16). It replaces the
// v1 sender trust refresh (internal/bot/service/source_trust.go, deleted with the v1 intake by Task 23), whose
// all-member passes read every member's signed history on a timer and per command.
//
// dillad pushes a user's whole signed device-list history (op 65 device_list) when it changes. Pushed hands it to
// helper op 24, then Changed re-judges only that user's current leaves in the Bot's groups: the helper's pending
// leaves are, for a Bot, exactly the leaves whose user's list or pinned key changed since they were last judged
// (core/dilla-core pending_leaves). That is O(that user's leaves) per push, and nothing on a timer.
//
// A user whose push cannot be applied is Held: their commands and leaves are refused (fail closed). Held covers:
//   - a transient failure of op 24 or of the re-judging (a helper restart, a timeout, a busy, unavailable or internal
//     helper status): the push is retried out of band on the RetryPolicy, per user, so a failing user never blocks
//     another user's events. When the attempts run out the user stays held until a later push for them applies;
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

// revalidateLimit is op 13's page bound; the helper drains every page of the leaf in one call.
const revalidateLimit = 256

// ErrHeld is returned by Pushed when the push applied but the user stays refused: a key change is pending, or the list
// is behind its signed head.
var ErrHeld = errors.New("bot trust: the user's pushed list is held")

// pendingRetry is one user's pending out-of-band retry.
type pendingRetry struct {
	push     []byte
	attempts int // attempts made, the first included
	timer    clock.Timer
	stop     chan struct{}
}

// Trust is one installation's membership trust. It outlives helpers: its TrustCore resolves the current one.
type Trust struct {
	core   TrustCore
	policy RetryPolicy
	clock  clock.Clock
	log    *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc

	mu sync.Mutex
	// keyHeld is the users whose latest applied push answered held or behind.
	keyHeld map[id.ID]bool
	// retries is the users whose latest push failed transiently: retried out of band, held meanwhile.
	retries map[id.ID]*pendingRetry
	// givenUp is the users whose retries ran out: held until a later push for them applies.
	givenUp map[id.ID]bool
	// unjudged is the users whose list changed but whose leaves were not re-judged yet (the re-judging failed): a
	// retry re-judges them even though its op 24 then changes nothing.
	unjudged map[id.ID]bool
	// users serialises the work of one user (an inline push and its retry never interleave); different users never
	// wait on each other.
	users map[id.ID]*sync.Mutex
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
	return &Trust{core: core, policy: retry, clock: clock.System(), log: slog.New(slog.DiscardHandler), ctx: ctx, cancel: cancel,
		keyHeld: map[id.ID]bool{}, retries: map[id.ID]*pendingRetry{}, givenUp: map[id.ID]bool{}, unjudged: map[id.ID]bool{}, users: map[id.ID]*sync.Mutex{}}
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

// Close stops every pending retry. Held users stay held.
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

// Held reports whether user's commands and leaves are refused: a push of theirs failed and has not applied since (being
// retried, or given up), or their latest push answered held or behind.
func (t *Trust) Held(user id.ID) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.keyHeld[user] || t.retries[user] != nil || t.givenUp[user]
}

func (t *Trust) userLock(user id.ID) *sync.Mutex {
	t.mu.Lock()
	defer t.mu.Unlock()
	m := t.users[user]
	if m == nil {
		m = &sync.Mutex{}
		t.users[user] = m
	}
	return m
}

// Pushed applies user's pushed history: op 24, then Changed. It never blocks on another user. A transient failure holds
// the user and schedules an out-of-band retry (it is returned, so the caller can log it or bind a fresh helper); a
// definitive refusal of the push (a malformed or refused history) is returned and holds no one for it. A held or behind
// answer applies and returns ErrHeld: the user is refused until a later push answers current.
func (t *Trust) Pushed(ctx context.Context, user id.ID, push []byte) (native.DeviceListIngest, error) {
	lock := t.userLock(user)
	lock.Lock()
	defer lock.Unlock()
	t.mu.Lock()
	if old := t.retries[user]; old != nil {
		old.cancel() // a newer push supersedes the retried one: it carries the whole history
		delete(t.retries, user)
	}
	t.mu.Unlock()
	got, err := t.apply(ctx, user, push)
	t.settle(user, push, 1, got, err)
	return got, err
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
			return got, err
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

// settle records the outcome of attempt number attempts of push for user. It runs under the user's lock, so no other
// attempt for the user is in flight and the user's state is this attempt's to write.
func (t *Trust) settle(user id.ID, push []byte, attempts int, got native.DeviceListIngest, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case err == nil || errors.Is(err, ErrHeld):
		delete(t.retries, user)
		delete(t.givenUp, user)
		if errors.Is(err, ErrHeld) {
			t.keyHeld[user] = true
			if got.State == native.DeviceListBehind {
				t.log.Info("bot trust device list behind its signed head; the user is refused until a history reaching it arrives", "version", got.Version)
			} else {
				t.log.Info("bot trust key change held from a push; the user is refused until it is resolved", "version", got.Version)
			}
		} else {
			delete(t.keyHeld, user)
		}
	case Definitive(err):
		// The push can never apply; the helper keeps the list it had. Any earlier hold of the user stays.
		delete(t.retries, user)
		t.log.Warn("bot trust device list refused by the helper", "error", err.Error())
	case attempts >= t.policy.Attempts || t.ctx.Err() != nil:
		delete(t.retries, user)
		t.givenUp[user] = true
		t.log.Warn("bot trust device list given up after its retries; the user stays held", "attempts", attempts, "error", err.Error())
	default:
		delay := t.policy.First
		for i := 1; i < attempts && delay < t.policy.Max; i++ {
			delay *= 2
		}
		delay = min(delay, t.policy.Max)
		r := &pendingRetry{push: push, attempts: attempts, stop: make(chan struct{}), timer: t.clock.NewTimer(delay)}
		t.retries[user] = r
		t.log.Warn("bot trust device list held; retrying it out of band", "after", delay, "attempt", attempts, "error", err.Error())
		go t.await(user, r)
	}
}

// await runs r once its timer fires. Kick and Close stop the wait (Kick then runs the attempt itself).
func (t *Trust) await(user id.ID, r *pendingRetry) {
	select {
	case <-t.ctx.Done():
		return
	case <-r.stop:
		return
	case <-r.timer.C():
	}
	t.run(user, r)
}

// run is one out-of-band attempt of r.
func (t *Trust) run(user id.ID, r *pendingRetry) {
	lock := t.userLock(user)
	lock.Lock()
	defer lock.Unlock()
	t.mu.Lock()
	current := t.retries[user] == r
	t.mu.Unlock()
	if !current || t.ctx.Err() != nil {
		return
	}
	got, err := t.apply(t.ctx, user, r.push)
	t.settle(user, r.push, r.attempts+1, got, err)
}

// Kick retries every user whose push is waiting for a retry now: a fresh helper was bound (the failures were a closed
// helper). A user whose retries ran out is not retried; their next push is.
func (t *Trust) Kick() {
	t.mu.Lock()
	var due []struct {
		user id.ID
		r    *pendingRetry
	}
	for u, r := range t.retries {
		select {
		case <-r.stop:
			continue
		default:
		}
		r.timer.Stop()
		close(r.stop) // the waiting goroutine leaves; this kick runs the attempt
		due = append(due, struct {
			user id.ID
			r    *pendingRetry
		}{u, r})
	}
	t.mu.Unlock()
	for _, d := range due {
		go t.run(d.user, d.r)
	}
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
	for _, l := range leaves {
		if l.User != user {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := t.core.RevalidateGroup(ctx, l.Group, uint64(l.Leaf), revalidateLimit); err != nil {
			return err
		}
	}
	return nil
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
	for _, l := range leaves {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := t.core.RevalidateGroup(ctx, l.Group, uint64(l.Leaf), revalidateLimit); err != nil {
			return err
		}
	}
	return nil
}

// Definitive reports whether err is a helper refusal that a retry of the same push cannot change: invalid, forbidden,
// expired, conflict, not_found or upgrade_required. busy, unavailable, internal, an unknown status and every non-status
// failure (a closed helper, a timeout, a malformed answer) are transient (R2-4).
func Definitive(err error) bool {
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
