//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/id"
)

const maxRestoredInteractionGroups = 2048

type interactionRestoreNative interface {
	Binding() native.Binding
	InteractionGroups(context.Context) ([]native.InteractionGroupDescriptor, error)
	InteractionHealSnapshot(context.Context, id.ID) (native.InteractionHealSnapshot, error)
}

type interactionRestoreSession interface {
	BotInteractionHealStatus(context.Context, id.ID, id.ID) (client.InteractionHealStatus, error)
	BotInteractionHeal(context.Context, id.ID, id.ID, []byte, [][]any, []byte) (client.HealResult, error)
}

// healRestoredInteractionGroups runs after ordinary membership, trust and readiness,
// before private source sync. A restored private group cannot pass the current-group
// read fence until its MLS snapshot is revalidated through the dedicated heal route.
func healRestoredInteractionGroups(ctx context.Context, source interactionRestoreNative, session interactionRestoreSession) (int, error) {
	if source == nil || session == nil {
		return 0, errors.New("interaction restore dependencies are unavailable")
	}
	binding := source.Binding()
	if binding.Instance.IsZero() || binding.Community.IsZero() || binding.Installation.IsZero() {
		return 0, errors.New("interaction restore binding is invalid")
	}
	groups, err := source.InteractionGroups(ctx)
	if err != nil {
		return 0, fmt.Errorf("interaction restore discovery: %w", err)
	}
	if len(groups) > maxRestoredInteractionGroups {
		return 0, errors.New("interaction restore group limit exceeded")
	}
	seen := make(map[id.ID]struct{}, len(groups))
	healed := 0
	for _, group := range groups {
		if err := ctx.Err(); err != nil {
			return healed, err
		}
		if group.Group.IsZero() || group.BotDevice.IsZero() || group.Binding.BotUser.IsZero() ||
			group.Binding.ParticipantUser.IsZero() || group.Binding.ParticipantDevice.IsZero() ||
			group.Binding.Instance != binding.Instance || group.Binding.Community != binding.Community ||
			group.Binding.Installation != binding.Installation || group.Binding.PolicyVersion == 0 ||
			group.Binding.E2EEVersion != 1 || group.Binding.MediaVersion != 0 {
			return healed, errors.New("interaction restore descriptor is outside native binding")
		}
		if _, duplicate := seen[group.Group]; duplicate {
			return healed, errors.New("duplicate interaction restore descriptor")
		}
		seen[group.Group] = struct{}{}
		if group.State != 2 {
			continue
		}
		status, err := session.BotInteractionHealStatus(ctx, binding.Installation, group.Group)
		if err != nil {
			return healed, fmt.Errorf("interaction restore status: %w", err)
		}
		if !status.NeedsHeal {
			continue
		}
		if status.Epoch > group.Epoch || status.NextSequence == 0 || status.NeedFromSequence == 0 {
			return healed, errors.New("interaction restore server state exceeds native snapshot")
		}
		snapshot, err := source.InteractionHealSnapshot(ctx, group.Group)
		if err != nil {
			return healed, fmt.Errorf("interaction restore native snapshot: %w", err)
		}
		if len(snapshot.GroupInfo) == 0 || len(snapshot.RatchetTree) == 0 || len(snapshot.Tail) > 64 {
			return healed, errors.New("interaction restore snapshot is invalid")
		}
		result, err := session.BotInteractionHeal(ctx, binding.Installation, group.Group,
			snapshot.GroupInfo, snapshot.Tail, snapshot.RatchetTree)
		if err != nil {
			return healed, fmt.Errorf("interaction restore heal: %w", err)
		}
		if result.Epoch < group.Epoch || result.NextSequence == 0 {
			return healed, errors.New("interaction restore heal result is invalid")
		}
		verified, err := session.BotInteractionHealStatus(ctx, binding.Installation, group.Group)
		if err != nil || verified.NeedsHeal || verified.Epoch != result.Epoch || verified.NextSequence != result.NextSequence {
			return healed, errors.New("interaction restore result was not confirmed")
		}
		healed++
	}
	return healed, nil
}
