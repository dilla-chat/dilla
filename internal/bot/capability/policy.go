// Package capability implements the community bot wire-compatibility policy shared by HTTP,
// the delivery service and the gateway.
package capability

import (
	"context"
	"errors"
	"fmt"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// Source is the narrow, repository-backed lookup the policy needs. Implementations must read
// committed state for each call; Policy deliberately does not cache installation status.
type Source interface {
	GetActiveBotInstallationByCommunity(context.Context, id.ID) (store.BotInstallationRow, error)
}

// ErrConfiguration identifies a missing policy source. A scoped request must fail closed when
// the policy has not been wired.
var ErrConfiguration = errors.New("bot capability: policy source is not configured")

// UpgradeRequired is the typed refusal for an authenticated wire version that cannot represent
// an interaction or a community with a live bot installation. It contains no resource identifiers.
type UpgradeRequired struct{}

func (*UpgradeRequired) Error() string { return "wire version 3 is required" }

// Policy checks the current committed installation state on every admission. The source is private
// so callers cannot replace the authority after construction.
type Policy struct {
	source Source
}

// New constructs a policy over the authoritative installation repository. A nil source is kept
// as an explicit fail-closed configuration error so it is safe to assemble optional consumers.
func New(source Source) *Policy { return &Policy{source: source} }

// Enabled reports whether a community currently has a nonremoved, non-disabled installation.
// Pending, enrolling, joining, ready and error are all enabled states.
func (p *Policy) Enabled(ctx context.Context, communityID id.ID) (bool, error) {
	if p == nil || p.source == nil {
		return false, ErrConfiguration
	}
	row, err := p.source.GetActiveBotInstallationByCommunity(ctx, communityID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read bot installation state: %w", err)
	}
	return row.RemovedAt == nil && row.State != store.BotInstallationDisabled, nil
}

// RequireCommunity admits wire 3 unconditionally, and otherwise requires it only when the
// community's current committed installation is enabled. Nil community scope is unscoped.
func (p *Policy) RequireCommunity(ctx context.Context, wireVersion uint64, communityID *id.ID) error {
	if communityID == nil {
		return nil
	}
	if p == nil || p.source == nil {
		return ErrConfiguration
	}
	if wireVersion >= 3 {
		return nil
	}
	enabled, err := p.Enabled(ctx, *communityID)
	if err != nil {
		return err
	}
	if enabled {
		return &UpgradeRequired{}
	}
	return nil
}

// RequireGroup applies the community policy and also enforces wire 3 for interaction groups,
// whose bindings intentionally have no community scope.
func (p *Policy) RequireGroup(ctx context.Context, wireVersion uint64, groupKind uint8, communityID *id.ID) error {
	if wireVersion >= 3 {
		return nil
	}
	if groupKind == 3 {
		return &UpgradeRequired{}
	}
	return p.RequireCommunity(ctx, wireVersion, communityID)
}

// RequireGroupTx is RequireGroup evaluated against a caller's transaction so a
// group-row fence also protects the community compatibility decision.
func (p *Policy) RequireGroupTx(ctx context.Context, tx store.Repository, wireVersion uint64, groupKind uint8, communityID *id.ID) error {
	if wireVersion >= 3 {
		return nil
	}
	if groupKind == 3 {
		return &UpgradeRequired{}
	}
	if communityID == nil {
		return nil
	}
	if p == nil || p.source == nil {
		return ErrConfiguration
	}
	row, err := tx.GetActiveBotInstallationByCommunity(ctx, *communityID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read bot installation state: %w", err)
	}
	if row.RemovedAt == nil && row.State != store.BotInstallationDisabled {
		return &UpgradeRequired{}
	}
	return nil
}
