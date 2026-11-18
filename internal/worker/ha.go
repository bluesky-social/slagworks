package worker

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"time"
)

type VIPMonitor struct {
	vip          net.IP
	pollInterval time.Duration
	log          *slog.Logger
}

func NewVIPMonitor(vip string, interval time.Duration) (*VIPMonitor, error) {
	ip := net.ParseIP(vip)
	if ip == nil {
		return nil, fmt.Errorf("invalid VIP address: %s", vip)
	}
	if interval == 0 {
		interval = 2 * time.Second
	}
	return &VIPMonitor{
		vip:          ip,
		pollInterval: interval,
		log:          slog.Default(),
	}, nil
}

func (m *VIPMonitor) SetLogger(l *slog.Logger) {
	m.log = l
}

// HasVIP checks if the VIP is currently assigned to a local interface.
func (m *VIPMonitor) HasVIP() bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		m.log.Warn("failed to list interface addresses", "err", err)
		return false
	}
	for _, addr := range addrs {
		var ip net.IP
		switch v := addr.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip != nil && ip.Equal(m.vip) {
			return true
		}
	}
	return false
}

// Run polls for VIP presence and sends state changes on the returned channel.
// Sends true when VIP is acquired, false when released. Closes channel on
// context cancellation.
func (m *VIPMonitor) Run(ctx context.Context) <-chan bool {
	// Buffer of 2 so both an acquire and release can be queued without
	// blocking the poll loop, avoiding delayed release notifications.
	ch := make(chan bool, 2)
	go func() {
		defer close(ch)
		holding := false

		check := func() {
			has := m.HasVIP()
			if has && !holding {
				holding = true
				m.log.Info("VIP acquired", "vip", m.vip.String())
				select {
				case ch <- true:
				case <-ctx.Done():
				}
			} else if !has && holding {
				holding = false
				m.log.Info("VIP released", "vip", m.vip.String())
				select {
				case ch <- false:
				case <-ctx.Done():
				}
			}
		}

		// Initial check
		check()

		ticker := time.NewTicker(m.pollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				check()
			}
		}
	}()
	return ch
}
