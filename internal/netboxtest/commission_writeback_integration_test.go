//go:build integration

package netboxtest_test

// Commission write-back cases whose outcome depends on a constraint NetBox
// itself enforces: module bay occupancy, device asset_tag uniqueness,
// interface name uniqueness. They run CommissionToSync and
// SyncCommissionData against a NetBox container, so that behaviour is
// NetBox's own and not a fake's; the property test in
// internal/netbox/commission_e2e_prop_test.go models the same constraints
// in internal/netbox/nbfake.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/bluesky-social/slagworks/internal/netbox"
	"github.com/bluesky-social/slagworks/internal/netboxtest"
)

func syncReport(t *testing.T, env *netboxtest.Env, dev *netbox.Device, report string) error {
	t.Helper()
	s, err := netbox.CommissionToSync(dev.ID, json.RawMessage(report))
	if err != nil {
		t.Fatalf("CommissionToSync: %v", err)
	}
	s.SiteID = dev.Site.ID
	s.Hostname = dev.Name
	return env.NetBox.SyncCommissionData(context.Background(), *s)
}

type bayModule struct {
	bays    map[int]string
	modules map[int]int // bay id -> module id
}

func listBaysModules(t *testing.T, env *netboxtest.Env, devID int) bayModule {
	t.Helper()
	ctx := context.Background()
	var bays []netbox.ModuleBay
	if err := env.NetBox.List(ctx, "/api/dcim/module-bays/", map[string][]string{"device_id": {fmt.Sprint(devID)}}, &bays); err != nil {
		t.Fatal(err)
	}
	var mods []netbox.Module
	if err := env.NetBox.List(ctx, "/api/dcim/modules/", map[string][]string{"device_id": {fmt.Sprint(devID)}}, &mods); err != nil {
		t.Fatal(err)
	}
	out := bayModule{bays: map[int]string{}, modules: map[int]int{}}
	for _, b := range bays {
		out.bays[b.ID] = b.Name
	}
	for _, m := range mods {
		if m.ModuleBay != nil {
			out.modules[m.ModuleBay.ID] = m.ID
		}
	}
	return out
}

// Two PSUs whose DMI type 39 Location is the same placeholder, as some
// server firmware reports.
const twoPSUsSameLocation = `{"system":{"serial_number":"S-PSU-DUP"},
 "psus":[{"location":"Not Specified","model":"PWR SPLY,750W","serial":"PSU-A","max_watts":750},
         {"location":"Not Specified","model":"PWR SPLY,750W","serial":"PSU-B","max_watts":750}]}`

func writebackDuplicatePSULocationKeepsBothPSUs(t *testing.T, env *netboxtest.Env) {
	site := env.SeedSite("psu")
	dev := env.SeedDevice(site, netboxtest.SeedDeviceOpts{})

	err1 := syncReport(t, env, dev, twoPSUsSameLocation)
	err2 := syncReport(t, env, dev, twoPSUsSameLocation)
	got := listBaysModules(t, env, dev.ID)
	t.Logf("first run: %v; second run: %v; bays %v; occupied %v", err1, err2, got.bays, got.modules)
	if err1 != nil {
		t.Errorf("first commission of a host with two same-located PSUs failed: %v", err1)
	}
	if len(got.modules) != 2 {
		t.Errorf("got %d PSU modules, want 2 (one per reported PSU)", len(got.modules))
	}
	for id, name := range got.bays {
		if _, ok := got.modules[id]; !ok {
			t.Errorf("module bay %d %q is empty: an orphan left by the duplicate bay name", id, name)
		}
	}
}

func writebackSharedAssetTagDoesNotFailSecondHost(t *testing.T, env *netboxtest.Env) {
	// SeedDevice names every device alike, so the two hosts sit in two
	// sites; asset_tag uniqueness is global in NetBox.
	a := env.SeedDevice(env.SeedSite("asset-a"), netboxtest.SeedDeviceOpts{})
	b := env.SeedDevice(env.SeedSite("asset-b"), netboxtest.SeedDeviceOpts{})
	report := `{"system":{"serial_number":"%s","asset_tag":"Chassis Asset Tag"}}`
	if err := syncReport(t, env, a, fmt.Sprintf(report, "S-ASSET-A")); err != nil {
		t.Fatalf("first host: %v", err)
	}
	if err := syncReport(t, env, b, fmt.Sprintf(report, "S-ASSET-B")); err != nil {
		t.Errorf("second host reporting the same chassis asset tag failed its commission: %v", err)
	}
}

func writebackMACLessIPMIRowIsRepaired(t *testing.T, env *netboxtest.Env) {
	ctx := context.Background()
	site := env.SeedSite("ipmi")
	dev := env.SeedDevice(site, netboxtest.SeedDeviceOpts{})
	var row netbox.Interface
	if err := env.NetBox.Post(ctx, "/api/dcim/interfaces/", map[string]any{
		"device": dev.ID, "name": "ipmi", "type": "1000base-t", "mgmt_only": true,
	}, &row); err != nil {
		t.Fatal(err)
	}
	report := `{"system":{"serial_number":"S-IPMI"},"bmc":{"ip":"10.0.0.5","mac":"d0:50:99:aa:bb:cc","firmware_version":"1.0"}}`
	if err := syncReport(t, env, dev, report); err != nil {
		t.Errorf("commission with a MAC-less ipmi row failed: %v", err)
	}
}

// TestCommissionWriteback shares one NetBox stack across the cases: each
// seeds its own site and devices.
func TestCommissionWriteback(t *testing.T) {
	env := netboxtest.New(t)
	t.Run("DuplicatePSULocationKeepsBothPSUs", func(t *testing.T) { writebackDuplicatePSULocationKeepsBothPSUs(t, env) })
	t.Run("SharedAssetTagDoesNotFailSecondHost", func(t *testing.T) { writebackSharedAssetTagDoesNotFailSecondHost(t, env) })
	t.Run("MACLessIPMIRowIsRepaired", func(t *testing.T) { writebackMACLessIPMIRowIsRepaired(t, env) })
	t.Run("OperatorIPMI0RowIsNotDuplicated", func(t *testing.T) { writebackOperatorIPMI0RowIsNotDuplicated(t, env) })
}

// The hardware docs have operators model the BMC row as "ipmi0", mgmt_only,
// with the BMC MAC. Commission must recognise it as the BMC row and not add a
// second management interface ("ipmi") carrying the same MAC.
func writebackOperatorIPMI0RowIsNotDuplicated(t *testing.T, env *netboxtest.Env) {
	ctx := context.Background()
	dev := env.SeedDevice(env.SeedSite("ipmi0"), netboxtest.SeedDeviceOpts{})
	var row netbox.Interface
	if err := env.NetBox.Post(ctx, "/api/dcim/interfaces/", map[string]any{
		"device": dev.ID, "name": "ipmi0", "type": "1000base-t", "mgmt_only": true,
	}, &row); err != nil {
		t.Fatal(err)
	}
	var mac netbox.MACAddressRef
	if err := env.NetBox.Post(ctx, "/api/dcim/mac-addresses/", map[string]any{
		"mac_address": "d0:50:99:aa:bb:01", "assigned_object_type": "dcim.interface", "assigned_object_id": row.ID,
	}, &mac); err != nil {
		t.Fatal(err)
	}
	if err := env.NetBox.Patch(ctx, fmt.Sprintf("/api/dcim/interfaces/%d/", row.ID), map[string]any{"primary_mac_address": mac.ID}, &row); err != nil {
		t.Fatal(err)
	}
	report := `{"system":{"serial_number":"S-IPMI0"},"bmc":{"ip":"10.0.0.6","mac":"d0:50:99:aa:bb:01","firmware_version":"1.0"}}`
	if err := syncReport(t, env, dev, report); err != nil {
		t.Fatalf("commission: %v", err)
	}
	ifaces, err := env.NetBox.ListDeviceInterfaces(ctx, dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	var mgmt []string
	for _, i := range ifaces {
		if i.MgmtOnly {
			mgmt = append(mgmt, i.Name+"/"+i.GetMAC())
		}
	}
	if len(mgmt) != 1 {
		t.Errorf("management interfaces after commission: %v, want only the operator's ipmi0", mgmt)
	}
}
