// Package clierr attaches a process exit code to an error returned from a
// cobra command. Cobra has no exit-code convention; slagctl's main exits with
// the code this package carries.
//
// Usage:
//
//	return clierr.WithExit(errors.New("bad arg"), 2)
//
// In main():
//
//	code, _ := clierr.Code(runErr)
//	os.Exit(code)
package clierr

import "errors"

// exitError wraps an error together with an integer exit code.
type exitError struct {
	cause error
	code  int
}

// Error implements the error interface.
func (e *exitError) Error() string {
	return e.cause.Error()
}

// Unwrap lets errors.Is/As traverse the chain through the wrapper.
func (e *exitError) Unwrap() error {
	return e.cause
}

// WithExit wraps err with an exit code. If err is nil, returns nil.
// The returned error implements Unwrap() so errors.Is/As work on the
// full chain. Multiple WithExit wrappers may be stacked; Code returns
// the outermost wrapper's code (the first found by errors.AsType).
func WithExit(err error, code int) error {
	if err == nil {
		return nil
	}
	return &exitError{cause: err, code: code}
}

// Code returns the exit code carried by the outermost exitError in err's
// chain (the first found via errors.AsType). Returns (0, false) for nil;
// (code, true) if a WithExit wrapper is found; (1, false) for any other
// non-nil error.
func Code(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	if ee, ok := errors.AsType[*exitError](err); ok {
		return ee.code, true
	}
	return 1, false
}
