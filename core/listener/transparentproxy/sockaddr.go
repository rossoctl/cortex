package transparentproxy

import (
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
)

// Linux address-family values. These are hardcoded rather than taken from
// golang.org/x/sys/unix because the sockaddr we parse always originates from
// a Linux kernel getsockopt (SO_ORIGINAL_DST), even when this code is compiled
// for a non-Linux dev host where AF_INET6 has a different numeric value
// (Darwin uses 30). Keeping them local makes parseSockaddr pure and lets its
// test run on any platform.
const (
	afInet  = 2  // AF_INET
	afInet6 = 10 // AF_INET6 on Linux
)

// parseSockaddr decodes a Linux sockaddr_in / sockaddr_in6 (as returned by the
// SO_ORIGINAL_DST getsockopt) into a "host:port" string. The buffer layout is:
//
//	sockaddr_in   : family[0:2] port[2:4](BE) addr[4:8]
//	sockaddr_in6  : family[0:2] port[2:4](BE) flowinfo[4:8] addr[8:24] scope[24:28]
//
// sa_family is in host byte order; the port is network byte order (big-endian).
// It is deliberately syscall-free so it is unit-testable on any OS.
func parseSockaddr(b []byte) (string, error) {
	if len(b) < 4 {
		return "", fmt.Errorf("transparentproxy: short sockaddr (%d bytes)", len(b))
	}
	family := binary.NativeEndian.Uint16(b[0:2])
	port := int(binary.BigEndian.Uint16(b[2:4]))

	switch family {
	case afInet:
		if len(b) < 8 {
			return "", fmt.Errorf("transparentproxy: short sockaddr_in (%d bytes)", len(b))
		}
		ip := net.IPv4(b[4], b[5], b[6], b[7])
		return net.JoinHostPort(ip.String(), strconv.Itoa(port)), nil
	case afInet6:
		if len(b) < 24 {
			return "", fmt.Errorf("transparentproxy: short sockaddr_in6 (%d bytes)", len(b))
		}
		ip := make(net.IP, net.IPv6len)
		copy(ip, b[8:24])
		return net.JoinHostPort(ip.String(), strconv.Itoa(port)), nil
	default:
		return "", fmt.Errorf("transparentproxy: unexpected sockaddr family %d", family)
	}
}
