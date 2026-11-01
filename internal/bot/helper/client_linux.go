//go:build linux

package helper

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/id"
)

const maxFrameBytes = 1 << 20

type linuxExchange struct {
	conn   *net.UnixConn
	cmd    *exec.Cmd
	done   chan struct{}
	waitMu sync.Mutex
	wait   error
	once   sync.Once
	kill   time.Duration
}

// privateDiagnosticWriter forwards only the helper's fixed trust-refusal line. The normal
// helper stderr remains discarded; this writer is installed only in an opted-in test process.
type privateDiagnosticWriter struct {
	mu      sync.Mutex
	partial []byte
	discard bool
	out     io.Writer
}

func (w *privateDiagnosticWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	consumed := 0
	for consumed < len(p) {
		if w.discard {
			for consumed < len(p) && p[consumed] != '\n' {
				consumed++
			}
			if consumed < len(p) {
				consumed++
				w.discard = false
			}
			continue
		}
		start := consumed
		for consumed < len(p) && p[consumed] != '\n' {
			consumed++
		}
		lineLen := len(w.partial) + consumed - start
		if lineLen > 256 {
			w.partial = w.partial[:0]
			w.discard = consumed == len(p)
			if !w.discard {
				consumed++
			}
			continue
		}
		w.partial = append(w.partial, p[start:consumed]...)
		if consumed == len(p) {
			break
		}
		consumed++ // newline
		line := string(w.partial)
		w.partial = w.partial[:0]
		if isPrivateBotDiagnostic(line) && w.out != nil {
			if _, err := io.WriteString(w.out, line+"\n"); err != nil {
				return len(p), nil
			}
		}
	}
	return len(p), nil
}

func isPrivateBotDiagnostic(line string) bool {
	const trustPrefix = "dilla_bot_helper trust_sender_refusal="
	const createPrefix = "dilla_bot_helper interaction_create_refusal="
	const readPrefix = "dilla_bot_helper interaction_read_refusal="
	prefix := trustPrefix
	if len(line) >= len(createPrefix) && line[:len(createPrefix)] == createPrefix {
		prefix = createPrefix
	}
	if len(line) >= len(readPrefix) && line[:len(readPrefix)] == readPrefix {
		prefix = readPrefix
	}
	if len(line) <= len(prefix) || line[:len(prefix)] != prefix {
		return false
	}
	switch line[len(prefix):] {
	case "binding", "credential", "device_list_stale", "device_unlisted", "tier_mismatch", "umk_changed", "core_input", "core_state", "core_storage", "other",
		"snapshot_clock", "device_trust_changed", "binding_mismatch", "user_pin_unavailable", "device_pin_unavailable", "group_exists", "participant_exists", "intent_conflict", "group_load", "group_unavailable", "snapshot_verification", "snapshot_pin_unavailable", "key_package_decode", "key_package_kind", "key_package_validation", "clock_unavailable", "snapshot_expired", "welcome_missing", "candidate_shape":
		return true
	default:
		return false
	}
}

func launch(options Options) (exchange, error) {
	if options.Executable == "" || options.StateDir == "" || !filepath.IsAbs(options.Executable) || !filepath.IsAbs(options.StateDir) {
		return nil, errors.New("helper executable and state directory must be explicit absolute paths")
	}
	info, err := os.Lstat(options.Executable)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		return nil, errors.New("helper executable is unavailable")
	}
	state, err := filepath.Abs(options.StateDir)
	if err != nil {
		return nil, errors.New("helper state directory is invalid")
	}
	sockets, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("could not create private helper socket")
	}
	parentFile := os.NewFile(uintptr(sockets[0]), "helper-parent")
	childFile := os.NewFile(uintptr(sockets[1]), "helper-child")
	parentConnRaw, err := net.FileConn(parentFile)
	_ = parentFile.Close()
	if err != nil {
		_ = childFile.Close()
		return nil, errors.New("could not adopt private helper socket")
	}
	parentConn, ok := parentConnRaw.(*net.UnixConn)
	if !ok {
		_ = parentConnRaw.Close()
		_ = childFile.Close()
		return nil, errors.New("private helper socket is not Unix")
	}
	//nolint:noctx // The exchange lifecycle controls process cancellation through stop and kill.
	// #nosec G204 -- Executable is an explicit configured absolute path validated as a regular executable above.
	cmd := exec.Command(options.Executable, "--state-dir", state)
	cmd.Dir = "/"
	cmd.Env = []string{"LANG=C", "LC_ALL=C", "PATH=/usr/bin:/bin"}
	cmd.Stdin = nil
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if os.Getenv("DILLA_BOT_PRIVATE_DIAGNOSTICS") == "1" {
		cmd.Env = append(cmd.Env, "DILLA_BOT_PRIVATE_DIAGNOSTICS=1")
		cmd.Stderr = &privateDiagnosticWriter{out: os.Stderr}
	}
	cmd.ExtraFiles = []*os.File{childFile}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if options.launcher != nil {
		err = options.launcher(cmd)
	} else {
		err = cmd.Start()
	}
	_ = childFile.Close()
	if err != nil {
		_ = parentConn.Close()
		return nil, errors.New("could not start native helper")
	}
	t := &linuxExchange{conn: parentConn, cmd: cmd, done: make(chan struct{}), kill: options.KillTimeout}
	go func() {
		err := cmd.Wait()
		t.waitMu.Lock()
		t.wait = err
		t.waitMu.Unlock()
		close(t.done)
	}()
	return t, nil
}

func (t *linuxExchange) request(ctx context.Context, requestID id.ID, operation protocol.HelperOperation, payload []any, timeout time.Duration) (protocol.HelperResponse, error) {
	var empty protocol.HelperResponse
	frame, err := protocol.EncodeHelperRequest(protocol.HelperRequest{RequestID: requestID, Operation: operation, Payload: payload})
	if err != nil || len(frame) > maxFrameBytes {
		clear(frame)
		return empty, errors.New("helper frame exceeds protocol limit")
	}
	defer clear(frame)
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := t.conn.SetDeadline(deadline); err != nil {
		_ = t.stop(0)
		return empty, &OutcomeUnknownError{cause: errors.New("transport deadline failed")}
	}
	cancelDone := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-ctx.Done():
			_ = t.conn.Close()
		case <-cancelDone:
		}
	}()
	defer func() { close(cancelDone); <-watchDone }()

	if err := writeFull(t.conn, frame); err != nil {
		_ = t.stop(0)
		return empty, unknown(err)
	}
	var prefix [4]byte
	if _, err := io.ReadFull(t.conn, prefix[:]); err != nil {
		_ = t.stop(0)
		return empty, unknown(err)
	}
	n := int(readU32(prefix[:]))
	if n < 1 || n > maxFrameBytes {
		_ = t.stop(0)
		return empty, unknown(errors.New("invalid response frame length"))
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(t.conn, body); err != nil {
		clear(body)
		_ = t.stop(0)
		return empty, unknown(err)
	}
	response, err := protocol.DecodeHelperResponseCBOR(body)
	clear(body)
	if err != nil || response.RequestID != requestID {
		_ = t.stop(0)
		return empty, unknown(errors.New("response envelope mismatch"))
	}
	if err := t.conn.SetDeadline(time.Time{}); err != nil {
		_ = t.stop(0)
		return empty, unknown(errors.New("transport deadline reset failed"))
	}
	return response, nil
}

func (t *linuxExchange) stop(timeout time.Duration) error {
	var result error
	t.once.Do(func() {
		_ = t.conn.Close()
		if timeout > 0 {
			select {
			case <-t.done:
			case <-time.After(timeout):
				t.killAndWait(&result)
			}
		} else {
			t.killAndWait(&result)
		}
	})
	return result
}

func (t *linuxExchange) killAndWait(result *error) {
	// Process.Kill is tied to os/exec's tracked process handle. Avoid a raw
	// negative-pid signal: Wait may already have reaped this child, making its
	// former process-group id reusable by an unrelated process.
	_ = t.cmd.Process.Kill()
	select {
	case <-t.done:
	case <-time.After(t.kill):
		*result = errors.New("native helper did not exit after termination")
	}
}

func writeFull(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if n > 0 {
			b = b[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func readU32(src []byte) uint32 {
	return uint32(src[0])<<24 | uint32(src[1])<<16 | uint32(src[2])<<8 | uint32(src[3])
}
