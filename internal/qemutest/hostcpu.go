//go:build integration

package qemutest

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// firstAllowedCPU returns the lowest-numbered host CPU the test process may
// run on, as taskset -c takes it. A CPU outside the process's own affinity
// mask may be offline or forbidden by a cgroup cpuset, so 0 is not a safe
// guess.
func firstAllowedCPU() (string, error) {
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return "", fmt.Errorf("reading the CPU affinity list: %w", err)
	}
	for _, line := range strings.Split(string(status), "\n") {
		if list, ok := strings.CutPrefix(line, "Cpus_allowed_list:"); ok {
			return firstCPUInList(list)
		}
	}
	return "", fmt.Errorf("/proc/self/status has no Cpus_allowed_list line")
}

// firstCPUInList returns the first CPU of a kernel CPU list such as
// "0-3,8-11" or "5".
func firstCPUInList(list string) (string, error) {
	first := strings.TrimSpace(list)
	if i := strings.IndexAny(first, ",-"); i >= 0 {
		first = first[:i]
	}
	if _, err := strconv.ParseUint(first, 10, 32); err != nil {
		return "", fmt.Errorf("CPU list %q does not start with a CPU number", strings.TrimSpace(list))
	}
	return first, nil
}
