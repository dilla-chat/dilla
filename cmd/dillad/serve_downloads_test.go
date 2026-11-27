package main

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/exit"
)

// Task 12: the download page is always on, so --desktop-downloads is a deprecated no-op. It stays accepted because
// CT 144's unit passes it, and a removed flag would be a usage error (exit 2) and a restart loop.
func TestServeDocumentsTheDeprecatedDownloadsFlag(t *testing.T) {
	out, stderr, err := run(t, "serve", "--help")
	if err != nil || stderr != "" {
		t.Fatalf("serve help: %v %s", err, stderr)
	}
	if !strings.Contains(out, "-desktop-downloads") || !strings.Contains(out, "deprecated") {
		t.Fatalf("the help does not mark -desktop-downloads deprecated: %s", out)
	}
}

func TestServeAcceptsTheDeprecatedDownloadsFlag(t *testing.T) {
	for _, args := range [][]string{
		{"serve", "--config=/nonexistent/dilla.toml", "--desktop-downloads"},
		{"serve", "--config=/nonexistent/dilla.toml", "--desktop-downloads", "--desktop-downloads-channel=gpui-alpha"},
		{"serve", "--config=/nonexistent/dilla.toml", "--desktop-downloads=false"},
	} {
		_, stderr, err := run(t, args...)
		var code exit.Code
		if errors.As(err, &code) && code == exit.Usage {
			t.Fatalf("%v: a usage error (%v, %s); the flag must stay accepted", args, err, stderr)
		}
		// It got as far as reading the config, which does not exist.
		if err == nil || !strings.Contains(err.Error(), "/nonexistent/dilla.toml") {
			t.Fatalf("%v: err = %v, want the config read to fail", args, err)
		}
	}
}

// One warning, naming only the flag, and only when it is set; nothing else changes (dillad.Options has no field for
// it any more).
func TestTheDeprecatedDownloadsFlagLogsOneWarning(t *testing.T) {
	for _, set := range []bool{false, true} {
		var buf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		warnDeprecatedServeFlags(log, set)
		lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
		if !set {
			if buf.Len() != 0 {
				t.Fatalf("logged without the flag: %q", buf.String())
			}
			continue
		}
		if len(lines) != 1 || !strings.Contains(lines[0], "level=WARN") || !strings.Contains(lines[0], "--desktop-downloads") ||
			!strings.Contains(lines[0], "deprecated") {
			t.Fatalf("want one WARN naming --desktop-downloads, got %q", buf.String())
		}
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
