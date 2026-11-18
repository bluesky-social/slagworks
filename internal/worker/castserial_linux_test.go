package worker

import "golang.org/x/sys/unix"

func termiosBaud(t *unix.Termios) int {
	for baud, code := range linuxBaud {
		if t.Cflag&unix.CBAUD == code {
			return baud
		}
	}
	return 0
}
