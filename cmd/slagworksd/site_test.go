package main

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/power"
	"github.com/bluesky-social/slagworks/internal/power/smcipmitool"
	"github.com/bluesky-social/slagworks/internal/worker"
)

func loopbackIface() string {
	if runtime.GOOS == "darwin" {
		return "lo0"
	}
	return "lo"
}

func applyDHCP(cache *worker.Cache, cfg model.DHCPConfig, machines ...model.MachineSnapshot) {
	cache.ApplySnapshot(&model.SiteSnapshot{
		SeqNum:   1,
		Site:     model.SiteInfo{Slug: "pop3", Name: "pop3"},
		DHCP:     cfg,
		Machines: machines,
	})
}

func TestBuildDHCPConfig_FromSnapshot(t *testing.T) {
	t.Parallel()
	cache := worker.NewCache(nil)
	applyDHCP(cache, model.DHCPConfig{
		Subnet:             "10.99.1.0/24",
		Gateway:            "10.99.1.1",
		EphemeralPoolStart: "10.99.1.100",
		EphemeralPoolEnd:   "10.99.1.200",
	})

	sc := &siteServiceConfig{
		cfg:   siteConfig{httpPort: 8080, iface: loopbackIface()},
		cache: cache,
		log:   slog.Default(),
	}

	cfg, err := buildDHCPConfig(sc, net.ParseIP("10.99.1.1"))
	if err != nil {
		t.Fatalf("buildDHCPConfig: %v", err)
	}
	if cfg.PoolConfig.Promiscuous {
		t.Error("promiscuous should be false when snapshot sets Promiscuous=false")
	}
	if cfg.RangeStart.String() != "10.99.1.100" {
		t.Errorf("RangeStart = %s, want 10.99.1.100", cfg.RangeStart)
	}
	if cfg.RangeEnd.String() != "10.99.1.200" {
		t.Errorf("RangeEnd = %s, want 10.99.1.200", cfg.RangeEnd)
	}
	if cfg.Gateway.String() != "10.99.1.1" {
		t.Errorf("Gateway = %s, want 10.99.1.1", cfg.Gateway)
	}
	ones, _ := cfg.Subnet.Mask.Size()
	if ones != 24 {
		t.Errorf("Subnet mask = /%d, want /24", ones)
	}
	if cfg.HTTPBootPort != 8080 {
		t.Errorf("HTTPBootPort = %d, want 8080", cfg.HTTPBootPort)
	}
	if cfg.KnownMACs == nil {
		t.Error("KnownMACs filter should be set in non-promiscuous mode")
	}
	if cfg.ReserveIP == nil {
		t.Error("ReserveIP callback should be set")
	}
	if cfg.OnACK == nil {
		t.Error("OnACK callback should be set")
	}
}

func TestBuildDHCPConfig_RelayPools(t *testing.T) {
	t.Parallel()
	cache := worker.NewCache(nil)
	applyDHCP(cache, model.DHCPConfig{
		Subnet:             "10.99.1.0/24",
		Gateway:            "10.99.1.1",
		EphemeralPoolStart: "10.99.1.100",
		EphemeralPoolEnd:   "10.99.1.200",
		RelayPools: []model.RelayPool{
			{Subnet: "10.254.27.0/24", RangeStart: "10.254.27.100", RangeEnd: "10.254.27.150", Gateway: "10.254.27.1"},
			{Subnet: "10.254.26.0/24", RangeStart: "10.254.26.100", RangeEnd: "10.254.26.150", Gateway: "10.254.26.1"},
		},
	})

	sc := &siteServiceConfig{
		cfg:   siteConfig{httpPort: 8080, iface: loopbackIface()},
		cache: cache,
		log:   slog.Default(),
	}

	cfg, err := buildDHCPConfig(sc, net.ParseIP("10.99.1.1"))
	if err != nil {
		t.Fatalf("buildDHCPConfig: %v", err)
	}
	if len(cfg.RelaySubnets) != 2 {
		t.Fatalf("RelaySubnets = %d, want 2", len(cfg.RelaySubnets))
	}
	rs := cfg.RelaySubnets[0]
	if !rs.Subnet.Contains(net.ParseIP("10.254.27.42")) {
		t.Errorf("relay subnet should contain 10.254.27.42, got %s", rs.Subnet.String())
	}
	if rs.RangeStart.String() != "10.254.27.100" || rs.RangeEnd.String() != "10.254.27.150" {
		t.Errorf("relay range = %s-%s, want 10.254.27.100-150", rs.RangeStart, rs.RangeEnd)
	}
	if rs.Gateway.String() != "10.254.27.1" {
		t.Errorf("relay gateway = %s, want 10.254.27.1", rs.Gateway)
	}
}

func TestBuildDHCPConfig_RelayPoolInvalidIPs(t *testing.T) {
	t.Parallel()
	cache := worker.NewCache(nil)
	applyDHCP(cache, model.DHCPConfig{
		Subnet:             "10.99.1.0/24",
		Gateway:            "10.99.1.1",
		EphemeralPoolStart: "10.99.1.100",
		EphemeralPoolEnd:   "10.99.1.200",
		RelayPools: []model.RelayPool{
			{Subnet: "10.254.27.0/24", RangeStart: "garbage", RangeEnd: "10.254.27.150", Gateway: "10.254.27.1"},
		},
	})

	sc := &siteServiceConfig{
		cfg:   siteConfig{httpPort: 8080, iface: loopbackIface()},
		cache: cache,
		log:   slog.Default(),
	}

	if _, err := buildDHCPConfig(sc, net.ParseIP("10.99.1.1")); err == nil {
		t.Fatal("expected error on malformed relay pool IP, got nil")
	}
}

func TestBuildDHCPConfig_EmptySnapshotIsNoOp(t *testing.T) {
	t.Parallel()
	cache := worker.NewCache(nil)

	sc := &siteServiceConfig{
		cfg:   siteConfig{httpPort: 8080, iface: loopbackIface()},
		cache: cache,
		log:   slog.Default(),
	}

	cfg, err := buildDHCPConfig(sc, net.ParseIP("10.50.0.1"))
	if err != nil {
		t.Fatalf("buildDHCPConfig: %v", err)
	}
	if cfg.RangeStart != nil {
		t.Errorf("expected nil RangeStart with no snapshot, got %s", cfg.RangeStart)
	}
}

func TestBuildDHCPConfig_InvalidSubnetCIDR(t *testing.T) {
	t.Parallel()
	cache := worker.NewCache(nil)
	applyDHCP(cache, model.DHCPConfig{
		Subnet:             "not-a-cidr",
		Gateway:            "10.99.1.1",
		EphemeralPoolStart: "10.99.1.100",
		EphemeralPoolEnd:   "10.99.1.200",
	})

	sc := &siteServiceConfig{
		cfg:   siteConfig{httpPort: 8080, iface: loopbackIface()},
		cache: cache,
		log:   slog.Default(),
	}

	_, err := buildDHCPConfig(sc, net.ParseIP("10.0.0.1"))
	if err == nil {
		t.Fatal("expected error for invalid subnet CIDR")
	}
}

func TestBuildDHCPConfig_InvalidIPs(t *testing.T) {
	t.Parallel()
	cache := worker.NewCache(nil)
	applyDHCP(cache, model.DHCPConfig{
		Subnet:             "10.99.1.0/24",
		Gateway:            "10.99.1.1",
		EphemeralPoolStart: "not-an-ip",
		EphemeralPoolEnd:   "10.99.1.200",
	})

	sc := &siteServiceConfig{
		cfg:   siteConfig{httpPort: 8080, iface: loopbackIface()},
		cache: cache,
		log:   slog.Default(),
	}

	_, err := buildDHCPConfig(sc, net.ParseIP("10.0.0.1"))
	if err == nil {
		t.Fatal("expected error for invalid pool IPs")
	}
}

func TestBuildDHCPConfig_PromiscuousMode(t *testing.T) {
	t.Parallel()
	cache := worker.NewCache(nil)
	applyDHCP(cache, model.DHCPConfig{
		Subnet:             "10.99.1.0/24",
		Gateway:            "10.99.1.1",
		EphemeralPoolStart: "10.99.1.100",
		EphemeralPoolEnd:   "10.99.1.200",
		Promiscuous:        true,
	})

	sc := &siteServiceConfig{
		cfg:   siteConfig{httpPort: 8080, iface: loopbackIface()},
		cache: cache,
		log:   slog.Default(),
	}

	cfg, err := buildDHCPConfig(sc, net.ParseIP("10.99.1.1"))
	if err != nil {
		t.Fatalf("buildDHCPConfig: %v", err)
	}
	if !cfg.PoolConfig.Promiscuous {
		t.Error("promiscuous should be true")
	}
	// KnownMACs stays wired even in promiscuous mode (the handler
	// consults the CURRENT pool state per request, so a runtime flip
	// to non-promiscuous re-arms the filter without a restart).
	if cfg.KnownMACs == nil {
		t.Error("KnownMACs should remain wired regardless of promiscuous mode")
	}
}

func TestBuildDHCPConfig_ReserveIPFromSnapshot(t *testing.T) {
	t.Parallel()
	machineID := uuid.New()
	cache := worker.NewCache(nil)
	applyDHCP(cache,
		model.DHCPConfig{
			Subnet:             "10.99.1.0/24",
			Gateway:            "10.99.1.1",
			EphemeralPoolStart: "10.99.1.100",
			EphemeralPoolEnd:   "10.99.1.200",
		},
		model.MachineSnapshot{
			ID:          machineID,
			State:       model.MachineStateNew,
			PXEEligible: true,
			Interfaces: []model.SnapshotInterface{
				{Name: "eth0", MAC: "52:54:00:aa:bb:01", IP: "10.99.1.50/24", Type: "physical"},
			},
		},
	)

	sc := &siteServiceConfig{
		cfg:   siteConfig{httpPort: 8080, iface: loopbackIface()},
		cache: cache,
		log:   slog.Default(),
	}

	cfg, err := buildDHCPConfig(sc, net.ParseIP("10.99.1.1"))
	if err != nil {
		t.Fatalf("buildDHCPConfig: %v", err)
	}
	if cfg.ReserveIP == nil {
		t.Fatal("ReserveIP should be set")
	}

	got := cfg.ReserveIP("52:54:00:aa:bb:01")
	if got == nil || !got.Equal(net.ParseIP("10.99.1.50")) {
		t.Fatalf("expected 10.99.1.50, got %v", got)
	}

	if got := cfg.ReserveIP("52:54:00:ff:ff:ff"); got != nil {
		t.Fatalf("expected nil for unregistered MAC, got %v", got)
	}
}

// fakeSMCLauncher writes a stand-in SMCIPMITool launcher and returns its
// path. The factory only resolves it; nothing here executes it.
func fakeSMCLauncher(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), smcipmitool.DefaultBin)
	if err := os.WriteFile(p, []byte("#!/usr/bin/env bash\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake SMCIPMITool: %v", err)
	}
	return p
}

func supermicroSnapshot() *model.MachineSnapshot {
	return &model.MachineSnapshot{
		ID:           uuid.New(),
		BMCIP:        "192.0.2.9",
		Manufacturer: "supermicro",
		BMCUser:      "ADMIN",
		BMCPass:      "secret",
	}
}

func TestPowerControllerFactory_SupermicroRoutesToSMC(t *testing.T) {
	for _, mode := range []smcipmitool.Mode{smcipmitool.ModeAuto, smcipmitool.ModeRequired} {
		t.Run(string(mode), func(t *testing.T) {
			factory := newPowerControllerFactory(smcipmitool.Tool{Mode: mode, Bin: fakeSMCLauncher(t)}, nil)
			ctrl, err := factory(context.Background(), supermicroSnapshot(), model.BMCRuntime{}, nil, nil)
			if err != nil {
				t.Fatalf("factory: %v", err)
			}
			defer ctrl.Close(context.Background())
			if _, ok := ctrl.(*power.SMCController); !ok {
				t.Fatalf("expected *power.SMCController with the tool installed, got %T", ctrl)
			}
			if got := ctrl.Transport(); got != "smcipmitool" {
				t.Fatalf("transport = %q, want smcipmitool", got)
			}
		})
	}
}

// In required mode a Supermicro host with the tool missing is a hard error.
// The error path returns before any NewBMCController/bmclib Open
// round-trip, so the factory can be driven to completion here.
func TestPowerControllerFactory_SupermicroRequiredMissingErrors(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	factory := newPowerControllerFactory(smcipmitool.Tool{Mode: smcipmitool.ModeRequired}, nil)
	ctrl, err := factory(context.Background(), supermicroSnapshot(), model.BMCRuntime{}, nil, nil)
	if err == nil {
		ctrl.Close(context.Background())
		t.Fatal("expected an error for a supermicro host with the tool missing in required mode")
	}
	for _, want := range []string{"SMCIPMITool", smcipmitool.InstallDoc} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

// The auto-mode-missing and off-mode fallbacks to bmclib are not driven to
// completion here: that path constructs a BMCController, whose
// NewBMCController performs a real bmclib Open round-trip against the BMC
// IP, unreachable and slow from a unit test. The branch decision itself is
// covered by smcipmitool's Tool.Lookup tests (off and missing both report
// ok=false), and buildPowerController only returns the error above in
// required mode.

func TestSMCToolWarner(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	t.Setenv("PATH", t.TempDir())
	count := 2
	warn := newSMCToolWarner(smcipmitool.Tool{Mode: smcipmitool.ModeAuto}, func() int { return count }, log)

	warn()
	if !strings.Contains(buf.String(), "SMCIPMITool is not installed") || !strings.Contains(buf.String(), smcipmitool.InstallDoc) {
		t.Fatalf("first snapshot should warn with the install doc, got: %q", buf.String())
	}

	buf.Reset()
	warn()
	if buf.Len() != 0 {
		t.Fatalf("an unchanged snapshot must not repeat the warning, got: %q", buf.String())
	}

	buf.Reset()
	count = 3
	warn()
	if !strings.Contains(buf.String(), "supermicro_machines=3") {
		t.Fatalf("a changed Supermicro count should re-warn, got: %q", buf.String())
	}

	buf.Reset()
	count = 0
	warn()
	if buf.Len() != 0 {
		t.Fatalf("a site with no Supermicro hosts must not warn, got: %q", buf.String())
	}
}

func TestSMCToolWarner_InstalledAndOff(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	bin := fakeSMCLauncher(t)

	newSMCToolWarner(smcipmitool.Tool{Mode: smcipmitool.ModeAuto, Bin: bin}, func() int { return 1 }, log)()
	if !strings.Contains(buf.String(), "SMCIPMITool found") {
		t.Fatalf("installed tool should log that it was found, got: %q", buf.String())
	}

	buf.Reset()
	newSMCToolWarner(smcipmitool.Tool{Mode: smcipmitool.ModeOff, Bin: bin}, func() int { return 1 }, log)()
	if buf.Len() != 0 {
		t.Fatalf("off mode is a deliberate operator choice and must not warn, got: %q", buf.String())
	}
}
