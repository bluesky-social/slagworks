package snapshot

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/awssecrets"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

//go:fix inline

// adminSecretInputs builds a minimal Inputs for a single machine whose NetBox
// device carries the given serial, plus an admin secret keyed by that serial
// (the snapshot's only BMC credential source).
func adminSecretInputs(serial string, secret *awssecrets.AdminSecret) Inputs {
	mID := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	nbID := 7
	return Inputs{
		Site:     netbox.Site{Name: "pop1", Slug: "pop1"},
		Config:   model.SiteConfig{Slug: "pop1", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200"},
		Prefixes: []netbox.Prefix{{Prefix: "10.0.0.0/24", CustomFields: gatewayJSON("10.0.0.1")}},
		Devices:  []netbox.Device{{ID: nbID, Name: "host-z", Serial: serial}},
		Machines: []model.Machine{
			{ID: mID, NetBoxDeviceID: &nbID, NetBoxSerial: serial, State: model.MachineStateRunning, DesiredOS: "ubuntu-24.04"},
		},
		BMCAdminSecret: secret,
	}
}

// TestBuildAdminSecret_KindGuard locks the Kind guard on the snapshot
// builder's admin-cred stamp: the per-pop secret is serial-keyed and holds
// BOTH bmc and pdu entries, so a serial that resolves to a kind=pdu entry must
// NOT fill BMCUser/BMCPass (that would stamp an `apc` PDU cred into BMC fields).
// A kind=bmc entry at the same serial-shape SHOULD fill.
func TestBuildAdminSecret_KindGuard(t *testing.T) {
	const serial = "SN-Z"

	// pdu entry must be rejected -> BMC creds stay empty.
	pduSecret := &awssecrets.AdminSecret{BySerial: map[string]awssecrets.AdminHost{
		serial: {Kind: awssecrets.KindPDU, Username: "apc", Password: "pdu-pw"},
	}}
	snap, err := Build(adminSecretInputs(serial, pduSecret))
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Machines[0]; got.BMCUser != "" || got.BMCPass != "" {
		t.Fatalf("pdu entry must NOT fill BMC creds, got user=%q pass=%q", got.BMCUser, got.BMCPass)
	}

	// bmc entry at the same serial SHOULD fill.
	bmcSecret := &awssecrets.AdminSecret{BySerial: map[string]awssecrets.AdminHost{
		serial: {Kind: awssecrets.KindBMC, Username: "ADMIN", Password: "bmc-pw"},
	}}
	snap, err = Build(adminSecretInputs(serial, bmcSecret))
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Machines[0]; got.BMCUser != "ADMIN" || got.BMCPass != "bmc-pw" {
		t.Fatalf("bmc entry should fill BMC creds, got user=%q pass=%q", got.BMCUser, got.BMCPass)
	}

	// No entry for the serial (or no secret at all): the snapshot carries no
	// BMC cred. There is no other source to fall back to.
	snap, err = Build(adminSecretInputs(serial, &awssecrets.AdminSecret{BySerial: map[string]awssecrets.AdminHost{}}))
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Machines[0]; got.BMCUser != "" || got.BMCPass != "" {
		t.Fatalf("no admin entry must leave BMC creds empty, got user=%q pass=%q", got.BMCUser, got.BMCPass)
	}
	snap, err = Build(adminSecretInputs(serial, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Machines[0]; got.BMCUser != "" || got.BMCPass != "" {
		t.Fatalf("no admin secret must leave BMC creds empty, got user=%q pass=%q", got.BMCUser, got.BMCPass)
	}
}

// TestBuildAMTEntry: a kind=amt entry marks the machine AMT and its digest
// credential fills BMCUser/BMCPass. A kind=bmc machine never gets AMT set.
func TestBuildAMTEntry(t *testing.T) {
	const serial = "SN-AMT"
	amtSecret := &awssecrets.AdminSecret{BySerial: map[string]awssecrets.AdminHost{
		serial: {Kind: awssecrets.KindAMT, Username: "admin", Password: "amt-pw"},
	}}
	snap, err := Build(adminSecretInputs(serial, amtSecret))
	if err != nil {
		t.Fatal(err)
	}
	got := snap.Machines[0]
	if !got.AMT || got.BMCUser != "admin" || got.BMCPass != "amt-pw" {
		t.Fatalf("amt entry: got AMT=%v user=%q pass=%q, want AMT=true admin/amt-pw", got.AMT, got.BMCUser, got.BMCPass)
	}

	// An incomplete amt entry still marks AMT (so no IPMI path runs) but
	// carries no credential (the AMT dialer then refuses to dial).
	partial := &awssecrets.AdminSecret{BySerial: map[string]awssecrets.AdminHost{
		serial: {Kind: awssecrets.KindAMT, Username: "admin"},
	}}
	snap, err = Build(adminSecretInputs(serial, partial))
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Machines[0]; !got.AMT || got.BMCUser != "" || got.BMCPass != "" {
		t.Fatalf("partial amt entry: got AMT=%v user=%q pass=%q, want AMT=true and empty creds", got.AMT, got.BMCUser, got.BMCPass)
	}

	bmcSecret := &awssecrets.AdminSecret{BySerial: map[string]awssecrets.AdminHost{
		serial: {Kind: awssecrets.KindBMC, Username: "ADMIN", Password: "bmc-pw"},
	}}
	snap, err = Build(adminSecretInputs(serial, bmcSecret))
	if err != nil {
		t.Fatal(err)
	}
	if snap.Machines[0].AMT {
		t.Fatal("kind=bmc entry must not mark the machine AMT")
	}
}

func TestBuildEmptySite(t *testing.T) {
	in := Inputs{
		Site:     netbox.Site{Name: "pop3", Slug: "pop3"},
		Config:   model.SiteConfig{Slug: "pop3", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200"},
		Prefixes: []netbox.Prefix{{Prefix: "10.0.0.0/24", CustomFields: gatewayJSON("10.0.0.1")}},
	}
	snap, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Site.Slug != "pop3" {
		t.Fatalf("got slug %q", snap.Site.Slug)
	}
	if snap.DHCP.Subnet != "10.0.0.0/24" || snap.DHCP.Gateway != "10.0.0.1" {
		t.Fatalf("bad dhcp: %+v", snap.DHCP)
	}
	if len(snap.Machines) != 0 {
		t.Fatalf("expected 0 machines, got %d", len(snap.Machines))
	}
}

func TestExtractDiskIdentities(t *testing.T) {
	got := extractDiskIdentities([]byte(`{"disks":[{"serial":"DISK-1","wwn":"0x5001"},{"serial":"","wwn":""},{"serial":"0","wwn":"0x0000000000000000"}]}`))
	if len(got) != 1 || got[0].Serial != "DISK-1" || got[0].WWN != "0x5001" {
		t.Fatalf("extractDiskIdentities = %+v", got)
	}
}

func TestExtractSystemIdentity(t *testing.T) {
	system, chassis, systemUUID := extractSystemIdentity([]byte(`{
		"system": {
			"serial_number": " SYS-1 ",
			"chassis_serial": " CHASSIS-1 ",
			"uuid": " 550e8400-e29b-41d4-a716-446655440000 "
		}
	}`))
	if system != "SYS-1" || chassis != "CHASSIS-1" || systemUUID != "550e8400-e29b-41d4-a716-446655440000" {
		t.Fatalf("extractSystemIdentity = (%q, %q, %q)", system, chassis, systemUUID)
	}
}

func TestExtractSystemIdentityDropsPlaceholderSerials(t *testing.T) {
	system, chassis, _ := extractSystemIdentity([]byte(`{
		"system": {
			"serial_number": "Default string",
			"chassis_serial": "0000000000",
			"uuid": "550e8400-e29b-41d4-a716-446655440000"
		}
	}`))
	if system != "" || chassis != "" {
		t.Fatalf("placeholder serials must be absent, got (%q, %q)", system, chassis)
	}
}

// TestBuildIgnoresReservedIPs locks the status filter on interface-IP
// joins: a renumber-in-progress site (pop3 bring-up) models each interface
// with its live status=active address AND a status=reserved future address.
// The reserved one must be invisible to machine-loop interface facts and OOB
// reservations. BMCIP comes independently from the device's canonical oob_ip.
func TestBuildIgnoresReservedIPs(t *testing.T) {
	mID := uuid.MustParse("00000000-0000-0000-0000-000000000003")
	nbID := 43
	active := &netbox.NestedValue{Value: "active"}
	reserved := &netbox.NestedValue{Value: "reserved"}
	in := Inputs{
		Site:     netbox.Site{Name: "pop3", Slug: "pop3"},
		Config:   model.SiteConfig{Slug: "pop3", EphemeralPoolStart: "10.252.1.100", EphemeralPoolEnd: "10.252.1.200"},
		Prefixes: []netbox.Prefix{{Prefix: "10.252.1.0/24", CustomFields: gatewayJSON("10.252.1.1")}},
		OOBPrefixes: []netbox.Prefix{
			{Prefix: "10.252.240.0/22", CustomFields: gatewayJSON("10.252.240.1")},
		},
		Devices: []netbox.Device{{ID: nbID, Name: "cpu2.pop3", Serial: "SN-YF", OOBIP: &netbox.NestedIPAddress{Address: "10.252.1.126/24"}}},
		Interfaces: []netbox.Interface{
			{ID: 200, Name: "ipmi", MgmtOnly: true, MACAddress: "AA:BB:CC:DD:EE:02", Type: &netbox.NestedValue{Value: "1000base-t"}, Device: &netbox.NestedRef{ID: nbID}},
		},
		IPs: []netbox.IPAddress{
			// live BMC address + its reserved renumber target on the SAME interface
			{Address: "10.252.1.126/24", Status: active, AssignedObjectID: new(200), AssignedObjectType: "dcim.interface"},
			{Address: "10.252.240.42/22", Status: reserved, AssignedObjectID: new(200), AssignedObjectType: "dcim.interface"},
		},
		Machines: []model.Machine{
			{ID: mID, NetBoxDeviceID: &nbID, NetBoxSerial: "SN-YF", State: model.MachineStateNew},
		},
	}
	snap, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Machines[0].BMCIP; got != "10.252.1.126" {
		t.Fatalf("BMCIP %q, want the active address (reserved renumber target must not win)", got)
	}
	// The reserved .240.42 falls inside the OOB prefix; without the status
	// filter it would also have tripped the one-OOB-IP-per-interface guard
	// or produced a reservation for an unreachable address.
	for _, r := range snap.Reservations {
		if r.IP == "10.252.240.42" {
			t.Fatalf("reserved IP leaked into OOB reservations: %+v", r)
		}
	}
}

// TestBuildRejectsInterfaceWithTwoActiveIPs: a machine's network facts carry
// one address per interface (SnapshotInterface.IP). When a bound machine's
// NetBox interface had two status=active addresses, Build kept whichever
// NetBox listed last and dropped the other without a word, so the address
// handed to the network-config script and the worker's DHCP reservation
// depended on list order. Build must refuse the site instead, naming the
// device, the interface and every address, with the same error for either
// order. Retiring one address (status=reserved, the renumber convention)
// builds again with the remaining one.
func TestBuildRejectsInterfaceWithTwoActiveIPs(t *testing.T) {
	devID := 7
	bound := []model.Machine{{ID: uuid.MustParse("00000000-0000-0000-0000-000000000007"), NetBoxDeviceID: &devID}}
	ipA := netbox.IPAddress{ID: 1, Address: "10.253.0.22/22", AssignedObjectType: "dcim.interface", AssignedObjectID: new(70)}
	ipB := netbox.IPAddress{ID: 2, Address: "10.253.0.99/22", AssignedObjectType: "dcim.interface", AssignedObjectID: new(70)}
	inputs := func(machines []model.Machine, ips ...netbox.IPAddress) Inputs {
		return Inputs{
			Site:     netbox.Site{Name: "site1", Slug: "site1"},
			Config:   model.SiteConfig{Slug: "site1"},
			Prefixes: []netbox.Prefix{{Prefix: "10.253.0.0/22", CustomFields: gatewayJSON("10.253.3.254")}},
			Devices:  []netbox.Device{{ID: devID, Name: "host-1"}},
			Interfaces: []netbox.Interface{{
				ID: 70, Name: "bond0", Type: &netbox.NestedValue{Value: "lag"},
				Device: &netbox.NestedRef{ID: devID},
			}},
			IPs:      ips,
			Machines: machines,
		}
	}

	_, errAB := Build(inputs(bound, ipA, ipB))
	_, errBA := Build(inputs(bound, ipB, ipA))
	if errAB == nil || errBA == nil {
		t.Fatalf("Build accepted an interface with two active addresses and kept one of them: [A,B] err=%v, [B,A] err=%v", errAB, errBA)
	}
	for _, want := range []string{"host-1", "bond0", ipA.Address, ipB.Address} {
		if !strings.Contains(errAB.Error(), want) {
			t.Errorf("error does not name %q: %v", want, errAB)
		}
	}
	if errAB.Error() != errBA.Error() {
		t.Errorf("error depends on NetBox list order:\n  [A,B] %v\n  [B,A] %v", errAB, errBA)
	}

	// The same interface on a device no machine is bound to reaches no
	// machine's facts, so it must not block the site.
	if _, err := Build(inputs(nil, ipA, ipB)); err != nil {
		t.Errorf("unbound device's multi-address interface failed the site build: %v", err)
	}

	// The remedy the error suggests: retire one address.
	retired := ipB
	retired.Status = &netbox.NestedValue{Value: "reserved"}
	snap, err := Build(inputs(bound, retired, ipA))
	if err != nil {
		t.Fatalf("Build with one address retired: %v", err)
	}
	if got := snap.Machines[0].Interfaces[0]; got.IP != ipA.Address || got.Gateway != "10.253.3.254" {
		t.Errorf("bond0 facts = %+v, want ip %s with gateway 10.253.3.254", got, ipA.Address)
	}
}

// TestBuildBMCIPFromBMClessSwitch locks the mgmt_only-conflation fix: a BMC-less
// device (e.g. an SN2700 switch) whose host PXE NIC is flagged mgmt_only
// and carries an IP must NOT be mistaken for a BMC. MachineSnapshot.BMCIP
// stays empty (there is no oob_ip), and the NIC remains physical so its MAC
// is indexed as machine identity and receives a PXE bootfile. It also remains
// available to the OOB DHCP reservation logic.
func TestBuildBMCIPFromBMClessSwitch(t *testing.T) {
	mID := uuid.MustParse("00000000-0000-0000-0000-000000000040")
	nbID := 730
	in := Inputs{
		Site:   netbox.Site{Name: "pop2", Slug: "pop2"},
		Config: model.SiteConfig{Slug: "pop2", EphemeralPoolStart: "10.253.0.100", EphemeralPoolEnd: "10.253.0.200"},
		Prefixes: []netbox.Prefix{
			{Prefix: "10.253.0.0/22", CustomFields: gatewayJSON("10.253.3.254")},
		},
		OOBPrefixes: []netbox.Prefix{
			{Prefix: "10.253.242.0/23", CustomFields: gatewayJSON("10.253.243.252")},
		},
		// No OOBIP on the device: this switch has no BMC.
		Devices: []netbox.Device{{
			ID: nbID, Name: "sw-1.pop2", Serial: "SN-SW1",
			Site: &netbox.NestedRef{Slug: "pop2"},
		}},
		Interfaces: []netbox.Interface{
			// mgmt_only management NIC with an IP, exactly the shape that
			// used to be misread as a BMC address.
			{ID: 731, Name: "mgmt0", MgmtOnly: true, MACAddress: "AA:BB:CC:00:D0:01", Device: &netbox.NestedRef{ID: nbID}},
		},
		IPs: []netbox.IPAddress{
			{Address: "10.253.242.10/23", AssignedObjectID: new(731), AssignedObjectType: "dcim.interface"},
		},
		Machines: []model.Machine{
			{ID: mID, NetBoxDeviceID: &nbID, NetBoxSerial: "SN-SW1", State: model.MachineStateRunning},
		},
	}
	snap, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	m := snap.Machines[0]
	if m.BMCIP != "" {
		t.Fatalf("BMCIP = %q, want empty for a BMC-less switch (no oob_ip)", m.BMCIP)
	}
	// With no device oob_ip there is no BMC. This management-only NIC is the
	// host OS/PXE NIC and must remain eligible for machine MAC indexing.
	mgmt := findIface(t, m.Interfaces, "mgmt0")
	if mgmt.Type != "physical" {
		t.Errorf("mgmt0 Type = %q, want physical for a BMC-less machine's PXE NIC", mgmt.Type)
	}
	if mgmt.IP != "10.253.242.10/23" {
		t.Errorf("mgmt0 IP = %q, want the management address to remain available", mgmt.IP)
	}
	// ...and to the OOB DHCP reservation set.
	var found bool
	for _, r := range snap.Reservations {
		if r.MAC == "aa:bb:cc:00:d0:01" && r.IP == "10.253.242.10" {
			found = true
		}
	}
	if !found {
		t.Errorf("mgmt0 MAC/IP missing from OOB reservations: %+v", snap.Reservations)
	}
}

// TestBuildBMCIPFromOOBIP locks the positive path: a normal server whose
// NetBox device carries oob_ip gets that address (CIDR stripped) as its
// BMCIP.
func TestBuildBMCIPFromOOBIP(t *testing.T) {
	mID := uuid.MustParse("00000000-0000-0000-0000-000000000041")
	nbID := 731
	in := Inputs{
		Site:   netbox.Site{Name: "pop1", Slug: "pop1"},
		Config: model.SiteConfig{Slug: "pop1"},
		Prefixes: []netbox.Prefix{
			{Prefix: "10.254.28.0/24", CustomFields: gatewayJSON("10.254.28.1")},
		},
		Devices: []netbox.Device{{
			ID: nbID, Name: "cpu1.pop1", Serial: "SN-CPU1",
			Site:  &netbox.NestedRef{Slug: "pop1"},
			OOBIP: &netbox.NestedIPAddress{Address: "10.254.241.7/24"},
		}},
		Machines: []model.Machine{
			{ID: mID, NetBoxDeviceID: &nbID, NetBoxSerial: "SN-CPU1", State: model.MachineStateRunning},
		},
	}
	snap, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Machines[0].BMCIP; got != "10.254.241.7" {
		t.Fatalf("BMCIP = %q, want stripped oob_ip 10.254.241.7", got)
	}
}

// TestBuildOOBIPWinsOverMisleadingInterface locks oob_ip as the canonical
// BMC signal: even when interface flags/names would point at a different
// address (a non-mgmt interface named like a BMC carrying its own IP, or
// a mgmt_only NIC with a stale address), the device's oob_ip wins.
func TestBuildOOBIPWinsOverMisleadingInterface(t *testing.T) {
	mID := uuid.MustParse("00000000-0000-0000-0000-000000000042")
	nbID := 732
	in := Inputs{
		Site:   netbox.Site{Name: "pop1", Slug: "pop1"},
		Config: model.SiteConfig{Slug: "pop1"},
		Prefixes: []netbox.Prefix{
			{Prefix: "10.254.28.0/24", CustomFields: gatewayJSON("10.254.28.1")},
		},
		Devices: []netbox.Device{{
			ID: nbID, Name: "cpu2.pop1", Serial: "SN-CPU2",
			Site:  &netbox.NestedRef{Slug: "pop1"},
			OOBIP: &netbox.NestedIPAddress{Address: "10.254.241.9/24"},
		}},
		Interfaces: []netbox.Interface{
			// A stale mgmt_only NIC must not win over the address selected as
			// the device's canonical oob_ip.
			{ID: 733, Name: "ipmi-old", MgmtOnly: true, MACAddress: "AA:BB:CC:00:B0:01", Device: &netbox.NestedRef{ID: nbID}},
			{ID: 734, Name: "bmc-current", MACAddress: "AA:BB:CC:00:B0:02", Device: &netbox.NestedRef{ID: nbID}},
		},
		IPs: []netbox.IPAddress{
			{Address: "10.254.241.99/24", AssignedObjectID: new(733), AssignedObjectType: "dcim.interface"},
			{Address: "10.254.241.9/24", AssignedObjectID: new(734), AssignedObjectType: "dcim.interface"},
		},
		Machines: []model.Machine{
			{ID: mID, NetBoxDeviceID: &nbID, NetBoxSerial: "SN-CPU2", State: model.MachineStateRunning},
		},
	}
	snap, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Machines[0].BMCIP; got != "10.254.241.9" {
		t.Fatalf("BMCIP = %q, want oob_ip 10.254.241.9 (canonical), not the misleading interface IP", got)
	}
}

func TestBuildJoinsMachineWithNetBox(t *testing.T) {
	mID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	nbID := 42
	in := Inputs{
		Site:     netbox.Site{Name: "pop3", Slug: "pop3"},
		Config:   model.SiteConfig{Slug: "pop3", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200"},
		Prefixes: []netbox.Prefix{{Prefix: "10.0.0.0/24", CustomFields: gatewayJSON("10.0.0.1")}},
		Devices:  []netbox.Device{{ID: nbID, Name: "host-a", Serial: "SN-A", OOBIP: &netbox.NestedIPAddress{Address: "10.0.0.200/24"}}},
		Interfaces: []netbox.Interface{
			{ID: 100, Name: "eno1", MACAddress: "AA:BB:CC:DD:EE:01", Type: &netbox.NestedValue{Value: "1000base-t"}, Device: &netbox.NestedRef{ID: nbID}},
			{ID: 101, Name: "ipmi", MgmtOnly: true, MACAddress: "AA:BB:CC:DD:EE:FF", Type: &netbox.NestedValue{Value: "other"}, Device: &netbox.NestedRef{ID: nbID}},
		},
		IPs: []netbox.IPAddress{
			{Address: "10.0.0.20/24", AssignedObjectID: new(100), AssignedObjectType: "dcim.interface"},
			{Address: "10.0.0.200/24", AssignedObjectID: new(101), AssignedObjectType: "dcim.interface"},
		},
		Machines: []model.Machine{
			{ID: mID, NetBoxDeviceID: &nbID, NetBoxSerial: "SN-A", State: model.MachineStateRunning, DesiredOS: "ubuntu-24.04"},
		},
		BMCAdminSecret: &awssecrets.AdminSecret{BySerial: map[string]awssecrets.AdminHost{
			"SN-A": {Kind: awssecrets.KindBMC, Username: "admin", Password: "secret"},
		}},
	}
	snap, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Machines) != 1 {
		t.Fatalf("got %d machines", len(snap.Machines))
	}
	m := snap.Machines[0]
	if m.ID != mID {
		t.Fatalf("wrong uuid: %s", m.ID)
	}
	if m.Hostname != "host-a" {
		t.Fatalf("hostname %q", m.Hostname)
	}
	// No hostname template in Inputs: the deployed hostname is the NetBox
	// device name unchanged.
	if m.FQDN != "host-a" {
		t.Fatalf("FQDN = %q, want the device name with no template configured", m.FQDN)
	}
	if m.BMCIP != "10.0.0.200" {
		t.Fatalf("bmc ip %q (want stripped /24)", m.BMCIP)
	}
	if m.State != model.MachineStateRunning {
		t.Fatalf("state %s", m.State)
	}
	if m.BMCUser != "admin" || m.BMCPass != "secret" {
		t.Fatalf("bmc cred should come from the admin entry, got user=%q pass=%q", m.BMCUser, m.BMCPass)
	}
	if len(m.Interfaces) != 2 {
		t.Fatalf("ifaces %d", len(m.Interfaces))
	}
	if got := findIface(t, m.Interfaces, "ipmi").Type; got != "ipmi" {
		t.Errorf("ipmi Type = %q, want ipmi when the device has a canonical oob_ip", got)
	}
	for _, iface := range m.Interfaces {
		if iface.MAC != strings.ToLower(iface.MAC) {
			t.Fatalf("MAC should be lowercased, got %q", iface.MAC)
		}
	}
	// No network-config script provided in Inputs → no per-machine script
	// matched (agent falls back to its built-in DHCP netplan).
	if m.NetworkScript != nil {
		t.Fatalf("expected nil NetworkScript with no scripts configured, got %+v", m.NetworkScript)
	}
}

// TestBuildMatchesNetworkScript exercises the full builder path for the
// network-config seam: a pop1 device matches a site=pop1 network script,
// and the chosen script's body is attached verbatim to
// MachineSnapshot.NetworkScript. The raw interface facts (two /31 legs +
// loopback /32) ride along in ms.Interfaces — slagworks renders no netplan
// itself; the script (run by the agent) owns all topology interpretation.
func TestBuildMatchesNetworkScript(t *testing.T) {
	mID := uuid.MustParse("00000000-0000-0000-0000-000000000009")
	nbID := 801
	script := "#!/usr/bin/env bash\n# slagworks:match site=pop1\n# slagworks:priority 100\necho '{}'\n"
	in := Inputs{
		Site:     netbox.Site{Name: "pop1", Slug: "pop1"},
		Config:   model.SiteConfig{Slug: "pop1"},
		Prefixes: []netbox.Prefix{{Prefix: "10.254.28.0/24", CustomFields: gatewayJSON("10.254.28.1")}},
		Devices: []netbox.Device{{
			ID: nbID, Name: "kworker-l34-1.pop1", Serial: "S500001X0000001",
			Site: &netbox.NestedRef{Slug: "pop1"},
		}},
		Interfaces: []netbox.Interface{
			{ID: 901, Name: "eth0", MACAddress: "7C:C2:55:BE:F7:98", Type: &netbox.NestedValue{Value: "25gbase-x-sfp28"}, Device: &netbox.NestedRef{ID: nbID}},
			{ID: 902, Name: "eth1", MACAddress: "7C:C2:55:BE:F7:99", Type: &netbox.NestedValue{Value: "25gbase-x-sfp28"}, Device: &netbox.NestedRef{ID: nbID}},
			{ID: 903, Name: "ipmi0", MgmtOnly: true, MACAddress: "90:5A:08:15:CC:75", Device: &netbox.NestedRef{ID: nbID}},
			{ID: 904, Name: "lo", Type: &netbox.NestedValue{Value: "virtual"}, Device: &netbox.NestedRef{ID: nbID}},
		},
		IPs: []netbox.IPAddress{
			{Address: "10.254.28.1/31", AssignedObjectID: new(901), AssignedObjectType: "dcim.interface"},
			{Address: "10.254.28.3/31", AssignedObjectID: new(902), AssignedObjectType: "dcim.interface"},
			{Address: "10.254.31.97/32", AssignedObjectID: new(904), AssignedObjectType: "dcim.interface"},
			{Address: "10.254.241.9/22", AssignedObjectID: new(903), AssignedObjectType: "dcim.interface"},
		},
		Machines: []model.Machine{
			{ID: mID, NetBoxDeviceID: &nbID, NetBoxSerial: "S500001X0000001", State: model.MachineStateRunning},
		},
		NetworkScripts: []model.NetworkScript{
			{Name: "pop1-l3", Priority: 100, Content: script},
		},
		HostnameTemplate: mustParseHostnameTemplate(t, internalZoneTemplate),
	}
	snap, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Machines[0].FQDN; got != "kworker-l34-1.internal.pop1.example.net" {
		t.Errorf("FQDN = %q, want the template's expansion", got)
	}
	ns := snap.Machines[0].NetworkScript
	if ns == nil {
		t.Fatal("expected a matched network script, got nil")
	}
	if ns.Name != "pop1-l3" || ns.Content != script {
		t.Errorf("matched script = %+v, want name=pop1-l3 with verbatim body", ns)
	}
	// The raw interface facts the script will consume must be present —
	// slagworks ships them; it does not interpret them.
	var loSeen, legs int
	for _, iface := range snap.Machines[0].Interfaces {
		if iface.IP == "10.254.31.97/32" {
			loSeen++
		}
		if strings.HasSuffix(iface.IP, "/31") {
			legs++
		}
	}
	if loSeen != 1 || legs != 2 {
		t.Errorf("expected loopback + 2 /31 legs in shipped facts, got lo=%d legs=%d", loSeen, legs)
	}
}

// TestBuildNoNetworkScriptMatch confirms a device whose identity matches
// no script gets a nil NetworkScript (agent DHCP fallback).
func TestBuildNoNetworkScriptMatch(t *testing.T) {
	mID := uuid.MustParse("00000000-0000-0000-0000-000000000010")
	nbID := 802
	in := Inputs{
		Site:     netbox.Site{Name: "pop1", Slug: "pop1"},
		Config:   model.SiteConfig{Slug: "pop1"},
		Prefixes: []netbox.Prefix{{Prefix: "10.254.28.0/24", CustomFields: gatewayJSON("10.254.28.1")}},
		Devices: []netbox.Device{{
			ID: nbID, Name: "oob-gw-1.pop1", Serial: "ZZ1",
			Site: &netbox.NestedRef{Slug: "pop1"},
		}},
		Machines: []model.Machine{
			{ID: mID, NetBoxDeviceID: &nbID, NetBoxSerial: "ZZ1", State: model.MachineStateRunning},
		},
		NetworkScripts: []model.NetworkScript{
			// Only matches kworkers by hostname regex; oob-gw won't match.
			{Name: "pop1-kworkers", Priority: 100, Content: "#!/usr/bin/env bash\n# slagworks:match hostname=/kworker-.*/\n# slagworks:priority 100\n"},
		},
	}
	snap, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Machines[0].NetworkScript != nil {
		t.Errorf("expected nil NetworkScript for non-matching host, got %+v", snap.Machines[0].NetworkScript)
	}
}

// TestBuildOmitsNetBoxFieldsWhenDeviceUnbound covers the case where a
// Machine row exists but isn't bound to a NetBox device (e.g. the
// device was deleted and not yet re-created). The builder should
// surface the row with empty netbox-sourced fields rather than failing.
func TestBuildOmitsNetBoxFieldsWhenDeviceUnbound(t *testing.T) {
	mID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	in := Inputs{
		Site:     netbox.Site{Name: "pop3", Slug: "pop3"},
		Config:   model.SiteConfig{Slug: "pop3", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200"},
		Prefixes: []netbox.Prefix{{Prefix: "10.0.0.0/24", CustomFields: gatewayJSON("10.0.0.1")}},
		Machines: []model.Machine{
			{ID: mID, NetBoxDeviceID: nil, NetBoxSerial: "SN-A", State: model.MachineStateNew},
		},
	}
	snap, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Machines) != 1 {
		t.Fatal("unbound machine should still appear in output")
	}
	m := snap.Machines[0]
	if m.NetBoxDeviceID != nil {
		t.Fatal("expected nil device id")
	}
	if m.Hostname != "" || m.BMCIP != "" || len(m.Interfaces) != 0 {
		t.Fatalf("expected empty netbox-sourced fields: %+v", m)
	}
	if m.State != model.MachineStateNew {
		t.Fatalf("state %s", m.State)
	}
}

func TestBuildFailsWithoutProductionPrefix(t *testing.T) {
	in := Inputs{
		Site:   netbox.Site{Slug: "pop3"},
		Config: model.SiteConfig{Slug: "pop3", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200"},
	}
	if _, err := Build(in); err == nil {
		t.Fatal("expected error when no prefix tagged slagworks-production")
	}
}

// TestBuildPopulatesManufacturerAndModel: the builder must stamp the
// device-type's manufacturer slug and model onto the snapshot, sourced from
// the same per-device DeviceType lookup that feeds KernelParams.
func TestBuildPopulatesManufacturerAndModel(t *testing.T) {
	mID := uuid.MustParse("00000000-0000-0000-0000-000000000020")
	nbID := 55
	dtID := 12
	in := Inputs{
		Site:     netbox.Site{Name: "pop1", Slug: "pop1"},
		Config:   model.SiteConfig{Slug: "pop1", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200"},
		Prefixes: []netbox.Prefix{{Prefix: "10.0.0.0/24", CustomFields: gatewayJSON("10.0.0.1")}},
		Devices: []netbox.Device{{
			ID: nbID, Name: "kworker-l34-1.pop1", Serial: "SN-SMC",
			DeviceType: &netbox.NestedRef{ID: dtID},
		}},
		DeviceTypes: []netbox.DeviceType{{
			ID:           dtID,
			Model:        "AS-1115HS-TNR",
			Manufacturer: &netbox.Manufacturer{ID: 12, Name: "Supermicro", Slug: "supermicro"},
		}},
		Machines: []model.Machine{
			{ID: mID, NetBoxDeviceID: &nbID, NetBoxSerial: "SN-SMC", State: model.MachineStateRunning},
		},
	}
	snap, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	m := snap.Machines[0]
	if m.Manufacturer != "supermicro" {
		t.Errorf("Manufacturer = %q, want supermicro", m.Manufacturer)
	}
	if m.Model != "AS-1115HS-TNR" {
		t.Errorf("Model = %q, want AS-1115HS-TNR", m.Model)
	}
}

// TestBuildLeavesVendorEmptyWithoutManufacturer covers a device-type with
// no manufacturer set: Manufacturer must stay empty so the worker takes the
// generic bmclib fallback (the correct default), while Model still
// populates from the device-type.
func TestBuildLeavesVendorEmptyWithoutManufacturer(t *testing.T) {
	mID := uuid.MustParse("00000000-0000-0000-0000-000000000021")
	nbID := 56
	dtID := 99
	in := Inputs{
		Site:     netbox.Site{Name: "pop3", Slug: "pop3"},
		Config:   model.SiteConfig{Slug: "pop3", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200"},
		Prefixes: []netbox.Prefix{{Prefix: "10.0.0.0/24", CustomFields: gatewayJSON("10.0.0.1")}},
		Devices: []netbox.Device{{
			ID: nbID, Name: "host-c", Serial: "SN-C",
			DeviceType: &netbox.NestedRef{ID: dtID},
		}},
		DeviceTypes: []netbox.DeviceType{{ID: dtID, Model: "RS501A-E11"}},
		Machines: []model.Machine{
			{ID: mID, NetBoxDeviceID: &nbID, NetBoxSerial: "SN-C", State: model.MachineStateRunning},
		},
	}
	snap, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	m := snap.Machines[0]
	if m.Manufacturer != "" {
		t.Errorf("Manufacturer = %q, want empty (generic fallback)", m.Manufacturer)
	}
	if m.Model != "RS501A-E11" {
		t.Errorf("Model = %q, want RS501A-E11", m.Model)
	}
}

func TestBuildUsesInterfacePrimaryMACFallback(t *testing.T) {
	// NetBox 4.3+ can return an Interface with a nil top-level
	// mac_address and the actual MAC in primary_mac_address. The builder
	// should call GetMAC() which handles the fallback.
	mID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	nbID := 10
	in := Inputs{
		Site:     netbox.Site{Slug: "pop3"},
		Config:   model.SiteConfig{Slug: "pop3", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200"},
		Prefixes: []netbox.Prefix{{Prefix: "10.0.0.0/24", CustomFields: gatewayJSON("10.0.0.1")}},
		Devices:  []netbox.Device{{ID: nbID, Name: "host-b"}},
		Interfaces: []netbox.Interface{{
			ID: 200, Name: "eno1",
			Device:     &netbox.NestedRef{ID: nbID},
			PrimaryMAC: &netbox.MACAddressRef{MACAddress: "AA:BB:CC:00:00:01"},
		}},
		Machines: []model.Machine{{ID: mID, NetBoxDeviceID: &nbID, NetBoxSerial: "SN", State: model.MachineStateNew}},
	}
	snap, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Machines[0].Interfaces) != 1 ||
		snap.Machines[0].Interfaces[0].MAC != "aa:bb:cc:00:00:01" {
		t.Fatalf("unexpected: %+v", snap.Machines[0].Interfaces)
	}
}

// findIface returns the SnapshotInterface with the given name, or fails.
func findIface(t *testing.T, ifaces []model.SnapshotInterface, name string) model.SnapshotInterface {
	t.Helper()
	for _, i := range ifaces {
		if i.Name == name {
			return i
		}
	}
	t.Fatalf("interface %q not in %+v", name, ifaces)
	return model.SnapshotInterface{}
}

// TestBuildPopulatesBondFacts is the L2-bond facts-extension lock: a device
// modeled like hot-spare-1.pop2 (a type=lag bond carrying the host IP, two
// member NICs whose lag points at the bond, an untagged VLAN, an MTU, and a
// /22 prefix with a gateway_ip custom field) must yield SnapshotInterfaces
// carrying LAG/VLAN/Gateway/MTU so the bond network-config script can
// reconstruct the topology.
func TestBuildPopulatesBondFacts(t *testing.T) {
	mID := uuid.MustParse("00000000-0000-0000-0000-000000000030")
	nbID := 1115
	in := Inputs{
		Site:   netbox.Site{Name: "pop2", Slug: "pop2"},
		Config: model.SiteConfig{Slug: "pop2", EphemeralPoolStart: "10.253.0.100", EphemeralPoolEnd: "10.253.0.200"},
		// /22 prefix carries the gateway_ip CF; a broad /16 supernet also
		// matches but must lose the longest-prefix-match.
		Prefixes: []netbox.Prefix{
			{Prefix: "10.253.0.0/22", CustomFields: gatewayJSON("10.253.3.254")},
			{Prefix: "10.253.0.0/16", CustomFields: gatewayJSON("10.253.255.254")},
		},
		Devices: []netbox.Device{{
			ID: nbID, Name: "hot-spare-1.pop2", Serial: "SN-HS1",
			Site: &netbox.NestedRef{Slug: "pop2"},
		}},
		Interfaces: []netbox.Interface{
			// bond0: type=lag, carries the host IP, no MAC, no lag ref.
			{ID: 10, Name: "bond0", Type: &netbox.NestedValue{Value: "lag"},
				UntaggedVLAN: &netbox.NestedVLAN{ID: 1, VID: 1, Name: "default"},
				MTU:          new(1500),
				Device:       &netbox.NestedRef{ID: nbID}},
			// member NICs: lag -> bond0.
			{ID: 11, Name: "enp129s0f0np0", MACAddress: "7C:C2:55:42:FC:30",
				Type: &netbox.NestedValue{Value: "25gbase-x-sfp28"},
				LAG:  &netbox.NestedRef{ID: 10, Name: "bond0"}, MTU: new(1500),
				Device: &netbox.NestedRef{ID: nbID}},
			{ID: 12, Name: "enp129s0f1np1", MACAddress: "7C:C2:55:42:FC:31",
				Type: &netbox.NestedValue{Value: "25gbase-x-sfp28"},
				LAG:  &netbox.NestedRef{ID: 10, Name: "bond0"}, MTU: new(1500),
				Device: &netbox.NestedRef{ID: nbID}},
			// ipmi: mgmt_only, on a different /23.
			{ID: 13, Name: "ipmi0", MgmtOnly: true, MACAddress: "3C:EC:EF:DE:D0:89",
				Device: &netbox.NestedRef{ID: nbID}},
		},
		IPs: []netbox.IPAddress{
			{Address: "10.253.0.22/22", AssignedObjectID: new(10), AssignedObjectType: "dcim.interface"},
			{Address: "10.253.241.108/23", AssignedObjectID: new(13), AssignedObjectType: "dcim.interface"},
		},
		Machines: []model.Machine{
			{ID: mID, NetBoxDeviceID: &nbID, NetBoxSerial: "SN-HS1", State: model.MachineStateRunning},
		},
	}
	snap, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	ifaces := snap.Machines[0].Interfaces

	bond := findIface(t, ifaces, "bond0")
	if bond.Type != "lag" {
		t.Errorf("bond0 Type = %q, want lag", bond.Type)
	}
	if bond.IP != "10.253.0.22/22" {
		t.Errorf("bond0 IP = %q, want 10.253.0.22/22", bond.IP)
	}
	if bond.LAG != "" {
		t.Errorf("bond0 LAG = %q, want empty (the bond is the parent)", bond.LAG)
	}
	if bond.VLAN != 1 {
		t.Errorf("bond0 VLAN = %d, want 1 (untagged PVID)", bond.VLAN)
	}
	if bond.MTU != 1500 {
		t.Errorf("bond0 MTU = %d, want 1500", bond.MTU)
	}
	// Longest-prefix-match: the /22 gateway_ip wins over the /16.
	if bond.Gateway != "10.253.3.254" {
		t.Errorf("bond0 Gateway = %q, want 10.253.3.254 (the /22 gateway_ip, not the /16)", bond.Gateway)
	}

	for _, name := range []string{"enp129s0f0np0", "enp129s0f1np1"} {
		m := findIface(t, ifaces, name)
		if m.LAG != "bond0" {
			t.Errorf("%s LAG = %q, want bond0", name, m.LAG)
		}
		if m.Type != "physical" {
			t.Errorf("%s Type = %q, want physical", name, m.Type)
		}
		if m.MAC == "" {
			t.Errorf("%s MAC empty; bond members must carry their MAC for match-by-MAC", name)
		}
	}
}

// TestBuildGatewayLongestPrefixWins locks the longest-prefix-match semantics
// of the per-interface gateway resolver, including the subtle case the roast
// caught: a host inside a specific gateway-less /24 that is also inside a
// gateway-bearing /16 must resolve to NO gateway (the most-specific containing
// prefix's gateway_ip, even when empty) — never the /16's gateway, which is
// off the host's L2 segment.
func TestBuildGatewayLongestPrefixWins(t *testing.T) {
	mID := uuid.MustParse("00000000-0000-0000-0000-000000000032")
	nbID := 710
	in := Inputs{
		Site:   netbox.Site{Name: "pop2", Slug: "pop2"},
		Config: model.SiteConfig{Slug: "pop2", EphemeralPoolStart: "10.253.0.100", EphemeralPoolEnd: "10.253.0.200"},
		Prefixes: []netbox.Prefix{
			// index 0 (DHCP primary) must carry a gateway; use the /22.
			{Prefix: "10.253.0.0/22", CustomFields: gatewayJSON("10.253.3.254")},
			// a gateway-LESS specific /24 enclosed by a gateway-bearing /16.
			{Prefix: "10.253.8.0/24"},
			{Prefix: "10.253.0.0/16", CustomFields: gatewayJSON("10.253.255.254")},
		},
		Devices: []netbox.Device{{ID: nbID, Name: "host-lp", Serial: "SN-LP", Site: &netbox.NestedRef{Slug: "pop2"}}},
		Interfaces: []netbox.Interface{
			// IP inside the gateway-less /24 (and the /16).
			{ID: 711, Name: "eno1", MACAddress: "AA:BB:CC:00:08:01", Type: &netbox.NestedValue{Value: "1000base-t"}, Device: &netbox.NestedRef{ID: nbID}},
			// IP inside the /22 (and the /16).
			{ID: 712, Name: "eno2", MACAddress: "AA:BB:CC:00:00:01", Type: &netbox.NestedValue{Value: "1000base-t"}, Device: &netbox.NestedRef{ID: nbID}},
		},
		IPs: []netbox.IPAddress{
			{Address: "10.253.8.5/24", AssignedObjectID: new(711), AssignedObjectType: "dcim.interface"},
			{Address: "10.253.0.22/22", AssignedObjectID: new(712), AssignedObjectType: "dcim.interface"},
		},
		Machines: []model.Machine{{ID: mID, NetBoxDeviceID: &nbID, NetBoxSerial: "SN-LP", State: model.MachineStateRunning}},
	}
	snap, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	ifaces := snap.Machines[0].Interfaces
	if gw := findIface(t, ifaces, "eno1").Gateway; gw != "" {
		t.Errorf("eno1 in gateway-less /24 Gateway = %q, want empty (must NOT inherit the /16 gateway)", gw)
	}
	if gw := findIface(t, ifaces, "eno2").Gateway; gw != "10.253.3.254" {
		t.Errorf("eno2 in /22 Gateway = %q, want 10.253.3.254", gw)
	}
}

// TestBuildGatewayResolvesFromOOBPrefix guards the fix that feeds slagworks-oob
// prefixes (not just slagworks-production) to the per-interface gateway
// resolver: an OOB-segment NIC (e.g. an oob-gw's eno3) must resolve its Gateway
// from the OOB prefix's gateway_ip CF — the OOB VIP. Before the fix the
// resolver saw only production prefixes, so every OOB NIC's Gateway came through
// empty even though the OOB prefix models a gateway, and the new-cage oob-gw
// netscript could not emit its transitional default route. The two tag sets
// cover disjoint subnets, so this must not perturb a production NIC's gateway.
func TestBuildGatewayResolvesFromOOBPrefix(t *testing.T) {
	mID := uuid.MustParse("00000000-0000-0000-0000-000000000033")
	nbID := 720
	in := Inputs{
		Site:   netbox.Site{Name: "pop2", Slug: "pop2"},
		Config: model.SiteConfig{Slug: "pop2", EphemeralPoolStart: "10.253.0.100", EphemeralPoolEnd: "10.253.0.200"},
		// Production prefixes (index 0 is the DHCP primary). The OOB /23 is
		// deliberately NOT here — it lives in OOBPrefixes, tag=slagworks-oob.
		Prefixes: []netbox.Prefix{
			{Prefix: "10.253.0.0/22", CustomFields: gatewayJSON("10.253.3.254")},
		},
		// The OOB segment, modeling the VIP as its gateway_ip — same shape as
		// the live pop2 new-cage prefix 10.253.242.0/23 (gateway_ip
		// 10.253.243.252).
		OOBPrefixes: []netbox.Prefix{
			{Prefix: "10.253.242.0/23", CustomFields: gatewayJSON("10.253.243.252")},
		},
		Devices: []netbox.Device{{ID: nbID, Name: "oob-gw-x", Serial: "SN-OOB", Site: &netbox.NestedRef{Slug: "pop2"}}},
		Interfaces: []netbox.Interface{
			// eno3 = OOB management leg, IP inside the slagworks-oob /23.
			{ID: 721, Name: "eno3", MACAddress: "AA:BB:CC:00:F3:01", Type: &netbox.NestedValue{Value: "1000base-t"}, Device: &netbox.NestedRef{ID: nbID}},
		},
		IPs: []netbox.IPAddress{
			{Address: "10.253.243.254/23", AssignedObjectID: new(721), AssignedObjectType: "dcim.interface"},
		},
		Machines: []model.Machine{{ID: mID, NetBoxDeviceID: &nbID, NetBoxSerial: "SN-OOB", State: model.MachineStateRunning}},
	}
	snap, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	ifaces := snap.Machines[0].Interfaces
	if gw := findIface(t, ifaces, "eno3").Gateway; gw != "10.253.243.252" {
		t.Errorf("eno3 on the OOB /23 Gateway = %q, want 10.253.243.252 (the OOB prefix's gateway_ip / VIP)", gw)
	}
}

// TestBuildL3InterfaceFactsUnaffected guards the additive contract: an L3
// routed-NIC kworker (no lag / no untagged_vlan / no modeled mtu) must produce
// interface facts whose L2-only fields (LAG/VLAN/MTU) are all zero-valued. The
// L3 network-config script reads only Name/Type/IP, so those three must be
// exactly the shape they are today. (Gateway MAY be populated from an enclosing
// prefix's gateway_ip, but the L3 script never reads it — so it is not part of
// the contract this test pins.) A regression in LAG/VLAN/MTU would mean the
// facts extension leaked L2 topology onto an L3 host.
func TestBuildL3InterfaceFactsUnaffected(t *testing.T) {
	mID := uuid.MustParse("00000000-0000-0000-0000-000000000031")
	nbID := 700
	in := Inputs{
		Site:     netbox.Site{Name: "pop1", Slug: "pop1"},
		Config:   model.SiteConfig{Slug: "pop1"},
		Prefixes: []netbox.Prefix{{Prefix: "10.254.28.0/24", CustomFields: gatewayJSON("10.254.28.1")}},
		Devices: []netbox.Device{{
			ID: nbID, Name: "kworker-l34-1.pop1", Serial: "SN-K1",
			Site: &netbox.NestedRef{Slug: "pop1"},
		}},
		Interfaces: []netbox.Interface{
			{ID: 701, Name: "eth0", MACAddress: "7C:C2:55:BE:F7:98", Type: &netbox.NestedValue{Value: "25gbase-x-sfp28"}, Device: &netbox.NestedRef{ID: nbID}},
			{ID: 702, Name: "eth1", MACAddress: "7C:C2:55:BE:F7:99", Type: &netbox.NestedValue{Value: "25gbase-x-sfp28"}, Device: &netbox.NestedRef{ID: nbID}},
			{ID: 703, Name: "lo", Type: &netbox.NestedValue{Value: "virtual"}, Device: &netbox.NestedRef{ID: nbID}},
		},
		IPs: []netbox.IPAddress{
			{Address: "10.254.28.1/31", AssignedObjectID: new(701), AssignedObjectType: "dcim.interface"},
			{Address: "10.254.28.3/31", AssignedObjectID: new(702), AssignedObjectType: "dcim.interface"},
			{Address: "10.254.31.97/32", AssignedObjectID: new(703), AssignedObjectType: "dcim.interface"},
		},
		Machines: []model.Machine{
			{ID: mID, NetBoxDeviceID: &nbID, NetBoxSerial: "SN-K1", State: model.MachineStateRunning},
		},
	}
	snap, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, iface := range snap.Machines[0].Interfaces {
		if iface.LAG != "" {
			t.Errorf("%s LAG = %q, want empty on L3 host", iface.Name, iface.LAG)
		}
		if iface.VLAN != 0 {
			t.Errorf("%s VLAN = %d, want 0 on L3 host", iface.Name, iface.VLAN)
		}
		if iface.MTU != 0 {
			t.Errorf("%s MTU = %d, want 0 on L3 host (script applies its own)", iface.Name, iface.MTU)
		}
		if iface.Type != "physical" {
			t.Errorf("%s Type = %q, want physical on L3 host", iface.Name, iface.Type)
		}
	}
}
