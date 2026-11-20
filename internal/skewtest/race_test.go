//go:build race

package skewtest

// A race-enabled test binary builds the driver with -race too (see
// TestMain).
func init() { raceBuild = true }
