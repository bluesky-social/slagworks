package worker

import "golang.org/x/sys/unix"

func termiosBaud(t *unix.Termios) int { return int(t.Ospeed) }
