package main

import (
	"context"
	"errors"

	"github.com/dilla-chat/dilla/internal/bot/service"
)

const maxStructuralEventPollPasses = 4

type structuralEventPoll func(context.Context) (service.StructuralEventPollResult, error)

// finishStructuralGap leaves the old event cursor untouched until the current
// member scan is durable and the server confirms that its fixed high-water is
// still readable. The caller retries an incomplete scan on its next pass.
func finishStructuralGap(
	ctx context.Context,
	gap *service.StructuralEventGapError,
	stage func(context.Context, uint64, uint64) (service.StructuralGapRecoveryResult, error),
	complete func(context.Context, uint64) error,
) (bool, error) {
	if ctx == nil || gap == nil || stage == nil || complete == nil {
		return false, errors.New("invalid structural gap recovery")
	}
	result, err := stage(ctx, gap.Floor, gap.HighWater)
	if err != nil || !result.Done {
		return false, err
	}
	if err = complete(ctx, result.HighWater); err != nil {
		return false, err
	}
	return true, nil
}

// drainStructuralEvents follows the poller's bounded More signal for a small,
// fixed number of passes so a busy installation cannot monopolize reconciliation.
func drainStructuralEvents(ctx context.Context, poll structuralEventPoll) (service.StructuralEventPollResult, error) {
	var total service.StructuralEventPollResult
	if ctx == nil || poll == nil {
		return total, errors.New("invalid structural event runtime poll")
	}
	for pass := 0; pass < maxStructuralEventPollPasses; pass++ {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		result, err := poll(ctx)
		total.Processed += result.Processed
		total.Pages += result.Pages
		if result.AcknowledgedThrough > total.AcknowledgedThrough {
			total.AcknowledgedThrough = result.AcknowledgedThrough
		}
		if err != nil {
			return total, err
		}
		total.More = result.More
		if !result.More {
			break
		}
	}
	return total, nil
}
