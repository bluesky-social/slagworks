package main

import (
	"bufio"
	"log/slog"
	"os"
	"sync"
	"time"
)

type logLine struct {
	Source    string `json:"source"`
	Timestamp string `json:"ts"`
	Line      string `json:"line"`
}

type logCollector struct {
	mu      sync.Mutex
	buf     []logLine
	flushCh chan struct{}
}

const flushThreshold = 50

func newLogCollector() *logCollector {
	return &logCollector{
		flushCh: make(chan struct{}, 1),
	}
}

func (lc *logCollector) Append(source, line string) {
	lc.mu.Lock()
	lc.buf = append(lc.buf, logLine{
		Source:    source,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Line:      line,
	})
	shouldFlush := len(lc.buf) >= flushThreshold
	lc.mu.Unlock()

	if shouldFlush {
		select {
		case lc.flushCh <- struct{}{}:
		default:
		}
	}
}

func (lc *logCollector) Drain() []logLine {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	lines := lc.buf
	lc.buf = nil
	return lines
}

func (lc *logCollector) FlushCh() <-chan struct{} {
	return lc.flushCh
}

func (lc *logCollector) Len() int {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	return len(lc.buf)
}

// startDmesgReader reads kernel messages from /proc/kmsg and appends
// them to the collector. Returns immediately if /proc/kmsg is not available.
func startDmesgReader(log *slog.Logger, collector *logCollector, done <-chan struct{}) {
	f, err := os.Open("/proc/kmsg")
	if err != nil {
		log.Warn("cannot open /proc/kmsg (dmesg capture disabled)", "err", err)
		return
	}

	go func() {
		<-done
		f.Close()
	}()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		collector.Append("dmesg", scanner.Text())
	}
}
