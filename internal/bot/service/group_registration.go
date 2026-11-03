package service

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

const (
	groupRegistrationStateArea       = 6
	groupRegistrationStateKey        = "dilla.bot.group-registration.v1/"
	groupRegistrationStateVer        = 1
	groupRegistrationPending         = 0
	groupRegistrationComplete        = 1
	groupRegistrationInitialEpoch    = 0
	groupRegistrationInitialSequence = 1
)

var (
	ErrGroupRegistrationRecovery       = errors.New("bot group registration requires recovery")
	ErrGroupRegistrationOutcomeUnknown = errors.New("bot group registration outcome is unknown")
	ErrGroupRegistrationDenied         = errors.New("bot group registration was denied")
)

type groupRegistrationNative interface {
	Binding() native.Binding
	Bootstrap(context.Context, [32]byte) (native.BootstrapResult, error)
	CreateGroup(context.Context, id.ID, id.ID, id.ID, uint64, uint64, []byte) (native.GroupCreateResult, error)
	GroupRegistered(context.Context, id.ID, id.ID, uint64) error
	StateGet(context.Context, uint64, []byte) (native.StateValue, error)
	StateTransaction(context.Context, id.ID, []native.StateChange) ([]uint64, error)
}

type groupRegistrationDiscovery interface {
	Discover(context.Context) (client.Discovery, error)
}

type groupRegistrationSession interface {
	RegisterBotGroup(context.Context, id.ID, id.ID, uint64, []byte) (client.BotGroupRegistrationReceipt, error)
	BotGroupRegistrationReceipt(context.Context, id.ID, id.ID) (client.BotGroupRegistrationReceipt, error)
	GroupInfo(context.Context, id.ID) (client.GroupInfo, error)
}

// FirstGroupRequest identifies one installation-bound group creation. GroupID
// is supplied by the caller so a restart can address the same durable journal.
type FirstGroupRequest struct {
	GroupID, TargetID id.ID
	Kind, Policy      uint64
}

// GroupRegistrationCoordinator serializes native and server registration
// mutations for one bot installation. Its state journal is encrypted by the
// native helper's state store.
type GroupRegistrationCoordinator struct {
	mu        sync.Mutex
	instance  id.ID
	community id.ID
	install   id.ID
	native    groupRegistrationNative
	discovery groupRegistrationDiscovery
	session   groupRegistrationSession
}

func NewGroupRegistrationCoordinator(instance, community, installation id.ID, n groupRegistrationNative, discovery groupRegistrationDiscovery, session groupRegistrationSession) (*GroupRegistrationCoordinator, error) {
	return newGroupRegistrationCoordinator(instance, community, installation, n, discovery, session)
}

func newGroupRegistrationCoordinator(instance, community, installation id.ID, n groupRegistrationNative, discovery groupRegistrationDiscovery, session groupRegistrationSession) (*GroupRegistrationCoordinator, error) {
	if instance.IsZero() || community.IsZero() || installation.IsZero() || n == nil || discovery == nil || session == nil {
		return nil, errors.New("invalid bot group registration binding")
	}
	if n.Binding() != (native.Binding{Instance: instance, Community: community, Installation: installation}) {
		return nil, errors.New("native group registration binding mismatch")
	}
	return &GroupRegistrationCoordinator{instance: instance, community: community, install: installation, native: n, discovery: discovery, session: session}, nil
}

type groupRegistrationIntent struct {
	Operation, Group, Target id.ID
	Kind, Policy             uint64
	ExternalSender           []byte
	Generation               uint64
	Body                     []byte
	Submitted                bool
	Phase                    uint64
	Receipt                  client.BotGroupRegistrationReceipt
}

// RegisterFirstGroup creates and registers a text or call group with one
// durable operation ID. Exact retries resume the same journal entry.
func (c *GroupRegistrationCoordinator) RegisterFirstGroup(ctx context.Context, request FirstGroupRequest) (client.BotGroupRegistrationReceipt, error) {
	if c == nil {
		return client.BotGroupRegistrationReceipt{}, errors.New("invalid first group request")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if request.GroupID.IsZero() || request.TargetID.IsZero() || request.Kind > 1 || request.Policy > math.MaxInt64 {
		return client.BotGroupRegistrationReceipt{}, errors.New("invalid first group request")
	}
	discovery, err := c.discovery.Discover(ctx)
	if err != nil {
		return client.BotGroupRegistrationReceipt{}, err
	}
	if discovery.InstanceID != c.instance || discovery.Generation == 0 || discovery.Generation > math.MaxInt64 {
		return client.BotGroupRegistrationReceipt{}, ErrGroupRegistrationRecovery
	}
	externalSenderKey, err := discoveryExternalSenderKey(discovery)
	if err != nil {
		return client.BotGroupRegistrationReceipt{}, ErrGroupRegistrationRecovery
	}
	if _, err := c.native.Bootstrap(ctx, externalSenderKey); err != nil {
		return client.BotGroupRegistrationReceipt{}, err
	}
	key := groupRegistrationKey(request.GroupID)
	value, err := c.native.StateGet(ctx, groupRegistrationStateArea, key)
	if err != nil {
		return client.BotGroupRegistrationReceipt{}, err
	}
	var intent groupRegistrationIntent
	revision := value.Revision
	if value.Tombstone {
		return client.BotGroupRegistrationReceipt{}, ErrGroupRegistrationRecovery
	}
	if revision == 0 {
		intent = groupRegistrationIntent{Operation: id.New(), Group: request.GroupID, Target: request.TargetID, Kind: request.Kind, Policy: request.Policy, ExternalSender: append([]byte(nil), discovery.ExternalSenderPub...), Generation: discovery.Generation}
		revision, err = c.saveGroupRegistration(ctx, key, revision, intent)
		if err != nil {
			return client.BotGroupRegistrationReceipt{}, fmt.Errorf("persist group registration intent: %w", err)
		}
	} else {
		intent, err = decodeGroupRegistrationIntent(value.Value)
		if err != nil || intent.Group != request.GroupID || intent.Target != request.TargetID || intent.Kind != request.Kind || intent.Policy != request.Policy {
			return client.BotGroupRegistrationReceipt{}, ErrGroupRegistrationRecovery
		}
		if intent.Phase == groupRegistrationComplete {
			return intent.Receipt, nil
		}
	}

	if len(intent.Body) != 0 {
		receipt, err := c.session.BotGroupRegistrationReceipt(ctx, c.install, intent.Operation)
		if err == nil {
			return c.acceptGroupRegistration(ctx, key, revision, intent, receipt)
		}
		if !errors.Is(err, client.ErrRegistrationReceiptNotFound) {
			return client.BotGroupRegistrationReceipt{}, fmt.Errorf("%w: receipt lookup failed: %w", ErrGroupRegistrationOutcomeUnknown, err)
		}
		if err := c.proveRetrySafe(ctx, intent); err != nil {
			return client.BotGroupRegistrationReceipt{}, err
		}
	} else {
		created, err := c.native.CreateGroup(ctx, intent.Operation, intent.Group, intent.Target, intent.Kind, intent.Policy, intent.ExternalSender)
		if err != nil {
			return client.BotGroupRegistrationReceipt{}, fmt.Errorf("%w: native create intent remains pending: %w", ErrGroupRegistrationRecovery, err)
		}
		if created.Group != intent.Group || created.Binding.Instance != c.instance || created.Binding.Community == nil || *created.Binding.Community != c.community || created.Binding.Target != intent.Target || created.Binding.Kind != intent.Kind || created.Binding.Policy != intent.Policy {
			return client.BotGroupRegistrationReceipt{}, ErrGroupRegistrationRecovery
		}
		body, err := cborx.Marshal([]any{created.Group, created.BindingBody, created.GroupInfo, created.RatchetTree})
		if err != nil || len(body) == 0 || len(body) > 65505 {
			return client.BotGroupRegistrationReceipt{}, ErrGroupRegistrationRecovery
		}
		intent.Body = body
		revision, err = c.saveGroupRegistration(ctx, key, revision, intent)
		if err != nil {
			return client.BotGroupRegistrationReceipt{}, fmt.Errorf("persist native registration body: %w", err)
		}
	}

	if err := c.proveRetrySafe(ctx, intent); err != nil {
		return client.BotGroupRegistrationReceipt{}, err
	}
	if !intent.Submitted {
		intent.Submitted = true
		var err error
		revision, err = c.saveGroupRegistration(ctx, key, revision, intent)
		if err != nil {
			return client.BotGroupRegistrationReceipt{}, fmt.Errorf("persist registration submission marker: %w", err)
		}
	}

	for attempt := 0; attempt < 2; attempt++ {
		receipt, postErr := c.session.RegisterBotGroup(ctx, c.install, intent.Operation, intent.Generation, intent.Body)
		if postErr == nil {
			return c.acceptGroupRegistration(ctx, key, revision, intent, receipt)
		}
		receipt, lookupErr := c.session.BotGroupRegistrationReceipt(ctx, c.install, intent.Operation)
		if lookupErr == nil {
			return c.acceptGroupRegistration(ctx, key, revision, intent, receipt)
		}
		if !errors.Is(lookupErr, client.ErrRegistrationReceiptNotFound) {
			return client.BotGroupRegistrationReceipt{}, fmt.Errorf("%w: receipt lookup failed: %w", ErrGroupRegistrationOutcomeUnknown, lookupErr)
		}
		if err := c.proveRetrySafe(ctx, intent); err != nil {
			return client.BotGroupRegistrationReceipt{}, err
		}
		if isDefinitiveRegistrationRefusal(postErr) {
			return client.BotGroupRegistrationReceipt{}, fmt.Errorf("%w: %w", ErrGroupRegistrationDenied, postErr)
		}
	}
	return client.BotGroupRegistrationReceipt{}, ErrGroupRegistrationOutcomeUnknown
}

func (c *GroupRegistrationCoordinator) proveRetrySafe(ctx context.Context, intent groupRegistrationIntent) error {
	discovery, err := c.discovery.Discover(ctx)
	if err != nil {
		return fmt.Errorf("%w: cannot verify instance generation: %w", ErrGroupRegistrationOutcomeUnknown, err)
	}
	if discovery.InstanceID != c.instance || discovery.Generation != intent.Generation {
		return ErrGroupRegistrationOutcomeUnknown
	}
	_, err = c.session.GroupInfo(ctx, intent.Group)
	if err == nil {
		return ErrGroupRegistrationRecovery
	}
	if !apiNotFound(err) {
		return fmt.Errorf("%w: cannot verify group absence: %w", ErrGroupRegistrationOutcomeUnknown, err)
	}
	return nil
}

func (c *GroupRegistrationCoordinator) acceptGroupRegistration(ctx context.Context, key []byte, revision uint64, intent groupRegistrationIntent, receipt client.BotGroupRegistrationReceipt) (client.BotGroupRegistrationReceipt, error) {
	wantHash := groupRegistrationHash(intent.Body)
	if receipt.OperationID != intent.Operation || receipt.GroupID != intent.Group || receipt.RegistrationSHA256 != wantHash || receipt.AcceptedGeneration != intent.Generation || receipt.InitialEpoch != groupRegistrationInitialEpoch || receipt.NextSequence != groupRegistrationInitialSequence {
		return client.BotGroupRegistrationReceipt{}, ErrGroupRegistrationRecovery
	}
	if err := c.native.GroupRegistered(ctx, intent.Operation, intent.Group, receipt.NextSequence); err != nil {
		return client.BotGroupRegistrationReceipt{}, fmt.Errorf("%w: native registration resolution failed: %w", ErrGroupRegistrationRecovery, err)
	}
	intent.Phase = groupRegistrationComplete
	intent.Receipt = receipt
	if _, err := c.saveGroupRegistration(ctx, key, revision, intent); err != nil {
		return client.BotGroupRegistrationReceipt{}, fmt.Errorf("%w: persist completed registration: %w", ErrGroupRegistrationRecovery, err)
	}
	return receipt, nil
}

func (c *GroupRegistrationCoordinator) saveGroupRegistration(ctx context.Context, key []byte, revision uint64, intent groupRegistrationIntent) (uint64, error) {
	value, err := encodeGroupRegistrationIntent(intent)
	if err != nil {
		return revision, err
	}
	seed := append([]byte("dilla.bot.group-registration.state.tx.v1\x00"), key...)
	var revBytes [8]byte
	binary.BigEndian.PutUint64(revBytes[:], revision)
	seed = append(seed, revBytes[:]...)
	seed = append(seed, value...)
	h := sha256.Sum256(seed)
	var tx id.ID
	copy(tx[:], h[:id.Size])
	revisions, err := c.native.StateTransaction(ctx, tx, []native.StateChange{{Area: groupRegistrationStateArea, Key: key, ExpectedRevision: revision, Value: value}})
	if err != nil {
		return revision, err
	}
	if len(revisions) != 1 || revisions[0] != revision+1 {
		return revision, ErrGroupRegistrationRecovery
	}
	return revisions[0], nil
}

func encodeGroupRegistrationIntent(i groupRegistrationIntent) ([]byte, error) {
	var receipt any
	if i.Phase == groupRegistrationComplete {
		receipt = []any{i.Receipt.OperationID, i.Receipt.GroupID, i.Receipt.RegistrationSHA256[:], i.Receipt.AcceptedGeneration, i.Receipt.InitialEpoch, i.Receipt.NextSequence}
	}
	return cborx.Marshal([]any{uint64(groupRegistrationStateVer), i.Operation, i.Group, i.Target, i.Kind, i.Policy, i.ExternalSender, i.Generation, nullableBytes(i.Body), boolUint(i.Submitted), i.Phase, receipt})
}

func decodeGroupRegistrationIntent(raw []byte) (groupRegistrationIntent, error) {
	var out groupRegistrationIntent
	if cborx.CheckDeterministic(raw) != nil {
		return out, ErrGroupRegistrationRecovery
	}
	var a []any
	if err := cborx.Unmarshal(raw, &a); err != nil || len(a) != 12 {
		return out, ErrGroupRegistrationRecovery
	}
	version, ok := a[0].(uint64)
	if !ok || version != groupRegistrationStateVer {
		return out, ErrGroupRegistrationRecovery
	}
	if out.Operation, ok = registrationID(a[1]); !ok || out.Operation.IsZero() {
		return out, ErrGroupRegistrationRecovery
	}
	if out.Group, ok = registrationID(a[2]); !ok || out.Group.IsZero() {
		return out, ErrGroupRegistrationRecovery
	}
	if out.Target, ok = registrationID(a[3]); !ok || out.Target.IsZero() {
		return out, ErrGroupRegistrationRecovery
	}
	if out.Kind, ok = a[4].(uint64); !ok || out.Kind > 1 {
		return out, ErrGroupRegistrationRecovery
	}
	if out.Policy, ok = a[5].(uint64); !ok || out.Policy > math.MaxInt64 {
		return out, ErrGroupRegistrationRecovery
	}
	if out.ExternalSender, ok = a[6].([]byte); !ok || len(out.ExternalSender) != 32 || allZeroBytes(out.ExternalSender) {
		return out, ErrGroupRegistrationRecovery
	}
	if out.Generation, ok = a[7].(uint64); !ok || out.Generation == 0 || out.Generation > math.MaxInt64 {
		return out, ErrGroupRegistrationRecovery
	}
	if a[8] != nil {
		out.Body, ok = a[8].([]byte)
		if !ok || len(out.Body) == 0 || len(out.Body) > 65505 {
			return out, ErrGroupRegistrationRecovery
		}
	}
	if out.Submitted, ok = uintBool(a[9]); !ok {
		return out, ErrGroupRegistrationRecovery
	}
	if out.Phase, ok = a[10].(uint64); !ok || out.Phase > groupRegistrationComplete {
		return out, ErrGroupRegistrationRecovery
	}
	if out.Phase == groupRegistrationComplete {
		if len(out.Body) == 0 || !out.Submitted {
			return out, ErrGroupRegistrationRecovery
		}
		r, ok := a[11].([]any)
		if !ok || len(r) != 6 {
			return out, ErrGroupRegistrationRecovery
		}
		if out.Receipt.OperationID, ok = registrationID(r[0]); !ok {
			return out, ErrGroupRegistrationRecovery
		}
		if out.Receipt.GroupID, ok = registrationID(r[1]); !ok {
			return out, ErrGroupRegistrationRecovery
		}
		hash, ok := r[2].([]byte)
		if !ok || len(hash) != sha256.Size {
			return out, ErrGroupRegistrationRecovery
		}
		copy(out.Receipt.RegistrationSHA256[:], hash)
		if out.Receipt.AcceptedGeneration, ok = r[3].(uint64); !ok {
			return out, ErrGroupRegistrationRecovery
		}
		if out.Receipt.InitialEpoch, ok = r[4].(uint64); !ok {
			return out, ErrGroupRegistrationRecovery
		}
		if out.Receipt.NextSequence, ok = r[5].(uint64); !ok || out.Receipt.NextSequence != groupRegistrationInitialSequence {
			return out, ErrGroupRegistrationRecovery
		}
		if out.Receipt.OperationID != out.Operation || out.Receipt.GroupID != out.Group || out.Receipt.AcceptedGeneration != out.Generation || out.Receipt.InitialEpoch != groupRegistrationInitialEpoch || len(out.Body) == 0 || out.Receipt.RegistrationSHA256 != groupRegistrationHash(out.Body) {
			return out, ErrGroupRegistrationRecovery
		}
	} else if a[11] != nil || out.Submitted && len(out.Body) == 0 {
		return out, ErrGroupRegistrationRecovery
	}
	return out, nil
}

func groupRegistrationHash(body []byte) [32]byte {
	h := sha256.New()
	_, _ = h.Write([]byte("dilla bot group registration v1\x00"))
	_, _ = h.Write(body)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func groupRegistrationKey(group id.ID) []byte {
	return []byte(groupRegistrationStateKey + group.String())
}
func nullableBytes(v []byte) any {
	if v == nil {
		return nil
	}
	return append([]byte(nil), v...)
}
func registrationID(v any) (id.ID, bool) {
	switch x := v.(type) {
	case id.ID:
		return x, true
	case []byte:
		if len(x) == id.Size {
			var out id.ID
			copy(out[:], x)
			return out, true
		}
	}
	return id.ID{}, false
}
func allZeroBytes(v []byte) bool {
	for _, b := range v {
		if b != 0 {
			return false
		}
	}
	return true
}

func apiNotFound(err error) bool {
	var apiErr *client.APIError
	return errors.As(err, &apiErr) && apiErr.Status == 404 && apiErr.Code == "E_NOT_FOUND"
}

func isDefinitiveRegistrationRefusal(err error) bool {
	var apiErr *client.APIError
	return errors.As(err, &apiErr) && apiErr.Outcome == client.OutcomeDefinitive && apiErr.Status >= 400 && apiErr.Status < 500
}
