//go:build linux && cgo

package frontend

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type probeProcessConfig struct {
	executable string
	args, env  []string
	deadline   time.Time
	nonce      string
	inspect    func(int, int, int) error
	known      map[int]knownProcess
}
type probeOutput struct {
	mu sync.Mutex
	bytes.Buffer
	overflow bool
}

func (w *probeOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	if w.Len()+n > 65536 {
		w.overflow = true
	}
	if avail := 65536 - w.Len(); avail > 0 {
		if len(p) > avail {
			p = p[:avail]
		}
		_, _ = w.Buffer.Write(p)
	}
	return n, nil
}
func (w *probeOutput) snapshot() (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.String(), w.overflow
}

var errProbeCleanup = errors.New("sandbox probe processes could not be reaped")

func runProbeProcess(c probeProcessConfig) (result ProbeResult, retErr error) {
	if c.executable == "" || c.inspect == nil || c.deadline.IsZero() {
		return ProbeFailed, errLaunch
	}
	if !time.Now().Before(c.deadline) {
		return ProbeTimedOut, nil
	}
	if c.known == nil {
		c.known = map[int]knownProcess{}
	}
	defer func() {
		for _, record := range c.known {
			_ = unix.Close(record.pidfd)
		}
	}()
	scope, e := beginProbeScope(c.deadline)
	if e != nil {
		return ProbeFailed, e
	}
	defer func() {
		if e := scope.finish(); e != nil {
			result, retErr = ProbeFailed, errProbeCleanup
		}
		if !time.Now().Before(c.deadline) {
			result = ProbeTimedOut
		}
	}()
	reportR, reportW, e := os.Pipe()
	if e != nil {
		return ProbeFailed, e
	}
	// These pipe closes are cleanup only; the sole acknowledgement write is checked below.
	defer func() { _ = reportR.Close() }()
	defer func() { _ = reportW.Close() }()
	ackR, ackW, e := os.Pipe()
	if e != nil {
		return ProbeFailed, e
	}
	defer func() { _ = ackR.Close() }()
	defer func() { _ = ackW.Close() }()
	// The probe owns cancellation and reaping until identity-checked cleanup; avoid independent context-triggered termination.
	// #nosec G204 -- executable is the fixed Electron sibling from the verified owned mount.
	cmd := exec.Command(c.executable, c.args...) //nolint:noctx // teardown owns deadline and reaping
	cmd.Env = c.env
	cmd.Stdin = nil
	cmd.Stdout = io.Discard
	var stderr probeOutput
	cmd.Stderr = &stderr
	cmd.ExtraFiles = []*os.File{reportW, ackR}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if !time.Now().Before(c.deadline) {
		return ProbeTimedOut, nil
	}
	if e = cmd.Start(); e != nil {
		return ProbeFailed, e
	}
	pid := cmd.Process.Pid
	_ = reportW.Close()
	_ = ackR.Close()
	type message struct {
		raw []byte
		err error
	}
	first := make(chan message, 1)
	rest := make(chan message, 1)
	go func() {
		r := bufio.NewReaderSize(reportR, 4097)
		line, e := r.ReadSlice('\n')
		first <- message{bytes.Clone(line), e}
		if e != nil {
			return
		}
		tail, e := io.ReadAll(io.LimitReader(r, 4097))
		rest <- message{tail, e}
	}()
	remaining := time.Until(c.deadline)
	reserve := remaining / 5
	if reserve > time.Second {
		reserve = time.Second
	}
	timer := time.NewTimer(remaining - reserve)
	defer timer.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	valid, bad, timedOut, exited, firstDone, restDone := false, false, false, false, false, false
	for (!exited || !firstDone || (valid && !restDone)) && !bad && !timedOut {
		select {
		case msg := <-first:
			firstDone = true
			first = nil
			if msg.err != nil {
				bad = len(msg.raw) > 0 || !errors.Is(msg.err, io.EOF)
			} else {
				report, e := parseProbeReport(msg.raw, c.nonce, pid)
				if e != nil {
					bad = true
					break
				}
				// Synchronous observation cannot outlive teardown or continue reading PIDs
				// after cancellation. A blocking kernel read can delay return, but the
				// shared deadline prevents its late result from authorizing anything.
				if c.inspect(pid, report.RendererPID, os.Getuid()) != nil {
					bad = true
					break
				}
				if !time.Now().Before(c.deadline) {
					timedOut = true
					break
				}
				valid = true
				if _, e = ackW.Write([]byte{1}); e != nil {
					bad = true
				}
				_ = ackW.Close()
			}
		case msg := <-rest:
			restDone = true
			rest = nil
			if msg.err != nil || len(msg.raw) != 0 {
				bad = true
			}
		case <-ticker.C:
			var e error
			exited, e = leaderWaitState(pid)
			if e != nil {
				bad = true
			}
		case <-timer.C:
			timedOut = true
		}
	}
	// No cmd.Wait occurs before the checked group signal. WNOWAIT keeps the group
	// leader unreaped and its numeric process-group ID reserved through cleanup.
	waitErr, cleanupErr := cleanupProbeChildren(cmd, c.deadline, c.known)
	_ = reportR.Close()
	_ = ackW.Close()
	if cleanupErr != nil {
		return ProbeFailed, cleanupErr
	}
	text, overflow := stderr.snapshot()
	bad = bad || overflow
	if timedOut || !time.Now().Before(c.deadline) {
		return ProbeTimedOut, nil
	}
	if valid && !bad && waitErr == nil {
		return ProbeSandboxed, nil
	}
	signal := 0
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		if s, ok := exitErr.Sys().(syscall.WaitStatus); ok && s.Signaled() {
			signal = int(s.Signal())
		}
	}
	return classifyFailure(true, valid, bad, false, signal, text), nil
}
func probeEnvironment(env []string) []string {
	result := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		switch {
		case key == "CHROME_DEVEL_SANDBOX", key == "ELECTRON_RUN_AS_NODE", key == "NODE_OPTIONS", key == "NODE_PATH", key == "LD_PRELOAD", key == "LD_AUDIT", strings.HasPrefix(key, "DILLA_UPDATE_"):
			continue
		}
		result = append(result, entry)
	}
	return result
}

// probeLifecycle creates the sole deadline before any preflight work. Calls
// that block in the kernel may return late; a late result is never success.
// Cleanup is skipped only when process teardown could not prove it safe.
func probeLifecycle(budget time.Duration, prepare func(time.Time) (func(time.Time) (ProbeResult, error), func() error, error)) (ProbeResult, error) {
	if budget <= 0 || prepare == nil {
		return ProbeFailed, errLaunch
	}
	deadline := time.Now().Add(budget)
	run, cleanup, err := prepare(deadline)
	result := ProbeFailed
	if err == nil {
		if !time.Now().Before(deadline) {
			result = ProbeTimedOut
		} else if run == nil {
			err = errLaunch
		} else {
			result, err = run(deadline)
		}
	}
	if !errors.Is(err, errProbeCleanup) && cleanup != nil {
		if cleanupErr := cleanup(); cleanupErr != nil {
			return ProbeFailed, cleanupErr
		}
	}
	if !time.Now().Before(deadline) {
		return ProbeTimedOut, err
	}
	return result, err
}
func RunSandboxProbe(owned OwnedExecutable) (ProbeResult, error) {
	return probeLifecycle(10*time.Second, func(deadline time.Time) (func(time.Time) (ProbeResult, error), func() error, error) {
		if e := owned.recheck(); e != nil {
			return nil, nil, e
		}
		if !time.Now().Before(deadline) {
			return nil, nil, nil
		}
		scratch, e := os.MkdirTemp("/tmp", "dilla-sandbox-probe-")
		if e != nil {
			return nil, nil, e
		}
		cleanup := func() error { return os.RemoveAll(scratch) }
		if !time.Now().Before(deadline) {
			return nil, cleanup, nil
		}
		b := make([]byte, 32)
		if _, e = rand.Read(b); e != nil {
			return nil, cleanup, e
		}
		nonce := hex.EncodeToString(b)
		if !time.Now().Before(deadline) {
			return nil, cleanup, nil
		}
		if e = owned.recheck(); e != nil {
			return nil, cleanup, e
		}
		return func(shared time.Time) (ProbeResult, error) {
			known := map[int]knownProcess{}
			return runProbeProcess(probeProcessConfig{executable: owned.electron, args: []string{"--dilla-sandbox-probe=" + nonce, "--dilla-sandbox-profile=" + scratch}, env: probeEnvironment(os.Environ()), deadline: shared, nonce: nonce, known: known, inspect: func(main, renderer, uid int) error { return inspectOwnedTree(main, renderer, uid, shared, known) }})
		}, cleanup, nil
	})
}
