package music

import (
	"context"
	"sync"
	"time"
)

// An advance's state-change notice waits for the next track's start (G3: the gap between queued tracks is at most
// 150 ms), but never longer than advanceNoticeLimit (ruling R36). The queue store's change callback (cmd/dillabot: a
// call hint) makes every listener's client read the music state at once, and each read is a durable command open on
// the Bot's one helper. Sent before the start, those reads ran between the start's own helper calls, one disk sync
// each: on a runner whose syncs took about 120 ms, the two listeners' reads made the switch 720 ms and the gap 550 ms
// (CI run 44790568832). A start that resolves its track first (a queued YouTube item, up to maxStartDuration) must not
// hold the listeners' now-playing that long: after the limit the notice goes out while the start goes on.
const advanceNoticeLimit = 500 * time.Millisecond

// WithAdvanceNoticeClock injects the scheduler of an advance's notice limit (advanceNoticeLimit).
func WithAdvanceNoticeClock(after func(time.Duration, func()) Timer) SupervisorOption {
	return func(s *Supervisor) { s.noticeAfter = after }
}

type deferredNoticesKey struct{}

type deferredNotices struct {
	mu      sync.Mutex
	flushed bool
	notices []func()
}

// withDeferredNotices returns the context of an advance's save, and flush, which sends the notices DeferNotice held
// under it, in order. flush is idempotent and safe from any goroutine; once it ran, DeferNotice holds nothing more.
func withDeferredNotices(ctx context.Context) (context.Context, func()) {
	d := &deferredNotices{}
	return context.WithValue(ctx, deferredNoticesKey{}, d), func() {
		d.mu.Lock()
		notices := d.notices
		d.notices, d.flushed = nil, true
		d.mu.Unlock()
		for _, notice := range notices {
			notice()
		}
	}
}

// DeferNotice holds notice until the advance whose save ctx belongs to has started the next track (or failed to, or
// advanceNoticeLimit passed), and reports whether it did. Outside an advance, or once its notices went out, it holds
// nothing and reports false: the caller sends notice itself.
func DeferNotice(ctx context.Context, notice func()) bool {
	d, _ := ctx.Value(deferredNoticesKey{}).(*deferredNotices)
	if d == nil || notice == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.flushed {
		return false
	}
	d.notices = append(d.notices, notice)
	return true
}
