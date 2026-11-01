//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/helper"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/id"
)

func waitManagerRestartRetirement(t *testing.T, registry *musicQueueRegistry, installation, community id.ID) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if registry.Native(installation, community) == nil {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("restart did not retire the current helper")
}

func TestManagerRestartWaitsForBorrowAndNativeClose(t *testing.T) {
	installation, community := id.New(), id.New()
	registry := newMusicQueueRegistry()
	n := registryTestNative(installation, community)
	closeEntered, allowClose := make(chan struct{}), make(chan struct{})
	n.close = func() { close(closeEntered); <-allowClose }
	// Literal entries occur in other registry users and must initialize close
	// completion lazily rather than requiring a constructor.
	registry.runtimes[installation] = &retainedMusicRuntime{community: community, native: n}
	release, ok := registry.AcquireNative(installation, community, n)
	if !ok {
		t.Fatal("failed to borrow helper")
	}
	restarted := make(chan error, 1)
	go func() { restarted <- registry.DisableAndWait(t.Context(), installation) }()
	waitManagerRestartRetirement(t, registry, installation, community)
	returnedEarly := false
	select {
	case err := <-restarted:
		returnedEarly = true
		t.Errorf("restart returned while helper was still borrowed: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	released := make(chan struct{})
	go func() { release(); release(); close(released) }()
	<-closeEntered
	if !returnedEarly {
		select {
		case err := <-restarted:
			returnedEarly = true
			t.Errorf("restart returned before native Close completed: %v", err)
		case <-time.After(30 * time.Millisecond):
		}
	}
	// Closing a generation must leave unrelated installation lookups available.
	lookupDone := make(chan struct{})
	go func() { _ = registry.Native(id.New(), id.New()); close(lookupDone) }()
	select {
	case <-lookupDone:
	case <-time.After(time.Second):
		t.Error("native Close held the registry mutex")
	}
	close(allowClose)
	<-released
	if !returnedEarly {
		select {
		case err := <-restarted:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("restart did not finish after native Close")
		}
	}
	if n.closes.Load() != 1 {
		t.Fatalf("native closes = %d, want 1", n.closes.Load())
	}
}

func TestManagerRestartReopensBorrowedNativeStoreInSamePass(t *testing.T) {
	binary := os.Getenv("DILLA_BOT_HELPER_BIN")
	if binary == "" {
		binary = "../../target/debug/dilla-bot-core"
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(binary); err != nil {
		t.Skip("build dilla-bot-core or set DILLA_BOT_HELPER_BIN for the native restart integration")
	}
	binding := native.Binding{Instance: id.New(), Installation: id.New(), Community: id.New()}
	key := bytes.Repeat([]byte{7}, 32)
	opts := helper.Options{Executable: binary, StateDir: filepath.Join(t.TempDir(), "native-helper")}
	n, err := native.Start(t.Context(), opts, binding, bytes.Clone(key))
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	stateKey, value := []byte("manager-restart"), []byte{0x81, 0x01}
	if _, err := n.StateTransaction(t.Context(), id.New(), []native.StateChange{{Area: 9, Key: stateKey, Value: value}}); err != nil {
		t.Fatal(err)
	}
	registry := newMusicQueueRegistry()
	if err := registry.RetainInteractions(binding.Installation, binding.Community, n); err != nil {
		t.Fatal(err)
	}
	release, ok := registry.AcquireNative(binding.Installation, binding.Community, n)
	if !ok {
		t.Fatal("failed to borrow real helper")
	}
	defer release()
	type opened struct {
		next *native.Client
		err  error
	}
	restarted := make(chan opened, 1)
	go func() {
		if err := registry.DisableAndWait(t.Context(), binding.Installation); err != nil {
			restarted <- opened{err: err}
			return
		}
		next, err := native.Start(t.Context(), opts, binding, bytes.Clone(key))
		restarted <- opened{next, err}
	}()
	waitManagerRestartRetirement(t, registry, binding.Installation, binding.Community)
	var result opened
	returnedEarly := false
	select {
	case result = <-restarted:
		returnedEarly = true
		t.Errorf("reopened native store before borrowed helper retired: %v", result.err)
	case <-time.After(30 * time.Millisecond):
	}
	release()
	if !returnedEarly {
		select {
		case result = <-restarted:
		case <-time.After(5 * time.Second):
			t.Fatal("restart did not reopen native helper")
		}
	}
	if result.err != nil {
		t.Fatalf("same-pass helper reopen: %v", result.err)
	}
	if result.next == nil {
		t.Fatal("restart did not return a helper")
	}
	defer result.next.Close()
	stored, err := result.next.StateGet(t.Context(), 9, stateKey)
	if err != nil || stored.Revision != 1 || !bytes.Equal(stored.Value, value) {
		t.Fatalf("reopened durable state = %+v, %v", stored, err)
	}
}

func TestManagerRestartWaitCanBeCanceledWhileBorrowed(t *testing.T) {
	installation, community := id.New(), id.New()
	registry := newMusicQueueRegistry()
	n := registryTestNative(installation, community)
	if err := registry.RetainInteractions(installation, community, n); err != nil {
		t.Fatal(err)
	}
	release, ok := registry.AcquireNative(installation, community, n)
	if !ok {
		t.Fatal("failed to borrow helper")
	}
	defer release()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- registry.DisableAndWait(ctx, installation) }()
	waitManagerRestartRetirement(t, registry, installation, community)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled restart = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("restart did not honor cancellation")
	}
	if n.closes.Load() != 0 {
		t.Fatal("canceled wait closed the still-borrowed native")
	}
	release()
	if n.closes.Load() != 1 {
		t.Fatal("last borrower did not finish retiring helper")
	}
}

func TestManagerRestartWaitsForAlreadyRetiredNative(t *testing.T) {
	installation, community := id.New(), id.New()
	registry := newMusicQueueRegistry()
	n := registryTestNative(installation, community)
	if err := registry.RetainInteractions(installation, community, n); err != nil {
		t.Fatal(err)
	}
	release, ok := registry.AcquireNative(installation, community, n)
	if !ok {
		t.Fatal("failed to borrow helper")
	}
	defer release()
	if err := registry.Disable(installation); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- registry.DisableAndWait(t.Context(), installation) }()
	returnedEarly := false
	select {
	case err := <-done:
		returnedEarly = true
		t.Errorf("restart ignored previously retired borrowed generation: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	release()
	if !returnedEarly {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("restart did not finish after prior retirement completed")
		}
	}
	if n.closes.Load() != 1 {
		t.Fatalf("native closes = %d, want 1", n.closes.Load())
	}
}

func TestManagerRestartWaitCoversEveryRetiredGeneration(t *testing.T) {
	installation, community := id.New(), id.New()
	registry := newMusicQueueRegistry()
	var releases []func()
	var generations []*blockingRegistryNative
	for range 2 {
		n := registryTestNative(installation, community)
		generations = append(generations, n)
		if err := registry.RetainInteractions(installation, community, n); err != nil {
			t.Fatal(err)
		}
		release, ok := registry.AcquireNative(installation, community, n)
		if !ok {
			t.Fatal("failed to borrow helper")
		}
		releases = append(releases, release)
		defer release()
		if err := registry.Disable(installation); err != nil {
			t.Fatal(err)
		}
	}
	// Keep a replacement published while an old generation closes: old cleanup
	// must remove only its pending close, not the current retained generation.
	replacement := registryTestNative(installation, community)
	if err := registry.RetainInteractions(installation, community, replacement); err != nil {
		t.Fatal(err)
	}
	releases[0]()
	if registry.Native(installation, community) != replacement || replacement.closes.Load() != 0 {
		t.Fatal("retired generation cleanup affected its live replacement")
	}
	done := make(chan error, 1)
	go func() { done <- registry.DisableAndWait(t.Context(), installation) }()
	waitManagerRestartRetirement(t, registry, installation, community)
	select {
	case err := <-done:
		t.Fatalf("restart ignored a previous generation: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	releases[1]()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("restart did not finish after every generation closed")
	}
	registry.mu.Lock()
	pending := len(registry.retiring)
	registry.mu.Unlock()
	if pending != 0 {
		t.Fatalf("completed native closes left %d pending installations", pending)
	}
	for _, n := range append(generations, replacement) {
		if n.closes.Load() != 1 {
			t.Fatalf("native closes = %d, want 1", n.closes.Load())
		}
	}
}

func TestManagerRestartCancellationDoesNotAbandonNativeClose(t *testing.T) {
	installation, community := id.New(), id.New()
	registry := newMusicQueueRegistry()
	n := registryTestNative(installation, community)
	entered, proceed := make(chan struct{}), make(chan struct{})
	n.close = func() { close(entered); <-proceed }
	if err := registry.RetainInteractions(installation, community, n); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- registry.DisableAndWait(ctx, installation) }()
	<-entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled restart = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked Close prevented cancellation")
	}
	// A later waiter must still find the closing generation, although the first
	// waiter was canceled and the active runtime was already removed.
	go func() { done <- registry.DisableAndWait(t.Context(), installation) }()
	select {
	case err := <-done:
		t.Errorf("retry missed the still-closing helper: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(proceed)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("retry did not observe completed native Close")
	}
	if n.closes.Load() != 1 {
		t.Fatalf("native closes = %d, want 1", n.closes.Load())
	}
}

type managerRestartCloseFailure struct {
	*blockingRegistryNative
	err error
}

func (n *managerRestartCloseFailure) Close() error {
	_ = n.blockingRegistryNative.Close()
	return n.err
}

func TestManagerRestartReportsNativeCloseFailure(t *testing.T) {
	for _, borrowed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unborrowed", true: "borrowed"}[borrowed], func(t *testing.T) {
			installation, community := id.New(), id.New()
			registry := newMusicQueueRegistry()
			want := errors.New("native close failed")
			n := &managerRestartCloseFailure{registryTestNative(installation, community), want}
			if err := registry.RetainInteractions(installation, community, n); err != nil {
				t.Fatal(err)
			}
			var release func()
			if borrowed {
				var ok bool
				release, ok = registry.AcquireNative(installation, community, n)
				if !ok {
					t.Fatal("failed to borrow helper")
				}
				defer release()
			}
			done := make(chan error, 1)
			go func() { done <- registry.DisableAndWait(t.Context(), installation) }()
			waitManagerRestartRetirement(t, registry, installation, community)
			if release != nil {
				release()
			}
			select {
			case err := <-done:
				if !errors.Is(err, want) {
					t.Fatalf("restart = %v, want native close failure", err)
				}
			case <-time.After(time.Second):
				t.Fatal("restart did not observe failed Close")
			}
		})
	}
}
