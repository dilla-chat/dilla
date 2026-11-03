package api

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/store"
)

// The idle call-group close (web-3, L-HTTP-105, RV-2): an open call group that no live call names is
// closed 600 s after it was created, so a group a member poisoned with a device no list names, and
// that no call keeps live, refuses honest joiners for at most 10 minutes. The next joiner registers a
// fresh group.

const (
	// IdleCallGroupAge is how old an open call group with no live call is when the close takes it.
	IdleCallGroupAge = 600 * time.Second
	// IdleCloseInterval is the close's cadence on the instance clock.
	IdleCloseInterval = 60 * time.Second
	// maxIdleClosePerPass bounds one pass; the rest wait for the next, oldest first.
	maxIdleClosePerPass = 1024
	// idleClosePassBudget bounds one whole pass.
	idleClosePassBudget = 30 * time.Second
)

// CloseIdleCallGroups runs one pass of the idle close and answers how many groups it closed. It
// closes each group as an ending call does (closeCallGroup: through the delivery service when the
// call events handed one over).
//
// Attacker: can a member force-close a live call by waiting? No. The store query skips every group
// a live voice session names, and each group is re-checked just before its close. A start that
// writes its session between that re-check and the close, and re-read the group before the close
// landed (CALLS-2's re-read then saw it open), would leave a live call on a closed group, which no
// leaf could ever join or leave: the pass re-reads after the close and ends such a call, whose
// client retries on a fresh group — the same one retry a start racing the close costs.
func (h *Calls) CloseIdleCallGroups(ctx context.Context) int {
	ctx, cancel := context.WithTimeout(ctx, idleClosePassBudget)
	defer cancel()
	cutoff := h.clk.Now().Add(-IdleCallGroupAge).Unix()
	groups, err := h.repo.ListIdleCallGroups(ctx, cutoff, maxIdleClosePerPass)
	if err != nil {
		h.log.WarnContext(ctx, "listing idle call groups failed; the next pass retries", "err", err)
		return 0
	}
	closed := 0
	for _, g := range groups {
		if ctx.Err() != nil {
			break
		}
		if _, live, err := h.liveSessionOn(ctx, g); err != nil || live {
			continue
		}
		h.closeCallGroup(ctx, g.GroupID)
		closed++
		if row, live, err := h.liveSessionOn(ctx, g); err == nil && live {
			if err := h.endCall(context.WithoutCancel(ctx), row.CallID, row.LivekitRoom, "idle_group_closed"); err != nil {
				h.log.WarnContext(ctx, "ending a call that opened on a group as it was closed failed",
					"call_id", row.CallID.String(), "err", err)
			}
		}
	}
	if closed > 0 {
		h.log.InfoContext(ctx, "closed idle call groups", "groups", closed)
	}
	return closed
}

// liveSessionOn is the live voice session that names g, if any.
func (h *Calls) liveSessionOn(ctx context.Context, g store.GroupRow) (store.VoiceSessionRow, bool, error) {
	row, err := h.repo.GetVoiceSession(ctx, callIDOfGroup(g))
	if errors.Is(err, store.ErrNotFound) {
		return row, false, nil
	}
	if err != nil {
		return row, false, err
	}
	return row, row.Ended == nil && row.GroupID != nil && *row.GroupID == g.GroupID, nil
}

// StartIdleClose runs CloseIdleCallGroups every IdleCloseInterval of the instance clock on its own
// goroutine, checking every `every` of wall time, until the returned stop is called.
func (h *Calls) StartIdleClose(every time.Duration) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(every)
		defer tick.Stop()
		last := h.clk.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if h.clk.Since(last) >= IdleCloseInterval {
					last = h.clk.Now()
					h.CloseIdleCallGroups(ctx)
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}
