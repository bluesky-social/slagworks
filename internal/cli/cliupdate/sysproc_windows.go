//go:build windows

package cliupdate

import "syscall"

// detachedSysProcAttr returns nil on Windows; Setsid is not available.
// The child will still run with no terminal session on Windows due to the
// DETACHED_PROCESS creation flag that exec.Cmd sets by default on Windows.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return nil
}
