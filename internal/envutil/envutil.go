// Package envutil holds small helpers for reading process environment
// variables. It exists so the two command binaries (slagworksd, slagctl)
// share one spelling of "env-var-or-default" instead of each defining a
// private envOr (cmd packages must not import each other).
package envutil

import "os"

// Or returns the value of the environment variable named key, or fallback
// when the variable is unset or empty.
func Or(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
