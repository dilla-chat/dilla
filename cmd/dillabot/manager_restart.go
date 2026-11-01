package main

import (
	"context"
	"errors"
	"slices"

	"github.com/jonasthim/dilla/internal/id"
)

// DisableAndWait retires the installation and waits for every native borrower
// and the helper's actual Close to finish before its store can be reopened.
// Cancellation stops waiting; it never abandons the retired helper's cleanup.
func (r *musicQueueRegistry) DisableAndWait(ctx context.Context, installation id.ID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r == nil {
		return nil
	}
	unlock := r.lifecycle.lock(installation)
	retained, closeNative := r.retire(installation)
	r.mu.Lock()
	pending := slices.Clone(r.retiring[installation])
	// The last borrower may have completed and removed this generation after
	// retire unlocked. Keep this call's generation so its close error survives.
	if retained != nil && !slices.Contains(pending, retained) {
		pending = append(pending, retained)
	}
	r.mu.Unlock()
	unlock()
	if closeNative {
		// Even a slow native Close must not hold an installation lifecycle
		// lock or prevent the restart waiter from observing cancellation.
		go func() {
			// Waiters receive the result through closeErr after closeDone.
			_ = r.finishNativeClose(installation, retained)
		}()
	}
	var errs []error
	for _, retired := range pending {
		select {
		case <-retired.closeDone:
			errs = append(errs, retired.closeErr)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return errors.Join(errs...)
}
