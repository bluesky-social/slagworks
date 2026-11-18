package worker

import (
	"fmt"

	"golang.org/x/sys/unix"
)

const (
	ioctlGetTermios = unix.TCGETS
	ioctlSetTermios = unix.TCSETS
)

// linuxBaud maps a baud rate to its termios Bnnn code. TCSETS (not
// TCSETS2/BOTHER) only takes the standard rates, which cover every
// console speed NetBox models.
var linuxBaud = map[int]uint32{
	1200:   unix.B1200,
	2400:   unix.B2400,
	4800:   unix.B4800,
	9600:   unix.B9600,
	19200:  unix.B19200,
	38400:  unix.B38400,
	57600:  unix.B57600,
	115200: unix.B115200,
	230400: unix.B230400,
	460800: unix.B460800,
	921600: unix.B921600,
}

func setTermiosSpeed(t *unix.Termios, baud int) error {
	code, ok := linuxBaud[baud]
	if !ok {
		return fmt.Errorf("unsupported baud rate %d", baud)
	}
	t.Cflag &^= unix.CBAUD
	t.Cflag |= code
	t.Ispeed = code
	t.Ospeed = code
	return nil
}
