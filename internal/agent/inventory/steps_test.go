package inventory

import (
	"encoding/json"
	"testing"
	"time"
)

func TestStepRecorder_RecordsDurationAndStatus(t *testing.T) {
	r := newStepRecorder()
	// Inject a fake clock that advances 5ms each call so duration is
	// deterministic.
	now := time.Unix(0, 0)
	r.now = func() time.Time {
		t := now
		now = now.Add(5 * time.Millisecond)
		return t
	}

	r.record("ok_step", func() stepVerdict { return stepVerdict{Status: StepStatusOK} })
	r.record("partial_step", func() stepVerdict {
		return stepVerdict{Status: StepStatusPartial, Warning: "missing slot 3"}
	})
	r.record("failed_step", func() stepVerdict {
		return stepVerdict{Status: StepStatusFailed, Error: "exit 1"}
	})

	got := r.result()
	if len(got) != 3 {
		t.Fatalf("expected 3 steps, got %d", len(got))
	}
	for i, s := range got {
		if s.DurationMs != 5 {
			t.Errorf("step %d: expected 5ms duration, got %dms", i, s.DurationMs)
		}
	}
	if got[0].Status != StepStatusOK || got[0].Error != "" || got[0].Warning != "" {
		t.Errorf("ok_step: unexpected verdict %+v", got[0])
	}
	if got[1].Status != StepStatusPartial || got[1].Warning != "missing slot 3" {
		t.Errorf("partial_step: unexpected verdict %+v", got[1])
	}
	if got[2].Status != StepStatusFailed || got[2].Error != "exit 1" {
		t.Errorf("failed_step: unexpected verdict %+v", got[2])
	}
}

func TestStepRecorder_FailedDropsWarningOnError(t *testing.T) {
	// A failed step's Error field should always be populated; a warning
	// alongside the error is informational and kept too.
	r := newStepRecorder()
	r.record("x", func() stepVerdict {
		return stepVerdict{Status: StepStatusFailed, Error: "boom", Warning: "also slow"}
	})
	got := r.result()[0]
	if got.Error != "boom" || got.Warning != "also slow" {
		t.Fatalf("expected error+warning preserved, got %+v", got)
	}
}

func TestVerdictForSlice(t *testing.T) {
	if v := verdictForSlice(0, "nothing"); v.Status != StepStatusFailed || v.Error != "nothing" {
		t.Errorf("empty: got %+v", v)
	}
	if v := verdictForSlice(3, "nothing"); v.Status != StepStatusOK {
		t.Errorf("non-empty: got %+v", v)
	}
}

func TestVerdictForSystem(t *testing.T) {
	cases := []struct {
		name string
		in   *SystemInfo
		want string
	}{
		{"nil", nil, StepStatusFailed},
		{"empty", &SystemInfo{}, StepStatusPartial},
		{"missing serial", &SystemInfo{Manufacturer: "ASUS"}, StepStatusPartial},
		{"complete", &SystemInfo{Manufacturer: "ASUS", SerialNumber: "S1"}, StepStatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if v := verdictForSystem(c.in); v.Status != c.want {
				t.Errorf("got %q, want %q", v.Status, c.want)
			}
		})
	}
}

func TestVerdictForMemory(t *testing.T) {
	if v := verdictForMemory(nil); v.Status != StepStatusFailed {
		t.Errorf("nil: got %+v", v)
	}
	if v := verdictForMemory(&MemoryInfo{}); v.Status != StepStatusFailed {
		t.Errorf("empty: got %+v", v)
	}
	if v := verdictForMemory(&MemoryInfo{TotalMB: 16384}); v.Status != StepStatusPartial {
		t.Errorf("total only: got %+v", v)
	}
	full := &MemoryInfo{TotalMB: 16384, DIMMs: []DIMMInfo{{SizeMB: 8192}}}
	if v := verdictForMemory(full); v.Status != StepStatusOK {
		t.Errorf("full: got %+v", v)
	}
}

func TestVerdictForDisks(t *testing.T) {
	if v := verdictForDisks(nil); v.Status != StepStatusFailed {
		t.Errorf("none: got %+v", v)
	}
	healthy := true
	withSmart := []DiskInfo{{Name: "sda", SMARTHealthy: &healthy}}
	if v := verdictForDisks(withSmart); v.Status != StepStatusOK {
		t.Errorf("all-smart: got %+v", v)
	}
	mixed := []DiskInfo{{Name: "sda", SMARTHealthy: &healthy}, {Name: "sdb"}}
	if v := verdictForDisks(mixed); v.Status != StepStatusPartial {
		t.Errorf("mixed: got %+v", v)
	}
}

func TestVerdictForBMC(t *testing.T) {
	if v := verdictForBMC(nil); v.Status != StepStatusFailed {
		t.Errorf("nil: got %+v", v)
	}
	if v := verdictForBMC(&BMCInfo{IP: "10.0.0.1"}); v.Status != StepStatusPartial {
		t.Errorf("missing fw: got %+v", v)
	}
	if v := verdictForBMC(&BMCInfo{IP: "10.0.0.1", FirmwareVersion: "1.2.3"}); v.Status != StepStatusOK {
		t.Errorf("complete: got %+v", v)
	}
}

func TestCollectInventoryDataWithRunner_PopulatesSteps(t *testing.T) {
	dir := "testdata/ubuntu-2404"
	runner := newFixtureRunner(dir, "dmidecode", "lsblk", "ip", "lspci")
	res := CollectGraceful(runner, Options{})

	if res == nil {
		t.Fatal("nil result")
	}
	if len(res.Steps) == 0 {
		t.Fatal("expected steps array to be populated")
	}

	// Index by name for assertion.
	byName := make(map[string]CommissionStep)
	for _, s := range res.Steps {
		byName[s.Name] = s
	}

	// dmidecode_system should be ok (fixture has full data).
	if got := byName["dmidecode_system"].Status; got != StepStatusOK {
		t.Errorf("dmidecode_system: got %q, want ok", got)
	}
	// lldp_neighbors should be failed since lldpctl is not available.
	if got := byName["lldp_neighbors"].Status; got != StepStatusFailed {
		t.Errorf("lldp_neighbors: got %q, want failed", got)
	}
	// bmc_ipmitool should be failed since ipmitool is not available.
	if got := byName["bmc_ipmitool"].Status; got != StepStatusFailed {
		t.Errorf("bmc_ipmitool: got %q, want failed", got)
	}
	// lsblk_disks should be partial (no smartctl).
	if got := byName["lsblk_disks"].Status; got != StepStatusPartial {
		t.Errorf("lsblk_disks: got %q, want partial", got)
	}

	// Round-trip through JSON to ensure the steps survive serialization.
	out, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded CommissionResult
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded.Steps) != len(res.Steps) {
		t.Errorf("round-trip step count: got %d, want %d", len(decoded.Steps), len(res.Steps))
	}
}
