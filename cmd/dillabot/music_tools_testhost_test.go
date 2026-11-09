//go:build dilla_testhost

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Fix round 2 (N6): a test host hands the Bot yt-dlp through its state directory, and nothing else: never the
// sandbox (bwrap) or the interpreters around it.
func TestTheStateDirectoryProvidesOnlyYtDlp(t *testing.T) {
	state := t.TempDir()
	t.Setenv("DILLA_BOT_STATE_DIR", state)
	tools := filepath.Join(state, "bot-tools")
	if err := os.Mkdir(tools, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"yt-dlp", "bwrap", "python3", "node", "ffmpeg"} {
		if err := os.WriteFile(filepath.Join(tools, name), []byte("#!/bin/sh\n"), 0o755); err != nil { // #nosec G306 -- an executable test tool
			t.Fatal(err)
		}
	}
	if got := testhostTool("yt-dlp"); got != filepath.Join(tools, "yt-dlp") {
		t.Fatalf("yt-dlp = %q", got)
	}
	for _, name := range []string{"bwrap", "python3", "node", "ffmpeg"} {
		if got := testhostTool(name); got != "" {
			t.Fatalf("%s came from the state directory: %q", name, got)
		}
	}
}
