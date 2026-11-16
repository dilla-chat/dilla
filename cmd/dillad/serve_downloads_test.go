package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/exit"
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

func TestServeDocumentsTheDownloadsChannelFlag(t *testing.T) {
	out, _, err := run(t, "serve", "--help")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "-desktop-downloads-channel") || !strings.Contains(out, `(default "beta")`) || !strings.Contains(out, "gpui-alpha") {
		t.Fatalf("missing channel flag: %s", out)
	}
}

// An unknown channel is a usage error at startup, before the config is read: it never falls
// back to beta.
func TestServeRefusesAnUnknownDownloadsChannel(t *testing.T) {
	for _, value := range []string{"alpha", "stable", "", "GPUI-ALPHA"} {
		_, stderr, err := run(t, "serve", "--config=/nonexistent/dilla.toml", "--desktop-downloads", "--desktop-downloads-channel="+value)
		var code exit.Code
		if !errors.As(err, &code) || code != exit.Usage {
			t.Fatalf("%q: err = %v, want exit.Usage", value, err)
		}
		if !strings.Contains(stderr, "--desktop-downloads-channel") || strings.Contains(stderr, "/nonexistent/dilla.toml") {
			t.Fatalf("%q: stderr = %q", value, stderr)
		}
	}
}
