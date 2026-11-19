package worker

// Property-based coverage for the site worker's commission identity gate
// (Cache.HandleProgressReport -> validateCommissionIdentity): the check that
// keeps a commission report from one physical host being persisted as
// another machine's inventory.
//
// A small fleet (target + up to three siblings) is generated over SMALL
// identity pools so serials, UUIDs, MACs and disk identifiers collide often.
// The report is built from the same pools with case / whitespace /
// placeholder variants. The verdict is compared with an independent
// reference model written from the gate's doc comment and the identity
// contract preflight enforces (internal/snapshot/preflight.go): placeholder
// identifiers never establish identity, comparisons are case- and
// whitespace-insensitive.

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
)

var (
	idGateSerialPool   = []string{"S515AB0001", "S515AB0002", "S515AB0003", "CH1190001"}
	idGatePlaceholders = []string{"", "To Be Filled By O.E.M.", "Default string", "0", "00000000", "Not Specified", "System Serial Number", "0123456789"}
	idGateUUIDPool     = []string{
		"4f1d62c0-1f7d-4e74-a57c-a58932e2aa11",
		"8a3c1e7b-9d1f-4c2a-b3e4-5f6a7b8c9d0e",
		"00000000-0000-0000-0000-000000000000",
		"03000200-0400-0500-0006-000700080009",
	}
	idGateMACPool  = []string{"3c:ec:ef:00:00:01", "3c:ec:ef:00:00:02", "3c:ec:ef:00:00:03", "3c:ec:ef:00:00:04", "3c:ec:ef:00:00:05"}
	idGateDiskPool = []string{"S7EVNX0T1", "S7EVNX0T2", "ZA1B2C3D", "0x5000c500a1b2c3d4", "eui.002538db11a012a8"}
)

// idGateNorm: trimmed identifier, "" for the documented placeholder set.
func idGateNorm(v string) string {
	v = strings.TrimSpace(v)
	switch strings.ToLower(v) {
	case "", "0", "00", "default string", "n/a", "not specified", "none", "unknown",
		"to be filled by o.e.m.", "to be filled by o.e.m", "no asset information",
		"system serial number", "chassis serial number", "0123456789":
		return ""
	}
	if strings.Trim(strings.TrimPrefix(strings.ToLower(v), "0x"), "0") == "" {
		return ""
	}
	return v
}

func idGateSame(a, b string) bool {
	a, b = idGateNorm(a), idGateNorm(b)
	return a != "" && strings.EqualFold(a, b)
}

func idGateUUID(v string) (uuid.UUID, bool) {
	u, err := uuid.Parse(strings.TrimSpace(v))
	if err != nil || u == uuid.Nil {
		return uuid.Nil, false
	}
	return u, true
}

type idGateReport struct {
	System *struct {
		SerialNumber  string `json:"serial_number"`
		ChassisSerial string `json:"chassis_serial"`
		UUID          string `json:"uuid"`
	} `json:"system"`
	NICs  []idGateNIC  `json:"nics"`
	Disks []idGateDisk `json:"disks"`
}

type idGateNIC struct {
	MAC    string `json:"mac"`
	Driver string `json:"driver"`
}

type idGateDisk struct {
	Serial string `json:"serial"`
	WWN    string `json:"wwn"`
}

// idGateVerdict returns "" when the report should be accepted, or a short
// reason when it must be rejected.
func idGateVerdict(target model.MachineSnapshot, fleet []model.MachineSnapshot, rep idGateReport) string {
	if rep.System == nil {
		return "no system"
	}
	if exp := idGateNorm(target.NetBoxSerial); exp != "" {
		if !idGateSame(exp, rep.System.SerialNumber) && !idGateSame(exp, rep.System.ChassisSerial) {
			return "serial mismatch"
		}
	}
	if strings.TrimSpace(target.SystemUUID) != "" {
		want, wok := idGateUUID(target.SystemUUID)
		got, gok := idGateUUID(rep.System.UUID)
		if !wok || !gok || want != got {
			// A stored placeholder UUID cannot be "drift": compare raw parse.
			wr, e1 := uuid.Parse(strings.TrimSpace(target.SystemUUID))
			gr, e2 := uuid.Parse(strings.TrimSpace(rep.System.UUID))
			if e1 != nil || e2 != nil || wr != gr {
				return "uuid drift"
			}
		}
	}
	// UUID ownership is deliberately fail-closed for ANY parseable UUID,
	// including the all-zero one: preflight refuses a nil UUID for deploy
	// anyway, so treating a shared nil UUID as "owned" only changes which
	// gap the operator sees.
	repUUID, repUUIDErr := uuid.Parse(strings.TrimSpace(rep.System.UUID))
	repUUIDOK := repUUIDErr == nil
	for _, o := range fleet {
		if o.ID == target.ID {
			continue
		}
		if ou, err := uuid.Parse(strings.TrimSpace(o.SystemUUID)); err == nil && repUUIDOK && ou == repUUID {
			return "uuid owned"
		}
		if idGateSame(o.NetBoxSerial, rep.System.SerialNumber) || idGateSame(o.NetBoxSerial, rep.System.ChassisSerial) {
			return "serial owned"
		}
		for _, d := range rep.Disks {
			for _, od := range o.Disks {
				if idGateSame(d.WWN, od.WWN) || idGateSame(d.Serial, od.Serial) {
					return "disk owned"
				}
			}
		}
	}
	owners := map[string]map[uuid.UUID]bool{}
	for _, m := range fleet {
		for _, i := range m.Interfaces {
			if i.MAC == "" || i.Type == "ipmi" {
				continue
			}
			if owners[i.MAC] == nil {
				owners[i.MAC] = map[uuid.UUID]bool{}
			}
			owners[i.MAC][m.ID] = true
		}
	}
	matched := false
	for _, n := range rep.NICs {
		mac := strings.ToLower(strings.TrimSpace(n.MAC))
		if mac == "" || n.Driver == "cdc_ether" || n.Driver == "rndis_host" {
			continue
		}
		own := owners[mac]
		switch {
		case len(own) > 1:
			return "ambiguous mac"
		case len(own) == 1 && !own[target.ID]:
			return "foreign mac"
		case len(own) == 1:
			matched = true
		}
	}
	if !matched {
		return "no owned nic"
	}
	return ""
}

func idGateVariant(t *rapid.T, s, label string) string {
	switch rapid.IntRange(0, 3).Draw(t, label+"Variant") {
	case 1:
		return strings.ToLower(s)
	case 2:
		return "  " + s + "\t"
	case 3:
		return strings.ToUpper(s)
	}
	return s
}

func genIDGateFleet(t *rapid.T) []model.MachineSnapshot {
	n := rapid.IntRange(1, 4).Draw(t, "fleetSize")
	serials := rapid.Permutation(idGateSerialPool).Draw(t, "serialPerm")
	fleet := make([]model.MachineSnapshot, n)
	for i := range fleet {
		m := model.MachineSnapshot{ID: uuid.New(), NetBoxSerial: serials[i]}
		if rapid.Bool().Draw(t, "commissioned") {
			m.SystemUUID = rapid.SampledFrom(idGateUUIDPool).Draw(t, "storedUUID")
			m.SystemSerial = idGateNorm(m.NetBoxSerial)
			if rapid.Bool().Draw(t, "storedChassis") {
				m.ChassisSerial = rapid.SampledFrom(append([]string{"CH1190001"}, idGatePlaceholders...)).Draw(t, "storedChassisVal")
			}
			for k := 0; k < rapid.IntRange(0, 2).Draw(t, "nStoredDisks"); k++ {
				// Builder normalization: placeholders dropped, trimmed.
				d := model.SnapshotDiskIdentity{
					Serial: idGateNorm(rapid.SampledFrom(idGateDiskPool).Draw(t, "storedDiskSerial")),
					WWN:    idGateNorm(rapid.SampledFrom(append([]string{""}, idGateDiskPool...)).Draw(t, "storedDiskWWN")),
				}
				if d.Serial != "" || d.WWN != "" {
					m.Disks = append(m.Disks, d)
				}
			}
		}
		for k := 0; k < rapid.IntRange(1, 3).Draw(t, "nIfaces"); k++ {
			typ := rapid.SampledFrom([]string{"physical", "physical", "ipmi"}).Draw(t, "ifType")
			if i == 0 && k == 0 && rapid.IntRange(0, 4).Draw(t, "targetHasBootNIC") > 0 {
				typ = "physical"
			}
			m.Interfaces = append(m.Interfaces, model.SnapshotInterface{
				MAC:  strings.ToLower(rapid.SampledFrom(idGateMACPool).Draw(t, "ifMAC")),
				Type: typ,
			})
		}
		fleet[i] = m
	}
	return fleet
}

func genIDGateReport(t *rapid.T, fleet []model.MachineSnapshot) idGateReport {
	target := fleet[0]
	var rep idGateReport
	rep.System = &struct {
		SerialNumber  string `json:"serial_number"`
		ChassisSerial string `json:"chassis_serial"`
		UUID          string `json:"uuid"`
	}{}
	// Start from the legitimate report the target's own hardware produces.
	if rapid.Bool().Draw(t, "serialInChassis") {
		rep.System.ChassisSerial = idGateVariant(t, target.NetBoxSerial, "chassis")
		rep.System.SerialNumber = rapid.SampledFrom(idGatePlaceholders).Draw(t, "sysPlaceholder")
	} else {
		rep.System.SerialNumber = idGateVariant(t, target.NetBoxSerial, "sys")
		rep.System.ChassisSerial = rapid.SampledFrom(append([]string{"CH1190001"}, idGatePlaceholders...)).Draw(t, "chassisVal")
	}
	if target.SystemUUID != "" {
		rep.System.UUID = idGateVariant(t, target.SystemUUID, "uuid")
	} else {
		rep.System.UUID = idGateVariant(t, rapid.SampledFrom(idGateUUIDPool).Draw(t, "freshUUID"), "uuid")
	}
	for _, i := range target.Interfaces {
		if i.Type != "ipmi" && rapid.Bool().Draw(t, "reportIface") {
			rep.NICs = append(rep.NICs, idGateNIC{MAC: idGateVariant(t, i.MAC, "mac"), Driver: "mlx5_core"})
		}
	}
	for k := 0; k < rapid.IntRange(0, 2).Draw(t, "nRepDisks"); k++ {
		rep.Disks = append(rep.Disks, idGateDisk{
			Serial: idGateVariant(t, rapid.SampledFrom([]string{"", "AAAABBBBCCCC3", "0000000000", "FRESH-DISK-1", "FRESH-DISK-2"}).Draw(t, "repDiskSerial"), "dserial"),
			WWN:    idGateVariant(t, rapid.SampledFrom([]string{"", "0x0000000000000000", "0x5000c500ffffffff"}).Draw(t, "repDiskWWN"), "dwwn"),
		})
	}
	// Then perturb 0..2 dimensions.
	for k := 0; k < rapid.IntRange(0, 2).Draw(t, "nPerturb"); k++ {
		switch rapid.IntRange(0, 9).Draw(t, "perturb") {
		case 9:
			// Host-visible BMC USB NIC (cdc_ether/rndis_host) whose MAC is
			// modeled on a sibling: the shared-MAC case the exemption exists
			// for.
			if len(fleet) > 1 {
				o := fleet[rapid.IntRange(1, len(fleet)-1).Draw(t, "pUSBOther")]
				for _, i := range o.Interfaces {
					rep.NICs = append(rep.NICs, idGateNIC{MAC: i.MAC, Driver: rapid.SampledFrom([]string{"cdc_ether", "rndis_host"}).Draw(t, "pUSBDriver")})
				}
			}
		case 0:
			rep.System = nil
			return rep
		case 1:
			rep.System.SerialNumber = idGateVariant(t, rapid.SampledFrom(idGateSerialPool).Draw(t, "pSerial"), "pSerial")
		case 2:
			rep.System.ChassisSerial = idGateVariant(t, rapid.SampledFrom(idGateSerialPool).Draw(t, "pChassis"), "pChassis")
		case 3:
			rep.System.SerialNumber = rapid.SampledFrom(idGatePlaceholders).Draw(t, "pSysPh")
			rep.System.ChassisSerial = rapid.SampledFrom(idGatePlaceholders).Draw(t, "pChPh")
		case 4:
			rep.System.UUID = idGateVariant(t, rapid.SampledFrom(append([]string{""}, idGateUUIDPool...)).Draw(t, "pUUID"), "pUUID")
		case 5:
			rep.NICs = append(rep.NICs, idGateNIC{
				MAC:    idGateVariant(t, rapid.SampledFrom(append([]string{"", "aa:bb:cc:dd:ee:ff"}, idGateMACPool...)).Draw(t, "pMAC"), "pMAC"),
				Driver: rapid.SampledFrom([]string{"mlx5_core", "ice", "cdc_ether"}).Draw(t, "pDriver"),
			})
		case 6:
			rep.NICs = nil
		case 7:
			rep.Disks = append(rep.Disks, idGateDisk{
				Serial: idGateVariant(t, rapid.SampledFrom(append([]string{""}, idGateDiskPool...)).Draw(t, "pDiskSerial"), "pds"),
				WWN:    idGateVariant(t, rapid.SampledFrom(append([]string{""}, idGateDiskPool...)).Draw(t, "pDiskWWN"), "pdw"),
			})
		case 8:
			if len(fleet) > 1 {
				o := fleet[rapid.IntRange(1, len(fleet)-1).Draw(t, "pOther")]
				rep.System.SerialNumber = idGateVariant(t, o.NetBoxSerial, "pOtherSerial")
				if o.SystemUUID != "" {
					rep.System.UUID = o.SystemUUID
				}
			}
		}
	}
	return rep
}

// TestCommissionIdentityGate_Property_MatchesModel compares the site worker's accept/reject
// verdict with the reference model. Reasons are not compared (the gate
// stops at its first finding); only accept vs reject.
func TestCommissionIdentityGate_Property_MatchesModel(t *testing.T) {
	idGateQuietSlog(t)
	var accepted, rejected int
	reasons := map[string]int{}
	rapid.Check(t, func(t *rapid.T) {
		fleet := genIDGateFleet(t)
		rep := genIDGateReport(t, fleet)
		data, err := json.Marshal(rep)
		if err != nil {
			t.Fatal(err)
		}
		c := NewCache(nil)
		c.ApplySnapshot(&model.SiteSnapshot{Machines: fleet})
		opID := uuid.New()
		seedOpsResync(c, model.Operation{ID: opID, MachineID: fleet[0].ID, Type: model.OpCommission, Status: model.OpRunning})
		gotErr := c.HandleProgressReport(&model.ProgressReport{
			OperationID: opID.String(), MachineID: fleet[0].ID.String(),
			Event: "commission_report", CommissionData: data,
		})
		want := idGateVerdict(fleet[0], fleet, rep)
		reasons[want]++
		if want == "" {
			accepted++
		} else {
			rejected++
		}
		if (gotErr == nil) != (want == "") {
			t.Fatalf("gate verdict diverges from model:\n  gate error: %v\n  model reason: %q\n  fleet: %s\n  report: %s",
				gotErr, want, idGateJSON(fleet), data)
		}
	})
	t.Logf("model verdicts: accepted=%d rejected=%d reasons=%v", accepted, rejected, reasons)
}

func idGateJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("<%v>", err)
	}
	return string(b)
}

func idGateQuietSlog(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
}
