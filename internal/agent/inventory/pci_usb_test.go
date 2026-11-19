package inventory

import "testing"

func TestParseUSBLine(t *testing.T) {
	tests := []struct {
		name string
		line string
		want *USBDevice
	}{
		{
			name: "standard hub",
			line: "Bus 001 Device 001: ID 1d6b:0002 Linux Foundation 2.0 root hub",
			want: &USBDevice{Bus: "001", Device: "001", VendorID: "1d6b", DeviceID: "0002", Name: "Linux Foundation 2.0 root hub"},
		},
		{
			name: "usb3 hub",
			line: "Bus 002 Device 001: ID 1d6b:0003 Linux Foundation 3.0 root hub",
			want: &USBDevice{Bus: "002", Device: "001", VendorID: "1d6b", DeviceID: "0003", Name: "Linux Foundation 3.0 root hub"},
		},
		{
			name: "empty line",
			line: "",
			want: nil,
		},
		{
			name: "no ID section",
			line: "Bus 001 Device 001:",
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseUSBLine(tt.line)
			if tt.want == nil {
				if got != nil {
					t.Fatalf("want nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("want non-nil, got nil")
			}
			if *got != *tt.want {
				t.Fatalf("got %+v, want %+v", *got, *tt.want)
			}
		})
	}
}

func TestParseTransceiver(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   *TransceiverInfo
	}{
		{
			name: "SFP28 with LC connector",
			output: `Identifier                                : 0x03 (SFP)
	Connector                                 : 0x07 (LC)
	Vendor PN                                 : FTLX8574D3BCV-B1
	Vendor SN                                 : ABC123
	Laser wavelength                          : 850nm`,
			want: &TransceiverInfo{
				Identifier: "0x03 (SFP)",
				Connector:  "0x07 (LC)",
				VendorPN:   "FTLX8574D3BCV-B1",
				VendorSN:   "ABC123",
				Wavelength: "850nm",
			},
		},
		{
			name:   "empty output",
			output: "",
			want:   nil,
		},
		{
			name:   "no recognized fields",
			output: "Speed: 10000Mb/s\nDuplex: Full",
			want:   nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseTransceiver(tt.output)
			if tt.want == nil {
				if got != nil {
					t.Fatalf("want nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("want non-nil, got nil")
			}
			if *got != *tt.want {
				t.Fatalf("got %+v, want %+v", *got, *tt.want)
			}
		})
	}
}

func TestParseLspciLine(t *testing.T) {
	tests := []struct {
		name string
		line string
		want *PCIDevice
	}{
		{
			name: "QEMU host bridge",
			line: `00:00.0 "Host bridge [0600]" "Intel Corporation [8086]" "82G33/G31/P35/P31 Express DRAM Controller [29c0]" -p00 "" ""`,
			want: &PCIDevice{Slot: "00:00.0", Class: "Host bridge", Vendor: "Intel Corporation", VendorID: "8086", Device: "82G33/G31/P35/P31 Express DRAM Controller", DeviceID: "29c0"},
		},
		{
			name: "virtio-net",
			line: `00:02.0 "Ethernet controller [0200]" "Red Hat, Inc. [1af4]" "Virtio 1.0 network device [1041]" -r01 -p00 "Red Hat, Inc. [1af4]" "Device [0001]"`,
			want: &PCIDevice{Slot: "00:02.0", Class: "Ethernet controller", Vendor: "Red Hat, Inc.", VendorID: "1af4", Device: "Virtio 1.0 network device", DeviceID: "1041"},
		},
		{
			name: "real AMD host",
			line: `00:00.0 "Host bridge [0600]" "Advanced Micro Devices, Inc. [AMD] [1022]" "Device [153a]" -p00 "Advanced Micro Devices, Inc. [AMD] [1022]" "Device [153a]"`,
			want: &PCIDevice{Slot: "00:00.0", Class: "Host bridge", Vendor: "Advanced Micro Devices, Inc. [AMD]", VendorID: "1022", Device: "Device", DeviceID: "153a"},
		},
		{
			name: "empty line",
			line: "",
			want: nil,
		},
		{
			name: "no quotes",
			line: "00:00.0 Host bridge",
			want: nil,
		},
		{
			name: "too few quoted fields",
			line: `00:00.0 "Host bridge [0600]" "Intel [8086]"`,
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseLspciLine(tt.line)
			if tt.want == nil {
				if got != nil {
					t.Fatalf("want nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("want non-nil, got nil")
			}
			if *got != *tt.want {
				t.Fatalf("\n got: %+v\nwant: %+v", *got, *tt.want)
			}
		})
	}
}

func TestClassifyInterfaceType(t *testing.T) {
	tests := []struct {
		name           string
		driver         string
		speed          string
		portType       string
		supportedPorts string
		xcvr           *TransceiverInfo
		want           string
	}{
		// Speed + port type fallback
		{"100G fibre", "", "100000Mb/s", "Fibre", "", nil, "100gbase-x-qsfp28"},
		{"100G copper", "", "100000Mb/s", "TP", "", nil, "100gbase-x-cxp"},
		{"50G fibre", "", "50000Mb/s", "Fibre", "", nil, "50gbase-x-sfp28"},
		{"40G fibre", "", "40000Mb/s", "Fibre", "", nil, "40gbase-x-qsfpp"},
		{"25G fibre", "", "25000Mb/s", "Fibre", "", nil, "25gbase-x-sfp28"},
		{"25G copper", "", "25000Mb/s", "TP", "", nil, "25gbase-kr"},
		{"10G fibre", "", "10000Mb/s", "Fibre", "", nil, "10gbase-x-sfpp"},
		{"10G copper", "", "10000Mb/s", "TP", "", nil, "10gbase-t"},
		{"1G fibre", "", "1000Mb/s", "Fibre", "", nil, "1000base-x-sfp"},
		{"1G copper", "", "1000Mb/s", "TP", "", nil, "1000base-t"},
		{"100M", "", "100Mb/s", "TP", "", nil, "100base-tx"},
		{"unknown speed", "", "Unknown!", "", "", nil, ""},

		// Driver-based overrides
		{"virtio_net", "virtio_net", "Unknown!", "Other", "", nil, "virtual"},
		{"rndis_host BMC USB", "rndis_host", "Unknown!", "Twisted Pair", "", nil, "1000base-t"},

		// Supported ports fallback (switch ASICs: port=Other, speed from supported modes)
		{"switch ASIC 100G via supported_ports", "mlxsw_spectrum", "100000Mb/s", "Other", "[ FIBRE ]", nil, "100gbase-x-qsfp28"},
		{"switch ASIC 25G via supported_ports", "sx_netdev", "25000Mb/s", "None", "[ FIBRE ]", nil, "25gbase-x-sfp28"},

		// Transceiver EEPROM: 25G LR (LC + 1310nm)
		{"25G LR", "", "25000Mb/s", "FIBRE", "", &TransceiverInfo{
			Identifier: "0x03 (SFP)", Connector: "0x07 (LC)", Wavelength: "1310nm",
		}, "25gbase-lr"},
		// Transceiver EEPROM: 25G SR (LC + 850nm)
		{"25G SR", "", "25000Mb/s", "FIBRE", "", &TransceiverInfo{
			Identifier: "0x03 (SFP)", Connector: "0x07 (LC)", Wavelength: "850nm",
		}, "25gbase-x-sfp28"},
		// Transceiver EEPROM: 100G DAC (no separable connector)
		{"100G DAC", "", "100000Mb/s", "FIBRE", "", &TransceiverInfo{
			Identifier: "0x11 (QSFP28)", Connector: "0x23 (No separable connector)",
		}, "100gbase-cr4"},
		// Transceiver EEPROM: 100G LR (LC + 1310nm)
		{"100G LR4", "", "100000Mb/s", "FIBRE", "", &TransceiverInfo{
			Identifier: "0x11 (QSFP28)", Connector: "0x07 (LC)", Wavelength: "1310nm",
		}, "100gbase-lr4"},
		// Transceiver EEPROM: 100G SR4 (LC + 850nm)
		{"100G SR4", "", "100000Mb/s", "FIBRE", "", &TransceiverInfo{
			Identifier: "0x11 (QSFP28)", Connector: "0x07 (LC)", Wavelength: "850nm",
		}, "100gbase-x-qsfp28"},
		// Transceiver EEPROM: 10G LR (LC + 1310nm)
		{"10G LR", "", "10000Mb/s", "", "", &TransceiverInfo{
			Identifier: "0x03 (SFP)", Connector: "0x07 (LC)", Wavelength: "1310nm",
		}, "10gbase-lr"},
		// Transceiver EEPROM: 10G SR (LC + 850nm)
		{"10G SR", "", "10000Mb/s", "", "", &TransceiverInfo{
			Identifier: "0x03 (SFP)", Connector: "0x07 (LC)", Wavelength: "850nm",
		}, "10gbase-x-sfpp"},
		// Transceiver type string: 25G LR from xcvr type (no wavelength)
		{"25G LR from xcvr type", "bnxt_en", "25000Mb/s", "FIBRE", "", &TransceiverInfo{
			Identifier: "0x03 (SFP)", Connector: "0x07 (LC)",
			TransceiverType: "Extended: 100G Base-LR4 or 25GBase-LR",
		}, "25gbase-lr"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyInterfaceType(tt.driver, tt.speed, tt.portType, tt.supportedPorts, tt.xcvr)
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMaxSupportedSpeed(t *testing.T) {
	tests := []struct {
		name  string
		modes []string
		want  string
	}{
		{
			"mlx5 ConnectX-6",
			[]string{"1000baseT/Full", "10000baseKR/Full", "25000baseCR/Full", "25000baseSR/Full"},
			"25000Mb/s",
		},
		{
			"igb 1GbE",
			[]string{"10baseT/Half", "10baseT/Full", "100baseT/Half", "100baseT/Full", "1000baseT/Full"},
			"1000Mb/s",
		},
		{
			"100G QSFP28",
			[]string{"100000baseCR4/Full", "100000baseSR4/Full", "100000baseLR4_ER4/Full"},
			"100000Mb/s",
		},
		{"empty", nil, ""},
		{"no numeric", []string{"notaspeed"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := maxSupportedSpeed(tt.modes)
			if got != tt.want {
				t.Errorf("maxSupportedSpeed(%v) = %q, want %q", tt.modes, got, tt.want)
			}
		})
	}
}
