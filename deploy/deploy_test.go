package deploy_test

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheUnitVerifies(t *testing.T) {
	if _, err := exec.LookPath("systemd-analyze"); err != nil {
		t.Skip("systemd-analyze is not installed on this box: skipping the unit verification")
	}
	// "dilla.service", not "deploy/dilla.service": go test runs a package with its
	// working directory set to the package directory, which is deploy/ — the same
	// reason read(t, "dilla.service") below is
	// spelled the way it is.
	// systemd-analyze exits non-zero for the two expected warnings too (systemd
	// 262 does for a missing ExecStart binary), so the exit status alone is not
	// the verdict: the output lines are.
	out, err := exec.CommandContext(t.Context(), "systemd-analyze", "verify", "dilla.service").CombinedOutput()
	// Two warning classes are expected on any box that is not the deployment
	// target and are ignored by name: the ExecStart binary is not installed, and
	// the dillad user does not exist. Anything else fails the test, so a real
	// directive error is still caught.
	ignored := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			continue
		case strings.Contains(line, "is not executable"),
			strings.Contains(line, "Failed to resolve user"),
			strings.Contains(line, "Failed to resolve group"):
			ignored++
		default:
			t.Errorf("systemd-analyze verify: %s", line)
		}
	}
	if err != nil && ignored == 0 {
		t.Fatalf("systemd-analyze verify: %v\n%s", err, out)
	}
}

func TestTheUnitCarriesTheRequiredDirectives(t *testing.T) {
	unit := read(t, "dilla.service")
	for _, want := range []string{
		"Type=notify",
		"NotifyAccess=main",
		"StateDirectory=dillad",
		"StateDirectoryMode=0700",
		"UMask=0077",
		"RestartPreventExitStatus=78",
		"AmbientCapabilities=CAP_NET_BIND_SERVICE",
		"CapabilityBoundingSet=CAP_NET_BIND_SERVICE",
		"NoNewPrivileges=yes",
		"ProtectSystem=strict",
		// AF_NETLINK (C8, fix wave): net.Interfaces, LiveKit's SFU setup and pion's stdnet read the
		// interface list over netlink; without it serve cannot start with the default config.
		"RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK",
		"TimeoutStartSec=300s",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("the unit is missing %q", want)
		}
	}
}

// The absence is the assertion, and the reason is in the unit file beside it.
func TestTheUnitDoesNotSetMemoryDenyWriteExecute(t *testing.T) {
	unit := read(t, "dilla.service")
	for _, line := range strings.Split(unit, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "MemoryDenyWriteExecute=") {
			t.Fatalf("MemoryDenyWriteExecute is set (%q); wazero's compiler back end writes and then executes generated machine code, so the wasi path would fail at runtime, not at start", trimmed)
		}
	}
	if !strings.Contains(unit, "wazero") {
		t.Fatal("the unit must carry a comment naming wazero as the reason MemoryDenyWriteExecute is absent")
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// Bot v2 task 11: dillad supervises the Bot. The keeper keeps CAP_SETUID and CAP_SETGID to start it as the dillabot
// user (CAP_SETPCAP lets the serving process and the launcher drop them from the bounding set), and CAP_KILL to
// signal it (v0.1.0-bot2.1: the Bot runs as another user, so without it every stop timed out); the Bot's bubblewrap
// sandbox needs the device and set-id rules the dillabot unit already had; its state directory is writable beside
// dillad's; the socket lives under dillad's runtime directory. Each of these lines carries its reason in the unit.
func TestDillaServiceAllowsBubblewrapAndKeeper(t *testing.T) {
	unit := read(t, "dilla.service")
	directives := map[string]string{}
	for _, line := range strings.Split(unit, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("unit line %q is not key=value", line)
		}
		if _, dup := directives[key]; dup {
			t.Fatalf("directive %s is set twice", key)
		}
		directives[key] = value
	}
	for key, want := range map[string]string{
		"RuntimeDirectory":      "dillad",
		"ReadWritePaths":        "-/var/lib/dillabot",
		"AmbientCapabilities":   "CAP_NET_BIND_SERVICE CAP_SETUID CAP_SETGID CAP_SETPCAP CAP_KILL",
		"CapabilityBoundingSet": "CAP_NET_BIND_SERVICE CAP_SETUID CAP_SETGID CAP_SETPCAP CAP_KILL",
		"DevicePolicy":          "closed",
		"LimitMEMLOCK":          "256M",
		"NoNewPrivileges":       "yes",
		"ProtectSystem":         "strict",
		"ProtectHome":           "yes",
		"PrivateTmp":            "yes",
		"ProtectProc":           "invisible",
		"ProtectKernelTunables": "yes",
		"ProtectKernelModules":  "yes",
		"ProtectControlGroups":  "yes",
		"LockPersonality":       "yes",
		"UMask":                 "0077",
		"KillMode":              "mixed",
	} {
		if got, ok := directives[key]; !ok || got != want {
			t.Errorf("%s=%q (set: %v), want %q", key, got, ok, want)
		}
	}
	for _, gone := range []string{"PrivateDevices", "RestrictSUIDSGID"} {
		if v, ok := directives[gone]; ok {
			t.Errorf("%s=%s is set; bubblewrap's nested mounts need it gone (see the unit comment)", gone, v)
		}
	}
	// Each change says why, next to it.
	for _, reason := range []string{"bot-keeper", "bubblewrap", "SQLCipher", "/run/dillad/bot", "dillabot", "KillMode=mixed: a stop sends SIGTERM to serve alone"} {
		if !strings.Contains(unit, reason) {
			t.Errorf("the unit's comments never mention %q", reason)
		}
	}
}

// Bot v2 task 24: the Bot ships with dillad and runs as its supervised child, so no dillabot.service is shipped, and
// no script writes one. install-bot.sh only retires a v1 unit; its rollback puts back the operator's own copy from the
// backup, never a unit of its own.
func TestNoDillabotUnitShipped(t *testing.T) {
	if _, err := os.Stat("dillabot.service"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("deploy/dillabot.service is still shipped (stat: %v); dillad supervises the Bot", err)
	}
	scripts, err := filepath.Glob("*.sh")
	if err != nil || len(scripts) == 0 {
		t.Fatalf("no deploy scripts found: %v", err)
	}
	for _, script := range scripts {
		if strings.HasSuffix(script, "_test.sh") {
			continue
		}
		body := read(t, script)
		for _, unitLine := range []string{
			"ExecStart=/usr/local/bin/dillabot",
			"Description=Dilla community bot",
			"systemctl enable --now dillabot",
		} {
			if strings.Contains(body, unitLine) {
				t.Errorf("%s still carries %q: the Bot has no unit of its own", script, unitLine)
			}
		}
	}
	// install.sh and install-bot.sh carry deploy/dilla.service verbatim (their tests compare the installed copy).
	unit := read(t, "dilla.service")
	for _, script := range []string{"install.sh", "install-bot.sh"} {
		if !strings.Contains(read(t, script), unit) {
			t.Errorf("%s does not carry deploy/dilla.service verbatim", script)
		}
	}
}
