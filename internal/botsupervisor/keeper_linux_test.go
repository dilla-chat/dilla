//go:build linux

package botsupervisor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"golang.org/x/sys/unix"
)

// The test binary plays three parts besides the tests (TestMain):
//   - `<test> bot-launch …`: the launcher, exactly as the keeper runs it;
//   - no arguments with DILLA_BOT_SOCKET set: the Bot, which writes what it was given to report.json in its cwd;
//   - DILLA_TEST_KEEPER=<json KeeperConfig>: a keeper serving the control socket on fd 3.
func runTestChild() {
	switch {
	case len(os.Args) > 1 && os.Args[1] == "bot-launch":
		err := LaunchMain(os.Args[2:])
		fmt.Fprintln(os.Stderr, "bot-launch:", err)
		os.Exit(70)
	case len(os.Args) == 1 && os.Getenv("DILLA_BOT_SOCKET") != "":
		if err := writeBotReport(); err != nil {
			fmt.Fprintln(os.Stderr, "bot report:", err)
			os.Exit(71)
		}
		os.Exit(0)
	case os.Getenv("DILLA_TEST_KEEPER") != "":
		var cfg KeeperConfig
		if err := json.Unmarshal([]byte(os.Getenv("DILLA_TEST_KEEPER")), &cfg); err != nil {
			os.Exit(72)
		}
		// An unprivileged user namespace refuses setgroups; production always calls it.
		cfg.noSetgroups = true
		// In a user namespace the host's root owns nothing (and /tmp holds the test binary); and the race build is
		// cgo, which cannot narrow every thread. The static-build serve test checks both for real.
		cfg.insecurePaths = true
		cfg.allowCgoNarrowing = true
		if err := ServeKeeper(cfg, os.NewFile(3, "keeper-control"), quietLog()); err != nil {
			fmt.Fprintln(os.Stderr, "keeper:", err)
			os.Exit(73)
		}
		os.Exit(0)
	}
}

type botReport struct {
	Status map[string]string `json:"status"`
	Env    []string          `json:"env"`
	FDs    []int             `json:"fds"`
	Argv   []string          `json:"argv"`
	Cwd    string            `json:"cwd"`
	Token  string            `json:"token"`
}

// writeBotReport records the launched process's view of itself, before it opens anything else: its open descriptors
// (raw getdents, minus the one it reads them through and the Go runtime's own cgroup file), its /proc status lines, its environment, argv, cwd and the
// launch token on fd 3.
func writeBotReport() error {
	dir, err := syscall.Open("/proc/self/fd", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	var fds []int
	buf := make([]byte, 8192)
	for {
		n, err := syscall.ReadDirent(dir, buf)
		if err != nil {
			return err
		}
		if n == 0 {
			break
		}
		var names []string
		_, _, names = syscall.ParseDirent(buf[:n], -1, names)
		for _, name := range names {
			fd, err := strconv.Atoi(name)
			if err != nil || fd == dir {
				continue
			}
			// The Go runtime of the report process opens its cgroup's cpu.max itself, after the exec, to size
			// GOMAXPROCS: that one was not inherited, and the real Bot opens it too.
			if target, _ := os.Readlink("/proc/self/fd/" + name); strings.HasPrefix(target, "/sys/fs/cgroup/") && strings.HasSuffix(target, "/cpu.max") {
				continue
			}
			fds = append(fds, fd)
		}
	}
	_ = syscall.Close(dir)
	slices.Sort(fds)
	token := make([]byte, 33)
	n, _ := io.ReadFull(os.NewFile(3, "launch-token"), token)
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return err
	}
	r := botReport{Status: map[string]string{}, Env: os.Environ(), FDs: fds, Argv: os.Args, Token: hex.EncodeToString(token[:n])}
	r.Cwd, _ = os.Getwd()
	sc := bufio.NewScanner(bytes.NewReader(status))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if ok {
			r.Status[k] = strings.TrimSpace(v)
		}
	}
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return os.WriteFile("report.json", body, 0o600)
}

func readReport(t *testing.T, stateDir string) botReport {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(stateDir, "report.json"))
	if err != nil {
		t.Fatalf("the launched child left no report: %v", err)
	}
	var r botReport
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("report: %v", err)
	}
	return r
}

func testExecutable(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return exe
}

func testKeeperConfig(t *testing.T) KeeperConfig {
	t.Helper()
	exe := testExecutable(t)
	state := t.TempDir()
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatal(err)
	}
	return KeeperConfig{
		Launcher: exe, Executable: exe, Helper: exe, WASI: exe,
		StateDir: state, Socket: "/run/dillad/bot/gateway.sock",
	}
}

// startUserNSKeeper runs a keeper as root in a new user namespace (uid and gid 0 map to the test's own), which gives
// it the full capability sets a keeper under dilla.service starts with, without being root on the host. It skips when
// the kernel or its policy refuses unprivileged user namespaces.
func startUserNSKeeper(t *testing.T, cfg KeeperConfig) *PipeKeeper {
	t.Helper()
	k, _ := startUserNSKeeperProcess(t, cfg)
	return k
}

// startUserNSKeeperProcess is startUserNSKeeper that also returns the keeper's process.
func startUserNSKeeperProcess(t *testing.T, cfg KeeperConfig) (*PipeKeeper, *os.Process) {
	t.Helper()
	if reason := userNSCapabilities(); reason != "" {
		t.Skip(reason)
	}
	pair, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	ours, theirs := os.NewFile(uintptr(pair[0]), "keeper-client"), os.NewFile(uintptr(pair[1]), "keeper-server")
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), testExecutable(t))
	// The keeper's own environment is noise the launch must not pass on.
	cmd.Env = []string{"DILLA_TEST_KEEPER=" + string(body), "PATH=/usr/bin:/bin", "LEAKED_SECRET=hunter2", "LD_DEBUG=", "DILLA_BOT_EXTRA=1"}
	cmd.ExtraFiles = []*os.File{theirs}
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}
	if err := cmd.Start(); err != nil {
		_ = ours.Close()
		_ = theirs.Close()
		t.Skipf("no unprivileged user namespaces here (%v): the launch's capability drop needs a keeper that holds capabilities", err)
	}
	_ = theirs.Close()
	k := NewPipeKeeper(ours)
	t.Cleanup(func() {
		_ = k.Close()
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
	return k, cmd.Process
}

func newToken(t *testing.T) [32]byte {
	t.Helper()
	var tok [32]byte
	if _, err := rand.Read(tok[:]); err != nil {
		t.Fatal(err)
	}
	return tok
}

func launchViaKeeper(t *testing.T, cfg KeeperConfig) (botReport, [32]byte) {
	t.Helper()
	k := startUserNSKeeper(t, cfg)
	token := newToken(t)
	pid, err := k.Start(LaunchSpec{Token: token})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	exit := make(chan string, 1)
	go func() { exit <- k.Wait(pid) }()
	select {
	case got := <-exit:
		if got != "exit status 0" {
			t.Fatalf("the launched child ended %q, want exit status 0", got)
		}
	case <-time.After(30 * time.Second):
		_ = k.Signal(pid, syscall.SIGKILL)
		t.Fatal("the launched child did not finish")
	}
	return readReport(t, cfg.StateDir), token
}

func TestLaunchedChildHasNoCapabilities(t *testing.T) {
	cfg := testKeeperConfig(t)
	r, _ := launchViaKeeper(t, cfg)
	for _, set := range []string{"CapEff", "CapPrm", "CapAmb", "CapBnd", "CapInh"} {
		if got := r.Status[set]; got != "0000000000000000" {
			t.Errorf("%s = %q, want 0000000000000000", set, got)
		}
	}
	if got := r.Status["NoNewPrivs"]; got != "1" {
		t.Errorf("NoNewPrivs = %q, want 1", got)
	}
	// The keeper handles SIGTERM and SIGINT rather than ignoring them, so nothing it starts inherits an ignored
	// signal (N-5).
	if got := r.Status["SigIgn"]; got != "0000000000000000" {
		t.Errorf("SigIgn = %q, want 0000000000000000", got)
	}
	// The keeper (root in its namespace, so holding every capability) ran the Bot as uid 0 there: the drop, not the
	// uid, is what emptied the sets. The keeper's own CAP_KILL included: the Bot cannot signal anyone else's process.
	if !strings.HasPrefix(r.Status["Uid"], "0\t") {
		t.Errorf("Uid = %q, want the configured target 0", r.Status["Uid"])
	}
}

func wantAllowlist(t *testing.T, cfg KeeperConfig, r botReport, token [32]byte) {
	t.Helper()
	want := []string{
		"PATH=" + LaunchPATH,
		"HOME=" + cfg.StateDir,
		"DILLA_BOT_SOCKET=" + cfg.Socket,
		"DILLA_BOT_STATE_DIR=" + cfg.StateDir,
		"DILLA_BOT_HELPER=" + cfg.Helper,
		"DILLA_BOT_WASI=" + cfg.WASI,
	}
	got := slices.Clone(r.Env)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("environment = %q, want exactly %q", got, want)
	}
	if !slices.Equal(r.FDs, []int{0, 1, 2, 3}) {
		t.Errorf("open descriptors = %v, want [0 1 2 3]", r.FDs)
	}
	if got := r.Status["Umask"]; got != "0077" {
		t.Errorf("Umask = %q, want 0077", got)
	}
	if r.Cwd != cfg.StateDir {
		t.Errorf("cwd = %q, want %q", r.Cwd, cfg.StateDir)
	}
	if !slices.Equal(r.Argv, []string{cfg.Executable}) {
		t.Errorf("argv = %q, want only the executable", r.Argv)
	}
	if r.Token != hex.EncodeToString(token[:]) {
		t.Errorf("fd 3 carried %q, want the 32-byte launch token and nothing more", r.Token)
	}
	hexTok := hex.EncodeToString(token[:])
	for _, v := range append(slices.Clone(r.Env), r.Argv...) {
		if strings.Contains(v, hexTok) || strings.Contains(v, string(token[:])) {
			t.Errorf("the launch token leaked into %q", v)
		}
	}
}

func TestLaunchedChildSeesOnlyAllowlistedEnvAndFds(t *testing.T) {
	// The launcher on its own, as dev_same_user runs it: no namespace needed. It inherits a stray descriptor (fd 5) and
	// a dirty environment and a loose umask, and must pass none of them on.
	t.Run("launcher", func(t *testing.T) {
		cfg := testKeeperConfig(t)
		token := newToken(t)
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(token[:]); err != nil {
			t.Fatal(err)
		}
		_ = w.Close()
		exe, err := os.Open(cfg.Executable)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = exe.Close() }()
		stray, err := os.Open(os.DevNull)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = stray.Close() }()
		cmd := exec.CommandContext(t.Context(), cfg.Launcher, launchArgs(cfg, true)...)
		cmd.Env = []string{"LEAKED_SECRET=hunter2", "PATH=/nowhere", "HOME=/root", "DILLA_BOT_SOCKET=/tmp/evil"}
		cmd.ExtraFiles = []*os.File{r, exe, stray}
		cmd.Dir = "/"
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		old := syscall.Umask(0o002)
		err = cmd.Run()
		syscall.Umask(old)
		_ = r.Close()
		if err != nil {
			t.Fatalf("bot-launch: %v\n%s", err, stderr.String())
		}
		wantAllowlist(t, cfg, readReport(t, cfg.StateDir), token)
	})
	t.Run("through the keeper", func(t *testing.T) {
		cfg := testKeeperConfig(t)
		r, token := launchViaKeeper(t, cfg)
		wantAllowlist(t, cfg, r, token)
	})
}

// The control protocol is CBOR [op, id, …] with three ops. A malformed frame or an unknown op ends the keeper; signal
// and wait act only on the keeper's own children; start takes the token and nothing else.
func TestKeeperProtocolRefusesWhatItDoesNotKnow(t *testing.T) {
	cfg := testKeeperConfig(t)
	for _, tc := range []struct {
		name  string
		frame []byte
	}{
		{"malformed", []byte{0xff, 0x00, 0x01}},
		{"unknown op", mustCBOR(t, []any{"exec", uint64(1), "/bin/sh"})},
		{"start with a path", mustCBOR(t, []any{"start", uint64(1), make([]byte, 32), "/bin/sh"})},
		{"start with a short token", mustCBOR(t, []any{"start", uint64(1), make([]byte, 31)})},
		{"signal with a wild signal", mustCBOR(t, []any{"signal", uint64(1), uint64(1), uint64(19)})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := startUserNSKeeper(t, cfg)
			if _, err := k.conn.Write(tc.frame); err != nil {
				t.Fatalf("write: %v", err)
			}
			_ = k.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			buf := make([]byte, 64)
			if n, err := k.conn.Read(buf); !errors.Is(err, io.EOF) {
				t.Fatalf("the keeper answered %x (%v); it must close the pipe", buf[:n], err)
			}
		})
	}
	t.Run("signal and wait on a pid it did not start", func(t *testing.T) {
		k := startUserNSKeeper(t, cfg)
		if err := k.Signal(1, syscall.SIGTERM); err == nil {
			t.Fatal("the keeper signalled pid 1")
		}
		if got := k.Wait(os.Getpid()); got != "not a child of this keeper" {
			t.Fatalf("Wait on a stranger = %q", got)
		}
		// The pipe is still good afterwards: those are answered refusals, not protocol errors.
		token := newToken(t)
		pid, err := k.Start(LaunchSpec{Token: token})
		if err != nil {
			t.Fatalf("Start after refusals: %v", err)
		}
		if got := k.Wait(pid); got != "exit status 0" {
			t.Fatalf("Wait = %q", got)
		}
		// A reaped child is no longer the keeper's: its pid could be anyone's now.
		if err := k.Signal(pid, syscall.SIGKILL); err == nil {
			t.Fatal("the keeper signalled a pid it already reaped")
		}
	})
}

func mustCBOR(t *testing.T, v any) []byte {
	t.Helper()
	b, err := cbor.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The keeper refuses a file the serving user, or the Bot, could rewrite or re-point, at its own start and before every
// launch: a writable file, a writable directory anywhere on its path, a symlink into an untrusted place or one that
// lives in an untrusted directory, and (requireRoot) anything not root's.
func TestKeeperConfigRefusesAWritableExecutable(t *testing.T) {
	trusted := "/usr/bin/true"
	if _, err := resolveTrusted(trusted, true, false); err != nil {
		t.Skipf("%s is not a root-owned file on a root-owned path here: %v", trusted, err)
	}
	// t.TempDir is under /tmp, which is world-writable (sticky) and so fails the rule on its own: work in a directory
	// beside the package instead, whose ancestors are the checkout's.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(cwd, "trusted-test-") //nolint:usetesting // t.TempDir is under /tmp, which the rule refuses
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveTrusted(dir, false, false); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Skipf("the checkout's own path is not trusted here: %v", err)
	}
	exe := filepath.Join(dir, "dillabot")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(exe, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := checkTrustedFile(exe, false); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("a group-writable file = %v", err)
	}
	if err := os.Chmod(exe, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkTrustedFile(exe, false); err != nil {
		t.Fatalf("dev_same_user accepts the user's own build: %v", err)
	}
	if _, err := resolveTrusted(exe, true, false); err == nil {
		t.Fatal("a file the test user owns passed the root rule")
	}

	t.Run("a writable parent directory", func(t *testing.T) {
		parent := filepath.Join(dir, "writable")
		if err := os.Mkdir(parent, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(parent, 0o777); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(parent, "dillabot")
		if err := os.WriteFile(file, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := resolveTrusted(file, false, false)
		if err == nil || !strings.Contains(err.Error(), parent) || !strings.Contains(err.Error(), "writable") {
			t.Fatalf("a file in a world-writable directory = %v, want the directory refused", err)
		}
		// The rule covers every ancestor, the system's /tmp included.
		if info, _ := os.Stat(os.TempDir()); info != nil && info.Mode().Perm()&0o022 != 0 {
			if _, err := resolveTrusted(exe, true, false); err == nil || !strings.Contains(err.Error(), "writable") && !strings.Contains(err.Error(), "owned by root") {
				t.Fatalf("a file under %s = %v", os.TempDir(), err)
			}
		}
	})
	t.Run("a symlink to an untrusted target", func(t *testing.T) {
		target := filepath.Join(dir, "untrusted")
		if err := os.WriteFile(target, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(target, 0o777); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, err := resolveTrusted(link, false, true); err == nil || !strings.Contains(err.Error(), target) {
			t.Fatalf("a symlink to a world-writable file = %v, want the target refused", err)
		}
	})
	t.Run("a symlink in an untrusted directory to a trusted target", func(t *testing.T) {
		// Whoever can write the link's directory can re-point it at any root-owned program between launches.
		link := filepath.Join(dir, "to-true")
		if err := os.Symlink(trusted, link); err != nil {
			t.Fatal(err)
		}
		if _, err := resolveTrusted(link, true, false); err == nil {
			t.Fatal("a symlink the test user owns, in its own directory, passed the root rule")
		}
		got, err := resolveTrusted(link, false, true)
		if err != nil || got != trusted {
			t.Fatalf("dev_same_user resolves %s to %q, %v", link, got, err)
		}
	})
	t.Run("a trusted chain passes", func(t *testing.T) {
		got, err := resolveTrusted(trusted, true, false)
		if err != nil || got != trusted {
			t.Fatalf("resolveTrusted(%s) = %q, %v", trusted, got, err)
		}
	})
}

// The keeper ignores SIGTERM and SIGINT: systemd's stop must not end it before serve has stopped the Bot through it.
func TestKeeperSurvivesSIGTERM(t *testing.T) {
	cfg := testKeeperConfig(t)
	k, proc := startUserNSKeeperProcess(t, cfg)
	// Make sure the keeper is serving (it installs its handlers before it reads the socket).
	if got := k.Wait(1); got != notChildExit {
		t.Fatalf("Wait(1) = %q", got)
	}
	for _, sig := range []os.Signal{syscall.SIGTERM, syscall.SIGINT} {
		if err := proc.Signal(sig); err != nil {
			t.Fatalf("signal the keeper: %v", err)
		}
	}
	time.Sleep(200 * time.Millisecond)
	pid, err := k.Start(LaunchSpec{Token: newToken(t)})
	if err != nil {
		t.Fatalf("Start after SIGTERM and SIGINT: %v", err)
	}
	if got := k.Wait(pid); got != "exit status 0" {
		t.Fatalf("Wait = %q", got)
	}
}

// One Bot per keeper, and one wait per child; a wait forgets only its own child.
func TestKeeperRunsOneBotAndOneWait(t *testing.T) {
	cfg := testKeeperConfig(t)
	cfg.SameUser = true
	c := newChildren(cfg)
	pid, err := c.start(newToken(t))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := c.start(newToken(t)); !errors.Is(err, errBotRunning) {
		t.Fatalf("a second start while the first is not waited for = %v, want errBotRunning", err)
	}
	first := make(chan string, 1)
	go func() { first <- c.wait(pid) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		waiting := c.m[pid] != nil && c.m[pid].waiting
		c.mu.Unlock()
		if waiting || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	// The first wait may already have returned and forgotten the child; either answer is a refusal to block twice.
	if got := c.wait(pid); got != alreadyWaitedExit && got != notChildExit {
		t.Fatalf("a second wait = %q", got)
	}
	if got := <-first; got != "exit status 0" {
		t.Fatalf("first wait = %q", got)
	}
	again, err := c.start(newToken(t))
	if err != nil {
		t.Fatalf("start after the wait: %v", err)
	}
	if got := c.wait(again); got != "exit status 0" {
		t.Fatalf("wait = %q", got)
	}
	// A wait forgets only its own child (M-1): if the pid came to name another entry while it waited, that entry stays.
	staleCmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "exit 9")
	if err := staleCmd.Start(); err != nil {
		t.Fatal(err)
	}
	stale := &child{cmd: staleCmd, exited: make(chan struct{})}
	other := &child{exited: make(chan struct{})}
	c.mu.Lock()
	c.m[42] = stale
	c.mu.Unlock()
	done := make(chan string, 1)
	go func() { done <- c.wait(42) }()
	for {
		c.mu.Lock()
		waiting := stale.waiting
		c.mu.Unlock()
		if waiting {
			break
		}
		time.Sleep(time.Millisecond)
	}
	c.mu.Lock()
	c.m[42] = other
	c.mu.Unlock()
	close(stale.exited)
	if got := <-done; got != "exit status 9" {
		t.Fatalf("the stale wait = %q", got)
	}
	c.mu.Lock()
	kept := c.m[42] == other
	delete(c.m, 42)
	c.mu.Unlock()
	if !kept {
		t.Fatal("a wait removed another child's entry under the same pid")
	}
}

// Every start re-checks the Bot's files: a file made writable after the keeper started is refused at the next launch.
func TestKeeperRechecksFilesEveryStart(t *testing.T) {
	cfg := testKeeperConfig(t)
	cfg.SameUser = true
	dir := t.TempDir()
	helper := filepath.Join(dir, "dilla-bot-core")
	if err := os.WriteFile(helper, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg.Helper = helper
	c := newChildren(cfg)
	pid, err := c.start(newToken(t))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	_ = c.wait(pid)
	if err := os.Chmod(helper, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := c.start(newToken(t)); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("start after the helper became writable = %v", err)
	}
}

// The serving process fails closed: holding CAP_SETUID it cannot drop (a cgo build) is an error in every mode, and
// only dev_same_user as root may go on.
func TestDropServingCapabilitiesFailsClosed(t *testing.T) {
	oldRead, oldAll := readCaps, allThreadsSyscall
	t.Cleanup(func() { readCaps, allThreadsSyscall = oldRead, oldAll })
	held := capSets{effective: keeperCapabilities, permitted: keeperCapabilities}
	readCaps = func() (capSets, uint64, uint64, error) { return held, 0, keeperCapabilities, nil }
	allThreadsSyscall = func(uintptr, uintptr, uintptr, uintptr) (uintptr, uintptr, syscall.Errno) {
		return 0, 0, syscall.ENOTSUP
	}
	for _, rootDevTest := range []bool{false, true} {
		err := DropServingCapabilities(quietLog(), rootDevTest)
		if os.Geteuid() != 0 || !rootDevTest {
			if !errors.Is(err, errCgoCapabilities) {
				t.Errorf("rootDevTest=%v: %v, want errCgoCapabilities", rootDevTest, err)
			}
		}
	}
	// Held without CAP_SETPCAP: the bounding set cannot shrink, which is refused too.
	readCaps = func() (capSets, uint64, uint64, error) {
		return capSets{permitted: 1 << capSetuid}, 0, keeperCapabilities, nil
	}
	if err := DropServingCapabilities(quietLog(), false); err == nil || !strings.Contains(err.Error(), "CAP_SETPCAP") {
		t.Fatalf("held without CAP_SETPCAP = %v", err)
	}
	// Holding none of them (an ordinary process, whose bounding set is full) is nothing to drop.
	readCaps = func() (capSets, uint64, uint64, error) { return capSets{}, 0, ^uint64(0), nil }
	if err := DropServingCapabilities(quietLog(), false); err != nil {
		t.Fatalf("nothing held = %v", err)
	}
	// The keeper's own narrowing is fatal when it fails, and when it lacks CAP_SETPCAP.
	readCaps = func() (capSets, uint64, uint64, error) { return held, 0, keeperCapabilities, nil }
	if err := narrowKeeperCapabilities(false); !errors.Is(err, errCgoCapabilities) {
		t.Fatalf("narrowing in a cgo build = %v", err)
	}
	readCaps = func() (capSets, uint64, uint64, error) {
		return capSets{effective: 1 << capSetuid, permitted: 1 << capSetuid}, 0, 0, nil
	}
	if err := narrowKeeperCapabilities(true); err == nil {
		t.Fatal("a keeper without CAP_SETPCAP narrowed")
	}
}

// CAP_KILL (v0.1.0-bot2.1): the keeper keeps it, because it signals a Bot that runs as another user; the serving
// process drops it with the keeper's other capabilities; a keeper without it says so, unless the Bot is its own user.
func TestKeeperKeepsCapKillAndServeDropsIt(t *testing.T) {
	if keeperCapabilities&(1<<capKill) == 0 {
		t.Fatalf("keeperCapabilities = %016x lacks CAP_KILL", keeperCapabilities)
	}
	var dropped uint64
	for _, c := range servingDrops {
		dropped |= 1 << c
	}
	if dropped != keeperCapabilities {
		t.Fatalf("the serving process drops %016x, want exactly the keeper's %016x", dropped, keeperCapabilities)
	}

	oldRead, oldAll := readCaps, allThreadsSyscall
	t.Cleanup(func() { readCaps, allThreadsSyscall = oldRead, oldAll })

	// A serving process that holds CAP_KILL (and CAP_SETPCAP) but no CAP_SETUID still drops it, from the bounding
	// and ambient sets as well.
	held := capSets{effective: 1<<capKill | 1<<capSetpcap, permitted: 1<<capKill | 1<<capSetpcap}
	readCaps = func() (capSets, uint64, uint64, error) { return held, 1 << capKill, keeperCapabilities, nil }
	var bounding, ambient []uintptr
	allThreadsSyscall = func(trap, a1, a2, a3 uintptr) (uintptr, uintptr, syscall.Errno) {
		if trap == unix.SYS_PRCTL && a1 == unix.PR_CAPBSET_DROP {
			bounding = append(bounding, a2)
		}
		if trap == unix.SYS_PRCTL && a1 == unix.PR_CAP_AMBIENT && a2 == unix.PR_CAP_AMBIENT_LOWER {
			ambient = append(ambient, a3)
		}
		return 0, 0, 0
	}
	if err := DropServingCapabilities(quietLog(), false); err != nil {
		t.Fatalf("DropServingCapabilities: %v", err)
	}
	if !slices.Contains(bounding, capKill) || !slices.Contains(ambient, capKill) {
		t.Fatalf("CAP_KILL was not dropped: bounding %v, ambient %v", bounding, ambient)
	}
	// Held without CAP_SETPCAP, CAP_KILL is refused like CAP_SETUID.
	readCaps = func() (capSets, uint64, uint64, error) {
		return capSets{permitted: 1 << capKill}, 0, keeperCapabilities, nil
	}
	if err := DropServingCapabilities(quietLog(), false); err == nil || !strings.Contains(err.Error(), "CAP_KILL") {
		t.Fatalf("CAP_KILL held without CAP_SETPCAP = %v", err)
	}

	// The keeper's start-up check: missing CAP_KILL is named, unless the Bot runs as dillad's own user.
	readCaps = func() (capSets, uint64, uint64, error) {
		s := keeperCapabilities &^ (1 << capKill)
		return capSets{effective: s, permitted: s}, 0, s, nil
	}
	if err := keeperKillCheck(false); !errors.Is(err, errKeeperLacksKill) {
		t.Fatalf("a keeper without CAP_KILL = %v, want errKeeperLacksKill", err)
	}
	if !strings.Contains(errKeeperLacksKill.Error(), "deploy/dilla.service") {
		t.Fatalf("the warning does not say where CAP_KILL comes from: %v", errKeeperLacksKill)
	}
	if err := keeperKillCheck(true); err != nil {
		t.Fatalf("dev_same_user needs no CAP_KILL, got %v", err)
	}
	readCaps = func() (capSets, uint64, uint64, error) {
		return capSets{effective: keeperCapabilities, permitted: keeperCapabilities}, 0, keeperCapabilities, nil
	}
	if err := keeperKillCheck(false); err != nil {
		t.Fatalf("a keeper with CAP_KILL = %v", err)
	}
}

// The keeper reaps a child only on a wait op, so the pidfd the supervisor opens after start binds the child it started,
// even one that has already exited: until the wait, the pid is that zombie's and no one else's (N-2).
func TestPidfdBindsAChildThatExitedAtOnce(t *testing.T) {
	cfg := testKeeperConfig(t)
	cfg.SameUser = true
	cfg.Launcher = "/bin/false" // exits at once, whatever bot-launch's arguments
	c := newChildren(cfg)
	pid, err := c.start(newToken(t))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	c.mu.Lock()
	ch := c.m[pid]
	c.mu.Unlock()
	select {
	case <-ch.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the child never exited")
	}
	// Exited, not reaped: still our zombie.
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatalf("the exited child was reaped before any wait: %v", err)
	}
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	if fields[0] != "Z" || fields[1] != strconv.Itoa(os.Getpid()) {
		t.Fatalf("/proc/%d/stat = %s, want a zombie child of this process", pid, stat)
	}
	h, err := openPidfd(pid)
	if err != nil {
		t.Fatalf("pidfd_open on the unreaped child: %v", err)
	}
	defer h.close()
	if h.alive() {
		t.Fatal("the pidfd of an exited child reads alive")
	}
	if got := c.wait(pid); got != "exit status 1" {
		t.Fatalf("wait = %q", got)
	}
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err == nil {
		if b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); strings.Contains(string(b), "(false) Z") {
			t.Fatal("wait did not reap the child")
		}
	}
	if h.alive() {
		t.Fatal("the pidfd reads alive after the reap")
	}
}

var (
	userNSProbe     sync.Once
	userNSProbeSkip string
)

// userNSCapabilities reports why a fresh user namespace here holds no capabilities, or "" when it does. Ubuntu
// 24.04's AppArmor (kernel.apparmor_restrict_unprivileged_userns=1) lets the namespace be created but strips its
// capabilities, so a keeper started in one cannot narrow its own; such a host cannot run the keeper's tests.
func userNSCapabilities() string {
	userNSProbe.Do(func() {
		cmd := exec.CommandContext(context.Background(), "grep", "^CapEff:", "/proc/self/status")
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Cloneflags:  syscall.CLONE_NEWUSER,
			UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
			GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		}
		out, err := cmd.Output()
		if err != nil {
			userNSProbeSkip = fmt.Sprintf("no unprivileged user namespaces here (%v): the launch's capability drop needs a keeper that holds capabilities", err)
			return
		}
		fields := strings.Fields(string(out))
		if len(fields) != 2 || strings.Trim(fields[1], "0") == "" {
			userNSProbeSkip = fmt.Sprintf("a user namespace here holds no capabilities (%s, AppArmor's unprivileged_userns restriction?): the keeper cannot narrow its own", strings.TrimSpace(string(out)))
		}
	})
	return userNSProbeSkip
}
