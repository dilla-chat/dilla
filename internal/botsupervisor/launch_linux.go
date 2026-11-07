//go:build linux

package botsupervisor

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// LaunchMain is `dillad bot-launch`, which the keeper runs as the Bot user with the arguments launchArgs builds from
// its own configuration. In this order, on one locked thread (the one that then execs, so its credentials are the new
// image's), it:
//
//  1. clears the ambient capability set;
//  2. drops every capability from the bounding set (CAP_SETPCAP, inherited from the keeper, allows it; with
//     --same-user there is none, and a development run, which holds no capability, keeps a full bounding set that
//     no_new_privs makes inert);
//  3. empties the effective, permitted and inheritable sets;
//  4. sets no_new_privs;
//  5. marks every descriptor above 3 close-on-exec, and 0-3 not: fd 3 is the launch token's pipe;
//  6. replaces the environment with the allowlist (PATH, HOME, DILLA_BOT_SOCKET, DILLA_BOT_STATE_DIR,
//     DILLA_BOT_HELPER, DILLA_BOT_WASI);
//  7. sets umask 0077 and changes to the state directory;
//
// and execs the executable with no arguments. The token is never in argv or the environment. It returns only on
// failure.
func LaunchMain(args []string) error {
	fs := flag.NewFlagSet("dillad bot-launch", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	executable := fs.String("executable", "", "")
	stateDir := fs.String("state-dir", "", "")
	socket := fs.String("socket", "", "")
	helper := fs.String("helper", "", "")
	wasi := fs.String("wasi", "", "")
	sameUser := fs.Bool("same-user", false, "")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("bot-launch: %w", err)
	}
	if fs.NArg() != 0 {
		return errors.New("bot-launch: unexpected arguments")
	}
	for name, p := range map[string]string{"executable": *executable, "state-dir": *stateDir, "socket": *socket, "helper": *helper, "wasi": *wasi} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return fmt.Errorf("bot-launch: --%s must be a clean absolute path", name)
		}
	}
	runtime.LockOSThread()
	// Never unlocked: the thread either execs or the process exits.

	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		return fmt.Errorf("bot-launch: clear the ambient set: %w", err)
	}
	for c := 0; c <= lastCap(); c++ {
		if err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(c), 0, 0, 0); err != nil {
			if *sameUser && errors.Is(err, unix.EPERM) {
				break
			}
			return fmt.Errorf("bot-launch: drop capability %d from the bounding set: %w", c, err)
		}
	}
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var none [2]unix.CapUserData
	if err := unix.Capset(&hdr, &none[0]); err != nil {
		return fmt.Errorf("bot-launch: empty the capability sets: %w", err)
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("bot-launch: no_new_privs: %w", err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(execFD, &st); err != nil {
		return fmt.Errorf("bot-launch: fd %d: %w", execFD, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("bot-launch: fd %d is not the executable (mode %o)", execFD, st.Mode)
	}
	if err := closeExtraDescriptors(); err != nil {
		return fmt.Errorf("bot-launch: descriptors: %w", err)
	}
	env := []string{
		"PATH=" + LaunchPATH,
		"HOME=" + *stateDir,
		"DILLA_BOT_SOCKET=" + *socket,
		"DILLA_BOT_STATE_DIR=" + *stateDir,
		"DILLA_BOT_HELPER=" + *helper,
		"DILLA_BOT_WASI=" + *wasi,
	}
	syscall.Umask(0o077)
	if err := os.Chdir(*stateDir); err != nil {
		return fmt.Errorf("bot-launch: %w", err)
	}
	//nolint:gosec // G204: the keeper's configured executable, checked root-owned and not writable by others at keeper start
	// The keeper opened and checked the executable and handed it over as fd 4; exec that very file. The path is
	// argv[0] only. fd 4 is close-on-exec by now, so the Bot does not inherit it.
	return fmt.Errorf("bot-launch: exec %s: %w", *executable, syscall.Exec(execFDPath, []string{*executable}, env))
}

// execFD is the descriptor the keeper passes the opened executable on; bot-launch execs it through /proc. (x/sys has no
// execveat, and a raw execveat would bypass the Go runtime's exec lock.)
const (
	execFD     = 4
	execFDPath = "/proc/self/fd/4"
)

// closeExtraDescriptors marks every descriptor above 3 close-on-exec (the Go runtime's own included: they close at
// the exec, not before it) and clears close-on-exec from 0-3, which must reach the Bot. Descriptor 3 must be open.
func closeExtraDescriptors() error {
	for fd := 0; fd <= 3; fd++ {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, 0); err != nil {
			return fmt.Errorf("fd %d: %w", fd, err)
		}
	}
	if err := unix.CloseRange(4, ^uint(0), unix.CLOSE_RANGE_CLOEXEC); err == nil {
		return nil
	}
	// Kernels before 5.11 have no CLOSE_RANGE_CLOEXEC: walk /proc/self/fd instead.
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return err
	}
	for _, e := range entries {
		fd, err := strconv.Atoi(e.Name())
		if err != nil || fd <= 3 {
			continue
		}
		_, _ = unix.FcntlInt(uintptr(fd), unix.F_SETFD, unix.FD_CLOEXEC)
	}
	return nil
}
