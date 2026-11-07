//go:build linux

package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/client"
)

// TestSupervisedRateLimitsAreBuiltInUnlessDevSameUser (Bot v2 task 14): there is no bot.toml, so a supervised Bot's
// client rate limits are the built-in defaults. A Bot that dillad runs as its own user (dev_same_user, development and
// the e2e test host) is told so by its socket, which is then its own, and runs without client pacing, as the test
// host's tuning file used to set; dillad's metering still applies to it.
func TestSupervisedRateLimitsAreBuiltInUnlessDevSameUser(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "gateway.sock")
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	got := supervisedRateLimits(sock)
	if got != unpacedRateLimits {
		t.Fatalf("own socket (dev_same_user): rate limits %+v, want unpaced", got)
	}
	if err := (unpacedRateLimits).Validate(); err != nil {
		t.Fatalf("unpaced rate limits are invalid: %v", err)
	}
	if got := supervisedRateLimits(filepath.Join(t.TempDir(), "absent.sock")); got != (client.RateLimits{}) {
		t.Fatalf("unreadable socket: rate limits %+v, want the built-in defaults", got)
	}
	if os.Geteuid() != 0 {
		// A socket another user owns: the production layout (dillad's socket, the Bot's group).
		if got := supervisedRateLimits("/"); got != (client.RateLimits{}) {
			t.Fatalf("a path another user owns: rate limits %+v, want the built-in defaults", got)
		}
	}
}
