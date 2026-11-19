package inventory

import (
	"errors"
	"testing"
)

// mlx5PortRunner replays `ip -j addr show` and ethtool output for a host
// with a single mlx5 port. `ethtool -m` fails, as it does with no module
// EEPROM to read.
type mlx5PortRunner struct{ ethtool string }

func (mlx5PortRunner) Available(string) bool       { return true }
func (mlx5PortRunner) Run(string, ...string) error { return nil }
func (r mlx5PortRunner) Output(name string, args ...string) (string, error) {
	switch {
	case name == "ip":
		return `[{"ifname":"enp1s0np0","address":"02:00:00:00:00:01","addr_info":[]}]`, nil
	case name == "ethtool" && len(args) == 2 && args[0] == "-i":
		return "driver: mlx5_core\nfirmware-version: 28.39.1002\nbus-info: 0000:01:00.0", nil
	case name == "ethtool" && len(args) == 2 && args[0] == "-m":
		return "", errors.New("module EEPROM not available")
	case name == "ethtool" && len(args) == 1:
		return r.ethtool, nil
	}
	return "", errors.New("unexpected command")
}

// A 400G port is classified into the 400G tier whether the type comes
// from the negotiated speed (link up) or from the highest supported link
// mode (link down, "Speed: Unknown!").
func TestCollectNICs400G(t *testing.T) {
	const modes = "\tSupported ports: [ FIBRE ]\n" +
		"\tSupported link modes:   100000baseCR4/Full\n" +
		"\t                        200000baseCR4/Full\n" +
		"\t                        400000baseCR4/Full\n"
	tests := []struct {
		name    string
		ethtool string
	}{
		{"link up", "Settings for enp1s0np0:\n" + modes +
			"\tSpeed: 400000Mb/s\n\tDuplex: Full\n\tPort: FIBRE\n\tLink detected: yes"},
		{"link down", "Settings for enp1s0np0:\n" + modes +
			"\tSpeed: Unknown!\n\tDuplex: Unknown! (255)\n\tPort: FIBRE\n\tLink detected: no"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nics := collectNICs(mlx5PortRunner{ethtool: tt.ethtool})
			if len(nics) != 1 {
				t.Fatalf("got %d NICs, want 1", len(nics))
			}
			if got, want := nics[0].IntfType, "400gbase-x-qsfpdd"; got != want {
				t.Errorf("IntfType = %q (speed %q), want %q", got, nics[0].Speed, want)
			}
		})
	}
}
