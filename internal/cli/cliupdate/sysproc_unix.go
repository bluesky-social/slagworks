//go:build !windows

package cliupdate

import "syscall"

// detachedSysProcAttr returns a SysProcAttr that starts the child in a new
// session (Setsid), so a SIGINT to the parent's process group doesn't kill it
// and it outlives any terminal session.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
