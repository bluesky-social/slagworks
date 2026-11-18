package worker

import (
	"context"
	"testing"
	"time"
)

func TestVIPMonitorParseIP(t *testing.T) {
	_, err := NewVIPMonitor("10.0.0.1", 0)
	if err != nil {
		t.Fatalf("valid IP should not error: %v", err)
	}

	_, err = NewVIPMonitor("not-an-ip", 0)
	if err == nil {
		t.Fatal("invalid IP should return error")
	}
}

func TestVIPMonitorDetectsLocalIP(t *testing.T) {
	m, err := NewVIPMonitor("127.0.0.1", time.Second)
	if err != nil {
		t.Fatalf("new monitor: %v", err)
	}
	if !m.HasVIP() {
		t.Fatal("expected loopback 127.0.0.1 to be detected as present")
	}
}

func TestVIPMonitorDetectsAbsentIP(t *testing.T) {
	// RFC 5737 TEST-NET-1 — guaranteed not routable/assigned locally
	m, err := NewVIPMonitor("192.0.2.1", time.Second)
	if err != nil {
		t.Fatalf("new monitor: %v", err)
	}
	if m.HasVIP() {
		t.Fatal("expected 192.0.2.1 to not be present on any interface")
	}
}

func TestVIPMonitorRunNotifiesOnAcquire(t *testing.T) {
	m, err := NewVIPMonitor("127.0.0.1", 50*time.Millisecond)
	if err != nil {
		t.Fatalf("new monitor: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch := m.Run(ctx)

	select {
	case acquired, ok := <-ch:
		if !ok {
			t.Fatal("channel closed without sending")
		}
		if !acquired {
			t.Fatal("expected true (VIP acquired) for loopback")
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for VIP notification")
	}

	cancel()

	// Channel should eventually close after context cancellation
	for range ch {
	}
}
