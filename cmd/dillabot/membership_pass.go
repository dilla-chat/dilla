//go:build linux

package main

import (
	"context"
	"errors"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/service"
)

// installationMembership is the installation's membership pass (Bot v2): Welcomes to Bot channels' groups, the groups
// synced, the groups of other channels forgotten.
type installationMembership interface {
	ReconcileOnce(context.Context, *client.Session) (service.MembershipResult, error)
}

// reconcileReadiness runs the installation's membership pass and then marks the installation ready once dillad's
// prerequisites (the Bot channels' joins) are met.
func reconcileReadiness(ctx context.Context, installation client.Installation, session *client.Session, membership installationMembership) (ready bool, stage string, err error) {
	stage = "membership_source_sync"
	if session == nil {
		return false, stage, errors.New("identity lifecycle returned no scoped device session")
	}
	if _, err = membership.ReconcileOnce(ctx, session); err != nil {
		return false, stage, err
	}
	stage = "readiness"
	ready, err = markReadyAfterMembership(ctx, session, installation)
	return ready, stage, err
}

// groupEventPoller drains the helper's event journal (*service.GroupEventPoller).
type groupEventPoller interface {
	PollOnce(context.Context, service.GroupEventHandler) (service.GroupEventPollResult, error)
}

// ignoredGroupEvents drains the journal without acting on an event: a channel message is never a command (Bot v2 Task
// 15: commands are sealed requests), and nothing else reads the journal since Task 23 removed the v1 intake.
type ignoredGroupEvents struct{}

func (ignoredGroupEvents) HandleGroupEvent(context.Context, native.GroupEvent) error { return nil }

// newTextPoller is an installation's hint-driven pass: membership (Welcomes, Bot channel groups), the gateway's group
// subscriptions, and one page of the event journal drained.
func newTextPoller(membership installationMembership, session *client.Session, subscribe func(context.Context) error, events groupEventPoller) func(context.Context) error {
	return func(ctx context.Context) error {
		if _, err := membership.ReconcileOnce(ctx, session); err != nil {
			return err
		}
		if err := subscribe(ctx); err != nil {
			return err
		}
		result, err := events.PollOnce(ctx, ignoredGroupEvents{})
		if err == nil && result.RateLimited != nil {
			return result.RateLimited
		}
		return err
	}
}
