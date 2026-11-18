package smcipmitool

import (
	"fmt"
	"os/exec"
	"strings"
)

// InstallDoc is the operator-facing install guide, referenced from every
// "SMCIPMITool is missing" log line, CLI hint, and error.
const InstallDoc = "docs/user/hardware/supermicro-smcipmitool.md"

// Mode selects how the site worker uses SMCIPMITool for Supermicro hosts
// (SLAGWORKS_SMCIPMITOOL).
type Mode string

const (
	// ModeAuto uses SMCIPMITool when it is installed and falls back to the
	// generic bmclib path when it is not. The default.
	ModeAuto Mode = "auto"
	// ModeRequired makes a Supermicro power operation fail when the tool
	// is missing, rather than silently degrading onto raw IPMI.
	ModeRequired Mode = "required"
	// ModeOff never uses SMCIPMITool, even when it is installed.
	ModeOff Mode = "off"
)

// ParseMode parses SLAGWORKS_SMCIPMITOOL. Empty means ModeAuto.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(strings.ToLower(strings.TrimSpace(s))); m {
	case "":
		return ModeAuto, nil
	case ModeAuto, ModeRequired, ModeOff:
		return m, nil
	default:
		return "", fmt.Errorf("invalid SLAGWORKS_SMCIPMITOOL %q (want auto, required, or off)", s)
	}
}

// Tool status values a site worker reports in its hello and that
// `slagctl worker list` displays.
const (
	StatusInstalled = "installed"
	// StatusMissing: not installed, Supermicro hosts fall back to bmclib.
	StatusMissing = "missing"
	// StatusMissingRequired: not installed in required mode, so Supermicro
	// power operations on this worker fail.
	StatusMissingRequired = "missing-required"
	StatusDisabled        = "disabled"
)

// Tool is a site worker's SMCIPMITool configuration: the mode and the
// launcher to run.
type Tool struct {
	Mode Mode
	// Bin is the launcher path or PATH-resolved name; "" → DefaultBin.
	Bin string
}

// Lookup resolves the launcher. It reports ok=false when the mode is off
// or the launcher can't be found; it re-checks on every call so a tool
// installed after the worker started is picked up without a restart.
// exec.LookPath is a cheap stat walk, so calling it per power operation
// is fine.
func (t Tool) Lookup() (path string, ok bool) {
	if t.Mode == ModeOff {
		return "", false
	}
	p, err := exec.LookPath(t.BinName())
	if err != nil {
		return "", false
	}
	return p, true
}

// BinName is the configured launcher, or DefaultBin when none is set.
func (t Tool) BinName() string {
	if t.Bin == "" {
		return DefaultBin
	}
	return t.Bin
}

// Status returns the StatusX value to report for this worker.
func (t Tool) Status() string {
	if t.Mode == ModeOff {
		return StatusDisabled
	}
	if _, ok := t.Lookup(); ok {
		return StatusInstalled
	}
	if t.Mode == ModeRequired {
		return StatusMissingRequired
	}
	return StatusMissing
}

// MissingHint is the operator-facing nudge shown when a Supermicro host
// took the generic BMC path because the tool isn't installed.
func MissingHint(worker string) string {
	return fmt.Sprintf("SMCIPMITool is not installed on site worker %s, so this Supermicro host "+
		"is using the generic IPMI path; see %s", worker, InstallDoc)
}
