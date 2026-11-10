//go:build linux

package gateway

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerCredentials reads SO_PEERCRED: the pid and uid of the process that connected, as the kernel recorded them at
// connect time. They cannot be forged by the peer.
func peerCredentials(c *net.UnixConn) (pid int32, uid uint32, err error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, 0, err
	}
	if credErr != nil {
		return 0, 0, credErr
	}
	return cred.Pid, cred.Uid, nil
}
