package youtube

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
)

// The worker is embedded so production does not depend on a source checkout.
// Python 3 and bubblewrap remain explicit host prerequisites.
//
//go:embed sandbox_worker.py
var sandboxWorker string

const defaultSandboxStartupTimeout = 20 * time.Second

func launchNetworkWorker(ctx context.Context, bwrap, python string, tools, workerArgs []string, startupTimeout, maxDuration time.Duration) (_ *isolatedPCM, failure error) {
	ctx = client.WithoutCooperativeStop(client.WithBackground(ctx))
	root, err := newSandboxTempDir()
	if err != nil {
		return nil, ErrSandboxUnavailable
	}
	socketPath := filepath.Join(root, "e.sock")
	listener, err := (&net.ListenConfig{}).Listen(ctx, "unix", socketPath)
	if err != nil {
		_ = os.RemoveAll(root)
		return nil, ErrSandboxUnavailable
	}
	broker := newEgressBroker(ctx, listener)
	runCtx, cancel := context.WithTimeout(ctx, maxDuration)
	cmd, stdout, err := sandboxCommand(runCtx, bwrap, python, tools, workerArgs, socketPath)
	if err != nil {
		cancel()
		broker.close()
		_ = os.RemoveAll(root)
		return nil, ErrSandboxUnavailable
	}
	var inputs []string
	if len(workerArgs) > 0 {
		if workerArgs[0] == "exec" {
			inputs = append(inputs, workerArgs[len(workerArgs)-1])
		}
	}
	if input, ok := ctx.Value(diagnosticInputKey{}).(string); ok {
		inputs = append(inputs, input)
	}
	process := &isolatedPCM{diagnosticInputs: inputs, stdout: stdout, cmd: cmd, ctx: runCtx, cancel: cancel, broker: broker, root: root, done: make(chan struct{})}
	if len(workerArgs) > 0 && workerArgs[0] == "serve" {
		// The long-lived lookup worker reads its requests from stdin. The sandbox
		// command (mounts, namespaces, broker socket) is the same as for any worker.
		stdin, err := cmd.StdinPipe()
		if err != nil {
			cancel()
			_ = stdout.Close()
			broker.close()
			_ = os.RemoveAll(root)
			return nil, ErrSandboxUnavailable
		}
		process.stdin = stdin
	}
	if err := cmd.Start(); err != nil {
		cancel()
		_ = stdout.Close()
		broker.close()
		_ = os.RemoveAll(root)
		return nil, ErrSandboxUnavailable
	}
	go process.wait()
	workerFailure := ErrPCMStart
	if len(workerArgs) > 0 && (workerArgs[0] == "exec" || workerArgs[0] == "serve") {
		workerFailure = ErrExtractFailed
	}
	timer := time.NewTimer(startupTimeout)
	defer timer.Stop()
	select {
	case <-broker.ready:
		return process, nil
	case <-process.done:
		_ = process.Close()
		return nil, process.startupFailure(workerFailure)
	case <-runCtx.Done():
		_ = process.Close()
		return nil, process.startupFailure(workerFailure)
	case <-timer.C:
		_ = process.Close()
		return nil, process.failure(errors.Join(ErrSandboxUnavailable, context.DeadlineExceeded))
	}
}

func newSandboxTempDir() (string, error) {
	root, err := os.MkdirTemp("", "dilla-yt-")
	if err != nil {
		return "", ErrSandboxUnavailable
	}
	if err := os.Chmod(root, 0700); err != nil { // #nosec G302 -- the socket directory is owner-only.
		_ = os.RemoveAll(root)
		return "", ErrSandboxUnavailable
	}
	return root, nil
}

func sandboxCommand(ctx context.Context, bwrap, python string, tools, workerArgs []string, socketPath string) (*exec.Cmd, io.ReadCloser, error) {
	args := []string{
		"--unshare-all", "--die-with-parent", "--new-session", "--cap-drop", "ALL",
		"--ro-bind", "/usr", "/usr",
	}
	for _, path := range []string{"/lib", "/lib64"} {
		if _, err := os.Stat(path); err == nil {
			args = append(args, "--ro-bind", path, path)
		}
	}
	if info, err := os.Lstat("/bin"); err == nil && info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink("/bin")
		if err != nil {
			return nil, nil, err
		}
		args = append(args, "--symlink", target, "/bin")
	} else {
		args = append(args, "--ro-bind", "/bin", "/bin")
	}
	caBundle, certDir, err := systemCABundle()
	if err != nil {
		return nil, nil, err
	}
	args = append(args,
		"--dir", "/etc", "--dir", "/etc/ssl", "--dir", "/etc/ssl/certs",
		"--ro-bind", certDir, "/etc/ssl/certs",
		"--ro-bind", caBundle, "/etc/ssl/cert.pem",
		"--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp", "--tmpfs", "/run",
	)
	// Debian's FFmpeg may resolve shared libraries through /etc/alternatives
	// (for example libblas.so.3). Keep those symlinks available read-only.
	if info, err := os.Stat("/etc/alternatives"); err == nil && info.IsDir() {
		args = append(args, "--dir", "/etc/alternatives", "--ro-bind", "/etc/alternatives", "/etc/alternatives")
	}
	for _, path := range append([]string{python}, tools...) {
		if path != "" {
			args = addPathMount(args, path)
		}
	}
	args = addParentDirs(args, socketPath)
	args = append(args,
		"--bind", socketPath, "/run/egress.sock",
		"--setenv", "HOME", "/tmp", "--setenv", "TMPDIR", "/tmp",
		"--setenv", "PATH", "/usr/bin:/bin", "--setenv", "LANG", "C.UTF-8",
		"--setenv", "LC_ALL", "C.UTF-8", "--setenv", "SSL_CERT_FILE", "/etc/ssl/cert.pem",
		"--", python, "-I", "-c", sandboxWorker,
	)
	args = append(args, workerArgs...)
	cmd := exec.CommandContext(ctx, bwrap, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C"}
	cmd.Stderr = &sandboxStderr{}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	return cmd, stdout, nil
}

type isolatedPCM struct {
	diagnosticInputs []string
	stdout           io.ReadCloser
	stdin            io.WriteCloser
	cmd              *exec.Cmd
	ctx              context.Context
	cancel           context.CancelFunc
	broker           *egressBroker
	root             string
	done             chan struct{}
	once             sync.Once
	err              error
	exitCode         int
	ctxErr           error
}

func (p *isolatedPCM) wait() {
	err := p.cmd.Wait()
	p.err = err
	if err == nil {
		p.exitCode = 0
	} else {
		exit := &exec.ExitError{}
		if errors.As(err, &exit) {
			p.exitCode = exit.ExitCode()
		} else {
			p.exitCode = -1
		}
	}
	p.ctxErr = p.ctx.Err()
	p.cancel()
	p.broker.close()
	_ = os.RemoveAll(p.root)
	close(p.done)
}

func (p *isolatedPCM) Close() error {
	p.once.Do(func() {
		p.cancel()
		_ = p.stdout.Close()
	})
	select {
	case <-p.done:
	case <-time.After(3 * time.Second):
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		<-p.done
	}
	return nil
}

type boundedProcessPCM struct {
	process *isolatedPCM
	limit   int64
	read    int64
}

func (r *boundedProcessPCM) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	remaining := r.limit - r.read
	if remaining <= 0 {
		var extra [1]byte
		n, err := r.process.stdout.Read(extra[:])
		if n > 0 {
			_ = r.Close()
			return 0, ErrPCMLimit
		}
		if processReadEnded(err) {
			<-r.process.done
			if r.process.ctxErr != nil {
				return 0, r.process.ctxErr
			}
			if r.process.exitCode == 75 {
				return 0, ErrPCMLimit
			}
			if r.process.err != nil {
				return 0, r.process.failure(ErrPCMStart)
			}
		}
		return 0, err
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := r.process.stdout.Read(p)
	r.read += int64(n)
	if processReadEnded(err) {
		<-r.process.done
		if r.process.ctxErr != nil {
			return n, r.process.ctxErr
		}
		if r.process.exitCode == 75 {
			return n, ErrPCMLimit
		}
		if r.process.err != nil {
			return n, r.process.failure(ErrPCMStart)
		}
	}
	return n, err
}

func (r *boundedProcessPCM) Close() error {
	return r.process.Close()
}

var _ io.ReadCloser = (*boundedProcessPCM)(nil)

// sandboxStderr bounds memory while continuing to drain the subprocess pipe. Bot v2 Task 23 removed the timing marks
// the "pcm" worker mode wrote here.
type sandboxStderr struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (b *sandboxStderr) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if remaining := 4096 - b.data.Len(); remaining > 0 {
		_, _ = b.data.Write(p[:min(len(p), remaining)])
	}
	return n, nil
}
func (b *sandboxStderr) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.data.String() }
func (p *isolatedPCM) failure(cause error) *ProcessError {
	stderr, _ := p.cmd.Stderr.(*sandboxStderr)
	text := ""
	if stderr != nil {
		text = stderr.String()
	}
	// Only the sandboxed worker can complete the broker handshake. Before that
	// point, exit codes and stderr belong to sandbox setup, never to a track.
	// 125 is reserved by the worker for its own setup failures after readiness.
	unavailable := p.exitCode == 125
	if p.broker != nil {
		select {
		case <-p.broker.ready:
		default:
			unavailable = true
		}
	}
	if unavailable {
		deadline := error(nil)
		if errors.Is(cause, context.DeadlineExceeded) {
			deadline = context.DeadlineExceeded
		}
		cause = errors.Join(ErrSandboxUnavailable, p.ctxErr, deadline)
	} else if p.exitCode == 75 {
		// Wait may close stdout before its reader observes EOF, or finish
		// before the launcher selects readiness. Retain the worker's limit
		// status in all paths rather than reporting an extraction failure.
		switch {
		case errors.Is(cause, ErrExtractFailed):
			cause = errors.Join(ErrMetadataTooLarge, p.ctxErr)
		case errors.Is(cause, ErrPCMStart):
			cause = errors.Join(ErrPCMLimit, p.ctxErr)
		}
	}
	failure := newProcessError(cause, text, p.exitCode, p.diagnosticInputs...)
	if unavailable {
		failure.class = "sandbox_unavailable"
	}
	return failure
}

// A fast worker can finish before launchNetworkWorker selects readiness. Use
// the same handshake-based classification for both process-exit select paths.
func (p *isolatedPCM) startupFailure(cause error) *ProcessError {
	return p.failure(errors.Join(cause, p.ctxErr))
}

// exec.Cmd.Wait closes StdoutPipe, which can race the reader's final EOF. Read
// the completed process diagnostics for either pipe termination signal.
func processReadEnded(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) || errors.Is(err, io.ErrClosedPipe)
}

func (r *boundedProcessPCM) SourceGone() bool {
	select {
	case <-r.process.done:
		return true
	default:
		return false
	}
}
