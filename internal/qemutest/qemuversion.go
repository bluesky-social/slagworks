//go:build integration

package qemutest

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
)

// QEMU 9.2 deprecated the socket chardev's `reconnect=<seconds>` option in
// favour of `reconnect-ms=<milliseconds>`, and QEMU 10 removed the old one:
// a VM started with it exits at once with "Invalid parameter 'reconnect'".
// Ubuntu 24.04 ships QEMU 8.2, which only knows the old option. The harness
// has to run on both, so it asks the installed QEMU which it is.

var qemuVersionRE = regexp.MustCompile(`version (\d+)\.(\d+)`)

// parseQEMUVersion extracts major and minor from `qemu-system-x86_64
// --version` output ("QEMU emulator version 8.2.2 (Debian ...)").
func parseQEMUVersion(out string) (major, minor int, err error) {
	m := qemuVersionRE.FindStringSubmatch(out)
	if m == nil {
		return 0, 0, fmt.Errorf("no version in %q", out)
	}
	major, _ = strconv.Atoi(m[1])
	minor, _ = strconv.Atoi(m[2])
	return major, minor, nil
}

// reconnectOption renders the socket-chardev reconnect option for the given
// QEMU version.
func reconnectOption(major, minor, seconds int) string {
	if major > 9 || (major == 9 && minor >= 2) {
		return fmt.Sprintf("reconnect-ms=%d", seconds*1000)
	}
	return fmt.Sprintf("reconnect=%d", seconds)
}

var installedQEMU = sync.OnceValues(func() ([2]int, error) {
	out, err := exec.Command("qemu-system-x86_64", "--version").Output()
	if err != nil {
		return [2]int{}, fmt.Errorf("qemu-system-x86_64 --version: %w (is QEMU installed?)", err)
	}
	major, minor, err := parseQEMUVersion(string(out))
	return [2]int{major, minor}, err
})

// chardevReconnect returns the reconnect option for the installed QEMU. If
// the version cannot be read it falls back to the old spelling; QEMU's own
// error then names the problem when the VM starts.
func chardevReconnect(seconds int) string {
	v, err := installedQEMU()
	if err != nil {
		return reconnectOption(0, 0, seconds)
	}
	return reconnectOption(v[0], v[1], seconds)
}
