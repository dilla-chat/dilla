package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/dillad/dilladtest"
)

func TestTheWebRootAndACLFlagsReachTheHostOptions(t *testing.T) {
	dir := t.TempDir()
	o, err := parseFlags([]string{"-listen", "127.0.0.1:8463", "-control", "127.0.0.1:8464",
		"-web-root", dir, "-production-acl"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if o.public != "127.0.0.1:8463" || o.control != "127.0.0.1:8464" || o.webRoot != dir || !o.productionACL {
		t.Fatalf("options %+v", o)
	}
	ho := o.hostOptions("/data", "/core.wasm", "token")
	if ho.WebRoot != dir || !ho.ProductionACL || ho.DataDir != "/data" || ho.CorePath != "/core.wasm" ||
		ho.ScrapeToken != "token" || ho.LogLevel != "warn" {
		t.Fatalf("host options %+v", ho)
	}
}

func TestWithoutTheNewFlagsTheHostKeepsThePlaceholderAndTheHarnessSeams(t *testing.T) {
	o, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if o.webRoot != "" || o.productionACL || o.public != "127.0.0.1:8443" || o.control != "127.0.0.1:8444" ||
		o.logLevel != "warn" || o.sfu.enabled || o.sfu.port != dilladtest.DefaultSFUPort ||
		o.sfu.udpPort != dilladtest.DefaultSFUUDPPort {
		t.Fatalf("defaults %+v", o)
	}
	if ho := o.hostOptions("/data", "/core.wasm", ""); ho.WebRoot != "" || ho.ProductionACL || ho.SFU {
		t.Fatalf("default host options %+v", ho)
	}
}

// web-3 task 3: a host that serves a web client and runs an SFU hands the browser its own public
// listener as the LiveKit URL (ws://, since the test host has no TLS); either flag alone keeps the
// instance's default.
func TestASFUHostWithAWebRootNamesItsListenerAsTheLiveKitURL(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"-listen", "127.0.0.1:8481", "-sfu", "-web-root", dir}, "ws://127.0.0.1:8481"},
		{[]string{"-listen", "127.0.0.1:8481", "-sfu"}, ""},
		{[]string{"-listen", "127.0.0.1:8481", "-web-root", dir}, ""},
	} {
		o, err := parseFlags(c.args)
		if err != nil {
			t.Fatalf("parseFlags %v: %v", c.args, err)
		}
		if got := o.hostOptions("/data", "/core.wasm", "").LiveKitURL; got != c.want {
			t.Errorf("%v: LiveKitURL = %q, want %q", c.args, got, c.want)
		}
	}
}

// Task-10 fix review N3: -sfu-departure-timeout reaches the host options; absent, it is zero (the
// instance default).
func TestTheSFUDepartureTimeoutFlagReachesTheHostOptions(t *testing.T) {
	o, err := parseFlags([]string{"-sfu", "-sfu-departure-timeout", "60"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if got := o.hostOptions("/data", "/core.wasm", "").SFUDepartureTimeout; got != 60 {
		t.Fatalf("SFUDepartureTimeout = %d, want 60", got)
	}
	o, err = parseFlags([]string{"-sfu"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if got := o.hostOptions("/data", "/core.wasm", "").SFUDepartureTimeout; got != 0 {
		t.Fatalf("default SFUDepartureTimeout = %d, want 0", got)
	}
}

func TestARelativeWebRootIsResolvedAgainstTheWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "dist"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Chdir(dir)
	o, err := parseFlags([]string{"-web-root", "dist"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	want, err := filepath.Abs("dist")
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	if o.webRoot != want || !filepath.IsAbs(o.webRoot) {
		t.Fatalf("-web-root dist resolved to %q, want %q", o.webRoot, want)
	}
}

func TestAWebRootThatIsNotADirectoryIsRefused(t *testing.T) {
	file := filepath.Join(t.TempDir(), "index.html")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, root := range []string{file, filepath.Join(t.TempDir(), "missing")} {
		if _, err := parseFlags([]string{"-web-root", root}); err == nil || !strings.Contains(err.Error(), "-web-root") {
			t.Errorf("-web-root %s: parseFlags = %v, want a -web-root refusal", root, err)
		}
	}
}
