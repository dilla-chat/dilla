//go:build dilla_testhost

package main

import (
	"os"
	"path/filepath"
)

// testhostTool is a provider tool a test host placed in the supervised Bot's state directory (<state>/bot-tools):
// bot-launch gives the Bot a fixed PATH, so a test host without root installs yt-dlp there for the music lane (Bot v2
// Task 19 fix round 1). Only a dilla_testhost build looks; a shipped Bot finds its tools on its PATH only.
func testhostTool(name string) string {
	// Only the provider (fix round 2, N6): never the sandbox (bwrap) or the interpreters around it.
	if name != "yt-dlp" {
		return ""
	}
	state := os.Getenv("DILLA_BOT_STATE_DIR")
	if state == "" {
		return ""
	}
	path := filepath.Join(state, "bot-tools", name)
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 { // #nosec G703 -- test-host build; the Bot's own state directory.
		return ""
	}
	return path
}
