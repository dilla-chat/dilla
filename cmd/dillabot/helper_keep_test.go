//go:build linux

package main

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/id"
)

// usableTestNative reports Usable() as native.Client does: false once closed, or once its helper died (dead).
type usableTestNative struct {
	*reasonNative
	dead atomic.Bool
}

func newUsableTestNative(installation, community id.ID) *usableTestNative {
	return &usableTestNative{reasonNative: &reasonNative{blockingRegistryNative: registryTestNative(installation, community)}}
}

func (n *usableTestNative) Usable() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return !n.closed && !n.dead.Load()
}

func musicTestChannels(community id.ID) []client.Channel {
	return []client.Channel{{ID: id.New(), CommunityID: community, Kind: musicVoiceChannelKind}}
}

// v0.1.0-bot2.1 on CT 144: a pass that Disabled the helper re-retained the closed client in the same pass, and every
// due tick after met it (diagnosis section 1, path (a)). A closed client is never registered again.
func TestRetainRefusesClosedClient(t *testing.T) {
	installation, community := id.New(), id.New()
	registry := newMusicQueueRegistry()
	t.Cleanup(func() { _ = registry.Close() })
	n := newUsableTestNative(installation, community)
	if err := registry.RetainInteractions(installation, community, n); err != nil {
		t.Fatal(err)
	}
	if err := registry.Disable(installation, helper.ReasonRetainedUnusable); err != nil {
		t.Fatal(err)
	}
	if err := registry.RetainInteractions(installation, community, n); !errors.Is(err, errHelperUnusable) {
		t.Fatalf("retain of a closed client = %v, want errHelperUnusable", err)
	}
	if got := registry.Native(installation, community); got != nil {
		t.Fatalf("registry holds %v after refusing a closed client", got)
	}
	owned, err := registry.Reconcile(t.Context(), installation, community, botMusicCapability, musicTestChannels(community), n, unavailableMusicQueueStarter())
	if owned || !errors.Is(err, errHelperUnusable) {
		t.Fatalf("music reconcile of a closed client = %v, %v, want false, errHelperUnusable", owned, err)
	}
	if got := registry.Native(installation, community); got != nil {
		t.Fatalf("registry holds %v after the music reconcile refused a closed client", got)
	}
}

// A retained helper that died on its own is retired, not kept, when a pass offers it again.
func TestRetainRetiresARetainedClientThatDied(t *testing.T) {
	installation, community := id.New(), id.New()
	registry := newMusicQueueRegistry()
	t.Cleanup(func() { _ = registry.Close() })
	n := newUsableTestNative(installation, community)
	if err := registry.RetainInteractions(installation, community, n); err != nil {
		t.Fatal(err)
	}
	n.dead.Store(true)
	if err := registry.RetainInteractions(installation, community, n); !errors.Is(err, errHelperUnusable) {
		t.Fatalf("retain of a dead retained client = %v, want errHelperUnusable", err)
	}
	if registry.Native(installation, community) != nil {
		t.Fatal("registry kept a dead client")
	}
	if len(n.reasons) != 1 || n.reasons[0] != helper.ReasonRetainedUnusable {
		t.Fatalf("close reasons = %v, want [retained_unusable]", n.reasons)
	}
	// The same through the music reconcile.
	m := newUsableTestNative(installation, community)
	if err := registry.RetainInteractions(installation, community, m); err != nil {
		t.Fatal(err)
	}
	m.dead.Store(true)
	if _, err := registry.Reconcile(t.Context(), installation, community, botMusicCapability, musicTestChannels(community), m, unavailableMusicQueueStarter()); !errors.Is(err, errHelperUnusable) {
		t.Fatalf("music reconcile of a dead client = %v, want errHelperUnusable", err)
	}
	if registry.Native(installation, community) != nil {
		t.Fatal("registry kept a dead client after the music reconcile")
	}
	if len(m.reasons) != 1 || m.reasons[0] != helper.ReasonRetainedUnusable {
		t.Fatalf("close reasons = %v, want [retained_unusable]", m.reasons)
	}
}

// A pass error that is not about the helper (here a dillad 409 during the membership reconcile) keeps the working
// helper, and the sealed intake wired to it keeps answering.
func TestPassErrorKeepsUsableHelper(t *testing.T) {
	w := newLoadWorld(t, 3, false)
	installation, community := w.entry.installation, w.community
	registry := newMusicQueueRegistry()
	t.Cleanup(func() { _ = registry.Close() })
	n := newUsableTestNative(installation, community)
	// The intake's helper is this client: closing it closes the intake's helper too.
	n.close = func() { w.helper.mu.Lock(); w.helper.closed = true; w.helper.mu.Unlock() }
	if err := registry.RetainInteractions(installation, community, n); err != nil {
		t.Fatal(err)
	}
	conflict := &client.APIError{Status: http.StatusConflict, Code: "E_CONFLICT"}
	if client.IsDeferred(conflict) {
		t.Fatal("test premise: a 409 is not deferred")
	}
	cleanupUnreadyInstallation(installation, false, false, conflict, registry)
	if got := registry.Native(installation, community); got != n || !n.Usable() || n.closes.Load() != 0 {
		t.Fatalf("pass error replaced or closed a usable helper (native=%v usable=%v closes=%d)", got, n.Usable(), n.closes.Load())
	}
	w.transport.mu.Lock()
	before := len(w.transport.answers)
	w.transport.mu.Unlock()
	w.handleNext(t.Context())
	waitUntil(t, func() bool {
		w.transport.mu.Lock()
		defer w.transport.mu.Unlock()
		return len(w.transport.answers) > before
	})
}

func TestMusicRestoreErrorKeepsHelper(t *testing.T) {
	installation, community := id.New(), id.New()
	registry := newMusicQueueRegistry()
	t.Cleanup(func() { _ = registry.Close() })
	n := newUsableTestNative(installation, community)
	if _, err := registry.Reconcile(t.Context(), installation, community, botMusicCapability, musicTestChannels(community), n, unavailableMusicQueueStarter()); err != nil {
		t.Fatal(err)
	}
	if err := registry.RetainInteractions(installation, community, n); err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.QueueStore(installation, community); !ok {
		t.Fatal("test premise: music queues restored")
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	forbidden := &client.APIError{Status: http.StatusForbidden, Code: "E_FORBIDDEN"}
	if deferred := cleanupFailedMusicRestore(logger, installation, false, forbidden, registry); deferred != nil {
		t.Fatalf("a 403 restore failure deferred: %v", deferred)
	}
	if got := registry.Native(installation, community); got != n || !n.Usable() || n.closes.Load() != 0 {
		t.Fatal("a music restore error closed the usable helper")
	}
	if _, ok := registry.QueueStore(installation, community); ok {
		t.Fatal("music not paused after the restore error")
	}
	if c := strings.Count(logs.String(), "bot music queue restore paused"); c != 1 {
		t.Fatalf("restore paused logged %d times, want 1:\n%s", c, logs.String())
	}
}

func TestPassErrorClosesUnusableHelper(t *testing.T) {
	conflict := &client.APIError{Status: http.StatusConflict, Code: "E_CONFLICT"}
	discard := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	for name, cleanup := range map[string]func(id.ID, *musicQueueRegistry){
		"unready": func(installation id.ID, registry *musicQueueRegistry) {
			cleanupUnreadyInstallation(installation, false, false, conflict, registry)
		},
		"music_restore": func(installation id.ID, registry *musicQueueRegistry) {
			_ = cleanupFailedMusicRestore(discard, installation, false, conflict, registry)
		},
	} {
		t.Run(name, func(t *testing.T) {
			installation, community := id.New(), id.New()
			registry := newMusicQueueRegistry()
			t.Cleanup(func() { _ = registry.Close() })
			n := newUsableTestNative(installation, community)
			if err := registry.RetainInteractions(installation, community, n); err != nil {
				t.Fatal(err)
			}
			n.dead.Store(true)
			cleanup(installation, registry)
			if registry.Native(installation, community) != nil {
				t.Fatal("an unusable helper stayed registered after the pass error")
			}
			if len(n.reasons) != 1 || n.reasons[0] != helper.ReasonRetainedUnusable {
				t.Fatalf("close reasons = %v, want [retained_unusable]", n.reasons)
			}
			// The next pass finds no retained helper, starts another and retains it.
			if _, retained := retainedNativeClient(registry, installation, community); retained {
				t.Fatal("next pass found a retained helper")
			}
			next := newUsableTestNative(installation, community)
			if err := registry.RetainInteractions(installation, community, next); err != nil {
				t.Fatal(err)
			}
			if registry.Native(installation, community) != next {
				t.Fatal("next pass's helper not retained")
			}
		})
	}
}

// The helper of an installation the pass already knows is gone or disabled is closed as installation_gone.
func TestPassErrorClosesGoneInstallationHelper(t *testing.T) {
	conflict := &client.APIError{Status: http.StatusConflict, Code: "E_CONFLICT"}
	discard := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	for name, cleanup := range map[string]func(id.ID, *musicQueueRegistry){
		"unready": func(installation id.ID, registry *musicQueueRegistry) {
			cleanupUnreadyInstallation(installation, false, true, conflict, registry)
		},
		"music_restore": func(installation id.ID, registry *musicQueueRegistry) {
			_ = cleanupFailedMusicRestore(discard, installation, true, conflict, registry)
		},
	} {
		t.Run(name, func(t *testing.T) {
			installation, community := id.New(), id.New()
			registry := newMusicQueueRegistry()
			t.Cleanup(func() { _ = registry.Close() })
			n := newUsableTestNative(installation, community)
			if err := registry.RetainInteractions(installation, community, n); err != nil {
				t.Fatal(err)
			}
			cleanup(installation, registry)
			if registry.Native(installation, community) != nil || n.Usable() {
				t.Fatal("a gone installation kept its helper")
			}
			if len(n.reasons) != 1 || n.reasons[0] != helper.ReasonInstallationGone {
				t.Fatalf("close reasons = %v, want [installation_gone]", n.reasons)
			}
		})
	}
}
