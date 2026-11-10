//go:build !linux

package gateway

import (
	"errors"
	"net"
)

// peerCredentials is Linux-only (SO_PEERCRED); elsewhere the Bot listener admits nobody.
func peerCredentials(*net.UnixConn) (int32, uint32, error) {
	return 0, 0, errors.New("gateway: peer credentials are read on Linux only")
}
