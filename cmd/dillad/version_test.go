package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// release.yml stamps the tag (without its leading v) into dillad at link time:
// -ldflags "-X main.Version=<version>". The linker sets only a package-level
// string variable and silently ignores a constant, so this builds dillad the way
// the release does and runs its version verb. Every other build, server.yml's
// included, is a development build and says 0.0.0-dev.
func TestVersionIsStampedAtLink(t *testing.T) {
	if testing.Short() {
		t.Skip("builds dillad")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("this test builds dillad and needs go on PATH: %v", err)
	}
	for _, tc := range []struct {
		name, ldflags, want string
	}{
		{"plain build", "", "dillad 0.0.0-dev "},
		{"release build", "-s -w -X main.Version=0.1.0-web3.1", "dillad 0.1.0-web3.1 "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "dillad")
			args := []string{"build", "-trimpath"}
			if tc.ldflags != "" {
				args = append(args, "-ldflags", tc.ldflags)
			}
			build := exec.CommandContext(t.Context(), goBin, append(args, "-o", bin, ".")...)
			build.Env = append(os.Environ(), "CGO_ENABLED=0")
			if out, err := build.CombinedOutput(); err != nil {
				t.Fatalf("go build: %v\n%s", err, out)
			}
			out, err := exec.CommandContext(t.Context(), bin, "version").Output()
			if err != nil {
				t.Fatalf("dillad version: %v", err)
			}
			if !strings.HasPrefix(string(out), tc.want) {
				t.Fatalf("dillad version printed %q, want it to start with %q", out, tc.want)
			}
		})
	}
}
