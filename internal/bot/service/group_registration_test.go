package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

func TestGroupRegistrationPersistsIntentAndResolvesLostCreateResponse(t *testing.T) {
	c := newRegistrationFixture(t)
	c.session.registerErr = errors.New("connection reset after request")
	c.session.prepareReceipt = true

	got, err := c.coordinator.RegisterFirstGroup(context.Background(), c.request())
	if err != nil {
		t.Fatalf("RegisterFirstGroup: %v", err)
	}
	if got.NextSequence != 1 || c.native.registeredNext != 1 {
		t.Fatalf("receipt next sequence=%d native registered=%d, want original 1", got.NextSequence, c.native.registeredNext)
	}
	if c.native.createCalls != 1 || c.session.registerCalls != 1 || c.session.receiptCalls != 1 || c.session.postedGeneration != 8 {
		t.Fatalf("calls create/register/receipt/generation = %d/%d/%d/%d, want 1/1/1/8", c.native.createCalls, c.session.registerCalls, c.session.receiptCalls, c.session.postedGeneration)
	}
}

func TestGroupRegistrationRestartUsesReceiptAfterGenerationAdvances(t *testing.T) {
	c := newRegistrationFixture(t)
	c.session.registerErr = errors.New("response lost")
	c.session.receiptErr = client.ErrRegistrationReceiptNotFound
	c.session.groupInfoErrors = []error{errGroupRegistrationNotFound, nil} // group row proves acceptance may have happened
	if _, err := c.coordinator.RegisterFirstGroup(context.Background(), c.request()); !errors.Is(err, ErrGroupRegistrationRecovery) {
		t.Fatalf("first pass error = %v, want recovery", err)
	}
	if c.native.registeredNext != 0 {
		t.Fatal("native group was registered without a receipt")
	}

	c.discovery.generation = 9
	c.session.receipt = c.receipt(1)
	c.session.receiptErr = nil
	c.session.registerErr = nil
	coordinator, err := NewGroupRegistrationCoordinator(c.instance, c.community, c.install, c.native, c.discovery, c.session)
	if err != nil {
		t.Fatalf("restart coordinator: %v", err)
	}
	c.coordinator = coordinator
	got, err := c.coordinator.RegisterFirstGroup(context.Background(), c.request())
	if err != nil {
		t.Fatalf("receipt recovery after generation advance: %v", err)
	}
	if got.AcceptedGeneration != 8 || c.session.registerCalls != 1 || c.native.createCalls != 1 || c.native.registeredNext != 1 {
		t.Fatalf("recovery used wrong operation: receipt generation=%d register calls=%d create calls=%d registered=%d", got.AcceptedGeneration, c.session.registerCalls, c.native.createCalls, c.native.registeredNext)
	}
}

func TestGroupRegistrationRejectsConflictingReceipt(t *testing.T) {
	c := newRegistrationFixture(t)
	c.session.registerErr = errors.New("response lost")
	c.session.prepareReceipt = true
	c.session.conflictReceipt = true

	if _, err := c.coordinator.RegisterFirstGroup(context.Background(), c.request()); !errors.Is(err, ErrGroupRegistrationRecovery) {
		t.Fatalf("error = %v, want recovery for contradictory receipt", err)
	}
	if c.native.registeredNext != 0 {
		t.Fatal("native group registered with a contradictory receipt")
	}
}

func TestGroupRegistrationRejectsReceiptWithWrongInitialStateAndRetainsIntent(t *testing.T) {
	for _, tc := range []struct {
		name         string
		initialEpoch uint64
		nextSequence uint64
	}{
		{name: "initial epoch", initialEpoch: 1, nextSequence: 1},
		{name: "next sequence", initialEpoch: 0, nextSequence: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newRegistrationFixture(t)
			c.session.prepareReceipt = true
			c.session.receiptInitialEpoch = tc.initialEpoch
			c.session.receiptNextSequence = tc.nextSequence

			if _, err := c.coordinator.RegisterFirstGroup(context.Background(), c.request()); !errors.Is(err, ErrGroupRegistrationRecovery) {
				t.Fatalf("error = %v, want recovery for invalid initial receipt state", err)
			}
			if c.native.registeredNext != 0 {
				t.Fatalf("native registered next sequence=%d, want unresolved intent", c.native.registeredNext)
			}
			value := c.native.values[registrationStateMapKey(groupRegistrationStateArea, groupRegistrationKey(c.group))]
			intent, err := decodeGroupRegistrationIntent(value.Value)
			if err != nil {
				t.Fatalf("decode retained intent: %v", err)
			}
			if intent.Phase != groupRegistrationPending || !intent.Submitted || len(intent.Body) == 0 {
				t.Fatalf("retained intent phase=%d submitted=%v body length=%d", intent.Phase, intent.Submitted, len(intent.Body))
			}
		})
	}
}

func TestDecodeGroupRegistrationIntentRejectsWrongCompletedInitialState(t *testing.T) {
	intent := groupRegistrationIntent{
		Operation: id.New(), Group: id.New(), Target: id.New(), Kind: 0, Policy: 1,
		ExternalSender: append([]byte{1}, make([]byte, 31)...), Generation: 8,
		Body: []byte{0x80}, Submitted: true, Phase: groupRegistrationComplete,
	}
	intent.Receipt = client.BotGroupRegistrationReceipt{
		OperationID: intent.Operation, GroupID: intent.Group,
		RegistrationSHA256: groupRegistrationHash(intent.Body), AcceptedGeneration: intent.Generation,
		InitialEpoch: 1, NextSequence: 1,
	}
	encoded, err := encodeGroupRegistrationIntent(intent)
	if err != nil {
		t.Fatalf("encode completed intent: %v", err)
	}
	if _, err := decodeGroupRegistrationIntent(encoded); !errors.Is(err, ErrGroupRegistrationRecovery) {
		t.Fatalf("decode error = %v, want recovery for noninitial epoch", err)
	}
}

func TestRegisterFirstGroupNilReceiverReturnsInvalidRequest(t *testing.T) {
	var coordinator *GroupRegistrationCoordinator
	if _, err := coordinator.RegisterFirstGroup(context.Background(), FirstGroupRequest{}); err == nil {
		t.Fatal("RegisterFirstGroup succeeded on nil coordinator")
	}
}

func TestGroupRegistrationRestoreGapRetainsIntent(t *testing.T) {
	c := newRegistrationFixture(t)
	c.discovery.generations = []uint64{8, 8, 9}
	c.session.registerErr = errors.New("response lost")
	c.session.receiptErr = client.ErrRegistrationReceiptNotFound

	if _, err := c.coordinator.RegisterFirstGroup(context.Background(), c.request()); !errors.Is(err, ErrGroupRegistrationOutcomeUnknown) {
		t.Fatalf("error = %v, want unknown outcome after restore gap", err)
	}
	if c.native.createCalls != 1 || c.session.registerCalls != 1 || c.native.registeredNext != 0 {
		t.Fatalf("restore gap calls create/register/registered=%d/%d/%d", c.native.createCalls, c.session.registerCalls, c.native.registeredNext)
	}
	if _, err := c.coordinator.RegisterFirstGroup(context.Background(), c.request()); !errors.Is(err, ErrGroupRegistrationOutcomeUnknown) {
		t.Fatalf("retry error = %v, want retained unknown outcome", err)
	}
	if c.native.createCalls != 1 || c.session.registerCalls != 1 {
		t.Fatalf("restore gap retried a native or server mutation: create=%d register=%d", c.native.createCalls, c.session.registerCalls)
	}
}

func TestGroupRegistrationStateTransactionFailureStopsBeforeNativeCreate(t *testing.T) {
	c := newRegistrationFixture(t)
	c.native.failTransactionAt = 1

	if _, err := c.coordinator.RegisterFirstGroup(context.Background(), c.request()); err == nil {
		t.Fatal("RegisterFirstGroup succeeded with failed journal transaction")
	}
	if c.native.createCalls != 0 || c.session.registerCalls != 0 {
		t.Fatalf("native create/server register calls = %d/%d, want 0/0", c.native.createCalls, c.session.registerCalls)
	}
}

func TestGroupRegistrationBodyPersistenceFailureStopsBeforeServerPost(t *testing.T) {
	c := newRegistrationFixture(t)
	c.native.failTransactionAt = 2

	if _, err := c.coordinator.RegisterFirstGroup(context.Background(), c.request()); err == nil {
		t.Fatal("RegisterFirstGroup succeeded when saving the exact registration body failed")
	}
	if c.native.createCalls != 1 || c.session.registerCalls != 0 {
		t.Fatalf("native create/server post calls = %d/%d, want 1/0", c.native.createCalls, c.session.registerCalls)
	}
}

func TestGroupRegistrationDenialDoesNotResolveNativeIntent(t *testing.T) {
	c := newRegistrationFixture(t)
	c.session.registerErr = &client.APIError{Status: http.StatusForbidden, Code: "E_FORBIDDEN", Outcome: client.OutcomeDefinitive}
	c.session.groupInfoErr = errGroupRegistrationNotFound

	if _, err := c.coordinator.RegisterFirstGroup(context.Background(), c.request()); !errors.Is(err, ErrGroupRegistrationDenied) {
		t.Fatalf("error = %v, want typed denial", err)
	}
	if c.native.registeredNext != 0 || c.native.discardCalls != 0 {
		t.Fatalf("native registered=%d discard calls=%d, want unresolved intent", c.native.registeredNext, c.native.discardCalls)
	}
}

type registrationFixture struct {
	coordinator *GroupRegistrationCoordinator
	native      *registrationNative
	session     *registrationSession
	discovery   *registrationDiscovery
	instance    id.ID
	community   id.ID
	install     id.ID
	group       id.ID
	target      id.ID
}

func newRegistrationFixture(t *testing.T) *registrationFixture {
	t.Helper()
	instance, community, install, group, target := id.New(), id.New(), id.New(), id.New(), id.New()
	state := &registrationNative{binding: native.Binding{Instance: instance, Community: community, Installation: install}, values: map[string]native.StateValue{}}
	discovery := &registrationDiscovery{value: client.Discovery{InstanceID: instance, Generation: 8, ExternalSenderPub: make([]byte, 32)}}
	discovery.value.ExternalSenderPub[0] = 1
	session := &registrationSession{groupInfoErr: errGroupRegistrationNotFound, receiptErr: client.ErrRegistrationReceiptNotFound}
	coordinator, err := NewGroupRegistrationCoordinator(instance, community, install, state, discovery, session)
	if err != nil {
		t.Fatalf("NewGroupRegistrationCoordinator: %v", err)
	}
	return &registrationFixture{coordinator: coordinator, native: state, session: session, discovery: discovery, instance: instance, community: community, install: install, group: group, target: target}
}

func (f *registrationFixture) request() FirstGroupRequest {
	return FirstGroupRequest{GroupID: f.group, TargetID: f.target, Kind: 0, Policy: 1}
}

func (f *registrationFixture) receipt(next uint64) client.BotGroupRegistrationReceipt {
	return client.BotGroupRegistrationReceipt{OperationID: f.native.lastOperation, GroupID: f.group, RegistrationSHA256: testRegistrationHash(f.native.lastBody), AcceptedGeneration: 8, InitialEpoch: 0, NextSequence: next}
}

func TestGroupRegistrationRechecksPinnedInstanceKeyBeforeRetry(t *testing.T) {
	f := newRegistrationFixture(t)
	f.session.prepareReceipt = true
	if _, err := f.coordinator.RegisterFirstGroup(context.Background(), f.request()); err != nil {
		t.Fatalf("initial registration: %v", err)
	}
	createCalls, registerCalls := f.native.createCalls, f.session.registerCalls
	f.discovery.value.ExternalSenderPub[0] ^= 0xff
	if _, err := f.coordinator.RegisterFirstGroup(context.Background(), f.request()); err == nil {
		t.Fatal("registration accepted a changed discovery key")
	}
	if f.native.createCalls != createCalls || f.session.registerCalls != registerCalls {
		t.Fatalf("changed key reached group work: creates %d->%d, registrations %d->%d", createCalls, f.native.createCalls, registerCalls, f.session.registerCalls)
	}
}

var errGroupRegistrationNotFound = &client.APIError{Status: http.StatusNotFound, Code: "E_NOT_FOUND"}

type registrationNative struct {
	binding           native.Binding
	pinnedKey         [32]byte
	bootstrapCalls    int
	values            map[string]native.StateValue
	createCalls       int
	registeredNext    uint64
	discardCalls      int
	failTransactionAt int
	transactionCalls  int
	lastOperation     id.ID
	lastBody          []byte
}

func (n *registrationNative) Binding() native.Binding { return n.binding }
func (n *registrationNative) Bootstrap(_ context.Context, key [32]byte) (native.BootstrapResult, error) {
	n.bootstrapCalls++
	if n.pinnedKey != ([32]byte{}) && n.pinnedKey != key {
		return native.BootstrapResult{}, errors.New("instance external-sender key changed")
	}
	n.pinnedKey = key
	return native.BootstrapResult{}, nil
}
func (n *registrationNative) StateGet(_ context.Context, area uint64, key []byte) (native.StateValue, error) {
	return n.values[registrationStateMapKey(area, key)], nil
}
func (n *registrationNative) StateTransaction(_ context.Context, _ id.ID, changes []native.StateChange) ([]uint64, error) {
	n.transactionCalls++
	if n.transactionCalls == n.failTransactionAt {
		return nil, errors.New("state transaction failed")
	}
	revisions := make([]uint64, len(changes))
	for i, change := range changes {
		key := registrationStateMapKey(change.Area, change.Key)
		old := n.values[key]
		if old.Revision != change.ExpectedRevision {
			return nil, errors.New("state revision conflict")
		}
		revisions[i] = old.Revision + 1
		n.values[key] = native.StateValue{Revision: revisions[i], Value: append([]byte(nil), change.Value...)}
	}
	return revisions, nil
}
func (n *registrationNative) CreateGroup(_ context.Context, operation, group, target id.ID, kind, policy uint64, external []byte) (native.GroupCreateResult, error) {
	n.createCalls++
	n.lastOperation = operation
	binding, _ := cborx.Marshal([]any{uint64(1), n.binding.Instance, n.binding.Community, target, kind, policy, uint64(1), uint64(0)})
	info, _ := cborx.Marshal([]any{group, kind, n.binding.Community, target, uint64(0), uint64(0), uint64(0), false})
	tree := []byte{0x80}
	body, _ := cborx.Marshal([]any{group, binding, info, tree})
	n.lastBody = body
	return native.GroupCreateResult{Group: group, Binding: native.GroupBinding{Version: 1, Instance: n.binding.Instance, Community: &n.binding.Community, Target: target, Kind: kind, Policy: policy, E2EEVersion: 1, MediaVersion: 0}, BindingBody: binding, GroupInfo: info, RatchetTree: tree}, nil
}
func (n *registrationNative) GroupRegistered(_ context.Context, operation, group id.ID, next uint64) error {
	if operation != n.lastOperation || group.IsZero() {
		return errors.New("unexpected group registration")
	}
	n.registeredNext = next
	return nil
}
func (n *registrationNative) DiscardGroup(context.Context, id.ID, id.ID) error {
	n.discardCalls++
	return nil
}

type registrationDiscovery struct {
	value       client.Discovery
	generation  uint64
	generations []uint64
}

func (d *registrationDiscovery) Discover(context.Context) (client.Discovery, error) {
	if len(d.generations) != 0 {
		d.value.Generation = d.generations[0]
		d.generations = d.generations[1:]
	}
	if d.generation != 0 {
		d.value.Generation = d.generation
	}
	return d.value, nil
}

type registrationSession struct {
	registerErr         error
	receiptErr          error
	groupInfoErr        error
	receipt             client.BotGroupRegistrationReceipt
	registerCalls       int
	receiptCalls        int
	groupInfoErrors     []error
	prepareReceipt      bool
	conflictReceipt     bool
	receiptInitialEpoch uint64
	receiptNextSequence uint64
	postedGeneration    uint64
}

func (s *registrationSession) RegisterBotGroup(_ context.Context, _ id.ID, operation id.ID, generation uint64, body []byte) (client.BotGroupRegistrationReceipt, error) {
	s.registerCalls++
	s.postedGeneration = generation
	if s.prepareReceipt {
		var fields []any
		_ = cborx.Unmarshal(body, &fields)
		group, _ := registrationID(fields[0])
		nextSequence := s.receiptNextSequence
		if nextSequence == 0 {
			nextSequence = 1
		}
		s.receipt = client.BotGroupRegistrationReceipt{OperationID: operation, GroupID: group, RegistrationSHA256: testRegistrationHash(body), AcceptedGeneration: generation, InitialEpoch: s.receiptInitialEpoch, NextSequence: nextSequence}
		if s.conflictReceipt {
			s.receipt.RegistrationSHA256[0] ^= 0xff
		}
	}
	return s.receipt, s.registerErr
}
func (s *registrationSession) BotGroupRegistrationReceipt(context.Context, id.ID, id.ID) (client.BotGroupRegistrationReceipt, error) {
	s.receiptCalls++
	if !s.receipt.OperationID.IsZero() && s.registerCalls != 0 {
		return s.receipt, nil
	}
	return s.receipt, s.receiptErr
}
func (s *registrationSession) GroupInfo(context.Context, id.ID) (client.GroupInfo, error) {
	if len(s.groupInfoErrors) != 0 {
		err := s.groupInfoErrors[0]
		s.groupInfoErrors = s.groupInfoErrors[1:]
		return client.GroupInfo{}, err
	}
	return client.GroupInfo{}, s.groupInfoErr
}

func registrationStateMapKey(area uint64, key []byte) string { return string(rune(area)) + string(key) }
func testRegistrationHash(body []byte) [32]byte {
	h := sha256.New()
	_, _ = h.Write([]byte("dilla bot group registration v1\x00"))
	_, _ = h.Write(body)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}
