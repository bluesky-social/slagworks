//go:build integration

package qemutest

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// Timing instrumentation for SerialMonitor.WaitFor and
// FakeHub.WaitForObservation. Every call records a single line on
// stderr in a fixed parseable format:
//
//   QEMUTEST_TIMING test=<t.Name()> kind=<serial|obs> what=<marker-or-event> ms=<duration> outcome=<ok|timeout|err>
//
// The workflow's `tee results.txt` captures stderr into the
// uploaded artifact, so a follow-up tool can parse this format and
// derive per-phase budgets.
//
// Format choices:
//   - Single line per event (greppable, awk-able).
//   - Space-separated key=value pairs (no escaping concerns since
//     marker strings are bounded by `=` boundaries; see emit()).
//   - Fixed prefix QEMUTEST_TIMING for cheap filtering against the
//     rest of the test log noise.
//
// Why stderr not t.Logf: t.Logf is per-test scoped and gets
// shuffled in -parallel runs; stderr is a single linear stream.
// We don't lose attribution because each line includes the test
// name (passed in by callers).

// emitTiming writes one timing record to stderr. Concurrent-safe.
func emitTiming(testName, kind, what string, dur time.Duration, outcome string) {
	timingMu.Lock()
	defer timingMu.Unlock()
	fmt.Fprintf(os.Stderr, "QEMUTEST_TIMING test=%s kind=%s what=%s ms=%d outcome=%s\n",
		sanitizeTimingField(testName),
		sanitizeTimingField(kind),
		sanitizeTimingField(what),
		dur.Milliseconds(),
		sanitizeTimingField(outcome),
	)
}

// sanitizeTimingField replaces space and equals in field values
// with '_' so the parseable format stays parseable even if a
// marker string happens to contain them. Markers in the test
// suite today are slogkit envelope substrings (`"msg":"deploy
// complete"`) which can contain spaces; flatten them.
func sanitizeTimingField(s string) string {
	if s == "" {
		return "-"
	}
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case ' ', '\t', '\n', '\r', '=':
			out[i] = '_'
		default:
			out[i] = c
		}
	}
	return string(out)
}

var timingMu sync.Mutex
