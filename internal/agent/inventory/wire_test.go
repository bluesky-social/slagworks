package inventory

import (
	"encoding/json"
	"sort"
	"testing"

	"github.com/bluesky-social/slagworks/internal/model"
)

// TestCommissionResultWireKeys pins the exact set of top-level JSON keys
// on the commission POST body. internal/netbox/sync.go deserializes this
// payload into private mirror structs; a silent key rename or drop here
// would break that consumer with no compile-time signal (the producer
// and consumer share no Go type). Every field is populated — including
// the omitempty ones — so they all render.
func TestCommissionResultWireKeys(t *testing.T) {
	rota := true
	res := &CommissionResult{
		Hostname:   "host1",
		System:     &SystemInfo{Manufacturer: "ASUS"},
		CPUs:       []CPUInfo{{Model: "epyc"}},
		Memory:     &MemoryInfo{TotalMB: 1},
		Disks:      []DiskInfo{{Name: "nvme0n1", Rotational: &rota}},
		NICs:       []NICInfo{{Name: "eth0"}},
		PCIDevices: []PCIDevice{{Slot: "00:00.0"}},
		LLDP:       json.RawMessage(`{}`),
		BMC:        &BMCInfo{IP: "10.0.0.1"},
		PSUs:       []PSUInfo{{Location: "PSU1"}},
		NUMA:       []NUMANode{{ID: 0}},
		USB:        []USBDevice{{Bus: "001"}},
		Steps:      []CommissionStep{{Name: "lscpu"}},
		ConsoleSweep: &model.ConsoleSweep{
			Candidates: []model.ConsoleSweepCandidate{{TTY: "ttyS1", Baud: 115200, Nonce: "abc"}},
		},
	}

	data, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	got := make([]string, 0, len(top))
	for k := range top {
		got = append(got, k)
	}
	sort.Strings(got)

	want := []string{
		"bmc", "console_sweep", "cpus", "disks", "hostname", "lldp",
		"memory", "nics", "numa", "pci_devices", "psus", "steps",
		"system", "usb",
	}
	sort.Strings(want)

	if len(got) != len(want) {
		t.Fatalf("top-level key count: got %d %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("top-level keys mismatch:\n got: %v\nwant: %v", got, want)
		}
	}
}
