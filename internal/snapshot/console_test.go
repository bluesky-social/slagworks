package snapshot

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

// consolePort builds a NetBox console port on devID cabled to serverID's
// console-server port named port (pass serverID 0 for an uncabled port).
func consolePort(id, devID int, devName string, serverID int, serverName, port string, speed int) netbox.ConsolePort {
	p := netbox.ConsolePort{
		ID:     id,
		Name:   "console",
		Device: &netbox.NestedRef{ID: devID, Name: devName},
	}
	if speed > 0 {
		p.Speed = &netbox.NestedIntValue{Value: speed}
	}
	if serverID != 0 {
		p.ConnectedEndpointsType = "dcim.consoleserverport"
		p.ConnectedEndpointsReachable = true
		p.ConnectedEndpoints = []netbox.ConsolePortEndpoint{{
			ID: id + 1000, Name: port, Device: &netbox.NestedRef{ID: serverID, Name: serverName},
		}}
	}
	return p
}

// TestBuildConsoleEndpoint covers the pop1 shape: switches cabled to an
// oob-gw's ttySn ports, one uncabled console port, a non-machine device
// on the same console server, and a NetBox port with no speed set.
func TestBuildConsoleEndpoint(t *testing.T) {
	const gwID = 583
	spineID, leafID, unc, noSpeed := 577, 579, 584, 590
	spine, leaf, uncabled, slow := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	in := Inputs{
		Site:     netbox.Site{Name: "pop1", Slug: "pop1"},
		Config:   model.SiteConfig{Slug: "pop1", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200"},
		Prefixes: []netbox.Prefix{{Prefix: "10.0.0.0/24", CustomFields: gatewayJSON("10.0.0.1")}},
		Devices: []netbox.Device{
			{ID: spineID, Name: "spine-1.pop1", Serial: "MT2308T000Y8"},
			{ID: leafID, Name: "leaf-3.pop1", Serial: "MT2005J02781"},
			{ID: unc, Name: "leaf-6.pop1", Serial: "MT-L6"},
			{ID: noSpeed, Name: "leaf-5.pop1", Serial: "MT-L5"},
			{ID: 585, Name: "leaf-1.pop1", Serial: "MT-L1"}, // not a Slagworks machine
		},
		Machines: []model.Machine{
			{ID: spine, NetBoxDeviceID: &spineID, State: model.MachineStateRunning},
			{ID: leaf, NetBoxDeviceID: &leafID, State: model.MachineStateRunning},
			{ID: uncabled, NetBoxDeviceID: &unc, State: model.MachineStateRunning},
			{ID: slow, NetBoxDeviceID: &noSpeed, State: model.MachineStateRunning},
		},
		ConsolePorts: []netbox.ConsolePort{
			consolePort(1, spineID, "spine-1.pop1", gwID, "oob-gw-1.pop1", "ttyS4", 115200),
			consolePort(2, leafID, "leaf-3.pop1", gwID, "oob-gw-1.pop1", "ttyS8", 9600),
			consolePort(3, unc, "leaf-6.pop1", 0, "", "", 115200),
			consolePort(4, noSpeed, "leaf-5.pop1", gwID, "oob-gw-1.pop1", "ttyS10", 0),
			consolePort(5, 585, "leaf-1.pop1", gwID, "oob-gw-1.pop1", "ttyS6", 115200),
		},
		ConsoleServers: []netbox.Device{{ID: gwID, Name: "oob-gw-1.pop1", Serial: "9HNZ4Z2"}},
	}
	snap, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Machines) != 4 {
		t.Fatalf("machines = %d, want 4 (leaf-1 is not a machine)", len(snap.Machines))
	}
	byID := make(map[uuid.UUID]model.MachineSnapshot)
	for _, m := range snap.Machines {
		byID[m.ID] = m
	}
	want := map[uuid.UUID]*model.SnapshotConsole{
		spine:    {Server: "oob-gw-1.pop1", ServerSerial: "9HNZ4Z2", Port: "ttyS4", Baud: 115200},
		leaf:     {Server: "oob-gw-1.pop1", ServerSerial: "9HNZ4Z2", Port: "ttyS8", Baud: 9600},
		uncabled: nil,
		slow:     {Server: "oob-gw-1.pop1", ServerSerial: "9HNZ4Z2", Port: "ttyS10", Baud: defaultConsoleBaud},
	}
	for id, w := range want {
		got := byID[id].Console
		switch {
		case w == nil && got != nil:
			t.Errorf("%s: Console = %+v, want nil (uncabled)", byID[id].Hostname, *got)
		case w != nil && got == nil:
			t.Errorf("%s: Console = nil, want %+v", byID[id].Hostname, *w)
		case w != nil && *got != *w:
			t.Errorf("%s: Console = %+v, want %+v", byID[id].Hostname, *got, *w)
		}
	}
}

// TestBuildConsoleEndpointAmbiguous: two cabled console ports on one
// device give no endpoint rather than an order-dependent pick; a port
// whose far end is not a console-server port is ignored.
func TestBuildConsoleEndpointAmbiguous(t *testing.T) {
	devID := 10
	mID := uuid.New()
	other := consolePort(3, devID, "sw", 600, "pdu", "x", 0)
	other.ConnectedEndpointsType = "dcim.powerport"
	in := Inputs{
		Site:     netbox.Site{Name: "pop1", Slug: "pop1"},
		Config:   model.SiteConfig{Slug: "pop1", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200"},
		Prefixes: []netbox.Prefix{{Prefix: "10.0.0.0/24", CustomFields: gatewayJSON("10.0.0.1")}},
		Devices:  []netbox.Device{{ID: devID, Name: "sw"}},
		Machines: []model.Machine{{ID: mID, NetBoxDeviceID: &devID}},
		ConsolePorts: []netbox.ConsolePort{
			consolePort(1, devID, "sw", 583, "oob-gw-1.pop1", "ttyS4", 0),
			consolePort(2, devID, "sw", 583, "oob-gw-1.pop1", "ttyS5", 0),
			other,
		},
		ConsoleServers: []netbox.Device{{ID: 583, Name: "oob-gw-1.pop1", Serial: "S"}},
	}
	snap, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if c := snap.Machines[0].Console; c != nil {
		t.Fatalf("Console = %+v, want nil for a device with two cabled console ports", *c)
	}

	in.ConsolePorts = []netbox.ConsolePort{other}
	snap, err = Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if c := snap.Machines[0].Console; c != nil {
		t.Fatalf("Console = %+v, want nil for a non-console-server far end", *c)
	}

	broken := consolePort(1, devID, "sw", 583, "oob-gw-1.pop1", "ttyS4", 0)
	broken.ConnectedEndpointsReachable = false
	in.ConsolePorts = []netbox.ConsolePort{broken}
	snap, err = Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if c := snap.Machines[0].Console; c != nil {
		t.Fatalf("Console = %+v, want nil for an unreachable cable path", *c)
	}
}

// TestDispatcherResolvesConsoleServer: the gather path fetches
// console ports for the scoped devices and resolves the console server's
// serial from the site device list.
func TestDispatcherResolvesConsoleServer(t *testing.T) {
	d, _, fs, nb := dispatcherFixture(t)
	nb.devices = []netbox.Device{
		{ID: 577, Name: "spine-1.pop3", Serial: "MT-S1"},
		{ID: 583, Name: "oob-gw-1.pop3", Serial: "9HNZ4Z2"},
	}
	nb.consolePorts = []netbox.ConsolePort{
		consolePort(1, 577, "spine-1.pop3", 583, "oob-gw-1.pop3", "ttyS4", 115200),
	}
	mID := uuid.New()
	fs.machines = []model.Machine{{ID: mID, NetBoxDeviceID: new(577), State: model.MachineStateRunning}}

	snap, err := d.BuildSnapshot(context.Background(), "pop3")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Machines) != 1 || snap.Machines[0].Console == nil {
		t.Fatalf("machines = %+v, want one with a console endpoint", snap.Machines)
	}
	want := model.SnapshotConsole{Server: "oob-gw-1.pop3", ServerSerial: "9HNZ4Z2", Port: "ttyS4", Baud: 115200}
	if got := *snap.Machines[0].Console; got != want {
		t.Fatalf("Console = %+v, want %+v", got, want)
	}
}
