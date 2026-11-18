//go:build integration

package qemutest

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// SerialMonitor reads one or more VM serial console streams and
// lets tests wait for marker strings. Reads happen on background
// goroutines; Lines() and WaitFor() are safe to call concurrently.
//
// Multiple readers are supported because ttyS0 and ttyS1 carry
// different stages of the boot: ttyS0 has iPXE/SeaBIOS output
// (firmware-side), ttyS1 has the kernel + agent (driven by the
// boot script's `console=ttyS1,115200n8` last). Both are merged
// into a single line-ordered stream so existing tests don't need
// to know which port emitted which marker.
type SerialMonitor struct {
	lines    []string
	mu       sync.Mutex
	notify   chan struct{}
	done     chan struct{}
	wg       sync.WaitGroup
	logFunc  func(string)
	testName string // for QEMUTEST_TIMING records
}

func newSerialMonitor(testName string, logFunc func(string), readers ...io.Reader) *SerialMonitor {
	m := &SerialMonitor{
		notify:   make(chan struct{}, 1),
		done:     make(chan struct{}),
		logFunc:  logFunc,
		testName: testName,
	}
	for _, r := range readers {
		m.wg.Add(1)
		go m.consume(r)
	}
	go func() {
		m.wg.Wait()
		close(m.done)
	}()
	return m
}

func (m *SerialMonitor) consume(r io.Reader) {
	defer m.wg.Done()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if m.logFunc != nil {
			m.logFunc(line)
		}
		m.mu.Lock()
		m.lines = append(m.lines, line)
		m.mu.Unlock()
		select {
		case m.notify <- struct{}{}:
		default:
		}
	}
}

// WaitFor blocks until a line containing marker appears in the serial
// output, or timeout/ctx expires. Emits a QEMUTEST_TIMING record
// to stderr on each call (ok / timeout / cancelled).
func (m *SerialMonitor) WaitFor(ctx context.Context, marker string, timeout time.Duration) error {
	start := time.Now()
	err := m.waitForInner(ctx, marker, timeout)
	outcome := "ok"
	switch {
	case err == nil:
	case ctx.Err() != nil:
		outcome = "cancelled"
	case strings.Contains(err.Error(), "stream ended"):
		// EOF is the VM dying, not a slow marker — labeling it
		// "timeout" would skew any budget analysis built on the
		// QEMUTEST_TIMING records.
		outcome = "eof"
	default:
		outcome = "timeout"
	}
	emitTiming(m.testName, "serial", marker, time.Since(start), outcome)
	return err
}

func (m *SerialMonitor) waitForInner(ctx context.Context, marker string, timeout time.Duration) error {
	return m.waitForFrom(ctx, 0, marker, timeout)
}

// waitForFrom is waitForInner scoped to a starting cursor index: only
// lines at index >= from are considered, so a caller can assert that a
// marker appears AFTER some prior point in the stream.
func (m *SerialMonitor) waitForFrom(ctx context.Context, from int, marker string, timeout time.Duration) error {
	deadline := time.After(timeout)
	seen := from
	if seen < 0 {
		seen = 0
	}
	for {
		m.mu.Lock()
		for ; seen < len(m.lines); seen++ {
			if strings.Contains(m.lines[seen], marker) {
				m.mu.Unlock()
				return nil
			}
		}
		m.mu.Unlock()

		select {
		case <-deadline:
			return fmt.Errorf("timeout (%s) waiting for serial marker %q (saw %d lines)", timeout, marker, seen)
		case <-ctx.Done():
			return fmt.Errorf("context cancelled waiting for serial marker %q: %w", marker, ctx.Err())
		case <-m.done:
			m.mu.Lock()
			for ; seen < len(m.lines); seen++ {
				if strings.Contains(m.lines[seen], marker) {
					m.mu.Unlock()
					return nil
				}
			}
			m.mu.Unlock()
			return fmt.Errorf("serial stream ended without seeing marker %q", marker)
		case <-m.notify:
			// loop and recheck
		}
	}
}

// Lines returns a snapshot of everything captured so far.
func (m *SerialMonitor) Lines() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.lines))
	copy(out, m.lines)
	return out
}

// Contains reports whether needle is present in any captured line.
func (m *SerialMonitor) Contains(needle string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, l := range m.lines {
		if strings.Contains(l, needle) {
			return true
		}
	}
	return false
}

// Cursor returns a snapshot of the number of lines captured so far. A
// cursor captured BEFORE an operation can be passed to WaitForAfter to
// require that a marker appears strictly after that point — so a
// post-reboot assertion can't be satisfied by a marker that was already
// present before the reboot.
func (m *SerialMonitor) Cursor() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.lines)
}

// WaitForAfter blocks until a line containing marker appears at an index
// >= cursor, or timeout/ctx expires. Errors match WaitFor's contract.
func (m *SerialMonitor) WaitForAfter(ctx context.Context, cursor int, marker string, timeout time.Duration) error {
	start := time.Now()
	err := m.waitForFrom(ctx, cursor, marker, timeout)
	outcome := "ok"
	switch {
	case err == nil:
	case ctx.Err() != nil:
		outcome = "cancelled"
	case strings.Contains(err.Error(), "stream ended"):
		outcome = "eof"
	default:
		outcome = "timeout"
	}
	emitTiming(m.testName, "serial-after", marker, time.Since(start), outcome)
	return err
}
