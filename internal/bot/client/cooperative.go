package client

import (
	"context"
	"errors"
)

// ErrCooperativeStop ends a worker turn at a safe boundary. It never cancels
// the context of an in-flight HTTP exchange or native helper transaction.
var ErrCooperativeStop = errors.New("bot command turn stopped cooperatively")

type cooperativeStopKey struct{}

func WithCooperativeStop(ctx context.Context, stop func() bool) context.Context {
	return context.WithValue(ctx, cooperativeStopKey{}, stop)
}
func WithoutCooperativeStop(ctx context.Context) context.Context {
	return WithCooperativeStop(ctx, nil)
}
func CooperativeCheckpoint(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	stop, _ := ctx.Value(cooperativeStopKey{}).(func() bool)
	if stop != nil && stop() {
		return ErrCooperativeStop
	}
	return nil
}

// WithBackground clears request priority, preserving cancellation and safe
// worker checkpoints. Long-lived work must separately choose its own lifetime.
func WithBackground(ctx context.Context) context.Context {
	ctx = context.WithValue(ctx, immediateControlContextKey{}, false)
	return context.WithValue(ctx, interactiveContextKey{}, false)
}
