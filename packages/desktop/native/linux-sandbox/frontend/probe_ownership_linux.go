//go:build linux && cgo

package frontend

import (
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Signal only while this process still owns an unreaped leader whose group ID
// and session IDs are the leader PID. No caller may reap between this check and the signal.
func signalOwnedGroup(pid int, owned func(int) (bool, error), group, session func(int) (int, error), send func(int, syscall.Signal) error) error {
	if pid < 2 {
		return errProbeCleanup
	}
	ok, e := owned(pid)
	if e != nil || !ok {
		return errProbeCleanup
	}
	g, e := group(pid)
	if e != nil || g != pid {
		return errProbeCleanup
	}
	s, e := session(pid)
	if e != nil || s != pid {
		return errProbeCleanup
	}
	return send(-pid, syscall.SIGKILL)
}

// Discover only from previously owned nodes. A complete bounded traversal must
// succeed before the target is inspected; reported PIDs are never used as paths.
func walkOwned(root, target int, children func(int) ([]int, error), inspect func(int) error) error {
	if root < 1 || target < 1 || root == target {
		return errProbeReport
	}
	queue := []int{root}
	seen := map[int]bool{root: true}
	found := false
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		kids, e := children(p)
		if e != nil {
			return errProbeReport
		}
		for _, child := range kids {
			if child < 2 || seen[child] || len(seen) >= 4096 {
				return errProbeReport
			}
			seen[child] = true
			queue = append(queue, child)
			if child == target {
				found = true
			}
		}
	}
	if !found {
		return errProbeReport
	}
	return inspect(target)
}

var probeScopeLock sync.Mutex

type probeScope struct {
	previous int
	active   bool
}

func readSubreaper() (int, error) {
	var value int32
	e := unix.Prctl(unix.PR_GET_CHILD_SUBREAPER, uintptr(unsafe.Pointer(&value)), 0, 0, 0)
	runtime.KeepAlive(&value)
	return int(value), e
}
func selfChildrenOnce(deadline time.Time) ([]int, error) {
	fd, e := unix.Open("/proc/self", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if e != nil {
		return nil, e
	}
	defer unix.Close(fd)
	tasks, e := listAt(fd, "task", 512)
	if e != nil || len(tasks) == 0 {
		return nil, errProbeCleanup
	}
	seen := map[int]bool{}
	var result []int
	for _, tid := range tasks {
		if !time.Now().Before(deadline) {
			return nil, errProbeCleanup
		}
		n, e := strconv.Atoi(tid)
		if e != nil || n < 1 || strconv.Itoa(n) != tid {
			return nil, errProbeCleanup
		}
		_, raw, e := readTaskChildren(fd, tid)
		if e != nil {
			return nil, errProbeCleanup
		}
		for _, word := range strings.Fields(raw) {
			pid, e := strconv.Atoi(word)
			if e != nil || pid < 2 || strconv.Itoa(pid) != word || seen[pid] || len(seen) >= 512 {
				return nil, errProbeCleanup
			}
			seen[pid] = true
			result = append(result, pid)
		}
	}
	again, e := listAt(fd, "task", 512)
	if e != nil || strings.Join(tasks, ",") != strings.Join(again, ",") {
		return nil, errProbeCleanup
	}
	return result, nil
}
func stableSelfChildren(deadline time.Time, scan func(time.Time) ([]int, error)) ([]int, error) {
	for attempt := 0; attempt < 3 && time.Now().Before(deadline); attempt++ {
		first, e := scan(deadline)
		if e == nil {
			second, e := scan(deadline)
			if e == nil {
				a := append([]int(nil), first...)
				b := append([]int(nil), second...)
				sort.Ints(a)
				sort.Ints(b)
				if len(a) == len(b) {
					same := true
					for i := range a {
						if a[i] != b[i] {
							same = false
							break
						}
					}
					if same && time.Now().Before(deadline) {
						return b, nil
					}
				}
			}
		}
		if time.Until(deadline) > time.Millisecond {
			time.Sleep(time.Millisecond)
		}
	}
	return nil, errProbeCleanup
}
func selfChildren(deadline time.Time) ([]int, error) {
	return stableSelfChildren(deadline, selfChildrenOnce)
}
func beginProbeScope(deadline time.Time) (*probeScope, error) {
	probeScopeLock.Lock()
	success := false
	defer func() {
		if !success {
			probeScopeLock.Unlock()
		}
	}()
	if !time.Now().Before(deadline) {
		return nil, errProbeCleanup
	}
	children, e := selfChildren(deadline)
	if e != nil || len(children) != 0 {
		return nil, errProbeCleanup
	}
	old, e := readSubreaper()
	if e != nil || old < 0 || old > 1 {
		return nil, errProbeCleanup
	}
	if e = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); e != nil {
		return nil, e
	}
	children, e = selfChildren(deadline)
	if e != nil || len(children) != 0 {
		if unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, uintptr(old), 0, 0, 0) != nil {
			return nil, errProbeCleanup
		}
		return nil, errProbeCleanup
	}
	success = true
	return &probeScope{old, true}, nil
}
func (s *probeScope) finish() error {
	if s == nil || !s.active {
		return errProbeCleanup
	}
	defer probeScopeLock.Unlock()
	s.active = false
	return unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, uintptr(s.previous), 0, 0, 0)
}

func leaderWaitState(pid int) (bool, error) {
	var info unix.Siginfo
	if e := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil); e != nil {
		return false, e
	}
	if info.Signo != 0 && info.Signo != int32(syscall.SIGCHLD) {
		return false, errProbeCleanup
	}
	return info.Signo != 0, nil
}
func groupSignal(pid int) error {
	return signalOwnedGroup(pid, func(id int) (bool, error) { _, e := leaderWaitState(id); return e == nil, e }, unix.Getpgid, unix.Getsid, func(group int, signal syscall.Signal) error { return unix.Kill(group, unix.Signal(signal)) })
}

type knownProcess struct {
	start string
	pidfd int
}

func signalAfterKnownChildren(leader int, children []int, known map[int]knownProcess, validate func(int) error, signal func() error) error {
	found := false
	for _, child := range children {
		if child == leader {
			found = true
			continue
		}
		if _, ok := known[child]; !ok {
			return errProbeCleanup
		}
		if validate(child) != nil {
			return errProbeCleanup
		}
	}
	if !found {
		return errProbeCleanup
	}
	return signal()
}
func validateKnownChild(child int, known map[int]knownProcess) (int, bool, error) {
	expected, ok := known[child]
	if !ok || expected.pidfd < 0 || unix.PidfdSendSignal(expected.pidfd, 0, nil, 0) != nil {
		return 0, false, errProbeCleanup
	}
	exited, e := leaderWaitState(child)
	if e != nil {
		return 0, false, errProbeCleanup
	}
	procFD, e := unix.Open("/proc/"+strconv.Itoa(child), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if e != nil {
		return 0, false, errProbeCleanup
	}
	raw, e := readAt(procFD, "stat", 8192)
	_ = unix.Close(procFD)
	if e != nil {
		return 0, false, errProbeCleanup
	}
	_, parent, group, start, e := parseOwnedStat(raw)
	if e != nil || parent != os.Getpid() || start != expected.start {
		return 0, false, errProbeCleanup
	}
	return group, exited, nil
}

// The scope is exclusive: before spawn it proved no children existed, and the
// launcher starts no unrelated subprocess while it is active. Its children
// and subreaper-adopted orphans can therefore only come from this probe.
func cleanupProbeChildren(cmd *exec.Cmd, deadline time.Time, known map[int]knownProcess) (error, error) {
	pid := cmd.Process.Pid
	exited, e := leaderWaitState(pid)
	if e != nil {
		return nil, errProbeCleanup
	}
	if !exited {
		if e = collectCleanupTree(pid, os.Getuid(), deadline, known); e != nil {
			return nil, errProbeCleanup
		}
	}
	children, e := selfChildren(deadline)
	if e != nil {
		return nil, errProbeCleanup
	}
	if e = signalAfterKnownChildren(pid, children, known, func(child int) error { _, _, e := validateKnownChild(child, known); return e }, func() error { return groupSignal(pid) }); e != nil {
		return nil, errProbeCleanup
	}
	escaped := false
	for time.Now().Before(deadline) {
		children, e := selfChildren(deadline)
		if e != nil {
			return nil, errProbeCleanup
		}
		hasLeader := false
		others := false
		for _, child := range children {
			if child == pid {
				hasLeader = true
				continue
			}
			others = true
			group, exited, e := validateKnownChild(child, known)
			if e != nil {
				return nil, errProbeCleanup
			}
			if group != pid {
				escaped = true
				if !exited {
					e = unix.PidfdSendSignal(known[child].pidfd, unix.SIGKILL, nil, 0)
					if e != nil {
						return nil, errProbeCleanup
					}
				}
			}
			if exited {
				var status unix.WaitStatus
				n, e := unix.Wait4(child, &status, unix.WNOHANG, nil)
				if e != nil || n != child {
					return nil, errProbeCleanup
				}
			}
		}
		if !hasLeader {
			return nil, errProbeCleanup
		}
		exited, e := leaderWaitState(pid)
		if e != nil {
			return nil, errProbeCleanup
		}
		if exited && !others {
			waitErr := cmd.Wait()
			remaining, e := selfChildren(deadline)
			if e != nil || len(remaining) != 0 {
				return waitErr, errProbeCleanup
			}
			if escaped {
				return waitErr, errProbeCleanup
			}
			return waitErr, nil
		}
		time.Sleep(time.Millisecond)
	}
	return nil, errProbeCleanup
}
