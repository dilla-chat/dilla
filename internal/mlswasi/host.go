// Package mlswasi hosts dilla-core-wasi, the wasm32-wasip1 build of the dilla
// Rust core, inside dillad using wazero.
//
// Three configuration choices are load-bearing and must never be dropped:
//
//   - WithSysWalltime() -- wazero's default wall clock is a fake that advances
//     1ms per reading from near the Unix epoch. OpenMLS validates every leaf's
//     KeyPackage lifetime, so the default clock makes every from_external fail
//     in microseconds, which looks like a very fast success in a benchmark.
//     TestClockAndRandomnessConfigurationIsLoadBearing runs the same module
//     under wazero's defaults and requires it to fail.
//   - WithRandSource(crypto/rand.Reader) -- wazero's default random source is
//     deterministic, and every RNG in the OpenMLS graph resolves to WASI
//     random_get on this target.
//   - The export assertion below. wazero instantiates any well-formed module,
//     and a build for the wrong target would only fail later, inside a call.
//
// The start-function story is the one place where the measured artifact
// contradicts the notes this package was planned from. gap-19 item 8 and
// facts-wazero §3.1 say a cdylib for wasm32-wasip1 is linked with
// crt1-reactor.o and therefore exports _initialize, so the host should name it
// with WithStartFunctions and assert the export. Measured on rustc 1.98.1 that
// is false: rustc passes --no-entry and links no crt object, so the module's
// export section holds the 17 dilla functions and `memory` and nothing else
// (core/dilla-core-wasi/src/lib.rs records the same finding and the way to
// reproduce it). Nothing is lost -- _initialize's only job is
// __wasm_call_ctors and this graph registers no constructors -- but it means
// RequiredExports must not contain _initialize, or New would reject the
// correct artifact. startFunctions still names both spellings: wazero skips a
// start function that does not exist, so naming them costs nothing today and
// initialises a future build that does link crt1-reactor.o.
//
// api.Function.Call is not goroutine-safe and api.Memory.Read returns a view
// that memory.grow invalidates, so the package hands out one instance per
// caller from a pool and copies every response out of linear memory before it
// returns.
package mlswasi

import (
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"

	"github.com/jonasthim/dilla/internal/prioritygate"
)

// ABIVersion is the version every request carries as its first element and every module must
// accept. **2** since 2026-09-24: public_group_process grew to eight elements and
// validate_key_package to six (interfaces.md §3, R27). **3** since 2026-09-29 (task 27a, Ruling
// C): public_group_process grew a ninth element, new_leaf, and the module grew
// device_list_entries and public_group_staged_group_info_validate (deviations B32, B33). There is
// no compatibility shim — this host is the guest's only consumer and CI builds both from one
// commit. **4** since 2026-10-05 (hardening C): validate_key_package grew a seventh element and
// each public_group_process applied item a sixth, the leaf's signature key, which the delivery
// service binds to the device's registered key. **5** since 2026-10-05 (hardening G):
// public_group_state grew a seventh element, leaf_count, every occupied leaf of the tree, which
// registration and heal's reseed compare with the member list before adopting a tree. **6** since
// 2026-10-06 (DS-MEMBERSHIP-01): public_group_state grew an eighth element, external_senders, the
// group context's [credential_type, credential, signature_key] entries, which registration and
// heal's reseed compare with the instance's own key and credential before adopting a tree. A
// module of another version answers every request E_ABI_VERSION, and a v5 response has seven
// elements where State expects eight: either way the request fails, nothing is adopted. **7** since
// 2026-10-21 (DILLA-BOT-04): the module adds verify_credential, backed by the core's strict
// Ed25519 verifier. **8** since 2026-10-21 (DILLA-BOT-04 Task5): validate_key_package appends the
// validated leaf credential bytes so managed Bot admission can compare the exact registered value.
// **9** adds bounded authenticated roster extraction. **10** adds strict Welcome recipient-ref inspection.
const ABIVersion uint64 = 10

// RequiredExports is the 26-export ABI v10 surface. New refuses any module that does not carry all
// of them. _initialize is deliberately absent: see the package comment.
var RequiredExports = []string{
	"dilla_alloc",
	"dilla_free",
	"dilla_abi",
	"vectors_check",
	"public_group_create",
	"public_group_import_state",
	"public_group_export_state",
	"public_group_close",
	"public_group_process",
	"public_group_merge",
	"public_group_tree",
	"public_group_state",
	"public_group_proposal_put",
	"public_group_proposal_list",
	"public_group_staged_discard",
	"public_group_group_info_validate",
	"public_group_staged_group_info_validate",
	"public_group_proposal_inspect",
	"private_message_aad",
	"validate_key_package",
	"external_propose_add",
	"external_propose_remove",
	"device_list_entries",
	"verify_credential",
	"public_group_roster_extract",
	"welcome_key_package_refs",
}

// startFunctions is what every instance, and the control arm of the clock test,
// passes to WithStartFunctions. Naming a function the module does not export is
// a no-op in wazero, so this is one list for both the current --no-entry
// artifact (neither name exists) and a future reactor build (_initialize does).
// It deliberately does not include _start: a cdylib has no main.
var startFunctions = []string{"_initialize"}

// ErrClosed is returned by Acquire or a retained instance's Call after Close.
var ErrClosed = errors.New("mlswasi: runtime is closed")

// ErrDepleted is returned by Acquire when replacing poisoned instances has
// failed and no usable instances remain. The runtime must be recreated.
var ErrDepleted = errors.New("mlswasi: runtime has no usable instances")

// Options configures a Runtime.
type Options struct {
	// CacheDir persists wazero's ahead-of-time compilation results between
	// process restarts. "" disables the cache.
	CacheDir string
	// PoolSize is the number of module instances. Zero means GOMAXPROCS.
	PoolSize int
	// Interpreter selects wazero's interpreter instead of its compiler. Only
	// the benchmark's comparison arm should set it.
	Interpreter bool
	// Now, when set, is the wall clock the guest sees. wazero calls it on every clock_time_get,
	// so advancing the clock after instantiation is visible to the guest without rebuilding the
	// pool — which is what makes OpenMLS's KeyPackage lifetime check testable (gap-33 §4.3).
	// Nil keeps WithSysWalltime(), the production setting.
	Now func() time.Time
	// OnCall, when set, is called with the name of every guest export the host invokes, before
	// the call. It is observation only — nothing about the call changes — and it exists because
	// "the delivery service imports a group's state blob lazily, and never rebuilds it from the
	// tree" is a statement about WHICH guest export ran, which nothing else on this API reports
	// (task 19). It runs on the caller's goroutine, so an implementation must be cheap and safe
	// for concurrent use.
	OnCall func(export string)
	// Priority selects admission order when all instances are busy. Higher values
	// run first; nil preserves FIFO order. Running guest calls are never preempted.
	Priority func(context.Context) int
}

// Runtime owns one wazero runtime, one compiled module and a bounded pool of
// instances.
type Runtime struct {
	rt       wazero.Runtime
	compiled wazero.CompiledModule
	pool     chan *Instance
	poolSize int
	now      func() time.Time
	onCall   func(export string)
	priority func(context.Context) int
	gate     *prioritygate.Gate
	done     chan struct{}
	// execution covers guest calls and module replacement, not retained handles.
	// Shutdown waits for those operations before destroying WASI host state.
	execution sync.RWMutex

	mu     sync.Mutex
	closed bool
}

// New compiles wasmBinary once (ahead of time, at startup, as wazero's own
// guidance requires) and fills the instance pool.
func New(ctx context.Context, wasmBinary []byte, opts Options) (*Runtime, error) {
	var rc wazero.RuntimeConfig
	if opts.Interpreter {
		rc = wazero.NewRuntimeConfigInterpreter()
	} else {
		rc = wazero.NewRuntimeConfigCompiler()
	}
	if opts.CacheDir != "" {
		cache, err := wazero.NewCompilationCacheWithDir(opts.CacheDir)
		if err != nil {
			return nil, fmt.Errorf("mlswasi: compilation cache %q: %w", opts.CacheDir, err)
		}
		rc = rc.WithCompilationCache(cache)
	}

	rt := wazero.NewRuntimeWithConfig(ctx, rc)
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("mlswasi: wasi_snapshot_preview1: %w", err)
	}
	compiled, err := rt.CompileModule(ctx, wasmBinary)
	if err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("mlswasi: compile: %w", err)
	}
	exported := compiled.ExportedFunctions()
	for _, name := range RequiredExports {
		if _, ok := exported[name]; !ok {
			_ = rt.Close(ctx)
			return nil, fmt.Errorf("mlswasi: missing export %q; is this a wasm32-wasip1 build of dilla-core-wasi?", name)
		}
	}
	if len(compiled.ExportedMemories()) == 0 {
		_ = rt.Close(ctx)
		return nil, errors.New("mlswasi: module exports no memory")
	}

	size := opts.PoolSize
	if size <= 0 {
		size = runtime.GOMAXPROCS(0)
	}
	r := &Runtime{rt: rt, compiled: compiled, pool: make(chan *Instance, size), poolSize: size, now: opts.Now, onCall: opts.OnCall,
		priority: opts.Priority, gate: prioritygate.New(size), done: make(chan struct{})}
	for range size {
		inst, err := r.newInstance(ctx)
		if err != nil {
			_ = rt.Close(ctx)
			return nil, err
		}
		r.pool <- inst
	}
	return r, nil
}

// newInstance instantiates the compiled module. Start functions run once per
// InstantiateModule, so a reactor entry point, if the artifact ever carries
// one, runs exactly once here and is never called again by hand.
func (r *Runtime) newInstance(ctx context.Context) (*Instance, error) {
	cfg := wazero.NewModuleConfig().
		WithName(""). // anonymous: many instances may share one runtime
		WithStartFunctions(startFunctions...).
		WithRandSource(crand.Reader).
		WithSysNanotime()
	if r.now == nil {
		cfg = cfg.WithSysWalltime()
	} else {
		// Deviation from the brief: sys.Walltime in wazero v1.12.0 is
		// `func() (sec int64, nsec int32)` (sys/clock.go:13), with no context parameter, so the
		// closure the brief wrote does not satisfy it.
		cfg = cfg.WithWalltime(func() (sec int64, nsec int32) {
			t := r.now()
			return t.Unix(), int32(t.Nanosecond())
		}, sys.ClockResolution(time.Microsecond.Nanoseconds()))
	}
	mod, err := r.rt.InstantiateModule(ctx, r.compiled, cfg)
	if err != nil {
		return nil, fmt.Errorf("mlswasi: instantiate: %w", err)
	}
	return &Instance{
		rt:    r,
		mod:   mod,
		alloc: mod.ExportedFunction("dilla_alloc"),
		free:  mod.ExportedFunction("dilla_free"),
	}, nil
}

// Close rejects queued acquisitions, waits for active calls, and closes every
// module, including instances retained by DS group caches. Retained handles must
// still be Released by their owners, but cannot delay runtime shutdown.
func (r *Runtime) Close(ctx context.Context) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	close(r.done)
	r.mu.Unlock()
	r.gate.Close()
	r.execution.Lock()
	defer r.execution.Unlock()

	for range r.poolSize {
		select {
		case inst := <-r.pool:
			_ = inst.mod.Close(ctx)
		default:
		}
	}
	return r.rt.Close(ctx)
}
