package tlssniff

import "time"

// SetPeekTimeout sets how long l waits for a connection's first byte.
func SetPeekTimeout(l *Listener, d time.Duration) { l.peekTimeout = d }
