//go:build linux && cgo

package frontend

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestWaitidLeavesExitedGroupLeaderUnreaped(t *testing.T) {
	cmd := exec.Command("/bin/true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	pid := cmd.Process.Pid
	done := false
	defer func() {
		if !done {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	until := time.Now().Add(2 * time.Second)
	for {
		var info unix.Siginfo
		if e := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil); e != nil {
			t.Fatal(e)
		}
		if info.Signo != 0 {
			break
		}
		if time.Now().After(until) {
			t.Fatal("child did not exit")
		}
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < 2; i++ {
		var info unix.Siginfo
		if e := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil); e != nil || info.Signo != int32(syscall.SIGCHLD) {
			t.Fatalf("waitid %v signo %d", e, info.Signo)
		}
	}
	if pg, e := unix.Getpgid(pid); e != nil || pg != pid {
		t.Fatalf("unreaped leader group=%d err=%v", pg, e)
	}
	if e := unix.Kill(-pid, unix.SIGKILL); e != nil && !errors.Is(e, syscall.ESRCH) {
		t.Fatal(e)
	}
	if e := cmd.Wait(); e != nil {
		t.Fatal(e)
	}
	done = true
	var info unix.Siginfo
	if e := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil); !errors.Is(e, syscall.ECHILD) {
		t.Fatalf("expected ECHILD after reaping, got %v", e)
	}
}

func TestNoSignalWithoutUnreapedMatchingLeader(t *testing.T) {
	for _, tc := range []struct {
		name     string
		owned    bool
		group    int
		session  int
		checkErr error
	}{
		{"reaped", false, 42, 42, nil}, {"wrong group", true, 43, 42, nil}, {"wrong session", true, 42, 43, nil}, {"unknown", false, 0, 0, syscall.ECHILD},
	} {
		t.Run(tc.name, func(t *testing.T) {
			signaled := false
			e := signalOwnedGroup(42, func(int) (bool, error) { return tc.owned, tc.checkErr }, func(int) (int, error) { return tc.group, nil }, func(int) (int, error) { return tc.session, nil }, func(int, syscall.Signal) error { signaled = true; return nil })
			if e == nil || signaled {
				t.Fatalf("err=%v signaled=%v", e, signaled)
			}
		})
	}
}
func TestOwnershipTraversalNeverInspectsForeignOrIncomplete(t *testing.T) {
	for _, tc := range []struct {
		name     string
		target   int
		children map[int][]int
		fail     int
		valid    bool
	}{
		{"owned", 4, map[int][]int{1: {2}, 2: {4}}, 0, true},
		{"foreign", 9, map[int][]int{1: {2}, 2: {4}}, 0, false},
		{"incomplete", 4, map[int][]int{1: {2}, 2: {4}}, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inspected := []int{}
			e := walkOwned(1, tc.target, func(pid int) ([]int, error) {
				if pid == tc.fail {
					return nil, errors.New("incomplete")
				}
				return tc.children[pid], nil
			}, func(pid int) error { inspected = append(inspected, pid); return nil })
			if (e == nil) != tc.valid {
				t.Fatalf("err=%v", e)
			}
			if tc.valid {
				if len(inspected) != 1 || inspected[0] != 4 {
					t.Fatalf("inspected %v", inspected)
				}
			} else if len(inspected) != 0 {
				t.Fatalf("inspected unowned %v", inspected)
			}
		})
	}
}

func TestOwnedGroupSignalChecksBeforeSending(t *testing.T) {
	var order []string
	err := signalOwnedGroup(42, func(int) (bool, error) { order = append(order, "waitid"); return true, nil }, func(int) (int, error) { order = append(order, "pgid"); return 42, nil }, func(int) (int, error) { order = append(order, "sid"); return 42, nil }, func(pid int, sig syscall.Signal) error {
		order = append(order, "signal")
		if pid != -42 || sig != syscall.SIGKILL {
			t.Fatal("wrong signal")
		}
		return nil
	})
	if err != nil || strings.Join(order, ",") != "waitid,pgid,sid,signal" {
		t.Fatalf("err=%v order=%v", err, order)
	}
}
func TestOwnershipTraversalRejectsDuplicateOrCyclicChildren(t *testing.T) {
	inspected := false
	if walkOwned(1, 3, func(pid int) ([]int, error) {
		if pid == 1 {
			return []int{2, 2}, nil
		}
		return []int{3}, nil
	}, func(int) error { inspected = true; return nil }) == nil || inspected {
		t.Fatal("duplicate accepted")
	}
	if walkOwned(1, 3, func(pid int) ([]int, error) { return []int{pid}, nil }, func(int) error { inspected = true; return nil }) == nil || inspected {
		t.Fatal("cycle accepted")
	}
}

func TestSubreaperScopeRestoresPriorSettingInIsolatedProcess(t *testing.T) {
	if os.Getenv("DILLA_TEST_SUBREAPER") != "1" {
		command := exec.Command(os.Args[0], "-test.run=^TestSubreaperScopeRestoresPriorSettingInIsolatedProcess$")
		command.Env = append(os.Environ(), "DILLA_TEST_SUBREAPER=1")
		if out, e := command.CombinedOutput(); e != nil {
			t.Fatalf("isolated: %v %s", e, out)
		}
		return
	}
	before, e := readSubreaper()
	if e != nil {
		t.Fatal(e)
	}
	scope, e := beginProbeScope(time.Now().Add(time.Second))
	if e != nil {
		t.Fatal(e)
	}
	during, e := readSubreaper()
	if e != nil || during != 1 {
		t.Fatalf("during=%d err=%v", during, e)
	}
	if e = scope.finish(); e != nil {
		t.Fatal(e)
	}
	after, e := readSubreaper()
	if e != nil || before != after {
		t.Fatalf("before=%d after=%d err=%v", before, after, e)
	}
}

func TestProbeScopeRejectsPreexistingChildWithoutSignalingIt(t *testing.T) {
	if os.Getenv("DILLA_TEST_EXISTING_CHILD") != "1" {
		command := exec.Command(os.Args[0], "-test.run=^TestProbeScopeRejectsPreexistingChildWithoutSignalingIt$")
		command.Env = append(os.Environ(), "DILLA_TEST_EXISTING_CHILD=1")
		if out, e := command.CombinedOutput(); e != nil {
			t.Fatalf("isolated: %v %s", e, out)
		}
		return
	}
	child := exec.Command("/bin/sleep", "30")
	if e := child.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	before, e := readSubreaper()
	if e != nil {
		t.Fatal(e)
	}
	if scope, e := beginProbeScope(time.Now().Add(time.Second)); e == nil {
		_ = scope.finish()
		t.Fatal("scope accepted existing child")
	}
	if e := child.Process.Signal(syscall.Signal(0)); e != nil {
		t.Fatalf("child was signaled: %v", e)
	}
	after, e := readSubreaper()
	if e != nil || before != after {
		t.Fatalf("subreaper changed %d -> %d, %v", before, after, e)
	}
}

func TestOwnedSnapshotRequiresEveryChildListUnchanged(t *testing.T) {
	recorded := map[int]string{1: "a", 2: "b"}
	if e := verifyStableLinks(recorded, func(pid int) (string, error) { return recorded[pid], nil }); e != nil {
		t.Fatal(e)
	}
	for _, read := range []func(int) (string, error){func(pid int) (string, error) {
		if pid == 2 {
			return "changed", nil
		}
		return "a", nil
	}, func(pid int) (string, error) {
		if pid == 2 {
			return "", errors.New("vanished")
		}
		return "a", nil
	}} {
		if e := verifyStableLinks(recorded, read); e == nil {
			t.Fatal("accepted unstable or incomplete child list")
		}
	}
}

func TestOwnedTreeFindsOnlyKernelListedDescendants(t *testing.T) {
	if os.Getenv("DILLA_TEST_OWNED_TREE") != "1" {
		command := exec.Command(os.Args[0], "-test.run=^TestOwnedTreeFindsOnlyKernelListedDescendants$")
		command.Env = append(os.Environ(), "DILLA_TEST_OWNED_TREE=1")
		if out, e := command.CombinedOutput(); e != nil {
			t.Fatalf("isolated: %v %s", e, out)
		}
		return
	}
	child := exec.Command("/bin/sleep", "30")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if e := child.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	tree, e := openOwnedTree(child.Process.Pid, os.Getuid(), time.Now().Add(time.Second))
	if e != nil {
		t.Fatal(e)
	}
	defer tree.close()
	descendants, e := tree.children(child.Process.Pid)
	if e != nil {
		t.Fatal(e)
	}
	if len(descendants) != 0 {
		t.Fatalf("unexpected descendants: %v", descendants)
	}
	if e := inspectOwnedTree(child.Process.Pid, os.Getpid(), os.Getuid(), time.Now().Add(time.Second), map[int]knownProcess{}); e == nil {
		t.Fatal("accepted foreign renderer")
	}
}

func TestUnexpectedAdoptedChildForbidsGroupSignal(t *testing.T) {
	sent := false
	err := signalAfterKnownChildren(42, []int{42, 99}, map[int]knownProcess{}, func(int) error { return nil }, func() error { sent = true; return nil })
	if err == nil || sent {
		t.Fatalf("unknown child caused signal: err=%v sent=%v", err, sent)
	}
}

func TestProbeScopePreservesExistingSubreaper(t *testing.T) {
	if os.Getenv("DILLA_TEST_ALREADY_SUBREAPER") != "1" {
		command := exec.Command(os.Args[0], "-test.run=^TestProbeScopePreservesExistingSubreaper$")
		command.Env = append(os.Environ(), "DILLA_TEST_ALREADY_SUBREAPER=1")
		if out, e := command.CombinedOutput(); e != nil {
			t.Fatalf("isolated: %v %s", e, out)
		}
		return
	}
	if e := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); e != nil {
		t.Fatal(e)
	}
	defer unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 0, 0, 0, 0)
	scope, e := beginProbeScope(time.Now().Add(time.Second))
	if e != nil {
		t.Fatal(e)
	}
	if e = scope.finish(); e != nil {
		t.Fatal(e)
	}
	after, e := readSubreaper()
	if e != nil || after != 1 {
		t.Fatalf("after=%d err=%v", after, e)
	}
}

func TestKnownChildRejectsReusedIdentityWithoutSignal(t *testing.T) {
	if os.Getenv("DILLA_TEST_STALE_CHILD") != "1" {
		command := exec.Command(os.Args[0], "-test.run=^TestKnownChildRejectsReusedIdentityWithoutSignal$")
		command.Env = append(os.Environ(), "DILLA_TEST_STALE_CHILD=1")
		if out, e := command.CombinedOutput(); e != nil {
			t.Fatalf("isolated: %v %s", e, out)
		}
		return
	}
	child := exec.Command("/bin/sleep", "30")
	if e := child.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	fd, e := unix.PidfdOpen(child.Process.Pid, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer unix.Close(fd)
	known := map[int]knownProcess{child.Process.Pid: {start: "0", pidfd: fd}}
	if _, _, e = validateKnownChild(child.Process.Pid, known); e == nil {
		t.Fatal("accepted stale start identity")
	}
	if e := child.Process.Signal(syscall.Signal(0)); e != nil {
		t.Fatalf("child was signaled: %v", e)
	}
}

func TestSelfChildSnapshotRetriesWholeChurnedObservation(t *testing.T) {
	calls := 0
	got, e := stableSelfChildren(time.Now().Add(time.Second), func(time.Time) ([]int, error) {
		calls++
		switch calls {
		case 1:
			return nil, errors.New("thread vanished")
		case 2:
			return []int{11}, nil
		case 3:
			return []int{12}, nil
		default:
			return []int{12}, nil
		}
	})
	if e != nil || len(got) != 1 || got[0] != 12 || calls != 5 {
		t.Fatalf("got %v err=%v calls=%d", got, e, calls)
	}
}
func TestSelfChildSnapshotRefusesPerpetualChurn(t *testing.T) {
	calls := 0
	got, e := stableSelfChildren(time.Now().Add(time.Second), func(time.Time) ([]int, error) { calls++; return []int{calls}, nil })
	if e == nil || got != nil {
		t.Fatalf("accepted churn: %v %v", got, e)
	}
}

func TestSelfChildSnapshotNeverAcceptsLateStableResult(t *testing.T) {
	got, e := stableSelfChildren(time.Now().Add(10*time.Millisecond), func(time.Time) ([]int, error) { time.Sleep(10 * time.Millisecond); return nil, nil })
	if e == nil || got != nil {
		t.Fatalf("accepted late snapshot %v %v", got, e)
	}
}
