package bootstrap

import (
	"fmt"
	"net"
	"os"
)

// NotifyReady tells systemd that the service is ready, with sd_notify's READY=1 sent to
// the datagram socket NOTIFY_SOCKET names. A name that starts with @ is an abstract
// socket. Without NOTIFY_SOCKET (launchd, a container) it does nothing.
func NotifyReady() error {
	name := os.Getenv("NOTIFY_SOCKET")
	if name == "" {
		return nil
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: name, Net: "unixgram"})
	if err != nil {
		return fmt.Errorf("sd_notify: %w", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("READY=1")); err != nil {
		return fmt.Errorf("sd_notify: %w", err)
	}
	return nil
}
