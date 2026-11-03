package main

import (
	"strings"
	"testing"
)

func TestServeDocumentsTheExplicitDownloadsFlag(t *testing.T) {
	out, stderr, err := run(t, "serve", "--help")
	if err != nil || stderr != "" {
		t.Fatalf("serve help: %v %s", err, stderr)
	}
	if !strings.Contains(out, "-desktop-downloads") || !strings.Contains(out, "enable only after desktop acceptance") {
		t.Fatalf("missing opt-in flag: %s", out)
	}
}
