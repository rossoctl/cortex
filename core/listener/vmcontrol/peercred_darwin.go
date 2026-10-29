//go:build darwin

package vmcontrol

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// peerUID returns the effective UID of the process that connected c
// (LOCAL_PEERCRED).
func peerUID(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var uid int
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		var x *unix.Xucred
		x, cerr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if cerr == nil {
			uid = int(x.Uid)
		}
	}); err != nil {
		return 0, err
	}
	if cerr != nil {
		return 0, fmt.Errorf("LOCAL_PEERCRED: %w", cerr)
	}
	return uid, nil
}
