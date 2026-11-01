// Package ds is dilla's MLS delivery service: the five roles and the eleven invariants of
// protocol/02-delivery-service.md, over the wazero-hosted Rust core.
//
// The state rule (R12): SQL is the record. A group's PublicGroup blob is written in the same
// transaction as the handshake row that produced it, and on start the DS imports blobs lazily,
// per group, through public_group_import_state — never from_external, which is O(n²) in
// credential validation.
package ds

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/bot/capability"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/clock"
	"github.com/jonasthim/dilla/internal/gateway"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/mlswasi"
	"github.com/jonasthim/dilla/internal/obs"
	"github.com/jonasthim/dilla/internal/store"
)

// InstanceKeys are the long-lived instance secrets the DS signs and tags with.
type InstanceKeys struct {
	InstanceID          id.ID
	ExternalSenderKeyID id.ID
	FrankingKeyID       id.ID
	ExternalSenderPriv  [32]byte
	FrankingKey         [32]byte
}

// Policy holds every tunable of protocol/02's invariants.
type Policy struct {
	ProposalTTLText time.Duration // 24h
	ProposalTTLCall time.Duration // 30s
	CommitDeadline  time.Duration // the deadline_ms in mls.commit_needed

	// Backoff and BackoffJitter are invariant 7's back-off window — "the other devices back off
	// 300 ms + random(0..300 ms)" (protocol/02-delivery-service.md:257). The instance only ever
	// addresses the elected device, so the window is advertised once, in `hello` (elements 7 and
	// 8), to the devices that are not; a client that never receives mls.commit_needed waits it out
	// before volunteering. The composition root copies both into gateway.Options (deviation B23,
	// closed by task 27a); nothing inside the delivery service reads them.
	Backoff       time.Duration // 300ms
	BackoffJitter time.Duration // 300ms

	WatchdogInterval time.Duration // 2s
	MaxLostRounds    int           // 3

	// ProposalSweepInterval is how often the call sweeper voids expired call-group proposals and
	// re-drives the voided call Removes whose leaf is still present (DEV-49, F9). The one-minute Sweep
	// still covers every group; this one covers call groups only, so a call Remove is void within
	// 30-35 s of issue instead of 30-90 s.
	ProposalSweepInterval time.Duration // 5s

	HandshakeRetention time.Duration // 30d
	MessageRetention   time.Duration // 30d
	HealWindow         time.Duration // 24h
	InactivityRemove   time.Duration // 90d

	// The freeze has exactly TWO backstop timers, and only two. gap-21-ds.md §5.3
	// ("The two backstop timers (and only two)") and its §11 constants table fix
	// both; R26's "all three" mis-cited gap-56, which is about community join
	// gates and carries no freeze parameter at all. There is no third timer and
	// no probe interval: the liveness path is event-driven — the election re-runs
	// on the first device that reaches READY, not on a tick.
	//
	// FreezeWarn = 24h: dilla_ds_group_frozen_seconds crossing it raises the
	// admin warning on the doctor / diagnostics page. The number equals the text
	// proposal TTL, which is 02's own "one TTL has passed with no committer".
	//
	// FreezeMax = 30d: the group is CLOSED and re-created by the channel owner's
	// device — invariant 11's terminal path (02:99-100). The number equals
	// handshake retention (02:94, 02:113); past it an offline device could not
	// catch up anyway and must resync by external commit.
	FreezeWarn time.Duration // 24h  (gap-21-ds §5.3, §11)
	FreezeMax  time.Duration // 30d  (gap-21-ds §5.3, §11; R26)

	MaxCiphertextBytes int // 131072
	MaxAddsPerCommit   int // 256

	// MaxKeyPackagesPerDevice bounds one device's KeyPackage directory: the number of packages
	// `POST /v1/keypackages` will store for a device in one call, refused with E_TOO_LARGE above
	// it.
	//
	// It is NOT a literal invented here. The value is `config.Default().Limits
	// .MaxKeypackagesPerDevice`, which 1a task 4 already fixes at 32 beside the refill threshold
	// of 8, and it is carried on Policy so ONE number governs the refusal here and the
	// `limits.max_keypackages_per_device` element of `GET /v1/instance/limits`. The task brief's
	// NV-B7 proposed 128 for want of a resolution; the shipped config default supersedes it, and
	// nothing else depends on the number — the refusal is a cap, not a protocol constant.
	//
	// internal/config is NOT imported here: it imports nothing of internal/ds, and the reverse
	// would make the delivery service depend on the file format. The composition root copies the
	// configured value across, exactly as it does for MaxCiphertextBytes.
	MaxKeyPackagesPerDevice int // 32

	// MaxKeyPackageLifetime bounds how far in the future a published KeyPackage may expire,
	// measured from the delivery service's clock: RFC 9420 ValSem #32, "applications MUST define a
	// maximum total lifetime". The guest does not enforce one (OpenMLS 0.9.0 checks only
	// `not_before <= now < not_after`), and `not_after` is the CLIENT's, so without this bound
	// `not_after = u64::MAX` is a valid package and, stored as an int64, an `expires` of -1 that
	// `CountKeyPackages` never counts against `MaxKeyPackagesPerDevice`.
	//
	// The default is the 90 days every dilla client builds (`KEY_PACKAGE_LIFETIME_DAYS`) plus one
	// day of clock skew between a client and the instance.
	MaxKeyPackageLifetime time.Duration // 91d
}

// DefaultPolicy is every value interfaces.md §6.2 fixes.
func DefaultPolicy() Policy {
	return Policy{
		ProposalTTLText:       24 * time.Hour,
		ProposalTTLCall:       30 * time.Second,
		CommitDeadline:        2 * time.Second,
		Backoff:               300 * time.Millisecond,
		BackoffJitter:         300 * time.Millisecond,
		WatchdogInterval:      2 * time.Second,
		MaxLostRounds:         3,
		ProposalSweepInterval: 5 * time.Second,
		HandshakeRetention:    30 * 24 * time.Hour,
		MessageRetention:      30 * 24 * time.Hour,
		HealWindow:            24 * time.Hour,
		InactivityRemove:      90 * 24 * time.Hour,
		FreezeWarn:            24 * time.Hour,
		FreezeMax:             30 * 24 * time.Hour,
		MaxCiphertextBytes:    131072,
		MaxAddsPerCommit:      256,
		// config.Default().Limits.MaxKeypackagesPerDevice, kept in step by
		// TestPublishRefusesMoreThanThePolicyCap rather than by a comment.
		MaxKeyPackagesPerDevice: 32,
		MaxKeyPackageLifetime:   91 * 24 * time.Hour,
	}
}

// normalisePolicy fills every unset tunable from DefaultPolicy, FIELD BY FIELD.
//
// The earlier form keyed the whole substitution on one field — `if Policy.MaxCiphertextBytes == 0
// { Policy = DefaultPolicy() }` — so a partially filled Policy, which is exactly the shape a
// config-derived one produces, kept every zero it arrived with. A zero `WatchdogInterval` then
// reached `time.NewTicker` inside Start's goroutine, where the panic it raises is unrecoverable
// and takes the process down at startup; a zero `MaxLostRounds` would remove the elected committer
// on its first overdue round.
//
// Every field of Policy is a positive duration or count, so "unset" is "not positive" and an
// operator can never mean zero.
func normalisePolicy(p Policy) Policy {
	d := DefaultPolicy()
	fill := func(v *time.Duration, def time.Duration) {
		if *v <= 0 {
			*v = def
		}
	}
	fillInt := func(v *int, def int) {
		if *v <= 0 {
			*v = def
		}
	}
	fill(&p.ProposalTTLText, d.ProposalTTLText)
	fill(&p.ProposalTTLCall, d.ProposalTTLCall)
	fill(&p.CommitDeadline, d.CommitDeadline)
	fill(&p.Backoff, d.Backoff)
	fill(&p.BackoffJitter, d.BackoffJitter)
	fill(&p.WatchdogInterval, d.WatchdogInterval)
	fillInt(&p.MaxLostRounds, d.MaxLostRounds)
	fill(&p.ProposalSweepInterval, d.ProposalSweepInterval)
	fill(&p.HandshakeRetention, d.HandshakeRetention)
	fill(&p.MessageRetention, d.MessageRetention)
	fill(&p.HealWindow, d.HealWindow)
	fill(&p.InactivityRemove, d.InactivityRemove)
	fill(&p.FreezeWarn, d.FreezeWarn)
	fill(&p.FreezeMax, d.FreezeMax)
	fill(&p.MaxKeyPackageLifetime, d.MaxKeyPackageLifetime)
	fillInt(&p.MaxCiphertextBytes, d.MaxCiphertextBytes)
	fillInt(&p.MaxAddsPerCommit, d.MaxAddsPerCommit)
	fillInt(&p.MaxKeyPackagesPerDevice, d.MaxKeyPackagesPerDevice)
	return p
}

// Session is the authenticated caller of every mutating entry point, and of the four group-scoped
// reads. It is an ALIAS of auth.Session, not a second declaration: interfaces §6.2 as Plan 2's
// P2-D26 amended it puts the type in `internal/auth`, the leaf package, precisely so `auth`,
// `ds`, `gateway` and `api` can all name it without a cycle. Spelling it `ds.Session` at a call
// site is therefore free.
type Session = auth.Session

type Options struct {
	Store store.Repository
	// BotCapability checks the current community bot-installation state for human group
	// admissions. New defaults it to a repository-backed policy when omitted.
	BotCapability *capability.Policy
	Wasm          *mlswasi.Runtime
	Gateway       *gateway.Gateway
	Clock         clock.Clock
	Log           *slog.Logger
	Metrics       *obs.Metrics
	Keys          InstanceKeys
	Policy        Policy

	// The three injected seams interfaces.md §6.2 names. Each has a Plan-1 default, and each
	// default is the conservative one: the rule runs, against a source that cannot yet answer.
	//
	// Channels is invariant 1's channel-mode source and the registration ACL. nil means a source
	// that refuses every registration; the composition root injects api.StructureChannels, which
	// reads the channels table Plan 2 task 2 created (NV-B5, closed).
	Channels Channels
	// ACL is invariant 4's eligibility source. nil means DenyUnlessMember{Store}, the conservative
	// default; the composition root injects api.ResolverACL, the permission resolver over roles
	// and channel overwrites (Plan 2 task 3, NV-B6 closed).
	ACL ACL
	// PostPolicy decides an end-to-end encrypted upload by its upload class (web3.7 SB1,
	// protocol/02 § Upload class): send_messages and slow mode. nil accepts every upload; the
	// composition root injects api.PostPolicy.
	PostPolicy PostPolicy
	// DeviceLists decodes and verifies a user's signed device list for invariant 4's DSK clause.
	// nil means NewDeviceLists(Store, Wasm), which verifies the stored list in the guest (NV-B8,
	// resolved by task 27a's ABI v3 export device_list_entries).
	//
	// ACL and DeviceLists have no call site before tasks 20-24 — checkAddedMember is the first —
	// but they are declared now, with Options, because Options is the one shape both plans read
	// and tasks 21 and 24 are written against all three fields.
	DeviceLists DeviceLists

	// CallEvictor is told which devices a commit, a heal or a registration took out of a call group,
	// after the group lock is released (G29). The composition root wires the SFU adapter's eviction
	// (dilla-media task 12); nil means nobody is told.
	CallEvictor CallEvictor

	// OnQuarantine is told the device a fork quorum has just quarantined, after the flag is written
	// and outside any group lock, on the fork-report request's goroutine, so it must return at once:
	// the composition root's hook only queues the cut (api.Calls.CutDevice); nil means nobody is told.
	OnQuarantine func(ctx context.Context, device id.ID)
}

// PostPolicy is the host's rule for one application message upload, by its upload class (web3.7
// SB1, protocol/02 § Upload class). Upload calls it under the group lock, after the current-leaf
// check and before the store transaction, and returns its error unchanged.
type PostPolicy interface {
	// Check decides an upload of class (0 a post, 1 anything else) by userID into g. Every upload
	// names its class (web3.8 S1). Production implementations should also satisfy
	// TransactionalPostPolicy so acceptance can decide policy and write the message together.
	Check(ctx context.Context, g store.GroupRow, userID id.ID, class uint8, now int64) error
}

// TransactionalPostPolicy evaluates policy against the same repository transaction
// that holds the group fence and will commit the accepted message.
type TransactionalPostPolicy interface {
	CheckTx(ctx context.Context, tx store.Repository, g store.GroupRow, userID id.ID, class uint8, now int64) error
}

type DS struct {
	opts Options

	// gauge is dilla_ds_proposals_outstanding's count (proposal_gauge.go).
	gauge proposalGauge

	// groupLocks serialises every mutation of one group. One commit per epoch is decided here and
	// in SQL: the lock keeps two commits for the same epoch from both passing validation, and the
	// epoch comparison inside the transaction is what makes it durable.
	groupLocks sync.Map // id.ID -> *sync.Mutex
	// targetLocks serialises the registration of a channel's one text or call group (lockTarget).
	targetLocks sync.Map // targetKey -> *sync.Mutex

	// reconcileAfter is where the sweeper's leaf reconcile resumes (reconcileLeaves).
	reconcileMu    sync.Mutex
	reconcileAfter id.ID

	states *stateCache

	// stale carries one bit out of withGroup's handle lock: a commit whose Merge succeeded and
	// whose transaction then failed has a cached PublicGroup one epoch ahead of SQL, and R12's
	// "SQL is the record" makes that handle unusable. Evicting it from inside withGroup would
	// wait on the very lock withGroup holds, so the group is marked here and evicted by the
	// caller once withGroup has returned.
	staleMu sync.Mutex
	stale   map[id.ID]struct{}

	// supersede carries one ref from `reissue` into the `storeInstanceProposal` it wraps, so that
	// call writes through `ReissueProposal` — which retires the old row and keeps its action_id —
	// rather than through `PutProposal`. Passing it down the call chain instead would put an
	// `oldRef []byte` parameter on both public Propose methods, where it means nothing.
	//
	// It is keyed BY GROUP, not one field. Each re-issue window runs under its own group's lock,
	// so two groups re-issuing at once are both legal — and a single field would let one group's
	// storeInstanceProposal pick up the other's ref and retire a proposal of a group it never
	// touched.
	supersedeMu sync.Mutex
	supersede   map[id.ID][]byte

	// evictions carries the devices a member-set writer removed from a call group out of the group
	// lock to the call evictor (queueEviction, flushEvictions).
	evictMu   sync.Mutex
	evictions map[id.ID][]id.ID

	// callWork is the set of call groups with instance proposals the call sweeper must look at
	// (markCallWork, sweepCallProposals), so its tick costs the call groups that have work rather
	// than every open group of the instance. It is in memory: the sweeper's first tick after a start
	// walks the open groups once to fill it (callWorkSeeded), and callWorkAfter is the round-robin
	// cursor when more groups have work than one tick visits.
	callWorkMu     sync.Mutex
	callWork       map[id.ID]struct{}
	callWorkSeeded bool
	callWorkAfter  id.ID

	// elections is invariant 7's in-flight committer round, one per group. It is in memory on
	// purpose: an election decided while everybody was away is stale by definition, and the
	// instance re-elects on the first device that reaches READY.
	elections elections

	// accepted counts the commits this DS has accepted, for AcceptedCommits (debug.go).
	accepted atomic.Int64

	// instanceCredential and externalSenderPub are the one external sender a text or call group
	// must name (externalSendersProblem): the basic credential's identity [1, "instance",
	// instance_id] and the public half of Keys.ExternalSenderPriv.
	instanceCredential []byte
	externalSenderPub  []byte

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// log is the DS's logger. Every call site uses it rather than d.opts.Log directly, so a nil
// logger is one check here instead of one at each of the places that log.
func (d *DS) log() *slog.Logger {
	if d.opts.Log == nil {
		return slog.Default()
	}
	return d.opts.Log
}

func New(o Options) (*DS, error) {
	if o.Clock == nil {
		o.Clock = clock.System()
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.BotCapability == nil {
		o.BotCapability = capability.New(o.Store)
	}
	o.Policy = normalisePolicy(o.Policy)
	if o.Channels == nil {
		o.Channels = closedChannels{}
	}
	if o.ACL == nil {
		o.ACL = DenyUnlessMember{Store: o.Store}
	}
	if o.DeviceLists == nil {
		o.DeviceLists = NewDeviceLists(o.Store, o.Wasm)
	}
	// Invariant 1's external sender (DS-MEMBERSHIP-01): the one entry a text or call group's
	// external_senders must carry, derived once from the keys the delivery service signs with.
	credential, err := cborx.Marshal([]any{uint64(1), "instance", o.Keys.InstanceID[:]})
	if err != nil {
		return nil, fmt.Errorf("ds: encode the instance's external-sender credential: %w", err)
	}
	ds := &DS{
		opts:               o,
		states:             newStateCache(o.Wasm),
		stop:               make(chan struct{}),
		instanceCredential: credential,
		// An Ed25519 private key is the seed followed by the public key (crypto/ed25519), as
		// internal/dillad derives the external_sender_pub it publishes.
		externalSenderPub: []byte(ed25519.NewKeyFromSeed(o.Keys.ExternalSenderPriv[:])[ed25519.SeedSize:]),
	}
	ds.elections.m = map[id.ID]*election{}
	return ds, nil
}

// Start first reseeds the gateway's group registry from SQL (SeedGateway): the fan-out list and
// leaf map are in memory, and a restarted instance would otherwise deliver nothing in a group
// and elect nobody until that group's next commit (C12, gap G5). A store error there is returned
// and nothing is started.
//
// Start runs the delivery service's background work: invariant 7's 2-second election watchdog and
// the one-minute sweeper that voids proposals past invariant 6's TTL. Task 26 extends the sweeper
// with retention pruning, on the same tick. dilla-media task 9 adds the call sweeper, which runs the
// call-group half of the proposal sweep every Policy.ProposalSweepInterval on the DS's clock.
//
// All three loops end on Shutdown, which closes d.stop and waits on d.wg. A test that drives the
// watchdog deterministically calls RunWatchdogOnce against a clock.Fake instead of starting it —
// these tickers are wall-clock, because a fake clock in production would be a stopped one.
func (d *DS) Start(ctx context.Context) error {
	if _, err := d.SeedGateway(ctx); err != nil {
		return err
	}
	// The outstanding-proposals gauge starts from SQL, not from zero: the sweep's voids of rows a
	// previous process issued would otherwise drive it negative. A diagnostic never fails a start;
	// the first Sweep re-seeds it.
	if err := d.seedProposalGauge(ctx); err != nil {
		d.log().Error("seeding the outstanding-proposals gauge failed; the first sweep retries", "err", err)
	}
	d.wg.Add(3)
	go func() {
		defer d.wg.Done()
		d.runWatchdog(ctx)
	}()
	go func() {
		defer d.wg.Done()
		d.runSweeper(ctx)
	}()
	go func() {
		defer d.wg.Done()
		d.runCallSweeper(ctx)
	}()
	return nil
}

func (d *DS) Shutdown(ctx context.Context) error {
	d.stopOnce.Do(func() { close(d.stop) })
	done := make(chan struct{})
	go func() { d.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return d.states.closeAll(ctx)
}

// lock serialises one group's mutations.
func (d *DS) lock(groupID id.ID) func() {
	v, _ := d.groupLocks.LoadOrStore(groupID, &sync.Mutex{})
	// groupLocks only ever holds *sync.Mutex values stored by the line above.
	mu, _ := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (d *DS) now() int64 { return d.opts.Clock.Now().Unix() }

// groupKindText and groupKindCall are GroupRow.Kind for a text and a call group (protocol/01
// § Group kinds).
const (
	groupKindText uint8 = 0
	groupKindCall uint8 = 1
)

// CallEvictor receives the devices a member-set writer removed from a call group.
type CallEvictor func(ctx context.Context, groupID id.ID, removed []id.ID)

// queueEviction records devices removed from a call group inside a writer's transaction.
func (d *DS) queueEviction(groupID id.ID, removed []id.ID) {
	if len(removed) == 0 || d.opts.CallEvictor == nil {
		return
	}
	d.evictMu.Lock()
	if d.evictions == nil {
		d.evictions = map[id.ID][]id.ID{}
	}
	d.evictions[groupID] = append(d.evictions[groupID], removed...)
	d.evictMu.Unlock()
}

// flushEvictions hands the queued devices to the call evictor. Every member-set entry point defers
// it before taking the group lock, so it runs after the unlock: the evictor makes a loopback twirp
// call per device, and the group must not wait on it.
func (d *DS) flushEvictions(ctx context.Context, groupID id.ID) {
	d.evictMu.Lock()
	removed := d.evictions[groupID]
	delete(d.evictions, groupID)
	d.evictMu.Unlock()
	if len(removed) == 0 || d.opts.CallEvictor == nil {
		return
	}
	// The change is durable: a client hanging up must not keep a removed device in the room.
	d.opts.CallEvictor(context.WithoutCancel(ctx), groupID, removed)
}
