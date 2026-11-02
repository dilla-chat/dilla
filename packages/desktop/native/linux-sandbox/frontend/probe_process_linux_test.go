//go:build linux && cgo

package frontend

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestProbeSessionRejectsSiblingJoiningGroup(t *testing.T) {
	if isolatedProbeTest(t) {
		return
	}
	n := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	attempted := false
	result, e := runProbeProcess(probeProcessConfig{executable: os.Args[0], args: []string{"-test.run=^TestProbeFixtureChild$"}, env: append(os.Environ(), "DILLA_TEST_PROBE_CHILD=1"), deadline: time.Now().Add(5 * time.Second), nonce: n, inspect: func(main, _, _ int) error {
		attempted = true
		sibling := exec.CommandContext(t.Context(), "/bin/sleep", "30")
		sibling.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: main}
		err := sibling.Start()
		if err == nil {
			_ = sibling.Process.Kill()
			_ = sibling.Wait()
			return fmt.Errorf("sibling joined probe group")
		}
		if !errors.Is(err, syscall.EPERM) {
			return fmt.Errorf("unexpected setpgid error: %w", err)
		}
		return nil
	}})
	if !attempted || e != nil || result != ProbeSandboxed {
		t.Fatalf("attempted=%v result=%q error=%v", attempted, result, e)
	}
}

func TestParseProcessStatusRequiresSandboxFacts(t *testing.T) {
	raw := "Name:\trenderer\nPPid:\t42\nUid:\t1000\t1000\t1000\t1000\nSeccomp:\t2\nNoNewPrivs:\t1\nCapEff:\t0000000000000000\n"
	if _, e := parseProcessStatus(raw, 1000, true); e != nil {
		t.Fatal(e)
	}
	for _, broken := range []string{
		"PPid: 42\nUid: 1000 1000 1000 1000\nSeccomp: 0\nNoNewPrivs: 1\nCapEff: 0\n",
		"PPid: 42\nUid: 1000 1000 0 1000\nSeccomp: 2\nNoNewPrivs: 1\nCapEff: 0\n",
		"PPid: 42\nUid: 1000 1000 1000 1000\nSeccomp: 2\nNoNewPrivs: 1\nCapEff: 1\n",
		"PPid: 42\nUid: 1000 1000 1000 1000\nSeccomp: 2\nNoNewPrivs: 1\n",
		raw + "Seccomp: 2\n",
	} {
		if _, e := parseProcessStatus(broken, 1000, true); e == nil {
			t.Fatalf("accepted %q", broken)
		}
	}
}

func isolatedProbeTest(t *testing.T) bool {
	t.Helper()
	if os.Getenv("DILLA_TEST_PROBE_SCOPE") == "1" {
		return false
	}
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^"+t.Name()+"$")
	command.Env = append(os.Environ(), "DILLA_TEST_PROBE_SCOPE=1")
	if output, e := command.CombinedOutput(); e != nil {
		t.Fatalf("isolated: %v %s", e, output)
	}
	return true
}
func TestProbeProcessRejectsTimeoutAndReapsChild(t *testing.T) {
	if isolatedProbeTest(t) {
		return
	}
	result, e := runProbeProcess(probeProcessConfig{executable: "/bin/sleep", args: []string{"1"}, deadline: time.Now().Add(500 * time.Millisecond), inspect: func(int, int, int) error { return nil }})
	if e != nil || result != ProbeTimedOut {
		t.Fatalf("result %q error %v", result, e)
	}
}

func TestProbeFixtureChild(t *testing.T) {
	if os.Getenv("DILLA_TEST_PROBE_CHILD") != "1" {
		return
	}
	n := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	_, _ = fmt.Fprintf(os.NewFile(3, "report"), "{\"format\":1,\"kind\":\"dilla-sandbox-probe\",\"nonce\":\"%s\",\"mainPid\":%d,\"rendererPid\":999,\"sandboxed\":true,\"contextIsolated\":true}\n", n, os.Getpid())
	var ack [1]byte
	if _, e := io.ReadFull(os.NewFile(4, "ack"), ack[:]); e != nil || ack[0] != 1 {
		os.Exit(7)
	}
	os.Exit(0)
}
func TestProbeProcessAcceptsOneOwnedReport(t *testing.T) {
	if isolatedProbeTest(t) {
		return
	}
	n := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	result, e := runProbeProcess(probeProcessConfig{executable: os.Args[0], args: []string{"-test.run=^TestProbeFixtureChild$"}, env: append(os.Environ(), "DILLA_TEST_PROBE_CHILD=1"), deadline: time.Now().Add(3 * time.Second), nonce: n, inspect: func(main, renderer, uid int) error {
		if main < 2 || renderer != 999 || uid != os.Getuid() {
			return errProbeReport
		}
		return nil
	}})
	if e != nil || result != ProbeSandboxed {
		t.Fatalf("result %q error %v", result, e)
	}
}

func TestProbeDeadlineIncludesDelayedPreflight(t *testing.T) {
	called := false
	cleaned := false
	result, e := probeLifecycle(20*time.Millisecond, func(deadline time.Time) (func(time.Time) (ProbeResult, error), func() error, error) {
		time.Sleep(40 * time.Millisecond)
		return func(time.Time) (ProbeResult, error) { called = true; return ProbeSandboxed, nil }, func() error { cleaned = true; return nil }, nil
	})
	if result != ProbeTimedOut || e != nil || called || !cleaned {
		t.Fatalf("result=%q err=%v process=%v cleaned=%v", result, e, called, cleaned)
	}
}
func TestProbeDeadlineIncludesDelayedCleanup(t *testing.T) {
	for _, childResult := range []ProbeResult{ProbeSandboxed, ProbeSandboxUnavailable} {
		t.Run(string(childResult), func(t *testing.T) {
			result, e := probeLifecycle(20*time.Millisecond, func(deadline time.Time) (func(time.Time) (ProbeResult, error), func() error, error) {
				return func(got time.Time) (ProbeResult, error) {
					if !got.Equal(deadline) {
						t.Error("deadline changed")
					}
					return childResult, nil
				}, func() error { time.Sleep(40 * time.Millisecond); return nil }, nil
			})
			if result != ProbeTimedOut || e != nil {
				t.Fatalf("late cleanup result=%q err=%v", result, e)
			}
		})
	}
}

func TestProbeDeadlineIncludesDelayedTeardown(t *testing.T) {
	cleaned := false
	result, e := probeLifecycle(20*time.Millisecond, func(deadline time.Time) (func(time.Time) (ProbeResult, error), func() error, error) {
		return func(got time.Time) (ProbeResult, error) {
			if !got.Equal(deadline) {
				t.Error("deadline changed")
			}
			time.Sleep(40 * time.Millisecond)
			return ProbeSandboxUnavailable, nil
		}, func() error { cleaned = true; return nil }, nil
	})
	if result != ProbeTimedOut || e != nil || !cleaned {
		t.Fatalf("result=%q err=%v cleaned=%v", result, e, cleaned)
	}
}

func TestProbeHoldsPipeChild(t *testing.T) {
	if os.Getenv("DILLA_TEST_HOLD_PIPE") != "1" {
		return
	}
	command := exec.CommandContext(t.Context(), "/bin/sleep", "30")
	command.ExtraFiles = []*os.File{os.NewFile(3, "report"), os.NewFile(4, "ack")}
	if command.Start() != nil {
		os.Exit(9)
	}
	n := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	_, _ = fmt.Fprintf(os.NewFile(3, "report"), "{\"format\":1,\"kind\":\"dilla-sandbox-probe\",\"nonce\":\"%s\",\"mainPid\":%d,\"rendererPid\":%d,\"sandboxed\":true,\"contextIsolated\":true}\n", n, os.Getpid(), command.Process.Pid)
	var ack [1]byte
	if _, e := io.ReadFull(os.NewFile(4, "ack"), ack[:]); e != nil || ack[0] != 1 {
		os.Exit(7)
	}
	os.Exit(0)
}
func TestProbeTimeoutKillsDescendantHoldingPipe(t *testing.T) {
	if isolatedProbeTest(t) {
		return
	}
	start := time.Now()
	known := map[int]knownProcess{}
	observed := false
	n := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	result, e := runProbeProcess(probeProcessConfig{executable: os.Args[0], args: []string{"-test.run=^TestProbeHoldsPipeChild$"}, env: append(os.Environ(), "DILLA_TEST_HOLD_PIPE=1"), deadline: start.Add(3 * time.Second), nonce: n, known: known, inspect: func(main, child, uid int) error {
		raw, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", child))
		if e != nil {
			return e
		}
		_, parent, _, start, e := parseOwnedStat(string(raw))
		if e != nil || parent != main {
			return errProbeReport
		}
		fd, e := unix.PidfdOpen(child, 0)
		if e != nil {
			return e
		}
		known[child] = knownProcess{start: start, pidfd: fd}
		observed = true
		return nil
	}})
	if result != ProbeTimedOut || e != nil || !observed {
		t.Fatalf("result=%q err=%v observed=%v", result, e, observed)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatal("owned descendant teardown exceeded test limit")
	}
}

func TestProbeDoesNotReturnWhileSamplerRuns(t *testing.T) {
	if os.Getenv("DILLA_TEST_ISOLATED_SAMPLER") != "1" {
		command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestProbeDoesNotReturnWhileSamplerRuns$")
		command.Env = append(os.Environ(), "DILLA_TEST_ISOLATED_SAMPLER=1")
		if out, e := command.CombinedOutput(); e != nil {
			t.Fatalf("isolated: %v %s", e, out)
		}
		return
	}
	n := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	finished := make(chan struct{})
	script := `printf '{"format":1,"kind":"dilla-sandbox-probe","nonce":"%s","mainPid":%s,"rendererPid":999,"sandboxed":true,"contextIsolated":true}\n' "$1" "$$" >&3; read -r x <&4`
	result, e := runProbeProcess(probeProcessConfig{executable: "/bin/sh", args: []string{"-c", script, "probe", n}, deadline: time.Now().Add(70 * time.Millisecond), nonce: n, inspect: func(int, int, int) error { time.Sleep(150 * time.Millisecond); close(finished); return nil }})
	select {
	case <-finished:
	default:
		t.Fatal("returned while sampler still active")
	}
	if result != ProbeTimedOut {
		t.Fatalf("result=%s err=%v", result, e)
	}
}

func TestUnexpectedChildDuringScopeNeverSignalsProbeGroup(t *testing.T) {
	if isolatedProbeTest(t) {
		return
	}
	n := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	mainPID := 0
	var unexpected *exec.Cmd
	script := `printf '{"format":1,"kind":"dilla-sandbox-probe","nonce":"%s","mainPid":%s,"rendererPid":999,"sandboxed":true,"contextIsolated":true}\n' "$1" "$$" >&3; read -r x <&4; exec sleep 30`
	result, e := runProbeProcess(probeProcessConfig{executable: "/bin/sh", args: []string{"-c", script, "probe", n}, deadline: time.Now().Add(250 * time.Millisecond), nonce: n, inspect: func(main, renderer, uid int) error {
		mainPID = main
		unexpected = exec.CommandContext(t.Context(), "/bin/sleep", "30")
		return unexpected.Start()
	}})
	defer func() {
		if unexpected != nil {
			_ = unexpected.Process.Kill()
			_ = unexpected.Wait()
		}
		if mainPID > 1 {
			_ = syscall.Kill(mainPID, syscall.SIGKILL)
			_, _ = syscall.Wait4(mainPID, nil, 0, nil)
		}
	}()
	if e == nil || result == ProbeSandboxed || mainPID < 2 || unexpected == nil {
		t.Fatalf("result=%s err=%v pid=%d", result, e, mainPID)
	}
	if exited, err := leaderWaitState(mainPID); err != nil || exited {
		t.Fatalf("owned leader exited under uncertain ownership: exited=%v err=%v", exited, err)
	}
	if err := unexpected.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("unexpected child was signaled: %v", err)
	}
}

func TestProbeLiveParentChild(t *testing.T) {
	if os.Getenv("DILLA_TEST_LIVE_TREE") != "1" {
		return
	}
	child := exec.CommandContext(t.Context(), "/bin/sleep", "30")
	if os.Getenv("DILLA_TEST_ESCAPE") == "1" {
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	if child.Start() != nil {
		os.Exit(9)
	}
	n := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	_, _ = fmt.Fprintf(os.NewFile(3, "report"), "{\"format\":1,\"kind\":\"dilla-sandbox-probe\",\"nonce\":\"%s\",\"mainPid\":%d,\"rendererPid\":%d,\"sandboxed\":true,\"contextIsolated\":true}\n", n, os.Getpid(), child.Process.Pid)
	var ack [1]byte
	if _, e := io.ReadFull(os.NewFile(4, "ack"), ack[:]); e != nil || ack[0] != 1 {
		os.Exit(7)
	}
	time.Sleep(30 * time.Second)
}
func TestTimeoutDiscoversOwnedLiveTreeBeforeGroupSignal(t *testing.T) {
	if isolatedProbeTest(t) {
		return
	}
	observed := false
	n := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	result, e := runProbeProcess(probeProcessConfig{executable: os.Args[0], args: []string{"-test.run=^TestProbeLiveParentChild$"}, env: append(os.Environ(), "DILLA_TEST_LIVE_TREE=1"), deadline: time.Now().Add(3 * time.Second), nonce: n, inspect: func(int, int, int) error { observed = true; return nil }})
	if !observed || result != ProbeTimedOut || e != nil {
		t.Fatalf("result=%s err=%v observed=%v", result, e, observed)
	}
}

func TestEscapedOwnedGroupFailsWithoutSignal(t *testing.T) {
	if isolatedProbeTest(t) {
		return
	}
	mainPID, childPID := 0, 0
	n := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	result, e := runProbeProcess(probeProcessConfig{executable: os.Args[0], args: []string{"-test.run=^TestProbeLiveParentChild$"}, env: append(os.Environ(), "DILLA_TEST_LIVE_TREE=1", "DILLA_TEST_ESCAPE=1"), deadline: time.Now().Add(3 * time.Second), nonce: n, inspect: func(main, child, uid int) error { mainPID, childPID = main, child; return nil }})
	defer func() {
		if childPID > 1 {
			_ = syscall.Kill(childPID, syscall.SIGKILL)
		}
		if mainPID > 1 {
			_ = syscall.Kill(mainPID, syscall.SIGKILL)
			_, _ = syscall.Wait4(mainPID, nil, 0, nil)
		}
	}()
	if e == nil || result == ProbeSandboxed || mainPID < 2 || childPID < 2 {
		t.Fatalf("result=%s err=%v main=%d child=%d", result, e, mainPID, childPID)
	}
	if exited, err := leaderWaitState(mainPID); err != nil || exited {
		t.Fatalf("leader signaled: exited=%v err=%v", exited, err)
	}
	if e := syscall.Kill(childPID, 0); e != nil {
		t.Fatalf("escaped child signaled: %v", e)
	}
}
