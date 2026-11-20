package inventory

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/bluesky-social/slagworks/internal/agent"
)

// Options tunes a collection run.
type Options struct {
	// SkipBMC skips the bmc_ipmitool step: the host's OOB endpoint is
	// Intel AMT (boot cmdline slagworks.oob=amt), so there is no IPMI BMC
	// behind /dev/ipmi0 to query, and whatever ipmitool might find must
	// never be reported as the device's BMC.
	SkipBMC bool
}

// skippedAMTBMC is the bmc_ipmitool verdict for an AMT host.
var skippedAMTBMC = stepVerdict{
	Status:  StepStatusSkipped,
	Warning: "OOB endpoint is Intel AMT (slagworks.oob=amt); no IPMI BMC to query",
}

// Collect gathers full hardware inventory in commission mode: dmidecode
// is attempted per-key with a per-key sysfs fallback (collectSystem),
// and lldpd is started to force neighbor discovery. Contrast
// CollectGraceful, which skips dmidecode wholesale when the binary is
// absent and never starts lldpd. A host whose dmidecode is broken still
// commissions on sysfs data; verdictForSystem downgrades the step when
// identity fields (manufacturer/serial) come back empty.
func Collect(r agent.Runner, opts Options) *CommissionResult {
	hostname, err := os.Hostname()
	if err != nil {
		slog.Warn("could not get hostname", "err", err)
	}
	res := &CommissionResult{Hostname: hostname}
	sr := newStepRecorder()

	sr.record("dmidecode_system", func() stepVerdict {
		res.System = collectSystem(r)
		if isCumulus() {
			backfillFromCumulus(r, res.System)
		}
		return verdictForSystem(res.System)
	})
	sr.record("lscpu", func() stepVerdict { return lscpuStep(r, res) })
	sr.record("dmidecode_memory", func() stepVerdict {
		res.Memory = collectMemory(r)
		return verdictForMemory(res.Memory)
	})
	sr.record("lsblk_disks", func() stepVerdict {
		res.Disks = collectDisks(r)
		return verdictForDisks(res.Disks)
	})
	sr.record("ip_nics", func() stepVerdict {
		res.NICs = collectNICs(r)
		return verdictForSlice(len(res.NICs), "no physical NICs detected")
	})
	sr.record("lspci", func() stepVerdict {
		res.PCIDevices = collectPCI(r)
		return verdictForSlice(len(res.PCIDevices), "lspci produced no devices")
	})
	sr.record("lldp_neighbors", func() stepVerdict {
		res.LLDP = collectLLDP(r)
		return verdictForLLDP(res.LLDP)
	})
	sr.record("bmc_ipmitool", func() stepVerdict {
		if opts.SkipBMC {
			return skippedAMTBMC
		}
		res.BMC = collectBMC(r)
		return verdictForBMC(res.BMC)
	})
	sr.record("dmidecode_psus", func() stepVerdict {
		res.PSUs = collectPSUs(r)
		return verdictForSlice(len(res.PSUs), "no PSU entries from dmidecode -t 39")
	})
	sr.record("numa", func() stepVerdict {
		res.NUMA = collectNUMA()
		return verdictForSlice(len(res.NUMA), "no NUMA nodes in /sys/devices/system/node")
	})
	sr.record("lsusb", func() stepVerdict {
		res.USB = collectUSB(r)
		return verdictForSlice(len(res.USB), "lsusb produced no devices")
	})

	res.Steps = sr.result()
	return res
}

// CollectGraceful collects hardware inventory in deploy/inventory mode.
// Gracefully skips sections when tools are unavailable and never starts
// lldpd.
func CollectGraceful(r agent.Runner, opts Options) *CommissionResult {
	hostname, _ := os.Hostname()
	res := &CommissionResult{Hostname: hostname}
	sr := newStepRecorder()

	sr.record("dmidecode_system", func() stepVerdict {
		res.System = collectSystemGraceful(r)
		if isCumulus() {
			backfillFromCumulus(r, res.System)
		}
		return verdictForSystem(res.System)
	})
	sr.record("lscpu", func() stepVerdict { return lscpuStep(r, res) })
	sr.record("dmidecode_memory", func() stepVerdict {
		res.Memory = collectMemory(r)
		return verdictForMemory(res.Memory)
	})
	sr.record("lsblk_disks", func() stepVerdict {
		res.Disks = collectDisks(r)
		return verdictForDisks(res.Disks)
	})
	sr.record("ip_nics", func() stepVerdict {
		res.NICs = collectNICs(r)
		return verdictForSlice(len(res.NICs), "no physical NICs detected")
	})
	sr.record("lspci", func() stepVerdict {
		res.PCIDevices = collectPCI(r)
		return verdictForSlice(len(res.PCIDevices), "lspci produced no devices")
	})
	sr.record("lldp_neighbors", func() stepVerdict {
		res.LLDP = collectLLDPGraceful(r)
		return verdictForLLDP(res.LLDP)
	})
	sr.record("bmc_ipmitool", func() stepVerdict {
		if opts.SkipBMC {
			return skippedAMTBMC
		}
		res.BMC = collectBMCGraceful(r)
		return verdictForBMC(res.BMC)
	})
	sr.record("dmidecode_psus", func() stepVerdict {
		res.PSUs = collectPSUs(r)
		return verdictForSlice(len(res.PSUs), "no PSU entries from dmidecode -t 39")
	})
	sr.record("numa", func() stepVerdict {
		res.NUMA = collectNUMA()
		return verdictForSlice(len(res.NUMA), "no NUMA nodes in /sys/devices/system/node")
	})
	sr.record("lsusb", func() stepVerdict {
		res.USB = collectUSB(r)
		return verdictForSlice(len(res.USB), "lsusb produced no devices")
	})

	res.Steps = sr.result()
	return res
}

// verdictForSlice maps an empty-slice result to a `failed` verdict and a
// non-empty result to `ok`. Used for collectors where the only signal
// is "got at least one element vs none."
func verdictForSlice(n int, failMsg string) stepVerdict {
	if n == 0 {
		return stepVerdict{Status: StepStatusFailed, Error: failMsg}
	}
	return stepVerdict{Status: StepStatusOK}
}

func verdictForSystem(sys *SystemInfo) stepVerdict {
	if sys == nil {
		return stepVerdict{Status: StepStatusFailed, Error: "no DMI/sysfs data available"}
	}
	if sys.Manufacturer == "" || sys.SerialNumber == "" {
		return stepVerdict{
			Status:  StepStatusPartial,
			Warning: "missing required DMI fields (manufacturer/serial)",
		}
	}
	return stepVerdict{Status: StepStatusOK}
}

func verdictForMemory(mem *MemoryInfo) stepVerdict {
	if mem == nil || (mem.TotalMB == 0 && len(mem.DIMMs) == 0) {
		return stepVerdict{Status: StepStatusFailed, Error: "no memory data available"}
	}
	if len(mem.DIMMs) == 0 {
		return stepVerdict{
			Status:  StepStatusPartial,
			Warning: "memory total reported but no per-DIMM detail (dmidecode -t 17 unavailable?)",
		}
	}
	return stepVerdict{Status: StepStatusOK}
}

// verdictForDisks downgrades to `partial` when smartctl couldn't
// produce a SMART health verdict for at least one disk. Operators
// commissioning a fleet care that smart status was probed; without
// that signal NetBox will silently lack the smart_healthy facet.
func verdictForDisks(disks []DiskInfo) stepVerdict {
	if len(disks) == 0 {
		return stepVerdict{Status: StepStatusFailed, Error: "no disks detected"}
	}
	missing := 0
	for _, d := range disks {
		if d.SMARTHealthy == nil {
			missing++
		}
	}
	if missing > 0 {
		return stepVerdict{
			Status:  StepStatusPartial,
			Warning: fmt.Sprintf("smart health unavailable for %d/%d disks", missing, len(disks)),
		}
	}
	return stepVerdict{Status: StepStatusOK}
}

func verdictForLLDP(raw json.RawMessage) stepVerdict {
	if len(raw) == 0 {
		return stepVerdict{Status: StepStatusFailed, Error: "no LLDP neighbor data captured"}
	}
	return stepVerdict{Status: StepStatusOK}
}

func verdictForBMC(info *BMCInfo) stepVerdict {
	if info == nil {
		return stepVerdict{Status: StepStatusFailed, Error: "ipmitool produced no BMC info"}
	}
	if info.IP == "" || info.FirmwareVersion == "" {
		return stepVerdict{
			Status:  StepStatusPartial,
			Warning: "BMC info incomplete (missing IP or firmware version)",
		}
	}
	return stepVerdict{Status: StepStatusOK}
}
