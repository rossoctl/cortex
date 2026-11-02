//go:build unix && !linux && !darwin

package vmcontrol

import (
	"errors"
	"net"
)

// peerUID refuses: without a verified peer credential the control socket would
// admit anyone who can reach it, so unsupported unixes fail closed.
func peerUID(*net.UnixConn) (int, error) {
	return 0, errors.New("peer credentials unsupported on this platform")
}
