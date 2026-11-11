package client

import (
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/cborx"
)

// v0.1.0-bot2.2 (F7): failed_commands is a seventh element, sent only when there are failures, so a report without any
// is the six elements older dillad reads.
func TestHealthBodyAddsFailedCommandsOnlyWhenThereAreSome(t *testing.T) {
	at := time.Unix(1_800_000_000, 0)
	decode := func(r HealthReport) []any {
		t.Helper()
		body, err := healthBody(r)
		if err != nil {
			t.Fatal(err)
		}
		var raw []any
		if err := cborx.Unmarshal(body, &raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	if got := decode(HealthReport{ProbeOK: true, ProbeAt: at}); len(got) != 6 {
		t.Fatalf("a report without failures has %d elements, want 6", len(got))
	}
	got := decode(HealthReport{ProbeOK: true, ProbeAt: at, FailedCommands: 3})
	if len(got) != 7 || got[6] != uint64(3) {
		t.Fatalf("a report with 3 failures = %v, want seven elements ending in 3", got)
	}
}
