package service

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

const (
	maxSenderHistoryVersions = 1024
	maxSenderHistoryBytes    = 4 << 20
	maxSenderOuterRowBytes   = 1 << 20
	senderHistoryPageSize    = 64
)

var ErrSenderHistoryStale = errors.New("sender device-list history is stale")

var ErrSourceTrustBusy = errors.New("sender trust refresh is already active")

// TrustPhase is a fixed, non-sensitive trust-refresh substage for diagnostics.
type TrustPhase uint8

const (
	TrustPhaseUnknown TrustPhase = iota
	TrustPhasePendingLeaves
	TrustPhaseSenderHistory
	TrustPhaseSenderTrust
	TrustPhaseGroupRevalidation
)

func (p TrustPhase) String() string {
	switch p {
	case TrustPhasePendingLeaves:
		return "pending_leaves"
	case TrustPhaseSenderHistory:
		return "sender_history"
	case TrustPhaseSenderTrust:
		return "sender_trust"
	case TrustPhaseGroupRevalidation:
		return "group_revalidation"
	default:
		return "unknown"
	}
}

// TrustPhaseError preserves a cause for programmatic inspection but never formats it.
type TrustPhaseError struct {
	Phase TrustPhase
	err   error
}

func (e *TrustPhaseError) Error() string { return "sender trust refresh failed" }
func (e *TrustPhaseError) Unwrap() error { return e.err }

func trustPhaseError(phase TrustPhase, err error) error {
	if err == nil {
		return nil
	}
	return &TrustPhaseError{Phase: phase, err: err}
}

type senderHistorySession interface {
	OwnDeviceList(context.Context, id.ID) ([]byte, error)
	DeviceListHistory(context.Context, id.ID, uint64) ([][]any, error)
}

type senderTrustNative interface {
	Binding() native.Binding
	PendingLeaves(context.Context) ([]native.GroupPendingLeaf, error)
	ProposedAdds(context.Context, id.ID, []byte) ([]native.GroupProposedAdd, error)
	TrustSender(context.Context, id.ID, uint64, id.ID, []byte, []byte, uint64) (native.GroupTrustVerdict, error)
	TrustProposedAdd(context.Context, id.ID, []byte, uint64, id.ID, []byte, []byte, uint64) (native.GroupTrustVerdict, error)
	RevalidateGroup(context.Context, id.ID, uint64, uint64) (uint32, error)
}

// SourceTrust reads bounded signed device-list history and delegates every signature, leaf,
// current-membership and key-binding decision to the native core.
type SourceTrust struct {
	native  senderTrustNative
	binding native.Binding
	now     func() time.Time
	mu      sync.Mutex
	active  map[id.ID]bool
}

// NewSourceTrust binds trust recovery to one opened native identity.
func NewSourceTrust(n senderTrustNative) (*SourceTrust, error) {
	if n == nil {
		return nil, errors.New("missing native client")
	}
	return newSourceTrust(n)
}

func newSourceTrust(n senderTrustNative) (*SourceTrust, error) {
	if n == nil {
		return nil, errors.New("missing native client")
	}
	binding := n.Binding()
	if binding.Instance.IsZero() || binding.Community.IsZero() || binding.Installation.IsZero() {
		return nil, errors.New("invalid native binding")
	}
	return &SourceTrust{native: n, binding: binding, now: time.Now}, nil
}

// TrustPendingLeaves refreshes current sender trust for the helper's bounded pending-leaf list.
// It does not drain or acknowledge authenticated events.
func (t *SourceTrust) TrustPendingLeaves(ctx context.Context, session *client.Session) (uint32, error) {
	if session == nil {
		return 0, errors.New("missing device session")
	}
	return t.trustPendingLeaves(ctx, session)
}

func (t *SourceTrust) trustPendingLeaves(ctx context.Context, session senderHistorySession) (uint32, error) {
	refresh, err := t.pendingLeaves(ctx, session, false)
	return refresh.Recovered, err
}

// TrustRefresh is the outcome of a trust pass that isolates per-sender failures. Unrefreshed
// holds the users whose signed device-list history could not be brought current: nothing was
// applied for them, so a revocation or key change they published may not be processed yet. Their
// new messages stay held by the native core as pending leaves; a caller that drains events must
// also hold their already-queued events (HoldUnrefreshedSenders).
type TrustRefresh struct {
	Recovered   uint32
	Unrefreshed map[id.ID]bool
	// Busy holds senders already being refreshed by another pass, without aging their events.
	Busy map[id.ID]bool
}

// RefreshPendingLeaves is TrustPendingLeaves for command intake: a failure scoped to one sender
// (its signed device-list history is stale) leaves that sender unrefreshed. A sender
// owned by another refresh is reported separately in Busy. The pass continues
// with the others. Every other failure (reading the pending leaves, a server or API failure, a
// native-core refusal or limit, group revalidation, the clock) stays fatal to the pass.
func (t *SourceTrust) RefreshPendingLeaves(ctx context.Context, session *client.Session) (TrustRefresh, error) {
	if session == nil {
		return TrustRefresh{}, errors.New("missing device session")
	}
	return t.pendingLeaves(ctx, session, true)
}

func (t *SourceTrust) refreshPendingLeaves(ctx context.Context, session senderHistorySession) (TrustRefresh, error) {
	return t.pendingLeaves(ctx, session, true)
}

// senderScopedTrustFailure is true only for a stale signed device-list history read for one
// sender: the sender's own published data, not the server, the core or the clock.
func senderScopedTrustFailure(err error) bool {
	var phase *TrustPhaseError
	return errors.As(err, &phase) && phase.Phase == TrustPhaseSenderHistory && errors.Is(err, ErrSenderHistoryStale)
}

func (t *SourceTrust) pendingLeaves(ctx context.Context, session senderHistorySession, isolate bool) (TrustRefresh, error) {
	var out TrustRefresh
	if err := client.CooperativeCheckpoint(ctx); err != nil {
		return out, err
	}
	leaves, err := t.native.PendingLeaves(ctx)
	if err != nil {
		return out, trustPhaseError(TrustPhasePendingLeaves, err)
	}
	var recovered uint64
	refreshedUsers := make(map[id.ID]bool, len(leaves))
	for _, leaf := range leaves {
		if err = client.CooperativeCheckpoint(ctx); err != nil {
			return TrustRefresh{}, err
		}
		if out.Unrefreshed[leaf.User] || out.Busy[leaf.User] {
			continue // this sender is stale or busy; its leaves stay pending in the core
		}
		// TrustSender validates the signed list against every current leaf for this user and
		// updates the shared cached list. A second pending row from this snapshot would still
		// carry the old cached version and be rejected as stale by the native core. Every
		// row still gets its own group revalidation so pending events are not left behind.
		if refreshedUsers[leaf.User] {
			n, e := t.native.RevalidateGroup(ctx, leaf.Group, uint64(leaf.Leaf), 256)
			if e != nil {
				return TrustRefresh{}, trustPhaseError(TrustPhaseGroupRevalidation, e)
			}
			recovered += uint64(n)
			if recovered > math.MaxUint32 {
				return TrustRefresh{}, errors.New("sender revalidation count overflow")
			}
			continue
		}
		n, e := t.trustSender(ctx, session, leaf.Group, uint64(leaf.Leaf), leaf.User, leaf.CachedVersion)
		if e != nil {
			if errors.Is(e, client.ErrCooperativeStop) {
				return TrustRefresh{}, e
			}
			if !isolate || ctx.Err() != nil || !senderScopedTrustFailure(e) && !errors.Is(e, ErrSourceTrustBusy) {
				return TrustRefresh{}, e
			}
			if errors.Is(e, ErrSourceTrustBusy) {
				if out.Busy == nil {
					out.Busy = map[id.ID]bool{}
				}
				out.Busy[leaf.User] = true
			} else {
				if out.Unrefreshed == nil {
					out.Unrefreshed = map[id.ID]bool{}
				}
				out.Unrefreshed[leaf.User] = true
			}
			continue
		}
		refreshedUsers[leaf.User] = true
		recovered += uint64(n)
		if recovered > math.MaxUint32 {
			return TrustRefresh{}, errors.New("sender revalidation count overflow")
		}
	}
	out.Recovered = uint32(recovered)
	return out, nil
}

// TrustSender refreshes one current leaf against a re-read current server head, then revalidates
// durable authenticated events under the new trust state.
func (t *SourceTrust) TrustSender(ctx context.Context, session *client.Session, group id.ID, leaf uint64, user id.ID, cachedVersion uint64) (uint32, error) {
	if session == nil {
		return 0, errors.New("missing device session")
	}
	return t.trustSender(ctx, session, group, leaf, user, cachedVersion)
}

func (t *SourceTrust) trustSender(ctx context.Context, session senderHistorySession, group id.ID, leaf uint64, user id.ID, cachedVersion uint64) (uint32, error) {
	if group.IsZero() || user.IsZero() || leaf > math.MaxUint32 {
		return 0, errors.New("invalid sender trust target")
	}
	release, claimed := t.claimUser(user)
	if !claimed {
		return 0, ErrSourceTrustBusy
	}
	defer release()
	history, head, _, err := readSenderHistory(ctx, session, user, cachedVersion)
	if err != nil {
		return 0, trustPhaseError(TrustPhaseSenderHistory, err)
	}
	now := t.now().Unix()
	if now <= 0 {
		return 0, ErrSenderHistoryStale
	}
	if _, err = t.native.TrustSender(ctx, group, leaf, user, history, head, uint64(now)); err != nil {
		return 0, trustPhaseError(TrustPhaseSenderTrust, err)
	}
	revalidated, err := t.native.RevalidateGroup(ctx, group, leaf, 256)
	if err != nil {
		return 0, trustPhaseError(TrustPhaseGroupRevalidation, err)
	}
	return revalidated, nil
}

// TrustProposedAdds validates each currently proposed Add against that user's signed list history.
// The native core remains responsible for whether the proposed key package is admissible.
func (t *SourceTrust) TrustProposedAdds(ctx context.Context, session *client.Session, group id.ID, proposals []byte) error {
	if session == nil {
		return errors.New("missing device session")
	}
	return t.trustProposedAdds(ctx, session, group, proposals)
}

func (t *SourceTrust) trustProposedAdds(ctx context.Context, session senderHistorySession, group id.ID, proposals []byte) error {
	if group.IsZero() || len(proposals) == 0 {
		return errors.New("invalid proposed-add trust target")
	}
	adds, err := t.native.ProposedAdds(ctx, group, proposals)
	if err != nil {
		return err
	}
	for _, add := range adds {
		if err = client.CooperativeCheckpoint(ctx); err != nil {
			return err
		}
		if e := t.trustProposedAdd(ctx, session, group, proposals, add); e != nil {
			return e
		}
	}
	return nil
}

func (t *SourceTrust) trustProposedAdd(ctx context.Context, session senderHistorySession, group id.ID, proposals []byte, add native.GroupProposedAdd) error {
	release, claimed := t.claimUser(add.User)
	if !claimed {
		return ErrSourceTrustBusy
	}
	defer release()
	history, head, _, err := readSenderHistory(ctx, session, add.User, add.CachedVersion)
	if err != nil {
		return err
	}
	now := t.now().Unix()
	if now <= 0 {
		return ErrSenderHistoryStale
	}
	_, err = t.native.TrustProposedAdd(ctx, group, proposals, uint64(add.Index), add.User, history, head, uint64(now))
	return err
}

// The native sender cache is shared by every group for a user. Claim only that
// user, releasing the mutex before any HTTP/helper call so unrelated senders proceed.
func (t *SourceTrust) claimUser(user id.ID) (func(), bool) {
	if shared, ok := t.native.(interface{ TryClaimTrust(id.ID) (func(), bool) }); ok {
		return shared.TryClaimTrust(user)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.active[user] {
		return nil, false
	}
	if t.active == nil {
		t.active = make(map[id.ID]bool)
	}
	t.active[user] = true
	return func() {
		t.mu.Lock()
		delete(t.active, user)
		t.mu.Unlock()
	}, true
}

// readSenderHistory returns the encoded outer history rows and the candidate head's INNER signed
// list. The candidate is re-read after collection to bound head races and prevent stale trust.
func readSenderHistory(ctx context.Context, session senderHistorySession, user id.ID, cachedVersion uint64) ([]byte, []byte, uint64, error) {
	if session == nil || user.IsZero() || cachedVersion > math.MaxInt64 {
		return nil, nil, 0, ErrSenderHistoryStale
	}
	var rowsUsed, bytesUsed uint64
	for attempt := 0; attempt < 2; attempt++ {
		if err := client.CooperativeCheckpoint(ctx); err != nil {
			return nil, nil, 0, err
		}
		candidateRaw, err := session.OwnDeviceList(ctx, user)
		if err != nil {
			return nil, nil, 0, err
		}
		candidate, version, err := decodeSenderOuter(candidateRaw)
		if err != nil || version < cachedVersion || version-cachedVersion > maxSenderHistoryVersions {
			return nil, nil, 0, ErrSenderHistoryStale
		}
		if uint64(len(candidateRaw)) > maxSenderOuterRowBytes {
			return nil, nil, 0, ErrSenderHistoryStale
		}
		candidateBytes := uint64(len(candidateRaw))
		if bytesUsed+candidateBytes > maxSenderHistoryBytes {
			return nil, nil, 0, ErrSenderHistoryStale
		}
		bytesUsed += candidateBytes
		collected := make([]any, 0, version-cachedVersion+1)
		after := uint64(0)
		if cachedVersion > 0 {
			after = cachedVersion - 1
		}
		overranCandidate := false
		if version == cachedVersion {
			collected = append(collected, candidate)
			rowsUsed++
		} else {
			for after < version {
				if err = client.CooperativeCheckpoint(ctx); err != nil {
					return nil, nil, 0, err
				}
				page, pageErr := session.DeviceListHistory(ctx, user, after)
				if pageErr != nil {
					return nil, nil, 0, pageErr
				}
				if len(page) == 0 || len(page) > senderHistoryPageSize {
					return nil, nil, 0, ErrSenderHistoryStale
				}
				for _, row := range page {
					rowVersion, rowRaw, rowErr := encodeSenderOuter(row)
					if rowErr != nil || rowVersion != after+1 {
						return nil, nil, 0, ErrSenderHistoryStale
					}
					if rowVersion > version {
						overranCandidate = true
						break
					}
					rowsUsed++
					bytesUsed += uint64(len(rowRaw))
					if rowsUsed > maxSenderHistoryVersions || bytesUsed > maxSenderHistoryBytes {
						return nil, nil, 0, ErrSenderHistoryStale
					}
					collected = append(collected, row)
					after = rowVersion
				}
				if overranCandidate {
					break
				}
			}
		}
		if rowsUsed > maxSenderHistoryVersions || bytesUsed > maxSenderHistoryBytes {
			return nil, nil, 0, ErrSenderHistoryStale
		}
		if !overranCandidate {
			if len(collected) == 0 {
				return nil, nil, 0, ErrSenderHistoryStale
			}
			lastRow, ok := collected[len(collected)-1].([]any)
			if !ok {
				return nil, nil, 0, ErrSenderHistoryStale
			}
			lastVersion, lastRaw, lastErr := encodeSenderOuter(lastRow)
			if lastErr != nil || lastVersion != version || !bytesEqual(lastRaw, candidateRaw) {
				return nil, nil, 0, ErrSenderHistoryStale
			}
		}
		currentRaw, readErr := session.OwnDeviceList(ctx, user)
		if readErr != nil {
			return nil, nil, 0, readErr
		}
		current, currentVersion, decodeErr := decodeSenderOuter(currentRaw)
		if decodeErr == nil && currentVersion == version && bytesEqual(currentRaw, candidateRaw) && !overranCandidate {
			innerValue, ok := candidate[1].([]byte)
			if !ok {
				return nil, nil, 0, ErrSenderHistoryStale
			}
			inner := append([]byte(nil), innerValue...)
			body, marshalErr := cborx.Marshal(collected)
			if marshalErr != nil || len(body) > maxSenderHistoryBytes {
				return nil, nil, 0, ErrSenderHistoryStale
			}
			return body, inner, version, nil
		}
		// Only a moving forward head justifies the one bounded retry. A rollback, malformed head,
		// or changed bytes at the same version is stale and must never be trusted.
		if decodeErr != nil || currentVersion <= version {
			return nil, nil, 0, ErrSenderHistoryStale
		}
		_ = current
	}
	return nil, nil, 0, ErrSenderHistoryStale
}

func decodeSenderOuter(raw []byte) ([]any, uint64, error) {
	if len(raw) == 0 || len(raw) > maxSenderOuterRowBytes {
		return nil, 0, ErrSenderHistoryStale
	}
	var row []any
	if err := cborx.Unmarshal(raw, &row); err != nil {
		return nil, 0, ErrSenderHistoryStale
	}
	version, _, err := encodeSenderOuter(row)
	if err != nil {
		return nil, 0, err
	}
	return row, version, nil
}

func encodeSenderOuter(row []any) (uint64, []byte, error) {
	if len(row) != 4 {
		return 0, nil, ErrSenderHistoryStale
	}
	version, ok := row[0].(uint64)
	if !ok || version == 0 || version > math.MaxInt64 {
		return 0, nil, ErrSenderHistoryStale
	}
	if _, ok = row[1].([]byte); !ok {
		return 0, nil, ErrSenderHistoryStale
	}
	signature, ok := row[2].([]byte)
	if !ok || len(signature) != 64 {
		return 0, nil, ErrSenderHistoryStale
	}
	previous, ok := row[3].([]byte)
	if !ok || len(previous) != 32 {
		return 0, nil, ErrSenderHistoryStale
	}
	raw, err := cborx.Marshal(row)
	if err != nil || len(raw) > maxSenderOuterRowBytes {
		return 0, nil, ErrSenderHistoryStale
	}
	return version, raw, nil
}

func bytesEqual(a, b []byte) bool {
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
