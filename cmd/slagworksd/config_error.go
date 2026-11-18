package main

import (
	"errors"
	"fmt"
)

// exitConfig is sysexits(3) EX_CONFIG. Both systemd units set
// RestartPreventExitStatus=78, so exiting with it marks the unit
// failed-permanent instead of restart-looping: missing or malformed
// configuration cannot heal by retrying. Anything else exits 1 and
// restarts normally (DB down, network blips, etc.).
const exitConfig = 78

// configError marks an error as a permanent configuration problem
// (missing env var, unparseable value, absent required flag).
type configError struct{ err error }

func (e configError) Error() string { return e.err.Error() }
func (e configError) Unwrap() error { return e.err }

func configErrorf(format string, args ...any) error {
	return configError{fmt.Errorf(format, args...)}
}

func exitCodeFor(err error) int {
	if _, ok := errors.AsType[configError](err); ok {
		return exitConfig
	}
	return 1
}
