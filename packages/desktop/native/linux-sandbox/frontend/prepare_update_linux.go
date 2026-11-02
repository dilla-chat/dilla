//go:build linux && cgo

package frontend

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func prepareUpdateRole(args, env []string) (string, bool, error) {
	return exactUpdateRole(args, env, "--dilla-sandbox-prepare-update")
}
func checkUpdateRole(args, env []string) (string, bool, error) {
	return exactUpdateRole(args, env, "--dilla-sandbox-check-update")
}
func exactUpdateRole(args, env []string, prefix string) (string, bool, error) {
	found := false
	for _, arg := range args {
		if strings.HasPrefix(arg, prefix) {
			found = true
		}
	}
	if !found {
		return "", false, nil
	}
	if len(args) != 1 || !strings.HasPrefix(args[0], prefix+"=") {
		return "", false, errRole
	}
	nonce := strings.TrimPrefix(args[0], prefix+"=")
	if !roleNonce.MatchString(nonce) {
		return "", false, errRole
	}
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "DILLA_UPDATE_") {
			return "", false, errRole
		}
	}
	return nonce, true, nil
}

func prepareUpdate(nonce string, e EmbeddedCapsule, checkOnly bool) error {
	if err := prepareEndpoints(); err != nil {
		return err
	}
	// Move the response away from the fixed authorization FD and mark it close-on-exec.
	reportFD, err := unix.FcntlInt(4, unix.F_DUPFD_CLOEXEC, 5)
	if err != nil {
		return errRole
	}
	if unix.SetNonblock(reportFD, true) != nil {
		_ = unix.Close(reportFD)
		return errRole
	}
	report := os.NewFile(uintptr(reportFD), "report")
	defer func() { _ = report.Close() }()
	_ = syscall.Close(4)
	signature, err := readPrepareSignature()
	if err != nil {
		return err
	}
	// Confirm this launcher came from its own trusted mounted AppImage, never a selected sibling.
	owned, err := ownExecutable()
	if err != nil || owned.recheck() != nil {
		return errLaunch
	}
	signed, err := e.WithSignature(signature)
	if err != nil {
		return errLaunch
	}
	state, err := checkStatus()
	if err != nil {
		return errLaunch
	}
	if err = prepareStatus(state, e, checkOnly, func() error {
		outcome, err := Ensure(signed, UpdateSetup)
		if err != nil || outcome != Prepared && outcome != AlreadyReady {
			return errLaunch
		}
		return nil
	}); err != nil {
		return err
	}
	state, err = checkStatus()
	if err != nil || !validStatus(state) || (launchState(state, e) != "missing" && launchState(state, e) != "ready") {
		return errLaunch
	}
	kind := "dilla-sandbox-prepared"
	if checkOnly {
		kind = "dilla-sandbox-checked"
	}
	result, err := json.Marshal(struct {
		Format             int    `json:"format"`
		Kind               string `json:"kind"`
		Nonce              string `json:"nonce"`
		Generation         int64  `json:"generation"`
		CompatibilityEpoch int64  `json:"compatibilityEpoch"`
	}{1, kind, nonce, e.constants.Generation, e.constants.CompatibilityEpoch})
	if err != nil || len(result) > 512 {
		return errors.New("invalid preparation report")
	}
	if err = report.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	_, err = report.Write(append(result, '\n'))
	if err != nil {
		return err
	}
	return report.Close()
}

func prepareStatus(state Status, e EmbeddedCapsule, checkOnly bool, ensure func() error) error {
	if !validStatus(state) {
		return errLaunch
	}
	switch launchState(state, e) {
	case "missing", "ready":
		return nil
	case "maintenance":
		if checkOnly || !state.resident || state.Generation > e.constants.Generation || ensure == nil {
			return errLaunch
		}
		return ensure()
	default:
		return errLaunch
	}
}

func prepareEndpoints() error {
	var input, output syscall.Stat_t
	if syscall.Fstat(3, &input) != nil || syscall.Fstat(4, &output) != nil || input.Dev == output.Dev && input.Ino == output.Ino {
		return errRole
	}
	for i, stat := range []syscall.Stat_t{input, output} {
		fd := 3 + i
		switch stat.Mode & syscall.S_IFMT {
		case syscall.S_IFIFO:
			flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
			want := unix.O_RDONLY
			if i == 1 {
				want = unix.O_WRONLY
			}
			if err != nil || flags&unix.O_ACCMODE != want {
				return errRole
			}
		case syscall.S_IFSOCK:
			typ, e1 := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TYPE)
			listener, e2 := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ACCEPTCONN)
			local, e3 := unix.Getsockname(fd)
			peer, e4 := unix.Getpeername(fd)
			l, lok := local.(*unix.SockaddrUnix)
			p, pok := peer.(*unix.SockaddrUnix)
			if e1 != nil || e2 != nil || e3 != nil || e4 != nil || typ != unix.SOCK_STREAM || listener != 0 || !lok || !pok || (l.Name != "" && l.Name != "@") || (p.Name != "" && p.Name != "@") {
				return errRole
			}
		default:
			return errRole
		}
	}
	return nil
}

func readPrepareSignature() ([]byte, error) {
	if unix.SetNonblock(3, true) != nil {
		return nil, errRole
	}
	source := os.NewFile(3, "signature")
	if err := source.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		_ = source.Close()
		return nil, errRole
	}
	signature, err := io.ReadAll(io.LimitReader(source, 65))
	_ = source.Close()
	if err != nil || len(signature) != 64 {
		return nil, errRole
	}
	return signature, nil
}
