//go:build linux

package botsupervisor

import (
	"bufio"
	"bytes"
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
	"syscall"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
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
		Launcher: exe, Executable: exe, Helper: "/usr/local/lib/dilla/dilla-bot-core", WASI: "/usr/local/lib/dilla/core.wasm",
		StateDir: state, Socket: "/run/dillad/bot/gateway.sock",
	}
}

// startUserNSKeeper runs a keeper as root in a new user namespace (uid and gid 0 map to the test's own), which gives
// it the full capability sets a keeper under dilla.service starts with, without being root on the host. It skips when
// the kernel or its policy refuses unprivileged user namespaces.
func startUserNSKeeper(t *testing.T, cfg KeeperConfig) *PipeKeeper {
	t.Helper()
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
	return k
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
	// The keeper (root in its namespace, so holding every capability) ran the Bot as uid 0 there: the drop, not the
	// uid, is what emptied the sets.
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
	// The launcher on its own, as dev_same_user runs it: no namespace needed. It inherits a stray descriptor (fd 4) and
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
		stray, err := os.Open(os.DevNull)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = stray.Close() }()
		cmd := exec.CommandContext(t.Context(), cfg.Launcher, launchArgs(cfg, true)...)
		cmd.Env = []string{"LEAKED_SECRET=hunter2", "PATH=/nowhere", "HOME=/root", "DILLA_BOT_SOCKET=/tmp/evil"}
		cmd.ExtraFiles = []*os.File{r, stray}
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

// The keeper refuses, at its own start, an executable the serving user could rewrite: swapping it would run the
// attacker's code as the Bot user.
func TestKeeperConfigRefusesAWritableExecutable(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "dillabot")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(exe, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := checkTrustedFile(exe, true); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("checkTrustedFile on a group-writable file = %v", err)
	}
	if err := os.Chmod(exe, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkTrustedFile(exe, true); err == nil || !strings.Contains(err.Error(), "owned by root") {
		t.Fatalf("checkTrustedFile on a file the test user owns = %v, want it refused as not root's", err)
	}
	if err := checkTrustedFile(exe, false); err != nil {
		t.Fatalf("dev_same_user accepts the user's own build: %v", err)
	}
}
