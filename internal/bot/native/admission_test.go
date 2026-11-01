package native

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/helper"
	"github.com/jonasthim/dilla/internal/bot/protocol"
)

type admissionTransport struct {
	fakeTransport
	active  atomic.Int32
	entered chan struct{}
	resume  chan struct{}
	calls   atomic.Int32
}

func (f *admissionTransport) Call(context.Context, protocol.HelperOperation, []any) ([]any, error) {
	if f.active.Add(1) != 1 {
		panic("concurrent helper transport use")
	}
	defer f.active.Add(-1)
	f.calls.Add(1)
	if f.entered != nil {
		f.entered <- struct{}{}
		<-f.resume
	}
	return []any{}, nil
}

func TestNativeConcurrentCallsStaySerialized(t *testing.T) {
	f := &admissionTransport{}
	c, err := newClient(f, testBinding())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 16 {
				if _, err := c.call(t.Context(), protocol.HelperStateGet, nil, false, voidResult); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if f.calls.Load() != 512 {
		t.Fatalf("transport calls = %d", f.calls.Load())
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeQueuedCancellationAndClosePreserveActiveCall(t *testing.T) {
	f := &admissionTransport{entered: make(chan struct{}, 1), resume: make(chan struct{})}
	c, err := newClient(f, testBinding())
	if err != nil {
		t.Fatal(err)
	}
	activeDone := make(chan error, 1)
	go func() {
		_, err := c.call(t.Context(), protocol.HelperStateGet, nil, false, voidResult)
		activeDone <- err
	}()
	<-f.entered
	ctx, cancel := context.WithCancel(t.Context())
	queuedDone := make(chan error, 1)
	go func() {
		_, err := c.call(ctx, protocol.HelperStateGet, nil, false, voidResult)
		queuedDone <- err
	}()
	cancel()
	if err := <-queuedDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("queued cancellation = %v", err)
	}
	go func() {
		_, err := c.call(t.Context(), protocol.HelperStateGet, nil, false, voidResult)
		queuedDone <- err
	}()
	closeDone := make(chan error, 1)
	go func() { closeDone <- c.Close() }()
	select {
	case err := <-queuedDone:
		if !errors.Is(err, helper.ErrClosed) {
			t.Fatalf("queued call during shutdown = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued call did not wake on shutdown")
	}
	select {
	case err := <-closeDone:
		t.Fatalf("Close completed while transport was active: %v", err)
	default:
	}
	close(f.resume)
	if err := <-activeDone; err != nil {
		t.Fatalf("active call interrupted: %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() != 1 {
		t.Fatalf("cancelled or closed call reached helper: calls = %d", f.calls.Load())
	}
}
