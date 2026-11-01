// Package service coordinates one installation's durable native bot identity
// lifecycle. It owns no private key or bearer persistence: those remain in the
// encrypted helper store. A Lifecycle is bound to one helper, runtime token,
// pinned instance and installation for its whole lifetime.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

const (
	identityStateArea = 5
	identityStateKey  = "dilla.bot.identity.lifecycle.v1"
	identityStateVer  = 2
	identityMaxPages  = 1000
	identityPageSize  = 100
)

var ErrRecovery = errors.New("bot identity lifecycle requires operator recovery")

type lifecycleNative interface {
	Identity(context.Context) (native.Identity, error)
	Bootstrap(context.Context, [32]byte) (native.BootstrapResult, error)
	CompleteEnrollment(context.Context, id.ID, string, uint64) (native.EnrollmentResult, error)
	SignSession(context.Context, [32]byte, uint64) (native.SessionSignature, error)
	StoreSession(context.Context, string, uint64, uint64) error
	Session(context.Context) (native.Session, error)
	ClearSession(context.Context) error
	DeviceList(context.Context) ([]byte, error)
	DeviceListPublished(context.Context) error
	KeyPackageIntent(context.Context, protocol.KeyPackageIntent) (protocol.KeyPackageIntentResult, error)
	ResolveKeyPackageIntent(context.Context, protocol.KeyPackageResolution) error
	StateGet(context.Context, uint64, []byte) (native.StateValue, error)
	StateTransaction(context.Context, id.ID, []native.StateChange) ([]uint64, error)
}

type lifecycleRuntime interface {
	InstanceID() id.ID
	Origin() string
	ListInstallationsPage(context.Context, *id.ID) ([]client.Installation, error)
	ClaimObserved(context.Context, id.ID, id.ID) (client.Claim, client.ResponseObservation, error)
	EnrollObserved(context.Context, id.ID, id.ID, id.ID, []byte, []byte, []byte, client.DeviceRegistration) (client.Enrollment, client.ResponseObservation, error)
}

type lifecycleSession interface {
	Negotiate(context.Context, []uint64, []uint64, []uint64) (client.Negotiated, error)
	FinalizeCredential(context.Context, id.ID, id.ID, uint64, []byte) (client.Installation, error)
	PublishDeviceList(context.Context, id.ID, uint64, []byte, []byte, []byte) error
	OwnDeviceList(context.Context, id.ID) ([]byte, error)
	KeyPackageInventory(context.Context, id.ID) (client.KeyPackageInventory, error)
	PublishKeyPackages(context.Context, id.ID, id.ID, uint64, []byte) (uint64, error)
	KeyPackagePublicationReceiptObserved(context.Context, id.ID, id.ID) (client.KeyPackagePublicationReceipt, client.ResponseObservation, error)
}

type lifecycleHTTP interface {
	InstanceID() id.ID
	DiscoverObserved(context.Context) (client.Discovery, client.ResponseObservation, error)
	InstanceLimits(context.Context) (client.Limits, error)
	Challenge(context.Context, id.ID) (client.SessionChallenge, error)
	EstablishSession(context.Context, id.ID, []byte, uint64, []byte, any, []byte) (client.DeviceSession, error)
	NewSession(string, id.ID) (lifecycleSession, error)
}

type productionIdentityHTTP struct{ c *client.Client }

func (h productionIdentityHTTP) InstanceID() id.ID { return h.c.InstanceID() }
func (h productionIdentityHTTP) DiscoverObserved(ctx context.Context) (client.Discovery, client.ResponseObservation, error) {
	return h.c.DiscoverObserved(ctx)
}
func (h productionIdentityHTTP) InstanceLimits(ctx context.Context) (client.Limits, error) {
	return h.c.InstanceLimits(ctx)
}
func (h productionIdentityHTTP) Challenge(ctx context.Context, device id.ID) (client.SessionChallenge, error) {
	return h.c.Challenge(ctx, device)
}
func (h productionIdentityHTTP) EstablishSession(ctx context.Context, device id.ID, nonce []byte, purpose uint64, signature []byte, registration any, login []byte) (client.DeviceSession, error) {
	return h.c.EstablishSession(ctx, device, nonce, purpose, signature, registration, login)
}
func (h productionIdentityHTTP) NewSession(token string, device id.ID) (lifecycleSession, error) {
	return h.c.SessionForDevice(token, device)
}

// LifecycleOptions controls caller-bounded retries and testable session renewal.
// Ensure performs one pass and never sleeps or retries internally.
type LifecycleOptions struct {
	Now             func() time.Time
	DesiredPackages uint64
	RenewMargin     time.Duration
}

// LifecycleResult contains no private key or persisted bearer. Session is
// intentionally explicit access to the current scoped device session.
type LifecycleResult struct {
	Phase                uint64
	Installation         client.Installation
	Session              *client.Session
	Packages             client.KeyPackageInventory
	PackageRefillPending bool
}

func (r LifecycleResult) String() string {
	return fmt.Sprintf("LifecycleResult{phase:%d installation:%s session:[redacted] packages:%d}", r.Phase, r.Installation.ID, r.Packages.OrdinaryActive)
}
func (r LifecycleResult) GoString() string { return r.String() }

type Lifecycle struct {
	mu          sync.Mutex
	instance    id.ID
	community   id.ID
	install     id.ID
	native      lifecycleNative
	runtime     lifecycleRuntime
	http        lifecycleHTTP
	now         func() time.Time
	desired     uint64
	renewMargin time.Duration
}

// NewIdentityLifecycle binds the real reviewed native, runtime and HTTP
// clients. The caller owns their process lifecycle; this value serializes all
// identity mutations for the bound installation.
func NewIdentityLifecycle(instance, community, installation id.ID, n *native.Client, runtime *client.Runtime, httpClient *client.Client, options LifecycleOptions) (*Lifecycle, error) {
	wantBinding := native.Binding{Instance: instance, Community: community, Installation: installation}
	if instance.IsZero() || community.IsZero() || installation.IsZero() || n == nil || runtime == nil || httpClient == nil || httpClient.InstanceID() != instance || runtime.InstanceID() != instance || runtime.Origin() != httpClient.Origin() || n.Binding() != wantBinding {
		return nil, errors.New("invalid bot identity lifecycle binding")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.DesiredPackages == 0 {
		options.DesiredPackages = 8
	}
	if options.DesiredPackages > 32 {
		return nil, errors.New("invalid desired KeyPackage count")
	}
	if options.RenewMargin <= 0 {
		options.RenewMargin = 2 * time.Minute
	}
	return newIdentityLifecycle(instance, community, installation, n, runtime, productionIdentityHTTP{httpClient}, options), nil
}

func newIdentityLifecycle(instance, community, installation id.ID, n lifecycleNative, runtime lifecycleRuntime, httpClient lifecycleHTTP, options LifecycleOptions) *Lifecycle {
	return &Lifecycle{instance: instance, community: community, install: installation, native: n, runtime: runtime, http: httpClient, now: options.Now, desired: options.DesiredPackages, renewMargin: options.RenewMargin}
}

func discoveryExternalSenderKey(discovery client.Discovery) ([32]byte, error) {
	var key [32]byte
	if len(discovery.ExternalSenderPub) != len(key) || allZeroBytes(discovery.ExternalSenderPub) {
		return key, errors.New("bot identity: discovery has no valid external-sender key")
	}
	copy(key[:], discovery.ExternalSenderPub)
	return key, nil
}

// Ensure reconciles one bounded pass of the identity state machine. It returns
// a usable ordinary device session only after final credential, signed device
// list and KeyPackage publication are reconciled. Group readiness remains D2's
// responsibility; the returned server phase is normally joining.
func (l *Lifecycle) Ensure(ctx context.Context) (LifecycleResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out LifecycleResult
	if ctx == nil {
		ctx = context.Background()
	}
	state, revision, err := l.loadState(ctx)
	if err != nil {
		return out, err
	}
	if !state.Initialized {
		state = identityState{Initialized: true, ClaimRequest: id.New(), EnrollRequest: id.New()}
		revision, err = l.saveState(ctx, revision, state)
		if err != nil {
			return out, err
		}
	}
	discovery, _, err := l.http.DiscoverObserved(ctx)
	if err != nil {
		return out, err
	}
	if discovery.InstanceID != l.instance {
		return out, client.ErrInstanceChanged
	}
	externalSenderKey, keyErr := discoveryExternalSenderKey(discovery)
	if keyErr != nil {
		return out, keyErr
	}
	inst, err := l.findInstallation(ctx)
	if err != nil {
		return out, err
	}
	if inst.CommunityID != l.community {
		return out, ErrRecovery
	}
	if inst.State > 5 || inst.State == 4 || inst.State == 5 {
		return out, fmt.Errorf("bot installation state %d refuses identity actions", inst.State)
	}
	identity, err := l.native.Identity(ctx)
	if err != nil {
		return out, err
	}
	if identity.Instance != nil && *identity.Instance != l.instance || identity.Device != nil && !state.BotDevice.IsZero() && *identity.Device != state.BotDevice {
		return out, ErrRecovery
	}
	if identity.Phase == 1 || identity.Phase == 3 {
		return out, fmt.Errorf("%w: native identity is in human signup phase %d", ErrRecovery, identity.Phase)
	}
	var boot native.BootstrapResult
	hasBootstrap := false
	switch identity.Phase {
	case 0:
		if (inst.BotUser != nil || inst.BotDevice != nil) && state.PendingDevice.IsZero() {
			return out, fmt.Errorf("%w: server Bot is enrolled while local pending identity proof is missing", ErrRecovery)
		}
		if !state.BootstrapRequested {
			state.BootstrapRequested = true
			revision, err = l.saveState(ctx, revision, state)
			if err != nil {
				return out, err
			}
		}
		boot, err = l.native.Bootstrap(ctx, externalSenderKey)
		if err != nil {
			return out, err
		}
		hasBootstrap = true
		fingerprint, fpErr := bootstrapFingerprint(boot)
		if fpErr != nil {
			return out, fpErr
		}
		device := id.ID(boot.Device.DeviceID)
		if state.PendingDevice.IsZero() {
			state.PendingDevice, state.BootstrapFingerprint = device, fingerprint
			revision, err = l.saveState(ctx, revision, state)
			if err != nil {
				return out, err
			}
		} else if state.PendingDevice != device || state.BootstrapFingerprint != fingerprint {
			return out, ErrRecovery
		}
	case 2:
		if state.PendingDevice.IsZero() || state.BootstrapFingerprint == ([32]byte{}) {
			return out, fmt.Errorf("%w: completed native Bot has no encrypted bootstrap receipt", ErrRecovery)
		}
		boot, bootErr := l.native.Bootstrap(ctx, externalSenderKey)
		if bootErr != nil {
			return out, bootErr
		}
		fingerprint, fpErr := bootstrapFingerprint(boot)
		if fpErr != nil || id.ID(boot.Device.DeviceID) != state.PendingDevice || fingerprint != state.BootstrapFingerprint {
			return out, ErrRecovery
		}
		if state.BotUser.IsZero() || state.BotDevice != state.PendingDevice || identity.User == nil || *identity.User != state.BotUser || identity.Device == nil || *identity.Device != state.BotDevice || inst.BotUser == nil || *inst.BotUser != state.BotUser || inst.BotDevice == nil || *inst.BotDevice != state.BotDevice {
			return out, fmt.Errorf("%w: completed native Bot and current installation tuple differ", ErrRecovery)
		}
	}
	pendingRegistration := identity.Phase == 0 && !state.PendingDevice.IsZero()
	if pendingRegistration && state.ClaimID.IsZero() {
		claim, _, claimErr := l.runtime.ClaimObserved(ctx, l.install, state.ClaimRequest)
		if claimErr != nil {
			return out, claimErr
		}
		if claim.InstallationID != l.install || claim.ClaimID.IsZero() || claim.Expires == 0 || claim.Expires > math.MaxInt64 {
			return out, ErrRecovery
		}
		state.ClaimID, state.ClaimExpires = claim.ClaimID, claim.Expires
		revision, err = l.saveState(ctx, revision, state)
		if err != nil {
			return out, err
		}
	}
	if pendingRegistration && !state.PendingDevice.IsZero() {
		if inst.BotDevice != nil && *inst.BotDevice != state.PendingDevice {
			return out, ErrRecovery
		}
		if !state.BotUser.IsZero() && inst.BotUser != nil && *inst.BotUser != state.BotUser {
			return out, ErrRecovery
		}
	}
	if pendingRegistration {
		if state.BotUser.IsZero() {
			if !state.ClaimID.IsZero() && !state.EnrollAttempted {
				claim, _, claimErr := l.runtime.ClaimObserved(ctx, l.install, state.ClaimRequest)
				if claimErr != nil {
					return out, claimErr
				}
				if claim.InstallationID != l.install || claim.ClaimID.IsZero() || claim.Expires == 0 || claim.Expires > math.MaxInt64 {
					return out, ErrRecovery
				}
				if claim.ClaimID != state.ClaimID {
					state.ClaimID, state.ClaimExpires = claim.ClaimID, claim.Expires
					revision, err = l.saveState(ctx, revision, state)
					if err != nil {
						return out, err
					}
				} else if claim.Expires != state.ClaimExpires {
					return out, ErrRecovery
				}
			}
			if !hasBootstrap {
				boot, err = l.native.Bootstrap(ctx, externalSenderKey)
				if err != nil {
					return out, err
				}
				fingerprint, fpErr := bootstrapFingerprint(boot)
				if fpErr != nil || id.ID(boot.Device.DeviceID) != state.PendingDevice || fingerprint != state.BootstrapFingerprint {
					return out, ErrRecovery
				}
			}
			registration := client.DeviceRegistration{DeviceID: id.ID(boot.Device.DeviceID), DSKPub: append([]byte(nil), boot.Device.DSKPublic[:]...), Tier: boot.Device.Tier, SignerTier: boot.Device.SignerTier, Credential: append([]byte(nil), boot.Device.Credential...)}
			state.EnrollAttempted = true
			revision, err = l.saveState(ctx, revision, state)
			if err != nil {
				return out, err
			}
			enrolled, observation, enrollErr := l.runtime.EnrollObserved(ctx, l.install, state.EnrollRequest, state.ClaimID, boot.UMKPublic[:], boot.SSKPublic[:], boot.SigUMKSSK[:], registration)
			if enrollErr != nil {
				var apiErr *client.APIError
				if client.IsDeferred(enrollErr) || errors.As(enrollErr, &apiErr) && apiErr.Outcome == client.OutcomeDefinitive {
					state.EnrollAttempted = false
					_, _ = l.saveState(ctx, revision, state)
				}
				return out, enrollErr
			}
			if enrolled.BotUser.IsZero() || enrolled.BotDevice != registration.DeviceID || observation.ServerTime == 0 || observation.ServerTime > math.MaxInt64 {
				return out, ErrRecovery
			}
			state.BotUser, state.BotDevice = enrolled.BotUser, enrolled.BotDevice
			state.Username, state.CompleteAt = enrolled.Username, observation.ServerTime
			state.EnrollAttempted = false
			revision, err = l.saveState(ctx, revision, state)
			if err != nil {
				return out, err
			}
		} else {
			remote, findErr := l.findInstallation(ctx)
			if findErr != nil || remote.BotUser == nil || remote.BotDevice == nil || *remote.BotUser != state.BotUser || *remote.BotDevice != state.BotDevice {
				return out, ErrRecovery
			}
		}
		if state.CompleteAt == 0 || state.Username == "" {
			return out, ErrRecovery
		}
		if _, err = l.native.CompleteEnrollment(ctx, state.BotUser, state.Username, state.CompleteAt); err != nil {
			return out, err
		}
		identity, err = l.native.Identity(ctx)
		if err != nil {
			return out, err
		}
	}
	if identity.Phase == 2 && (state.BotUser.IsZero() || identity.User == nil || *identity.User != state.BotUser || identity.Device == nil || state.BotDevice.IsZero() || *identity.Device != state.BotDevice) {
		return out, fmt.Errorf("%w: native account exists without matching encrypted lifecycle receipt", ErrRecovery)
	}
	if identity.Phase != 2 {
		return out, errors.New("bot identity enrollment is still pending")
	}
	if state.CompleteAt == 0 || state.Username != identity.Username {
		return out, ErrRecovery
	}
	completed, err := l.native.CompleteEnrollment(ctx, state.BotUser, state.Username, state.CompleteAt)
	if err != nil {
		return out, err
	}
	deviceList, err := decodeDeviceList(completed.DeviceListBody)
	if err != nil {
		return out, err
	}
	session, err := l.establishSession(ctx, state.BotUser, state.BotDevice, discovery, false)
	if err != nil {
		return out, err
	}
	negotiated, err := session.Negotiate(ctx, []uint64{3}, []uint64{1}, []uint64{1})
	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && (apiErr.Status == 401 || apiErr.Status == 403) {
			fresh, findErr := l.findInstallation(ctx)
			if findErr != nil || fresh.CommunityID != l.community || fresh.BotUser == nil || fresh.BotDevice == nil || *fresh.BotUser != state.BotUser || *fresh.BotDevice != state.BotDevice || fresh.State == 4 || fresh.State == 5 {
				return out, ErrRecovery
			}
			_ = l.native.ClearSession(ctx)
			session, err = l.establishSession(ctx, state.BotUser, state.BotDevice, discovery, true)
			if err == nil {
				negotiated, err = session.Negotiate(ctx, []uint64{3}, []uint64{1}, []uint64{1})
			}
		}
		if err != nil {
			return out, err
		}
	}
	if negotiated != (client.Negotiated{Wire: 3, E2EE: 1, Media: 1}) {
		return out, client.ErrProtocol
	}
	inst, err = l.findInstallation(ctx)
	if err != nil {
		return out, err
	}
	if inst.CommunityID != l.community || inst.BotUser == nil || *inst.BotUser != state.BotUser || inst.BotDevice == nil || *inst.BotDevice != state.BotDevice {
		return out, ErrRecovery
	}
	if inst.State != 1 && inst.State != 2 && inst.State != 3 {
		return out, fmt.Errorf("bot installation no longer permits identity actions")
	}
	if inst.State == 1 || inst.State == 2 {
		if state.FinalRequest.IsZero() {
			state.FinalRequest, state.FinalRevision = id.New(), inst.Revision
			revision, err = l.saveState(ctx, revision, state)
			if err != nil {
				return out, err
			}
		}
	} else if state.FinalRequest.IsZero() {
		// A later Ready phase does not prove that this lifecycle's exact
		// credential request was accepted; only its receipt-bearing endpoint does.
		return out, ErrRecovery
	}
	inst, err = l.finalizeCredential(ctx, session, inst, state, completed.Credential)
	if err != nil {
		return out, err
	}
	// FinalizeCredential replays its original operation receipt. Another valid
	// installation update may have advanced the revision since that receipt was
	// recorded, so use the authenticated current head for the subsequent Ready.
	current, err := l.findInstallation(ctx)
	if err != nil {
		return out, err
	}
	if current.ID != inst.ID || current.CommunityID != inst.CommunityID ||
		current.BotUser == nil || *current.BotUser != state.BotUser ||
		current.BotDevice == nil || *current.BotDevice != state.BotDevice ||
		(current.State != 2 && current.State != 3) || current.Revision < inst.Revision {
		return out, ErrRecovery
	}
	inst = current
	if err = l.reconcileDeviceList(ctx, session, state.BotUser, completed.DeviceListBody, deviceList, &identity, &state, &revision); err != nil {
		return out, err
	}
	inventory, err := session.KeyPackageInventory(ctx, l.install)
	if err != nil {
		return out, err
	}
	if inventory.Generation != discovery.Generation {
		// The inventory is the authoritative, same-response generation snapshot.
		discovery.Generation = inventory.Generation
	}
	refillDeferred, err := l.ensureKeyPackages(ctx, session, &state, &revision, inventory)
	if err != nil {
		return out, err
	}
	inventory, err = session.KeyPackageInventory(ctx, l.install)
	if err != nil {
		return out, err
	}
	if inventory.Generation != discovery.Generation {
		return out, fmt.Errorf("instance generation changed during identity reconciliation")
	}
	concreteSession, _ := session.(*client.Session)
	refillDeferred = refillDeferred || state.KeyPackageCapacityDeferred
	return LifecycleResult{Phase: inst.State, Installation: inst, Session: concreteSession, Packages: inventory, PackageRefillPending: refillDeferred}, nil
}

// reconcileDeviceList checks the authenticated current server head on every
// pass. The helper's published bit can outlive a server restore, so it is only
// a local progress hint; exact head equality is the acceptance evidence.
func (l *Lifecycle) reconcileDeviceList(ctx context.Context, session lifecycleSession, user id.ID, currentRaw []byte, current deviceList, identity *native.Identity, state *identityState, revision *uint64) error {
	remote, getErr := session.OwnDeviceList(ctx, user)
	needPublish := false
	if getErr == nil {
		if !equalBytes(remote, currentRaw) {
			previous, parseErr := decodeDeviceList(remote)
			if parseErr != nil || previous.Version >= math.MaxInt64 || current.Version != previous.Version+1 {
				return fmt.Errorf("%w: server device-list head cannot safely advance to native head", ErrRecovery)
			}
			previousHash := sha256.Sum256(previous.Blob)
			if !equalBytes(current.PreviousHash, previousHash[:]) {
				return fmt.Errorf("%w: native device-list head does not extend current server head", ErrRecovery)
			}
			needPublish = true
		}
	} else if isMissingDeviceList(getErr) {
		if current.Version != 1 || !equalBytes(current.PreviousHash, make([]byte, 32)) {
			return fmt.Errorf("%w: server device-list rollback is behind the repairable native head", ErrRecovery)
		}
		needPublish = true
	} else {
		return getErr
	}
	if needPublish {
		publishErr := session.PublishDeviceList(ctx, user, current.Version, current.Blob, current.Signature, current.PreviousHash)
		remote, getErr = session.OwnDeviceList(ctx, user)
		if getErr != nil {
			if publishErr != nil {
				return publishErr
			}
			return getErr
		}
		if !equalBytes(remote, currentRaw) {
			if publishErr != nil {
				return publishErr
			}
			return fmt.Errorf("%w: published device-list head does not match native head", ErrRecovery)
		}
	}
	if !identity.DeviceListPublished {
		if err := l.native.DeviceListPublished(ctx); err != nil {
			return err
		}
		identity.DeviceListPublished = true
	}
	if !state.ListPublished {
		state.ListPublished = true
		var err error
		*revision, err = l.saveState(ctx, *revision, *state)
		if err != nil {
			return err
		}
	}
	return nil
}

func isMissingDeviceList(err error) bool {
	var apiErr *client.APIError
	return errors.As(err, &apiErr) && apiErr.Status == 404 && apiErr.Code == "E_NOT_FOUND"
}

type deviceList struct {
	Version                       uint64
	Blob, Signature, PreviousHash []byte
}

func decodeDeviceList(raw []byte) (deviceList, error) {
	var out deviceList
	if cborx.CheckDeterministic(raw) != nil {
		return out, client.ErrProtocol
	}
	var a []any
	if err := cborx.Unmarshal(raw, &a); err != nil || len(a) != 4 {
		return out, client.ErrProtocol
	}
	var ok bool
	if out.Version, ok = a[0].(uint64); !ok || out.Version == 0 || out.Version > math.MaxInt64 {
		return deviceList{}, client.ErrProtocol
	}
	if out.Blob, ok = a[1].([]byte); !ok || len(out.Blob) == 0 {
		return deviceList{}, client.ErrProtocol
	}
	if out.Signature, ok = a[2].([]byte); !ok || len(out.Signature) != 64 {
		return deviceList{}, client.ErrProtocol
	}
	if out.PreviousHash, ok = a[3].([]byte); !ok || len(out.PreviousHash) != 32 {
		return deviceList{}, client.ErrProtocol
	}
	return out, nil
}

func (l *Lifecycle) findInstallation(ctx context.Context) (client.Installation, error) {
	var after *id.ID
	for pageNum := 0; pageNum < identityMaxPages; pageNum++ {
		page, err := l.runtime.ListInstallationsPage(ctx, after)
		if err != nil {
			return client.Installation{}, err
		}
		for _, item := range page {
			if item.ID == l.install {
				return item, nil
			}
		}
		if len(page) < identityPageSize {
			return client.Installation{}, ErrRecovery
		}
		last := page[len(page)-1].ID
		after = &last
	}
	return client.Installation{}, client.ErrPageLimit
}

func (l *Lifecycle) finalizeCredential(ctx context.Context, session lifecycleSession, current client.Installation, st identityState, credential []byte) (client.Installation, error) {
	// Retries deliberately use the original revision and repeatable native
	// credential bytes, even when the current server revision has advanced.
	result, err := session.FinalizeCredential(ctx, l.install, st.FinalRequest, st.FinalRevision, credential)
	if err != nil {
		return client.Installation{}, err
	}
	if result.ID != l.install || result.CommunityID != l.community || result.BotUser == nil || result.BotDevice == nil || *result.BotUser != st.BotUser || *result.BotDevice != st.BotDevice {
		return client.Installation{}, ErrRecovery
	}
	if result.State != 2 && result.State != 3 {
		return client.Installation{}, ErrRecovery
	}
	if current.ID != result.ID {
		return client.Installation{}, ErrRecovery
	}
	return result, nil
}

func (l *Lifecycle) establishSession(ctx context.Context, user, device id.ID, discovery client.Discovery, forceRenew bool) (lifecycleSession, error) {
	stored, err := l.native.Session(ctx)
	if err != nil {
		return nil, err
	}
	if stored.Token() != "" && !forceRenew && stored.Expires > uint64(max(int64(0), l.now().Unix()+int64(l.renewMargin/time.Second))) {
		s, e := l.http.NewSession(stored.Token(), device)
		if e != nil {
			return nil, e
		}
		return s, nil
	}
	purpose := uint64(0)
	if stored.Token() != "" || forceRenew {
		purpose = 1
	}
	if stored.Token() != "" {
		if err := l.native.ClearSession(ctx); err != nil {
			return nil, err
		}
	}
	challenge, err := l.http.Challenge(ctx, device)
	if err != nil {
		return nil, err
	}
	if len(challenge.Nonce) != 32 {
		return nil, client.ErrProtocol
	}
	var nonce [32]byte
	copy(nonce[:], challenge.Nonce)
	proof, err := l.native.SignSession(ctx, nonce, purpose)
	if err != nil {
		return nil, err
	}
	session, err := l.http.EstablishSession(ctx, device, challenge.Nonce, purpose, proof.Signature[:], nil, nil)
	if err != nil {
		return nil, err
	}
	if session.Scope != 0 || session.UserID != user || session.DeviceID != device || session.Generation != discovery.Generation {
		return nil, ErrRecovery
	}
	if err = l.native.StoreSession(ctx, session.Token, session.Expires, session.IdleExpires); err != nil {
		return nil, err
	}
	return l.http.NewSession(session.Token, device)
}

func (l *Lifecycle) ensureKeyPackages(ctx context.Context, session lifecycleSession, st *identityState, revision *uint64, inventory client.KeyPackageInventory) (bool, error) {
	if st.KeyPackageResolvePending {
		resolution := protocol.KeyPackageResolution{OperationID: st.KeyPackageOperation, PackageRefs: st.KeyPackageRefs, Outcome: st.KeyPackageResolveOutcome, Now: st.KeyPackageResolveNow, CurrentGeneration: st.KeyPackageResolveGeneration}
		if err := l.native.ResolveKeyPackageIntent(ctx, resolution); err != nil {
			return false, err
		}
		st.KeyPackageResolvePending, st.KeyPackageDone, st.KeyPackageCapacityDeferred = false, true, false
		var err error
		*revision, err = l.saveState(ctx, *revision, *st)
		return false, err
	}
	desired := l.desired
	maxPerDevice := uint64(32)
	if limits, err := l.http.InstanceLimits(ctx); err == nil {
		if limits.Values[4] > 0 {
			maxPerDevice = limits.Values[4]
		}
		if refill := limits.Values[5]; refill > desired {
			desired = refill
		}
	}
	if desired > maxPerDevice {
		desired = maxPerDevice
	}
	if desired > 32 {
		desired = 32
	}
	need := uint64(0)
	if inventory.OrdinaryActive < desired {
		need = desired - inventory.OrdinaryActive
	}
	lastResort := uint64(0)
	if !inventory.HasCurrentFallback {
		lastResort = 1
	}
	if st.KeyPackageOperation.IsZero() || st.KeyPackageDone {
		if need == 0 && lastResort == 0 {
			return false, nil
		}
		if need == 0 {
			if inventory.OrdinaryActive >= maxPerDevice {
				if inventory.OrdinaryActive >= 1 {
					return true, nil
				}
				return false, errors.New("KeyPackage fallback refill is capacity-deferred without an ordinary package for D2")
			}
			need = 1
		}
		st.KeyPackageOperation, st.KeyPackageCount, st.KeyPackageFallback, st.KeyPackageGeneration, st.KeyPackageDone = id.New(), need, lastResort, inventory.Generation, false
		st.KeyPackageResolvePending, st.KeyPackageRefs = false, nil
		st.KeyPackageResolveOutcome, st.KeyPackageResolveNow, st.KeyPackageResolveGeneration = 0, 0, 0
		var err error
		*revision, err = l.saveState(ctx, *revision, *st)
		if err != nil {
			return false, err
		}
	}
	intent := protocol.KeyPackageIntent{OperationID: st.KeyPackageOperation, Count: st.KeyPackageCount, LastResort: st.KeyPackageFallback, ExpectedGeneration: st.KeyPackageGeneration}
	result, err := l.native.KeyPackageIntent(ctx, intent)
	if err != nil {
		return false, err
	}
	resolution := protocol.KeyPackageResolution{OperationID: intent.OperationID, PackageRefs: result.PackageRefs, Outcome: 0, CurrentGeneration: inventory.Generation}
	if inventory.Generation > intent.ExpectedGeneration {
		resolution.Outcome = 1
	} else if inventory.ServerTime >= result.GreatestExpiry {
		resolution.Outcome, resolution.Now = 1, inventory.ServerTime
	} else {
		if st.KeyPackageCapacityDeferred {
			free := uint64(0)
			if inventory.OrdinaryActive < maxPerDevice {
				free = maxPerDevice - inventory.OrdinaryActive
			}
			if free < st.KeyPackageCount {
				if inventory.OrdinaryActive >= 1 {
					return true, nil
				}
				return false, errors.New("KeyPackage refill remains capacity-deferred without an ordinary package for D2")
			}
			st.KeyPackageCapacityDeferred = false
			*revision, err = l.saveState(ctx, *revision, *st)
			if err != nil {
				return false, err
			}
		}
		_, publishErr := session.PublishKeyPackages(ctx, l.install, intent.OperationID, intent.ExpectedGeneration, result.UploadBody)
		var apiErr *client.APIError
		if errors.As(publishErr, &apiErr) && apiErr.Outcome == client.OutcomeDefinitive {
			// A capacity or authority refusal is known not to have published.
			// Keep the durable intent pending for a later bounded Ensure pass.
			if apiErr.Code == "E_TOO_LARGE" && inventory.OrdinaryActive >= maxPerDevice && inventory.OrdinaryActive >= 1 {
				st.KeyPackageCapacityDeferred = true
				*revision, err = l.saveState(ctx, *revision, *st)
				if err != nil {
					return false, err
				}
				return true, nil
			}
			return false, publishErr
		}
		receipt, _, receiptErr := session.KeyPackagePublicationReceiptObserved(ctx, l.install, intent.OperationID)
		if receiptErr != nil {
			// A missing receipt does not settle the outcome. The exact same
			// native intent and operation ID are safe to submit again.
			_, retryErr := session.PublishKeyPackages(ctx, l.install, intent.OperationID, intent.ExpectedGeneration, result.UploadBody)
			receipt, _, receiptErr = session.KeyPackagePublicationReceiptObserved(ctx, l.install, intent.OperationID)
			if receiptErr != nil {
				if publishErr != nil {
					return false, publishErr
				}
				if retryErr != nil {
					return false, retryErr
				}
				return false, receiptErr
			}
		}
		if receipt.Generation != intent.ExpectedGeneration || receipt.Count != uint64(len(result.PackageRefs)) || receipt.UploadSHA256 != sha256.Sum256(result.UploadBody) {
			return false, ErrRecovery
		}
	}
	st.KeyPackageRefs = append([][32]byte(nil), result.PackageRefs...)
	st.KeyPackageResolveOutcome, st.KeyPackageResolveNow, st.KeyPackageResolveGeneration = resolution.Outcome, resolution.Now, resolution.CurrentGeneration
	st.KeyPackageResolvePending = true
	*revision, err = l.saveState(ctx, *revision, *st)
	if err != nil {
		return false, err
	}
	if err = l.native.ResolveKeyPackageIntent(ctx, resolution); err != nil {
		return false, err
	}
	st.KeyPackageResolvePending, st.KeyPackageDone, st.KeyPackageCapacityDeferred = false, true, false
	*revision, err = l.saveState(ctx, *revision, *st)
	return false, err
}

type identityState struct {
	Initialized                                                                 bool
	ClaimRequest, ClaimID                                                       id.ID
	ClaimExpires                                                                uint64
	EnrollRequest                                                               id.ID
	EnrollAttempted                                                             bool
	BotUser, BotDevice                                                          id.ID
	Username                                                                    string
	CompleteAt                                                                  uint64
	FinalRequest                                                                id.ID
	FinalRevision                                                               uint64
	ListPublished                                                               bool
	KeyPackageOperation                                                         id.ID
	KeyPackageCount, KeyPackageFallback, KeyPackageGeneration                   uint64
	KeyPackageDone                                                              bool
	KeyPackageResolvePending                                                    bool
	KeyPackageCapacityDeferred                                                  bool
	KeyPackageRefs                                                              [][32]byte
	KeyPackageResolveOutcome, KeyPackageResolveNow, KeyPackageResolveGeneration uint64
	BootstrapRequested                                                          bool
	PendingDevice                                                               id.ID
	BootstrapFingerprint                                                        [32]byte
}

func bootstrapFingerprint(boot native.BootstrapResult) ([32]byte, error) {
	device := []any{id.ID(boot.Device.DeviceID), boot.Device.DSKPublic[:], boot.Device.Tier, boot.Device.SignerTier, boot.Device.Credential}
	raw, err := cborx.Marshal([]any{boot.UMKPublic[:], boot.SSKPublic[:], boot.SigUMKSSK[:], device})
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(raw), nil
}

func (l *Lifecycle) loadState(ctx context.Context) (identityState, uint64, error) {
	v, err := l.native.StateGet(ctx, identityStateArea, []byte(identityStateKey))
	if err != nil {
		return identityState{}, 0, err
	}
	if v.Tombstone {
		return identityState{}, 0, ErrRecovery
	}
	if v.Revision == 0 {
		return identityState{}, 0, nil
	}
	st, err := decodeIdentityState(v.Value)
	return st, v.Revision, err
}

func (l *Lifecycle) saveState(ctx context.Context, revision uint64, st identityState) (uint64, error) {
	refs := make([]any, len(st.KeyPackageRefs))
	for i := range st.KeyPackageRefs {
		refs[i] = st.KeyPackageRefs[i][:]
	}
	value, err := cborx.Marshal([]any{uint64(identityStateVer), boolUint(st.Initialized), st.ClaimRequest, nullableID(st.ClaimID), st.ClaimExpires, st.EnrollRequest, boolUint(st.EnrollAttempted), nullableID(st.BotUser), nullableID(st.BotDevice), st.Username, st.CompleteAt, nullableID(st.FinalRequest), st.FinalRevision, boolUint(st.ListPublished), nullableID(st.KeyPackageOperation), st.KeyPackageCount, st.KeyPackageFallback, st.KeyPackageGeneration, boolUint(st.KeyPackageDone), boolUint(st.KeyPackageResolvePending), refs, st.KeyPackageResolveOutcome, st.KeyPackageResolveNow, st.KeyPackageResolveGeneration, boolUint(st.BootstrapRequested), nullableID(st.PendingDevice), st.BootstrapFingerprint[:], boolUint(st.KeyPackageCapacityDeferred)})
	if err != nil {
		return revision, err
	}
	seed := append([]byte("dilla.bot.identity.state.tx.v1"), []byte(identityStateKey)...)
	var expected [8]byte
	binary.BigEndian.PutUint64(expected[:], revision)
	seed = append(seed, expected[:]...)
	txhash := sha256.Sum256(append(seed, value...))
	var tx id.ID
	copy(tx[:], txhash[:16])
	revs, err := l.native.StateTransaction(ctx, tx, []native.StateChange{{Area: identityStateArea, Key: []byte(identityStateKey), ExpectedRevision: revision, Value: value}})
	if err != nil {
		return revision, err
	}
	if len(revs) != 1 {
		return revision, ErrRecovery
	}
	return revs[0], nil
}

func decodeIdentityState(raw []byte) (identityState, error) {
	var st identityState
	if cborx.CheckDeterministic(raw) != nil {
		return st, ErrRecovery
	}
	var a []any
	if err := cborx.Unmarshal(raw, &a); err != nil || len(a) != 27 && len(a) != 28 {
		return st, ErrRecovery
	}
	version, ok := a[0].(uint64)
	if !ok || version != 1 && version != identityStateVer || version == 1 && len(a) != 27 || version == identityStateVer && len(a) != 28 {
		return st, ErrRecovery
	}
	if st.Initialized, ok = uintBool(a[1]); !ok {
		return st, ErrRecovery
	}
	if st.ClaimRequest, ok = idValue(a[2]); !ok || st.ClaimRequest.IsZero() {
		return st, ErrRecovery
	}
	if st.ClaimID, ok = nullableIDValue(a[3]); !ok {
		return st, ErrRecovery
	}
	if st.ClaimExpires, ok = a[4].(uint64); !ok {
		return st, ErrRecovery
	}
	if st.EnrollRequest, ok = idValue(a[5]); !ok || st.EnrollRequest.IsZero() {
		return st, ErrRecovery
	}
	if st.EnrollAttempted, ok = uintBool(a[6]); !ok {
		return st, ErrRecovery
	}
	if st.BotUser, ok = nullableIDValue(a[7]); !ok {
		return st, ErrRecovery
	}
	if st.BotDevice, ok = nullableIDValue(a[8]); !ok {
		return st, ErrRecovery
	}
	if st.Username, ok = a[9].(string); !ok {
		return st, ErrRecovery
	}
	if st.CompleteAt, ok = a[10].(uint64); !ok {
		return st, ErrRecovery
	}
	if st.FinalRequest, ok = nullableIDValue(a[11]); !ok {
		return st, ErrRecovery
	}
	if st.FinalRevision, ok = a[12].(uint64); !ok {
		return st, ErrRecovery
	}
	if st.ListPublished, ok = uintBool(a[13]); !ok {
		return st, ErrRecovery
	}
	if st.KeyPackageOperation, ok = nullableIDValue(a[14]); !ok {
		return st, ErrRecovery
	}
	if st.KeyPackageCount, ok = a[15].(uint64); !ok {
		return st, ErrRecovery
	}
	if st.KeyPackageFallback, ok = a[16].(uint64); !ok || st.KeyPackageFallback > 1 {
		return st, ErrRecovery
	}
	if st.KeyPackageGeneration, ok = a[17].(uint64); !ok {
		return st, ErrRecovery
	}
	if st.KeyPackageDone, ok = uintBool(a[18]); !ok {
		return st, ErrRecovery
	}
	if st.KeyPackageResolvePending, ok = uintBool(a[19]); !ok {
		return st, ErrRecovery
	}
	refRows, ok := a[20].([]any)
	if !ok || len(refRows) > 33 {
		return st, ErrRecovery
	}
	st.KeyPackageRefs = make([][32]byte, len(refRows))
	for i, rawRef := range refRows {
		ref, ok := rawRef.([]byte)
		if !ok || len(ref) != 32 {
			return identityState{}, ErrRecovery
		}
		copy(st.KeyPackageRefs[i][:], ref)
	}
	if st.KeyPackageResolveOutcome, ok = a[21].(uint64); !ok || st.KeyPackageResolveOutcome > 1 {
		return st, ErrRecovery
	}
	if st.KeyPackageResolveNow, ok = a[22].(uint64); !ok {
		return st, ErrRecovery
	}
	if st.KeyPackageResolveGeneration, ok = a[23].(uint64); !ok {
		return st, ErrRecovery
	}
	if st.BootstrapRequested, ok = uintBool(a[24]); !ok {
		return st, ErrRecovery
	}
	if st.PendingDevice, ok = nullableIDValue(a[25]); !ok {
		return st, ErrRecovery
	}
	fingerprint, ok := a[26].([]byte)
	if !ok || len(fingerprint) != 32 {
		return st, ErrRecovery
	}
	copy(st.BootstrapFingerprint[:], fingerprint)
	if version >= 2 {
		if st.KeyPackageCapacityDeferred, ok = uintBool(a[27]); !ok {
			return st, ErrRecovery
		}
	}
	if !st.PendingDevice.IsZero() && (!st.BootstrapRequested || st.BootstrapFingerprint == ([32]byte{})) {
		return st, ErrRecovery
	}
	if st.KeyPackageResolvePending && (st.KeyPackageOperation.IsZero() || len(st.KeyPackageRefs) == 0) {
		return st, ErrRecovery
	}
	return st, nil
}

func boolUint(v bool) uint64 {
	if v {
		return 1
	}
	return 0
}
func uintBool(v any) (bool, bool) { n, ok := v.(uint64); return n == 1, ok && n <= 1 }
func nullableID(v id.ID) any {
	if v.IsZero() {
		return nil
	}
	return v
}
func nullableIDValue(v any) (id.ID, bool) {
	if v == nil {
		return id.ID{}, true
	}
	return idValue(v)
}
func idValue(v any) (id.ID, bool) {
	b, ok := v.([]byte)
	if !ok || len(b) != 16 {
		return id.ID{}, false
	}
	var out id.ID
	copy(out[:], b)
	return out, !out.IsZero()
}
func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
