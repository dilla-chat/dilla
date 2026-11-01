package ds

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/mlswasi"
)

type deviceListScopeKey struct{}

// deviceListScope lends one owned native instance only until its synchronous
// callback returns. Late use fails closed; concurrent verification is serialized.
// The runtime identity prevents a different verifier from borrowing this guest.
type deviceListScope struct {
	mu       sync.Mutex
	runtime  *mlswasi.Runtime
	verifier DeviceListVerifier
	active   atomic.Bool
	owner    context.Context
}

func (s *deviceListScope) close() {
	s.active.Store(false)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.verifier = nil
}
func (s *deviceListScope) run(ctx context.Context, fn func(context.Context) error) error {
	if fn == nil {
		return ErrDeviceListUnavailable
	}
	if !s.active.Load() || s.owner.Err() != nil {
		return ErrDeviceListUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	scoped, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.owner, cancel)
	defer stop()
	defer cancel()
	return fn(context.WithValue(scoped, deviceListScopeKey{}, s))
}
func withBorrowedDeviceListVerifier(ctx context.Context, runtime *mlswasi.Runtime, verifier DeviceListVerifier, fn func(context.Context) error) error {
	scope := &deviceListScope{runtime: runtime, verifier: verifier, owner: ctx}
	scope.active.Store(true)
	defer scope.close()
	return scope.run(ctx, fn)
}
func (s *deviceListScope) verify(ctx context.Context, runtime *mlswasi.Runtime, blob, ssk []byte, user id.ID) ([]mlswasi.DeviceListEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active.Load() || s.runtime != runtime || s.verifier == nil {
		return nil, ErrDeviceListUnavailable
	}
	if err := s.owner.Err(); err != nil {
		s.active.Store(false)
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		s.active.Store(false)
		return nil, err
	}
	callCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.owner, cancel)
	defer stop()
	defer cancel()
	entries, err := s.verifier.DeviceListEntries(callCtx, blob, ssk, user[:])
	if ownerErr := s.owner.Err(); ownerErr != nil {
		s.active.Store(false)
		return nil, ownerErr
	}
	if contextErr := ctx.Err(); contextErr != nil {
		s.active.Store(false)
		return nil, contextErr
	}
	if errors.Is(err, mlswasi.ErrTrap) {
		s.active.Store(false)
	}
	return entries, err
}

// WithVerification reserves the native guest before the caller enters a write
// transaction. Verify still performs every signature check through the supplied
// DeviceLists implementation, including wrappers. Matching nested scopes reuse
// the lease; no verification may use it after the owning callback returns.
func (c *coreDeviceLists) WithVerification(ctx context.Context, fn func(context.Context) error) error {
	if fn == nil || c.wasm == nil {
		return ErrDeviceListUnavailable
	}
	if scope, ok := ctx.Value(deviceListScopeKey{}).(*deviceListScope); ok {
		valid := scope.active.Load() && scope.runtime == c.wasm && scope.owner.Err() == nil
		if !valid {
			return ErrDeviceListUnavailable
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return fn(ctx)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	inst, err := c.wasm.Acquire(ctx)
	if err != nil {
		return err
	}
	defer inst.Release()
	if err := ctx.Err(); err != nil {
		return err
	}
	return withBorrowedDeviceListVerifier(ctx, c.wasm, inst, fn)
}

// WithDeviceListVerification lends this read callback's already-owned guest to
// signed-list checks. It must be called synchronously inside the fenced read
// callback; saved contexts or proof values cannot extend that guest's lifetime.
func (c CurrentBotInteractionGroupContext) WithDeviceListVerification(ctx context.Context, fn func(context.Context) error) error {
	if c.verification == nil {
		return errors.New("ds: current interaction verification scope is unavailable")
	}
	return c.verification.run(ctx, fn)
}
