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
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/bluesky-social/slagworks/internal/dhcp"
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
	if r := sc.fabricPoolErr.Load(); r != nil {
		t.Errorf("fabricPoolErr = %q, want unset for a parseable config", *r)
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

// validDHCP is a fabric DHCP config buildPoolConfig accepts.
var validDHCP = model.DHCPConfig{
	Subnet:             "10.99.1.0/24",
	Gateway:            "10.99.1.1",
	EphemeralPoolStart: "10.99.1.100",
	EphemeralPoolEnd:   "10.99.1.200",
}

func withDHCP(edit func(*model.DHCPConfig)) model.DHCPConfig {
	c := validDHCP
	edit(&c)
	return c
}

// A snapshot DHCP config that does not parse must not fail the service
// start: the snapshot is persisted and global re-serves it, so an error
// here would fail every restart. The handler starts with no pool (as on a
// cold cache), readiness gets the reason, and everything else is wired.
func TestBuildDHCPConfig_UnparseablePoolStartsWithoutPool(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		dhcp model.DHCPConfig
		want string // in the readiness reason
	}{
		{"gateway_ip with its mask", withDHCP(func(c *model.DHCPConfig) { c.Gateway = "10.99.1.1/24" }), `gw="10.99.1.1/24"`},
		{"gateway_ip empty", withDHCP(func(c *model.DHCPConfig) { c.Gateway = "" }), `gw=""`},
		{"pool start not an IP", withDHCP(func(c *model.DHCPConfig) { c.EphemeralPoolStart = "not-an-ip" }), `start="not-an-ip"`},
		{"subnet not a CIDR", withDHCP(func(c *model.DHCPConfig) { c.Subnet = "not-a-cidr" }), `"not-a-cidr"`},
		{"relay pool IP not an IP", withDHCP(func(c *model.DHCPConfig) {
			c.RelayPools = []model.RelayPool{
				{Subnet: "10.254.27.0/24", RangeStart: "garbage", RangeEnd: "10.254.27.150", Gateway: "10.254.27.1"},
			}
		}), `start="garbage"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cache := worker.NewCache(nil)
			applyDHCP(cache, tc.dhcp)
			sc := &siteServiceConfig{
				cfg:   siteConfig{httpPort: 8080, iface: loopbackIface()},
				cache: cache,
				log:   slog.Default(),
			}

			cfg, err := buildDHCPConfig(sc, net.ParseIP("10.99.1.1"))
			if err != nil {
				t.Fatalf("buildDHCPConfig: %v; an unparseable snapshot DHCP config must not fail the start", err)
			}
			if cfg.RangeStart != nil || cfg.Gateway != nil || len(cfg.RelaySubnets) != 0 {
				t.Errorf("PoolConfig = %+v, want empty (no pool from a config that does not parse)", cfg.PoolConfig)
			}
			if cfg.KnownMACs == nil || cfg.ReserveIP == nil || cfg.OnACK == nil {
				t.Error("callbacks must stay wired so pools installed by hot reload are filtered and reported")
			}
			reason := sc.fabricPoolErr.Load()
			if reason == nil {
				t.Fatal("fabricPoolErr unset; readiness must report a fabric leg serving no pool")
			}
			for _, want := range []string{tc.want, "gateway_ip"} {
				if !strings.Contains(*reason, want) {
					t.Errorf("readiness reason %q should mention %q", *reason, want)
				}
			}
		})
	}
}

// dns_servers is a comma-separated free-text NetBox field. An entry that
// is not an IP is dropped, not fatal; with none left the default stays.
func TestBuildDHCPConfig_DNSServers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		dns  []string
		want []string
	}{
		{"all valid", []string{"10.99.1.2", "10.99.1.3"}, []string{"10.99.1.2", "10.99.1.3"}},
		{"one unparseable entry dropped", []string{"10.99.1.2", "10.99.1.3; 10.99.1.4"}, []string{"10.99.1.2"}},
		{"none parse keeps the default", []string{"10.99.1.3; 10.99.1.4"}, []string{"1.1.1.1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cache := worker.NewCache(nil)
			applyDHCP(cache, withDHCP(func(c *model.DHCPConfig) { c.DNS = tc.dns }))
			sc := &siteServiceConfig{
				cfg:   siteConfig{httpPort: 8080, iface: loopbackIface()},
				cache: cache,
				log:   slog.Default(),
			}

			cfg, err := buildDHCPConfig(sc, net.ParseIP("10.99.1.1"))
			if err != nil {
				t.Fatalf("buildDHCPConfig: %v", err)
			}
			var got []string
			for _, ip := range cfg.DNS {
				got = append(got, ip.String())
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("DNS = %v, want %v", got, tc.want)
			}
			if cfg.RangeStart == nil {
				t.Error("a bad dns_servers entry must not cost the pool")
			}
			if r := sc.fabricPoolErr.Load(); r != nil {
				t.Errorf("fabricPoolErr = %q, want unset: the pool is served", *r)
			}
		})
	}
}

// A worker that started without a pool picks one up from the first
// snapshot that parses, and readiness clears; a later unparseable config
// keeps the installed pools (stale-but-working) without flagging readiness.
func TestReloadFabricPools_RecoversFromUnparseableStart(t *testing.T) {
	t.Parallel()
	cache := worker.NewCache(nil)
	seq := int64(0)
	apply := func(c model.DHCPConfig) {
		seq++
		cache.ApplySnapshot(&model.SiteSnapshot{SeqNum: seq, Site: model.SiteInfo{Slug: "site-a", Name: "site-a"}, DHCP: c})
	}
	apply(withDHCP(func(c *model.DHCPConfig) { c.Gateway = "10.99.1.1/24" }))

	metrics := dhcp.NewMetrics(prometheus.NewRegistry())
	sc := &siteServiceConfig{
		cfg:         siteConfig{httpPort: 8080, iface: loopbackIface()},
		cache:       cache,
		log:         slog.Default(),
		dhcpMetrics: metrics,
	}
	cfg, err := buildDHCPConfig(sc, net.ParseIP("10.99.1.1"))
	if err != nil {
		t.Fatalf("buildDHCPConfig: %v", err)
	}
	h := dhcp.NewHandler(cfg)
	cache.OnSnapshotApplied(func() { reloadFabricPools(sc, h) })
	poolSeries := func() int { return testutil.CollectAndCount(metrics.PoolSize) }
	if n := poolSeries(); n != 0 {
		t.Fatalf("pool series at start = %d, want 0", n)
	}

	// Still unparseable, differently: the reason follows the latest config.
	apply(withDHCP(func(c *model.DHCPConfig) { c.Gateway = "" }))
	if r := sc.fabricPoolErr.Load(); r == nil || !strings.Contains(*r, `gw=""`) {
		t.Fatalf("fabricPoolErr = %v, want the latest config's error", r)
	}

	apply(validDHCP)
	if r := sc.fabricPoolErr.Load(); r != nil {
		t.Fatalf("fabricPoolErr = %q after a parseable snapshot, want unset", *r)
	}
	if got := testutil.ToFloat64(metrics.PoolSize.WithLabelValues("10.99.1.0/24")); got != 101 {
		t.Fatalf("pool size = %v, want 101 once hot reload installed the pool", got)
	}

	apply(withDHCP(func(c *model.DHCPConfig) { c.Gateway = "10.99.1.1/24" }))
	if r := sc.fabricPoolErr.Load(); r != nil {
		t.Errorf("fabricPoolErr = %q, want unset: the previous pools are still served", *r)
	}
	if got := testutil.ToFloat64(metrics.PoolSize.WithLabelValues("10.99.1.0/24")); got != 101 {
		t.Errorf("pool size = %v, want the previous pool kept", got)
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

// The fabric resolvers come from a free-text NetBox custom field
// (dns_servers on the slagworks-production prefix) that global copies into
// the snapshot unvalidated. An entry that is not an IP address must cost
// that entry, as it does on the OOB segment (parseIPs), not the site: the
// worker reads these at service start only, so a failure would not show
// when the field is edited but at the worker's next restart or VIP
// takeover, as a start-up error that systemd repeats until the field is
// fixed. The dropped entry is logged at error level, naming the field.
func TestBuildDHCPConfig_MalformedDNSEntryCostsThatEntryNotTheSite(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		dns  []string
		want []string
		bad  []string
	}{
		{
			// "10.99.1.2, 10.99.1.3 10.99.1.4" in NetBox: the second
			// separator is a space, so the builder's comma split yields one
			// bad entry.
			name: "bad entry dropped, good one kept",
			dns:  []string{"10.99.1.2", "10.99.1.3 10.99.1.4"},
			want: []string{"10.99.1.2"},
			bad:  []string{"10.99.1.3 10.99.1.4"},
		},
		{
			name: "no good entry keeps the default",
			dns:  []string{"resolver.example", "10.99.1.3/32"},
			want: []string{"1.1.1.1"},
			bad:  []string{"resolver.example", "10.99.1.3/32"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cache := worker.NewCache(nil)
			applyDHCP(cache, model.DHCPConfig{
				Subnet:             "10.99.1.0/24",
				Gateway:            "10.99.1.1",
				EphemeralPoolStart: "10.99.1.100",
				EphemeralPoolEnd:   "10.99.1.200",
				DNS:                tt.dns,
			})
			var logs bytes.Buffer
			sc := &siteServiceConfig{
				cfg:   siteConfig{httpPort: 8080, iface: loopbackIface()},
				cache: cache,
				log:   slog.New(slog.NewTextHandler(&logs, nil)),
			}
			cfg, err := buildDHCPConfig(sc, net.ParseIP("10.99.1.1"))
			if err != nil {
				t.Fatalf("a malformed dns_servers entry stops the site's services from starting: %v", err)
			}
			var got []string
			for _, ip := range cfg.DNS {
				got = append(got, ip.String())
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("DNS = %v, want %v", got, tt.want)
			}
			out := logs.String()
			for _, b := range tt.bad {
				if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "dns_servers") || !strings.Contains(out, b) {
					t.Errorf("no error-level log naming dns_servers and the dropped entry %q; logs:\n%s", b, out)
				}
			}
		})
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

// TestBuildDHCPConfig_OnACKIgnoresTheBootedEnvironmentsDHCP: a booting host
// DHCPs three times, not two. The firmware's exchange is the dhcp_ack phase
// and slagworks iPXE's is ipxe_dhcp (docs/dev/state-machine.md, "Phase write
// sites"). Then, after ipxe_loaded, the boot script's ip=dhcp makes the
// deploy or commission environment's own client (dhcpcd in the initrd) ask
// again, and again at every renewal. That client is neither boot stage, so
// its ACKs must leave the op's phase alone: writing dhcp_ack there runs the
// phase backwards on every deploy, and an op that then dies before the
// agent's first report times out labelled dhcp_ack, which points the
// operator at PXE for a host that had already booted Linux.
func TestBuildDHCPConfig_OnACKIgnoresTheBootedEnvironmentsDHCP(t *testing.T) {
	t.Parallel()
	const mac = "52:54:00:aa:bb:07"
	machineID := uuid.New()
	cache := worker.NewCache(nil)
	applyDHCP(cache, model.DHCPConfig{
		Subnet:             "10.99.1.0/24",
		Gateway:            "10.99.1.1",
		EphemeralPoolStart: "10.99.1.100",
		EphemeralPoolEnd:   "10.99.1.200",
	}, model.MachineSnapshot{
		ID: machineID, State: model.MachineStateCommissioned, PXEEligible: true,
		Interfaces: []model.SnapshotInterface{{Name: "eth0", MAC: mac, Type: "physical"}},
	})
	cache.ApplyOpResync(nil, nil)
	op := model.Operation{ID: uuid.New(), MachineID: machineID, Type: model.OpDeploy, Status: model.OpClaimed, Version: 1}
	cache.ApplyOpDelta(op, model.MachineStateDeploying)

	sc := &siteServiceConfig{
		cfg:   siteConfig{httpPort: 8080, iface: loopbackIface()},
		cache: cache,
		log:   slog.Default(),
	}
	cfg, err := buildDHCPConfig(sc, net.ParseIP("10.99.1.1"))
	if err != nil {
		t.Fatalf("buildDHCPConfig: %v", err)
	}
	opID := op.ID.String()
	phaseOf := func() string { return cache.GetOperation(opID).Phase }

	// The firmware's and iPXE's exchanges advance the boot prefix.
	cache.UpdateOpPhase(opID, "pxe_booting")
	cfg.OnACK(mac, dhcp.ClientFirmware)
	if got := phaseOf(); got != "dhcp_ack" {
		t.Fatalf("firmware ACK at pxe_booting: phase %q, want dhcp_ack", got)
	}
	cfg.OnACK(mac, dhcp.ClientIPXE)
	if got := phaseOf(); got != "ipxe_dhcp" {
		t.Fatalf("iPXE ACK at dhcp_ack: phase %q, want ipxe_dhcp", got)
	}

	// The environment's own exchange, and each renewal, arrives later.
	for _, phase := range []string{"ipxe_loaded", "agent_starting", "rsyncing_os", "finalizing"} {
		cache.UpdateOpPhase(opID, phase)
		cfg.OnACK(mac, dhcp.ClientOther)
		if got := phaseOf(); got != phase {
			t.Errorf("environment DHCP ACK at %s moved the phase to %q", phase, got)
		}
	}

	// A host the firmware re-PXEs mid-install (a reset or power loss) really
	// is back at the firmware's DHCP, and the phase says so.
	cache.UpdateOpPhase(opID, "rsyncing_os")
	cfg.OnACK(mac, dhcp.ClientFirmware)
	if got := phaseOf(); got != "dhcp_ack" {
		t.Errorf("firmware ACK at rsyncing_os: phase %q, want dhcp_ack", got)
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
