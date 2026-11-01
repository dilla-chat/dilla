package main

import (
	"context"
	"errors"
	"sync"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

// Task 10c: Glenn's card shows the runtime's version and has Restart. The server keeps a restart
// request counter per installation; this process honours each increase once and reports back the
// last request it finished together with its version.

type runtimeStatusReporter interface {
	ReportStatus(ctx context.Context, installation id.ID, version *string, handled uint64) error
}

type runtimeStatusTracker struct {
	mu sync.Mutex
	// handled is, per installation, the restart request this process has honoured. The first
	// sight of an installation counts any pending request as honoured: starting the process is
	// itself the restart.
	handled map[id.ID]uint64
	version string
}

func newRuntimeStatusTracker(version string) *runtimeStatusTracker {
	return &runtimeStatusTracker{handled: make(map[id.ID]uint64), version: version}
}

// Observe reports whether the installation has a restart request this process has not honoured
// yet. A server that does not send the widened installation never asks for a restart.
func (t *runtimeStatusTracker) Observe(installation client.Installation) bool {
	if t == nil || !installation.Widened {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	seen, ok := t.handled[installation.ID]
	if !ok {
		t.handled[installation.ID] = installation.RestartRequested
		return false
	}
	if installation.RestartRequested > seen {
		t.handled[installation.ID] = installation.RestartRequested
		return true
	}
	return false
}

// Report tells the server this runtime's version and the last request it honoured, only when the
// server's copy differs, so a steady state costs no request.
func (t *runtimeStatusTracker) Report(ctx context.Context, reporter runtimeStatusReporter, installation client.Installation) error {
	if t == nil || reporter == nil || !installation.Widened {
		return nil
	}
	t.mu.Lock()
	handled, ok := t.handled[installation.ID]
	version := t.version
	t.mu.Unlock()
	if !ok {
		return nil
	}
	// A build without a version (fix round 1, M3) has none to report: only the restart count can be
	// behind, so a steady state still costs no request.
	versionCurrent := version == "" || installation.RuntimeVersion != nil && *installation.RuntimeVersion == version
	if installation.RestartHandled >= handled && versionCurrent {
		return nil
	}
	var reported *string
	if version != "" {
		reported = &version
	}
	return reporter.ReportStatus(ctx, installation.ID, reported, handled)
}

// RetainOnly forgets installations this runtime no longer serves.
func (t *runtimeStatusTracker) RetainOnly(active []id.ID) {
	if t == nil {
		return
	}
	keep := make(map[id.ID]bool, len(active))
	for _, a := range active {
		keep[a] = true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for installation := range t.handled {
		if !keep[installation] {
			delete(t.handled, installation)
		}
	}
}

// currentInstallationCapabilities reads the installation's capability mask from the server on
// each call, as the music authority does: poll and reminder creation check it per request. An
// installation the server no longer lists for this runtime, or one that is not ready, holds
// nothing.
func currentInstallationCapabilities(runtime *client.Runtime, installation, community id.ID) func(context.Context) (uint16, error) {
	source := runtimeMusicAuthorityStore{runtime: runtime, installation: installation, community: community}
	return func(ctx context.Context) (uint16, error) {
		row, err := source.GetActiveBotInstallationByCommunity(ctx, community)
		if errors.Is(err, store.ErrNotFound) {
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
		if row.State != store.BotInstallationReady {
			return 0, nil
		}
		return row.Capabilities, nil
	}
}
