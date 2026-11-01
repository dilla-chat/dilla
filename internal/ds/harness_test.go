package ds_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	_ "modernc.org/sqlite"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/bot/capability"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/clock"
	"github.com/jonasthim/dilla/internal/ds"
	"github.com/jonasthim/dilla/internal/ds/dstest"
	"github.com/jonasthim/dilla/internal/gateway"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/mlswasi"
	"github.com/jonasthim/dilla/internal/obs"
	"github.com/jonasthim/dilla/internal/store"
	"github.com/jonasthim/dilla/internal/store/sqlite"
	"github.com/jonasthim/dilla/internal/store/sqlite/sqlitetest"
)

// dsHarness is a whole delivery service over a real SQLite file, a real wazero runtime and a real
// gateway, driven by a fake clock. Nothing under test is mocked: the wasm guest, the SQL and the
// fan-out are the things the DS invariants are defined over, and a double for any of them would
// test the double.
//
// Later tasks add the helpers their own tests name; each lists
// `Modify: internal/ds/harness_test.go` in its Files and shows the helper body beside the test
// that first needs it.
type dsHarness struct {
	t       *testing.T
	path    string
	ds      *ds.DS
	gw      *gateway.Gateway
	clk     *clock.Fake
	wasm    *mlswasi.Runtime
	repo    *failingRepo
	metrics *obs.Metrics

	channels *fakeChannels
	// acl is invariant 4's eligibility source: ds.DenyUnlessMember, plus the users a test names
	// eligible for an external join (join_test.go).
	acl *fakeACL
	// postPolicy is the upload class seam (web3.7 SB1): it records every question Upload asks
	// and accepts unless a test sets its answer (message_test.go).
	postPolicy *recordingPostPolicy
	// auth is the gateway's token resolver. The gateway holds an `Authenticator` interface, not
	// *auth.Sessions (deviation B9), so the delivery service's tests satisfy that seam directly
	// rather than standing a whole account stack up to put one device online.
	auth *fakeAuth

	// calls counts guest exports by name, through mlswasi.Options.OnCall. It is what makes
	// "the state blob is imported lazily, and the tree is never rebuilt" an assertion rather
	// than a claim.
	callsMu sync.Mutex
	calls   map[string]int64

	// sessions, conns and hello belong to task 22's election harness, whose helpers are in
	// election_harness_test.go: the session of every member device the fixture exposed, the live
	// gateway connection of every device a test put online, and the hello frame the last of them
	// was greeted with. They are fields here because `dsHarness` is one type; a test that never
	// elects a committer leaves all three nil.
	sessions map[id.ID]auth.Session
	conns    map[id.ID]*deviceConn
	hello    recordedFrame

	// keyPackagesUsed is how many devices of the committed KeyPackage set
	// (testkit/fixtures/key-packages) this harness has registered; keypackage_set_test.go hands
	// each new device the next entry, so no two devices of one test share a package.
	keyPackagesUsed int
}

func newDSHarness(t *testing.T) *dsHarness {
	t.Helper()
	return newDSHarnessWithFrameBudget(t, 0)
}

func newDSHarnessWithFrameBudget(t *testing.T, maxFrameBytes uint64) *dsHarness {
	t.Helper()
	ctx := context.Background()
	// The fake clock starts at the wall clock's current second rather than a fixed epoch. The
	// guest sees this clock (mlswasi.Options.Now) and OpenMLS validates every leaf's KeyPackage
	// lifetime, so a fixed timestamp would put the whole 1,500-leaf fixture outside its validity
	// window and every from_external would fail with LeafNodeValidation(Lifetime(...)). Nothing
	// here depends on the absolute value — a test that needs time to pass calls clk.Advance — so
	// the tests stay deterministic in everything they actually assert.
	clk := clock.NewFake(time.Unix(time.Now().Unix(), 0))

	// A file, not :memory:: restartDS reopens the same database, and an in-memory database
	// vanishes with its first connection.
	path := filepath.Join(t.TempDir(), "dilla.db")
	base := openMigratedSQLite(t, path)
	repo := &failingRepo{Repository: base, welcomeReads: &atomic.Int64{}, calls: &sync.Map{}, communities: &sync.Map{}}

	h := &dsHarness{t: t, path: path, clk: clk, repo: repo, calls: map[string]int64{}}

	// The `instances` row, which task 27 made load-bearing: invariant 11's generation lives in it,
	// and `HealStatus` and `OnRestore` both read it. Registration never writes one — `dillad init`
	// does, at first boot — so a harness without it is a database no shipped instance ever has.
	keys := testInstanceKeys(t)
	if err := repo.CreateInstance(ctx, store.InstanceRow{
		InstanceID:          keys.InstanceID,
		ExternalSenderKeyID: keys.ExternalSenderKeyID,
		KeyHistory:          []byte{0x80},
		FrankingKeyID:       keys.FrankingKeyID,
		Generation:          1,
		PolicyVersion:       1,
		Created:             clk.Now().Unix(),
	}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	wasm, err := mlswasi.New(ctx, loadWasmForDS(t), mlswasi.Options{
		PoolSize: 2,
		CacheDir: sharedWasmCacheDir(t),
		Now:      clk.Now,
		OnCall:   h.countCall,
	})
	if err != nil {
		t.Fatalf("mlswasi.New: %v", err)
	}
	t.Cleanup(func() { _ = wasm.Close(context.Background()) })
	h.wasm = wasm

	h.auth = newFakeAuth()
	botCapability := capability.New(repo)
	// The back-off window comes from the policy the delivery service is built with, exactly as the
	// composition root wires it (deviation B23).
	h.gw = gateway.New(gateway.Options{
		Clock: clk, Store: gatewayStore{repo}, Generation: 1, Auth: h.auth,
		ScopeResolver: gatewayScopeResolver{repo: repo}, CommunityCompatibility: botCapability,
		Backoff: ds.DefaultPolicy().Backoff, BackoffJitter: ds.DefaultPolicy().BackoffJitter,
		MaxFrameBytes: maxFrameBytes,
	})
	t.Cleanup(func() { _ = h.gw.Shutdown(context.Background()) })

	h.channels = &fakeChannels{modes: map[id.ID][2]uint8{}}
	h.acl = &fakeACL{deny: ds.DenyUnlessMember{Store: repo}, eligible: map[id.ID]bool{}}
	h.postPolicy = &recordingPostPolicy{}

	registry := prometheus.NewRegistry()
	h.metrics = obs.NewMetrics(registry, registry)
	d, err := ds.New(ds.Options{
		Metrics:       h.metrics,
		Store:         repo,
		BotCapability: botCapability,
		Wasm:          wasm,
		Gateway:       h.gw,
		Clock:         clk,
		Keys:          testInstanceKeys(t),
		Policy:        ds.DefaultPolicy(),
		Channels:      h.channels,
		ACL:           h.acl,
		PostPolicy:    h.postPolicy,
	})
	if err != nil {
		t.Fatalf("ds.New: %v", err)
	}
	t.Cleanup(func() { _ = d.Shutdown(context.Background()) })
	h.ds = d
	return h
}

type gatewayScopeResolver struct{ repo store.Repository }

func (r gatewayScopeResolver) GroupResource(ctx context.Context, groupID id.ID) (gateway.GroupResource, error) {
	group, err := r.repo.GetGroup(ctx, groupID)
	if err != nil {
		return gateway.GroupResource{}, err
	}
	return gateway.GroupResource{Kind: group.Kind, CommunityID: group.CommunityID}, nil
}

func (r gatewayScopeResolver) ChannelCommunity(ctx context.Context, channelID id.ID) (*id.ID, error) {
	channel, err := r.repo.GetChannel(ctx, channelID)
	if err != nil {
		return nil, err
	}
	return channel.CommunityID, nil
}

func openMigratedSQLite(t *testing.T, path string) store.Repository {
	t.Helper()
	sqlitetest.MigratedFile(t, path)
	write, err := sqlite.OpenWrite(path)
	if err != nil {
		t.Fatalf("sqlite.OpenWrite: %v", err)
	}
	read, err := sqlite.OpenRead(path)
	if err != nil {
		t.Fatalf("sqlite.OpenRead: %v", err)
	}
	repo := sqlite.New(write, read)
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

// gatewayStore is the real repository plus the one method `store.Cursors` owns. `device_cursors`
// is 00002_mls.sql's table but `Cursors` does not join `store.Repository` until task 23, so the
// method is answered here: a zero cursor, which is what a group nobody has read means.
//
// It answers (zero, nil), NOT store.ErrNotFound. `sendReady` reads one cursor per group of the
// connecting device and returns the first error it gets, so an ErrNotFound here closes every
// connection with 4000 "register" before ready — and `Gateway.Online`, which invariants 5, 6 and
// 7 are all defined over, would then be false for every device forever. The gateway's own harness
// answers the same shape (internal/gateway/harness_test.go:255).
type gatewayStore struct{ store.Repository }

func (gatewayStore) GetCursor(_ context.Context, _, _ id.ID) (store.CursorRow, error) {
	return store.CursorRow{}, nil
}

func (h *dsHarness) countCall(export string) {
	h.callsMu.Lock()
	h.calls[export]++
	h.callsMu.Unlock()
}

// wasmCalls is the number of times the guest export was invoked so far.
func (h *dsHarness) wasmCalls(export string) int64 {
	h.callsMu.Lock()
	defer h.callsMu.Unlock()
	return h.calls[export]
}

// failNextTx makes the named repository call inside the next transaction fail, which is how the
// R12 tests prove that a failed transaction leaves neither SQL nor the cached PublicGroup
// half-written.
func (h *dsHarness) failNextTx(method string) { h.repo.failNext(method) }

// restartDS closes the delivery service and builds a new one over the SAME database and the same
// fake clock, which is what a process restart looks like from SQL's point of view.
func (h *dsHarness) restartDS() {
	h.t.Helper()
	if err := h.ds.Shutdown(context.Background()); err != nil {
		h.t.Fatalf("Shutdown: %v", err)
	}
	d, err := ds.New(ds.Options{
		Store: h.repo, Wasm: h.wasm, Gateway: h.gw, Clock: h.clk,
		Keys: testInstanceKeys(h.t), Policy: ds.DefaultPolicy(), Channels: h.channels,
		ACL: h.acl,
	})
	if err != nil {
		h.t.Fatalf("ds.New after restart: %v", err)
	}
	h.t.Cleanup(func() { _ = d.Shutdown(context.Background()) })
	h.ds = d
}

// channel declares the visibility and mode the injected Channels source reports for the fixture's
// target, and returns that target. Invariant 1's input is an injected seam here exactly as it is
// in production, where api.StructureChannels reads it from the `channels` table.
func (h *dsHarness) channel(t *testing.T, visibility, mode uint8) id.ID {
	t.Helper()
	target := dsFixture(t).targetID
	h.channels.modes[target] = [2]uint8{visibility, mode}
	return target
}

// registerRequest builds the registration the committed 1,500-leaf fixture supports. The binding
// is encoded HERE, in Go, and `Register` checks it against the one the Rust core put in the group
// context — so a positive case is a cross-language assertion that the two encoders agree byte for
// byte.
func (h *dsHarness) registerRequest(t *testing.T, target id.ID) ds.RegisterRequest {
	t.Helper()
	f := dsFixture(t)
	binding, err := cborx.Marshal(f.binding)
	if err != nil {
		t.Fatalf("encode the binding: %v", err)
	}
	return ds.RegisterRequest{
		GroupID:     f.groupID,
		Binding:     binding,
		GroupInfo:   f.groupInfo,
		RatchetTree: f.ratchetTree,
		Session:     auth.Session{Scope: auth.ScopeEnrolled},
	}
}

// mustRegister puts the 1,500-leaf fixture group on a private end-to-end-encrypted channel and
// returns it together with a session for the creating device, which every group-scoped read needs:
// `Info` and `Tree` are member-only and answer E_NOT_FOUND to a non-member.
//
// It SEEDS the group (dstest.SeedGroup) rather than registering it: POST /v1/groups adopts one
// leaf, the registering device's own (hardening G), and this fixture is a group at epoch 6 with
// 1,500 members that no registration produces. What it leaves behind is what Register leaves: the
// group row, the state blob and one member row per leaf. A test of Register itself registers a
// one-leaf group of testkit/fixtures/registration (register_leaf_test.go's mustRegisterOneLeaf).
func (h *dsHarness) mustRegister(t *testing.T) (ds.RegisterResult, auth.Session) {
	t.Helper()
	req := h.registerRequest(t, h.channel(t, 0, 0))
	if err := dstest.SeedGroup(context.Background(), h.ds, h.repo, h.wasm,
		testInstanceKeys(t).ExternalSenderKeyID, h.clk.Now().Unix(), dstest.Group{
			GroupID: req.GroupID, Binding: req.Binding, GroupInfo: req.GroupInfo, RatchetTree: req.RatchetTree,
		}); err != nil {
		t.Fatalf("seed the fixture group: %v", err)
	}
	return ds.RegisterResult{GroupID: req.GroupID, NextSeq: 1}, h.memberSession(t, req.GroupID, 0)
}

// memberSession is a session for the device at the given leaf of a registered group, read back
// out of `mls_members` — so the identity in the test is the one the guest's own credential
// carried, not one the test invented.
func (h *dsHarness) memberSession(t *testing.T, groupID id.ID, leaf uint32) auth.Session {
	t.Helper()
	members, err := h.repo.ListMembers(context.Background(), groupID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	for _, m := range members {
		if m.LeafIndex == leaf {
			return auth.Session{UserID: m.UserID, DeviceID: m.DeviceID, Scope: auth.ScopeEnrolled}
		}
	}
	t.Fatalf("no member at leaf %d of %s", leaf, groupID)
	return auth.Session{}
}

// fakeChannels is the delivery service's own tests' channel source. Production injects
// api.StructureChannels over the real `channels` table (Plan 2 task 2); these tests use the one
// committed fixture, whose binding names no community, so invariant 1's input and the
// registration ACL's verdict are declared here instead. It is not a mock of the delivery
// service — it is the other side of a seam the production build also injects, and
// internal/api/dschannels_test.go drives the real implementation through the real Register.
type fakeChannels struct {
	modes map[id.ID][2]uint8
	// refuse is what MayRegister answers; nil admits every registration.
	refuse error
	// asked records each MayRegister question, so a test can assert who was asked about.
	asked []registrationQuestion
	// recreate is what MayRecreate answers; nil admits the re-creation of a group whose heal is
	// pending (invariant 11).
	recreate error
}

func (f *fakeChannels) MayRecreate(context.Context, id.ID, ds.Binding) error { return f.recreate }

type registrationQuestion struct {
	user    id.ID
	binding ds.Binding
}

func (f *fakeChannels) MayRegister(_ context.Context, userID id.ID, b ds.Binding) error {
	f.asked = append(f.asked, registrationQuestion{user: userID, binding: b})
	return f.refuse
}

func (f *fakeChannels) Channel(_ context.Context, targetID id.ID) (visibility, mode uint8, err error) {
	v, ok := f.modes[targetID]
	if !ok {
		return 0, 0, ds.ErrNoChannel
	}
	return v[0], v[1], nil
}

// failingRepo wraps a real repository and can be told to fail one named call inside the next
// transaction. It is a fault injector, not a mock: every call it does not fail goes to the real
// store.
type failingRepo struct {
	store.Repository

	mu     sync.Mutex
	failOn string
	// onPut is called with every proposal row as it is written, INSIDE the transaction that
	// writes it. It is how a test observes what was true at that moment — for task 22, whether
	// the loop writing the row was running inside a batch's election window — without a double
	// for the store.
	onPut func(store.ProposalRow)
	// welcomeReads counts `ListWelcomes` round trips. The Welcome queue is the one read a commit
	// makes PER JOINER, and every row of it carries that epoch's whole ratchet tree, so "the
	// fan-out does not read the queue at all when the frame cannot be carried" is a claim about
	// this number and about nothing visible on the wire. It is a pointer so the sub-repository Tx
	// builds counts into the same total.
	welcomeReads *atomic.Int64
	// calls marks groups the store reports as call groups (kind 1). The committed fixture is a text
	// group, and the call-group rules — the 30 s TTL, the re-drive, the call sweep, the evictor — are
	// keyed on GroupRow.Kind; the override is the only way to reach them with real MLS state.
	calls        *sync.Map
	interactions sync.Map
	communities  *sync.Map
	// openGroupPages counts ListOpenGroups calls: the call sweeper's per-tick work must not grow with
	// the number of open groups on the instance.
	openGroupPages atomic.Int64
	// broken holds the groups whose GetGroup fails, on the outer repository only: one call group
	// whose sweep fails must not stop the call sweeper's tick.
	broken sync.Map
	// readHooks runs once, after the next ListMembers of its group returns: the window between a
	// caller's read of the member set and its next statement, in which a commit can reuse a leaf.
	readHooks map[id.ID]func(context.Context)
	// deletedProposals records that this transaction's repository reached DeleteProposals, so the
	// "TxCommit" fault fails only the transaction of a commit that ran all the way through.
	deletedProposals bool
	deviceLockHook   func()
}

// breakGroup makes the store's GetGroup of groupID fail.
func (r *failingRepo) breakGroup(groupID id.ID) { r.broken.Store(groupID, true) }

// reuseLeafAfterNextRead hands leaf to newcomer in mls_members right after the next ListMembers of
// groupID returns — what a commit that removed the leaf's device and added another at its index
// leaves behind.
func (r *failingRepo) reuseLeafAfterNextRead(t *testing.T, groupID id.ID, leaf uint32, newcomer id.ID) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.readHooks == nil {
		r.readHooks = map[id.ID]func(context.Context){}
	}
	r.readHooks[groupID] = func(ctx context.Context) {
		row, err := r.Repository.GetGroup(ctx, groupID)
		if err != nil {
			t.Errorf("GetGroup: %v", err)
			return
		}
		members, err := r.Repository.ListMembers(ctx, groupID)
		if err != nil {
			t.Errorf("ListMembers: %v", err)
			return
		}
		for i := range members {
			if members[i].LeafIndex == leaf {
				members[i].DeviceID = newcomer
			}
		}
		if err := r.Repository.ReplaceMembers(ctx, groupID, row.Epoch, members); err != nil {
			t.Errorf("ReplaceMembers: %v", err)
		}
	}
}

func (r *failingRepo) takeReadHook(groupID id.ID) func(context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	hook := r.readHooks[groupID]
	delete(r.readHooks, groupID)
	return hook
}

func (r *failingRepo) ListMembers(ctx context.Context, groupID id.ID) ([]store.MemberRow, error) {
	rows, err := r.Repository.ListMembers(ctx, groupID)
	if err == nil {
		if hook := r.takeReadHook(groupID); hook != nil {
			hook(ctx)
		}
	}
	return rows, err
}

func (r *failingRepo) DeleteProposals(ctx context.Context, groupID id.ID, refs [][]byte) error {
	if r.take("DeleteProposals") {
		return errInjected
	}
	r.deletedProposals = true
	return r.Repository.DeleteProposals(ctx, groupID, refs)
}

// markCall makes the store report groupID as a call group.
func (r *failingRepo) markCall(groupID id.ID) { r.calls.Store(groupID, true) }

func (r *failingRepo) isCall(groupID id.ID) bool {
	if r.calls == nil {
		return false
	}
	_, ok := r.calls.Load(groupID)
	return ok
}

func (r *failingRepo) GetGroup(ctx context.Context, groupID id.ID) (store.GroupRow, error) {
	if _, broken := r.broken.Load(groupID); broken {
		return store.GroupRow{}, errInjected
	}
	row, err := r.Repository.GetGroup(ctx, groupID)
	if err == nil {
		if community, scoped := r.communities.Load(groupID); scoped {
			cid := community.(id.ID)
			row.Kind = 0
			row.CommunityID = &cid
		} else if _, interaction := r.interactions.Load(groupID); interaction {
			row.Kind = 3
		} else if r.isCall(groupID) {
			row.Kind = 1
		}
	}
	return row, err
}

func (r *failingRepo) markInteraction(groupID id.ID) { r.interactions.Store(groupID, true) }

func (r *failingRepo) bindCommunity(groupID, communityID id.ID) {
	r.communities.Store(groupID, communityID)
}

func (r *failingRepo) ListOpenGroups(ctx context.Context, after id.ID, limit int32) ([]store.GroupRow, error) {
	r.openGroupPages.Add(1)
	rows, err := r.Repository.ListOpenGroups(ctx, after, limit)
	for i := range rows {
		if r.isCall(rows[i].GroupID) {
			rows[i].Kind = 1
		}
	}
	return rows, err
}

// restartWithEvictor rebuilds the delivery service over the same store with a call evictor.
func (h *dsHarness) restartWithEvictor(e ds.CallEvictor) {
	h.t.Helper()
	if err := h.ds.Shutdown(context.Background()); err != nil {
		h.t.Fatalf("Shutdown: %v", err)
	}
	d, err := ds.New(ds.Options{
		Store: h.repo, Wasm: h.wasm, Gateway: h.gw, Clock: h.clk,
		Keys: testInstanceKeys(h.t), Policy: ds.DefaultPolicy(), Channels: h.channels,
		ACL: h.acl, CallEvictor: e,
	})
	if err != nil {
		h.t.Fatalf("ds.New: %v", err)
	}
	h.t.Cleanup(func() { _ = d.Shutdown(context.Background()) })
	h.ds = d
}

var errInjected = errors.New("injected failure")

func (r *failingRepo) failNext(method string) {
	r.mu.Lock()
	r.failOn = method
	r.mu.Unlock()
}

func (r *failingRepo) take(method string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failOn != method {
		return false
	}
	r.failOn = ""
	return true
}

// observeProposals installs the hook PutProposal calls. Passing nil removes it.
func (r *failingRepo) observeProposals(fn func(store.ProposalRow)) {
	r.mu.Lock()
	r.onPut = fn
	r.mu.Unlock()
}

func (r *failingRepo) snapshotOnPut() func(store.ProposalRow) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.onPut
}

func (r *failingRepo) ListWelcomes(ctx context.Context, deviceID id.ID, afterID int64, limit int32) ([]store.WelcomeFull, error) {
	if r.welcomeReads != nil {
		r.welcomeReads.Add(1)
	}
	return r.Repository.ListWelcomes(ctx, deviceID, afterID, limit)
}

func (r *failingRepo) PutWelcomes(ctx context.Context, rows []store.WelcomeRow) error {
	if r.take("PutWelcomes") {
		return errInjected
	}
	return r.Repository.PutWelcomes(ctx, rows)
}

// welcomeQueueReads is how many `ListWelcomes` round trips the store has served so far.
func (h *dsHarness) welcomeQueueReads() int64 { return h.repo.welcomeReads.Load() }

func (r *failingRepo) PutProposal(ctx context.Context, row store.ProposalRow) error {
	if fn := r.snapshotOnPut(); fn != nil {
		fn(row)
	}
	return r.Repository.PutProposal(ctx, row)
}

func (r *failingRepo) Tx(ctx context.Context, fn func(store.Repository) error) error {
	return r.Repository.Tx(ctx, func(tx store.Repository) error {
		sub := &failingRepo{
			Repository: tx, failOn: r.snapshotFailOn(), onPut: r.snapshotOnPut(),
			welcomeReads: r.welcomeReads, calls: r.calls, communities: r.communities, deviceLockHook: r.deviceLockHook,
		}
		err := fn(sub)
		// The sub-repository owns the injection for the duration of the transaction; whatever it
		// did not consume goes back, so failNextTx("X") before a call that never reaches X does
		// not silently disarm.
		r.failNext(sub.snapshotFailOn())
		// "TxCommit" fails a transaction whose every statement succeeded, at the commit: the
		// rollback after the last statement of a commit's transaction. Returning an error from the
		// closure is what makes the real store roll it back.
		if err == nil && sub.deletedProposals && r.take("TxCommit") {
			return errInjected
		}
		return err
	})
}

func (r *failingRepo) CreateBotKeyPackagePublicationReceipt(ctx context.Context, row store.KeyPackagePublicationReceipt) error {
	if r.take("CreateBotKeyPackagePublicationReceipt") {
		return errInjected
	}
	return r.Repository.CreateBotKeyPackagePublicationReceipt(ctx, row)
}

func (r *failingRepo) LockKeyPackageDevice(ctx context.Context, deviceID id.ID) error {
	if r.deviceLockHook != nil {
		r.deviceLockHook()
	}
	return r.Repository.LockKeyPackageDevice(ctx, deviceID)
}

func (r *failingRepo) snapshotFailOn() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failOn
}

func (r *failingRepo) CreateGroup(ctx context.Context, row store.GroupRow) error {
	if r.take("CreateGroup") {
		return errInjected
	}
	return r.Repository.CreateGroup(ctx, row)
}

func (r *failingRepo) PutGroupState(ctx context.Context, groupID id.ID, epoch uint64, state, groupInfo, treeHash []byte) error {
	if r.take("PutGroupState") {
		return errInjected
	}
	return r.Repository.PutGroupState(ctx, groupID, epoch, state, groupInfo, treeHash)
}

func (r *failingRepo) ReplaceMembers(ctx context.Context, groupID id.ID, epoch uint64, m []store.MemberRow) error {
	if r.take("ReplaceMembers") {
		return errInjected
	}
	return r.Repository.ReplaceMembers(ctx, groupID, epoch, m)
}

func (r *failingRepo) AppendHandshake(ctx context.Context, row store.HandshakeRow) error {
	if r.take("AppendHandshake") {
		return errInjected
	}
	return r.Repository.AppendHandshake(ctx, row)
}

// ReissueProposal is the write a re-issue and a same-epoch re-arm go through (DS-1).
func (r *failingRepo) ReissueProposal(ctx context.Context, oldRef []byte, p store.ProposalRow) error {
	if r.take("ReissueProposal") {
		return errInjected
	}
	return r.Repository.ReissueProposal(ctx, oldRef, p)
}

// ---------------------------------------------------------------- the fixture

// dsFixtureData is the committed 1,500-leaf public group of testkit/fixtures/ds-1500, plus the
// `dilla_binding` its generator put in the group context (testkit/src/fixtures.rs:96-104).
type dsFixtureData struct {
	groupID     id.ID
	targetID    id.ID
	groupInfo   []byte
	ratchetTree []byte
	binding     ds.Binding
}

var (
	fixtureOnce sync.Once
	fixtureVal  dsFixtureData
	fixtureErr  error
)

const dsFixtureDir = "../../testkit/fixtures/ds-1500"

// dsFixture reads the committed fixture once per `go test` run and refuses to run once its
// KeyPackage lifetimes have expired: PublicGroup::from_external validates every leaf's lifetime
// and would otherwise fail with an opaque MLS error.
func dsFixture(tb testing.TB) dsFixtureData {
	tb.Helper()
	fixtureOnce.Do(func() { fixtureVal, fixtureErr = loadDSFixture() })
	if fixtureErr != nil {
		tb.Fatalf("testkit/fixtures/ds-1500: %v; regenerate it with\n"+
			"  cargo run -p dilla-testkit --bin dilla-testkit -- gen-public-group --leaves 1500 "+
			"--out testkit/fixtures/ds-1500/", fixtureErr)
	}
	return fixtureVal
}

func loadDSFixture() (dsFixtureData, error) {
	raw, err := os.ReadFile(filepath.Join(dsFixtureDir, "manifest.json"))
	if err != nil {
		return dsFixtureData{}, err
	}
	var m struct {
		GroupIDHex string `json:"group_id_hex"`
		NotAfter   uint64 `json:"not_after"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return dsFixtureData{}, err
	}
	if m.NotAfter == 0 {
		return dsFixtureData{}, errors.New("manifest.json carries no not_after")
	}
	if now := uint64(time.Now().Unix()); now >= m.NotAfter {
		return dsFixtureData{}, errors.New("the fixture's KeyPackage lifetimes have expired")
	}
	groupID, err := id.Parse(m.GroupIDHex)
	if err != nil {
		return dsFixtureData{}, err
	}
	var out dsFixtureData
	out.groupID = groupID
	// The generator binds the group to its own target, and the group id IS that target
	// (testkit/src/fixtures.rs:106).
	out.targetID = groupID
	if out.groupInfo, err = os.ReadFile(filepath.Join(dsFixtureDir, "group_info.mls")); err != nil {
		return dsFixtureData{}, err
	}
	if out.ratchetTree, err = os.ReadFile(filepath.Join(dsFixtureDir, "ratchet_tree.mls")); err != nil {
		return dsFixtureData{}, err
	}
	// testkit/src/fixtures.rs:96-104, verbatim.
	var instance id.ID
	for i := range instance {
		instance[i] = 0x11
	}
	out.binding = ds.Binding{
		V: 1, InstanceID: instance, CommunityID: nil, TargetID: groupID,
		Kind: 0, PolicyVersion: 1, E2EEVersion: 1, MediaVersion: 0,
	}
	return out, nil
}

// loadWasmForDS reads the same artifact internal/mlswasi's tests use. An absent module is a hard
// failure, not a skip: CI downloads it from the rust-wasi job, so its absence means the
// cross-target leg did not run.
func loadWasmForDS(tb testing.TB) []byte {
	tb.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "mlswasi", "testdata", "dilla_core_wasi.wasm"))
	if err != nil {
		tb.Fatalf("the wasi module is absent (%v); build it with "+
			"`cargo build -p dilla-core-wasi --target wasm32-wasip1 --release --locked` and copy "+
			"it to internal/mlswasi/testdata/", err)
	}
	return raw
}

var (
	dsCacheOnce sync.Once
	dsCacheDir  string
)

// sharedWasmCacheDir compiles the large OpenMLS module once per `go test` run rather than once per
// harness: gap-31 §5 records wazero's compile time on it as unmeasured, so it is not a cost to pay
// twenty-five times.
func sharedWasmCacheDir(tb testing.TB) string {
	tb.Helper()
	// One directory for every test in the package, removed by TestMain: tb.TempDir would be removed
	// when the first test ends.
	dsCacheOnce.Do(func() {
		dsCacheDir, _ = os.MkdirTemp("", "ds-wasm-cache-") //nolint:usetesting // shared by the whole package, removed by TestMain
	})
	return dsCacheDir
}

// TestMain drops the shared compilation cache once the package's tests are done.
func TestMain(m *testing.M) {
	code := m.Run()
	if dsCacheDir != "" {
		_ = os.RemoveAll(dsCacheDir)
	}
	os.Exit(code)
}

// testInstanceKeys is one deterministic set of instance keys for the whole package. InstanceID is
// the fixture generator's own instance (0x11 sixteen times, testkit/src/fixtures.rs:98).
func testInstanceKeys(tb testing.TB) ds.InstanceKeys {
	tb.Helper()
	var k ds.InstanceKeys
	for i := range k.InstanceID {
		k.InstanceID[i] = 0x11
	}
	for i := range k.ExternalSenderKeyID {
		k.ExternalSenderKeyID[i] = 0x22
	}
	for i := range k.FrankingKeyID {
		k.FrankingKeyID[i] = 0x33
	}
	// The instance's external-sender signing key is the fixture's own, re-derived rather than
	// invented: `queue_proposal` resolves an external proposal's sender through the group's
	// `external_senders` extension and verifies the signature against it, so a key of the test's
	// invention would make every ProposeAdd and ProposeRemove fail at the guest and leave the
	// whole issuing path of task 21 unexercised. See fixtureExternalSenderKey in proposal_test.go.
	k.ExternalSenderPriv = fixtureExternalSenderKey()
	for i := range k.FrankingKey {
		k.FrankingKey[i] = byte(0x80 + i)
	}
	return k
}

func (r *failingRepo) ListClosedGroupLiveProposals(ctx context.Context, limit int32) ([]store.ProposalRow, error) {
	if r.take("ListClosedGroupLiveProposals") {
		return nil, errInjected
	}
	return r.Repository.ListClosedGroupLiveProposals(ctx, limit)
}
