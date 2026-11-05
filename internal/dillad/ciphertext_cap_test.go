package dillad_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/dillad"
)

// Final review M4 (signal3): an operator who kept the old 131072 ciphertext cap refuses large
// ten-file messages for everyone. dillad says so once at startup, naming the setting and the
// recommended value; at the default it says nothing.
func TestStartupWarnsOnceWhenTheCiphertextCapIsBelowTheDefault(t *testing.T) {
	for _, c := range []struct {
		name  string
		limit int64
		warns bool
	}{
		{"the signal2 value", 131072, true},
		{"the default", 196608, false},
		{"above the default", 262144, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Limits.MaxCiphertextBytes = c.limit
			logs := &recordLog{}
			s, err := dillad.New(context.Background(), dillad.Options{Config: cfg, Clock: clock.System(), Wasm: sharedRuntime(t), Log: slog.New(logs)})
			if err != nil {
				t.Fatalf("dillad.New: %v", err)
			}
			t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
			want := map[string]string{"setting": "limits.max_ciphertext_bytes", "recommended": "196608"}
			got := logs.has(slog.LevelWarn, "max_ciphertext_bytes", want)
			if got != c.warns {
				t.Fatalf("limit %d: warned = %v, want %v (%v)", c.limit, got, c.warns, logs.messages())
			}
			if n := warnings(logs, "max_ciphertext_bytes"); c.warns && n != 1 {
				t.Fatalf("limit %d: the warning was logged %d times, want once", c.limit, n)
			}
		})
	}
}

// warnings counts the captured WARN records whose message contains msgPart.
func warnings(l *recordLog, msgPart string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, r := range l.recs {
		if r.Level == slog.LevelWarn && strings.Contains(r.Message, msgPart) {
			n++
		}
	}
	return n
}
