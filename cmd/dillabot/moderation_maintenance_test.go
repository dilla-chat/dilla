//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
)

type purgeFake struct {
	removed int
	err     error
}

func (f purgeFake) PurgeExpiredResumable(context.Context) (int, error) {
	return f.removed, f.err
}

func TestMaintainModerationEvidenceReportsRemovedCount(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, nil))
	installation := id.New()
	if err := maintainModerationEvidence(context.Background(), logger, installation, purgeFake{removed: 4}); err != nil {
		t.Fatalf("maintenance error = %v", err)
	}
	line := output.String()
	if !strings.Contains(line, "removed_records=4") || !strings.Contains(line, installation.String()) {
		t.Fatalf("success diagnostic omits bounded result or installation: %q", line)
	}
}

func TestMaintainModerationEvidenceDoesNotLogBackendError(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, nil))
	backendErr := errors.New("backend detail with sensitive payload")
	if err := maintainModerationEvidence(context.Background(), logger, id.New(), purgeFake{err: backendErr}); !errors.Is(err, backendErr) {
		t.Fatalf("maintenance error = %v, want wrapped backend error", err)
	}
	line := output.String()
	if !strings.Contains(line, "stage=retention_purge") || strings.Contains(line, "sensitive payload") {
		t.Fatalf("failure diagnostic is missing stage or leaked backend error: %q", line)
	}
}
