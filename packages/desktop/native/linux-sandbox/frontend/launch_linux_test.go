//go:build linux && cgo

package frontend

import (
	"strings"
	"testing"
)

func TestLaunchDecisionPreservesManagedStates(t *testing.T) {
	for _, tc := range []struct {
		state string
		want  launchDecision
	}{
		{"missing", decisionProbe}, {"ready", decisionExec}, {"maintenance", decisionRepair}, {"removed", decisionStop}, {"unsafe", decisionStop},
	} {
		if got := decideLaunch(tc.state); got != tc.want {
			t.Errorf("state=%s got %v want %v", tc.state, got, tc.want)
		}
	}
}

func TestFatalRequiresOwnedSignalExactPinnedMessage(t *testing.T) {
	const fatal = "[3134:1102/120603.375595:FATAL:content/browser/zygote_host/zygote_host_impl_linux.cc:130] No usable sandbox! If you are running on Ubuntu 23.10+ or another Linux distro that has disabled unprivileged user namespaces with AppArmor, see https://chromium.googlesource.com/chromium/src/+/main/docs/security/apparmor-userns-restrictions.md. Otherwise see https://chromium.googlesource.com/chromium/src/+/main/docs/linux/suid_sandbox_development.md for more information on developing with the (older) SUID sandbox. If you want to live dangerously and need an immediate workaround, you can try using --no-sandbox.\n"
	for _, tc := range []struct {
		name                     string
		owned, ack, bad, timeout bool
		signal                   int
		stderr                   string
		want                     ProbeResult
	}{
		{"captured fatal", true, false, false, false, 5, fatal, ProbeSandboxUnavailable},
		{"foreign", false, false, false, false, 5, fatal, ProbeFailed},
		{"text only", true, false, false, false, 0, fatal, ProbeFailed},
		{"wrong signal", true, false, false, false, 6, fatal, ProbeFailed},
		{"truncated", true, false, false, false, 5, fatal[:100], ProbeFailed},
		{"forged fixture", true, false, false, false, 5, "[FATAL:zygote_host_impl_linux.cc:132] No usable sandbox!\n", ProbeFailed},
		{"ack", true, true, false, false, 5, fatal, ProbeFailed},
		{"bad pipe", true, false, true, false, 5, fatal, ProbeFailed},
		{"timeout", true, false, false, true, 5, fatal, ProbeTimedOut},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyFailure(tc.owned, tc.ack, tc.bad, tc.timeout, tc.signal, tc.stderr); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

func TestReservedLaunchArgs(t *testing.T) {
	if err := validateLaunchArgs([]string{"dilla://invite?x=--update-watchdog"}); err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{"--update-watchdog=old", "--update-launch=old", "--update-recovery", "--dilla-sandbox-probe=x", "--setup-sandbox"} {
		if validateLaunchArgs([]string{arg}) == nil {
			t.Fatalf("accepted reserved role %s", arg)
		}
	}
}
func TestStockInstallOnlyLaunchCannotProvisionOrRunElectron(t *testing.T) {
	t.Setenv("APPIMAGE_EXIT_AFTER_INSTALL", "true")
	// Empty compiled configuration would fail ordinary startup before resolving
	// a mount or contacting the component. Install-only returns without either.
	if err := DispatchLaunchVersion(nil, EmbeddedCapsule{}, "1.0.1"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--dilla-sandbox-prepare-update=" + strings.Repeat("a", 64)}, {"--setup-sandbox"}, {"dilla://invite/a"}} {
		if DispatchLaunchVersion(args, EmbeddedCapsule{}, "1.0.1") == nil {
			t.Fatal("accepted mixed install-only role")
		}
	}
	t.Setenv("APPIMAGE_EXIT_AFTER_INSTALL", "")
	if DispatchLaunchVersion(nil, EmbeddedCapsule{}, "1.0.1") == nil {
		t.Fatal("ordinary startup bypassed compiled configuration")
	}
}

func TestMountInfoRequiresExactReadOnlyFuseMount(t *testing.T) {
	info := "21 10 0:40 / /tmp/.mount_dilla rw,nosuid - fuse.squashfuse dilla ro,user_id=1000\n22 10 0:41 / /other ro - ext4 /dev/x ro\n23 10 0:42 / /tmp/rw rw - fuse.squashfuse dilla rw\n"
	if !readOnlyFuseMount(info, 21) {
		t.Fatal("valid FUSE mount not recognized")
	}
	for _, id := range []uint64{0, 22, 23, 24} {
		if readOnlyFuseMount(info, id) {
			t.Fatalf("accepted mount %d", id)
		}
	}
}

func TestProbeReportRequiresExactBoundedOwnedFrame(t *testing.T) {
	n := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	good := `{"format":1,"kind":"dilla-sandbox-probe","nonce":"` + n + `","mainPid":12,"rendererPid":13,"sandboxed":true,"contextIsolated":true}` + "\n"
	for _, tc := range []struct {
		raw  string
		good bool
	}{
		{good, true}, {strings.Replace(good, n, "wrong", 1), false},
		{strings.Replace(good, `"mainPid":12`, `"mainPid":14`, 1), false},
		{strings.Replace(good, `"sandboxed":true`, `"sandboxed":false`, 1), false},
		{strings.TrimSuffix(good, "\n"), false}, {good + good, false},
		{strings.Replace(good, `"format":1`, `"format":1,"format":1`, 1), false},
		{strings.Repeat("x", 4097), false},
	} {
		_, e := parseProbeReport([]byte(tc.raw), n, 12)
		if (e == nil) != tc.good {
			t.Errorf("report %q: %v", tc.raw, e)
		}
	}
}

func TestDispatchProbeAndRepairSequence(t *testing.T) {
	var calls []string
	deps := dispatchDeps{
		status: func() (string, error) {
			calls = append(calls, "status")
			if len(calls) == 1 {
				return "missing", nil
			}
			return "ready", nil
		},
		probe: func() (ProbeResult, error) {
			calls = append(calls, "probe")
			if len(calls) == 2 {
				return ProbeSandboxUnavailable, nil
			}
			return ProbeSandboxed, nil
		},
		ensure: func() error { calls = append(calls, "ensure"); return nil },
		exec:   func(args []string) error { calls = append(calls, "exec:"+strings.Join(args, "|")); return nil },
	}
	args := []string{"two words", "dilla://invite/a"}
	if e := dispatch(args, deps); e != nil {
		t.Fatal(e)
	}
	got := strings.Join(calls, ",")
	want := "status,probe,ensure,status,probe,exec:two words|dilla://invite/a"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestValidatedLaunchStateRejectsIncompatibleReady(t *testing.T) {
	hash := strings.Repeat("A", 86) + "=="
	e := EmbeddedCapsule{unsigned: []byte{1}, constants: BuildConstants{Generation: 2, CompatibilityEpoch: 1, SandboxAPI: 1, MaintainerSHA512: hash}, helperSHA512: hash}
	ready := Status{Format: 1, Status: "ready", Generation: 2, CompatibilityEpoch: 1, SandboxAPI: 1, MaintainerSHA512: hash, HelperSHA512: hash, resident: true}
	if got := launchState(ready, e); got != "ready" {
		t.Fatalf("got %s", got)
	}
	ready.Generation = 1
	if got := launchState(ready, e); got != "maintenance" {
		t.Fatalf("old generation: %s", got)
	}
	ready.Generation = 2
	ready.HelperSHA512 = strings.Repeat("B", 86) + "=="
	if got := launchState(ready, e); got != "unsafe" {
		t.Fatalf("invalid hash: %s", got)
	}
	if got := launchState(Status{Format: 1, Status: "missing"}, e); got != "missing" {
		t.Fatalf("missing: %s", got)
	}
}

func TestDispatchLaunchRejectsMissingCompiledConfiguration(t *testing.T) {
	if e := DispatchLaunch(nil, EmbeddedCapsule{}); e == nil {
		t.Fatal("accepted missing production constants")
	}
}

func TestFailedProbeNeverOffersSetup(t *testing.T) {
	for _, result := range []ProbeResult{ProbeFailed, ProbeTimedOut} {
		called := false
		deps := dispatchDeps{status: func() (string, error) { return "missing", nil }, probe: func() (ProbeResult, error) { return result, nil }, ensure: func() error { called = true; return nil }, exec: func([]string) error { called = true; return nil }}
		if dispatch(nil, deps) == nil || called {
			t.Fatalf("result %s offered setup or executed", result)
		}
	}
}
func TestLaunchEnvironmentRemovesSandboxOverrides(t *testing.T) {
	env := launchEnvironment([]string{"APPDIR=/untrusted", "CHROME_DEVEL_SANDBOX=/tmp/x", "NODE_OPTIONS=bad", "LD_PRELOAD=bad", "DISPLAY=:1"}, "/mounted")
	got := strings.Join(env, "|")
	if got != "DISPLAY=:1|APPDIR=/mounted" {
		t.Fatal(got)
	}
}
