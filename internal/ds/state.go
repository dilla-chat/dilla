package ds

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/fxamacker/cbor/v2"

	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/mlswasi"
	"github.com/jonasthim/dilla/internal/store"
)

// stateCache holds live PublicGroup handles, each inside the wasm instance that created it: a
// handle is valid only there (mlswasi's package contract), so the instance travels with it.
//
// Three rules make that safe, and all three are load-bearing:
//
//  1. The cache holds at most `capacity` groups, which is `PoolSize - 1`. `Runtime.Acquire`
//     blocks on a buffered channel of `PoolSize` instances (`internal/mlswasi/pool.go:34-46`), so
//     an unbounded cache stops serving the moment the number of live groups reaches the pool size
//     — on a default `PoolSize = GOMAXPROCS` that is a handful of groups. The least recently used
//     group is closed and its instance released to make room; the next request for it re-imports
//     its blob, which is exactly what `withGroup` already does on a cold cache.
//  2. A group's slot is RESERVED before the cache mutex is released: `acquire` inserts an entry
//     whose `ready` channel is still open and imports into it afterwards. Making room and then
//     acquiring an instance is not one step otherwise — N goroutines first-touching N distinct
//     groups would all pass one room check while the cache sat a slot below capacity, all call
//     `Runtime.Acquire`, and the winners would keep their instances in the cache while the losers
//     blocked in Acquire, which returns only an instance or a cancelled context. Nothing else in
//     the DS releases an instance, so that burst degrades to ctx-deadline failures and, for a
//     caller passing context.Background(), to a hang. The reservation bounds the instances the
//     cache holds AND is importing at `capacity`, and it makes two first touches of one group
//     import it once.
//  3. `Runtime.Acquire` is NEVER called while `c.mu` is held. Acquire blocks, and blocking under
//     the cache mutex freezes every other group, `evict` included.
//
// `liveGroup` has its own mutex because `mlswasi.Instance` is not safe for concurrent use: two
// requests for one group must not call into the same module at once. Every path that touches a
// handle goes through `withGroup`, which holds it for the duration of the call sequence.
type stateCache struct {
	wasm     *mlswasi.Runtime
	capacity int

	mu     sync.Mutex
	groups map[id.ID]*liveGroup
	lru    []id.ID // least recently used first
}

// liveGroup is one group's handle and the instance it lives in. An entry whose `ready` channel is
// still open is a RESERVATION: it already owns its slot in the cache, its import is in flight, and
// `inst` and `group` are nil until `ready` closes. A reservation whose import fails takes itself
// out of the map before closing `ready`, so the next caller reserves a fresh slot rather than
// finding a half-built entry.
type liveGroup struct {
	mu    sync.Mutex
	inst  *mlswasi.Instance
	group *mlswasi.PublicGroup

	ready chan struct{}
	err   error // the import's own failure; read only after ready is closed
}

// filled reports whether the import has landed.
func (l *liveGroup) filled() bool {
	select {
	case <-l.ready:
		return true
	default:
		return false
	}
}

// wait blocks until the import lands or the caller's context is done. The import's own error is
// `err`, read separately: a caller that is only waiting for a slot to free up does not inherit it.
func (l *liveGroup) wait(ctx context.Context) error {
	select {
	case <-l.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// errNoSlot cannot happen while `groups` and `lru` agree — it is the assertion that they do.
var errNoSlot = errors.New("ds: the state cache has no slot to give")

func newStateCache(w *mlswasi.Runtime) *stateCache {
	capacity := 1
	if w != nil {
		capacity = w.PoolSize() - 1
	}
	if capacity < 1 {
		capacity = 1
	}
	return &stateCache{wasm: w, capacity: capacity, groups: map[id.ID]*liveGroup{}}
}

// withGroup runs fn against the group's live PublicGroup, importing the stored blob on first use.
// The import is lazy and per group: a restart with 10,000 groups costs nothing until a request
// arrives for one of them.
func (d *DS) withGroup(ctx context.Context, groupID id.ID, fn func(*mlswasi.PublicGroup) error) error {
	live, err := d.states.acquire(ctx, groupID, func() ([]byte, error) {
		row, err := d.opts.Store.GetGroup(ctx, groupID)
		if err != nil {
			return nil, err
		}
		if len(row.PublicGroupState) == 0 {
			return nil, fmt.Errorf("group %s has no state blob", groupID)
		}
		return row.PublicGroupState, nil
	})
	if err != nil {
		return err
	}
	// mlswasi.Instance is not safe for concurrent use (internal/mlswasi/pool.go:21-23) and the
	// cache hands the same instance to every caller of a group, so the handle lock is held for
	// the whole call sequence. Two concurrent GET /tree on one group — the ordinary joiner path —
	// would otherwise call api.Function.Call on one module at once and corrupt the guest
	// allocator, which -race cannot see.
	live.mu.Lock()
	defer live.mu.Unlock()
	return fn(live.group)
}

func (c *stateCache) acquire(ctx context.Context, groupID id.ID, load func() ([]byte, error)) (*liveGroup, error) {
	if live, ok := c.lookup(groupID); ok {
		return waitFor(ctx, live)
	}
	// The blob is read BEFORE a slot is reserved: a request for a group that has none must not
	// evict a live one on its way to failing.
	blob, err := load()
	if err != nil {
		return nil, err
	}
	for {
		c.mu.Lock()
		if live, ok := c.groups[groupID]; ok {
			c.touchLocked(groupID)
			c.mu.Unlock()
			return waitFor(ctx, live)
		}
		if len(c.groups) < c.capacity {
			live := &liveGroup{ready: make(chan struct{})}
			c.groups[groupID] = live
			c.touchLocked(groupID)
			c.mu.Unlock()
			// The reservation is this goroutine's to fill, and fill always closes ready — the
			// other callers of this group are already waiting on it.
			c.fill(ctx, groupID, live, blob)
			if live.err != nil {
				return nil, live.err
			}
			return live, nil
		}
		c.mu.Unlock()
		if err := c.makeRoom(ctx); err != nil {
			return nil, err
		}
	}
}

// waitFor is the shared tail of every cache hit: wait for the import to land, then answer with
// its result.
func waitFor(ctx context.Context, live *liveGroup) (*liveGroup, error) {
	if err := live.wait(ctx); err != nil {
		return nil, err
	}
	if live.err != nil {
		return nil, live.err
	}
	return live, nil
}

// lookup is the cache's fast path, and the only one that does not make room.
func (c *stateCache) lookup(groupID id.ID) (*liveGroup, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	live, ok := c.groups[groupID]
	if ok {
		c.touchLocked(groupID)
	}
	return live, ok
}

// fill imports the blob into a reservation this goroutine owns. It closes `ready` on every path.
func (c *stateCache) fill(ctx context.Context, groupID id.ID, live *liveGroup, blob []byte) {
	// Acquire OUTSIDE c.mu: it blocks on the pool's channel, and holding the cache mutex across
	// it would freeze every other group behind one slow import.
	inst, err := c.wasm.Acquire(ctx)
	if err == nil {
		var group *mlswasi.PublicGroup
		group, err = inst.PublicGroupImport(ctx, blob, groupID[:])
		if err == nil {
			live.inst, live.group = inst, group
		} else {
			inst.Release()
		}
	}
	if err != nil {
		// Give the slot back before the waiters wake: an entry that never imported must not hold
		// a slot, and the next caller has to be able to reserve a fresh one.
		c.forget(groupID, live)
		live.err = err
	}
	close(live.ready)
}

// makeRoom frees one slot, or waits for an in-flight import to land so the caller can look again.
// It is called WITHOUT c.mu, and the caller re-checks the cache afterwards rather than assuming
// the room it made is still there.
func (c *stateCache) makeRoom(ctx context.Context) error {
	c.mu.Lock()
	victim, pending, ok := c.victimLocked()
	c.mu.Unlock()
	if ok {
		return c.evict(ctx, victim)
	}
	if pending == nil {
		return errNoSlot
	}
	// Every slot is an import that has not landed yet. Waiting for one is the whole point: it is
	// what keeps the number of outstanding instances at capacity instead of at the number of
	// callers.
	return pending.wait(ctx)
}

// victimLocked picks the least recently used FILLED entry. A reservation is never a victim — it
// owns no instance yet, and evicting it would strand the one its importer is about to store — so
// when every entry is a reservation it hands one back to wait on instead.
func (c *stateCache) victimLocked() (id.ID, *liveGroup, bool) {
	var pending *liveGroup
	for _, gid := range c.lru {
		live, ok := c.groups[gid]
		if !ok {
			continue
		}
		if live.filled() {
			return gid, nil, true
		}
		if pending == nil {
			pending = live
		}
	}
	return id.ID{}, pending, false
}

func (c *stateCache) touchLocked(groupID id.ID) {
	for i, g := range c.lru {
		if g == groupID {
			c.lru = append(append(c.lru[:i:i], c.lru[i+1:]...), groupID)
			return
		}
	}
	c.lru = append(c.lru, groupID)
}

func (c *stateCache) dropLocked(groupID id.ID) {
	for i, g := range c.lru {
		if g == groupID {
			c.lru = append(c.lru[:i:i], c.lru[i+1:]...)
			return
		}
	}
}

// forget removes a reservation whose import failed, and only that reservation: a slot handed to
// someone else in the meantime is not this goroutine's to drop.
func (c *stateCache) forget(groupID id.ID, live *liveGroup) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cur, ok := c.groups[groupID]; ok && cur == live {
		delete(c.groups, groupID)
		c.dropLocked(groupID)
	}
}

// put installs a freshly created handle, which registration and heal produce. It makes room the
// same way acquire does, so a burst of registrations cannot exhaust the pool either, and it
// evicts any handle already under that id rather than dropping it on the floor: an overwritten
// handle would hold its instance out of the pool for the life of the process.
//
// The instance it installs was acquired by its caller, outside this cache's accounting. That is
// the slot capacity leaves free: `capacity` is `PoolSize - 1`, so one caller can hold an instance
// of its own while the cache is full.
func (c *stateCache) put(ctx context.Context, groupID id.ID, inst *mlswasi.Instance, group *mlswasi.PublicGroup) error {
	if err := c.evict(ctx, groupID); err != nil {
		return err
	}
	for {
		c.mu.Lock()
		if _, ok := c.groups[groupID]; ok {
			c.mu.Unlock()
			// Someone imported the same group between the evict and here. Drop theirs: this
			// handle is the newer one, and the caller has no other place to put it.
			if err := c.evict(ctx, groupID); err != nil {
				return err
			}
			continue
		}
		if len(c.groups) < c.capacity {
			ready := make(chan struct{})
			close(ready)
			c.groups[groupID] = &liveGroup{inst: inst, group: group, ready: ready}
			c.touchLocked(groupID)
			c.mu.Unlock()
			return nil
		}
		c.mu.Unlock()
		if err := c.makeRoom(ctx); err != nil {
			return err
		}
	}
}

// evict drops a handle, which restore, close and the LRU do; the next request re-imports.
func (c *stateCache) evict(ctx context.Context, groupID id.ID) error {
	for {
		c.mu.Lock()
		live, ok := c.groups[groupID]
		if !ok {
			c.mu.Unlock()
			return nil
		}
		if !live.filled() {
			c.mu.Unlock()
			// An import owns this entry until it lands. Evicting it now would strand the instance
			// the importer is about to store in it.
			if err := live.wait(ctx); err != nil {
				return err
			}
			continue
		}
		delete(c.groups, groupID)
		c.dropLocked(groupID)
		c.mu.Unlock()
		// The handle may be mid-call; waiting for it is what makes eviction safe.
		live.mu.Lock()
		err := live.group.Close(ctx)
		live.inst.Release()
		live.mu.Unlock()
		return err
	}
}

func (c *stateCache) closeAll(ctx context.Context) error {
	var firstErr error
	for {
		c.mu.Lock()
		var pick id.ID
		found := false
		for gid := range c.groups {
			pick, found = gid, true
			break
		}
		if !found {
			c.lru = nil
			c.mu.Unlock()
			return firstErr
		}
		c.mu.Unlock()
		// evict waits for an import in flight and releases what it finds, so shutdown never
		// closes a handle out from under the goroutine that is still building it.
		if err := c.evict(ctx, pick); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			if ctx.Err() != nil {
				return firstErr
			}
		}
	}
}

// persistState writes the group's state blob and its derived columns. Every caller runs it inside
// the same transaction as the handshake row it belongs to (R12).
func persistState(ctx context.Context, repo store.Repository, groupID id.ID, g *mlswasi.PublicGroup, groupInfo []byte) error {
	blob, err := g.ExportState(ctx)
	if err != nil {
		return err
	}
	state, err := g.State(ctx)
	if err != nil {
		return err
	}
	return repo.PutGroupState(ctx, groupID, state.Epoch, blob, groupInfo, state.TreeHash)
}

// decodeCredentialIdentity reads dilla's CredentialIdentity out of a leaf credential.
//
// The layout is `core/dilla-core/src/identity/credential.rs:28-42`, and it is a TEN-element
// fixed-position array, not three:
//
//	[v, umk_pub(32), user_id(16), device_id(16), kind, tier, signer_tier,
//	 ssk_pub(32), sig_umk_ssk(64), sig_ssk_dev(64)]
//
// so user_id is at index 2 and device_id at index 3 — reading them at 1 and 2 would take
// `umk_pub` for the device id and `user_id` for the device id, and the length check alone rejects
// every real credential. `protocol/vectors/identity.json`'s `credential_identity.cbor` is the
// committed example, and TestCredentialIdentityDecodesTheCommittedVector decodes it.
//
// The DS never invents a credential; it only reads what the core validated, and it deliberately
// does not verify the two signatures here — `validate_key_package` and the guest's own leaf
// validation already did.
func decodeCredentialIdentity(b []byte) (deviceID, userID id.ID, err error) {
	var elems []cbor.RawMessage
	if err := cborx.Unmarshal(b, &elems); err != nil {
		return id.ID{}, id.ID{}, err
	}
	if len(elems) != 10 {
		return id.ID{}, id.ID{}, fmt.Errorf("credential identity has %d elements, want 10", len(elems))
	}
	if err := cborx.Unmarshal(elems[2], &userID); err != nil {
		return id.ID{}, id.ID{}, fmt.Errorf("credential identity user_id: %w", err)
	}
	if err := cborx.Unmarshal(elems[3], &deviceID); err != nil {
		return id.ID{}, id.ID{}, fmt.Errorf("credential identity device_id: %w", err)
	}
	return deviceID, userID, nil
}

// decodeCredentialKind reads `kind` (index 4 of the same ten-element array): 0 is a human account,
// 1 a Bot. Anything else — a wrong shape, a non-integer, a value no account kind has — is an
// error, so every caller fails closed on a credential it cannot read (Task S1).
func decodeCredentialKind(b []byte) (uint8, error) {
	var elems []cbor.RawMessage
	if err := cborx.Unmarshal(b, &elems); err != nil {
		return 0, err
	}
	if len(elems) != 10 {
		return 0, fmt.Errorf("credential identity has %d elements, want 10", len(elems))
	}
	var kind uint64
	if err := cborx.Unmarshal(elems[4], &kind); err != nil {
		return 0, fmt.Errorf("credential identity kind: %w", err)
	}
	if kind > 1 {
		return 0, fmt.Errorf("credential identity kind %d is no account kind", kind)
	}
	return uint8(kind), nil
}

// Binding is `dilla_binding`, group context extension 0xF001. The layout is
// `core/dilla-core/src/mls/binding.rs:70-82` and `protocol/01-groups.md`: a deterministic-CBOR
// EIGHT-element fixed-position array. `protocol/00` states that every dilla structure is a CBOR
// array, never a map, so there are no field names on the wire and no JSON anywhere.
//
//	[v, instance_id(16), community_id(16|null), target_id(16), kind, policy_version,
//	 e2ee_version, media_version]
//
// There is no `call_id` member: R9 puts the call id in a companion column.
type Binding struct {
	_             struct{} `cbor:",toarray"`
	V             uint64
	InstanceID    id.ID
	CommunityID   *id.ID
	TargetID      id.ID
	Kind          uint8
	PolicyVersion uint64
	E2EEVersion   uint64
	MediaVersion  uint64
}

// decodeBinding reads the bytes `public_group_state` handed over as a bstr wrapping the array
// (`core/dilla-core-wasi/src/exports.rs:249-263`, deviation A2-13).
func decodeBinding(b []byte) (Binding, error) {
	var out Binding
	if err := cborx.Unmarshal(b, &out); err != nil {
		return Binding{}, err
	}
	return out, nil
}

// callIDOf is the companion-column value of R9: for a call group the binding's target IS the call,
// and for every other kind there is no call.
func callIDOf(b Binding) *id.ID {
	const kindCall = 1
	if b.Kind != kindCall {
		return nil
	}
	target := b.TargetID
	return &target
}
