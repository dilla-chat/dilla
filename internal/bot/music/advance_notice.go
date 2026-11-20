package music

import (
	"context"
	"sync"
)

// An advance's state-change notice waits for the next track's start (G3: the gap between queued tracks is at most
// 150 ms). The queue store's change callback (cmd/dillabot: a call hint) makes every listener's client read the music
// state at once, and each read is a durable command open on the Bot's one helper. Sent before the start, those reads
// ran between the start's own helper calls, one disk sync each: on a runner whose syncs took about 120 ms, the two
// listeners' reads made the switch 720 ms and the gap 550 ms (CI run 44790568832).

type deferredNoticesKey struct{}

type deferredNotices struct {
	mu      sync.Mutex
	notices []func()
}

// withDeferredNotices returns the context of an advance's save, and flush, which sends the notices DeferNotice held
// under it, in order. flush is idempotent.
func withDeferredNotices(ctx context.Context) (context.Context, func()) {
	d := &deferredNotices{}
	return context.WithValue(ctx, deferredNoticesKey{}, d), func() {
		d.mu.Lock()
		notices := d.notices
		d.notices = nil
		d.mu.Unlock()
		for _, notice := range notices {
			notice()
		}
	}
}

// DeferNotice holds notice until the advance whose save ctx belongs to has started the next track (or failed to),
// and reports whether it did. Outside an advance it holds nothing and reports false: the caller sends notice itself.
func DeferNotice(ctx context.Context, notice func()) bool {
	d, _ := ctx.Value(deferredNoticesKey{}).(*deferredNotices)
	if d == nil || notice == nil {
		return false
	}
	d.mu.Lock()
	d.notices = append(d.notices, notice)
	d.mu.Unlock()
	return true
}
