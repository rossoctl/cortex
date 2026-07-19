//go:build linux

package transparentproxy

import (
	"fmt"
	"net"
	"unsafe"

	"golang.org/x/sys/unix"
)

// soOriginalDst is netfilter's SO_ORIGINAL_DST (IPv4, SOL_IP) and
// IP6T_SO_ORIGINAL_DST (IPv6, SOL_IPV6) option number. After an iptables
// REDIRECT/DNAT, conntrack records the pre-NAT destination and exposes it
// to the receiving socket via getsockopt with this option.
const soOriginalDst = 80

// originalDst recovers the pre-REDIRECT destination of a connection accepted on
// a transparent (iptables-REDIRECTed) listener, via the SO_ORIGINAL_DST socket
// option. It tries IPv4 (SOL_IP) first, then IPv6 (SOL_IPV6). Returns the
// destination as "host:port".
//
// This relies on REDIRECT/DNAT (conntrack-backed) — the same mechanism Envoy's
// original_dst listener filter uses — NOT TPROXY (which would instead read the
// socket's local address).
func originalDst(conn *net.TCPConn) (string, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return "", fmt.Errorf("transparentproxy: SyscallConn: %w", err)
	}

	var (
		dst      string
		innerErr error
	)
	ctrlErr := raw.Control(func(fd uintptr) {
		dst, innerErr = getOrigDst(fd)
	})
	if ctrlErr != nil {
		return "", fmt.Errorf("transparentproxy: RawConn.Control: %w", ctrlErr)
	}
	return dst, innerErr
}

// getOrigDst performs the raw getsockopt for SO_ORIGINAL_DST on fd. The buffer
// is sized for sockaddr_in6 (the larger of the two); parseSockaddr reads only
// the bytes the kernel reports via the (in/out) optlen.
func getOrigDst(fd uintptr) (string, error) {
	var buf [unix.SizeofSockaddrInet6]byte
	size := uint32(len(buf))

	// IPv4 (SOL_IP) first — the common case in rossoctl clusters.
	if errno := getsockopt(fd, unix.SOL_IP, soOriginalDst, &buf[0], &size); errno == 0 {
		return parseSockaddr(buf[:size])
	}

	// IPv6 (SOL_IPV6).
	size = uint32(len(buf))
	if errno := getsockopt(fd, unix.SOL_IPV6, soOriginalDst, &buf[0], &size); errno == 0 {
		return parseSockaddr(buf[:size])
	}

	return "", fmt.Errorf("transparentproxy: getsockopt SO_ORIGINAL_DST failed (not a REDIRECTed connection?)")
}

func getsockopt(fd uintptr, level, name int, val *byte, length *uint32) unix.Errno {
	_, _, errno := unix.Syscall6(
		unix.SYS_GETSOCKOPT,
		fd,
		uintptr(level),
		uintptr(name),
		uintptr(unsafe.Pointer(val)),
		uintptr(unsafe.Pointer(length)),
		0,
	)
	return errno
}
