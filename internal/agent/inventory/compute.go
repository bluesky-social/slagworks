package inventory

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/bluesky-social/slagworks/internal/agent"
)

// maxCPUSockets bounds the socket count collectCPUs accepts from lscpu. The
// largest machines built have a few hundred sockets and the kernel's CPU
// limit (NR_CPUS, at most 8192) caps what lscpu can honestly report, so a
// count above 1024 is corrupted output, not hardware.
const maxCPUSockets = 1024

// collectCPUs returns one CPUInfo per socket lscpu reports, and nil when
// it names no model. It returns an error when lscpu cannot be run or does
// not print JSON, and when it reports more sockets than maxCPUSockets: the
// count sizes the result, and a corrupted one must not become an allocation
// of billions of entries. A count that is not a number ("-", or absent)
// reads as one socket.
func collectCPUs(r agent.Runner) ([]CPUInfo, error) {
	out, err := r.Output("lscpu", "--json")
	if err != nil {
		return nil, fmt.Errorf("lscpu failed: %w", err)
	}

	var parsed struct {
		Lscpu []struct {
			Field string `json:"field"`
			Data  string `json:"data"`
		} `json:"lscpu"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		return nil, fmt.Errorf("lscpu --json printed something other than its JSON: %w", err)
	}

	var model, speed, arch, flags, socketsField string
	var sockets, coresPerSocket, tpc int
	for _, f := range parsed.Lscpu {
		field := strings.TrimRight(f.Field, ":")
		switch field {
		case "Model name":
			model = f.Data
		case "Socket(s)":
			socketsField = strings.TrimSpace(f.Data)
			fmt.Sscanf(socketsField, "%d", &sockets)
		case "Core(s) per socket":
			fmt.Sscanf(f.Data, "%d", &coresPerSocket)
		case "Thread(s) per core":
			fmt.Sscanf(f.Data, "%d", &tpc)
		case "CPU max MHz", "CPU MHz":
			if speed == "" {
				speed = f.Data + " MHz"
			}
		case "Architecture":
			arch = f.Data
		case "Flags":
			flags = f.Data
		}
	}
	if sockets < 1 {
		sockets = 1
	}
	// A count too large for an int fails the scan above and would read as
	// one socket; it is over the bound all the same.
	_, atoiErr := strconv.Atoi(socketsField)
	if sockets > maxCPUSockets || errors.Is(atoiErr, strconv.ErrRange) {
		return nil, fmt.Errorf("lscpu reports %q CPU sockets, more than the %d any machine has; "+
			"its output is corrupt, so no CPU inventory was recorded: run `lscpu --json` on the host and check \"Socket(s)\"",
			socketsField, maxCPUSockets)
	}
	if model == "" {
		return nil, nil
	}

	// Parse flags into a slice
	var flagList []string
	if flags != "" {
		flagList = append(flagList, strings.Fields(flags)...)
	}

	// One entry per socket so each maps to a separate Module in NetBox
	cpus := make([]CPUInfo, sockets)
	for i := range cpus {
		cpus[i] = CPUInfo{
			Model:   model,
			Cores:   coresPerSocket,
			Threads: coresPerSocket * tpc,
			Speed:   speed,
			Arch:    arch,
			Flags:   flagList,
		}
	}
	return cpus, nil
}

// lscpuStep collects the CPUs into res and returns the lscpu step's verdict.
func lscpuStep(r agent.Runner, res *CommissionResult) stepVerdict {
	cpus, err := collectCPUs(r)
	if err != nil {
		slog.Warn("no CPU inventory", "err", err)
		return stepVerdict{Status: StepStatusFailed, Error: err.Error()}
	}
	res.CPUs = cpus
	return verdictForSlice(len(res.CPUs), "lscpu produced no CPU info")
}

func collectMemory(r agent.Runner) *MemoryInfo {
	info := &MemoryInfo{}

	// Total from /proc/meminfo
	if raw, err := os.ReadFile("/proc/meminfo"); err == nil {
		for line := range strings.SplitSeq(string(raw), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				var kb int
				fmt.Sscanf(strings.TrimPrefix(line, "MemTotal:"), "%d", &kb)
				info.TotalMB = kb / 1024
				break
			}
		}
	}

	// DIMMs from dmidecode
	dmiOut, err := r.Output("dmidecode", "-t", "17")
	if err != nil {
		return info
	}

	var currentDIMM *DIMMInfo
	for line := range strings.SplitSeq(dmiOut, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Memory Device") {
			if currentDIMM != nil && currentDIMM.SizeMB > 0 {
				info.DIMMs = append(info.DIMMs, *currentDIMM)
			}
			currentDIMM = &DIMMInfo{}
			continue
		}
		if currentDIMM == nil {
			continue
		}
		if k, v, ok := strings.Cut(line, ": "); ok {
			switch strings.TrimSpace(k) {
			case "Size":
				currentDIMM.SizeMB = parseDMISizeMB(v)
			case "Locator":
				currentDIMM.Locator = v
			case "Type":
				currentDIMM.Type = v
			case "Speed":
				currentDIMM.Speed = v
			case "Part Number":
				currentDIMM.Part = strings.TrimSpace(v)
			case "Serial Number":
				currentDIMM.Serial = strings.TrimSpace(v)
			case "Manufacturer":
				currentDIMM.Manufacturer = strings.TrimSpace(v)
			}
		}
	}
	if currentDIMM != nil && currentDIMM.SizeMB > 0 {
		info.DIMMs = append(info.DIMMs, *currentDIMM)
	}

	return info
}

// parseDMISizeMB converts a dmidecode Type 17 "Size" value to MiB. dmidecode
// prints the size in the largest binary unit that keeps it an integer
// ("32 GB", "1536 MB", "1 TB", "2048 kB"), so the unit token decides the
// scale. "No Module Installed", "Unknown" and any other unit return 0, which
// leaves the slot out of the inventory.
func parseDMISizeMB(v string) int {
	var n int
	var unit string
	if _, err := fmt.Sscanf(v, "%d %s", &n, &unit); err != nil {
		return 0
	}
	switch unit {
	case "kB":
		return n >> 10
	case "MB":
		return n
	case "GB":
		return n << 10
	case "TB":
		return n << 20
	}
	return 0
}

func collectPSUs(r agent.Runner) []PSUInfo {
	if !r.Available("dmidecode") {
		return nil
	}
	out, err := r.Output("dmidecode", "-t", "39")
	if err != nil {
		return nil
	}

	var psus []PSUInfo
	var current *PSUInfo
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "System Power Supply") {
			if current != nil {
				psus = append(psus, *current)
			}
			current = &PSUInfo{}
			continue
		}
		if current == nil {
			continue
		}
		if k, v, ok := strings.Cut(line, ": "); ok {
			v = strings.TrimSpace(v)
			switch strings.TrimSpace(k) {
			case "Location":
				current.Location = v
			case "Name", "Model Part Number":
				if current.Model == "" && v != "" && !isJunkDMI(v) {
					current.Model = v
				}
			case "Serial Number":
				if !isJunkDMI(v) {
					current.Serial = v
				}
			case "Max Power Capacity":
				fmt.Sscanf(v, "%d", &current.MaxWatts)
			case "Status":
				current.Status = v
			}
		}
	}
	if current != nil {
		psus = append(psus, *current)
	}
	return psus
}

func collectNUMA() []NUMANode {
	entries, err := os.ReadDir("/sys/devices/system/node/")
	if err != nil {
		return nil
	}

	var nodes []NUMANode
	for _, entry := range entries {
		name := entry.Name()
		var nodeID int
		if _, err := fmt.Sscanf(name, "node%d", &nodeID); err != nil {
			continue
		}

		node := NUMANode{ID: nodeID}

		cpuPath := fmt.Sprintf("/sys/devices/system/node/%s/cpulist", name)
		if raw, err := os.ReadFile(cpuPath); err == nil {
			node.CPUs = strings.TrimSpace(string(raw))
		}

		memPath := fmt.Sprintf("/sys/devices/system/node/%s/meminfo", name)
		if raw, err := os.ReadFile(memPath); err == nil {
			for line := range strings.SplitSeq(string(raw), "\n") {
				if strings.Contains(line, "MemTotal") {
					var kb int
					// Format: "Node X MemTotal:    12345 kB"
					fields := strings.Fields(line)
					if len(fields) >= 4 {
						fmt.Sscanf(fields[3], "%d", &kb)
						node.MemMB = kb / 1024
					}
				}
			}
		}

		nodes = append(nodes, node)
	}
	return nodes
}
