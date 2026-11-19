package api

// Property test for buildNetBoxDiff. The diff is a report, not an
// applicable patch, so the checked relations are soundness and
// completeness of Matched against an independent oracle:
//
//   D1 Matched is true iff both sides exist and slagworks_id, device id and
//      serial agree (blank on both sides counts as agreement only via the
//      rendered "(none)" placeholder, which is what operators see).
//   D2 every comparable field's Match equals the equality of its rendered
//      sides; context fields never carry Match.
//   D3 a stale binding (NetBox device's slagworks_id names a different
//      machine, or device id differs) is never reported as matched.

import (
	"encoding/json"
	"flag"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

// The properties in this package run from a fixed seed unless one is given,
// so a plain go test explores the same cases every time. The default is set
// before flags are parsed: -rapid.seed and -rapid.checks on the command
// line still apply, and a deep run should pass a fresh seed.
func init() {
	if err := flag.Set("rapid.seed", "1"); err != nil {
		panic(err)
	}
}

func renderOracle(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}

func TestPropNetBoxDiffMatched(t *testing.T) {
	ids := []uuid.UUID{uuid.MustParse("11111111-1111-1111-1111-111111111111"), uuid.MustParse("22222222-2222-2222-2222-222222222222")}
	rapid.Check(t, func(t *rapid.T) {
		id := rapid.SampledFrom(ids).Draw(t, "id")
		var machine *model.Machine
		if rapid.IntRange(0, 4).Draw(t, "hasMachine") != 0 {
			m := &model.Machine{ID: id, NetBoxSerial: rapid.SampledFrom([]string{"SN1", "SN2", " "}).Draw(t, "swSerial"), State: model.MachineStateNew}
			if rapid.Bool().Draw(t, "bound") {
				d := rapid.IntRange(1, 3).Draw(t, "swDev")
				m.NetBoxDeviceID = &d
			}
			machine = m
		}
		var device *netbox.Device
		if rapid.IntRange(0, 4).Draw(t, "hasDevice") != 0 {
			d := &netbox.Device{ID: rapid.IntRange(1, 3).Draw(t, "nbDev"), Serial: rapid.SampledFrom([]string{"SN1", "SN2", ""}).Draw(t, "nbSerial")}
			switch rapid.IntRange(0, 2).Draw(t, "cf") {
			case 1:
				d.CustomFields, _ = json.Marshal(map[string]any{"slagworks_id": rapid.SampledFrom(ids).Draw(t, "cfID").String()})
			case 2:
				d.CustomFields = json.RawMessage(`{"slagworks_id":null}`)
			}
			device = d
		}
		if machine == nil && device == nil {
			return
		}
		var missing DiffMissing
		switch {
		case machine == nil:
			missing = MissingSlagworks
		case device == nil:
			missing = MissingNetBox
		}
		got := buildNetBoxDiff(id, machine, device, nil, "0", missing)

		// Oracle.
		want := missing == ""
		if want {
			var cfID string
			var cf map[string]any
			if json.Unmarshal(device.CustomFields, &cf) == nil {
				cfID, _ = cf["slagworks_id"].(string)
			}
			swDev := "(none)"
			if machine.NetBoxDeviceID != nil {
				swDev = strconv.Itoa(*machine.NetBoxDeviceID)
			}
			want = machine.ID.String() == renderOracle(cfID) &&
				swDev == strconv.Itoa(device.ID) &&
				renderOracle(machine.NetBoxSerial) == renderOracle(device.Serial)
		}
		if got.Matched != want {
			t.Fatalf("D1: Matched=%v, want %v; diff=%+v", got.Matched, want, got)
		}
		for _, f := range got.Fields {
			if f.Comparable {
				if f.Match == nil || *f.Match != (f.Slagworks == f.NetBox) {
					t.Fatalf("D2: field %s match=%v but sides %q vs %q", f.Name, f.Match, f.Slagworks, f.NetBox)
				}
			} else if f.Match != nil {
				t.Fatalf("D2: context field %s carries Match", f.Name)
			}
		}
	})
}
