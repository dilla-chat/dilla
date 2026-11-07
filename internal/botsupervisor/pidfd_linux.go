//go:build linux

package botsupervisor

import (
	"sync"

	"golang.org/x/sys/unix"
)

// pidfd is a childHandle over pidfd_open(2): it becomes readable when the process ends.
type pidfd struct {
	mu sync.Mutex
	fd int
}

func openPidfd(pid int) (childHandle, error) {
	fd, err := unix.PidfdOpen(pid, unix.PIDFD_NONBLOCK)
	if err != nil {
		return nil, err
	}
	return &pidfd{fd: fd}, nil
}

func (p *pidfd) alive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fd < 0 {
		return false
	}
	fds := []unix.PollFd{{Fd: int32(p.fd), Events: unix.POLLIN}} //nolint:gosec // G115: a descriptor
	n, err := unix.Poll(fds, 0)
	return err == nil && n == 0
}

func (p *pidfd) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fd >= 0 {
		_ = unix.Close(p.fd)
		p.fd = -1
	}
}
