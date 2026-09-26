//go:build !linux

package transparentproxy

import (
	"fmt"
	"net"
)

// originalDst is unsupported off Linux. SO_ORIGINAL_DST is a netfilter feature;
// the transparent listener only runs in-cluster (Linux). This stub keeps the
// proxy binary buildable on dev hosts (e.g. macOS).
func originalDst(_ *net.TCPConn) (string, error) {
	return "", fmt.Errorf("transparentproxy: SO_ORIGINAL_DST is only supported on Linux")
}
