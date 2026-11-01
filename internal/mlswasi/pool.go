package mlswasi

import (
	"context"
	"errors"
	"fmt"

	"github.com/tetratelabs/wazero/api"

	"github.com/jonasthim/dilla/internal/prioritygate"
)

// ErrTrap is returned when the guest traps. The caller must discard the
// instance: linear memory, the allocator included, may be mid-update
// (gap-17 6 item 2). Release does that automatically.
var ErrTrap = errors.New("mlswasi: guest trapped")

// ErrReleased is returned by Call on an instance that has already been Released.
// The module behind it belongs to the pool again — or is closed — so using this
// handle would race whoever holds it now.
var ErrReleased = errors.New("mlswasi: instance already released")

// Instance is one module instance. It is not safe for concurrent use: hold it
// for the duration of a call sequence and Release it afterwards.
type Instance struct {
	rt    *Runtime
	mod   api.Module
	alloc api.Function
	free  api.Function

	poisoned bool
	release  func()
}

// Acquire takes one instance out of the pool, blocking until one is free or ctx
// is done.
func (r *Runtime) Acquire(ctx context.Context) (*Instance, error) {
	priority := 0
	if r.priority != nil {
		priority = r.priority(ctx)
	}
	release, err := r.gate.Acquire(ctx, priority)
	if err != nil {
		if errors.Is(err, prioritygate.ErrClosed) {
			err = ErrClosed
		} else if errors.Is(err, prioritygate.ErrDepleted) {
			err = ErrDepleted
		}
		return nil, err
	}
	select {
	case inst := <-r.pool:
		inst.release = release
		if err := ctx.Err(); err != nil {
			inst.Release()
			return nil, err
		}
		select {
		case <-r.done:
			inst.Release()
			return nil, ErrClosed
		default:
		}
		return inst, nil
	case <-r.done:
		release()
		return nil, ErrClosed
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	}
}

// Release returns the instance to the pool. A poisoned instance is closed and
// replaced instead.
//
// Release is idempotent. It clears this handle's module first, so a second call
// is ignored rather than acted on: a double Release of a healthy instance would
// otherwise queue it twice, the pool would hand one module to two callers at
// once — one linear memory, one guest allocator, and api.Function.Call is not
// goroutine-safe — and the damage would appear somewhere else entirely, as a
// corrupt response to an unrelated call. The pool receives a fresh handle over
// the same module rather than this one, because this one is now spent in the
// caller's hands; Call says so instead of dereferencing a nil module.
func (i *Instance) Release() {
	mod := i.mod
	if mod == nil {
		return // already released
	}
	i.mod = nil
	if i.release != nil {
		defer i.release()
	}

	r := i.rt
	r.execution.RLock()
	defer r.execution.RUnlock()
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()

	ctx := context.Background()
	if closed {
		_ = mod.Close(ctx)
		return
	}
	if !i.poisoned {
		r.pool <- &Instance{rt: r, mod: mod, alloc: i.alloc, free: i.free}
		return
	}
	_ = mod.Close(ctx)
	replacement, err := r.newInstance(ctx)
	if err != nil {
		// This admission still owns the failed resource. Retire its capacity
		// before the deferred release can admit anyone else, so gate capacity
		// always tracks the usable pool, including at zero.
		r.gate.Retire()
		return
	}
	r.pool <- replacement
}

// invoke calls one exported function and converts any guest-side failure into
// ErrTrap, poisoning the instance.
func (i *Instance) invoke(ctx context.Context, name string, fn api.Function, params ...uint64) ([]uint64, error) {
	if fn == nil {
		return nil, fmt.Errorf("mlswasi: no export %q", name)
	}
	out, err := fn.Call(ctx, params...)
	if err != nil {
		i.poisoned = true
		return nil, fmt.Errorf("%w: %s: %w", ErrTrap, name, err)
	}
	return out, nil
}

// Call writes req into guest memory with dilla_alloc, invokes export, copies the
// response out of linear memory and frees both buffers. The returned slice is
// owned by the caller and survives later guest calls.
func (i *Instance) Call(ctx context.Context, export string, req []byte) ([]byte, error) {
	if i.mod == nil {
		return nil, ErrReleased
	}
	i.rt.execution.RLock()
	defer i.rt.execution.RUnlock()
	select {
	case <-i.rt.done:
		return nil, ErrClosed
	default:
	}
	if i.poisoned {
		return nil, ErrTrap
	}
	fn := i.mod.ExportedFunction(export)
	if fn == nil {
		return nil, fmt.Errorf("mlswasi: no export %q", export)
	}
	if i.rt != nil && i.rt.onCall != nil {
		i.rt.onCall(export)
	}
	reqLen := uint32(len(req))

	allocated, err := i.invoke(ctx, "dilla_alloc", i.alloc, uint64(reqLen))
	if err != nil {
		return nil, err
	}
	reqPtr := uint32(allocated[0])
	if reqPtr == 0 && reqLen != 0 {
		i.poisoned = true
		return nil, fmt.Errorf("%w: dilla_alloc returned a null pointer for %d bytes", ErrTrap, reqLen)
	}
	defer func() {
		if !i.poisoned {
			_, _ = i.invoke(ctx, "dilla_free", i.free, uint64(reqPtr), uint64(reqLen))
		}
	}()

	if !i.mod.Memory().Write(reqPtr, req) {
		i.poisoned = true
		return nil, fmt.Errorf("%w: writing %d bytes at %d exceeds the %d-byte memory",
			ErrTrap, reqLen, reqPtr, i.mod.Memory().Size())
	}

	packed, err := i.invoke(ctx, export, fn, uint64(reqPtr), uint64(reqLen))
	if err != nil {
		return nil, err
	}
	respPtr := uint32(packed[0] >> 32)
	respLen := uint32(packed[0])

	view, ok := i.mod.Memory().Read(respPtr, respLen)
	if !ok {
		i.poisoned = true
		return nil, fmt.Errorf("%w: reading %d bytes at %d exceeds the %d-byte memory",
			ErrTrap, respLen, respPtr, i.mod.Memory().Size())
	}
	// Memory.Read returns a view, not a copy, and memory.grow invalidates it.
	resp := make([]byte, respLen)
	copy(resp, view)

	if _, err := i.invoke(ctx, "dilla_free", i.free, uint64(respPtr), uint64(respLen)); err != nil {
		return nil, err
	}
	return resp, nil
}
