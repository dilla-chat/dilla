//go:build linux

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/config"
)

const (
	capKill    = 5
	capSetgid  = 6
	capSetuid  = 7
	capSetpcap = 8
)

// buildStaticDillad builds this command with cgo off, as release.yml does: the serving process drops its capabilities
// on every thread with syscall.AllThreadsSyscall, which a cgo build (the race detector is one) cannot use.
func buildStaticDillad(t *testing.T) string {
	t.Helper()
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain on PATH to build a release-shaped dillad")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "dillad")
	// Built with the dilla_unsafe_test tag for this test only (internal/botsupervisor/unsafe_on.go): serve runs as root
	// in a user namespace (the keeper otherwise refuses euid 0), and in there the host's root owns no file (the Bot-file
	// ownership rules otherwise refuse every path).
	cmd := exec.CommandContext(t.Context(), gobin, "build", "-tags", "dilla_unsafe_test", "-o", bin, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	core, err := os.ReadFile(filepath.Join("..", "..", "internal", "mlswasi", "testdata", "dilla_core_wasi.wasm"))
	if err != nil {
		t.Fatalf("read the wasi core: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dilla_core_wasi.wasm"), core, 0o600); err != nil {
		t.Fatal(err)
	}
	return bin
}

// capSets reads the four sets of one /proc status file.
func capSets(t *testing.T, path string) map[string]uint64 {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sets := map[string]uint64{}
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok || !strings.HasPrefix(k, "Cap") {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(v), 16, 64)
		if err != nil {
			t.Fatalf("%s %s: %v", path, k, err)
		}
		sets[k] = n
	}
	return sets
}

// The serving process is started in a user namespace as its root, so it begins with every capability the way dillad
// begins with the unit's ambient set. Once it serves, no thread of it holds CAP_SETUID, CAP_SETGID, CAP_SETPCAP or
// CAP_KILL in any set, while its keeper child still holds all four (CAP_KILL to signal a Bot of another user).
func TestServingDilladHoldsNoSetuidCapability(t *testing.T) {
	bin := buildStaticDillad(t)
	cfgPath := bootstrapServeConfig(t)
	exe := filepath.Join(t.TempDir(), "dillabot")
	body, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, body, 0o755); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	rewriteConfig(t, cfgPath, func(c *config.Config) {
		c.Bot = config.Bot{
			Enabled: true, Executable: exe, HelperExecutable: exe, StateDir: state,
			User: "nobody", SocketDir: filepath.Join(t.TempDir(), "bot"),
		}
	})

	// Inside the namespace only one gid exists; make it the Bot user's primary group, so serve (root there, with
	// CAP_CHOWN) can give the socket directory to that group as it does in production.
	nobody, err := user.Lookup("nobody")
	if err != nil {
		t.Skipf("no nobody user: %v", err)
	}
	botGID, err := strconv.Atoi(nobody.Gid)
	if err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	stdoutR, stdoutW := io.Pipe()
	cmd := exec.CommandContext(t.Context(), bin, "serve", "--config="+cfgPath)
	cmd.Stdout = stdoutW
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: botGID, HostID: os.Getgid(), Size: 1}},
	}
	if err := cmd.Start(); err != nil {
		t.Skipf("no unprivileged user namespaces here (%v)", err)
	}
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		_ = stdoutW.Close()
	}()
	addr := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdoutR)
		for sc.Scan() {
			stdout.WriteString(sc.Text() + "\n")
			select {
			case addr <- strings.TrimSpace(sc.Text()):
			default:
			}
		}
	}()
	var listen string
	select {
	case listen = <-addr:
	case <-time.After(60 * time.Second):
		t.Fatal("serve never printed its listen address")
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := probe(listen)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve never answered /healthz: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	pid := cmd.Process.Pid
	tasks, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil || len(tasks) < 2 {
		t.Fatalf("threads of the serving process: %v (%d)", err, len(tasks))
	}
	for _, task := range tasks {
		sets := capSets(t, fmt.Sprintf("/proc/%d/task/%s/status", pid, task.Name()))
		for _, set := range []string{"CapEff", "CapPrm", "CapAmb", "CapBnd", "CapInh"} {
			for _, bit := range []int{capSetuid, capSetgid, capSetpcap, capKill} {
				if sets[set]&(1<<bit) != 0 {
					t.Errorf("thread %s holds capability %d in %s (%016x)", task.Name(), bit, set, sets[set])
				}
			}
		}
		// The drop is selective: the rest of what it was started with stays (in production, CAP_NET_BIND_SERVICE).
		if sets["CapEff"]&(1<<10) == 0 {
			t.Errorf("thread %s lost CAP_NET_BIND_SERVICE too: %016x", task.Name(), sets["CapEff"])
		}
	}

	// Every thread's children: Go forks from whichever thread runs the goroutine, and a task's children file lists only
	// what that thread forked.
	var children []byte
	for _, task := range tasks {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%s/children", pid, task.Name()))
		if err == nil {
			children = append(children, b...)
			children = append(children, ' ')
		}
	}
	keeper := 0
	for _, f := range strings.Fields(string(children)) {
		cmdline, _ := os.ReadFile("/proc/" + f + "/cmdline")
		if bytes.Contains(cmdline, []byte("bot-keeper")) {
			keeper, _ = strconv.Atoi(f)
		}
	}
	if keeper == 0 {
		t.Fatalf("serve started no bot-keeper (children %q)", children)
	}
	// The keeper holds exactly CAP_SETUID, CAP_SETGID, CAP_SETPCAP and CAP_KILL, bounding set included. Root in a namespace
	// starts with empty ambient and inheritable sets (dilla.service's ambient set is where production's come from),
	// so those two only must not hold anything else.
	const keeperMask = 1<<capSetuid | 1<<capSetgid | 1<<capSetpcap | 1<<capKill
	sets := capSets(t, fmt.Sprintf("/proc/%d/status", keeper))
	for _, set := range []string{"CapEff", "CapPrm", "CapBnd"} {
		if sets[set] != keeperMask {
			t.Errorf("the keeper's %s = %016x, want exactly %016x", set, sets[set], uint64(keeperMask))
		}
	}
	for _, set := range []string{"CapInh", "CapAmb"} {
		if sets[set]&^keeperMask != 0 {
			t.Errorf("the keeper's %s = %016x holds more than %016x", set, sets[set], uint64(keeperMask))
		}
	}
}
