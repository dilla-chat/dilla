//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
)

// Fix round, I-2: one installation's helper start (an Open running the one-off v1 cleanup, up to 10 min) runs off the
// Bot's loop. While it is in flight, the loop runs another installation's passes, the due-work ticks and the text
// poll; the installation itself gets no second start, and its start asks for the pass that takes its result.
func TestAHelperStartInFlightDoesNotHoldTheLoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		r := newBotGatewayRuntime(ctx, nil, true, quietLogger())
		slow, other := id.New(), id.New()
		release := make(chan struct{})
		var starts, dueTicks, polls atomic.Int32
		openFailed := errors.New("open failed")
		var mu sync.Mutex // the pass's records, read by the test
		var otherPasses, slowPending int
		var slowStage string
		var slowErr error
		wake := make(chan struct{}, 1)
		loop := botLoop{clock: clock.System(), log: quietLogger(), passes: r.Pass(), take: r.takePassTargets,
			reload: make(chan os.Signal), wake: wake,
			poll:     func(context.Context) { polls.Add(1) },
			maintain: func(context.Context) error { dueTicks.Add(1); return nil },
			due:      dueInterval,
			pass: func(_ context.Context, targets map[id.ID]bool) error {
				mu.Lock()
				defer mu.Unlock()
				if targets == nil || targets[other] {
					otherPasses++
				}
				if targets == nil || targets[slow] {
					// As reconcile does for an installation with no usable retained helper.
					client, pending, err := passHelper(&r.starts, slow)
					if err != nil {
						slowStage, slowErr = "helper_start", err
						return nil
					}
					if client == nil && !pending {
						beginPassHelper(&r.starts, slow, make([]byte, 32), func(startCtx context.Context, key []byte) (*native.Client, error) {
							starts.Add(1)
							select {
							case <-release:
								return nil, openFailed
							case <-startCtx.Done():
								return nil, startCtx.Err()
							}
						})
						pending = true
					}
					if pending {
						slowPending++
						return &passIncomplete{after: map[id.ID]time.Duration{slow: helperStartRecheck}}
					}
				}
				return nil
			}}
		finished := make(chan struct{})
		go func() { _ = loop.run(ctx); close(finished) }()
		defer func() {
			cancel()
			<-finished
			r.Close()
		}()
		snapshot := func() (int, int, string, error) {
			mu.Lock()
			defer mu.Unlock()
			return otherPasses, slowPending, slowStage, slowErr
		}
		time.Sleep(time.Second)
		if _, pending, _, _ := snapshot(); starts.Load() != 1 || pending != 1 {
			t.Fatalf("start pass: starts=%d pending=%d, want the slow installation's start begun and left pending", starts.Load(), pending)
		}
		// Three minutes into the slow start: the other installation's passes, the due ticks and the text poll all run.
		for range 3 {
			r.requestPass(other)
			r.requestPass(slow) // asked again while its start is in flight: no second start
			time.Sleep(time.Minute)
		}
		wake <- struct{}{}
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if passes, _, _, _ := snapshot(); passes < 4 {
			t.Fatalf("the other installation ran %d passes while the slow start was in flight, want the start pass and 3 more", passes)
		}
		if got := dueTicks.Load(); got < 18 {
			t.Fatalf("%d due ticks in 3 min of a slow start, want one every %v", got, dueInterval)
		}
		if polls.Load() != 1 {
			t.Fatalf("the text poll ran %d times while the slow start was in flight, want 1", polls.Load())
		}
		if starts.Load() != 1 {
			t.Fatalf("%d starts of one installation while one was in flight, want 1", starts.Load())
		}
		// The start ends (here: its Open fails); it asks for a pass of its installation, which takes the result.
		close(release)
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if _, _, stage, err := snapshot(); stage != "helper_start" || !errors.Is(err, openFailed) {
			t.Fatalf("after the start ended: stage=%q err=%v, want its failure taken by the pass it asked for", stage, err)
		}
		if starts.Load() != 1 {
			t.Fatalf("%d starts, want 1 (the failed start is taken once; the next pass begins the next)", starts.Load())
		}
	})
}

// A start in flight ends with the Bot: Close cancels it and waits for it. A removed installation's start is cancelled
// too, and no second start of it begins until the first has ended.
func TestHelperStartsEndWithTheBotAndTheirInstallation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newBotGatewayRuntime(t.Context(), nil, true, quietLogger())
		removed, kept := id.New(), id.New()
		var cancelled atomic.Int32
		wait := func(ctx context.Context) (*native.Client, error) {
			<-ctx.Done()
			time.Sleep(time.Second) // the helper is still being stopped
			cancelled.Add(1)
			return nil, ctx.Err()
		}
		if !r.starts.begin(removed, wait) || !r.starts.begin(kept, wait) {
			t.Fatal("no start began")
		}
		if r.starts.begin(removed, wait) {
			t.Fatal("a second start of one installation began while the first was in flight")
		}
		r.RetainOnly([]id.ID{kept})
		synctest.Wait()
		if r.starts.begin(removed, wait) {
			t.Fatal("a start began while the removed installation's cancelled start had not ended")
		}
		time.Sleep(2 * time.Second)
		if cancelled.Load() != 1 {
			t.Fatalf("%d starts cancelled, want the removed installation's", cancelled.Load())
		}
		if _, pending, finished, _ := r.starts.take(removed); pending || finished {
			t.Fatal("a removed installation's cancelled start left a result")
		}
		r.Close()
		if cancelled.Load() != 2 {
			t.Fatalf("Close returned with %d of 2 starts ended", cancelled.Load())
		}
		if r.starts.begin(kept, wait) {
			t.Fatal("a start began after Close")
		}
	})
}
