package api

import (
	"runtime"
	"testing"
	"time"
)

// TestRenderCastScreen_NoGoroutineLeak guards that the snapshot renderer is
// goroutine-free. It suppresses DSR query responses at the source (no-op CSI
// handlers) rather than draining the emulator's response pipe with a background
// goroutine, so there is no goroutine to leak and no Close()/e.closed race.
// Render many DSR-bearing casts and assert the goroutine count stays flat.
func TestRenderCastScreen_NoGoroutineLeak(t *testing.T) {
	chunks := [][]byte{[]byte("# \x1b[6n\x1b[2J\x1b[1;1Hbooted\x1b[6n")}
	_ = renderCastScreenANSI(chunks, 80, 24) // warm up
	time.Sleep(50 * time.Millisecond)
	runtime.GC()
	before := runtime.NumGoroutine()

	for i := range 200 {
		if f := renderCastScreenANSI(chunks, 80, 24); len(f) == 0 {
			t.Fatalf("iter %d: empty frame", i)
		}
	}
	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	after := runtime.NumGoroutine()
	t.Logf("goroutines before=%d after=%d (200 renders)", before, after)
	if after > before+2 {
		t.Errorf("snapshot render must not spawn goroutines: before=%d after=%d", before, after)
	}
}
