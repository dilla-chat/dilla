//go:build linux

package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/id"
)

type blockingRegistryNative struct {
	*runtimeStateNative
	load   func()
	close  func()
	closes atomic.Uint64
}

func (n *blockingRegistryNative) StateGet(ctx context.Context, namespace uint64, key []byte) (native.StateValue, error) {
	if n.load != nil {
		n.load()
	}
	return n.runtimeStateNative.StateGet(ctx, namespace, key)
}

func (n *blockingRegistryNative) Close() error {
	n.closes.Add(1)
	if n.close != nil {
		n.close()
	}
	return n.runtimeStateNative.Close()
}

func registryTestNative(installation, community id.ID) *blockingRegistryNative {
	return &blockingRegistryNative{runtimeStateNative: &runtimeStateNative{
		binding: native.Binding{Instance: id.New(), Installation: installation, Community: community},
		rows:    make(map[string]native.StateValue),
	}}
}

func TestMusicRegistryBorrowDefersRetiredNativeClose(t *testing.T) {
	installation, community := id.New(), id.New()
	registry := newMusicQueueRegistry()
	n := registryTestNative(installation, community)
	if err := registry.RetainInteractions(installation, community, n); err != nil {
		t.Fatal(err)
	}
	release, ok := registry.AcquireNative(installation, community, n)
	if !ok {
		t.Fatal("failed to borrow current native")
	}
	if err := registry.Disable(installation); err != nil {
		t.Fatal(err)
	}
	if n.closes.Load() != 0 || registry.Native(installation, community) != nil {
		t.Fatal("retirement must remove lookup while preserving the borrowed helper")
	}
	next := registryTestNative(installation, community)
	if err := registry.RetainInteractions(installation, community, next); err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.AcquireNative(installation, community, n); ok {
		t.Fatal("old callback borrowed a replacement generation")
	}
	release()
	release()
	if n.closes.Load() != 1 || next.closes.Load() != 0 {
		t.Fatalf("old closes=%d replacement closes=%d", n.closes.Load(), next.closes.Load())
	}
	if registry.Native(installation, id.New()) != nil || next.closes.Load() != 0 {
		t.Fatal("mismatched read mutated the retained native")
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	if err := registry.RetainInteractions(installation, community, n); err == nil {
		t.Fatal("closed registry accepted a new native")
	}
}

func TestMusicRegistryRestoreDoesNotBlockOtherInstallation(t *testing.T) {
	installation, community, channel := id.New(), id.New(), id.New()
	registry := newMusicQueueRegistry()
	entered, proceed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	n := registryTestNative(installation, community)
	n.load = func() { once.Do(func() { close(entered) }); <-proceed }
	done := make(chan error, 1)
	go func() {
		_, err := registry.Reconcile(t.Context(), installation, community, botMusicCapability,
			[]client.Channel{{ID: channel, CommunityID: community, Kind: musicVoiceChannelKind}}, n, unavailableMusicQueueStarter())
		done <- err
	}()
	<-entered
	otherInstallation, otherCommunity := id.New(), id.New()
	other := registryTestNative(otherInstallation, otherCommunity)
	lookupDone := make(chan error, 1)
	go func() {
		lookupDone <- registry.RetainInteractions(otherInstallation, otherCommunity, other)
	}()
	select {
	case err := <-lookupDone:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(time.Second):
		t.Error("helper restore held the registry lock across state loading")
	}
	close(proceed)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := registry.QueueCount(installation, community); got != 1 {
		t.Fatalf("queue count=%d, want 1", got)
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMusicRegistryNativeCloseDoesNotBlockOtherInstallation(t *testing.T) {
	installation, community := id.New(), id.New()
	registry := newMusicQueueRegistry()
	n := registryTestNative(installation, community)
	entered, proceed := make(chan struct{}), make(chan struct{})
	n.close = func() { close(entered); <-proceed }
	if err := registry.RetainInteractions(installation, community, n); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- registry.Disable(installation) }()
	<-entered
	lookupDone := make(chan struct{})
	go func() {
		_ = registry.Native(id.New(), id.New())
		close(lookupDone)
	}()
	select {
	case <-lookupDone:
	case <-time.After(time.Second):
		t.Error("helper Close held the registry lock")
	}
	close(proceed)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestMusicRegistryCloseRacesFinalBorrowRelease(t *testing.T) {
	for range 50 {
		installation, community := id.New(), id.New()
		registry := newMusicQueueRegistry()
		n := registryTestNative(installation, community)
		if err := registry.RetainInteractions(installation, community, n); err != nil {
			t.Fatal(err)
		}
		release, ok := registry.AcquireNative(installation, community, n)
		if !ok {
			t.Fatal("failed to borrow current native")
		}
		start := make(chan struct{})
		done := make(chan struct{})
		go func() { <-start; release(); release(); close(done) }()
		close(start)
		if err := registry.Close(); err != nil {
			t.Fatal(err)
		}
		<-done
		if n.closes.Load() != 1 {
			t.Fatalf("native closes=%d, want exactly once", n.closes.Load())
		}
	}
}
