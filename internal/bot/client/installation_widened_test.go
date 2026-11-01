package client

import (
	"errors"
	"testing"

	"github.com/jonasthim/dilla/internal/id"
)

// Task 10c: the runtime list answers 8 fields from an older server and 13 from a newer one.
func TestParseInstallationReadsTheWidenedShape(t *testing.T) {
	installation, community := id.New(), id.New()
	base := []any{installation[:], community[:], uint64(3), uint64(4), nil, nil, uint64(1 | 1<<8), uint64(99)}
	old, err := parseInstallation(base)
	if err != nil || old.Widened || old.Capabilities != 1|1<<8 {
		t.Fatalf("8-field installation = %+v, %v", old, err)
	}
	widened := append(append([]any(nil), base...), uint64(3), uint64(2), "0.4.2", uint64(50), uint64(15))
	got, err := parseInstallation(widened)
	if err != nil || !got.Widened || got.RestartRequested != 3 || got.RestartHandled != 2 || got.RuntimeVersion == nil ||
		*got.RuntimeVersion != "0.4.2" || got.MusicVolume != 50 || got.MusicIdleMinutes != 15 {
		t.Fatalf("13-field installation = %+v, %v", got, err)
	}
	noVersion := append(append([]any(nil), base...), uint64(0), uint64(0), nil, uint64(100), uint64(30))
	if got, err := parseInstallation(noVersion); err != nil || got.RuntimeVersion != nil {
		t.Fatalf("installation without a version = %+v, %v", got, err)
	}
	for name, bad := range map[string][]any{
		"handled above requested": append(append([]any(nil), base...), uint64(1), uint64(2), nil, uint64(100), uint64(30)),
		"empty version":           append(append([]any(nil), base...), uint64(0), uint64(0), "", uint64(100), uint64(30)),
		"long version":            append(append([]any(nil), base...), uint64(0), uint64(0), "0123456789012345678901234567890123", uint64(100), uint64(30)),
		"twelve fields":           append(append([]any(nil), base...), uint64(0), uint64(0), nil, uint64(100)),
	} {
		if _, err := parseInstallation(bad); !errors.Is(err, ErrProtocol) {
			t.Fatalf("%s: err = %v, want ErrProtocol", name, err)
		}
	}
}

// The status report is a PUT answered 204 (it is the only route of that shape the client accepts).
func TestRuntimeStatusReportExpectsNoContent(t *testing.T) {
	installation := id.New()
	status, ok := expectedStatus("PUT", "bot-runtime/installations/"+installation.String()+"/status")
	if !ok || status != 204 {
		t.Fatalf("expected status = %d, %v; want 204", status, ok)
	}
	if _, ok := expectedStatus("POST", "bot-runtime/installations/"+installation.String()+"/status"); ok {
		t.Fatal("a POST to the status route is accepted")
	}
}
