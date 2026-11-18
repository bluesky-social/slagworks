// Package sdnotify implements the systemd sd_notify protocol.
// It is a no-op when not running under systemd (NOTIFY_SOCKET not set).
package sdnotify

import (
	"net"
	"os"
)

const (
	Ready    = "READY=1"
	Watchdog = "WATCHDOG=1"
)

// Send sends a notification to systemd via NOTIFY_SOCKET.
// Returns silently if NOTIFY_SOCKET is not set.
func Send(state string) error {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return nil
	}
	// Abstract namespace sockets are prefixed with '@'.
	if addr[0] == '@' {
		addr = "\x00" + addr[1:]
	}
	conn, err := net.Dial("unixgram", addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_, err = conn.Write([]byte(state))
	return err
}
