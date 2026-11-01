//go:build linux

package main

import (
	"context"
	"errors"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/service"
	"github.com/jonasthim/dilla/internal/id"
)

type installationMembership interface {
	ReconcileOnce(context.Context, *client.Session) (service.MembershipResult, error)
	ReconcileInteractionGroupsOnce(context.Context, *client.Session) (service.MembershipResult, error)
}

// reconcileMembershipReadiness keeps the installation's membership, trust and
// private-source stages together so every pass uses the same bound session.
//
// The trust stage is the same per-person isolating pass command intake runs (refreshTrust): a
// sender whose signed device-list history is stale is returned in unrefreshed, and the caller
// holds that sender's queued events when it drains (DrainHolding), so one stale member cannot
// freeze the installation after a restart. Every other trust failure stays fatal to the pass.
func reconcileMembershipReadiness(ctx context.Context, installation client.Installation, source interactionRestoreNative, session *client.Session, membership installationMembership, trust intakeTrust, report func(service.TrustRefresh)) (ready bool, healed int, stage string, unrefreshed map[id.ID]bool, err error) {
	stage = "membership_source_sync"
	if session == nil {
		return false, 0, stage, nil, errors.New("identity lifecycle returned no scoped device session")
	}
	_, err = membership.ReconcileOnce(ctx, session)
	if err != nil {
		return
	}
	stage = "membership_trust"
	// Newly joined MLS members require checked signed device-list history.
	unrefreshed, err = refreshTrust(ctx, trust, session, report)
	if err != nil {
		return
	}
	stage = "readiness"
	ready, err = markReadyAfterMembership(ctx, session, installation)
	if err != nil || !ready {
		return
	}
	// A re-enabled installation is Joining even when its private groups are
	// unchanged. Both restore and source reads require the server's Ready state;
	// attempting either before MarkReady prevents this pass from ever reaching it.
	stage = "interaction_restore"
	healed, err = healRestoredInteractionGroups(ctx, source, session)
	if err != nil {
		return
	}
	stage = "interaction_source_sync"
	_, err = membership.ReconcileInteractionGroupsOnce(ctx, session)
	return
}
