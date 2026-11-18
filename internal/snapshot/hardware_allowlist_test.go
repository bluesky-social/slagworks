package snapshot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bluesky-social/slagworks/internal/netbox"
	"github.com/bluesky-social/slagworks/internal/storage"
)

// TestHardwareAllowlist is a fail-closed census guard: it
// asserts that across EVERY hardware class in the fleet, the ONLY hosts any
// shipped config script (network OR storage) matches are the cohorts we have
// explicitly chosen to deploy slagworks against:
//
//	kworker-*, hot-spare-*, oob-gw-*, leaf-*, spine-*, cpu-*.pop3
//
// (cpu-*.pop3 are the original pop3 ASUS RS501A validation boxes — a legitimate
// deploy cohort, operator-confirmed 2026-06-21.)
//
// Everything else — the production servers that share a chassis with kworkers
// (nova/algo/inferno/relay/router/search/vortex/appview/...), PDUs, patch
// panels, the SN2410 leaves, the pop3 ASUS/Minisforum boxes — MUST match
// nothing. A storage or network script that broadens to catch one of these is
// a defense-in-depth breach: slagworks could be pointed at a host that should
// never be reimaged.
//
// This is INTENTIONALLY a maintenance burden: onboarding a new hardware cohort
// to slagworks requires editing this census (add the cohort as allowed, or its
// hardware as a tracked deny entry). That edit is the gate — it forces a
// human decision that "yes, slagworks may now image this class".
//
// The census below mirrors the live NetBox fleet (pop1/pop2/pop3, surveyed
// 2026-06-21). If NetBox grows a hardware class not represented here, this test
// won't know about it — so it pairs with the per-cohort positive coverage in
// the collective match tests, and should be refreshed when the fleet changes.

// censusHost is one representative of a (role, device_type, hostname) class in
// the fleet, plus its DMI product string for storage matching. allowed marks
// whether slagworks is permitted to image this class.
type censusHost struct {
	desc       string
	hostname   string // NetBox device name
	site       string
	location   string
	deviceType string // NetBox device_type slug
	dmiProduct string // DMI system_product (commission data); "" if uncommissioned/NA
	allowed    bool   // true => an allowed cohort (kworker/hot-spare/oob-gw/leaf/spine)
}

func TestHardwareAllowlist(t *testing.T) {
	netScripts := loadNetworkScripts(t)
	storScripts := loadStorageScripts(t)

	// The fleet census. Every distinct (role, device_type) class observed in
	// pop1/pop2/pop3 is represented by at least one row; the shared-chassis
	// production hosts (same model as a kworker/hot-spare but a different
	// hostname) are represented explicitly because they are the dangerous
	// near-misses this guard exists for.
	census := []censusHost{
		// --- ALLOWED cohorts ---
		{desc: "pop1 L3 kworker", hostname: "kworker-l34-1.pop1", site: "pop1", location: "pop1-02-004730", deviceType: "supermicro-as-1115hs-tnr", dmiProduct: "AS -1115HS-TNR", allowed: true},
		{desc: "pop1 L3 kworker l56", hostname: "kworker-l56-3.pop1", site: "pop1", location: "pop1-02-004730", deviceType: "supermicro-as-1115hs-tnr", dmiProduct: "AS -1115HS-TNR", allowed: true},
		{desc: "pop2 old-cage hot-spare", hostname: "hot-spare-1.pop2", site: "pop2", location: "pop2-01-005330", deviceType: "supermicro-as-1015cs-tnr", dmiProduct: "AS -1015CS-TNR", allowed: true},
		{desc: "pop1 oob-gw", hostname: "oob-gw-1.pop1", site: "pop1", location: "pop1-02-004730", deviceType: "dell-r640", dmiProduct: "PowerEdge R640", allowed: true},
		{desc: "pop2 old-cage oob-gw", hostname: "oob-gw-1.pop2", site: "pop2", location: "pop2-01-005330", deviceType: "dell-r640", dmiProduct: "PowerEdge R640", allowed: true},
		{desc: "pop2 new-cage oob-gw", hostname: "oob-gw-3.pop2", site: "pop2", location: "pop2-01-005250", deviceType: "dell-r640", dmiProduct: "PowerEdge R640", allowed: true},
		{desc: "pop1 SN2700 leaf", hostname: "leaf-3.pop1", site: "pop1", location: "pop1-02-004730", deviceType: "mellanox-sn2700", dmiProduct: "MSN2700", allowed: true},
		{desc: "pop1 SN2700-CS2FO leaf", hostname: "leaf-4.pop1", site: "pop1", location: "pop1-02-004730", deviceType: "mellanox-sn2700-cs2fo", dmiProduct: "MSN2700-CS2FO", allowed: true}, // live DMI, 2026-10-23
		{desc: "pop1 SN2700 spine", hostname: "spine-1.pop1", site: "pop1", location: "pop1-02-004730", deviceType: "mellanox-sn2700", dmiProduct: "MSN2700", allowed: true},
		// pop3 ASUS RS501A validation boxes — a legitimate deploy cohort. NOTE
		// pop3 has a duplicate device-type for the same hardware (rs501a-e12-rs4u
		// id on cpu1/cpu2 vs asus-rs501a on cpu3), like the R640/MSN2700 dups —
		// tracked separately. Both match the asus-rs501a storage script by DMI
		// product, which is what matters here.
		{desc: "pop3 cpu (RS501A, rs501a-e12-rs4u device-type)", hostname: "cpu1.pop3", site: "pop3", location: "pop3-01-412290", deviceType: "rs501a-e12-rs4u", dmiProduct: "RS501A-E12-RS4U", allowed: true},
		{desc: "pop3 cpu (RS501A, asus-rs501a device-type dup)", hostname: "cpu3.pop3", site: "pop3", location: "pop3-01-412290", deviceType: "asus-rs501a", dmiProduct: "RS501A-E12-RS4U", allowed: true},
		// pop3 Minisforum MS-01 router — onboarded 2026-07-13 (fabric plan
		// Phase 5+: reimage router1 via the oob-gw-1 worker onto a boot
		// RAID1). SMBIOS product is "Venus Series", NOT "MS-01" — Minisforum
		// doesn't put the model name in DMI (verified via dmidecode on the
		// live box).
		{desc: "pop3 Minisforum router", hostname: "router1.pop3", site: "pop3", location: "pop3-01-412290", deviceType: "minisforum-ms-01", dmiProduct: "Venus Series", allowed: true},

		// --- NOT allowed: production servers sharing a chassis with kworker/hot-spare ---
		// These are the dangerous near-misses: identical system_product +
		// device_type + role=server as an allowed cohort, separated ONLY by
		// hostname. A model-only storage match would wrongly claim them.
		{desc: "nova on 1115HS TNR (kworker chassis)", hostname: "nova1.pop1", site: "pop1", location: "pop1-02-004730", deviceType: "supermicro-as-1115hs-tnr", dmiProduct: "AS -1115HS-TNR", allowed: false},
		{desc: "algo on 1015CS TNR (hot-spare chassis)", hostname: "algo1.pop2", site: "pop2", location: "pop2-01-005330", deviceType: "supermicro-as-1015cs-tnr", dmiProduct: "AS -1015CS-TNR", allowed: false},
		{desc: "inferno on 1015CS TNR", hostname: "inferno1.pop1", site: "pop1", location: "pop1-02-004730", deviceType: "supermicro-as-1015cs-tnr", dmiProduct: "AS -1015CS-TNR", allowed: false},
		{desc: "relay on 1015CS TNR", hostname: "relay1.pop2", site: "pop2", location: "pop2-01-005330", deviceType: "supermicro-as-1015cs-tnr", dmiProduct: "AS -1015CS-TNR", allowed: false},
		{desc: "router (server-class) on 1015CS TNR", hostname: "router1.pop1", site: "pop1", location: "pop1-02-004730", deviceType: "supermicro-as-1015cs-tnr", dmiProduct: "AS -1015CS-TNR", allowed: false},
		{desc: "appview on 2125HS TNR", hostname: "appview1.pop1", site: "pop1", location: "pop1-02-004730", deviceType: "supermicro-as-2125hs-tnr", dmiProduct: "AS -2125HS-TNR", allowed: false},
		{desc: "search on 1015CS TNR", hostname: "search1.pop2", site: "pop2", location: "pop2-01-005330", deviceType: "supermicro-as-1015cs-tnr", dmiProduct: "AS -1015CS-TNR", allowed: false},
		{desc: "vortex on 1015CS TNR", hostname: "vortex1.pop1", site: "pop1", location: "pop1-02-004730", deviceType: "supermicro-as-1015cs-tnr", dmiProduct: "AS -1015CS-TNR", allowed: false},

		// --- NOT allowed: other fleet hardware classes (no script should touch) ---
		{desc: "SN2410 leaf (older switch, not SN2700)", hostname: "leaf-1.pop1", site: "pop1", location: "pop1-02-004730", deviceType: "mellanox-sn2410", dmiProduct: "MSN2410", allowed: false},
		{desc: "APC PDU", hostname: "pdu-0101-left.4730.sv2", site: "pop1", location: "pop1-02-004730", deviceType: "apc-apdu9965", dmiProduct: "", allowed: false},
		{desc: "Equinix fiber patch panel", hostname: "PP-1328850", site: "pop1", location: "pop1-02-004730", deviceType: "eqx-48p-fiber-pp", dmiProduct: "", allowed: false},
		{desc: "Equinix mixed patch panel", hostname: "PP-1328851", site: "pop1", location: "pop1-02-004730", deviceType: "eqx-48p-mixed-pp", dmiProduct: "", allowed: false},
	}

	for _, h := range census {
		dev := netbox.Device{Name: h.hostname}
		if h.site != "" {
			dev.Site = &netbox.NestedRef{Slug: h.site}
		}
		if h.location != "" {
			dev.Location = &netbox.NestedRef{Slug: h.location}
		}
		if h.deviceType != "" {
			dev.DeviceType = &netbox.NestedRef{Slug: h.deviceType}
		}

		net := matchNetworkScript(netScripts, dev)
		netName := ""
		if net != nil {
			netName = net.Name
		}

		// Storage matches on DMI product + the hostname identity the builder
		// supplies. An uncommissioned host (no dmiProduct) can't match a
		// storage script regardless — mirror that by passing empty commission
		// data when dmiProduct is "".
		storName := ""
		if h.dmiProduct != "" {
			commData := mustCommissionJSON(t, h.dmiProduct)
			st, err := storage.Match(storScripts, commData, map[string]string{"hostname": h.hostname})
			if err != nil {
				t.Fatalf("%s: storage match: %v", h.desc, err)
			}
			if st != nil {
				storName = st.Path
			}
		}

		if h.allowed {
			// Allowed cohorts must match SOMETHING (at least one of net/storage)
			// — otherwise the row is mislabeled or a script regressed. (Switches
			// match network only; uncommissioned-but-allowed would match net.)
			if netName == "" && storName == "" {
				t.Errorf("ALLOWED host %q (%s) matched NO script — expected a network and/or storage match; a cohort script may have regressed", h.hostname, h.desc)
			}
		} else {
			// The core guard: a not-allowed host must match NOTHING.
			if netName != "" {
				t.Errorf("DENY BREACH: not-allowed host %q (%s) matched NETWORK script %q — a config script broadened to catch hardware we never chose to deploy. If this host SHOULD be onboarded, add it to the allowlist deliberately; otherwise narrow the script.", h.hostname, h.desc, netName)
			}
			if storName != "" {
				t.Errorf("DENY BREACH: not-allowed host %q (%s) matched STORAGE script %q — a config script broadened to catch hardware we never chose to deploy. If this host SHOULD be onboarded, add it to the allowlist deliberately; otherwise narrow the script.", h.hostname, h.desc, storName)
			}
		}
	}
}

func mustCommissionJSON(t *testing.T, product string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"system": map[string]any{"product_name": product},
	})
	if err != nil {
		t.Fatalf("marshal commission json: %v", err)
	}
	return b
}

// loadStorageScripts mirrors loadNetworkScripts but for assets/storage/, via
// the storage parse path. Kept here (not in internal/storage) so this census
// test can drive both matchers from one place.
func loadStorageScripts(t *testing.T) []storage.Script {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := filepath.Join(filepath.Dir(filepath.Dir(wd)), "assets", "storage")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read assets/storage: %v", err)
	}
	var scripts []storage.Script
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		s, err := storage.ParseScript(e.Name(), body)
		if err != nil {
			t.Fatalf("parse storage %s: %v", e.Name(), err)
		}
		scripts = append(scripts, *s)
	}
	if len(scripts) == 0 {
		t.Fatal("no storage scripts found in assets/storage")
	}
	return scripts
}
