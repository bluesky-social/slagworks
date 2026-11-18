package worker

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// Darwin support exists so the serial dialer's tests run on developer
// laptops against a pty pair; production site workers are Linux.
const (
	ioctlGetTermios = unix.TIOCGETA
	ioctlSetTermios = unix.TIOCSETA
)

func setTermiosSpeed(t *unix.Termios, baud int) error {
	if baud <= 0 {
		return fmt.Errorf("unsupported baud rate %d", baud)
	}
	t.Ispeed = uint64(baud)
	t.Ospeed = uint64(baud)
	return nil
}
