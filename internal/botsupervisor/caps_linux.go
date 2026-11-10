//go:build linux

package botsupervisor

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Capability numbers (linux/capability.h).
const (
	capSetgid         = 6
	capSetuid         = 7
	capSetpcap        = 8
	capNetBindService = 10
)

// capSets is one thread's effective, permitted and inheritable sets (64 bits each, from the two v3 words).
type capSets struct{ effective, permitted, inheritable uint64 }

func capget() (capSets, error) {
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		return capSets{}, err
	}
	join := func(lo, hi uint32) uint64 { return uint64(lo) | uint64(hi)<<32 }
	return capSets{
		effective:   join(data[0].Effective, data[1].Effective),
		permitted:   join(data[0].Permitted, data[1].Permitted),
		inheritable: join(data[0].Inheritable, data[1].Inheritable),
	}, nil
}

func capData(s capSets) [2]unix.CapUserData {
	split := func(v uint64) (uint32, uint32) { return uint32(v), uint32(v >> 32) } //nolint:gosec // G115: the two halves
	var d [2]unix.CapUserData
	d[0].Effective, d[1].Effective = split(s.effective)
	d[0].Permitted, d[1].Permitted = split(s.permitted)
	d[0].Inheritable, d[1].Inheritable = split(s.inheritable)
	return d
}

// lastCap is the highest capability number this kernel knows.
func lastCap() int {
	b, err := os.ReadFile("/proc/sys/kernel/cap_last_cap")
	if err != nil {
		return 63
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n < 0 || n > 63 {
		return 63
	}
	return n
}

// statusSets reads the ambient and bounding sets from /proc/self/status (capget has neither).
func statusSets() (ambient, bounding uint64, err error) {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch k {
		case "CapAmb":
			ambient, err = strconv.ParseUint(strings.TrimSpace(v), 16, 64)
		case "CapBnd":
			bounding, err = strconv.ParseUint(strings.TrimSpace(v), 16, 64)
		}
		if err != nil {
			return 0, 0, err
		}
	}
	return ambient, bounding, nil
}

// HoldsSetuid reports whether this process can switch users: CAP_SETUID and CAP_SETGID in its permitted set.
func HoldsSetuid() bool {
	s, err := capget()
	return err == nil && s.permitted&(1<<capSetuid) != 0 && s.permitted&(1<<capSetgid) != 0
}

// errCgoCapabilities is DropServingCapabilities' refusal in a cgo build, where Go cannot change every thread's
// capabilities (syscall.AllThreadsSyscall returns ENOTSUP). Release builds are CGO_ENABLED=0.
var errCgoCapabilities = errors.New("this dillad was built with cgo and cannot drop capabilities from every thread; build it with CGO_ENABLED=0 as the release does")

// allThreadsSyscall is syscall.AllThreadsSyscall; a variable so a test can stand in for a cgo build.
var allThreadsSyscall = syscall.AllThreadsSyscall

// allThreads runs one raw syscall on every thread of the process.
func allThreads(trap, a1, a2, a3 uintptr) error {
	if _, _, errno := allThreadsSyscall(trap, a1, a2, a3); errno != 0 {
		if errno == syscall.ENOTSUP {
			return errCgoCapabilities
		}
		return errno
	}
	return nil
}

// dropOnAllThreads removes the capabilities in drop from every set of every thread: bounding first (it needs
// CAP_SETPCAP, so CAP_SETPCAP itself goes last), then ambient, then the effective, permitted and inheritable sets.
func dropOnAllThreads(drop []int, canBound bool) error {
	if canBound {
		for _, c := range drop {
			if c == capSetpcap {
				continue
			}
			if err := allThreads(unix.SYS_PRCTL, unix.PR_CAPBSET_DROP, uintptr(c), 0); err != nil {
				return fmt.Errorf("drop capability %d from the bounding set: %w", c, err)
			}
		}
		for _, c := range drop {
			if c == capSetpcap {
				if err := allThreads(unix.SYS_PRCTL, unix.PR_CAPBSET_DROP, uintptr(c), 0); err != nil {
					return fmt.Errorf("drop capability %d from the bounding set: %w", c, err)
				}
			}
		}
	}
	for _, c := range drop {
		// PR_CAP_AMBIENT takes five arguments; the last two must be zero, which a three-argument call leaves them.
		if err := allThreads(unix.SYS_PRCTL, unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_LOWER, uintptr(c)); err != nil {
			return fmt.Errorf("drop capability %d from the ambient set: %w", c, err)
		}
	}
	s, err := capget()
	if err != nil {
		return err
	}
	for _, c := range drop {
		mask := ^(uint64(1) << c)
		s.effective &= mask
		s.permitted &= mask
		s.inheritable &= mask
	}
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := capData(s)
	//nolint:gosec // G103: capset(2) takes the header and data by pointer; both outlive the call
	if err := allThreads(unix.SYS_CAPSET, uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0); err != nil {
		return fmt.Errorf("capset: %w", err)
	}
	return nil
}

// readCaps is the process's capability sets (capget and /proc/self/status); a variable so a test can stand in.
var readCaps = func() (s capSets, ambient, bounding uint64, err error) {
	if s, err = capget(); err != nil {
		return s, 0, 0, fmt.Errorf("capget: %w", err)
	}
	if ambient, bounding, err = statusSets(); err != nil {
		return s, 0, 0, fmt.Errorf("read /proc/self/status: %w", err)
	}
	return s, ambient, bounding, nil
}

// DropServingCapabilities removes CAP_SETUID, CAP_SETGID and CAP_SETPCAP from every set of every thread of the
// serving dillad: the keeper, already started, is the only process left that holds them. A process that holds none
// of them in its effective, permitted, inheritable or ambient set (a development run, a test) has nothing to drop.
//
// It fails closed: holding one of them without CAP_SETPCAP (so the bounding set cannot shrink), or being unable to
// change every thread (a cgo build: syscall.AllThreadsSyscall is ENOTSUP), is an error, in every mode. The one
// exception is rootDevTest (dev_same_user) in a process whose euid is 0, a development or test run as root, where
// dropping three capabilities from uid 0 buys nothing; it is logged.
func DropServingCapabilities(log *slog.Logger, rootDevTest bool) error {
	drop := []int{capSetuid, capSetgid, capSetpcap}
	s, ambient, _, err := readCaps()
	if err != nil {
		return err
	}
	var mask uint64
	for _, c := range drop {
		mask |= 1 << c
	}
	canBound := s.effective&(1<<capSetpcap) != 0
	held := (s.effective|s.permitted|s.inheritable|ambient)&mask != 0
	if !held {
		return nil
	}
	if !canBound {
		err = errors.New("dillad holds CAP_SETUID or CAP_SETGID without CAP_SETPCAP, so it cannot remove them from its bounding set; give the unit CAP_SETPCAP as deploy/dilla.service does")
	} else {
		err = dropOnAllThreads(drop, true)
	}
	if err != nil && rootDevTest && os.Geteuid() == 0 {
		if log != nil {
			log.Warn("dev_same_user as root: keeping CAP_SETUID and CAP_SETGID", "err", err)
		}
		return nil
	}
	return err
}

// keeperCapabilities is what the keeper keeps: CAP_SETUID and CAP_SETGID to start the Bot as its user, and
// CAP_SETPCAP, which passes to bot-launch so it can empty its own bounding set.
const keeperCapabilities = uint64(1)<<capSetuid | uint64(1)<<capSetgid | uint64(1)<<capSetpcap

// narrowKeeperCapabilities leaves the keeper only keeperCapabilities, in every set of every thread. Failing to is
// fatal for the keeper: without CAP_SETPCAP it cannot, and in a cgo build it cannot change every thread (allowCgo,
// for the package's own cgo test keeper only, lets that one go).
func narrowKeeperCapabilities(allowCgo bool) error {
	s, _, _, err := readCaps()
	if err != nil {
		return err
	}
	if s.effective&(1<<capSetpcap) == 0 {
		return errors.New("the bot keeper needs CAP_SETPCAP (deploy/dilla.service grants it)")
	}
	var drop []int
	for c := 0; c <= lastCap(); c++ {
		if keeperCapabilities&(1<<c) == 0 {
			drop = append(drop, c)
		}
	}
	err = dropOnAllThreads(drop, true)
	if err != nil && allowCgo && errors.Is(err, errCgoCapabilities) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("narrow its capabilities to CAP_SETUID, CAP_SETGID and CAP_SETPCAP: %w", err)
	}
	return nil
}
