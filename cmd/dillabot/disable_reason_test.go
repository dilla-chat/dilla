package main

import (
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/id"
)

// reasonNative records the reason its close was given, as native.Client passes it down to the helper client.
type reasonNative struct {
	*blockingRegistryNative
	reasons []string
}

func (n *reasonNative) CloseWithReason(reason string) error {
	n.reasons = append(n.reasons, reason)
	return n.blockingRegistryNative.Close()
}

func TestDisablePassesReason(t *testing.T) {
	installation, community := id.New(), id.New()
	registry := newMusicQueueRegistry()
	n := &reasonNative{blockingRegistryNative: registryTestNative(installation, community)}
	if err := registry.RetainInteractions(installation, community, n); err != nil {
		t.Fatal(err)
	}
	if err := registry.Disable(installation, helper.ReasonMusicRestoreFailed); err != nil {
		t.Fatal(err)
	}
	if len(n.reasons) != 1 || n.reasons[0] != helper.ReasonMusicRestoreFailed {
		t.Fatalf("close reasons = %v, want [music_restore_failed]", n.reasons)
	}
}

func TestRetainOnlyReasonsShutdownAndGone(t *testing.T) {
	installation, community := id.New(), id.New()
	registry := newMusicQueueRegistry()
	n := &reasonNative{blockingRegistryNative: registryTestNative(installation, community)}
	if err := registry.RetainInteractions(installation, community, n); err != nil {
		t.Fatal(err)
	}
	if err := registry.RetainOnly(nil); err != nil {
		t.Fatal(err)
	}
	if len(n.reasons) != 1 || n.reasons[0] != helper.ReasonInstallationGone {
		t.Fatalf("pruned installation close reasons = %v", n.reasons)
	}
	m := &reasonNative{blockingRegistryNative: registryTestNative(installation, community)}
	if err := registry.RetainInteractions(installation, community, m); err != nil {
		t.Fatal(err)
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	if len(m.reasons) != 1 || m.reasons[0] != helper.ReasonShutdown {
		t.Fatalf("shutdown close reasons = %v", m.reasons)
	}
}
