//go:build !integration

package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bluesky-social/slagworks/internal/storage"
)

const pop2KworkerInventory = `{
  "disks": [
    {"name":"nvme0n1","model":"MZQL23T8HCLS-00A07","size_bytes":3840755982336,"transport":"nvme"},
    {"name":"nvme1n1","model":"MZQL23T8HCLS-00A07","size_bytes":3840755982336,"transport":"nvme"},
    {"name":"nvme2n1","model":"MZQL23T8HCLS-00A07","size_bytes":3840755982336,"transport":"nvme"},
    {"name":"nvme3n1","model":"MZQL23T8HCLS-00A07","size_bytes":3840755982336,"transport":"nvme"},
    {"name":"nvme4n1","model":"MZ1L23T8HBLA-00A07","size_bytes":3840755982336,"transport":"nvme"},
    {"name":"nvme5n1","model":"MZ1L23T8HBLA-00A07","size_bytes":3840755982336,"transport":"nvme"},
    {"name":"nvme6n1","model":"MZQL23T8HCLS-00A07","size_bytes":3840755982336,"transport":"nvme"},
    {"name":"nvme7n1","model":"MZQL23T8HCLS-00A07","size_bytes":3840755982336,"transport":"nvme"},
    {"name":"nvme8n1","model":"MZQL23T8HCLS-00A07","size_bytes":3840755982336,"transport":"nvme"},
    {"name":"nvme9n1","model":"MZQL23T8HCLS-00A07","size_bytes":3840755982336,"transport":"nvme"}
  ]
}`

func TestPop2KworkerStorage_RetainsEveryDataDisk(t *testing.T) {
	script := dellScriptPath(t, "storage", "250-pop2-kworker-preserve-lvm")
	out, stderr, err := runScript(t, script, "SLAGWORKS_INVENTORY", pop2KworkerInventory)
	if err != nil {
		t.Fatalf("script failed: %v\nstderr: %s", err, stderr)
	}

	var layout storage.Layout
	if err := json.Unmarshal([]byte(out), &layout); err != nil {
		t.Fatalf("layout is not valid JSON: %v\nstdout: %s", err, out)
	}
	for _, disk := range []string{"nvme0n1", "nvme1n1", "nvme2n1", "nvme3n1", "nvme6n1", "nvme7n1", "nvme8n1", "nvme9n1"} {
		if got := layout.Devices[disk].Type; got != "retain" {
			t.Errorf("%s type = %q, want retain", disk, got)
		}
	}
	for _, disk := range []string{"nvme4n1", "nvme5n1"} {
		if got := layout.Devices[disk].Type; got != "disk" {
			t.Errorf("%s type = %q, want disk", disk, got)
		}
	}

	var inventory struct {
		Disks []storage.BlockDevice `json:"disks"`
	}
	if err := json.Unmarshal([]byte(pop2KworkerInventory), &inventory); err != nil {
		t.Fatalf("parse inventory fixture: %v", err)
	}
	commands, err := storage.Plan(layout, inventory.Disks)
	if err != nil {
		t.Fatalf("plan emitted layout: %v", err)
	}
	for _, command := range commands {
		for _, disk := range []string{"nvme0n1", "nvme1n1", "nvme2n1", "nvme3n1", "nvme6n1", "nvme7n1", "nvme8n1", "nvme9n1"} {
			if strings.Contains(command.String(), disk) {
				t.Fatalf("retained disk %s appears in command: %s", disk, command)
			}
		}
	}
}

func TestPop2KworkerStorage_HardFailsUnlessEightDataDisksPresent(t *testing.T) {
	var inventory struct {
		Disks []map[string]any `json:"disks"`
	}
	if err := json.Unmarshal([]byte(pop2KworkerInventory), &inventory); err != nil {
		t.Fatalf("parse inventory fixture: %v", err)
	}
	inventory.Disks = inventory.Disks[:9]
	encoded, err := json.Marshal(inventory)
	if err != nil {
		t.Fatalf("marshal short inventory: %v", err)
	}

	script := dellScriptPath(t, "storage", "250-pop2-kworker-preserve-lvm")
	if _, stderr, err := runScript(t, script, "SLAGWORKS_INVENTORY", string(encoded)); err == nil {
		t.Fatal("expected script to fail with seven data disks")
	} else if !strings.Contains(stderr, "expected 8 retained data drives, found 7") {
		t.Fatalf("expected clear retained-disk count error, got %s", stderr)
	}
}

func TestPop2KworkerStorage_QEMUUsesSyntheticBootSerialsOnlyForQEMUModel(t *testing.T) {
	var inventory struct {
		System map[string]any   `json:"system"`
		Disks  []map[string]any `json:"disks"`
	}
	if err := json.Unmarshal([]byte(pop2KworkerInventory), &inventory); err != nil {
		t.Fatalf("parse inventory fixture: %v", err)
	}
	inventory.System = map[string]any{"manufacturer": "QEMU"}
	for i, disk := range inventory.Disks {
		disk["model"] = "QEMU NVMe Ctrl"
		if i == 4 || i == 5 {
			disk["serial"] = "MZ1L23T8HBLA-QEMU-" + string(rune('A'+i))
		} else {
			disk["serial"] = "MZQL23T8HCLS-QEMU-" + string(rune('A'+i))
		}
	}
	encoded, err := json.Marshal(inventory)
	if err != nil {
		t.Fatalf("marshal QEMU inventory: %v", err)
	}

	script := dellScriptPath(t, "storage", "250-pop2-kworker-preserve-lvm")
	if _, stderr, err := runScript(t, script, "SLAGWORKS_INVENTORY", string(encoded)); err != nil {
		t.Fatalf("QEMU inventory failed: %v\nstderr: %s", err, stderr)
	}

	for _, disk := range inventory.Disks {
		disk["model"] = "MZQL23T8HCLS-00A07"
	}
	encoded, err = json.Marshal(inventory)
	if err != nil {
		t.Fatalf("marshal non-QEMU inventory: %v", err)
	}
	if _, stderr, err := runScript(t, script, "SLAGWORKS_INVENTORY", string(encoded)); err == nil {
		t.Fatal("synthetic serial fallback must fail closed without QEMU's controller model")
	} else if !strings.Contains(stderr, "expected 2 PM9A3 boot drives") {
		t.Fatalf("expected model-match failure outside QEMU, got %s", stderr)
	}
}
