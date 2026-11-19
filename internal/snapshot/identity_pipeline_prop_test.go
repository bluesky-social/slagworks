package snapshot

// Pipeline property: agent commission report JSON -> extractSystemIdentity /
// extractDiskIdentities (snapshot build) -> MachineSnapshot -> deploy
// preflight identity gaps.
//
// A fleet of physically DISTINCT hosts (unique system serial, unique UUID,
// unique disks, unique MACs) is generated. Fields firmware commonly leaves
// unprogrammed (chassis serial, disk serial/WWN) are drawn from vendor
// default strings. Distinct hosts must never be reported as owning each
// other's identity: the contract is pinned by
// TestPreflight_PlaceholderSerialsDoNotCreateForeignOwnership and
// TestPreflight_PlaceholderDiskIdentityDoesNotCreateOwnership.
//
// The vendor-default list is the set model.NormalizeHardwareIdentifier
// documents as firmware placeholders.

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

// pipelinePlaceholders are values NormalizeHardwareIdentifier documents.
var pipelinePlaceholders = []string{
	"", "To Be Filled By O.E.M.", "Default string", "Not Specified", "System Serial Number",
	"Chassis Serial Number", "0", "00", "0000000000", "None", "N/A", "0123456789",
}

type pipelineHost struct {
	m       model.MachineSnapshot
	dev     netbox.Device
	report  []byte
	chassis string
}

func genPipelineFleet(t *rapid.T, vendorDefaultSerials []string) []pipelineHost {
	n := rapid.IntRange(2, 5).Draw(t, "hosts")
	hosts := make([]pipelineHost, n)
	for i := range hosts {
		serial := fmt.Sprintf("S515AB%04d", i+1)
		sysUUID := uuid.New().String()
		chassis := rapid.SampledFrom(append([]string{fmt.Sprintf("CH119%04d", i+1)}, vendorDefaultSerials...)).Draw(t, "chassis")
		var disks []map[string]string
		for k := 0; k < rapid.IntRange(1, 3).Draw(t, "disks"); k++ {
			dserial := fmt.Sprintf("DISK-%d-%d", i, k)
			if rapid.IntRange(0, 4).Draw(t, "diskSerialPlaceholder") == 0 {
				dserial = rapid.SampledFrom(vendorDefaultSerials).Draw(t, "diskSerialVal")
			}
			wwn := fmt.Sprintf("0x5000c500%04x%04x", i, k)
			if rapid.IntRange(0, 4).Draw(t, "wwnPlaceholder") == 0 {
				wwn = rapid.SampledFrom([]string{"", "0x0000000000000000"}).Draw(t, "wwnVal")
			}
			disks = append(disks, map[string]string{"name": fmt.Sprintf("nvme%dn1", k), "serial": dserial, "wwn": wwn})
		}
		report, _ := json.Marshal(map[string]any{
			"system": map[string]any{"serial_number": serial, "chassis_serial": chassis, "uuid": sysUUID},
			"disks":  disks,
		})
		sys, ch, u := extractSystemIdentity(report)
		devID := 100 + i
		m := model.MachineSnapshot{
			ID:             uuid.New(),
			NetBoxDeviceID: &devID,
			NetBoxSerial:   serial,
			Hostname:       fmt.Sprintf("host-%d", i),
			SystemSerial:   sys,
			ChassisSerial:  ch,
			SystemUUID:     u,
			Disks:          extractDiskIdentities(report),
			Interfaces:     []model.SnapshotInterface{{Name: "eno1", MAC: fmt.Sprintf("3c:ec:ef:00:00:%02x", i), Type: "physical"}},
		}
		hosts[i] = pipelineHost{
			m:       m,
			dev:     netbox.Device{ID: devID, Name: m.Hostname, Serial: serial},
			report:  report,
			chassis: chassis,
		}
	}
	return hosts
}

// TestPreflight_Property_DistinctHostsNeverCrossAttributed: no host of a fleet
// of distinct hosts gets a deploy identity gap, whatever placeholders its
// firmware reports.
func TestPreflight_Property_DistinctHostsNeverCrossAttributed(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		hosts := genPipelineFleet(t, pipelinePlaceholders)
		var fleet []model.MachineSnapshot
		for _, h := range hosts {
			fleet = append(fleet, h.m)
		}
		for i := range hosts {
			m := hosts[i].m
			gaps := preflightIdentityGaps(PreflightInput{Machine: &m, Machines: fleet, Device: &hosts[i].dev}, model.OpDeploy)
			if len(gaps) != 0 {
				t.Fatalf("distinct host %s got identity gaps %q\nreport: %s", m.Hostname, gaps, hosts[i].report)
			}
		}
	})
}
