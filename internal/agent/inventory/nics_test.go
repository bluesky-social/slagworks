package inventory

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// ethtoolRunner implements agent.Runner for a single NIC, serving the ip and
// ethtool output collectNICs reads. An empty module makes `ethtool -m` fail,
// as it does when a driver cannot read the module EEPROM.
type ethtoolRunner struct {
	ifname, speed, port, module string
}

func (e ethtoolRunner) Run(string, ...string) error { return nil }
func (e ethtoolRunner) Available(string) bool       { return true }
func (e ethtoolRunner) Output(name string, args ...string) (string, error) {
	switch name + " " + strings.Join(args, " ") {
	case "ip -j addr show":
		return `[{"ifname":"` + e.ifname + `","address":"3c:ec:ef:00:00:01","addr_info":[]}]`, nil
	case "ethtool -i " + e.ifname:
		return "driver: ice\nfirmware-version: 4.40\nbus-info: 0000:41:00.0\n", nil
	case "ethtool " + e.ifname:
		return "Settings for " + e.ifname + ":\n" +
			"\tSupported ports: [ FIBRE ]\n" +
			"\tSpeed: " + e.speed + "\n" +
			"\tDuplex: Full\n" +
			"\tPort: " + e.port + "\n" +
			"\tLink detected: yes\n", nil
	case "ethtool -m " + e.ifname:
		if e.module != "" {
			return e.module, nil
		}
	}
	return "", os.ErrNotExist
}

// sfpModule renders `ethtool -m` output for an SFP optic in ethtool's own
// "\t%-41s : %s" layout. ethtool labels the wavelength "Laser wavelength"
// (sfpid.c, sff8079_show_wavelength_or_copper_compliance).
func sfpModule(xcvrType, wavelength string) string {
	var b strings.Builder
	for _, kv := range [][2]string{
		{"Identifier", "0x03 (SFP)"},
		{"Connector", "0x07 (LC)"},
		{"Transceiver type", xcvrType},
		{"Laser wavelength", wavelength},
		{"Vendor PN", "FTLX8574D3BCL"},
		{"Vendor SN", "AQK0Z1L"},
	} {
		fmt.Fprintf(&b, "\t%-41s : %s\n", kv[0], kv[1])
	}
	return b.String()
}

// TestCollectNICs_ClassifiesRealEthtoolOutput feeds collectNICs the labels
// ethtool actually prints. An 850nm LC optic is short reach, and a port
// ethtool reports as "Direct Attach Copper" (its name for PORT_DA, in both
// ethtool.c and netlink/settings.c) is an SFP-class cage, not twisted pair
// or backplane.
func TestCollectNICs_ClassifiesRealEthtoolOutput(t *testing.T) {
	tests := []struct {
		name string
		r    ethtoolRunner
		want string
	}{
		{"10G SR optic", ethtoolRunner{speed: "10000Mb/s", port: "FIBRE",
			module: sfpModule("10G Ethernet: 10G Base-SR", "850nm")}, "10gbase-x-sfpp"},
		{"25G SR optic", ethtoolRunner{speed: "25000Mb/s", port: "FIBRE",
			module: sfpModule("Extended: 100G Base-SR4 or 25GBase-SR", "850nm")}, "25gbase-x-sfp28"},
		{"1G SX optic", ethtoolRunner{speed: "1000Mb/s", port: "FIBRE",
			module: sfpModule("Ethernet: 1000BASE-SX", "850nm")}, "1000base-x-sfp"},
		{"10G LR optic", ethtoolRunner{speed: "10000Mb/s", port: "FIBRE",
			module: sfpModule("10G Ethernet: 10G Base-LR", "1310nm")}, "10gbase-lr"},
		{"10G DAC, EEPROM unreadable", ethtoolRunner{speed: "10000Mb/s", port: "Direct Attach Copper"}, "10gbase-x-sfpp"},
		{"25G DAC, EEPROM unreadable", ethtoolRunner{speed: "25000Mb/s", port: "Direct Attach Copper"}, "25gbase-x-sfp28"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.r.ifname = "ens1f0"
			nics := collectNICs(tt.r)
			if len(nics) != 1 {
				t.Fatalf("got %d NICs, want 1", len(nics))
			}
			if nics[0].IntfType != tt.want {
				t.Fatalf("interface type = %q, want %q", nics[0].IntfType, tt.want)
			}
		})
	}
}

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
