package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

func sid(n byte) id.ID { var x id.ID; x[15] = n; return x }

type fakeLifecycleNative struct {
	binding         native.Binding
	phase           uint64
	user, device    id.ID
	username        string
	published       bool
	state           []byte
	stateRevision   uint64
	stateErrOnce    bool
	stateTxIDs      []id.ID
	bootstrap       native.BootstrapResult
	bootstrapKey    [32]byte
	pending         bool
	completeCalls   int
	completeErrOnce bool
	completeTimes   []uint64
	intents         int
	resolved        int
	lastIntent      protocol.KeyPackageIntent
	lastResolution  protocol.KeyPackageResolution
	resolveErrOnce  bool
	greatestExpiry  uint64
}

func newFakeNative(i, c, installation id.ID) *fakeLifecycleNative {
	n := &fakeLifecycleNative{binding: native.Binding{Instance: i, Community: c, Installation: installation}, phase: 0}
	n.device, n.user, n.username = sid(44), sid(45), "bot-45"
	n.bootstrap.Device = native.DeviceRegistration{DeviceID: [16]byte(n.device), Tier: 0, SignerTier: 0, Credential: []byte("native-placeholder")}
	n.bootstrap.Device.DSKPublic[0] = 1
	n.bootstrap.UMKPublic[0], n.bootstrap.SSKPublic[0], n.bootstrap.SigUMKSSK[0] = 2, 3, 4
	return n
}
func (n *fakeLifecycleNative) Identity(context.Context) (native.Identity, error) {
	if n.phase == 0 {
		return native.Identity{Phase: 0}, nil
	}
	i, u, d := n.binding.Instance, n.user, n.device
	return native.Identity{Phase: 2, Instance: &i, User: &u, Device: &d, Username: n.username, DeviceListPublished: n.published}, nil
}
func (n *fakeLifecycleNative) Bootstrap(_ context.Context, key [32]byte) (native.BootstrapResult, error) {
	if n.bootstrapKey != ([32]byte{}) && n.bootstrapKey != key {
		return native.BootstrapResult{}, errors.New("instance external-sender key changed")
	}
	n.bootstrapKey = key
	n.pending = true
	return n.bootstrap, nil
}
func (n *fakeLifecycleNative) CompleteEnrollment(_ context.Context, user id.ID, name string, completedAt uint64) (native.EnrollmentResult, error) {
	if user != n.user || name != n.username {
		return native.EnrollmentResult{}, ErrRecovery
	}
	n.phase = 2
	n.pending = false
	n.completeCalls++
	n.completeTimes = append(n.completeTimes, completedAt)
	if n.completeErrOnce {
		n.completeErrOnce = false
		return native.EnrollmentResult{}, errors.New("synthetic accepted native completion with lost response")
	}
	body, _ := cborx.Marshal([]any{uint64(1), []byte("list"), make([]byte, 64), make([]byte, 32)})
	return native.EnrollmentResult{Credential: []byte("final-credential"), DeviceListBody: body}, nil
}
func (n *fakeLifecycleNative) SignSession(_ context.Context, nonce [32]byte, purpose uint64) (native.SessionSignature, error) {
	return native.SessionSignature{Nonce: nonce, Purpose: purpose}, nil
}
func (*fakeLifecycleNative) StoreSession(context.Context, string, uint64, uint64) error { return nil }
func (*fakeLifecycleNative) Session(context.Context) (native.Session, error) {
	return native.Session{}, nil
}
func (*fakeLifecycleNative) ClearSession(context.Context) error           { return nil }
func (n *fakeLifecycleNative) DeviceList(context.Context) ([]byte, error) { return nil, nil }
func (n *fakeLifecycleNative) DeviceListPublished(context.Context) error {
	n.published = true
	return nil
}
func (n *fakeLifecycleNative) KeyPackageIntent(_ context.Context, in protocol.KeyPackageIntent) (protocol.KeyPackageIntentResult, error) {
	n.intents++
	n.lastIntent = in
	ordinary := make([]any, in.Count)
	refs := make([][32]byte, in.Count+in.LastResort)
	for i := range ordinary {
		ordinary[i] = []byte(fmt.Sprintf("public-kp-%d", i))
		refs[i][0], refs[i][1] = byte(i+1), in.OperationID[15]
	}
	var fallback any
	if in.LastResort != 0 {
		fallback = []byte("fallback")
		refs[len(ordinary)][0], refs[len(ordinary)][1] = 0xff, in.OperationID[15]
	}
	upload, _ := cborx.Marshal([]any{ordinary, fallback})
	expiry := n.greatestExpiry
	if expiry == 0 {
		expiry = 1_900_000_000
	}
	return protocol.KeyPackageIntentResult{UploadBody: upload, PackageRefs: refs, GreatestExpiry: expiry}, nil
}
func (n *fakeLifecycleNative) ResolveKeyPackageIntent(_ context.Context, resolution protocol.KeyPackageResolution) error {
	n.resolved++
	n.lastResolution = resolution
	if n.resolveErrOnce {
		n.resolveErrOnce = false
		return errors.New("synthetic uncertain native resolution")
	}
	return nil
}
func (n *fakeLifecycleNative) StateGet(context.Context, uint64, []byte) (native.StateValue, error) {
	if n.state == nil {
		return native.StateValue{}, nil
	}
	return native.StateValue{Revision: n.stateRevision, Value: append([]byte(nil), n.state...)}, nil
}
func (n *fakeLifecycleNative) StateTransaction(_ context.Context, tx id.ID, changes []native.StateChange) ([]uint64, error) {
	n.stateTxIDs = append(n.stateTxIDs, tx)
	if len(changes) != 1 || changes[0].ExpectedRevision != n.stateRevision {
		return nil, ErrRecovery
	}
	n.state = append([]byte(nil), changes[0].Value...)
	n.stateRevision++
	if n.stateErrOnce {
		n.stateErrOnce = false
		return nil, errors.New("synthetic committed state transaction with lost reply")
	}
	return []uint64{n.stateRevision}, nil
}

type fakeLifecycleRuntime struct {
	instance                    id.ID
	origin                      string
	installation                client.Installation
	user, device                id.ID
	claimRequest, enrollRequest id.ID
	claimCalls, enrollCalls     int
	loseEnrollOnce              bool
	enrollErrOnce               error
	enrollClaim                 id.ID
	claimSequence               []client.Claim
	enrollObservations          []client.ResponseObservation
}

func (r *fakeLifecycleRuntime) InstanceID() id.ID { return r.instance }
func (r *fakeLifecycleRuntime) Origin() string    { return r.origin }

func (r *fakeLifecycleRuntime) ListInstallationsPage(_ context.Context, after *id.ID) ([]client.Installation, error) {
	if after != nil {
		return nil, nil
	}
	return []client.Installation{r.installation}, nil
}
func (r *fakeLifecycleRuntime) ClaimObserved(_ context.Context, installation, request id.ID) (client.Claim, client.ResponseObservation, error) {
	r.claimCalls++
	if r.claimRequest.IsZero() {
		r.claimRequest = request
	} else if r.claimRequest != request {
		return client.Claim{}, client.ResponseObservation{}, ErrRecovery
	}
	if len(r.claimSequence) > 0 {
		claim := r.claimSequence[0]
		r.claimSequence = r.claimSequence[1:]
		return claim, client.ResponseObservation{}, nil
	}
	return client.Claim{InstallationID: installation, ClaimID: sid(77), Expires: 1_800_000_600}, client.ResponseObservation{ServerTime: 1_800_000_000, HasServerTime: true}, nil
}
func (r *fakeLifecycleRuntime) EnrollObserved(_ context.Context, installation, request, claim id.ID, _, _, _ []byte, d client.DeviceRegistration) (client.Enrollment, client.ResponseObservation, error) {
	r.enrollCalls++
	r.enrollClaim = claim
	if r.enrollRequest.IsZero() {
		r.enrollRequest = request
	} else if r.enrollRequest != request {
		return client.Enrollment{}, client.ResponseObservation{}, ErrRecovery
	}
	if r.enrollErrOnce != nil {
		err := r.enrollErrOnce
		r.enrollErrOnce = nil
		return client.Enrollment{}, client.ResponseObservation{}, err
	}
	if r.loseEnrollOnce {
		r.loseEnrollOnce = false
		r.installation.State = 1
		r.installation.BotUser = &r.user
		r.installation.BotDevice = &d.DeviceID
		return client.Enrollment{}, client.ResponseObservation{}, errors.New("synthetic lost response")
	}
	r.installation.State = 2
	r.installation.Revision++
	r.installation.BotUser = &r.user
	r.installation.BotDevice = &d.DeviceID
	observation := client.ResponseObservation{ServerTime: 1_800_000_001, HasServerTime: true, Generation: 1, HasGeneration: true}
	if len(r.enrollObservations) > 0 {
		observation = r.enrollObservations[0]
		r.enrollObservations = r.enrollObservations[1:]
	}
	return client.Enrollment{BotUser: r.user, BotDevice: d.DeviceID, Username: "bot-45"}, observation, nil
}

type fakeIdentityHTTP struct {
	instance, user, device id.ID
	session                *fakeIdentitySession
	challengeCalls         int
	externalSenderPub      []byte
}

func TestDiscoveryExternalSenderKeyMustBePresentAndNonzero(t *testing.T) {
	for _, discovery := range []client.Discovery{
		{},
		{ExternalSenderPub: make([]byte, 32)},
		{ExternalSenderPub: make([]byte, 31)},
	} {
		if _, err := discoveryExternalSenderKey(discovery); err == nil {
			t.Fatalf("accepted missing, zero, or malformed key: %x", discovery.ExternalSenderPub)
		}
	}
	key := append([]byte{1}, make([]byte, 31)...)
	if got, err := discoveryExternalSenderKey(client.Discovery{ExternalSenderPub: key}); err != nil || got[0] != 1 {
		t.Fatalf("valid key: %x, %v", got, err)
	}
}

func (h *fakeIdentityHTTP) InstanceID() id.ID { return h.instance }
func (h *fakeIdentityHTTP) DiscoverObserved(context.Context) (client.Discovery, client.ResponseObservation, error) {
	key := h.externalSenderPub
	if key == nil {
		key = []byte{0x77, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	}
	return client.Discovery{InstanceID: h.instance, Generation: 1, WireVersions: []uint64{3}, E2EEVersions: []uint64{1}, MediaVersions: []uint64{1}, ExternalSenderPub: key}, client.ResponseObservation{ServerTime: 1_800_000_000, HasServerTime: true, Generation: 1, HasGeneration: true}, nil
}
func (*fakeIdentityHTTP) InstanceLimits(context.Context) (client.Limits, error) {
	return client.Limits{}, nil
}
func (h *fakeIdentityHTTP) Challenge(context.Context, id.ID) (client.SessionChallenge, error) {
	h.challengeCalls++
	return client.SessionChallenge{Nonce: make([]byte, 32), Expires: 1_800_000_060}, nil
}
func (h *fakeIdentityHTTP) EstablishSession(_ context.Context, device id.ID, nonce []byte, purpose uint64, _ []byte, _ any, _ []byte) (client.DeviceSession, error) {
	return client.DeviceSession{Token: "secret-bearer", Scope: 0, UserID: h.user, DeviceID: device, Expires: 1_800_001_000, IdleExpires: 1_800_001_000, Generation: 1}, nil
}
func (h *fakeIdentityHTTP) NewSession(string, id.ID) (lifecycleSession, error) { return h.session, nil }

type fakeIdentitySession struct {
	installation        client.Installation
	generation          uint64
	active              uint64
	fallback            bool
	list                []byte
	publicationCount    int
	receipt             client.KeyPackagePublicationReceipt
	publicationOps      []id.ID
	receiptErrOnce      bool
	publishErr          error
	receiptCalls        int
	runtime             *fakeLifecycleRuntime
	finalizeLostOnce    bool
	finalizeReceipt     *client.Installation
	finalizeRequests    []id.ID
	finalizeRevisions   []uint64
	publishErrOnce      bool
	ownListNotFoundOnce bool
	deviceListCalls     int
}

func (s *fakeIdentitySession) Negotiate(context.Context, []uint64, []uint64, []uint64) (client.Negotiated, error) {
	return client.Negotiated{Wire: 3, E2EE: 1, Media: 1}, nil
}
func (s *fakeIdentitySession) FinalizeCredential(_ context.Context, _, request id.ID, revision uint64, _ []byte) (client.Installation, error) {
	s.finalizeRequests = append(s.finalizeRequests, request)
	s.finalizeRevisions = append(s.finalizeRevisions, revision)
	if s.finalizeReceipt != nil {
		return *s.finalizeReceipt, nil
	}
	if s.finalizeLostOnce {
		s.finalizeLostOnce = false
		s.installation.State = 2
		s.installation.Revision++
		if s.runtime != nil {
			s.runtime.installation = s.installation
		}
		return client.Installation{}, errors.New("synthetic accepted credential with lost response")
	}
	if s.installation.State != 2 && s.installation.State != 3 {
		s.installation.State = 2
		s.installation.Revision++
	}
	if s.runtime != nil {
		s.runtime.installation = s.installation
	}
	return s.installation, nil
}
func (s *fakeIdentitySession) PublishDeviceList(_ context.Context, _ id.ID, version uint64, blob, signature, previousHash []byte) error {
	s.deviceListCalls++
	s.list, _ = cborx.Marshal([]any{version, blob, signature, previousHash})
	if s.publishErrOnce {
		s.publishErrOnce = false
		return errors.New("synthetic accepted device-list publish with lost response")
	}
	return nil
}
func (s *fakeIdentitySession) OwnDeviceList(context.Context, id.ID) ([]byte, error) {
	if s.ownListNotFoundOnce {
		s.ownListNotFoundOnce = false
		return nil, &client.APIError{Status: 404, Code: "E_NOT_FOUND", Outcome: client.OutcomeDefinitive}
	}
	return append([]byte(nil), s.list...), nil
}
func (s *fakeIdentitySession) KeyPackageInventory(context.Context, id.ID) (client.KeyPackageInventory, error) {
	generation := s.generation
	if generation == 0 {
		generation = 1
	}
	return client.KeyPackageInventory{Generation: generation, ServerTime: 1_800_000_010, OrdinaryActive: s.active, HasCurrentFallback: s.fallback}, nil
}
func (s *fakeIdentitySession) PublishKeyPackages(_ context.Context, _ id.ID, op id.ID, _ uint64, upload []byte) (uint64, error) {
	s.publicationCount++
	s.publicationOps = append(s.publicationOps, op)
	if s.publishErr != nil {
		return 0, s.publishErr
	}
	var rows []any
	if err := cborx.Unmarshal(upload, &rows); err != nil || len(rows) != 2 {
		return 0, ErrRecovery
	}
	ordinary, ok := rows[0].([]any)
	if !ok {
		return 0, ErrRecovery
	}
	s.active = uint64(len(ordinary))
	s.fallback = rows[1] != nil
	count := uint64(len(ordinary))
	if s.fallback {
		count++
	}
	s.receipt = client.KeyPackagePublicationReceipt{Generation: 1, Count: count, UploadSHA256: sha256.Sum256(upload)}
	return s.receipt.Count, nil
}
func (s *fakeIdentitySession) KeyPackagePublicationReceiptObserved(context.Context, id.ID, id.ID) (client.KeyPackagePublicationReceipt, client.ResponseObservation, error) {
	s.receiptCalls++
	if s.receiptErrOnce {
		s.receiptErrOnce = false
		return client.KeyPackagePublicationReceipt{}, client.ResponseObservation{}, errors.New("synthetic receipt read failure")
	}
	return s.receipt, client.ResponseObservation{}, nil
}

func TestIdentityEnsureReplaysLostEnrollmentWithSameRequestAndReturnsJoining(t *testing.T) {
	ctx := context.Background()
	instance, community, installation := sid(1), sid(2), sid(3)
	user, device := sid(45), sid(44)
	n := newFakeNative(instance, community, installation)
	r := &fakeLifecycleRuntime{installation: client.Installation{ID: installation, CommunityID: community, State: 0, Revision: 1}, user: user, device: device, loseEnrollOnce: true}
	list, _ := cborx.Marshal([]any{uint64(1), []byte("list"), make([]byte, 64), make([]byte, 32)})
	s := &fakeIdentitySession{installation: client.Installation{ID: installation, CommunityID: community, State: 2, Revision: 2, BotUser: &user, BotDevice: &device}, list: list, publishErrOnce: true, ownListNotFoundOnce: true}
	h := &fakeIdentityHTTP{instance: instance, user: user, device: device, session: s}
	service := newIdentityLifecycle(instance, community, installation, n, r, h, LifecycleOptions{Now: func() time.Time { return time.Unix(1_800_000_000, 0) }, DesiredPackages: 8, RenewMargin: time.Minute})
	if _, err := service.Ensure(ctx); err == nil {
		t.Fatal("expected simulated lost enroll response")
	}
	first, err := n.StateGet(ctx, identityStateArea, []byte(identityStateKey))
	if err != nil {
		t.Fatal(err)
	}
	firstState, err := decodeIdentityState(first.Value)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Ensure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Phase != 2 || result.Installation.State != 2 || result.Packages.OrdinaryActive != 8 {
		t.Fatalf("unexpected lifecycle result: %#v", result)
	}
	listCalls := s.deviceListCalls
	s.ownListNotFoundOnce = true // server-side rollback while the helper still has published=true.
	if _, err = service.Ensure(ctx); err != nil {
		t.Fatalf("Ensure did not repair restored-away v1 device list: %v", err)
	}
	if s.deviceListCalls != listCalls+1 || !n.published {
		t.Fatalf("restore reconciliation skipped exact native-head repair: before=%d after=%d nativePublished=%v", listCalls, s.deviceListCalls, n.published)
	}
	if r.enrollCalls != 2 || r.enrollRequest != firstState.EnrollRequest {
		t.Fatalf("enrollment did not replay the exact request: calls=%d", r.enrollCalls)
	}
	if n.completeCalls < 1 || !n.published || n.intents != 1 || n.resolved != 1 || s.publicationCount != 1 {
		t.Fatalf("lifecycle effects missing: complete=%d list=%v intents=%d resolved=%d publications=%d", n.completeCalls, n.published, n.intents, n.resolved, s.publicationCount)
	}
	if s.deviceListCalls != 2 {
		t.Fatalf("lost device-list response was not reconciled from the authenticated current head: calls=%d", s.deviceListCalls)
	}
	if n.bootstrapKey != ([32]byte{0x77}) {
		t.Fatalf("native bootstrap did not receive authenticated discovery key: %x", n.bootstrapKey)
	}
	h.externalSenderPub = append([]byte{0x78}, make([]byte, 31)...)
	if _, err = service.Ensure(ctx); err == nil {
		t.Fatal("ready Bot reconciliation accepted changed discovery key")
	}
	if got := fmt.Sprintf("%+v", result); got == "" || contains(got, "secret-bearer") {
		t.Fatalf("result formatting exposed bearer: %q", got)
	}
}

func TestFinalCredentialLostResponseReplaysOriginalRequestAndRevision(t *testing.T) {
	ctx := context.Background()
	instance, community, installation := sid(1), sid(2), sid(3)
	user, device := sid(45), sid(44)
	n := newFakeNative(instance, community, installation)
	r := &fakeLifecycleRuntime{installation: client.Installation{ID: installation, CommunityID: community, State: 0, Revision: 1}, user: user, device: device}
	list, _ := cborx.Marshal([]any{uint64(1), []byte("list"), make([]byte, 64), make([]byte, 32)})
	s := &fakeIdentitySession{installation: client.Installation{ID: installation, CommunityID: community, State: 2, Revision: 2, BotUser: &user, BotDevice: &device}, list: list, runtime: r, finalizeLostOnce: true}
	h := &fakeIdentityHTTP{instance: instance, user: user, device: device, session: s}
	l := newIdentityLifecycle(instance, community, installation, n, r, h, LifecycleOptions{Now: func() time.Time { return time.Unix(1_800_000_000, 0) }, DesiredPackages: 8})
	if _, err := l.Ensure(ctx); err == nil {
		t.Fatal("expected lost final credential response")
	}
	stored, err := n.StateGet(ctx, identityStateArea, []byte(identityStateKey))
	if err != nil {
		t.Fatal(err)
	}
	firstState, err := decodeIdentityState(stored.Value)
	if err != nil || firstState.FinalRequest.IsZero() || firstState.FinalRevision != 2 {
		t.Fatalf("credential request was not durable before call: %#v %v", firstState, err)
	}
	if result, err := l.Ensure(ctx); err != nil || result.Installation.State != 2 {
		t.Fatalf("credential replay failed: %#v %v", result, err)
	}
	if len(s.finalizeRequests) != 2 || s.finalizeRequests[0] != firstState.FinalRequest || s.finalizeRequests[1] != firstState.FinalRequest || s.finalizeRevisions[0] != 2 || s.finalizeRevisions[1] != 2 {
		t.Fatalf("credential retry changed exact request or revision: requests=%v revisions=%v", s.finalizeRequests, s.finalizeRevisions)
	}
}

func TestEnsureUsesCurrentInstallationAfterCredentialReceiptReplay(t *testing.T) {
	ctx := context.Background()
	instance, community, installation := sid(1), sid(2), sid(3)
	user, device := sid(45), sid(44)
	n := newFakeNative(instance, community, installation)
	r := &fakeLifecycleRuntime{installation: client.Installation{ID: installation, CommunityID: community, State: 0, Revision: 1}, user: user, device: device}
	list, _ := cborx.Marshal([]any{uint64(1), []byte("list"), make([]byte, 64), make([]byte, 32)})
	s := &fakeIdentitySession{installation: client.Installation{ID: installation, CommunityID: community, State: 2, Revision: 2, BotUser: &user, BotDevice: &device}, list: list, runtime: r}
	h := &fakeIdentityHTTP{instance: instance, user: user, device: device, session: s}
	l := newIdentityLifecycle(instance, community, installation, n, r, h, LifecycleOptions{Now: func() time.Time { return time.Unix(1_800_000_000, 0) }, DesiredPackages: 8})
	first, err := l.Ensure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	receipt := first.Installation
	s.finalizeReceipt = &receipt
	r.installation.Revision = receipt.Revision + 2
	result, err := l.Ensure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Installation.Revision != r.installation.Revision || result.Installation.State != 2 {
		t.Fatalf("credential receipt hid current installation: result=%+v current=%+v", result.Installation, r.installation)
	}
	if s.finalizeRevisions[len(s.finalizeRevisions)-1] != s.finalizeRevisions[0] {
		t.Fatal("credential replay changed its original expected revision")
	}
}

func TestExpiredClaimReplacementUsesServerReturnedClaimWithoutDate(t *testing.T) {
	ctx := context.Background()
	instance, community, installation := sid(1), sid(2), sid(3)
	user, device := sid(45), sid(44)
	n := newFakeNative(instance, community, installation)
	r := &fakeLifecycleRuntime{installation: client.Installation{ID: installation, CommunityID: community, State: 0, Revision: 1}, user: user, device: device, claimSequence: []client.Claim{{InstallationID: installation, ClaimID: sid(77), Expires: 1_800_000_600}, {InstallationID: installation, ClaimID: sid(78), Expires: 1_800_001_200}}}
	list, _ := cborx.Marshal([]any{uint64(1), []byte("list"), make([]byte, 64), make([]byte, 32)})
	s := &fakeIdentitySession{installation: client.Installation{ID: installation, CommunityID: community, State: 2, Revision: 2, BotUser: &user, BotDevice: &device}, list: list}
	h := &fakeIdentityHTTP{instance: instance, user: user, device: device, session: s}
	l := newIdentityLifecycle(instance, community, installation, n, r, h, LifecycleOptions{Now: func() time.Time { return time.Unix(9_999_999_999, 0) }, DesiredPackages: 8})
	if _, err := l.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	if r.claimCalls != 2 || r.enrollClaim != sid(78) {
		t.Fatalf("replacement claim was not persisted before enrollment: claims=%d enrollClaim=%s", r.claimCalls, r.enrollClaim)
	}
	stateValue, err := n.StateGet(ctx, identityStateArea, []byte(identityStateKey))
	if err != nil {
		t.Fatal(err)
	}
	state, err := decodeIdentityState(stateValue.Value)
	if err != nil || state.ClaimID != sid(78) || state.ClaimExpires != 1_800_001_200 {
		t.Fatalf("replacement claim not durable: %#v %v", state, err)
	}
}

func TestMismatchedAuthenticatedDeviceListHeadRefusesToMarkNativePublished(t *testing.T) {
	ctx := context.Background()
	instance, community, installation := sid(1), sid(2), sid(3)
	user, device := sid(45), sid(44)
	n := newFakeNative(instance, community, installation)
	r := &fakeLifecycleRuntime{installation: client.Installation{ID: installation, CommunityID: community, State: 0, Revision: 1}, user: user, device: device}
	list, _ := cborx.Marshal([]any{uint64(1), []byte("different-list-head"), make([]byte, 64), make([]byte, 32)})
	s := &fakeIdentitySession{installation: client.Installation{ID: installation, CommunityID: community, State: 2, Revision: 2, BotUser: &user, BotDevice: &device}, list: list}
	h := &fakeIdentityHTTP{instance: instance, user: user, device: device, session: s}
	l := newIdentityLifecycle(instance, community, installation, n, r, h, LifecycleOptions{Now: func() time.Time { return time.Unix(1_800_000_000, 0) }, DesiredPackages: 8})
	if _, err := l.Ensure(ctx); !errors.Is(err, ErrRecovery) {
		t.Fatalf("mismatched list head did not refuse lifecycle: %v", err)
	}
	if n.published || s.publicationCount != 0 {
		t.Fatalf("mismatched list head was accepted: nativePublished=%v kpPosts=%d", n.published, s.publicationCount)
	}
}

func TestDeviceListRepairRequiresExactNextHeadAfterRollback(t *testing.T) {
	i, c, installation := sid(1), sid(2), sid(3)
	n := newFakeNative(i, c, installation)
	l := newIdentityLifecycle(i, c, installation, n, &fakeLifecycleRuntime{}, &fakeIdentityHTTP{instance: i}, LifecycleOptions{})
	previousRaw, _ := cborx.Marshal([]any{uint64(1), []byte("old"), make([]byte, 64), make([]byte, 32)})
	previous, err := decodeDeviceList(previousRaw)
	if err != nil {
		t.Fatal(err)
	}
	previousHash := sha256.Sum256(previous.Blob)
	currentRaw, _ := cborx.Marshal([]any{uint64(2), []byte("new"), make([]byte, 64), previousHash[:]})
	current, err := decodeDeviceList(currentRaw)
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeIdentitySession{list: previousRaw}
	identity := native.Identity{DeviceListPublished: true}
	state := identityState{ListPublished: true}
	var revision uint64
	if err = l.reconcileDeviceList(context.Background(), s, sid(45), currentRaw, current, &identity, &state, &revision); err != nil {
		t.Fatalf("exact next native head should repair a one-version rollback: %v", err)
	}
	if s.deviceListCalls != 1 || !equalBytes(s.list, currentRaw) || !identity.DeviceListPublished {
		t.Fatalf("exact next-head repair failed: publishes=%d listMatches=%v flag=%v", s.deviceListCalls, equalBytes(s.list, currentRaw), identity.DeviceListPublished)
	}

	// A missing server list after the native helper has advanced beyond version one
	// cannot be safely reconstructed from its current head alone.
	s = &fakeIdentitySession{ownListNotFoundOnce: true}
	if err = l.reconcileDeviceList(context.Background(), s, sid(45), currentRaw, current, &identity, &state, &revision); !errors.Is(err, ErrRecovery) {
		t.Fatalf("unrepairable rollback was accepted: %v", err)
	}
	if s.deviceListCalls != 0 {
		t.Fatalf("unrepairable rollback attempted publication %d times", s.deviceListCalls)
	}
}

func TestMissingEnrollmentDateDoesNotSettleUncertainEnrollment(t *testing.T) {
	ctx := context.Background()
	instance, community, installation := sid(1), sid(2), sid(3)
	user, device := sid(45), sid(44)
	n := newFakeNative(instance, community, installation)
	r := &fakeLifecycleRuntime{installation: client.Installation{ID: installation, CommunityID: community, State: 0, Revision: 1}, user: user, device: device, enrollObservations: []client.ResponseObservation{{Generation: 1, HasGeneration: true}, {ServerTime: 1_800_000_001, HasServerTime: true, Generation: 1, HasGeneration: true}}}
	list, _ := cborx.Marshal([]any{uint64(1), []byte("list"), make([]byte, 64), make([]byte, 32)})
	s := &fakeIdentitySession{installation: client.Installation{ID: installation, CommunityID: community, State: 2, Revision: 2, BotUser: &user, BotDevice: &device}, list: list}
	h := &fakeIdentityHTTP{instance: instance, user: user, device: device, session: s}
	l := newIdentityLifecycle(instance, community, installation, n, r, h, LifecycleOptions{Now: func() time.Time { return time.Unix(9_999_999_999, 0) }, DesiredPackages: 8})
	if _, err := l.Ensure(ctx); !errors.Is(err, ErrRecovery) {
		t.Fatalf("missing authoritative completion time should suspend: %v", err)
	}
	stored, err := n.StateGet(ctx, identityStateArea, []byte(identityStateKey))
	if err != nil {
		t.Fatal(err)
	}
	first, err := decodeIdentityState(stored.Value)
	if err != nil || !first.EnrollAttempted || !first.BotUser.IsZero() {
		t.Fatalf("uncertain enrollment state was incorrectly settled: %#v %v", first, err)
	}
	if _, err = l.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	final, _, err := l.loadState(ctx)
	if err != nil || final.BotUser != user || final.CompleteAt != 1_800_000_001 || r.enrollCalls != 2 || first.EnrollRequest != final.EnrollRequest {
		t.Fatalf("exact enrollment replay did not recover with authenticated Date: state=%#v calls=%d err=%v", final, r.enrollCalls, err)
	}
}

func TestNativeCompletionReplayKeepsOriginalTimestampAcrossReopen(t *testing.T) {
	ctx := context.Background()
	instance, community, installation := sid(1), sid(2), sid(3)
	user, device := sid(45), sid(44)
	n := newFakeNative(instance, community, installation)
	n.completeErrOnce = true
	r := &fakeLifecycleRuntime{installation: client.Installation{ID: installation, CommunityID: community, State: 0, Revision: 1}, user: user, device: device}
	list, _ := cborx.Marshal([]any{uint64(1), []byte("list"), make([]byte, 64), make([]byte, 32)})
	s := &fakeIdentitySession{installation: client.Installation{ID: installation, CommunityID: community, State: 2, Revision: 2, BotUser: &user, BotDevice: &device}, list: list}
	h := &fakeIdentityHTTP{instance: instance, user: user, device: device, session: s}
	l := newIdentityLifecycle(instance, community, installation, n, r, h, LifecycleOptions{Now: func() time.Time { return time.Unix(1_800_000_000, 0) }, DesiredPackages: 8})
	if _, err := l.Ensure(ctx); err == nil {
		t.Fatal("expected lost native completion response")
	}
	stored, err := n.StateGet(ctx, identityStateArea, []byte(identityStateKey))
	if err != nil {
		t.Fatal(err)
	}
	first, err := decodeIdentityState(stored.Value)
	if err != nil || first.CompleteAt != 1_800_000_001 || first.BotUser != user {
		t.Fatalf("completion timestamp/account were not durable before native call: %#v %v", first, err)
	}
	// Reconstruct the coordinator around the same native/helper, encrypted state
	// and HTTP clients, as a supervisor restart would.
	l = newIdentityLifecycle(instance, community, installation, n, r, h, LifecycleOptions{Now: func() time.Time { return time.Unix(9_999_999_999, 0) }, DesiredPackages: 8})
	if _, err = l.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	if len(n.completeTimes) < 2 || n.completeTimes[0] != first.CompleteAt || n.completeTimes[1] != first.CompleteAt {
		t.Fatalf("native completion retry changed timestamp: %v", n.completeTimes)
	}
}

func TestCompletedNativeBotWithChangedRemoteDeviceRefusesBeforeSessionMint(t *testing.T) {
	ctx := context.Background()
	instance, community, installation := sid(1), sid(2), sid(3)
	user, device := sid(45), sid(44)
	n := newFakeNative(instance, community, installation)
	n.phase = 2
	changedDevice := sid(46)
	r := &fakeLifecycleRuntime{installation: client.Installation{ID: installation, CommunityID: community, State: 2, Revision: 2, BotUser: &user, BotDevice: &changedDevice}, user: user, device: changedDevice}
	s := &fakeIdentitySession{}
	h := &fakeIdentityHTTP{instance: instance, user: user, device: device, session: s}
	l := newIdentityLifecycle(instance, community, installation, n, r, h, LifecycleOptions{DesiredPackages: 8})
	fingerprint, err := bootstrapFingerprint(n.bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	st := identityState{Initialized: true, ClaimRequest: sid(4), EnrollRequest: sid(5), BotUser: user, BotDevice: device, Username: n.username, CompleteAt: 1_800_000_001, BootstrapRequested: true, PendingDevice: device, BootstrapFingerprint: fingerprint}
	if _, err = l.saveState(ctx, 0, st); err != nil {
		t.Fatal(err)
	}
	if _, err = l.Ensure(ctx); !errors.Is(err, ErrRecovery) {
		t.Fatalf("changed remote device was accepted: %v", err)
	}
	if h.challengeCalls != 0 {
		t.Fatalf("session challenge minted before tuple validation: %d", h.challengeCalls)
	}
}

func contains(a, b string) bool { return len(b) == 0 || len(a) >= len(b) && index(a, b) >= 0 }
func index(a, b string) int {
	for i := 0; i+len(b) <= len(a); i++ {
		if a[i:i+len(b)] == b {
			return i
		}
	}
	return -1
}

func TestIdentityStateTransactionIDIsStableAcrossExactReplay(t *testing.T) {
	i, c, installation := sid(1), sid(2), sid(3)
	n := newFakeNative(i, c, installation)
	l := newIdentityLifecycle(i, c, installation, n, &fakeLifecycleRuntime{}, &fakeIdentityHTTP{instance: i}, LifecycleOptions{})
	st := identityState{Initialized: true, ClaimRequest: sid(4), EnrollRequest: sid(5)}
	if _, err := l.saveState(context.Background(), 0, st); err != nil {
		t.Fatal(err)
	}
	want := append([]byte(nil), n.state...)
	decoded, err := decodeIdentityState(want)
	if err != nil || decoded.EnrollRequest != st.EnrollRequest {
		t.Fatalf("persisted state did not roundtrip: %v", err)
	}
	// The transaction id is derived from canonical value bytes; equivalent replay
	// after helper reopen therefore has the same native receipt identity.
	h1 := sha256.Sum256(append(append([]byte("dilla.bot.identity.state.tx.v1"), []byte(identityStateKey)...), want...))
	if _, err = l.saveState(context.Background(), n.stateRevision, st); err != nil {
		t.Fatal(err)
	}
	h2 := sha256.Sum256(append(append([]byte("dilla.bot.identity.state.tx.v1"), []byte(identityStateKey)...), want...))
	if h1 != h2 {
		t.Fatal("stable state transaction identity changed")
	}
}

func TestEnsureReopensAfterCommittedProgressWriteReplyLoss(t *testing.T) {
	ctx := context.Background()
	instance, community, installation := sid(1), sid(2), sid(3)
	user, device := sid(45), sid(44)
	n := newFakeNative(instance, community, installation)
	n.stateErrOnce = true
	r := &fakeLifecycleRuntime{installation: client.Installation{ID: installation, CommunityID: community, State: 0, Revision: 1}, user: user, device: device}
	list, _ := cborx.Marshal([]any{uint64(1), []byte("list"), make([]byte, 64), make([]byte, 32)})
	s := &fakeIdentitySession{installation: client.Installation{ID: installation, CommunityID: community, State: 2, Revision: 2, BotUser: &user, BotDevice: &device}, list: list}
	h := &fakeIdentityHTTP{instance: instance, user: user, device: device, session: s}
	l := newIdentityLifecycle(instance, community, installation, n, r, h, LifecycleOptions{DesiredPackages: 8})
	if _, err := l.Ensure(ctx); err == nil {
		t.Fatal("expected lost helper progress-write response")
	}
	committed, err := n.StateGet(ctx, identityStateArea, []byte(identityStateKey))
	if err != nil {
		t.Fatal(err)
	}
	first, err := decodeIdentityState(committed.Value)
	if err != nil || first.ClaimRequest.IsZero() || first.EnrollRequest.IsZero() {
		t.Fatalf("initial operations were not committed before side effects: %#v %v", first, err)
	}
	l = newIdentityLifecycle(instance, community, installation, n, r, h, LifecycleOptions{DesiredPackages: 8})
	if _, err = l.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	final, _, err := l.loadState(ctx)
	if err != nil || final.ClaimRequest != first.ClaimRequest || final.EnrollRequest != first.EnrollRequest || r.enrollCalls != 1 {
		t.Fatalf("reopen changed operation IDs or duplicated enrollment: initial=%#v final=%#v calls=%d err=%v", first, final, r.enrollCalls, err)
	}
}

func TestKeyPackageObsoleteUsesAuthenticatedInventoryNotLocalClock(t *testing.T) {
	for _, tc := range []struct {
		name       string
		generation uint64
		serverTime uint64
		wantNow    uint64
	}{
		{name: "expiry boundary", generation: 1, serverTime: 1_900_000_000, wantNow: 1_900_000_000},
		{name: "generation advanced", generation: 2, serverTime: 1_800_000_010, wantNow: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i, c, installation := sid(1), sid(2), sid(3)
			n := newFakeNative(i, c, installation)
			if tc.name == "expiry boundary" {
				n.greatestExpiry = tc.serverTime
			}
			s := &fakeIdentitySession{}
			l := newIdentityLifecycle(i, c, installation, n, &fakeLifecycleRuntime{}, &fakeIdentityHTTP{instance: i, session: s}, LifecycleOptions{Now: func() time.Time { return time.Unix(9_999_999_999, 0) }, DesiredPackages: 8})
			st := identityState{Initialized: true, ClaimRequest: sid(4), EnrollRequest: sid(5), KeyPackageOperation: sid(6), KeyPackageCount: 8, KeyPackageGeneration: 1}
			rev, err := l.saveState(context.Background(), 0, st)
			if err != nil {
				t.Fatal(err)
			}
			inventory := client.KeyPackageInventory{Generation: tc.generation, ServerTime: tc.serverTime, OrdinaryActive: 0}
			if _, err = l.ensureKeyPackages(context.Background(), s, &st, &rev, inventory); err != nil {
				t.Fatal(err)
			}
			if n.lastResolution.Outcome != 1 || n.lastResolution.Now != tc.wantNow || n.lastResolution.CurrentGeneration != tc.generation {
				t.Fatalf("unexpected obsolete proof: %#v", n.lastResolution)
			}
			if s.publicationCount != 0 {
				t.Fatalf("obsolete intent was published %d times", s.publicationCount)
			}
		})
	}
}

func TestEnsureClearsCapacityDeferredAfterObsoleteIntentResolution(t *testing.T) {
	ctx := context.Background()
	instance, community, installation := sid(1), sid(2), sid(3)
	user, device := sid(45), sid(44)
	n := newFakeNative(instance, community, installation)
	r := &fakeLifecycleRuntime{installation: client.Installation{ID: installation, CommunityID: community, State: 0, Revision: 1}, user: user, device: device}
	list, _ := cborx.Marshal([]any{uint64(1), []byte("list"), make([]byte, 64), make([]byte, 32)})
	s := &fakeIdentitySession{installation: client.Installation{ID: installation, CommunityID: community, State: 2, Revision: 2, BotUser: &user, BotDevice: &device}, list: list}
	h := &fakeIdentityHTTP{instance: instance, user: user, device: device, session: s}
	l := newIdentityLifecycle(instance, community, installation, n, r, h, LifecycleOptions{DesiredPackages: 8})
	if _, err := l.Ensure(ctx); err != nil {
		t.Fatalf("failed to establish lifecycle fixture: %v", err)
	}
	st, revision, err := l.loadState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st.KeyPackageOperation, st.KeyPackageCount, st.KeyPackageFallback = sid(6), 1, 1
	st.KeyPackageGeneration, st.KeyPackageDone, st.KeyPackageCapacityDeferred = 1, false, true
	st.KeyPackageResolvePending = false
	intentsBefore, resolvedBefore, publicationsBefore := n.intents, n.resolved, s.publicationCount
	if _, err = l.saveState(ctx, revision, st); err != nil {
		t.Fatal(err)
	}
	s.generation, s.active, s.fallback = 2, 32, true
	if _, err = l.Ensure(ctx); err != nil {
		t.Fatalf("obsolete intent resolution failed: %v", err)
	}
	result, err := l.Ensure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	final, _, err := l.loadState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.PackageRefillPending || final.KeyPackageCapacityDeferred {
		t.Fatalf("resolved obsolete intent left refill falsely pending: result=%#v state=%#v", result, final)
	}
	if !final.KeyPackageDone || n.intents != intentsBefore+1 || n.resolved != resolvedBefore+1 || s.publicationCount != publicationsBefore || n.lastResolution.OperationID != sid(6) || len(n.lastResolution.PackageRefs) != 2 {
		t.Fatalf("obsolete intent was not resolved once with its native material preserved: state=%#v intents=%d resolved=%d posts=%d resolution=%#v", final, n.intents, n.resolved, s.publicationCount, n.lastResolution)
	}
}

func TestKeyPackageResolveReplayKeepsExactIntentRefsAndOutcome(t *testing.T) {
	i, c, installation := sid(1), sid(2), sid(3)
	n := newFakeNative(i, c, installation)
	n.resolveErrOnce = true
	s := &fakeIdentitySession{}
	l := newIdentityLifecycle(i, c, installation, n, &fakeLifecycleRuntime{}, &fakeIdentityHTTP{instance: i, session: s}, LifecycleOptions{DesiredPackages: 8})
	st := identityState{Initialized: true, ClaimRequest: sid(4), EnrollRequest: sid(5)}
	rev, err := l.saveState(context.Background(), 0, st)
	if err != nil {
		t.Fatal(err)
	}
	inventory := client.KeyPackageInventory{Generation: 1, ServerTime: 1_800_000_010}
	if _, err = l.ensureKeyPackages(context.Background(), s, &st, &rev, inventory); err == nil {
		t.Fatal("expected uncertain native resolution")
	}
	persisted, rev, err := l.loadState(context.Background())
	if err != nil || !persisted.KeyPackageResolvePending {
		t.Fatalf("pending resolution was not durable: %#v %v", persisted, err)
	}
	wantRefs := append([][32]byte(nil), persisted.KeyPackageRefs...)
	wantResolution := protocol.KeyPackageResolution{OperationID: persisted.KeyPackageOperation, PackageRefs: wantRefs, Outcome: persisted.KeyPackageResolveOutcome, Now: persisted.KeyPackageResolveNow, CurrentGeneration: persisted.KeyPackageResolveGeneration}
	intentBefore, postsBefore := n.intents, s.publicationCount
	if _, err = l.ensureKeyPackages(context.Background(), s, &persisted, &rev, inventory); err != nil {
		t.Fatal(err)
	}
	if n.intents != intentBefore || s.publicationCount != postsBefore || n.lastResolution.OperationID != wantResolution.OperationID || n.lastResolution.Outcome != wantResolution.Outcome || n.lastResolution.Now != wantResolution.Now || n.lastResolution.CurrentGeneration != wantResolution.CurrentGeneration || !reflect.DeepEqual(n.lastResolution.PackageRefs, wantRefs) {
		t.Fatalf("resolution retry changed durable inputs: %#v", n.lastResolution)
	}
	final, _, err := l.loadState(context.Background())
	if err != nil || final.KeyPackageResolvePending || !final.KeyPackageDone {
		t.Fatalf("resolution completion not persisted: %#v %v", final, err)
	}
}

func TestKeyPackageMissingReceiptRepeatsExactPublicationThenValidatesCount(t *testing.T) {
	i, c, installation := sid(1), sid(2), sid(3)
	n := newFakeNative(i, c, installation)
	s := &fakeIdentitySession{receiptErrOnce: true}
	l := newIdentityLifecycle(i, c, installation, n, &fakeLifecycleRuntime{}, &fakeIdentityHTTP{instance: i, session: s}, LifecycleOptions{DesiredPackages: 8})
	st := identityState{Initialized: true, ClaimRequest: sid(4), EnrollRequest: sid(5)}
	rev, err := l.saveState(context.Background(), 0, st)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = l.ensureKeyPackages(context.Background(), s, &st, &rev, client.KeyPackageInventory{Generation: 1, ServerTime: 1_800_000_010}); err != nil {
		t.Fatal(err)
	}
	if s.publicationCount != 2 || len(s.publicationOps) != 2 || s.publicationOps[0] != s.publicationOps[1] {
		t.Fatalf("publication retry changed operation identity: calls=%d ops=%v", s.publicationCount, s.publicationOps)
	}
	if n.resolved != 1 || n.lastResolution.Outcome != 0 {
		t.Fatalf("accepted receipt did not resolve accepted intent: %#v", n.lastResolution)
	}
}

func TestKeyPackageDefinitiveCapacityRefusalLeavesIntentPending(t *testing.T) {
	i, c, installation := sid(1), sid(2), sid(3)
	n := newFakeNative(i, c, installation)
	s := &fakeIdentitySession{publishErr: &client.APIError{Code: "E_TOO_LARGE", Outcome: client.OutcomeDefinitive}}
	l := newIdentityLifecycle(i, c, installation, n, &fakeLifecycleRuntime{}, &fakeIdentityHTTP{instance: i, session: s}, LifecycleOptions{DesiredPackages: 8})
	st := identityState{Initialized: true, ClaimRequest: sid(4), EnrollRequest: sid(5), KeyPackageOperation: sid(6), KeyPackageCount: 1, KeyPackageFallback: 1, KeyPackageGeneration: 1}
	rev, err := l.saveState(context.Background(), 0, st)
	if err != nil {
		t.Fatal(err)
	}
	inventory := client.KeyPackageInventory{Generation: 1, ServerTime: 1_800_000_010, OrdinaryActive: 32}
	deferred, err := l.ensureKeyPackages(context.Background(), s, &st, &rev, inventory)
	if err != nil || !deferred {
		t.Fatalf("capacity refusal did not defer while preserving usable ordinary packages: deferred=%v err=%v", deferred, err)
	}
	pending, _, loadErr := l.loadState(context.Background())
	if loadErr != nil || pending.KeyPackageOperation != sid(6) || pending.KeyPackageCount != 1 || pending.KeyPackageFallback != 1 || !pending.KeyPackageCapacityDeferred || pending.KeyPackageDone || pending.KeyPackageResolvePending || n.resolved != 0 || s.publicationCount != 1 || s.receiptCalls != 0 {
		t.Fatalf("definitive refusal did not leave the exact intent pending: %#v resolved=%d posts=%d receiptReads=%d err=%v", pending, n.resolved, s.publicationCount, s.receiptCalls, loadErr)
	}
	deferred, err = l.ensureKeyPackages(context.Background(), s, &pending, &rev, inventory)
	if err != nil || !deferred || s.publicationCount != 1 || n.lastIntent.OperationID != sid(6) {
		t.Fatalf("full-cap retry repeated impossible upload or lost the exact intent: deferred=%v posts=%d intent=%#v err=%v", deferred, s.publicationCount, n.lastIntent, err)
	}
}

func TestFullOrdinaryInventoryDefersFallbackWithoutCreatingImpossibleIntent(t *testing.T) {
	i, c, installation := sid(1), sid(2), sid(3)
	n := newFakeNative(i, c, installation)
	s := &fakeIdentitySession{active: 32, fallback: false}
	l := newIdentityLifecycle(i, c, installation, n, &fakeLifecycleRuntime{}, &fakeIdentityHTTP{instance: i, session: s}, LifecycleOptions{DesiredPackages: 8})
	st := identityState{Initialized: true, ClaimRequest: sid(4), EnrollRequest: sid(5)}
	rev, err := l.saveState(context.Background(), 0, st)
	if err != nil {
		t.Fatal(err)
	}
	deferred, err := l.ensureKeyPackages(context.Background(), s, &st, &rev, client.KeyPackageInventory{Generation: 1, ServerTime: 1_800_000_010, OrdinaryActive: 32})
	if err != nil || !deferred || n.intents != 0 || !st.KeyPackageOperation.IsZero() || s.publicationCount != 0 {
		t.Fatalf("fallback-only refill created an impossible op5 batch: deferred=%v intent=%#v ops=%d posts=%d err=%v", deferred, st.KeyPackageOperation, n.intents, s.publicationCount, err)
	}
}

func TestEnsureReturnsJoiningWithPendingFallbackAtOrdinaryPackageCap(t *testing.T) {
	ctx := context.Background()
	instance, community, installation := sid(1), sid(2), sid(3)
	user, device := sid(45), sid(44)
	n := newFakeNative(instance, community, installation)
	r := &fakeLifecycleRuntime{installation: client.Installation{ID: installation, CommunityID: community, State: 0, Revision: 1}, user: user, device: device}
	list, _ := cborx.Marshal([]any{uint64(1), []byte("list"), make([]byte, 64), make([]byte, 32)})
	s := &fakeIdentitySession{installation: client.Installation{ID: installation, CommunityID: community, State: 2, Revision: 2, BotUser: &user, BotDevice: &device}, active: 32, list: list}
	h := &fakeIdentityHTTP{instance: instance, user: user, device: device, session: s}
	l := newIdentityLifecycle(instance, community, installation, n, r, h, LifecycleOptions{DesiredPackages: 8})
	result, err := l.Ensure(ctx)
	if err != nil || result.Phase != 2 || !result.PackageRefillPending || result.Packages.OrdinaryActive != 32 || n.intents != 0 {
		t.Fatalf("full ordinary inventory withheld usable joining result: phase=%d pending=%v active=%d intents=%d err=%v", result.Phase, result.PackageRefillPending, result.Packages.OrdinaryActive, n.intents, err)
	}
}

func TestEnrollmentYieldClearsAttemptAndRefreshesClaim(t *testing.T) {
	instance, community, installation := sid(1), sid(2), sid(3)
	user, device := sid(45), sid(44)
	n := newFakeNative(instance, community, installation)
	r := &fakeLifecycleRuntime{installation: client.Installation{ID: installation, CommunityID: community, State: 0, Revision: 1}, user: user, device: device, enrollErrOnce: client.ErrCooperativeStop}
	list, _ := cborx.Marshal([]any{uint64(1), []byte("list"), make([]byte, 64), make([]byte, 32)})
	s := &fakeIdentitySession{installation: client.Installation{ID: installation, CommunityID: community, State: 2, Revision: 2, BotUser: &user, BotDevice: &device}, list: list}
	h := &fakeIdentityHTTP{instance: instance, user: user, device: device, session: s}
	lifecycle := newIdentityLifecycle(instance, community, installation, n, r, h, LifecycleOptions{Now: func() time.Time { return time.Unix(1_800_000_000, 0) }, DesiredPackages: 8, RenewMargin: time.Minute})
	if _, err := lifecycle.Ensure(t.Context()); !errors.Is(err, client.ErrCooperativeStop) {
		t.Fatal(err)
	}
	state, err := decodeIdentityState(n.state)
	if err != nil {
		t.Fatal(err)
	}
	if state.EnrollAttempted {
		t.Fatal("yield left enrollment marked as attempted")
	}
	oldRequest := state.EnrollRequest
	beforeClaims := r.claimCalls
	r.claimSequence = []client.Claim{{InstallationID: installation, ClaimID: sid(78), Expires: 1_800_000_900}}
	if _, err := lifecycle.Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	state, err = decodeIdentityState(n.state)
	if err != nil {
		t.Fatal(err)
	}
	if r.claimCalls <= beforeClaims || r.enrollClaim != sid(78) || state.EnrollRequest != oldRequest {
		t.Fatalf("retry did not refresh claim and retain request: claims=%d claim=%s", r.claimCalls, r.enrollClaim)
	}
}
