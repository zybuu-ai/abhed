//go:build !linux && !darwin

package microvm

import (
	"errors"
	"net"
)

func peerUID(net.Conn) (uint32, error) {
	return 0, errors.New("the socket's owner cannot be checked here")
}
